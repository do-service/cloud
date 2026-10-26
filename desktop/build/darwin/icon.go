//go:build ignore

// Draws the app icon, a 1024 px PNG, for `make icon` to turn into
// icons.icns. The repository has no logo to start from, so it's the tray's
// cloud (internal/app/trayicon.go) in white on a blue rounded square, the
// shape and margins of macOS 11+ app icons. A file of its own rather than
// a package: it isn't part of the client, and `go run` takes it as is.
//
//	go run icon.go out.png
package main

import (
	"image"
	"image/color"
	"image/png"
	"log"
	"math"
	"os"
)

const (
	size = 1024
	// The grid of Apple's icon template: an 824 px plate with 185 px
	// corners, centred, the rest left for the shadow the Dock draws.
	plate  = 824.0
	radius = 185.0
	// Samples per pixel side, for anti-aliased edges.
	ss = 4
)

// Mantine's blue[4] to blue[7], the web UI's primary colour.
var top, bottom = [3]float64{0x4d, 0xab, 0xf7}, [3]float64{0x1c, 0x7e, 0xd6}

func inPlate(x, y float64) bool {
	x, y = math.Abs(x-size/2), math.Abs(y-size/2)
	h := plate/2 - radius
	if x <= h || y <= h {
		return x <= plate/2 && y <= plate/2
	}
	return math.Hypot(x-h, y-h) <= radius
}

// inCloud is the tray icon's cloud in the plate's units, its bounding box
// centred on the plate's, and the flat bottom kept flat (the left circle
// would bulge below it at this size).
func inCloud(x, y float64) bool {
	u, v := (x-size/2)/plate+0.525, (y-size/2)/plate+0.485
	if v > 0.75 {
		return false
	}
	for _, c := range [][3]float64{{0.33, 0.58, 0.18}, {0.55, 0.45, 0.23}, {0.75, 0.6, 0.15}} {
		if math.Hypot(u-c[0], v-c[1]) <= c[2] {
			return true
		}
	}
	return u >= 0.33 && u <= 0.75 && v >= 0.58 && v <= 0.75
}

func main() {
	if len(os.Args) != 2 {
		log.Fatal("usage: go run icon.go out.png")
	}
	img := image.NewNRGBA(image.Rect(0, 0, size, size))
	for py := 0; py < size; py++ {
		t := float64(py) / size
		var bg [3]float64
		for i := range bg {
			bg[i] = top[i] + (bottom[i]-top[i])*t
		}
		for px := 0; px < size; px++ {
			var a, w float64
			for sy := 0; sy < ss; sy++ {
				for sx := 0; sx < ss; sx++ {
					x, y := float64(px)+(float64(sx)+0.5)/ss, float64(py)+(float64(sy)+0.5)/ss
					if inPlate(x, y) {
						a++
						if inCloud(x, y) {
							w++
						}
					}
				}
			}
			if a == 0 {
				continue
			}
			// Colour over the plate's own coverage, so its edge stays blue.
			f := w / a
			img.SetNRGBA(px, py, color.NRGBA{
				R: uint8(bg[0] + (255-bg[0])*f),
				G: uint8(bg[1] + (255-bg[1])*f),
				B: uint8(bg[2] + (255-bg[2])*f),
				A: uint8(math.Round(255 * a / (ss * ss))),
			})
		}
	}
	f, err := os.Create(os.Args[1])
	if err != nil {
		log.Fatal(err)
	}
	if err := png.Encode(f, img); err != nil {
		log.Fatal(err)
	}
	if err := f.Close(); err != nil {
		log.Fatal(err)
	}
}
