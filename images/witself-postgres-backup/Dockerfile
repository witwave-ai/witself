# PostgreSQL 18.6 / Alpine 3.24.2, official multi-architecture index verified
# 2026-09-20 against Docker Hub and the registry content hash (amd64 + arm64).
# Keep the base digest and Alpine package pins in sync when updating this image.
FROM postgres:18-alpine@sha256:6c538e7206ea40ff740ef27883529390a690b6ead6ba96b44c67a9f7c638e8fd

# The base supplies bash, psql, pg_dump, gzip and date. These exact age/aws-cli
# revisions are available for both x86_64 and aarch64 in Alpine v3.24/community.
# Resolve packages only while building; the scheduled job needs no apk/mirrors.
RUN set -eu; \
    apk add --no-cache age=1.3.1-r6 aws-cli=2.34.63-r0; \
    for tool in bash psql pg_dump gzip date age aws; do command -v "$tool" >/dev/null; done; \
    age --version; \
    aws --version; \
    pg_dump --version; \
    psql --version

# The chart mounts and invokes the runner. Never inherit PostgreSQL server
# initialization as the backup image's default entrypoint.
ENTRYPOINT []
CMD ["/bin/bash"]
