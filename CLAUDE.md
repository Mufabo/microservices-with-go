# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

This git repo holds two things: the book *Microservices in Go* (Markdown sources in `files/`, built into `Microservices-in-Go.epub`), and **Snapvault**, the companion Go backend in `snapvault/`. Go commands run from `snapvault/`, which has its own `go.mod` (module `codeberg.org/example/snapvault`).

## Commands

Snapvault (run from `snapvault/`):

```sh
go mod tidy                                   # after changing imports; go.mod and go.sum are committed together
go vet ./...                                  # the only static check
gofmt -l .                                    # lists unformatted files
go test ./...                                 # unit tests
go test -race ./...                           # what CI runs (with gofmt, vet, and the image check below)
go test -run TestName ./services/thumbnails/  # single unit test
go test -tags=integration ./...               # integration tests; needs Docker (Testcontainers starts Postgres/NATS)
go test -tags=integration -run TestEnsureStream_RoundTrip ./internal/platform/messaging/natsutil/   # single integration test
docker compose up --build                     # full local stack; gateway on localhost:8080
docker build --build-arg SERVICE=images -t snapvault/images .   # SERVICE: gateway|images|thumbnails|metadata|feed
deploy/k8s/check-images.sh                    # renders the prod overlay; fails on an unmapped placeholder image
```

Book (run from `files/`; needs Python 3 and the Pandoc version pinned at the top of `build.sh`):

```sh
./build.sh   # writes files/Microservices-in-Go.epub and regenerates files/book-src.zip
```

## Snapvault architecture

**Event flow.** The images service stores the original (`originals/<id>`) and publishes `images.uploaded` to the JetStream stream `IMAGES`, then returns `201 {id}`. Three independent consumers react, each with its own durable consumer:

- `thumbnails` (durable `thumbnailer`, filter `images.uploaded`) resizes to a 320px JPEG, writes `thumbs/<id>.jpg`, and publishes `images.thumbnailed`.
- `metadata` (durable `metadata`, filter `images.uploaded`) upserts a MongoDB document.
- `feed` (durable `feed`, filter `images.>`) inserts a Postgres row on `uploaded` and fills in thumbnail columns on `thumbnailed`.

Delivery is at-least-once, so every handler must be idempotent (feed uses `ON CONFLICT DO NOTHING`, metadata uses upsert). Undecodable or permanently failing messages are `Term`ed; transient failures are `Nak`ed and retried up to `MaxDeliver: 5`. Idempotency has to hold across writers, not just per handler: feed's `Add` must stay `DO NOTHING`, because a `DO UPDATE` would let a redelivered `uploaded` erase a thumbnail. The feed integration tests assert this, but CI can't run them (no Docker on hosted runners), so run `go test -tags=integration ./services/feed/` after touching the feed store or consumer. Ordering is not guaranteed either, since two feed replicas can handle an image's two events at once. `feed.SetThumb` therefore returns `errNoFeedItem` when the row doesn't exist yet, and the consumer answers with `NakWithDelay(5s)` instead of acknowledging and losing the thumbnail.

**Boundaries.** The rules below are enforced by convention only; nothing checks them.

- `internal/events` is the only shared domain code (subjects and payloads). Services never import each other. The gateway re-declares the JSON shapes it needs.
- `internal/platform/` is infrastructure only. It holds env config, `httpx` (graceful `Serve`, health, request-ID middleware), the `storage.Blob` interface, `natsutil`, and telemetry/metrics/logging. `EnsureStream` is idempotent, and every service calls it at startup, so start order does not matter.
- Each service is `services/<name>/main.go`, which wires dependencies and defines handler methods, plus `store.go` where it has a database.
- Storage backend is chosen by env: `STORAGE=gcs` uses GCS, otherwise local disk at `DATA_DIR`. In compose, `images` and `thumbnails` share the `blob-data` volume, so the worker can read originals the images service wrote. Any new service that reads blobs needs the same storage config.
- `config.FileOrEnv` and `config.MustString` both read `<KEY>_FILE` when it's set, so cluster secrets arrive as mounted files and laptops use plain env vars. Only `config.String` ignores `_FILE`, so don't use it for secrets.

**Gateway** (`services/gateway`) is the only public entry point. It reverse-proxies `/images/{name}` and `/thumbs/{name}` to images, and `/api/feed` to feed. `GET /api/images/{id}` aggregates two calls concurrently. The feed entry is essential and yields a 404 when missing. Metadata is enrichment with a 2s deadline. When metadata fails, the response still returns with `metadata_status: "unavailable"`.

**Thumbnails** (`services/thumbnails`) has no business HTTP surface. Its only HTTP listener is health and metrics on `HEALTH_PORT` (8081). Readiness requires both an attached consumer and a live NATS connection.

**Known gaps.** An upload whose publish fails after the blob is stored is logged, not failed, so the image is stored but never shows up in the feed or metadata. The feed service creates its schema at startup, and that is all Snapvault relies on. `migrations/001_init.sql` is the versioned copy, but no pipeline applies it, so keep the two in sync by hand. There is no authentication: `owner` is a client-supplied form field.

## Deployment

- One parameterized `Dockerfile` builds every service (distroless, `SERVICE` build arg).
- `deploy/k8s/base` plus `deploy/k8s/overlays/prod` are Kustomize. Base manifests use placeholder images (`snapvault/<svc>:latest`), which the overlay's `images:` list maps to Artifact Registry. A mismatched name is silently left unmapped, which breaks both the overlay and CI's `kustomize edit set image snapvault/<svc>=...`. `deploy/k8s/check-images.sh` catches this and runs in CI. The overlay has `REGION`/`PROJECT_ID` placeholders that must be filled in.
- The feed Pod reaches Cloud SQL through a Cloud SQL Auth Proxy native sidecar (an init container with `restartPolicy: Always`), so its `DATABASE_URL` points at `127.0.0.1:5432`. The overlay patches in the instance connection name, which is the `cloudsql_connection_name` Terraform output.
- Production is GitOps. CI pushes images tagged with the commit SHA and pins those tags in an external config repo (`example/snapvault-config`). ArgoCD syncs from there, so do not `kubectl apply` by hand.
- `infra/` is Terraform for GCS, GKE Autopilot, Cloud SQL, IAM, Workload Identity Federation, and ArgoCD.
- CI for this repo is the root `.forgejo/workflows/snapvault.yaml`: gofmt, vet, unit tests with `-race`, and the image check, on Codeberg's hosted runners. `snapvault/.forgejo/workflows/ci.yaml`, which builds images and promotes them, sits below the repo root, so it runs only if snapvault is split into its own repository root.

## Book source (`files/`)

- Chapters are `00-about.md` through `15-appendix.md`, listed explicitly in `build.sh`. Adding or renaming a chapter means editing `build.sh`.
- Asides use fenced divs (`::: note`, `::: tip`, `::: warning`). An em dash is written `---` inline. A lone `---` inside a code block is literal.
- Chapter code listings mirror `snapvault/`: identifiers, JSON fields, routes, and manifests. When you change one, change the other. Drift between the two was the main source of errors in the book.
- Figures are named by chapter (`images/chNN_*.png`).
- `files/book-src.zip` is generated by `build.sh` (all sources under `book-src/`). Never edit it by hand.
- Pushing a change under `files/` to `main` runs `.forgejo/workflows/epub.yaml`. That workflow commits the rebuilt `Microservices-in-Go.epub` (repo root) and `files/book-src.zip` when they change, and republishes the `latest` release. Builds are reproducible: `build.sh` pins the Pandoc version, which CI reads from that line, and fixes `SOURCE_DATE_EPOCH`. So a local build committed along with its sources matches CI exactly, and CI pushes nothing. Change the Pandoc version only by editing the pin.
