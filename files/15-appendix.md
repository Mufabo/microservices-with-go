# Appendix --- Command Reference

This appendix gathers the commands used throughout the book into one place, grouped by tool, so you can find them quickly while building your own systems. Commands are shown with the Snapvault names (the `snapvault` cluster, the `snapvault` Artifact Registry repository, `us-central1`) so they line up with the chapters; substitute your own project ID, region, and names as needed. Throughout, `$PROJECT_ID` stands for your Google Cloud project.

## Go toolchain

The everyday commands for building, testing, and tidying a Go module.

``` bash
$ go mod init codeberg.org/example/snapvault   # start a module
$ go mod tidy                                 # add missing / drop unused deps
$ go build ./...                              # compile every package
$ go vet ./...                                # static checks
$ go test ./...                               # run fast (unit) tests
$ go test -race ./...                         # with the race detector
$ go test -tags integration ./...             # include integration tests
$ go test -run TestScaleToFit ./services/thumbnails   # one test
$ go fmt ./...                                # format
$ go run ./services/gateway                   # build and run a service
```

Building a static binary the way our Dockerfile does, for reference:

``` bash
$ CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /bin/app ./services/images
```

## Docker

Building and running the single parameterized image from Chapter 3.

``` bash
$ docker build --build-arg SERVICE=images -t snapvault/images:dev .
$ docker run --rm -p 8080:8080 \
    -e PORT=8080 -e STORAGE=disk snapvault/images:dev
$ docker images                               # list local images
$ docker logs <container>                     # view logs
$ docker exec -it <container> sh              # shell in (not distroless!)
$ docker system prune                         # reclaim space
```

Tagging and pushing to Artifact Registry (Chapter 8):

``` bash
$ gcloud auth configure-docker us-central1-docker.pkg.dev
$ docker tag snapvault/images:dev \
    us-central1-docker.pkg.dev/$PROJECT_ID/snapvault/images:$(git rev-parse --short HEAD)
$ docker push \
    us-central1-docker.pkg.dev/$PROJECT_ID/snapvault/images:$(git rev-parse --short HEAD)
```

## Docker Compose

Running the whole system locally (Chapters 4--6).

``` bash
$ docker compose up --build        # build and start everything
$ docker compose up -d             # start detached
$ docker compose ps                # what's running
$ docker compose logs -f images    # follow one service's logs
$ docker compose exec postgres psql -U snapvault   # into a dependency
$ docker compose down              # stop and remove
$ docker compose down -v           # also remove volumes (wipes data)
```

## NATS CLI

Inspecting the broker and JetStream (Chapter 6). Install the `nats` CLI separately; point it at your server with `--server` or `NATS_URL`.

``` bash
$ nats context add local --server nats://localhost:4222 --select
$ nats server check connection     # verify connectivity
$ nats stream ls                   # list JetStream streams
$ nats stream info IMAGES          # details: subjects, messages, replicas
$ nats consumer ls IMAGES          # consumers on the stream
$ nats consumer info IMAGES thumbnailer   # pending count, ack stats
$ nats sub "images.>"              # watch events live
$ nats pub images.uploaded '{"id":"test"}'   # publish a test event
$ nats stream view IMAGES          # browse stored messages
```

The `consumer info` pending count is the queue-depth signal we autoscale the thumbnailer on in Chapter 14.

## kubectl

Operating the cluster (Chapters 9 through 13). These assume the default namespace is `snapvault`.

``` bash
$ kubectl apply -f k8s/                 # apply all manifests in a dir
$ kubectl get pods                      # list pods
$ kubectl get pods -o wide -w           # watch, with node/IP detail
$ kubectl get deploy,svc,hpa            # deployments, services, autoscalers
$ kubectl describe pod <name>           # events and status for one pod
$ kubectl logs deployment/thumbnails    # logs from a deployment's pods
$ kubectl logs -f <pod> --previous      # follow; --previous for a crash
$ kubectl rollout status deployment/images
$ kubectl set image deployment/images images=...:NEW_SHA   # new version
$ kubectl rollout undo deployment/images          # roll back
$ kubectl scale deployment/thumbnails --replicas=4
$ kubectl exec -it <pod> -- sh          # only on images with a shell
$ kubectl port-forward svc/images 8080:8080   # reach a ClusterIP locally
$ kubectl get secret snapvault-secrets -o yaml
$ kubectl create namespace snapvault
$ kubectl config set-context --current --namespace=snapvault
```

## ArgoCD

GitOps operations (Chapter 12). Most "deploys" are a commit to the config repo, not a command --- these are for inspecting and, occasionally, intervening.

``` bash
$ argocd login <argocd-server>          # authenticate the CLI
$ argocd app list                       # list Applications
$ argocd app get snapvault              # status: Synced? Healthy?
$ argocd app diff snapvault             # what differs between Git and the cluster
$ argocd app sync snapvault             # force a sync now (else auto / 3-min poll / webhook)
$ argocd app history snapvault          # past synced revisions
$ kubectl get applications -n argocd    # Applications viewed as Kubernetes objects
$ kubectl apply -n argocd -f app.yaml   # create/update an Application declaratively
```

To roll back a deployment the GitOps way, revert in the config repo rather than touching the cluster --- ArgoCD syncs the previous state back:

``` bash
$ git revert <commit> && git push       # undo a deploy via the config repo
```

## gcloud

Project, GKE, Artifact Registry, and auth (Chapters 8--10).

``` bash
$ gcloud auth login
$ gcloud config set project $PROJECT_ID
$ gcloud services enable \
    container.googleapis.com artifactregistry.googleapis.com \
    sqladmin.googleapis.com secretmanager.googleapis.com

# GKE Autopilot
$ gcloud container clusters create-auto snapvault \
    --project=$PROJECT_ID --region=us-central1
$ gcloud container clusters get-credentials snapvault \
    --region=us-central1 --project=$PROJECT_ID

# Artifact Registry
$ gcloud artifacts repositories create snapvault \
    --repository-format=docker --location=us-central1
$ gcloud artifacts docker images list \
    us-central1-docker.pkg.dev/$PROJECT_ID/snapvault

# Secret Manager
$ gcloud secrets create snapvault-database-url --replication-policy=automatic
$ printf '%s' "$DSN" | gcloud secrets versions add snapvault-database-url --data-file=-
$ gcloud secrets versions access latest --secret=snapvault-database-url

# Storage
$ gcloud storage buckets create gs://snapvault-uploads-prod --location=us-central1
$ gcloud storage ls gs://snapvault-uploads-prod
```

## Terraform

The infrastructure-as-code loop from Chapter 11, run inside `infra/`.

``` bash
$ terraform init        # download providers, configure the backend
$ terraform fmt         # format .tf files
$ terraform validate    # check syntax and types
$ terraform plan        # preview the diff against real infrastructure
$ terraform apply       # enact the plan (prompts for confirmation)
$ terraform output      # print declared outputs (e.g. WIF provider name)
$ terraform state list  # what Terraform currently manages
$ terraform destroy     # tear it all down (guarded by deletion_protection)
```

A typical change is: edit a `.tf` file, `terraform fmt && terraform validate`, open a pull request, and let review plus `terraform plan` show the diff before `apply`.

## Git (for SHA-tagged images)

The commands the pipeline and your tagging rely on (Chapter 8).

``` bash
$ git rev-parse --short HEAD    # the short SHA used as an image tag
$ git tag v1.4.0 && git push --tags   # cut a release (adds a semver tag)
```

## A note on substitution

Every command above is deliberately concrete rather than templated, because concrete commands are easier to read and adapt than ones full of angle brackets. When you reuse them, the values you'll most often change are the project ID (`$PROJECT_ID`), the region (`us-central1`), the cluster and repository names (`snapvault`), and the bucket name (`snapvault-uploads-prod`). Everything else --- the structure of the commands, the flags, the workflow --- carries over unchanged to whatever system you build next. That, in the end, is what this book has been about: not Snapvault specifically, but the repeatable shape of building, shipping, and operating microservices in Go on Google Cloud, ready for you to apply to a problem of your own.
