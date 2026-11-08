#!/usr/bin/env bash
# Print only the absolute directory containing verified monitoring test assets.
# An override must include each required archive AND its executable exposed tool.
# Tool bytes must match extraction from that verified archive; overrides are read
# only. Cache tools are always re-extracted and exposed through relative symlinks
# to <tool>-<version>-<platform>, so stale extracted binaries cannot be reused.
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
pins_file="$repo_root/scripts/testdata/monitoring-test-assets.pins"

malformed_pins() {
  printf 'monitoring test assets: malformed pins file at line %s\n' "$1" >&2
  exit 1
}

names=() platforms=() versions=() checksums=() urls=()
count=0
line_number=0
[[ -f "$pins_file" ]] || malformed_pins 1
while IFS= read -r line || [[ -n "$line" ]]; do
  line_number=$((line_number + 1))
  [[ -z "$line" || "$line" =~ ^[[:space:]]*# ]] && continue
  read -r name platform version checksum url extra <<< "$line"
  [[ -n "$name" && -n "$platform" && -n "$version" && -n "$checksum" && -n "$url" && -z "$extra" ]] \
    || malformed_pins "$line_number"
  [[ "$line" == "$name $platform $version $checksum $url" ]] || malformed_pins "$line_number"
  [[ "$checksum" =~ ^[[:xdigit:]]{64}$ && "$version" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] \
    || malformed_pins "$line_number"
  case "$name:$platform" in
    chart:all) [[ "$url" == helm://https://*/* ]] || malformed_pins "$line_number" ;;
    promtool:linux-amd64|promtool:darwin-amd64|promtool:darwin-arm64|amtool:linux-amd64|amtool:darwin-amd64|amtool:darwin-arm64)
      [[ "$url" == https://* ]] || malformed_pins "$line_number" ;;
    *) malformed_pins "$line_number" ;;
  esac
  for ((i = 0; i < count; i++)); do
    [[ "${names[$i]}:${platforms[$i]}" != "$name:$platform" ]] || malformed_pins "$line_number"
  done
  names[count]="$name"
  platforms[count]="$platform"
  versions[count]="$version"
  checksums[count]="$(printf '%s' "$checksum" | tr 'A-F' 'a-f')"
  urls[count]="$url"
  count=$((count + 1))
done < "$pins_file"

host="$(uname -s)-$(uname -m)"
case "$host" in
  Linux-x86_64) tool_platform=linux-amd64 ;;
  Darwin-x86_64) tool_platform=darwin-amd64 ;;
  Darwin-arm64) tool_platform=darwin-arm64 ;;
  *) printf 'unsupported monitoring-test host: %s\n' "$host" >&2; exit 1 ;;
esac

required=()
required_count=0
for required_name in chart promtool amtool; do
  if [[ "$required_name" != chart ]] && command -v "$required_name" >/dev/null 2>&1; then
    continue
  fi
  found=false
  for ((i = 0; i < count; i++)); do
    if [[ "${names[$i]}" == "$required_name" && ( "${platforms[$i]}" == all || "${platforms[$i]}" == "$tool_platform" ) ]]; then
      required[required_count]="$i"
      required_count=$((required_count + 1))
      found=true
      break
    fi
  done
  "$found" || malformed_pins "$((line_number + 1))"
done

sha256_file() {
  local output
  if command -v sha256sum >/dev/null 2>&1; then
    output="$(sha256sum "$1")" || return 1
  else
    output="$(shasum -a 256 "$1")" || return 1
  fi
  printf '%s\n' "${output%% *}"
}

verified() {
  local actual
  [[ -f "$1" ]] || return 1
  actual="$(sha256_file "$1")" || return 1
  [[ "$actual" == "$2" ]]
}

checksum_failure() {
  printf 'monitoring test assets: checksum mismatch for %s; expected sha256 %s\n' "$1" "$checksum" >&2
  exit 1
}

publish_file() {
  # $1 staged source, $2 final cache path. [[ -d ]] follows symlinks, so a symlink to a directory is rejected too.
  if [[ -d "$2" ]]; then
    printf 'monitoring test assets: refusing to publish %s: destination is a directory\n' "${2##*/}" >&2
    exit 1
  fi
  mv -f "$1" "$2"
}

select_asset() {
  name="${names[$1]}"
  version="${versions[$1]}"
  checksum="${checksums[$1]}"
  url="${urls[$1]}"
  case "$name" in
    chart)
      chart_url="${url#helm://}"
      chart_name="${chart_url##*/}"
      archive="$chart_name-$version.tgz"
      ;;
    promtool) top_dir="prometheus-$version.$tool_platform"; archive="$top_dir.tar.gz" ;;
    amtool) top_dir="alertmanager-$version.$tool_platform"; archive="$top_dir.tar.gz" ;;
  esac
}

stage_dir=
trap 'if [[ -n "$stage_dir" ]]; then rm -rf "$stage_dir"; fi' EXIT
if [[ "${WITSELF_MONITORING_TEST_ASSETS+x}" == x ]]; then
  assets="$WITSELF_MONITORING_TEST_ASSETS"
  # Validate every archive before making even a temporary extraction directory.
  for index in "${required[@]}"; do
    select_asset "$index"
    [[ -n "$assets" ]] || checksum_failure "$archive"
    verified "$assets/$archive" "$checksum" || checksum_failure "$archive"
  done
  stage_dir="$(mktemp -d "${TMPDIR:-/tmp}/witself-monitoring-assets.XXXXXX")"
  for index in "${required[@]}"; do
    select_asset "$index"
    [[ "$name" != chart ]] || continue
    [[ -f "$assets/$name" && -x "$assets/$name" ]] || checksum_failure "$name"
    tar -xzf "$assets/$archive" -C "$stage_dir" "$top_dir/$name" >&2
    binary_checksum="$(sha256_file "$stage_dir/$top_dir/$name")"
    verified "$assets/$name" "$binary_checksum" || checksum_failure "$name"
  done
  (cd "$assets" && pwd -P)
  exit 0
fi

cache_dir="${WITSELF_TEST_ASSET_CACHE:-${XDG_CACHE_HOME:-$HOME/.cache}/witself/test-assets}"
mkdir -p "$cache_dir"
cache_dir="$(cd "$cache_dir" && pwd -P)"
# Stage on the cache filesystem so each verified file is published atomically.
stage_dir="$(mktemp -d "$cache_dir/.monitoring-assets.XXXXXX")"
export HELM_CACHE_HOME="$stage_dir/helm-cache"
export HELM_CONFIG_HOME="$stage_dir/helm-config"
export HELM_DATA_HOME="$stage_dir/helm-data"
export HELM_REGISTRY_CONFIG="$stage_dir/helm-registry.json"
export HELM_REPOSITORY_CACHE="$stage_dir/helm-cache/repository"
export HELM_REPOSITORY_CONFIG="$stage_dir/helm-repositories.yaml"

fetch_asset() {
  local attempt
  for attempt in 1 2 3; do
    rm -f "$stage_dir/$archive"
    if { if [[ "$name" == chart ]]; then
      helm pull --repo "${chart_url%/*}" "$chart_name" --version "$version" --destination "$stage_dir"
    else
      curl -fsSL --retry 0 "$url" -o "$stage_dir/$archive"
    fi; } >&2; then
      verified "$stage_dir/$archive" "$checksum" || checksum_failure "$archive"
      publish_file "$stage_dir/$archive" "$cache_dir/$archive"
      return 0
    fi
    printf 'monitoring test assets: %s attempt %s/3 failed\n' "$archive" "$attempt" >&2
    if [[ "$attempt" -lt 3 ]]; then
      sleep 20
    fi
  done
  printf 'monitoring test assets: fetch of %s failed after 3 attempts\n' "$archive" >&2
  exit 1
}

for index in "${required[@]}"; do
  select_asset "$index"
  if ! verified "$cache_dir/$archive" "$checksum"; then
    fetch_asset
  fi
  if [[ "$name" != chart ]]; then
    tar -xzf "$cache_dir/$archive" -C "$stage_dir" "$top_dir/$name" >&2
    [[ -f "$stage_dir/$top_dir/$name" && -x "$stage_dir/$top_dir/$name" ]] || checksum_failure "$name"
    binary="$name-$version-$tool_platform"
    publish_file "$stage_dir/$top_dir/$name" "$cache_dir/$binary"
    ln -s "$binary" "$stage_dir/$name"
    publish_file "$stage_dir/$name" "$cache_dir/$name"
  fi
done
for index in "${required[@]}"; do
  select_asset "$index"
  if ! [[ -f "$cache_dir/$archive" && ! -L "$cache_dir/$archive" ]]; then
    printf 'monitoring test assets: published %s is not a verified file\n' "$archive" >&2
    exit 1
  fi
  if [[ "$name" != chart ]]; then
    binary="$name-$version-$tool_platform"
    if ! [[ -f "$cache_dir/$binary" && -x "$cache_dir/$binary" && ! -L "$cache_dir/$binary" ]]; then
      printf 'monitoring test assets: published %s is not a verified file\n' "$binary" >&2
      exit 1
    fi
    if ! [[ -L "$cache_dir/$name" && "$(readlink "$cache_dir/$name")" == "$binary" && -x "$cache_dir/$name" ]]; then
      printf 'monitoring test assets: published %s is not a verified file\n' "$name" >&2
      exit 1
    fi
  fi
done
printf '%s\n' "$cache_dir"
