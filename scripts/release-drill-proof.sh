#!/usr/bin/env bash
# Refuse tracing before any credential can be read or copied from the environment.
if { [[ $- == *x* || ":${SHELLOPTS-}:" == *:xtrace:* ]]; } 2>/dev/null; then
  { set +x; } 2>/dev/null
  printf '%s\n' 'release-drill-proof: FAIL xtrace must be disabled' >&2
  printf '%s\n' 'release-drill-proof: FAIL xtrace must be disabled'
  exit 2
fi
set -euo pipefail

# Strip inherited credentials before starting even a local helper process.
unset CONTROL_PLANE_EDGE_TOKEN WITSELF_FLEET_TOKEN CLOUDFLARE_API_TOKEN CLOUDFLARE_API_TOKEN_OLD
unset RO_REDACT_EDGE RO_REDACT_CF FLEET_TOKEN
ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
RO_NAME=release-drill-proof
# shellcheck source=scripts/lib/release-ops.sh
source "$ROOT/scripts/lib/release-ops.sh"

usage() {
  cat <<'USAGE'
usage: scripts/release-drill-proof.sh --account-id ID --fleet-token-file PATH [options]
  --account-id ID         Account whose committed backup is drilled                    [required]
  --fleet-token-file PATH Fleet token (0600; 16-8192 bytes)                            [required]
  --drill-cell NAME       Rollback-only drill cell (default civo-sandbox-use1-backup)
  --backup-id ID          Backup to drill (default: newest catalog entry not failed)
  --control-plane URL     Control-plane origin (default https://self.witwave.ai)
  --max-wait-min N        Poll bound for a running drill (95)
  --execute               Start the drill; without it, print the plan only
USAGE
}

ID='' FLEET='' BID='' TMP=''
DRILL=civo-sandbox-use1-backup
CP=https://self.witwave.ai
MAX_WAIT_MIN=95
EXECUTE=false
while [ "$#" -gt 0 ]; do
  case "$1" in
    --help) usage; exit 0 ;;
    --account-id|--fleet-token-file|--drill-cell|--backup-id|--control-plane|--max-wait-min)
      [ "$#" -ge 2 ] || ro_usage_error "missing option value"
      case "$1" in
        --account-id) ID=$2 ;;
        --fleet-token-file) FLEET=$2 ;;
        --drill-cell) DRILL=$2 ;;
        --backup-id) BID=$2 ;;
        --control-plane) CP=$2 ;;
        --max-wait-min) MAX_WAIT_MIN=$2 ;;
      esac
      shift 2 ;;
    --execute) EXECUTE=true; shift ;;
    *) ro_usage_error "unknown argument" ;;
  esac
done
[[ "$ID" =~ ^[A-Za-z0-9_-]{1,128}$ ]] || ro_usage_error "invalid or missing account id"
[[ "$DRILL" =~ ^[a-z0-9-]{1,64}$ ]] || ro_usage_error "invalid drill cell"
[[ -z "$BID" || "$BID" =~ ^backup_[0-9]{8}T[0-9]{6}Z$ ]] || ro_usage_error "invalid backup id"
# DNS names, IPv4 and bracketed IPv6 origins; no credentials, path or query.
[[ "$CP" =~ ^https://([A-Za-z0-9]([A-Za-z0-9.-]*[A-Za-z0-9])?|\[[0-9A-Fa-f:]+\])(:[0-9]{1,5})?$ ]] || ro_usage_error "invalid control-plane origin"
[[ "$MAX_WAIT_MIN" =~ ^[0-9]{1,6}$ ]] || ro_usage_error "invalid maximum wait"
MAX_WAIT_MIN=$((10#$MAX_WAIT_MIN))
[ "$MAX_WAIT_MIN" -gt 0 ] || ro_usage_error "invalid maximum wait"
[ -n "$FLEET" ] || ro_usage_error "missing fleet token file"
ro_require_private_file "fleet token" "$FLEET" 16 8192
ro_read_token "$FLEET" FLEET_TOKEN
# shellcheck disable=SC2034
RO_REDACT_EDGE=$FLEET_TOKEN
# shellcheck disable=SC2034
RO_REDACT_CF=

# Responses can contain arbitrary diagnostic text. Keep redaction values in this
# shell and strip public digests only after the builtin token substitutions.
safe_print() {
  printf '%s\n' "$@" | ro_redact_stream | sed -E 's/[0-9a-fA-F]{64}/<hex64>/g' || ro_die "output redaction failed"
}

if [ "$EXECUTE" = false ]; then
  safe_print "Plan: drill account $ID on $DRILL via $CP; backup ${BID:-newest non-failed catalog entry}; poll at most $MAX_WAIT_MIN minutes; require drill DB rows 0."
  printf '%s: DRY-RUN (no action taken; add --execute)\n' "$RO_NAME"
  unset FLEET_TOKEN RO_REDACT_EDGE RO_REDACT_CF
  exit 0
fi

RO_SUCCESS=false
RO_SUMMARY=''
cleanup() {
  local status=$? cleanup_failed=false
  trap - EXIT INT TERM
  if [ -n "$TMP" ]; then rm -rf -- "$TMP" 2>/dev/null || cleanup_failed=true; fi
  unset FLEET_TOKEN RO_REDACT_EDGE RO_REDACT_CF
  if [ "$cleanup_failed" = true ]; then
    printf '%s: FAIL temporary directory cleanup failed\n' "$RO_NAME" >&2
    printf '%s: FAIL temporary directory cleanup failed\n' "$RO_NAME"
    status=1
  elif [ "$status" -eq 0 ] && [ "$RO_SUCCESS" = true ]; then
    printf '%s\n' "$RO_SUMMARY"
  elif [ "${RO_FAILURE_REPORTED-false}" != true ]; then
    printf '%s: FAIL unexpected command failure\n' "$RO_NAME" >&2
    printf '%s: FAIL unexpected command failure\n' "$RO_NAME"
    status=1
  fi
  exit "$status"
}
trap cleanup EXIT
trap 'ro_die "interrupted"' INT TERM
TMP=$(mktemp -d "${TMPDIR:-/tmp}/witself-release.XXXXXX") || ro_die "cannot create private temporary directory"
chmod 700 "$TMP" || ro_die "cannot protect temporary directory"
( umask 077; printf 'Authorization: Bearer %s\n' "$FLEET_TOKEN" > "$TMP/auth.hdr" ) || ro_die "cannot write authorization header"

auth() { curl -s --fail-with-body -m 120 -H @"$TMP/auth.hdr" "$@"; }
record() { auth "$CP/v1/backups/status?account_id=$ID" | jq -c '.account.restore_drill // {}'; }
json_field() { jq -r "$1" 2>/dev/null; }
server_state() {
  kubectl --context "witself-$DRILL" --request-timeout=30s -n witself get pod -l app.kubernetes.io/component=server \
    -o jsonpath='{range .items[*]}{.metadata.name}:{.status.containerStatuses[0].restartCount}:{.status.containerStatuses[0].lastState.terminated.reason}{" "}{end}'
}
print_record() {
  local projected
  projected=$(printf '%s\n' "$RECORD" | jq -c '{drill_id, driver, state, started_at, deadline_at, finished_at, validated_at, error}' 2>/dev/null) || ro_die "cannot read drill record"
  safe_print "record: $projected"
}
read_record() {
  RECORD=$(record 2>/dev/null) || ro_die "cannot read drill record"
  local observed
  observed=$(printf '%s\n' "$RECORD" | json_field '.drill_id // empty') || ro_die "cannot read drill record"
  [ "$observed" = "$DRILL_ID" ] || ro_die "drill record belongs to another drill"
  STATE=$(printf '%s\n' "$RECORD" | json_field '.state // empty') || ro_die "cannot read drill state"
}

VERSION_BODY=$(curl -fsS -m 20 "$CP/v1/version" 2>/dev/null) || ro_die "cannot read control-plane version"
VERSION_LINE=$(printf '%s\n' "$VERSION_BODY" | jq -r '"\(.version // "unknown") \((.commit // "")[0:8])"' 2>/dev/null) || ro_die "cannot read control-plane version"
safe_print "== CP version: $VERSION_LINE"
STATUS=$(auth "$CP/v1/backups/status?account_id=$ID" 2>/dev/null) || ro_die "cannot read backup status"
printf '%s\n' "$STATUS" | jq -e '.account | type == "object" and has("restore_drill")' >/dev/null 2>&1 || ro_die "control plane predates the drill record"
RECORD=$(printf '%s\n' "$STATUS" | jq -c '.account.restore_drill // {}' 2>/dev/null) || ro_die "cannot read drill record"
if printf '%s\n' "$RECORD" | jq -e '.state == "running"' >/dev/null 2>&1; then
  print_record
  ro_die "a drill is already running for the account"
fi
if [ -z "$BID" ]; then
  BID=$(printf '%s\n' "$STATUS" | json_field '[.account.backups.catalog[]? | select(.status != "failed")] | sort_by(.backup_id) | last | .backup_id // empty') || ro_die "cannot read backup catalog"
fi
[[ "$BID" =~ ^backup_[0-9]{8}T[0-9]{6}Z$ ]] || ro_die "no backup is available to drill"
ENTRY=$(printf '%s\n' "$STATUS" | jq -ce --arg bid "$BID" '.account.backups.catalog[]? | select(.backup_id == $bid) | {backup_id, size, chunks, entries, archive_schema_version, committed_at}' 2>/dev/null) || ro_die "backup is absent from the catalog"
safe_print "$ENTRY"
SERVER_BEFORE=$(server_state 2>/dev/null) || ro_die "cannot read drill server state"

post_drill() {
  # The body contains only prevalidated identifiers; the bearer is exclusively in
  # the private header file, never in curl's arguments or environment.
  CURL_EXIT=0
  HTTP=$(curl -s -m 1800 -D "$TMP/hdr" -o "$TMP/out" -w '%{http_code}' -X POST -H @"$TMP/auth.hdr" \
    -H 'Content-Type: application/json' \
    --data "{\"account_id\":\"$ID\",\"backup_id\":\"$BID\",\"target_cell\":\"$DRILL\",\"wait\":false}" \
    "$CP/v1/backups:restore-drill" 2>/dev/null) || CURL_EXIT=$?
  HEADER_ID='' HEADER_DRIVER=''
  local line value compact sanitized
  if [ -f "$TMP/hdr" ]; then
    while IFS= read -r line || [ -n "$line" ]; do
      value=${line#*:}; value=${value# }; value=${value%$'\r'}
      case "$line" in
        [Xx]-[Ww][Ii][Tt][Ss][Ee][Ll][Ff]-[Rr][Ee][Ss][Tt][Oo][Rr][Ee]-[Dd][Rr][Ii][Ll][Ll]-[Ii][Dd]:*) HEADER_ID=$value ;;
        [Xx]-[Ww][Ii][Tt][Ss][Ee][Ll][Ff]-[Rr][Ee][Ss][Tt][Oo][Rr][Ee]-[Dd][Rr][Ii][Ll][Ll]-[Dd][Rr][Ii][Vv][Ee][Rr]:*) HEADER_DRIVER=$value ;;
      esac
    done < "$TMP/hdr"
  fi
  safe_print "http=$HTTP curl_exit=$CURL_EXIT drill_id=${HEADER_ID:-none} driver=${HEADER_DRIVER:-none}"
  compact=$(jq -c . "$TMP/out" 2>/dev/null) || compact='unreadable response body'
  # Redact before truncation so a token crossing byte 300 cannot leak a prefix.
  sanitized=$(safe_print "$compact") || ro_die "cannot redact drill response"
  LC_ALL=C printf '%.300s\n' "$sanitized"
}

for ((POST_ATTEMPT=0; POST_ATTEMPT<2; POST_ATTEMPT++)); do
  post_drill
  if [ "$HTTP" = 409 ] && jq -e '.error | strings | test("backup validation protocol 2")' "$TMP/out" >/dev/null 2>&1; then
    [ "$POST_ATTEMPT" -eq 0 ] || ro_die "drill cell refused protocol 2 twice; use the connected procedure in docs/runbooks.md"
    sleep 60 || ro_die "drill wait failed"
  else
    break
  fi
done

HOW=
DRILL_ID=$HEADER_ID
if jq -e '.validated == true' "$TMP/out" >/dev/null 2>&1; then
  HOW=answer
  if [ -z "$DRILL_ID" ]; then
    DRILL_ID=$(jq -r '.drill_id // .restore_drill.drill_id // "connected"' "$TMP/out" 2>/dev/null) || ro_die "cannot read drill answer"
  fi
elif [ "$HTTP" = 202 ]; then
  if [ -z "$DRILL_ID" ]; then
    DRILL_ID=$(jq -r '.restore_drill.drill_id // empty' "$TMP/out" 2>/dev/null) || ro_die "cannot read drill id"
  fi
  [ -n "$DRILL_ID" ] || ro_die "drill not recorded"
elif [ "$HTTP" = 502 ]; then
  [ -n "$DRILL_ID" ] || ro_die "drill not recorded"
  sleep 60 || ro_die "drill wait failed"
else
  safe_print "unexpected drill response: http=$HTTP"
  ro_die "drill not started (http ${HTTP//[^0-9]/})"
fi

if [ "$HOW" != answer ]; then
  for ((POLL=0; POLL<=MAX_WAIT_MIN; POLL++)); do
    read_record
    [ "$STATE" = running ] || break
    [ "$POLL" -lt "$MAX_WAIT_MIN" ] || break
    if [ "$POLL" -gt 0 ] && [ "$((POLL % 10))" -eq 0 ]; then
      ro_log "drill running after $POLL minutes"
    fi
    sleep 60 || ro_die "drill wait failed"
  done
  if printf '%s\n' "$RECORD" | jq -e '.state == "failed" and .finished_at == null and .driver == "alarm"' >/dev/null 2>&1; then
    sleep 120 || ro_die "drill wait failed"
    read_record
  fi
  print_record
  if [ "$STATE" = validated ]; then
    HOW=record
  else
    STATUS=$(auth "$CP/v1/backups/status?account_id=$ID" 2>/dev/null) || ro_die "cannot read backup validation"
    # Verify the fence again on the catalog read. ISO UTC values are compared as
    # strings; both must be present so null or empty dates cannot prove success.
    OBSERVED=$(printf '%s\n' "$STATUS" | json_field '.account.restore_drill.drill_id // empty') || ro_die "cannot read drill record"
    [ "$OBSERVED" = "$DRILL_ID" ] || ro_die "drill record belongs to another drill"
    if printf '%s\n' "$STATUS" | jq -e --arg bid "$BID" --arg drill "$DRILL" '
      .account as $account | $account.restore_drill.started_at as $started |
      ($started | type == "string" and endswith("Z")) and
      any($account.backups.catalog[]? | select(.backup_id == $bid) | .validations[]?;
        .target_cell == $drill and (.validated_at | type == "string" and endswith("Z")) and .validated_at >= $started)
    ' >/dev/null 2>&1; then
      HOW=catalog
    else
      safe_print "drill outcome state: $STATE"
      case "$STATE" in running|failed|pending|cancelled|'') ;; *) STATE=unknown ;; esac
      ro_die "drill not validated (state $STATE)"
    fi
  fi
fi

SERVER_AFTER=$(server_state 2>/dev/null) || ro_die "cannot read drill server state"
if [ "$SERVER_AFTER" != "$SERVER_BEFORE" ]; then
  safe_print "WARN server restarted during drill: $SERVER_AFTER"
fi
# Keep SQL substitution in the pod shell, where the validated id is positional.
# shellcheck disable=SC2016
PQ='pf="${POSTGRES_PASSWORD_FILE:-${POSTGRESQL_PASSWORD_FILE:-/opt/bitnami/postgresql/secrets/password}}"; PGPASSWORD="$(cat "$pf")" psql --no-password --host=127.0.0.1 --username="${POSTGRES_USER:-${POSTGRESQL_USERNAME:-witself}}" --dbname="${POSTGRES_DB:-${POSTGRESQL_DATABASE:-witself}}" -tAc "select count(*) from accounts where id = '"'"'$1'"'"'"'
ROWS=$(kubectl --context "witself-$DRILL" --request-timeout=30s -n witself exec witself-postgresql-0 -c postgresql -- sh -c "$PQ" sh "$ID" 2>/dev/null) || ro_die "cannot read drill DB rows"
safe_print "drill DB rows for the account (must be 0): $ROWS"
[ "$ROWS" = 0 ] || ro_die "drill DB rows are not 0"
# Probe gauges are informational; an unavailable metrics endpoint cannot erase
# the independently verified drill result and empty database.
{ curl -s -m 30 "$CP/metrics/probes" 2>/dev/null | grep -E '^witself_control_plane_account_backup_' | head -8 | ro_redact_stream | sed -E 's/[0-9a-fA-F]{64}/<hex64>/g'; } || true
RO_SUMMARY=$(safe_print "$RO_NAME: PASS drill $DRILL_ID validated ($HOW) on $DRILL for $BID; drill DB rows 0") || ro_die "cannot redact drill verdict"
RO_SUCCESS=true
