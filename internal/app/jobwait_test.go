package app

import (
	"context"
	"strings"
	"testing"
	"time"

	"labmill-client/internal/agent"
	"labmill-client/internal/backend"
	"labmill-client/internal/config"
	"labmill-client/internal/store"
)

// countingBackend becomes completed after doneAfter Status calls.
type countingBackend struct {
	*fakeBackend
	calls, doneAfter int
}

func (c *countingBackend) Status(ctx context.Context, id string) (backend.Status, error) {
	c.calls++
	if c.calls >= c.doneAfter {
		c.fakeBackend.status = backend.Status{State: store.StateCompleted}
	}
	return c.fakeBackend.Status(ctx, id)
}

func TestJobStatusWait(t *testing.T) {
	cases := []struct {
		name      string
		wait      int
		doneAfter int
		wantCalls int
		wantSleep []time.Duration
		wantText  string
	}{
		{"no wait", 0, 3, 1, nil, "running"},
		{"completes", 120, 3, 3, []time.Duration{15 * time.Second, 15 * time.Second}, "completed"},
		{"times out", 40, 100, 4, []time.Duration{15 * time.Second, 15 * time.Second, 10 * time.Second}, "running"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			cb := &countingBackend{fakeBackend: h.be, doneAfter: tc.doneAfter}
			h.be.status = backend.Status{State: store.StateRunning}
			h.env.Backend = func(context.Context, config.Cluster) (backend.Backend, error) { return cb, nil }
			id, _ := h.env.Store.CreateJob(context.Background(), store.Job{Name: "w", Cluster: "hpcone", Backend: "slurm", RemoteID: "7", State: store.StateRunning})
			var slept []time.Duration
			ts := &toolset{env: h.env, sleep: func(_ context.Context, d time.Duration) error { slept = append(slept, d); return nil }}
			out, err := ts.jobStatus().Run(context.Background(), mustJSON(map[string]any{"job_id": id, "wait_seconds": tc.wait}), agent.Decision{})
			if err != nil || !strings.Contains(out, tc.wantText) {
				t.Fatalf("out=%q err=%v", out, err)
			}
			if cb.calls != tc.wantCalls || len(slept) != len(tc.wantSleep) {
				t.Fatalf("calls=%d slept=%v", cb.calls, slept)
			}
			for i, d := range tc.wantSleep {
				if slept[i] != d {
					t.Errorf("sleep %d = %v", i, slept[i])
				}
			}
		})
	}
}

func TestJobStatusWaitBounds(t *testing.T) {
	h := newHarness(t)
	id, _ := h.env.Store.CreateJob(context.Background(), store.Job{Name: "w", Cluster: "hpcone", Backend: "slurm", RemoteID: "7", State: store.StateRunning})
	for _, w := range []int{-1, 121, 601} {
		if _, err := h.run(t, "job_status", map[string]any{"job_id": id, "wait_seconds": w}); err == nil {
			t.Errorf("wait_seconds %d accepted", w)
		}
	}
}

func TestJobStatusWaitCancel(t *testing.T) {
	h := newHarness(t)
	id, _ := h.env.Store.CreateJob(context.Background(), store.Job{Name: "w", Cluster: "hpcone", Backend: "slurm", RemoteID: "7", State: store.StateRunning})
	ctx, cancel := context.WithCancel(context.Background())
	ts := &toolset{env: h.env, sleep: func(ctx context.Context, _ time.Duration) error { cancel(); return ctx.Err() }}
	if _, err := ts.jobStatus().Run(ctx, mustJSON(map[string]any{"job_id": id, "wait_seconds": 60}), agent.Decision{}); err == nil {
		t.Error("cancel ignored")
	}
}

func TestUploadSummaryShowsHomeRelative(t *testing.T) {
	h := newHarness(t)
	if _, err := h.run(t, "stage_data", map[string]any{"root": h.home, "paths": []string{"data"}, "job_name": "j1"}); err != nil {
		t.Fatal(err)
	}
	id := 1
	p, err := h.tools["upload_data"].Propose(mustJSON(map[string]any{"job_id": id, "cluster": "hpcone", "remote_dir": "labmill/j1"}))
	if err != nil || !strings.Contains(p.Summary, "hpcone:~/labmill/j1 (through") {
		t.Fatalf("%q %v", p.Summary, err)
	}
}

// TestBackgroundJobsPrompt: the model is told not to block the chat on a
// running job, and job_status says the wait is capped and only on request.
func TestBackgroundJobsPrompt(t *testing.T) {
	h := newHarness(t)
	sp := SystemPrompt(h.env)
	for _, want := range []string{"runs in the background", "end your turn", "Do not wait for a job unless the user explicitly asks", "at most 120"} {
		if !strings.Contains(sp, want) {
			t.Errorf("system prompt lacks %q", want)
		}
	}
	ts := &toolset{env: h.env}
	d := ts.jobStatus().Description
	if !strings.Contains(d, "at most 120") || !strings.Contains(d, "explicitly asks") {
		t.Errorf("job_status description = %q", d)
	}
}
