#!/usr/bin/env bash
set -euo pipefail

source_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
captured_at=2026-08-17T22:00:00Z
work_dir=$(mktemp -d "${TMPDIR:-/tmp}/witself-billing-rollout-test.XXXXXX")
cleanup() {
  find "$work_dir" -depth -mindepth 1 -delete 2>/dev/null || true
  rmdir "$work_dir" 2>/dev/null || true
}
trap cleanup EXIT INT TERM

# Build a complete local Git fixture so this suite does not depend on checkout
# depth, historical tags, or the branch ancestry of the calling repository.
repo_root="$work_dir/repo"
mkdir -p \
  "$repo_root/scripts" \
  "$repo_root/internal/billing/lifecycle/compatibility"
cp "$source_root/scripts/billing-transition-rollout-preflight.sh" \
  "$repo_root/scripts/billing-transition-rollout-preflight.sh"
chmod +x "$repo_root/scripts/billing-transition-rollout-preflight.sh"
git -C "$repo_root" init -q
git -C "$repo_root" config user.name "Witself rollout test"
git -C "$repo_root" config user.email "rollout-test@invalid.example"
printf 'historical\n' >"$repo_root/release-state"
git -C "$repo_root" add .
git -C "$repo_root" commit -qm "historical release"
git -C "$repo_root" tag v0.0.253
printf 'unsafe predecessor\n' >"$repo_root/release-state"
git -C "$repo_root" add release-state
git -C "$repo_root" commit -qm "unsafe predecessor"
git -C "$repo_root" tag v0.0.254
printf 'witself.billing.exact-provider-target.v1\n' \
  >"$repo_root/internal/billing/lifecycle/compatibility/exact-provider-target-v1"
git -C "$repo_root" add \
  internal/billing/lifecycle/compatibility/exact-provider-target-v1
git -C "$repo_root" commit -qm "safe reader capability"
preflight="$repo_root/scripts/billing-transition-rollout-preflight.sh"

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

expect_failure unsafe-target 'does not carry the safe reader/canceller capability marker' \
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

expect_failure rollback-mode 'unsupported --mode: rollback' \
  "$preflight" --mode rollback
expect_failure compatible-roll-mode 'unsupported --mode: compatible-roll' \
  "$preflight" --mode compatible-roll

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
