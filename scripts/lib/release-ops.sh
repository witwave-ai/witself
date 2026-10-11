# shellcheck shell=bash

ro_log() { printf '%s: %s %s\n' "$RO_NAME" "$(date -u +%H:%M:%SZ)" "$*"; }
ro_die() {
  # Consumed by callers' EXIT traps.
  # shellcheck disable=SC2034
  RO_FAILURE_REPORTED=true
  printf '%s: FAIL %s\n' "$RO_NAME" "$*" >&2
  printf '%s: FAIL %s\n' "$RO_NAME" "$*"
  exit 1
}
ro_usage_error() { usage >&2; printf '%s: %s\n' "$RO_NAME" "$*" >&2; exit 2; }

ro_valid_version() { [[ "$1" =~ ^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]]; }
ro_version_lower() {
  local left=$1 right=$2 a b component
  ro_valid_version "$left" && ro_valid_version "$right" || return 1
  for (( component=0; component<3; component++ )); do
    a=${left%%.*}; b=${right%%.*}
    if [ "${#a}" -ne "${#b}" ]; then [ "${#a}" -lt "${#b}" ]; return; fi
    if [ "$a" != "$b" ]; then [[ "$a" < "$b" ]]; return; fi
    left=${left#*.}; right=${right#*.}
  done
  return 1
}

ro_file_mode() {
  local mode
  mode="$(stat -f '%Lp' "$1" 2>/dev/null || true)"
  if [[ ! "$mode" =~ ^[0-7]{3,4}$ ]]; then
    mode="$(stat -c '%a' "$1" 2>/dev/null || true)"
  fi
  printf '%s\n' "$mode"
}

ro_canonical_file_path() {
  local path=$1 parent base resolved_parent
  case "$path" in /*) ;; *) return 1 ;; esac
  parent=${path%/*}; base=${path##*/}
  [ -n "$parent" ] || parent=/
  [ -n "$base" ] && [ "$base" != . ] && [ "$base" != .. ] || return 1
  [ -d "$parent" ] && [ ! -L "$parent" ] || return 1
  resolved_parent="$(cd "$parent" && pwd -P)" || return 1
  [ "$resolved_parent" != / ] || resolved_parent=''
  [ "$path" = "$resolved_parent/$base" ] || return 1
  printf '%s\n' "$path"
}

ro_path_in_repo_worktree() {
  local path=$1 common worktree entry metadata bare configured
  # The dry-run contract forbids even `git worktree list`. Read Git's linked
  # worktree metadata instead; no registration or filesystem mutation is needed.
  common=$(git -C "$ROOT" rev-parse --git-common-dir) || ro_die 'cannot locate repository metadata'
  case "$common" in /*) ;; *) common=$ROOT/$common ;; esac
  common=$(cd "$common" && pwd -P) || ro_die 'cannot resolve repository metadata'
  case "$path" in "$ROOT"|"$ROOT"/*) return 0 ;; esac
  bare=$(git -C "$ROOT" config --get core.bare) || bare=false
  if [ "$bare" != true ]; then
    configured=$(git -C "$ROOT" config --get core.worktree) || configured=''
    if [ -n "$configured" ]; then
      case "$configured" in /*) worktree=$configured ;; *) worktree=$common/$configured ;; esac
    else
      [ "${common##*/}" = .git ] || ro_die 'cannot verify primary worktree location'
      worktree=${common%/*}
    fi
    worktree=$(cd "$worktree" && pwd -P) || ro_die 'cannot resolve primary worktree location'
    case "$path" in "$worktree"|"$worktree"/*) return 0 ;; esac
  fi
  for entry in "$common"/worktrees/*/gitdir; do
    [ -f "$entry" ] || continue
    metadata=''
    IFS= read -r metadata < "$entry" || [ -n "$metadata" ] || ro_die 'cannot read linked worktree metadata'
    case "$metadata" in /*/.git) worktree=${metadata%/.git} ;; *) ro_die 'cannot verify linked worktree location' ;; esac
    if [ -d "$worktree" ]; then
      worktree=$(cd "$worktree" && pwd -P) || ro_die 'cannot resolve linked worktree location'
    fi
    case "$path" in "$worktree"|"$worktree"/*) return 0 ;; esac
  done
  return 1
}

ro_require_private_parent() {
  local path=$1 parent mode
  parent=${path%/*}; [ -n "$parent" ] || parent=/
  mode=$(ro_file_mode "$parent")
  [[ "$mode" =~ ^[0-7]{3,4}$ ]] || ro_die 'could not verify private parent permissions'
  (( (8#$mode & 8#077) == 0 )) || ro_die 'private parent must not be accessible by group or other users'
  if ro_path_in_repo_worktree "$path"; then ro_die 'private artifacts must be outside every Git worktree'; fi
}

ro_read_token() {
  local ro_token_read='' ro_token_extra='' ro_token_nul=''
  [[ "$2" =~ ^[A-Za-z_][A-Za-z0-9_]*$ ]] || ro_die 'invalid token destination'
  # Bash drops NUL bytes from ordinary reads. Detect one explicitly first.
  # Only read's status matters; its private buffer must never be echoed.
  # shellcheck disable=SC2034
  if IFS= read -r -d '' ro_token_nul < "$1"; then ro_die 'token file contains a NUL'; fi
  {
    IFS= read -r ro_token_read || :
    if IFS= read -r ro_token_extra || [ -n "$ro_token_extra" ]; then
      ro_die 'token file must contain one line'
    fi
  } < "$1"
  case "$ro_token_read" in *$'\r'*) ro_die 'token file contains a CR' ;; esac
  printf -v "$2" '%s' "$ro_token_read"
}

ro_require_private_file() {
  local label=$1 path=$2 minimum=$3 maximum=$4 mode size checked_token=''
  local LC_ALL=C
  ro_canonical_file_path "$path" >/dev/null || ro_die "$label path must be canonical and absolute"
  [ -f "$path" ] && [ ! -L "$path" ] || ro_die "$label must be a regular non-symbolic file"
  mode=$(ro_file_mode "$path")
  [ "$mode" = 600 ] || ro_die "$label must have mode 0600"
  ro_require_private_parent "$path"
  size=$(wc -c < "$path")
  [[ "$size" =~ ^[[:space:]]*[0-9]+[[:space:]]*$ ]] || ro_die "$label has an invalid size"
  (( size >= minimum && size <= maximum + 1 )) || ro_die "$label has an invalid size"
  ro_read_token "$path" checked_token
  (( ${#checked_token} >= minimum && ${#checked_token} <= maximum )) || ro_die "$label has an invalid size"
}

ro_redact_stream() {
  local line='' value
  while IFS= read -r line || [ -n "$line" ]; do
    value=${RO_REDACT_EDGE-}
    if [ -n "$value" ]; then line=${line//"$value"/<edge-token>}; fi
    value=${RO_REDACT_CF-}
    if [ -n "$value" ]; then line=${line//"$value"/<cloudflare-token>}; fi
    printf '%s\n' "$line" || return
  done
}

ro_hhmm_minutes() {
  local hhmm=$1 hours minutes
  [[ "$hhmm" =~ ^[0-2][0-9][0-5][0-9]$ ]] || return 1
  hours=${hhmm%??}; minutes=${hhmm#??}
  (( 10#$hours < 24 )) || return 1
  printf '%s\n' "$((10#$hours * 60 + 10#$minutes))"
}

ro_in_window() {
  local now start end
  now=$(ro_hhmm_minutes "$1") || return 1
  start=$(ro_hhmm_minutes "$2") || return 1
  end=$(ro_hhmm_minutes "$3") || return 1
  if (( start < end )); then (( now >= start && now < end ));
  else (( now >= start || now < end )); fi
}

ro_wait_outside_window() {
  local window=$1 clearance=$2 maximum=$3 start end now hhmm waited=0 guarded duration
  [[ "$window" =~ ^[0-9]{4}-[0-9]{4}$ ]] || ro_die 'invalid backup window'
  [[ "$clearance" =~ ^[0-9]{1,4}$ && "$maximum" =~ ^[0-9]{1,6}$ ]] || ro_die 'invalid window wait bound'
  clearance=$((10#$clearance)); maximum=$((10#$maximum))
  (( clearance < 1440 )) || ro_die 'invalid window clearance'
  start=$(ro_hhmm_minutes "${window%-*}") || ro_die 'invalid backup window'
  end=$(ro_hhmm_minutes "${window#*-}") || ro_die 'invalid backup window'
  guarded=$(((start - clearance + 1440) % 1440))
  duration=$(((end - start + 1440) % 1440))
  if (( duration == 0 || duration + clearance >= 1440 )); then guarded=$end; fi
  # The counter advances only after an external sleep; even a frozen clock fails.
  for (( waited=0; waited <= maximum; waited+=5 )); do
    hhmm=$(date -u +%H%M) || ro_die 'cannot read clock'
    now=$(ro_hhmm_minutes "$hhmm") || ro_die 'cannot read clock'
    if (( guarded < end )); then
      if (( now < guarded || now >= end )); then return 0; fi
    elif (( now < guarded && now >= end )); then return 0; fi
    (( waited + 5 <= maximum )) || ro_die 'backup window wait exceeded its bound'
    ro_log "in backup window or its clearance (${hhmm%??}:${hhmm#??}Z); waiting"
    sleep 300 || ro_die 'backup window sleep failed'
  done
  ro_die 'backup window wait exceeded its bound'
}
