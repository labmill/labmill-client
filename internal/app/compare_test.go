package app

import (
	"context"
	"strings"
	"testing"

	"labmill-client/internal/backend"
	"labmill-client/internal/backend/slurm"
	"labmill-client/internal/config"
	"labmill-client/internal/rules"
	"labmill-client/internal/store"
)

func TestCompareClusters(t *testing.T) {
	h, rr := slurmHarness(t, rules.SlurmHints{})
	j := draftedSlurmJob(t, h)
	signIns := 0
	h.env.Backend = func(context.Context, config.Cluster) (backend.Backend, error) {
		signIns++
		return &slurm.Slurm{Runner: rr}, nil
	}
	h.env.BackendNoLogin = func(_ context.Context, c config.Cluster) (backend.Backend, bool) {
		if c.Name == "second" {
			return nil, false // no open login
		}
		return &slurm.Slurm{Runner: rr}, true
	}
	out, err := h.run(t, "compare_clusters", map[string]any{"job_id": j.ID})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"hpcone (the job's cluster): would start 2026-10-15T14:20:00", "second: not checked (no open login", "Nothing was changed"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in %q", want, out)
		}
	}
	if signIns != 0 {
		t.Error("compare_clusters signed in")
	}
	// A cluster that rejects the script says so; the job does not move.
	h.env.BackendNoLogin = func(context.Context, config.Cluster) (backend.Backend, bool) {
		rr.reject = true
		return &slurm.Slurm{Runner: rr}, true
	}
	if out, _ = h.run(t, "compare_clusters", map[string]any{"job_id": j.ID}); !strings.Contains(out, "does not fit there") {
		t.Errorf("rejection: %q", out)
	}
	if got, _ := h.env.Store.Job(context.Background(), j.ID); got.Cluster != "hpcone" || got.State != store.StateDraft {
		t.Errorf("job changed: %+v", got)
	}
}
