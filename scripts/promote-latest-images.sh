#!/usr/bin/env bash
set -euo pipefail
export LC_ALL=C

usage() {
  cat >&2 <<'USAGE'
usage: promote-latest-images.sh [HOMEBREW_TAP_URL] [BRANCH]

Reconciles the public Witself container images' latest tags to the release
version currently agreed by all three formulae in the Homebrew tap. The default
tap is https://github.com/witwave-ai/homebrew-tap.git on branch main.
USAGE
}

if (( $# > 2 )); then
  usage
  exit 2
fi

repository_url=${1:-https://github.com/witwave-ai/homebrew-tap.git}
branch=${2:-main}

for dependency in git docker; do
  if ! command -v "$dependency" >/dev/null 2>&1; then
    echo "error: $dependency is required" >&2
    exit 1
  fi
done

work_dir=$(mktemp -d "${TMPDIR:-/tmp}/witself-latest-promotion.XXXXXX")
trap 'rm -rf -- "$work_dir"' EXIT
tap_dir="$work_dir/homebrew-tap"

if ! git clone --quiet --depth=1 --branch "$branch" -- "$repository_url" "$tap_dir"; then
  echo "error: could not clone Homebrew tap branch $branch" >&2
  exit 1
fi

semantic_version_pattern='^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?(\+[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?$'
formulae=(witself witself-admin witself-infra)
version=

for name in "${formulae[@]}"; do
  formula="$tap_dir/Formula/$name.rb"
  if [[ ! -f $formula || -L $formula ]]; then
    echo "error: Homebrew tap formula is missing or not a regular file: Formula/$name.rb" >&2
    exit 1
  fi

  candidate_count=$(awk '/^  version "[^"]*"$/ { count++ } END { print count + 0 }' "$formula")
  candidate=$(sed -n 's/^  version "\([^"]*\)"$/\1/p' "$formula")
  if (( candidate_count != 1 )) || [[ -z $candidate || $candidate == *$'\n'* ]]; then
    echo "error: cannot resolve one version from Formula/$name.rb" >&2
    exit 1
  fi
  if [[ ! $candidate =~ $semantic_version_pattern ]]; then
    echo "error: Formula/$name.rb has a malformed semantic version: $candidate" >&2
    exit 1
  fi
  if [[ -n $version && $candidate != "$version" ]]; then
    echo "error: Homebrew tap formula versions disagree" >&2
    exit 1
  fi
  version=$candidate
done

images=(
  ghcr.io/witwave-ai/images/witself
  ghcr.io/witwave-ai/images/witself-server
)
digests=()
digest_pattern='^sha256:[0-9a-f]{64}$'

# Resolve and validate every immutable source before changing either mutable
# tag. A missing release image therefore cannot create a half-promoted channel.
for image in "${images[@]}"; do
  source_ref="$image:$version"
  if ! digest=$(docker buildx imagetools inspect "$source_ref" --format '{{.Manifest.Digest}}'); then
    echo "error: immutable image is missing or unreadable: $source_ref" >&2
    exit 1
  fi
  if [[ ! $digest =~ $digest_pattern ]]; then
    echo "error: immutable image returned a malformed digest: $source_ref" >&2
    exit 1
  fi
  digests+=("$digest")
done

for (( index = 0; index < ${#images[@]}; index++ )); do
  image=${images[index]}
  digest=${digests[index]}
  latest_ref="$image:latest"

  if ! docker buildx imagetools create \
    --tag "$latest_ref" \
    "$image@$digest"; then
    echo "error: failed to promote $latest_ref" >&2
    exit 1
  fi

  if ! promoted_digest=$(docker buildx imagetools inspect \
    "$latest_ref" --format '{{.Manifest.Digest}}'); then
    echo "error: promoted image is unreadable: $latest_ref" >&2
    exit 1
  fi
  if [[ $promoted_digest != "$digest" ]]; then
    echo "error: $latest_ref resolved to $promoted_digest, expected $digest" >&2
    exit 1
  fi
done

echo "Promoted Witself container latest tags to $version"
