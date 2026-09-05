#!/usr/bin/env bash
set -euo pipefail

# Exercise Git's real three-way text merge without creating commits or refs.
# roll-train requires both baseline pins to be below its target and edits both.
# Thus even a newer main pin arriving after the final fetch conflicts with the
# checked PR, instead of being overwritten by GitHub's squash merge.
TEST_ROOT=$(mktemp -d "${TMPDIR:-/tmp}/witself-roll-train-merge-fence.XXXXXX")
cleanup() {
  local status=$?
  trap - EXIT INT TERM
  rm -f "$TEST_ROOT/base" "$TEST_ROOT/train" "$TEST_ROOT/main" "$TEST_ROOT/merged"
  rmdir "$TEST_ROOT"
  exit "$status"
}
trap cleanup EXIT INT TERM

fail() { printf 'roll train merge fence test: FAIL: %s\n' "$*" >&2; exit 1; }
command -v git >/dev/null 2>&1 || fail 'git is required'

write_values() {
  cat <<EOF
apps:
  witselfServer:
    enabled: true
    namespace: witself
    chartVersion: $1
    imageTag: $2
    backendKind: managed
EOF
}

write_values 1.2.2 1.2.2 >"$TEST_ROOT/base"
write_values 1.2.3 1.2.3 >"$TEST_ROOT/train"

assert_conflict() {
  local label=$1 chart=$2 image=$3 status=0
  write_values "$chart" "$image" >"$TEST_ROOT/main"
  git merge-file -p "$TEST_ROOT/train" "$TEST_ROOT/base" "$TEST_ROOT/main" \
    >"$TEST_ROOT/merged" || status=$?
  [ "$status" -eq 1 ] || fail "$label returned $status; expected one conflicting hunk"
  case "$(cat "$TEST_ROOT/merged")" in
    *'<<<<<<< '*'======='*'>>>>>>> '*) ;;
    *) fail "$label did not produce conflict markers" ;;
  esac
}

assert_conflict 'both pins advanced after final fetch' 1.2.4 1.2.4
assert_conflict 'newer chart with identical target image' 1.2.4 1.2.3
assert_conflict 'identical target chart with newer image' 1.2.3 1.2.4
assert_conflict 'chart-only concurrent advancement' 1.2.4 1.2.2
assert_conflict 'image-only concurrent advancement' 1.2.2 1.2.4

# An independent PR may first squash the identical target changes into main.
# A subsequent newer rollout still merges against this train's original base:
# its head is fenced, and its parent remains that exact baseline commit.
assert_conflict 'identical target followed by another newer rollout' 1.3.0 1.3.0

write_values 1.2.3 1.2.3 >"$TEST_ROOT/main"
git merge-file -p "$TEST_ROOT/train" "$TEST_ROOT/base" "$TEST_ROOT/main" \
  >"$TEST_ROOT/merged" || fail 'identical target changes should merge cleanly'
cmp -s "$TEST_ROOT/train" "$TEST_ROOT/merged" || fail 'identical merge changed target pins'

printf 'roll train merge fence tests passed\n'
