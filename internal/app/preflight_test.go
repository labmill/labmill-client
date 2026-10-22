package app

import (
	"context"
	"errors"
	"strings"
	"testing"

	"labmill-client/internal/agent"
	"labmill-client/internal/backend"
	"labmill-client/internal/config"
	"labmill-client/internal/rules"
	"labmill-client/internal/store"
)

func draftedSlurmJob(t *testing.T, h *harness) store.Job {
	t.Helper()
	if _, err := h.run(t, "draft_job", draftSpec(nil, nil)); err != nil {
		t.Fatal(err)
	}
	jobs, _ := h.env.Store.Jobs(context.Background(), store.JobFilter{})
	return jobs[0]
}

func TestPreflightSlurm(t *testing.T) {
	tests := []struct {
		name string
		prep func(h *harness, rr *placeRunner)
		want string
	}{
		{"accepts", func(*harness, *placeRunner) {}, "Slurm accepts this script, expected start 2026-10-15T14:20:00 (cluster time)."},
		{"rejects", func(_ *harness, rr *placeRunner) { rr.reject = true }, "Slurm rejects this script: sbatch: error: Invalid account"},
		{"controller down", func(_ *harness, rr *placeRunner) {
			rr.rejectMsg = "sbatch: error: Unable to contact slurm controller (connect failure)"
		}, "Pre-flight check: not available"},
		{"unavailable", func(h *harness, _ *placeRunner) {
			h.env.Backend = func(context.Context, config.Cluster) (backend.Backend, error) {
				return nil, errors.New("no connection")
			}
		}, "Pre-flight check: not available"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h, rr := slurmHarness(t, rules.SlurmHints{})
			j := draftedSlurmJob(t, h)
			tc.prep(h, rr)
			p, err := h.tools["submit_job"].Propose(mustJSON(map[string]any{"job_id": j.ID}))
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(p.Summary, tc.want) {
				t.Errorf("summary = %q, want %q", p.Summary, tc.want)
			}
		})
	}
}

// The approved review carries a pre-flight line that differs from the one
// built at submit time; submit still goes through.
func TestPreflightDoesNotBlockSubmit(t *testing.T) {
	h, rr := slurmHarness(t, rules.SlurmHints{})
	j := draftedSlurmJob(t, h)
	rr.reject = true
	in := mustJSON(map[string]any{"job_id": j.ID})
	p, err := h.tools["submit_job"].Propose(in)
	if err != nil || !strings.Contains(p.Summary, "Slurm rejects") {
		t.Fatalf("summary = %q, %v", p.Summary, err)
	}
	rr.reject = false
	if _, err := h.tools["submit_job"].Run(context.Background(), in, agent.Decision{Approved: true, Proposal: p}); err != nil {
		t.Fatal(err)
	}
}

// A job that changed after the review was shown is refused even when the
// review carries a pre-flight line.
func TestPreflightKeepsChangeCheck(t *testing.T) {
	h, _ := slurmHarness(t, rules.SlurmHints{})
	j := draftedSlurmJob(t, h)
	in := mustJSON(map[string]any{"job_id": j.ID})
	p, err := h.tools["submit_job"].Propose(in)
	if err != nil || !strings.Contains(p.Summary, preflightMark) {
		t.Fatalf("summary = %q, %v", p.Summary, err)
	}
	if err := h.env.Store.UpdateJob(context.Background(), j.ID, func(j *store.Job) error { j.Name = "other"; return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := h.tools["submit_job"].Run(context.Background(), in, agent.Decision{Approved: true, Proposal: p}); !errors.Is(err, errChanged) {
		t.Errorf("err = %v, want errChanged", err)
	}
	// Extra text after the pre-flight line is not accepted either.
	h2, _ := slurmHarness(t, rules.SlurmHints{})
	j2 := draftedSlurmJob(t, h2)
	in2 := mustJSON(map[string]any{"job_id": j2.ID})
	p2, _ := h2.tools["submit_job"].Propose(in2)
	p2.Summary += "\nMore text"
	if _, err := h2.tools["submit_job"].Run(context.Background(), in2, agent.Decision{Approved: true, Proposal: p2}); !errors.Is(err, errChanged) {
		t.Errorf("err = %v, want errChanged", err)
	}
}
