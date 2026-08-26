package main

import (
	"context"
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
