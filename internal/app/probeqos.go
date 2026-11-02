package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"labmill-client/internal/agent"
	"labmill-client/internal/queueplan"
)

// probe_qos (docs/preemptible-qos.md): one read-only round trip on the login node, then the pure-Go ladder of
// internal/queueplan. It changes nothing on the cluster and sends nothing to Labmill.

// qosScript prints the partitions, the QOS table and the user's association, in sections. Every command is read
// only (scontrol show, sacctmgr show). A hidden or denied sacctmgr gives an empty section, which means low confidence.
const qosScript = `printf '@@PART\n'; scontrol show partition -o 2>/dev/null | head -n 500
printf '@@QOS\n'; sacctmgr -nP show qos format=Name,MaxWall,Preempt,PreemptMode,UsageFactor 2>/dev/null | head -n 500
printf '@@ASSOC\n'; sacctmgr -nP show assoc user="$USER" format=QOS,DefaultQOS 2>/dev/null | head -n 500
exit 0`

// qosProbe splits the probe output into its three sections and parses them.
func qosProbe(out string) queueplan.Probe {
	var part, qos, assoc strings.Builder
	var w *strings.Builder
	for _, line := range strings.Split(out, "\n") {
		switch line {
		case "@@PART":
			w = &part
		case "@@QOS":
			w = &qos
		case "@@ASSOC":
			w = &assoc
		default:
			if w != nil {
				w.WriteString(line + "\n")
			}
		}
	}
	allowed, def, readable := queueplan.ParseAssocQOS(assoc.String())
	return queueplan.BuildProbe(queueplan.ParsePartitions(part.String()), queueplan.ParseQOS(qos.String()),
		queueplan.Assoc{Allowed: allowed, Default: def, Readable: readable})
}

func (t *toolset) probeQOS() agent.Tool {
	return agent.Tool{
		Name: "probe_qos",
		Description: "Find which queues a job can use on a cluster (read only, one round trip on the login node): the preemptible partition or QOS that may start sooner, other partitions, and the normal queue as the last step. " +
			"Use it before draft_job when the site skill names no preemptible queue. Give whether the job can restart from a checkpoint and its time. The answer is a ladder with the #SBATCH lines of each entry: use the first entry, and the next one after a failure. A line 'asks' means the user must approve that entry first. It never submits anything.",
		Schema: objSchema(map[string]any{
			"cluster":      strProp("cluster name from list_clusters"),
			"restartable":  map[string]any{"type": "boolean", "description": "the job saves a checkpoint and resumes from it, or is an array of short independent tasks"},
			"hours":        intProp("requested wall time in whole hours (rounded up)"),
			"estimate_hrs": intProp("expected run time in hours (0 if unknown)"),
		}, "cluster", "restartable", "hours"),
		Run: func(ctx context.Context, input json.RawMessage, _ agent.Decision) (string, error) {
			var in struct {
				Cluster     string `json:"cluster"`
				Restartable bool   `json:"restartable"`
				Hours       int    `json:"hours"`
				EstimateHrs int    `json:"estimate_hrs"`
			}
			if err := decode(input, &in); err != nil {
				return "", err
			}
			c, err := t.cluster(in.Cluster)
			if err != nil {
				return "", err
			}
			if t.env.LoginRunner == nil {
				return "", errors.New("Looking at the queues of a cluster isn't available right now.")
			}
			r, err := t.env.LoginRunner(ctx, c)
			if err != nil {
				return "", fmt.Errorf("I couldn't connect to %s: %w", c.Name, err)
			}
			res, err := r.Run(ctx, []string{"sh", "-c", qosScript}, nil)
			if err != nil {
				return "", fmt.Errorf("I couldn't look at the queues of %s: %w", c.Name, err)
			}
			pr := qosProbe(res.Stdout)
			h := time.Duration(in.Hours) * time.Hour
			job := queueplan.Job{Restartable: in.Restartable, TimeLimit: h, Estimate: h}
			if in.EstimateHrs > 0 {
				job.Estimate = time.Duration(in.EstimateHrs) * time.Hour
			}
			return renderQueues(c.Name, pr, queueplan.ChooseQueue(pr, job, queueplan.Prefs{})), nil
		},
	}
}

// renderQueues is the text the model reads: the confidence and the ladder, best first, with the sbatch lines.
func renderQueues(cluster string, pr queueplan.Probe, ladder []queueplan.Option) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Queues on %s (login node, read only). Default partition: %s.", sanitize(cluster), sanitize(pr.Default.Name))
	if pr.Hidden {
		b.WriteString(" The user's QOS list could not be read (low confidence): ask before using anything but the normal queue.")
	}
	b.WriteString("\nLadder, first entry first:\n")
	for i, o := range ladder {
		lines, err := queueplan.SbatchLines(o)
		if err != nil {
			continue // an unsafe name from probe output is never offered
		}
		ask := ""
		if o.NeedsAsk {
			ask = " (asks)"
		}
		fmt.Fprintf(&b, "%d. %s %s%s: %s\n", i+1, o.Tech, sanitize(o.Name), ask, strings.Join(lines, " "))
	}
	return b.String()
}
