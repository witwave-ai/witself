#!/usr/bin/env bash
# Local-only migration rehearsal. The owner of the container also owns cleanup.
set -euo pipefail
export LC_ALL=C LANG=C
unset DOCKER_CONTEXT DOCKER_HOST DOCKER_TLS_VERIFY DOCKER_CERT_PATH DOCKER_API_VERSION
unset WITSELF_REHEARSAL_WORK WITSELF_REHEARSAL_CONTAINER WITSELF_REHEARSAL_ROOT
if [[ ${WITSELF_REHEARSAL_TEST_MODE:-} == 1 ]]; then
  export PATH="${WITSELF_REHEARSAL_TEST_BIN:?test tool directory required}"
else
  export PATH=/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin
fi
root=$(CDPATH='' cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd -P)
work=$(mktemp -d "${TMPDIR:-/tmp}/witself-rehearsal.XXXXXXXX")
name="witself-rehearsal-${work##*.}"
child=''
cleaning=0
# shellcheck disable=SC2329 # Invoked indirectly by the EXIT trap below.
cleanup() {
  trap '' INT TERM HUP
  [[ "$cleaning" == 0 ]] || return
  cleaning=1
  status=$1
  if [[ -n "$child" ]]; then
    kill -TERM "$child" 2>/dev/null || true
    wait "$child" 2>/dev/null || true
  fi
  # Never inherit a remote Docker context or a credential-bearing configuration.
  DOCKER_CONFIG="$work/docker" docker --host unix:///var/run/docker.sock rm -f -v "$name" >/dev/null 2>&1 || true
  if DOCKER_CONFIG="$work/docker" docker --host unix:///var/run/docker.sock inspect "$name" >/dev/null 2>&1; then
    printf 'rehearse-store-migrations: cleanup_failed container=%s\n' "$name" >&2
    [[ "$status" != 0 ]] || status=1
  fi
  DOCKER_CONFIG="$work/docker" docker --host unix:///var/run/docker.sock volume rm "$name-data" >/dev/null 2>&1 || true
  if DOCKER_CONFIG="$work/docker" docker --host unix:///var/run/docker.sock volume inspect "$name-data" >/dev/null 2>&1; then
    printf 'rehearse-store-migrations: cleanup_failed volume=%s-data\n' "$name" >&2
    [[ "$status" != 0 ]] || status=1
  fi
  rm -rf "$work"
  trap - EXIT
  exit "$status"
}
trap 'cleanup "$?"' EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
trap 'exit 129' HUP
mkdir -p "$work/docker"
export WITSELF_REHEARSAL_WORK="$work" WITSELF_REHEARSAL_CONTAINER="$name" WITSELF_REHEARSAL_ROOT="$root"
python3 "$root/scripts/lib/rehearse_store_migrations.py" "$@" <&0 &
child=$!
status=0
wait "$child" || status=$?
child=''
exit "$status"
