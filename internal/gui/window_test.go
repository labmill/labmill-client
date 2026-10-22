package gui

import "testing"

func TestRestore(t *testing.T) {
	primary := Screen{0, 0, 1920, 1040}
	left := Screen{-1280, 0, 1280, 984}
	def, min := Size{960, 700}, Size{640, 480}
	tests := []struct {
		name    string
		g       *WindowGeom
		screens []Screen
		x, y    int
		w, h    int
		max     bool
		center  bool
	}{
		{"nil", nil, []Screen{primary}, 0, 0, 960, 700, false, true},
		{"invalid", &WindowGeom{X: 5, Y: 5}, []Screen{primary}, 0, 0, 960, 700, false, true},
		{"fits", &WindowGeom{X: 100, Y: 80, Width: 1000, Height: 800}, []Screen{primary}, 100, 80, 1000, 800, false, false},
		{"second monitor left (negative x)", &WindowGeom{X: -1200, Y: 40, Width: 1000, Height: 700}, []Screen{primary, left}, -1200, 40, 1000, 700, false, false},
		{"monitor unplugged", &WindowGeom{X: -1200, Y: 40, Width: 1000, Height: 700}, []Screen{primary}, 0, 0, 960, 700, false, true},
		{"resolution shrank", &WindowGeom{X: 2500, Y: 100, Width: 900, Height: 700}, []Screen{primary}, 0, 0, 960, 700, false, true},
		{"title bar below the screen", &WindowGeom{X: 100, Y: 1100, Width: 900, Height: 700}, []Screen{primary}, 0, 0, 960, 700, false, true},
		{"only the outer edge overlaps", &WindowGeom{X: 1800, Y: 100, Width: 900, Height: 700}, []Screen{primary}, 0, 0, 960, 700, false, true},
		{"title bar partly above the top", &WindowGeom{X: 100, Y: -20, Width: 900, Height: 700}, []Screen{primary}, 100, 0, 900, 700, false, false},
		{"far above the top", &WindowGeom{X: 100, Y: -300, Width: 900, Height: 700}, []Screen{primary}, 0, 0, 960, 700, false, true},
		{"no screens known", &WindowGeom{X: 1, Y: 1, Width: 900, Height: 700}, nil, 0, 0, 960, 700, false, true},
		{"too small", &WindowGeom{X: 100, Y: 100, Width: 200, Height: 100}, []Screen{primary}, 100, 100, 640, 480, false, false},
		{"too big for the screen", &WindowGeom{X: 10, Y: 10, Width: 5000, Height: 4000}, []Screen{primary}, 10, 10, 1920, 1040, false, false},
		{"screen smaller than the minimum", &WindowGeom{X: 0, Y: 0, Width: 900, Height: 700}, []Screen{{0, 0, 500, 400}}, 0, 0, 640, 480, false, false},
		{"maximised", &WindowGeom{X: 50, Y: 60, Width: 900, Height: 700, Maximised: true}, []Screen{primary}, 50, 60, 900, 700, true, false},
	}
	for _, tc := range tests {
		x, y, w, h, mx, c := Restore(tc.g, tc.screens, def, min)
		if c != tc.center || mx != tc.max || w != tc.w || h != tc.h || (!c && (x != tc.x || y != tc.y)) {
			t.Errorf("%s: got x=%d y=%d w=%d h=%d max=%v center=%v; want x=%d y=%d w=%d h=%d max=%v center=%v",
				tc.name, x, y, w, h, mx, c, tc.x, tc.y, tc.w, tc.h, tc.max, tc.center)
		}
	}
}

func TestRestoreScaled(t *testing.T) {
	// A 150% display: the work area is 2880x1560 physical pixels; sizes are
	// logical (900x600 logical = 1350x900 physical).
	screen := []Screen{{0, 0, 2880, 1560}}
	def, min := Size{960, 700}, Size{640, 480}
	x, y, w, h, _, c := RestoreScaled(&WindowGeom{X: 200, Y: 100, Width: 900, Height: 600}, screen, def, min, 1.5)
	if c || x != 200 || y != 100 || w != 900 || h != 600 {
		t.Errorf("fits: %d,%d %dx%d center=%v", x, y, w, h, c)
	}
	// Bigger than the screen: limited to it (2880x1560 physical = 1920x1040 logical).
	_, _, w, h, _, _ = RestoreScaled(&WindowGeom{X: 0, Y: 0, Width: 4000, Height: 3000}, screen, def, min, 1.5)
	if w != 1920 || h != 1040 {
		t.Errorf("too big: %dx%d, want 1920x1040", w, h)
	}
	// Too small: raised to the minimum, still logical.
	_, _, w, h, _, _ = RestoreScaled(&WindowGeom{X: 0, Y: 0, Width: 100, Height: 100}, screen, def, min, 1.5)
	if w != 640 || h != 480 {
		t.Errorf("too small: %dx%d", w, h)
	}
	// Off screen: the default size, logical.
	_, _, w, h, _, c = RestoreScaled(&WindowGeom{X: 5000, Y: 0, Width: 900, Height: 600}, screen, def, min, 1.5)
	if !c || w != 960 || h != 700 {
		t.Errorf("off screen: %dx%d center=%v", w, h, c)
	}
	// Scale 1 is Restore.
	if _, _, w, _, _, _ := RestoreScaled(&WindowGeom{Width: 900, Height: 600}, screen, def, min, 1); w != 900 {
		t.Errorf("scale 1: w=%d", w)
	}
}

func TestWindowGeomValid(t *testing.T) {
	for g, want := range map[WindowGeom]bool{
		{X: 0, Y: 0, Width: 800, Height: 600}:          true,
		{X: -500, Y: -20, Width: 800, Height: 600}:     true,
		{Width: 0, Height: 600}:                        false,
		{Width: 800, Height: -1}:                       false,
		{X: 1 << 30, Width: 800, Height: 600}:          false,
		{X: -32000, Y: -32000, Width: 160, Height: 28}: true, // Valid is about numbers; Restore rejects off-screen
	} {
		if got := g.Valid(); got != want {
			t.Errorf("%+v: Valid=%v, want %v", g, got, want)
		}
	}
}
