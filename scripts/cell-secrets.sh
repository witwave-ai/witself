#!/usr/bin/env bash
# Keep plaintext out of shell variables, arguments, tracing, and temporary files.
set +x
set -Eeuo pipefail
umask 077
ulimit -c 0

for binary in ruby sops age; do
  command -v "$binary" >/dev/null 2>&1 || {
    printf 'cell-secrets: required binary missing: %s\n' "$binary" >&2
    exit 1
  }
done
repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
exec ruby "$repo_root/scripts/lib/cell-secrets.rb" "$@"
