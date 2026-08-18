#!/usr/bin/env bash
# Capture the production billing-rollout registry inventory between two exact
# Cloudflare source-fleet observations. Successful private evidence is retained
# in WORK_DIR; failed captures remove only the fixed artifacts created there.
builtin set +x
builtin set -euo pipefail
builtin export -n SHELLOPTS BASHOPTS 2>/dev/null || :
builtin shopt -u expand_aliases
builtin unalias -a 2>/dev/null || true
# No wrapper functions have been declared yet, so the exact allowed imported
# function set is empty. This prevents an exported dirname/stat/hash shim from
# observing the subsequently captured non-exported credentials.
while IFS= builtin read -r inherited_function; do
  builtin test -n "$inherited_function" || continue
  builtin unset -f "$inherited_function" 2>/dev/null || :
done < <(builtin compgen -A function)
builtin unset inherited_function

readonly PRODUCTION_ACCOUNT_ID="8f0bf04a4e7aab3a8cc60f02cc8c8fdb"
readonly PRODUCTION_R2_ENDPOINT="https://${PRODUCTION_ACCOUNT_ID}.r2.cloudflarestorage.com"
readonly PRODUCTION_R2_BUCKET="witself-control-plane"
readonly PRODUCTION_R2_PREFIX="registry/"
readonly IN_FLIGHT_BOUND_SECONDS=240
readonly MAX_PRIVATE_ARTIFACT_BYTES=131072

usage() {
  cat <<'EOF'
usage: capture-billing-rollout-inventory.sh \
  --release-snapshot-config ABS_PRIVATE_0400_WRANGLER_CONFIG \
  --control-plane-binary ABS_WITSELF_CONTROL_PLANE \
  --control-plane-binary-sha256 LOWERCASE_SHA256 \
  --work-dir ABSENT_ABSOLUTE_DIRECTORY \
  --output ABSENT_ABSOLUTE_FILE \
  --expected-account-id 8f0bf04a4e7aab3a8cc60f02cc8c8fdb \
  --expected-target-application-id LOWERCASE_UUID \
  --expected-target-application-version POSITIVE_INTEGER \
  --expected-target-image-digest sha256:LOWERCASE_SHA256 \
  --expected-target-release-version X.Y.Z \
  --expected-target-release-commit FULL_LOWERCASE_40_HEX

The release snapshot and output parent must already exist at real, canonical
absolute paths. WORK_DIR and OUTPUT must not exist. The command takes its own
initial source observation, waits the fixed four-minute in-flight bound, scans
the exact production registry, and captures the post-scan source observation.
The snapshot must come from the tagged control-plane release-snapshot generator
and remain in operator custody for the complete capture. Retain that generator's
source_sha256 evidence with the successful private WORK_DIR evidence.

Required dedicated read-only registry environment:
  WITSELF_BILLING_INVENTORY_R2_ENDPOINT
  WITSELF_BILLING_INVENTORY_R2_BUCKET
  WITSELF_BILLING_INVENTORY_R2_PREFIX
  WITSELF_BILLING_INVENTORY_R2_ACCESS_KEY
  WITSELF_BILLING_INVENTORY_R2_SECRET_KEY
EOF
}

die() {
  printf 'billing rollout inventory capture: FAIL: %s\n' "$1" >&2
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

file_identity() {
  local identity
  identity="$(stat -f '%d:%i:%z:%m' "$1" 2>/dev/null || true)"
  if [[ ! "$identity" =~ ^[0-9]+:[0-9]+:[0-9]+:[0-9]+$ ]]; then
    identity="$(stat -c '%d:%i:%s:%Y' "$1" 2>/dev/null || true)"
  fi
  printf '%s\n' "$identity"
}

directory_identity() {
  local identity
  identity="$(stat -f '%d:%i' "$1" 2>/dev/null || true)"
  if [[ ! "$identity" =~ ^[0-9]+:[0-9]+$ ]]; then
    identity="$(stat -c '%d:%i' "$1" 2>/dev/null || true)"
  fi
  printf '%s\n' "$identity"
}

canonical_real_directory() {
  local path="$1"
  local resolved
  case "$path" in /*) ;; *) return 1 ;; esac
  [[ ! "$path" =~ [[:cntrl:]] ]] || return 1
  [ -d "$path" ] && [ ! -L "$path" ] || return 1
  resolved="$(cd "$path" 2>/dev/null && pwd -P)" || return 1
  [ "$resolved" = "$path" ] || return 1
}

canonical_real_file() {
  local path="$1"
  local parent base resolved_parent
  case "$path" in /*) ;; *) return 1 ;; esac
  [[ ! "$path" =~ [[:cntrl:]] ]] || return 1
  parent="$(dirname "$path")"
  base="$(basename "$path")"
  [ "$base" != . ] && [ "$base" != .. ] || return 1
  canonical_real_directory "$parent" || return 1
  resolved_parent="$(cd "$parent" 2>/dev/null && pwd -P)" || return 1
  [ "$path" = "$resolved_parent/$base" ] || return 1
  [ -f "$path" ] && [ ! -L "$path" ] || return 1
}

canonical_absent_path() {
  local path="$1"
  local parent base resolved_parent
  case "$path" in /*) ;; *) return 1 ;; esac
  [[ ! "$path" =~ [[:cntrl:]] ]] || return 1
  parent="$(dirname "$path")"
  base="$(basename "$path")"
  [ "$base" != . ] && [ "$base" != .. ] || return 1
  canonical_real_directory "$parent" || return 1
  resolved_parent="$(cd "$parent" 2>/dev/null && pwd -P)" || return 1
  [ "$path" = "$resolved_parent/$base" ] || return 1
  [ ! -e "$path" ] && [ ! -L "$path" ] || return 1
}

require_exact_mode() {
  local path="$1"
  local expected="$2"
  local mode
  mode="$(file_mode "$path")"
  [ "$mode" = "$expected" ] || return 1
}

require_private_artifact() {
  local path="$1"
  local size
  canonical_real_file "$path" || die "a private evidence artifact was not a real regular file"
  require_exact_mode "$path" 600 || die "a private evidence artifact did not have mode 0600"
  size="$(wc -c <"$path" | tr -d '[:space:]')"
  if [[ ! "$size" =~ ^[0-9]+$ ]] ||
     (( size < 1 || size > MAX_PRIVATE_ARTIFACT_BYTES )); then
    die "a private evidence artifact had an invalid size"
  fi
}

assert_private_artifact_stable() {
  local path="$1"
  local expected_identity="$2"
  local expected_sha256="$3"
  require_private_artifact "$path"
  [ "$(file_identity "$path")" = "$expected_identity" ] &&
    [ "$(file_sha256 "$path")" = "$expected_sha256" ] ||
    die "private evidence changed after it was captured"
}

stable_file_identity() {
  local path="$1"
  local expected_identity="$2"
  local expected_mode="$3"
  local expected_sha256="$4"
  canonical_real_file "$path" || return 1
  [ "$(file_identity "$path")" = "$expected_identity" ] || return 1
  require_exact_mode "$path" "$expected_mode" || return 1
  [ "$(file_sha256 "$path")" = "$expected_sha256" ]
}

stable_executable_identity() {
  local path="$1"
  local expected_identity="$2"
  local expected_sha256="$3"
  local mode
  canonical_real_file "$path" || return 1
  [ "$(file_identity "$path")" = "$expected_identity" ] || return 1
  [ "$(file_sha256 "$path")" = "$expected_sha256" ] || return 1
  [ -x "$path" ] || return 1
  mode="$(file_mode "$path")"
  [[ "$mode" =~ ^[0-7]{3,4}$ ]] || return 1
  (( (8#$mode & 8#111) != 0 && (8#$mode & 8#022) == 0 ))
}

file_sha256() {
  local output digest
  case "$sha256_kind" in
    sha256sum) output="$("$sha256_binary" "$1")" || return 1 ;;
    shasum) output="$("$sha256_binary" -a 256 "$1")" || return 1 ;;
    *) return 1 ;;
  esac
  digest="${output%%[[:space:]]*}"
  [[ "$digest" =~ ^[0-9a-f]{64}$ ]] || return 1
  printf '%s\n' "$digest"
}

scrub_environment_except() {
  local allowed=",$1,"
  local env_name
  while IFS= read -r env_name; do
    [[ "$env_name" =~ ^[A-Za-z_][A-Za-z0-9_]*$ ]] || return 1
    case "$allowed" in
      *",$env_name,"*) ;;
      *) unset "$env_name" 2>/dev/null || return 1 ;;
    esac
  done < <(compgen -e)
}

run_source_phase() (
  scrub_environment_except \
    "HOME,TMPDIR,TMP,TEMP,LANG,TZ"
  CLOUDFLARE_ACCOUNT_ID="$cloudflare_account_id" \
  CLOUDFLARE_API_TOKEN="$cloudflare_api_token" \
    "$@"
)

run_scan_phase() (
  scrub_environment_except \
    "PATH"
  WITSELF_BILLING_INVENTORY_R2_ENDPOINT="$r2_endpoint" \
  WITSELF_BILLING_INVENTORY_R2_BUCKET="$r2_bucket" \
  WITSELF_BILLING_INVENTORY_R2_PREFIX="$r2_prefix" \
  WITSELF_BILLING_INVENTORY_R2_ACCESS_KEY="$r2_access_key" \
  WITSELF_BILLING_INVENTORY_R2_SECRET_KEY="$r2_secret_key" \
    "$@"
)

run_finalize_phase() (
  scrub_environment_except \
    "PATH"
  WITSELF_BILLING_INVENTORY_R2_ENDPOINT="$r2_endpoint" \
  WITSELF_BILLING_INVENTORY_R2_BUCKET="$r2_bucket" \
  WITSELF_BILLING_INVENTORY_R2_PREFIX="$r2_prefix" \
    "$@"
)

run_runtime_phase() (
  scrub_environment_except "PATH"
  "$@"
)

release_snapshot_config=""
control_plane_binary=""
control_plane_binary_sha256=""
work_dir=""
output=""
expected_account_id=""
expected_target_application_id=""
expected_target_application_version=""
expected_target_image_digest=""
expected_target_release_version=""
expected_target_release_commit=""

seen_release_snapshot_config=false
seen_control_plane_binary=false
seen_control_plane_binary_sha256=false
seen_work_dir=false
seen_output=false
seen_expected_account_id=false
seen_expected_target_application_id=false
seen_expected_target_application_version=false
seen_expected_target_image_digest=false
seen_expected_target_release_version=false
seen_expected_target_release_commit=false

if (( $# == 1 )) && [[ "$1" = --help || "$1" = -h ]]; then
  usage
  exit 0
fi

while (( $# > 0 )); do
  (( $# >= 2 )) || { usage >&2; die "an argument was missing its value"; }
  name="$1"
  value="$2"
  shift 2
  [ -n "$value" ] || die "an argument had an empty value"
  case "$name" in
    --release-snapshot-config)
      [ "$seen_release_snapshot_config" = false ] || die "an argument was duplicated"
      seen_release_snapshot_config=true
      release_snapshot_config="$value"
      ;;
    --control-plane-binary)
      [ "$seen_control_plane_binary" = false ] || die "an argument was duplicated"
      seen_control_plane_binary=true
      control_plane_binary="$value"
      ;;
    --control-plane-binary-sha256)
      [ "$seen_control_plane_binary_sha256" = false ] || die "an argument was duplicated"
      seen_control_plane_binary_sha256=true
      control_plane_binary_sha256="$value"
      ;;
    --work-dir)
      [ "$seen_work_dir" = false ] || die "an argument was duplicated"
      seen_work_dir=true
      work_dir="$value"
      ;;
    --output)
      [ "$seen_output" = false ] || die "an argument was duplicated"
      seen_output=true
      output="$value"
      ;;
    --expected-account-id)
      [ "$seen_expected_account_id" = false ] || die "an argument was duplicated"
      seen_expected_account_id=true
      expected_account_id="$value"
      ;;
    --expected-target-application-id)
      [ "$seen_expected_target_application_id" = false ] || die "an argument was duplicated"
      seen_expected_target_application_id=true
      expected_target_application_id="$value"
      ;;
    --expected-target-application-version)
      [ "$seen_expected_target_application_version" = false ] || die "an argument was duplicated"
      seen_expected_target_application_version=true
      expected_target_application_version="$value"
      ;;
    --expected-target-image-digest)
      [ "$seen_expected_target_image_digest" = false ] || die "an argument was duplicated"
      seen_expected_target_image_digest=true
      expected_target_image_digest="$value"
      ;;
    --expected-target-release-version)
      [ "$seen_expected_target_release_version" = false ] || die "an argument was duplicated"
      seen_expected_target_release_version=true
      expected_target_release_version="$value"
      ;;
    --expected-target-release-commit)
      [ "$seen_expected_target_release_commit" = false ] || die "an argument was duplicated"
      seen_expected_target_release_commit=true
      expected_target_release_commit="$value"
      ;;
    *) die "an unknown argument was supplied" ;;
  esac
done

for required in \
  "$seen_release_snapshot_config" \
  "$seen_control_plane_binary" \
  "$seen_control_plane_binary_sha256" \
  "$seen_work_dir" \
  "$seen_output" \
  "$seen_expected_account_id" \
  "$seen_expected_target_application_id" \
  "$seen_expected_target_application_version" \
  "$seen_expected_target_image_digest" \
  "$seen_expected_target_release_version" \
  "$seen_expected_target_release_commit"; do
  [ "$required" = true ] || { usage >&2; die "required arguments were incomplete"; }
done

[ "$expected_account_id" = "$PRODUCTION_ACCOUNT_ID" ] ||
  die "the expected account must be the exact production account"
[[ "$expected_target_application_id" =~ ^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$ ]] ||
  die "the expected target application id was invalid"
[[ "$expected_target_application_version" =~ ^[1-9][0-9]{0,15}$ ]] ||
  die "the expected target application version was invalid"
if (( ${#expected_target_application_version} == 16 )) &&
   (( 10#$expected_target_application_version > 9007199254740991 )); then
  die "the expected target application version exceeded the JSON safe-integer limit"
fi
[[ "$expected_target_image_digest" =~ ^sha256:[0-9a-f]{64}$ ]] ||
  die "the expected target image digest was invalid"
[[ "$expected_target_release_version" =~ ^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]] ||
  die "the expected target release version was invalid"
[[ "$expected_target_release_commit" =~ ^[0-9a-f]{40}$ ]] ||
  die "the expected target release commit was invalid"
[[ "$control_plane_binary_sha256" =~ ^[0-9a-f]{64}$ ]] ||
  die "the expected control-plane binary SHA-256 was invalid"

readonly r2_endpoint="${WITSELF_BILLING_INVENTORY_R2_ENDPOINT:-}"
readonly r2_bucket="${WITSELF_BILLING_INVENTORY_R2_BUCKET:-}"
readonly r2_prefix="${WITSELF_BILLING_INVENTORY_R2_PREFIX:-}"
readonly r2_access_key="${WITSELF_BILLING_INVENTORY_R2_ACCESS_KEY:-}"
readonly r2_secret_key="${WITSELF_BILLING_INVENTORY_R2_SECRET_KEY:-}"
readonly ordinary_r2_access_key="${WITSELF_CP_R2_ACCESS_KEY:-}"
readonly ordinary_r2_secret_key="${WITSELF_CP_R2_SECRET_KEY:-}"
[ "$r2_endpoint" = "$PRODUCTION_R2_ENDPOINT" ] ||
  die "the dedicated inventory endpoint was not the exact production authority"
[ "$r2_bucket" = "$PRODUCTION_R2_BUCKET" ] ||
  die "the dedicated inventory bucket was not the exact production authority"
[ "$r2_prefix" = "$PRODUCTION_R2_PREFIX" ] ||
  die "the dedicated inventory prefix was not the exact production authority"
for credential in "$r2_access_key" "$r2_secret_key"; do
  [ -n "$credential" ] || die "dedicated inventory credentials were incomplete"
  [[ ! "$credential" =~ ^[[:space:]]|[[:space:]]$ ]] ||
    die "a dedicated inventory credential was not canonical"
  [[ ! "$credential" =~ [[:cntrl:]] ]] ||
    die "a dedicated inventory credential was not canonical"
done
[ -z "$ordinary_r2_access_key" ] || [ "$r2_access_key" != "$ordinary_r2_access_key" ] ||
  die "dedicated inventory credentials reused an ordinary control-plane credential"
[ -z "$ordinary_r2_secret_key" ] || [ "$r2_secret_key" != "$ordinary_r2_secret_key" ] ||
  die "dedicated inventory credentials reused an ordinary control-plane credential"

readonly cloudflare_account_id="${CLOUDFLARE_ACCOUNT_ID:-}"
readonly cloudflare_api_token="${CLOUDFLARE_API_TOKEN:-}"
[ "$cloudflare_account_id" = "$PRODUCTION_ACCOUNT_ID" ] ||
  die "the Cloudflare identity was not the exact production account"
[ -n "$cloudflare_api_token" ] && (( ${#cloudflare_api_token} <= 4096 )) &&
  [[ ! "$cloudflare_api_token" =~ [[:space:][:cntrl:]] ]] ||
  die "the Cloudflare API token was missing or invalid"

# From this point forward, no ordinary helper subprocess receives credentials.
# Each network-capable phase explicitly exports only its captured allowlist in
# a subshell, without putting credential assignments in process argv.
scrub_environment_except "PATH,HOME,TMPDIR,TMP,TEMP,LANG,TZ" ||
  die "the ambient environment could not be isolated"

canonical_real_file "$release_snapshot_config" ||
  die "the release snapshot config was not a real canonical absolute file"
config_directory="$(dirname "$release_snapshot_config")"
cloudflare_root="$(dirname "$config_directory")"
infra_root="$(dirname "$cloudflare_root")"
repository_root="$(dirname "$infra_root")"
snapshot_root="$(dirname "$repository_root")"
snapshot_work_dir="$snapshot_root/work"
snapshot_control_plane_root="$cloudflare_root/control-plane"
source_fence_script="$snapshot_control_plane_root/scripts/billing-rollout-source-fence.mjs"
reviewed_env_file="$cloudflare_root/wrangler-production-empty.env"

[ "$(basename "$release_snapshot_config")" = wrangler.generated.jsonc ] &&
  [[ "$(basename "$config_directory")" =~ ^witself-control-plane-deploy-[A-Za-z0-9]{6}$ ]] &&
  [ "$(basename "$cloudflare_root")" = cloudflare ] &&
  [ "$(basename "$infra_root")" = infra ] &&
  [ "$(basename "$repository_root")" = repository ] &&
  [[ "$(basename "$snapshot_root")" =~ ^witself-control-plane-release-[A-Za-z0-9]{6}$ ]] ||
  die "the release snapshot config did not have the exact immutable layout"

if ! canonical_real_directory "$snapshot_root" ||
   ! require_exact_mode "$snapshot_root" 700; then
  die "the release snapshot root was not a real mode-0700 directory"
fi
if ! canonical_real_directory "$repository_root" ||
   ! require_exact_mode "$repository_root" 555; then
  die "the release snapshot repository was not frozen"
fi
if ! canonical_real_directory "$snapshot_work_dir" ||
   ! require_exact_mode "$snapshot_work_dir" 700; then
  die "the release snapshot private work directory was invalid"
fi
if ! canonical_real_directory "$config_directory" ||
   ! require_exact_mode "$config_directory" 700; then
  die "the release snapshot config directory was invalid"
fi
if ! canonical_real_directory "$snapshot_control_plane_root" ||
   ! require_exact_mode "$snapshot_control_plane_root" 555; then
  die "the release snapshot control-plane source was not frozen"
fi
require_exact_mode "$release_snapshot_config" 400 ||
  die "the release snapshot config did not have mode 0400"
if ! canonical_real_file "$source_fence_script" ||
   ! require_exact_mode "$source_fence_script" 444; then
  die "the immutable source-fence command was unavailable"
fi
if ! canonical_real_file "$reviewed_env_file" ||
   ! require_exact_mode "$reviewed_env_file" 444; then
  die "the immutable reviewed Wrangler environment was unavailable"
fi

canonical_real_file "$control_plane_binary" ||
  die "the control-plane binary was not a real canonical absolute file"
[ "$(basename "$control_plane_binary")" = witself-control-plane ] ||
  die "the control-plane binary had the wrong executable name"
[ -x "$control_plane_binary" ] || die "the control-plane binary was not executable"
control_plane_mode="$(file_mode "$control_plane_binary")"
if [[ ! "$control_plane_mode" =~ ^[0-7]{3,4}$ ]] ||
   ! (( (8#$control_plane_mode & 8#111) != 0 &&
        (8#$control_plane_mode & 8#022) == 0 )); then
  die "the control-plane binary permissions were unsafe"
fi

realpath_binary="$(command -v realpath 2>/dev/null || true)"
[[ "$realpath_binary" = /* && -f "$realpath_binary" && -x "$realpath_binary" ]] ||
  die "realpath was unavailable"
node_candidate="$(command -v node 2>/dev/null || true)"
sleep_candidate="$(command -v sleep 2>/dev/null || true)"
node_binary="$("$realpath_binary" "$node_candidate" 2>/dev/null || true)"
sleep_binary="$("$realpath_binary" "$sleep_candidate" 2>/dev/null || true)"
canonical_real_file "$node_binary" && [ -x "$node_binary" ] ||
  die "node was unavailable"
canonical_real_file "$sleep_binary" && [ -x "$sleep_binary" ] ||
  die "sleep was unavailable"

sha256_binary="$(command -v sha256sum 2>/dev/null || true)"
sha256_kind=sha256sum
if [[ ! "$sha256_binary" = /* || ! -f "$sha256_binary" || ! -x "$sha256_binary" ]]; then
  sha256_binary="$(command -v shasum 2>/dev/null || true)"
  sha256_kind=shasum
fi
[[ "$sha256_binary" = /* && -f "$sha256_binary" && -x "$sha256_binary" ]] ||
  die "a SHA-256 implementation was unavailable"

config_identity="$(file_identity "$release_snapshot_config")"
source_fence_identity="$(file_identity "$source_fence_script")"
reviewed_env_identity="$(file_identity "$reviewed_env_file")"
control_plane_identity="$(file_identity "$control_plane_binary")"
config_sha256="$(file_sha256 "$release_snapshot_config")"
source_fence_sha256="$(file_sha256 "$source_fence_script")"
reviewed_env_sha256="$(file_sha256 "$reviewed_env_file")"
actual_control_plane_sha256="$(file_sha256 "$control_plane_binary")"
node_identity="$(file_identity "$node_binary")"
sleep_identity="$(file_identity "$sleep_binary")"
node_sha256="$(file_sha256 "$node_binary")"
sleep_sha256="$(file_sha256 "$sleep_binary")"
readonly config_identity source_fence_identity reviewed_env_identity
readonly control_plane_identity config_sha256 source_fence_sha256
readonly reviewed_env_sha256 actual_control_plane_sha256
readonly node_identity sleep_identity node_sha256 sleep_sha256
[[ "$config_identity" =~ ^[0-9]+:[0-9]+:[0-9]+:[0-9]+$ &&
   "$source_fence_identity" =~ ^[0-9]+:[0-9]+:[0-9]+:[0-9]+$ &&
   "$reviewed_env_identity" =~ ^[0-9]+:[0-9]+:[0-9]+:[0-9]+$ &&
   "$control_plane_identity" =~ ^[0-9]+:[0-9]+:[0-9]+:[0-9]+$ ]] ||
  die "an immutable input identity could not be captured"
[[ "$config_sha256" =~ ^[0-9a-f]{64}$ &&
   "$source_fence_sha256" =~ ^[0-9a-f]{64}$ &&
   "$reviewed_env_sha256" =~ ^[0-9a-f]{64}$ &&
   "$actual_control_plane_sha256" =~ ^[0-9a-f]{64}$ ]] ||
  die "an immutable input SHA-256 could not be captured"
[[ "$node_identity" =~ ^[0-9]+:[0-9]+:[0-9]+:[0-9]+$ &&
   "$sleep_identity" =~ ^[0-9]+:[0-9]+:[0-9]+:[0-9]+$ &&
   "$node_sha256" =~ ^[0-9a-f]{64}$ &&
   "$sleep_sha256" =~ ^[0-9a-f]{64}$ ]] ||
  die "a runtime command identity could not be captured"
[ "$actual_control_plane_sha256" = "$control_plane_binary_sha256" ] ||
  die "the control-plane binary did not match its reviewed SHA-256"

assert_release_snapshot_stable() {
  if ! stable_file_identity \
      "$release_snapshot_config" "$config_identity" 400 "$config_sha256" ||
     ! stable_file_identity \
      "$source_fence_script" "$source_fence_identity" 444 "$source_fence_sha256" ||
     ! stable_file_identity \
      "$reviewed_env_file" "$reviewed_env_identity" 444 "$reviewed_env_sha256"; then
    die "the release snapshot changed during capture"
  fi
}

assert_control_plane_stable() {
  stable_executable_identity \
    "$control_plane_binary" "$control_plane_identity" \
    "$actual_control_plane_sha256" ||
    die "the control-plane binary changed during capture"
}

assert_node_stable() {
  stable_executable_identity "$node_binary" "$node_identity" "$node_sha256" ||
    die "the Node runtime changed during capture"
}

assert_sleep_stable() {
  stable_executable_identity "$sleep_binary" "$sleep_identity" "$sleep_sha256" ||
    die "the sleep command changed during capture"
}

assert_control_plane_stable
binary_version_output="$(run_runtime_phase "$control_plane_binary" version 2>/dev/null)" ||
  die "the control-plane binary version probe failed"
if [[ ! "$binary_version_output" =~ ^witself-control-plane\ ([0-9]+\.[0-9]+\.[0-9]+)\ \(commit\ ([0-9a-f]{40}),\ built\ ([0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z)\)$ ]] ||
   [ "${BASH_REMATCH[1]:-}" != "$expected_target_release_version" ] ||
   [ "${BASH_REMATCH[2]:-}" != "$expected_target_release_commit" ]; then
  die "the control-plane binary did not match the exact target release"
fi
unset binary_version_output

canonical_absent_path "$work_dir" ||
  die "the private work directory must be an absent canonical absolute path"
canonical_absent_path "$output" ||
  die "the public output must be an absent canonical absolute path"

umask 077
work_created=false
capture_complete=false
work_identity=""
output_created_kind=""
output_created_identity=""
initial_attestation="$work_dir/initial-lifecycle-disabled.json"
before_fence="$work_dir/source-fence-before.json"
provisional="$work_dir/registry-provisional.json"
after_fence="$work_dir/source-fence-after.json"
initial_identity=""
initial_sha256=""
before_identity=""
before_sha256=""
provisional_identity=""
provisional_sha256=""
after_identity=""
after_sha256=""

cleanup() {
  local status=$?
  local artifact
  trap - EXIT INT TERM HUP
  set +e
  if [ "$capture_complete" != true ]; then
    case "$output_created_kind" in
      regular)
        if canonical_real_file "$output" &&
           [ "$(file_identity "$output")" = "$output_created_identity" ]; then
          rm -f -- "$output" 2>/dev/null || true
        fi
        ;;
      symlink)
        if [ -L "$output" ] &&
           [ "$(file_identity "$output")" = "$output_created_identity" ]; then
          rm -f -- "$output" 2>/dev/null || true
        fi
        ;;
    esac
  fi
  if [ "$capture_complete" != true ] && [ "$work_created" = true ] &&
     canonical_real_directory "$work_dir" &&
     [ "$(directory_identity "$work_dir")" = "$work_identity" ]; then
    for artifact in \
      "$initial_attestation" "$before_fence" "$provisional" "$after_fence"; do
      if [ -e "$artifact" ] || [ -L "$artifact" ]; then
        if [ "$(dirname "$artifact")" = "$work_dir" ]; then
          rm -f -- "$artifact" 2>/dev/null || true
        fi
      fi
    done
    rmdir -- "$work_dir" 2>/dev/null || true
  fi
  exit "$status"
}
trap cleanup EXIT
trap 'exit 130' INT TERM HUP

record_created_output() {
  if [ -L "$output" ]; then
    output_created_kind=symlink
    output_created_identity="$(file_identity "$output")"
  elif canonical_real_file "$output"; then
    output_created_kind=regular
    output_created_identity="$(file_identity "$output")"
  fi
}

mkdir -m 700 -- "$work_dir" || die "the private work directory could not be created"
work_created=true
if ! canonical_real_directory "$work_dir" ||
   ! require_exact_mode "$work_dir" 700; then
  die "the private work directory was not a real mode-0700 directory"
fi
work_identity="$(directory_identity "$work_dir")"
[[ "$work_identity" =~ ^[0-9]+:[0-9]+$ ]] ||
  die "the private work directory identity could not be captured"

source_common_args=(
  --config "$release_snapshot_config"
  --expected-account-id "$expected_account_id"
  --expected-target-application-id "$expected_target_application_id"
  --expected-target-application-version "$expected_target_application_version"
  --expected-target-image-digest "$expected_target_image_digest"
  --expected-target-release-version "$expected_target_release_version"
  --expected-target-release-commit "$expected_target_release_commit"
)

capture_source_fence() {
  local destination="$1"
  local prior="${2:-}"
  local command=(
    "$node_binary" "$source_fence_script" "${source_common_args[@]}"
  )
  assert_release_snapshot_stable
  assert_node_stable
  canonical_absent_path "$destination" ||
    die "a private source-fence destination was not absent"
  if [ -n "$prior" ]; then
    require_private_artifact "$prior"
    command+=(--prior-lifecycle-disabled-attestation "$prior")
  fi
  if ! (set -o noclobber; run_source_phase "${command[@]}" >"$destination"); then
    return 1
  fi
  require_private_artifact "$destination"
}

capture_source_fence "$initial_attestation" ||
  die "the initial lifecycle-disabled source attestation failed"
initial_identity="$(file_identity "$initial_attestation")"
initial_sha256="$(file_sha256 "$initial_attestation")"
assert_release_snapshot_stable
assert_sleep_stable
run_runtime_phase "$sleep_binary" "$IN_FLIGHT_BOUND_SECONDS" ||
  die "the fixed lifecycle in-flight wait failed"
assert_private_artifact_stable \
  "$initial_attestation" "$initial_identity" "$initial_sha256"

capture_source_fence "$before_fence" "$initial_attestation" ||
  die "the pre-scan source fence failed"
before_identity="$(file_identity "$before_fence")"
before_sha256="$(file_sha256 "$before_fence")"

assert_control_plane_stable
canonical_absent_path "$provisional" ||
  die "the registry provisional destination was not absent"
run_scan_phase "$control_plane_binary" billing-rollout-inventory scan \
  --source-fence-before "$before_fence" \
  --provisional "$provisional" ||
  die "the bounded registry scan failed"
require_private_artifact "$provisional"
provisional_identity="$(file_identity "$provisional")"
provisional_sha256="$(file_sha256 "$provisional")"
assert_private_artifact_stable "$before_fence" "$before_identity" "$before_sha256"

capture_source_fence "$after_fence" "$initial_attestation" ||
  die "the post-scan source fence failed"
after_identity="$(file_identity "$after_fence")"
after_sha256="$(file_sha256 "$after_fence")"

assert_control_plane_stable
canonical_absent_path "$output" ||
  die "the public output path changed before finalization"
if ! canonical_real_directory "$work_dir" ||
   [ "$(directory_identity "$work_dir")" != "$work_identity" ] ||
   ! require_exact_mode "$work_dir" 700; then
  die "the private work directory changed before finalization"
fi
assert_private_artifact_stable \
  "$initial_attestation" "$initial_identity" "$initial_sha256"
assert_private_artifact_stable "$before_fence" "$before_identity" "$before_sha256"
assert_private_artifact_stable \
  "$provisional" "$provisional_identity" "$provisional_sha256"
assert_private_artifact_stable "$after_fence" "$after_identity" "$after_sha256"
assert_release_snapshot_stable
assert_control_plane_stable

if ! run_finalize_phase "$control_plane_binary" billing-rollout-inventory finalize \
    --source-fence-before "$before_fence" \
    --provisional "$provisional" \
    --source-fence-after "$after_fence" \
    --output "$output"; then
  die "the fenced registry finalization failed"
fi

record_created_output
require_private_artifact "$output"
assert_private_artifact_stable \
  "$initial_attestation" "$initial_identity" "$initial_sha256"
assert_private_artifact_stable "$before_fence" "$before_identity" "$before_sha256"
assert_private_artifact_stable \
  "$provisional" "$provisional_identity" "$provisional_sha256"
assert_private_artifact_stable "$after_fence" "$after_identity" "$after_sha256"
assert_release_snapshot_stable
assert_control_plane_stable
assert_node_stable
assert_sleep_stable

capture_complete=true
printf 'billing rollout inventory capture: OK\n'
