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
    if _, err := io.Copy(f, r); err != nil {
        _ = f.Close()
        return err
    }
    return f.Close() // a failed write can surface only here
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

With `Put` in place we can add an upload handler, the entry point for new photos. It caps the request size, checks that the file really is an image, stores the original under `originals/<id>`, and returns a generated ID:

``` go
const maxUploadBytes = 25 << 20 // 25 MiB

func uploadImage(store blobStore, log *slog.Logger) http.HandlerFunc {
    return func(w http.ResponseWriter, r *http.Request) {
        // Never read an unbounded body from the internet.
        r.Body = http.MaxBytesReader(w, r.Body, maxUploadBytes)
        if err := r.ParseMultipartForm(maxUploadBytes); err != nil {
            http.Error(w, "invalid upload", http.StatusBadRequest)
            return
        }
        file, hdr, err := r.FormFile("image")
        if err != nil {
            http.Error(w, "expected an 'image' file field", http.StatusBadRequest)
            return
        }
        defer file.Close()

        // Trust the file's bytes, not the client's Content-Type header.
        _, format, err := image.DecodeConfig(file)
        if err != nil || !allowedFormats[format] {
            http.Error(w, "unsupported image: upload a JPEG, PNG, or GIF",
                http.StatusUnsupportedMediaType)
            return
        }
        if _, err := file.Seek(0, io.SeekStart); err != nil {
            http.Error(w, "invalid upload", http.StatusBadRequest)
            return
        }

        id := newID()
        ct := "image/" + format
        if err := store.Put(r.Context(), "originals/"+id, ct, file); err != nil {
            log.Error("store upload", "id", id, "err", err)
            http.Error(w, "could not store image", http.StatusInternalServerError)
            return
        }

        log.Info("image uploaded", "id", id, "name", hdr.Filename)
        w.Header().Set("Content-Type", "application/json")
        w.WriteHeader(http.StatusCreated)
        _ = json.NewEncoder(w).Encode(map[string]string{"id": id})
    }
}

// allowedFormats are the image.DecodeConfig format names we accept.
var allowedFormats = map[string]bool{"jpeg": true, "png": true, "gif": true}

// newID returns a random 128-bit hex identifier.
func newID() string {
    var b [16]byte
    _, _ = rand.Read(b[:])
    return hex.EncodeToString(b[:])
}
```

`http.MaxBytesReader` is the line people forget. Without it, anyone can stream an arbitrarily large body at the service and fill its memory or disk; with it, the read fails once the limit is passed and the client gets a `400`.

The format check is the other line that matters. `image.DecodeConfig` reads just the file's header and reports which registered decoder recognized it --- `"jpeg"`, `"png"`, or `"gif"`, once those packages are imported for their side effects (`_ "image/jpeg"` and so on). Anything else, including a file whose header *claims* `image/jpeg` but whose bytes are an HTML page, gets a `415 Unsupported Media Type` and is never stored. The stored content type then comes from what the bytes actually are. A `multipart.File` is seekable, so after peeking at the header we rewind and store the whole file.

Uploads now live under `originals/<id>` --- and, from Chapter 6 on, thumbnails under `thumbs/<id>.jpg` --- so the Chapter 2 `serveImage`, which read the bare request path as the key, would no longer find them. Give it a key prefix, and let it sniff the content type instead of assuming JPEG, since uploads can be PNG or GIF too --- but only ever *declare* an image type:

``` go
// servableTypes are the only content types serveImage ever declares.
var servableTypes = map[string]bool{"image/jpeg": true, "image/png": true, "image/gif": true}

// serveImage streams the object at prefix+{name}, sniffing its content type
// but declaring only image types.
func serveImage(store blobStore, prefix string) http.HandlerFunc {
    return func(w http.ResponseWriter, r *http.Request) {
        rc, err := store.Get(r.Context(), prefix+r.PathValue("name"))
        if err != nil {
            http.Error(w, "not found", http.StatusNotFound)
            return
        }
        defer rc.Close()

        buf := bufio.NewReader(rc)
        ct := "application/octet-stream"
        if peek, _ := buf.Peek(512); len(peek) > 0 {
            if sniffed := http.DetectContentType(peek); servableTypes[sniffed] {
                ct = sniffed
            }
        }
        w.Header().Set("Content-Type", ct)
        w.Header().Set("X-Content-Type-Options", "nosniff")
        w.Header().Set("Cache-Control", "public, max-age=86400")
        _, _ = io.Copy(w, buf)
    }
}
```

::: warning
**User uploads are served from your own origin.** In Chapter 6 the gateway proxies these bytes to the public, so they arrive from the same origin as the rest of Snapvault. `http.DetectContentType` happily recognizes HTML, so a handler that declared whatever it sniffed would serve an uploaded HTML page as `text/html`, and any script in it would run with your site's privileges. That's a stored cross-site scripting hole. The upload check keeps non-images out of storage, and `serveImage` is the second line of defense: it never declares anything but an image type, and `X-Content-Type-Options: nosniff` tells the browser not to second-guess the type it was given.
:::

Add `"bufio"`, `"crypto/rand"`, `"encoding/hex"`, `"encoding/json"`, and `"image"` to the imports, plus the decoders for side effects (`_ "image/gif"`, `_ "image/jpeg"`, `_ "image/png"`), and register the routes:

``` go
mux.HandleFunc("POST /images", uploadImage(store, log))
mux.HandleFunc("GET /images/{name}", serveImage(store, "originals/"))
mux.HandleFunc("GET /thumbs/{name}", serveImage(store, "thumbs/"))
```

`GET /images/<id>` now returns the original you uploaded, and `GET /thumbs/<id>.jpg` is ready for the thumbnails Chapter 6 will produce. The upload works now --- but nothing else in the system *knows* an upload happened. There is no thumbnail, no metadata record, no feed entry. Closing that gap is the whole of Chapter 6. For this chapter, the point is that originals now live in object storage, exactly where blobs belong.

## Document data: the metadata service with MongoDB

The metadata service records the descriptive facts about each image --- owner, original filename, content type, size, tags, when it was uploaded. This data is a natural fit for a document database: tags are a variable-length list, and the shape may grow over time (a caption, camera details) without a migration. MongoDB suits it well, and the official Go driver (v2) is straightforward. Create `services/metadata/store.go`:

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
    ID           string    `bson:"_id"            json:"id"`
    Owner        string    `bson:"owner"          json:"owner"`
    ContentType  string    `bson:"content_type"   json:"content_type"`
    OriginalName string    `bson:"original_name"  json:"original_name"`
    Size         int64     `bson:"size"           json:"size"`
    Tags         []string  `bson:"tags,omitempty" json:"tags,omitempty"`
    UploadedAt   time.Time `bson:"uploaded_at"    json:"uploaded_at"`
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

// Upsert writes m keyed by its ID. It sets only the mutable fields (never
// _id), so it is idempotent: applying the same record twice leaves the same
// state, which matters once messages can be redelivered (Chapter 6).
func (s *metaStore) Upsert(ctx context.Context, m ImageMeta) error {
    _, err := s.coll.UpdateOne(ctx,
        bson.D{{Key: "_id", Value: m.ID}},
        bson.D{{Key: "$set", Value: bson.D{
            {Key: "owner", Value: m.Owner},
            {Key: "content_type", Value: m.ContentType},
            {Key: "original_name", Value: m.OriginalName},
            {Key: "size", Value: m.Size},
            {Key: "tags", Value: m.Tags},
            {Key: "uploaded_at", Value: m.UploadedAt},
        }}},
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

// FeedItem is one row of the public gallery. The thumbnail fields stay empty
// until the thumbnails worker has run (Chapter 6).
type FeedItem struct {
    ID          string    `json:"id"                     db:"id"`
    Owner       string    `json:"owner"                  db:"owner"`
    ThumbObject string    `json:"thumb_object,omitempty" db:"thumb_object"`
    Width       int       `json:"width,omitempty"        db:"width"`
    Height      int       `json:"height,omitempty"       db:"height"`
    UploadedAt  time.Time `json:"uploaded_at"            db:"uploaded_at"`
}

type feedStore struct{ pool *pgxpool.Pool }

const schema = `
CREATE TABLE IF NOT EXISTS feed_items (
    id           TEXT PRIMARY KEY,
    owner        TEXT NOT NULL,
    thumb_object TEXT,
    width        INTEGER,
    height       INTEGER,
    uploaded_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS feed_items_uploaded_at_idx
    ON feed_items (uploaded_at DESC);`

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

// Add inserts a new item and ignores duplicates — idempotent, for the same
// redelivery reasons as the metadata store.
func (s *feedStore) Add(ctx context.Context, it FeedItem) error {
    _, err := s.pool.Exec(ctx,
        `INSERT INTO feed_items (id, owner, uploaded_at)
         VALUES ($1, $2, $3)
         ON CONFLICT (id) DO NOTHING`,
        it.ID, it.Owner, it.UploadedAt)
    return err
}

func (s *feedStore) Recent(ctx context.Context, limit int) ([]FeedItem, error) {
    rows, err := s.pool.Query(ctx,
        `SELECT id, owner,
                COALESCE(thumb_object, '') AS thumb_object,
                COALESCE(width, 0)         AS width,
                COALESCE(height, 0)        AS height,
                uploaded_at
         FROM feed_items ORDER BY uploaded_at DESC LIMIT $1`, limit)
    if err != nil {
        return nil, err
    }
    defer rows.Close()
    return pgx.CollectRows(rows, pgx.RowToStructByName[FeedItem])
}
```

Notice `DO NOTHING` rather than `DO UPDATE`. It's tempting to make a repeated insert "refresh" the row, but in Chapter 6 a *different* event fills in the thumbnail columns. If a redelivered upload event overwrote the row, it would wipe a thumbnail that had already been attached. An idempotent write has to be idempotent with respect to every other writer of the same row, not just to itself. The `COALESCE` calls turn the thumbnail columns' `NULL`s (no thumbnail yet) into zero values that scan cleanly into the struct's plain `string` and `int` fields.

A few `pgx` idioms are worth pointing out. The pool (`pgxpool`) is safe for concurrent use and is what a service should hold --- never a single connection. Parameters are passed positionally as `$1`, `$2`, ..., never by string-concatenating values into SQL, which is how injection happens. And `pgx.CollectRows` with `pgx.RowToStructByName[FeedItem]` scans result rows straight into our struct by matching the `db` tags to column names --- which is why `FeedItem` carries both `json` tags (for the API) and `db` tags (for the database). Creating the schema with `IF NOT EXISTS` on startup is a pragmatic choice for this book; a production system would manage schema with versioned migrations, which we note in Chapter 8.

The feed service exposes `GET /feed`, returning the recent items as JSON --- the data behind Snapvault's public gallery --- and `GET /feed/{id}` for a single item, which the gateway uses in Chapter 14. As with metadata, only the feed service speaks Postgres; everyone else sees JSON.

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
    restart: unless-stopped

  feed:
    build:
      context: .
      args: { SERVICE: feed }
    environment:
      PORT: "8080"
      DATABASE_URL: postgres://snapvault:snapvault@postgres:5432/snapvault?sslmode=disable
    depends_on: [postgres]
    restart: unless-stopped

volumes:
  images-data:
  mongo-data:
  pg-data:
```

The `restart: unless-stopped` lines are Chapter 4's lesson put to work. On the very first `docker compose up`, Postgres spends several seconds initializing its data directory before it accepts connections, and the feed service, started the moment the Postgres container exists, fails to create its schema and exits. Rather than teach every service to wait, we let it fail fast and have Compose start it again until the database is ready. That is the same crash-and-restart loop Kubernetes runs for us in Chapter 9, so the services behave identically in both places. Every service that connects to something at startup gets this line; the images service gets it too once it depends on NATS in Chapter 6.

Notice also that the official `mongo` and `postgres` images *do* contain a shell, so unlike our distroless service images they could carry Compose healthchecks if we wanted to gate startup on them. More importantly, notice the topology now matches the architecture diagram from the front matter: images talks to object storage, metadata owns Mongo, feed owns Postgres, and no service reaches into another's store. The Snapvault on your laptop is now genuinely the system this book set out to build --- it is just missing the nervous system that connects the parts. That nervous system is messaging.

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
