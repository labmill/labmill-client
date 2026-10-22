package gui

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

func TestRunAction(t *testing.T) {
	rec, a, s := newStep()
	release := make(chan struct{})
	if err := a.RunAction("Check the sign-in on gpu", func(ctx context.Context) (string, error) {
		<-release
		return "Signed in.", nil
	}); err != nil {
		t.Fatal(err)
	}
	// While the action runs, another action is refused and a message queues.
	if err := a.RunAction("again", func(context.Context) (string, error) { return "", nil }); !errors.Is(err, ErrBusy) {
		t.Errorf("second action = %v, want ErrBusy", err)
	}
	if err := a.Send("hello"); err != nil {
		t.Fatal(err)
	}
	close(release)
	if got := <-s.got; got != "hello" {
		t.Errorf("queued turn = %q", got)
	}
	close(s.release)
	waitIdle(t, a)
	turns := rec.all(EvTurn)
	if len(turns) != 2 || turns[0].(TurnEvent).Text != "Check the sign-in on gpu" {
		t.Errorf("turns = %v", turns)
	}
	texts := rec.all(EvText)
	if len(texts) != 1 || texts[0].(TextEvent).Text != "Signed in." {
		t.Errorf("texts = %v", texts)
	}
	if got := rec.names(EvTurn, EvText, EvDone); !reflect.DeepEqual(got, []string{EvTurn, EvText, EvDone, EvTurn, EvDone}) {
		t.Errorf("events = %v", got)
	}
}

func TestRunActionCancelAndErrors(t *testing.T) {
	rec, a, _ := newStep()
	started := make(chan struct{})
	if err := a.RunAction("slow", func(ctx context.Context) (string, error) {
		close(started)
		<-ctx.Done()
		return "", nil
	}); err != nil {
		t.Fatal(err)
	}
	<-started
	a.Cancel()
	waitIdle(t, a)
	done := rec.all(EvDone)
	if len(done) != 1 || done[0].(DoneEvent).Error == "" {
		t.Errorf("done = %v (a canceled action reports the cancel)", done)
	}
	if err := a.RunAction(" ", func(context.Context) (string, error) { return "", nil }); err == nil {
		t.Error("an empty label was accepted")
	}
	a.Shutdown()
	if err := a.RunAction("x", func(context.Context) (string, error) { return "", nil }); err == nil {
		t.Error("an action after Shutdown was accepted")
	}
}

func TestLifecycleResetNotice(t *testing.T) {
	var firsts []bool
	done := make(chan struct{}, 4)
	l := &Lifecycle{Tray: func() bool { return true }, Hide: func(first bool) { firsts = append(firsts, first); done <- struct{}{} }}
	l.SetNoticeShown()
	l.BeforeClose()
	<-done
	l.ResetNotice()
	l.BeforeClose()
	<-done
	if !reflect.DeepEqual(firsts, []bool{false, true}) {
		t.Errorf("firsts = %v", firsts)
	}
}
