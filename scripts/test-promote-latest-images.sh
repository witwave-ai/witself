#!/usr/bin/env bash
set -euo pipefail

repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
work_dir=$(mktemp -d "${TMPDIR:-/tmp}/witself-latest-promotion-test.XXXXXX")
trap 'rm -rf -- "$work_dir"' EXIT

cli_digest=sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
server_digest=sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
old_cli_digest=sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd
old_server_digest=sha256:eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee
mock_bin="$work_dir/mock-bin"
mkdir -p "$mock_bin"
ln -s "$repo_root/scripts/testdata/homebrew-mock-docker.sh" "$mock_bin/docker"

make_tap() {
  local label=$1 witself_version=$2 admin_version=$3 infra_version=$4
  local seed="$work_dir/$label-seed"
  local remote="$work_dir/$label.git"

  git init --quiet --initial-branch=main "$seed"
  mkdir -p "$seed/Formula"
  write_formula "$seed" witself Witself "$witself_version"
  write_formula "$seed" witself-admin WitselfAdmin "$admin_version"
  write_formula "$seed" witself-infra WitselfInfra "$infra_version"
  git -C "$seed" add Formula
  git -C "$seed" -c user.name=test -c user.email=test@example.invalid \
    commit --quiet -m seed
  git clone --quiet --bare "$seed" "$remote"
  printf '%s\n' "$remote"
}

write_formula() {
  local directory=$1 name=$2 class_name=$3 formula_version=$4
  [[ $formula_version != missing ]] || return
  if [[ $formula_version == duplicate ]]; then
    printf '%s\n' \
      "class $class_name < Formula" \
      '  version "1.2.3"' \
      '  version ""' \
      'end' > "$directory/Formula/$name.rb"
    return
  fi
  printf '%s\n' \
    "class $class_name < Formula" \
    "  version \"$formula_version\"" \
    'end' > "$directory/Formula/$name.rb"
}

run_promoter() {
  local remote=$1 state_dir=$2 docker_log=$3
  shift 3
  WITSELF_TEST_DOCKER_STATE_DIR="$state_dir" \
  WITSELF_TEST_DOCKER_LOG="$docker_log" \
  WITSELF_TEST_DOCKER_VERSION=1.2.3 \
  WITSELF_TEST_WITSELF_DIGEST="$cli_digest" \
  WITSELF_TEST_SERVER_DIGEST="$server_digest" \
  PATH="$mock_bin:$PATH" \
    "$@" bash "$repo_root/scripts/promote-latest-images.sh" \
      "file://$remote" main
}

version=1.2.3
happy_remote=$(make_tap happy "$version" "$version" "$version")
happy_state="$work_dir/happy-state"
happy_log="$work_dir/happy-docker.log"
mkdir -p "$happy_state"
printf '%s\n' "$old_cli_digest" > "$happy_state/witself.latest"
printf '%s\n' "$old_server_digest" > "$happy_state/witself-server.latest"
run_promoter "$happy_remote" "$happy_state" "$happy_log" env

[[ $(<"$happy_state/witself.latest") == "$cli_digest" ]] \
  || { echo "error: witself latest was not promoted" >&2; exit 1; }
[[ $(<"$happy_state/witself-server.latest") == "$server_digest" ]] \
  || { echo "error: witself-server latest was not promoted" >&2; exit 1; }
grep -Fqx \
  "buildx imagetools create --tag ghcr.io/witwave-ai/images/witself:latest ghcr.io/witwave-ai/images/witself@$cli_digest" \
  "$happy_log" \
  || { echo "error: witself was not promoted by digest" >&2; exit 1; }
grep -Fqx \
  "buildx imagetools create --tag ghcr.io/witwave-ai/images/witself-server:latest ghcr.io/witwave-ai/images/witself-server@$server_digest" \
  "$happy_log" \
  || { echo "error: witself-server was not promoted by digest" >&2; exit 1; }

for fixture in missing malformed disagree duplicate; do
  case $fixture in
    missing) remote=$(make_tap "$fixture" "$version" "$version" missing) ;;
    malformed) remote=$(make_tap "$fixture" "$version" invalid "$version") ;;
    disagree) remote=$(make_tap "$fixture" "$version" 1.2.2 "$version") ;;
    duplicate) remote=$(make_tap "$fixture" "$version" duplicate "$version") ;;
  esac
  state="$work_dir/$fixture-state"
  log="$work_dir/$fixture-docker.log"
  mkdir -p "$state"
  if run_promoter "$remote" "$state" "$log" env >/dev/null 2>&1; then
    echo "error: $fixture tap state was accepted" >&2
    exit 1
  fi
  [[ ! -s $log ]] || { echo "error: docker ran for $fixture tap state" >&2; exit 1; }
done

missing_image_remote=$(make_tap missing-image "$version" "$version" "$version")
missing_image_state="$work_dir/missing-image-state"
missing_image_log="$work_dir/missing-image-docker.log"
mkdir -p "$missing_image_state"
printf '%s\n' "$old_cli_digest" > "$missing_image_state/witself.latest"
printf '%s\n' "$old_server_digest" > "$missing_image_state/witself-server.latest"
if run_promoter "$missing_image_remote" "$missing_image_state" \
  "$missing_image_log" env WITSELF_TEST_MISSING_IMAGE=witself-server \
  >/dev/null 2>&1; then
  echo "error: missing immutable image was accepted" >&2
  exit 1
fi
if grep -Fq 'imagetools create' "$missing_image_log"; then
  echo "error: promotion began before every immutable image passed preflight" >&2
  exit 1
fi
[[ $(<"$missing_image_state/witself.latest") == "$old_cli_digest" ]] \
  || { echo "error: failed preflight changed witself latest" >&2; exit 1; }
[[ $(<"$missing_image_state/witself-server.latest") == "$old_server_digest" ]] \
  || { echo "error: failed preflight changed witself-server latest" >&2; exit 1; }

mismatch_remote=$(make_tap mismatch "$version" "$version" "$version")
mismatch_state="$work_dir/mismatch-state"
mismatch_log="$work_dir/mismatch-docker.log"
mismatch_output="$work_dir/mismatch-output.log"
mkdir -p "$mismatch_state"
printf '%s\n' "$old_cli_digest" > "$mismatch_state/witself.latest"
printf '%s\n' "$old_server_digest" > "$mismatch_state/witself-server.latest"
if run_promoter "$mismatch_remote" "$mismatch_state" "$mismatch_log" \
  env WITSELF_TEST_MISMATCH_IMAGE=witself-server >"$mismatch_output" 2>&1; then
  echo "error: mismatched promoted digest was accepted" >&2
  exit 1
fi
grep -Fq 'witself-server:latest resolved to' "$mismatch_output" \
  || { echo "error: digest mismatch did not fail at verification" >&2; exit 1; }

echo "GHCR latest image promotion tests passed"
