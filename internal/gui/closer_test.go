package gui

import (
	"sync/atomic"
	"testing"
	"time"
)

func TestCloserNeverBlocksAndRunsOnce(t *testing.T) {
	release := make(chan struct{})
	quit := make(chan struct{})
	var hides, shuts, quits atomic.Int32
	c := &Closer{
		Hide: func() { hides.Add(1) },
		Shutdown: func() {
			shuts.Add(1)
			<-release // a slow shutdown
		},
		Quit: func() { quits.Add(1); close(quit) },
	}
	returned := make(chan bool)
	go func() { returned <- c.BeforeClose() }()
	select {
	case prevent := <-returned:
		if !prevent {
			t.Fatal("first BeforeClose allowed the close; want prevent")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("BeforeClose blocked on the slow shutdown")
	}
	// A second attempt during shutdown prevents too, and starts nothing.
	if !c.BeforeClose() {
		t.Error("BeforeClose during shutdown allowed the close")
	}
	close(release)
	select {
	case <-quit:
	case <-time.After(2 * time.Second):
		t.Fatal("Quit was never called")
	}
	// The Quit's own BeforeClose now lets the close through.
	if c.BeforeClose() {
		t.Error("BeforeClose after shutdown prevented the close")
	}
	if hides.Load() != 1 || shuts.Load() != 1 || quits.Load() != 1 {
		t.Errorf("hide/shutdown/quit = %d/%d/%d, want 1/1/1", hides.Load(), shuts.Load(), quits.Load())
	}
}

func TestCloserOrder(t *testing.T) {
	var order []string
	done := make(chan struct{})
	c := &Closer{
		Hide:     func() { order = append(order, "hide") },
		Shutdown: func() { order = append(order, "shutdown") },
		Quit:     func() { order = append(order, "quit"); close(done) },
	}
	c.BeforeClose()
	<-done
	if got := len(order); got != 3 || order[0] != "hide" || order[1] != "shutdown" || order[2] != "quit" {
		t.Errorf("order = %v", order)
	}
}

// Windows calls OnBeforeClose synchronously from Quit: the re-entry must not
// deadlock and must allow the close.
func TestCloserSynchronousReentry(t *testing.T) {
	var c *Closer
	reentered := make(chan bool, 1)
	c = &Closer{
		Hide:     func() {},
		Shutdown: func() {},
		Quit:     func() { reentered <- c.BeforeClose() },
	}
	if !c.BeforeClose() {
		t.Fatal("first close must be prevented while shutting down")
	}
	select {
	case allowed := <-reentered:
		if allowed {
			t.Error("re-entry from Quit returned true (would keep the window open forever)")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("re-entry from Quit deadlocked")
	}
}

func TestCloserWatchdogExitsOnHang(t *testing.T) {
	exited := make(chan struct{})
	release := make(chan struct{})
	defer close(release)
	c := &Closer{
		Hide:     func() {},
		Shutdown: func() { <-release },
		Quit:     func() {},
		Exit:     func() { close(exited) },
		Timeout:  50 * time.Millisecond,
	}
	c.BeforeClose()
	select {
	case <-exited:
	case <-time.After(2 * time.Second):
		t.Fatal("watchdog did not exit a hung shutdown")
	}
}

func TestCloserWatchdogStoppedAfterShutdown(t *testing.T) {
	var exited atomic.Bool
	quit := make(chan struct{})
	c := &Closer{
		Hide:     func() {},
		Shutdown: func() {},
		Quit:     func() { close(quit) },
		Exit:     func() { exited.Store(true) },
		Timeout:  50 * time.Millisecond,
	}
	c.BeforeClose()
	<-quit
	time.Sleep(150 * time.Millisecond)
	if exited.Load() {
		t.Error("watchdog fired after a shutdown that finished in time")
	}
}
