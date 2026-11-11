package launch

import (
	"context"
	"errors"
	"testing"

	"labmill-client/internal/account"
	"labmill-client/internal/backend"
	"labmill-client/internal/config"
	"labmill-client/internal/store"
)

// outBackend fails its first Output call.
type outBackend struct {
	backend.Backend
	calls int
}

func (b *outBackend) Output(context.Context, string, int, int) ([]string, []string, error) {
	b.calls++
	if b.calls == 1 {
		return nil, nil, errors.New("ssh timeout")
	}
	return []string{"hello"}, nil, nil
}

// An ended job whose first output read failed gets its output event on a
// later pass; a job that keeps failing records one problem.
func TestMonitorRetriesOutput(t *testing.T) {
	f := newMonitorFixture(t)
	ob := &outBackend{}
	f.m.backend = func(context.Context, config.Cluster) (backend.Backend, bool) { return ob, true }
	var probs int
	f.m.problem = func(store.Job, error) { probs++ }
	f.m.collect = func(ctx context.Context, j store.Job, be backend.Backend) error {
		return account.CollectOutput(ctx, f.db, be, j, account.NewRedactor())
	}
	id := f.job(t, store.Job{Name: "j", Cluster: "hpcone", RemoteID: "7", State: store.StateCompleted})
	f.m.check(context.Background())
	if n := outputEvents(f, id); n != 0 {
		t.Fatalf("output events after the failed read = %d", n)
	}
	f.m.check(context.Background())

	if n := outputEvents(f, id); n != 1 {
		t.Fatalf("output events = %d", n)
	}
	f.m.check(context.Background())
	if ob.calls != 2 || probs != 0 {
		t.Errorf("calls = %d, problems = %d", ob.calls, probs)
	}
	// A backend that always fails: one problem after outputMaxFailures reads.
	f.m.backend = func(context.Context, config.Cluster) (backend.Backend, bool) { return &alwaysFail{}, true }
	f.job(t, store.Job{Name: "k", Cluster: "hpcone", RemoteID: "8", State: store.StateFailed})
	for i := 0; i < 6; i++ {
		f.m.check(context.Background())
	}
	if probs != 1 {
		t.Errorf("problems = %d, want 1", probs)
	}
}

type alwaysFail struct{ backend.Backend }

func (alwaysFail) Output(context.Context, string, int, int) ([]string, []string, error) {
	return nil, nil, errors.New("down")
}

func outputEvents(f *monitorFixture, id int64) (n int) {
	evs, _ := f.db.Events(context.Background(), id, 0)
	for _, e := range evs {
		if e.Kind == account.EventOutput {
			n++
		}
	}
	return n
}
