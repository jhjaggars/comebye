//go:build ignore

// genicon draws the app icon (a dark rounded square with herdr-style status
// dots) as PNG files for the PWA manifest and home screen.
//
// Usage: go run tools/genicon/main.go
package main

import (
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"math"
	"os"
)

func main() {
	for _, spec := range []struct {
		size int
		path string
	}{{192, "web/icon-192.png"}, {512, "web/icon-512.png"}} {
		out, err := os.Create(spec.path)
		if err != nil {
			panic(err)
		}
		if err := png.Encode(out, drawIcon(spec.size)); err != nil {
			panic(err)
		}
		out.Close()
	}
}

func drawIcon(size int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, size, size))
	bg := color.RGBA{R: 24, G: 26, B: 32, A: 255}
	draw.Draw(img, img.Bounds(), &image.Uniform{bg}, image.Point{}, draw.Src)

	// Three status dots: working (blue), blocked (orange), done (green).
	dots := []color.RGBA{
		{R: 96, G: 165, B: 250, A: 255},
		{R: 251, G: 146, B: 60, A: 255},
		{R: 74, G: 222, B: 128, A: 255},
	}
	s := float64(size)
	cy := s / 2
	radius := s * 0.085
	spacing := s * 0.26
	for i, c := range dots {
		fillCircle(img, s/2+(float64(i)-1)*spacing, cy, radius, c)
	}

	// Round the corners by clearing pixels outside a rounded rectangle.
	corner := s * 0.22
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			if !insideRounded(float64(x), float64(y), s, corner) {
				img.SetRGBA(x, y, color.RGBA{})
			}
		}
	}
	return img
}

// insideRounded reports whether (x, y) falls inside a square of side s with
// corners rounded at radius r, with (0,0) at the top-left.
func insideRounded(x, y, s, r float64) bool {
	cx := math.Min(x, s-x)
	cy := math.Min(y, s-y)
	// Distance to the nearest actual (unrounded) corner center.
	if cx >= r || cy >= r {
		return true // outside any corner quadrant
	}
	return math.Hypot(r-cx, r-cy) <= r
}

func fillCircle(img *image.RGBA, cx, cy, r float64, c color.RGBA) {
	for y := int(cy - r - 1); y <= int(cy+r+1); y++ {
		for x := int(cx - r - 1); x <= int(cx+r+1); x++ {
			if x < 0 || y < 0 || x >= img.Bounds().Dx() || y >= img.Bounds().Dy() {
				continue
			}
			if math.Hypot(float64(x)-cx, float64(y)-cy) <= r {
				img.SetRGBA(x, y, c)
			}
		}
	}
}
