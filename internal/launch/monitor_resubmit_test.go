package launch

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"labmill-client/internal/app"
	"labmill-client/internal/backend"
	"labmill-client/internal/config"
	"labmill-client/internal/store"
)

// resubMonBackend adds the Slurm calls a resubmission makes to statusBackend.
type resubMonBackend struct {
	*statusBackend
	submitErr error
	submits   int
	canceled  []string
}

func (b *resubMonBackend) Submit(context.Context, string, backend.JobSpec) (string, error) {
	if b.submitErr != nil {
		return "", b.submitErr
	}
	b.submits++
	return "201", nil
}

func (b *resubMonBackend) Cancel(_ context.Context, id string) error {
	b.canceled = append(b.canceled, id)
	return nil
}

const monPlan = `{"preemptible":true,"partition":"preempt","fallback_partition":"normal","max_resubmits":2}`

func newResubMonitor(t *testing.T, withPlan bool) (*monitorFixture, *resubMonBackend, int64) {
	t.Helper()
	f := newMonitorFixture(t)
	rb := &resubMonBackend{statusBackend: f.be}
	f.m.backend = func(context.Context, config.Cluster) (backend.Backend, bool) { return rb, true }
	rs := &app.Resubmitter{Store: f.db, Notify: f.notices.notify}
	f.m.resubmit = rs.Handle
	id := f.job(t, store.Job{Name: "train", Cluster: "hpcone", Backend: "slurm", RemoteID: "200", State: store.StateRunning,
		Script: "#!/bin/bash\n#SBATCH --partition=preempt\necho hi\n", RemoteDir: "/w"})
	if withPlan {
		if err := f.db.AddEvent(context.Background(), id, "plan", monPlan); err != nil {
			t.Fatal(err)
		}
	}
	return f, rb, id
}

func TestMonitorResubmitsPreemptedJobOnce(t *testing.T) {
	f, rb, id := newResubMonitor(t, true)
	var mu sync.Mutex
	var fetches []int64
	f.m.autoOn = func() bool { return true }
	f.m.autoFetch = func(_ context.Context, jid int64) app.AutoFetchResult {
		mu.Lock()
		fetches = append(fetches, jid)
		mu.Unlock()
		return app.AutoFetchResult{Text: "downloaded", Downloaded: true}
	}
	f.m.fetched = func(_ int64, text string) { f.notices.notify(text) }
	ctx := context.Background()

	f.be.set("200", backend.Status{State: store.StateFailed, Reason: "PREEMPTED"})
	f.be.set("201", backend.Status{State: store.StateRunning})
	f.m.check(ctx)
	f.m.fetching.Wait()
	j := f.state(t, id)
	if j.RemoteID != "201" || j.State != store.StateSubmitted || rb.submits != 1 {
		t.Fatalf("job = %+v, submits = %d", j, rb.submits)
	}
	got := f.notices.all()
	if len(got) != 1 || !strings.Contains(got[0], "Job #1 (train) was preempted on preempt; resubmitted (attempt 2 of 3), new Slurm job 201.") {
		t.Fatalf("notices = %q", got)
	}
	if len(fetches) != 0 {
		t.Fatalf("automatic download started on an intermediate attempt: %v", fetches)
	}

	// The monitor now watches the new Slurm job; its end is the final one.
	f.be.set("201", backend.Status{State: store.StateCompleted})
	f.m.check(ctx)
	f.m.fetching.Wait()
	if f.be.count("201") != 1 || f.state(t, id).State != store.StateCompleted {
		t.Errorf("new job not watched: %+v", f.state(t, id))
	}
	if len(fetches) != 1 || fetches[0] != id {
		t.Errorf("fetches = %v, want one at the final end", fetches)
	}
}

func TestMonitorNoResubmitCases(t *testing.T) {
	tests := []struct {
		name     string
		withPlan bool
		st       backend.Status
	}{
		{"no plan event", false, backend.Status{State: store.StateFailed, Reason: "PREEMPTED"}},
		{"timeout", true, backend.Status{State: store.StateFailed, Reason: "TIMEOUT"}},
		{"out of memory", true, backend.Status{State: store.StateFailed, Reason: "OUT_OF_MEMORY"}},
		{"program failure", true, backend.Status{State: store.StateFailed, Reason: "FAILED", ExitCode: 2}},
		{"user cancel", true, backend.Status{State: store.StateCanceled, Reason: "CANCELLED"}},
		{"completed", true, backend.Status{State: store.StateCompleted}},
		{"requeued and pending", true, backend.Status{State: store.StatePending, Reason: "BeginTime"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f, rb, id := newResubMonitor(t, tt.withPlan)
			f.be.set("200", tt.st)
			f.m.check(context.Background())
			if rb.submits != 0 || len(rb.canceled) != 0 {
				t.Errorf("submits = %d, canceled = %v", rb.submits, rb.canceled)
			}
			if j := f.state(t, id); j.RemoteID != "200" || j.State != tt.st.State {
				t.Errorf("job = %+v", j)
			}
		})
	}
}

// With no usable connection the resubmission waits and the user hears of it
// once; the job is not marked failed meanwhile.
func TestMonitorResubmitDeferredOnce(t *testing.T) {
	f, rb, id := newResubMonitor(t, true)
	rb.submitErr = errors.New("not signed in")
	f.be.set("200", backend.Status{State: store.StateFailed, Reason: "NODE_FAIL"})
	ctx := context.Background()
	f.m.check(ctx)
	f.m.check(ctx)
	if got := f.notices.all(); len(got) != 1 || !strings.Contains(got[0], "couldn't resubmit it now (not signed in)") {
		t.Fatalf("notices = %q", got)
	}
	if j := f.state(t, id); j.State != store.StateRunning || j.RemoteID != "200" {
		t.Errorf("job = %+v", j)
	}
	rb.submitErr = nil
	f.m.check(ctx)
	if j := f.state(t, id); j.RemoteID != "201" {
		t.Errorf("not resubmitted when the connection returned: %+v", j)
	}
}
