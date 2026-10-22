package gui

import (
	"sync"
	"time"
)

// WindowProbe is what WindowWatcher needs from the window system (Wails in
// the app, fakes in tests).
type WindowProbe struct {
	Pos       func() (x, y int)
	Size      func() (w, h int)
	Maximised func() bool
	Minimised func() bool
	Hidden    func() bool // hidden in the tray: nothing to read or save
	Save      func(WindowGeom)
}

// WindowWatcher saves the window's geometry when it changed. Wails v2 has no
// move or resize events on every platform, so Run polls.
type WindowWatcher struct {
	p WindowProbe

	mu     sync.Mutex
	normal WindowGeom // last geometry seen while not maximised
	saved  WindowGeom // last geometry written (or restored)
}

// NewWindowWatcher returns a watcher that reads and saves through p.
func NewWindowWatcher(p WindowProbe) *WindowWatcher { return &WindowWatcher{p: p} }

// Seed sets the geometry the window opened with, so it isn't written back
// needlessly and a maximise before the first poll keeps it as the normal one.
func (k *WindowWatcher) Seed(g WindowGeom) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.normal, k.saved = g, g
}

// Read is the window's geometry now; ok is false while it is minimised or
// hidden (nothing worth saving). A maximised window reports the last normal
// size and position, so un-maximising later returns to them.
func (k *WindowWatcher) Read() (g WindowGeom, ok bool) {
	if k.p.Hidden() || k.p.Minimised() {
		return g, false
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.p.Maximised() {
		g = k.normal
		g.Maximised = true
		if !g.Valid() { // started maximised with no earlier normal geometry
			g.X, g.Y = k.p.Pos()
			g.Width, g.Height = k.p.Size()
		}
		return g, g.Valid()
	}
	g.X, g.Y = k.p.Pos()
	g.Width, g.Height = k.p.Size()
	if !g.Valid() {
		return g, false
	}
	k.normal = g
	return g, true
}

// Flush writes the geometry now if it differs from the last one written.
func (k *WindowWatcher) Flush() {
	if g, ok := k.Read(); ok {
		k.write(g)
	}
}

func (k *WindowWatcher) write(g WindowGeom) {
	k.mu.Lock()
	if g == k.saved {
		k.mu.Unlock()
		return
	}
	k.saved = g
	k.mu.Unlock()
	k.p.Save(g)
}

// Poll is one watcher step: it writes a change that stayed the same since
// prev (the debounce) and returns the geometry to pass as prev next time.
func (k *WindowWatcher) Poll(prev WindowGeom) WindowGeom {
	g, ok := k.Read()
	if ok && g == prev {
		k.write(g)
	}
	return g
}

// Run polls every interval until done is closed.
func (k *WindowWatcher) Run(done <-chan struct{}, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	var prev WindowGeom
	for {
		select {
		case <-done:
			return
		case <-t.C:
		}
		prev = k.Poll(prev)
	}
}
