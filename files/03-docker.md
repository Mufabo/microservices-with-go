# Chapter 3 --- Containerizing with Docker

Our images service runs on your machine. The next question is how to make it run identically on a colleague's machine, on a CI server, and on a Kubernetes node in Google Cloud --- without anyone installing Go or fiddling with paths. The answer is a **container**: your binary plus exactly the bits of operating system it needs, packaged into an image that runs the same everywhere. This chapter turns the images service into a tiny, secure image, and introduces the registry where Google Cloud will eventually pull it from.

## What a container gives you

The old curse of "it works on my machine" comes from the gap between the environment where software is built and the environment where it runs: different library versions, different paths, a dependency that happened to be installed on the developer's laptop. A container closes that gap by shipping the environment *with* the application. The image is immutable and self-contained; the same image you test is the exact one that runs in production, byte for byte.

This matters doubly for microservices. With one application you can perhaps manage one carefully-configured server. With a dozen services, each potentially needing different dependencies, manual environment management collapses. Containers make every service a uniform, deployable unit --- a box the orchestrator can place anywhere --- and that uniformity is what makes Kubernetes possible in Chapter 9.

Go and containers are an especially happy pairing, for the reason Chapter 1 mentioned: a Go program compiles to a single static binary with no runtime to install. That lets us build an image containing essentially *nothing but our binary* --- a few megabytes total --- where a typical interpreted-language image carries hundreds of megabytes of runtime and OS packages.

## A multi-stage Dockerfile

The key technique is the **multi-stage build**. We use a large image containing the full Go toolchain to compile our binary, then copy just that binary into a tiny final image and throw the toolchain away. The compiler never ships to production.

Create a `Dockerfile` at the repository root --- not inside `services/images/`, because this one file will build every service:

``` dockerfile
# syntax=docker/dockerfile:1

# ---- build stage: compile a static binary ----
FROM golang:1.27 AS build
WORKDIR /src

# Download dependencies first, in their own layer, so this step is cached
# and re-run only when go.mod/go.sum change — not on every source edit.
COPY go.* ./
RUN go mod download

# Now copy the source and build.
COPY . .
ARG SERVICE=images
RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/go/pkg/mod \
    CGO_ENABLED=0 GOOS=linux \
    go build -trimpath -ldflags="-s -w" -o /out/service ./services/${SERVICE}
RUN mkdir -p /out/data

# ---- final stage: just the binary ----
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/service /service
COPY --from=build --chown=65532:65532 /out/data /data
EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/service"]
```

Every line earns its place. Walking through the non-obvious ones:

-   **`CGO_ENABLED=0`** disables cgo, producing a fully static binary with no dependency on the system C library. This is what lets us use the `distroless/static` base, which contains no libc at all. Forget this flag and you'll get a binary that builds fine but won't start in the minimal image --- a classic first-container puzzle.
-   **`-trimpath`** strips your local filesystem paths out of the binary (so `/home/yourname/...` doesn't leak into stack traces), and **`-ldflags="-s -w"`** drops the symbol table and debug info, shaving the binary down. Together they make the artifact smaller and a touch more private.
-   **`COPY go.* ./` then `go mod download` as a separate step** is the most important caching trick. Docker caches each layer and reuses it if its inputs are unchanged. Because dependencies change far less often than source code, downloading them in their own layer means an ordinary code change reuses the cached dependency layer and rebuilds in seconds.
-   **The `--mount=type=cache` lines** go further, persisting Go's build and module caches *across* builds (a BuildKit feature, on by default in current Docker). The first build is cold; every build after is dramatically faster.
-   **`gcr.io/distroless/static-debian12:nonroot`** is a Google-maintained base image that contains a CA-certificates bundle, timezone data, and `/etc/passwd` --- and nothing else. No shell, no package manager, no utilities. That's a feature: an attacker who finds a way into your container finds no tools to pivot with, and there are essentially no OS packages to accumulate vulnerabilities. The `:nonroot` tag and the `USER nonroot` line mean the process runs as an unprivileged user, never root.
-   **The empty `/data` owned by UID 65532** (the `nonroot` user) is for Chapter 4. There, Compose mounts a *named volume* at `/data` for the images the service stores, and Docker seeds a new named volume from whatever the image has at that path, ownership included. Without this directory the volume comes up owned by root, the unprivileged service can't write to it, and every upload fails with `permission denied`. The build stage creates the directory because the distroless image has no shell to run `mkdir` in.
-   **`ARG SERVICE=images`** parameterizes which service to build. Because every Snapvault service shares this exact Dockerfile, in Chapter 8 we'll build all five from one template by passing `--build-arg SERVICE=feed` and so on.

Add a `.dockerignore` at the repository root so the build context stays small and clean --- you don't want your `.git` directory or local data shipped into the build:

    .git
    data
    *.md
    **/*_test.go
    .dockerignore

## Building and running

Build the image from the repository root (the `.` at the end is the build context --- the whole repo, which the multi-stage build needs because the module spans `services/` and `internal/`):

``` bash
$ docker build --build-arg SERVICE=images -t snapvault/images:dev .
```

Check how small it came out:

``` bash
$ docker images snapvault/images:dev
REPOSITORY        TAG   SIZE
snapvault/images  dev   8.7MB
```

Under ten megabytes, and most of that is your own binary. An equivalent service in a runtime-based language commonly lands between 100 MB and 1 GB. Multiply the difference across a dozen services and dozens of deploys a day and the savings in registry storage, network transfer, and pod startup time become very real.

Run it, mapping a host port to the container's port and mounting a directory of images:

``` bash
$ docker run --rm -p 8080:8080 \
    -e DATA_DIR=/data \
    -v "$(pwd)/data:/data:ro" \
    snapvault/images:dev
```

-   `-p 8080:8080` forwards localhost:8080 on your machine to port 8080 in the container.
-   `-e DATA_DIR=/data` sets the environment variable our `config` package reads --- the same mechanism that will configure the service in production, exercised here.
-   `-v "$(pwd)/data:/data:ro"` mounts your local `data` folder read-only into the container, so it can serve the sample image without baking images into the image.

Visit `http://localhost:8080/images/sample.jpg` --- the containerized service serves your photo, configured entirely through its environment.

::: warning
**Don't run as root, and don't bake secrets into images.** Two habits to lock in now. First, always run containers as a non-root user (the `nonroot` distroless tag does this for you); a container escape as root is a far worse day than one as an unprivileged user. Second, never `COPY` a secret --- a password, an API key, a service-account file --- into an image. Images get pushed to registries and shared; a secret in an image is a secret leaked. Secrets arrive at runtime through the environment or a secrets manager, which we set up in Chapters 9 and 14.
:::

## Where images live: Artifact Registry

A container image is only useful to a cluster if the cluster can pull it. That requires a **registry** --- a server that stores and serves images. On Google Cloud, that's **Artifact Registry**.

We'll automate this in Chapter 8 (CI builds and pushes for you) and provision the registry itself with Terraform in Chapter 11. But it's worth seeing the manual flow once so the automated version isn't a black box. Assuming you have the `gcloud` CLI installed and a project selected:

``` bash
# Create a Docker repository in Artifact Registry (once per project/region).
$ gcloud artifacts repositories create snapvault \
    --repository-format=docker \
    --location=us-central1 \
    --description="Snapvault service images"

# Let Docker authenticate to that region's registry using your gcloud login.
$ gcloud auth configure-docker us-central1-docker.pkg.dev
```

Images in Artifact Registry are named by a path that encodes the region, project, repository, and image:

    us-central1-docker.pkg.dev/PROJECT_ID/snapvault/images:TAG

Tag the local image with that full path and push it:

``` bash
$ docker tag snapvault/images:dev \
    us-central1-docker.pkg.dev/$PROJECT_ID/snapvault/images:v0.1.0
$ docker push \
    us-central1-docker.pkg.dev/$PROJECT_ID/snapvault/images:v0.1.0
```

::: tip
**Tag with versions, not just `latest`.** It's tempting to push everything as `:latest`, but `latest` is a moving target --- you can never be sure which build a pod is running, and rolling back becomes guesswork. Tag images with an immutable identifier instead: a semantic version like `v0.1.0`, or, in CI, the Git commit SHA. Then "what's running in production?" and "roll back to the previous build" both have precise answers. Chapter 8 tags every build with its commit SHA automatically.
:::

## Build it once, reuse it everywhere

You now have a repeatable recipe that takes Go source and produces a tiny, non-root, statically-linked image --- and crucially, the *same* recipe will produce every other Snapvault service by changing one build argument. That uniformity is the quiet superpower of containerizing: from the orchestrator's point of view, the images service, the feed service, and the thumbnails worker are interchangeable boxes that each expose a port and read their configuration from the environment. The orchestrator doesn't need to know or care that one resizes images and another serves JSON.

But running one container by hand with a long `docker run` line is not how you run a *system*. Snapvault is about to become five services plus a message broker plus two databases, and starting each by hand is a non-starter. The next chapter introduces Docker Compose, which describes the whole system in one file and brings it all up with one command.

### Summary

-   A container packages your binary with its environment, eliminating "works on my machine" and giving the orchestrator a uniform unit to deploy.
-   A multi-stage build compiles in a full toolchain image and copies only the resulting binary into a minimal final image, so the compiler never ships.
-   `CGO_ENABLED=0` yields a static binary that runs on a `distroless/static` base; the result is an image under 10 MB that runs as a non-root user with almost no attack surface.
-   Ordering the Dockerfile so dependencies download in a separate, cacheable layer --- and using BuildKit cache mounts --- makes rebuilds fast.
-   A single parameterized Dockerfile (`ARG SERVICE`) builds every Snapvault service.
-   Images are stored in and pulled from a registry; on Google Cloud that's Artifact Registry, and images should be tagged with immutable versions or commit SHAs, never just `latest`.
