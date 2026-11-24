//go:build integration

package natsutil

import (
	"context"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"
	"github.com/testcontainers/testcontainers-go"
	natstc "github.com/testcontainers/testcontainers-go/modules/nats"
)

func TestEnsureStream_RoundTrip(t *testing.T) {
	ctx := context.Background()

	container, err := natstc.Run(ctx, "nats:2.10")
	if err != nil {
		t.Fatalf("start nats: %v", err)
	}
	t.Cleanup(func() { _ = testcontainers.TerminateContainer(container) })

	url, err := container.ConnectionString(ctx)
	if err != nil {
		t.Fatalf("connection string: %v", err)
	}

	nc, js, err := Connect(url, "test")
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer func() { _ = nc.Drain() }()

	const stream = "TESTSTREAM"
	// EnsureStream must be idempotent: calling it twice is not an error.
	if err := EnsureStream(ctx, js, stream, []string{"test.>"}); err != nil {
		t.Fatalf("ensure stream: %v", err)
	}
	if err := EnsureStream(ctx, js, stream, []string{"test.>"}); err != nil {
		t.Fatalf("ensure stream (second call): %v", err)
	}

	if _, err := js.Publish(ctx, "test.hello", []byte("payload")); err != nil {
		t.Fatalf("publish: %v", err)
	}

	s, err := js.Stream(ctx, stream)
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}
	cons, err := s.CreateOrUpdateConsumer(ctx, jetstream.ConsumerConfig{
		Durable:   "testconsumer",
		AckPolicy: jetstream.AckExplicitPolicy,
	})
	if err != nil {
		t.Fatalf("create consumer: %v", err)
	}

	msgs, err := cons.Fetch(1, jetstream.FetchMaxWait(5*time.Second))
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	msg := <-msgs.Messages()
	if msg == nil {
		t.Fatal("no message received")
	}
	if got := string(msg.Data()); got != "payload" {
		t.Fatalf("data = %q, want %q", got, "payload")
	}
	_ = msg.Ack()
}

// NakWithBackoff must hold a failed message back for the first retry delay,
// not hand it straight back the way a plain Nak does.
func TestNakWithBackoff_delaysRedelivery(t *testing.T) {
	ctx := context.Background()

	container, err := natstc.Run(ctx, "nats:2.10")
	if err != nil {
		t.Fatalf("start nats: %v", err)
	}
	t.Cleanup(func() { _ = testcontainers.TerminateContainer(container) })

	url, err := container.ConnectionString(ctx)
	if err != nil {
		t.Fatalf("connection string: %v", err)
	}
	nc, js, err := Connect(url, "test")
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer func() { _ = nc.Drain() }()

	if err := EnsureStream(ctx, js, "BACKOFF", []string{"backoff.>"}); err != nil {
		t.Fatalf("ensure stream: %v", err)
	}
	if _, err := js.Publish(ctx, "backoff.x", []byte("payload")); err != nil {
		t.Fatalf("publish: %v", err)
	}
	cons, err := js.CreateOrUpdateConsumer(ctx, "BACKOFF", jetstream.ConsumerConfig{
		Durable:   "backoff",
		AckPolicy: jetstream.AckExplicitPolicy,
	})
	if err != nil {
		t.Fatalf("create consumer: %v", err)
	}

	fetchOne := func(wait time.Duration) jetstream.Msg {
		t.Helper()
		msgs, err := cons.Fetch(1, jetstream.FetchMaxWait(wait))
		if err != nil {
			t.Fatalf("fetch: %v", err)
		}
		return <-msgs.Messages()
	}

	msg := fetchOne(5 * time.Second)
	if msg == nil {
		t.Fatal("no message received")
	}
	nakked := time.Now()
	if err := NakWithBackoff(msg); err != nil {
		t.Fatalf("nak: %v", err)
	}

	if early := fetchOne(2 * time.Second); early != nil {
		t.Fatalf("redelivered after %v; want a delay of %v", time.Since(nakked), firstRetryDelay)
	}
	again := fetchOne(10 * time.Second)
	if again == nil {
		t.Fatal("message was never redelivered")
	}
	if waited := time.Since(nakked); waited < firstRetryDelay-time.Second {
		t.Fatalf("redelivered after %v; want about %v", waited, firstRetryDelay)
	}
	if md, err := again.Metadata(); err != nil || md.NumDelivered != 2 {
		t.Fatalf("metadata = %+v, err %v; want NumDelivered 2", md, err)
	}
	_ = again.Ack()
}
