//go:build desktop

package main

import (
	"testing"

	"labmill-client/internal/gui"
)

func TestSplashWanted(t *testing.T) {
	cases := []struct {
		name string
		c    splashCase
		want bool
	}{
		{"normal start", splashCase{}, true},
		{"tray start", splashCase{Tray: true}, false},
		{"self-test", splashCase{SelfTest: true}, false},
		{"smoke test", splashCase{Smoke: true}, false},
		{"second instance (handover)", splashCase{SecondInstance: true}, false},
	}
	for _, tc := range cases {
		if got := splashWanted(tc.c); got != tc.want {
			t.Errorf("%s: splashWanted = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestSplashRect(t *testing.T) {
	primary := gui.Screen{X: 0, Y: 0, Width: 1920, Height: 1040}
	left := gui.Screen{X: -1280, Y: 0, Width: 1280, Height: 984}
	screens := []gui.Screen{primary, left}
	cases := []struct {
		name           string
		g              *gui.WindowGeom
		scale          float64
		wx, wy, ww, wh int
	}{
		{"no geometry: primary center", nil, 1, 780, 460, 360, 120},
		{"invalid geometry: primary center", &gui.WindowGeom{}, 1, 780, 460, 360, 120},
		{"geometry at 96 DPI", &gui.WindowGeom{X: 100, Y: 100, Width: 960, Height: 700}, 1, 400, 390, 360, 120},
		{"DPI scaled (150%)", &gui.WindowGeom{X: 100, Y: 100, Width: 960, Height: 700}, 1.5, 100 + 720 - 270, 100 + 525 - 90, 540, 180},
		{"second monitor", &gui.WindowGeom{X: -1200, Y: 50, Width: 640, Height: 480}, 1, -880 - 180, 290 - 60, 360, 120},
		{"clamped at the right edge", &gui.WindowGeom{X: 1700, Y: 100, Width: 400, Height: 700}, 1, 1920 - 360, 390, 360, 120},
		{"center on no screen falls back to primary", &gui.WindowGeom{X: 5000, Y: 5000, Width: 100, Height: 100}, 1, 780, 460, 360, 120},
	}
	for _, tc := range cases {
		x, y, w, h := splashRect(tc.g, screens, primary, tc.scale)
		if x != tc.wx || y != tc.wy || w != tc.ww || h != tc.wh {
			t.Errorf("%s: got %d,%d %dx%d, want %d,%d %dx%d", tc.name, x, y, w, h, tc.wx, tc.wy, tc.ww, tc.wh)
		}
	}
}

func TestSplashTitleDiffersFromMainWindow(t *testing.T) {
	if splashTitle == mainTitle {
		t.Fatalf("the splash title %q equals the main window title; the position restore would move the splash", splashTitle)
	}
}

func TestSplashRectClampsIntoWorkArea(t *testing.T) {
	small := gui.Screen{X: 0, Y: 0, Width: 300, Height: 100}
	x, y, w, h := splashRect(nil, []gui.Screen{small}, small, 1)
	if x != 0 || y != 0 || w != 360 || h != 120 {
		t.Errorf("oversized splash: got %d,%d %dx%d, want it at the work area's corner", x, y, w, h)
	}
}
