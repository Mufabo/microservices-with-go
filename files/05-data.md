# Chapter 5 --- Giving Services Their Own Data

The single rule that most distinguishes a microservices system from a distributed monolith is this: **each service owns its data, and no other service touches that data directly.** If the feed service can run a query against the metadata service's database, you do not have two services --- you have one database with two front ends, and you will get all of the operational pain of distribution with none of the independence that was supposed to be the reward. In this chapter we give Snapvault three kinds of data store, each owned by the service that needs it: **Google Cloud Storage** for image blobs, **MongoDB** for flexible metadata, and **PostgreSQL** for the query-optimized feed. Choosing different stores on purpose is itself a lesson --- services are free to pick the database that fits their job.

## Why a database per service

When every service shares one database, that database becomes the tightest possible coupling between them. A schema change to support one service risks breaking another. A slow query in one feature degrades every feature. You cannot deploy, scale, or reason about one service without considering all of them, because they meet in the tables. The shared database quietly becomes the thing that must be coordinated, and coordination is exactly what microservices exist to avoid.

Database-per-service breaks that coupling. Each service can choose the store that suits its data, evolve its schema on its own schedule, and be scaled or tuned independently. The cost is equally real and must be faced honestly: data that used to live in one place, joinable with a single `JOIN`, is now spread across stores that cannot be joined. Keeping it consistent becomes a design problem rather than a database feature --- and the tool that solves it is asynchronous messaging, which is why this chapter (data) and the next (messaging) are inseparable halves of one idea. Here we stand up the stores; in Chapter 6 we keep them in sync with events.

## Object storage for images

Image files do not belong in a database. They are large, opaque binaries; storing them as database rows or columns bloats your backups, blows out your row sizes, and wastes a transactional engine on data that needs none of it. The right home for blobs is *object storage* --- a service built to store and serve arbitrarily large files cheaply, addressed by a key, and on Google Cloud that's **Google Cloud Storage** (GCS).

Recall the design from Chapter 2: the images service depends on a narrow `blobStore` interface, satisfied today by a `diskStore`. The promised payoff arrives now. We extend the interface with the one method an upload path needs and add a GCS implementation, and `main` chooses between them based on configuration. First, extend the interface in `services/images/store.go` and teach `diskStore` to write:

``` go
// blobStore is the storage the images service needs: read a blob, write a blob.
type blobStore interface {
    Get(ctx context.Context, key string) (io.ReadCloser, error)
    Put(ctx context.Context, key, contentType string, r io.Reader) error
}

func (d diskStore) Put(_ context.Context, key, _ string, r io.Reader) error {
    path := filepath.Join(d.base, filepath.Clean("/"+key))
    if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
        return err
    }
    f, err := os.Create(path)
    if err != nil {
        return err
    }
    defer f.Close()
    _, err = io.Copy(f, r)
    return err
}
```

Now the cloud implementation. Add `services/images/gcs.go`:

``` go
package main

import (
    "context"
    "fmt"
    "io"

    "cloud.google.com/go/storage"
)

// gcsStore satisfies blobStore using a Google Cloud Storage bucket.
type gcsStore struct {
    client *storage.Client
    bucket string
}

func newGCSStore(ctx context.Context, bucket string) (*gcsStore, error) {
    client, err := storage.NewClient(ctx)
    if err != nil {
        return nil, fmt.Errorf("gcs: new client: %w", err)
    }
    return &gcsStore{client: client, bucket: bucket}, nil
}

func (s *gcsStore) Get(ctx context.Context, key string) (io.ReadCloser, error) {
    return s.client.Bucket(s.bucket).Object(key).NewReader(ctx)
}

func (s *gcsStore) Put(ctx context.Context, key, contentType string, r io.Reader) error {
    w := s.client.Bucket(s.bucket).Object(key).NewWriter(ctx)
    w.ContentType = contentType
    if _, err := io.Copy(w, r); err != nil {
        _ = w.Close()
        return fmt.Errorf("gcs: write %q: %w", key, err)
    }
    return w.Close() // the write is only durable once Close succeeds
}
```

That last comment is a real GCS gotcha worth internalizing: the object is not committed until `Close()` returns without error. Forgetting to check the error from `Close` is how you end up with silently truncated uploads.

Selecting the implementation belongs in `main`, driven by an environment variable so the *same image* runs against local disk on your laptop and GCS in the cloud:

``` go
ctx := context.Background()
var store blobStore
switch config.String("STORAGE", "disk") {
case "gcs":
    gs, err := newGCSStore(ctx, config.MustString("GCS_BUCKET"))
    if err != nil {
        log.Error("init gcs store", "err", err)
        os.Exit(1)
    }
    store = gs
default:
    store = diskStore{base: config.String("DATA_DIR", "./data")}
}
```

::: tip
**Credentials you never see in code.** The GCS client authenticates through *Application Default Credentials*: locally it uses the login from `gcloud auth application-default login`; on GKE it uses the pod's Workload Identity (Chapter 9), with no key file anywhere. Notice there is no API key or secret in the code above --- and there never should be. Hard-coded cloud credentials are the single most common way private buckets become public breaches.
:::

With `Put` in place we can add an upload handler, the entry point for new photos. It stores the original and returns a generated ID:

``` go
func uploadImage(store blobStore, log *slog.Logger) http.HandlerFunc {
    return func(w http.ResponseWriter, r *http.Request) {
        file, hdr, err := r.FormFile("image")
        if err != nil {
            http.Error(w, "expected an 'image' file field", http.StatusBadRequest)
            return
        }
        defer file.Close()

        id := newID()
        ct := hdr.Header.Get("Content-Type")
        if err := store.Put(r.Context(), "originals/"+id, ct, file); err != nil {
            log.Error("store upload", "id", id, "err", err)
            http.Error(w, "could not store image", http.StatusInternalServerError)
            return
        }

        w.Header().Set("Content-Type", "application/json")
        _ = json.NewEncoder(w).Encode(map[string]string{"id": id})
        log.Info("image uploaded", "id", id, "name", hdr.Filename)
    }
}

// newID returns a random 128-bit hex identifier.
func newID() string {
    var b [16]byte
    _, _ = rand.Read(b[:])
    return hex.EncodeToString(b[:])
}
```

Add `"crypto/rand"`, `"encoding/hex"`, and `"encoding/json"` to the imports, and register the route with `mux.HandleFunc("POST /images", uploadImage(store, log))`. The upload works now --- but nothing else in the system *knows* an upload happened. There is no thumbnail, no metadata record, no feed entry. Closing that gap is the whole of Chapter 6. For this chapter, the point is that originals now live in object storage, exactly where blobs belong.

## Document data: the metadata service with MongoDB

The metadata service records the descriptive facts about each image --- owner, title, tags, when it was uploaded. This data is a natural fit for a document database: tags are a variable-length list, titles are free text, and the shape may grow over time without a migration. MongoDB suits it well, and the official Go driver (v2) is straightforward. Create `services/metadata/store.go`:

``` go
package main

import (
    "context"
    "time"

    "go.mongodb.org/mongo-driver/v2/bson"
    "go.mongodb.org/mongo-driver/v2/mongo"
    "go.mongodb.org/mongo-driver/v2/mongo/options"
    "go.mongodb.org/mongo-driver/v2/mongo/readpref"
)

// ImageMeta is the document we store per image.
type ImageMeta struct {
    ID         string    `bson:"_id"         json:"id"`
    Owner      string    `bson:"owner"       json:"owner"`
    Title      string    `bson:"title"       json:"title"`
    Tags       []string  `bson:"tags"        json:"tags"`
    UploadedAt time.Time `bson:"uploaded_at" json:"uploaded_at"`
}

type metaStore struct{ coll *mongo.Collection }

// newMetaStore connects, verifies the connection, and returns the store
// along with a disconnect function for graceful shutdown.
func newMetaStore(ctx context.Context, uri, db string) (*metaStore, func(context.Context) error, error) {
    client, err := mongo.Connect(options.Client().ApplyURI(uri))
    if err != nil {
        return nil, nil, err
    }
    if err := client.Ping(ctx, readpref.Primary()); err != nil {
        _ = client.Disconnect(ctx)
        return nil, nil, err
    }
    coll := client.Database(db).Collection("images")
    return &metaStore{coll: coll}, client.Disconnect, nil
}

// Upsert writes or replaces a metadata document. It is idempotent: applying
// the same record twice leaves the same state, which matters once messages
// can be redelivered (Chapter 6).
func (s *metaStore) Upsert(ctx context.Context, m ImageMeta) error {
    _, err := s.coll.UpdateOne(ctx,
        bson.D{{Key: "_id", Value: m.ID}},
        bson.D{{Key: "$set", Value: m}},
        options.UpdateOne().SetUpsert(true),
    )
    return err
}

func (s *metaStore) Get(ctx context.Context, id string) (ImageMeta, error) {
    var m ImageMeta
    err := s.coll.FindOne(ctx, bson.D{{Key: "_id", Value: id}}).Decode(&m)
    return m, err
}

func (s *metaStore) ByTag(ctx context.Context, tag string) ([]ImageMeta, error) {
    cur, err := s.coll.Find(ctx, bson.D{{Key: "tags", Value: tag}})
    if err != nil {
        return nil, err
    }
    defer cur.Close(ctx)
    var out []ImageMeta
    if err := cur.All(ctx, &out); err != nil {
        return nil, err
    }
    return out, nil
}
```

::: note
**Driver v2 dropped the context argument from `Connect`.** If you've used an older release of the MongoDB Go driver, note the v2 signature: `mongo.Connect(options.Client().ApplyURI(uri))` takes options only --- no `context.Context`. You then pass a context to operations like `Ping`, `FindOne`, and `Disconnect`. Mixing up the v1 and v2 signatures is the most common compile error when upgrading.
:::

The service wraps this store in a small read API. We design the `Upsert` to be idempotent now, deliberately, because in the next chapter the writes will come from messages that the broker may deliver more than once --- and an idempotent write turns "delivered twice" from a bug into a non-event. Wire it up in `services/metadata/main.go`:

``` go
func main() {
    log := svclog.New("metadata")
    ctx := context.Background()

    store, disconnect, err := newMetaStore(ctx,
        config.MustString("MONGO_URI"),
        config.String("MONGO_DB", "snapvault"))
    if err != nil {
        log.Error("connect mongo", "err", err)
        os.Exit(1)
    }
    defer disconnect(context.Background())

    mux := http.NewServeMux()
    httpx.Health(mux, nil)
    mux.HandleFunc("GET /metadata/{id}", getMeta(store, log))
    mux.HandleFunc("GET /metadata", listByTag(store, log))

    if err := httpx.Serve(":"+config.String("PORT", "8080"), mux, log); err != nil {
        log.Error("server stopped", "err", err)
        os.Exit(1)
    }
}
```

The two handlers --- `getMeta` decoding a single document to JSON, `listByTag` reading `?tag=` from the query and returning a list --- are short variations on the closure-over-dependencies pattern from Chapter 2, so we won't belabor them. The shape that matters is the boundary: the outside world sees JSON over HTTP; only this service ever speaks Mongo.

## Relational data: the feed service with PostgreSQL

The public feed is a different problem. It is read constantly (everyone browsing Snapvault hits it), written occasionally (only on new uploads), and always answers the same question: "what are the most recent images?" That is a textbook case for a relational database with an index doing the heavy lifting, and PostgreSQL is the natural choice. We'll use `pgx`, the fast, idiomatic Postgres driver and connection pool for Go. Create `services/feed/store.go`:

``` go
package main

import (
    "context"
    "time"

    "github.com/jackc/pgx/v5"
    "github.com/jackc/pgx/v5/pgxpool"
)

type FeedItem struct {
    ID        string    `json:"id"         db:"id"`
    Owner     string    `json:"owner"      db:"owner"`
    Title     string    `json:"title"      db:"title"`
    ThumbURL  string    `json:"thumb_url"  db:"thumb_url"`
    CreatedAt time.Time `json:"created_at" db:"created_at"`
}

type feedStore struct{ pool *pgxpool.Pool }

const schema = `
CREATE TABLE IF NOT EXISTS feed_items (
    id         TEXT PRIMARY KEY,
    owner      TEXT NOT NULL,
    title      TEXT NOT NULL DEFAULT '',
    thumb_url  TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS feed_items_created_at_idx
    ON feed_items (created_at DESC);`

func newFeedStore(ctx context.Context, dsn string) (*feedStore, error) {
    pool, err := pgxpool.New(ctx, dsn)
    if err != nil {
        return nil, err
    }
    if _, err := pool.Exec(ctx, schema); err != nil {
        pool.Close()
        return nil, err
    }
    return &feedStore{pool: pool}, nil
}

// Add inserts an item, or updates it if the id already exists — idempotent,
// for the same redelivery reasons as the metadata store.
func (s *feedStore) Add(ctx context.Context, it FeedItem) error {
    _, err := s.pool.Exec(ctx,
        `INSERT INTO feed_items (id, owner, title, thumb_url, created_at)
         VALUES ($1, $2, $3, $4, $5)
         ON CONFLICT (id) DO UPDATE
           SET title = EXCLUDED.title, thumb_url = EXCLUDED.thumb_url`,
        it.ID, it.Owner, it.Title, it.ThumbURL, it.CreatedAt)
    return err
}

func (s *feedStore) Recent(ctx context.Context, limit int) ([]FeedItem, error) {
    rows, err := s.pool.Query(ctx,
        `SELECT id, owner, title, thumb_url, created_at
         FROM feed_items ORDER BY created_at DESC LIMIT $1`, limit)
    if err != nil {
        return nil, err
    }
    defer rows.Close()
    return pgx.CollectRows(rows, pgx.RowToStructByName[FeedItem])
}
```

A few `pgx` idioms are worth pointing out. The pool (`pgxpool`) is safe for concurrent use and is what a service should hold --- never a single connection. Parameters are passed positionally as `$1`, `$2`, ..., never by string-concatenating values into SQL, which is how injection happens. And `pgx.CollectRows` with `pgx.RowToStructByName[FeedItem]` scans result rows straight into our struct by matching the `db` tags to column names --- which is why `FeedItem` carries both `json` tags (for the API) and `db` tags (for the database). Creating the schema with `IF NOT EXISTS` on startup is a pragmatic choice for this book; a production system would manage schema with versioned migrations, which we note in Chapter 8.

The feed service exposes a single read endpoint, `GET /feed`, returning the recent items as JSON --- the data behind Snapvault's public gallery. As with metadata, only the feed service speaks Postgres; everyone else sees JSON.

## Adding the data stores to Compose

Local development should run the real databases, not mocks. Extend `compose.yaml` with MongoDB and PostgreSQL, and wire the two new services to them:

``` yaml
  mongo:
    image: mongo:7
    volumes:
      - mongo-data:/data/db

  postgres:
    image: postgres:16
    environment:
      POSTGRES_USER: snapvault
      POSTGRES_PASSWORD: snapvault
      POSTGRES_DB: snapvault
    volumes:
      - pg-data:/var/lib/postgresql/data

  metadata:
    build:
      context: .
      args: { SERVICE: metadata }
    environment:
      PORT: "8080"
      MONGO_URI: mongodb://mongo:27017
      MONGO_DB: snapvault
    depends_on: [mongo]

  feed:
    build:
      context: .
      args: { SERVICE: feed }
    environment:
      PORT: "8080"
      DATABASE_URL: postgres://snapvault:snapvault@postgres:5432/snapvault?sslmode=disable
    depends_on: [postgres]

volumes:
  images-data:
  mongo-data:
  pg-data:
```

Notice that the official `mongo` and `postgres` images *do* contain a shell, so unlike our distroless service images they could carry Compose healthchecks if we wanted to gate startup on them. More importantly, notice the topology now matches the architecture diagram from the front matter: images talks to object storage, metadata owns Mongo, feed owns Postgres, and no service reaches into another's store. The Snapvault on your laptop is now genuinely the system this book set out to build --- it is just missing the nervous system that connects the parts. That nervous system is messaging.

::: warning
**Those passwords are for your laptop only.** The `snapvault/snapvault` credentials above are fine for a local Compose stack and nowhere else. Real databases get strong, unique secrets delivered through a secrets manager, never committed to a repository --- exactly the practice Chapter 14 sets up with Google Secret Manager. Treat any credential that appears in a YAML file as disposable and local by definition.
:::

### Summary

-   Each service owns its data and no other service touches that store directly; sharing a database recreates the coupling microservices exist to remove.
-   Different services can choose different stores: object storage (GCS) for image blobs, a document database (MongoDB) for flexible metadata, a relational database (PostgreSQL) for the query-heavy feed.
-   The images service swaps `diskStore` for `gcsStore` by satisfying the same `blobStore` interface, selected by an environment variable --- the same image runs on disk locally and on GCS in the cloud, with credentials supplied by the environment, never the code.
-   The MongoDB v2 driver's `Connect` takes options only (no context); operations like `Ping` and `FindOne` take the context.
-   `pgx` uses a connection pool, positional `$1` parameters, and `CollectRows` with struct tags to scan rows directly into Go values.
-   Writes are designed to be idempotent now so that redelivered messages in Chapter 6 cause no harm.

The stores are standing, but they are islands: an upload fills object storage and tells no one. Connecting them --- turning an upload into a thumbnail, a metadata record, and a feed entry, all without blocking the uploader --- is the job of asynchronous messaging, and it's the heart of the book. On to NATS.
