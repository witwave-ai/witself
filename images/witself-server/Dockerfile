# Runtime image for witself-server. GoReleaser prebuilds the binary and copies
# it in — nothing is compiled here, so the build is multi-arch-cheap. distroless
# static + nonroot keeps the image minimal and rootless.
FROM gcr.io/distroless/static:nonroot

COPY witself-server /usr/local/bin/witself-server

# 8080 API, 8081 health probes, 9090 Prometheus metrics.
EXPOSE 8080 8081 9090

ENTRYPOINT ["/usr/local/bin/witself-server"]
CMD ["serve"]
