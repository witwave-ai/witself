#!/usr/bin/env bash
# Refuse tracing before touching inherited credentials or starting any child.
if { [[ $- == *x* || ":${SHELLOPTS-}:" == *:xtrace:* ]]; } 2>/dev/null; then
  { set +x; } 2>/dev/null
  printf '%s\n' 'release-schema-check: FAIL xtrace must be disabled' >&2
  printf '%s\n' 'release-schema-check: FAIL xtrace must be disabled'
  exit 2
fi
set -euo pipefail

unset CONTROL_PLANE_EDGE_TOKEN WITSELF_FLEET_TOKEN CLOUDFLARE_API_TOKEN CLOUDFLARE_API_TOKEN_OLD
unset RO_REDACT_EDGE RO_REDACT_CF EDGE_TOKEN CF_TOKEN FLEET_TOKEN RO_CF_ENV
ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)
RO_NAME=release-schema-check
# shellcheck source=scripts/lib/release-ops.sh
source "$ROOT/scripts/lib/release-ops.sh"

usage() {
  cat <<'USAGE'
usage: scripts/release-schema-check.sh VERSION --serving-url URL [options]
  --serving-url URL   Serving cell origin whose /v1/version is read                 [required]
  --commit SHA        Commit to check while tag vVERSION does not exist (default: origin/main)
  --execute           Fetch and read the live schema; without it, print the plan only
The last line names the train gate: gate=--no-schema-change or gate=--backup-evidence.
USAGE
}

VERSION='' SERVING_URL='' COMMIT='' EXECUTE=false
while [ "$#" -gt 0 ]; do
  case "$1" in
    --help) usage; exit 0 ;;
    --serving-url|--commit)
      [ "$#" -ge 2 ] && [ -n "$2" ] || ro_usage_error "missing value for $1"
      case "$1" in --serving-url) SERVING_URL=$2 ;; --commit) COMMIT=$2 ;; esac
      shift 2 ;;
    --execute) EXECUTE=true; shift ;;
    -*) ro_usage_error 'unknown option' ;;
    *) [ -z "$VERSION" ] || ro_usage_error 'unexpected argument'; VERSION=$1; shift ;;
  esac
done
ro_valid_version "$VERSION" || ro_usage_error 'VERSION must be a decimal major.minor.patch version'
[[ "$SERVING_URL" =~ ^https://([A-Za-z0-9]([A-Za-z0-9.-]*[A-Za-z0-9])?|\[[0-9A-Fa-f:]+\])(:[0-9]{1,5})?$ ]] \
  || ro_usage_error 'invalid serving origin'
[[ -z "$COMMIT" || "$COMMIT" =~ ^[0-9a-fA-F]{40}$ ]] || ro_usage_error '--commit must be a 40-hex commit'

if [ "$EXECUTE" = false ]; then
  printf '%s: plan: fetch main and tags; resolve v%s or %s; compare migrations with the previous tag; read %s/v1/version; select the train gate\n' \
    "$RO_NAME" "$VERSION" "${COMMIT:-origin/main}" "$SERVING_URL"
  printf '%s: DRY-RUN (no action taken; add --execute)\n' "$RO_NAME"
  exit 0
fi

TMP='' RO_SUCCESS=false RO_SUMMARY=''
# shellcheck disable=SC2329
cleanup() {
  local status=$? cleanup_failed=false
  trap - EXIT INT TERM
  if [ -n "$TMP" ]; then rm -rf -- "$TMP" 2>/dev/null || cleanup_failed=true; fi
  if [ "$cleanup_failed" = true ]; then
    printf '%s: FAIL temporary directory cleanup failed\n' "$RO_NAME" >&2
    printf '%s: FAIL temporary directory cleanup failed\n' "$RO_NAME"
    status=1
  elif [ "$status" -eq 0 ] && [ "$RO_SUCCESS" = true ]; then
    printf '%s: PASS %s\n' "$RO_NAME" "$RO_SUMMARY"
  elif [ "${RO_FAILURE_REPORTED-false}" != true ]; then
    printf '%s: FAIL unexpected command failure\n' "$RO_NAME" >&2
    printf '%s: FAIL unexpected command failure\n' "$RO_NAME"
    status=1
  fi
  exit "$status"
}
trap cleanup EXIT
trap 'ro_die "interrupted"' INT TERM
TMP=$(mktemp -d "${TMPDIR:-/tmp}/witself-release.XXXXXX") || ro_die 'cannot create private temporary directory'
chmod 700 "$TMP" || ro_die 'cannot secure temporary directory'

git -C "$ROOT" fetch -q origin main --tags 2>/dev/null || ro_die 'cannot fetch main and release tags'
if TAGSHA=$(git -C "$ROOT" rev-parse --verify "refs/tags/v$VERSION^{commit}" 2>/dev/null); then
  SHA=$TAGSHA
  if [ -n "$COMMIT" ]; then
    requested=$(git -C "$ROOT" rev-parse --verify "$COMMIT^{commit}" 2>/dev/null) || ro_die 'cannot resolve requested commit'
    [ "$requested" = "$SHA" ] || ro_die 'existing release tag differs from --commit'
  fi
else
  SHA=$(git -C "$ROOT" rev-parse --verify "${COMMIT:-origin/main}^{commit}" 2>/dev/null) || ro_die 'cannot resolve release commit'
fi
git -C "$ROOT" merge-base --is-ancestor "$SHA" origin/main 2>/dev/null || ro_die 'release commit is not on origin/main'
prev=$(git -C "$ROOT" describe --tags --abbrev=0 --match 'v[0-9]*' --exclude "v$VERSION" "$SHA" 2>/dev/null) \
  || ro_die 'cannot find previous release tag'
if ! ro_valid_version "${prev#v}" || ! ro_version_lower "${prev#v}" "$VERSION"; then
  ro_die 'previous release tag must be valid and lower than VERSION'
fi
diff=$(git -C "$ROOT" diff --name-only "$prev" "$SHA" -- internal/store/migrations 2>/dev/null) || ro_die 'cannot compare release migrations'
changed=false
if [ -n "$diff" ]; then changed=true; fi
migrations=$(git -C "$ROOT" ls-tree --name-only "$SHA" internal/store/migrations/ 2>/dev/null) || ro_die 'cannot read release migrations'
tagmax=0
while IFS= read -r path; do
  base=${path##*/}
  [[ "$base" =~ ^[0-9]+_.*\.sql$ ]] || continue
  number=${base%%_*}
  # Strip redundant zeros before bounding arithmetic to avoid integer overflow.
  while [ "${#number}" -gt 1 ] && [ "${number#0}" != "$number" ]; do number=${number#0}; done
  [ "${#number}" -le 18 ] || ro_die 'migration number is outside the supported range'
  number=$((10#$number))
  if (( number > tagmax )); then tagmax=$number; fi
done <<< "$migrations"
(( tagmax >= 1 )) || ro_die 'release commit has no numbered SQL migration'
live=$(curl -fsS -m 20 "$SERVING_URL/v1/version" 2>/dev/null | jq -r '.store_schema_version' 2>/dev/null) \
  || ro_die 'cannot read live store schema version'
[[ "$live" =~ ^[0-9]+$ ]] || ro_die 'cannot read live store schema version'
while [ "${#live}" -gt 1 ] && [ "${live#0}" != "$live" ]; do live=${live#0}; done
[ "${#live}" -le 18 ] || ro_die 'live store schema version is outside the supported range'
live=$((10#$live))
if (( live > tagmax )); then ro_die "live schema $live is ahead of v$VERSION's $tagmax"; fi
gate=--no-schema-change
if [ "$changed" = true ] || (( live < tagmax )); then gate=--backup-evidence; fi
RO_SUMMARY="gate=$gate previous=$prev changed=$changed live=$live tag=$tagmax commit=${SHA:0:8}"
RO_SUCCESS=true
