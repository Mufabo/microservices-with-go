package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sync"
	"time"

	"codeberg.org/example/snapvault/internal/platform/httpx"
)

// The gateway decodes only the fields it needs and deliberately does not
// import the feed or metadata packages: services stay decoupled, and the
// gateway depends on the JSON shape, not on another service's types.

type feedItem struct {
	ID          string `json:"id"`
	Owner       string `json:"owner"`
	ThumbObject string `json:"thumb_object,omitempty"`
	Width       int    `json:"width,omitempty"`
	Height      int    `json:"height,omitempty"`
	UploadedAt  string `json:"uploaded_at"`
}

type imageMeta struct {
	ContentType  string   `json:"content_type"`
	OriginalName string   `json:"original_name"`
	Size         int64    `json:"size"`
	Tags         []string `json:"tags,omitempty"`
}

// errUpstreamNotFound reports that an upstream service answered 404.
var errUpstreamNotFound = errors.New("upstream: not found")

// imageDetail aggregates two upstream calls into one response. The feed entry
// is essential — without it there is nothing to show, so its absence is a 404,
// and a feed that can't answer at all (timeout, 5xx) is a 502: the image may
// well exist, and telling the client otherwise would be a lie.
// Metadata is enrichment: if it is slow or down, the response still returns,
// marked so the client knows the enrichment is missing. The two calls run
// concurrently so the response is as fast as the slower essential call.
func (g *Gateway) imageDetail(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	log := httpx.LoggerFrom(r.Context(), g.log)

	var (
		wg      sync.WaitGroup
		fi      *feedItem
		feedErr error
		meta    *imageMeta
		metaOK  bool
	)

	wg.Add(1)
	go func() {
		defer wg.Done()
		fi, feedErr = g.fetchFeed(r.Context(), id)
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		// Enrichment gets its own short deadline so a slow metadata service
		// can't hold up the whole response.
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		m, err := g.fetchMeta(ctx, id)
		if err != nil {
			log.Warn("metadata unavailable; degrading", "id", id, "err", err)
			return
		}
		meta, metaOK = m, true
	}()

	wg.Wait()

	switch {
	case errors.Is(feedErr, errUpstreamNotFound):
		http.Error(w, "not found", http.StatusNotFound)
		return
	case feedErr != nil:
		log.Error("feed unavailable", "id", id, "err", feedErr)
		http.Error(w, "feed unavailable", http.StatusBadGateway)
		return
	}

	resp := map[string]any{
		"id":           fi.ID,
		"owner":        fi.Owner,
		"thumb_object": fi.ThumbObject,
		"width":        fi.Width,
		"height":       fi.Height,
		"uploaded_at":  fi.UploadedAt,
	}
	if metaOK {
		resp["content_type"] = meta.ContentType
		resp["original_name"] = meta.OriginalName
		resp["size"] = meta.Size
		resp["tags"] = meta.Tags
		resp["metadata_status"] = "ok"
	} else {
		resp["metadata_status"] = "unavailable"
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func (g *Gateway) fetchFeed(ctx context.Context, id string) (*feedItem, error) {
	var fi feedItem
	if err := g.getJSON(ctx, g.feedURL+"/feed/"+url.PathEscape(id), &fi); err != nil {
		return nil, err
	}
	return &fi, nil
}

func (g *Gateway) fetchMeta(ctx context.Context, id string) (*imageMeta, error) {
	var m imageMeta
	if err := g.getJSON(ctx, g.metaURL+"/metadata/"+url.PathEscape(id), &m); err != nil {
		return nil, err
	}
	return &m, nil
}

func (g *Gateway) getJSON(ctx context.Context, rawURL string, dst any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return err
	}
	if id := httpx.RequestIDFrom(ctx); id != "" {
		req.Header.Set("X-Request-Id", id)
	}
	resp, err := g.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return errUpstreamNotFound
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("upstream %s returned %d", rawURL, resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(dst)
}
