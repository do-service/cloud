//go:build ignore

// Draws the app icon, a 1024 px PNG, for `make icon` to turn into
// icons.icns. The repository has no logo to start from, so it's the tray's
// cloud (internal/app/trayicon.go) in white on a blue rounded square, the
// shape and margins of macOS 11+ app icons. A file of its own rather than
// a package: it isn't part of the client, and `go run` takes it as is.
//
// Given a .ico name, it writes the Windows icon for `make windows`
// instead: the same drawing at the usual sizes, with the narrow margins
// of Windows icons.
//
//	go run icon.go out.png|out.ico
package main

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"image/png"
	"log"
	"math"
	"os"
	"strings"
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

// render draws the icon n px wide; span is how much of the 1024 px grid
// it shows, centred: less than size crops the margins.
func render(n int, span float64) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, n, n))
	scale := span / float64(n)
	at := func(p int, s int) float64 { return size/2 + (float64(p)+(float64(s)+0.5)/ss-float64(n)/2)*scale }
	for py := 0; py < n; py++ {
		t := (size/2 + (float64(py)-float64(n)/2)*scale - (size-span)/2) / span
		var bg [3]float64
		for i := range bg {
			bg[i] = top[i] + (bottom[i]-top[i])*t
		}
		for px := 0; px < n; px++ {
			var a, w float64
			for sy := 0; sy < ss; sy++ {
				for sx := 0; sx < ss; sx++ {
					x, y := at(px, sx), at(py, sy)
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
	return img
}

// ico packs images as uncompressed 32-bit bitmaps, which every reader
// takes (NSIS among them), unlike the PNG entries of newer icons.
func ico(imgs []*image.NRGBA) []byte {
	var dir, data bytes.Buffer
	le := func(b *bytes.Buffer, v ...any) {
		for _, x := range v {
			binary.Write(b, binary.LittleEndian, x)
		}
	}
	le(&dir, uint16(0), uint16(1), uint16(len(imgs)))
	offset := 6 + 16*len(imgs)
	for _, img := range imgs {
		n := img.Rect.Dx()
		maskRow := (n + 31) / 32 * 4
		var bmp bytes.Buffer
		// BITMAPINFOHEADER; the height counts the AND mask too.
		le(&bmp, uint32(40), int32(n), int32(2*n), uint16(1), uint16(32), uint32(0),
			uint32(n*n*4+n*maskRow), int32(0), int32(0), uint32(0), uint32(0))
		for y := n - 1; y >= 0; y-- {
			for x := 0; x < n; x++ {
				c := img.NRGBAAt(x, y)
				bmp.Write([]byte{c.B, c.G, c.R, c.A})
			}
		}
		// All zero: the alpha channel alone says what's transparent.
		bmp.Write(make([]byte, n*maskRow))
		// 0 means 256 in the one-byte width and height.
		le(&dir, uint8(n), uint8(n), uint8(0), uint8(0), uint16(1), uint16(32), uint32(bmp.Len()), uint32(offset))
		offset += bmp.Len()
		data.Write(bmp.Bytes())
	}
	return append(dir.Bytes(), data.Bytes()...)
}

func main() {
	if len(os.Args) != 2 {
		log.Fatal("usage: go run icon.go out.png|out.ico")
	}
	out := os.Args[1]
	if strings.HasSuffix(out, ".ico") {
		var imgs []*image.NRGBA
		for _, n := range []int{16, 24, 32, 48, 64, 256} {
			// The plate fills all but a few percent: Windows draws no
			// shadow around icons, as the Dock does.
			imgs = append(imgs, render(n, plate*1.06))
		}
		if err := os.WriteFile(out, ico(imgs), 0o644); err != nil {
			log.Fatal(err)
		}
		return
	}
	f, err := os.Create(out)
	if err != nil {
		log.Fatal(err)
	}
	if err := png.Encode(f, render(size, size)); err != nil {
		log.Fatal(err)
	}
	if err := f.Close(); err != nil {
		log.Fatal(err)
	}
}
