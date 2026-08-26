// Package telemetry wires OpenTelemetry tracing and exports spans to Google
// Cloud Trace. It also provides a carrier that lets trace context survive a
// trip through NATS message headers, so a single upload's trace spans the
// synchronous HTTP hops *and* the asynchronous thumbnailing that follows.
package telemetry

import (
	"context"

	texporter "github.com/GoogleCloudPlatform/opentelemetry-operations-go/exporter/trace"
	"github.com/nats-io/nats.go"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// Init configures the W3C trace-context propagator and, when projectID is
// non-empty, a Cloud Trace exporter with parent-based 10% sampling. When
// projectID is empty (typical for local development) it installs the
// propagator only and leaves the global no-op tracer in place, so the same
// binary runs locally without Google Cloud credentials. The returned function
// flushes spans on shutdown.
func Init(ctx context.Context, projectID string) (func(context.Context) error, error) {
	otel.SetTextMapPropagator(propagation.TraceContext{})

	if projectID == "" {
		return func(context.Context) error { return nil }, nil
	}

	exp, err := texporter.New(texporter.WithProjectID(projectID))
	if err != nil {
		return nil, err
	}
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exp),
		// Keep 10% of traces, but keep each trace whole: if the edge sampled a
		// request, every downstream span for it is kept too.
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(0.1))),
	)
	otel.SetTracerProvider(tp)
	return tp.Shutdown, nil
}

// NatsHeaderCarrier adapts NATS message headers to OpenTelemetry's
// TextMapCarrier so trace context can be injected on publish and extracted on
// consume.
type NatsHeaderCarrier nats.Header

func (c NatsHeaderCarrier) Get(key string) string { return nats.Header(c).Get(key) }
func (c NatsHeaderCarrier) Set(key, val string)   { nats.Header(c).Set(key, val) }

func (c NatsHeaderCarrier) Keys() []string {
	keys := make([]string, 0, len(c))
	for k := range c {
		keys = append(keys, k)
	}
	return keys
}

// Compile-time check that the carrier satisfies the propagation interface.
var _ propagation.TextMapCarrier = NatsHeaderCarrier{}
