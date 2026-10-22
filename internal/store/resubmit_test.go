package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

func TestResubmitClaimProtocol(t *testing.T) {
	ctx := context.Background()
	s, err := Open(filepath.Join(t.TempDir(), "y.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	id, err := s.CreateJob(ctx, Job{Name: "j", Backend: "slurm", RemoteID: "10", State: StateRunning})
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := s.ClaimResubmit(ctx, id, "99", "x"); err != nil || ok {
		t.Errorf("claim of another ID = %v, %v", ok, err)
	}
	if ok, err := s.ClaimResubmit(ctx, id, "10", "{}"); err != nil || !ok {
		t.Fatalf("first claim = %v, %v", ok, err)
	}
	if ok, _ := s.ClaimResubmit(ctx, id, "10", "{}"); ok {
		t.Error("second claim succeeded")
	}
	evs, _ := s.Events(ctx, id, 0)
	if c, open := OpenResubmitClaim(evs); !open || c.Old != "10" || c.Payload != "{}" {
		t.Errorf("open claim = %+v %v", c, open)
	}
	// Released: claimable again.
	if err := s.ReleaseResubmit(ctx, id, "10"); err != nil {
		t.Fatal(err)
	}
	if ok, _ := s.ClaimResubmit(ctx, id, "10", "{}"); !ok {
		t.Fatal("claim after release failed")
	}
	if err := s.FinishResubmit(ctx, id, "10", "11", "script2", "msg", "resubmitted", "{}"); err != nil {
		t.Fatal(err)
	}
	j, _ := s.Job(ctx, id)
	if j.RemoteID != "11" || j.State != StateSubmitted || j.Script != "script2" || j.Message != "msg" {
		t.Errorf("job = %+v", j)
	}
	evs, _ = s.Events(ctx, id, 0)
	if _, open := OpenResubmitClaim(evs); open {
		t.Error("claim still open after finish")
	}
	// The old ID is gone: finishing again changes nothing.
	if err := s.FinishResubmit(ctx, id, "10", "12", "", "m", "resubmitted", "{}"); !errors.Is(err, ErrJobChanged) {
		t.Errorf("second finish = %v, want ErrJobChanged", err)
	}
	// A canceled job can be neither claimed nor finished.
	if err := s.UpdateJob(ctx, id, func(j *Job) error { j.State = StateCanceled; return nil }); err != nil {
		t.Fatal(err)
	}
	if ok, _ := s.ClaimResubmit(ctx, id, "11", "{}"); ok {
		t.Error("claimed a canceled job")
	}
}
