//go:build e2e

// Package e2e exercises the whole running system through the public gateway,
// exactly as a client would. Run it against a live stack:
//
//	docker compose up -d --build
//	GATEWAY_URL=http://localhost:8080 go test -tags=e2e ./test/e2e/...
package e2e

import (
	"bytes"
	"encoding/json"
	"image"
	"image/png"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"testing"
	"time"
)

// upload posts data as the multipart "image" field and returns the response.
func upload(t *testing.T, base, filename string, data []byte) *http.Response {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	part, err := mw.CreateFormFile("image", filename)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = part.Write(data)
	_ = mw.Close()

	resp, err := http.Post(base+"/api/images", mw.FormDataContentType(), &body)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func gatewayURL(t *testing.T) string {
	base := os.Getenv("GATEWAY_URL") // e.g. http://localhost:8080
	if base == "" {
		t.Skip("set GATEWAY_URL to run end-to-end tests")
	}
	return base
}

// An upload travels the whole pipeline: images stores it and publishes
// images.uploaded, thumbnails makes a thumbnail and publishes
// images.thumbnailed, and feed records both. The system is eventually
// consistent, so the test polls the feed rather than expecting it at once.
func TestUploadAppearsInFeed(t *testing.T) {
	base := gatewayURL(t)

	var img bytes.Buffer
	if err := png.Encode(&img, image.NewRGBA(image.Rect(0, 0, 640, 480))); err != nil {
		t.Fatal(err)
	}
	resp := upload(t, base, "test.png", img.Bytes())
	var up struct{ ID string }
	_ = json.NewDecoder(resp.Body).Decode(&up)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated || up.ID == "" {
		t.Fatalf("upload: status %d, id %q; want 201 with an id", resp.StatusCode, up.ID)
	}

	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if r, err := http.Get(base + "/api/feed"); err == nil {
			var items []struct {
				ID          string `json:"id"`
				ThumbObject string `json:"thumb_object"`
				Width       int    `json:"width"`
				Height      int    `json:"height"`
			}
			b, _ := io.ReadAll(r.Body)
			r.Body.Close()
			_ = json.Unmarshal(b, &items)
			for _, it := range items {
				if it.ID == up.ID && it.ThumbObject != "" {
					if it.Width != 320 || it.Height != 240 {
						t.Fatalf("thumbnail is %dx%d, want 320x240", it.Width, it.Height)
					}
					return // uploaded, thumbnailed, and in the feed
				}
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("upload %s did not appear in the feed with a thumbnail in time", up.ID)
}

// A file that isn't an image is refused at the edge, whatever it's called.
func TestUploadRejectsNonImage(t *testing.T) {
	base := gatewayURL(t)

	resp := upload(t, base, "photo.jpg", []byte("<!doctype html><script>alert(1)</script>"))
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnsupportedMediaType {
		t.Fatalf("status = %d, want 415", resp.StatusCode)
	}
}
