#!/usr/bin/env bash
# Run a local gate, log it, and exit with the gate's own status.
#
# This exists because of a specific failure mode. A gate wrapped inline as
#
#   make check > gate.log 2>&1; echo "EXIT=$?" >> gate.log; tail -3 gate.log
#
# exits with the status of the LAST command in the list — the tail, which
# always succeeds. Any caller reading the wrapper's exit code, including an
# agent harness reporting a completed background task, is then told the gate
# passed while make actually failed. That has already hidden a genuine test
# failure once.
#
# The verdict here is the gate's real status: the log's last line says PASS or
# FAIL and the script exits with the underlying code, so a caller cannot read
# success into a failed run.
set -euo pipefail

usage() {
  cat <<'EOF'
usage: run-gate.sh [--log PATH] -- <command> [args...]
       run-gate.sh [--log PATH] check|check-infra

Runs the command (or the named make target), tees output to a log, and exits
with the command's own status. The final log line is the machine-readable
verdict: "run-gate: PASS <command>" or "run-gate: FAIL <code> <command>".

  --log PATH   where to write the log (default: a mktemp file, path printed)

examples:
  scripts/run-gate.sh check
  scripts/run-gate.sh --log /tmp/infra.log check-infra
  scripts/run-gate.sh -- go test ./internal/store -count=1
EOF
}

log_path=""
while [ $# -gt 0 ]; do
  case "$1" in
    --log) [ $# -ge 2 ] || { usage >&2; exit 2; }; log_path="$2"; shift 2 ;;
    -h|--help) usage; exit 0 ;;
    --) shift; break ;;
    check|check-infra) set -- make "$1"; break ;;
    *) usage >&2; exit 2 ;;
  esac
done

[ $# -gt 0 ] || { usage >&2; exit 2; }
[ -n "$log_path" ] || log_path="$(mktemp -t witself-gate)"

printf 'run-gate: log %s\n' "$log_path"
printf 'run-gate: running %s\n' "$*" | tee "$log_path"

set +e
"$@" >>"$log_path" 2>&1
code=$?
set -e

if [ "$code" -eq 0 ]; then
  printf 'run-gate: PASS %s\n' "$*" | tee -a "$log_path"
else
  printf 'run-gate: FAIL %d %s\n' "$code" "$*" | tee -a "$log_path"
  printf 'run-gate: last 20 log lines follow\n'
  tail -20 "$log_path"
fi
exit "$code"
