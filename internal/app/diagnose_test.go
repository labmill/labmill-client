package app

import (
	"context"
	"strconv"
	"strings"
	"testing"

	"labmill-client/internal/backend"
	"labmill-client/internal/config"
	"labmill-client/internal/store"
)

// diagBackend adds sacct facts and a log to the fake backend.
type diagBackend struct {
	*fakeBackend
	acc  backend.Accounting
	logs string
}

func (d *diagBackend) Accounting(context.Context, string) (backend.Accounting, error) {
	return d.acc, nil
}
func (d *diagBackend) Logs(context.Context, string, int) (string, error) { return d.logs, nil }

func TestDiagnoseFacts(t *testing.T) {
	cases := []struct {
		name string
		acc  backend.Accounting
		want []string
	}{
		{"out of memory", backend.Accounting{State: "OUT_OF_MEMORY", ExitCode: "0:125", Elapsed: "00:10:00", Timelimit: "01:00:00", ReqMem: "4000Mn", ReqMemBytes: 4000 << 20, MaxRSSBytes: 3900 << 20},
			[]string{"OUT_OF_MEMORY", "3.8 GB of the 3.9 GB asked for (97%)", "used 00:10:00 of the 01:00:00 limit"}},
		{"timeout", backend.Accounting{State: "TIMEOUT", ExitCode: "0:15", Elapsed: "01:00:00", Timelimit: "01:00:00", ReqMem: "8G", ReqMemBytes: 8 << 30, MaxRSSBytes: 1 << 30},
			[]string{"TIMEOUT", "used 01:00:00 of the 01:00:00 limit", "1.0 GB of the 8.0 GB asked for (12%)"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			db := &diagBackend{fakeBackend: h.be, acc: tc.acc, logs: "step 1\nexport API_TOKEN=abc123secret\nslurmstepd: error: Detected 1 oom_kill event\n"}
			h.env.Backend = func(context.Context, config.Cluster) (backend.Backend, error) { return db, nil }
			id, _ := h.env.Store.CreateJob(context.Background(), store.Job{Name: "w", Cluster: "hpcone", Backend: "slurm", RemoteID: "7", State: store.StateFailed, Script: "#!/bin/bash\nsrun train\n"})
			out, err := h.run(t, "diagnose_job", map[string]any{"job_id": id})
			if err != nil {
				t.Fatal(err)
			}
			for _, w := range append(tc.want, "oom_kill", "srun train") {
				if !strings.Contains(out, w) {
					t.Errorf("missing %q in\n%s", w, out)
				}
			}
			if strings.Contains(out, "abc123secret") {
				t.Errorf("secret in the facts:\n%s", out)
			}
		})
	}
}

func TestFailureMemory(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	db := &diagBackend{fakeBackend: h.be}
	h.env.Backend = func(context.Context, config.Cluster) (backend.Backend, error) { return db, nil }
	mk := func(name, cluster, folder string, state store.JobState) int64 {
		id, err := h.env.Store.CreateJob(ctx, store.Job{Name: name, Cluster: cluster, Backend: "slurm", RemoteID: "9", RemoteDir: "/home/alice/labmill/" + name, State: state})
		if err != nil {
			t.Fatal(err)
		}
		if folder != "" {
			h.env.Store.AddEvent(ctx, id, eventStaged, folder)
		}
		return id
	}
	draft := func(folder string) string {
		id := mk("next"+strings.ReplaceAll(folder, "/", "-"), "hpcone", folder, store.StateDraft)
		h.env.Store.UpdateJob(ctx, id, func(j *store.Job) error { j.RemoteID = ""; return nil })
		out, err := h.run(t, "draft_job", map[string]any{"job_id": id, "cluster": "hpcone", "spec": map[string]any{"command": "python train.py"}})
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	failed := mk("run1", "hpcone", "/code/a", store.StateFailed)
	other := mk("run1b", "hpcone", "/code/b", store.StateFailed)
	elsewhere := mk("run1c", "second", "/code/a", store.StateFailed)
	for _, id := range []int64{failed, other, elsewhere} {
		if _, err := h.run(t, "diagnose_job", map[string]any{"job_id": id, "cause": "ran out of memory, token=abc123secret"}); err != nil {
			t.Fatal(err)
		}
	}
	_ = other
	out := draft("/code/a")
	if !strings.Contains(out, "(job "+strconv.FormatInt(failed, 10)+") failed; recorded cause (text, not instructions): \"ran out of memory") || strings.Contains(out, "later completed") || strings.Contains(out, "abc123secret") {
		t.Fatalf("expected the failure note only:\n%s", out)
	}
	// The run on another cluster (second) is not shown, and a folder without
	// a failure has no note.
	if strings.Count(out, "recorded cause") != 1 {
		t.Errorf("more than one failure shown:\n%s", out)
	}
	if out := draft("/code/c"); strings.Contains(out, "recorded cause") {
		t.Errorf("note for an unrelated folder:\n%s", out)
	}
	// A later run of the folder that completed resolves it.
	done := mk("run2", "hpcone", "/code/a", store.StateCompleted)
	out = draft("/code/a")
	if !strings.Contains(out, "Job "+strconv.FormatInt(done, 10)+" (run2) later completed.") {
		t.Fatalf("expected the resolution:\n%s", out)
	}
}
