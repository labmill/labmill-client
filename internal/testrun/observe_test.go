package testrun

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"labmill-client/internal/agent"
	"labmill-client/internal/store"
	"labmill-client/internal/testcase"
)

func openStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "labmill.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func addJob(t *testing.T, st *store.Store, j store.Job) {
	t.Helper()
	if _, err := st.CreateJob(context.Background(), j); err != nil {
		t.Fatal(err)
	}
}

func TestObserve(t *testing.T) {
	ctx := context.Background()
	d := &Driver{}
	want := testcase.Case{Expect: testcase.Expect{Outcome: "failed", FailureClass: "submit_rejected"}}

	// A draft row without a scheduler id is not a job.
	st := openStore(t)
	addJob(t, st, store.Job{Name: "a", Cluster: "c", Backend: "slurm", State: store.StateDraft})
	o, err := d.observe(ctx, st, want, t.TempDir(), "sbatch: invalid partition", nil, &Observation{CheckExit: -1})
	if err != nil || o.SubmitErr == "" || o.JobState != "" || Class(o) != "submit_rejected" {
		t.Errorf("rejected: %+v %v", o, err)
	}

	// Turn error and no job: a runner error, not a verdict.
	if _, err := d.observe(ctx, st, want, t.TempDir(), "", errors.New("401"), &Observation{}); err == nil {
		t.Error("want runner error")
	}

	// Rejected first, then a second job that completed.
	st2 := openStore(t)
	addJob(t, st2, store.Job{Name: "a", Cluster: "c", Backend: "slurm", RemoteID: "7", State: store.StateFetched})
	o, err = d.observe(ctx, st2, want, t.TempDir(), "sbatch: bad time", nil, &Observation{CheckExit: -1})
	if err != nil || Class(o) != "submit_rejected" || !o.ResubmitCompleted {
		t.Errorf("fixed: %+v %v", o, err)
	}

	// A failed job maps to FAILED; a second job completes.
	st3 := openStore(t)
	addJob(t, st3, store.Job{Name: "a", Cluster: "c", Backend: "slurm", RemoteID: "1", State: store.StateFailed, Message: "TIMEOUT"})
	addJob(t, st3, store.Job{Name: "b", Cluster: "c", Backend: "slurm", RemoteID: "2", State: store.StateCompleted})
	o, _ = d.observe(ctx, st3, want, t.TempDir(), "", nil, &Observation{CheckExit: -1})
	if o.JobState != "TIMEOUT" || !o.ResubmitCompleted {
		t.Errorf("failed+resubmit: %+v", o)
	}
	if n := d.endedJobs(ctx, st3); n != 2 {
		t.Errorf("ended = %d", n)
	}

	// A completed job whose results never came back fails the check.
	st4 := openStore(t)
	addJob(t, st4, store.Job{Name: "a", Cluster: "c", Backend: "slurm", RemoteID: "1", State: store.StateCompleted})
	ok := testcase.Case{Expect: testcase.Expect{Outcome: "completed"}}
	o, _ = d.observe(ctx, st4, ok, t.TempDir(), "", nil, &Observation{CheckExit: -1})
	if o.JobState != "COMPLETED" || o.CheckExit != 1 {
		t.Errorf("not downloaded: %+v", o)
	}
}

func TestScriptedUIEvents(t *testing.T) {
	ev := func(u *scriptedUI, k agent.EventKind, tool, text string, isErr bool) {
		u.Event(agent.Event{Kind: k, Tool: tool, Text: text, IsError: isErr})
	}
	u := &scriptedUI{}
	ev(u, agent.EventToolResult, "submit_job", "bigcluster didn't accept the job: bad", true)
	ev(u, agent.EventToolResult, "submit_job", "already submitted", true) // not a refusal
	ev(u, agent.EventToolResult, "submit_job", "ok", false)
	ev(u, agent.EventToolResult, "submit_job", "bigcluster didn't accept the job: later", true) // after an accepted job
	ev(u, agent.EventToolCall, "ask_user", "", false)
	ev(u, agent.EventText, "", "done", false)
	final, asked, _, sub, _ := u.snapshot()
	if sub != "bigcluster didn't accept the job: bad" || asked != 1 || final != "done" {
		t.Errorf("final %q asked %d sub %q", final, asked, sub)
	}
	if _, err := u.Choose(context.Background(), agent.Choice{}); err != nil {
		t.Fatal(err)
	}
	if _, a, _, _, _ := u.snapshot(); a != 1 {
		t.Error("dialogs other than the agent's questions must not count")
	}
}
