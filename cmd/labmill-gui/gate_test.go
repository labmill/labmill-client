//go:build desktop

package main

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"labmill-client/internal/gui"
)

// doneRecorder is a Bridge emitter that records the errors of EvDone.
type doneRecorder struct {
	mu   sync.Mutex
	done chan string
}

func newDoneRecorder() *doneRecorder { return &doneRecorder{done: make(chan string, 4)} }

func (r *doneRecorder) emit(name string, data any) {
	if name == gui.EvDone {
		r.done <- data.(gui.DoneEvent).Error
	}
}

func sendAsync(g *gate, ctx context.Context) {
	go g.Send(ctx, "hello")
}

// A message sent before the runtime exists waits (the turn stays open) until
// the gate settles.
func TestGateSendWaitsForRuntime(t *testing.T) {
	rec := newDoneRecorder()
	g := newGate(gui.NewBridge(rec.emit))
	sendAsync(g, context.Background())
	select {
	case e := <-rec.done:
		t.Fatalf("turn ended before startup finished: %q", e)
	case <-time.After(100 * time.Millisecond):
	}
	g.shutdown() // settles without a runtime
	select {
	case e := <-rec.done:
		if e == "" {
			t.Fatal("expected the not-ready error")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("waiting Send was not released")
	}
}

// fakeSession counts the messages the gate forwards.
type fakeSession struct {
	mu    sync.Mutex
	texts []string
}

func (f *fakeSession) Send(_ context.Context, text string) {
	f.mu.Lock()
	f.texts = append(f.texts, text)
	f.mu.Unlock()
}
func (f *fakeSession) Wait(time.Duration) bool { return true }

// The success path: a message sent before the runtime exists waits, and goes
// to the session exactly once when it is installed.
func TestGateSendForwardsOnceAfterInstall(t *testing.T) {
	rec := newDoneRecorder()
	g := newGate(gui.NewBridge(rec.emit))
	fs := &fakeSession{}
	finished := make(chan struct{})
	go func() { g.Send(context.Background(), "hello"); close(finished) }()
	select {
	case <-finished:
		t.Fatal("Send returned before the runtime existed")
	case <-time.After(100 * time.Millisecond):
	}
	fs.mu.Lock()
	if len(fs.texts) != 0 {
		t.Fatal("forwarded before install")
	}
	fs.mu.Unlock()
	if !g.install(nil, fs) {
		t.Fatal("install refused")
	}
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("Send did not continue after install")
	}
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if len(fs.texts) != 1 || fs.texts[0] != "hello" {
		t.Fatalf("forwarded %v, want [hello] once", fs.texts)
	}
	select {
	case e := <-rec.done:
		t.Fatalf("gate ended the turn itself: %q", e)
	default:
	}
}

// A failed startup ends the waiting turn with its error.
func TestGateSendFailsWithStartupError(t *testing.T) {
	rec := newDoneRecorder()
	g := newGate(gui.NewBridge(rec.emit))
	sendAsync(g, context.Background())
	time.Sleep(50 * time.Millisecond)
	g.fail(errors.New("no config"))
	select {
	case e := <-rec.done:
		if e != "no config" {
			t.Fatalf("error = %q", e)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("waiting Send was not released")
	}
}

// Cancel ends the wait.
func TestGateSendCancelEndsWait(t *testing.T) {
	rec := newDoneRecorder()
	g := newGate(gui.NewBridge(rec.emit))
	ctx, cancel := context.WithCancel(context.Background())
	sendAsync(g, ctx)
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case e := <-rec.done:
		if e == "" {
			t.Fatal("expected the cancel error")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("canceled Send did not return")
	}
}
