package main

// Backdrop for the frosted-glass look: a soft, hazy slate-blue image generated
// at startup (no image files to ship). Panels are drawn translucent on top.

import (
	"image"
	"image/color"
	"math"
	"math/rand"
)

func makeBackdrop(w, h int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	type blob struct {
		x, y, r    float64 // in 0..1 of width/height, radius in widths
		cr, cg, cb float64
		a          float64
	}
	blobs := []blob{
		{0.10, 0.08, 0.70, 150, 160, 172, 0.55}, // light haze top-left
		{0.95, 0.22, 0.55, 70, 120, 170, 0.45},  // cool blue right
		{0.35, 0.55, 0.60, 95, 105, 118, 0.35},  // grey middle
		{0.85, 0.85, 0.65, 40, 70, 105, 0.50},   // deep blue bottom-right
		{0.05, 0.95, 0.55, 20, 24, 30, 0.60},    // dark bottom-left
	}
	rng := rand.New(rand.NewSource(7))
	for y := 0; y < h; y++ {
		fy := float64(y) / float64(h)
		// base vertical gradient
		br := 58 - 34*fy
		bg := 63 - 36*fy
		bb := 72 - 38*fy
		for x := 0; x < w; x++ {
			fx := float64(x) / float64(w)
			r, g, b := br, bg, bb
			for _, bl := range blobs {
				dx := (fx - bl.x)
				dy := (fy - bl.y) * float64(h) / float64(w)
				d2 := (dx*dx + dy*dy) / (bl.r * bl.r)
				k := bl.a * math.Exp(-2.2*d2)
				r += (bl.cr - r) * k
				g += (bl.cg - g) * k
				b += (bl.cb - b) * k
			}
			n := (rng.Float64() - 0.5) * 3 // dither to avoid banding
			img.SetRGBA(x, y, color.RGBA{clamp8(r + n), clamp8(g + n), clamp8(b + n), 255})
		}
	}
	return img
}

func clamp8(v float64) uint8 {
	if v < 0 {
		return 0
	}
	if v > 255 {
		return 255
	}
	return uint8(v)
}
