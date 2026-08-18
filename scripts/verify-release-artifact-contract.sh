#!/usr/bin/env bash
set -euo pipefail

readonly expected_archive_count=25
readonly expected_sbom_count=25
readonly expected_checksum_entry_count=50
readonly expected_release_asset_count=54
readonly expected_provenance_subject_count=25

repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)

usage() {
  cat >&2 <<'EOF'
usage:
  verify-release-artifact-contract.sh local DIST_DIR VERSION FULL_COMMIT
  verify-release-artifact-contract.sh published DIST_DIR TAG REPOSITORY FULL_COMMIT
EOF
  exit 2
}

mode=${1:-}
case $mode in
  local)
    (( $# == 4 )) || usage
    ;;
  published)
    (( $# == 5 )) || usage
    ;;
  *) usage ;;
esac
dist_dir=$2
identity=$3
repository_or_commit=$4
published_commit=${5:-}

[[ $dist_dir = /* && -d $dist_dir ]] || {
  echo "error: dist directory must be an existing absolute path" >&2
  exit 1
}

for dependency in jq tar unzip; do
  command -v "$dependency" >/dev/null 2>&1 || {
    echo "error: $dependency is required" >&2
    exit 1
  }
done

work_dir=$(mktemp -d "${TMPDIR:-/tmp}/witself-release-contract.XXXXXX")
cleanup() {
  rm -rf -- "$work_dir"
}
trap cleanup EXIT

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

write_expected_archive_names() {
  local version=$1
  local product target
  for target in darwin_amd64 darwin_arm64 linux_amd64 linux_arm64; do
    printf 'witself_%s_%s.tar.gz\n' "$version" "$target"
  done
  printf 'witself_%s_windows_amd64.zip\n' "$version"
  for product in witself-admin witself-control-plane witself-infra witself-server witself-worker; do
    for target in darwin_amd64 darwin_arm64 linux_amd64 linux_arm64; do
      printf '%s_%s_%s.tar.gz\n' "$product" "$version" "$target"
    done
  done
}

write_local_public_asset_names() {
  local version=$1 archive
  while IFS= read -r archive; do
    printf '%s\n%s.sbom.json\n' "$archive" "$archive"
  done < <(write_expected_archive_names "$version")
  printf '%s\n' \
    checksums.txt \
    checksums.txt.pem \
    checksums.txt.sig \
    checksums.txt.sigstore.json
}

expected_binary_for_archive() {
  case $1 in
    witself-control-plane_*) printf '%s\n' witself-control-plane ;;
    witself-server_*) printf '%s\n' witself-server ;;
    witself-worker_*) printf '%s\n' witself-worker ;;
    witself-admin_*) printf '%s\n' witself-admin ;;
    witself-infra_*) printf '%s\n' witself-infra ;;
    witself_*.zip) printf '%s\n' witself.exe ;;
    witself_*) printf '%s\n' witself ;;
    *) return 1 ;;
  esac
}

validate_local_release() {
  local version=$1
  local full_commit=$2
  [[ $version =~ ^[0-9]+\.[0-9]+\.[0-9]+([+-][0-9A-Za-z.-]+)?$ ]] || {
    echo "error: invalid release version: $version" >&2
    exit 1
  }
  [[ $full_commit =~ ^[0-9a-f]{40}$ ]] || {
    echo "error: expected commit must be full lowercase 40-hex" >&2
    exit 1
  }
  for file in artifacts.json checksums.txt checksums.txt.pem checksums.txt.sig checksums.txt.sigstore.json; do
    [[ -s $dist_dir/$file ]] || {
      echo "error: release metadata/signing asset is missing or empty: $file" >&2
      exit 1
    }
  done

  write_expected_archive_names "$version" | LC_ALL=C sort >"$work_dir/expected-archives"
  jq -er '
    [.[] | select(.type == "Archive") | .name]
    | if length == 25 and (unique | length) == 25
      then sort[]
      else error("expected 25 unique archives")
      end
  ' "$dist_dir/artifacts.json" >"$work_dir/actual-archives"
  cmp -s "$work_dir/expected-archives" "$work_dir/actual-archives" || {
    echo "error: the release archive inventory did not match the exact 25-archive contract" >&2
    diff -u "$work_dir/expected-archives" "$work_dir/actual-archives" >&2 || true
    exit 1
  }

  sed 's/$/.sbom.json/' "$work_dir/expected-archives" >"$work_dir/expected-sboms"
  jq -er '
    [.[] | select(.type == "SBOM") | .name]
    | if length == 25 and (unique | length) == 25
      then sort[]
      else error("expected 25 unique SBOMs")
      end
  ' "$dist_dir/artifacts.json" >"$work_dir/actual-sboms"
  cmp -s "$work_dir/expected-sboms" "$work_dir/actual-sboms" || {
    echo "error: the release SBOM inventory did not match the exact 25-SBOM contract" >&2
    diff -u "$work_dir/expected-sboms" "$work_dir/actual-sboms" >&2 || true
    exit 1
  }

  cat "$work_dir/expected-archives" "$work_dir/expected-sboms" | LC_ALL=C sort \
    >"$work_dir/expected-checksum-names"
  awk '
    NF != 2 || $1 !~ /^[0-9a-f]+$/ || length($1) != 64 { exit 1 }
    { print $2 }
  ' "$dist_dir/checksums.txt" | LC_ALL=C sort >"$work_dir/actual-checksum-names" || {
    echo "error: checksums.txt contained a malformed entry" >&2
    exit 1
  }
  if [[ $(wc -l <"$work_dir/actual-checksum-names" | tr -d '[:space:]') != "$expected_checksum_entry_count" ]] ||
     [[ $(LC_ALL=C sort -u "$work_dir/actual-checksum-names" | wc -l | tr -d '[:space:]') != "$expected_checksum_entry_count" ]] ||
     ! cmp -s "$work_dir/expected-checksum-names" "$work_dir/actual-checksum-names"; then
      echo "error: checksums.txt did not contain the exact 50 archive and SBOM payloads" >&2
      exit 1
  fi

  local archive archive_name binary members member_mode manifest_sha actual_sha sbom
  while IFS= read -r archive_name; do
    archive="$dist_dir/$archive_name"
    [[ -s $archive ]] || { echo "error: release archive is missing or empty: $archive_name" >&2; exit 1; }
    binary=$(expected_binary_for_archive "$archive_name") || {
      echo "error: no binary contract for archive: $archive_name" >&2
      exit 1
    }
    if [[ $archive_name == *.zip ]]; then
      members=$(unzip -Z1 "$archive") || exit 1
    else
      members=$(tar -tzf "$archive") || exit 1
    fi
    [[ $members == "$binary" ]] || {
      echo "error: $archive_name must contain exactly one root executable named $binary" >&2
      exit 1
    }
    if [[ $archive_name != *.zip ]]; then
      member_mode=$(tar -tvzf "$archive" | awk 'NR == 1 { print $1 }') || exit 1
      [[ $member_mode == -* && $member_mode == *x* ]] || {
        echo "error: $archive_name member must be a regular executable" >&2
        exit 1
      }
    fi

    manifest_sha=$(awk -v name="$archive_name" '$2 == name { print $1 }' "$dist_dir/checksums.txt")
    actual_sha=$(sha256_file "$archive")
    [[ $manifest_sha == "$actual_sha" ]] || {
      echo "error: checksum mismatch for $archive_name" >&2
      exit 1
    }

    sbom="$dist_dir/$archive_name.sbom.json"
    [[ -s $sbom ]] || { echo "error: SBOM is missing or empty: $archive_name.sbom.json" >&2; exit 1; }
    jq -e --arg archive "$archive_name" --arg binary "$binary" '
      .spdxVersion == "SPDX-2.3"
      and .name == $archive
      and ([.files[]?.fileName] == [$binary])
    ' "$sbom" >/dev/null || {
      echo "error: SBOM did not describe the exact one-executable archive: $archive_name" >&2
      exit 1
    }
    manifest_sha=$(awk -v name="$archive_name.sbom.json" '$2 == name { print $1 }' "$dist_dir/checksums.txt")
    actual_sha=$(sha256_file "$sbom")
    [[ $manifest_sha == "$actual_sha" ]] || {
      echo "error: checksum mismatch for $archive_name.sbom.json" >&2
      exit 1
    }
  done <"$work_dir/expected-archives"

  write_local_public_asset_names "$version" | LC_ALL=C sort >"$work_dir/expected-assets"
  [[ $(wc -l <"$work_dir/expected-assets" | tr -d '[:space:]') == "$expected_release_asset_count" ]] || {
    echo "error: internal release asset-count contract was inconsistent" >&2
    exit 1
  }

  bash "$repo_root/scripts/test-control-plane-release-artifact.sh" \
    "$dist_dir" "$version" "$full_commit"

  printf 'Release artifact contract passed: %d archives, %d SBOMs, %d checksum entries, %d public assets\n' \
    "$expected_archive_count" "$expected_sbom_count" \
    "$expected_checksum_entry_count" "$expected_release_asset_count"
}

validate_published_release() {
  local tag=$1
  local repository=$2
  local full_commit=$3
  [[ $tag =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]] || {
    echo "error: invalid stable release tag: $tag" >&2
    exit 1
  }
  [[ $repository =~ ^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$ ]] || {
    echo "error: invalid GitHub repository: $repository" >&2
    exit 1
  }
  [[ $full_commit =~ ^[0-9a-f]{40}$ ]] || {
    echo "error: published release commit must be full lowercase 40-hex" >&2
    exit 1
  }
  command -v gh >/dev/null 2>&1 || { echo "error: gh is required" >&2; exit 1; }
  command -v git >/dev/null 2>&1 || { echo "error: git is required" >&2; exit 1; }

  local version=${tag#v}
  local tag_commit checkout_commit
  tag_commit=$(gh api "repos/$repository/commits/$tag" --jq .sha)
  [[ $tag_commit == "$full_commit" ]] || {
    echo "error: release tag resolved to $tag_commit, expected $full_commit" >&2
    exit 1
  }
  checkout_commit=$(git -C "$repo_root" rev-parse --verify HEAD)
  [[ $checkout_commit == "$full_commit" ]] || {
    echo "error: release checkout resolved to $checkout_commit, expected $full_commit" >&2
    exit 1
  }
  write_local_public_asset_names "$version" | LC_ALL=C sort >"$work_dir/expected-assets"
  gh api "repos/$repository/releases/tags/$tag" >"$work_dir/release.json"
  jq -er --arg tag "$tag" '
    if .tag_name != $tag or .draft != false or .prerelease != false
      then error("release was not the requested stable publication")
      else .assets
    | if length == 54
        and ([.[].name] | unique | length) == 54
        and all(.[]; .size > 0)
        and all(.[]; .digest | test("^sha256:[0-9a-f]{64}$"))
      then .[] | [.name, (.size | tostring), .digest] | @tsv
      else error("expected 54 unique nonempty release assets")
      end
    end
  ' "$work_dir/release.json" | LC_ALL=C sort >"$work_dir/published-assets"

  local asset local_size local_digest
  while IFS= read -r asset; do
    [[ -s $dist_dir/$asset ]] || {
      echo "error: local release output is missing public asset: $asset" >&2
      exit 1
    }
    local_size=$(wc -c <"$dist_dir/$asset" | tr -d '[:space:]')
    local_digest="sha256:$(sha256_file "$dist_dir/$asset")"
    printf '%s\t%s\t%s\n' "$asset" "$local_size" "$local_digest"
  done <"$work_dir/expected-assets" | LC_ALL=C sort >"$work_dir/local-assets"
  cmp -s "$work_dir/local-assets" "$work_dir/published-assets" || {
    echo "error: published GitHub asset names, sizes, or SHA-256 digests differed from local release output" >&2
    diff -u "$work_dir/local-assets" "$work_dir/published-assets" >&2 || true
    exit 1
  }

  jq -er '
    [.[] | select(.type == "Archive") | .name]
    | if length == 25 and (unique | length) == 25
      then sort[]
      else error("expected 25 provenance subjects")
      end
  ' "$dist_dir/artifacts.json" >"$work_dir/provenance-subjects"
  [[ $(wc -l <"$work_dir/provenance-subjects" | tr -d '[:space:]') == "$expected_provenance_subject_count" ]] || exit 1

  local archive attempt verified
  while IFS= read -r archive; do
    verified=false
    for attempt in 1 2 3 4 5; do
      if gh attestation verify "$dist_dir/$archive" \
        --repo "$repository" \
        --signer-workflow "$repository/.github/workflows/release.yml" \
        --signer-digest "$full_commit" \
        --source-ref "refs/tags/$tag" \
        --source-digest "$full_commit" \
        --deny-self-hosted-runners \
        >"$work_dir/attestation-verify.log" 2>&1; then
        verified=true
        break
      fi
      sleep "$attempt"
    done
    [[ $verified == true ]] || {
      echo "error: build provenance was unavailable for $archive" >&2
      cat "$work_dir/attestation-verify.log" >&2
      exit 1
    }
  done <"$work_dir/provenance-subjects"

  printf 'Published release contract passed: %d assets and %d archive provenance subjects\n' \
    "$expected_release_asset_count" "$expected_provenance_subject_count"
}

case $mode in
  local) validate_local_release "$identity" "$repository_or_commit" ;;
  published) validate_published_release "$identity" "$repository_or_commit" "$published_commit" ;;
  *) usage ;;
esac
