// Command thumbnails is Snapvault's background worker. It has no business HTTP
// surface: it consumes images.uploaded from JetStream, generates a thumbnail,
// stores it, and announces images.thumbnailed. A minimal health-only HTTP
// listener exists solely so Kubernetes can probe it.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"sync/atomic"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"go.opentelemetry.io/otel"

	"codeberg.org/example/snapvault/internal/events"
	"codeberg.org/example/snapvault/internal/platform/config"
	"codeberg.org/example/snapvault/internal/platform/httpx"
	svclog "codeberg.org/example/snapvault/internal/platform/log"
	"codeberg.org/example/snapvault/internal/platform/messaging/natsutil"
	"codeberg.org/example/snapvault/internal/platform/metrics"
	"codeberg.org/example/snapvault/internal/platform/storage"
	"codeberg.org/example/snapvault/internal/platform/telemetry"
)

type worker struct {
	blob storage.Blob
	js   jetstream.JetStream
	log  *slog.Logger
}

// consuming is 1 once the consumer is attached; readiness reflects it so a
// worker that has lost its consumer reports "not ready".
var consuming int32

func main() {
	log := svclog.New("thumbnails")
	ctx := context.Background()

	shutdown, err := telemetry.Init(ctx, config.String("GOOGLE_CLOUD_PROJECT", ""))
	if err != nil {
		log.Error("telemetry init", "err", err)
		os.Exit(1)
	}
	defer func() { _ = shutdown(ctx) }()

	blob, err := storage.NewFromEnv(ctx)
	if err != nil {
		log.Error("storage init", "err", err)
		os.Exit(1)
	}

	nc, js, err := natsutil.Connect(config.MustString("NATS_URL"), "thumbnails")
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

	w := &worker{blob: blob, js: js, log: log}

	stream, err := js.Stream(ctx, events.StreamName)
	if err != nil {
		log.Error("open stream", "err", err)
		os.Exit(1)
	}
	cons, err := stream.CreateOrUpdateConsumer(ctx, jetstream.ConsumerConfig{
		Durable:       "thumbnailer",
		FilterSubject: events.SubjectUploaded,
		AckPolicy:     jetstream.AckExplicitPolicy,
		AckWait:       30 * time.Second,
		MaxDeliver:    5,
	})
	if err != nil {
		log.Error("create consumer", "err", err)
		os.Exit(1)
	}

	cc, err := cons.Consume(w.handle)
	if err != nil {
		log.Error("start consume", "err", err)
		os.Exit(1)
	}
	defer cc.Stop()
	atomic.StoreInt32(&consuming, 1)
	log.Info("thumbnailer consuming", "subject", events.SubjectUploaded)

	// Health-only listener for Kubernetes probes (the worker serves no
	// business traffic). httpx.Serve blocks until SIGINT/SIGTERM; the consumer
	// keeps running in the background until the deferred cc.Stop runs.
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", metrics.Handler())
	httpx.Health(mux, func() bool {
		return atomic.LoadInt32(&consuming) == 1 && nc.IsConnected()
	})
	addr := ":" + config.String("HEALTH_PORT", "8081")
	if err := httpx.Serve(addr, mux, log); err != nil {
		log.Error("health server stopped", "err", err)
		os.Exit(1)
	}
}

// handle processes one images.uploaded event: download, resize, store,
// announce. Errors are classified — transient failures are Nak'd for
// redelivery, permanent ones (undecodable data) are Term'd so they don't loop.
func (w *worker) handle(msg jetstream.Msg) {
	ctx := otel.GetTextMapPropagator().Extract(context.Background(),
		telemetry.NatsHeaderCarrier(msg.Headers()))
	ctx, span := otel.Tracer("thumbnails").Start(ctx, "generate thumbnail")
	defer span.End()

	var evt events.ImageUploaded
	if err := json.Unmarshal(msg.Data(), &evt); err != nil {
		w.log.Error("undecodable message; terminating", "err", err)
		_ = msg.Term()
		return
	}
	log := w.log.With("id", evt.ID)

	src, err := w.blob.Get(ctx, evt.ObjectName)
	if err != nil {
		log.Error("download original failed; will retry", "err", err)
		_ = msg.Nak()
		return
	}
	defer src.Close()

	thumb, width, height, err := generateThumbnail(src)
	if err != nil {
		// A corrupt or unsupported image won't decode on retry either.
		log.Error("thumbnail generation failed; terminating", "err", err)
		_ = msg.Term()
		return
	}

	thumbObject := "thumbs/" + evt.ID + ".jpg"
	if err := w.blob.Put(ctx, thumbObject, "image/jpeg", bytes.NewReader(thumb)); err != nil {
		log.Error("store thumbnail failed; will retry", "err", err)
		_ = msg.Nak()
		return
	}

	done := events.ImageThumbnailed{
		ID:          evt.ID,
		ThumbObject: thumbObject,
		Width:       width,
		Height:      height,
		CreatedAt:   time.Now().UTC(),
	}
	if err := w.publish(ctx, events.SubjectThumbnailed, done); err != nil {
		log.Error("publish thumbnailed failed; will retry", "err", err)
		_ = msg.Nak()
		return
	}

	log.Info("thumbnail generated", "object", thumbObject, "w", width, "h", height)
	_ = msg.Ack()
}

func (w *worker) publish(ctx context.Context, subject string, evt any) error {
	data, err := json.Marshal(evt)
	if err != nil {
		return err
	}
	msg := &nats.Msg{Subject: subject, Header: nats.Header{}, Data: data}
	otel.GetTextMapPropagator().Inject(ctx, telemetry.NatsHeaderCarrier(msg.Header))
	_, err = w.js.PublishMsg(ctx, msg)
	return err
}
