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
    rm -rf -- "$work_dir"
  fi
}
trap cleanup EXIT INT TERM

home="$work_dir/home"
install_dir="$work_dir/installed"
package_dir="$work_dir/package"
good_release="$work_dir/release-good"
bad_release="$work_dir/release-bad"
mkdir -p "$home" "$package_dir" "$good_release" "$bad_release"

good_version="9.9.9-install-smoke"
good_asset="witself_${good_version}_${goos}_${goarch}.tar.gz"
good_binary="$package_dir/witself"
(
  cd "$repo_root"
  go build -trimpath \
    -ldflags "-X github.com/witwave-ai/witself/internal/version.Version=${good_version} -X github.com/witwave-ai/witself/internal/version.Commit=installer-smoke -X github.com/witwave-ai/witself/internal/version.Date=synthetic" \
    -o "$good_binary" ./cmd/witself
)
tar -czf "$good_release/$good_asset" -C "$package_dir" witself
printf '%s  %s\n' "$(sha256_file "$good_release/$good_asset")" "$good_asset" \
  > "$good_release/checksums.txt"

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
[[ $primary_version == "witself $good_version "* ]] \
  || fail "installed witself reported unexpected version: $primary_version"
[[ $alias_version == "$primary_version" ]] \
  || fail "ws reported a different version: $alias_version"

primary_before=$(sha256_file "$primary")
alias_target_before=$(readlink "$alias_path")

bad_version="9.9.10-install-smoke"
bad_asset="witself_${bad_version}_${goos}_${goarch}.tar.gz"
bad_package="$work_dir/package-bad"
mkdir -p "$bad_package"
cp "$good_binary" "$bad_package/witself"
printf '\nchecksum-mismatch-fixture\n' >> "$bad_package/witself"
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

printf 'macOS/Linux installer artifact smoke passed for %s/%s\n' "$goos" "$goarch"
