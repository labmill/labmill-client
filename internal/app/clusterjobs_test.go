package app

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"labmill-client/internal/backend"
	"labmill-client/internal/config"
	"labmill-client/internal/store"
)

type listerBackend struct {
	*fakeBackend
	jobs  []backend.RemoteJob
	err   error
	since time.Time
	limit int
}

func (l *listerBackend) Jobs(_ context.Context, since time.Time, limit int) ([]backend.RemoteJob, error) {
	l.since, l.limit = since, limit
	return l.jobs, l.err
}

func withLister(h *harness, l *listerBackend) {
	h.env.Backend = func(context.Context, config.Cluster) (backend.Backend, error) { return l, nil }
}

func TestClusterJobs(t *testing.T) {
	old := time.Local
	time.Local = time.UTC
	defer func() { time.Local = old }()
	h := newHarness(t)
	l := &listerBackend{fakeBackend: h.be, jobs: []backend.RemoteJob{
		{ID: "1", Name: "train", State: store.StateRunning, Partition: "gpu", Node: "n01", Start: time.Date(2026, 10, 3, 9, 0, 0, 0, time.UTC)},
		{ID: "2", Name: "wait", State: store.StatePending, Partition: "gpu", Reason: "Resources", Submit: time.Date(2026, 10, 3, 10, 0, 0, 0, time.UTC)},
		{ID: "3", Name: "old\x1b[31m", State: store.StateFailed, End: time.Date(2026, 10, 2, 8, 30, 0, 0, time.UTC)},
	}}
	withLister(h, l)
	out, err := h.run(t, "cluster_jobs", map[string]any{"cluster": "hpcone", "limit": 2})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"1 train: running, gpu, on n01, started Oct 3 09:00", "2 wait: pending, gpu, Resources, submitted Oct 3 10:00", "more jobs not shown"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "old") {
		t.Errorf("limit not applied:\n%s", out)
	}
	if l.limit != 3 || !l.since.Equal(time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)) {
		t.Errorf("backend got since=%v limit=%d", l.since, l.limit)
	}
	// days=0: active only; control characters removed.
	out, err = h.run(t, "cluster_jobs", map[string]any{"cluster": "hpcone", "days": 0})
	if err != nil || !l.since.IsZero() || strings.Contains(out, "\x1b") || !strings.Contains(out, "ended Oct 2 08:30") {
		t.Errorf("days=0: since=%v err=%v out=%q", l.since, err, out)
	}
}

func TestClusterJobsNoAccountingAndEmpty(t *testing.T) {
	h := newHarness(t)
	l := &listerBackend{fakeBackend: h.be, jobs: []backend.RemoteJob{{ID: "1", Name: "x", State: store.StateRunning}}, err: fmt.Errorf("wrapped: %w", backend.ErrNoAccounting)}
	withLister(h, l)
	out, err := h.run(t, "cluster_jobs", map[string]any{"cluster": "hpcone"})
	if err != nil || !strings.Contains(out, "- 1 x: running") || !strings.Contains(out, "finished jobs aren't available") {
		t.Errorf("out=%q err=%v", out, err)
	}
	l.jobs = nil
	out, err = h.run(t, "cluster_jobs", map[string]any{"cluster": "hpcone"})
	if err != nil || !strings.Contains(out, "No queued or running jobs") || !strings.Contains(out, "finished jobs aren't available") {
		t.Errorf("empty: out=%q err=%v", out, err)
	}
	l.err = nil
	out, _ = h.run(t, "cluster_jobs", map[string]any{"cluster": "hpcone"})
	if !strings.Contains(out, "none finished in the last 7 days") {
		t.Errorf("empty: %q", out)
	}
	l.err = errors.New("boom")
	if _, err := h.run(t, "cluster_jobs", map[string]any{"cluster": "hpcone"}); err == nil {
		t.Error("backend error not reported")
	}
}

func TestClusterJobsNotSupportedAndValidation(t *testing.T) {
	h := newHarness(t) // fakeBackend does not implement Lister
	out, err := h.run(t, "cluster_jobs", map[string]any{"cluster": "hpcone"})
	if err != nil || !strings.Contains(out, "isn't available") {
		t.Errorf("out=%q err=%v", out, err)
	}
	for _, in := range []map[string]any{
		{"cluster": "hpcone", "days": 31}, {"cluster": "hpcone", "days": -1},
		{"cluster": "hpcone", "limit": 201}, {"cluster": "nope"},
	} {
		if _, err := h.run(t, "cluster_jobs", in); err == nil {
			t.Errorf("%v accepted", in)
		}
	}
}

func TestClusterJobsFormatAndNotes(t *testing.T) {
	line := formatRemoteJob(backend.RemoteJob{ID: "1", Name: "a\nb\tc", State: store.StateFailed, Reason: "x\ny", Partition: "p\tq", Node: "n\n1"}, time.Now())
	if strings.ContainsAny(line, "\n\t") {
		t.Errorf("control whitespace left: %q", line)
	}
	h := newHarness(t)
	l := &listerBackend{fakeBackend: h.be, jobs: []backend.RemoteJob{{ID: "1", Name: "n", State: store.StateCompleted}}, err: backend.ErrTruncated}
	withLister(h, l)
	out, err := h.run(t, "cluster_jobs", map[string]any{"cluster": "hpcone"})
	if err != nil || !strings.Contains(out, "some older jobs may be missing") {
		t.Errorf("truncated note: %v %q", err, out)
	}
}

func TestClusterJobsAsksWhichCluster(t *testing.T) {
	h := newHarness(t)
	h.env.Config = &config.Config{Clusters: []config.Cluster{{Name: "a"}, {Name: "b"}, {Name: "c"}}}
	out, err := h.run(t, "cluster_jobs", map[string]any{})
	if err != nil || out != "Say which cluster: a, b, c." {
		t.Errorf("out=%q err=%v", out, err)
	}
}
