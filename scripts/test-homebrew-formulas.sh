#!/usr/bin/env bash
set -euo pipefail

repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
if (( $# != 0 && $# != 2 )); then
  echo "usage: test-homebrew-formulas.sh [DIST_DIR VERSION]" >&2
  exit 2
fi
for dependency in brew go git ruby tar; do
  if ! command -v "$dependency" >/dev/null 2>&1; then
    echo "error: $dependency is required" >&2
    exit 1
  fi
done

work_dir=$(mktemp -d "${TMPDIR:-/tmp}/witself-homebrew-formula-test.XXXXXX")
tap_name="witself-ci/release-formula-$$"
tap_created=false

cleanup() {
  if [[ $tap_created == true ]]; then
    HOMEBREW_NO_AUTO_UPDATE=1 brew untap --force "$tap_name" >/dev/null 2>&1 || true
  fi
  rm -rf -- "$work_dir"
}
trap cleanup EXIT

if brew tap | grep -Fqx "$tap_name"; then
  echo "error: temporary test tap already exists: $tap_name" >&2
  exit 1
fi

version=${2:-1.2.3}
dist_dir=${1:-$work_dir/dist}
output_dir="$work_dir/rendered"
if (( $# == 0 )); then
  mkdir -p "$dist_dir"
  for binary in witself witself-admin witself-infra; do
    for target in darwin_amd64 darwin_arm64 linux_amd64 linux_arm64; do
      printf 'fixture:%s:%s\n' "$binary" "$target" \
        > "$dist_dir/${binary}_${version}_${target}.tar.gz"
    done
  done
else
  # Formula installation expects each GoReleaser archive to place its binary at
  # the archive root. Checking only names and hashes would miss a future
  # wrap_in_directory or binary rename that renders a valid but broken formula.
  for binary in witself witself-admin witself-infra; do
    for target in darwin_amd64 darwin_arm64 linux_amd64 linux_arm64; do
      archive="$dist_dir/${binary}_${version}_${target}.tar.gz"
      [[ -f $archive ]] \
        || { echo "error: release archive is missing: $archive" >&2; exit 1; }
      root_binary_count=$(tar -tzf "$archive" \
        | sed 's#^\./##' \
        | grep -Fxc "$binary" || true)
      [[ $root_binary_count == 1 ]] \
        || { echo "error: $archive must contain exactly one root-level $binary" >&2; exit 1; }
    done
  done
fi

(
  cd "$repo_root"
  go run ./tools/homebrew-formula \
    --version "$version" \
    --dist "$dist_dir" \
    --output "$output_dir"
)

for formula in "$output_dir"/Formula/*.rb; do
  ruby -c "$formula" >/dev/null
done

HOMEBREW_NO_AUTO_UPDATE=1 brew tap-new --no-git "$tap_name" >/dev/null
tap_created=true
tap_dir=$(brew --repo "$tap_name")
install -m 0644 "$output_dir"/Formula/*.rb "$tap_dir/Formula/"

HOMEBREW_NO_AUTO_UPDATE=1 brew style --display-cop-names \
  "$tap_dir/Formula/witself.rb" \
  "$tap_dir/Formula/witself-admin.rb" \
  "$tap_dir/Formula/witself-infra.rb"

HOMEBREW_NO_AUTO_UPDATE=1 brew audit --strict --formula \
  "$tap_name/witself" \
  "$tap_name/witself-admin" \
  "$tap_name/witself-infra"

echo "Homebrew formula render, style, and strict audit tests passed"
