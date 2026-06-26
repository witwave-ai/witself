# syntax=docker/dockerfile:1
#
# Image: ghcr.io/witwave-ai/images/witself-server
#
# The witself-server backend image. Its entrypoint is the witself-server
# binary (serve, serve --dev, migrate). It exposes the API, health, and metrics
# ports declared in docs/scaffold-readiness.md.
#
# Multi-arch: built for linux/amd64 and linux/arm64. CGO is disabled so the
# resulting binary is a static executable that runs on the distroless static
# base as a non-root user.

# ---- builder ----------------------------------------------------------------
FROM golang:1.26 AS builder

# Build metadata, wired into internal/version via -ldflags. Defaults match the
# version package defaults ("dev") so an unparameterised build still succeeds.
ARG VERSION=dev
ARG COMMIT=dev
ARG DATE=dev

# Cross-compilation targets, populated by BuildKit for each platform when using
# `docker buildx build --platform linux/amd64,linux/arm64`.
ARG TARGETOS=linux
ARG TARGETARCH=amd64

ENV CGO_ENABLED=0 \
    GOOS=${TARGETOS} \
    GOARCH=${TARGETARCH}

WORKDIR /src

# Resolve modules first so this layer caches across source-only changes. The
# skeleton is stdlib-only, so there is no separate dependency-download step.
COPY go.mod go.sum* ./
RUN go mod download

COPY . .

# Build the server binary. -trimpath keeps paths reproducible; the -X flags
# wire the build identity into internal/version.
RUN go build \
        -trimpath \
        -ldflags="-s -w \
            -X github.com/witwave-ai/witself/internal/version.Version=${VERSION} \
            -X github.com/witwave-ai/witself/internal/version.Commit=${COMMIT} \
            -X github.com/witwave-ai/witself/internal/version.Date=${DATE}" \
        -o /out/witself-server \
        ./cmd/witself-server

# ---- final ------------------------------------------------------------------
# Distroless static: minimal base with CA certificates and a built-in nonroot
# user (uid/gid 65532). No shell, no package manager.
FROM gcr.io/distroless/static-debian12:nonroot

WORKDIR /

COPY --from=builder /out/witself-server /usr/local/bin/witself-server

# API (8080), health probes (8081), and Prometheus metrics (9090).
EXPOSE 8080 8081 9090

USER nonroot:nonroot

ENTRYPOINT ["/usr/local/bin/witself-server"]
