# Runtime image for ws. GoReleaser prebuilds the binary and copies it into
# this image, so the container contains the same executable as the release
# archive. distroless static + nonroot keeps it small and rootless.
FROM gcr.io/distroless/static:nonroot

COPY ws /usr/local/bin/ws

ENTRYPOINT ["/usr/local/bin/ws"]
