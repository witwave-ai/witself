#!/usr/bin/env bash
set -euo pipefail

real_git=${WITSELF_TEST_REAL_GIT:?}
remote=${WITSELF_TEST_GITHUB_REMOTE:?}
log=${WITSELF_TEST_GIT_LOG:?}
args=("$@")
authenticated=false

for (( index = 0; index < ${#args[@]}; index++ )); do
  [[ ${args[index]} != *test-token* ]] \
    || { echo "mock git: token leaked into arguments" >&2; exit 1; }
  if [[ ${args[index]} == 'credential.helper=!gh auth git-credential' ]]; then
    authenticated=true
  fi
  if [[ ${args[index]} == https://github.com/witwave-ai/homebrew-tap.git ]]; then
    [[ $authenticated == true ]] || { echo "mock git: credential helper missing" >&2; exit 1; }
    [[ ${GH_TOKEN:-} == test-token ]] || { echo "mock git: GH_TOKEN missing" >&2; exit 1; }
    args[index]="file://$remote"
  fi
done

if [[ $authenticated == true ]]; then
  echo authenticated >> "$log"
else
  echo plain >> "$log"
fi
exec "$real_git" "${args[@]}"
