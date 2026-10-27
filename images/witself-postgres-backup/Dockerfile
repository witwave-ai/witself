# PostgreSQL 18.6 / Alpine 3.24, official multi-architecture index verified
# 2026-10-27 against Docker Hub and the registry content hash (amd64 + arm64).
# Keep the base digest and Alpine package pins in sync when updating this image.
# The base layer predates the Alpine fixes for brotli-libs CVE-2026-111497,
# libcurl CVE-2026-92392 and zlib CVE-2026-85091, so those three packages are
# upgraded in place from the Alpine 3.24 repositories and verified against the
# fixed revisions below; the build fails if the repositories move again.
FROM postgres:18-alpine@sha256:10a5c69a83035ab5239f5620d708520e8b28f5aa1f2b20143e6024532edadfb5

# The base supplies bash, psql, pg_dump, gzip and date. These exact age/aws-cli
# revisions are pinned per architecture in Alpine v3.24/community. Both now
# publish age revision r1; x86_64 retired r0 on 2026-10-16 after our release.
# Resolve packages only while building; the scheduled job needs no apk/mirrors.
RUN set -eu; \
    case "$(apk --print-arch)" in \
      x86_64) age_version=1.3.2-r1 ;; \
      aarch64) age_version=1.3.2-r1 ;; \
      *) echo "unsupported backup image architecture" >&2; exit 1 ;; \
    esac; \
    apk add --no-cache "age=$age_version" aws-cli=2.34.63-r0; \
    apk upgrade --no-cache brotli-libs libcurl zlib; \
    for pin in brotli-libs-1.2.1-r0 libcurl-8.23.0-r0 zlib-1.3.2-r1; do \
      apk info -v | grep -qx "$pin" || { echo "expected $pin" >&2; exit 1; }; \
    done; \
    for tool in bash psql pg_dump gzip date age aws; do command -v "$tool" >/dev/null; done; \
    age --version; \
    aws --version; \
    pg_dump --version; \
    psql --version

# The chart mounts and invokes the runner. Never inherit PostgreSQL server
# initialization as the backup image's default entrypoint.
ENTRYPOINT []
CMD ["/bin/bash"]
