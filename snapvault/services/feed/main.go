// Command feed builds the public gallery in PostgreSQL. It consumes the whole
// images.> subject space: images.uploaded creates a feed row, and
// images.thumbnailed attaches the thumbnail to it. It serves the gallery over
// HTTP.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/nats-io/nats.go/jetstream"
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

const defaultFeedLimit = 50

type service struct {
	store *feedStore
	log   *slog.Logger
}

func main() {
	log := svclog.New("feed")
	ctx := context.Background()

	shutdown, err := telemetry.Init(ctx, config.String("GOOGLE_CLOUD_PROJECT", ""))
	if err != nil {
		log.Error("telemetry init", "err", err)
		os.Exit(1)
	}
	defer func() { _ = shutdown(ctx) }()

	store, err := newFeedStore(ctx, config.FileOrEnv("DATABASE_URL"))
	if err != nil {
		log.Error("postgres connect", "err", err)
		os.Exit(1)
	}
	defer store.Close()

	nc, js, err := natsutil.Connect(config.MustString("NATS_URL"), "feed")
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
	// The feed reacts to both events, so it filters on the whole subject space.
	cons, err := stream.CreateOrUpdateConsumer(ctx, jetstream.ConsumerConfig{
		Durable:       "feed",
		FilterSubject: events.SubjectWildcard,
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
	mux.HandleFunc("GET /feed", svc.list)
	mux.HandleFunc("GET /feed/{id}", svc.getOne)
	mux.Handle("GET /metrics", metrics.Handler())
	httpx.Health(mux, func() bool { return svc.store.Ping(ctx) == nil })

	var h http.Handler = mux
	h = metrics.Middleware(h)
	h = httpx.RequestID(h)
	h = otelhttp.NewHandler(h, "feed")

	addr := ":" + config.String("PORT", "8080")
	if err := httpx.Serve(addr, h, log); err != nil {
		log.Error("server stopped", "err", err)
		os.Exit(1)
	}
}

// handle dispatches on the message subject. Events arrive in stream order, so
// images.uploaded (which creates the row) is always seen before the matching
// images.thumbnailed (which fills in the thumbnail).
func (s *service) handle(msg jetstream.Msg) {
	ctx := otel.GetTextMapPropagator().Extract(context.Background(),
		telemetry.NatsHeaderCarrier(msg.Headers()))

	switch msg.Subject() {
	case events.SubjectUploaded:
		var evt events.ImageUploaded
		if err := json.Unmarshal(msg.Data(), &evt); err != nil {
			s.log.Error("undecodable uploaded event; terminating", "err", err)
			_ = msg.Term()
			return
		}
		if err := s.store.Add(ctx, FeedItem{
			ID: evt.ID, Owner: evt.Owner, UploadedAt: evt.UploadedAt,
		}); err != nil {
			s.log.Error("add feed item failed; will retry", "id", evt.ID, "err", err)
			_ = msg.Nak()
			return
		}
		s.log.Info("feed item added", "id", evt.ID)

	case events.SubjectThumbnailed:
		var evt events.ImageThumbnailed
		if err := json.Unmarshal(msg.Data(), &evt); err != nil {
			s.log.Error("undecodable thumbnailed event; terminating", "err", err)
			_ = msg.Term()
			return
		}
		if err := s.store.SetThumb(ctx, evt.ID, evt.ThumbObject, evt.Width, evt.Height); err != nil {
			s.log.Error("attach thumbnail failed; will retry", "id", evt.ID, "err", err)
			_ = msg.Nak()
			return
		}
		s.log.Info("feed thumbnail attached", "id", evt.ID)
	}
	_ = msg.Ack()
}

func (s *service) list(w http.ResponseWriter, r *http.Request) {
	items, err := s.store.Recent(r.Context(), defaultFeedLimit)
	if err != nil {
		httpx.LoggerFrom(r.Context(), s.log).Error("list feed failed", "err", err)
		http.Error(w, "error", http.StatusInternalServerError)
		return
	}
	if items == nil {
		items = []FeedItem{}
	}
	writeJSON(w, items)
}

func (s *service) getOne(w http.ResponseWriter, r *http.Request) {
	item, err := s.store.Get(r.Context(), r.PathValue("id"))
	if errors.Is(err, pgx.ErrNoRows) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if err != nil {
		httpx.LoggerFrom(r.Context(), s.log).Error("get feed item failed", "err", err)
		http.Error(w, "error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, item)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
