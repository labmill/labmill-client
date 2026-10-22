package gui

import (
	"sync"
	"time"
)

// Closer runs the window-close sequence off the UI thread. Wails calls
// OnBeforeClose on the UI thread (Windows), where a slow shutdown would
// freeze the window, and runtime.Quit calls OnBeforeClose again. So
// BeforeClose never blocks: the first call starts the sequence in a
// goroutine (hide the window, shut down, mark done, quit) and asks to keep
// the window open for now; calls while it runs do the same without starting
// another; once it is done (the quit it ends with arrives here), the close
// is allowed.
type Closer struct {
	// Hide hides the window; Shutdown stops the session and runtime (it may
	// take seconds); Quit asks the app to exit. Each runs once, in that
	// order, in the sequence's goroutine.
	Hide, Shutdown, Quit func()
	// Exit, when set with a positive Timeout, is called if Shutdown hasn't
	// returned Timeout after the window was hidden, so a hung shutdown can't
	// leave an invisible process behind (the window is already gone).
	Exit    func()
	Timeout time.Duration

	mu      sync.Mutex
	started bool
	done    bool
}

// BeforeClose is for Wails' OnBeforeClose. It returns true to prevent the
// close (the sequence is running or has just been started) and false once
// shutdown has finished. It returns at once.
func (c *Closer) BeforeClose() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.done {
		return false
	}
	if !c.started {
		c.started = true
		go c.run()
	}
	return true
}

func (c *Closer) run() {
	c.Hide()
	if c.Exit != nil && c.Timeout > 0 {
		t := time.AfterFunc(c.Timeout, c.Exit)
		c.Shutdown()
		t.Stop()
	} else {
		c.Shutdown()
	}
	c.mu.Lock()
	c.done = true
	c.mu.Unlock()
	c.Quit()
}
