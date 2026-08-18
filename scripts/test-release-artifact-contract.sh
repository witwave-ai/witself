#!/usr/bin/env bash
set -euo pipefail

repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
for dependency in go jq tar unzip zip; do
  command -v "$dependency" >/dev/null 2>&1 || {
    echo "error: $dependency is required" >&2
    exit 1
  }
done

work_dir=$(mktemp -d "${TMPDIR:-/tmp}/witself-release-contract-test.XXXXXX")
cleanup() {
  rm -rf -- "$work_dir"
}
trap cleanup EXIT

dist_dir="$work_dir/dist"
package_dir="$work_dir/package"
artifact_rows="$work_dir/artifacts.jsonl"
mkdir -p "$dist_dir" "$package_dir"

version=9.8.7
full_commit=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
native_goos=$(go env GOOS)
native_goarch=$(go env GOARCH)
case "$native_goos/$native_goarch" in
  darwin/amd64 | darwin/arm64 | linux/amd64 | linux/arm64) ;;
  *)
    echo "error: unsupported native release-contract target: $native_goos/$native_goarch" >&2
    exit 1
    ;;
esac

sha256_file() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | awk '{print $1}'
  elif command -v shasum >/dev/null 2>&1; then
    shasum -a 256 "$1" | awk '{print $1}'
  else
    echo "error: sha256sum or shasum is required" >&2
    return 1
  fi
}

write_fixture_binary() {
  local path=$1
  local binary=$2
  if [[ $binary == witself-control-plane ]]; then
    # These parameter expansions belong to the generated fixture executable.
    # shellcheck disable=SC2016
    printf '%s\n' \
      '#!/bin/sh' \
      'if [ "${1:-}" = version ]; then' \
      "  echo 'witself-control-plane $version (commit $full_commit, built 2026-08-18T00:00:00Z)'" \
      '  exit 0' \
      'fi' \
      'if [ "${1:-}" = billing-rollout-inventory ] && {' \
      '   [ "${2:-}" = scan ] || [ "${2:-}" = finalize ];' \
      '}; then' \
      "  echo 'witself-control-plane: billing rollout inventory: billing rollout inventory arguments are incomplete' >&2" \
      '  exit 1' \
      'fi' \
      'exit 2' >"$path"
  else
    printf '%s\n' '#!/bin/sh' 'exit 0' >"$path"
  fi
  chmod 0755 "$path"
}

add_archive() {
  local product=$1
  local goos=$2
  local goarch=$3
  local format=$4
  local binary=$product
  local archive_name
  if [[ $format == zip ]]; then
    binary=witself.exe
    archive_name="${product}_${version}_${goos}_${goarch}.zip"
  else
    archive_name="${product}_${version}_${goos}_${goarch}.tar.gz"
  fi

  rm -f -- "$package_dir/$binary"
  write_fixture_binary "$package_dir/$binary" "$product"
  if [[ $format == zip ]]; then
    (cd "$package_dir" && zip -q "$dist_dir/$archive_name" "$binary")
  else
    tar -czf "$dist_dir/$archive_name" -C "$package_dir" "$binary"
  fi

  jq -cn \
    --arg name "$archive_name" \
    --arg goos "$goos" \
    --arg goarch "$goarch" \
    --arg id "$product" \
    '{type:"Archive", name:$name, goos:$goos, goarch:$goarch, extra:{ID:$id}}' \
    >>"$artifact_rows"

  jq -n \
    --arg archive "$archive_name" \
    --arg binary "$binary" \
    '{
      spdxVersion:"SPDX-2.3",
      name:$archive,
      files:[{fileName:$binary}]
    }' >"$dist_dir/$archive_name.sbom.json"
  jq -cn \
    --arg name "$archive_name.sbom.json" \
    '{type:"SBOM", name:$name, extra:{ID:"archive"}}' \
    >>"$artifact_rows"
}

for target in darwin_amd64 darwin_arm64 linux_amd64 linux_arm64; do
  add_archive witself "${target%_*}" "${target#*_}" tar.gz
done
add_archive witself windows amd64 zip
for product in witself-admin witself-control-plane witself-infra witself-server witself-worker; do
  for target in darwin_amd64 darwin_arm64 linux_amd64 linux_arm64; do
    add_archive "$product" "${target%_*}" "${target#*_}" tar.gz
  done
done

jq -s '.' "$artifact_rows" >"$dist_dir/artifacts.json"
printf '%s\n' fixture >"$dist_dir/checksums.txt.pem"
printf '%s\n' fixture >"$dist_dir/checksums.txt.sig"
printf '%s\n' fixture >"$dist_dir/checksums.txt.sigstore.json"

for payload in "$dist_dir"/*.tar.gz "$dist_dir"/*.zip "$dist_dir"/*.sbom.json; do
  printf '%s  %s\n' "$(sha256_file "$payload")" "$(basename "$payload")"
done | LC_ALL=C sort -k2 >"$dist_dir/checksums.txt"

output=$(bash "$repo_root/scripts/verify-release-artifact-contract.sh" \
  local "$dist_dir" "$version" "$full_commit")
[[ $output == *'25 archives, 25 SBOMs, 50 checksum entries, 54 public assets'* ]] || {
  echo "error: complete fixture did not report the exact release count contract" >&2
  exit 1
}

cp "$dist_dir/artifacts.json" "$work_dir/artifacts.good.json"
jq '. + [.[0]]' "$work_dir/artifacts.good.json" >"$dist_dir/artifacts.json"
if bash "$repo_root/scripts/verify-release-artifact-contract.sh" \
  local "$dist_dir" "$version" "$full_commit" >/dev/null 2>&1; then
  echo "error: duplicated archive metadata passed the release contract" >&2
  exit 1
fi
cp "$work_dir/artifacts.good.json" "$dist_dir/artifacts.json"

cp "$dist_dir/checksums.txt" "$work_dir/checksums.good.txt"
sed '$d' "$work_dir/checksums.good.txt" >"$dist_dir/checksums.txt"
if bash "$repo_root/scripts/verify-release-artifact-contract.sh" \
  local "$dist_dir" "$version" "$full_commit" >/dev/null 2>&1; then
  echo "error: incomplete checksum inventory passed the release contract" >&2
  exit 1
fi

echo "Release artifact count and archive-shape contract tests passed"
