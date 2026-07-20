#!/usr/bin/env bash
set -euo pipefail
export LC_ALL=C

usage() {
  cat >&2 <<'USAGE'
usage: publish-homebrew-formulas.sh OUTPUT_DIR VERSION [REPOSITORY_URL] [BRANCH]

Publishes OUTPUT_DIR/Formula/{witself,witself-admin,witself-infra}.rb and the
tracked ws alias to the Homebrew tap in one non-force Git commit. The default
repository is https://github.com/witwave-ai/homebrew-tap.git on branch main.
HOMEBREW_TAP_TOKEN is required for that GitHub repository.
USAGE
}

if (( $# < 2 || $# > 4 )); then
  usage
  exit 2
fi

output_dir=$1
version=$2
repository_url=${3:-https://github.com/witwave-ai/homebrew-tap.git}
branch=${4:-main}

if [[ ! $version =~ ^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?(\+[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?$ ]]; then
  echo "error: invalid release version: $version" >&2
  exit 2
fi

# Prints -1, 0, or 1 using SemVer precedence. Build metadata is intentionally
# ignored; prerelease identifiers follow SemVer's numeric/lexical ordering.
semver_compare() {
  local left=${1%%+*} right=${2%%+*}
  local left_core=$left right_core=$right left_pre='' right_pre=''
  local left_major left_minor left_patch right_major right_minor right_patch
  local index left_identifier right_identifier
  local -a left_identifiers=() right_identifiers=()

  if [[ $left == *-* ]]; then
    left_core=${left%%-*}
    left_pre=${left#*-}
  fi
  if [[ $right == *-* ]]; then
    right_core=${right%%-*}
    right_pre=${right#*-}
  fi

  IFS=. read -r left_major left_minor left_patch <<< "$left_core"
  IFS=. read -r right_major right_minor right_patch <<< "$right_core"
  for index in 1 2 3; do
    case $index in
      1) left_identifier=$left_major; right_identifier=$right_major ;;
      2) left_identifier=$left_minor; right_identifier=$right_minor ;;
      3) left_identifier=$left_patch; right_identifier=$right_patch ;;
    esac
    if (( 10#$left_identifier < 10#$right_identifier )); then
      echo -1
      return
    fi
    if (( 10#$left_identifier > 10#$right_identifier )); then
      echo 1
      return
    fi
  done

  if [[ -z $left_pre && -z $right_pre ]]; then
    echo 0
    return
  fi
  if [[ -z $left_pre ]]; then
    echo 1
    return
  fi
  if [[ -z $right_pre ]]; then
    echo -1
    return
  fi

  IFS=. read -r -a left_identifiers <<< "$left_pre"
  IFS=. read -r -a right_identifiers <<< "$right_pre"
  for (( index = 0; ; index++ )); do
    if (( index >= ${#left_identifiers[@]} && index >= ${#right_identifiers[@]} )); then
      echo 0
      return
    fi
    if (( index >= ${#left_identifiers[@]} )); then
      echo -1
      return
    fi
    if (( index >= ${#right_identifiers[@]} )); then
      echo 1
      return
    fi

    left_identifier=${left_identifiers[index]}
    right_identifier=${right_identifiers[index]}
    [[ $left_identifier == "$right_identifier" ]] && continue

    if [[ $left_identifier =~ ^[0-9]+$ && $right_identifier =~ ^[0-9]+$ ]]; then
      if (( 10#$left_identifier < 10#$right_identifier )); then
        echo -1
      else
        echo 1
      fi
      return
    fi
    if [[ $left_identifier =~ ^[0-9]+$ ]]; then
      echo -1
      return
    fi
    if [[ $right_identifier =~ ^[0-9]+$ ]]; then
      echo 1
      return
    fi
    if [[ $left_identifier < $right_identifier ]]; then
      echo -1
    else
      echo 1
    fi
    return
  done
}

formulae=(witself witself-admin witself-infra)
for name in "${formulae[@]}"; do
  formula="$output_dir/Formula/$name.rb"
  if [[ ! -f $formula ]]; then
    echo "error: rendered formula is missing: $formula" >&2
    exit 1
  fi
  if ! grep -Fqx "  version \"$version\"" "$formula"; then
    echo "error: $formula does not declare version $version" >&2
    exit 1
  fi
  if command -v ruby >/dev/null 2>&1; then
    ruby -c "$formula" >/dev/null
  fi
done

work_dir=$(mktemp -d "${TMPDIR:-/tmp}/witself-homebrew-publish.XXXXXX")
trap 'rm -rf -- "$work_dir"' EXIT
max_attempts=4

git_with_github_auth() {
  GH_TOKEN="$HOMEBREW_TAP_TOKEN" git \
    -c credential.helper='!gh auth git-credential' "$@"
}

github_repository=false
if [[ $repository_url == https://github.com/* ]]; then
  github_repository=true
  if [[ -z ${HOMEBREW_TAP_TOKEN:-} ]]; then
    echo "error: HOMEBREW_TAP_TOKEN is required for GitHub publication" >&2
    exit 1
  fi
  if ! command -v gh >/dev/null 2>&1; then
    echo "error: gh is required for GitHub publication" >&2
    exit 1
  fi
fi

clone_tap() {
  local destination=$1

  # The gh credential helper reads HOMEBREW_TAP_TOKEN through GH_TOKEN. The
  # token never appears in the remote URL, Git configuration, or command line.
  if [[ $github_repository == true ]]; then
    git_with_github_auth clone --quiet --depth=1 --branch "$branch" "$repository_url" "$destination"
  else
    git clone --quiet --depth=1 --branch "$branch" "$repository_url" "$destination"
  fi
}

push_tap() {
  local source=$1

  if [[ $github_repository == true ]]; then
    git_with_github_auth -C "$source" push --quiet origin "HEAD:refs/heads/$branch"
  else
    git -C "$source" push --quiet origin "HEAD:refs/heads/$branch"
  fi
}

for (( attempt = 1; attempt <= max_attempts; attempt++ )); do
  tap_dir="$work_dir/homebrew-tap-$attempt"
  if ! clone_tap "$tap_dir"; then
    if (( attempt == max_attempts )); then
      echo "error: could not clone Homebrew tap after $max_attempts attempts" >&2
      exit 1
    fi
    echo "Homebrew tap clone failed; retrying from the current branch head ($attempt/$max_attempts)" >&2
    continue
  fi

  current_version=
  current_count=0
  for name in "${formulae[@]}"; do
    current_formula="$tap_dir/Formula/$name.rb"
    [[ -f $current_formula ]] || continue
    candidate=$(sed -n 's/^  version "\([^"]*\)"$/\1/p' "$current_formula")
    if [[ -z $candidate || $candidate == *$'\n'* ]]; then
      echo "error: cannot resolve one current version from $current_formula" >&2
      exit 1
    fi
    if [[ ! $candidate =~ ^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?(\+[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?$ ]]; then
      echo "error: current tap version is not semantic: $candidate" >&2
      exit 1
    fi
    if [[ -n $current_version && $candidate != "$current_version" ]]; then
      echo "error: current tap formula versions disagree" >&2
      exit 1
    fi
    current_version=$candidate
    (( current_count += 1 ))
  done

  if (( current_count != 0 && current_count != ${#formulae[@]} )); then
    echo "error: current tap has only $current_count of ${#formulae[@]} Witself formulae" >&2
    exit 1
  fi
  if [[ -n $current_version ]] && (( $(semver_compare "$version" "$current_version") < 0 )); then
    echo "Homebrew tap already contains newer witself $current_version; skipping $version"
    exit 0
  fi

  mkdir -p "$tap_dir/Formula" "$tap_dir/Aliases"
  for name in "${formulae[@]}"; do
    install -m 0644 "$output_dir/Formula/$name.rb" "$tap_dir/Formula/$name.rb"
  done
  ln -sfn ../Formula/witself.rb "$tap_dir/Aliases/ws"

  git -C "$tap_dir" add -- \
    Formula/witself.rb \
    Formula/witself-admin.rb \
    Formula/witself-infra.rb \
    Aliases/ws

  if git -C "$tap_dir" diff --cached --quiet; then
    echo "Homebrew tap already contains witself $version"
    exit 0
  fi

  changed_paths=$(git -C "$tap_dir" diff --cached --name-only | LC_ALL=C sort)
  while IFS= read -r path; do
    case $path in
      Aliases/ws|Formula/witself.rb|Formula/witself-admin.rb|Formula/witself-infra.rb) ;;
      *)
        echo "error: refusing to publish unexpected tap change: $path" >&2
        exit 1
        ;;
    esac
  done <<< "$changed_paths"

  git -C "$tap_dir" \
    -c user.name=witself-release \
    -c user.email=release@witself.com \
    commit --quiet -m "witself $version"

  if push_tap "$tap_dir"; then
    echo "Published Homebrew formulae for witself $version"
    exit 0
  fi

  if (( attempt == max_attempts )); then
    echo "error: Homebrew tap push did not converge after $max_attempts attempts" >&2
    exit 1
  fi
  echo "Homebrew tap advanced while publishing; retrying from the current branch head ($attempt/$max_attempts)" >&2
done
