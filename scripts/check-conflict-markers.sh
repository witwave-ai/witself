#!/usr/bin/env bash
set -euo pipefail
export LC_ALL=C

# Read current files, not Git's cached contents: assume-unchanged and
# skip-worktree must not hide a conflict introduced after checkout.
umask 077
work_dir=$(mktemp -d "${TMPDIR:-/tmp}/witself-conflict-check.XXXXXX")
cleanup() { rm -rf -- "$work_dir"; }
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

fail() {
  printf 'conflict marker check: %s\n' "$1" >&2
  exit 1
}

if ! source_root=$(git rev-parse --show-toplevel 2>"$work_dir/errors"); then
  fail 'could not find the current worktree'
fi
[[ ! -s "$work_dir/errors" ]] || fail 'Git reported a worktree error'
cd -- "$source_root"
if ! git ls-files --stage -z >"$work_dir/tracked" 2>"$work_dir/errors"; then
  fail 'could not list tracked files'
fi
[[ ! -s "$work_dir/errors" ]] || fail 'Git reported a tracked-file error'

found=false
while IFS= read -r -d '' entry; do
  [[ "$entry" == *$'\t'* ]] || fail 'invalid tracked-file record'
  metadata=${entry%%$'\t'*}
  path=${entry#*$'\t'}
  case "${metadata%% *}" in
    120000|160000) continue ;; # Index symlinks and submodule gitlinks.
    100644|100755) ;;
    *) fail 'unsupported tracked-file mode' ;;
  esac

  # Also exclude a regular index entry replaced by a symlink, or whose
  # directory was replaced by one. Never read through such a link.
  parent=
  remaining=$path
  linked=false
  while [[ "$remaining" == */* ]]; do
    component=${remaining%%/*}
    parent=${parent:+$parent/}$component
    if [[ -L "$parent" ]]; then linked=true; break; fi
    # A denied ancestor can make -e below look like an intentional deletion.
    [[ ! -d "$parent" || -x "$parent" ]] || fail 'could not inspect a tracked directory'
    remaining=${remaining#*/}
  done
  if [[ "$linked" == true || -L "$path" ]]; then continue; fi
  # Unstaged deletions have no current content to inspect.
  [[ -e "$path" ]] || continue
  [[ -f "$path" ]] || fail 'a tracked path is not a regular file'

  # -I excludes files grep classifies as binary. Equals-only Markdown rules
  # and inline examples are not markers: only seven chevrons then a space
  # at the beginning of a line are rejected. Never print matching content.
  result=0
  grep -I -q -E '^(<{7}|>{7}) ' -- "$path" 2>"$work_dir/errors" || result=$?
  [[ ! -s "$work_dir/errors" ]] || fail 'could not read a tracked file'
  case "$result" in
    0)
      if [[ "$found" == false ]]; then
        printf 'conflict marker check: conflict markers found in tracked files:\n' >&2
      fi
      printf '  %q\n' "$path" >&2
      found=true
      ;;
    1) ;;
    *) fail 'could not read a tracked file' ;;
  esac
done <"$work_dir/tracked"

[[ "$found" == false ]] || exit 1
printf 'conflict marker check: PASS\n'
