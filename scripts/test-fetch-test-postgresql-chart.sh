#!/usr/bin/env bash
# The real verified chart is only fixture input; all fetches below use stub helm.
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
fetch="$repo_root/scripts/fetch-test-postgresql-chart.sh"
tmp="$(mktemp -d "${TMPDIR:-/tmp}/witself-test-chart-fetch.XXXXXX")"
tmp="$(cd "$tmp" && pwd -P)"
trap 'rm -rf "$tmp"' EXIT

fail() {
  printf 'PostgreSQL chart fetch test: FAIL: %s\n' "$1" >&2
  exit 1
}

# Never download fixture input or write to the real cache from this test suite.
# CI fetches once before running this suite and exports the verified archive.
fixture="${WITSELF_TEST_POSTGRESQL_CHART:-${WITSELF_TEST_CHART_CACHE:-${XDG_CACHE_HOME:-$HOME/.cache}/witself/test-charts}/postgresql-18.8.0.tgz}"
[[ -f "$fixture" ]] || fail 'verified fixture missing; run scripts/fetch-test-postgresql-chart.sh first'
if ! WITSELF_TEST_POSTGRESQL_CHART="$fixture" bash "$fetch" >"$tmp/fixture.path" 2>"$tmp/fixture.err"; then
  fail 'fixture does not match the pinned PostgreSQL chart'
fi
cp "$fixture" "$tmp/good.tgz"
printf 'not a chart\n' >"$tmp/bad.tgz"
# Derive the diagnostic assertion from the verified fixture, without printing it.
if command -v sha256sum >/dev/null 2>&1; then
  expected_pin="$(sha256sum "$tmp/good.tgz" | awk '{print $1}')"
else
  expected_pin="$(shasum -a 256 "$tmp/good.tgz" | awk '{print $1}')"
fi

mkdir -p "$tmp/bin"
cat >"$tmp/bin/helm" <<'HELM'
#!/usr/bin/env bash
set -euo pipefail
[[ "$1" == pull && "$2" == oci://registry-1.docker.io/bitnamicharts/postgresql ]]
shift 2
[[ "$1" == --version && "$2" == 18.8.0 && "$3" == --destination && "$#" == 4 ]]
destination="$4"
[[ "$HELM_CACHE_HOME" == "$destination/helm-cache" ]]
[[ "$HELM_CONFIG_HOME" == "$destination/helm-config" ]]
[[ "$HELM_DATA_HOME" == "$destination/helm-data" ]]
[[ "$HELM_REGISTRY_CONFIG" == "$destination/helm-registry.json" ]]
count=0
if [[ -f "$FETCH_COUNTER" ]]; then
  read -r count <"$FETCH_COUNTER"
fi
count=$((count + 1))
printf '%s\n' "$count" >"$FETCH_COUNTER"
printf '%s\n' "$destination" >>"$FETCH_DESTINATIONS"
if [[ "$count" -le "${FETCH_FAILURES:-0}" ]]; then
  printf 'partial chart\n' >"$destination/postgresql-18.8.0.tgz"
  exit 1
fi
cp "$FETCH_FIXTURE" "$destination/postgresql-18.8.0.tgz"
echo 'stub helm output must not contaminate the returned path'
HELM
cat >"$tmp/bin/sleep" <<'SLEEP'
#!/usr/bin/env bash
set -euo pipefail
[[ "$#" == 1 && "$1" == 20 ]]
printf '%s\n' "$1" >>"$FETCH_SLEEPS"
SLEEP
chmod +x "$tmp/bin/helm" "$tmp/bin/sleep"
export PATH="$tmp/bin:$PATH"
export FETCH_COUNTER="$tmp/counter" FETCH_DESTINATIONS="$tmp/destinations" FETCH_SLEEPS="$tmp/sleeps"
export FETCH_FIXTURE="$tmp/good.tgz" FETCH_FAILURES=0
export WITSELF_TEST_CHART_CACHE="$tmp/cache"
unset WITSELF_TEST_POSTGRESQL_CHART

reset_case() {
  rm -rf "$tmp/cache"
  rm -f "$FETCH_COUNTER" "$FETCH_DESTINATIONS" "$FETCH_SLEEPS"
  FETCH_FIXTURE="$tmp/good.tgz"
  FETCH_FAILURES=0
}

assert_clean_downloads() {
  local destination
  while IFS= read -r destination; do
    [[ ! -e "$destination" ]] || fail 'download staging directory survived'
  done <"$FETCH_DESTINATIONS"
}

expect_failure() {
  local status=0
  bash "$fetch" >"$tmp/result" 2>"$tmp/error" || status=$?
  [[ "$status" == 1 ]] || fail 'expected exit 1'
  [[ ! -s "$tmp/result" ]] || fail 'failure printed a chart path'
}

result="$(bash "$fetch")"
[[ "$result" == "$tmp/cache/postgresql-18.8.0.tgz" ]] || fail 'wrong cached path'
cmp -s "$tmp/good.tgz" "$result" || fail 'cached archive differs from verified input'
[[ "$(cat "$FETCH_COUNTER")" == 1 ]] || fail 'first call did not fetch exactly once'
[[ "$(bash "$fetch")" == "$result" ]] || fail 'cache hit returned a different path'
[[ "$(cat "$FETCH_COUNTER")" == 1 ]] || fail 'cache hit called helm'
assert_clean_downloads
echo 'PostgreSQL chart fetch test: PASS verified archive cached; cache hit makes no helm call'

reset_case
FETCH_FIXTURE="$tmp/bad.tgz"
expect_failure
grep -Fq "$expected_pin" "$tmp/error" || fail 'checksum failure omitted expected pin'
[[ "$(cat "$FETCH_COUNTER")" == 1 ]] || fail 'checksum mismatch was retried'
[[ -z "$(find "$tmp/cache" -mindepth 1 -print -quit)" ]] || fail 'mismatch wrote cache content'
assert_clean_downloads
echo 'PostgreSQL chart fetch test: PASS mismatch exits 1, names pin, removes download, leaves cache empty'

reset_case
FETCH_FAILURES=2
result="$(bash "$fetch" 2>"$tmp/error")"
[[ "$result" == "$tmp/cache/postgresql-18.8.0.tgz" ]] || fail 'retry returned wrong cached path'
[[ "$(cat "$FETCH_COUNTER")" == 3 ]] || fail 'transient failure retry count is wrong'
[[ "$(wc -l <"$FETCH_SLEEPS" | tr -d ' ')" == 2 ]] || fail 'transient failure pause count is wrong'
cmp -s "$tmp/good.tgz" "$result" || fail 'retry published unverified content'
assert_clean_downloads
echo 'PostgreSQL chart fetch test: PASS two transient failures retry twice then cache verified archive'

reset_case
export WITSELF_TEST_POSTGRESQL_CHART="$tmp/bad.tgz"
expect_failure
grep -Fq "$expected_pin" "$tmp/error" || fail 'override mismatch omitted expected pin'
[[ ! -e "$FETCH_COUNTER" && ! -e "$tmp/cache" ]] || fail 'override mismatch fetched or wrote cache'
echo 'PostgreSQL chart fetch test: PASS mismatching explicit archive fails without helm or cache writes'

export WITSELF_TEST_POSTGRESQL_CHART=
expect_failure
grep -Fq "$expected_pin" "$tmp/error" || fail 'empty override omitted expected pin'
[[ ! -e "$FETCH_COUNTER" && ! -e "$tmp/cache" ]] || fail 'empty override fetched or wrote cache'
echo 'PostgreSQL chart fetch test: PASS explicitly empty archive fails without helm or cache writes'

export WITSELF_TEST_POSTGRESQL_CHART="$tmp/good.tgz"
result="$(bash "$fetch")"
[[ "$result" == "$tmp/good.tgz" ]] || fail 'verified override did not return its absolute path'
[[ ! -e "$FETCH_COUNTER" && ! -e "$tmp/cache" ]] || fail 'verified override fetched or wrote cache'
unset WITSELF_TEST_POSTGRESQL_CHART
echo 'PostgreSQL chart fetch test: PASS verified explicit archive bypasses helm and cache'

cp "$tmp/good.tgz" "$tmp/chart with spaces.tgz"
result="$(cd "$tmp" && WITSELF_TEST_POSTGRESQL_CHART='chart with spaces.tgz' bash "$fetch")"
[[ "$result" == "$tmp/chart with spaces.tgz" ]] || fail 'relative override did not return its absolute path'
[[ ! -e "$FETCH_COUNTER" && ! -e "$tmp/cache" ]] || fail 'relative override fetched or wrote cache'
echo 'PostgreSQL chart fetch test: PASS relative explicit archive returns an absolute path with spaces intact'

reset_case
FETCH_FAILURES=3
expect_failure
[[ "$(cat "$FETCH_COUNTER")" == 3 ]] || fail 'exhausted fetch did not stop after three calls'
[[ "$(wc -l <"$FETCH_SLEEPS" | tr -d ' ')" == 2 ]] || fail 'exhausted fetch slept after final attempt'
[[ -z "$(find "$tmp/cache" -mindepth 1 -print -quit)" ]] || fail 'exhausted fetch wrote cache content'
assert_clean_downloads
echo 'PostgreSQL chart fetch test: PASS exhausted retries fail after three attempts and only two pauses'

reset_case
mkdir -p "$tmp/cache"
cp "$tmp/bad.tgz" "$tmp/cache/postgresql-18.8.0.tgz"
result="$(bash "$fetch")"
cmp -s "$tmp/good.tgz" "$result" || fail 'invalid cache was accepted instead of replaced'
[[ "$(cat "$FETCH_COUNTER")" == 1 ]] || fail 'invalid cache did not fetch a replacement'
assert_clean_downloads
echo 'PostgreSQL chart fetch test: PASS invalid cache is replaced only by a verified archive'

reset_case
cat >"$tmp/bin/sha256sum" <<'CHECKSUM'
#!/usr/bin/env bash
set -euo pipefail
shasum -a 256 "$1"
exit 1
CHECKSUM
chmod +x "$tmp/bin/sha256sum"
export WITSELF_TEST_POSTGRESQL_CHART="$tmp/good.tgz"
expect_failure
[[ ! -e "$FETCH_COUNTER" && ! -e "$tmp/cache" ]] || fail 'checksum command failure fetched or wrote cache'
unset WITSELF_TEST_POSTGRESQL_CHART
rm "$tmp/bin/sha256sum"
echo 'PostgreSQL chart fetch test: PASS checksum utility failure rejects even matching checksum output'

# Force the macOS-compatible fallback even on hosts providing sha256sum.
mkdir -p "$tmp/fallback-bin"
for command_name in bash dirname awk shasum; do
  ln -s "$(command -v "$command_name")" "$tmp/fallback-bin/$command_name"
done
result="$(PATH="$tmp/fallback-bin" WITSELF_TEST_POSTGRESQL_CHART="$tmp/good.tgz" bash "$fetch")"
[[ "$result" == "$tmp/good.tgz" ]] || fail 'shasum fallback did not verify the chart'
echo 'PostgreSQL chart fetch test: PASS shasum fallback verifies the pinned archive'
