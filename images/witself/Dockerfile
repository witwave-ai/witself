# Runtime image for the witself CLI. GoReleaser prebuilds the binary and
# copies it into this image, so the container contains the same executable
# as the release archive. distroless static + nonroot keeps it small and
# rootless.
FROM gcr.io/distroless/static:nonroot

COPY witself /usr/local/bin/witself

ENTRYPOINT ["/usr/local/bin/witself"]
