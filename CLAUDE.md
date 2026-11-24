# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

This git repo holds two things: the book *Microservices in Go* (Markdown sources in `files/`, built into `Microservices-in-Go.epub`), and **Snapvault**, the companion Go backend in `snapvault/`. Go commands run from `snapvault/`, which has its own `go.mod` (module `codeberg.org/example/snapvault`).

## Commands

Snapvault (run from `snapvault/`):

```sh
go mod tidy                                   # after changing imports; go.mod and go.sum are committed together
go vet ./...                                  # the only static check
go vet -tags=integration ./...                # also vets the integration test files
gofmt -l .                                    # lists unformatted files
go test ./...                                 # unit tests
go test -race ./...                           # the full pre-push check is gofmt, both vets, this, and the image check below
go test -run TestName ./services/thumbnails/  # single unit test
go test -tags=integration ./...               # integration tests; needs Docker (Testcontainers starts Postgres/NATS)
go test -tags=integration -run TestEnsureStream_RoundTrip ./internal/platform/messaging/natsutil/   # single integration test
docker compose up --build                     # full local stack; gateway on localhost:8080
docker build --build-arg SERVICE=images -t snapvault/images .   # SERVICE: gateway|images|thumbnails|metadata|feed
deploy/k8s/check-images.sh                    # renders the prod overlay; fails on an unmapped placeholder image
GOTOOLCHAIN=go1.27.2 go run golang.org/x/vuln/cmd/govulncheck@latest ./...   # vuln scan; set GOTOOLCHAIN to the newest patch release
curl -F "image=@photo.png" localhost:8080/api/images   # smoke test against compose; the form field is `image`
```

govulncheck checks the standard library of the toolchain it runs under, so with an old local Go it reports stdlib issues that image builds don't have.

Book (run from `files/`; needs Python 3 and the Pandoc version pinned at the top of `build.sh`):

```sh
./build.sh   # writes files/Microservices-in-Go.epub and regenerates files/book-src.zip
mv Microservices-in-Go.epub ..   # the tracked EPUB is the one at the repo root
```

The copy `build.sh` leaves in `files/` is not gitignored, so move it to the root (as `epub.yaml` does) rather than committing it.

## Snapvault architecture

**Event flow.** The images service stores the original (`originals/<id>`) and publishes `images.uploaded` to the JetStream stream `IMAGES`, then returns `201 {id}`. Three independent consumers react, each with its own durable consumer:

- `thumbnails` (durable `thumbnailer`, filter `images.uploaded`) resizes to a 320px JPEG, writes `thumbs/<id>.jpg`, and publishes `images.thumbnailed`.
- `metadata` (durable `metadata`, filter `images.uploaded`) upserts a MongoDB document.
- `feed` (durable `feed`, filter `images.>`) inserts a Postgres row on `uploaded` and fills in thumbnail columns on `thumbnailed`.

Delivery is at-least-once, so every handler must be idempotent (feed uses `ON CONFLICT DO NOTHING`, metadata uses upsert). Undecodable or permanently failing messages are `Term`ed. Transient failures go through `natsutil.NakWithBackoff` (redelivery after 5s, 15s, 45s, then 2m), never a bare `Nak`, which redelivers at once and would spend all of `MaxDeliver: 5` in under a second. Idempotency has to hold across writers, not just per handler: feed's `Add` must stay `DO NOTHING`, because a `DO UPDATE` would let a redelivered `uploaded` erase a thumbnail. The feed integration tests assert this, but no CI runs them, so run `go test -tags=integration ./services/feed/` after touching the feed store or consumer. Ordering is not guaranteed either, since two feed replicas can handle an image's two events at once. `feed.SetThumb` therefore returns `errNoFeedItem` when the row doesn't exist yet, and the consumer answers with `NakWithBackoff` instead of acknowledging and losing the thumbnail. It gets the full backoff so it outlasts an upload event that is itself being retried.

**Boundaries.** The rules below are enforced by convention only; nothing checks them.

- `internal/events` is the only shared domain code (subjects and payloads). Services never import each other. The gateway re-declares the JSON shapes it needs.
- `internal/platform/` is infrastructure only. It holds env config, `httpx` (graceful `Serve`, health, request-ID middleware), the `storage.Blob` interface, `natsutil`, and telemetry/metrics/logging. `EnsureStream` is idempotent, and every service calls it at startup, so start order does not matter.
- Each service is `services/<name>/main.go`, which wires dependencies and defines handler methods, plus `store.go` where it has a database.
- Storage backend is chosen by env: `STORAGE=gcs` uses GCS, otherwise local disk at `DATA_DIR`. In compose, `images` and `thumbnails` share the `blob-data` volume, so the worker can read originals the images service wrote. Any new service that reads blobs needs the same storage config.
- `config.FileOrEnv` and `config.MustString` both read `<KEY>_FILE` when it's set, so cluster secrets arrive as mounted files and laptops use plain env vars. Only `config.String` ignores `_FILE`, so don't use it for secrets.

**Gateway** (`services/gateway`) is the only public entry point. It reverse-proxies `/images/{name}` and `/thumbs/{name}` to images, and `/api/feed` to feed. `POST /api/images` requires a user (see Authentication below); reads are public. `GET /api/images/{id}` aggregates two calls concurrently. The feed entry is essential and yields a 404 when missing. Metadata is enrichment with a 2s deadline. When metadata fails, the response still returns with `metadata_status: "unavailable"`.

**Authentication** (`services/gateway/auth.go`). Users sign in with Google Identity Platform. The gateway verifies the OIDC ID token with `go-oidc`, checking signature, issuer, audience and expiry. It then replaces the `Authorization` header with the verified user ID in `httpx.UserHeader` (`X-Snapvault-User`), and the images service takes the image's owner from that header and nothing else.
- The scheme is only sound because `stripInternalHeaders` deletes `X-Snapvault-User` from every inbound request. Keep it outermost around the mux, and never let a client-supplied value reach an internal service.
- Configuration fails closed:
  - `AUTH_ISSUER` + `AUTH_AUDIENCE` turn on real verification.
  - `AUTH_DEV_USER` alone is local dev mode (Compose sets it); every upload belongs to that user.
  - With neither set, the gateway refuses to start.
- Tests in `auth_test.go` run against a fake issuer with self-signed tokens, with no Google dependency.
- Internal services can trust the header because `deploy/k8s/base/networkpolicy.yaml` admits only gateway Pods (plus Managed Prometheus from `gmp-system`) to images. A new service that has to call images needs a rule there; a new service that serves a user-attributed write needs its own policy.

**Untrusted uploads.** The gateway serves uploaded bytes from its own origin, so two checks guard against stored XSS. `upload` accepts only what `image.DecodeConfig` recognizes as JPEG, PNG or GIF, and takes the content type from the bytes, not the client. `serveImage` declares only image content types (anything else becomes `application/octet-stream`) and sends `nosniff`. Keep both. The thumbnailer reads the header before decoding and `Term`s images over `maxPixels` (40 MP). Decoding allocates the full bitmap, so without that cap a tiny file declaring huge dimensions OOM-kills the 512Mi Pod on every redelivery.

**Thumbnails** (`services/thumbnails`) has no business HTTP surface. Its only HTTP listener is health and metrics on `HEALTH_PORT` (8081). Readiness requires both an attached consumer and a live NATS connection.

**Known gaps.** An upload whose publish fails after the blob is stored is logged, not failed, so the image is stored but never shows up in the feed or metadata. The feed service creates its schema at startup, and that is all Snapvault relies on. `migrations/001_init.sql` is the versioned copy, but no pipeline applies it, so keep the two in sync by hand.

## Deployment

- The Go version (1.27) appears in `go.mod`, the Dockerfile's `GO_VERSION`, `snapvault/.forgejo/workflows/ci.yaml`, and the book (chapters 0, 1, 2, 3 and 8), `files/metadata.yaml`, and `snapvault/README.md`. Chapter 1 has a "What Go 1.27 brings" section written from the release notes. A version bump touches all of them.
- `compose.yaml` publishes Postgres, MongoDB, and NATS on host ports 5432, 27017, and 4222, besides the gateway on 8080. A database already listening on one of those ports (including one in another WSL distro, since WSL2 distros share a network) stops that container from starting.
- One parameterized `Dockerfile` builds every service (distroless, `SERVICE` build arg). It ships an empty `/data` owned by UID 65532, so the Compose named volume mounted there is writable by the nonroot user. A volume created by an older image stays root-owned until you run `docker compose down -v`.
- `deploy/k8s/base` plus `deploy/k8s/overlays/prod` are Kustomize. The overlay's ConfigMap patch also sets `AUTH_ISSUER` and `AUTH_AUDIENCE` (the `auth_issuer`/`auth_audience` Terraform outputs); the base sets neither, so an unconfigured gateway won't start. Base manifests use placeholder images (`snapvault/<svc>:latest`), which the overlay's `images:` list maps to Artifact Registry. A mismatched name is silently left unmapped, which breaks both the overlay and CI's `kustomize edit set image snapvault/<svc>=...`. `deploy/k8s/check-images.sh` catches this; run it after touching the overlay or base image names. The overlay has `REGION`/`PROJECT_ID` placeholders that must be filled in.
- The feed Pod reaches Cloud SQL through a Cloud SQL Auth Proxy native sidecar (an init container with `restartPolicy: Always`), so its `DATABASE_URL` points at `127.0.0.1:5432`. The overlay patches in the instance connection name, which is the `cloudsql_connection_name` Terraform output.
- Production is GitOps. CI pushes images tagged with the commit SHA and pins those tags in an external config repo (`example/snapvault-config`). ArgoCD syncs from there, so do not `kubectl apply` by hand.
- `infra/` is Terraform for GCS, GKE Autopilot, Cloud SQL, IAM, Workload Identity Federation, ArgoCD, and Identity Platform (`identity.tf`). Validate without a backend: `terraform init -backend=false && terraform validate`, ideally in a copy so `.terraform/` doesn't land in the tree.
- The repo is hosted on GitHub (`Mufabo/microservices-with-go`; the old Codeberg repo is the `codeberg` remote). The only GitHub workflow is the EPUB release (below); nothing checks Snapvault on push, so run the Go checks above yourself. `snapvault/.forgejo/workflows/ci.yaml` is the Forgejo pipeline chapter 8 teaches. It is book companion code, not this repo's CI, and never runs here.

## Book source (`files/`)

- Chapters are `00-about.md` through `15-appendix.md`, listed explicitly in `build.sh`. Adding or renaming a chapter means editing `build.sh`.
- Asides use fenced divs (`::: note`, `::: tip`, `::: warning`). An em dash is written `---` inline. A lone `---` inside a code block is literal.
- Chapter code listings mirror `snapvault/`: identifiers, JSON fields, routes, and manifests. When you change one, change the other. Drift between the two was the main source of errors in the book.
- Figures are named by chapter (`images/chNN_*.png`).
- Code blocks have a dark background from `epub.css`, but token colors come from `code-dark.theme` (`--syntax-highlighting` in `build.sh`). Pandoc embeds the theme as `code span.xx` rules that outrank `epub.css`, so change highlight colors in the theme, not the stylesheet.
- `files/book-src.zip` is generated by `build.sh` (all sources under `book-src/`). Never edit it by hand.
- Pushing a change under `files/` to `main` runs `.github/workflows/epub.yaml`. That workflow commits the rebuilt `Microservices-in-Go.epub` (repo root) and `files/book-src.zip` when they change, and recreates the `latest` GitHub release with the EPUB attached. Builds are reproducible: `build.sh` pins the Pandoc version, which CI reads from that line, and fixes `SOURCE_DATE_EPOCH`. So a local build committed along with its sources matches CI exactly, and CI pushes nothing. Change the Pandoc version only by editing the pin.
