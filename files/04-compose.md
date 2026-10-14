# Chapter 4 --- Running a System with Docker Compose

One container is a program. A *system* is many containers that find each other, start in the right order, and share configuration --- and starting them by hand with a stack of `docker run` commands, each with its own flags, ports, and environment variables, is exactly the kind of error-prone ritual that containers were supposed to save us from. In this chapter we add a second service, the **gateway**, and introduce **Docker Compose**: a single file that declares the whole system and a single command that brings it up.

## A second service: the gateway

So far Snapvault has one service. To have a system worth orchestrating we need at least two, and the natural next one is the **gateway** --- the single public front door through which the outside world reaches Snapvault. Everything else (images, thumbnails, metadata, feed) becomes an internal service that the gateway talks to; clients never address them directly. This is a deliberate and common pattern, sometimes called an *API gateway* or *backend for frontend*, and it buys us a lot: one place to terminate TLS, one place to authenticate users (Chapter 14 adds it), one public IP to manage, and the freedom to rename, split, or move internal services without changing anything a client sees.

The gateway's first job is modest: serve a landing page and proxy image requests through to the images service. Create `services/gateway/main.go`:

``` go
package main

import (
    "net/http"
    "net/http/httputil"
    "net/url"
    "os"

    "codeberg.org/example/snapvault/internal/platform/config"
    "codeberg.org/example/snapvault/internal/platform/httpx"
    svclog "codeberg.org/example/snapvault/internal/platform/log"
)

func main() {
    log := svclog.New("gateway")
    addr := ":" + config.String("PORT", "8080")

    // Where the internal services live. In Compose and Kubernetes these
    // resolve to service names; locally you might point them at localhost.
    imagesURL := config.MustString("IMAGES_URL")
    target, err := url.Parse(imagesURL)
    if err != nil {
        log.Error("invalid IMAGES_URL", "value", imagesURL, "err", err)
        os.Exit(1)
    }
    imagesProxy := httputil.NewSingleHostReverseProxy(target)

    mux := http.NewServeMux()
    httpx.Health(mux, nil)
    mux.Handle("GET /images/", imagesProxy)
    mux.HandleFunc("GET /", home(log))

    if err := httpx.Serve(addr, mux, log); err != nil {
        log.Error("server stopped with error", "err", err)
        os.Exit(1)
    }
}

func home(log *slog.Logger) http.HandlerFunc {
    const page = `<!doctype html>
<title>Snapvault</title>
<h1>Snapvault</h1>
<p>A photo-sharing system built one service at a time.</p>`
    return func(w http.ResponseWriter, _ *http.Request) {
        w.Header().Set("Content-Type", "text/html; charset=utf-8")
        _, _ = w.Write([]byte(page))
    }
}
```

Add `"log/slog"` to the imports. The `httputil.NewSingleHostReverseProxy` is part of the standard library and does exactly what we want here: forward any request whose path begins with `/images/` to the images service and stream the response back. We are using Go's batteries rather than reaching for a third-party proxy, in keeping with the book's habit of adding dependencies only when the standard library genuinely falls short.

::: note
**The gateway will grow.** Right now it is a thin pass-through. As Snapvault gains a feed and metadata (Chapter 5) and an upload path (Chapter 6), the gateway becomes a true *aggregator* --- a single home page that fans out to several backend services, combines their responses, and degrades gracefully when one of them is slow. We are building it in the order a real project would: a working front door first, richer behavior as there is something to aggregate.
:::

## Describing the system

Compose reads a YAML file --- `compose.yaml` at the repository root --- that names each service, says how to build or pull its image, and wires up its environment, ports, networks, and volumes. Here is the starting point for Snapvault:

``` yaml
name: snapvault

services:
  images:
    build:
      context: .
      dockerfile: Dockerfile
      args:
        SERVICE: images
    environment:
      PORT: "8080"
      DATA_DIR: /data
      LOG_LEVEL: debug
    volumes:
      - images-data:/data

  gateway:
    build:
      context: .
      dockerfile: Dockerfile
      args:
        SERVICE: gateway
    environment:
      PORT: "8080"
      IMAGES_URL: http://images:8080
      LOG_LEVEL: debug
    ports:
      - "8080:8080"
    depends_on:
      - images

volumes:
  images-data:
```

A handful of things in that file are doing real work. The single parameterized `Dockerfile` from Chapter 3 builds *both* services --- only the `SERVICE` build argument differs, so Compose builds `images` and `gateway` from the same recipe. Each service gets `PORT: "8080"` because, on their own private network, there is no port collision; they can all happily listen on 8080. Only the gateway *publishes* a port to your host --- `"8080:8080"` maps your laptop's port 8080 to the gateway's --- which enforces the architecture physically: the only way in is through the gateway.

The line `IMAGES_URL: http://images:8080` is the piece that makes a distributed system feel like magic the first time. Compose puts every service on a shared network and runs a DNS resolver on it, so the hostname `images` resolves to the images container. Service discovery, for free, by name. The same names will work, unchanged, when we move to Kubernetes.

::: tip
**`compose.yaml`, not `docker-compose.yml`.** The Compose Specification standardized on the filename `compose.yaml`, and the modern `docker compose` command (a subcommand of `docker`, not the old standalone `docker-compose` binary) picks it up automatically. Both names still work, but new projects should prefer `compose.yaml` and the `docker compose` spelling.
:::

## Up, logs, and down

Bring the whole system up:

``` bash
$ docker compose up --build
```

Compose builds both images, creates the network and volume, and starts both containers, interleaving their logs in your terminal with a colored prefix per service. Because we log JSON, you'll see each service's structured lines side by side --- already a preview of what aggregated logging will feel like. Visit `http://localhost:8080/` and you get the gateway's landing page; visit `http://localhost:8080/images/sample.jpg` (after dropping a file into the volume, or rebuilding with a sample baked in) and the gateway proxies it from the images service.

To run it in the background, add `-d` (detached), then use the other lifecycle commands:

``` bash
$ docker compose up -d --build      # start in the background
$ docker compose ps                 # what's running
$ docker compose logs -f gateway    # follow one service's logs
$ docker compose down               # stop and remove containers + network
$ docker compose down -v            # also remove named volumes (deletes data)
```

That last distinction matters: `down` leaves your named volumes --- and therefore your data --- intact, while `down -v` wipes them. More than one developer has lost their local database by reflexively adding `-v`.

## Startup order is not readiness

The `depends_on: [images]` line tells Compose to *start* the images container before the gateway. It does **not** wait for images to be *ready* to serve traffic --- only for its container to have started. This is one of the most important early lessons in distributed systems, and Compose teaches it the hard way: the moment a container process exists, its dependents launch, even if the program inside is still opening database connections or warming a cache.

The lesson is not "find a way to make Compose wait." It's that **services must tolerate their dependencies being temporarily absent.** A gateway whose images service isn't ready yet should return a clean error --- `httputil.ReverseProxy` answers `502 Bad Gateway` when it can't reach the upstream --- not crash. A consumer whose message broker isn't up yet should retry the connection with backoff, not exit. We designed for this in Chapter 2 with the readiness endpoint, and we'll lean on it throughout: the network is unreliable and things start in an unpredictable order, so robustness comes from each service handling absence gracefully --- not from orchestrating a perfect boot sequence that reality will violate the first time a node reboots.

::: warning
**Distroless images have no shell, so the classic healthcheck won't run.** A common Compose pattern is a `healthcheck` that runs `curl` or `wget` inside the container. Our images are built `FROM distroless/static` --- they contain our binary and nothing else: no shell, no `curl`. That is exactly the small attack surface we wanted, but it means an in-container healthcheck would need a purpose-built check binary baked into the image. Rather than do that, we rely on each service's own resilience under Compose and let **Kubernetes** perform real HTTP health probing against our `/health/live` and `/health/ready` endpoints in Chapter 9 --- which is where readiness gating genuinely belongs.
:::

## Overriding configuration without editing the file

You'll often want to tweak the system locally --- bump a log level, point a service at a different dependency --- without editing the committed `compose.yaml`. Compose reads a `.env` file in the project directory and substitutes variables, so you can parameterize:

``` yaml
  images:
    environment:
      LOG_LEVEL: ${LOG_LEVEL:-info}
```

Here `${LOG_LEVEL:-info}` means "use the `LOG_LEVEL` from the environment or `.env`, defaulting to `info`." This same substitution syntax lets one `compose.yaml` serve every developer's machine, with personal overrides living in an untracked `.env` (add it to `.gitignore`). For genuinely different topologies --- say, a `compose.override.yaml` that adds debugging tools --- Compose merges multiple files, but a single file with sensible env defaults carries us a long way.

## What Compose is and isn't for

Compose is superb for local development and for simple, single-host deployments. It gives you the entire system on your laptop with one command, which is invaluable: every developer runs the real thing, integration tests (Chapter 7) spin up real dependencies, and "works on my machine" finally means "works." But Compose runs containers on *one* machine. It does not spread services across a fleet, reschedule them when a node dies, or scale a service to thirty copies behind a load balancer. Those are the jobs of an orchestrator, and from Chapter 9 onward that orchestrator is Kubernetes. Think of Compose as the development and testing harness for a system whose production home is a cluster --- the two are complementary, and the service definitions you write here map almost directly onto Kubernetes manifests later.

### Summary

-   A system needs a public front door; the **gateway** is Snapvault's single entry point, and every other service becomes internal, reachable only through it.
-   Docker Compose describes a multi-container system in one `compose.yaml` and manages its whole lifecycle with `docker compose up` / `down`.
-   Compose puts services on a shared network with DNS by service name, so `http://images:8080` just works --- the same discovery model Kubernetes uses.
-   Only the gateway publishes a host port, which physically enforces "the only way in is through the gateway."
-   `depends_on` controls *start* order, not *readiness*; robust systems tolerate absent dependencies rather than relying on a perfect boot sequence.
-   Compose is for local development and single-host runs; multi-node scaling and self-healing are Kubernetes' job, introduced in Chapter 9.

We now have two services cooperating on one network. The next thing that makes them genuinely independent --- and genuinely a microservices system rather than a distributed monolith --- is giving each its own data. That's Chapter 5.
