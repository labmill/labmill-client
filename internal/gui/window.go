package gui

// WindowGeom is the main window's last normal (not maximised) size and
// position in gui.json, plus whether it was maximised. X and Y are the
// window system's own coordinates as Wails reports them: desktop coordinates
// on Windows and Linux (negative on a monitor left of or above the primary
// one), relative to the current screen on macOS.
type WindowGeom struct {
	X         int  `json:"x"`
	Y         int  `json:"y"`
	Width     int  `json:"width"`
	Height    int  `json:"height"`
	Maximised bool `json:"maximized"`
}

// Screen is the work area (the monitor minus taskbar and dock) of one monitor
// in the same desktop coordinates as WindowGeom.
type Screen struct{ X, Y, Width, Height int }

// Size is a window size.
type Size struct{ Width, Height int }

// maxCoord bounds a believable stored value; a hand-edited or corrupt file
// with more is treated as no geometry.
const maxCoord = 100000

// titleStrip is the height of the title-bar strip that must stay reachable
// (checked over the middle half of the window's width).
const titleStrip = 40

// Valid reports whether g can be restored at all: a positive size and
// believable numbers.
func (g WindowGeom) Valid() bool {
	in := func(v int) bool { return v >= -maxCoord && v <= maxCoord }
	return g.Width > 0 && g.Height > 0 && g.Width <= maxCoord && g.Height <= maxCoord && in(g.X) && in(g.Y)
}

// Restore decides how to open the window. nil or invalid g, or a window whose
// title-bar strip touches no screen (a monitor was unplugged, the resolution
// changed), opens centered at the default size (center is true, x and y are
// unused). Otherwise the saved position is kept, the size is limited to the
// screen holding the title bar and raised to min (min wins over the screen),
// and a window above the top of that screen is moved down to it.
func Restore(g *WindowGeom, screens []Screen, def, min Size) (x, y, w, h int, maximized, center bool) {
	if g == nil || !g.Valid() {
		return 0, 0, def.Width, def.Height, false, true
	}
	// The strip: middle half of the width, top titleStrip pixels.
	sx0, sx1 := g.X+g.Width/4, g.X+g.Width-g.Width/4
	sy0, sy1 := g.Y, g.Y+titleStrip
	best, bestArea := -1, 0
	for i, s := range screens {
		ox := overlap(sx0, sx1, s.X, s.X+s.Width)
		oy := overlap(sy0, sy1, s.Y, s.Y+s.Height)
		if a := ox * oy; a > bestArea {
			best, bestArea = i, a
		}
	}
	if best < 0 {
		return 0, 0, def.Width, def.Height, false, true
	}
	s := screens[best]
	w, h = g.Width, g.Height
	if w > s.Width {
		w = s.Width
	}
	if h > s.Height {
		h = s.Height
	}
	if w < min.Width {
		w = min.Width
	}
	if h < min.Height {
		h = min.Height
	}
	x, y = g.X, g.Y
	if y < s.Y {
		y = s.Y
	}
	return x, y, w, h, g.Maximised, false
}

// RestoreScaled is Restore for a window system that counts sizes in logical
// pixels but positions and screens in physical ones (Windows with display
// scaling): scale is physical per logical pixel (1.5 at 150%). g, def, min
// and the returned w and h are logical; x, y and screens are physical.
func RestoreScaled(g *WindowGeom, screens []Screen, def, min Size, scale float64) (x, y, w, h int, maximized, center bool) {
	if scale <= 0 || scale == 1 {
		return Restore(g, screens, def, min)
	}
	up := func(v int) int { return int(float64(v)*scale + 0.5) }
	down := func(v int) int { return int(float64(v)/scale + 0.5) }
	var pg *WindowGeom
	if g != nil {
		c := *g
		c.Width, c.Height = up(c.Width), up(c.Height)
		pg = &c
	}
	x, y, w, h, maximized, center = Restore(pg, screens, Size{up(def.Width), up(def.Height)}, Size{up(min.Width), up(min.Height)})
	return x, y, down(w), down(h), maximized, center
}

// overlap is the length of the intersection of [a0,a1) and [b0,b1), or 0.
func overlap(a0, a1, b0, b1 int) int {
	lo, hi := a0, a1
	if b0 > lo {
		lo = b0
	}
	if b1 < hi {
		hi = b1
	}
	if hi > lo {
		return hi - lo
	}
	return 0
}
