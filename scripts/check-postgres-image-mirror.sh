#!/usr/bin/env bash
# Pre-tag tripwire: copy each approved upstream image to a disposable directory.
set -euo pipefail

if ! command -v skopeo >/dev/null 2>&1; then
  case "${CI:-}:${GITHUB_ACTIONS:-}" in
    true:*|1:*|*:true)
      echo "postgres image mirror check: skopeo is required in CI" >&2
      exit 1
      ;;
  esac
  echo "postgres image mirror check skipped: no skopeo"
  exit 0
fi

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
work_dir=$(mktemp -d "${TMPDIR:-/tmp}/witself-postgres-image-check.XXXXXX")
trap 'rm -rf -- "$work_dir"' EXIT

config="$repo_root/images/postgresql/mirror.json"
jq -er '.cells | keys[]' "$config" >"$work_dir/cells"
while IFS= read -r cell; do
  bash "$repo_root/scripts/mirror-postgresql-image.sh" 0.0.0 "$cell" \
    "$config" "dir:$work_dir/$cell"
done <"$work_dir/cells"
echo "postgres image mirror check passed"
