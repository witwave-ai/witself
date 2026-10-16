#!/usr/bin/env bash
# Exercise both image platforms without Docker, a registry, or credentials.
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
fail() { echo refused-fake-docker-call >>"$BACKUP_CHECK_LOG"; echo "fake docker: $*" >&2; exit 90; }
case "${1:-}" in
  info)
    echo info >>"$BACKUP_CHECK_LOG"
    [[ $BACKUP_CHECK_SCENARIO != daemon-fails ]]
    ;;
  build)
    shift
    platform= dockerfile= image= context= no_cache=false
    while (($#)); do
      case $1 in
        --no-cache) no_cache=true; shift ;;
        --platform) platform=$2; shift 2 ;;
        --file|-f) dockerfile=$2; shift 2 ;;
        --tag|-t) image=$2; shift 2 ;;
        --*) fail "unexpected build option" ;;
        *) [[ -z $context ]] || fail "extra build argument"; context=$1; shift ;;
      esac
    done
    [[ $platform == linux/amd64 || $platform == linux/arm64 ]] || fail "unexpected build platform"
    architecture=${platform#linux/}
    [[ $no_cache == true ]] || fail "cached APK install could hide pin drift"
    [[ $dockerfile == "$BACKUP_CHECK_ROOT/images/witself-postgres-backup/Dockerfile" ]] || fail "wrong Dockerfile"
    [[ $context == "$BACKUP_CHECK_ROOT/images/witself-postgres-backup" ]] || fail "wrong build context"
    [[ $image == witself-postgres-backup-check:*"-$architecture" ]] || fail "image must have an isolated per-platform tag"
    printf '%s\n' "$image" >"$BACKUP_CHECK_IMAGES/image-$architecture"
    printf 'build:%s\n' "$architecture" >>"$BACKUP_CHECK_LOG"
    [[ $BACKUP_CHECK_SCENARIO != "$architecture-build-fails" ]]
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
    [[ $platform == linux/amd64 || $platform == linux/arm64 ]] || fail "unexpected run platform"
    architecture=${platform#linux/}
    [[ $remove == true && $network == none && $entrypoint == /bin/sh ]] || fail "unsafe runtime options"
    [[ $# == 3 && $1 == "$(<"$BACKUP_CHECK_IMAGES/image-$architecture")" && $2 == -ec ]] || fail "wrong image or shell arguments"
    printf 'run:%s\n' "$architecture" >>"$BACKUP_CHECK_LOG"
    [[ $BACKUP_CHECK_SCENARIO != "$architecture-run-fails" ]] || exit 93
    BACKUP_CHECK_PLATFORM="$architecture" PATH="$BACKUP_CHECK_VERSIONS:$PATH" /bin/sh "$2" "$3"
    ;;
  image)
    shift
    [[ ${1:-} == rm ]] || fail "unexpected image command"
    shift
    [[ $# == 1 ]] || fail "cleanup must target one owned tag without force"
    architecture=${1##*-}
    [[ $architecture == amd64 || $architecture == arm64 ]] || fail "cleanup platform missing"
    [[ -f $BACKUP_CHECK_IMAGES/image-$architecture && $1 == "$(<"$BACKUP_CHECK_IMAGES/image-$architecture")" ]] || fail "cleanup targeted an unowned image"
    printf 'cleanup:%s\n' "$architecture" >>"$BACKUP_CHECK_LOG"
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
printf '%s:%s\n' "$name" "$BACKUP_CHECK_PLATFORM" >>"$BACKUP_CHECK_LOG"
[ "$BACKUP_CHECK_SCENARIO" != "$BACKUP_CHECK_PLATFORM-$name-fails" ] || exit 92
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
    BACKUP_CHECK_IMAGES="$case_dir" BACKUP_CHECK_SCENARIO="$scenario" \
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
    local expected_log=info architecture probe attempted=()
    for architecture in amd64 arm64; do
      attempted+=("$architecture")
      expected_log+=$'\nbuild:'"$architecture"
      if [[ $scenario == "$architecture-build-fails" ]]; then break; fi
      expected_log+=$'\nrun:'"$architecture"
      if [[ $scenario == "$architecture-run-fails" ]]; then break; fi
      for probe in age aws pg_dump; do
        expected_log+=$'\n'"$probe:$architecture"
        if [[ $scenario == "$architecture-$probe-fails" ]]; then break; fi
      done
      if [[ $scenario == "$architecture-"*-fails ]]; then break; fi
    done
    for architecture in "${attempted[@]}"; do expected_log+=$'\ncleanup:'"$architecture"; done
    [[ $(<"$case_dir/docker.log") == "$expected_log" ]] || {
      echo "backup image check: $name missed a platform, version probe, failure boundary, or owned image cleanup" >&2
      cat "$case_dir/docker.log" >&2
      exit 1
    }
    if grep -Fq 'skipped: no docker' "$case_dir/output"; then
      echo "backup image check: $name concealed an available-Docker result as a skip" >&2
      exit 1
    fi
    if [[ $expected == failure ]] && grep -Fq 'image check passed' "$case_dir/output"; then
      echo "backup image check: $name falsely announced success" >&2
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
for architecture in amd64 arm64; do
  for failure in build run age aws pg_dump; do
    scenario="$architecture-$failure-fails"
    run_case "$scenario" "$scenario" '' '' failure
  done
done

printf 'postgres backup image check tests: PASS (%s cases)\n' "$case_count"
