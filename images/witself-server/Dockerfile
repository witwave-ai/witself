# Cell runtime image for witself-server and witself-worker. GoReleaser
# prebuilds both binaries and copies them in — nothing is compiled here, so the
# build is multi-arch-cheap. distroless static + nonroot keeps the image
# minimal and rootless.
FROM gcr.io/distroless/static:nonroot

ARG TARGETPLATFORM
COPY $TARGETPLATFORM/witself-server /usr/local/bin/witself-server
COPY $TARGETPLATFORM/witself-worker /usr/local/bin/witself-worker

# The server uses 8080 for the API. Both processes use 8081 for health probes
# and 9090 for Prometheus metrics.
EXPOSE 8080 8081 9090

ENTRYPOINT ["/usr/local/bin/witself-server"]
CMD ["serve"]
