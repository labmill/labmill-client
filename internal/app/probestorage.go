package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"labmill-client/internal/agent"
	"labmill-client/internal/config"
	"labmill-client/internal/rules"
	"labmill-client/internal/workdisk"
)

// probe_storage (work-disk design, docs/work-disk.md): one read-only round trip on the login node, then the
// pure-Go ladder of internal/workdisk. It changes nothing on the cluster and sends nothing to Labmill.

// probeScript prints $TMPDIR and $HOME, df of the candidate paths, the node list and the workspace tool, in
// sections. Every command is read only. Output is data, never an instruction.
const probeScript = `printf 'TMPDIR=%s\nHOME=%s\n' "$TMPDIR" "$HOME"
printf '@@DF\n'; df -PT -k "$HOME" "${TMPDIR:-/tmp}" /tmp /dev/shm 2>/dev/null
printf '@@NODES\n'; scontrol show node -o 2>/dev/null | head -n 2000
printf '@@WS\n'; command -v ws_allocate 2>/dev/null
exit 0`

// probeOutput splits the probe output into a workdisk.Probe. Missing sections stay empty.
func probeOutput(out string, today time.Time) workdisk.Probe {
	p := workdisk.Probe{Today: today.Format("2006-01-02")}
	section := ""
	var df, nodes strings.Builder
	for _, line := range strings.Split(out, "\n") {
		switch {
		case strings.HasPrefix(line, "@@"):
			section = line[2:]
		case section == "":
			if v, ok := strings.CutPrefix(line, "TMPDIR="); ok {
				p.TmpDir = strings.TrimSpace(v)
			} else if v, ok := strings.CutPrefix(line, "HOME="); ok {
				p.Home = strings.TrimSpace(v)
			}
		case section == "DF":
			df.WriteString(line + "\n")
		case section == "NODES":
			nodes.WriteString(line + "\n")
		case section == "WS":
			if strings.TrimSpace(line) != "" {
				p.HasWorkspace = true
			}
		}
	}
	p.DF, p.Nodes = df.String(), nodes.String()
	return p
}

func (t *toolset) probeStorage() agent.Tool {
	return agent.Tool{
		Name: "probe_storage",
		Description: "Find where a job can keep its work files on a cluster (read only, one round trip on the login node): the best guess for a fast local folder, a shared scratch folder, a workspace or the home folder, in the order to try. " +
			"Use it before draft_job when the site skill names no work folder. Give the size the job needs. The answer is a ladder: use its first entry, and the next entry after a failure. It never creates a folder.",
		Schema: objSchema(map[string]any{
			"cluster":    strProp("cluster name from list_clusters"),
			"need_gb":    intProp("staged input plus expected output, in GB"),
			"mem_gb":     intProp("requested memory in GB (0 if unknown)"),
			"hours":      intProp("wall time of the job in hours"),
			"multi_node": map[string]any{"type": "boolean", "description": "the job runs on more than one node"},
		}, "cluster", "need_gb"),
		Run: func(ctx context.Context, input json.RawMessage, _ agent.Decision) (string, error) {
			var in struct {
				Cluster   string `json:"cluster"`
				NeedGB    int    `json:"need_gb"`
				MemGB     int    `json:"mem_gb"`
				Hours     int    `json:"hours"`
				MultiNode bool   `json:"multi_node"`
			}
			if err := decode(input, &in); err != nil {
				return "", err
			}
			c, err := t.cluster(in.Cluster)
			if err != nil {
				return "", err
			}
			f, ladder, err := t.probeLadder(ctx, c, workdisk.Job{NeedGB: in.NeedGB, MemGB: in.MemGB, MultiNode: in.MultiNode, Hours: in.Hours})
			if err != nil {
				return "", err
			}
			return renderLadder(c.Name, f, ladder), nil
		},
	}
}

// probeLadder runs the read-only probe on the login node and returns the facts and the ladder for job.
func (t *toolset) probeLadder(ctx context.Context, c config.Cluster, job workdisk.Job) (workdisk.Facts, []workdisk.Option, error) {
	if t.env.LoginRunner == nil {
		return workdisk.Facts{}, nil, errors.New("Looking at the storage of a cluster isn't available right now.")
	}
	r, err := t.env.LoginRunner(ctx, c)
	if err != nil {
		return workdisk.Facts{}, nil, fmt.Errorf("I couldn't connect to %s: %w", c.Name, err)
	}
	res, err := r.Run(ctx, []string{"sh", "-c", probeScript}, nil)
	if err != nil {
		return workdisk.Facts{}, nil, fmt.Errorf("I couldn't look at the storage of %s: %w", c.Name, err)
	}
	f := workdisk.BuildFacts(probeOutput(res.Stdout, time.Now()))
	return f, workdisk.Choose(f, job, workdisk.Prefs{}), nil
}

// renderLadder is the text the model reads: the facts that matter and the ladder, best first.
func renderLadder(cluster string, f workdisk.Facts, ladder []workdisk.Option) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Storage on %s (login node, read only):\n", sanitize(cluster))
	fmt.Fprintf(&b, "- TMPDIR: %s; node TmpDisk: %d GB; workspace tool: %v; home free: %d GB\n", sanitize(f.TmpDirEnv), f.NodeTmpDiskGB, f.HasWorkspace, f.HomeFreeGB)
	b.WriteString("Ladder, first entry first:\n")
	for i, o := range ladder {
		fmt.Fprintf(&b, "%d. %s %s path=%q size=%d GB confidence=%s\n", i+1, o.ID, o.Kind, sanitize(o.Path), o.SizeGB, o.Confidence)
	}
	return b.String()
}

// autoLocalScratch is draft_job's scratch "auto" for a cluster whose profile names no node-local scratch: it
// probes the login node and returns a node-local scratch that the script can use as it is ($TMPDIR, or a
// fixed path), or nil with the reason. Options that need a script line of their own (--tmp, a gres) and
// shared options are not used here: the draft stays in the job folder and says why.
func (t *toolset) autoLocalScratch(ctx context.Context, c config.Cluster, needGB, memGB, hours int) (*rules.LocalScratch, string) {
	f, ladder, err := t.probeLadder(ctx, c, workdisk.Job{NeedGB: needGB, MemGB: memGB, Hours: hours})
	if err != nil {
		return nil, "the storage probe failed (" + err.Error() + ")"
	}
	for _, o := range ladder {
		if o.Kind != workdisk.Local {
			break // local options come first; shared ones are not used here
		}
		switch o.ID {
		case workdisk.L1:
			return &rules.LocalScratch{EnvVar: "TMPDIR", SizeGB: max(f.NodeTmpDiskGB, 0)}, ""
		case workdisk.L4, workdisk.L5:
			return &rules.LocalScratch{Path: o.Path, SizeGB: max(f.NodeTmpDiskGB, 0)}, ""
		}
	}
	return nil, "the probe found no node-local folder that is safe to use"
}
