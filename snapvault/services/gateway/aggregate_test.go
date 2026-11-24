package main

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// fakeUpstream returns a test server that answers every request with status and
// body, and records the last path it was asked for.
func fakeUpstream(t *testing.T, status int, body string, lastPath *string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if lastPath != nil {
			*lastPath = r.URL.EscapedPath()
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func detail(t *testing.T, feed, meta *httptest.Server, path string) *httptest.ResponseRecorder {
	t.Helper()
	g := &Gateway{
		log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		client:  &http.Client{Timeout: 5 * time.Second},
		feedURL: feed.URL,
		metaURL: meta.URL,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/images/{id}", g.imageDetail)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

const feedJSON = `{"id":"abc","owner":"u1","uploaded_at":"2026-01-01T00:00:00Z"}`

func TestImageDetail_feedStatusDecidesResponse(t *testing.T) {
	cases := []struct {
		name       string
		feedStatus int
		want       int
	}{
		{"missing image is a 404", http.StatusNotFound, http.StatusNotFound},
		{"feed error is a 502, not a 404", http.StatusInternalServerError, http.StatusBadGateway},
		{"feed unavailable is a 502", http.StatusServiceUnavailable, http.StatusBadGateway},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			feed := fakeUpstream(t, c.feedStatus, "", nil)
			meta := fakeUpstream(t, http.StatusOK, `{}`, nil)
			if rec := detail(t, feed, meta, "/api/images/abc"); rec.Code != c.want {
				t.Fatalf("status = %d, want %d", rec.Code, c.want)
			}
		})
	}
}

func TestImageDetail_degradesWithoutMetadata(t *testing.T) {
	feed := fakeUpstream(t, http.StatusOK, feedJSON, nil)
	meta := fakeUpstream(t, http.StatusInternalServerError, "", nil)

	rec := detail(t, feed, meta, "/api/images/abc")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got["id"] != "abc" || got["metadata_status"] != "unavailable" {
		t.Fatalf("body = %v, want id abc with metadata_status unavailable", got)
	}
}

// The id is a decoded path segment, so it can contain "/" or "?". It must
// reach the upstream as one escaped segment, not as extra path or a query.
func TestImageDetail_escapesID(t *testing.T) {
	var feedPath string
	feed := fakeUpstream(t, http.StatusNotFound, "", &feedPath)
	meta := fakeUpstream(t, http.StatusNotFound, "", nil)

	detail(t, feed, meta, "/api/images/..%2Fmetrics%3Fx=1")
	if want := "/feed/..%2Fmetrics%3Fx=1"; feedPath != want {
		t.Fatalf("feed was asked for %q, want %q", feedPath, want)
	}
}
