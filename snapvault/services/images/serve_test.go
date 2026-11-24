package main

import (
	"bytes"
	"context"
	"image"
	pngenc "image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// fakeBlob is an in-memory storage.Blob for tests — no disk, no cloud.
type fakeBlob struct{ objects map[string]string }

func (f *fakeBlob) Get(_ context.Context, key string) (io.ReadCloser, error) {
	data, ok := f.objects[key]
	if !ok {
		return nil, os.ErrNotExist
	}
	return io.NopCloser(strings.NewReader(data)), nil
}

func (f *fakeBlob) Put(_ context.Context, key, _ string, r io.Reader) error {
	b, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	f.objects[key] = string(b)
	return nil
}

func TestServeImage_servesStoredObject(t *testing.T) {
	blob := &fakeBlob{objects: map[string]string{"originals/abc": "the-bytes"}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /images/{name}", serveImage(blob, "originals/"))

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/images/abc", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := rec.Body.String(); got != "the-bytes" {
		t.Fatalf("body = %q, want %q", got, "the-bytes")
	}
}

func TestServeImage_notFound(t *testing.T) {
	blob := &fakeBlob{objects: map[string]string{}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /images/{name}", serveImage(blob, "originals/"))

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/images/missing", nil))

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

// Whatever is in storage, serveImage declares only image types, so stored
// HTML (from before uploads were validated) can't render on our origin.
func TestServeImage_contentTypes(t *testing.T) {
	var png bytes.Buffer
	if err := pngenc.Encode(&png, image.NewRGBA(image.Rect(0, 0, 1, 1))); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name, data, wantType string
	}{
		{"png is declared as png", png.String(), "image/png"},
		{"html is not declared as html", "<!doctype html><script>alert(1)</script>", "application/octet-stream"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			blob := &fakeBlob{objects: map[string]string{"originals/x": c.data}}
			mux := http.NewServeMux()
			mux.HandleFunc("GET /images/{name}", serveImage(blob, "originals/"))

			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/images/x", nil))

			if got := rec.Header().Get("Content-Type"); got != c.wantType {
				t.Errorf("Content-Type = %q, want %q", got, c.wantType)
			}
			if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
				t.Errorf("X-Content-Type-Options = %q, want nosniff", got)
			}
		})
	}
}
