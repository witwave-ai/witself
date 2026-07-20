#!/usr/bin/env bash
set -euo pipefail

repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
work_dir=$(mktemp -d "${TMPDIR:-/tmp}/witself-homebrew-publish-test.XXXXXX")
trap 'rm -rf -- "$work_dir"' EXIT

version=1.2.3
formula_dir="$work_dir/rendered"
seed_dir="$work_dir/seed"
remote_dir="$work_dir/homebrew-tap.git"
mkdir -p "$formula_dir/Formula"

write_formula() {
  local name=$1 class_name=$2
  printf '%s\n' \
    "class $class_name < Formula" \
    "  desc \"Release publisher fixture for $name\"" \
    '  homepage "https://github.com/witwave-ai/witself"' \
    "  version \"$version\"" \
    '  url "https://example.invalid/archive.tar.gz"' \
    "  sha256 \"$(printf '%064d' 0)\"" \
    '  def install' \
    "    bin.install \"$name\"" \
    '  end' \
    'end' > "$formula_dir/Formula/$name.rb"
}

write_formula witself Witself
write_formula witself-admin WitselfAdmin
write_formula witself-infra WitselfInfra

git init --quiet --initial-branch=main "$seed_dir"
mkdir -p "$seed_dir/Aliases"
printf 'legacy copied formula\n' > "$seed_dir/Aliases/ws"
git -C "$seed_dir" add Aliases/ws
git -C "$seed_dir" -c user.name=test -c user.email=test@example.invalid \
  commit --quiet -m seed
git clone --quiet --bare "$seed_dir" "$remote_dir"

bash "$repo_root/scripts/publish-homebrew-formulas.sh" \
  "$formula_dir" "$version" "file://$remote_dir" main

for name in witself witself-admin witself-infra; do
  git --git-dir="$remote_dir" show "main:Formula/$name.rb" \
    | cmp -s - "$formula_dir/Formula/$name.rb" \
    || { echo "error: published $name formula differs" >&2; exit 1; }
done

alias_mode=$(git --git-dir="$remote_dir" ls-tree main Aliases/ws | awk '{print $1}')
alias_target=$(git --git-dir="$remote_dir" show main:Aliases/ws)
[[ $alias_mode == 120000 ]] || { echo "error: ws is not a Git symlink" >&2; exit 1; }
[[ $alias_target == ../Formula/witself.rb ]] \
  || { echo "error: ws alias target is incorrect" >&2; exit 1; }

first_count=$(git --git-dir="$remote_dir" rev-list --count main)
bash "$repo_root/scripts/publish-homebrew-formulas.sh" \
  "$formula_dir" "$version" "file://$remote_dir" main
second_count=$(git --git-dir="$remote_dir" rev-list --count main)
[[ $first_count == "$second_count" ]] \
  || { echo "error: idempotent publication created another commit" >&2; exit 1; }

version=1.2.3+build.7
write_formula witself Witself
write_formula witself-admin WitselfAdmin
write_formula witself-infra WitselfInfra
bash "$repo_root/scripts/publish-homebrew-formulas.sh" \
  "$formula_dir" "$version" "file://$remote_dir" main
published_version=$(git --git-dir="$remote_dir" show main:Formula/witself.rb \
  | sed -n 's/^  version "\([^"]*\)"$/\1/p')
[[ $published_version == "$version" ]] \
  || { echo "error: SemVer build metadata was not published" >&2; exit 1; }

# Reject the first push to model another release advancing the remote between
# clone and push. The publisher must discard its stale clone, re-read the tap,
# and converge with one final commit.
reject_marker="$work_dir/rejected-once"
printf '%s\n' \
  '#!/usr/bin/env bash' \
  'set -euo pipefail' \
  "if [[ ! -e $reject_marker ]]; then" \
  "  : > $reject_marker" \
  '  echo "simulated non-fast-forward publication race" >&2' \
  '  exit 1' \
  'fi' \
  > "$remote_dir/hooks/pre-receive"
chmod +x "$remote_dir/hooks/pre-receive"

version=1.2.4
write_formula witself Witself
write_formula witself-admin WitselfAdmin
write_formula witself-infra WitselfInfra
before_retry_count=$(git --git-dir="$remote_dir" rev-list --count main)
bash "$repo_root/scripts/publish-homebrew-formulas.sh" \
  "$formula_dir" "$version" "file://$remote_dir" main
after_retry_count=$(git --git-dir="$remote_dir" rev-list --count main)
[[ $after_retry_count -eq $((before_retry_count + 1)) ]] \
  || { echo "error: retry publication did not create exactly one remote commit" >&2; exit 1; }
retry_version=$(git --git-dir="$remote_dir" show main:Formula/witself.rb \
  | sed -n 's/^  version "\([^"]*\)"$/\1/p')
[[ $retry_version == "$version" ]] \
  || { echo "error: retry publication did not converge" >&2; exit 1; }

version=1.2.2
write_formula witself Witself
write_formula witself-admin WitselfAdmin
write_formula witself-infra WitselfInfra
skip_output=$(bash "$repo_root/scripts/publish-homebrew-formulas.sh" \
  "$formula_dir" "$version" "file://$remote_dir" main)
[[ $skip_output == *"already contains newer"* ]] \
  || { echo "error: older release was not reported as a safe no-op" >&2; exit 1; }
after_downgrade=$(git --git-dir="$remote_dir" show main:Formula/witself.rb \
  | sed -n 's/^  version "\([^"]*\)"$/\1/p')
[[ $after_downgrade == 1.2.4 ]] \
  || { echo "error: skipped downgrade changed the tap" >&2; exit 1; }

if bash "$repo_root/scripts/publish-homebrew-formulas.sh" \
  "$formula_dir" invalid "file://$remote_dir" main >/dev/null 2>&1; then
  echo "error: invalid version was accepted" >&2
  exit 1
fi

# Exercise the GitHub-specific credential-helper path without touching the
# network. The mock git rewrites only the public tap URL to a second local bare
# repository and verifies that the token arrived through the environment.
version=1.2.3
write_formula witself Witself
write_formula witself-admin WitselfAdmin
write_formula witself-infra WitselfInfra
github_seed="$work_dir/github-seed"
github_remote="$work_dir/github-homebrew-tap.git"
mock_bin="$work_dir/mock-bin"
git_log="$work_dir/mock-git.log"
real_git=$(command -v git)
mkdir -p "$github_seed" "$mock_bin"
git init --quiet --initial-branch=main "$github_seed"
git -C "$github_seed" -c user.name=test -c user.email=test@example.invalid \
  commit --quiet --allow-empty -m seed
git clone --quiet --bare "$github_seed" "$github_remote"
ln -s "$repo_root/scripts/testdata/homebrew-mock-git.sh" "$mock_bin/git"
ln -s "$repo_root/scripts/testdata/homebrew-mock-gh.sh" "$mock_bin/gh"

WITSELF_TEST_REAL_GIT="$real_git" \
WITSELF_TEST_GITHUB_REMOTE="$github_remote" \
WITSELF_TEST_GIT_LOG="$git_log" \
HOMEBREW_TAP_TOKEN=test-token \
PATH="$mock_bin:$PATH" \
  bash "$repo_root/scripts/publish-homebrew-formulas.sh" \
    "$formula_dir" "$version" \
    https://github.com/witwave-ai/homebrew-tap.git main

github_version=$(git --git-dir="$github_remote" show main:Formula/witself.rb \
  | sed -n 's/^  version "\([^"]*\)"$/\1/p')
[[ $github_version == "$version" ]] \
  || { echo "error: authenticated publisher path did not push" >&2; exit 1; }
helper_count=$(grep -Fc authenticated "$git_log")
[[ $helper_count == 2 ]] \
  || { echo "error: credential helper was not used for clone and push" >&2; exit 1; }

echo "Homebrew formula publisher tests passed"
