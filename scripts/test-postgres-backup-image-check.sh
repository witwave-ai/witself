#!/usr/bin/env bash
# Exercise the image gate without Docker, a registry, or credentials.
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
work_dir=$(mktemp -d "${TMPDIR:-/tmp}/witself-backup-image-check-test.XXXXXX")
trap 'rm -rf -- "$work_dir"' EXIT
bash_bin=$(command -v bash)
mkdir -p "$work_dir/no-docker" "$work_dir/docker" "$work_dir/versions"
for executable in bash dirname; do
  ln -s "$(command -v "$executable")" "$work_dir/no-docker/$executable"
  ln -s "$(command -v "$executable")" "$work_dir/docker/$executable"
done

cat >"$work_dir/docker/docker" <<'DOCKER'
#!/usr/bin/env bash
set -euo pipefail
fail() { echo "fake docker: $*" >&2; exit 90; }
case "${1:-}" in
  info)
    echo info >>"$BACKUP_CHECK_LOG"
    [[ $BACKUP_CHECK_SCENARIO != daemon-fails ]]
    ;;
  build)
    shift
    platform= dockerfile= image= context=
    while (($#)); do
      case $1 in
        --platform) platform=$2; shift 2 ;;
        --file|-f) dockerfile=$2; shift 2 ;;
        --tag|-t) image=$2; shift 2 ;;
        --*) fail "unexpected build option" ;;
        *) [[ -z $context ]] || fail "extra build argument"; context=$1; shift ;;
      esac
    done
    [[ $platform == linux/amd64 ]] || fail "build platform must be linux/amd64"
    [[ $dockerfile == "$BACKUP_CHECK_ROOT/images/witself-postgres-backup/Dockerfile" ]] || fail "wrong Dockerfile"
    [[ $context == "$BACKUP_CHECK_ROOT/images/witself-postgres-backup" ]] || fail "wrong build context"
    [[ $image == witself-postgres-backup-check:* ]] || fail "image must have an isolated local tag"
    printf '%s\n' "$image" >"$BACKUP_CHECK_IMAGE"
    echo build >>"$BACKUP_CHECK_LOG"
    [[ $BACKUP_CHECK_SCENARIO != build-fails ]]
    ;;
  run)
    shift
    platform= network= entrypoint= remove=false
    while (($#)); do
      case $1 in
        --rm) remove=true; shift ;;
        --platform) platform=$2; shift 2 ;;
        --network) network=$2; shift 2 ;;
        --entrypoint) entrypoint=$2; shift 2 ;;
        --*) fail "unexpected run option" ;;
        *) break ;;
      esac
    done
    [[ $platform == linux/amd64 ]] || fail "run platform must be linux/amd64"
    [[ $remove == true && $network == none && $entrypoint == /bin/sh ]] || fail "unsafe runtime options"
    [[ $# == 3 && $1 == "$(<"$BACKUP_CHECK_IMAGE")" && $2 == -ec ]] || fail "wrong image or shell arguments"
    echo run >>"$BACKUP_CHECK_LOG"
    PATH="$BACKUP_CHECK_VERSIONS:$PATH" /bin/sh "$2" "$3"
    ;;
  image)
    shift
    [[ ${1:-} == rm ]] || fail "unexpected image command"
    shift
    if [[ ${1:-} == -f || ${1:-} == --force ]]; then shift; fi
    [[ $# == 1 && $1 == "$(<"$BACKUP_CHECK_IMAGE")" ]] || fail "cleanup targeted the wrong image"
    echo cleanup >>"$BACKUP_CHECK_LOG"
    ;;
  *) fail "unexpected command (pushing is forbidden)" ;;
esac
DOCKER
chmod +x "$work_dir/docker/docker"

for executable in age aws pg_dump; do
  cat >"$work_dir/versions/$executable" <<'VERSION'
#!/bin/sh
set -eu
name=${0##*/}
[ "$#" = 1 ] && [ "$1" = --version ] || exit 91
printf '%s\n' "$name" >>"$BACKUP_CHECK_LOG"
[ "$BACKUP_CHECK_SCENARIO" != "$name-fails" ] || exit 92
printf '%s test-version\n' "$name"
VERSION
  chmod +x "$work_dir/versions/$executable"
done

case_count=0
run_case() {
  local name=$1 scenario=$2 ci=$3 actions=$4 expected=$5
  shift 5
  local case_dir="$work_dir/$name" test_path="$work_dir/docker" status=0
  mkdir -p "$case_dir"
  : >"$case_dir/docker.log"
  if [[ $scenario == no-cli ]]; then test_path="$work_dir/no-docker"; fi
  env PATH="$test_path" CI="$ci" GITHUB_ACTIONS="$actions" \
    BACKUP_CHECK_ROOT="$repo_root" BACKUP_CHECK_LOG="$case_dir/docker.log" \
    BACKUP_CHECK_IMAGE="$case_dir/image" BACKUP_CHECK_SCENARIO="$scenario" \
    BACKUP_CHECK_VERSIONS="$work_dir/versions" \
    "$bash_bin" "$repo_root/scripts/check-postgres-backup-image.sh" "$@" \
    >"$case_dir/output" 2>&1 || status=$?
  if [[ $expected == success && $status != 0 ]] || [[ $expected == failure && $status == 0 ]]; then
    echo "backup image check: unexpected $name exit status $status" >&2
    cat "$case_dir/output" >&2
    exit 1
  fi
  if [[ $scenario == no-cli || $scenario == daemon-fails ]]; then
    if [[ $expected == success ]]; then
      grep -Fq 'skipped: no docker' "$case_dir/output" || {
        echo "backup image check: $name did not explain its local skip" >&2; exit 1;
      }
    elif grep -Fq 'skipped: no docker' "$case_dir/output"; then
      echo "backup image check: $name advertised a forbidden CI skip" >&2
      exit 1
    fi
    if [[ $scenario == no-cli ]]; then
      [[ ! -s $case_dir/docker.log ]] || { echo "backup image check: docker ran with no CLI" >&2; exit 1; }
    else
      [[ $(<"$case_dir/docker.log") == info ]] || { echo "backup image check: work ran without a daemon" >&2; exit 1; }
    fi
  else
    local expected_log=$'info\nbuild'
    if [[ $scenario != build-fails ]]; then
      expected_log+=$'\nrun\nage'
      if [[ $scenario != age-fails ]]; then
        expected_log+=$'\naws'
        if [[ $scenario != aws-fails ]]; then expected_log+=$'\npg_dump'; fi
      fi
    fi
    expected_log+=$'\ncleanup'
    [[ $(<"$case_dir/docker.log") == "$expected_log" ]] || {
      echo "backup image check: $name missed a version probe, failure boundary, or image cleanup" >&2
      exit 1
    }
    if grep -Fq 'skipped: no docker' "$case_dir/output"; then
      echo "backup image check: $name concealed an available-Docker result as a skip" >&2
      exit 1
    fi
  fi
  case_count=$((case_count + 1))
}

for scenario in no-cli daemon-fails; do
  run_case "local-$scenario" "$scenario" '' '' success
  run_case "ci-true-$scenario" "$scenario" true '' failure
  run_case "ci-one-$scenario" "$scenario" 1 '' failure
  run_case "actions-$scenario" "$scenario" '' true failure
  run_case "required-$scenario" "$scenario" '' '' failure --require-docker
done
run_case success success '' '' success
run_case ci-success success true true success --require-docker
for scenario in build-fails age-fails aws-fails pg_dump-fails; do
  run_case "$scenario" "$scenario" '' '' failure
done

printf 'postgres backup image check tests: PASS (%s cases)\n' "$case_count"
