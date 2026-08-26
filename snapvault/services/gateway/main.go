// Command gateway is Snapvault's single entry point. It proxies image bytes
// and the upload/feed endpoints to the right service, and serves an aggregated
// image-detail view that stitches together the feed and metadata services.
package main

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

	"codeberg.org/example/snapvault/internal/platform/config"
	"codeberg.org/example/snapvault/internal/platform/httpx"
	svclog "codeberg.org/example/snapvault/internal/platform/log"
	"codeberg.org/example/snapvault/internal/platform/metrics"
	"codeberg.org/example/snapvault/internal/platform/telemetry"
)

// Gateway holds the dependencies for the aggregation handler.
type Gateway struct {
	log     *slog.Logger
	client  *http.Client
	feedURL string
	metaURL string
}

func main() {
	log := svclog.New("gateway")
	ctx := context.Background()

	shutdown, err := telemetry.Init(ctx, config.String("GOOGLE_CLOUD_PROJECT", ""))
	if err != nil {
		log.Error("telemetry init", "err", err)
		os.Exit(1)
	}
	defer func() { _ = shutdown(ctx) }()

	imagesURL := mustURL(config.MustString("IMAGES_URL"))
	feedURL := mustURL(config.MustString("FEED_URL"))
	metaURL := config.MustString("METADATA_URL")

	g := &Gateway{
		log: log,
		client: &http.Client{
			Timeout:   10 * time.Second,
			Transport: otelhttp.NewTransport(http.DefaultTransport),
		},
		feedURL: feedURL.String(),
		metaURL: metaURL,
	}

	imagesProxy := httputil.NewSingleHostReverseProxy(imagesURL)
	feedProxy := httputil.NewSingleHostReverseProxy(feedURL)
	instrument(imagesProxy)
	instrument(feedProxy)

	mux := http.NewServeMux()

	// Image bytes are served straight from the images service.
	mux.Handle("GET /images/{name}", imagesProxy)
	mux.Handle("GET /thumbs/{name}", imagesProxy)

	// Public JSON/upload API.
	mux.HandleFunc("POST /api/images", func(w http.ResponseWriter, r *http.Request) {
		r.URL.Path = "/images"
		imagesProxy.ServeHTTP(w, r)
	})
	mux.HandleFunc("GET /api/feed", func(w http.ResponseWriter, r *http.Request) {
		r.URL.Path = "/feed"
		feedProxy.ServeHTTP(w, r)
	})
	mux.HandleFunc("GET /api/images/{id}", g.imageDetail)

	mux.HandleFunc("GET /", g.index)
	mux.Handle("GET /metrics", metrics.Handler())
	httpx.Health(mux, nil)

	var h http.Handler = mux
	h = metrics.Middleware(h)
	h = httpx.RequestID(h)
	h = otelhttp.NewHandler(h, "gateway")

	addr := ":" + config.String("PORT", "8080")
	if err := httpx.Serve(addr, h, log); err != nil {
		log.Error("server stopped", "err", err)
		os.Exit(1)
	}
}

// instrument makes a reverse proxy forward the request ID and propagate trace
// context to the upstream service.
func instrument(p *httputil.ReverseProxy) {
	orig := p.Director
	p.Director = func(r *http.Request) {
		orig(r)
		if id := httpx.RequestIDFrom(r.Context()); id != "" {
			r.Header.Set("X-Request-Id", id)
		}
	}
	p.Transport = otelhttp.NewTransport(http.DefaultTransport)
}

func mustURL(raw string) *url.URL {
	u, err := url.Parse(raw)
	if err != nil {
		panic("gateway: invalid URL " + raw + ": " + err.Error())
	}
	return u
}

func (g *Gateway) index(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte("Snapvault gateway. Try GET /api/feed or POST /api/images.\n"))
}
