#!/usr/bin/env bash
# Refuse tracing before root discovery or any credential handling.
if { [[ $- == *x* || ":${SHELLOPTS-}:" == *:xtrace:* ]]; } 2>/dev/null; then
  { set +x; } 2>/dev/null
  printf '%s\n' 'release-merge-pr: FAIL xtrace must be disabled' >&2
  printf '%s\n' 'release-merge-pr: FAIL xtrace must be disabled'
  exit 2
fi
set -euo pipefail
unset CONTROL_PLANE_EDGE_TOKEN WITSELF_FLEET_TOKEN CLOUDFLARE_API_TOKEN CLOUDFLARE_API_TOKEN_OLD
unset RO_REDACT_EDGE RO_REDACT_CF EDGE_TOKEN CF_TOKEN RO_CF_ENV
LC_ALL=C
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
RO_NAME=release-merge-pr
# shellcheck source=scripts/lib/release-ops.sh
source "$ROOT/scripts/lib/release-ops.sh"

usage() {
  cat <<'USAGE'
usage: scripts/release-merge-pr.sh PR --expect-head SHA [options]
  --expect-head SHA      40-hex commit you reviewed; the PR head must equal it
  --repo OWNER/NAME      GitHub repository (default witwave-ai/witself)
  --ci-timeout-min N     Bound for PR checks and for post-merge CI (90)
  --execute              Merge; without it, print the plan only
USAGE
}

PR='' EXPECT_HEAD='' REPO=witwave-ai/witself CI_TIMEOUT_MIN=90 EXECUTE=false
while [ "$#" -gt 0 ]; do
  case "$1" in
    --help) usage; exit 0 ;;
    --expect-head|--repo|--ci-timeout-min)
      [ "$#" -ge 2 ] && [ -n "$2" ] || ro_usage_error "missing value for $1"
      case "$1" in
        --expect-head) EXPECT_HEAD=$2 ;;
        --repo) REPO=$2 ;;
        --ci-timeout-min) CI_TIMEOUT_MIN=$2 ;;
      esac
      shift 2 ;;
    --execute) EXECUTE=true; shift ;;
    -*) ro_usage_error 'unknown option' ;;
    *) [ -z "$PR" ] || ro_usage_error 'unexpected argument'; PR=$1; shift ;;
  esac
done
[[ "$PR" =~ ^[1-9][0-9]*$ ]] || ro_usage_error 'PR must be a positive number'
[[ "$EXPECT_HEAD" =~ ^[0-9a-f]{40}$ ]] || ro_usage_error '--expect-head must be a 40-hex commit'
[[ "$REPO" =~ ^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$ ]] || ro_usage_error 'repository must be OWNER/NAME'
[[ "$CI_TIMEOUT_MIN" =~ ^[0-9]{1,6}$ ]] || ro_usage_error 'CI timeout must be a positive number of minutes'
CI_TIMEOUT_MIN=$((10#$CI_TIMEOUT_MIN))
(( CI_TIMEOUT_MIN > 0 )) || ro_usage_error 'CI timeout must be positive'

if [ "$EXECUTE" != true ]; then
  printf '%s: PR #%s repository=%s expected-head=%s\n' "$RO_NAME" "$PR" "$REPO" "$EXPECT_HEAD"
  printf '%s\n' '1. Wait for mergeability to settle.' '2. Wait for every PR check to pass or skip.' \
    '3. Verify the PR head is the reviewed commit.' '4. Squash merge with the reviewed head fence and delete the branch.' \
    '5. Read the squash merge commit.' '6. Wait for successful main CI on the merge commit or a descendant.'
  printf '%s: DRY-RUN (no action taken; add --execute)\n' "$RO_NAME"
  exit 0
fi

TMP='' RO_SUCCESS=false RO_FAILURE_REPORTED=false SUCCESS_SUMMARY=''
# shellcheck disable=SC2329
cleanup() {
  local status=$?
  trap - EXIT INT TERM
  if [ -n "$TMP" ] && ! rm -rf "$TMP"; then
    printf '%s: FAIL temporary directory cleanup failed\n' "$RO_NAME" >&2
    printf '%s: FAIL temporary directory cleanup failed\n' "$RO_NAME"
    status=1
  elif [ "$status" -eq 0 ] && [ "$RO_SUCCESS" = true ]; then
    printf '%s: PASS %s\n' "$RO_NAME" "$SUCCESS_SUMMARY"
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

MERGEABILITY_RECHECKS=6 MERGEABILITY_POLL_SECONDS=20
CHECK_POLL_SECONDS=60 MAX_EMPTY_CHECK_POLLS=15 MAX_CHECK_READ_FAILURES=3
MERGE_COMMIT_RECHECKS=6 MERGE_COMMIT_POLL_SECONDS=10
for (( poll=0; poll <= MERGEABILITY_RECHECKS; poll++ )); do
  response=$(gh pr view "$PR" --repo "$REPO" --json mergeable 2>/dev/null) || ro_die 'cannot read PR mergeability'
  mergeable=$(printf '%s' "$response" | jq -er '.mergeable | select(type == "string")' 2>/dev/null) \
    || ro_die 'cannot read PR mergeability'
  case "$mergeable" in
    MERGEABLE) break ;;
    CONFLICTING) ro_die 'PR conflicts with main' ;;
    UNKNOWN) ;;
    *) ro_die 'cannot read PR mergeability' ;;
  esac
  (( poll < MERGEABILITY_RECHECKS )) || ro_die 'PR mergeability remained unknown'
  sleep "$MERGEABILITY_POLL_SECONDS" || ro_die 'mergeability wait failed'
done

empty_polls=0 read_failures=0 checks_ready=false
for (( poll=0; poll <= CI_TIMEOUT_MIN; poll++ )); do
  rc=0
  checks=$(gh pr checks "$PR" --repo "$REPO" --json name,bucket,state,link 2>"$TMP/checks.err") || rc=$?
  if [ "$rc" -eq 1 ] && grep -Eq "^no checks reported on the '[^']+' branch$" "$TMP/checks.err"; then
    # Match only the documented empty-list response, not mixed diagnostics.
    error_lines=$(wc -l < "$TMP/checks.err")
    [ "$error_lines" -eq 1 ] && checks='[]' && rc=0
  fi
  # gh can use exit 1 for failed checks. Such JSON may explain a refusal,
  # but may never supply a green verdict or reset the read-failure counter.
  if [ "$rc" -eq 1 ] && printf '%s' "$checks" | jq -e 'type == "array" and any(.[]; .bucket == "fail" or .bucket == "cancel")' >/dev/null 2>&1; then
    printf '%s' "$checks" | jq -r '.[] | select(.bucket == "fail" or .bucket == "cancel") | [.name, .bucket] | @tsv' \
      | sed -E 's/[0-9a-fA-F]{64}/<hex64>/g'
    ro_die 'PR checks failed or were cancelled'
  fi
  valid=false
  if [ "$rc" -eq 0 ] || [ "$rc" -eq 8 ]; then
    if printf '%s' "$checks" | jq -e 'type == "array" and all(.[]; type == "object" and (.name | type == "string") and (.bucket == "pass" or .bucket == "skipping" or .bucket == "pending" or .bucket == "fail" or .bucket == "cancel"))' >/dev/null 2>&1; then
      valid=true
    fi
  fi
  if [ "$valid" != true ]; then
    read_failures=$((read_failures + 1))
    (( read_failures < MAX_CHECK_READ_FAILURES )) || ro_die 'PR checks read failed three times in a row'
  else
    read_failures=0
    count=$(printf '%s' "$checks" | jq -r 'length')
    if [ "$count" -eq 0 ]; then
      empty_polls=$((empty_polls + 1))
      (( empty_polls < MAX_EMPTY_CHECK_POLLS )) || ro_die 'no PR checks after 15 polls'
    else
      empty_polls=0
      if printf '%s' "$checks" | jq -e 'any(.[]; .bucket == "fail" or .bucket == "cancel")' >/dev/null; then
        printf '%s' "$checks" | jq -r '.[] | select(.bucket == "fail" or .bucket == "cancel") | [.name, .bucket] | @tsv' \
          | sed -E 's/[0-9a-fA-F]{64}/<hex64>/g'
        ro_die 'PR checks failed or were cancelled'
      fi
      if printf '%s' "$checks" | jq -e 'all(.[]; .bucket == "pass" or .bucket == "skipping")' >/dev/null; then
        checks_ready=true
        break
      fi
    fi
  fi
  (( poll < CI_TIMEOUT_MIN )) || break
  sleep "$CHECK_POLL_SECONDS" || ro_die 'PR checks wait failed'
done
[ "$checks_ready" = true ] || ro_die 'PR checks exceeded the CI timeout'

head=$(gh pr view "$PR" --repo "$REPO" --json headRefOid -q .headRefOid 2>/dev/null) || ro_die 'cannot read PR head'
[[ "$head" =~ ^[0-9a-f]{40}$ ]] || ro_die 'cannot read PR head'
[ "$head" = "$EXPECT_HEAD" ] || ro_die "PR head ${head:0:8} is not the reviewed head ${EXPECT_HEAD:0:8}"
gh pr merge "$PR" --repo "$REPO" --squash --match-head-commit "$EXPECT_HEAD" --delete-branch >/dev/null 2>&1 \
  || ro_die 'PR merge failed'

MERGE=''
for (( poll=0; poll <= MERGE_COMMIT_RECHECKS; poll++ )); do
  MERGE=$(gh pr view "$PR" --repo "$REPO" --json mergeCommit -q .mergeCommit.oid 2>/dev/null) || MERGE=''
  [[ "$MERGE" =~ ^[0-9a-f]{40}$ ]] && break
  (( poll < MERGE_COMMIT_RECHECKS )) || ro_die 'cannot read the merge commit'
  sleep "$MERGE_COMMIT_POLL_SECONDS" || ro_die 'merge commit wait failed'
done

for (( poll=0; poll <= CI_TIMEOUT_MIN; poll++ )); do
  git -C "$ROOT" fetch -q origin main 2>/dev/null || ro_die 'cannot fetch post-merge main'
  runs=$(gh run list --repo "$REPO" --workflow ci.yml --branch main --limit 15 --json databaseId,headSha,status,conclusion 2>/dev/null) \
    || ro_die 'cannot read post-merge CI runs'
  printf '%s' "$runs" | jq -e 'type == "array" and all(.[]; type == "object" and (.headSha | type == "string") and (.status | type == "string"))' >/dev/null 2>&1 \
    || ro_die 'cannot read post-merge CI runs'
  printf '%s' "$runs" | jq -r '.[] | select(.status == "completed") | [.databaseId, .headSha, (.conclusion // "unknown")] | @tsv' > "$TMP/runs"
  while IFS=$'\t' read -r run_id run_head conclusion; do
    [[ "$run_id" =~ ^[0-9]+$ && "$run_head" =~ ^[0-9a-f]{40}$ ]] || ro_die 'invalid post-merge CI run'
    if [ "$conclusion" = success ] && git -C "$ROOT" merge-base --is-ancestor "$MERGE" "$run_head" 2>/dev/null; then
      SUCCESS_SUMMARY="#$PR merged as ${MERGE:0:8}; post-merge CI run $run_id success"
      RO_SUCCESS=true
      exit 0
    fi
    if [ "$run_head" = "$MERGE" ] && [ "$conclusion" != success ] && [ "$conclusion" != cancelled ]; then
      ro_die 'post-merge CI failed on the merge commit'
    fi
  done < "$TMP/runs"
  (( poll < CI_TIMEOUT_MIN )) || break
  sleep "$CHECK_POLL_SECONDS" || ro_die 'post-merge CI wait failed'
done
ro_die 'post-merge CI exceeded the CI timeout'
