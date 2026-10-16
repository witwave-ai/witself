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
images=()
cleanup() {
  local image
  for image in "${images[@]}"; do
    docker image rm "$image" >/dev/null 2>&1 || true
  done
}
trap cleanup EXIT

# Alpine package revisions can diverge between architectures. Build each release
# platform without layer reuse so a cached APK install cannot conceal pin drift.
for architecture in amd64 arm64; do
  platform="linux/$architecture"
  image="witself-postgres-backup-check:$$-$architecture"
  images+=("$image")
  docker build --no-cache --platform "$platform" \
    --file "$repo_root/images/witself-postgres-backup/Dockerfile" \
    --tag "$image" "$repo_root/images/witself-postgres-backup"
  docker run --rm --network none --platform "$platform" --entrypoint /bin/sh \
    "$image" -ec 'age --version; aws --version; pg_dump --version'
done
echo "postgres backup image check passed (linux/amd64, linux/arm64)"
