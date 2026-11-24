package main

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	_ "image/gif" // register the GIF decoder
	"image/jpeg"  // JPEG encoder (and registers the JPEG decoder)
	_ "image/png" // register the PNG decoder

	"golang.org/x/image/draw"
)

// longestEdge caps a thumbnail's longest side, in pixels.
const longestEdge = 320

// maxPixels caps the size of image the worker will decode. Decoding allocates
// the whole bitmap up front, up to 4 bytes a pixel, so a file of a few
// kilobytes that declares itself 50000x50000 would ask for 10 GB and get the
// Pod OOM-killed, again on every redelivery. 40 megapixels is a 160 MB bitmap,
// well inside the worker's 512 Mi limit, and larger than most camera photos.
const maxPixels = 40_000_000

// errTooLarge reports an image whose declared dimensions exceed maxPixels.
var errTooLarge = errors.New("image too large to decode")

// generateThumbnail decodes the image in original, scales it to fit within
// longestEdge on its longest side (preserving aspect ratio), and re-encodes it
// as JPEG. It returns the encoded bytes and the thumbnail's dimensions. It
// reads the image's header first and refuses anything over maxPixels before
// allocating the bitmap.
func generateThumbnail(original []byte) (data []byte, width, height int, err error) {
	cfg, _, err := image.DecodeConfig(bytes.NewReader(original))
	if err != nil {
		return nil, 0, 0, fmt.Errorf("decode header: %w", err)
	}
	if int64(cfg.Width)*int64(cfg.Height) > maxPixels {
		return nil, 0, 0, fmt.Errorf("%w: %dx%d", errTooLarge, cfg.Width, cfg.Height)
	}

	src, _, err := image.Decode(bytes.NewReader(original))
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
