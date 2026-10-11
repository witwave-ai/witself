#!/usr/bin/env bash
# Refuse tracing before reading or copying any inherited credential.
if { [[ $- == *x* || ":${SHELLOPTS-}:" == *:xtrace:* ]]; } 2>/dev/null; then
  { set +x; } 2>/dev/null
  printf '%s\n' 'release-chain: FAIL xtrace must be disabled' >&2
  printf '%s\n' 'release-chain: FAIL xtrace must be disabled'
  exit 2
fi
set -euo pipefail

# Unset the destination first so an inherited export attribute cannot survive.
unset CONTROL_PLANE_EDGE_TOKEN WITSELF_FLEET_TOKEN RO_REDACT_EDGE RO_REDACT_CF
unset EDGE_TOKEN FLEET_TOKEN CF_TOKEN RO_CF_ENV CHAIN_CF_TOKEN CHECKED_CF
CHAIN_CF_TOKEN=${CLOUDFLARE_API_TOKEN-}
unset CLOUDFLARE_API_TOKEN CLOUDFLARE_API_TOKEN_OLD
unset CP_DEPLOY_CANONICAL_EMAIL_ACTIVE CP_DEPLOY_SIGNUP_OPEN_ACTIVE
unset CLOUDFLARE_ACCOUNT_ID EMAIL_DIRECTORY_KV_ID AGENT_EMAIL_ROUTE_SIGNING_KEY_ID
unset CP_AGENT_EMAIL_MANAGED_DELIVERY_ACCOUNT_ALLOWLIST
unset DEPLOY_ACCOUNT DEPLOY_KV DEPLOY_KEY DEPLOY_ALLOWLIST
LC_ALL=C
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
RO_NAME=release-chain
# shellcheck source=scripts/lib/release-ops.sh
source "$ROOT/scripts/lib/release-ops.sh"

usage() {
  cat <<'USAGE'
usage: scripts/release-chain.sh VERSION --order cp-first|cells-first --serving-url URL [options]
  --order cp-first|cells-first    Which side of the release goes first (no default)      [required]
  --serving-url URL               Sandbox serving cell origin, as for roll-train.sh       [required]
  --edge-token-file PATH          } forwarded to release-deploy-control-plane.sh          [required]
  --deploy-env-file PATH          }                                                       [required]
  --cloudflare-token-file PATH    } (else CLOUDFLARE_API_TOKEN, read once; see below)
  --attest-canonical-email-active }
  --attest-signup-open-active     }
  --fleet-token-file PATH         } forwarded to release-drill-proof.sh; also used for    [required]
                                  } the production-cell registry check
  --drill-account-id ID           } (as --account-id)                                     [required]
  --drill-cell NAME               }
  --backup-evidence DIR           Forwarded to roll-train.sh (give twice) for a schema release
  --repo OWNER/NAME               Forwarded to release-tag.sh
  --workdir DIR                   Stage logs; forwarded to roll-train.sh and the deploy
  --avoid-window HHMM-HHMM        Backup window no stage may start in (2330-0045)
  --train-clearance-min N         Minutes before the window in which no train starts (150)
  --max-window-wait-min N         Bound on any single window wait (240)
  --execute                       Run the chain; without it, print the plan only
Covers the sandbox pair only; refuses to run while civo-prod-use1-serving is registered.
USAGE
}

VERSION='' ORDER='' SERVING_URL='' EDGE_FILE='' ENV_FILE='' CF_FILE='' FLEET=''
ID='' DRILL=civo-sandbox-use1-backup REPO=witwave-ai/witself WORKDIR=''
WINDOW=2330-0045 TRAIN_CLEARANCE=150 MAX_WINDOW_WAIT=240
EXECUTE=false ATTEST_EMAIL=false ATTEST_SIGNUP=false EVIDENCE_COUNT=0
EVIDENCE_ARGS=()
while [ "$#" -gt 0 ]; do
  case "$1" in
    --help) usage; exit 0 ;;
    --order|--serving-url|--edge-token-file|--deploy-env-file|--cloudflare-token-file|--fleet-token-file|--drill-account-id|--drill-cell|--backup-evidence|--repo|--workdir|--avoid-window|--train-clearance-min|--max-window-wait-min)
      [ "$#" -ge 2 ] && [ -n "$2" ] || ro_usage_error "missing value for $1"
      case "$1" in
        --order) ORDER=$2 ;;
        --serving-url) SERVING_URL=$2 ;;
        --edge-token-file) EDGE_FILE=$2 ;;
        --deploy-env-file) ENV_FILE=$2 ;;
        --cloudflare-token-file) CF_FILE=$2 ;;
        --fleet-token-file) FLEET=$2 ;;
        --drill-account-id) ID=$2 ;;
        --drill-cell) DRILL=$2 ;;
        --backup-evidence)
          [ "$EVIDENCE_COUNT" -lt 2 ] || ro_usage_error 'give --backup-evidence exactly twice'
          [ -d "$2" ] || ro_usage_error 'backup evidence must name an existing directory'
          EVIDENCE_ARGS+=(--backup-evidence "$2"); EVIDENCE_COUNT=$((EVIDENCE_COUNT + 1)) ;;
        --repo) REPO=$2 ;;
        --workdir) WORKDIR=$2 ;;
        --avoid-window) WINDOW=$2 ;;
        --train-clearance-min) TRAIN_CLEARANCE=$2 ;;
        --max-window-wait-min) MAX_WINDOW_WAIT=$2 ;;
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
[[ "$ORDER" = cp-first || "$ORDER" = cells-first ]] || ro_usage_error '--order cp-first or cells-first is required'
# Match the train's DNS/IPv4 origin support before any release stage acts.
[[ "$SERVING_URL" =~ ^https://[A-Za-z0-9]([A-Za-z0-9.-]*[A-Za-z0-9])?(:[0-9]{1,5})?$ ]] || ro_usage_error 'a serving HTTPS hostname or IPv4 origin is required'
[[ "$REPO" =~ ^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$ ]] || ro_usage_error 'invalid repository'
[[ "$ID" =~ ^[A-Za-z0-9_-]{1,128}$ ]] || ro_usage_error 'invalid or missing drill account id'
[[ "$DRILL" =~ ^[a-z0-9-]{1,64}$ ]] || ro_usage_error 'invalid drill cell'
[ -n "$EDGE_FILE" ] && [ -n "$ENV_FILE" ] && [ -n "$FLEET" ] || ro_usage_error 'edge, deploy environment and fleet files are required'
[ "$EVIDENCE_COUNT" -ne 1 ] || ro_usage_error 'give --backup-evidence exactly twice'
[[ "$WINDOW" =~ ^[0-9]{4}-[0-9]{4}$ ]] || ro_usage_error 'invalid backup window'
ro_hhmm_minutes "${WINDOW%-*}" >/dev/null || ro_usage_error 'invalid backup window'
ro_hhmm_minutes "${WINDOW#*-}" >/dev/null || ro_usage_error 'invalid backup window'
[[ "$TRAIN_CLEARANCE" =~ ^[0-9]{1,4}$ && "$MAX_WINDOW_WAIT" =~ ^[0-9]{1,6}$ ]] || ro_usage_error 'invalid window bound'
TRAIN_CLEARANCE=$((10#$TRAIN_CLEARANCE)); MAX_WINDOW_WAIT=$((10#$MAX_WINDOW_WAIT))
(( TRAIN_CLEARANCE < 1440 && MAX_WINDOW_WAIT > 0 )) || ro_usage_error 'invalid window bound'
if [ -n "$WORKDIR" ]; then
  [ -d "$WORKDIR" ] && [ ! -L "$WORKDIR" ] && [ -w "$WORKDIR" ] || ro_usage_error 'workdir must be an existing writable non-symbolic directory'
  WORKDIR=$(cd "$WORKDIR" && pwd -P) || ro_usage_error 'cannot resolve workdir'
  [ "$(ro_file_mode "$WORKDIR")" = 700 ] || ro_usage_error 'workdir must have mode 0700'
fi

ro_require_private_file 'edge token' "$EDGE_FILE" 16 8192
ro_require_private_file 'fleet token' "$FLEET" 16 8192
if [ -n "$CF_FILE" ]; then
  if [ -n "$CHAIN_CF_TOKEN" ]; then
    printf '%s: note: CLOUDFLARE_API_TOKEN from the environment is ignored; using --cloudflare-token-file\n' "$RO_NAME"
  fi
  unset CHAIN_CF_TOKEN
  ro_require_private_file 'Cloudflare token' "$CF_FILE" 1 4096
  ro_read_token "$CF_FILE" CHECKED_CF
else
  CHECKED_CF=$CHAIN_CF_TOKEN
fi
[[ -n "$CHECKED_CF" && ${#CHECKED_CF} -le 4096 && ! "$CHECKED_CF" =~ [[:space:][:cntrl:]] ]] \
  || ro_usage_error 'a Cloudflare token of 1-4096 bytes without whitespace is required'
unset CHECKED_CF

# This validator deliberately does not source the file or retain its values.
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
        seen_account=true ;;
      EMAIL_DIRECTORY_KV_ID)
        [ "$seen_kv" = false ] || ro_usage_error "invalid deploy env file line $line_number"
        seen_kv=true ;;
      AGENT_EMAIL_ROUTE_SIGNING_KEY_ID)
        [ "$seen_key" = false ] || ro_usage_error "invalid deploy env file line $line_number"
        seen_key=true ;;
      CP_AGENT_EMAIL_MANAGED_DELIVERY_ACCOUNT_ALLOWLIST)
        [ "$seen_allowlist" = false ] || ro_usage_error "invalid deploy env file line $line_number"
        seen_allowlist=true ;;
      *) ro_usage_error "invalid deploy env file line $line_number" ;;
    esac
  done < "$ENV_FILE"
  [ "$seen_account:$seen_kv:$seen_key:$seen_allowlist" = true:true:true:true ] \
    || ro_usage_error "invalid deploy env file line $line_number"
}
parse_deploy_env
for tool in git gh jq yq kubectl curl shasum npm node witself-admin; do
  command -v "$tool" >/dev/null 2>&1 || ro_usage_error "required tool is not on PATH: $tool"
done

DEPLOY_ARGS=("$VERSION" --edge-token-file "$EDGE_FILE" --deploy-env-file "$ENV_FILE")
if [ -n "$CF_FILE" ]; then DEPLOY_ARGS+=(--cloudflare-token-file "$CF_FILE"); fi
if [ "$ATTEST_EMAIL" = true ]; then DEPLOY_ARGS+=(--attest-canonical-email-active); fi
if [ "$ATTEST_SIGNUP" = true ]; then DEPLOY_ARGS+=(--attest-signup-open-active); fi
TRAIN_ARGS=("$VERSION" --serving-url "$SERVING_URL")
if [ "$EVIDENCE_COUNT" -gt 0 ]; then TRAIN_ARGS+=("${EVIDENCE_ARGS[@]}"); fi
if [ -n "$WORKDIR" ]; then
  DEPLOY_ARGS+=(--workdir "$WORKDIR")
  TRAIN_ARGS+=(--workdir "$WORKDIR")
fi
DRILL_ARGS=(--account-id "$ID" --fleet-token-file "$FLEET" --drill-cell "$DRILL")

print_command() { printf '%s: plan:' "$RO_NAME"; printf ' %q' "$@"; printf '\n'; }
if [ "$EXECUTE" = false ]; then
  local_tags=$(git -C "$ROOT" for-each-ref --format='%(refname:strip=2)' refs/tags) || ro_die 'cannot read local tags'
  highest=none
  while IFS= read -r tag; do
    case "$tag" in v*) candidate=${tag#v} ;; *) continue ;; esac
    ro_valid_version "$candidate" || continue
    if [ "$highest" = none ] || ro_version_lower "$highest" "$candidate"; then highest=$candidate; fi
  done < <(printf '%s\n' "$local_tags")
  local_main=$(git -C "$ROOT" rev-parse --verify 'origin/main^{commit}') || ro_die 'cannot read local origin/main'
  printf '%s: plan: highest local valid tag=%s; local origin/main=%.8s (not fetched)\n' "$RO_NAME" "$highest" "$local_main"
  if [ "$ORDER" = cp-first ]; then stages='preflight schema tag cp train drill'; else stages='preflight schema tag train cp drill'; fi
  printf '%s: plan: stages %s\n' "$RO_NAME" "$stages"
  printf '%s: plan: fetch main and tags; require HEAD equals origin/main; refuse a registered production cell\n' "$RO_NAME"
  print_command witself-admin cells show civo-prod-use1-serving --token-file "$FLEET" --json
  print_command "$BASH" "$ROOT/scripts/release-schema-check.sh" "$VERSION" --commit '<selected-commit>' --serving-url "$SERVING_URL" --execute
  print_command "$BASH" "$ROOT/scripts/release-tag.sh" "$VERSION" --commit '<selected-commit>' --repo "$REPO" --execute
  for planned_stage in $stages; do
    case "$planned_stage" in
      cp) print_command "$BASH" "$ROOT/scripts/release-deploy-control-plane.sh" "${DEPLOY_ARGS[@]}" --execute ;;
      train)
        if [ "$EVIDENCE_COUNT" -gt 0 ]; then
          print_command "$BASH" "$ROOT/scripts/roll-train.sh" "${TRAIN_ARGS[@]}"
        else
          print_command "$BASH" "$ROOT/scripts/roll-train.sh" "${TRAIN_ARGS[@]}" '<schema-gate>'
        fi ;;
    esac
  done
  print_command "$BASH" "$ROOT/scripts/release-drill-proof.sh" "${DRILL_ARGS[@]}" --execute
  printf '%s: plan: guard cp and drill with 30 minutes clearance, train with %s, window %s; wait at most %s minutes each\n' "$RO_NAME" "$TRAIN_CLEARANCE" "$WINDOW" "$MAX_WINDOW_WAIT"
  unset CHAIN_CF_TOKEN
  printf '%s: DRY-RUN (no action taken; add --execute)\n' "$RO_NAME"
  exit 0
fi

TMP='' CURRENT_STAGE=preflight STAGE_REPORTED=false RO_SUCCESS=false GATE=''
stage_begin() { CURRENT_STAGE=$1; STAGE_REPORTED=false; }
stage_fail() {
  printf '%s: %s\n' "$RO_NAME" "$*" >&2
  printf '%s: STAGE %s FAIL\n' "$RO_NAME" "$CURRENT_STAGE"
  STAGE_REPORTED=true
  ro_die "$CURRENT_STAGE v$VERSION"
}
stage_pass() {
  printf '%s: STAGE %s PASS\n' "$RO_NAME" "$CURRENT_STAGE"
  STAGE_REPORTED=true
}
cleanup() {
  local status=$? cleanup_failed=false
  trap - EXIT INT TERM
  unset CHAIN_CF_TOKEN
  if [ -n "$TMP" ]; then rm -rf "$TMP" || cleanup_failed=true; fi
  if [ "$status" -eq 0 ] && [ "$RO_SUCCESS" = true ] && [ "$cleanup_failed" = false ]; then
    printf '%s\n' 'next: brew upgrade witself witself-admin witself-infra'
    printf '%s: PASS v%s (order %s; train gate %s; drill validated)\n' "$RO_NAME" "$VERSION" "$ORDER" "$GATE"
  else
    if [ "$cleanup_failed" = true ]; then
      printf '%s: temporary directory cleanup failed\n' "$RO_NAME" >&2
      RO_FAILURE_REPORTED=false
    fi
    if [ "${RO_FAILURE_REPORTED-false}" != true ]; then
      if [ "$STAGE_REPORTED" = false ]; then printf '%s: STAGE %s FAIL\n' "$RO_NAME" "$CURRENT_STAGE"; fi
      printf '%s: FAIL %s v%s\n' "$RO_NAME" "$CURRENT_STAGE" "$VERSION" >&2
      printf '%s: FAIL %s v%s\n' "$RO_NAME" "$CURRENT_STAGE" "$VERSION"
    fi
    status=1
  fi
  exit "$status"
}
trap cleanup EXIT
trap 'stage_fail "interrupted"' INT TERM
TMP=$(mktemp -d "${TMPDIR:-/tmp}/witself-release.XXXXXX") || stage_fail 'cannot create private temporary directory'
chmod 700 "$TMP" || stage_fail 'cannot secure temporary directory'
if [ -z "$WORKDIR" ]; then WORKDIR=$TMP; fi

# Log only the child's output; verdicts additionally require that the logger and
# the child both succeeded and that the final record is the child's PASS line.
run_child() {
  local child=$1 expected=$2 line='' output_line='' status
  shift 2
  [ ! -L "$WORKDIR/$CURRENT_STAGE.log" ] || stage_fail 'stage log must not be a symbolic link'
  set +e
  (
    if [ "$CURRENT_STAGE" = cp ] && [ -z "$CF_FILE" ]; then
      export CLOUDFLARE_API_TOKEN=$CHAIN_CF_TOKEN
    fi
    exec "$BASH" "$ROOT/scripts/$child.sh" "$@"
  ) 2>&1 | tee "$WORKDIR/$CURRENT_STAGE.log"
  status=("${PIPESTATUS[@]}")
  set -e
  [ "${status[0]}" -eq 0 ] || stage_fail "$child exited ${status[0]}"
  [ "${status[1]}" -eq 0 ] || stage_fail 'stage log write failed'
  while IFS= read -r output_line || [ -n "$output_line" ]; do line=$output_line; done < "$WORKDIR/$CURRENT_STAGE.log"
  [[ "$line" == "$expected"* ]] || stage_fail "$child did not end with its PASS line"
  LAST_LINE=$line
}

git -C "$ROOT" fetch -q origin main --tags 2>/dev/null || stage_fail 'cannot fetch main and tags'
head=$(git -C "$ROOT" rev-parse --verify HEAD) || stage_fail 'cannot read HEAD'
main=$(git -C "$ROOT" rev-parse --verify 'origin/main^{commit}') || stage_fail 'cannot read origin/main'
[ "$head" = "$main" ] || stage_fail 'run the chain from an up-to-date main checkout'
if SHA=$(git -C "$ROOT" rev-parse --verify "refs/tags/v$VERSION^{commit}" 2>/dev/null); then :; else SHA=$main; fi
set +e
witself-admin cells show civo-prod-use1-serving --token-file "$FLEET" --json > "$TMP/registry.out" 2> "$TMP/registry.err"
registry_status=$?
set -e
if [ "$registry_status" -eq 0 ]; then
  stage_fail 'civo-prod-use1-serving is registered; this chain covers only the sandbox pair; roll it as docs/runbooks.md "Two-wave roll (automated)" describes for the production pair'
fi
# Preserve all trailing bytes rather than allowing command substitution to trim
# blank lines: only the CLI's exact diagnostic, with one LF, is accepted.
registry_error=$(cat "$TMP/registry.err"; printf '.')
if [ "$registry_status" -ne 1 ] || [ "$registry_error" != $'witself-admin cells: cell is not registered with the control plane\n.' ]; then
  stage_fail 'registry read failed'
fi
stage_pass

stage_begin schema
run_child release-schema-check 'release-schema-check: PASS ' "$VERSION" --commit "$SHA" --serving-url "$SERVING_URL" --execute
schema_live='' schema_tag='' schema_gate_count=0
for field in $LAST_LINE; do
  case "$field" in
    gate=*) GATE=${field#gate=}; schema_gate_count=$((schema_gate_count + 1)) ;;
    live=*) schema_live=${field#live=} ;;
    tag=*) schema_tag=${field#tag=} ;;
  esac
done
[[ "$schema_gate_count" = 1 && ( "$GATE" = --no-schema-change || "$GATE" = --backup-evidence ) ]] || stage_fail 'schema check did not report a valid train gate'
[[ "$schema_live" =~ ^[0-9]+$ && "$schema_tag" =~ ^[0-9]+$ ]] || stage_fail 'schema check did not report schema versions'
if [ "$GATE" = --backup-evidence ] && [ "$EVIDENCE_COUNT" -eq 0 ]; then
  stage_fail "schema advances from $schema_live to $schema_tag; follow \"Back up both reviewed Civo databases before a migration\" and pass --backup-evidence twice"
fi
if [ "$EVIDENCE_COUNT" -eq 0 ]; then TRAIN_ARGS+=(--no-schema-change); else GATE=--backup-evidence; fi
stage_pass

stage_begin tag
run_child release-tag 'release-tag: PASS ' "$VERSION" --commit "$SHA" --repo "$REPO" --execute
stage_pass

run_cp() {
  stage_begin cp
  (ro_wait_outside_window "$WINDOW" 30 "$MAX_WINDOW_WAIT") || stage_fail 'backup window guard failed'
  run_child release-deploy-control-plane 'release-deploy-control-plane: PASS ' "${DEPLOY_ARGS[@]}" --execute
  unset CHAIN_CF_TOKEN
  stage_pass
}
run_train() {
  local reported
  stage_begin train
  (ro_wait_outside_window "$WINDOW" "$TRAIN_CLEARANCE" "$MAX_WINDOW_WAIT") || stage_fail 'backup window guard failed'
  run_child roll-train "roll-train: Both waves verified at $VERSION." "${TRAIN_ARGS[@]}"
  reported=$(curl -fsS -m 20 "$SERVING_URL/v1/version" 2>/dev/null | jq -r '.version' 2>/dev/null) || stage_fail 'cannot read the serving version'
  [ "$reported" = "$VERSION" ] || stage_fail 'serving version does not match the release'
  stage_pass
}
if [ "$ORDER" = cp-first ]; then run_cp; run_train; else run_train; run_cp; fi
stage_begin drill
(ro_wait_outside_window "$WINDOW" 30 "$MAX_WINDOW_WAIT") || stage_fail 'backup window guard failed'
run_child release-drill-proof 'release-drill-proof: PASS ' "${DRILL_ARGS[@]}" --execute
stage_pass
RO_SUCCESS=true
