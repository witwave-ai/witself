#!/usr/bin/env bash
set -euo pipefail

# d12af5c is the first reader/canceller that refuses targetless provider
# cleanup and binds cancellation to the exact provider object. Do not advance
# this floor by version number alone; the target Git object must contain it.
readonly safe_reader_floor=d12af5c7384cb443c3f79910c86b2b597a447e85
readonly inventory_schema=witself.billing-rollout-inventory.v1

fail() {
  printf 'billing rollout preflight: FAIL: %s\n' "$1" >&2
  exit 1
}

usage() {
  cat <<'EOF'
Usage:
  scripts/billing-transition-rollout-preflight.sh \
    --mode activate|rollback|compatible-roll \
    --from-version vMAJOR.MINOR.PATCH --from-ref GIT_REF \
    --to-version vMAJOR.MINOR.PATCH --to-ref GIT_REF \
    --inventory PATH [--allow-untagged-source] [--allow-untagged-target]

The command is hermetic: it reads the local Git graph and one count-only JSON
inventory. It does not query or mutate Kubernetes, Cloudflare, R2, or Stripe.

Use either --allow-untagged-* flag only for a reviewed pre-release drill. A
tagged deployment must omit them so each requested version is bound to its
exact tag.
EOF
}

mode=
from_version=
from_ref=
to_version=
to_ref=
inventory_path=
allow_untagged_target=false
allow_untagged_source=false

while (($# > 0)); do
  case $1 in
    --mode)
      (($# >= 2)) || fail "--mode requires a value"
      mode=$2
      shift 2
      ;;
    --from-version)
      (($# >= 2)) || fail "--from-version requires a value"
      from_version=$2
      shift 2
      ;;
    --from-ref)
      (($# >= 2)) || fail "--from-ref requires a value"
      from_ref=$2
      shift 2
      ;;
    --to-version)
      (($# >= 2)) || fail "--to-version requires a value"
      to_version=$2
      shift 2
      ;;
    --to-ref)
      (($# >= 2)) || fail "--to-ref requires a value"
      to_ref=$2
      shift 2
      ;;
    --inventory)
      (($# >= 2)) || fail "--inventory requires a value"
      inventory_path=$2
      shift 2
      ;;
    --allow-untagged-target)
      allow_untagged_target=true
      shift
      ;;
    --allow-untagged-source)
      allow_untagged_source=true
      shift
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      fail "unknown argument: $1"
      ;;
  esac
done

case $mode in
  activate|rollback|compatible-roll) ;;
  '') fail "--mode is required" ;;
  *) fail "unsupported --mode: $mode" ;;
esac

[[ $from_version =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]] \
  || fail "--from-version must be vMAJOR.MINOR.PATCH"
[[ $to_version =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]] \
  || fail "--to-version must be vMAJOR.MINOR.PATCH"
[[ $from_version != "$to_version" ]] \
  || fail "source and target versions must differ"

valid_git_ref() {
  local value=$1 label=$2
  [[ $value =~ ^[A-Za-z0-9][A-Za-z0-9._/-]*$ ]] \
    || fail "$label contains unsupported Git-ref characters"
  [[ $value != *..* && $value != *'@{'* && $value != */. && $value != ./* ]] \
    || fail "$label is not a canonical Git ref or commit"
}

[[ -n $from_ref ]] || fail "--from-ref is required"
[[ -n $to_ref ]] || fail "--to-ref is required"
valid_git_ref "$from_ref" "--from-ref"
valid_git_ref "$to_ref" "--to-ref"
[[ -n $inventory_path ]] || fail "--inventory is required"
[[ -f $inventory_path ]] || fail "inventory file does not exist"
command -v git >/dev/null 2>&1 || fail "git is required"
command -v jq >/dev/null 2>&1 || fail "jq is required"

repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)

resolve_commit() {
  local ref=$1 label=$2 commit
  commit=$(git -C "$repo_root" rev-parse --verify "${ref}^{commit}" 2>/dev/null) \
    || fail "$label does not resolve to a commit"
  printf '%s\n' "$commit"
}

from_commit=$(resolve_commit "$from_ref" "--from-ref")
to_commit=$(resolve_commit "$to_ref" "--to-ref")
floor_commit=$(resolve_commit "$safe_reader_floor" "safe reader/canceller floor")

bind_version_tag() {
  local version=$1 commit=$2 label=$3 allow_missing=$4 tag_commit
  if tag_commit=$(git -C "$repo_root" rev-parse --verify \
    "refs/tags/${version}^{commit}" 2>/dev/null); then
    [[ $commit == "$tag_commit" ]] \
      || fail "$label does not resolve to requested version tag $version"
    return
  fi
  [[ $allow_missing == true ]] \
    || fail "requested version tag $version is absent; refuse an unbound deployment"
}

bind_version_tag \
  "$from_version" "$from_commit" "--from-ref" "$allow_untagged_source"
bind_version_tag \
  "$to_version" "$to_commit" "--to-ref" "$allow_untagged_target"

contains_safe_floor() {
  git -C "$repo_root" merge-base --is-ancestor "$floor_commit" "$1"
}

from_safe=false
to_safe=false
if contains_safe_floor "$from_commit"; then
  from_safe=true
fi
if contains_safe_floor "$to_commit"; then
  to_safe=true
fi

case $mode in
  activate)
    [[ $from_version == v0.0.254 && $from_safe == false ]] \
      || fail "activate must start from the known-unsafe v0.0.254 predecessor"
    [[ $to_safe == true ]] \
      || fail "activation target does not contain safe reader/canceller floor $safe_reader_floor"
    ;;
  rollback)
    [[ $from_safe == true ]] \
      || fail "rollback source does not contain the safe reader/canceller floor"
    [[ $to_version == v0.0.254 && $to_safe == false ]] \
      || fail "this guard supports rollback only to the known v0.0.254 predecessor"
    ;;
  compatible-roll)
    [[ $from_safe == true && $to_safe == true ]] \
      || fail "compatible-roll requires both source and target to contain $safe_reader_floor"
    ;;
esac

if ! jq -e --arg schema "$inventory_schema" '
  def nonnegative_integer:
    type == "number" and . >= 0 and floor == .;
  type == "object" and
  keys == [
    "billing_mutation_cohort_accounts",
    "captured_at",
    "records",
    "schema",
    "source_fleet"
  ] and
  .schema == $schema and
  (.captured_at | type == "string" and
    test("^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z$")) and
  (.billing_mutation_cohort_accounts | nonnegative_integer) and
  (.source_fleet | type == "object" and
    keys == ["api_replicas", "reconciler_replicas"] and
    (.api_replicas | nonnegative_integer) and
    (.reconciler_replicas | nonnegative_integer)) and
  (.records | type == "object" and
    keys == [
      "malformed_mutation_receipts",
      "malformed_pending_changes",
      "post_retry_horizon_receipts",
      "prepared_downgrades",
      "targetless_pending_changes"
    ] and
    (.malformed_mutation_receipts | nonnegative_integer) and
    (.malformed_pending_changes | nonnegative_integer) and
    (.post_retry_horizon_receipts | nonnegative_integer) and
    (.prepared_downgrades | nonnegative_integer) and
    (.targetless_pending_changes | nonnegative_integer))
' "$inventory_path" >/dev/null; then
  fail "inventory is not strict $inventory_schema count-only JSON"
fi

cohort_accounts=$(jq -r '.billing_mutation_cohort_accounts' "$inventory_path")
source_api=$(jq -r '.source_fleet.api_replicas' "$inventory_path")
source_reconcilers=$(jq -r '.source_fleet.reconciler_replicas' "$inventory_path")
prepared=$(jq -r '.records.prepared_downgrades' "$inventory_path")
targetless=$(jq -r '.records.targetless_pending_changes' "$inventory_path")
malformed_pending=$(jq -r '.records.malformed_pending_changes' "$inventory_path")
malformed_receipts=$(jq -r '.records.malformed_mutation_receipts' "$inventory_path")
post_horizon=$(jq -r '.records.post_retry_horizon_receipts' "$inventory_path")

((targetless == 0)) \
  || fail "targetless pending changes require operator quarantine"
((malformed_pending == 0)) \
  || fail "malformed pending changes require operator quarantine"
((malformed_receipts == 0)) \
  || fail "malformed mutation receipts require operator quarantine"
((post_horizon == 0)) \
  || fail "post-retry-horizon receipts require operator reconciliation"

if [[ $mode != compatible-roll ]]; then
  ((cohort_accounts == 0)) \
    || fail "billing mutation cohort must be empty for an incompatible cutover"
  ((source_api == 0)) \
    || fail "source API replicas must be fully drained before the first target writer"
  ((source_reconcilers == 0)) \
    || fail "source reconcilers must be fully drained before the first target writer"
  ((prepared == 0)) \
    || fail "prepared downgrades forbid activation with or rollback to v0.0.254"
fi

printf '%s\n' \
  "billing rollout preflight: PASS" \
  "mode=$mode" \
  "from=$from_version commit=$from_commit safe_reader=$from_safe" \
  "to=$to_version commit=$to_commit safe_reader=$to_safe" \
  "cohort_accounts=$cohort_accounts source_api=$source_api source_reconcilers=$source_reconcilers" \
  "prepared=$prepared targetless=$targetless malformed_pending=$malformed_pending malformed_receipts=$malformed_receipts post_horizon=$post_horizon"
