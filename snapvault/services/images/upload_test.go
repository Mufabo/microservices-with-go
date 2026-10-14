package main

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// An upload that didn't come through the gateway's authentication carries no
// user header, and must be refused before anything is stored or published.
func TestUpload_requiresVerifiedUser(t *testing.T) {
	blob := &fakeBlob{objects: map[string]string{}}
	s := &server{blob: blob, log: slog.New(slog.NewTextHandler(io.Discard, nil))}

	r := httptest.NewRequest(http.MethodPost, "/images", strings.NewReader("ignored"))
	rec := httptest.NewRecorder()
	s.upload(rec, r)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if len(blob.objects) != 0 {
		t.Fatalf("stored %d object(s) for an unauthenticated upload", len(blob.objects))
	}
}
