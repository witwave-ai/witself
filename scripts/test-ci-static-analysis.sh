#!/usr/bin/env bash
set -euo pipefail

fail() {
  printf 'static analysis: %s\n' "$1" >&2
  exit 1
}

[[ $(uname -s) == Linux && $(uname -m) == x86_64 ]] \
  || fail "the pinned tool bundle currently supports Linux x86_64 only"

work_dir=$(mktemp -d "${TMPDIR:-/tmp}/witself-static-analysis.XXXXXX")
cleanup() {
  find "$work_dir" -depth -mindepth 1 -delete 2>/dev/null || true
  rmdir "$work_dir" 2>/dev/null || true
}
trap cleanup EXIT INT TERM

shellcheck_version=0.11.0
shellcheck_archive="shellcheck-v${shellcheck_version}.linux.x86_64.tar.gz"
shellcheck_sha256=b7af85e41cc99489dcc21d66c6d5f3685138f06d34651e6d34b42ec6d54fe6f6
curl --fail --silent --show-error --location \
  "https://github.com/koalaman/shellcheck/releases/download/v${shellcheck_version}/${shellcheck_archive}" \
  --output "$work_dir/$shellcheck_archive"
printf '%s  %s\n' "$shellcheck_sha256" "$work_dir/$shellcheck_archive" | sha256sum --check --status \
  || fail "ShellCheck archive checksum mismatch"
tar -xzf "$work_dir/$shellcheck_archive" -C "$work_dir"

actionlint_version=1.7.12
actionlint_archive="actionlint_${actionlint_version}_linux_amd64.tar.gz"
actionlint_sha256=8aca8db96f1b94770f1b0d72b6dddcb1ebb8123cb3712530b08cc387b349a3d8
curl --fail --silent --show-error --location \
  "https://github.com/rhysd/actionlint/releases/download/v${actionlint_version}/${actionlint_archive}" \
  --output "$work_dir/$actionlint_archive"
printf '%s  %s\n' "$actionlint_sha256" "$work_dir/$actionlint_archive" | sha256sum --check --status \
  || fail "actionlint archive checksum mismatch"
tar -xzf "$work_dir/$actionlint_archive" -C "$work_dir"

mapfile -d '' shell_files < <(find scripts -type f -name '*.sh' -print0)
(( ${#shell_files[@]} > 0 )) || fail "no shell scripts found"
"$work_dir/shellcheck-v${shellcheck_version}/shellcheck" install.sh "${shell_files[@]}"
"$work_dir/actionlint" .github/workflows/*.yml

printf 'pinned ShellCheck %s and actionlint %s passed\n' "$shellcheck_version" "$actionlint_version"
