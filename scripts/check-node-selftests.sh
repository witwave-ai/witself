#!/usr/bin/env bash
set -euo pipefail

if ! command -v node >/dev/null 2>&1; then
  printf 'NOT RUN check-node-selftests: node missing\n' >&2
  if [[ ${WITSELF_TEST_REQUIRE_NODE:-} == 1 ]]; then exit 1; fi
  exit 0
fi

source_root=$(CDPATH='' cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
cd -- "$source_root"

# Prove the selector before trusting the list it reads from the workflows, then
# run every repository-root Node self-test those workflow steps name.
node --test scripts/check-node-selftests.test.mjs
exec node scripts/check-node-selftests.mjs
