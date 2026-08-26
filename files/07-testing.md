# Chapter 7 --- Testing Microservices

We have written a lot of code and verified it by hand --- uploading a photo, squinting at logs, checking that a thumbnail appeared. That works exactly once, for exactly the person who remembers how to do it. A system you intend to change safely needs tests that run on every commit and tell you, in seconds, whether you broke something. Microservices make testing both easier and harder: easier because each service is small and has clear inputs and outputs, harder because the interesting behavior often lives *between* services, in the messages they exchange. This chapter builds Snapvault's test suite in three layers --- fast unit tests, realistic integration tests against real dependencies, and a thin end-to-end check over the whole running system --- and shows how Go and Testcontainers make all three pleasant.

## The testing pyramid, adapted

The classic guidance is a pyramid: many fast unit tests at the base, fewer integration tests in the middle, a handful of slow end-to-end tests at the top. The shape holds for microservices, but the emphasis shifts. Because each service is small, its pure logic is easy to unit-test exhaustively. But because services are defined largely by their interactions with a database or a message broker, the integration layer carries more weight than it would in a monolith --- a unit test with a mocked database tells you your code compiles and calls the mock, not that your SQL is correct or your Mongo query matches anything. The trick that used to make integration tests painful --- needing a real Postgres, a real NATS --- is exactly what Testcontainers removes, so we lean into the middle of the pyramid harder than tradition suggests.

The three layers we'll build:

1.  **Unit tests** --- pure functions and HTTP handlers, with fakes standing in for dependencies. No network, no containers, milliseconds to run.
2.  **Integration tests** --- a single service against a *real* database or broker, started in a throwaway container. Seconds to run; gated behind a build tag so they don't slow down the inner loop.
3.  **End-to-end tests** --- the whole system up under Compose, exercised through the gateway exactly as a client would.

## Unit tests: pure logic and fakes

Start where testing is cheapest: pure functions. The thumbnailer's `scaleToFit` takes dimensions in and gives dimensions out, with no I/O --- the ideal unit-test target, and a perfect fit for Go's table-driven style. Create `services/thumbnails/thumbnail_test.go`:

``` go
package main

import (
    "image"
    "testing"
)

func TestScaleToFit(t *testing.T) {
    cases := []struct {
        name                     string
        w, h, max, wantW, wantH int
    }{
        {"landscape", 800, 400, 320, 320, 160},
        {"portrait", 400, 800, 320, 160, 320},
        {"square", 1000, 1000, 320, 320, 320},
        {"already small", 100, 80, 320, 100, 80},
    }
    for _, c := range cases {
        t.Run(c.name, func(t *testing.T) {
            src := image.NewRGBA(image.Rect(0, 0, c.w, c.h))
            got := scaleToFit(src, c.max)
            if got.Bounds().Dx() != c.wantW || got.Bounds().Dy() != c.wantH {
                t.Errorf("scaleToFit(%dx%d, %d) = %dx%d, want %dx%d",
                    c.w, c.h, c.max, got.Bounds().Dx(), got.Bounds().Dy(),
                    c.wantW, c.wantH)
            }
        })
    }
}
```

Each case becomes a named subtest, so a failure tells you *which* shape broke, and adding a case is one line. This is the rhythm of Go testing: a slice of cases, a loop, `t.Run`. No framework, no assertions library --- and for the vast majority of tests, none is needed.

Handlers are nearly as easy, because of a decision we made in Chapter 2: handlers close over a narrow interface rather than a concrete dependency. The images service's `serveImage` needs only a `storage.Blob`, so a test can hand it a fake --- an in-memory map --- and never touch a disk or a cloud. Create `services/images/handlers_test.go`:

``` go
package main

import (
    "bytes"
    "context"
    "io"
    "io/fs"
    "log/slog"
    "net/http"
    "net/http/httptest"
    "sync"
    "testing"
)

// fakeBlob is an in-memory storage.Blob for tests.
type fakeBlob struct {
    mu   sync.Mutex
    data map[string][]byte
}

func (f *fakeBlob) Get(_ context.Context, key string) (io.ReadCloser, error) {
    f.mu.Lock()
    defer f.mu.Unlock()
    b, ok := f.data[key]
    if !ok {
        return nil, fs.ErrNotExist
    }
    return io.NopCloser(bytes.NewReader(b)), nil
}

func (f *fakeBlob) Put(_ context.Context, key, _ string, r io.Reader) error {
    b, err := io.ReadAll(r)
    if err != nil {
        return err
    }
    f.mu.Lock()
    f.data[key] = b
    f.mu.Unlock()
    return nil
}

func TestServeImage(t *testing.T) {
    store := &fakeBlob{data: map[string][]byte{"cat.jpg": []byte("\xff\xd8jpeg-bytes")}}
    log := slog.New(slog.NewTextHandler(io.Discard, nil))

    // A ServeMux so the {id} path variable is populated as in production.
    mux := http.NewServeMux()
    mux.HandleFunc("GET /images/{id}", serveImage(store, log))
    srv := httptest.NewServer(mux)
    defer srv.Close()

    t.Run("found", func(t *testing.T) {
        resp, err := http.Get(srv.URL + "/images/cat.jpg")
        if err != nil {
            t.Fatal(err)
        }
        defer resp.Body.Close()
        if resp.StatusCode != http.StatusOK {
            t.Fatalf("status = %d, want 200", resp.StatusCode)
        }
        body, _ := io.ReadAll(resp.Body)
        if !bytes.Equal(body, store.data["cat.jpg"]) {
            t.Error("served bytes do not match stored bytes")
        }
    })

    t.Run("not found", func(t *testing.T) {
        resp, err := http.Get(srv.URL + "/images/missing.jpg")
        if err != nil {
            t.Fatal(err)
        }
        resp.Body.Close()
        if resp.StatusCode != http.StatusNotFound {
            t.Fatalf("status = %d, want 404", resp.StatusCode)
        }
    })
}
```

That `fakeBlob` satisfies `storage.Blob` --- the same interface `gcsBlob` and `diskBlob` satisfy --- which is the entire payoff of programming to interfaces finally cashed in. The test runs in milliseconds, exercises the real handler code path including the 404 branch, and depends on nothing external. The standard library's `net/http/httptest` gives us a real server on a real loopback port without any of the ceremony of mocking the HTTP machinery.

::: tip
**Run the race detector in CI, always.** Snapvault's services are concurrent --- handlers run in their own goroutines, consumers process messages concurrently. Go ships a race detector that catches data races at runtime: `go test -race ./...`. It roughly doubles test time and memory, which is why it's not always on locally, but it should run on every CI build. A race it catches in a one-second test is a race you won't be debugging in production at 2 a.m.
:::

## Integration tests with Testcontainers

A unit test with a fake database proves your code calls the fake correctly. It cannot prove your SQL is valid, your `ON CONFLICT` clause does what you think, or your Mongo filter matches a real document --- and those are precisely the things that break. To test them you need the real database, and **Testcontainers** lets a Go test start one in Docker, use it, and throw it away, all within the test itself. No shared test database to keep clean, no fixtures to reset, no "works on the CI box only."

The feed store's idempotent upsert is exactly the kind of SQL worth testing for real. Create `services/feed/store_integration_test.go`, behind a build tag so it only runs when asked:

``` go
//go:build integration

package main

import (
    "context"
    "testing"
    "time"

    "github.com/testcontainers/testcontainers-go"
    "github.com/testcontainers/testcontainers-go/modules/postgres"
    "github.com/testcontainers/testcontainers-go/wait"
)

func TestFeedStore_Integration(t *testing.T) {
    ctx := context.Background()

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
        t.Fatal(err)
    }

    store, err := newFeedStore(ctx, dsn) // also runs the schema migration
    if err != nil {
        t.Fatalf("new feed store: %v", err)
    }

    item := FeedItem{
        ID: "img-1", Owner: "ada", Title: "first",
        CreatedAt: time.Now().UTC().Truncate(time.Millisecond),
    }
    if err := store.Add(ctx, item); err != nil {
        t.Fatalf("add: %v", err)
    }
    // Adding the same id again must not create a duplicate — the property
    // our at-least-once consumers rely on.
    if err := store.Add(ctx, item); err != nil {
        t.Fatalf("re-add (idempotency): %v", err)
    }

    got, err := store.Recent(ctx, 10)
    if err != nil {
        t.Fatalf("recent: %v", err)
    }
    if len(got) != 1 {
        t.Fatalf("got %d items, want 1 (duplicate not deduplicated?)", len(got))
    }
    if got[0].ID != item.ID || got[0].Owner != item.Owner {
        t.Errorf("got %+v, want id=%q owner=%q", got[0], item.ID, item.Owner)
    }
}
```

This test starts a real PostgreSQL 16, runs the real schema migration, exercises the real `INSERT ... ON CONFLICT`, and asserts the idempotency property our consumers depend on --- the thing a mock could never verify. The `wait` strategy is important: containers report "started" before the database inside is accepting connections, so we wait for the log line that means *truly* ready (twice, because Postgres logs it once during its init phase and again when it's actually up). `t.Cleanup` tears the container down whether the test passes or fails.

Testing the messaging layer is just as direct, and arguably more valuable, because the message flow *is* the architecture. Here's a round-trip against a real NATS with JetStream, in `internal/platform/messaging/natsutil/natsutil_integration_test.go`:

``` go
//go:build integration

package natsutil

import (
    "context"
    "testing"
    "time"

    "github.com/nats-io/nats.go/jetstream"
    "github.com/testcontainers/testcontainers-go"
    natstc "github.com/testcontainers/testcontainers-go/modules/nats"

    "codeberg.org/example/snapvault/internal/events"
)

func TestJetStreamRoundTrip(t *testing.T) {
    ctx := context.Background()

    container, err := natstc.Run(ctx, "nats:2.10")
    if err != nil {
        t.Fatalf("start nats: %v", err)
    }
    t.Cleanup(func() { _ = testcontainers.TerminateContainer(container) })

    url, err := container.ConnectionString(ctx)
    if err != nil {
        t.Fatal(err)
    }

    nc, js, err := Connect(url, "test")
    if err != nil {
        t.Fatalf("connect: %v", err)
    }
    defer nc.Drain()

    if err := EnsureStream(ctx, js,
        events.StreamName, []string{events.SubjectWildcard}); err != nil {
        t.Fatalf("ensure stream: %v", err)
    }

    if _, err := js.Publish(ctx, events.SubjectUploaded, []byte(`{"id":"x1"}`)); err != nil {
        t.Fatalf("publish: %v", err)
    }

    stream, _ := js.Stream(ctx, events.StreamName)
    cons, err := stream.CreateOrUpdateConsumer(ctx, jetstream.ConsumerConfig{
        Durable:       "test",
        FilterSubject: events.SubjectUploaded,
        AckPolicy:     jetstream.AckExplicitPolicy,
    })
    if err != nil {
        t.Fatalf("consumer: %v", err)
    }

    msgs, err := cons.Fetch(1, jetstream.FetchMaxWait(5*time.Second))
    if err != nil {
        t.Fatalf("fetch: %v", err)
    }
    msg := <-msgs.Messages()
    if msg == nil {
        t.Fatal("no message delivered within timeout")
    }
    if string(msg.Data()) != `{"id":"x1"}` {
        t.Errorf("payload = %q", msg.Data())
    }
    _ = msg.Ack()
}
```

Notice this test uses `Fetch` --- the *pull* style of consuming, where you ask for a batch of messages and get them --- rather than the *push* style (`Consume` with a callback) the services use. Pull consumers are ideal in tests because they're synchronous: you ask for one message and block until it arrives or the wait elapses, with no goroutines or channels to coordinate. Both styles consume from the same durable consumer concept; pull just hands control of the timing to the caller, which is what a test wants.

::: note
**Testcontainers needs a Docker daemon.** These tests require Docker to be running wherever they execute --- your laptop, and the CI runner. On a self-hosted Forgejo Runner with the host's Docker socket available, they work out of the box (Chapter 8 sets this up); Codeberg's *hosted* runners deliberately don't offer Docker-in-Docker, so the integration job runs on a runner you control. The first run pulls the `postgres` and `nats` images, which is slow; subsequent runs reuse the cached images and are fast. If a developer doesn't have Docker, the build tag means `go test ./...` still runs all the unit tests cleanly --- the integration tests are simply skipped.
:::

## Separating fast tests from slow ones

The `//go:build integration` tag at the top of those files is the mechanism that keeps the inner development loop fast. By default, `go test` ignores files behind a tag you haven't requested, so:

``` bash
$ go test ./...                    # unit tests only — fast, no Docker
$ go test -tags=integration ./...  # also runs container-backed tests
$ go test -race -tags=integration ./...   # the full CI invocation
```

The first command is what you run constantly while coding: it finishes in a second or two and needs nothing but Go. The second is what CI runs on every push, accepting the extra time in exchange for testing the real database and broker behavior. This separation is worth setting up early; a test suite that takes a minute to run is a test suite developers stop running.

## End-to-end: the whole system

At the top of the pyramid sits one kind of test that no amount of unit and integration testing replaces: does the *entire system*, wired together, actually do its job? For Snapvault that means uploading a photo through the gateway and confirming it shows up in the feed with a thumbnail --- the full fan-out across five services and the message bus, exercised exactly as a real client would.

Because the system is eventually consistent, an end-to-end test must *poll* rather than assume instant results: after uploading, the feed entry and its thumbnail appear within a moment, not synchronously. Here is the shape, behind an `e2e` tag, pointed at a Compose stack via an environment variable:

``` go
//go:build e2e

package e2e

import (
    "bytes"
    "encoding/json"
    "io"
    "mime/multipart"
    "net/http"
    "os"
    "testing"
    "time"
)

func TestUploadAppearsInFeed(t *testing.T) {
    base := os.Getenv("GATEWAY_URL") // e.g. http://localhost:8080
    if base == "" {
        t.Skip("set GATEWAY_URL to run end-to-end tests")
    }

    // Upload a tiny image through the public gateway.
    var body bytes.Buffer
    mw := multipart.NewWriter(&body)
    part, _ := mw.CreateFormFile("image", "test.png")
    part.Write(onePixelPNG()) // a minimal valid PNG, defined elsewhere
    mw.WriteField("owner", "e2e")
    mw.Close()

    resp, err := http.Post(base+"/api/images", mw.FormDataContentType(), &body)
    if err != nil {
        t.Fatal(err)
    }
    var up struct{ ID string }
    json.NewDecoder(resp.Body).Decode(&up)
    resp.Body.Close()
    if up.ID == "" {
        t.Fatal("upload returned no id")
    }

    // Poll the feed until our upload appears with a thumbnail, or give up.
    deadline := time.Now().Add(15 * time.Second)
    for time.Now().Before(deadline) {
        r, err := http.Get(base + "/api/feed")
        if err == nil {
            var items []struct {
                ID       string `json:"id"`
                ThumbURL string `json:"thumb_url"`
            }
            b, _ := io.ReadAll(r.Body)
            r.Body.Close()
            json.Unmarshal(b, &items)
            for _, it := range items {
                if it.ID == up.ID && it.ThumbURL != "" {
                    return // success: uploaded, thumbnailed, and in the feed
                }
            }
        }
        time.Sleep(500 * time.Millisecond)
    }
    t.Fatalf("upload %s did not appear in feed with a thumbnail in time", up.ID)
}
```

You run it against a live stack:

``` bash
$ docker compose up -d --build
$ GATEWAY_URL=http://localhost:8080 go test -tags=e2e ./test/e2e/...
```

This single test touches every component --- gateway routing, image storage, the upload event, the thumbnailer, the thumbnail event, and the feed read model --- and would fail if any link in that chain were broken. You want very few of these (they're slow and, being timing-dependent, can be flaky if the polling window is too tight), but the few you have are worth their weight: they catch the integration mistakes that unit tests, by construction, cannot see.

::: warning
**Polling windows and flakiness.** End-to-end tests over an asynchronous system are inherently timing-sensitive. Too short a deadline and the test fails on a slow CI runner even though the system is correct; too long and a real hang takes ages to surface. Prefer generous deadlines with frequent polls (as above), make the assertion specific (the *right* id, *with* a thumbnail), and treat a genuinely flaky end-to-end test as a bug to fix --- by widening the window or fixing a real race --- never as noise to rerun until green.
:::

### Summary

-   Test in three layers: many fast **unit** tests, a solid layer of **integration** tests against real dependencies, and a few **end-to-end** tests over the whole system.
-   Microservices shift weight toward integration tests, because a service's behavior is largely its interaction with a database or broker --- things a mock cannot verify.
-   Go's table-driven tests and `net/http/httptest` make unit-testing pure functions and handlers trivial; handlers that close over narrow interfaces (`storage.Blob`) accept in-memory fakes directly.
-   **Testcontainers** starts a real Postgres, Mongo, or NATS in Docker inside the test and tears it down after --- so integration tests run anywhere Docker runs, with no shared test database.
-   A `//go:build integration` tag keeps container-backed tests out of the fast inner loop; `go test ./...` stays quick, `go test -tags=integration ./...` runs the full suite in CI.
-   End-to-end tests must **poll**, because the system is eventually consistent; keep them few, specific, and generously timed.
-   Always run `-race` in CI.

A test suite is only valuable if it runs automatically. Next we put it in a pipeline that tests every commit and ships every passing build to the cloud --- continuous integration and delivery.
