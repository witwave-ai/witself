#!/usr/bin/env bash
# Pre-tag tripwire for the pinned backup image's base and Alpine packages.
set -euo pipefail

require_docker=false
case "${CI:-}" in true|1) require_docker=true ;; esac
if [[ ${GITHUB_ACTIONS:-} == true ]]; then require_docker=true; fi
if [[ $# -gt 0 ]]; then
  if [[ $# == 1 && $1 == --require-docker ]]; then
    require_docker=true
  else
    echo "usage: $0 [--require-docker]" >&2
    exit 2
  fi
fi

if ! command -v docker >/dev/null 2>&1 || ! docker info >/dev/null 2>&1; then
  if [[ $require_docker == true ]]; then
    echo "postgres backup image check: Docker is required in CI or with --require-docker" >&2
    exit 1
  fi
  echo "postgres backup image check skipped: no docker"
  exit 0
fi

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
image="witself-postgres-backup-check:$$"
cleanup() {
  docker image rm "$image" >/dev/null 2>&1 || true
}
trap cleanup EXIT

# One native CI platform is enough to detect missing pinned APK revisions.
# Keep the normal layer cache; the release build still covers both platforms.
docker build --platform linux/amd64 \
  --file "$repo_root/images/witself-postgres-backup/Dockerfile" \
  --tag "$image" "$repo_root/images/witself-postgres-backup"
docker run --rm --network none --platform linux/amd64 --entrypoint /bin/sh \
  "$image" -ec 'age --version; aws --version; pg_dump --version'
echo "postgres backup image check passed"
