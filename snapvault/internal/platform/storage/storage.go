// Package storage provides object storage for binary blobs (image originals
// and thumbnails) behind a small interface, with a local-disk backend for
// development and a Google Cloud Storage backend for production.
package storage

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"

	gcs "cloud.google.com/go/storage"
)

// Blob is the object-storage surface every Snapvault service depends on. It is
// deliberately tiny: services should not know whether bytes live on a disk or
// in a bucket.
type Blob interface {
	// Get opens the object at key for reading. The caller must Close it.
	Get(ctx context.Context, key string) (io.ReadCloser, error)
	// Put stores r at key with the given content type, overwriting any
	// existing object. It returns nil only once the write has completed.
	Put(ctx context.Context, key, contentType string, r io.Reader) error
}

// NewFromEnv selects a backend from the environment:
//
//	STORAGE=gcs + GCS_BUCKET=<name>   -> Google Cloud Storage
//	otherwise                         -> local disk at DATA_DIR (default ./data)
//
// The GCS backend authenticates with Application Default Credentials, so there
// is never a key file in the code or image.
func NewFromEnv(ctx context.Context) (Blob, error) {
	if os.Getenv("STORAGE") == "gcs" {
		bucket := os.Getenv("GCS_BUCKET")
		if bucket == "" {
			return nil, fmt.Errorf("storage: STORAGE=gcs but GCS_BUCKET is empty")
		}
		client, err := gcs.NewClient(ctx)
		if err != nil {
			return nil, fmt.Errorf("storage: new gcs client: %w", err)
		}
		return &gcsBlob{bucket: client.Bucket(bucket)}, nil
	}
	dir := os.Getenv("DATA_DIR")
	if dir == "" {
		dir = "./data"
	}
	return &diskBlob{dir: dir}, nil
}

// ---- local disk ----

type diskBlob struct{ dir string }

// path joins key under the data dir, guarding against traversal outside it.
func (d *diskBlob) path(key string) string {
	clean := filepath.Clean("/" + key) // collapses .. so it can't escape
	return filepath.Join(d.dir, clean)
}

func (d *diskBlob) Get(_ context.Context, key string) (io.ReadCloser, error) {
	return os.Open(d.path(key))
}

func (d *diskBlob) Put(_ context.Context, key, _ string, r io.Reader) error {
	p := d.path(key)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	f, err := os.Create(p)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, r); err != nil {
		_ = f.Close()
		return err
	}
	// Close can report a failed write (a full disk, say), so its error counts.
	return f.Close()
}

// ---- Google Cloud Storage ----

type gcsBlob struct{ bucket *gcs.BucketHandle }

func (g *gcsBlob) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	return g.bucket.Object(key).NewReader(ctx)
}

func (g *gcsBlob) Put(ctx context.Context, key, contentType string, r io.Reader) error {
	w := g.bucket.Object(key).NewWriter(ctx)
	w.ContentType = contentType
	if _, err := io.Copy(w, r); err != nil {
		_ = w.Close()
		return err
	}
	// The object becomes durable only after Close succeeds.
	return w.Close()
}
