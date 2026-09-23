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
# These synthetic expectations are independent of the static test-only report.
export GITHUB_REPOSITORY=witwave-ai/witself
export GITHUB_RUN_ID=100
export GITHUB_RUN_ATTEMPT=1
provider_evidence_name=provider-contract-evidence.json
provider_evidence_fixture="$repo_root/tools/provider-contract-evidence/testdata/valid-release-matrix.json"
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
cp "$provider_evidence_fixture" "$dist_dir/$provider_evidence_name"

for payload in "$dist_dir"/*.tar.gz "$dist_dir"/*.zip "$dist_dir"/*.sbom.json "$dist_dir/$provider_evidence_name"; do
  printf '%s  %s\n' "$(sha256_file "$payload")" "$(basename "$payload")"
done | LC_ALL=C sort -k2 >"$dist_dir/checksums.txt"

output=$(bash "$repo_root/scripts/verify-release-artifact-contract.sh" \
  local "$dist_dir" "$version" "$full_commit")
[[ $output == *'25 archives, 25 SBOMs, 51 checksum entries, 55 public assets'* ]] || {
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
cp "$work_dir/checksums.good.txt" "$dist_dir/checksums.txt"

refusal_count=0
assert_refusal() {
  local label=$1
  local expected=$2
  shift 2
  if "$@" >"$work_dir/refusal.log" 2>&1; then
    echo "error: $label passed the release contract" >&2
    exit 1
  fi
  if ! grep -Fq -- "$expected" "$work_dir/refusal.log"; then
    echo "error: $label failed outside its intended contract boundary" >&2
    cat "$work_dir/refusal.log" >&2
    exit 1
  fi
  refusal_count=$((refusal_count + 1))
}

local_verifier=(bash "$repo_root/scripts/verify-release-artifact-contract.sh"
  local "$dist_dir" "$version" "$full_commit")
for context_name in GITHUB_REPOSITORY GITHUB_RUN_ID GITHUB_RUN_ATTEMPT; do
  assert_refusal "missing trusted $context_name" 'trusted publishing workflow' \
    env -u "$context_name" "${local_verifier[@]}"
done
for mismatch in GITHUB_REPOSITORY=other/repository GITHUB_RUN_ID=101 GITHUB_RUN_ATTEMPT=2; do
  assert_refusal "mismatched trusted ${mismatch%%=*}" 'provider contract evidence did not match' \
    env "$mismatch" "${local_verifier[@]}"
done
assert_refusal 'mismatched publishing version' 'provider contract evidence did not match' \
  bash "$repo_root/scripts/verify-release-artifact-contract.sh" \
  local "$dist_dir" 9.8.8 "$full_commit"
assert_refusal 'mismatched source commit' 'provider contract evidence did not match' \
  bash "$repo_root/scripts/verify-release-artifact-contract.sh" \
  local "$dist_dir" "$version" bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb

rm "$dist_dir/$provider_evidence_name"
assert_refusal 'missing provider evidence' 'nonempty regular file' "${local_verifier[@]}"
: >"$dist_dir/$provider_evidence_name"
assert_refusal 'empty provider evidence' 'nonempty regular file' "${local_verifier[@]}"
rm "$dist_dir/$provider_evidence_name"
ln -s "$provider_evidence_fixture" "$dist_dir/$provider_evidence_name"
assert_refusal 'symlink provider evidence' 'nonempty regular file' "${local_verifier[@]}"
rm "$dist_dir/$provider_evidence_name"
printf '%s\n' '{' >"$dist_dir/$provider_evidence_name"
assert_refusal 'malformed provider evidence' 'provider contract evidence did not match' "${local_verifier[@]}"
jq '. + {unexpected_private_field:"fixture-private-sentinel"}' \
  "$provider_evidence_fixture" >"$dist_dir/$provider_evidence_name"
assert_refusal 'unknown evidence field' 'provider contract evidence did not match' "${local_verifier[@]}"
jq '.cells |= .[1:]' "$provider_evidence_fixture" >"$dist_dir/$provider_evidence_name"
assert_refusal 'incomplete native matrix' 'provider contract evidence did not match' "${local_verifier[@]}"
jq '.identity.workflow = "ci" | .cells[].identity.workflow = "ci"' \
  "$provider_evidence_fixture" >"$dist_dir/$provider_evidence_name"
assert_refusal 'CI evidence used as release evidence' 'provider contract evidence did not match' "${local_verifier[@]}"
jq '.identity.source_ref = "refs/heads/main" | .cells[].identity.source_ref = "refs/heads/main"' \
  "$provider_evidence_fixture" >"$dist_dir/$provider_evidence_name"
assert_refusal 'branch evidence used as tagged evidence' 'provider contract evidence did not match' "${local_verifier[@]}"
cp "$provider_evidence_fixture" "$dist_dir/$provider_evidence_name"
printf '\n' >>"$dist_dir/$provider_evidence_name"
assert_refusal 'changed evidence bytes' 'checksum mismatch for provider-contract-evidence.json' "${local_verifier[@]}"
cp "$provider_evidence_fixture" "$dist_dir/$provider_evidence_name"

awk -v name="$provider_evidence_name" '$2 != name' \
  "$work_dir/checksums.good.txt" >"$dist_dir/checksums.txt"
assert_refusal 'missing evidence checksum' 'exact 51 archive, SBOM and provider evidence payloads' "${local_verifier[@]}"
cp "$work_dir/checksums.good.txt" "$dist_dir/checksums.txt"
awk -v name="$provider_evidence_name" '$2 == name' \
  "$work_dir/checksums.good.txt" >>"$dist_dir/checksums.txt"
assert_refusal 'duplicate evidence checksum' 'exact 51 archive, SBOM and provider evidence payloads' "${local_verifier[@]}"
cp "$work_dir/checksums.good.txt" "$dist_dir/checksums.txt"

# Published-mode fixtures substitute only GitHub and the checkout identity.
# The real shared evidence validator, file checksums and asset comparison run.
fixture_bin="$work_dir/fixture-bin"
mkdir -p "$fixture_bin"
export WITSELF_RELEASE_FIXTURE_REAL_GIT
WITSELF_RELEASE_FIXTURE_REAL_GIT=$(command -v git)
export WITSELF_RELEASE_FIXTURE_COMMIT="$full_commit"
export WITSELF_RELEASE_FIXTURE_TAG="v$version"
export WITSELF_RELEASE_FIXTURE_RESPONSE="$work_dir/published.json"
export WITSELF_RELEASE_FIXTURE_ASSETS="$work_dir/asset-pages.json"
export WITSELF_RELEASE_FIXTURE_ASSET_CALLS="$work_dir/asset-calls"
export WITSELF_RELEASE_FIXTURE_CALLS="$work_dir/github-calls"
export WITSELF_RELEASE_FIXTURE_ATTESTATIONS="$work_dir/attestations"
cat >"$fixture_bin/git" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
if [[ $# == 5 && $1 == -C && $3 == rev-parse && $4 == --verify && $5 == HEAD ]]; then
  printf '%s\n' "$WITSELF_RELEASE_FIXTURE_COMMIT"
else
  exec "$WITSELF_RELEASE_FIXTURE_REAL_GIT" "$@"
fi
EOF
cat >"$fixture_bin/gh" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$1" >>"$WITSELF_RELEASE_FIXTURE_CALLS"
if [[ $# == 4 && $1 == api && $2 == "repos/$GITHUB_REPOSITORY/commits/$WITSELF_RELEASE_FIXTURE_TAG" && $3 == --jq && $4 == .sha ]]; then
  printf '%s\n' "$WITSELF_RELEASE_FIXTURE_COMMIT"
elif [[ $# == 2 && $1 == api && $2 == "repos/$GITHUB_REPOSITORY/releases/tags/$WITSELF_RELEASE_FIXTURE_TAG" ]]; then
  cat "$WITSELF_RELEASE_FIXTURE_RESPONSE"
elif [[ $# == 4 && $1 == api && $2 == --paginate && $3 == --slurp &&
        $4 == "repos/$GITHUB_REPOSITORY/releases/123/assets?per_page=100" ]]; then
  printf '%s\n' assets >>"$WITSELF_RELEASE_FIXTURE_ASSET_CALLS"
  cat "$WITSELF_RELEASE_FIXTURE_ASSETS"
  # A later HTTP page may fail after valid JSON has already been received.
  [[ ${WITSELF_RELEASE_FIXTURE_ASSET_FAILURE:-0} == 0 ]] || exit 1
elif [[ $# == 14 && $1 == attestation && $2 == verify &&
        $4 == --repo && $5 == "$GITHUB_REPOSITORY" &&
        $6 == --signer-workflow && $7 == "$GITHUB_REPOSITORY/.github/workflows/release.yml" &&
        $8 == --signer-digest && $9 == "$WITSELF_RELEASE_FIXTURE_COMMIT" &&
        ${10} == --source-ref && ${11} == "refs/tags/$WITSELF_RELEASE_FIXTURE_TAG" &&
        ${12} == --source-digest && ${13} == "$WITSELF_RELEASE_FIXTURE_COMMIT" &&
        ${14} == --deny-self-hosted-runners ]]; then
  printf '%s\n' "${3##*/}" >>"$WITSELF_RELEASE_FIXTURE_ATTESTATIONS"
else
  echo 'error: unexpected GitHub operation in offline release fixture' >&2
  exit 1
fi
EOF
chmod 0755 "$fixture_bin/git" "$fixture_bin/gh"

{
  jq -r '.[] | select(.type == "Archive" or .type == "SBOM") | .name' "$dist_dir/artifacts.json"
  printf '%s\n' checksums.txt checksums.txt.pem checksums.txt.sig checksums.txt.sigstore.json "$provider_evidence_name"
} | LC_ALL=C sort >"$work_dir/public-asset-names"
while IFS= read -r asset; do
  jq -cn --arg name "$asset" \
    --argjson size "$(wc -c <"$dist_dir/$asset" | tr -d '[:space:]')" \
    --arg digest "sha256:$(sha256_file "$dist_dir/$asset")" \
    '{name:$name,size:$size,digest:$digest,state:"uploaded"}'
done <"$work_dir/public-asset-names" | jq -s --arg tag "v$version" \
  '{id:123,tag_name:$tag,draft:false,prerelease:false,assets:.}' >"$work_dir/published.good.json"
# The embedded list is deliberately stale; only dedicated enumeration is used.
jq '.assets | [.[0:27], .[27:]]' "$work_dir/published.good.json" >"$work_dir/asset-pages.good.json"
jq '.assets = []' "$work_dir/published.good.json" >"$work_dir/release.good.json"
cp "$work_dir/release.good.json" "$WITSELF_RELEASE_FIXTURE_RESPONSE"
cp "$work_dir/asset-pages.good.json" "$WITSELF_RELEASE_FIXTURE_ASSETS"
published_verifier=(env "PATH=$fixture_bin:$PATH"
  bash "$repo_root/scripts/verify-release-artifact-contract.sh"
  published "$dist_dir" "v$version" "$GITHUB_REPOSITORY" "$full_commit")
output=$("${published_verifier[@]}")
[[ $output == *'55 assets and 25 archive provenance subjects'* ]] || {
  echo 'error: complete published fixture did not report the exact release contract' >&2
  exit 1
}
jq -r '.[] | select(.type == "Archive") | .name' "$dist_dir/artifacts.json" \
  | LC_ALL=C sort >"$work_dir/expected-attestations"
LC_ALL=C sort "$WITSELF_RELEASE_FIXTURE_ATTESTATIONS" >"$work_dir/actual-attestations"
cmp "$work_dir/expected-attestations" "$work_dir/actual-attestations" || {
  echo 'error: published fixture changed the exact archive attestation subjects' >&2
  exit 1
}

# Stable identity must be established before any asset-list request.
for invalid_id in null 0 -1 1.5 '"123"' true '{}' 9007199254740992; do
  jq --argjson id "$invalid_id" '.id = $id' "$work_dir/release.good.json" >"$WITSELF_RELEASE_FIXTURE_RESPONSE"
  : >"$WITSELF_RELEASE_FIXTURE_ASSET_CALLS"
  assert_refusal 'invalid release ID' 'invalid release ID' "${published_verifier[@]}"
  [[ ! -s $WITSELF_RELEASE_FIXTURE_ASSET_CALLS ]] || { echo 'error: invalid ID reached asset API' >&2; exit 1; }
done
for identity_filter in 'del(.id)' '.tag_name = "v9.8.8"' '.draft = true' '.prerelease = true'; do
  jq "$identity_filter" "$work_dir/release.good.json" >"$WITSELF_RELEASE_FIXTURE_RESPONSE"
  : >"$WITSELF_RELEASE_FIXTURE_ASSET_CALLS"
  assert_refusal 'invalid stable release identity' 'release' "${published_verifier[@]}"
  [[ ! -s $WITSELF_RELEASE_FIXTURE_ASSET_CALLS ]] || { echo 'error: invalid identity reached asset API' >&2; exit 1; }
done
cp "$work_dir/release.good.json" "$WITSELF_RELEASE_FIXTURE_RESPONSE"

# Never accept a complete-looking partial response from a failed transport.
assert_refusal 'later page API failure' 'could not enumerate every release asset page' \
  env WITSELF_RELEASE_FIXTURE_ASSET_FAILURE=1 "${published_verifier[@]}"
for malformed_pages in '{}' '[{}]' '[[null]]' '[]' 'null'; do
  printf '%s\n' "$malformed_pages" >"$WITSELF_RELEASE_FIXTURE_ASSETS"
  assert_refusal 'invalid paginated response shape' 'release asset listing' "${published_verifier[@]}"
done
printf '%s\n' '{' >"$WITSELF_RELEASE_FIXTURE_ASSETS"
assert_refusal 'malformed paginated JSON' 'parse error' "${published_verifier[@]}"
cat "$work_dir/asset-pages.good.json" "$work_dir/asset-pages.good.json" >"$WITSELF_RELEASE_FIXTURE_ASSETS"
assert_refusal 'multiple paginated documents' 'one paginated array' "${published_verifier[@]}"
jq '.[0:1]' "$work_dir/asset-pages.good.json" >"$WITSELF_RELEASE_FIXTURE_ASSETS"
assert_refusal 'truncated page set' 'assets=27' "${published_verifier[@]}"
jq '.[1] += [.[0][0]]' "$work_dir/asset-pages.good.json" >"$WITSELF_RELEASE_FIXTURE_ASSETS"
assert_refusal 'duplicate across pages' 'assets=56 unique_names=55' "${published_verifier[@]}"

# These cases retain a valid embedded inventory: it must never be a fallback.
cp "$work_dir/published.good.json" "$WITSELF_RELEASE_FIXTURE_RESPONSE"
jq --arg name "$provider_evidence_name" '[add | map(select(.name != $name))]' \
  "$work_dir/asset-pages.good.json" >"$WITSELF_RELEASE_FIXTURE_ASSETS"
assert_refusal 'missing published evidence asset' 'expected 55 unique nonempty release assets' "${published_verifier[@]}"
jq --arg name "$provider_evidence_name" '[add | . + [.[] | select(.name == $name)]]' \
  "$work_dir/asset-pages.good.json" >"$WITSELF_RELEASE_FIXTURE_ASSETS"
assert_refusal 'duplicate published evidence asset' 'expected 55 unique nonempty release assets' "${published_verifier[@]}"
jq '[add | . + [.[0] | .name = "unexpected-public-asset"]]' \
  "$work_dir/asset-pages.good.json" >"$WITSELF_RELEASE_FIXTURE_ASSETS"
assert_refusal 'extra distinct asset' 'assets=56 unique_names=56' "${published_verifier[@]}"
for invalid_filter in '.[0].state = "starter"' '.[0].size = 0' '.[0].size = "1"' '.[0].size = 1.5' '.[0].digest = null' '.[0].digest = "bad"' '.[0].name = null'; do
  jq "[add | $invalid_filter]" "$work_dir/asset-pages.good.json" >"$WITSELF_RELEASE_FIXTURE_ASSETS"
  assert_refusal 'invalid asset metadata' 'expected 55 unique nonempty release assets' "${published_verifier[@]}"
done
jq '[add | .[0].state = "starter" | .[1].size = 0 | .[2].digest = null]' \
  "$work_dir/asset-pages.good.json" >"$WITSELF_RELEASE_FIXTURE_ASSETS"
assert_refusal 'value-free rejection diagnostics' 'invalid_states=1 invalid_sizes=1 invalid_digests=1' "${published_verifier[@]}"
jq --arg name "$provider_evidence_name" \
  '[add | (.[] | select(.name == $name) | .digest) = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"]' \
  "$work_dir/asset-pages.good.json" >"$WITSELF_RELEASE_FIXTURE_ASSETS"
assert_refusal 'wrong published evidence digest' 'asset names, sizes, or SHA-256 digests differed' "${published_verifier[@]}"
jq --arg name "$provider_evidence_name" '[add | (.[] | select(.name == $name) | .size) += 1]' \
  "$work_dir/asset-pages.good.json" >"$WITSELF_RELEASE_FIXTURE_ASSETS"
assert_refusal 'wrong published evidence size' 'asset names, sizes, or SHA-256 digests differed' "${published_verifier[@]}"
cp "$work_dir/release.good.json" "$WITSELF_RELEASE_FIXTURE_RESPONSE"
cp "$work_dir/asset-pages.good.json" "$WITSELF_RELEASE_FIXTURE_ASSETS"
: >"$WITSELF_RELEASE_FIXTURE_CALLS"
printf '%s\n' '{' >"$dist_dir/$provider_evidence_name"
assert_refusal 'invalid evidence before published API use' 'provider contract evidence did not match' "${published_verifier[@]}"
[[ ! -s $WITSELF_RELEASE_FIXTURE_CALLS ]] || {
  echo 'error: invalid evidence reached the published release API' >&2
  exit 1
}
cp "$provider_evidence_fixture" "$dist_dir/$provider_evidence_name"
awk -v name="$provider_evidence_name" '$2 != name' \
  "$work_dir/checksums.good.txt" >"$dist_dir/checksums.txt"
assert_refusal 'unsigned evidence before published API use' 'exactly one provider evidence digest' "${published_verifier[@]}"
[[ ! -s $WITSELF_RELEASE_FIXTURE_CALLS ]] || {
  echo 'error: unsigned evidence reached the published release API' >&2
  exit 1
}

printf 'Release artifact contract fixtures passed: local and published success, 25 archive subjects, %d evidence and asset refusals\n' "$refusal_count"
