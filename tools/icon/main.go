// Command icon generates the Labmill icon files into assets/icon. Run it from
// the repo root: go run ./tools/icon
package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"math"
	"os"
)

const out = "assets/icon/"

// L geometry on a 1024 grid: stem and foot as rounded boxes.
type box struct{ x0, y0, x1, y1, r float64 }

var lBoxes = []box{{300, 214, 478, 810, 34}, {300, 632, 760, 810, 34}}

func sdBox(b box, x, y float64) float64 {
	cx, cy := (b.x0+b.x1)/2, (b.y0+b.y1)/2
	hx, hy := (b.x1-b.x0)/2-b.r, (b.y1-b.y0)/2-b.r
	dx, dy := math.Abs(x-cx)-hx, math.Abs(y-cy)-hy
	return math.Hypot(math.Max(dx, 0), math.Max(dy, 0)) + math.Min(math.Max(dx, dy), 0) - b.r
}

func inL(x, y float64) bool {
	for _, b := range lBoxes {
		if sdBox(b, x, y) <= 0 {
			return true
		}
	}
	return false
}

func inTile(x, y float64) bool {
	return sdBox(box{0, 0, 1024, 1024, 225}, x, y) <= 0
}

func lerp(a, b uint8, t float64) uint8 { return uint8(float64(a) + (float64(b)-float64(a))*t + .5) }

// tile renders the app icon at n px; glyph (non-nil) renders only the L in that color on transparent.
func render(n int, glyph *color.RGBA) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, n, n))
	const ss = 4
	for py := 0; py < n; py++ {
		for px := 0; px < n; px++ {
			var r, g, b, a float64
			for sy := 0; sy < ss; sy++ {
				for sx := 0; sx < ss; sx++ {
					x := (float64(px) + (float64(sx)+.5)/ss) * 1024 / float64(n)
					y := (float64(py) + (float64(sy)+.5)/ss) * 1024 / float64(n)
					var c color.RGBA
					switch {
					case glyph != nil:
						if inL(x, y) {
							c = *glyph
						}
					case inL(x, y):
						c = color.RGBA{255, 255, 255, 255}
					case inTile(x, y):
						// gradient vector (0,0)->(0.35,1) in bbox units
						u := ((x/1024)*0.35 + (y/1024)*1) / (0.35*0.35 + 1)
						u = math.Min(math.Max(u, 0), 1)
						c = color.RGBA{lerp(0x1E, 0x7C, u), lerp(0x40, 0x3A, u), lerp(0xAF, 0xED, u), 255}
					}
					r += float64(c.R)
					g += float64(c.G)
					b += float64(c.B)
					a += float64(c.A)
				}
			}
			if a > 0 {
				img.SetNRGBA(px, py, color.NRGBA{uint8(r/(a/255) + .5), uint8(g/(a/255) + .5), uint8(b/(a/255) + .5), uint8(a/(ss*ss) + .5)})
			}
		}
	}
	return img
}

func pngBytes(im image.Image) []byte {
	var b bytes.Buffer
	png.Encode(&b, im)
	return b.Bytes()
}

func write(name string, data []byte) {
	if err := os.WriteFile(out+name, data, 0o644); err != nil {
		panic(err)
	}
}

func ico(sizes []int, glyph *color.RGBA) []byte {
	var imgs [][]byte
	for _, s := range sizes {
		imgs = append(imgs, pngBytes(render(s, glyph)))
	}
	var b bytes.Buffer
	binary.Write(&b, binary.LittleEndian, [3]uint16{0, 1, uint16(len(sizes))})
	off := 6 + 16*len(sizes)
	for i, s := range sizes {
		d := byte(s)
		if s >= 256 {
			d = 0
		}
		b.Write([]byte{d, d, 0, 0})
		binary.Write(&b, binary.LittleEndian, [2]uint16{1, 32})
		binary.Write(&b, binary.LittleEndian, [2]uint32{uint32(len(imgs[i])), uint32(off)})
		off += len(imgs[i])
	}
	for _, d := range imgs {
		b.Write(d)
	}
	return b.Bytes()
}

func icns() []byte {
	types := []struct {
		t string
		n int
	}{{"ic11", 32}, {"ic12", 64}, {"ic07", 128}, {"ic08", 256}, {"ic13", 256}, {"ic09", 512}, {"ic14", 512}, {"ic10", 1024}}
	var body bytes.Buffer
	for _, t := range types {
		d := pngBytes(render(t.n, nil))
		body.WriteString(t.t)
		binary.Write(&body, binary.BigEndian, uint32(8+len(d)))
		body.Write(d)
	}
	var b bytes.Buffer
	b.WriteString("icns")
	binary.Write(&b, binary.BigEndian, uint32(8+body.Len()))
	b.Write(body.Bytes())
	return b.Bytes()
}

func main() {
	for _, n := range []int{16, 24, 32, 48, 64, 128, 256, 512, 1024} {
		write(fmt.Sprintf("labmill-%d.png", n), pngBytes(render(n, nil)))
	}
	write("labmill.ico", ico([]int{16, 24, 32, 48, 64, 128, 256}, nil))
	write("labmill.icns", icns())
	light, dark := color.RGBA{255, 255, 255, 255}, color.RGBA{0x1F, 0x29, 0x37, 255}
	for name, c := range map[string]color.RGBA{"light": light, "dark": dark} {
		c := c
		for _, n := range []int{16, 32, 64} {
			write(fmt.Sprintf("tray-%s-%d.png", name, n), pngBytes(render(n, &c)))
		}
		write("tray-"+name+".ico", ico([]int{16, 24, 32, 48, 64}, &c))
	}
	// preview: app icon at several sizes on a light and a dark strip
	pv := image.NewNRGBA(image.Rect(0, 0, 1100, 420))
	for y := 0; y < 420; y++ {
		for x := 0; x < 1100; x++ {
			if y < 210 {
				pv.SetNRGBA(x, y, color.NRGBA{0xF3, 0xF4, 0xF6, 255})
			} else {
				pv.SetNRGBA(x, y, color.NRGBA{0x11, 0x18, 0x27, 255})
			}
		}
	}
	x := 40
	for _, n := range []int{160, 128, 96, 64, 48, 32, 16} {
		for row := 0; row < 2; row++ {
			im := render(n, nil)
			oy := row*210 + (210-n)/2
			for yy := 0; yy < n; yy++ {
				for xx := 0; xx < n; xx++ {
					c := im.NRGBAAt(xx, yy)
					if c.A == 0 {
						continue
					}
					bg := pv.NRGBAAt(x+xx, oy+yy)
					al := float64(c.A) / 255
					pv.SetNRGBA(x+xx, oy+yy, color.NRGBA{uint8(float64(c.R)*al + float64(bg.R)*(1-al)), uint8(float64(c.G)*al + float64(bg.G)*(1-al)), uint8(float64(c.B)*al + float64(bg.B)*(1-al)), 255})
				}
			}
		}
		x += n + 40
	}
	write("preview.png", pngBytes(pv))
}
