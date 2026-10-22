package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"labmill-client/internal/backend"
	"labmill-client/internal/store"
)

// resubBackend fakes the Slurm calls Handle makes.
type resubBackend struct {
	backend.Backend
	restarts  int
	submitErr error
	submitted []string // scripts, in order
	canceled  []string
	next      int
	mu        sync.Mutex
	onSubmit  func()              // runs inside Submit, before it returns
	queued    []backend.QueuedJob // what QueuedJobs finds
	history   []backend.QueuedJob // what JobsSince finds
	uncertain bool                // the first Submit fails with ErrSubmitUncertain
}

func (b *resubBackend) Restarts(context.Context, string) (int, error) { return b.restarts, nil }
func (b *resubBackend) Cancel(_ context.Context, id string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.canceled = append(b.canceled, id)
	return nil
}
func (b *resubBackend) Submit(_ context.Context, script string, _ backend.JobSpec) (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.submitErr != nil {
		return "", b.submitErr
	}
	if b.uncertain {
		b.uncertain = false
		return "", fmt.Errorf("sbatch: %w: connection reset", backend.ErrSubmitUncertain)
	}
	if b.onSubmit != nil {
		b.onSubmit()
	}
	b.submitted = append(b.submitted, script)
	b.next++
	return []string{"201", "202", "203", "204"}[b.next-1], nil
}
func (b *resubBackend) JobsSince(context.Context, time.Time) ([]backend.QueuedJob, error) {
	return b.history, nil
}
func (b *resubBackend) QueuedJobs(context.Context) ([]backend.QueuedJob, error) { return b.queued, nil }

const preemptScript = "#!/bin/bash\n#SBATCH --job-name=t\n#SBATCH --partition=preempt\n#SBATCH --qos=low\n#SBATCH --requeue\n#SBATCH --chdir=/w\necho partition=preempt\n"

type resubFixture struct {
	db     *store.Store
	rs     *Resubmitter
	notes  []string
	logs   []string
	jobID  int64
	remote string
}

func newResubFixture(t *testing.T, plan *placementPlan, prior ...resubmitEvent) *resubFixture {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "y.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	f := &resubFixture{db: db, remote: "200"}
	f.rs = &Resubmitter{Store: db, Notify: func(s string) { f.notes = append(f.notes, s) }, Log: func(s string) { f.logs = append(f.logs, s) }}
	ctx := context.Background()
	f.jobID, err = db.CreateJob(ctx, store.Job{Name: "train", Cluster: "hpcone", Backend: "slurm", RemoteID: "200",
		State: store.StateRunning, Script: preemptScript, RemoteDir: "/w"})
	if err != nil {
		t.Fatal(err)
	}
	spec, _ := json.Marshal(backend.JobSpec{Name: "train", WorkDir: "/w"})
	if err := db.AddEvent(ctx, f.jobID, eventSpec, string(spec)); err != nil {
		t.Fatal(err)
	}
	if plan != nil {
		pj, _ := json.Marshal(plan)
		if err := db.AddEvent(ctx, f.jobID, eventPlan, string(pj)); err != nil {
			t.Fatal(err)
		}
	}
	for _, p := range prior {
		pj, _ := json.Marshal(p)
		if err := db.AddEvent(ctx, f.jobID, eventResubmitted, string(pj)); err != nil {
			t.Fatal(err)
		}
	}
	return f
}

func (f *resubFixture) job(t *testing.T) store.Job {
	t.Helper()
	j, err := f.db.Job(context.Background(), f.jobID)
	if err != nil {
		t.Fatal(err)
	}
	return j
}

func preemptPlan() *placementPlan {
	return &placementPlan{Preemptible: true, Partition: "preempt", QOS: "low", Requeue: true,
		FallbackPartition: "normal", MaxResubmits: 2}
}

func failed(reason string, exit int) backend.Status {
	return backend.Status{State: store.StateFailed, Reason: reason, ExitCode: exit}
}

func TestResubmitDecisions(t *testing.T) {
	noFallback := preemptPlan()
	noFallback.FallbackPartition, noFallback.FallbackNeedsPartition = "", true
	notPreemptible := &placementPlan{Partition: "normal"}
	tests := []struct {
		name       string
		plan       *placementPlan
		prior      []resubmitEvent
		st         backend.Status
		restarts   int
		submitErr  error
		wantHandle bool
		wantSubmit int
		wantCancel int
		wantNote   string
		wantRemote string
		wantFall   bool // script is the fallback one
	}{
		{name: "preempted", plan: preemptPlan(), st: failed("PREEMPTED", 0), wantHandle: true, wantSubmit: 1, wantRemote: "201",
			wantNote: "Job #1 (train) was preempted on preempt; resubmitted (attempt 2 of 3), new Slurm job 201."},
		{name: "node fail", plan: preemptPlan(), st: failed("NODE_FAIL", 0), wantHandle: true, wantSubmit: 1, wantRemote: "201",
			wantNote: "lost its node on preempt; resubmitted (attempt 2 of 3)"},
		{name: "second resubmission", plan: preemptPlan(), prior: []resubmitEvent{{Attempt: 2, Old: "199", New: "200", Used: 1}},
			st: failed("PREEMPTED", 0), wantHandle: true, wantSubmit: 1, wantRemote: "201", wantNote: "attempt 3 of 3"},
		{name: "third failure goes to the fallback", plan: preemptPlan(), prior: []resubmitEvent{{Attempt: 3, Old: "199", New: "200", Used: 2}},
			st: failed("PREEMPTED", 0), wantHandle: true, wantSubmit: 1, wantCancel: 1, wantRemote: "201", wantFall: true,
			wantNote: "for the 3rd time; moving to normal as planned"},
		{name: "slurm requeues count as attempts", plan: preemptPlan(), restarts: 2,
			st: failed("PREEMPTED", 0), wantHandle: true, wantSubmit: 1, wantCancel: 1, wantRemote: "201", wantFall: true,
			wantNote: "moving to normal as planned"},
		{name: "requeue loop ended failed", plan: preemptPlan(), restarts: 1,
			st: failed("FAILED", 0), wantHandle: true, wantSubmit: 1, wantRemote: "201", wantNote: "requeued 1 time(s) by Slurm and then failed"},
		{name: "fallback needs a partition", plan: noFallback, prior: []resubmitEvent{{Used: 2}},
			st: failed("PREEMPTED", 0), wantNote: "no fallback partition"},
		{name: "after the fallback nothing more", plan: preemptPlan(), prior: []resubmitEvent{{Used: 3, Fallback: true}},
			st: failed("PREEMPTED", 0)},
		{name: "program failure", plan: preemptPlan(), restarts: 1, st: failed("FAILED", 1)},
		{name: "failed without a requeue", plan: preemptPlan(), st: failed("FAILED", 0)},
		{name: "timeout", plan: preemptPlan(), st: failed("TIMEOUT", 0)},
		{name: "out of memory", plan: preemptPlan(), st: failed("OUT_OF_MEMORY", 0)},
		{name: "completed", plan: preemptPlan(), st: backend.Status{State: store.StateCompleted}},
		{name: "canceled by the user", plan: preemptPlan(), st: backend.Status{State: store.StateCanceled, Reason: "CANCELLED"}},
		{name: "still pending after a Slurm requeue", plan: preemptPlan(), st: backend.Status{State: store.StatePending, Reason: "BeginTime"}},
		{name: "no plan event", plan: nil, st: failed("PREEMPTED", 0)},
		{name: "plan not preemptible", plan: notPreemptible, st: failed("PREEMPTED", 0)},
		{name: "sbatch fails: deferred once", plan: preemptPlan(), st: failed("PREEMPTED", 0), submitErr: errors.New("connection lost"),
			wantHandle: true, wantNote: "couldn't resubmit it now (connection lost)"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newResubFixture(t, tt.plan, tt.prior...)
			be := &resubBackend{restarts: tt.restarts, submitErr: tt.submitErr}
			j := f.job(t)
			got := f.rs.Handle(context.Background(), j, be, tt.st)
			if got != tt.wantHandle {
				t.Fatalf("Handle = %v, want %v (notes %q)", got, tt.wantHandle, f.notes)
			}
			if len(be.submitted) != tt.wantSubmit || len(be.canceled) != tt.wantCancel {
				t.Fatalf("submits = %d, cancels = %v; want %d, %d", len(be.submitted), be.canceled, tt.wantSubmit, tt.wantCancel)
			}
			if tt.wantCancel > 0 && be.canceled[0] != "200" {
				t.Errorf("canceled %v, want only job 200", be.canceled)
			}
			note := strings.Join(f.notes, "\n")
			if tt.wantNote == "" && len(f.notes) != 0 {
				t.Errorf("unexpected notes %q", f.notes)
			}
			if !strings.Contains(note, tt.wantNote) {
				t.Errorf("notes = %q, want %q", note, tt.wantNote)
			}
			if tt.wantNote != "" && len(f.logs) == 0 {
				t.Error("nothing logged")
			}
			after := f.job(t)
			if tt.wantRemote != "" {
				if after.RemoteID != tt.wantRemote || after.State != store.StateSubmitted {
					t.Errorf("job = %s %s, want %s submitted", after.RemoteID, after.State, tt.wantRemote)
				}
				evs, _ := f.db.Events(context.Background(), f.jobID, 0)
				var re resubmitEvent
				found := false
				for _, e := range evs {
					if e.Kind == eventResubmitted {
						found = json.Unmarshal([]byte(e.Text), &re) == nil && re.New == tt.wantRemote
					}
				}
				if !found || re.Old != "200" || re.Fallback != tt.wantFall {
					t.Errorf("resubmitted event = %+v (found %v)", re, found)
				}
				if tt.wantFall && after.Script != be.submitted[0] {
					t.Errorf("job script not the fallback one:\n%s", after.Script)
				}
				script := be.submitted[0]
				if tt.wantFall {
					for _, gone := range []string{"--qos", "--requeue", "--partition=preempt"} {
						if strings.Contains(script, gone) {
							t.Errorf("fallback script still has %q:\n%s", gone, script)
						}
					}
					if !strings.Contains(script, "#SBATCH --partition=normal\n") || !strings.Contains(script, "echo partition=preempt") {
						t.Errorf("fallback script wrong:\n%s", script)
					}
					if !strings.Contains(note, "removed #SBATCH --qos=low") || !strings.Contains(note, "-> #SBATCH --partition=normal") {
						t.Errorf("changes not shown: %q", note)
					}
				} else if script != preemptScript {
					t.Errorf("resubmitted script differs from the approved one:\n%s", script)
				}
			} else if after.RemoteID != "200" || after.State != store.StateRunning {
				t.Errorf("job changed: %s %s", after.RemoteID, after.State)
			}
		})
	}
}

func TestResubmitDeferredTellsOnce(t *testing.T) {
	f := newResubFixture(t, preemptPlan())
	be := &resubBackend{submitErr: errors.New("no connection")}
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		if !f.rs.Handle(ctx, f.job(t), be, failed("PREEMPTED", 0)) {
			t.Fatalf("try %d not deferred", i)
		}
	}
	if len(f.notes) != 1 {
		t.Fatalf("notes = %q, want one", f.notes)
	}
	// The third failed try gives up: the job ends failed for the caller.
	if f.rs.Handle(ctx, f.job(t), be, failed("PREEMPTED", 0)) {
		t.Error("third failed try still deferred")
	}
	if len(f.notes) != 2 || !strings.Contains(f.notes[1], "gave up") {
		t.Errorf("notes = %q", f.notes)
	}
	// The connection is back: the next check resubmits.
	f2 := newResubFixture(t, preemptPlan())
	be2 := &resubBackend{submitErr: errors.New("no connection")}
	f2.rs.Handle(ctx, f2.job(t), be2, failed("PREEMPTED", 0))
	be2.submitErr = nil
	if !f2.rs.Handle(ctx, f2.job(t), be2, failed("PREEMPTED", 0)) || f2.job(t).RemoteID != "201" {
		t.Errorf("not resubmitted after the connection returned: %+v", f2.job(t))
	}
}

func TestResubmitFallbackNeedsPartitionOnce(t *testing.T) {
	noFallback := preemptPlan()
	noFallback.FallbackPartition, noFallback.FallbackNeedsPartition = "", true
	f := newResubFixture(t, noFallback, resubmitEvent{Used: 2})
	be := &resubBackend{}
	if f.rs.Handle(context.Background(), f.job(t), be, failed("NODE_FAIL", 0)) {
		t.Error("handled although the job needs a partition")
	}
	if len(be.submitted) != 0 || len(be.canceled) != 0 {
		t.Errorf("backend used: %+v", be)
	}
}

func TestFallbackScript(t *testing.T) {
	tests := []struct {
		name, in, partition string
		wantHas, wantNot    []string
	}{
		{"replace", "#!/bin/bash\n#SBATCH --partition=a\n#SBATCH -n 2\nrun --partition=a\n", "b",
			[]string{"#SBATCH --partition=b\n", "#SBATCH -n 2", "run --partition=a"}, []string{"--partition=a\n#SBATCH"}},
		{"short form", "#!/bin/bash\n#SBATCH -p a\nrun\n", "b", []string{"#SBATCH --partition=b\n"}, []string{"-p a"}},
		{"remove for default", "#!/bin/bash\n#SBATCH --partition=a\n#SBATCH --qos=q\nrun\n", "", nil, []string{"partition", "qos"}},
		{"add when missing", "#!/bin/bash\n#SBATCH --qos q\nrun\n", "b", []string{"#!/bin/bash\n#SBATCH --partition=b\n"}, []string{"qos"}},
		{"comment and -q", "#!/bin/bash\n#SBATCH -p a # preemptible\n#SBATCH -q low\n#SBATCH --partition=a\nrun\n", "b", []string{"#SBATCH --partition=b\n"}, []string{"-q low", "partition=a", "-p a"}},
		{"body left alone", "#!/bin/bash\n#SBATCH --requeue\necho hi\n#SBATCH --qos=q\n", "b", []string{"echo hi\n#SBATCH --qos=q"}, []string{"--requeue"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, changes := fallbackScript(tt.in, tt.partition)
			for _, w := range tt.wantHas {
				if !strings.Contains(got, w) {
					t.Errorf("missing %q in:\n%s", w, got)
				}
			}
			for _, w := range tt.wantNot {
				if strings.Contains(got, w) {
					t.Errorf("unexpected %q in:\n%s", w, got)
				}
			}
			if len(changes) == 0 {
				t.Error("no changes reported")
			}
		})
	}
}
