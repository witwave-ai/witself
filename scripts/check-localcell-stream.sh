#!/usr/bin/env bash
set -u

if ! command -v go >/dev/null 2>&1; then
  printf 'FAIL check-localcell-stream: go missing\n' >&2
  exit 1
fi
if ! command -v node >/dev/null 2>&1; then
  printf 'NOT RUN check-localcell-stream: node missing\n' >&2
  if [[ ${WITSELF_TEST_REQUIRE_NODE:-} == 1 ]]; then exit 1; fi
  exit 0
fi

source_root=$(CDPATH='' cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd) || exit 1
cd -- "$source_root" || exit 1

# Match CI's unit step: the separate native PostgreSQL proof stays unconfigured.
unset WITSELF_LOCALCELL_PGBIN
# Do not enable errexit: both pipeline statuses must be captured even on failure.
go test -json ./internal/localcell ./internal/runtimeconfig ./internal/tools/localcellharness -count=1 -timeout=20m | node scripts/ci-go-test-json-check.mjs
test_exits=("${PIPESTATUS[@]}")
if (( test_exits[0] != 0 || test_exits[1] != 0 )); then
  printf 'Hint: rerun go test ./internal/localcell ./internal/runtimeconfig ./internal/tools/localcellharness -count=1 -timeout=20m for detail\n'
  printf 'check-localcell-stream: FAIL go=%s checker=%s\n' "${test_exits[0]}" "${test_exits[1]}"
  exit 1
fi
printf 'check-localcell-stream: PASS\n'
