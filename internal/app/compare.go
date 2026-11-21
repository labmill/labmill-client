package app

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"labmill-client/internal/agent"
	"labmill-client/internal/backend"
	"labmill-client/internal/config"
	"labmill-client/internal/store"
)

// compare_clusters (owner queue (e)): asks the other clusters, with `sbatch
// --test-only`, when the drafted script would start there. It only reads, it
// never signs in (a cluster with no open login is skipped) and it never
// switches: the user decides.

func (t *toolset) compareClusters() agent.Tool {
	return agent.Tool{
		Name: "compare_clusters",
		Description: "Ask the user's other clusters when a drafted job would start there (Slurm's own `sbatch --test-only`; nothing is submitted, nothing is signed in to: a cluster without an open login is skipped). " +
			"Use it after draft_job when the expected start on the job's cluster is late, or when the user asks if another cluster is better. " +
			"It only reports. Never switch the job to another cluster by yourself: tell the user the comparison and let them choose. " +
			"A script written for one cluster may name partitions or accounts the others do not have; a rejection says so.",
		Schema:        objSchema(map[string]any{"job_id": intProp("id of the drafted job")}, "job_id"),
		NeedsApproval: true,
		Propose: func(input json.RawMessage) (agent.Proposal, error) {
			var in struct {
				JobID int64 `json:"job_id"`
			}
			if err := decode(input, &in); err != nil {
				return agent.Proposal{}, err
			}
			j, targets, err := t.compareTargets(context.Background(), in.JobID)
			if err != nil {
				return agent.Proposal{}, err
			}
			names := make([]string, len(targets))
			for i, c := range targets {
				names[i] = sanitize(c.Name)
				if c.Name == j.Cluster {
					names[i] += " (the job's cluster)"
				}
			}
			return agent.Proposal{Tool: "compare_clusters",
				Summary: fmt.Sprintf("Send the script of job %d to %s for a start-time test (nothing is submitted)", j.ID, strings.Join(names, ", ")),
				Details: j.Script}, nil
		},
		Run: func(ctx context.Context, input json.RawMessage, _ agent.Decision) (string, error) {
			var in struct {
				JobID int64 `json:"job_id"`
			}
			if err := decode(input, &in); err != nil {
				return "", err
			}
			j, targets, err := t.compareTargets(ctx, in.JobID)
			if err != nil {
				return "", err
			}
			var b strings.Builder
			fmt.Fprintf(&b, "Expected start of job %d (%s) by cluster. Times are each cluster's own clock.\n", j.ID, sanitize(j.Name))
			for _, c := range targets {
				mark := ""
				if c.Name == j.Cluster {
					mark = " (the job's cluster)"
				}
				fmt.Fprintf(&b, "- %s%s: %s\n", c.Name, mark, t.startOn(ctx, c, j.Script))
			}
			b.WriteString("Nothing was changed. The job stays on its cluster unless the user asks to move it; a new draft on another cluster needs its own data upload and approval.")
			return b.String(), nil
		},
	}
}

// compareTargets checks the job and returns the clusters that get its script:
// all configured ones, the job's own included. Propose and Run both use it, so
// the card names what Run asks.
func (t *toolset) compareTargets(ctx context.Context, id int64) (store.Job, []config.Cluster, error) {
	j, err := t.job(ctx, id)
	if err != nil {
		return j, nil, err
	}
	if j.RemoteID != "" {
		return j, nil, fmt.Errorf("Job %d is already submitted to %s. Compare before submit_job.", j.ID, j.Cluster)
	}
	if strings.TrimSpace(j.Script) == "" {
		return j, nil, fmt.Errorf("Job %d has no script yet. Call draft_job first.", j.ID)
	}
	if t.env.Config == nil || t.env.BackendNoLogin == nil {
		return j, nil, fmt.Errorf("Comparing clusters isn't available right now.")
	}
	all := t.env.Config.List()
	if len(all) < 2 {
		return j, nil, fmt.Errorf("The user has no other cluster configured, so there is nothing to compare.")
	}
	return j, all, nil
}

// startOn is the one-line start estimate of script on cluster c.
func (t *toolset) startOn(ctx context.Context, c config.Cluster, script string) string {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	be, ok := t.env.BackendNoLogin(ctx, c)
	if !ok {
		return "not checked (no open login right now; sign in to it first)"
	}
	e, ok := be.(backend.StartEstimator)
	if !ok {
		return "not available (this scheduler can't estimate a start)"
	}
	est, err := e.EstimateStart(ctx, script)
	switch {
	case err == nil:
		s := "would start " + sanitize(est.Start)
		if est.Partition != "" {
			s += " in " + sanitize(est.Partition)
		}
		return s
	case strings.Contains(err.Error(), " failed (exit ") && !controllerDown(err.Error()):
		_, msg, _ := strings.Cut(err.Error(), "): ")
		return "the script does not fit there: " + clip(oneLine(sanitize(msg)), 200)
	}
	return "unknown (the cluster did not answer)"
}
