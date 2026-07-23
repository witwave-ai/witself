#!/usr/bin/env bash
set -euo pipefail

fail() {
  printf 'installer smoke: %s\n' "$1" >&2
  exit 1
}

sha256_file() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | awk '{print $1}'
  elif command -v shasum >/dev/null 2>&1; then
    shasum -a 256 "$1" | awk '{print $1}'
  else
    fail "need sha256sum or shasum"
  fi
}

script_dir=$(CDPATH='' cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
repo_root=$(CDPATH='' cd -- "$script_dir/.." && pwd)
installer="$repo_root/install.sh"

goos=$(go env GOOS)
goarch=$(go env GOARCH)
case "$goos/$goarch" in
  darwin/amd64 | darwin/arm64 | linux/amd64 | linux/arm64) ;;
  *) fail "unsupported native target $goos/$goarch" ;;
esac

work_dir=$(mktemp -d "${TMPDIR:-/tmp}/witself-install-smoke.XXXXXX")
cleanup() {
  if [[ -n ${work_dir:-} && -d $work_dir ]]; then
    if [[ -n ${WITSELF_INSTALL_SMOKE_KEEP_DIR:-} && ${WITSELF_INSTALL_SMOKE_KEEP_DIR:-} != 0 ]]; then
      printf 'installer smoke: kept work directory %s\n' "$work_dir" >&2
    else
      rm -rf -- "$work_dir"
    fi
  fi
}
trap cleanup EXIT INT TERM

home="$work_dir/home"
install_dir="$work_dir/installed"
package_dir="$work_dir/package"
good_release="$work_dir/release-good"
bad_release="$work_dir/release-bad"
wrong_version_release="$work_dir/release-wrong-version"
nonrunning_release="$work_dir/release-nonrunning"
rollback_release="$work_dir/release-rollback"
archive_shape_release="$work_dir/release-archive-shape"
mkdir -p \
  "$home" "$package_dir" "$good_release" "$bad_release" \
  "$wrong_version_release" "$nonrunning_release" "$rollback_release" \
  "$archive_shape_release"

good_version="9.9.9-install-smoke"
good_binary="$package_dir/witself"
goreleaser_dist=${WITSELF_INSTALL_SMOKE_GORELEASER_DIST:-}
if [[ -n $goreleaser_dist && -n ${WITSELF_INSTALL_SMOKE_BINARY:-} ]]; then
  fail "set only one of WITSELF_INSTALL_SMOKE_GORELEASER_DIST and WITSELF_INSTALL_SMOKE_BINARY"
fi

if [[ -n $goreleaser_dist ]]; then
  command -v jq >/dev/null 2>&1 \
    || fail "jq is required for the exact GoReleaser artifact smoke"
  [[ -d $goreleaser_dist ]] \
    || fail "WITSELF_INSTALL_SMOKE_GORELEASER_DIST is not a directory: $goreleaser_dist"
  goreleaser_dist=$(CDPATH='' cd -P "$goreleaser_dist" && pwd) \
    || fail "could not resolve GoReleaser dist directory: $goreleaser_dist"
  [[ -f $goreleaser_dist/artifacts.json ]] \
    || fail "GoReleaser artifacts.json is missing from $goreleaser_dist"
  [[ -f $goreleaser_dist/checksums.txt ]] \
    || fail "GoReleaser checksums.txt is missing from $goreleaser_dist"
  jq -e \
    '[.[] | select(.type == "Checksum" and .name == "checksums.txt")] | length == 1' \
    "$goreleaser_dist/artifacts.json" >/dev/null \
    || fail "GoReleaser metadata does not contain exactly one checksums.txt artifact"

  archive_metadata=$(
    jq -er \
      --arg goos "$goos" \
      --arg goarch "$goarch" \
      '
        [
          .[]
          | select(
              .type == "Archive"
              and .goos == $goos
              and .goarch == $goarch
              and .extra.ID == "witself"
              and .extra.Format == "tar.gz"
              and .extra.Binaries == ["witself"]
              and (.extra.Files | length) == 0
            )
        ]
        | if length == 1 then .[0] else error("expected exactly one native witself archive") end
        | [.name, .extra.Checksum]
        | @tsv
      ' \
      "$goreleaser_dist/artifacts.json"
  ) || fail "GoReleaser metadata does not describe one exact native witself-only archive"
  IFS=$'\t' read -r good_asset manifest_checksum <<<"$archive_metadata"

  asset_prefix="witself_"
  asset_suffix="_${goos}_${goarch}.tar.gz"
  case "$good_asset" in
    "${asset_prefix}"*"${asset_suffix}") ;;
    *) fail "GoReleaser produced an unexpected native archive name: $good_asset" ;;
  esac
  good_version=${good_asset#"$asset_prefix"}
  good_version=${good_version%"$asset_suffix"}
  case "$good_version" in
    "" | *[!A-Za-z0-9._+-]*)
      fail "GoReleaser archive contains an invalid version: $good_asset"
      ;;
  esac

  good_archive="$goreleaser_dist/$good_asset"
  [[ -f $good_archive ]] \
    || fail "GoReleaser native archive is missing: $good_archive"
  checksum_matches=$(
    awk -v asset="$good_asset" 'NF == 2 && $2 == asset { print $1 }' \
      "$goreleaser_dist/checksums.txt"
  )
  [[ $(printf '%s\n' "$checksum_matches" | awk 'NF { count++ } END { print count + 0 }') -eq 1 ]] \
    || fail "GoReleaser checksums.txt must contain exactly one entry for $good_asset"
  archive_sha=$(sha256_file "$good_archive")
  [[ $checksum_matches == "$archive_sha" ]] \
    || fail "GoReleaser checksum does not match $good_asset"
  [[ $manifest_checksum == "sha256:$archive_sha" ]] \
    || fail "GoReleaser artifact metadata checksum does not match $good_asset"

  archive_members=$(tar -tzf "$good_archive") \
    || fail "could not list the GoReleaser native archive"
  [[ $archive_members == witself ]] \
    || fail "GoReleaser native archive must contain exactly one root witself executable"
  tar -xzf "$good_archive" -C "$package_dir" \
    || fail "could not extract the GoReleaser native archive"
  [[ -f $good_binary && ! -L $good_binary ]] \
    || fail "GoReleaser native archive did not extract one regular witself executable"
  good_release=$goreleaser_dist
elif [[ -n ${WITSELF_INSTALL_SMOKE_BINARY:-} ]]; then
  [[ -f $WITSELF_INSTALL_SMOKE_BINARY ]] \
    || fail "WITSELF_INSTALL_SMOKE_BINARY is not a file: $WITSELF_INSTALL_SMOKE_BINARY"
  cp -- "$WITSELF_INSTALL_SMOKE_BINARY" "$good_binary"
  chmod +x "$good_binary"
else
  (
    cd "$repo_root"
    go build -trimpath \
      -ldflags "-X github.com/witwave-ai/witself/internal/version.Version=${good_version} -X github.com/witwave-ai/witself/internal/version.Commit=installer-smoke -X github.com/witwave-ai/witself/internal/version.Date=synthetic" \
      -o "$good_binary" ./cmd/witself
  )
fi

expected_version=$("$good_binary" version) \
  || fail "the installer smoke binary failed its version command"
[[ $(printf '%s\n' "$expected_version" | wc -l | tr -d '[:space:]') == 1 ]] \
  || fail "the installer smoke binary returned multiline version output"
reported_program=$(printf '%s\n' "$expected_version" | awk '{print $1}')
reported_version=$(printf '%s\n' "$expected_version" | awk '{print $2}')
[[ $reported_program == witself && -n $reported_version ]] \
  || fail "the installer smoke binary returned an unexpected version format"
if [[ -n $goreleaser_dist ]]; then
  [[ $reported_version == "$good_version" ]] \
    || fail "GoReleaser archive version $good_version does not match binary version $reported_version"
elif [[ -n ${WITSELF_INSTALL_SMOKE_BINARY:-} ]]; then
  # GoReleaser snapshot builds report their generated snapshot version. Package
  # the exact binary beneath that matching synthetic tag so the installer can
  # enforce the same version invariant as a real tagged release.
  good_version=$reported_version
elif [[ $reported_version != "$good_version" ]]; then
  fail "the stamped installer smoke binary reported $reported_version, want $good_version"
fi
good_asset="witself_${good_version}_${goos}_${goarch}.tar.gz"
expected_binary_sha=$(sha256_file "$good_binary")
if [[ -z $goreleaser_dist ]]; then
  tar -czf "$good_release/$good_asset" -C "$package_dir" witself
  printf '%s  %s\n' "$(sha256_file "$good_release/$good_asset")" "$good_asset" \
    > "$good_release/checksums.txt"
fi

HOME="$home" \
  WS_INSTALL_DIR="$install_dir" \
  WS_RELEASE_DIR="$good_release" \
  sh "$installer" witself "v$good_version"

primary="$install_dir/witself"
alias_path="$install_dir/ws"
[[ -x $primary ]] || fail "installer did not create an executable witself"
[[ -L $alias_path ]] || fail "installer did not create the ws symlink"
[[ $(readlink "$alias_path") == witself ]] || fail "ws does not target witself"

primary_version=$("$primary" version)
alias_version=$("$alias_path" version)
[[ $primary_version == "$expected_version" ]] \
  || fail "installed witself reported an unexpected version: $primary_version"
[[ $alias_version == "$primary_version" ]] \
  || fail "ws reported a different version: $alias_version"
[[ $(sha256_file "$primary") == "$expected_binary_sha" ]] \
  || fail "installer did not preserve the exact smoke binary bytes"

primary_before=$(sha256_file "$primary")
alias_target_before=$(readlink "$alias_path")

# Reinstalling an installer-owned binary and alias is an atomic replacement.
HOME="$home" \
  WS_INSTALL_DIR="$install_dir" \
  WS_RELEASE_DIR="$good_release" \
  sh "$installer" witself "v$good_version"
[[ $(sha256_file "$primary") == "$primary_before" ]] \
  || fail "reinstall changed the expected witself bytes"
[[ -L $alias_path && $(readlink "$alias_path") == witself ]] \
  || fail "reinstall did not repair the ws alias"
[[ $("$primary" version) == "$primary_version" ]] \
  || fail "reinstalled witself reported a different version"
alias_target_before=$(readlink "$alias_path")

# Even a correctly checksummed archive is untrusted until its complete member
# list is proven to be the one documented root binary. Extra members, traversal
# paths, links, and nested payloads must be rejected before extraction.
archive_shape_version="9.9.15-install-smoke"
archive_shape_asset="witself_${archive_shape_version}_${goos}_${goarch}.tar.gz"
archive_shape_package="$work_dir/package-archive-shape"
mkdir -p "$archive_shape_package"
cp -- "$good_binary" "$archive_shape_package/witself"
printf '%s\n' 'unexpected archive member' > "$archive_shape_package/unexpected.txt"
tar -czf "$archive_shape_release/$archive_shape_asset" \
  -C "$archive_shape_package" witself unexpected.txt
printf '%s  %s\n' \
  "$(sha256_file "$archive_shape_release/$archive_shape_asset")" \
  "$archive_shape_asset" > "$archive_shape_release/checksums.txt"
set +e
HOME="$home" \
  WS_INSTALL_DIR="$install_dir" \
  WS_RELEASE_DIR="$archive_shape_release" \
  sh "$installer" witself "v$archive_shape_version" >"$work_dir/archive-shape.log" 2>&1
archive_shape_status=$?
set -e
[[ $archive_shape_status -ne 0 ]] \
  || fail "installer accepted an archive with an unexpected extra member"
grep -F "must contain exactly one regular root entry named witself" \
  "$work_dir/archive-shape.log" >/dev/null \
  || fail "installer did not report the invalid archive shape"
[[ $(sha256_file "$primary") == "$primary_before" ]] \
  || fail "archive-shape rejection changed the installed witself binary"
[[ -L $alias_path && $(readlink "$alias_path") == "$alias_target_before" ]] \
  || fail "archive-shape rejection changed the ws alias"

archive_link_version="9.9.16-install-smoke"
archive_link_asset="witself_${archive_link_version}_${goos}_${goarch}.tar.gz"
archive_link_package="$work_dir/package-archive-link"
mkdir -p "$archive_link_package"
ln -s /tmp/not-a-release-binary "$archive_link_package/witself"
tar -czf "$archive_shape_release/$archive_link_asset" \
  -C "$archive_link_package" witself
printf '%s  %s\n' \
  "$(sha256_file "$archive_shape_release/$archive_link_asset")" \
  "$archive_link_asset" > "$archive_shape_release/checksums.txt"
set +e
HOME="$home" \
  WS_INSTALL_DIR="$install_dir" \
  WS_RELEASE_DIR="$archive_shape_release" \
  sh "$installer" witself "v$archive_link_version" >"$work_dir/archive-link.log" 2>&1
archive_link_status=$?
set -e
[[ $archive_link_status -ne 0 ]] \
  || fail "installer accepted a linked release archive member"
grep -F "must contain exactly one regular root entry named witself" \
  "$work_dir/archive-link.log" >/dev/null \
  || fail "installer did not report the linked archive member"
[[ $(sha256_file "$primary") == "$primary_before" ]] \
  || fail "linked-archive rejection changed the installed witself binary"
[[ -L $alias_path && $(readlink "$alias_path") == "$alias_target_before" ]] \
  || fail "linked-archive rejection changed the ws alias"

# A foreign ws symlink is not installer-owned and must never be overwritten.
ln -sfn "foreign-command" "$alias_path"
set +e
HOME="$home" \
  WS_INSTALL_DIR="$install_dir" \
  WS_RELEASE_DIR="$good_release" \
  sh "$installer" witself "v$good_version" >"$work_dir/alias-collision.log" 2>&1
alias_collision_status=$?
set -e
[[ $alias_collision_status -ne 0 ]] \
  || fail "installer replaced a foreign ws alias"
grep -F "refusing to replace non-Witself alias" "$work_dir/alias-collision.log" >/dev/null \
  || fail "installer did not report the foreign ws alias collision"
[[ $(sha256_file "$primary") == "$primary_before" ]] \
  || fail "foreign alias collision changed the installed witself binary"
[[ -L $alias_path && $(readlink "$alias_path") == foreign-command ]] \
  || fail "foreign ws alias was not preserved"
ln -sfn "witself" "$alias_path"
alias_target_before=$(readlink "$alias_path")

bad_version="9.9.10-install-smoke"
bad_asset="witself_${bad_version}_${goos}_${goarch}.tar.gz"
bad_package="$work_dir/package-bad"
mkdir -p "$bad_package"
cp "$good_binary" "$bad_package/witself"
tar -czf "$bad_release/$bad_asset" -C "$bad_package" witself
printf '%064d  %s\n' 0 "$bad_asset" > "$bad_release/checksums.txt"

set +e
HOME="$home" \
  WS_INSTALL_DIR="$install_dir" \
  WS_RELEASE_DIR="$bad_release" \
  sh "$installer" witself "v$bad_version" >"$work_dir/checksum-mismatch.log" 2>&1
bad_status=$?
set -e
[[ $bad_status -ne 0 ]] || fail "installer accepted an invalid checksum"
grep -F "checksum mismatch for $bad_asset" "$work_dir/checksum-mismatch.log" >/dev/null \
  || fail "installer did not report the checksum mismatch"
[[ $(sha256_file "$primary") == "$primary_before" ]] \
  || fail "checksum failure replaced the installed witself binary"
[[ -L $alias_path && $(readlink "$alias_path") == "$alias_target_before" ]] \
  || fail "checksum failure changed the ws alias"
[[ $("$primary" version) == "$primary_version" ]] \
  || fail "installed witself no longer runs after checksum refusal"

make_script_release() {
  local release_dir=$1
  local version=$2
  local source_binary=$3
  local fixture_dir="$work_dir/package-$version"
  local asset="witself_${version}_${goos}_${goarch}.tar.gz"

  mkdir -p "$fixture_dir"
  cp -- "$source_binary" "$fixture_dir/witself"
  chmod +x "$fixture_dir/witself"
  tar -czf "$release_dir/$asset" -C "$fixture_dir" witself
  printf '%s  %s\n' "$(sha256_file "$release_dir/$asset")" "$asset" \
    > "$release_dir/checksums.txt"
}

# A runnable archive with a valid checksum still cannot claim a different tag.
# Rejection happens while staged, before either live install path changes.
wrong_version="${good_version}-wrong"
make_script_release "$wrong_version_release" "$wrong_version" "$good_binary"
set +e
HOME="$home" \
  WS_INSTALL_DIR="$install_dir" \
  WS_RELEASE_DIR="$wrong_version_release" \
  sh "$installer" witself "v$wrong_version" >"$work_dir/wrong-version.log" 2>&1
wrong_version_status=$?
set -e
[[ $wrong_version_status -ne 0 ]] \
  || fail "installer accepted a checksummed runnable binary with the wrong version"
grep -F "staged witself reported a version other than requested v$wrong_version" \
  "$work_dir/wrong-version.log" >/dev/null \
  || fail "installer did not report the staged version mismatch"
[[ $(sha256_file "$primary") == "$primary_before" ]] \
  || fail "version mismatch replaced the installed witself binary"
[[ -L $alias_path && $(readlink "$alias_path") == "$alias_target_before" ]] \
  || fail "version mismatch changed the ws alias"
[[ $("$primary" version) == "$primary_version" ]] \
  || fail "installed witself no longer runs after version mismatch refusal"

# A correctly checksummed archive is still rejected if its staged executable
# cannot run. The live binary and alias must remain untouched.
nonrunning_version="9.9.11-install-smoke"
nonrunning_binary="$work_dir/nonrunning-witself"
printf '%s\n' '#!/bin/sh' 'exit 74' > "$nonrunning_binary"
chmod +x "$nonrunning_binary"
make_script_release "$nonrunning_release" "$nonrunning_version" "$nonrunning_binary"

set +e
HOME="$home" \
  WS_INSTALL_DIR="$install_dir" \
  WS_RELEASE_DIR="$nonrunning_release" \
  sh "$installer" witself "v$nonrunning_version" >"$work_dir/nonrunning.log" 2>&1
nonrunning_status=$?
set -e
[[ $nonrunning_status -ne 0 ]] \
  || fail "installer accepted a checksummed binary that could not run"
grep -F "staged witself failed to run" "$work_dir/nonrunning.log" >/dev/null \
  || fail "installer did not report the staged runtime failure"
[[ $(sha256_file "$primary") == "$primary_before" ]] \
  || fail "staged runtime failure replaced the installed witself binary"
[[ -L $alias_path && $(readlink "$alias_path") == "$alias_target_before" ]] \
  || fail "staged runtime failure changed the ws alias"
[[ $("$primary" version) == "$primary_version" ]] \
  || fail "installed witself no longer runs after staged runtime refusal"

# This fixture succeeds while staged, then fails from the committed path. It
# proves the post-commit check restores both the verified binary and ws alias.
rollback_version="9.9.12-install-smoke"
rollback_binary="$work_dir/rollback-witself"
rollback_counter="$work_dir/rollback-counter"
rollback_postcommit="$work_dir/rollback-postcommit"
printf '%s\n' \
  '#!/bin/sh' \
  "counter=\${WS_INSTALL_ROLLBACK_COUNTER:?}" \
  "postcommit=\${WS_INSTALL_ROLLBACK_POSTCOMMIT:?}" \
  "concurrent_alias=\${WS_INSTALL_ROLLBACK_CONCURRENT_ALIAS:-}" \
  "if [ -e \"\$counter\" ]; then" \
  "  if [ -n \"\$concurrent_alias\" ]; then" \
  "    rm -f -- \"\$concurrent_alias\"" \
  "    printf '%s\\n' 'later writer bytes' > \"\$concurrent_alias\"" \
  '  fi' \
  "  : > \"\$postcommit\"" \
  '  exit 75' \
  'fi' \
  ": > \"\$counter\"" \
  "printf '%s\\n' 'witself ${rollback_version} (commit installer-smoke, built synthetic)'" \
  > "$rollback_binary"
chmod +x "$rollback_binary"
make_script_release "$rollback_release" "$rollback_version" "$rollback_binary"

set +e
HOME="$home" \
  WS_INSTALL_DIR="$install_dir" \
  WS_RELEASE_DIR="$rollback_release" \
  WS_INSTALL_ROLLBACK_COUNTER="$rollback_counter" \
  WS_INSTALL_ROLLBACK_POSTCOMMIT="$rollback_postcommit" \
  sh "$installer" witself "v$rollback_version" >"$work_dir/rollback.log" 2>&1
rollback_status=$?
set -e
[[ $rollback_status -ne 0 ]] \
  || fail "installer accepted a binary that failed after commit"
[[ -f $rollback_counter && -f $rollback_postcommit ]] \
  || fail "rollback fixture did not reach both staged and post-commit checks"
grep -F "failed to run after commit" "$work_dir/rollback.log" >/dev/null \
  || fail "installer did not report the post-commit runtime failure"
grep -F "Restored the previous witself installation." "$work_dir/rollback.log" >/dev/null \
  || fail "installer did not report the successful rollback"
[[ $(sha256_file "$primary") == "$primary_before" ]] \
  || fail "post-commit failure did not restore the prior witself binary"
[[ -L $alias_path && $(readlink "$alias_path") == "$alias_target_before" ]] \
  || fail "post-commit failure did not restore the prior ws alias"
[[ $("$primary" version) == "$primary_version" ]] \
  || fail "restored witself no longer runs after rollback"

# A live lock is never stolen. Timeout zero makes this deterministic while the
# default installer behavior waits briefly for a concurrent transaction.
lock_dir="$install_dir/.witself-install.lock"
mkdir "$lock_dir"
printf '%s %s\n' "$$" "$(id -u)" > "$lock_dir/owner"
set +e
HOME="$home" \
  WS_INSTALL_DIR="$install_dir" \
  WS_RELEASE_DIR="$good_release" \
  WS_INSTALL_LOCK_TIMEOUT=0 \
  sh "$installer" witself "v$good_version" >"$work_dir/lock-contention.log" 2>&1
lock_status=$?
set -e
[[ $lock_status -ne 0 ]] || fail "installer ignored a live install lock"
grep -F "another Witself installation is in progress" "$work_dir/lock-contention.log" >/dev/null \
  || fail "installer did not report lock contention"
[[ -d $lock_dir && -f $lock_dir/owner ]] \
  || fail "installer removed another process's live lock"
[[ $(sha256_file "$primary") == "$primary_before" ]] \
  || fail "lock contention changed the installed witself binary"
[[ -L $alias_path && $(readlink "$alias_path") == "$alias_target_before" ]] \
  || fail "lock contention changed the ws alias"
rm -f -- "$lock_dir/owner"
rmdir -- "$lock_dir"

if find "$install_dir" -maxdepth 1 \
  \( -name '.witself-install.*' -o -name '.witself-install.lock' \) \
  -print -quit | grep -q .; then
  fail "installer left a transaction or lock artifact behind"
fi

if [[ -n ${WITSELF_INSTALL_SMOKE_OUTPUT:-} ]]; then
  output_dir=$(dirname -- "$WITSELF_INSTALL_SMOKE_OUTPUT")
  mkdir -p -- "$output_dir"
  cp -- "$primary" "$WITSELF_INSTALL_SMOKE_OUTPUT"
  chmod +x "$WITSELF_INSTALL_SMOKE_OUTPUT"
  [[ $(sha256_file "$WITSELF_INSTALL_SMOKE_OUTPUT") == "$primary_before" ]] \
    || fail "copied smoke output differs from the verified installed binary"
fi

# If another process replaces an installed path after commit but before the
# installer's post-commit check finishes, rollback must not overwrite those
# later bytes. The verified prior binary remains recoverable and the transaction
# artifacts remain available for explicit inspection.
concurrent_counter="$work_dir/concurrent-rollback-counter"
concurrent_postcommit="$work_dir/concurrent-rollback-postcommit"
set +e
HOME="$home" \
  WS_INSTALL_DIR="$install_dir" \
  WS_RELEASE_DIR="$rollback_release" \
  WS_INSTALL_ROLLBACK_COUNTER="$concurrent_counter" \
  WS_INSTALL_ROLLBACK_POSTCOMMIT="$concurrent_postcommit" \
  WS_INSTALL_ROLLBACK_CONCURRENT_ALIAS="$alias_path" \
  sh "$installer" witself "v$rollback_version" >"$work_dir/concurrent-rollback.log" 2>&1
concurrent_status=$?
set -e
[[ $concurrent_status -ne 0 ]] \
  || fail "installer accepted a binary that failed after a concurrent replacement"
[[ -f $concurrent_counter && -f $concurrent_postcommit ]] \
  || fail "concurrent rollback fixture did not reach both runtime checks"
grep -F "automatic rollback of witself did not complete" \
  "$work_dir/concurrent-rollback.log" >/dev/null \
  || fail "installer did not report the unsafe automatic rollback refusal"
grep -F "Transaction recovery artifacts remain" \
  "$work_dir/concurrent-rollback.log" >/dev/null \
  || fail "installer did not report the preserved recovery transaction"
[[ -f $alias_path && ! -L $alias_path ]] \
  || fail "rollback did not preserve the concurrently replaced ws path"
[[ $(<"$alias_path") == "later writer bytes" ]] \
  || fail "rollback changed the concurrent writer's ws bytes"
[[ $(sha256_file "$primary") == "$primary_before" ]] \
  || fail "concurrent alias replacement prevented safe primary rollback"
[[ $("$primary" version) == "$primary_version" ]] \
  || fail "primary no longer runs after concurrent alias rollback refusal"
find "$install_dir" -maxdepth 1 -type d -name '.witself-install.*' -print -quit \
  | grep -q . \
  || fail "installer removed recovery artifacts after an unsafe rollback refusal"

printf 'macOS/Linux installer artifact smoke passed for %s/%s\n' "$goos" "$goarch"
