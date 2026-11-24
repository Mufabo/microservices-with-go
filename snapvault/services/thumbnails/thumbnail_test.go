package main

import (
	"bytes"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"image"
	"image/png"
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

// A tiny file that declares enormous dimensions must be refused from its
// header, before the decoder allocates a bitmap for it.
func TestGenerateThumbnail_refusesHugeDimensions(t *testing.T) {
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewGray(image.Rect(0, 0, 1, 1))); err != nil {
		t.Fatal(err)
	}
	b := buf.Bytes()
	// Rewrite the IHDR chunk to claim 50000x50000 (a 10 GB RGBA bitmap) and
	// fix its CRC so the header still parses. IHDR's type starts at byte 12,
	// its data (width, height, ...) at 16, and its CRC follows 13 data bytes.
	binary.BigEndian.PutUint32(b[16:], 50000)
	binary.BigEndian.PutUint32(b[20:], 50000)
	binary.BigEndian.PutUint32(b[29:], crc32.ChecksumIEEE(b[12:29]))

	_, _, _, err := generateThumbnail(b)
	if !errors.Is(err, errTooLarge) {
		t.Fatalf("err = %v, want errTooLarge", err)
	}
}

func TestGenerateThumbnail_scalesAndEncodesJPEG(t *testing.T) {
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 1000, 500))); err != nil {
		t.Fatal(err)
	}
	data, w, h, err := generateThumbnail(buf.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if w != 320 || h != 160 {
		t.Fatalf("size = %dx%d, want 320x160", w, h)
	}
	if _, format, err := image.Decode(bytes.NewReader(data)); err != nil || format != "jpeg" {
		t.Fatalf("output decodes as %q, err %v; want jpeg", format, err)
	}
}
