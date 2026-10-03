#!/usr/bin/env bash
set -euo pipefail
# Re-exec once under a per-run token, so process checks match only this run's
# subshells (forked subshells keep this argv), never another run or a wrapper.
if [ "${1:-}" != --run-token ]; then exec "$BASH" "$0" --run-token "run$$r$RANDOM"; fi
RUN_TOKEN=$2
HANG_SECS="31.7$$" INTERRUPT_SECS="32.3$$"

SOURCE_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
TEST_ROOT="$(mktemp -d "${TMPDIR:-/tmp}/witself-roll-train-poll.XXXXXX")"
RUN_DIR="$TEST_ROOT/run"
interrupt_wrapper='' interrupt_parent='' interrupt_command=''
cleanup() {
  local status=$?
  trap - EXIT INT TERM
  if [ -n "$interrupt_parent" ]; then kill -TERM "$interrupt_parent" 2>/dev/null || :; fi
  if [ -n "$interrupt_command" ]; then kill -KILL "$interrupt_command" 2>/dev/null || :; fi
  if [ -n "$interrupt_wrapper" ]; then wait "$interrupt_wrapper" 2>/dev/null || :; fi
  rm -rf "$TEST_ROOT"
  exit "$status"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
# shellcheck source=scripts/roll-train.sh
source "${ROLL_TRAIN_TEST_SOURCE:-$SOURCE_ROOT/scripts/roll-train.sh}"
mkdir -p "$RUN_DIR"
shopt -s nullglob
POLL_INTERVAL=1

fail() { printf 'roll train poll test: FAIL: %s\n' "$*" >&2; exit 1; }
command -v pgrep >/dev/null 2>&1 || fail 'pgrep is required'

fast_reads() {
  local count=100 started=$SECONDS before elapsed total result status i slow=0
  for ((i=1; i<=count; i++)); do
    before=$SECONDS
    status=0
    result=$(one_shot_read "fast $i" printf ok) || status=$?
    elapsed=$((SECONDS - before))
    [ "$elapsed" -lt 4 ] || slow=$((slow + 1))
    if [ "$status" -ne 0 ] || [ "$elapsed" -ge 4 ]; then
      fail "fast $i: elapsed=${elapsed}s status=$status; slow calls=$slow of $i attempted (stopped at first failure)"
    fi
    [ "$result" = ok ] || fail "fast $i returned unexpected output"
    [ "$((SECONDS - started))" -lt 60 ] || fail "fast loop reached 60s at call $i"
  done
  total=$((SECONDS - started))
  printf 'roll train poll test: fast reads passed (100 calls, slow calls=0, %ss)\n' "$total"
}

# This run's process group: forked subshells, and orphans reparented to PID 1,
# keep it; another run of this suite elsewhere has its own.
RUN_PGID=$(ps -o pgid= -p "$$" | tr -d ' ')
[[ "$RUN_PGID" =~ ^[0-9]+$ ]] || fail 'cannot read this run'"'"'s process group'

# This shell and its ancestors: Linux pgrep lists them (for example a wrapper
# such as timeout whose argv names this script); macOS pgrep omits them.
ancestors() {
  local pid=$$
  while [[ "$pid" =~ ^[0-9]+$ ]] && [ "$pid" -gt 1 ]; do
    printf '%s\n' "$pid"
    pid=$(ps -o ppid= -p "$pid" | tr -d ' ')
  done
}

poll_subshells() {
  local status=0
  ancestors >"$RUN_DIR/ancestors"
  # A command substitution here would fork a shell with the same script argv.
  pgrep -g "$RUN_PGID" -f "test-roll-train-poll\\.sh --run-token $RUN_TOKEN" >"$RUN_DIR/procs" || status=$?
  [ "$status" -le 1 ] || fail "cannot inspect poll subshells (pgrep exit $status)"
  grep -vxF -f "$RUN_DIR/ancestors" "$RUN_DIR/procs" >"$RUN_DIR/procs.other" || :
}

assert_no_subshells() {
  local probe
  # Positive control: a live subshell of this script must be visible.
  ( exec </dev/null >/dev/null 2>&1; sleep 3; : ) &
  probe=$!
  poll_subshells
  grep -qx "$probe" "$RUN_DIR/procs.other" || fail 'pgrep cannot see subshells of this script'
  pkill -KILL -P "$probe" 2>/dev/null || :
  kill -KILL "$probe" 2>/dev/null || :
  wait "$probe" 2>/dev/null || :
  poll_subshells
  [ ! -s "$RUN_DIR/procs.other" ] || fail "$1 left a poll subshell alive"
}

no_orphans() {
  local files
  sleep 1
  files=("$RUN_DIR"/poll.*)
  [ "${#files[@]}" -eq 0 ] || fail 'fast loop left poll files'
  assert_no_subshells 'fast loop'
  printf 'roll train poll test: no orphans or poll files after fast reads\n'
}

hanging_read() {
  local started=$SECONDS elapsed status=0 markers probe_status=0
  run_before "$((SECONDS + 2))" hang sleep "$HANG_SECS" >"$TEST_ROOT/out" 2>"$TEST_ROOT/err" || status=$?
  elapsed=$((SECONDS - started))
  [ "$status" -eq 1 ] || fail "hang returned $status instead of 1"
  [ "$elapsed" -ge 1 ] && [ "$elapsed" -le 4 ] || fail "hang took ${elapsed}s (expected 1-4s)"
  markers=("$RUN_DIR"/poll.*.timeout)
  [ "${#markers[@]}" -eq 1 ] || fail 'hang did not retain one timeout marker'
  [ -f "${markers[0]%.timeout}" ] || fail 'hang did not retain its output'
  grep -Fxq "roll-train: ERROR: hang timed out (poll output retained at ${markers[0]%.timeout})" "$TEST_ROOT/err" \
    || fail 'hang missed the timed-out diagnostic'
  pgrep -g "$RUN_PGID" -fx "sleep ${HANG_SECS//./\\.}" >"$RUN_DIR/hanging-procs" || probe_status=$?
  [ "$probe_status" -eq 1 ] || fail "hang process check expected no match (pgrep exit $probe_status)"
  printf 'roll train poll test: hanging read killed and output retained (%ss)\n' "$elapsed"
}

interrupt_read() {
  local started=$SECONDS status=0 probe_status=0 elapsed
  # Refuse before launching a child if this sandbox cannot inspect its parent.
  ps -o ppid= -p "$$" >"$TEST_ROOT/parent" || fail 'ps is required to find the run_before subshell'
  run_before "$((SECONDS + 20))" interrupt sleep "$INTERRUPT_SECS" >"$TEST_ROOT/out" 2>"$TEST_ROOT/err" &
  interrupt_wrapper=$!
  while :; do
    probe_status=0
    pgrep -g "$RUN_PGID" -fx "sleep ${INTERRUPT_SECS//./\\.}" >"$RUN_DIR/interrupt-procs" || probe_status=$?
    if [ "$probe_status" -eq 0 ]; then
      [ "$(wc -l <"$RUN_DIR/interrupt-procs" | tr -d ' ')" -eq 1 ] || fail 'interrupt command PID is ambiguous'
      interrupt_command=$(cat "$RUN_DIR/interrupt-procs")
      break
    fi
    [ "$probe_status" -eq 1 ] || fail "cannot inspect interrupt command (pgrep exit $probe_status)"
    [ "$((SECONDS - started))" -lt 3 ] || fail 'interrupt command did not start within 3s'
    sleep 0.1
  done
  interrupt_parent=$(ps -o ppid= -p "$interrupt_command" | tr -d ' ') \
    || fail 'could not inspect interrupt command parent'
  [[ "$interrupt_parent" =~ ^[0-9]+$ ]] && [ "$interrupt_parent" -gt 1 ] \
    || fail 'invalid run_before subshell PID'
  sleep 0.5
  kill -TERM "$interrupt_parent" || fail 'could not interrupt run_before subshell'
  wait "$interrupt_wrapper" || status=$?
  elapsed=$((SECONDS - started))
  interrupt_wrapper=''
  [ "$status" -eq 143 ] || fail "interrupt returned $status instead of 143"
  [ "$elapsed" -le 5 ] || fail "interrupt took ${elapsed}s (expected at most 5s)"
  if kill -0 "$interrupt_command" 2>/dev/null; then fail 'interrupt left its command alive'; fi
  if kill -0 "$interrupt_parent" 2>/dev/null; then fail 'interrupt left run_before alive'; fi
  interrupt_command='' interrupt_parent=''
  assert_no_subshells interrupt
  printf 'roll train poll test: TERM exits 143 and reaps children (%ss)\n' "$elapsed"
}

# Select a row only for focused diagnosis; the default always runs every row.
case "${ROLL_TRAIN_TEST_CASE:-all}" in
  all) fast_reads; no_orphans; hanging_read; interrupt_read ;;
  fast) fast_reads ;;
  hang) hanging_read ;;
  interrupt) interrupt_read ;;
  *) fail 'unknown ROLL_TRAIN_TEST_CASE' ;;
esac
printf 'roll train poll tests passed\n'
