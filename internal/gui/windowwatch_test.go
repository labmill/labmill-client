package gui

import "testing"

type fakeWin struct {
	x, y, w, h       int
	max, min, hidden bool
	saves            []WindowGeom
}

func (f *fakeWin) watcher() *WindowWatcher {
	return NewWindowWatcher(WindowProbe{
		Pos:       func() (int, int) { return f.x, f.y },
		Size:      func() (int, int) { return f.w, f.h },
		Maximised: func() bool { return f.max },
		Minimised: func() bool { return f.min },
		Hidden:    func() bool { return f.hidden },
		Save:      func(g WindowGeom) { f.saves = append(f.saves, g) },
	})
}

func TestWindowWatcher(t *testing.T) {
	f := &fakeWin{x: 100, y: 80, w: 900, h: 700}
	k := f.watcher()
	k.Seed(WindowGeom{X: 100, Y: 80, Width: 900, Height: 700})
	var prev WindowGeom

	// Unchanged since the seed: no write, ever.
	prev = k.Poll(prev)
	prev = k.Poll(prev)
	if len(f.saves) != 0 {
		t.Fatalf("wrote an unchanged geometry: %v", f.saves)
	}
	// A move is written after one stable poll, once.
	f.x = 300
	prev = k.Poll(prev) // first sight
	if len(f.saves) != 0 {
		t.Fatal("wrote before the geometry was stable")
	}
	prev = k.Poll(prev)
	prev = k.Poll(prev)
	if len(f.saves) != 1 || f.saves[0].X != 300 {
		t.Fatalf("saves = %v, want one write at x=300", f.saves)
	}
	// Hidden and minimised: nothing read, nothing written.
	f.hidden, f.x = true, 500
	for i := 0; i < 3; i++ {
		prev = k.Poll(prev)
	}
	k.Flush()
	f.hidden, f.min = false, true
	for i := 0; i < 3; i++ {
		prev = k.Poll(prev)
	}
	k.Flush()
	if len(f.saves) != 1 {
		t.Fatalf("wrote while hidden or minimised: %v", f.saves)
	}
	// Maximised: the last normal geometry with Maximised set; the maximised
	// rectangle never becomes the normal one.
	f.min, f.x = false, 300
	k.Poll(prev)
	f.max, f.x, f.y, f.w, f.h = true, 0, 0, 1920, 1040
	prev = k.Poll(prev)
	prev = k.Poll(prev)
	k.Flush()
	if n := len(f.saves); n != 2 {
		t.Fatalf("saves = %v, want 2", f.saves)
	}
	got := f.saves[1]
	if !got.Maximised || got.Width != 900 || got.X != 300 {
		t.Errorf("maximised save = %+v, want the normal 900-wide geometry at x=300, Maximised", got)
	}
	// Flush skips an unchanged geometry.
	k.Flush()
	if len(f.saves) != 2 {
		t.Errorf("flush wrote an unchanged geometry: %v", f.saves)
	}
	// Un-maximised again at a new size: flush writes it at once.
	f.max, f.w = false, 800
	k.Flush()
	if len(f.saves) != 3 || f.saves[2].Maximised || f.saves[2].Width != 800 {
		t.Errorf("saves = %v", f.saves)
	}
}

func TestWindowWatcherStartsMaximisedWithoutNormal(t *testing.T) {
	f := &fakeWin{x: 0, y: 0, w: 1920, h: 1040, max: true}
	k := f.watcher()
	k.Flush()
	if len(f.saves) != 1 || !f.saves[0].Maximised || f.saves[0].Width != 1920 {
		t.Errorf("saves = %v", f.saves)
	}
}
