package account

import (
	"context"
	"encoding/json"
	"io"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"labmill-client/internal/backend"
	"labmill-client/internal/backend/slurm"
	"labmill-client/internal/store"
)

// sacctRunner answers squeue (job gone) and the two sacct queries of Status.
type sacctRunner struct{}

func (sacctRunner) Run(_ context.Context, argv []string, _ io.Reader) (backend.Result, error) {
	switch {
	case slices.Contains(argv, "squeue"):
		return backend.Result{}, nil
	case slices.Contains(argv, "-X"):
		return backend.Result{Stdout: "OUT_OF_MEMORY|0:125|2026-10-03T10:00:00|2026-10-03T11:30:00|n1|4242|5400|billing=8,cpu=8,gres/gpu:a100=1,gres/gpu=1,mem=32G,node=1\n"}, nil
	case slices.Contains(argv, "--format=JobID,TotalCPU,MaxRSS"):
		return backend.Result{Stdout: "4242|1-02:03:04.500|\n4242.batch|1-02:03:00|22437888K\n4242.0|00:04.100|1024K\n"}, nil
	}
	return backend.Result{ExitCode: 127}, nil
}

// The facts from a real sacct status reach the report: exit code, the
// scheduler's times and the use.
func TestUploadSendsSchedulerFacts(t *testing.T) {
	ctx := context.Background()
	st, err := (&slurm.Slurm{Runner: sacctRunner{}}).Status(ctx, "4242")
	if err != nil {
		t.Fatal(err)
	}
	facts, ok := st.Facts()
	if !ok {
		t.Fatalf("no facts in %+v", st)
	}
	f := &fakeAPI{}
	srv := httptest.NewServer(f.handler(t))
	defer srv.Close()
	s := newStore(t)
	p := link(t, srv.URL, time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)) // before the sacct end time
	id := addJob(t, s, store.StateFailed, "OUT_OF_MEMORY")
	if err := s.UpdateJob(ctx, id, func(j *store.Job) error { j.Facts = facts; return nil }); err != nil {
		t.Fatal(err)
	}
	u := &Uploader{Store: s, LinkPath: p, Institution: func(string) string { return "Example University" }}
	if n, err := u.Pass(ctx); err != nil || n != 1 {
		t.Fatalf("pass: %d %v", n, err)
	}
	var got map[string]any
	json.Unmarshal([]byte(f.jobs[0]), &got)
	want := map[string]any{"exit_code": "0:125", "started_at": "2026-10-03T10:00:00Z", "ended_at": "2026-10-03T11:30:00Z"}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %v, want %v", k, got[k], v)
		}
	}
	used, _ := got["used"].(map[string]any)
	if used["elapsed"] != "01:30:00" || used["cpu_time"] != "26:03:04" || used["max_rss"] != "21.4G" || used["gpu"] != 1.0 || len(used) != 4 {
		t.Errorf("used = %v", used)
	}
	if strings.Contains(f.jobs[0], "n1") {
		t.Errorf("report holds the node name: %s", f.jobs[0])
	}

	// Facts of an earlier run (the job was resubmitted) are not sent.
	j, _ := s.Job(ctx, id)
	j.Facts.RemoteID = "4241"
	if rep, _ := BuildReport(j, nil, "lmc-x"); rep.ExitCode != "" || rep.Used != nil || rep.StartedAt != nil {
		t.Errorf("old facts used: %+v", rep)
	}
}
