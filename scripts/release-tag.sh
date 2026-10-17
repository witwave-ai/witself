#!/usr/bin/env bash
# Refuse tracing before touching inherited credentials or starting any child.
if { [[ $- == *x* || ":${SHELLOPTS-}:" == *:xtrace:* ]]; } 2>/dev/null; then
  { set +x; } 2>/dev/null
  printf '%s\n' 'release-tag: FAIL xtrace must be disabled' >&2
  printf '%s\n' 'release-tag: FAIL xtrace must be disabled'
  exit 2
fi
set -euo pipefail

unset CONTROL_PLANE_EDGE_TOKEN WITSELF_FLEET_TOKEN CLOUDFLARE_API_TOKEN CLOUDFLARE_API_TOKEN_OLD
unset RO_REDACT_EDGE RO_REDACT_CF EDGE_TOKEN CF_TOKEN FLEET_TOKEN RO_CF_ENV
ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)
RO_NAME='release-tag'
# shellcheck source=scripts/lib/release-ops.sh
source "$ROOT/scripts/lib/release-ops.sh"

usage() {
  cat <<'USAGE'
usage: scripts/release-tag.sh VERSION [options]
  --commit SHA             Commit to tag; must be on origin/main. Default: the commit of an
                           existing tag vVERSION, else origin/main after fetch
  --repo OWNER/NAME        GitHub repository (default witwave-ai/witself)
  --ci-timeout-min N       Wait for ci.yml on the commit (90)
  --release-timeout-min N  Wait for release.yml on the tag (120)
  --execute                Tag and push; without it, print the plan only
USAGE
}

VERSION='' COMMIT='' REPO=witwave-ai/witself CI_TIMEOUT=90 RELEASE_TIMEOUT=120 EXECUTE=false
RELEASE_DISCOVERY_POLLS=12 RELEASE_DISCOVERY_SLEEP=30
while [ "$#" -gt 0 ]; do
  case "$1" in
    --help) usage; exit 0 ;;
    --commit|--repo|--ci-timeout-min|--release-timeout-min)
      [ "$#" -ge 2 ] && [ -n "$2" ] || ro_usage_error "missing value for $1"
      case "$1" in
        --commit) COMMIT=$2 ;; --repo) REPO=$2 ;;
        --ci-timeout-min) CI_TIMEOUT=$2 ;; --release-timeout-min) RELEASE_TIMEOUT=$2 ;;
      esac
      shift 2 ;;
    --execute) EXECUTE=true; shift ;;
    -*) ro_usage_error 'unknown option' ;;
    *) [ -z "$VERSION" ] || ro_usage_error 'unexpected argument'; VERSION=$1; shift ;;
  esac
done
ro_valid_version "$VERSION" || ro_usage_error 'VERSION must be a decimal major.minor.patch version'
[[ -z "$COMMIT" || "$COMMIT" =~ ^[0-9a-fA-F]{40}$ ]] || ro_usage_error '--commit must be a 40-hex commit'
[[ "$REPO" =~ ^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$ && "$REPO" != -* ]] || ro_usage_error 'invalid repository'
[[ "$CI_TIMEOUT" =~ ^[0-9]{1,6}$ && "$RELEASE_TIMEOUT" =~ ^[0-9]{1,6}$ ]] || ro_usage_error 'timeouts must be positive decimal minutes'
CI_TIMEOUT=$((10#$CI_TIMEOUT)); RELEASE_TIMEOUT=$((10#$RELEASE_TIMEOUT))
(( CI_TIMEOUT > 0 && RELEASE_TIMEOUT > 0 )) || ro_usage_error 'timeouts must be positive decimal minutes'

highest_local_tag() {
  local tags tag highest=''
  tags=$(git -C "$ROOT" for-each-ref --format='%(refname:strip=2)' refs/tags 2>/dev/null) || ro_die 'cannot read local tags'
  while IFS= read -r tag; do
    case "$tag" in v*) ;; *) continue ;; esac
    ro_valid_version "${tag#v}" || continue
    if [ -z "$highest" ] || ro_version_lower "$highest" "${tag#v}"; then highest=${tag#v}; fi
  done <<< "$tags"
  printf '%s\n' "$highest"
}

if [ "$EXECUTE" = false ]; then
  highest=$(highest_local_tag) || ro_die 'cannot read local tags'
  printf '%s: highest local valid tag: %s\n' "$RO_NAME" "${highest:+v}${highest:-none}"
  printf '%s: plan: fetch main and tags; resolve v%s or %s; wait for exact-commit CI; create and push an annotated tag if absent; verify its push release and published assets in %s\n' \
    "$RO_NAME" "$VERSION" "${COMMIT:-origin/main}" "$REPO"
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
EXISTING=false
if TAGSHA=$(git -C "$ROOT" rev-parse --verify "refs/tags/v$VERSION^{commit}" 2>/dev/null); then
  EXISTING=true; SHA=$TAGSHA
  if [ -n "$COMMIT" ]; then
    requested=$(git -C "$ROOT" rev-parse --verify "$COMMIT^{commit}" 2>/dev/null) || ro_die 'cannot resolve requested commit'
    [ "$requested" = "$SHA" ] || ro_die 'existing release tag differs from --commit'
  fi
else
  SHA=$(git -C "$ROOT" rev-parse --verify "${COMMIT:-origin/main}^{commit}" 2>/dev/null) || ro_die 'cannot resolve release commit'
fi
git -C "$ROOT" merge-base --is-ancestor "$SHA" origin/main 2>/dev/null || ro_die 'release commit is not on origin/main'
if [ "$EXISTING" = true ]; then
  printf '%s: tag v%s already at %s; not re-tagging\n' "$RO_NAME" "$VERSION" "${SHA:0:8}"
else
  highest=$(highest_local_tag) || ro_die 'cannot read local tags'
  if [ -n "$highest" ] && ! ro_version_lower "$highest" "$VERSION"; then
    ro_die 'VERSION must be strictly greater than the highest valid tag'
  fi
  ci_ready=false
  for (( attempt=0; attempt<CI_TIMEOUT; attempt++ )); do
    runs=$(gh run list --repo "$REPO" --workflow ci.yml --branch main --limit 20 --json databaseId,headSha,status,conclusion 2>/dev/null) \
      || ro_die 'cannot read CI runs'
    run=$(printf '%s\n' "$runs" | jq -ce --arg sha "$SHA" 'if type != "array" then error("runs") else map(select(.headSha == $sha)) | first // {} end' 2>/dev/null) \
      || ro_die 'cannot read CI runs'
    status=$(printf '%s\n' "$run" | jq -er '.status // "missing" | select(type == "string")' 2>/dev/null) || ro_die 'cannot read CI status'
    conclusion=$(printf '%s\n' "$run" | jq -er '.conclusion // "none" | select(type == "string")' 2>/dev/null) || ro_die 'cannot read CI conclusion'
    if [ "$status" = completed ]; then
      if [ "$conclusion" = success ]; then ci_ready=true; break; fi
      if [ "$conclusion" = cancelled ]; then
        id=$(printf '%s\n' "$run" | jq -r '.databaseId' 2>/dev/null) || ro_die 'cannot read CI run identifier'
        [[ "$id" =~ ^[0-9]+$ ]] || ro_die 'cannot read CI run identifier'
        ro_die "CI on ${SHA:0:8} was cancelled (run $id); re-run it with: gh run rerun $id --repo $REPO, then run release-tag.sh again"
      fi
      [[ "$conclusion" =~ ^[a-z_]+$ ]] || ro_die 'cannot read CI conclusion'
      ro_die "CI on ${SHA:0:8} is '$status $conclusion'"
    fi
    if (( attempt + 1 < CI_TIMEOUT )); then sleep 60 || ro_die 'CI sleep failed'; fi
  done
  [ "$ci_ready" = true ] || ro_die 'CI wait exceeded its bound'
  git -C "$ROOT" tag -a "v$VERSION" -m "v$VERSION" "$SHA" 2>/dev/null || ro_die 'cannot create annotated release tag'
  git -C "$ROOT" push origin "refs/tags/v$VERSION" 2>/dev/null || ro_die 'cannot push release tag'
  printf '%s: TAGGED v%s at %s\n' "$RO_NAME" "$VERSION" "${SHA:0:8}"
fi

id=''
for (( attempt=0; attempt<RELEASE_DISCOVERY_POLLS; attempt++ )); do
  runs=$(gh run list --repo "$REPO" --workflow release.yml --limit 10 --json databaseId,headBranch,event,status,conclusion 2>/dev/null) \
    || ro_die 'cannot read release runs'
  id=$(printf '%s\n' "$runs" | jq -r --arg tag "v$VERSION" 'if type != "array" then error("runs") else map(select(.headBranch == $tag and .event == "push")) | first | .databaseId // empty end' 2>/dev/null) \
    || ro_die 'cannot read release runs'
  if [ -n "$id" ]; then [[ "$id" =~ ^[0-9]+$ ]] || ro_die 'cannot read release run identifier'; break; fi
  if (( attempt + 1 < RELEASE_DISCOVERY_POLLS )); then sleep "$RELEASE_DISCOVERY_SLEEP" || ro_die 'release discovery sleep failed'; fi
done
[ -n "$id" ] || ro_die 'no push release run appeared'
release_ready=false
for (( attempt=0; attempt<RELEASE_TIMEOUT; attempt++ )); do
  run=$(gh run view "$id" --repo "$REPO" --json status,conclusion 2>/dev/null) || ro_die 'cannot read release run'
  status=$(printf '%s\n' "$run" | jq -er '.status | select(type == "string")' 2>/dev/null) || ro_die 'cannot read release status'
  conclusion=$(printf '%s\n' "$run" | jq -er '.conclusion // "none" | select(type == "string")' 2>/dev/null) || ro_die 'cannot read release conclusion'
  if [ "$status" = completed ]; then
    if [ "$conclusion" = success ]; then release_ready=true; fi
    break
  fi
  if (( attempt + 1 < RELEASE_TIMEOUT )); then sleep 60 || ro_die 'release wait sleep failed'; fi
done
if [ "$release_ready" != true ]; then
  jobs=$(gh run view "$id" --repo "$REPO" --json jobs 2>/dev/null) || ro_die 'cannot read failed release jobs'
  printf '%s\n' "$jobs" | jq -r '.jobs[] | select(.conclusion != "success") | [.name, .conclusion] | @tsv' 2>/dev/null \
    | sed -E 's/[0-9a-fA-F]{64}/<hex64>/g' || ro_die 'cannot read failed release jobs'
  ro_die "release run $id did not complete successfully"
fi
release=$(gh release view "v$VERSION" --repo "$REPO" --json isDraft,isPrerelease,assets 2>/dev/null) || ro_die 'cannot read published release'
assets=$(printf '%s\n' "$release" | jq -er 'select(.isDraft == false and .isPrerelease == false and (.assets | type == "array")) | .assets | length' 2>/dev/null) \
  || ro_die 'release must be published and neither a draft nor a prerelease'
RO_SUMMARY="v$VERSION at ${SHA:0:8}; release run $id success; assets=$assets"
RO_SUCCESS=true
