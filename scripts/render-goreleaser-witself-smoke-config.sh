#!/usr/bin/env bash
set -euo pipefail

if [[ $# -lt 1 || $# -gt 2 ]]; then
  echo "usage: $0 OUTPUT [SOURCE_CONFIG]" >&2
  exit 2
fi

output=$1
source_config=${2:-.goreleaser.yaml}

if [[ ! -f "$source_config" ]]; then
  echo "GoReleaser source config not found: $source_config" >&2
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
  printf '\nbuilds:\n'
  extract_item builds witself
  printf '\narchives:\n'
  extract_item archives witself
  printf '\n'
  extract_section checksum
} >"$output"

[[ $(grep -c '^  - id: witself$' "$output") -eq 2 ]]
grep -q '^      - windows$' "$output"
grep -q '^          - zip$' "$output"
if grep -q 'witself-server\|witself-admin\|witself-infra\|dockers' "$output"; then
  echo "rendered smoke config contains a non-CLI release pipeline" >&2
  exit 1
fi
