# syntax=docker/dockerfile:1
#
# One Dockerfile builds every Snapvault service. Select which one with the
# SERVICE build argument, e.g.:
#   docker build --build-arg SERVICE=images -t snapvault/images .
#
ARG GO_VERSION=1.26

# ---- build stage: compile a static binary ----
FROM golang:${GO_VERSION} AS build
ARG SERVICE=images
WORKDIR /src

# Download dependencies first, in their own cached layer, so this step re-runs
# only when go.mod/go.sum change — not on every source edit.
# (Run `go mod tidy` locally first to generate go.sum.)
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download

# Now copy the source and build the selected service.
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" \
    -o /out/app ./services/${SERVICE}

# ---- final stage: just the binary on a distroless base ----
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/app /app
USER nonroot
ENTRYPOINT ["/app"]
