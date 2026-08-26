// Package natsutil wraps the NATS connection and JetStream stream setup shared
// by every Snapvault service. It is infrastructure only: it knows how to talk
// to NATS, not what any subject means. Callers pass the subjects to capture,
// which keeps this package free of domain knowledge.
package natsutil

import (
	"context"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// Connect dials NATS with sensible production defaults — infinite reconnects
// and a steady reconnect wait — and returns both the core connection and a
// JetStream context.
func Connect(url, name string) (*nats.Conn, jetstream.JetStream, error) {
	nc, err := nats.Connect(url,
		nats.Name(name),
		nats.MaxReconnects(-1),
		nats.ReconnectWait(2*time.Second),
	)
	if err != nil {
		return nil, nil, err
	}
	js, err := jetstream.New(nc)
	if err != nil {
		nc.Close()
		return nil, nil, err
	}
	return nc, js, nil
}

// EnsureStream creates or updates the named stream to capture subjects. It is
// idempotent, so every service can call it on startup and start order stops
// mattering — there is no separate "set up the broker" step that must run
// first.
//
// For production resilience, set Replicas to 3 (requires a NATS cluster) so a
// node failure loses neither the broker nor its messages.
func EnsureStream(ctx context.Context, js jetstream.JetStream, name string, subjects []string) error {
	_, err := js.CreateOrUpdateStream(ctx, jetstream.StreamConfig{
		Name:      name,
		Subjects:  subjects,
		Storage:   jetstream.FileStorage,
		Retention: jetstream.LimitsPolicy,
		MaxAge:    7 * 24 * time.Hour,
		// Replicas: 3, // enable on a clustered NATS deployment
	})
	return err
}
