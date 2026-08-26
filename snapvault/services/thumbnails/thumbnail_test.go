package main

import (
	"image"
	"testing"
)

func TestScaleToFit(t *testing.T) {
	cases := []struct {
		name         string
		w, h         int
		wantW, wantH int
	}{
		{"landscape shrinks on width", 1000, 500, 320, 160},
		{"portrait shrinks on height", 500, 1000, 160, 320},
		{"square shrinks evenly", 800, 800, 320, 320},
		{"already small is untouched", 100, 80, 100, 80},
		{"exactly at the cap is untouched", 320, 240, 320, 240},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			src := image.NewRGBA(image.Rect(0, 0, c.w, c.h))
			dst := scaleToFit(src, longestEdge)
			b := dst.Bounds()
			if b.Dx() != c.wantW || b.Dy() != c.wantH {
				t.Fatalf("scaleToFit(%dx%d) = %dx%d, want %dx%d",
					c.w, c.h, b.Dx(), b.Dy(), c.wantW, c.wantH)
			}
		})
	}
}
