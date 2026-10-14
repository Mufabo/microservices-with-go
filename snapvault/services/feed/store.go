package main

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// FeedItem is one row of the public gallery. It carries db tags (for pgx) and
// json tags (for the API).
type FeedItem struct {
	ID          string    `db:"id" json:"id"`
	Owner       string    `db:"owner" json:"owner"`
	ThumbObject string    `db:"thumb_object" json:"thumb_object,omitempty"`
	Width       int       `db:"width" json:"width,omitempty"`
	Height      int       `db:"height" json:"height,omitempty"`
	UploadedAt  time.Time `db:"uploaded_at" json:"uploaded_at"`
}

type feedStore struct{ pool *pgxpool.Pool }

// newFeedStore opens a connection pool and ensures the schema exists. Creating
// the schema on startup is pragmatic for this example; a production system
// would apply the versioned migrations in ./migrations before deploying.
func newFeedStore(ctx context.Context, dsn string) (*feedStore, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, err
	}
	s := &feedStore{pool: pool}
	if _, err := pool.Exec(ctx, schema); err != nil {
		pool.Close()
		return nil, err
	}
	return s, nil
}

func (s *feedStore) Close()                         { s.pool.Close() }
func (s *feedStore) Ping(ctx context.Context) error { return s.pool.Ping(ctx) }

// Add inserts a new feed item, ignoring duplicates. The ON CONFLICT clause
// makes it idempotent, which is what lets the consumer safely process an event
// more than once under at-least-once delivery.
func (s *feedStore) Add(ctx context.Context, it FeedItem) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO feed_items (id, owner, uploaded_at)
		 VALUES ($1, $2, $3)
		 ON CONFLICT (id) DO NOTHING`,
		it.ID, it.Owner, it.UploadedAt)
	return err
}

// errNoFeedItem reports that SetThumb found no row to attach a thumbnail to,
// because the matching images.uploaded event has not been applied yet.
var errNoFeedItem = errors.New("feed item does not exist yet")

// SetThumb attaches thumbnail details to an existing item. Re-applying the
// same update is harmless, so this too is safe to replay. If the row does not
// exist yet it returns errNoFeedItem instead of silently updating nothing, so
// the caller can retry once the upload event has been processed.
func (s *feedStore) SetThumb(ctx context.Context, id, thumbObject string, w, h int) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE feed_items SET thumb_object=$2, width=$3, height=$4 WHERE id=$1`,
		id, thumbObject, w, h)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return errNoFeedItem
	}
	return nil
}

// Recent returns the newest items first.
func (s *feedStore) Recent(ctx context.Context, limit int) ([]FeedItem, error) {
	rows, err := s.pool.Query(ctx, selectColumns+` ORDER BY uploaded_at DESC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByName[FeedItem])
}

// Get returns one item, or pgx.ErrNoRows if absent.
func (s *feedStore) Get(ctx context.Context, id string) (FeedItem, error) {
	rows, err := s.pool.Query(ctx, selectColumns+` WHERE id=$1`, id)
	if err != nil {
		return FeedItem{}, err
	}
	return pgx.CollectExactlyOneRow(rows, pgx.RowToStructByName[FeedItem])
}

// selectColumns coalesces nullable columns so they scan cleanly into the
// non-pointer FeedItem fields, and aliases them to match the db tags.
const selectColumns = `
SELECT id,
       owner,
       COALESCE(thumb_object, '') AS thumb_object,
       COALESCE(width, 0)         AS width,
       COALESCE(height, 0)        AS height,
       uploaded_at
FROM feed_items`

const schema = `
CREATE TABLE IF NOT EXISTS feed_items (
    id           TEXT PRIMARY KEY,
    owner        TEXT NOT NULL,
    thumb_object TEXT,
    width        INTEGER,
    height       INTEGER,
    uploaded_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS feed_items_uploaded_at_idx ON feed_items (uploaded_at DESC);
`
