// Package metrics provides Prometheus instrumentation: a RED-metrics HTTP
// middleware (Rate, Errors, Duration from one latency histogram) and the
// /metrics exposition handler that Google Managed Service for Prometheus
// scrapes.
package metrics

import (
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var httpDuration = promauto.NewHistogramVec(
	prometheus.HistogramOpts{
		Name:    "http_request_duration_seconds",
		Help:    "Latency of HTTP requests.",
		Buckets: prometheus.DefBuckets,
	},
	[]string{"route", "method", "status"},
)

// Middleware records rate, errors, and duration for every request, labelled by
// the matched route pattern (not the raw path) so high-cardinality IDs don't
// explode the metric series.
func Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		start := time.Now()
		next.ServeHTTP(rec, r)
		httpDuration.WithLabelValues(
			routeOf(r), r.Method, strconv.Itoa(rec.status),
		).Observe(time.Since(start).Seconds())
	})
}

// Handler serves the Prometheus exposition endpoint (wire it at /metrics).
func Handler() http.Handler { return promhttp.Handler() }

// routeOf returns the matched ServeMux pattern (e.g. "/images/{id}"), falling
// back to the raw path. Using the pattern keeps per-ID paths from each
// becoming their own metric series.
func routeOf(r *http.Request) string {
	if p := r.Pattern; p != "" {
		return p
	}
	return r.URL.Path
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(c int) {
	s.status = c
	s.ResponseWriter.WriteHeader(c)
}
