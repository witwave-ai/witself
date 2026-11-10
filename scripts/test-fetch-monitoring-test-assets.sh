#!/usr/bin/env bash
# Hermetic fixtures exercise verification and retries without real release assets.
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
tmp="$(mktemp -d "${TMPDIR:-/tmp}/witself-monitoring-fetch-test.XXXXXX")"
tmp="$(cd "$tmp" && pwd -P)"
trap 'rm -rf "$tmp"' EXIT
case_name=production-pins
cases=0
fail() {
  printf 'monitoring test asset fetch contract: FAIL (%s): %s\n' "$case_name" "$1" >&2
  exit 1
}
pass() { cases=$((cases + 1)); }
sha256_file() {
  local checksum
  if command -v sha256sum >/dev/null 2>&1; then
    checksum="$(sha256sum "$1")"
  else
    checksum="$(shasum -a 256 "$1")"
  fi
  printf '%s\n' "${checksum%% *}"
}

# Inspect the real table independently; never print a pin, even on failure.
rows=0
keys=' '
while IFS= read -r line || [[ -n "$line" ]]; do
  [[ -z "$line" || "$line" == \#* ]] && continue
  read -r name platform version pin url extra <<<"$line"
  [[ -z "$extra" && -n "$url" && "$pin" =~ ^[a-fA-F0-9]{64}$ ]] || fail 'malformed production row'
  [[ "$line" == "$name $platform $version $pin $url" ]] || fail 'production row spacing'
  [[ "$keys" != *" $name/$platform "* ]] || fail 'duplicate production row'
  keys+="$name/$platform "
  case "$name/$platform" in
    chart/all) [[ "$version" == 87.6.0 ]] || fail 'chart version' ;;
    promtool/linux-amd64|promtool/darwin-amd64|promtool/darwin-arm64)
      [[ "$version" == 3.13.2 ]] || fail 'promtool version' ;;
    amtool/linux-amd64|amtool/darwin-amd64|amtool/darwin-arm64)
      [[ "$version" == 0.33.0 ]] || fail 'amtool version' ;;
    *) fail 'unexpected production asset or platform' ;;
  esac
  rows=$((rows + 1))
done <"$repo_root/scripts/testdata/monitoring-test-assets.pins"
[[ "$rows" == 7 ]] || fail 'expected seven production rows'
pass

mkdir -p "$tmp/repo/scripts/testdata" "$tmp/fixtures" "$tmp/bin" "$tmp/homes"
cp "$repo_root/scripts/fetch-monitoring-test-assets.sh" "$tmp/repo/scripts/"
fetch="$tmp/repo/scripts/fetch-monitoring-test-assets.sh"
pins="$tmp/repo/scripts/testdata/monitoring-test-assets.pins"
chart=kube-prometheus-stack-87.6.0.tgz
real_tar="$(command -v tar)"
export TEST_REAL_TAR="$real_tar"
mkdir -p "$tmp/source/chart"
printf 'fake chart\n' >"$tmp/source/chart/Chart.yaml"
tar -czf "$tmp/fixtures/$chart" -C "$tmp/source" chart
printf '# name platform version sha256 url\nchart all 87.6.0 %s helm://https://prometheus-community.github.io/helm-charts/kube-prometheus-stack\n' \
  "$(sha256_file "$tmp/fixtures/$chart")" >"$pins"
for platform in linux-amd64 darwin-amd64 darwin-arm64; do
  for name in promtool amtool; do
    if [[ "$name" == promtool ]]; then
      package=prometheus version=3.13.2
    else
      package=alertmanager version=0.33.0
    fi
    top="$package-$version.$platform"
    mkdir -p "$tmp/source/$top"
    printf '#!/usr/bin/env bash\nprintf "fixture %s %s\\n"\n' "$name" "$platform" >"$tmp/source/$top/$name"
    chmod +x "$tmp/source/$top/$name"
    tar -czf "$tmp/fixtures/$top.tar.gz" -C "$tmp/source" "$top"
    printf '%s %s %s %s https://github.com/prometheus/%s/releases/download/v%s/%s.tar.gz\n' \
      "$name" "$platform" "$version" "$(sha256_file "$tmp/fixtures/$top.tar.gz")" "$package" "$version" "$top" >>"$pins"
  done
done
cp "$pins" "$tmp/good.pins"

# Restrict PATH so an installed host tool cannot silently bypass a fixture fetch.
for tool in bash dirname mkdir rm mktemp cp mv chmod ln readlink wc tr cmp cat sleep gzip ls; do
  ln -s "$(command -v "$tool")" "$tmp/bin/$tool"
done
for tool in sha256sum shasum; do
  if command -v "$tool" >/dev/null 2>&1; then
    ln -s "$(command -v "$tool")" "$tmp/bin/$tool"
  fi
done
rm "$tmp/bin/sleep"
cat >"$tmp/bin/helm" <<'HELM'
#!/usr/bin/env bash
set -euo pipefail
[[ "$#" == 8 && "$1" == pull && "$2" == --repo ]]
[[ "$3" == https://prometheus-community.github.io/helm-charts && "$4" == kube-prometheus-stack ]]
[[ "$5" == --version && "$6" == 87.6.0 && "$7" == --destination ]]
destination="$8"
[[ "$HELM_CACHE_HOME" == "$destination/helm-cache" ]]
[[ "$HELM_CONFIG_HOME" == "$destination/helm-config" ]]
[[ "$HELM_DATA_HOME" == "$destination/helm-data" ]]
[[ "$HELM_REGISTRY_CONFIG" == "$destination/helm-registry.json" ]]
[[ "$HELM_REPOSITORY_CACHE" == "$destination/helm-cache/repository" ]]
[[ "$HELM_REPOSITORY_CONFIG" == "$destination/helm-repositories.yaml" ]]
bash "$TEST_DOWNLOAD" chart "$destination/kube-prometheus-stack-87.6.0.tgz"
HELM
cat >"$tmp/bin/curl" <<'CURL'
#!/usr/bin/env bash
set -euo pipefail
[[ "$#" == 6 && "$1" == -fsSL && "$2" == --retry && "$3" == 0 ]]
[[ "$5" == -o ]]
case "$4" in
  https://github.com/prometheus/prometheus/releases/download/v3.13.2/*) asset=promtool ;;
  https://github.com/prometheus/alertmanager/releases/download/v0.33.0/*) asset=amtool ;;
  *) exit 1 ;;
esac
[[ "${4##*/}" == "${6##*/}" ]]
bash "$TEST_DOWNLOAD" "$asset" "$6"
CURL
cat >"$tmp/download" <<'DOWNLOAD'
#!/usr/bin/env bash
set -euo pipefail
asset="$1" output="$2"
count=0
if [[ -f "$TEST_LOG/$asset.count" ]]; then read -r count <"$TEST_LOG/$asset.count"; fi
count=$((count + 1))
printf '%s\n' "$count" >"$TEST_LOG/$asset.count"
printf '%s\n' "$asset" >>"$TEST_LOG/calls"
printf '%s\n' "$(dirname "$output")" >>"$TEST_LOG/stages"
printf 'stub %s noise must go to stderr\n' "$asset"
if [[ "$asset" == "$TEST_TARGET" && "$count" -le "$TEST_FAILURES" ]]; then
  printf 'partial download\n' >"$output"
  exit 1
fi
if [[ "$asset" == "$TEST_TARGET" && "$TEST_CORRUPT" == 1 ]]; then
  printf 'complete but wrong download\n' >"$output"
else
  cp "$TEST_FIXTURES/${output##*/}" "$output"
fi
DOWNLOAD
cat >"$tmp/bin/sleep" <<'SLEEP'
#!/usr/bin/env bash
set -euo pipefail
[[ "$#" == 1 && "$1" == 20 ]]
printf '%s\n' "$1" >>"$TEST_LOG/sleeps"
SLEEP
cat >"$tmp/bin/uname" <<'UNAME'
#!/usr/bin/env bash
set -euo pipefail
case "$1" in
  -s) printf '%s\n' "${TEST_HOST%-*}" ;;
  -m) printf '%s\n' "${TEST_HOST##*-}" ;;
  *) exit 1 ;;
esac
UNAME
cat >"$tmp/bin/tar" <<'TAR'
#!/usr/bin/env bash
set -euo pipefail
[[ "$#" == 5 && "$1" == -xzf && "$3" == -C ]]
archive="${2##*/}"
case "$archive" in
  prometheus-*) [[ "$5" == "${archive%.tar.gz}/promtool" ]] ;;
  alertmanager-*) [[ "$5" == "${archive%.tar.gz}/amtool" ]] ;;
  *) exit 1 ;;
esac
printf '%s\n' "$*" >>"$TEST_LOG/tar"
exec "$TEST_REAL_TAR" "$@"
TAR
chmod +x "$tmp/bin/helm" "$tmp/bin/curl" "$tmp/bin/sleep" "$tmp/bin/uname" "$tmp/bin/tar"
export PATH="$tmp/bin"
export TEST_DOWNLOAD="$tmp/download" TEST_FIXTURES="$tmp/fixtures"
export WITSELF_HOME="$tmp/homes/witself" DSH_HOME="$tmp/homes/dsh"
export TMPDIR="$tmp"
export TEST_TARGET=chart TEST_FAILURES=0 TEST_CORRUPT=0 TEST_HOST=Linux-x86_64
export TEST_LOG="$tmp/log"
export WITSELF_TEST_ASSET_CACHE="$tmp/cache"
unset WITSELF_MONITORING_TEST_ASSETS

reset_case() {
  case_name="$1"
  rm -rf "$tmp/cache" "$tmp/override" "$TEST_LOG"
  mkdir -p "$TEST_LOG"
  rm -f "$tmp/bin/promtool" "$tmp/bin/amtool"
  cp "$tmp/good.pins" "$pins"
  TEST_TARGET=chart TEST_FAILURES=0 TEST_CORRUPT=0 TEST_HOST=Linux-x86_64
  unset WITSELF_MONITORING_TEST_ASSETS
}
count_calls() {
  if [[ -f "$TEST_LOG/$1.count" ]]; then cat "$TEST_LOG/$1.count"; else printf '0\n'; fi
}
assert_clean() {
  local stage
  if [[ -f "$TEST_LOG/stages" ]]; then
    while IFS= read -r stage; do
      [[ ! -e "$stage" ]] || fail 'download staging survived'
    done <"$TEST_LOG/stages"
  fi
  for stage in "$tmp/cache"/.monitoring-assets.* "$tmp"/witself-monitoring-assets.*; do
    [[ ! -e "$stage" ]] || fail 'cache staging survived'
  done
}
run_success() {
  if ! bash "$fetch" >"$tmp/result" 2>"$tmp/error"; then
    local error_line
    while IFS= read -r error_line; do
      while [[ "$error_line" =~ [a-fA-F0-9]{64} ]]; do
        error_line="${error_line//${BASH_REMATCH[0]}/<hex>}"
      done
      printf '%s\n' "$error_line" >&2
    done <"$tmp/error"
    fail 'fetch unexpectedly failed'
  fi
  [[ "$(wc -l <"$tmp/result" | tr -d ' ')" == 1 ]] || fail 'stdout is not exactly one line'
  [[ "$(cat "$tmp/result")" == "${WITSELF_MONITORING_TEST_ASSETS:-$tmp/cache}" ]] || fail 'wrong absolute output path'
  assert_clean
}
expect_failure() {
  local status=0
  bash "$fetch" >"$tmp/result" 2>"$tmp/error" || status=$?
  [[ "$status" == 1 ]] || fail 'expected exit 1'
  [[ ! -s "$tmp/result" ]] || fail 'failure printed a path'
  assert_clean
}
assert_error() {
  local content
  content="$(cat "$tmp/error")"
  [[ "$content" == *"$1"* ]] || fail 'missing expected diagnostic'
}
asset_file() {
  case "$1" in
    chart) printf '%s\n' "$chart" ;;
    promtool) printf 'prometheus-3.13.2.linux-amd64.tar.gz\n' ;;
    amtool) printf 'alertmanager-0.33.0.linux-amd64.tar.gz\n' ;;
  esac
}
asset_pin() {
  local name platform version pin url
  while read -r name platform version pin url; do
    if [[ "$name" == "$1" && ( "$platform" == all || "$platform" == linux-amd64 ) ]]; then
      printf '%s\n' "$pin"
      return
    fi
  done <"$pins"
  return 1
}
seed_dir() {
  local directory="$1"
  mkdir -p "$directory"
  cp "$tmp/fixtures/$chart" "$directory/"
  cp "$tmp/fixtures/prometheus-3.13.2.linux-amd64.tar.gz" "$directory/"
  cp "$tmp/fixtures/alertmanager-0.33.0.linux-amd64.tar.gz" "$directory/"
  cp "$tmp/source/prometheus-3.13.2.linux-amd64/promtool" "$directory/promtool-3.13.2-linux-amd64"
  cp "$tmp/source/alertmanager-0.33.0.linux-amd64/amtool" "$directory/amtool-0.33.0-linux-amd64"
  ln -s promtool-3.13.2-linux-amd64 "$directory/promtool"
  ln -s amtool-0.33.0-linux-amd64 "$directory/amtool"
}
assert_no_download() {
  [[ ! -f "$TEST_LOG/calls" ]] || fail 'unexpected download'
}
assert_tools() {
  local platform="$1"
  [[ "$(readlink "$tmp/cache/promtool")" == "promtool-3.13.2-$platform" ]] || fail 'promtool is not a relative versioned symlink'
  [[ "$(readlink "$tmp/cache/amtool")" == "amtool-0.33.0-$platform" ]] || fail 'amtool is not a relative versioned symlink'
  cmp -s "$tmp/source/prometheus-3.13.2.$platform/promtool" "$tmp/cache/promtool" || fail 'wrong promtool binary'
  cmp -s "$tmp/source/alertmanager-0.33.0.$platform/amtool" "$tmp/cache/amtool" || fail 'wrong amtool binary'
  [[ -x "$tmp/cache/promtool" && -x "$tmp/cache/amtool" ]] || fail 'tools are not executable'
}

reset_case 'a cache hit'
seed_dir "$tmp/cache"
run_success
assert_no_download
assert_tools linux-amd64
pass

for asset in chart promtool amtool; do
  reset_case "b corrupt cached archive ($asset)"
  seed_dir "$tmp/cache"
  file="$(asset_file "$asset")"
  printf 'corrupt archive\n' >"$tmp/cache/$file"
  printf 'stale binary\n' >"$tmp/cache/promtool-3.13.2-linux-amd64"
  printf 'stale binary\n' >"$tmp/cache/amtool-0.33.0-linux-amd64"
  run_success
  [[ "$(count_calls "$asset")" == 1 ]] || fail 'corrupt archive not fetched once'
  [[ "$(wc -l <"$TEST_LOG/calls" | tr -d ' ')" == 1 ]] || fail 'valid archives downloaded'
  cmp -s "$tmp/fixtures/$file" "$tmp/cache/$file" || fail 'corrupt cache not replaced'
  assert_tools linux-amd64
  pass
done

for asset in chart promtool amtool; do
  reset_case "c retry then success ($asset)"
  TEST_TARGET="$asset" TEST_FAILURES=2
  run_success
  [[ "$(count_calls "$asset")" == 3 ]] || fail 'expected third attempt to succeed'
  [[ "$(cat "$TEST_LOG/sleeps")" == $'20\n20' ]] || fail 'expected exactly two sleep 20 calls'
  file="$(asset_file "$asset")"
  assert_error "monitoring test assets: $file attempt 1/3 failed"
  assert_error "monitoring test assets: $file attempt 2/3 failed"
  assert_tools linux-amd64
  pass

done
for asset in chart promtool amtool; do
  reset_case "d retry exhaustion ($asset)"
  TEST_TARGET="$asset" TEST_FAILURES=3
  expect_failure
  file="$(asset_file "$asset")"
  assert_error "monitoring test assets: $file attempt 3/3 failed"
  assert_error "monitoring test assets: fetch of $file failed after 3 attempts"
  [[ "$(count_calls "$asset")" == 3 ]] || fail 'wrong exhausted attempt count'
  [[ "$(cat "$TEST_LOG/sleeps")" == $'20\n20' ]] || fail 'wrong exhausted sleep count'
  [[ ! -e "$tmp/cache/$file" ]] || fail 'failed archive published'
  pass
done
for asset in chart promtool amtool; do
  reset_case "e downloaded checksum mismatch ($asset)"
  TEST_TARGET="$asset" TEST_CORRUPT=1
  expect_failure
  file="$(asset_file "$asset")"
  assert_error "monitoring test assets: checksum mismatch for $file; expected sha256 $(asset_pin "$asset")"
  [[ "$(count_calls "$asset")" == 1 ]] || fail 'checksum mismatch retried'
  [[ ! -e "$tmp/cache/$file" ]] || fail 'unverified archive published'
  [[ ! -e "$TEST_LOG/sleeps" ]] || fail 'checksum mismatch slept'
  pass
done

reset_case 'f verified override'
seed_dir "$tmp/override"
export WITSELF_MONITORING_TEST_ASSETS="$tmp/override"
run_success
assert_no_download
[[ ! -e "$tmp/cache" ]] || fail 'override touched cache'
pass

for damage in chart promtool amtool missing-archive stale-binary missing-binary; do
  reset_case "g unverified override ($damage)"
  seed_dir "$tmp/override"
  export WITSELF_MONITORING_TEST_ASSETS="$tmp/override"
  case "$damage" in
    chart|promtool|amtool) file="$(asset_file "$damage")"; printf 'bad\n' >"$tmp/override/$file" ;;
    missing-archive) rm "$tmp/override/prometheus-3.13.2.linux-amd64.tar.gz" ;;
    stale-binary) printf 'bad binary\n' >"$tmp/override/promtool-3.13.2-linux-amd64" ;;
    missing-binary) rm "$tmp/override/promtool" ;;
  esac
  expect_failure
  assert_error 'monitoring test assets: checksum mismatch for '
  assert_no_download
  [[ ! -e "$tmp/cache" ]] || fail 'unverified override touched cache'
  pass
done
reset_case 'g empty override'
export WITSELF_MONITORING_TEST_ASSETS=
expect_failure
assert_no_download
[[ ! -e "$tmp/cache" ]] || fail 'empty override touched cache'
pass

for installed in both promtool amtool; do
  reset_case "h PATH tools ($installed)"
  if [[ "$installed" == both || "$installed" == promtool ]]; then
    cp "$tmp/source/prometheus-3.13.2.linux-amd64/promtool" "$tmp/bin/promtool"
  fi
  if [[ "$installed" == both || "$installed" == amtool ]]; then
    cp "$tmp/source/alertmanager-0.33.0.linux-amd64/amtool" "$tmp/bin/amtool"
  fi
  run_success
  [[ "$(count_calls chart)" == 1 ]] || fail 'chart not fetched'
  for asset in promtool amtool; do
    if [[ "$installed" == both || "$installed" == "$asset" ]]; then
      [[ "$(count_calls "$asset")" == 0 ]] || fail 'installed tool was downloaded'
      [[ ! -e "$tmp/cache/$(asset_file "$asset")" ]] || fail 'installed tool archive cached'
    else
      [[ "$(count_calls "$asset")" == 1 ]] || fail 'missing tool was not downloaded'
    fi
  done
  pass
done

reset_case 'i unsupported platform'
TEST_HOST=Linux-aarch64
expect_failure
assert_error 'unsupported monitoring-test host: Linux-aarch64'
assert_no_download
pass

for defect in fields checksum name duplicate; do
  reset_case "j malformed pins ($defect)"
  pin="$(asset_pin chart)"
  bad_line=9
  case "$defect" in
    fields) printf 'chart all 87.6.0\n' >>"$pins" ;;
    checksum)
      contents="$(cat "$pins")"
      printf '%s\n' "${contents/$pin/broken}" >"$pins"
      bad_line=2
      ;;
    name) printf 'unknown all 1.0.0 %s https://invalid.example/test\n' "$pin" >>"$pins" ;;
    duplicate) printf 'chart all 87.6.0 %s helm://https://prometheus-community.github.io/helm-charts/kube-prometheus-stack\n' "$pin" >>"$pins" ;;
  esac
  expect_failure
  assert_error "monitoring test assets: malformed pins file at line $bad_line"
  assert_no_download
  pass
done

reset_case 'k stdout hygiene'
run_success
[[ "$(wc -l <"$TEST_LOG/calls" | tr -d ' ')" == 3 ]] || fail 'noisy download stubs did not all run'
assert_tools linux-amd64
pass

for asset in chart promtool amtool; do
  reset_case "l directory publication destination ($asset)"
  seed_dir "$tmp/cache"
  case "$asset" in
    chart) destination="$tmp/cache/$(asset_file chart)" ;;
    promtool) destination="$tmp/cache/promtool-3.13.2-linux-amd64" ;;
    amtool) destination="$tmp/cache/amtool" ;;
  esac
  rm -f "$destination"
  mkdir "$destination"
  expect_failure
  assert_error "monitoring test assets: refusing to publish ${destination##*/}: destination is a directory"
  [[ -d "$destination" && ! -L "$destination" ]] || fail 'directory destination replaced'
  [[ -z "$(ls -A "$destination")" ]] || fail 'file nested inside directory destination'
  assert_clean
  pass
done

for asset in chart promtool amtool; do
  reset_case "m symlink directory publication destination ($asset)"
  seed_dir "$tmp/cache"
  case "$asset" in
    chart) destination="$tmp/cache/$(asset_file chart)" ;;
    promtool) destination="$tmp/cache/promtool-3.13.2-linux-amd64" ;;
    amtool) destination="$tmp/cache/amtool" ;;
  esac
  rm -f "$destination"
  mkdir "$tmp/outside"
  ln -s "$tmp/outside" "$destination"
  expect_failure
  assert_error "monitoring test assets: refusing to publish ${destination##*/}: destination is a directory"
  [[ -d "$destination" && -L "$destination" ]] || fail 'symlink directory destination replaced'
  [[ "$(readlink "$destination")" == "$tmp/outside" ]] || fail 'symlink directory destination redirected'
  [[ -z "$(ls -A "$tmp/outside")" ]] || fail 'file written outside cache'
  assert_clean
  rm -rf "$tmp/outside"
  pass
done

for host in Darwin-x86_64 Darwin-arm64; do
  reset_case "platform mapping ($host)"
  TEST_HOST="$host"
  run_success
  if [[ "$host" == Darwin-x86_64 ]]; then platform=darwin-amd64; else platform=darwin-arm64; fi
  assert_tools "$platform"
  pass
done

reset_case 'verified archives repair missing or stale cached tools'
seed_dir "$tmp/cache"
rm "$tmp/cache/promtool-3.13.2-linux-amd64"
printf 'stale binary\n' >"$tmp/cache/amtool-0.33.0-linux-amd64"
run_success
assert_no_download
assert_tools linux-amd64
pass

reset_case 'relative override with spaces'
seed_dir "$tmp/override"
mv "$tmp/override" "$tmp/override with spaces"
result="$(cd "$tmp" && WITSELF_MONITORING_TEST_ASSETS='override with spaces' bash "$fetch" 2>"$tmp/error")" \
  || fail 'relative override rejected'
[[ "$result" == "$tmp/override with spaces" ]] || fail 'override was not canonicalized'
assert_no_download
[[ ! -e "$tmp/cache" ]] || fail 'relative override touched cache'
rm -rf "$tmp/override with spaces"
pass

if [[ -x "$tmp/bin/shasum" ]]; then
  reset_case 'shasum fallback'
  seed_dir "$tmp/override"
  export WITSELF_MONITORING_TEST_ASSETS="$tmp/override"
  if [[ -e "$tmp/bin/sha256sum" ]]; then mv "$tmp/bin/sha256sum" "$tmp/sha256sum"; fi
  run_success
  assert_no_download
  if [[ -e "$tmp/sha256sum" ]]; then mv "$tmp/sha256sum" "$tmp/bin/sha256sum"; fi
  pass
fi

reset_case 'checksum command failure'
seed_dir "$tmp/override"
export WITSELF_MONITORING_TEST_ASSETS="$tmp/override"
if [[ -e "$tmp/bin/sha256sum" ]]; then mv "$tmp/bin/sha256sum" "$tmp/sha256sum"; fi
TEST_CHECKSUM_VALUE="$(asset_pin chart)"
export TEST_CHECKSUM_VALUE
cat >"$tmp/bin/sha256sum" <<'CHECKSUM'
#!/usr/bin/env bash
set -euo pipefail
printf '%s  %s\n' "$TEST_CHECKSUM_VALUE" "$1"
exit 1
CHECKSUM
chmod +x "$tmp/bin/sha256sum"
expect_failure
assert_no_download
[[ ! -e "$tmp/cache" ]] || fail 'failed checksum command touched cache'
rm "$tmp/bin/sha256sum"
if [[ -e "$tmp/sha256sum" ]]; then mv "$tmp/sha256sum" "$tmp/bin/sha256sum"; fi
pass

printf 'monitoring test asset fetch contract: PASS (%s cases)\n' "$cases"
