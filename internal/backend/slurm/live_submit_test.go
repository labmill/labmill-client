package slurm

import (
	"context"
	"strings"
	"testing"
	"time"

	"labmill-client/internal/backend"
	"labmill-client/internal/livetest"
	"labmill-client/internal/store"
)

// TestLiveSubmit submits one tiny CPU-only job on Hpcone and follows it to
// the end. It needs LABMILL_LIVE=1 and LABMILL_LIVE_SUBMIT=1, and writes only under
// the labmill-live directory.
func TestLiveSubmit(t *testing.T) {
	alias := livetest.RequireSubmit(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	run := livetest.Runner{Alias: alias}
	s := &Slurm{Runner: run}

	const dir = "/data/mygroup/labmill-live/slurm"
	if res, err := run.Run(ctx, []string{"mkdir", "-p", dir}, nil); err != nil || res.ExitCode != 0 {
		t.Fatalf("mkdir %s: %v %+v", dir, err, res)
	}
	spec := backend.JobSpec{
		Name: "labmill-live-slurm", CPUs: 1, MemoryGB: 1, Time: 2 * time.Minute,
		WorkDir: dir,
		Command: "hostname; date; echo labmill-live-ok > result.txt",
	}
	script, err := s.Render(spec)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if strings.Contains(strings.ToLower(script), "gres") || strings.Contains(strings.ToLower(script), "gpu") {
		t.Fatalf("script requests a GPU:\n%s", script)
	}
	t.Logf("script:\n%s", script)
	id, err := s.Submit(ctx, script, spec)
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	t.Logf("submitted slurm job %s", id)

	start := time.Now()
	var st backend.Status
	for {
		st, err = s.Status(ctx, id)
		if err != nil {
			t.Fatalf("Status(%s): %v", id, err)
		}
		t.Logf("%s state=%s reason=%q node=%s", time.Since(start).Round(time.Second), st.State, st.Reason, st.Node)
		if st.State == store.StateCompleted || st.State.Terminal() {
			break
		}
		if time.Since(start) > 10*time.Minute {
			t.Fatalf("job %s not finished after 10 minutes (state %s)", id, st.State)
		}
		time.Sleep(10 * time.Second)
	}
	if st.State != store.StateCompleted {
		t.Fatalf("job %s ended %s (%s)", id, st.State, st.Reason)
	}
	logs, err := s.Logs(ctx, id, 50)
	if err != nil {
		t.Fatalf("Logs: %v", err)
	}
	t.Logf("logs:\n%s", logs)
	// The job's `date` may run just before New Year's midnight: accept the year
	// it was submitted in as well as the current one.
	if strings.TrimSpace(logs) == "" || !(strings.Contains(logs, start.Format("2006")) || strings.Contains(logs, time.Now().Format("2006"))) {
		t.Errorf("logs lack hostname/date output: %q", logs)
	}
	res, err := run.Run(ctx, []string{"cat", dir + "/result.txt"}, nil)
	if err != nil || res.ExitCode != 0 {
		t.Fatalf("cat result.txt: %v %+v", err, res)
	}
	if strings.TrimSpace(res.Stdout) != "labmill-live-ok" {
		t.Errorf("result.txt = %q", res.Stdout)
	}
}
