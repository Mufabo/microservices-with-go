# Chapter 2 --- Your First Go Microservice

A microservices system is just a collection of small programs that each do one job well. So the right place to start is with one such program, built carefully, because the structure you establish now is the structure you'll copy four more times before the book is done. We'll create the Snapvault repository, write the **images** service as a minimal HTTP server, and give it the production habits --- configuration from the environment, structured logging, graceful shutdown, health endpoints --- that every service in this book will share.

## Setting up Go 1.27

Install Go 1.27 from the official downloads if you don't already have it, then confirm:

``` bash
$ go version
go version go1.27.0 linux/amd64
```

Any 1.27.x patch release is fine. If you're on an older Go, upgrade --- the `go 1.27.0` line in our module requires it, so an older toolchain will either download 1.27 for you or refuse to build.

::: tip
**Let the toolchain manage itself.** Since Go 1.21, a `go` line like `go 1.27.0` in `go.mod` is not just documentation --- if your installed Go is newer, it's used; if a matching toolchain isn't present, the `go` command can download the right one automatically. You rarely need to install Go versions by hand anymore.
:::

## Creating the repository

Snapvault lives in a single Git repository --- a *monorepo* --- with one Go module shared across all services. We'll discuss the alternative (a repository and module per service) at the end of the chapter; for learning, one repo you can clone and run in one step is far kinder.

``` bash
$ mkdir snapvault && cd snapvault
$ git init -q
$ go mod init codeberg.org/example/snapvault
```

That last command writes a `go.mod`. Open it and set the toolchain line explicitly:

    module codeberg.org/example/snapvault

    go 1.27.0

Now lay out the directories. The layout below is the one we'll grow into:

    snapvault/
    ├── go.mod
    ├── internal/
    │   └── platform/          # shared infrastructure code
    │       ├── config/
    │       ├── httpx/
    │       └── log/
    └── services/
        ├── images/            # this chapter
        ├── thumbnails/
        ├── metadata/
        ├── feed/
        └── gateway/

Two conventions are worth naming. Everything reusable goes under `internal/platform`; Go's `internal` rule means those packages can be imported by our services but never by an outside module, which keeps our shared helpers from accidentally becoming a public API we have to support. And each service is a `package main` under `services/<name>`, so each compiles to its own binary that we can build, containerize, and deploy independently --- even though they live in one module today.

## The platform packages

We'll need three small shared packages before we write any service. They are deliberately tiny; resist the urge to build a framework.

### Configuration from the environment

Microservices read their configuration from environment variables. This is not a Go preference --- it's the contract Kubernetes, Docker, and every cloud platform expect, because it lets the same image run in development, staging, and production with nothing changed but the environment. Create `internal/platform/config/config.go`:

``` go
// Package config reads service configuration from environment variables.
package config

import (
    "os"
    "strconv"
)

// String returns the value of key, or def if it is unset or empty.
func String(key, def string) string {
    if v, ok := os.LookupEnv(key); ok && v != "" {
        return v
    }
    return def
}

// MustString returns the value of key, or panics if it is unset or empty.
// Use it for configuration a service cannot run without.
func MustString(key string) string {
    if v, ok := os.LookupEnv(key); ok && v != "" {
        return v
    }
    panic("config: required environment variable " + key + " is not set")
}

// Int returns the integer value of key, or def if it is unset or invalid.
func Int(key string, def int) int {
    if v, ok := os.LookupEnv(key); ok && v != "" {
        if n, err := strconv.Atoi(v); err == nil {
            return n
        }
    }
    return def
}
```

Panicking on missing *required* configuration is intentional: a service that cannot reach its database is useless, and failing loudly at startup is far better than limping along and erroring on the first request. Fail fast, fail visibly.

### Structured logging

In a system of many services, logs are your primary window into behavior, and you'll be reading them through a tool like Cloud Logging that indexes structured fields. So we log JSON, not prose, from the start. Go's standard `log/slog` does exactly this. Create `internal/platform/log/log.go`:

``` go
// Package log builds the structured logger used by every Snapvault service.
package log

import (
    "log/slog"
    "os"
)

// New returns a JSON logger tagged with the service name. Set LOG_LEVEL=debug
// to include debug records.
func New(service string) *slog.Logger {
    level := slog.LevelInfo
    if os.Getenv("LOG_LEVEL") == "debug" {
        level = slog.LevelDebug
    }
    handler := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level})
    return slog.New(handler).With("service", service)
}
```

Tagging every line with `"service"` means that once all five services are pouring logs into one place, you can still filter to just the images service in a click.

### A graceful HTTP server

The single most common mistake in a first network service is calling `http.ListenAndServe` and walking away. When Kubernetes wants to stop your pod --- to roll out a new version, to move it to another node --- it sends `SIGTERM` and expects the process to finish in-flight requests and exit. A server that ignores the signal gets killed mid-request, dropping client connections on every single deploy. We solve this once, in `internal/platform/httpx/httpx.go`:

``` go
// Package httpx provides a graceful HTTP server and common handlers.
package httpx

import (
    "context"
    "errors"
    "log/slog"
    "net/http"
    "os/signal"
    "syscall"
    "time"
)

// Serve runs h on addr and blocks until the process receives SIGINT or
// SIGTERM, then drains in-flight requests before returning.
func Serve(addr string, h http.Handler, log *slog.Logger) error {
    srv := &http.Server{
        Addr:              addr,
        Handler:           h,
        ReadHeaderTimeout: 5 * time.Second,
        ReadTimeout:       30 * time.Second,
        WriteTimeout:      120 * time.Second, // generous: uploads can be large
        IdleTimeout:       90 * time.Second,
    }

    ctx, stop := signal.NotifyContext(context.Background(),
        syscall.SIGINT, syscall.SIGTERM)
    defer stop()

    errc := make(chan error, 1)
    go func() {
        log.Info("http server listening", "addr", addr)
        if err := srv.ListenAndServe(); err != nil &&
            !errors.Is(err, http.ErrServerClosed) {
            errc <- err
        }
    }()

    select {
    case err := <-errc:
        return err // failed to bind, e.g. port in use
    case <-ctx.Done():
        log.Info("shutdown signal received; draining")
        drainCtx, cancel := context.WithTimeout(context.Background(),
            20*time.Second)
        defer cancel()
        return srv.Shutdown(drainCtx)
    }
}
```

The timeouts matter more than they look. A server with no read or write timeout can have its goroutines and memory exhausted by a single slow or malicious client that opens connections and never finishes them. The `Shutdown` call stops accepting new connections and waits up to twenty seconds for active handlers to return --- long enough for a normal request, short enough that a stuck deploy doesn't hang forever.

While we're here, add the health handlers every service and Kubernetes will use. Create `internal/platform/httpx/health.go`:

``` go
package httpx

import "net/http"

// Health registers liveness and readiness endpoints on mux.
//
//   - /health/live  reports that the process is up (used by Kubernetes to
//     decide whether to restart the container).
//   - /health/ready reports that the service can serve traffic (used to
//     decide whether to route requests to it). ready is a function so a
//     service can report "not ready" while a dependency is unavailable.
func Health(mux *http.ServeMux, ready func() bool) {
    mux.HandleFunc("GET /health/live", func(w http.ResponseWriter, _ *http.Request) {
        w.WriteHeader(http.StatusOK)
    })
    mux.HandleFunc("GET /health/ready", func(w http.ResponseWriter, _ *http.Request) {
        if ready == nil || ready() {
            w.WriteHeader(http.StatusOK)
            return
        }
        http.Error(w, "not ready", http.StatusServiceUnavailable)
    })
}
```

The distinction between *liveness* and *readiness* is one Kubernetes leans on heavily, and we'll wire both up properly in Chapter 9. For now, note the design: liveness means "the process is alive, don't kill me," readiness means "I can actually do useful work right now." A service waiting for its database connection is alive but not ready, and Kubernetes should keep it running while withholding traffic --- exactly what these two endpoints express.

## The images service, version one

Now the service itself. The first version does almost nothing --- it answers health checks and a placeholder route --- but it does so with the full production scaffolding in place. Create `services/images/main.go`:

``` go
package main

import (
    "net/http"
    "os"

    "codeberg.org/example/snapvault/internal/platform/config"
    "codeberg.org/example/snapvault/internal/platform/httpx"
    svclog "codeberg.org/example/snapvault/internal/platform/log"
)

func main() {
    log := svclog.New("images")
    addr := ":" + config.String("PORT", "8080")

    mux := http.NewServeMux()
    httpx.Health(mux, nil) // always ready for now

    mux.HandleFunc("GET /", func(w http.ResponseWriter, _ *http.Request) {
        _, _ = w.Write([]byte("snapvault images service\n"))
    })

    if err := httpx.Serve(addr, mux, log); err != nil {
        log.Error("server stopped with error", "err", err)
        os.Exit(1)
    }
    log.Info("server stopped cleanly")
}
```

Notice the routing. Since Go 1.22, the standard `http.ServeMux` understands method-and-path patterns like `GET /health/live` and path variables like `GET /images/{id}` --- which we'll use shortly. For a long time, "you need a router library in Go" was received wisdom; for the routes a typical service exposes, that's no longer true. One fewer dependency.

Tidy the module and run it:

``` bash
$ go mod tidy
$ go run ./services/images
```

You'll see a JSON log line:

``` json
{"time":"2026-06-20T10:15:04Z","level":"INFO","msg":"http server listening","service":"images","addr":":8080"}
```

In another terminal, exercise it:

``` bash
$ curl localhost:8080/
snapvault images service
$ curl -i localhost:8080/health/live
HTTP/1.1 200 OK
```

Now press `Ctrl-C` in the server terminal. Instead of dying instantly, it logs `shutdown signal received; draining` and then `server stopped cleanly`. That graceful exit is the whole point --- when Kubernetes sends the same signal in Chapter 9, the service will behave identically.

## Serving an actual image

A service called "images" should serve an image. We don't have cloud storage yet --- that's Chapter 5 --- so we'll serve from local disk, structured so the switch to Google Cloud Storage later changes one small piece and nothing else.

The trick that makes that switch painless is to define what we need as an interface and depend on the interface, not on the disk. Create `services/images/store.go`:

``` go
package main

import (
    "context"
    "io"
    "os"
    "path/filepath"
)

// blobStore is the storage Snapvault's images service needs. Today it's
// backed by the local filesystem; in Chapter 5 a Google Cloud Storage
// implementation will satisfy the same interface, and main won't change.
type blobStore interface {
    Get(ctx context.Context, key string) (io.ReadCloser, error)
}

// diskStore serves files from a base directory.
type diskStore struct{ base string }

func (d diskStore) Get(_ context.Context, key string) (io.ReadCloser, error) {
    // filepath.Clean + Join guards against "../" path-traversal in key.
    path := filepath.Join(d.base, filepath.Clean("/"+key))
    return os.Open(path)
}
```

That `filepath.Clean("/" + key)` is not decoration. Without it, a request for `/images/../../etc/passwd` would happily walk out of your data directory. Sanitizing untrusted input before it touches the filesystem is a habit, not an afterthought.

Now wire a handler that uses the interface. Update `services/images/main.go` to construct a store and serve from it:

``` go
func main() {
    log := svclog.New("images")
    addr := ":" + config.String("PORT", "8080")

    store := diskStore{base: config.String("DATA_DIR", "./data")}

    mux := http.NewServeMux()
    httpx.Health(mux, nil)
    mux.HandleFunc("GET /images/{id}", serveImage(store, log))

    if err := httpx.Serve(addr, mux, log); err != nil {
        log.Error("server stopped with error", "err", err)
        os.Exit(1)
    }
}

func serveImage(store blobStore, log *slog.Logger) http.HandlerFunc {
    return func(w http.ResponseWriter, r *http.Request) {
        id := r.PathValue("id")
        rc, err := store.Get(r.Context(), id)
        if err != nil {
            log.Warn("image not found", "id", id, "err", err)
            http.Error(w, "not found", http.StatusNotFound)
            return
        }
        defer rc.Close()

        w.Header().Set("Content-Type", "image/jpeg")
        w.Header().Set("Cache-Control", "public, max-age=86400")
        if _, err := io.Copy(w, rc); err != nil {
            log.Error("failed writing image", "id", id, "err", err)
        }
    }
}
```

You'll need to add `"io"` and `"log/slog"` to the imports. Drop a JPEG into `./data/sample.jpg`, run the service, and visit `http://localhost:8080/images/sample.jpg` --- your image appears.

The pattern here recurs throughout the book and is worth internalizing: a handler is a function that *closes over* its dependencies (the store, the logger) and returns an `http.HandlerFunc`. This keeps dependencies explicit --- you can see exactly what `serveImage` needs from its signature --- and makes the handler trivially testable, because a test can pass a fake `blobStore` (Chapter 7 does precisely this).

::: note
**Why an interface for one implementation?** It looks like over-engineering to define `blobStore` when `diskStore` is the only thing that satisfies it. The payoff arrives in Chapter 5, when a `gcsStore` satisfies the same interface and `main` swaps `diskStore{...}` for `gcsStore{...}` with no other change, and again in Chapter 7, when a test passes an in-memory fake. Depending on a narrow interface rather than a concrete type is the single most useful design habit in this style of system --- but note the restraint: the interface has exactly one method, the one method we actually use. We didn't speculatively add `Put`, `Delete`, or `List` until a caller needs them.
:::

## One repo or many?

We chose a monorepo with one module. The principled alternative is a repository (and Go module) per service, which is what you'll often see in the wild. The trade-off is real and worth understanding.

A **module per service** maximizes independence: each service has its own dependency versions, its own version history, its own CI pipeline, and a hard wall that makes it impossible to accidentally share code between services through a function call instead of a network call. The cost is ceremony --- to share even the tiny `config` helper, you must publish it as its own versioned module and depend on it, and a change touching two services becomes two pull requests across two repositories.

A **monorepo** keeps everything in one place: one clone, one `go build ./...`, trivial code sharing, atomic cross-service changes. The risk is that easy sharing tempts you to couple services through shared code in ways that undermine their independence. The discipline that keeps a monorepo honest is simple: shared code under `internal/platform` may contain *infrastructure* (how to connect to NATS, how to log) but never *domain logic* (what an image is, what the feed contains). Plumbing can be shared; meaning cannot.

For Snapvault, and for most teams below a certain size, the monorepo's tractability wins, and that's what we'll use. When a service truly needs to diverge --- its own release cadence, its own language --- you lift it out into its own repository, and the network contracts between services mean nothing else has to change.

### Summary

-   A microservices system is built one small program at a time; the scaffolding you establish on the first service is the template for the rest.
-   Configuration comes from environment variables, logs are structured JSON, and the HTTP server shuts down gracefully on `SIGTERM` --- these are not optional polish; they are what makes a service behave in a container.
-   Go 1.22+'s `ServeMux` handles method-and-path routing and path variables, so a typical service needs no router dependency.
-   Handlers that close over their dependencies and depend on narrow interfaces (like `blobStore`) are explicit and testable, and they let us swap local disk for cloud storage later with a one-line change.
-   We use a monorepo with one module for tractability, sharing only infrastructure code --- never domain logic --- under `internal/platform`.

With one service running well, the next question is how to package it so it runs the same way everywhere. That's Docker, and it's next.
