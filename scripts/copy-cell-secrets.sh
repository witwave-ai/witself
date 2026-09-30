#!/usr/bin/env bash
# Copies or compares the agent-email cohort and canary Secrets between cells.
# Secret data never enters a shell variable, an argument, a trace or a file:
# the Ruby helper keeps it in memory and hands it to kubectl through a pipe.
set +x
set -Eeuo pipefail
umask 077
ulimit -c 0

for binary in ruby kubectl; do
  command -v "$binary" >/dev/null 2>&1 || {
    printf 'copy-cell-secrets: required binary missing: %s\n' "$binary" >&2
    exit 1
  }
done
repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
exec env -u RUBYOPT -u RUBYLIB ruby "$repo_root/scripts/lib/copy-cell-secrets.rb" "$@"
