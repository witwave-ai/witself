#!/usr/bin/env bash
set -euo pipefail

repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
preflight="$repo_root/scripts/billing-transition-rollout-preflight.sh"
captured_at=2026-08-17T22:00:00Z
work_dir=$(mktemp -d "${TMPDIR:-/tmp}/witself-billing-rollout-test.XXXXXX")
cleanup() {
  find "$work_dir" -depth -mindepth 1 -delete 2>/dev/null || true
  rmdir "$work_dir" 2>/dev/null || true
}
trap cleanup EXIT INT TERM

write_inventory() {
  local path=$1 cohort=$2 api=$3 reconcilers=$4 prepared=$5 targetless=$6
  local malformed_pending=$7 malformed_receipts=$8 post_horizon=$9
  jq -n \
    --argjson cohort "$cohort" \
    --argjson api "$api" \
    --argjson reconcilers "$reconcilers" \
    --argjson prepared "$prepared" \
    --argjson targetless "$targetless" \
    --argjson malformed_pending "$malformed_pending" \
    --argjson malformed_receipts "$malformed_receipts" \
    --argjson post_horizon "$post_horizon" \
    --arg captured_at "$captured_at" \
    '{
      schema: "witself.billing-rollout-inventory.v1",
      captured_at: $captured_at,
      billing_mutation_cohort_accounts: $cohort,
      source_fleet: {
        api_replicas: $api,
        reconciler_replicas: $reconcilers
      },
      records: {
        prepared_downgrades: $prepared,
        targetless_pending_changes: $targetless,
        malformed_pending_changes: $malformed_pending,
        malformed_mutation_receipts: $malformed_receipts,
        post_retry_horizon_receipts: $post_horizon
      }
    }' >"$path"
}

run_activate() {
  "$preflight" \
    --mode activate \
    --from-version v0.0.254 --from-ref v0.0.254 \
    --to-version v0.0.255 --to-ref HEAD \
    --allow-untagged-target \
    --inventory "$1" --expected-captured-at "$captured_at"
}

expect_failure() {
  local label=$1 expected=$2
  shift 2
  local output="$work_dir/$label.output"
  if "$@" >"$output" 2>&1; then
    printf 'error: %s unexpectedly passed\n' "$label" >&2
    exit 1
  fi
  grep -Fq "$expected" "$output" || {
    printf 'error: %s did not report %q\n' "$label" "$expected" >&2
    cat "$output" >&2
    exit 1
  }
}

clean="$work_dir/clean.json"
write_inventory "$clean" 0 0 0 0 0 0 0 0
run_activate "$clean" >"$work_dir/activate-pass.output"
grep -Fq 'billing rollout preflight: PASS' "$work_dir/activate-pass.output"

for field in cohort api reconcilers prepared targetless malformed_pending malformed_receipts post_horizon; do
  fixture="$work_dir/$field.json"
  case $field in
    cohort) write_inventory "$fixture" 1 0 0 0 0 0 0 0 ;;
    api) write_inventory "$fixture" 0 1 0 0 0 0 0 0 ;;
    reconcilers) write_inventory "$fixture" 0 0 1 0 0 0 0 0 ;;
    prepared) write_inventory "$fixture" 0 0 0 1 0 0 0 0 ;;
    targetless) write_inventory "$fixture" 0 0 0 0 1 0 0 0 ;;
    malformed_pending) write_inventory "$fixture" 0 0 0 0 0 1 0 0 ;;
    malformed_receipts) write_inventory "$fixture" 0 0 0 0 0 0 1 0 ;;
    post_horizon) write_inventory "$fixture" 0 0 0 0 0 0 0 1 ;;
  esac
  expect_failure "$field" "FAIL:" run_activate "$fixture"
done

expect_failure unsafe-target 'does not contain safe reader/canceller floor' \
  "$preflight" \
    --mode activate \
    --from-version v0.0.254 --from-ref v0.0.254 \
    --to-version v0.0.255 --to-ref v0.0.254 \
    --allow-untagged-target --inventory "$clean" \
    --expected-captured-at "$captured_at"

expect_failure wrong-version-binding 'does not resolve to requested version tag' \
  "$preflight" \
    --mode activate \
    --from-version v0.0.254 --from-ref v0.0.254 \
    --to-version v0.0.253 --to-ref HEAD \
    --allow-untagged-target --inventory "$clean" \
    --expected-captured-at "$captured_at"

rollback_prepared="$work_dir/rollback-prepared.json"
write_inventory "$rollback_prepared" 0 0 0 1 0 0 0 0
expect_failure rollback-prepared 'prepared downgrades forbid' \
  "$preflight" \
    --mode rollback \
    --from-version v0.0.255 --from-ref HEAD \
    --to-version v0.0.254 --to-ref v0.0.254 \
    --inventory "$rollback_prepared" --allow-untagged-source \
    --expected-captured-at "$captured_at"

"$preflight" \
  --mode rollback \
  --from-version v0.0.255 --from-ref HEAD \
  --to-version v0.0.254 --to-ref v0.0.254 \
  --inventory "$clean" --allow-untagged-source \
  --expected-captured-at "$captured_at" \
  >"$work_dir/rollback-pass.output"

compatible="$work_dir/compatible.json"
write_inventory "$compatible" 1 2 1 3 0 0 0 0
"$preflight" \
  --mode compatible-roll \
  --from-version v0.0.255 --from-ref HEAD \
  --to-version v0.0.256 --to-ref HEAD \
  --inventory "$compatible" --allow-untagged-source --allow-untagged-target \
  --expected-captured-at "$captured_at" \
  >"$work_dir/compatible-pass.output"

printf '{"schema":"wrong"}\n' >"$work_dir/malformed.json"
expect_failure malformed-inventory 'count-only JSON' run_activate "$work_dir/malformed.json"

expect_failure stale-inventory-fence 'does not match the operator-supplied exact fence' \
  "$preflight" \
    --mode activate \
    --from-version v0.0.254 --from-ref v0.0.254 \
    --to-version v0.0.255 --to-ref HEAD \
    --allow-untagged-target --inventory "$clean" \
    --expected-captured-at 2026-08-17T22:00:01Z

printf 'billing transition rollout preflight tests passed\n'
