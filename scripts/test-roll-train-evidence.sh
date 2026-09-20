#!/usr/bin/env bash
set -euo pipefail

SOURCE_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
TEST_ROOT_RAW="$(mktemp -d "${TMPDIR:-/tmp}/witself-roll-train-evidence-test.XXXXXX")"
TEST_ROOT="$(cd "$TEST_ROOT_RAW" && pwd -P)"
TRAIN="$SOURCE_ROOT/scripts/roll-train.sh"
BACKUP=civo-sandbox-use1-backup
SERVING=civo-sandbox-use1-serving
VERSION=1.2.3
EVIDENCE_A="$TEST_ROOT/evidence/$BACKUP-pre-v$VERSION-20260820T113000Z-0a1b2c3d"
EVIDENCE_B="$TEST_ROOT/evidence/$SERVING-pre-v$VERSION-20260820T113000Z-0a1b2c3d"

fail() {
  printf 'roll train evidence test: FAIL: %s\n' "$1" >&2
  if [ -f "$TEST_ROOT/output" ]; then cat "$TEST_ROOT/output" >&2; fi
  exit 1
}
cleanup() {
  local status=$?
  trap - EXIT INT TERM
  find "$TEST_ROOT" -depth -mindepth 1 -delete 2>/dev/null || true
  rmdir "$TEST_ROOT" 2>/dev/null || true
  exit "$status"
}
trap cleanup EXIT INT TERM

command -v jq >/dev/null 2>&1 || fail 'jq is required'
command -v yq >/dev/null 2>&1 || fail 'yq is required'
command -v go >/dev/null 2>&1 || fail 'go is required'
command -v shasum >/dev/null 2>&1 || fail 'shasum is required'
ROLL_TRAIN_REAL_YQ=$(command -v yq)
umask 077
mkdir -p "$TEST_ROOT/bin" "$TEST_ROOT/evidence"

# Use actual, integrity-valid artifact triples matching the verifier fixtures.
# Their release and reviewed-cell coverage must pass the real offline verifier:
# unrelated evidence cannot become relevant merely because --cells changes.
for cell in "$BACKUP" "$SERVING"; do
  backup_id="$cell-pre-v$VERSION-20260820T113000Z-0a1b2c3d"
  evidence="$TEST_ROOT/evidence/$backup_id"
  mkdir "$evidence"
  printf 'witself-test-ciphertext-%s\n' "$cell" >"$evidence/$backup_id.dump.age"
  digest=$(shasum -a 256 "$evidence/$backup_id.dump.age")
  digest=${digest%% *}
  bytes=$(wc -c <"$evidence/$backup_id.dump.age")
  printf '%s  %s.dump.age\n' "$digest" "$backup_id" >"$evidence/$backup_id.sha256"
  jq -n --arg cell "$cell" --arg release "$VERSION" --arg id "$backup_id" \
    --arg digest "$digest" --argjson bytes "$bytes" '{
      schema: "witself.civo-pre-migration-backup.v1", backup_id: $id,
      source: {cell: $cell, kubernetes_context: "civo-admin@witself",
        postgresql_version_num: 180003, schema_version: 91,
        pgvector_extension_installed: true},
      target_release: $release, created_at: "2026-08-20T11:30:00Z",
      artifact: {file: ($id + ".dump.age"), bytes: $bytes, encryption: "age",
        checksum_algorithm: "sha256", ciphertext_sha256: $digest,
        checksum_file: ($id + ".sha256")},
      procedure: {script_sha256: ("ab" * 32)},
      restore_verification: {status: "verified", verified_at: "2026-08-20T11:35:00Z",
        network: "none", plaintext_storage: "container tmpfs",
        image_ref: "pgvector/pgvector:pg18", image_id: ("sha256:" + ("cd" * 32)),
        schema_version: 91, public_table_count: 120, account_count: 3,
        invalid_index_count: 0, unvalidated_constraint_count: 0,
        pgvector_extension_installed: true, pgvector_extension_matches_source: true,
        disposable_target_cleaned: true}
    }' >"$evidence/$backup_id.json"
done
(
  cd "$SOURCE_ROOT"
  go run ./cmd/witself-admin backup-evidence verify --release "$VERSION" -- \
    "$EVIDENCE_A" "$EVIDENCE_B"
) >"$TEST_ROOT/evidence-verification" 2>&1 || {
  cat "$TEST_ROOT/evidence-verification" >&2
  fail 'reviewed-cell fixture evidence must pass the real verifier'
}

# Only the two local git path reads needed by dry-run are allowed. Every other
# operational call would be a failure, before network access or pin mutation.
cat >"$TEST_ROOT/bin/git" <<'EOF_GIT'
#!/usr/bin/env bash
set -euo pipefail
printf 'git %s\n' "$*" >>"$TEST_LOG"
case "$*" in
  'rev-parse --show-toplevel') printf '%s\n' "$SOURCE_ROOT" ;;
  *'rev-parse --git-common-dir') printf '%s/.git\n' "$SOURCE_ROOT" ;;
  *) exit 97 ;;
esac
EOF_GIT
for tool in gh kubectl curl yq witself-admin; do
  cat >"$TEST_ROOT/bin/$tool" <<'EOF_TOOL'
#!/usr/bin/env bash
printf '%s %s\n' "${0##*/}" "$*" >>"$TEST_LOG"
exit 97
EOF_TOOL
done
chmod +x "$TEST_ROOT/bin/"*
export TEST_LOG="$TEST_ROOT/commands" SOURCE_ROOT
export PATH="$TEST_ROOT/bin:$PATH"

for cells in "civo-sandbox-use1-dev,$SERVING" "$BACKUP,civo-sandbox-usw2-other" "$SERVING,$BACKUP"; do
  for mode in real dry; do
    : >"$TEST_LOG"
    args=("$VERSION" --cells "$cells" --backup-evidence "$EVIDENCE_A"
      --backup-evidence "$EVIDENCE_B" --workdir "$TEST_ROOT/train")
    if [ "$mode" = dry ]; then args+=(--dry-run); fi
    status=0
    bash "$TRAIN" "${args[@]}" >"$TEST_ROOT/output" 2>&1 || status=$?
    [ "$status" -eq 2 ] || fail "$mode custom evidence pair should fail argument validation: $cells (exit $status)"
    grep -Fq -- '--backup-evidence requires --cells civo-sandbox-use1-backup,civo-sandbox-use1-serving' "$TEST_ROOT/output" ||
      fail "$mode custom pair did not explain verifier coverage"
    [ ! -s "$TEST_LOG" ] || fail "$mode custom evidence pair reached an operational command"
    [ ! -e "$TEST_ROOT/train" ] || fail "$mode custom evidence pair created a workdir"
  done
done

# Preserve both supported configurations: the default evidence pair, and an
# explicitly attested custom pair. Dry-run does not verify or mutate anything.
for selection in default explicit; do
  args=("$VERSION" --backup-evidence "$EVIDENCE_A"
    --backup-evidence "$EVIDENCE_B" --dry-run)
  if [ "$selection" = explicit ]; then args+=(--cells "$BACKUP,$SERVING"); fi
  bash "$TRAIN" "${args[@]}" >"$TEST_ROOT/output" 2>&1 || fail "$selection reviewed-cell pair was rejected"
done
bash "$TRAIN" "$VERSION" --cells "civo-sandbox-use1-dev,$SERVING" \
  --no-schema-change --dry-run >"$TEST_ROOT/output" 2>&1 || fail 'custom no-schema-change pair was rejected'

# Exercise the real live-version guard with two local desired-state snapshots.
# The second pre-merge guard must still recognize the running baseline digest
# after roll-cell has changed the checked-out values to the target digest.
# Every live read is a fixture, and all registry/network commands remain refused.
# shellcheck disable=SC1090,SC1091 # This local, sourceable script is exercised below.
source "$TRAIN"
yq() { "$ROLL_TRAIN_REAL_YQ" "$@"; }
PIN_ROOT="$TEST_ROOT/pin-repo"
PIN_VALUES="$PIN_ROOT/.gitops/cells/$BACKUP/values.yaml"
BASELINE_VALUES="$PIN_ROOT/baseline.yaml"
mkdir -p "$(dirname "$PIN_VALUES")" "$PIN_ROOT/.gitops/charts/apps" "$TEST_ROOT/live"
cp "$SOURCE_ROOT/.gitops/charts/apps/values.yaml" "$PIN_ROOT/.gitops/charts/apps/values.yaml"
export LIVE_FIXTURE_ROOT="$TEST_ROOT/live"
cat >"$TEST_ROOT/bin/kubectl" <<'EOF_LIVE_KUBECTL'
#!/usr/bin/env bash
set -euo pipefail
printf 'kubectl %s\n' "$*" >>"$TEST_LOG"
case "$*" in
  *'get applications.argoproj.io witself-server -o json') cat "$LIVE_FIXTURE_ROOT/app.json" ;;
  *'get pods -l '*) cat "$LIVE_FIXTURE_ROOT/pods.json" ;;
  *'get deployments -l '*) cat "$LIVE_FIXTURE_ROOT/deployments.json" ;;
  *) exit 97 ;;
esac
EOF_LIVE_KUBECTL
chmod +x "$TEST_ROOT/bin/kubectl"

write_pin_values() {
  local path=$1 chart=$2 tag=$3 digest_pair=$4
  jq -n --arg chart "$chart" --arg tag "$tag" --arg pair "$digest_pair" '{apps: {witselfServer: {
    chartVersion: $chart, imageTag: $tag, imageDigest: ("sha256:" + ($pair * 32)),
    worker: {enabled: false}
  }}}' >"$path"
}

write_live_pin() {
  local spec_pair=$1 running_pair=${2:-$1} deployment_pair=${3:-$1}
  jq -n --arg cell "$BACKUP" '{metadata: {labels: {"witself.io/cell": $cell}},
    status: {sync: {revision: "1.2.2"}}}' >"$LIVE_FIXTURE_ROOT/app.json"
  jq -n --arg spec "$spec_pair" --arg running "$running_pair" '
    def image($pair): "ghcr.io/witwave-ai/witself-server@sha256:" + ($pair * 32);
    {items: [{spec: {containers: [{name: "witself-server", image: image($spec)}]},
      status: {containerStatuses: [{name: "witself-server", image: image($running)}]}}]}
  ' >"$LIVE_FIXTURE_ROOT/pods.json"
  jq -n --arg pair "$deployment_pair" '{items: [{metadata: {name: "witself-server"},
    spec: {template: {spec: {containers: [{name: "witself-server",
      image: ("ghcr.io/witwave-ai/witself-server@sha256:" + ($pair * 32))}]}}}}]}
  ' >"$LIVE_FIXTURE_ROOT/deployments.json"
}

expect_pin_guard() {
  local expectation=$1 label=$2 status=0
  shift 2
  : >"$TEST_LOG"
  (require_live_not_newer "$BACKUP" witself "$PIN_VALUES" "$@") >"$TEST_ROOT/output" 2>&1 || status=$?
  case "$expectation:$status" in
    pass:0) ;;
    fail:0) fail "$label unexpectedly passed the live-version guard" ;;
    fail:*) grep -Fq 'roll-train: ERROR:' "$TEST_ROOT/output" || fail "$label failed without a guard diagnostic" ;;
    *) fail "$label was rejected by the live-version guard" ;;
  esac
  if grep -Ev '^kubectl --context witself-' "$TEST_LOG" | grep -q .; then
    fail "$label reached a command outside the fixture live reads"
  fi
}

write_pin_values "$PIN_VALUES" 1.2.2 1.2.2 ab
write_live_pin ab
expect_pin_guard pass 'matching baseline digest'
cp "$PIN_VALUES" "$BASELINE_VALUES"
write_pin_values "$PIN_VALUES" 1.2.3 1.2.3 cd
expect_pin_guard pass 'baseline digest after desired pin changes' "$BASELINE_VALUES"
write_live_pin cd
expect_pin_guard pass 'matching target digest' "$BASELINE_VALUES"
write_live_pin cd ab cd
expect_pin_guard pass 'mixed baseline running status and target requested digest' "$BASELINE_VALUES"

# A syntactically valid digest carries no sortable release version. Every live
# source must map to one of the exact, versioned local pins; guessing is unsafe.
for inventory in spec running deployment; do
  case "$inventory" in
    spec) write_live_pin ef cd cd ;;
    running) write_live_pin cd ef cd ;;
    deployment) write_live_pin cd cd ef ;;
  esac
  expect_pin_guard fail "unmapped $inventory digest" "$BASELINE_VALUES"
done
write_live_pin cd
expect_pin_guard pass 'all matching digests after unknown-digest cases' "$BASELINE_VALUES"

# Keep Argo's reported revision old so the pin's recorded release versions,
# rather than the independent Argo check, must prevent this downgrade.
write_pin_values "$PIN_VALUES" 1.2.4 1.2.4 cd
expect_pin_guard fail 'newer live digest release'
write_pin_values "$PIN_VALUES" 1.2.2 1.2.4 cd
expect_pin_guard fail 'newer imageTag beside the live digest'
write_pin_values "$PIN_VALUES" 1.2.4 1.2.2 cd
expect_pin_guard fail 'newer chartVersion beside the live digest'
write_pin_values "$PIN_VALUES" 1.2.3 1.2.3 cd
write_pin_values "$BASELINE_VALUES" 1.2.4 1.2.4 ab
write_live_pin ab
expect_pin_guard fail 'newer live baseline digest after desired pin changes' "$BASELINE_VALUES"
write_pin_values "$BASELINE_VALUES" 1.2.4 1.2.4 cd
write_live_pin cd
expect_pin_guard fail 'target digest also recorded by newer baseline release' "$BASELINE_VALUES"

write_pin_values "$PIN_VALUES" 1.2.2 1.2.2 ab
write_live_pin ab
jq '.items[0].spec.containers[0].image = "ghcr.io/witwave-ai/witself-server@sha256:short"' \
  "$LIVE_FIXTURE_ROOT/pods.json" >"$TEST_ROOT/changed-pods.json"
mv "$TEST_ROOT/changed-pods.json" "$LIVE_FIXTURE_ROOT/pods.json"
expect_pin_guard fail 'malformed live digest'
write_live_pin ab
jq '.apps.witselfServer.imageDigest = "sha256:short"' "$PIN_VALUES" >"$TEST_ROOT/changed-values.json"
mv "$TEST_ROOT/changed-values.json" "$PIN_VALUES"
expect_pin_guard fail 'malformed desired digest'

# jq's end-of-line anchor can match before a final newline, and command
# substitution then strips that newline. Validate the original pin length.
write_pin_values "$PIN_VALUES" 1.2.2 1.2.2 ab
jq '.apps.witselfServer.imageDigest += "\n"' "$PIN_VALUES" >"$TEST_ROOT/changed-values.json"
mv "$TEST_ROOT/changed-values.json" "$PIN_VALUES"
expect_pin_guard fail 'desired digest with trailing newline'

# Absent and empty pins retain tag-only behavior. Other JSON/YAML shapes must
# fail even when every live workload uses an otherwise accepted version tag.
for inventory in pods deployments; do
  jq 'walk(if type == "object" and has("image") then
    .image = "ghcr.io/witwave-ai/witself-server:1.2.2" else . end)' \
    "$LIVE_FIXTURE_ROOT/$inventory.json" >"$TEST_ROOT/changed-inventory.json"
  mv "$TEST_ROOT/changed-inventory.json" "$LIVE_FIXTURE_ROOT/$inventory.json"
done
for pin_shape in absent empty null boolean array object; do
  expectation=fail
  case "$pin_shape" in
    absent) pin_filter='del(.apps.witselfServer.imageDigest)'; expectation=pass ;;
    empty) pin_filter='.apps.witselfServer.imageDigest = ""'; expectation=pass ;;
    null) pin_filter='.apps.witselfServer.imageDigest = null' ;;
    boolean) pin_filter='.apps.witselfServer.imageDigest = false' ;;
    array) pin_filter='.apps.witselfServer.imageDigest = []' ;;
    object) pin_filter='.apps.witselfServer.imageDigest = {}' ;;
  esac
  write_pin_values "$PIN_VALUES" 1.2.2 1.2.2 ab
  jq "$pin_filter" "$PIN_VALUES" >"$TEST_ROOT/changed-values.json"
  mv "$TEST_ROOT/changed-values.json" "$PIN_VALUES"
  expect_pin_guard "$expectation" "$pin_shape optional digest pin"
done

printf 'roll train evidence tests passed\n'
