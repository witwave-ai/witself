#!/usr/bin/env bash
set -euo pipefail
export LC_ALL=C

source_root=$(CDPATH='' cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
checker="$source_root/scripts/check-conflict-markers.sh"
real_git=$(command -v git)
umask 077
work_dir=$(mktemp -d "${TMPDIR:-/tmp}/witself-conflict-test.XXXXXX")
cleanup() {
  if [[ -d "$work_dir/repo/denied" ]]; then chmod 0700 "$work_dir/repo/denied"; fi
  rm -rf -- "$work_dir"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

fail() {
  printf 'conflict marker regression: FAIL: %s\n' "$1" >&2
  exit 1
}

# Fixture commits never use the caller's identity, hooks or Git configuration.
unset GIT_DIR GIT_WORK_TREE GIT_INDEX_FILE GIT_COMMON_DIR
unset GIT_OBJECT_DIRECTORY GIT_ALTERNATE_OBJECT_DIRECTORIES
unset GIT_CONFIG_COUNT GIT_CONFIG_PARAMETERS
export GIT_CONFIG_NOSYSTEM=1 GIT_CONFIG_SYSTEM=/dev/null GIT_CONFIG_GLOBAL=/dev/null
export GIT_AUTHOR_NAME=Fixture GIT_COMMITTER_NAME=Fixture
export GIT_AUTHOR_EMAIL=fixture@example.invalid GIT_COMMITTER_EMAIL=fixture@example.invalid
mkdir -p "$work_dir/repo/subdir" "$work_dir/hooks" "$work_dir/shims" "$work_dir/templates"
repo="$work_dir/repo"
fixture_git() { "$real_git" -c core.hooksPath="$work_dir/hooks" -C "$repo" "$@"; }
fixture_git init -q --template="$work_dir/templates"
printf 'clean tracked text\n' >"$repo/note with spaces.md"
printf 'nested clean text\n' >"$repo/subdir/note.md"
fixture_git add -- .
fixture_git -c commit.gpgsign=false commit -qm 'clean fixture'
start=$(printf '%s%s' '<<<' '<<<<')
end=$(printf '%s%s' '>>>' '>>>>')
passed=0

run_check() {
  local expected=$1 name=$2 directory=$3
  local result=0
  (cd -- "$directory" && bash "$checker") >"$work_dir/output" 2>&1 || result=$?
  if [[ "$expected" == pass ]]; then
    [[ "$result" == 0 ]] || fail "$name unexpectedly failed"
    grep -Fxq 'conflict marker check: PASS' "$work_dir/output" || fail "$name lacked a pass verdict"
  else
    [[ "$result" == 1 ]] || fail "$name did not refuse with exit 1"
    grep -Fq "$expected" "$work_dir/output" || fail "$name lacked its intended refusal"
    if grep -Fq 'PRIVATE_MARKER_SENTINEL' "$work_dir/output"; then
      fail "$name printed file content"
    fi
  fi
  passed=$((passed + 1))
  printf 'conflict marker regression: %s passed\n' "$name"
}

marker_refusal='conflict markers found in tracked files:'
run_check pass 'clean tracked tree' "$repo"
printf '%s PRIVATE_MARKER_SENTINEL\ntext\n%s fixture\n' "$start" "$end" >"$repo/note with spaces.md"
fixture_git add -- 'note with spaces.md'
fixture_git -c commit.gpgsign=false commit -qm 'committed conflict fixture'
run_check "$marker_refusal" 'committed markers' "$repo"
grep -Fq 'note\ with\ spaces.md' "$work_dir/output" || fail 'spaced filename was not identified'
run_check "$marker_refusal" 'subdirectory sees root conflict' "$repo/subdir"
printf 'clean tracked text\n' >"$repo/note with spaces.md"
fixture_git add -- 'note with spaces.md'
fixture_git -c commit.gpgsign=false commit -qm 'clear committed fixture'

printf '%s PRIVATE_MARKER_SENTINEL\n' "$start" >"$repo/note with spaces.md"
run_check "$marker_refusal" 'modified start marker only' "$repo"
printf '%s PRIVATE_MARKER_SENTINEL\n' "$end" >"$repo/note with spaces.md"
run_check "$marker_refusal" 'modified end marker only' "$repo"
fixture_git update-index --assume-unchanged -- 'note with spaces.md'
run_check "$marker_refusal" 'assume-unchanged current content' "$repo"
fixture_git update-index --no-assume-unchanged -- 'note with spaces.md'
fixture_git update-index --skip-worktree -- 'note with spaces.md'
run_check "$marker_refusal" 'skip-worktree current content' "$repo"
fixture_git update-index --no-skip-worktree -- 'note with spaces.md'
printf 'clean tracked text\n' >"$repo/note with spaces.md"

printf '%s fixture\n' "$start" >"$repo/staged.md"
fixture_git add -- staged.md
run_check "$marker_refusal" 'new staged file' "$repo"
fixture_git rm -q -f -- staged.md

printf 'Heading\n=======\ninline %s example\ninline %s example\n%s\n%s\n' \
  "$start" "$end" "$start" "$end" >"$repo/note with spaces.md"
run_check pass 'Markdown and inline examples' "$repo"
printf '%s fixture\n' "$start" >"$repo/untracked.md"
run_check pass 'untracked file excluded' "$repo"
printf '\000%s fixture\n' "$start" >"$repo/binary.dat"
fixture_git add -- binary.dat
run_check pass 'binary file excluded' "$repo"

printf '%s fixture\n' "$start" >"$work_dir/private.md"
ln -s "$work_dir/private.md" "$repo/tracked-link"
fixture_git add -- tracked-link
run_check pass 'tracked symlink excluded' "$repo"
rm -- "$repo/note with spaces.md"
ln -s "$work_dir/private.md" "$repo/note with spaces.md"
run_check pass 'regular index entry replaced by symlink' "$repo"
rm -- "$repo/note with spaces.md"
run_check pass 'missing tracked file excluded' "$repo"
printf 'clean tracked text\n' >"$repo/note with spaces.md"
mkdir "$work_dir/private-dir"
printf '%s fixture\n' "$start" >"$work_dir/private-dir/note.md"
rm -- "$repo/subdir/note.md"
rmdir "$repo/subdir"
ln -s "$work_dir/private-dir" "$repo/subdir"
run_check pass 'ancestor symlink excluded' "$repo"
rm -- "$repo/subdir"
mkdir "$repo/subdir"
printf 'nested clean text\n' >"$repo/subdir/note.md"

mkdir "$repo/denied"
printf '%s fixture\n' "$start" >"$repo/denied/note.md"
fixture_git add -- denied/note.md
chmod 000 "$repo/denied"
if [[ -x "$repo/denied" ]]; then
  # A privileged runner can bypass directory permissions; it must still
  # inspect the current file and refuse its marker rather than skip it.
  run_check "$marker_refusal" 'privileged directory content inspected' "$repo"
else
  run_check 'could not inspect a tracked directory' 'inaccessible tracked directory' "$repo"
fi
chmod 0700 "$repo/denied"
fixture_git rm -q -f -- denied/note.md

commit=$(fixture_git rev-parse HEAD)
fixture_git update-index --add --cacheinfo "160000,$commit,vendor"
mkdir "$repo/vendor"
printf '%s fixture\n' "$start" >"$repo/vendor/untracked.md"
run_check pass 'gitlink excluded' "$repo"
mkdir "$work_dir/outside"
run_check 'could not find the current worktree' 'outside repository' "$work_dir/outside"

cat >"$work_dir/shims/git" <<'SH'
#!/usr/bin/env bash
if [[ "$1" == ls-files ]]; then
  case "$CONFLICT_GIT_FAILURE" in
    stderr) printf 'synthetic read error\n' >&2; exit 0 ;;
    one) printf 'synthetic read error\n' >&2; exit 1 ;;
    two) exit 2 ;;
  esac
fi
exec "$CONFLICT_REAL_GIT" "$@"
SH
chmod 0755 "$work_dir/shims/git"
export CONFLICT_REAL_GIT="$real_git"
for failure in stderr one two; do
  if [[ "$failure" == stderr ]]; then expected='Git reported a tracked-file error';
  else expected='could not list tracked files'; fi
  PATH="$work_dir/shims:$PATH" CONFLICT_GIT_FAILURE="$failure" \
    run_check "$expected" "Git operational failure $failure" "$repo"
done

cat >"$work_dir/shims/grep" <<'SH'
#!/usr/bin/env bash
printf 'synthetic read error\n' >&2
exit 2
SH
chmod 0755 "$work_dir/shims/grep"
result=0
(cd -- "$repo" && PATH="$work_dir/shims:$PATH" CONFLICT_GIT_FAILURE=none bash "$checker") \
  >"$work_dir/output" 2>&1 || result=$?
[[ "$result" == 1 ]] || fail 'file read error did not refuse'
grep -Fq 'could not read a tracked file' "$work_dir/output" || fail 'file read error lacked its intended refusal'
passed=$((passed + 1))
printf 'conflict marker regression: file read error passed\n'

# Prove the shared entry points run these checks before existing gate work.
found=false
while IFS= read -r line; do
  if [[ "$line" == 'check: '* ]]; then
    IFS= read -r first
    IFS= read -r second
    [[ "$first" == $'\tbash scripts/check-conflict-markers.sh' && \
       "$second" == $'\tbash scripts/test-conflict-markers.sh' ]] || fail 'Make check wiring/order'
    found=true
    break
  fi
done <"$source_root/Makefile"
[[ "$found" == true ]] || fail 'Make check target missing'

found=false
# Match literal shell source here; these expressions must not expand.
# shellcheck disable=SC2016
while IFS= read -r line; do
  if [[ "$line" == 'bash "$source_root/scripts/check-conflict-markers.sh"' ]]; then
    IFS= read -r line
    [[ "$line" == 'bash "$source_root/scripts/test-conflict-markers.sh"' ]] || fail 'static guard regression order'
    found=true
    break
  fi
  [[ "$line" != *'$(uname '* && "$line" != curl\ * ]] || fail 'static guards run too late'
done <"$source_root/scripts/test-ci-static-analysis.sh"
[[ "$found" == true ]] || fail 'static guard wiring missing'
for workflow in ci release; do
  grep -Fq 'run: bash scripts/test-ci-static-analysis.sh' "$source_root/.github/workflows/$workflow.yml" \
    || fail "$workflow lost the shared static gate"
done
printf 'conflict marker regression: PASS (%s cases; local/CI/release wiring)\n' "$passed"
