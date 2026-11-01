#!/usr/bin/env bash
set -euo pipefail

source_root=$(CDPATH='' cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
# shellcheck source=scripts/lib/static-analysis.sh
source "$source_root/scripts/lib/static-analysis.sh"

missing=false
for tool in shellcheck actionlint; do
  if ! command -v "$tool" >/dev/null 2>&1; then
    printf 'NOT RUN check-static: %s missing (brew install shellcheck actionlint)\n' "$tool" >&2
    missing=true
  fi
done
if [[ "$missing" == true ]]; then
  if [[ ${WITSELF_TEST_REQUIRE_STATIC:-} == 1 ]]; then exit 1; fi
  exit 0
fi

shellcheck_bin=$(command -v shellcheck)
actionlint_bin=$(command -v actionlint)
# Preserve relative PATH entries when analysis changes to the repository root.
[[ "$shellcheck_bin" == /* ]] || shellcheck_bin="$PWD/$shellcheck_bin"
[[ "$actionlint_bin" == /* ]] || actionlint_bin="$PWD/$actionlint_bin"
shellcheck_version=$("$shellcheck_bin" --version | sed -n 's/^version: //p')
actionlint_version=$("$actionlint_bin" -version)
actionlint_version=${actionlint_version%%$'\n'*}
printf 'check-static: shellcheck %s actionlint %s\n' "$shellcheck_version" "$actionlint_version"
if [[ "$shellcheck_version" != "$STATIC_SHELLCHECK_VERSION" ]]; then
  printf 'WARNING check-static: shellcheck %s differs from pinned %s; CI remains authoritative\n' \
    "$shellcheck_version" "$STATIC_SHELLCHECK_VERSION" >&2
fi
if [[ "$actionlint_version" != "$STATIC_ACTIONLINT_VERSION" ]]; then
  printf 'WARNING check-static: actionlint %s differs from pinned %s; CI remains authoritative\n' \
    "$actionlint_version" "$STATIC_ACTIONLINT_VERSION" >&2
fi

cd -- "$source_root"
static_analysis_run "$shellcheck_bin" "$actionlint_bin"
