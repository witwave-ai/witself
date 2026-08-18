#!/usr/bin/env bash
set -euo pipefail

if [[ $# -lt 2 || $# -gt 3 ]]; then
  echo "usage: $0 OUTPUT DIST_DIR [SOURCE_CONFIG]" >&2
  exit 2
fi

output=$1
dist_dir=$2
source_config=${3:-.goreleaser.yaml}

if [[ ! -f $source_config ]]; then
  echo "GoReleaser source config not found: $source_config" >&2
  exit 1
fi
if [[ $dist_dir != /* || $dist_dir == *$'\n'* || $dist_dir == *$'\r'* ]]; then
  echo "GoReleaser smoke dist must be an absolute single-line path" >&2
  exit 1
fi

extract_item() {
  local section=$1
  local id=$2
  awk -v section="$section" -v id="$id" '
    $0 == section ":" { in_section = 1; next }
    in_section && /^[^ ]/ { exit }
    in_section && $0 == "  - id: " id { capture = 1 }
    in_section && capture && /^  - id:/ && $0 != "  - id: " id { exit }
    capture { print }
  ' "$source_config"
}

extract_section() {
  local section=$1
  awk -v section="$section" '
    $0 == section ":" { capture = 1 }
    capture && $0 != section ":" && /^[^ ]/ { exit }
    capture { print }
  ' "$source_config"
}

{
  grep -m1 '^version:' "$source_config"
  grep -m1 '^project_name:' "$source_config"
  # JSON string syntax is valid YAML and safely preserves spaces and other
  # punctuation in the temporary directory path.
  printf 'dist: '
  printf '%s' "$dist_dir" | jq -Rs .
  printf '\nbuilds:\n'
  extract_item builds witself-control-plane
  printf '\narchives:\n'
  extract_item archives witself-control-plane
  printf '\nchecksum:\n'
  awk '
    $0 == "checksum:" { in_section = 1; next }
    in_section && /^[^ ]/ { exit }
    in_section { print }
  ' "$source_config"
  printf '\n'
  extract_section sboms
} >"$output"

[[ $(grep -c '^  - id: witself-control-plane$' "$output") -eq 2 ]]
[[ $(grep -Fc '{{ .FullCommit }}' "$output") -eq 1 ]]
grep -q '^    main: ./cmd/witself-control-plane$' "$output"
grep -q '^      - linux$' "$output"
grep -q '^      - darwin$' "$output"
grep -q '^      - amd64$' "$output"
grep -q '^      - arm64$' "$output"
grep -q '^    files:$' "$output"
grep -q '^      - none\*$' "$output"
if grep -q '{{ .ShortCommit }}\|id: witself$\|witself-server\|witself-worker\|witself-admin\|witself-infra\|dockers' "$output"; then
  echo "rendered smoke config contains a non-control-plane release pipeline or a short commit stamp" >&2
  exit 1
fi
