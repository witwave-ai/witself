#!/usr/bin/env bash
set -euo pipefail

repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)

if (( $# != 0 && $# != 3 )); then
  echo "usage: $0 [DIST_DIR VERSION FULL_COMMIT]" >&2
  exit 2
fi

for dependency in git go jq tar; do
  command -v "$dependency" >/dev/null 2>&1 || {
    echo "error: $dependency is required" >&2
    exit 1
  }
done

work_dir=$(mktemp -d "${TMPDIR:-/tmp}/witself-control-plane-release-test.XXXXXX")
cleanup() {
  rm -rf -- "$work_dir"
}
trap cleanup EXIT

if (( $# == 0 )); then
  command -v goreleaser >/dev/null 2>&1 || {
    echo "error: goreleaser is required" >&2
    exit 1
  }
  dist_dir="$work_dir/dist"
  config="$work_dir/goreleaser-control-plane.yaml"
  "$repo_root/scripts/render-goreleaser-control-plane-smoke-config.sh" \
    "$config" "$dist_dir" "$repo_root/.goreleaser.yaml"
  (
    cd "$repo_root"
    goreleaser release \
      --config "$config" \
      --snapshot \
      --clean \
      --skip=before,sign,sbom,docker \
      --parallelism 1 \
      --timeout 30m
  )
  version=$(jq -er '.version | select(type == "string" and length > 0)' "$dist_dir/metadata.json")
  full_commit=$(git -C "$repo_root" rev-parse --verify HEAD)
else
  dist_dir=$1
  version=$2
  full_commit=$3
fi

[[ $dist_dir = /* && -d $dist_dir ]] || {
  echo "error: dist directory must be an existing absolute path" >&2
  exit 1
}
[[ $version =~ ^[0-9]+\.[0-9]+\.[0-9]+([+~-][0-9A-Za-z.-]+)?$ ]] || {
  echo "error: invalid release version: $version" >&2
  exit 1
}
[[ $full_commit =~ ^[0-9a-f]{40}$ ]] || {
  echo "error: expected commit must be full lowercase 40-hex" >&2
  exit 1
}
[[ -f $dist_dir/artifacts.json && -f $dist_dir/checksums.txt ]] || {
  echo "error: GoReleaser metadata or checksums are missing" >&2
  exit 1
}

list_archives() {
  jq -er --arg version "$version" '
    [
      .[]
      | select(.type == "Archive")
      | select(.name | startswith("witself-control-plane_" + $version + "_"))
      | .name
    ]
    | sort
    | if length == 4 then .[] else error("expected four control-plane archives") end
  ' "$dist_dir/artifacts.json"
}

archive_names=()
while IFS= read -r archive_name; do
  archive_names+=("$archive_name")
done < <(list_archives)

expected_names=(
  "witself-control-plane_${version}_darwin_amd64.tar.gz"
  "witself-control-plane_${version}_darwin_arm64.tar.gz"
  "witself-control-plane_${version}_linux_amd64.tar.gz"
  "witself-control-plane_${version}_linux_arm64.tar.gz"
)
[[ ${archive_names[*]} == "${expected_names[*]}" ]] || {
  echo "error: control-plane release target inventory was unexpected" >&2
  exit 1
}

native_goos=$(go env GOOS)
native_goarch=$(go env GOARCH)
native_checked=false

for archive_name in "${archive_names[@]}"; do
  archive="$dist_dir/$archive_name"
  [[ -f $archive ]] || { echo "error: missing archive: $archive_name" >&2; exit 1; }

  target=${archive_name#"witself-control-plane_${version}_"}
  target=${target%.tar.gz}
  target_goos=${target%_*}
  target_goarch=${target#*_}
  jq -e \
    --arg name "$archive_name" \
    --arg goos "$target_goos" \
    --arg goarch "$target_goarch" '
      [.[] | select(
        .type == "Archive"
        and .name == $name
        and .goos == $goos
        and .goarch == $goarch
        and .extra.ID == "witself-control-plane"
      )] | length == 1
    ' "$dist_dir/artifacts.json" >/dev/null || {
      echo "error: archive metadata did not bind the expected control-plane build target: $archive_name" >&2
      exit 1
    }

  members=$(tar -tzf "$archive") || {
    echo "error: could not list archive: $archive_name" >&2
    exit 1
  }
  [[ $members == witself-control-plane ]] || {
    echo "error: $archive_name must contain exactly one root witself-control-plane executable" >&2
    exit 1
  }
  member_mode=$(tar -tvzf "$archive" | awk 'NR == 1 { print $1 }') || exit 1
  [[ $member_mode == -* && $member_mode == *x* ]] || {
    echo "error: $archive_name member must be a regular executable" >&2
    exit 1
  }

  if command -v sha256sum >/dev/null 2>&1; then
    archive_sha=$(sha256sum "$archive" | awk '{print $1}')
  elif command -v shasum >/dev/null 2>&1; then
    archive_sha=$(shasum -a 256 "$archive" | awk '{print $1}')
  else
    echo "error: sha256sum or shasum is required" >&2
    exit 1
  fi
  checksum_matches=$(awk -v name="$archive_name" '$2 == name { print $1 }' "$dist_dir/checksums.txt")
  [[ $checksum_matches == "$archive_sha" ]] || {
    echo "error: checksum manifest did not bind exactly $archive_name" >&2
    exit 1
  }

  if [[ $target == "${native_goos}_${native_goarch}" ]]; then
    native_dir="$work_dir/native"
    mkdir -p "$native_dir"
    tar -xzf "$archive" -C "$native_dir"
    [[ -f $native_dir/witself-control-plane && -x $native_dir/witself-control-plane ]] || {
      echo "error: native archive did not contain one executable" >&2
      exit 1
    }
    version_output=$("$native_dir/witself-control-plane" version)
    [[ $version_output =~ ^witself-control-plane\ ([^\ ]+)\ \(commit\ ([0-9a-f]{40}),\ built\ ([0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z)\)$ ]] || {
      echo "error: native control-plane version output did not carry a full commit" >&2
      exit 1
    }
    [[ ${BASH_REMATCH[1]} == "$version" && ${BASH_REMATCH[2]} == "$full_commit" ]] || {
      echo "error: native control-plane version identity did not match the release" >&2
      exit 1
    }
    for inventory_phase in scan finalize; do
      set +e
      inventory_output=$("$native_dir/witself-control-plane" \
        billing-rollout-inventory "$inventory_phase" 2>&1)
      inventory_status=$?
      set -e
      [[ $inventory_status -ne 0 &&
         $inventory_output == "witself-control-plane: billing rollout inventory: billing rollout inventory arguments are incomplete" ]] || {
        echo "error: native control-plane binary did not expose the $inventory_phase inventory command" >&2
        exit 1
      }
    done
    native_checked=true
  fi
done

[[ $native_checked == true ]] || {
  echo "error: the release matrix did not contain the native test target" >&2
  exit 1
}

# Publishing an operator evidence artifact must not silently turn it into a
# universal-installer product. Exercise the back-compat selector with all
# network-independent inputs fixed and prove rejection happens before a target
# directory is created.
installer_target="$work_dir/installer-target"
set +e
installer_output=$(HOME="$work_dir/installer-home" \
  WITSELF_BINARY=witself-control-plane \
  WS_VERSION=v0.0.0 \
  WS_INSTALL_DIR="$installer_target" \
  sh "$repo_root/install.sh" 2>&1)
installer_status=$?
set -e
[[ $installer_status -ne 0 &&
   $installer_output == 'install: unknown binary "witself-control-plane" (want witself|witself-infra|witself-server|witself-admin)' &&
   ! -e $installer_target ]] || {
  echo "error: operator-only control-plane binary entered the universal installer surface" >&2
  exit 1
}

echo "Control-plane release archive and full-commit tests passed"
