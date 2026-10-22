//go:build desktop && !windows

package main

import (
	"context"

	wr "github.com/wailsapp/wails/v2/pkg/runtime"

	"labmill-client/internal/gui"
)

// windowScreens: Wails v2 reports only the size of each monitor, not where
// it sits, so the layout is unknown here. The window managers on macOS and
// Linux already pull a window off a missing monitor back onto a screen, so
// the check is permissive: one rectangle that covers any layout of the
// monitors (the size is limited to it, not to one monitor). Windows asks the
// OS for the real layout (windowgeom_windows.go). Positions are what Wails
// reports: macOS reads and sets them relative to the current screen, so a
// window left on a second screen reopens at the same offset on whichever
// screen it opens on (a known limitation); Linux uses desktop coordinates
// (windowgeom_linux.go).
func windowScreens(ctx context.Context) []gui.Screen {
	all, err := wr.ScreenGetAll(ctx)
	sumW, maxW, maxH := 0, 0, 0
	for _, s := range all {
		w, h := s.Size.Width, s.Size.Height
		if w <= 0 || h <= 0 {
			w, h = s.Width, s.Height
		}
		sumW += w
		maxW, maxH = max(maxW, w), max(maxH, h)
	}
	if err != nil || maxW <= 0 || maxH <= 0 {
		return []gui.Screen{{X: -30000, Y: -30000, Width: 60000, Height: 60000}}
	}
	return []gui.Screen{{X: -sumW, Y: -maxH, Width: 2 * sumW, Height: 3 * maxH}}
}

// windowScale: sizes and positions share one unit outside Windows.
func windowScale() float64 { return 1 }
