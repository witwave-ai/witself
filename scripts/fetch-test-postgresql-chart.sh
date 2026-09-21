#!/usr/bin/env bash
# Print only the absolute path of the checksum-verified PostgreSQL test chart.
set -euo pipefail

chart_version=18.8.0
chart_archive="postgresql-${chart_version}.tgz"
chart_sha256="fe14c233d3544f04a6d20831896273984e7ead129404f5205890deeebe5f18d9"

sha256_file() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | awk '{print $1}'
  else
    shasum -a 256 "$1" | awk '{print $1}'
  fi
}

verified() {
  local checksum
  [[ -f "$1" ]] || return 1
  checksum="$(sha256_file "$1")" || return 1
  [[ "$checksum" == "$chart_sha256" ]]
}

checksum_failure() {
  printf 'PostgreSQL test chart: checksum mismatch; expected sha256 %s\n' "$chart_sha256" >&2
  exit 1
}

if [[ "${WITSELF_TEST_POSTGRESQL_CHART+x}" == x ]]; then
  verified "$WITSELF_TEST_POSTGRESQL_CHART" || checksum_failure
  chart_dir="$(cd "$(dirname "$WITSELF_TEST_POSTGRESQL_CHART")" && pwd -P)"
  printf '%s/%s\n' "$chart_dir" "${WITSELF_TEST_POSTGRESQL_CHART##*/}"
  exit 0
fi

cache_dir="${WITSELF_TEST_CHART_CACHE:-${XDG_CACHE_HOME:-$HOME/.cache}/witself/test-charts}"
if verified "$cache_dir/$chart_archive"; then
  printf '%s/%s\n' "$(cd "$cache_dir" && pwd -P)" "$chart_archive"
  exit 0
fi

mkdir -p "$cache_dir"
cache_dir="$(cd "$cache_dir" && pwd -P)"
# Stage on the cache filesystem so publishing the verified archive is atomic.
download_dir="$(mktemp -d "$cache_dir/.postgresql-${chart_version}.XXXXXX")"
trap 'rm -rf "$download_dir"' EXIT
export HELM_CACHE_HOME="$download_dir/helm-cache"
export HELM_CONFIG_HOME="$download_dir/helm-config"
export HELM_DATA_HOME="$download_dir/helm-data"
export HELM_REGISTRY_CONFIG="$download_dir/helm-registry.json"

for attempt in 1 2 3; do
  rm -f "$download_dir/$chart_archive"
  if helm pull oci://registry-1.docker.io/bitnamicharts/postgresql \
    --version "$chart_version" --destination "$download_dir" \
    >"$download_dir/helm.log" 2>&1; then
    verified "$download_dir/$chart_archive" || checksum_failure
    mv -f "$download_dir/$chart_archive" "$cache_dir/$chart_archive"
    printf '%s/%s\n' "$cache_dir" "$chart_archive"
    exit 0
  fi
  printf 'PostgreSQL test chart: helm pull attempt %s/3 failed\n' "$attempt" >&2
  if [[ "$attempt" -lt 3 ]]; then
    sleep 20
  fi
done

echo 'PostgreSQL test chart: fetch failed after 3 attempts' >&2
exit 1
