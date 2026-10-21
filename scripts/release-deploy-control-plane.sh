#!/usr/bin/env bash
# Refuse tracing before any credential can be read or copied from the environment.
if { [[ $- == *x* || ":${SHELLOPTS-}:" == *:xtrace:* ]]; } 2>/dev/null; then
  { set +x; } 2>/dev/null
  printf '%s\n' 'release-deploy-control-plane: FAIL xtrace must be disabled' >&2
  printf '%s\n' 'release-deploy-control-plane: FAIL xtrace must be disabled'
  exit 2
fi
set -euo pipefail

# Remove inherited credentials before the first child, including root discovery.
unset CONTROL_PLANE_EDGE_TOKEN WITSELF_FLEET_TOKEN RO_REDACT_EDGE RO_REDACT_CF
unset EDGE_TOKEN CF_TOKEN RO_CF_ENV
RO_CF_ENV=${CLOUDFLARE_API_TOKEN-}
unset CLOUDFLARE_API_TOKEN CLOUDFLARE_API_TOKEN_OLD
unset CP_DEPLOY_CANONICAL_EMAIL_ACTIVE CP_DEPLOY_SIGNUP_OPEN_ACTIVE
unset CLOUDFLARE_ACCOUNT_ID EMAIL_DIRECTORY_KV_ID AGENT_EMAIL_ROUTE_SIGNING_KEY_ID
unset CP_AGENT_EMAIL_MANAGED_DELIVERY_ACCOUNT_ALLOWLIST
unset DEPLOY_ACCOUNT DEPLOY_KV DEPLOY_KEY DEPLOY_ALLOWLIST
LC_ALL=C
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
RO_NAME=release-deploy-control-plane
# shellcheck source=scripts/lib/release-ops.sh
source "$ROOT/scripts/lib/release-ops.sh"

usage() {
  cat <<'USAGE'
usage: scripts/release-deploy-control-plane.sh VERSION [options]
  --edge-token-file PATH          Control-plane edge token (0600; 16-8192 bytes)        [required]
  --deploy-env-file PATH          The four deploy identifiers (0600; see below)          [required]
  --cloudflare-token-file PATH    Cloudflare API token (0600; 1-4096 bytes). Wins over
                                  CLOUDFLARE_API_TOKEN; without it, that variable is used
  --attest-canonical-email-active Pass CP_DEPLOY_CANONICAL_EMAIL_ACTIVE=true (reviewed live state)
  --attest-signup-open-active     Pass CP_DEPLOY_SIGNUP_OPEN_ACTIVE=true (reviewed live state)
  --workdir DIR                   Parent of the temporary tag worktree (default: the private temp dir)
  --execute                       Deploy; without it, print the plan only
The control plane is always https://self.witwave.ai; the deploy cannot target another origin.
VERSION may equal the live version (a re-deploy) but never be lower: rollback is not supported.
USAGE
}

VERSION='' EDGE_FILE='' ENV_FILE='' CF_FILE='' WORKDIR=''
EXECUTE=false ATTEST_EMAIL=false ATTEST_SIGNUP=false
while [ "$#" -gt 0 ]; do
  case "$1" in
    --help) usage; exit 0 ;;
    --edge-token-file|--deploy-env-file|--cloudflare-token-file|--workdir)
      [ "$#" -ge 2 ] && [ -n "$2" ] || ro_usage_error "missing value for $1"
      case "$1" in
        --edge-token-file) EDGE_FILE=$2 ;;
        --deploy-env-file) ENV_FILE=$2 ;;
        --cloudflare-token-file) CF_FILE=$2 ;;
        --workdir) WORKDIR=$2 ;;
      esac
      shift 2 ;;
    --attest-canonical-email-active) ATTEST_EMAIL=true; shift ;;
    --attest-signup-open-active) ATTEST_SIGNUP=true; shift ;;
    --execute) EXECUTE=true; shift ;;
    -*) ro_usage_error 'unknown option' ;;
    *) [ -z "$VERSION" ] || ro_usage_error 'unexpected argument'; VERSION=$1; shift ;;
  esac
done
ro_valid_version "$VERSION" || ro_usage_error 'VERSION must be a decimal major.minor.patch version'
[ -n "$EDGE_FILE" ] && [ -n "$ENV_FILE" ] || ro_usage_error 'edge and deploy environment files are required'
if [ -n "$WORKDIR" ]; then
  [ -d "$WORKDIR" ] && [ -w "$WORKDIR" ] || ro_usage_error 'workdir must be an existing writable directory'
  WORKDIR=$(cd "$WORKDIR" && pwd -P) || ro_usage_error 'cannot resolve workdir'
fi

ro_require_private_file 'edge token' "$EDGE_FILE" 16 8192
ro_read_token "$EDGE_FILE" EDGE_TOKEN
CF_TOKEN=''
if [ -n "$CF_FILE" ]; then
  if [ -n "$RO_CF_ENV" ]; then
    printf '%s: note: CLOUDFLARE_API_TOKEN from the environment is ignored; using --cloudflare-token-file\n' "$RO_NAME"
  fi
  ro_require_private_file 'Cloudflare token' "$CF_FILE" 1 4096
  ro_read_token "$CF_FILE" CF_TOKEN
else
  CF_TOKEN=$RO_CF_ENV
fi
unset RO_CF_ENV
[[ -n "$CF_TOKEN" && ${#CF_TOKEN} -le 4096 && ! "$CF_TOKEN" =~ [[:space:][:cntrl:]] ]] \
  || ro_usage_error 'a Cloudflare token of 1-4096 bytes without whitespace is required'
RO_REDACT_EDGE=$EDGE_TOKEN
RO_REDACT_CF=$CF_TOKEN

parse_deploy_env() {
  local line='' name value line_number=0 mode nul='' LC_ALL=C
  local seen_account=false seen_kv=false seen_key=false seen_allowlist=false
  ro_canonical_file_path "$ENV_FILE" >/dev/null || ro_usage_error 'deploy env file path must be canonical and absolute'
  [ -f "$ENV_FILE" ] && [ ! -L "$ENV_FILE" ] || ro_usage_error 'deploy env file must be a regular non-symbolic file'
  mode=$(ro_file_mode "$ENV_FILE")
  [ "$mode" = 600 ] || ro_usage_error 'deploy env file must have mode 0600'
  ro_require_private_parent "$ENV_FILE"
  if IFS= read -r -d '' nul < "$ENV_FILE"; then
    line_number=1
    while [[ "$nul" == *$'\n'* ]]; do
      line_number=$((line_number + 1)); nul=${nul#*$'\n'}
    done
    ro_usage_error "invalid deploy env file line $line_number"
  fi
  while IFS= read -r line || [ -n "$line" ]; do
    line_number=$((line_number + 1))
    case "$line" in ''|'#'*) continue ;; esac
    case "$line" in *=*) name=${line%%=*}; value=${line#*=} ;; *) ro_usage_error "invalid deploy env file line $line_number" ;; esac
    [ -n "$value" ] && [ "${#value}" -le 4096 ] || ro_usage_error "invalid deploy env file line $line_number"
    [[ ! "$value" =~ [[:space:]] ]] || ro_usage_error "invalid deploy env file line $line_number"
    case "$value" in *"'"*|*'"'*|*'$'*|*'`'*|*\\*|*';'*) ro_usage_error "invalid deploy env file line $line_number" ;; esac
    case "$name" in
      CLOUDFLARE_ACCOUNT_ID)
        [ "$seen_account" = false ] || ro_usage_error "invalid deploy env file line $line_number"
        DEPLOY_ACCOUNT=$value; seen_account=true ;;
      EMAIL_DIRECTORY_KV_ID)
        [ "$seen_kv" = false ] || ro_usage_error "invalid deploy env file line $line_number"
        DEPLOY_KV=$value; seen_kv=true ;;
      AGENT_EMAIL_ROUTE_SIGNING_KEY_ID)
        [ "$seen_key" = false ] || ro_usage_error "invalid deploy env file line $line_number"
        DEPLOY_KEY=$value; seen_key=true ;;
      CP_AGENT_EMAIL_MANAGED_DELIVERY_ACCOUNT_ALLOWLIST)
        [ "$seen_allowlist" = false ] || ro_usage_error "invalid deploy env file line $line_number"
        DEPLOY_ALLOWLIST=$value; seen_allowlist=true ;;
      *) ro_usage_error "invalid deploy env file line $line_number" ;;
    esac
  done < "$ENV_FILE"
  [ "$seen_account:$seen_kv:$seen_key:$seen_allowlist" = true:true:true:true ] \
    || ro_usage_error "invalid deploy env file line $line_number"
}
parse_deploy_env

if [ "$EXECUTE" = false ]; then
  printf '%s: plan: verify the canonical live version; fetch v%s; create a clean tag worktree; npm ci; deploy; verify version\n' "$RO_NAME" "$VERSION"
  printf '%s: plan: attest canonical email=%s signup open=%s\n' "$RO_NAME" "$ATTEST_EMAIL" "$ATTEST_SIGNUP"
  unset EDGE_TOKEN CF_TOKEN RO_REDACT_EDGE RO_REDACT_CF
  printf '%s: DRY-RUN (no action taken; add --execute)\n' "$RO_NAME"
  exit 0
fi

TMP='' WT='' RO_SUCCESS=false
# shellcheck disable=SC2329
cleanup() {
  local status=$? cleanup_failed=false
  trap - EXIT INT TERM
  if [ -n "$WT" ]; then
    git -C "$ROOT" worktree remove --force "$WT" >/dev/null 2>&1 || cleanup_failed=true
  fi
  if [ -n "$TMP" ]; then rm -rf "$TMP" || cleanup_failed=true; fi
  unset EDGE_TOKEN CF_TOKEN RO_REDACT_EDGE RO_REDACT_CF
  if [ "$cleanup_failed" = true ]; then
    printf '%s: FAIL temporary worktree cleanup failed\n' "$RO_NAME" >&2
    printf '%s: FAIL temporary worktree cleanup failed\n' "$RO_NAME"
    status=1
  elif [ "$status" -eq 0 ] && [ "$RO_SUCCESS" = true ]; then
    printf '%s: PASS control plane at %s commit=%s\n' "$RO_NAME" "$VERSION" "$COMMIT_SHORT"
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

live=$(curl -fsS -m 20 https://self.witwave.ai/v1/version 2>/dev/null | jq -r '.version // empty' 2>/dev/null) \
  || ro_die 'cannot read the live control-plane version'
ro_valid_version "$live" || ro_die 'cannot read the live control-plane version'
if ro_version_lower "$VERSION" "$live"; then
  ro_die "refusing to deploy $VERSION below the live $live; to roll back, follow docs/runbook-control-plane-rollback.md"
fi
git -C "$ROOT" fetch -q origin --tags 2>/dev/null || ro_die 'cannot fetch release tags'
git -C "$ROOT" rev-parse --verify "refs/tags/v$VERSION^{commit}" >/dev/null 2>&1 || ro_die 'release tag does not exist'
if [ -n "$WORKDIR" ]; then target=$WORKDIR/${TMP##*/}; else target=$TMP/worktree; fi
[ ! -e "$target" ] && [ ! -L "$target" ] || ro_die 'temporary worktree destination already exists'
(umask 022; git -C "$ROOT" worktree add -q --detach "$target" "v$VERSION") 2>/dev/null || ro_die 'cannot create tag worktree'
WT=$target
dirty=$(git -C "$WT" status --porcelain) || ro_die 'cannot inspect tag worktree'
[ -z "$dirty" ] || ro_die 'tag worktree is not clean'
tags=$(git -C "$WT" tag --points-at HEAD --list 'v*') || ro_die 'cannot inspect tag worktree tags'
[ "$tags" = "v$VERSION" ] || ro_die 'tag worktree must have exactly one release tag'

set +e
(cd "$WT/infra/cloudflare/control-plane" && umask 022 && npm ci --silent) 2>&1 | ro_redact_stream | sed -E 's/[0-9a-fA-F]{64}/<hex64>/g'
ci_st=("${PIPESTATUS[@]}")
set -e
[ "${ci_st[1]}" -eq 0 ] || ro_die "redaction pipeline exited ${ci_st[1]}"
[ "${ci_st[2]}" -eq 0 ] || ro_die "redaction pipeline exited ${ci_st[2]}"
[ "${ci_st[0]}" -eq 0 ] || ro_die "npm ci exited ${ci_st[0]}"

set +e
(
  cd "$WT/infra/cloudflare/control-plane" || exit 1
  umask 022
  export CLOUDFLARE_ACCOUNT_ID=$DEPLOY_ACCOUNT EMAIL_DIRECTORY_KV_ID=$DEPLOY_KV
  export AGENT_EMAIL_ROUTE_SIGNING_KEY_ID=$DEPLOY_KEY
  export CP_AGENT_EMAIL_MANAGED_DELIVERY_ACCOUNT_ALLOWLIST=$DEPLOY_ALLOWLIST
  export CLOUDFLARE_API_TOKEN=$CF_TOKEN CONTROL_PLANE_EDGE_TOKEN=$EDGE_TOKEN
  if [ "$ATTEST_EMAIL" = true ]; then export CP_DEPLOY_CANONICAL_EMAIL_ACTIVE=true; fi
  if [ "$ATTEST_SIGNUP" = true ]; then export CP_DEPLOY_SIGNUP_OPEN_ACTIVE=true; fi
  export PATH="$PWD/node_modules/.bin:$PATH"
  exec npm run deploy
) 2>&1 | ro_redact_stream | sed -E 's/[0-9a-fA-F]{64}/<hex64>/g'
st=("${PIPESTATUS[@]}")
set -e
[ "${st[1]}" -eq 0 ] || ro_die "redaction pipeline exited ${st[1]}"
[ "${st[2]}" -eq 0 ] || ro_die "redaction pipeline exited ${st[2]}"
[ "${st[0]}" -eq 0 ] || ro_die "npm run deploy exited ${st[0]}"

DEPLOY_VERSION_POLLS=10
DEPLOY_VERSION_POLL_SECONDS=15
reported=unavailable
COMMIT_SHORT=unknown
for (( poll=0; poll < DEPLOY_VERSION_POLLS; poll++ )); do
  response=$(curl -fsS -m 20 https://self.witwave.ai/v1/version 2>/dev/null) || response='{}'
  reported=$(printf '%s' "$response" | jq -r '.version // empty' 2>/dev/null) || reported=''
  if [ "$reported" = "$VERSION" ]; then
    commit=$(printf '%s' "$response" | jq -r '.commit // empty' 2>/dev/null) || commit=''
    if [[ "$commit" =~ ^[0-9a-fA-F]{8,40}$ ]]; then COMMIT_SHORT=${commit:0:8}; fi
    RO_SUCCESS=true
    exit 0
  fi
  if (( poll + 1 < DEPLOY_VERSION_POLLS )); then sleep "$DEPLOY_VERSION_POLL_SECONDS" || ro_die 'version poll sleep failed'; fi
done
ro_valid_version "$reported" || reported=unavailable
ro_die "control plane reports $reported"
