#!/usr/bin/env bash
# Shared tool pins and invocation contract for the local and pinned CI gates.
# These public constants are consumed by scripts sourcing this library.
# shellcheck disable=SC2034
STATIC_SHELLCHECK_VERSION=0.11.0
# shellcheck disable=SC2034
STATIC_ACTIONLINT_VERSION=1.7.12

# Paths are relative to the repository root; NUL delimiters preserve filenames.
static_analysis_shell_files() {
  printf '%s\0' install.sh
  find scripts -type f -name '*.sh' -print0
}

static_analysis_workflow_files() {
  printf '%s\0' .github/workflows/*.yml
}

static_analysis_run() (
  local shellcheck_bin=$1 actionlint_bin=$2 file discovery_file
  local -a shell_files=() workflow_files=()
  if ! discovery_file=$(mktemp "${TMPDIR:-/tmp}/witself-static-analysis.XXXXXX"); then
    printf 'static analysis: could not create discovery file\n' >&2
    return 1
  fi
  # The subshell keeps these cleanup traps from replacing the caller's traps.
  trap 'rm -f -- "$discovery_file"' EXIT
  trap 'exit 129' HUP
  trap 'exit 130' INT
  trap 'exit 143' TERM
  if ! static_analysis_shell_files > "$discovery_file"; then
    printf 'static analysis: shell script discovery failed\n' >&2
    return 1
  fi
  # read -d works on macOS's Bash 3.2 as well as newer Linux Bash versions.
  while IFS= read -r -d '' file; do
    shell_files+=("$file")
  done < "$discovery_file"
  if (( ${#shell_files[@]} <= 1 )); then
    printf 'static analysis: no shell scripts found\n' >&2
    return 1
  fi
  if ! static_analysis_workflow_files > "$discovery_file"; then
    printf 'static analysis: workflow discovery failed\n' >&2
    return 1
  fi
  while IFS= read -r -d '' file; do
    workflow_files+=("$file")
  done < "$discovery_file"
  "$shellcheck_bin" "${shell_files[@]}" || return "$?"
  "$actionlint_bin" "${workflow_files[@]}"
)
