package stall

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestRunReturnsResult(t *testing.T) {
	v, err := Run(context.Background(), NewLimiter(1), "a.txt", time.Second, func() (int, error) { return 7, nil })
	if err != nil || v != 7 {
		t.Fatalf("got %v, %v", v, err)
	}
	want := errors.New("boom")
	if _, err := Run(context.Background(), NewLimiter(1), "a.txt", time.Second, func() (int, error) { return 0, want }); err != want {
		t.Fatalf("error not passed through: %v", err)
	}
}

func TestRunAbandonsStuckRead(t *testing.T) {
	l := NewLimiter(1)
	release := make(chan struct{})
	_, err := Run(context.Background(), l, "mount/x.csv", 20*time.Millisecond, func() (int, error) {
		<-release
		return 1, nil
	})
	var se *Error
	if !errors.As(err, &se) || se.Busy || !IsStall(err) {
		t.Fatalf("want stall error, got %v", err)
	}
	if !strings.Contains(err.Error(), "mount/x.csv didn't answer within 1 s") {
		t.Fatalf("message: %q", err)
	}
	// The only slot is still held by the abandoned read: refuse at once.
	ran := false
	_, err = Run(context.Background(), l, "y", time.Second, func() (int, error) { ran = true; return 0, nil })
	if !errors.As(err, &se) || !se.Busy || ran {
		t.Fatalf("want busy refusal without running, got %v (ran=%v)", err, ran)
	}
	// Once the abandoned read finishes, the slot comes back.
	close(release)
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := Run(context.Background(), l, "z", time.Second, func() (int, error) { return 2, nil }); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("slot never released")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestRunContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	release := make(chan struct{})
	defer close(release)
	go func() { time.Sleep(10 * time.Millisecond); cancel() }()
	_, err := Run(ctx, NewLimiter(1), "f", 0, func() (int, error) { <-release; return 0, nil })
	if !errors.Is(err, context.Canceled) || IsStall(err) {
		t.Fatalf("want ctx.Err() on cancel, got %v", err)
	}
	// An already-ended context takes no slot and starts nothing.
	l := NewLimiter(1)
	if _, err := Run(ctx, l, "g", time.Second, func() (int, error) { t.Error("ran"); return 0, nil }); !errors.Is(err, context.Canceled) || l.Stuck() != 0 {
		t.Fatalf("done ctx: %v stuck=%d", err, l.Stuck())
	}
}
