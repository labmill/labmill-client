package app

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"labmill-client/internal/backend"
	"labmill-client/internal/store"
)

func claimJSON(t *testing.T, p claimPayload) string {
	t.Helper()
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// A caller holding an old snapshot (another caller already resubmitted)
// submits nothing; neither does one for a job the user canceled meanwhile.
func TestResubmitStaleSnapshot(t *testing.T) {
	f := newResubFixture(t, preemptPlan())
	stale := f.job(t)
	be := &resubBackend{}
	if !f.rs.Handle(context.Background(), stale, be, failed("PREEMPTED", 0)) || len(be.submitted) != 1 {
		t.Fatalf("first call: submits = %d", len(be.submitted))
	}
	if !f.rs.Handle(context.Background(), stale, be, failed("PREEMPTED", 0)) || len(be.submitted) != 1 {
		t.Errorf("stale snapshot submitted again: %d", len(be.submitted))
	}
	g := newResubFixture(t, preemptPlan())
	old := g.job(t)
	if err := g.db.UpdateJob(context.Background(), g.jobID, func(j *store.Job) error { j.State = store.StateCanceled; return nil }); err != nil {
		t.Fatal(err)
	}
	be2 := &resubBackend{}
	g.rs.Handle(context.Background(), old, be2, failed("PREEMPTED", 0))
	if len(be2.submitted) != 0 {
		t.Error("canceled job resubmitted")
	}
}

// Two Labmill processes (two Resubmitters, one database) submit once.
func TestResubmitTwoProcessesOneSubmit(t *testing.T) {
	f := newResubFixture(t, preemptPlan())
	be := &resubBackend{}
	other := &Resubmitter{Store: f.db}
	j := f.job(t)
	var wg sync.WaitGroup
	for _, rs := range []*Resubmitter{f.rs, other, f.rs, other} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rs.Handle(context.Background(), j, be, failed("PREEMPTED", 0))
		}()
	}
	wg.Wait()
	if len(be.submitted) != 1 {
		t.Fatalf("submits = %d, want 1", len(be.submitted))
	}
	if f.job(t).RemoteID != "201" {
		t.Errorf("job = %+v", f.job(t))
	}
	evs, _ := f.db.Events(context.Background(), f.jobID, 0)
	n := 0
	for _, e := range evs {
		if e.Kind == eventResubmitted {
			n++
		}
	}
	if n != 1 {
		t.Errorf("resubmitted events = %d, want 1", n)
	}
}

// The context ends right after sbatch succeeded: the new ID is still saved,
// and a restart does not submit again.
func TestResubmitCancelledAfterSubmitIsRecorded(t *testing.T) {
	f := newResubFixture(t, preemptPlan())
	ctx, cancel := context.WithCancel(context.Background())
	be := &resubBackend{onSubmit: cancel}
	before := f.job(t)
	f.rs.Handle(ctx, before, be, failed("PREEMPTED", 0))
	if got := f.job(t); got.RemoteID != "201" {
		t.Fatalf("new ID not saved: %+v", got)
	}
	restarted := &Resubmitter{Store: f.db}
	restarted.Handle(context.Background(), before, be, failed("PREEMPTED", 0))
	if len(be.submitted) != 1 {
		t.Errorf("submits = %d after restart", len(be.submitted))
	}
}

// A claim left open by a crash: the job is looked up by name; found means
// recorded, not found means one message and no new submit.
func TestResubmitCrashRecovery(t *testing.T) {
	for _, tt := range []struct {
		name   string
		queued func() []backend.QueuedJob
		want   string
	}{
		{"found", func() []backend.QueuedJob {
			return []backend.QueuedJob{{ID: "150", Name: "train", WorkDir: "/w", Submitted: time.Now()}, {ID: "205", Name: "train", WorkDir: "/w/", Submitted: time.Now()}}
		}, "205"},
		{"not found", nil, "200"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := newResubFixture(t, preemptPlan())
			f.rs.Now = func() time.Time { return time.Now().Add(time.Hour) }
			p := claimPayload{Event: resubmitEvent{Attempt: 2, Old: "200", Used: 1}, Msg: "resubmitted (attempt 2 of 3)"}
			if ok, err := f.db.ClaimResubmit(context.Background(), f.jobID, "200", claimJSON(t, p)); err != nil || !ok {
				t.Fatalf("claim: %v %v", ok, err)
			}
			be := &resubBackend{}
			if tt.queued != nil {
				be.queued = tt.queued()
			}
			before := f.job(t)
			f.rs.Handle(context.Background(), before, be, failed("PREEMPTED", 0))
			f.rs.Handle(context.Background(), before, be, failed("PREEMPTED", 0)) // a repeat check
			if len(be.submitted) != 0 {
				t.Errorf("submitted %d times during recovery", len(be.submitted))
			}
			if got := f.job(t); got.RemoteID != tt.want {
				t.Errorf("RemoteID = %s, want %s", got.RemoteID, tt.want)
			}
			if tt.want == "200" && len(f.notes) != 1 {
				t.Errorf("notes = %q, want one", f.notes)
			}
		})
	}
	t.Run("fresh claim belongs to another process", func(t *testing.T) {
		f := newResubFixture(t, preemptPlan())
		if ok, _ := f.db.ClaimResubmit(context.Background(), f.jobID, "200", "{}"); !ok {
			t.Fatal("claim")
		}
		be := &resubBackend{queued: []backend.QueuedJob{{ID: "205", Name: "train", WorkDir: "/w", Submitted: time.Now()}}}
		if !f.rs.Handle(context.Background(), f.job(t), be, failed("PREEMPTED", 0)) || f.job(t).RemoteID != "200" || len(f.notes) != 0 {
			t.Errorf("touched a fresh claim: %+v %q", f.job(t), f.notes)
		}
	})
}

// A submit whose save failed is saved later and never submitted twice.
func TestResubmitUnsavedIsRetriedNotResubmitted(t *testing.T) {
	f := newResubFixture(t, preemptPlan())
	p := claimPayload{Event: resubmitEvent{Attempt: 2, Old: "200", Used: 1}, Msg: "resubmitted (attempt 2 of 3)"}
	if ok, _ := f.db.ClaimResubmit(context.Background(), f.jobID, "200", claimJSON(t, p)); !ok {
		t.Fatal("claim")
	}
	f.rs.unsaved = map[string]*unsavedSave{"1:200": {newID: "300", announce: "Job #1 (train) was preempted on preempt; resubmitted (attempt 2 of 3)", p: p}}
	be := &resubBackend{}
	before := f.job(t)
	for i := 0; i < 2; i++ {
		if !f.rs.Handle(context.Background(), before, be, failed("PREEMPTED", 0)) || len(be.submitted) != 0 {
			t.Fatal("handled wrongly")
		}
	}
	if f.job(t).RemoteID != "300" || len(f.notes) != 1 {
		t.Errorf("job = %+v notes = %q", f.job(t), f.notes)
	}
}

// A job Slurm requeues itself past the limit (still pending or running)
// moves to the fallback: the requeued copy is cancelled, one submit.
func TestResubmitLiveRequeueLoopGoesToFallback(t *testing.T) {
	for _, state := range []store.JobState{store.StatePending, store.StateRunning} {
		t.Run(string(state), func(t *testing.T) {
			st := backend.Status{State: state, Reason: "Resources"}
			f := newResubFixture(t, preemptPlan())
			be := &resubBackend{restarts: 3}
			if state == store.StateRunning {
				if f.rs.Handle(context.Background(), f.job(t), be, st) || len(be.submitted) != 0 || len(be.canceled) != 0 {
					t.Error("acted on a running copy")
				}
				return
			}
			if !f.rs.Handle(context.Background(), f.job(t), be, st) {
				t.Fatalf("not handled; notes %q", f.notes)
			}
			if len(be.submitted) != 1 || len(be.canceled) != 1 || be.canceled[0] != "200" {
				t.Fatalf("submits = %d, canceled = %v", len(be.submitted), be.canceled)
			}
			if !strings.Contains(be.submitted[0], "--partition=normal") || strings.Contains(be.submitted[0], "--qos") {
				t.Errorf("not the fallback script:\n%s", be.submitted[0])
			}
			// Below the limit, or a plan without --requeue: nothing happens.
			g := newResubFixture(t, preemptPlan())
			be2 := &resubBackend{restarts: 1}
			if g.rs.Handle(context.Background(), g.job(t), be2, st) || len(be2.submitted) != 0 {
				t.Error("resubmitted below the limit")
			}
			np := preemptPlan()
			np.Requeue = false
			h := newResubFixture(t, np)
			be3 := &resubBackend{restarts: 5}
			if h.rs.Handle(context.Background(), h.job(t), be3, st) || len(be3.submitted) != 0 {
				t.Error("acted on a plan without --requeue")
			}
		})
	}
}

func TestPreemptionCauseSignals(t *testing.T) {
	tests := []struct {
		st       backend.Status
		restarts int
		want     bool
	}{
		{failed("BOOT_FAIL", 0), 0, true},
		{backend.Status{State: store.StateFailed, Reason: "FAILED", Signal: 9}, 1, false},
		{backend.Status{State: store.StateFailed, Reason: "FAILED", Signal: 11}, 2, false},
		{failed("FAILED", 0), 1, true},
	}
	for _, tt := range tests {
		if got := preemptionCause(tt.st, tt.restarts) != ""; got != tt.want {
			t.Errorf("%+v restarts %d: got %v, want %v", tt.st, tt.restarts, got, tt.want)
		}
	}
}

// sbatch's answer is lost (the connection dropped): the claim stays open, a
// later check submits nothing, and after the claim ages the queue is searched.
func TestResubmitUncertainSubmitNeverRepeats(t *testing.T) {
	f := newResubFixture(t, preemptPlan())
	be := &resubBackend{uncertain: true}
	before := f.job(t)
	for i := 0; i < 3; i++ {
		if !f.rs.Handle(context.Background(), before, be, failed("PREEMPTED", 0)) {
			t.Fatalf("check %d not handled", i)
		}
	}
	if len(be.submitted) != 0 || f.job(t).RemoteID != "200" || len(f.notes) != 1 || !strings.Contains(f.notes[0], "can't tell whether") {
		t.Fatalf("submits %d, job %+v, notes %q", len(be.submitted), f.job(t), f.notes)
	}
	// Long after: the job did reach the cluster and is adopted.
	f.rs.Now = func() time.Time { return time.Now().Add(time.Hour) }
	be.queued = []backend.QueuedJob{{ID: "210", Name: "train", WorkDir: "/w", Submitted: time.Now()}}
	f.rs.Handle(context.Background(), before, be, failed("PREEMPTED", 0))
	if len(be.submitted) != 0 || f.job(t).RemoteID != "210" {
		t.Errorf("not adopted: submits %d, job %+v", len(be.submitted), f.job(t))
	}
}

// Another process adopted the job while this one's slow sbatch was running:
// this one's save must not cancel what the other recorded.
func TestResubmitSaveAfterAdoptionNeverCancels(t *testing.T) {
	for _, otherID := range []string{"201", "205"} {
		t.Run("other recorded "+otherID, func(t *testing.T) {
			f := newResubFixture(t, preemptPlan())
			be := &resubBackend{}
			be.onSubmit = func() { // runs while this Submit is in flight
				if err := f.db.FinishResubmit(context.Background(), f.jobID, "200", otherID, "", "m", "resubmitted", "{}"); err != nil {
					t.Error(err)
				}
			}
			f.rs.Handle(context.Background(), f.job(t), be, failed("PREEMPTED", 0))
			if len(be.canceled) != 0 {
				t.Errorf("canceled %v", be.canceled)
			}
			if got := f.job(t).RemoteID; got != otherID {
				t.Errorf("RemoteID = %s, want %s", got, otherID)
			}
		})
	}
}

// Only the crashed resubmission's own job is adopted.
func TestResubmitAdoptionIsStrict(t *testing.T) {
	now := time.Now()
	tests := []struct {
		name string
		q    backend.QueuedJob
		take bool // another Labmill job already has this ID
	}{
		{"another folder", backend.QueuedJob{ID: "205", Name: "train", WorkDir: "/other", Submitted: now}, false},
		{"another name", backend.QueuedJob{ID: "205", Name: "other", WorkDir: "/w", Submitted: now}, false},
		{"older than the claim", backend.QueuedJob{ID: "205", Name: "train", WorkDir: "/w", Submitted: now.Add(-time.Hour)}, false},
		{"no submit time", backend.QueuedJob{ID: "205", Name: "train", WorkDir: "/w"}, false},
		{"lower ID", backend.QueuedJob{ID: "150", Name: "train", WorkDir: "/w", Submitted: now}, false},
		{"recorded by another job", backend.QueuedJob{ID: "205", Name: "train", WorkDir: "/w", Submitted: now}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newResubFixture(t, preemptPlan())
			f.rs.Now = func() time.Time { return now.Add(time.Hour) }
			if tt.take {
				if _, err := f.db.CreateJob(context.Background(), store.Job{Name: "train", Cluster: "hpcone", Backend: "slurm", RemoteID: "205", State: store.StateRunning, RemoteDir: "/w"}); err != nil {
					t.Fatal(err)
				}
			}
			p := claimPayload{Event: resubmitEvent{Attempt: 2, Old: "200", Used: 1}, Msg: "m"}
			if ok, _ := f.db.ClaimResubmit(context.Background(), f.jobID, "200", claimJSON(t, p)); !ok {
				t.Fatal("claim")
			}
			be := &resubBackend{queued: []backend.QueuedJob{tt.q}}
			f.rs.Handle(context.Background(), f.job(t), be, failed("PREEMPTED", 0))
			if got := f.job(t).RemoteID; got != "200" {
				t.Errorf("adopted %s", got)
			}
			if len(be.submitted) != 0 || len(be.canceled) != 0 {
				t.Errorf("backend used: %+v", be)
			}
		})
	}
}

// A job whose pending requeue copy was cancelled by Labmill and whose sbatch
// answer was lost: the cancelled status of the claimed ID is handled, the job
// is not marked canceled and nothing is submitted.
func TestResubmitCancelledStatusOfClaimedID(t *testing.T) {
	cancelled := backend.Status{State: store.StateCanceled, Reason: "CANCELLED"}
	t.Run("uncertain submit", func(t *testing.T) {
		f := newResubFixture(t, preemptPlan())
		be := &resubBackend{restarts: 3, uncertain: true}
		before := f.job(t)
		if !f.rs.Handle(context.Background(), before, be, backend.Status{State: store.StatePending, Reason: "Resources"}) {
			t.Fatal("requeue-loop fallback not handled")
		}
		if !f.rs.Handle(context.Background(), f.job(t), be, cancelled) {
			t.Error("cancelled status of the claimed ID not handled")
		}
		if j := f.job(t); j.State == store.StateCanceled || j.RemoteID != "200" || len(be.submitted) != 0 {
			t.Errorf("job = %+v, submits %d", j, len(be.submitted))
		}
	})
	t.Run("failed save", func(t *testing.T) {
		f := newResubFixture(t, preemptPlan())
		p := claimPayload{Event: resubmitEvent{Fallback: true, Old: "200", Used: 3}, Msg: "moved"}
		if ok, _ := f.db.ClaimResubmit(context.Background(), f.jobID, "200", claimJSON(t, p)); !ok {
			t.Fatal("claim")
		}
		f.rs.unsaved = map[string]*unsavedSave{"1:200": {newID: "300", announce: "Job #1 moved", p: p}}
		be := &resubBackend{}
		if !f.rs.Handle(context.Background(), f.job(t), be, cancelled) {
			t.Fatal("not handled")
		}
		if j := f.job(t); j.RemoteID != "300" || j.State == store.StateCanceled || len(be.submitted) != 0 {
			t.Errorf("job = %+v", j)
		}
	})
}

// After the queue shows nothing, the job history (a job that already ended)
// is searched before the resubmission is reported lost.
func TestResubmitAdoptsFromHistory(t *testing.T) {
	f := newResubFixture(t, preemptPlan())
	f.rs.Now = func() time.Time { return time.Now().Add(time.Hour) }
	p := claimPayload{Event: resubmitEvent{Attempt: 2, Old: "200", Used: 1}, Msg: "m"}
	if ok, _ := f.db.ClaimResubmit(context.Background(), f.jobID, "200", claimJSON(t, p)); !ok {
		t.Fatal("claim")
	}
	be := &resubBackend{history: []backend.QueuedJob{{ID: "230", Name: "train", WorkDir: "/w", Submitted: time.Now()}}}
	f.rs.Handle(context.Background(), f.job(t), be, failed("PREEMPTED", 0))
	if f.job(t).RemoteID != "230" || len(be.submitted) != 0 {
		t.Errorf("job = %+v, submits %d", f.job(t), len(be.submitted))
	}
}
