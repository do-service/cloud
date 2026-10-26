package app

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"math"
	"sync"
)

// The tray's icons: a cloud with the state cut out of it, drawn here
// rather than shipped as files: five states, one shape, and macOS wants
// them as template images (black on transparent, recoloured by the menu
// bar for light and dark), where a glyph is all a state can show.

const iconSize = 44 // px: the menu bar is 22 pt, Retina doubles it

var (
	iconsOnce sync.Once
	iconPNGs  map[Status][]byte
)

// TrayIcon is the template PNG for the tray state s (see Overall).
func TrayIcon(s Status) []byte {
	iconsOnce.Do(func() {
		iconPNGs = map[Status][]byte{}
		for _, st := range []Status{StatusSynced, StatusSyncing, StatusPaused, StatusError, StatusOffline} {
			iconPNGs[st] = drawIcon(st)
		}
	})
	if b, ok := iconPNGs[s]; ok {
		return b
	}
	return iconPNGs[StatusSynced]
}

type seg struct{ x1, y1, x2, y2 float64 }

// glyphs are cut out of the cloud, in the icon's unit square.
var glyphs = map[Status][]seg{
	StatusSynced:  {{0.37, 0.61, 0.46, 0.70}, {0.46, 0.70, 0.63, 0.52}},
	StatusSyncing: {{0.42, 0.73, 0.42, 0.51}, {0.42, 0.51, 0.36, 0.57}, {0.42, 0.51, 0.48, 0.57}, {0.58, 0.51, 0.58, 0.73}, {0.58, 0.73, 0.52, 0.67}, {0.58, 0.73, 0.64, 0.67}},
	StatusPaused:  {{0.44, 0.52, 0.44, 0.71}, {0.56, 0.52, 0.56, 0.71}},
	StatusError:   {{0.5, 0.50, 0.5, 0.62}, {0.5, 0.71, 0.5, 0.71}},
}

// cloud reports whether (x, y) is in the cloud grown by d (negative:
// shrunk).
func cloud(x, y, d float64) bool {
	circles := [][3]float64{{0.33, 0.58, 0.18}, {0.55, 0.45, 0.23}, {0.75, 0.6, 0.15}}
	for _, c := range circles {
		if math.Hypot(x-c[0], y-c[1]) <= c[2]+d {
			return true
		}
	}
	// The flat bottom. Its top stays put when shrunk: inside the
	// circles, it would leave a seam in the outline between them.
	return x >= 0.33-d && x <= 0.75+d && y >= 0.58-max(d, 0) && y <= 0.75+d
}

func nearSeg(x, y float64, s seg) float64 {
	dx, dy := s.x2-s.x1, s.y2-s.y1
	t := 0.0
	if l := dx*dx + dy*dy; l > 0 {
		t = math.Max(0, math.Min(1, ((x-s.x1)*dx+(y-s.y1)*dy)/l))
	}
	return math.Hypot(x-(s.x1+t*dx), y-(s.y1+t*dy))
}

func inside(st Status, x, y float64) bool {
	if st == StatusOffline {
		// An outline, struck through.
		slash := seg{0.2, 0.22, 0.82, 0.86}
		d := nearSeg(x, y, slash)
		if d < 0.035 {
			return true
		}
		return d > 0.075 && cloud(x, y, 0) && !cloud(x, y, -0.06)
	}
	if !cloud(x, y, 0) {
		return false
	}
	for _, s := range glyphs[st] {
		if nearSeg(x, y, s) < 0.035 {
			return false
		}
	}
	return true
}

func drawIcon(st Status) []byte {
	img := image.NewNRGBA(image.Rect(0, 0, iconSize, iconSize))
	const ss = 4 // samples per pixel side, for smooth edges
	for py := range iconSize {
		for px := range iconSize {
			n := 0
			for sy := range ss {
				for sx := range ss {
					x := (float64(px) + (float64(sx)+0.5)/ss) / iconSize
					y := (float64(py) + (float64(sy)+0.5)/ss) / iconSize
					if inside(st, x, y) {
						n++
					}
				}
			}
			img.SetNRGBA(px, py, color.NRGBA{A: uint8(n * 255 / (ss * ss))})
		}
	}
	var b bytes.Buffer
	_ = png.Encode(&b, img)
	return b.Bytes()
}
