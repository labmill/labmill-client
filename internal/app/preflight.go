package app

import (
	"context"
	"strings"
	"time"

	"labmill-client/internal/agent"
	"labmill-client/internal/backend"
	"labmill-client/internal/store"
)

// preflightMark starts the pre-flight text at the end of the submit review.
// The text depends on the cluster's state at that moment, so submit_job
// leaves it out when it compares the approved review with the job (M64.2).
const preflightMark = "\n\nPre-flight check: "

// withoutPreflight is the approved review with its pre-flight line removed,
// but only when the rest is exactly the built Summary followed by a one-line
// pre-flight text; anything else is returned unchanged and so differs.
func withoutPreflight(built, approved agent.Proposal) agent.Proposal {
	rest, ok := strings.CutPrefix(approved.Summary, built.Summary)
	if !ok {
		return approved
	}
	if text, isPre := strings.CutPrefix(rest, preflightMark); rest == "" || isPre && !strings.Contains(text, "\n") {
		approved.Summary = built.Summary
	}
	return approved
}

// preflight is the read-only check shown in the submit review: Slurm's own
// answer to `sbatch --test-only`. It submits nothing and never blocks; when the cluster can't
// answer it says so.
func (t *toolset) preflight(ctx context.Context, j store.Job) string {
	const unavailable = "not available (Labmill could not ask the cluster); the job can still be submitted."
	c, err := t.cluster(j.Cluster)
	if err != nil {
		return unavailable
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	be, err := t.backendFor(ctx, c)
	if err != nil {
		return unavailable
	}
	e, ok := be.(backend.StartEstimator)
	if !ok {
		return unavailable
	}
	est, err := e.EstimateStart(ctx, j.Script)
	switch {
	case err == nil:
		return "Slurm accepts this script, expected start " + sanitize(est.Start) + " (cluster time)."
	case strings.Contains(err.Error(), " failed (exit ") && !controllerDown(err.Error()):
		// Slurm answered with an error: show it so the user can fix the script.
		_, msg, _ := strings.Cut(err.Error(), "): ")
		return "Slurm rejects this script: " + clip(oneLine(sanitize(msg)), 300)
	}
	return unavailable
}

// controllerDown is true for sbatch errors that say nothing about the script.
func controllerDown(msg string) bool {
	for _, s := range []string{"Unable to contact slurm controller", "Socket timed out", "Slurm controller not responding"} {
		if strings.Contains(msg, s) {
			return true
		}
	}
	return false
}
