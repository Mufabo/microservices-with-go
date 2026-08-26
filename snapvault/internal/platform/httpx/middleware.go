package httpx

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net/http"
)

type ctxKey string

const requestIDKey ctxKey = "request_id"

// RequestID ensures every request has an ID (reusing an inbound X-Request-Id
// if present), stores it on the request context, and echoes it back on the
// response. The gateway generates the ID at the edge and forwards it on its
// proxied calls; downstream services reuse it, so a single log query on
// request_id returns the whole story of one request across services.
func RequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-Id")
		if id == "" {
			b := make([]byte, 8)
			_, _ = rand.Read(b)
			id = hex.EncodeToString(b)
		}
		ctx := context.WithValue(r.Context(), requestIDKey, id)
		w.Header().Set("X-Request-Id", id)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// RequestIDFrom returns the request ID stored on ctx, or "" if none.
func RequestIDFrom(ctx context.Context) string {
	if id, ok := ctx.Value(requestIDKey).(string); ok {
		return id
	}
	return ""
}

// LoggerFrom returns a logger pre-tagged with the request ID on ctx.
func LoggerFrom(ctx context.Context, base *slog.Logger) *slog.Logger {
	if id := RequestIDFrom(ctx); id != "" {
		return base.With("request_id", id)
	}
	return base
}
