# witself-control-plane image, built by wrangler for Cloudflare Containers.
# Unlike images/witself-server (COPY-only, binary prebuilt by GoReleaser), this
# is multi-stage: wrangler builds straight from source with the repo root as the
# build context. The release renderer injects VERSION, COMMIT, and DATE through
# wrangler.generated.jsonc image_vars. Cloudflare Containers requires linux/amd64.
FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
ARG COMMIT=none
ARG DATE=unknown
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build \
    -ldflags "-s -w \
      -X github.com/witwave-ai/witself/internal/version.Version=${VERSION} \
      -X github.com/witwave-ai/witself/internal/version.Commit=${COMMIT} \
      -X github.com/witwave-ai/witself/internal/version.Date=${DATE}" \
    -o /witself-control-plane ./cmd/witself-control-plane

FROM gcr.io/distroless/static:nonroot
COPY --from=build /witself-control-plane /usr/local/bin/witself-control-plane
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/witself-control-plane"]
