#!/usr/bin/env bash
set -euo pipefail

source_root=$(CDPATH='' cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
make_bin=$(command -v make)
[[ "$make_bin" == /* ]] || make_bin="$PWD/$make_bin"
work_dir=$(mktemp -d "${TMPDIR:-/tmp}/witself-lint-proxy-test.XXXXXX")
trap 'rm -rf -- "$work_dir"' EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

fail() {
  printf 'lint-proxy regression: FAIL: %s\n' "$1" >&2
  exit 1
}

version=$(awk '
  $1 == "GOLANGCI_LINT_VERSION" && $2 == ":=" { print $3; count++ }
  END { if (count != 1) exit 1 }
' "$source_root/Makefile") || fail 'expected one GOLANGCI_LINT_VERSION pin'

scratch_cache="$work_dir/module cache"
scratch_proxy="file://$work_dir/upstream,off"
mkdir -p "$scratch_cache" "$work_dir/upstream" "$work_dir/bin"
# Only the two go env queries may run. Fixture values are supplied below, and
# rejecting everything else prevents a regression from executing the linter.
cat >"$work_dir/bin/go" <<'SH'
#!/usr/bin/env bash
case "$*" in
  'env GOMODCACHE') printf '%s\n' "$GOMODCACHE" ;;
  'env GOPROXY') printf '%s\n' "$GOPROXY" ;;
  *) exit 1 ;;
esac
SH
# Unrelated immediate Make assignments query revision metadata while parsing.
# They need no repository history for this dry-run contract.
cat >"$work_dir/bin/git" <<'SH'
#!/usr/bin/env bash
case "$*" in
  'describe --tags --always --dirty'|'rev-parse HEAD') printf 'lint-proxy-fixture\n' ;;
  *) exit 1 ;;
esac
SH
chmod +x "$work_dir/bin/go" "$work_dir/bin/git"
ln -s "$BASH" "$work_dir/bin/bash"
cd -- "$source_root"
# check already recursively calls check-infra. Suppress recursive makes so each
# requested target prints once; -n still prints every top-level recipe verbatim.
# The go fixture and a file-only proxy keep expansion fast and hermetic too.
PATH="$work_dir/bin" MAKEFLAGS='' MFLAGS='' MAKEFILES='' MAKELEVEL=0 \
  GOMODCACHE="$scratch_cache" GOPROXY="$scratch_proxy" \
  "$make_bin" -n --no-print-directory MAKE=: check check-infra >"$work_dir/dry-run" \
  || fail 'make dry-run failed'

lint_command='go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@'
lint_count=$(grep -Fc "$lint_command" "$work_dir/dry-run" || true)
[[ "$lint_count" == 2 ]] || fail 'expected exactly two golangci-lint go run lines'

proxy_prefix="GOPROXY=\"file://$scratch_cache/cache/download,$scratch_proxy\" "
root_count=0
infra_count=0
while IFS= read -r line; do
  if [[ "$line" == *"$lint_command"* ]]; then
    command_line=${line#'cd infra/pulumi && '}
    [[ "$command_line" == "$proxy_prefix"* ]] \
      || fail 'lint recipe lost its cache-first GOPROXY prefix'
    [[ "$command_line" == "$proxy_prefix$lint_command$version run ./..." ]] \
      || fail 'lint version or arguments differ from the Makefile pin'
    if [[ "$line" == "$command_line" ]]; then
      root_count=$((root_count + 1))
    else
      infra_count=$((infra_count + 1))
    fi
  elif [[ "$line" == *GOPROXY=* ]]; then
    fail 'a non-lint recipe gained GOPROXY='
  fi
done <"$work_dir/dry-run"
[[ "$root_count" == 1 && "$infra_count" == 1 ]] \
  || fail 'expected one root and one cd infra/pulumi lint recipe'

printf 'lint-proxy regression: four assertions passed (two lint recipes)\n'
