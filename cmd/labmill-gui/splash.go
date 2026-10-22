//go:build desktop

package main

import (
	"sync/atomic"

	"labmill-client/internal/gui"
)

// The start splash (M53.1): on Windows a small native window shows at once,
// before the tray and Wails, because the app-starting
// cursor ends as soon as the tray's message loop runs and nothing else is
// visible until the window appears. The Win32 part is splash_windows.go; the
// other systems get a no-op (splash_other.go). This file holds the pure
// decisions, which the tests cover.

// The splash window's title and class. The title differs from the main
// window's, which windowSetPos and the self-test look for by title; those
// finders also skip the splash's class.
const (
	splashTitle = "Starting labmill"
	splashClass = "LabmillSplash"
	mainTitle   = "labmill"
)

// Splash size at 96 DPI, in pixels.
const (
	splashWidth, splashHeight = 360, 120
)

// splashGeom is what startSplash needs to know about where the main window
// will appear: the saved window geometry (gui.json, M47), or nil.
type splashGeom struct {
	Window *gui.WindowGeom
}

// splashStarts counts startSplash calls; the self-test asserts it is zero
// (the hidden self-test never shows a splash).
var splashStarts atomic.Int32

// splashCase is what the start knows when it decides whether to show the
// splash.
type splashCase struct {
	Tray           bool // started with --tray (hidden in the tray, no window)
	SelfTest       bool // LABMILL_GUI_SELFTEST: runs hidden on Windows
	Smoke          bool // LABMILL_GUI_SMOKE
	SecondInstance bool // another instance holds the single-instance lock and gets the handover
}

// splashWanted reports whether to show the splash. Not for a start that
// hides in the tray, the tests, or a second instance that only hands over
// to the running app (it would flash for a moment and vanish).
func splashWanted(c splashCase) bool {
	return !c.Tray && !c.SelfTest && !c.Smoke && !c.SecondInstance
}

// savedWindowGeom reads just the saved window geometry from gui.json (the
// splash needs it before main loads the preferences). nil when there is
// none or the file can't be read.
func savedWindowGeom() *gui.WindowGeom {
	path, err := gui.PrefsPath()
	if err != nil {
		return nil
	}
	p, err := gui.LoadPrefs(path)
	if err != nil {
		return nil
	}
	return p.Window
}

// splashRect is where the splash goes, in physical desktop pixels. It is
// centered on the rectangle the main window will open in (saved geometry:
// position physical, size logical, so scaled by scale), on the screen that
// holds the middle of that rectangle (else the primary work area), and
// clamped into that screen's work area. Without a usable geometry it
// centers in the primary work area. scale is physical per logical pixel.
func splashRect(g *gui.WindowGeom, screens []gui.Screen, primary gui.Screen, scale float64) (x, y, w, h int) {
	if scale <= 0 {
		scale = 1
	}
	w, h = int(splashWidth*scale+0.5), int(splashHeight*scale+0.5)
	scr := primary
	cx, cy := primary.X+primary.Width/2, primary.Y+primary.Height/2
	if g != nil && g.Valid() {
		gx, gy := g.X+int(float64(g.Width)*scale+0.5)/2, g.Y+int(float64(g.Height)*scale+0.5)/2
		for _, s := range screens {
			if gx >= s.X && gx < s.X+s.Width && gy >= s.Y && gy < s.Y+s.Height {
				scr, cx, cy = s, gx, gy
				break
			}
		}
	}
	x, y = cx-w/2, cy-h/2
	// Clamp into the work area (the work area's corner wins if the splash is bigger).
	x = max(scr.X, min(x, scr.X+scr.Width-w))
	y = max(scr.Y, min(y, scr.Y+scr.Height-h))
	return x, y, w, h
}
