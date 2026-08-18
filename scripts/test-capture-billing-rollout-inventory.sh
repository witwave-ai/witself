#!/usr/bin/env bash
set -euo pipefail

SCRIPT_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)"
CAPTURE_SCRIPT="$SCRIPT_ROOT/capture-billing-rollout-inventory.sh"
PRODUCTION_ACCOUNT_ID="8f0bf04a4e7aab3a8cc60f02cc8c8fdb"
PRODUCTION_ENDPOINT="https://${PRODUCTION_ACCOUNT_ID}.r2.cloudflarestorage.com"
TARGET_APPLICATION_ID="11111111-2222-3333-4444-555555555555"
TARGET_APPLICATION_VERSION="18"
TARGET_IMAGE_DIGEST="sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
TARGET_RELEASE_VERSION="1.2.3"
TARGET_RELEASE_COMMIT="cccccccccccccccccccccccccccccccccccccccc"
ACCESS_KEY="inventory-access-key"
SECRET_KEY="inventory-secret-sentinel"
CF_TOKEN="cloudflare-token-sentinel"

fail() {
  printf 'capture billing rollout inventory test: FAIL: %s\n' "$1" >&2
  exit 1
}

file_mode() {
  local mode
  mode="$(stat -f '%Lp' "$1" 2>/dev/null || true)"
  if [[ ! "$mode" =~ ^[0-7]{3,4}$ ]]; then
    mode="$(stat -c '%a' "$1" 2>/dev/null || true)"
  fi
  printf '%s\n' "$mode"
}

file_sha256() {
  local output
  if command -v sha256sum >/dev/null 2>&1; then
    output="$(sha256sum "$1")"
  else
    output="$(shasum -a 256 "$1")"
  fi
  printf '%s\n' "${output%%[[:space:]]*}"
}

TEST_ROOT_RAW="$(mktemp -d "${TMPDIR:-/tmp}/witself-billing-rollout-capture-test.XXXXXX")"
TEST_ROOT="$(cd "$TEST_ROOT_RAW" && pwd -P)"
cleanup() {
  local status=$?
  trap - EXIT INT TERM
  set +e
  find "$TEST_ROOT" -depth -type d -exec chmod 700 {} + 2>/dev/null
  find "$TEST_ROOT" -depth -mindepth 1 -delete 2>/dev/null || true
  rmdir "$TEST_ROOT" 2>/dev/null || true
  if [ -e "$TEST_ROOT" ] || [ -L "$TEST_ROOT" ]; then
    printf 'capture billing rollout inventory test: FAIL: test cleanup was incomplete\n' >&2
    status=1
  fi
  exit "$status"
}
trap cleanup EXIT INT TERM

write_fake_node() {
  local path="$1"
  cat >"$path" <<'FAKE_NODE'
#!/usr/bin/env bash
set -euo pipefail
case_root="$(cd "$(dirname "$0")/.." && pwd -P)"
log="$case_root/log"
state="$case_root/source-count"
fail_phase="$(cat "$case_root/fail-phase" 2>/dev/null || true)"

case ":${PATH:-}:" in
  *":$case_root/fakes:"*) exit 70 ;;
esac

[ "${CLOUDFLARE_ACCOUNT_ID:-}" = "8f0bf04a4e7aab3a8cc60f02cc8c8fdb" ] || exit 71
[ "${CLOUDFLARE_API_TOKEN:-}" = "cloudflare-token-sentinel" ] || exit 72
[ -z "${NODE_OPTIONS+x}" ] && [ -z "${NODE_PATH+x}" ] &&
  [ -z "${DYLD_INSERT_LIBRARIES+x}" ] && [ -z "${LD_PRELOAD+x}" ] &&
  [ -z "${WITSELF_CP_STRIPE_SECRET_KEY+x}" ] &&
  [ -z "${WITSELF_CP_R2_SECRET_KEY+x}" ] &&
  [ -z "${WITSELF_BILLING_INVENTORY_R2_SECRET_KEY+x}" ] &&
  [ -z "${AMBIENT_SENTINEL+x}" ] || exit 73

count=0
[ ! -f "$state" ] || count="$(cat "$state")"
count=$((count + 1))
printf '%s\n' "$count" >"$state"
case "$count" in
  1) phase=initial ;;
  2) phase=before ;;
  3) phase=after ;;
  *) exit 74 ;;
esac
printf 'source:%s\n' "$phase" >>"$log"

snapshot="$case_root/witself-control-plane-release-Ab12c3"
config="$snapshot/repository/infra/cloudflare/witself-control-plane-deploy-Cd34e5/wrangler.generated.jsonc"
source_script="$snapshot/repository/infra/cloudflare/control-plane/scripts/billing-rollout-source-fence.mjs"
initial="$case_root/evidence/initial-lifecycle-disabled.json"

[ "$1" = "$source_script" ] || exit 75
shift
[ "$1" = --config ] && [ "$2" = "$config" ] &&
  [ "$3" = --expected-account-id ] &&
  [ "$4" = 8f0bf04a4e7aab3a8cc60f02cc8c8fdb ] &&
  [ "$5" = --expected-target-application-id ] &&
  [ "$6" = 11111111-2222-3333-4444-555555555555 ] &&
  [ "$7" = --expected-target-application-version ] && [ "$8" = 18 ] &&
  [ "$9" = --expected-target-image-digest ] &&
  [ "${10}" = sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa ] &&
  [ "${11}" = --expected-target-release-version ] && [ "${12}" = 1.2.3 ] &&
  [ "${13}" = --expected-target-release-commit ] &&
  [ "${14}" = cccccccccccccccccccccccccccccccccccccccc ] || exit 76
if [ "$phase" = initial ]; then
  [ "$#" -eq 14 ] || exit 77
else
  [ "$#" -eq 16 ] &&
    [ "${15}" = --prior-lifecycle-disabled-attestation ] &&
    [ "${16}" = "$initial" ] || exit 78
fi

if [ "$fail_phase" = "source-$phase" ]; then
  printf '{"partial":true}\n'
  exit 79
fi
printf '{"phase":"%s"}\n' "$phase"
FAKE_NODE
  chmod 700 "$path"
}

write_fake_wrangler() {
  local path="$1"
  cat >"$path" <<'FAKE_WRANGLER'
#!/bin/bash
set -euo pipefail
case_root="$(cd "$(dirname "$0")/.." && pwd -P)"
printf 'invoked\n' >"$case_root/wrangler-invoked"
exit 119
FAKE_WRANGLER
  chmod 700 "$path"
}

write_fake_sleep() {
  local path="$1"
  cat >"$path" <<'FAKE_SLEEP'
#!/usr/bin/env bash
set -euo pipefail
case_root="$(cd "$(dirname "$0")/.." && pwd -P)"
[ "$#" -eq 1 ] && [ "$1" = 240 ] || exit 81
[ -z "${CLOUDFLARE_API_TOKEN+x}" ] &&
  [ -z "${WITSELF_BILLING_INVENTORY_R2_SECRET_KEY+x}" ] &&
  [ -z "${WITSELF_CP_STRIPE_SECRET_KEY+x}" ] &&
  [ -z "${AMBIENT_SENTINEL+x}" ] || exit 82
printf 'sleep:240\n' >>"$case_root/log"
[ "$(cat "$case_root/fail-phase" 2>/dev/null || true)" != sleep ]
FAKE_SLEEP
  chmod 700 "$path"
}

write_fake_sha256sum() {
  local path="$1"
  cat >"$path" <<'FAKE_SHA256SUM'
#!/usr/bin/env bash
set -euo pipefail
[ -z "${CLOUDFLARE_API_TOKEN+x}" ] &&
  [ -z "${WITSELF_BILLING_INVENTORY_R2_SECRET_KEY+x}" ] &&
  [ -z "${WITSELF_CP_STRIPE_SECRET_KEY+x}" ] &&
  [ -z "${WITSELF_CP_R2_SECRET_KEY+x}" ] &&
  [ -z "${AWS_SECRET_ACCESS_KEY+x}" ] &&
  [ -z "${AMBIENT_SENTINEL+x}" ] || exit 83
if [ -x /usr/bin/sha256sum ]; then
  exec /usr/bin/sha256sum "$@"
fi
exec /usr/bin/shasum -a 256 "$@"
FAKE_SHA256SUM
  chmod 700 "$path"
}

write_fake_control_plane() {
  local path="$1"
  cat >"$path" <<'FAKE_CONTROL_PLANE'
#!/usr/bin/env bash
set -euo pipefail
case_root="$(cd "$(dirname "$0")/.." && pwd -P)"
log="$case_root/log"
fail_phase="$(cat "$case_root/fail-phase" 2>/dev/null || true)"

ambient_absent() {
  [ -z "${WITSELF_CP_STRIPE_SECRET_KEY+x}" ] &&
    [ -z "${WITSELF_CP_R2_ACCESS_KEY+x}" ] &&
    [ -z "${WITSELF_CP_R2_SECRET_KEY+x}" ] &&
    [ -z "${CLOUDFLARE_API_TOKEN+x}" ] &&
    [ -z "${CLOUDFLARE_BASE_URL+x}" ] &&
    [ -z "${NODE_OPTIONS+x}" ] && [ -z "${NODE_PATH+x}" ] &&
    [ -z "${DYLD_INSERT_LIBRARIES+x}" ] && [ -z "${LD_PRELOAD+x}" ] &&
    [ -z "${AWS_SECRET_ACCESS_KEY+x}" ] && [ -z "${AMBIENT_SENTINEL+x}" ]
}

if [ "${1:-}" = version ]; then
  ambient_absent || exit 91
  [ -z "${WITSELF_BILLING_INVENTORY_R2_ENDPOINT+x}" ] &&
    [ -z "${WITSELF_BILLING_INVENTORY_R2_ACCESS_KEY+x}" ] || exit 92
  printf 'binary:version\n' >>"$log"
  version="$(cat "$case_root/binary-version" 2>/dev/null || printf '1.2.3')"
  printf 'witself-control-plane %s (commit cccccccccccccccccccccccccccccccccccccccc, built 2026-08-17T12:00:00Z)\n' "$version"
  exit 0
fi

[ "${1:-}" = billing-rollout-inventory ] || exit 93
case "${2:-}" in
  scan)
    printf 'binary:scan\n' >>"$log"
    ambient_absent || exit 94
    [ "${WITSELF_BILLING_INVENTORY_R2_ENDPOINT:-}" = "https://8f0bf04a4e7aab3a8cc60f02cc8c8fdb.r2.cloudflarestorage.com" ] &&
      [ "${WITSELF_BILLING_INVENTORY_R2_BUCKET:-}" = witself-control-plane ] &&
      [ "${WITSELF_BILLING_INVENTORY_R2_PREFIX:-}" = registry/ ] &&
      [ "${WITSELF_BILLING_INVENTORY_R2_ACCESS_KEY:-}" = inventory-access-key ] &&
      [ "${WITSELF_BILLING_INVENTORY_R2_SECRET_KEY:-}" = inventory-secret-sentinel ] || exit 95
    before="$case_root/evidence/source-fence-before.json"
    provisional="$case_root/evidence/registry-provisional.json"
    [ "$#" -eq 6 ] && [ "$3" = --source-fence-before ] && [ "$4" = "$before" ] &&
      [ "$5" = --provisional ] && [ "$6" = "$provisional" ] || exit 96
    if [ "$fail_phase" = scan ]; then
      printf '{"partial":true}\n' >"$provisional"
      chmod 600 "$provisional"
      exit 97
    fi
    printf '{"provisional":true}\n' >"$provisional"
    chmod 600 "$provisional"
    if [ "$fail_phase" = mutate-binary-after-scan ]; then
      printf '# mutation\n' >>"$0"
    fi
    ;;
  finalize)
    printf 'binary:finalize\n' >>"$log"
    ambient_absent || exit 98
    [ "${WITSELF_BILLING_INVENTORY_R2_ENDPOINT:-}" = "https://8f0bf04a4e7aab3a8cc60f02cc8c8fdb.r2.cloudflarestorage.com" ] &&
      [ "${WITSELF_BILLING_INVENTORY_R2_BUCKET:-}" = witself-control-plane ] &&
      [ "${WITSELF_BILLING_INVENTORY_R2_PREFIX:-}" = registry/ ] &&
      [ -z "${WITSELF_BILLING_INVENTORY_R2_ACCESS_KEY+x}" ] &&
      [ -z "${WITSELF_BILLING_INVENTORY_R2_SECRET_KEY+x}" ] || exit 99
    before="$case_root/evidence/source-fence-before.json"
    provisional="$case_root/evidence/registry-provisional.json"
    after="$case_root/evidence/source-fence-after.json"
    output="$case_root/output/inventory.json"
    [ "$#" -eq 10 ] && [ "$3" = --source-fence-before ] && [ "$4" = "$before" ] &&
      [ "$5" = --provisional ] && [ "$6" = "$provisional" ] &&
      [ "$7" = --source-fence-after ] && [ "$8" = "$after" ] &&
      [ "$9" = --output ] && [ "${10}" = "$output" ] || exit 100
    case "$fail_phase" in
      finalize-racer-file)
        printf 'racer-owned\n' >"$output"
        chmod 600 "$output"
        exit 101
        ;;
      finalize-racer-symlink)
        ln -s "$case_root/racer-sentinel" "$output"
        exit 102
        ;;
      finalize-bad-mode)
        printf '{"schema":"witself.billing-rollout-inventory.v1"}\n' >"$output"
        chmod 644 "$output"
        ;;
      finalize-symlink-success)
        ln -s "$case_root/racer-sentinel" "$output"
        ;;
      finalize-mutate-binary)
        printf '{"schema":"witself.billing-rollout-inventory.v1"}\n' >"$output"
        chmod 600 "$output"
        printf '# mutation\n' >>"$0"
        ;;
      finalize-mutate-before)
        printf '{"schema":"witself.billing-rollout-inventory.v1"}\n' >"$output"
        chmod 600 "$output"
        printf '{"replaced":true}\n' >"$before"
        chmod 600 "$before"
        ;;
      finalize)
        printf '{"partial":true}\n' >"$output"
        chmod 600 "$output"
        exit 103
        ;;
      *)
        printf '%s\n' '{"schema":"witself.billing-rollout-inventory.v1","captured_at":"2026-08-17T12:05:00Z","billing_mutation_cohort_accounts":0,"source_fleet":{"api_replicas":0,"reconciler_replicas":0},"records":{"prepared_downgrades":0,"targetless_pending_changes":0,"malformed_pending_changes":0,"malformed_mutation_receipts":0,"post_retry_horizon_receipts":0}}' >"$output"
        chmod 600 "$output"
        ;;
    esac
    ;;
  *) exit 104 ;;
esac
FAKE_CONTROL_PLANE
  chmod 700 "$path"
}

make_fixture() {
  local name="$1"
  CASE_ROOT="$TEST_ROOT/$name"
  SNAPSHOT_ROOT="$CASE_ROOT/witself-control-plane-release-Ab12c3"
  REPOSITORY_ROOT="$SNAPSHOT_ROOT/repository"
  CLOUDFLARE_ROOT="$REPOSITORY_ROOT/infra/cloudflare"
  CONTROL_PLANE_ROOT="$CLOUDFLARE_ROOT/control-plane"
  CONFIG_DIR="$CLOUDFLARE_ROOT/witself-control-plane-deploy-Cd34e5"
  CONFIG="$CONFIG_DIR/wrangler.generated.jsonc"
  BINARY="$CASE_ROOT/bin/witself-control-plane"
  WORK_DIR="$CASE_ROOT/evidence"
  OUTPUT="$CASE_ROOT/output/inventory.json"
  LOG="$CASE_ROOT/log"
  STDOUT_FILE="$CASE_ROOT/stdout"
  STDERR_FILE="$CASE_ROOT/stderr"

  mkdir -m 700 "$CASE_ROOT" "$SNAPSHOT_ROOT" "$SNAPSHOT_ROOT/work" \
    "$REPOSITORY_ROOT" "$REPOSITORY_ROOT/infra" "$CLOUDFLARE_ROOT" \
    "$CONTROL_PLANE_ROOT" "$CONTROL_PLANE_ROOT/scripts" "$CONTROL_PLANE_ROOT/src" \
    "$CONFIG_DIR" "$CONFIG_DIR/.wrangler" "$CONFIG_DIR/.wrangler/tmp" \
    "$CASE_ROOT/bin" "$CASE_ROOT/fakes" "$CASE_ROOT/output"
  printf '{}\n' >"$CONFIG"
  printf '// immutable fake source command\n' >"$CONTROL_PLANE_ROOT/scripts/billing-rollout-source-fence.mjs"
  printf '// immutable fake entrypoint\n' >"$CONTROL_PLANE_ROOT/src/index.js"
  printf '# Intentionally empty: production Wrangler commands must not load local dotenv files.\n' \
    >"$CLOUDFLARE_ROOT/wrangler-production-empty.env"
  : >"$LOG"
  printf 'sentinel\n' >"$CASE_ROOT/racer-sentinel"

  chmod 400 "$CONFIG"
  chmod 444 "$CONTROL_PLANE_ROOT/scripts/billing-rollout-source-fence.mjs" \
    "$CONTROL_PLANE_ROOT/src/index.js" "$CLOUDFLARE_ROOT/wrangler-production-empty.env"
  chmod 555 "$REPOSITORY_ROOT" "$REPOSITORY_ROOT/infra" "$CLOUDFLARE_ROOT" \
    "$CONTROL_PLANE_ROOT" "$CONTROL_PLANE_ROOT/scripts" "$CONTROL_PLANE_ROOT/src"
  chmod 700 "$CONFIG_DIR" "$CONFIG_DIR/.wrangler" "$CONFIG_DIR/.wrangler/tmp"

  write_fake_node "$CASE_ROOT/fakes/node"
  write_fake_wrangler "$CASE_ROOT/fakes/wrangler"
  write_fake_sleep "$CASE_ROOT/fakes/sleep"
  write_fake_sha256sum "$CASE_ROOT/fakes/sha256sum"
  write_fake_control_plane "$BINARY"
  BINARY_SHA256="$(file_sha256 "$BINARY")"
}

invoke_capture() {
  local ordinary_access="${ORDINARY_ACCESS_OVERRIDE:-ordinary-access-sentinel}"
  local ordinary_secret="${ORDINARY_SECRET_OVERRIDE:-ordinary-secret-sentinel}"
  local -a args=(
    --release-snapshot-config "$CONFIG"
    --control-plane-binary "$BINARY"
    --control-plane-binary-sha256 "$BINARY_SHA256"
    --work-dir "$WORK_DIR"
    --output "$OUTPUT"
    --expected-account-id "$PRODUCTION_ACCOUNT_ID"
    --expected-target-application-id "$TARGET_APPLICATION_ID"
    --expected-target-application-version "$TARGET_APPLICATION_VERSION"
    --expected-target-image-digest "$TARGET_IMAGE_DIGEST"
    --expected-target-release-version "$TARGET_RELEASE_VERSION"
    --expected-target-release-commit "$TARGET_RELEASE_COMMIT"
  )
  if (( $# > 0 )); then
    args+=("$@")
  fi
  PATH="$CASE_ROOT/fakes:/usr/bin:/bin:/usr/sbin:/sbin" \
  HOME="$CASE_ROOT/home" TMPDIR="$CASE_ROOT" LANG=C TZ=UTC \
  CLOUDFLARE_ACCOUNT_ID="$PRODUCTION_ACCOUNT_ID" \
  CLOUDFLARE_API_TOKEN="$CF_TOKEN" \
  WITSELF_BILLING_INVENTORY_R2_ENDPOINT="$PRODUCTION_ENDPOINT" \
  WITSELF_BILLING_INVENTORY_R2_BUCKET=witself-control-plane \
  WITSELF_BILLING_INVENTORY_R2_PREFIX=registry/ \
  WITSELF_BILLING_INVENTORY_R2_ACCESS_KEY="$ACCESS_KEY" \
  WITSELF_BILLING_INVENTORY_R2_SECRET_KEY="$SECRET_KEY" \
  WITSELF_CP_STRIPE_SECRET_KEY=stripe-ambient-sentinel \
  WITSELF_CP_R2_ACCESS_KEY="$ordinary_access" \
  WITSELF_CP_R2_SECRET_KEY="$ordinary_secret" \
  CLOUDFLARE_BASE_URL=https://ambient.invalid \
  NODE_OPTIONS=--no-warnings \
  NODE_PATH=/ambient/node/path \
  DYLD_INSERT_LIBRARIES='' \
  LD_PRELOAD='' \
  AWS_SECRET_ACCESS_KEY=aws-ambient-sentinel \
  AMBIENT_SENTINEL=ambient-value \
    bash "$CAPTURE_SCRIPT" "${args[@]}" >"$STDOUT_FILE" 2>"$STDERR_FILE"
}

assert_no_secret_leak() {
  if grep -F -e "$SECRET_KEY" -e "$CF_TOKEN" -e stripe-ambient-sentinel \
      -e ordinary-secret-sentinel -e aws-ambient-sentinel \
      "$STDOUT_FILE" "$STDERR_FILE" "$LOG" >/dev/null 2>&1; then
    fail "a secret sentinel leaked"
  fi
}

make_fixture success
invoke_capture || fail "happy path failed"
[ "$(cat "$STDOUT_FILE")" = "billing rollout inventory capture: OK" ] ||
  fail "happy path stdout was not value-free"
[ ! -s "$STDERR_FILE" ] || fail "happy path wrote stderr"
expected_log="$(printf '%s\n' \
  binary:version source:initial sleep:240 source:before binary:scan source:after binary:finalize)"
[ "$(cat "$LOG")" = "$expected_log" ] || fail "phase order was not exact"
[ ! -e "$CASE_ROOT/wrangler-invoked" ] ||
  fail "PATH-shadowed Wrangler was invoked"
[ -d "$WORK_DIR" ] && [ "$(file_mode "$WORK_DIR")" = 700 ] ||
  fail "successful private work directory was not retained at mode 0700"
for artifact in \
  "$WORK_DIR/initial-lifecycle-disabled.json" \
  "$WORK_DIR/source-fence-before.json" \
  "$WORK_DIR/registry-provisional.json" \
  "$WORK_DIR/source-fence-after.json" "$OUTPUT"; do
  [ -f "$artifact" ] && [ ! -L "$artifact" ] && [ "$(file_mode "$artifact")" = 600 ] ||
    fail "a successful artifact was not retained at mode 0600"
done
assert_no_secret_leak

make_fixture scan_failure
printf 'scan\n' >"$CASE_ROOT/fail-phase"
if invoke_capture; then fail "scan failure was accepted"; fi
[ ! -e "$WORK_DIR" ] && [ ! -L "$WORK_DIR" ] ||
  fail "scan failure left private partials"
[ ! -e "$OUTPUT" ] && [ ! -L "$OUTPUT" ] || fail "scan failure left output"
assert_no_secret_leak

make_fixture source_failure
printf 'source-before\n' >"$CASE_ROOT/fail-phase"
if invoke_capture; then fail "source failure was accepted"; fi
[ ! -e "$WORK_DIR" ] && [ ! -L "$WORK_DIR" ] ||
  fail "source failure left private partials"
assert_no_secret_leak

make_fixture finalize_bad_mode
printf 'finalize-bad-mode\n' >"$CASE_ROOT/fail-phase"
if invoke_capture; then fail "bad final output mode was accepted"; fi
[ ! -e "$OUTPUT" ] && [ ! -L "$OUTPUT" ] ||
  fail "owned bad final output was not removed"
[ ! -e "$WORK_DIR" ] || fail "post-create failure left private partials"

make_fixture finalize_mutated_binary
printf 'finalize-mutate-binary\n' >"$CASE_ROOT/fail-phase"
if invoke_capture; then fail "post-finalize binary mutation was accepted"; fi
[ ! -e "$OUTPUT" ] && [ ! -L "$OUTPUT" ] ||
  fail "post-create stability failure left output"
[ ! -e "$WORK_DIR" ] || fail "post-create stability failure left private partials"

make_fixture finalize_mutated_evidence
printf 'finalize-mutate-before\n' >"$CASE_ROOT/fail-phase"
if invoke_capture; then fail "post-finalize evidence mutation was accepted"; fi
[ ! -e "$OUTPUT" ] && [ ! -L "$OUTPUT" ] ||
  fail "evidence stability failure left output"
[ ! -e "$WORK_DIR" ] || fail "evidence stability failure left private partials"

make_fixture finalize_racer_file
printf 'finalize-racer-file\n' >"$CASE_ROOT/fail-phase"
if invoke_capture; then fail "failed finalize with racer file was accepted"; fi
[ -f "$OUTPUT" ] && [ "$(cat "$OUTPUT")" = racer-owned ] ||
  fail "unknown racer file was deleted after failed finalize"

make_fixture finalize_racer_symlink
printf 'finalize-racer-symlink\n' >"$CASE_ROOT/fail-phase"
if invoke_capture; then fail "failed finalize with racer symlink was accepted"; fi
[ -L "$OUTPUT" ] && [ "$(cat "$CASE_ROOT/racer-sentinel")" = sentinel ] ||
  fail "unknown racer symlink was deleted or followed after failed finalize"

make_fixture finalize_owned_symlink
printf 'finalize-symlink-success\n' >"$CASE_ROOT/fail-phase"
if invoke_capture; then fail "successful finalize symlink was accepted"; fi
[ ! -e "$OUTPUT" ] && [ ! -L "$OUTPUT" ] ||
  fail "owned invalid symlink output was not removed"
[ "$(cat "$CASE_ROOT/racer-sentinel")" = sentinel ] ||
  fail "owned invalid symlink output was followed"

make_fixture binary_hash_mismatch
bad_sha="${BINARY_SHA256%?}0"
[ "$bad_sha" != "$BINARY_SHA256" ] || bad_sha="${BINARY_SHA256%?}1"
BINARY_SHA256="$bad_sha"
if invoke_capture; then fail "binary SHA-256 mismatch was accepted"; fi
[ ! -e "$WORK_DIR" ] || fail "binary SHA-256 refusal created a work directory"

make_fixture binary_version_mismatch
printf '9.9.9\n' >"$CASE_ROOT/binary-version"
if invoke_capture; then fail "binary version mismatch was accepted"; fi
[ ! -e "$WORK_DIR" ] || fail "binary version refusal created a work directory"

make_fixture work_reuse
mkdir -m 700 "$WORK_DIR"
printf 'owned\n' >"$WORK_DIR/sentinel"
if invoke_capture; then fail "reused work directory was accepted"; fi
[ "$(cat "$WORK_DIR/sentinel")" = owned ] || fail "reused work directory was modified"

make_fixture work_symlink
mkdir -m 700 "$CASE_ROOT/other-work"
printf 'owned\n' >"$CASE_ROOT/other-work/sentinel"
ln -s "$CASE_ROOT/other-work" "$WORK_DIR"
if invoke_capture; then fail "symlink work directory was accepted"; fi
[ "$(cat "$CASE_ROOT/other-work/sentinel")" = owned ] ||
  fail "symlink work target was modified"

make_fixture output_exists
printf 'owned\n' >"$OUTPUT"
chmod 600 "$OUTPUT"
if invoke_capture; then fail "existing output was accepted"; fi
[ "$(cat "$OUTPUT")" = owned ] || fail "existing output was modified"

make_fixture output_symlink
ln -s "$CASE_ROOT/racer-sentinel" "$OUTPUT"
if invoke_capture; then fail "symlink output was accepted"; fi
[ -L "$OUTPUT" ] && [ "$(cat "$CASE_ROOT/racer-sentinel")" = sentinel ] ||
  fail "pre-existing output symlink was modified or followed"

make_fixture output_parent_symlink
rmdir "$CASE_ROOT/output"
mkdir -m 700 "$CASE_ROOT/real-output"
ln -s "$CASE_ROOT/real-output" "$CASE_ROOT/output"
if invoke_capture; then fail "symlink output parent was accepted"; fi
[ ! -e "$CASE_ROOT/real-output/inventory.json" ] ||
  fail "symlink output parent target was modified"

make_fixture snapshot_config_symlink
mv "$CONFIG" "$CONFIG.real"
ln -s "$CONFIG.real" "$CONFIG"
if invoke_capture; then fail "symlink release snapshot config was accepted"; fi
[ ! -e "$WORK_DIR" ] || fail "snapshot symlink refusal created a work directory"

make_fixture wrong_authority
if PATH="$CASE_ROOT/fakes:/usr/bin:/bin:/usr/sbin:/sbin" \
    CLOUDFLARE_ACCOUNT_ID="$PRODUCTION_ACCOUNT_ID" CLOUDFLARE_API_TOKEN="$CF_TOKEN" \
    WITSELF_BILLING_INVENTORY_R2_ENDPOINT="$PRODUCTION_ENDPOINT" \
    WITSELF_BILLING_INVENTORY_R2_BUCKET=wrong-empty-bucket \
    WITSELF_BILLING_INVENTORY_R2_PREFIX=registry/ \
    WITSELF_BILLING_INVENTORY_R2_ACCESS_KEY="$ACCESS_KEY" \
    WITSELF_BILLING_INVENTORY_R2_SECRET_KEY="$SECRET_KEY" \
    bash "$CAPTURE_SCRIPT" \
      --release-snapshot-config "$CONFIG" --control-plane-binary "$BINARY" \
      --control-plane-binary-sha256 "$BINARY_SHA256" --work-dir "$WORK_DIR" \
      --output "$OUTPUT" --expected-account-id "$PRODUCTION_ACCOUNT_ID" \
      --expected-target-application-id "$TARGET_APPLICATION_ID" \
      --expected-target-application-version "$TARGET_APPLICATION_VERSION" \
      --expected-target-image-digest "$TARGET_IMAGE_DIGEST" \
      --expected-target-release-version "$TARGET_RELEASE_VERSION" \
      --expected-target-release-commit "$TARGET_RELEASE_COMMIT" \
      >"$STDOUT_FILE" 2>"$STDERR_FILE"; then
  fail "wrong registry authority was accepted"
fi
[ ! -e "$WORK_DIR" ] || fail "authority refusal created a work directory"

make_fixture reused_access_key
ORDINARY_ACCESS_OVERRIDE="$ACCESS_KEY"
if invoke_capture; then fail "reused ordinary R2 access key was accepted"; fi
unset ORDINARY_ACCESS_OVERRIDE
[ ! -e "$WORK_DIR" ] || fail "access-key reuse refusal created a work directory"

make_fixture reused_secret_key
ORDINARY_SECRET_OVERRIDE="$SECRET_KEY"
if invoke_capture; then fail "reused ordinary R2 secret key was accepted"; fi
unset ORDINARY_SECRET_OVERRIDE
[ ! -e "$WORK_DIR" ] || fail "secret-key reuse refusal created a work directory"

make_fixture exported_helper_injection
HELPER_MARKER="$CASE_ROOT/exported-helper-ran"
export HELPER_MARKER
# Invoked only if the child wrapper fails to remove the exported injection.
# shellcheck disable=SC2329
dirname() {
  printf '%s\n' "${WITSELF_BILLING_INVENTORY_R2_SECRET_KEY:-missing}" \
    >"$HELPER_MARKER"
  /usr/bin/dirname "$@"
}
export -f dirname
helper_status=0
invoke_capture || helper_status=$?
unset -f dirname
unset HELPER_MARKER
[ "$helper_status" -eq 0 ] || fail "exported helper function broke the capture"
[ ! -e "$CASE_ROOT/exported-helper-ran" ] ||
  fail "an exported helper function ran inside the wrapper"
assert_no_secret_leak

make_fixture timestamp_argument
if invoke_capture --captured-at 2026-08-17T12:00:00Z; then
  fail "caller timestamp was accepted"
fi
[ ! -e "$WORK_DIR" ] || fail "caller timestamp refusal created a work directory"

make_fixture xtrace_nonleak
if PATH="$CASE_ROOT/fakes:/usr/bin:/bin:/usr/sbin:/sbin" \
    HOME="$CASE_ROOT/home" TMPDIR="$CASE_ROOT" LANG=C TZ=UTC \
    CLOUDFLARE_ACCOUNT_ID="$PRODUCTION_ACCOUNT_ID" CLOUDFLARE_API_TOKEN="$CF_TOKEN" \
    WITSELF_BILLING_INVENTORY_R2_ENDPOINT="$PRODUCTION_ENDPOINT" \
    WITSELF_BILLING_INVENTORY_R2_BUCKET=witself-control-plane \
    WITSELF_BILLING_INVENTORY_R2_PREFIX=registry/ \
    WITSELF_BILLING_INVENTORY_R2_ACCESS_KEY="$ACCESS_KEY" \
    WITSELF_BILLING_INVENTORY_R2_SECRET_KEY="$SECRET_KEY" \
    bash -x "$CAPTURE_SCRIPT" \
      --release-snapshot-config "$CONFIG" --control-plane-binary "$BINARY" \
      --control-plane-binary-sha256 "$BINARY_SHA256" --work-dir "$WORK_DIR" \
      --output "$OUTPUT" --expected-account-id wrong \
      --expected-target-application-id "$TARGET_APPLICATION_ID" \
      --expected-target-application-version "$TARGET_APPLICATION_VERSION" \
      --expected-target-image-digest "$TARGET_IMAGE_DIGEST" \
      --expected-target-release-version "$TARGET_RELEASE_VERSION" \
      --expected-target-release-commit "$TARGET_RELEASE_COMMIT" \
      >"$STDOUT_FILE" 2>"$STDERR_FILE"; then
  fail "xtrace refusal case unexpectedly succeeded"
fi
if grep -F -e "$SECRET_KEY" -e "$CF_TOKEN" "$STDOUT_FILE" "$STDERR_FILE" >/dev/null; then
  fail "xtrace leaked a credential sentinel"
fi

printf 'capture billing rollout inventory tests: PASS (23 cases)\n'
