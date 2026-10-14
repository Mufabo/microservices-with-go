package main

import (
	"bytes"
	"fmt"
	"image"
	_ "image/gif" // register the GIF decoder
	"image/jpeg"  // JPEG encoder (and registers the JPEG decoder)
	_ "image/png" // register the PNG decoder
	"io"

	"golang.org/x/image/draw"
)

// longestEdge caps a thumbnail's longest side, in pixels.
const longestEdge = 320

// generateThumbnail decodes an image from r, scales it to fit within
// longestEdge on its longest side (preserving aspect ratio), and re-encodes it
// as JPEG. It returns the encoded bytes and the thumbnail's dimensions.
func generateThumbnail(r io.Reader) (data []byte, width, height int, err error) {
	src, _, err := image.Decode(r)
	if err != nil {
		return nil, 0, 0, fmt.Errorf("decode: %w", err)
	}
	dst := scaleToFit(src, longestEdge)

	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, dst, &jpeg.Options{Quality: 82}); err != nil {
		return nil, 0, 0, fmt.Errorf("encode: %w", err)
	}
	b := dst.Bounds()
	return buf.Bytes(), b.Dx(), b.Dy(), nil
}

// scaleToFit returns src scaled so its longest side is at most maxEdge,
// preserving aspect ratio. Images already small enough are returned unchanged.
func scaleToFit(src image.Image, maxEdge int) image.Image {
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	if w <= maxEdge && h <= maxEdge {
		return src
	}

	var nw, nh int
	if w >= h {
		nw = maxEdge
		nh = int(float64(h) * float64(maxEdge) / float64(w))
	} else {
		nh = maxEdge
		nw = int(float64(w) * float64(maxEdge) / float64(h))
	}
	if nw < 1 {
		nw = 1
	}
	if nh < 1 {
		nh = 1
	}

	dst := image.NewRGBA(image.Rect(0, 0, nw, nh))
	draw.CatmullRom.Scale(dst, dst.Bounds(), src, b, draw.Over, nil)
	return dst
}
