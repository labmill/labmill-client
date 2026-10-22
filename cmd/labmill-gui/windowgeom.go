//go:build desktop

package main

import (
	"context"
	"time"

	wr "github.com/wailsapp/wails/v2/pkg/runtime"

	"labmill-client/internal/gui"
)

// Window size limits and the default size (also the Wails options).
const (
	winWidth, winHeight       = 960, 700
	winMinWidth, winMinHeight = 640, 480
)

// geomPoll is how often the watcher looks at the window; a change is written
// once it stayed the same for one poll (the debounce).
const geomPoll = time.Second

// applyGeom opens the window where the user left it (gui.RestoreScaled):
// position and size first, then maximised (only if maximise; the caller
// leaves it to the first show while the window starts hidden in the tray,
// since maximising can show it). Without a usable geometry it centers at the
// default size. It returns the geometry now in effect (the normal one, even
// when maximised) so the watcher doesn't write it back needlessly, and
// whether the window should be maximised.
func applyGeom(ctx context.Context, g *gui.WindowGeom, maximise bool) (now gui.WindowGeom, wantMax bool) {
	screens := windowScreens(ctx)
	x, y, w, h, maximised, center := gui.RestoreScaled(g, screens, gui.Size{Width: winWidth, Height: winHeight}, gui.Size{Width: winMinWidth, Height: winMinHeight}, windowScale())
	wr.WindowSetSize(ctx, w, h)
	if center {
		wr.WindowCenter(ctx)
		now.X, now.Y = wr.WindowGetPosition(ctx)
		now.Width, now.Height = wr.WindowGetSize(ctx)
		return now, false
	}
	windowSetPos(ctx, x, y)
	if maximised && maximise {
		wr.WindowMaximise(ctx)
	}
	return gui.WindowGeom{X: x, Y: y, Width: w, Height: h, Maximised: maximised}, maximised
}

// newGeomWatcher watches the Wails window (ctx is read at each poll through
// ctxOf, which is nil before OnStartup).
func newGeomWatcher(ctxOf func() context.Context, save func(change func(*gui.Prefs)), hidden func() bool) *gui.WindowWatcher {
	return gui.NewWindowWatcher(gui.WindowProbe{
		Pos:       func() (int, int) { return wr.WindowGetPosition(ctxOf()) },
		Size:      func() (int, int) { return wr.WindowGetSize(ctxOf()) },
		Maximised: func() bool { return wr.WindowIsMaximised(ctxOf()) },
		Minimised: func() bool { return wr.WindowIsMinimised(ctxOf()) },
		Hidden:    hidden,
		Save:      func(g gui.WindowGeom) { save(func(p *gui.Prefs) { c := g; p.Window = &c }) },
	})
}
