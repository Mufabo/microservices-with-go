# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

This git repo holds two things: the book *Microservices in Go* (Markdown sources in `files/`, built into `Microservices-in-Go.epub`), and **Snapvault**, the companion Go backend in `snapvault/`. Go commands run from `snapvault/`, which has its own `go.mod` (module `codeberg.org/example/snapvault`).

## Commands

Snapvault (run from `snapvault/`):

```sh
go mod tidy                                   # after changing imports; go.mod and go.sum are committed together
go vet ./...                                  # the only static check; CI runs it
gofmt -l .                                    # lists unformatted files
go test ./...                                 # unit tests
go test -race ./...                           # what CI runs
go test -run TestName ./services/thumbnails/  # single unit test
go test -tags=integration ./...               # integration tests; needs Docker (Testcontainers starts Postgres/NATS)
go test -tags=integration -run TestEnsureStream_RoundTrip ./internal/platform/messaging/natsutil/   # single integration test
docker compose up --build                     # full local stack; gateway on localhost:8080
docker build --build-arg SERVICE=images -t snapvault/images .   # SERVICE: gateway|images|thumbnails|metadata|feed
```

Book (run from `files/`, needs Pandoc 3.x; CI pins 3.5):

```sh
./build.sh   # writes files/Microservices-in-Go.epub
```

## Snapvault architecture

**Event flow.** The images service stores the original (`originals/<id>`) and publishes `images.uploaded` to the JetStream stream `IMAGES`, then returns `201 {id}`. Three independent consumers react, each with its own durable consumer:

- `thumbnails` (durable `thumbnailer`, filter `images.uploaded`) resizes to a 320px JPEG, writes `thumbs/<id>.jpg`, and publishes `images.thumbnailed`.
- `metadata` (durable `metadata`, filter `images.uploaded`) upserts a MongoDB document.
- `feed` (durable `feed`, filter `images.>`) inserts a Postgres row on `uploaded` and fills in thumbnail columns on `thumbnailed`.

Delivery is at-least-once, so every handler must be idempotent (feed uses `ON CONFLICT DO NOTHING`, metadata uses upsert). Undecodable or permanently failing messages are `Term`ed; transient failures are `Nak`ed and retried up to `MaxDeliver: 5`. Idempotency has to hold across writers, not just per handler: feed's `Add` must stay `DO NOTHING`, because a `DO UPDATE` would let a redelivered `uploaded` erase a thumbnail. Ordering is not guaranteed either, since two feed replicas can handle an image's two events at once. `feed.SetThumb` therefore returns `errNoFeedItem` when the row doesn't exist yet, and the consumer answers with `NakWithDelay(5s)` instead of acknowledging and losing the thumbnail.

**Boundaries.** The rules below are enforced by convention only; nothing checks them.

- `internal/events` is the only shared domain code (subjects and payloads). Services never import each other. The gateway re-declares the JSON shapes it needs.
- `internal/platform/` is infrastructure only. It holds env config, `httpx` (graceful `Serve`, health, request-ID middleware), the `storage.Blob` interface, `natsutil`, and telemetry/metrics/logging. `EnsureStream` is idempotent, and every service calls it at startup, so start order does not matter.
- Each service is `services/<name>/main.go`, which wires dependencies and defines handler methods, plus `store.go` where it has a database.
- Storage backend is chosen by env: `STORAGE=gcs` uses GCS, otherwise local disk at `DATA_DIR`. In compose, `images` and `thumbnails` share the `blob-data` volume, so the worker can read originals the images service wrote. Any new service that reads blobs needs the same storage config.
- `config.FileOrEnv` reads `<KEY>_FILE` when set, so cluster secrets arrive as mounted files and laptops use plain env vars. Every secret goes through it: feed reads `DATABASE_URL` and metadata reads `MONGO_URI` this way. A new secret read with `MustString` would ignore the mounted file.

**Gateway** (`services/gateway`) is the only public entry point. It reverse-proxies `/images/{name}` and `/thumbs/{name}` to images, and `/api/feed` to feed. `GET /api/images/{id}` aggregates two calls concurrently. The feed entry is essential and yields a 404 when missing. Metadata is enrichment with a 2s deadline. When metadata fails, the response still returns with `metadata_status: "unavailable"`.

**Thumbnails** (`services/thumbnails`) has no business HTTP surface. Its only HTTP listener is health and metrics on `HEALTH_PORT` (8081). Readiness requires both an attached consumer and a live NATS connection.

**Known gaps.** An upload whose publish fails after the blob is stored is logged, not failed, so the image is stored but never shows up in the feed or metadata. The feed service creates its schema at startup, and that is all Snapvault relies on. `migrations/001_init.sql` is the versioned copy, but no pipeline applies it, so keep the two in sync by hand. There is no authentication: `owner` is a client-supplied form field.

## Deployment

- One parameterized `Dockerfile` builds every service (distroless, `SERVICE` build arg).
- `deploy/k8s/base` plus `deploy/k8s/overlays/prod` are Kustomize. Base manifests use placeholder images (`snapvault/<svc>:latest`), which the overlay's `images:` list maps to Artifact Registry. CI's `kustomize edit set image snapvault/<svc>=...` depends on those names matching. The overlay has `REGION`/`PROJECT_ID` placeholders that must be filled in. Check changes with `kubectl kustomize deploy/k8s/overlays/prod`.
- The feed Pod reaches Cloud SQL through a Cloud SQL Auth Proxy native sidecar (an init container with `restartPolicy: Always`), so its `DATABASE_URL` points at `127.0.0.1:5432`. The overlay patches in the instance connection name, which is the `cloudsql_connection_name` Terraform output.
- Production is GitOps. CI pushes images tagged with the commit SHA and pins those tags in an external config repo (`example/snapvault-config`). ArgoCD syncs from there, so do not `kubectl apply` by hand.
- `infra/` is Terraform for GCS, GKE Autopilot, Cloud SQL, IAM, Workload Identity Federation, and ArgoCD.
- `snapvault/.forgejo/workflows/ci.yaml` sits below the repo root. The root's `.forgejo/` only holds `epub.yaml`, so this CI pipeline runs only if snapvault is split into its own repository root.

## Book source (`files/`)

- Chapters are `00-about.md` through `15-appendix.md`, listed explicitly in `build.sh`. Adding or renaming a chapter means editing `build.sh`.
- Asides use fenced divs (`::: note`, `::: tip`, `::: warning`). An em dash is written `---` inline. A lone `---` inside a code block is literal.
- Chapter code listings mirror `snapvault/`: identifiers, JSON fields, routes, and manifests. When you change one, change the other. Drift between the two was the main source of errors in the book.
- Figures are named by chapter (`images/chNN_*.png`).
- `files/book-src.zip` is a manual snapshot of the sources, laid out under `book-src/`. CI doesn't rebuild it, so regenerate it after source changes.
- Pushing a change under `files/` to `main` runs `.forgejo/workflows/epub.yaml`. That workflow commits the rebuilt `Microservices-in-Go.epub` at the repo root and republishes the `latest` release. Do not hand-edit the root EPUB, because the next build overwrites it. A local Pandoc newer than 3.5 builds fine but produces different bytes.
