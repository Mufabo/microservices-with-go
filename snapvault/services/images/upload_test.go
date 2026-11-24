package main

import (
	"bytes"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"strings"
	"testing"

	"codeberg.org/example/snapvault/internal/platform/httpx"
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

// An HTML page dressed up as a JPEG must be refused before it is stored:
// served back from the gateway's origin it could run script there.
func TestUpload_rejectsNonImage(t *testing.T) {
	blob := &fakeBlob{objects: map[string]string{}}
	s := &server{blob: blob, log: slog.New(slog.NewTextHandler(io.Discard, nil))}

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	hdr := textproto.MIMEHeader{}
	hdr.Set("Content-Disposition", `form-data; name="image"; filename="photo.jpg"`)
	hdr.Set("Content-Type", "image/jpeg") // the client's claim, which is a lie
	part, err := mw.CreatePart(hdr)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = part.Write([]byte("<!doctype html><script>alert(document.domain)</script>"))
	_ = mw.Close()

	r := httptest.NewRequest(http.MethodPost, "/images", &body)
	r.Header.Set("Content-Type", mw.FormDataContentType())
	r.Header.Set(httpx.UserHeader, "user-1")
	rec := httptest.NewRecorder()
	s.upload(rec, r)

	if rec.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("status = %d, want 415", rec.Code)
	}
	if len(blob.objects) != 0 {
		t.Fatalf("stored %d object(s) for a non-image upload", len(blob.objects))
	}
}
