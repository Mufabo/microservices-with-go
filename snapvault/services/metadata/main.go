// Command metadata maintains a MongoDB document per image, built from the
// images.uploaded event stream, and exposes a small read API to fetch metadata
// by id or list images by tag.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/nats-io/nats.go/jetstream"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"

	"codeberg.org/example/snapvault/internal/events"
	"codeberg.org/example/snapvault/internal/platform/config"
	"codeberg.org/example/snapvault/internal/platform/httpx"
	svclog "codeberg.org/example/snapvault/internal/platform/log"
	"codeberg.org/example/snapvault/internal/platform/messaging/natsutil"
	"codeberg.org/example/snapvault/internal/platform/metrics"
	"codeberg.org/example/snapvault/internal/platform/telemetry"
)

type service struct {
	store *metaStore
	log   *slog.Logger
}

func main() {
	log := svclog.New("metadata")
	ctx := context.Background()

	shutdown, err := telemetry.Init(ctx, config.String("GOOGLE_CLOUD_PROJECT", ""))
	if err != nil {
		log.Error("telemetry init", "err", err)
		os.Exit(1)
	}
	defer func() { _ = shutdown(ctx) }()

	store, err := newMetaStore(ctx,
		config.MustString("MONGO_URI"),
		config.String("MONGO_DB", "snapvault"))
	if err != nil {
		log.Error("mongo connect", "err", err)
		os.Exit(1)
	}
	defer func() { _ = store.Close(ctx) }()

	nc, js, err := natsutil.Connect(config.MustString("NATS_URL"), "metadata")
	if err != nil {
		log.Error("nats connect", "err", err)
		os.Exit(1)
	}
	defer func() { _ = nc.Drain() }()

	if err := natsutil.EnsureStream(ctx, js, events.StreamName,
		[]string{events.SubjectWildcard}); err != nil {
		log.Error("ensure stream", "err", err)
		os.Exit(1)
	}

	svc := &service{store: store, log: log}

	stream, err := js.Stream(ctx, events.StreamName)
	if err != nil {
		log.Error("open stream", "err", err)
		os.Exit(1)
	}
	cons, err := stream.CreateOrUpdateConsumer(ctx, jetstream.ConsumerConfig{
		Durable:       "metadata",
		FilterSubject: events.SubjectUploaded,
		AckPolicy:     jetstream.AckExplicitPolicy,
		AckWait:       30 * time.Second,
		MaxDeliver:    5,
	})
	if err != nil {
		log.Error("create consumer", "err", err)
		os.Exit(1)
	}
	cc, err := cons.Consume(svc.handle)
	if err != nil {
		log.Error("start consume", "err", err)
		os.Exit(1)
	}
	defer cc.Stop()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /metadata/{id}", svc.getOne)
	mux.HandleFunc("GET /metadata", svc.byTag)
	mux.Handle("GET /metrics", metrics.Handler())
	httpx.Health(mux, func() bool { return svc.store.Ping(ctx) == nil })

	var h http.Handler = mux
	h = metrics.Middleware(h)
	h = httpx.RequestID(h)
	h = otelhttp.NewHandler(h, "metadata")

	addr := ":" + config.String("PORT", "8080")
	if err := httpx.Serve(addr, h, log); err != nil {
		log.Error("server stopped", "err", err)
		os.Exit(1)
	}
}

func (s *service) handle(msg jetstream.Msg) {
	ctx := otel.GetTextMapPropagator().Extract(context.Background(),
		telemetry.NatsHeaderCarrier(msg.Headers()))

	var evt events.ImageUploaded
	if err := json.Unmarshal(msg.Data(), &evt); err != nil {
		s.log.Error("undecodable message; terminating", "err", err)
		_ = msg.Term()
		return
	}
	meta := ImageMeta{
		ID:           evt.ID,
		Owner:        evt.Owner,
		ContentType:  evt.ContentType,
		OriginalName: evt.OriginalName,
		Size:         evt.Size,
		Tags:         tagsFor(evt),
		UploadedAt:   evt.UploadedAt,
	}
	if err := s.store.Upsert(ctx, meta); err != nil {
		s.log.Error("upsert failed; will retry", "id", evt.ID, "err", err)
		_ = msg.Nak()
		return
	}
	s.log.Info("metadata stored", "id", evt.ID)
	_ = msg.Ack()
}

// tagsFor derives simple tags so the ?tag= query has something to match here
// we tag by media subtype, e.g. "jpeg" or "png".
func tagsFor(evt events.ImageUploaded) []string {
	ct := evt.ContentType
	if i := strings.IndexByte(ct, '/'); i >= 0 && i+1 < len(ct) {
		sub := ct[i+1:]
		if j := strings.IndexByte(sub, ';'); j >= 0 {
			sub = sub[:j]
		}
		if sub != "" {
			return []string{strings.ToLower(strings.TrimSpace(sub))}
		}
	}
	return nil
}

func (s *service) getOne(w http.ResponseWriter, r *http.Request) {
	m, err := s.store.Get(r.Context(), r.PathValue("id"))
	if errors.Is(err, mongo.ErrNoDocuments) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if err != nil {
		httpx.LoggerFrom(r.Context(), s.log).Error("get metadata failed", "err", err)
		http.Error(w, "error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, m)
}

func (s *service) byTag(w http.ResponseWriter, r *http.Request) {
	tag := r.URL.Query().Get("tag")
	if tag == "" {
		http.Error(w, "tag query parameter required", http.StatusBadRequest)
		return
	}
	items, err := s.store.ByTag(r.Context(), tag)
	if err != nil {
		httpx.LoggerFrom(r.Context(), s.log).Error("query by tag failed", "err", err)
		http.Error(w, "error", http.StatusInternalServerError)
		return
	}
	if items == nil {
		items = []ImageMeta{}
	}
	writeJSON(w, items)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
