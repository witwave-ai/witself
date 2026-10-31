#!/usr/bin/env bash
set -euo pipefail

source_root=$(CDPATH='' cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
bash_bin=$BASH
work_dir=$(mktemp -d "${TMPDIR:-/tmp}/witself-check-static-test.XXXXXX")
cleanup() {
  if [[ -d "$work_dir/repo/scripts/denied" ]]; then chmod 0700 "$work_dir/repo/scripts/denied"; fi
  rm -rf -- "$work_dir"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

fail() {
  printf 'check-static regression: FAIL: %s\n' "$1" >&2
  exit 1
}

# Pin the real recipes, not matching commands in comments or other targets.
awk '
  /^[^ \t#]/ { recipe = "" }
  /^check:([ \t]|$)/ { recipe = "check"; check_targets++; next }
  /^check-static:([ \t]|$)/ { recipe = "static"; static_targets++; next }
  recipe == "check" && $0 == "\t$(MAKE) check-static" { static_call = NR; static_calls++ }
  recipe == "check" && $0 == "\t$(MAKE) check-docs" { docs_call = NR; docs_calls++ }
  recipe == "static" && /^\t/ {
    static_lines++
    if (static_lines == 1 && $0 != "\tbash scripts/check-static-local.sh") bad = 1
    if (static_lines == 2 && $0 != "\tbash scripts/test-check-static-local.sh") bad = 1
  }
  /^\.PHONY:/ {
    sub(/#.*/, "")
    for (i = 2; i <= NF; i++) if ($i == "check-static") phony = 1
  }
  END {
    exit !(check_targets == 1 && static_targets == 1 && static_calls == 1 &&
      docs_calls == 1 && static_call < docs_call && static_lines == 2 && !bad && phony)
  }
' "$source_root/Makefile" || fail 'Makefile check-static wiring changed'
printf 'check-static regression: Makefile wiring passed\n'

# Follow the current YAML structure through jobs, the job, and its steps only.
awk '
  /^[^[:space:]#]/ { jobs = ($0 == "jobs:"); job = 0; steps = 0 }
  /^  [^[:space:]#]/ {
    job = (jobs && $0 == "  static-analysis:")
    steps = 0
    if (job) job_count++
  }
  /^    [^[:space:]#]/ { steps = (job && $0 == "    steps:") }
  steps && $0 == "        run: bash scripts/test-ci-static-analysis.sh" { runs++ }
  END { exit !(job_count == 1 && runs == 1) }
' "$source_root/.github/workflows/ci.yml" || fail 'CI static-analysis job wiring changed'
# Pin executable lines: a source directive comment does not load the library.
# shellcheck disable=SC2016
grep -Fxq 'source "$source_root/scripts/lib/static-analysis.sh"' "$source_root/scripts/test-ci-static-analysis.sh" \
  || fail 'CI does not source the shared static-analysis library'
# shellcheck disable=SC2016
grep -Fxq 'static_analysis_run "$work_dir/shellcheck-v${shellcheck_version}/shellcheck" "$work_dir/actionlint"' \
  "$source_root/scripts/test-ci-static-analysis.sh" || fail 'CI shared runner must receive exactly the two tool paths'
[[ $(grep -Ec '^[[:blank:]]*static_analysis_run([[:blank:]]|$)' "$source_root/scripts/test-ci-static-analysis.sh") == 1 ]] \
  || fail 'CI shared runner invocation changed'
if grep -Eq 'find[[:space:]]+scripts' "$source_root/scripts/test-ci-static-analysis.sh" || \
   grep -Fq -e mapfile -e install.sh -e '.github/workflows/*.yml' "$source_root/scripts/test-ci-static-analysis.sh"; then
  fail 'CI has a private static-analysis file set'
fi
printf 'check-static regression: CI job and shared runner wiring passed\n'

repo="$work_dir/repo"
mkdir -p "$repo/scripts/lib" "$repo/scripts/nested/deep" "$repo/scripts/directory.sh" \
  "$repo/.github/workflows/nested" "$work_dir/support" "$work_dir/both" \
  "$work_dir/shell-only" "$work_dir/action-only" "$work_dir/missing" "$work_dir/discovery-tmp"
cp "$source_root/scripts/check-static-local.sh" "$repo/scripts/"
cp "$source_root/scripts/lib/static-analysis.sh" "$repo/scripts/lib/"
touch "$repo/install.sh" "$repo/outside.sh" "$repo/scripts/top.sh" \
  "$repo/scripts/nested/deep/name with spaces.sh" "$repo/scripts/nested/line"$'\n'"break.sh" \
  "$repo/scripts/ignored.txt" "$repo/.github/workflows/ci.yml" \
  "$repo/.github/workflows/space name.yml" "$repo/.github/workflows/ignored.yaml" \
  "$repo/.github/workflows/nested/ignored.yml"

# Only these support utilities are visible: installed lint tools cannot leak in.
for utility in dirname find sed mktemp rm; do
  ln -s "$(command -v "$utility")" "$work_dir/support/$utility"
done
cat >"$work_dir/both/shellcheck" <<'SH'
#!/bin/bash
if [[ "$1" == --version ]]; then
  printf 'ShellCheck - fixture\nversion: %s\nlicense: fixture\n' "$STATIC_TEST_SHELLCHECK_VERSION"
  exit 0
fi
printf '%s\0' "$@" >>"$STATIC_TEST_LOG/shellcheck.args"
printf 'called\n' >>"$STATIC_TEST_LOG/shellcheck.calls"
if [[ "$STATIC_TEST_SHELLCHECK_STATUS" != 0 ]]; then
  printf 'synthetic ShellCheck finding\n' >&2
fi
exit "$STATIC_TEST_SHELLCHECK_STATUS"
SH
cat >"$work_dir/both/actionlint" <<'SH'
#!/bin/bash
if [[ "$1" == -version ]]; then
  printf '%s\nfixture build\n' "$STATIC_TEST_ACTIONLINT_VERSION"
  exit 0
fi
printf '%s\0' "$@" >>"$STATIC_TEST_LOG/actionlint.args"
printf 'called\n' >>"$STATIC_TEST_LOG/actionlint.calls"
if [[ "$STATIC_TEST_ACTIONLINT_STATUS" != 0 ]]; then
  printf 'synthetic actionlint finding\n' >&2
fi
exit "$STATIC_TEST_ACTIONLINT_STATUS"
SH
chmod +x "$work_dir/both/shellcheck" "$work_dir/both/actionlint"
cp "$work_dir/both/shellcheck" "$work_dir/shell-only/"
cp "$work_dir/both/actionlint" "$work_dir/action-only/"

# shellcheck source=scripts/lib/static-analysis.sh
source "$source_root/scripts/lib/static-analysis.sh"
[[ "$STATIC_SHELLCHECK_VERSION" == 0.11.0 && "$STATIC_ACTIONLINT_VERSION" == 1.7.12 ]] \
  || fail 'shared version pins changed'
# Match literal source assignments, not expanded values or comments.
# shellcheck disable=SC2016
grep -Fxq 'shellcheck_version=$STATIC_SHELLCHECK_VERSION' "$source_root/scripts/test-ci-static-analysis.sh" \
  || fail 'CI does not reference the shared ShellCheck pin'
# shellcheck disable=SC2016
grep -Fxq 'actionlint_version=$STATIC_ACTIONLINT_VERSION' "$source_root/scripts/test-ci-static-analysis.sh" \
  || fail 'CI does not reference the shared actionlint pin'
[[ $(grep -c '^shellcheck_version=' "$source_root/scripts/test-ci-static-analysis.sh") == 1 && \
   $(grep -c '^actionlint_version=' "$source_root/scripts/test-ci-static-analysis.sh") == 1 ]] \
  || fail 'CI overrides a shared version pin'
printf 'check-static regression: shared CI version pins passed\n'

(cd -- "$repo" && static_analysis_shell_files) >"$work_dir/shell-files"
count=0
while IFS= read -r -d '' file; do
  case "$file" in
    install.sh|scripts/check-static-local.sh|scripts/lib/static-analysis.sh|scripts/top.sh|\
      'scripts/nested/deep/name with spaces.sh'|$'scripts/nested/line\nbreak.sh') ;;
    *) fail 'unexpected shell fixture path' ;;
  esac
  count=$((count + 1))
done <"$work_dir/shell-files"
[[ "$count" == 6 ]] || fail 'shell file set omitted or added a path'
for expected in install.sh scripts/check-static-local.sh scripts/lib/static-analysis.sh scripts/top.sh \
  'scripts/nested/deep/name with spaces.sh' $'scripts/nested/line\nbreak.sh'; do
  occurrences=0
  while IFS= read -r -d '' file; do
    if [[ "$file" == "$expected" ]]; then occurrences=$((occurrences + 1)); fi
  done <"$work_dir/shell-files"
  [[ "$occurrences" == 1 ]] || fail 'shell file set omitted or duplicated a path'
done
(cd -- "$repo" && static_analysis_workflow_files) >"$work_dir/workflow-files"
printf '%s\0' .github/workflows/ci.yml '.github/workflows/space name.yml' >"$work_dir/expected-workflows"
cmp -s "$work_dir/workflow-files" "$work_dir/expected-workflows" || fail 'workflow glob changed'
printf 'check-static regression: exact NUL-separated shell and workflow file sets passed\n'

passed=0
skipped=0
clear_tool_logs() {
  rm -f "$work_dir/shellcheck.args" "$work_dir/actionlint.args" \
    "$work_dir/shellcheck.calls" "$work_dir/actionlint.calls"
}
assert_no_tools() {
  [[ ! -e "$work_dir/shellcheck.calls" && ! -e "$work_dir/actionlint.calls" ]] \
    || fail 'discovery failure invoked a linter'
}
assert_discovery_cleanup() {
  [[ -z $(find "$work_dir/discovery-tmp" -mindepth 1 -print) ]] || fail 'discovery temporary file leaked'
}
# Each child receives fresh fixture settings; exports intentionally stay local.
# shellcheck disable=SC2030,SC2031
run_case() {
  local name=$1 expected=$2 tool_path=$3 require=$4 result=0
  clear_tool_logs
  (
    unset WITSELF_TEST_REQUIRE_STATIC
    if [[ "$require" == 1 ]]; then export WITSELF_TEST_REQUIRE_STATIC=1; fi
    export STATIC_TEST_LOG="$work_dir"
    export STATIC_TEST_SHELLCHECK_STATUS="${5:-0}" STATIC_TEST_ACTIONLINT_STATUS="${6:-0}"
    export STATIC_TEST_SHELLCHECK_VERSION="${7:-$STATIC_SHELLCHECK_VERSION}"
    export STATIC_TEST_ACTIONLINT_VERSION="${8:-$STATIC_ACTIONLINT_VERSION}"
    # The wrapper must find the repository even when called from elsewhere.
    cd -- "$work_dir"
    TMPDIR="$work_dir/discovery-tmp" PATH="$tool_path:$work_dir/support" \
      "$bash_bin" "$repo/scripts/check-static-local.sh"
  ) >"$work_dir/stdout" 2>"$work_dir/stderr" || result=$?
  [[ "$result" == "$expected" ]] || fail "$name returned $result instead of $expected"
  assert_discovery_cleanup
  passed=$((passed + 1))
  printf 'check-static regression: %s passed\n' "$name"
}

assert_missing() {
  local tool=$1
  grep -Fxq "NOT RUN check-static: $tool missing (brew install shellcheck actionlint)" "$work_dir/stderr" \
    || fail "$tool missing verdict absent from stderr"
  [[ ! -s "$work_dir/stdout" && ! -e "$work_dir/shellcheck.args" && ! -e "$work_dir/actionlint.args" ]] \
    || fail 'missing tools ran partial analysis or printed success'
}

run_case 'both tools missing, flag unset' 0 "$work_dir/missing" 0
assert_missing shellcheck
assert_missing actionlint
run_case 'both tools missing, strict flag' 1 "$work_dir/missing" 1
assert_missing shellcheck
assert_missing actionlint
run_case 'only ShellCheck missing' 0 "$work_dir/action-only" 0
assert_missing shellcheck
run_case 'only ShellCheck missing, strict flag' 1 "$work_dir/action-only" 1
assert_missing shellcheck
run_case 'only actionlint missing' 0 "$work_dir/shell-only" 0
assert_missing actionlint
run_case 'only actionlint missing, strict flag' 1 "$work_dir/shell-only" 1
assert_missing actionlint

run_case 'ShellCheck finding' 1 "$work_dir/both" 0 1
grep -Fxq 'synthetic ShellCheck finding' "$work_dir/stderr" || fail 'ShellCheck finding swallowed'
[[ ! -e "$work_dir/actionlint.args" ]] || fail 'analysis continued after ShellCheck failure'
run_case 'actionlint finding' 1 "$work_dir/both" 0 0 1
grep -Fxq 'synthetic actionlint finding' "$work_dir/stderr" || fail 'actionlint finding swallowed'

run_case 'both tools pass' 0 "$work_dir/both" 0
grep -Fxq "check-static: shellcheck $STATIC_SHELLCHECK_VERSION actionlint $STATIC_ACTIONLINT_VERSION" \
  "$work_dir/stdout" || fail 'version line absent'
[[ ! -s "$work_dir/stderr" ]] || fail 'matching versions warned'
cmp -s "$work_dir/shell-files" "$work_dir/shellcheck.args" || fail 'ShellCheck arguments differ from CI contract'
cmp -s "$work_dir/workflow-files" "$work_dir/actionlint.args" || fail 'actionlint arguments differ from CI contract'
run_case 'both tools pass, strict flag' 0 "$work_dir/both" 1
run_case 'relative PATH tools from another directory' 0 both 0
cmp -s "$work_dir/shell-files" "$work_dir/shellcheck.args" || fail 'relative PATH lost ShellCheck'
cmp -s "$work_dir/workflow-files" "$work_dir/actionlint.args" || fail 'relative PATH lost actionlint'

run_case 'version mismatches warn and still run' 0 "$work_dir/both" 1 0 0 0.0.1 0.0.2
grep -Fxq "WARNING check-static: shellcheck 0.0.1 differs from pinned $STATIC_SHELLCHECK_VERSION; CI remains authoritative" \
  "$work_dir/stderr" || fail 'ShellCheck version mismatch did not warn'
grep -Fxq "WARNING check-static: actionlint 0.0.2 differs from pinned $STATIC_ACTIONLINT_VERSION; CI remains authoritative" \
  "$work_dir/stderr" || fail 'actionlint version mismatch did not warn'
[[ -e "$work_dir/shellcheck.args" && -e "$work_dir/actionlint.args" ]] || fail 'version mismatch skipped analysis'

# Exercise real traversal failure as well as a deterministic partial producer.
if (( EUID == 0 )); then
  printf 'check-static regression: NOT RUN unreadable shell directory (root bypasses permissions; 2 cases)\n'
  skipped=$((skipped + 2))
else
  mkdir "$repo/scripts/denied"
  # The defective script is fixture text, not an expansion in this test.
  # shellcheck disable=SC2016
  printf '#!/bin/bash\necho $unquoted\n' >"$repo/scripts/denied/bad.sh"
  chmod 000 "$repo/scripts/denied"
  for require in 0 1; do
    run_case "unreadable shell directory fails closed, strict=$require" 1 "$work_dir/both" "$require"
    grep -Fxq 'static analysis: shell script discovery failed' "$work_dir/stderr" \
      || fail 'unreadable shell directory lacked discovery diagnostic'
    assert_no_tools
  done
  chmod 0700 "$repo/scripts/denied"
  rm -rf "$repo/scripts/denied"
fi

mkdir "$work_dir/partial-find" "$work_dir/empty-find"
cat >"$work_dir/partial-find/find" <<'SH'
#!/bin/bash
printf '%s\0' scripts/top.sh
printf 'synthetic traversal error\n' >&2
exit 1
SH
printf '#!/bin/bash\nexit 0\n' >"$work_dir/empty-find/find"
chmod +x "$work_dir/partial-find/find" "$work_dir/empty-find/find"
for require in 0 1; do
  run_case "partial shell discovery fails closed, strict=$require" 1 \
    "$work_dir/partial-find:$work_dir/both" "$require"
  grep -Fxq 'static analysis: shell script discovery failed' "$work_dir/stderr" \
    || fail 'partial shell discovery lacked discovery diagnostic'
  assert_no_tools
done
run_case 'empty shell discovery fails closed' 1 "$work_dir/empty-find:$work_dir/both" 0
grep -Fxq 'static analysis: no shell scripts found' "$work_dir/stderr" \
  || fail 'empty shell discovery lacked empty-result diagnostic'
assert_no_tools

# A separate Bash process calls the library in an if condition: errexit is
# suppressed throughout the function, so its explicit return guards matter.
# shellcheck disable=SC2030,SC2031
run_conditional_case() {
  local name=$1 expected=$2 shell_status=$3 action_status=$4 workflow_status=${5:-0} result=0
  clear_tool_logs
  (
    cd -- "$repo"
    export STATIC_TEST_LOG="$work_dir" STATIC_TEST_SHELLCHECK_STATUS="$shell_status"
    export STATIC_TEST_ACTIONLINT_STATUS="$action_status" STATIC_TEST_WORKFLOW_STATUS="$workflow_status"
    # Expand these variables in the child Bash, after sourcing its library.
    # shellcheck disable=SC2016
    TMPDIR="$work_dir/discovery-tmp" PATH="$work_dir/both:$work_dir/support" "$bash_bin" -c '
      set -euo pipefail
      source scripts/lib/static-analysis.sh
      if [[ "$STATIC_TEST_WORKFLOW_STATUS" != 0 ]]; then
        static_analysis_workflow_files() { printf "%s\0" .github/workflows/ci.yml; return 1; }
      fi
      if static_analysis_run shellcheck actionlint; then exit 0; else exit "$?"; fi
    '
  ) >"$work_dir/stdout" 2>"$work_dir/stderr" || result=$?
  [[ "$result" == "$expected" ]] || fail "$name returned $result instead of $expected"
  assert_discovery_cleanup
  passed=$((passed + 1))
  printf 'check-static regression: %s passed\n' "$name"
}

for status in 1 7; do
  run_conditional_case "conditional shared runner preserves ShellCheck failure $status" "$status" "$status" 0
  cmp -s "$work_dir/shell-files" "$work_dir/shellcheck.args" || fail 'conditional ShellCheck arguments changed'
  [[ ! -e "$work_dir/actionlint.calls" ]] || fail 'conditional runner continued after ShellCheck failure'
done
run_conditional_case 'conditional shared runner preserves actionlint failure' 1 0 1
cmp -s "$work_dir/shell-files" "$work_dir/shellcheck.args" || fail 'conditional ShellCheck arguments changed'
cmp -s "$work_dir/workflow-files" "$work_dir/actionlint.args" || fail 'conditional actionlint arguments changed'
run_conditional_case 'partial workflow discovery fails closed' 1 0 0 1
grep -Fxq 'static analysis: workflow discovery failed' "$work_dir/stderr" \
  || fail 'workflow discovery lacked discovery diagnostic'
assert_no_tools

# Execute the real CI driver and both conflict-marker prelude scripts. Only
# Linux detection, downloads, checksum verification and extraction are faked.
ci_repo="$work_dir/ci-repo"
ci_bin="$work_dir/ci-bin"
cp -R "$repo" "$ci_repo"
cp "$source_root/Makefile" "$ci_repo/Makefile"
cp "$source_root/scripts/test-ci-static-analysis.sh" "$source_root/scripts/check-conflict-markers.sh" \
  "$source_root/scripts/test-conflict-markers.sh" "$ci_repo/scripts/"
cp "$source_root/.github/workflows/ci.yml" "$source_root/.github/workflows/release.yml" \
  "$ci_repo/.github/workflows/"
mkdir "$ci_bin" "$work_dir/git-templates"
for utility in dirname find sed mktemp rm rmdir mkdir cp chmod git grep ln cat touch; do
  ln -s "$(command -v "$utility")" "$ci_bin/$utility"
done
ln -s "$bash_bin" "$ci_bin/bash"
cat >"$ci_bin/uname" <<'SH'
#!/bin/bash
case "$1" in
  -s) printf 'Linux\n' ;;
  -m) printf 'x86_64\n' ;;
  *) exit 1 ;;
esac
SH
cat >"$ci_bin/curl" <<'SH'
#!/bin/bash
while (( $# > 0 )); do
  if [[ "$1" == --output && $# == 2 ]]; then
    cp "$STATIC_TEST_ARCHIVE" "$2"
    exit "$?"
  fi
  shift
done
exit 1
SH
cat >"$ci_bin/sha256sum" <<'SH'
#!/bin/bash
[[ $# == 2 && "$1" == --check && "$2" == --status ]] || exit 1
cat >/dev/null
SH
cat >"$ci_bin/tar" <<'SH'
#!/bin/bash
set -euo pipefail
[[ $# == 4 && "$1" == -xzf && "$3" == -C && -s "$2" ]] || exit 1
case "${2##*/}" in
  "shellcheck-v${STATIC_TEST_SHELLCHECK_VERSION}.linux.x86_64.tar.gz")
    mkdir -p "$4/shellcheck-v${STATIC_TEST_SHELLCHECK_VERSION}"
    cp "$STATIC_TEST_TOOLS/shellcheck" "$4/shellcheck-v${STATIC_TEST_SHELLCHECK_VERSION}/shellcheck"
    ;;
  "actionlint_${STATIC_TEST_ACTIONLINT_VERSION}_linux_amd64.tar.gz")
    cp "$STATIC_TEST_TOOLS/actionlint" "$4/actionlint"
    ;;
  *) exit 1 ;;
esac
SH
chmod +x "$ci_bin/uname" "$ci_bin/curl" "$ci_bin/sha256sum" "$ci_bin/tar"
# Use a real, small archive as the fake download payload; extraction is stubbed.
tar -czf "$work_dir/download.tar.gz" -C "$work_dir/missing" .
(cd -- "$ci_repo" && static_analysis_shell_files) >"$work_dir/ci-shell-files"
(cd -- "$ci_repo" && static_analysis_workflow_files) >"$work_dir/ci-workflow-files"

# shellcheck disable=SC2030,SC2031
run_ci_case() {
  local name=$1 expected=$2 shell_status=$3 action_status=$4 result=0
  clear_tool_logs
  (
    # Scratch Git metadata is sufficient for the prelude; the candidate index
    # and user configuration must never be involved in this fixture.
    unset GIT_DIR GIT_WORK_TREE GIT_INDEX_FILE GIT_COMMON_DIR
    unset GIT_OBJECT_DIRECTORY GIT_ALTERNATE_OBJECT_DIRECTORIES GIT_CONFIG_COUNT GIT_CONFIG_PARAMETERS
    export GIT_CONFIG_NOSYSTEM=1 GIT_CONFIG_SYSTEM=/dev/null GIT_CONFIG_GLOBAL=/dev/null
    export STATIC_TEST_LOG="$work_dir" STATIC_TEST_TOOLS="$work_dir/both"
    export STATIC_TEST_ARCHIVE="$work_dir/download.tar.gz"
    export STATIC_TEST_SHELLCHECK_VERSION="$STATIC_SHELLCHECK_VERSION"
    export STATIC_TEST_ACTIONLINT_VERSION="$STATIC_ACTIONLINT_VERSION"
    export STATIC_TEST_SHELLCHECK_STATUS="$shell_status" STATIC_TEST_ACTIONLINT_STATUS="$action_status"
    export PATH="$ci_bin" TMPDIR="$work_dir/discovery-tmp"
    cd -- "$ci_repo"
    git init -q --template="$work_dir/git-templates" || exit "$?"
    "$bash_bin" scripts/test-ci-static-analysis.sh
  ) >"$work_dir/stdout" 2>"$work_dir/stderr" || result=$?
  [[ "$result" == "$expected" ]] || fail "$name returned $result instead of $expected"
  grep -Fxq 'conflict marker check: PASS' "$work_dir/stdout" || fail 'CI conflict check did not run'
  grep -Fq 'conflict marker regression: PASS (' "$work_dir/stdout" || fail 'CI conflict regression did not run'
  if [[ "$expected" == 0 ]]; then
    [[ $(tail -n 1 "$work_dir/stdout") == \
      "pinned ShellCheck $STATIC_SHELLCHECK_VERSION and actionlint $STATIC_ACTIONLINT_VERSION passed" ]] \
      || fail 'CI final success verdict missing'
  elif grep -Fq 'pinned ShellCheck' "$work_dir/stdout"; then
    fail 'CI printed success after a tool failure'
  fi
  cmp -s "$work_dir/ci-shell-files" "$work_dir/shellcheck.args" || fail 'CI ShellCheck invocation/file set changed'
  [[ $(cat "$work_dir/shellcheck.calls") == called ]] || fail 'CI ShellCheck invocation count changed'
  if [[ "$shell_status" == 0 ]]; then
    cmp -s "$work_dir/ci-workflow-files" "$work_dir/actionlint.args" || fail 'CI actionlint invocation/file set changed'
    [[ $(cat "$work_dir/actionlint.calls") == called ]] || fail 'CI actionlint invocation count changed'
  else
    [[ ! -e "$work_dir/actionlint.calls" ]] || fail 'CI continued after ShellCheck failure'
  fi
  assert_discovery_cleanup
  passed=$((passed + 1))
  printf 'check-static regression: %s passed\n' "$name"
}
run_ci_case 'CI driver actually invokes both pinned tools' 0 0 0
run_ci_case 'CI driver preserves ShellCheck failure' 1 1 0
run_ci_case 'CI driver preserves actionlint failure' 1 0 1
printf 'check-static regression: PASS (%s execution cases; %s NOT RUN; file sets, shared pins, and wiring)\n' \
  "$passed" "$skipped"
