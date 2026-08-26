// Command images is Snapvault's upload-and-serve service. It stores image
// originals in object storage, serves originals and thumbnails back, and
// announces each upload on the IMAGES stream so the rest of the system can
// react.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
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

const maxUploadBytes = 25 << 20 // 25 MiB

type server struct {
	blob storage.Blob
	js   jetstream.JetStream
	log  *slog.Logger
}

func main() {
	log := svclog.New("images")
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

	nc, js, err := natsutil.Connect(config.MustString("NATS_URL"), "images")
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

	srv := &server{blob: blob, js: js, log: log}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /images", srv.upload)
	mux.HandleFunc("GET /images/{name}", serveImage(blob, "originals/"))
	mux.HandleFunc("GET /thumbs/{name}", serveImage(blob, "thumbs/"))
	mux.Handle("GET /metrics", metrics.Handler())
	httpx.Health(mux, func() bool { return nc.IsConnected() })

	var h http.Handler = mux
	h = metrics.Middleware(h)
	h = httpx.RequestID(h)
	h = otelhttp.NewHandler(h, "images")

	addr := ":" + config.String("PORT", "8080")
	if err := httpx.Serve(addr, h, log); err != nil {
		log.Error("server stopped", "err", err)
		os.Exit(1)
	}
}

// upload accepts a multipart form with an "image" file and an "owner" field,
// stores the original, and publishes images.uploaded.
func (s *server) upload(w http.ResponseWriter, r *http.Request) {
	log := httpx.LoggerFrom(r.Context(), s.log)

	r.Body = http.MaxBytesReader(w, r.Body, maxUploadBytes)
	if err := r.ParseMultipartForm(maxUploadBytes); err != nil {
		http.Error(w, "invalid upload", http.StatusBadRequest)
		return
	}
	file, hdr, err := r.FormFile("image")
	if err != nil {
		http.Error(w, "missing image field", http.StatusBadRequest)
		return
	}
	defer file.Close()

	owner := r.FormValue("owner")
	if owner == "" {
		owner = "anonymous"
	}

	id := newID()
	objectName := "originals/" + id
	contentType := hdr.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "application/octet-stream"
	}

	if err := s.blob.Put(r.Context(), objectName, contentType, file); err != nil {
		log.Error("store original failed", "err", err)
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}

	evt := events.ImageUploaded{
		ID:           id,
		Owner:        owner,
		ObjectName:   objectName,
		ContentType:  contentType,
		OriginalName: hdr.Filename,
		Size:         hdr.Size,
		UploadedAt:   time.Now().UTC(),
	}
	if err := s.publish(r.Context(), events.SubjectUploaded, evt); err != nil {
		// The bytes are safely stored; failing to announce them is the
		// two-write problem in miniature (see the messaging chapter). We log
		// loudly rather than fail the upload, since the original is not lost.
		log.Error("publish uploaded failed", "id", id, "err", err)
	}

	log.Info("image uploaded", "id", id, "owner", owner, "bytes", hdr.Size)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]string{"id": id})
}

// publish marshals evt and publishes it, injecting the current trace context
// into the message headers so consumers can continue the same trace.
func (s *server) publish(ctx context.Context, subject string, evt any) error {
	data, err := json.Marshal(evt)
	if err != nil {
		return err
	}
	msg := &nats.Msg{Subject: subject, Header: nats.Header{}, Data: data}
	otel.GetTextMapPropagator().Inject(ctx, telemetry.NatsHeaderCarrier(msg.Header))
	_, err = s.js.PublishMsg(ctx, msg)
	return err
}

// serveImage streams the object at prefix+{name}, sniffing its content type.
func serveImage(blob storage.Blob, prefix string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key := prefix + r.PathValue("name")
		rc, err := blob.Get(r.Context(), key)
		if err != nil {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		defer rc.Close()

		buf := bufio.NewReader(rc)
		if peek, _ := buf.Peek(512); len(peek) > 0 {
			w.Header().Set("Content-Type", http.DetectContentType(peek))
		}
		w.Header().Set("Cache-Control", "public, max-age=86400")
		_, _ = io.Copy(w, buf)
	}
}
