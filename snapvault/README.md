# Snapvault

Snapvault is the companion code for *Microservices in Go: Building Cloud-Native
Systems with NATS and Google Cloud*. It is a small but complete community
photo-sharing backend: you upload an image, it is stored, a thumbnail is
generated asynchronously, metadata is recorded, and the image appears in a
public feed.

It is intended to be **read alongside the book** and **run end to end**. Every
pattern the book teaches — an API gateway, a database per service, asynchronous
messaging over a broker, containerization, Kubernetes, infrastructure as code,
GitOps delivery, and observability — appears here as working Go.

This repository is original work released under the [MIT License](LICENSE); you
are free to use, modify, and build on it.

## Architecture

```
                     ┌─────────────┐
   client ──────────▶│   gateway   │  (the only public entry point)
                     └──────┬──────┘
        ┌───────────────────┼───────────────────┐
        ▼                   ▼                   ▼
  ┌───────────┐       ┌───────────┐       ┌───────────┐
  │  images   │       │   feed    │       │ metadata  │
  │  (+blob)  │       │ (Postgres)│       │ (MongoDB) │
  └─────┬─────┘       └─────▲─────┘       └─────▲─────┘
        │ publish           │ consume           │ consume
        │ images.uploaded   │ images.>          │ images.uploaded
        ▼                   │                   │
   ┌─────────────────────  NATS JetStream  ─────────────────────┐
   │  stream: IMAGES   subjects: images.uploaded, images.thumbnailed
   └───────────────────────────────────────────────────────────┘
        ▲ consume images.uploaded          │ publish images.thumbnailed
        │                                  │
   ┌────┴───────┐                          │
   │ thumbnails │──────────────────────────┘
   │  (worker)  │
   └────────────┘
```

The **gateway** proxies uploads and image bytes and serves an aggregated
image-detail view. The **images** service stores originals and announces
`images.uploaded`. The **thumbnails** worker consumes that event, resizes the
image, and announces `images.thumbnailed`. The **metadata** service records a
document per image. The **feed** service builds the public gallery, reacting to
both events. Services never call each other's databases and communicate
asynchronously through NATS.

## Stack

| Concern         | Choice                                   |
| --------------- | ---------------------------------------- |
| Language        | Go 1.26                                  |
| Messaging       | NATS JetStream                           |
| Object storage  | Local disk (dev) / Google Cloud Storage  |
| Feed database   | PostgreSQL (Cloud SQL) via pgx v5        |
| Metadata store  | MongoDB v2 driver                        |
| Containers      | Docker, distroless images                |
| Orchestration   | Kubernetes (GKE Autopilot), Kustomize    |
| Infrastructure  | Terraform                                |
| Delivery        | Forgejo Actions + ArgoCD (GitOps)        |
| Observability   | OpenTelemetry traces, Prometheus metrics |

## Repository layout

```
internal/events/            shared message contract (subjects + payloads)
internal/platform/          infrastructure, no domain logic
  config/  log/  httpx/      env config, logging, HTTP server + middleware
  storage/                   Blob interface; disk + GCS backends
  messaging/natsutil/        NATS connect + stream setup
  metrics/  telemetry/       Prometheus + OpenTelemetry
services/
  gateway/                   edge: proxying + aggregation
  images/                    upload + serve + publish
  thumbnails/                background resize worker
  metadata/                  MongoDB-backed metadata + read API
  feed/                      PostgreSQL-backed public feed
migrations/                  SQL for the feed database
deploy/k8s/base/             Kubernetes manifests
deploy/k8s/overlays/prod/    prod Kustomize overlay (images, HPA, PDB, WI)
deploy/k8s/argocd-application.yaml
infra/                       Terraform (GCS, GKE, Cloud SQL, IAM, WIF, ArgoCD)
.forgejo/workflows/ci.yaml   test → build → promote pipeline
Dockerfile                   one parameterized build for every service
compose.yaml                 local development stack
```

## Prerequisites

- Go 1.26+
- Docker (with Compose) for the local stack and integration tests
- For deployment: a Google Cloud project, `gcloud`, `kubectl`, and Terraform

Generate `go.sum` once after cloning:

```sh
go mod tidy
```

## Run locally

```sh
docker compose up --build
```

Then exercise it:

```sh
# upload an image
curl -F "image=@photo.jpg" -F "owner=alice" http://localhost:8080/api/images
# -> {"id":"<id>"}

# the public feed (the thumbnail attaches a moment later)
curl http://localhost:8080/api/feed

# aggregated detail (feed entry + metadata)
curl http://localhost:8080/api/images/<id>

# the generated thumbnail
curl http://localhost:8080/thumbs/<id>.jpg --output thumb.jpg
```

## Tests

```sh
# fast unit tests
go test ./...

# integration tests (start real NATS/Postgres via Testcontainers; needs Docker)
go test -tags=integration ./...
```

## Build images

```sh
docker build --build-arg SERVICE=images -t snapvault/images .
# repeat for gateway, thumbnails, metadata, feed
```

## Deploy to Google Cloud

1. **Provision infrastructure** (bucket, Artifact Registry, GKE Autopilot,
   Cloud SQL, IAM, Workload Identity Federation, ArgoCD):

   ```sh
   cd infra
   cp terraform.tfvars.example terraform.tfvars   # edit values
   terraform init -backend-config="bucket=YOUR_TF_STATE_BUCKET"
   terraform apply
   ```

   Note the outputs (`wif_provider`, `deployer_service_account`,
   `artifact_registry`, …) — the CI secrets use them.

2. **Configure CI secrets** in your Forgejo repo: `GCP_WIF_PROVIDER`,
   `GCP_DEPLOYER_SA`, `GCP_PROJECT_ID`, `GCP_REGION`, and `CONFIG_REPO_TOKEN`.

3. **Hand deployment to ArgoCD**:

   ```sh
   kubectl apply -n argocd -f deploy/k8s/argocd-application.yaml
   ```

## GitOps flow

The cluster is never deployed to imperatively. On a push to `main`, the pipeline
runs tests, builds and pushes one image per service tagged with the commit SHA,
then commits those tags into a **config repository** (the rendered manifests in
`deploy/k8s/overlays/prod`, kept in their own repo). ArgoCD watches that repo
and syncs the change to the cluster. Rolling back is `git revert` on the config
repo. The ArgoCD `Application` ignores each Deployment's replica count so it
never fights the Horizontal Pod Autoscaler.

## Environment variables

| Variable               | Services           | Meaning                                  |
| ---------------------- | ------------------ | ---------------------------------------- |
| `PORT`                 | gateway, images, metadata, feed | HTTP port (default 8080)    |
| `HEALTH_PORT`          | thumbnails         | health probe port (default 8081)         |
| `NATS_URL`             | all but gateway    | NATS address                             |
| `STORAGE`              | images, thumbnails | `gcs` or disk (default)                  |
| `GCS_BUCKET`           | images, thumbnails | bucket name when `STORAGE=gcs`           |
| `DATA_DIR`             | images, thumbnails | disk path when not using GCS             |
| `MONGO_URI`/`MONGO_DB` | metadata           | MongoDB connection (or `MONGO_URI_FILE`) |
| `DATABASE_URL`         | feed               | PostgreSQL DSN (or `DATABASE_URL_FILE`)  |
| `IMAGES_URL`/`FEED_URL`/`METADATA_URL` | gateway | upstream service addresses           |
| `GOOGLE_CLOUD_PROJECT` | all                | enables Cloud Trace export when set      |
| `LOG_LEVEL`            | all                | `debug` for verbose logs                 |

## Mapping to the book

The shared platform packages and the first service correspond to the early
chapters; Docker and Compose to the containerization chapters; the data and
messaging chapters to the metadata/feed stores and the NATS pipeline; then
testing, CI/CD with Forgejo Actions, Kubernetes on GKE, Terraform, GitOps with
ArgoCD, and finally observability and production hardening. Read a chapter, then
read the matching code here.
