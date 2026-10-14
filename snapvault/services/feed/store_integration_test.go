//go:build integration

package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
)

// startPostgres boots a real PostgreSQL in Docker and returns its DSN. The
// container is torn down when the test finishes.
func startPostgres(ctx context.Context, t *testing.T) string {
	t.Helper()
	pg, err := postgres.Run(ctx, "postgres:16",
		postgres.WithDatabase("snapvault"),
		postgres.WithUsername("snapvault"),
		postgres.WithPassword("snapvault"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(60*time.Second)),
	)
	if err != nil {
		t.Fatalf("start postgres: %v", err)
	}
	t.Cleanup(func() { _ = testcontainers.TerminateContainer(pg) })

	dsn, err := pg.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("connection string: %v", err)
	}
	return dsn
}

func TestFeedStore_AddIsIdempotentAndThumbAttaches(t *testing.T) {
	ctx := context.Background()
	store, err := newFeedStore(ctx, startPostgres(ctx, t))
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	defer store.Close()

	item := FeedItem{ID: "img1", Owner: "alice", UploadedAt: time.Now().UTC()}

	// Replaying the same upload event must not create duplicate rows.
	for i := 0; i < 3; i++ {
		if err := store.Add(ctx, item); err != nil {
			t.Fatalf("add #%d: %v", i, err)
		}
	}
	items, err := store.Recent(ctx, 10)
	if err != nil {
		t.Fatalf("recent: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("got %d rows, want 1 (insert must be idempotent)", len(items))
	}

	// The thumbnailed event attaches the thumbnail to the existing row.
	if err := store.SetThumb(ctx, "img1", "thumbs/img1.jpg", 320, 200); err != nil {
		t.Fatalf("set thumb: %v", err)
	}
	got, err := store.Get(ctx, "img1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.ThumbObject != "thumbs/img1.jpg" || got.Width != 320 || got.Height != 200 {
		t.Fatalf("thumbnail not attached: %+v", got)
	}

	// A redelivered upload event must not erase the thumbnail.
	if err := store.Add(ctx, item); err != nil {
		t.Fatalf("re-add: %v", err)
	}
	got, err = store.Get(ctx, "img1")
	if err != nil {
		t.Fatalf("get after re-add: %v", err)
	}
	if got.ThumbObject != "thumbs/img1.jpg" {
		t.Fatalf("re-adding the upload erased the thumbnail: %+v", got)
	}
}

func TestFeedStore_SetThumbBeforeAddReportsMissingRow(t *testing.T) {
	ctx := context.Background()
	store, err := newFeedStore(ctx, startPostgres(ctx, t))
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	defer store.Close()

	// The consumer relies on this error to retry instead of losing the thumbnail.
	err = store.SetThumb(ctx, "early", "thumbs/early.jpg", 10, 10)
	if !errors.Is(err, errNoFeedItem) {
		t.Fatalf("SetThumb on a missing row: err = %v, want errNoFeedItem", err)
	}
}
