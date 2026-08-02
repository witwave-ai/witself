#!/usr/bin/env bash
# Create and restore-verify an encrypted logical PostgreSQL backup before a
# schema-changing rollout to one of Witself's two serving Civo cells.
#
# The source path is deliberately read-only. The script performs Kubernetes
# GETs plus psql/pg_dump reads inside the existing PostgreSQL pod. Restore
# verification happens in a network-isolated, disposable local container.
set -euo pipefail

usage() {
  cat <<'EOF'
usage: civo-pre-migration-backup.sh \
  --cell CELL \
  --kubeconfig FILE \
  --context CONTEXT \
  --release MAJOR.MINOR.PATCH \
  --output-dir DIRECTORY \
  --age-recipient-file FILE \
  --age-identity-file FILE \
  --restore-image POSTGRES_IMAGE

Supported serving cells:
  civo-sandbox-use1-backup
  civo-sandbox-usw2-dev

POSTGRES_IMAGE must already exist in the local Docker image store. The script
runs its immutable image ID with --pull=never; it never pulls during a backup.
It must match the source PostgreSQL major and include the pgvector extension.
EOF
}

die() {
  printf 'error: %s\n' "$*" >&2
  exit 1
}

require_command() {
  command -v "$1" >/dev/null 2>&1 || die "$1 is required"
}

file_mode() {
  local path="$1"
  local mode
  mode="$(stat -f '%Lp' "$path" 2>/dev/null || true)"
  if [[ ! "$mode" =~ ^[0-7]{3,4}$ ]]; then
    mode="$(stat -c '%a' "$path" 2>/dev/null || true)"
  fi
  printf '%s\n' "$mode"
}

require_private_file() {
  local label="$1"
  local path="$2"
  local mode
  [ -f "$path" ] || die "$label is not a regular file"
  [ ! -L "$path" ] || die "$label must not be a symbolic link"
  mode="$(file_mode "$path")"
  [[ "$mode" =~ ^[0-7]{3,4}$ ]] || die "could not determine permissions for $label"
  if (( (8#$mode & 8#077) != 0 )); then
    die "$label must not be accessible by group or other users (mode is $mode)"
  fi
}

sha256_file() {
  local path="$1"
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$path" | awk '{print $1}'
  else
    shasum -a 256 "$path" | awk '{print $1}'
  fi
}

CELL=""
KUBECONFIG_FILE=""
KUBE_CONTEXT=""
RELEASE_VERSION=""
OUTPUT_DIR=""
AGE_RECIPIENT_FILE=""
AGE_IDENTITY_FILE=""
RESTORE_IMAGE=""

while [ "$#" -gt 0 ]; do
  case "$1" in
    --cell) [ "$#" -ge 2 ] || die "$1 requires a value"; CELL="$2"; shift 2 ;;
    --kubeconfig) [ "$#" -ge 2 ] || die "$1 requires a value"; KUBECONFIG_FILE="$2"; shift 2 ;;
    --context) [ "$#" -ge 2 ] || die "$1 requires a value"; KUBE_CONTEXT="$2"; shift 2 ;;
    --release) [ "$#" -ge 2 ] || die "$1 requires a value"; RELEASE_VERSION="$2"; shift 2 ;;
    --output-dir) [ "$#" -ge 2 ] || die "$1 requires a value"; OUTPUT_DIR="$2"; shift 2 ;;
    --age-recipient-file) [ "$#" -ge 2 ] || die "$1 requires a value"; AGE_RECIPIENT_FILE="$2"; shift 2 ;;
    --age-identity-file) [ "$#" -ge 2 ] || die "$1 requires a value"; AGE_IDENTITY_FILE="$2"; shift 2 ;;
    --restore-image) [ "$#" -ge 2 ] || die "$1 requires a value"; RESTORE_IMAGE="$2"; shift 2 ;;
    -h|--help) usage; exit 0 ;;
    *) usage >&2; die "unknown or incomplete argument: $1" ;;
  esac
done

[ -n "$CELL" ] || { usage >&2; die "--cell is required"; }
[ -n "$KUBECONFIG_FILE" ] || { usage >&2; die "--kubeconfig is required"; }
[ -n "$KUBE_CONTEXT" ] || { usage >&2; die "--context is required"; }
[ -n "$RELEASE_VERSION" ] || { usage >&2; die "--release is required"; }
[ -n "$OUTPUT_DIR" ] || { usage >&2; die "--output-dir is required"; }
[ -n "$AGE_RECIPIENT_FILE" ] || { usage >&2; die "--age-recipient-file is required"; }
[ -n "$AGE_IDENTITY_FILE" ] || { usage >&2; die "--age-identity-file is required"; }
[ -n "$RESTORE_IMAGE" ] || { usage >&2; die "--restore-image is required"; }

case "$CELL" in
  civo-sandbox-use1-backup|civo-sandbox-usw2-dev) ;;
  *) die "unsupported source cell $CELL; this guard intentionally excludes drill and non-Civo cells" ;;
esac
[[ "$RELEASE_VERSION" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] ||
  die "--release must be MAJOR.MINOR.PATCH without a v prefix"
[[ "$KUBE_CONTEXT" =~ ^[A-Za-z0-9._:@/-]+$ ]] || die "--context contains unsupported characters"
[[ "$RESTORE_IMAGE" =~ ^[A-Za-z0-9][A-Za-z0-9._/@:-]{0,255}$ ]] ||
  die "--restore-image contains unsupported characters"

for command_name in age date docker jq kubectl openssl stat; do
  require_command "$command_name"
done
if ! command -v sha256sum >/dev/null 2>&1; then
  require_command shasum
fi

require_private_file "kubeconfig" "$KUBECONFIG_FILE"
require_private_file "age recipient file" "$AGE_RECIPIENT_FILE"
require_private_file "age identity file" "$AGE_IDENTITY_FILE"
if grep -Eq 'AGE-SECRET-KEY-|BEGIN (OPENSSH|RSA|EC|DSA) PRIVATE KEY' "$AGE_RECIPIENT_FILE"; then
  die "age recipient file appears to contain private key material; provide public recipients only"
fi

[ -d "$OUTPUT_DIR" ] || die "--output-dir must be an existing directory"
[ ! -L "$OUTPUT_DIR" ] || die "--output-dir must not be a symbolic link"
OUTPUT_DIR="$(cd "$OUTPUT_DIR" && pwd -P)"
[ "$OUTPUT_DIR" != "/" ] || die "refusing to use / as --output-dir"
OUTPUT_MODE="$(file_mode "$OUTPUT_DIR")"
[[ "$OUTPUT_MODE" =~ ^[0-7]{3,4}$ ]] || die "could not determine output-directory permissions"
if (( (8#$OUTPUT_MODE & 8#077) != 0 )); then
  die "--output-dir must not be accessible by group or other users (mode is $OUTPUT_MODE)"
fi

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd -P)"
if [[ "$OUTPUT_DIR" == "$REPO_ROOT" || "$OUTPUT_DIR" == "$REPO_ROOT/"* ]]; then
  die "backup artifacts must be stored outside the source checkout"
fi

umask 077

# Prove the supplied public recipients and private identity work together before
# making any connection to a cell. No probe plaintext is written to disk.
ENCRYPTION_PROBE="witself-civo-backup-encryption-probe-v1"
DECRYPTED_PROBE="$({
  printf '%s' "$ENCRYPTION_PROBE" |
    age -R "$AGE_RECIPIENT_FILE" |
    age --decrypt -i "$AGE_IDENTITY_FILE"
} 2>/dev/null)" || die "age recipient/identity preflight failed"
[ "$DECRYPTED_PROBE" = "$ENCRYPTION_PROBE" ] || die "age recipient/identity preflight returned unexpected bytes"
unset DECRYPTED_PROBE ENCRYPTION_PROBE

docker info >/dev/null 2>&1 || die "Docker is unavailable"
RESTORE_IMAGE_ID="$(docker image inspect --format '{{.Id}}' "$RESTORE_IMAGE" 2>/dev/null)" ||
  die "restore image is not present locally: $RESTORE_IMAGE"
[[ "$RESTORE_IMAGE_ID" == sha256:* ]] || die "Docker returned an unexpected restore image ID"

KUBE=(kubectl --kubeconfig "$KUBECONFIG_FILE" --context "$KUBE_CONTEXT")
OBSERVED_CELL="$(
  "${KUBE[@]}" -n argocd get applications.argoproj.io witself-postgresql \
    -o 'jsonpath={.metadata.labels.witself\.io/cell}'
)" || die "could not verify the Civo cell identity through Argo CD"
[ "$OBSERVED_CELL" = "$CELL" ] ||
  die "context identity mismatch: requested $CELL but cluster reports $OBSERVED_CELL"

PODS_JSON="$(
  "${KUBE[@]}" -n witself get pods \
    -l 'app.kubernetes.io/instance=witself-postgresql,app.kubernetes.io/component=primary' \
    -o json
)" || die "could not locate the source PostgreSQL pod"
POSTGRES_POD="$(
  jq -er '
    [.items[] |
      select(.status.phase == "Running") |
      select(any(.status.conditions[]?; .type == "Ready" and .status == "True"))] |
    if length == 1 then .[0].metadata.name
    else error("expected exactly one ready PostgreSQL primary pod") end
  ' <<<"$PODS_JSON"
)" || die "expected exactly one ready PostgreSQL primary pod"
POSTGRES_CONTAINER="$(
  jq -er --arg pod "$POSTGRES_POD" '
    .items[] | select(.metadata.name == $pod) | [.spec.containers[].name] |
    if index("postgresql") then "postgresql"
    elif length == 1 then .[0]
    else error("could not select PostgreSQL container") end
  ' <<<"$PODS_JSON"
)" || die "could not select the PostgreSQL container"
unset PODS_JSON

READ_ONLY_PGOPTIONS='-c default_transaction_read_only=on -c statement_timeout=0'
live_sql() {
  local sql="$1"
  # This script is evaluated by sh inside the PostgreSQL pod, not by this host.
  # shellcheck disable=SC2016
  "${KUBE[@]}" -n witself exec "$POSTGRES_POD" -c "$POSTGRES_CONTAINER" -- \
    env PGOPTIONS="$READ_ONLY_PGOPTIONS" sh -eu -c '
      password_file="${POSTGRES_PASSWORD_FILE:-${POSTGRESQL_PASSWORD_FILE:-/opt/bitnami/postgresql/secrets/password}}"
      test -r "$password_file"
      PGPASSWORD="$(cat "$password_file")"
      export PGPASSWORD
      db_user="${POSTGRES_USER:-${POSTGRESQL_USERNAME:-witself}}"
      db_name="${POSTGRES_DATABASE:-${POSTGRESQL_DATABASE:-witself}}"
      exec psql --no-password --host=127.0.0.1 \
        --port="${POSTGRESQL_PORT_NUMBER:-5432}" \
        --username="$db_user" --dbname="$db_name" \
        --tuples-only --no-align --set=ON_ERROR_STOP=1 --command="$1"
    ' sh "$sql"
}

SOURCE_VERSION_NUM="$(live_sql 'SHOW server_version_num;' | tr -d '[:space:]')" ||
  die "could not read the source PostgreSQL version"
[[ "$SOURCE_VERSION_NUM" =~ ^[0-9]{5,6}$ ]] || die "source PostgreSQL returned an invalid version"
SOURCE_MAJOR=$((SOURCE_VERSION_NUM / 10000))
SOURCE_SCHEMA_VERSION="$(
  live_sql "SELECT COALESCE(MAX(version_id) FILTER (WHERE is_applied), 0) FROM public.goose_db_version;" |
    tr -d '[:space:]'
)" || die "could not read the source schema version"
[[ "$SOURCE_SCHEMA_VERSION" =~ ^[0-9]+$ ]] || die "source schema version is invalid"
[ "$SOURCE_SCHEMA_VERSION" -gt 0 ] || die "source schema version is not initialized"
SOURCE_PGVECTOR_INSTALLED="$(
  live_sql "SELECT count(*) FROM pg_catalog.pg_extension WHERE extname = 'vector';" |
    tr -d '[:space:]'
)" || die "could not read the source pgvector extension state"
case "$SOURCE_PGVECTOR_INSTALLED" in
  0|1) ;;
  *) die "source pgvector extension state is invalid" ;;
esac
RESTORE_IMAGE_MAJOR="$(
  docker run --rm --pull=never --network none "$RESTORE_IMAGE_ID" sh -eu -c '
    vector_control="$(pg_config --sharedir)/extension/vector.control"
    test -r "$vector_control"
    postgres --version
  ' | awk '{split($3, version, "."); print version[1]}'
)" || die "restore image must contain PostgreSQL and the pgvector extension"
[[ "$RESTORE_IMAGE_MAJOR" =~ ^[0-9]+$ ]] || die "restore image returned an invalid PostgreSQL version"
[ "$RESTORE_IMAGE_MAJOR" -eq "$SOURCE_MAJOR" ] ||
  die "restore image PostgreSQL major $RESTORE_IMAGE_MAJOR does not match source major $SOURCE_MAJOR"

UTC_STAMP="$(date -u +%Y%m%dT%H%M%SZ)"
CREATED_AT="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
RANDOM_SUFFIX="$(openssl rand -hex 4)"
BACKUP_ID="${CELL}-pre-v${RELEASE_VERSION}-${UTC_STAMP}-${RANDOM_SUFFIX}"
SESSION_DIR="$OUTPUT_DIR/$BACKUP_ID"
mkdir -m 700 "$SESSION_DIR" || die "could not create the backup directory"
ARTIFACT="$SESSION_DIR/$BACKUP_ID.dump.age"
ARTIFACT_PART="$ARTIFACT.part"
CHECKSUM_FILE="$SESSION_DIR/$BACKUP_ID.sha256"
MANIFEST_FILE="$SESSION_DIR/$BACKUP_ID.json"
MANIFEST_PART="$MANIFEST_FILE.part"
DUMP_LOG="$SESSION_DIR/pg_dump.stderr"
VERIFY_CONTAINER=""
VERIFY_CONTAINER_CREATED=false

cleanup_verify_container() {
  if [ "$VERIFY_CONTAINER_CREATED" != true ]; then
    return 0
  fi
  local label
  if ! docker inspect "$VERIFY_CONTAINER" >/dev/null 2>&1; then
    VERIFY_CONTAINER_CREATED=false
    return 0
  fi
  label="$(docker inspect --format '{{index .Config.Labels "com.witwave.witself.backup-id"}}' "$VERIFY_CONTAINER" 2>/dev/null || true)"
  if [ "$label" != "$BACKUP_ID" ]; then
    printf 'error: refusing to remove unverified container %s\n' "$VERIFY_CONTAINER" >&2
    return 1
  fi
  docker stop --time 10 "$VERIFY_CONTAINER" >/dev/null 2>&1 || true
  docker rm "$VERIFY_CONTAINER" >/dev/null 2>&1 ||
    docker rm --force "$VERIFY_CONTAINER" >/dev/null 2>&1 || return 1
  VERIFY_CONTAINER_CREATED=false
}

cleanup() {
  local status=$?
  trap - EXIT INT TERM
  if ! cleanup_verify_container; then
    status=1
  fi
  rm -f "$ARTIFACT_PART" "$MANIFEST_PART"
  if [ ! -f "$ARTIFACT" ]; then
    rm -f "$DUMP_LOG"
    rmdir "$SESSION_DIR" 2>/dev/null || true
  fi
  exit "$status"
}
trap cleanup EXIT INT TERM

printf 'Creating encrypted backup %s from %s...\n' "$BACKUP_ID" "$CELL"
if ! {
  # This script is evaluated by sh inside the PostgreSQL pod, not by this host.
  # shellcheck disable=SC2016
  "${KUBE[@]}" -n witself exec "$POSTGRES_POD" -c "$POSTGRES_CONTAINER" -- \
    env PGOPTIONS="$READ_ONLY_PGOPTIONS" sh -eu -c '
      password_file="${POSTGRES_PASSWORD_FILE:-${POSTGRESQL_PASSWORD_FILE:-/opt/bitnami/postgresql/secrets/password}}"
      test -r "$password_file"
      PGPASSWORD="$(cat "$password_file")"
      export PGPASSWORD
      db_user="${POSTGRES_USER:-${POSTGRESQL_USERNAME:-witself}}"
      db_name="${POSTGRES_DATABASE:-${POSTGRESQL_DATABASE:-witself}}"
      exec pg_dump --no-password --host=127.0.0.1 \
        --port="${POSTGRESQL_PORT_NUMBER:-5432}" \
        --username="$db_user" --dbname="$db_name" \
        --format=custom --compress=0 --serializable-deferrable \
        --lock-wait-timeout=30000
    ' 2>"$DUMP_LOG" |
    age -R "$AGE_RECIPIENT_FILE" -o "$ARTIFACT_PART"
}; then
  die "pg_dump/encryption failed; no complete backup artifact was created"
fi
[ -s "$ARTIFACT_PART" ] || die "encrypted backup artifact is empty"
chmod 600 "$ARTIFACT_PART"
mv "$ARTIFACT_PART" "$ARTIFACT"
rm -f "$DUMP_LOG"

CIPHERTEXT_SHA256="$(sha256_file "$ARTIFACT")"
[[ "$CIPHERTEXT_SHA256" =~ ^[0-9a-f]{64}$ ]] || die "could not calculate the artifact checksum"
printf '%s  %s\n' "$CIPHERTEXT_SHA256" "$(basename "$ARTIFACT")" >"$CHECKSUM_FILE"
chmod 600 "$CHECKSUM_FILE"
ARTIFACT_BYTES="$(wc -c <"$ARTIFACT" | tr -d '[:space:]')"
SCRIPT_SHA256="$(sha256_file "$0")"

write_manifest() {
  local verification_status="$1"
  local verified_at="$2"
  local restored_schema_version="$3"
  local public_table_count="$4"
  local account_count="$5"
  local invalid_index_count="$6"
  local unvalidated_constraint_count="$7"
  local restored_pgvector_installed="$8"
  jq -n \
    --arg schema 'witself.civo-pre-migration-backup.v1' \
    --arg backup_id "$BACKUP_ID" \
    --arg cell "$CELL" \
    --arg context "$KUBE_CONTEXT" \
    --arg target_release "$RELEASE_VERSION" \
    --arg created_at "$CREATED_AT" \
    --arg artifact "$(basename "$ARTIFACT")" \
    --arg checksum_file "$(basename "$CHECKSUM_FILE")" \
    --arg ciphertext_sha256 "$CIPHERTEXT_SHA256" \
    --arg script_sha256 "$SCRIPT_SHA256" \
    --arg restore_image_ref "$RESTORE_IMAGE" \
    --arg restore_image_id "$RESTORE_IMAGE_ID" \
    --arg verification_status "$verification_status" \
    --arg verified_at "$verified_at" \
    --argjson artifact_bytes "$ARTIFACT_BYTES" \
    --argjson source_postgresql_version_num "$SOURCE_VERSION_NUM" \
    --argjson source_schema_version "$SOURCE_SCHEMA_VERSION" \
    --argjson source_pgvector_installed "$SOURCE_PGVECTOR_INSTALLED" \
    --argjson restored_schema_version "$restored_schema_version" \
    --argjson public_table_count "$public_table_count" \
    --argjson account_count "$account_count" \
    --argjson invalid_index_count "$invalid_index_count" \
    --argjson unvalidated_constraint_count "$unvalidated_constraint_count" \
    --argjson restored_pgvector_installed "$restored_pgvector_installed" \
    '{
      schema: $schema,
      backup_id: $backup_id,
      source: {
        cell: $cell,
        kubernetes_context: $context,
        postgresql_version_num: $source_postgresql_version_num,
        schema_version: $source_schema_version,
        pgvector_extension_installed: ($source_pgvector_installed == 1)
      },
      target_release: $target_release,
      created_at: $created_at,
      artifact: {
        file: $artifact,
        bytes: $artifact_bytes,
        encryption: "age",
        checksum_algorithm: "sha256",
        ciphertext_sha256: $ciphertext_sha256,
        checksum_file: $checksum_file
      },
      procedure: {script_sha256: $script_sha256},
      restore_verification: {
        status: $verification_status,
        verified_at: (if $verified_at == "" then null else $verified_at end),
        network: "none",
        plaintext_storage: "container tmpfs",
        image_ref: $restore_image_ref,
        image_id: $restore_image_id,
        schema_version: $restored_schema_version,
        public_table_count: $public_table_count,
        account_count: $account_count,
        invalid_index_count: $invalid_index_count,
        unvalidated_constraint_count: $unvalidated_constraint_count,
        pgvector_extension_installed: ($restored_pgvector_installed == 1),
        pgvector_extension_matches_source: (
          $verification_status == "verified" and
          $restored_pgvector_installed == $source_pgvector_installed
        ),
        disposable_target_cleaned: ($verification_status == "verified")
      }
    }' >"$MANIFEST_PART"
  chmod 600 "$MANIFEST_PART"
  mv "$MANIFEST_PART" "$MANIFEST_FILE"
}

# A pending manifest intentionally survives a failed drill. It makes an
# encrypted-but-unverified artifact unambiguous and prevents it from being used
# as rollout evidence.
write_manifest pending "" 0 0 0 0 0 0

CURRENT_SHA256="$(sha256_file "$ARTIFACT")"
[ "$CURRENT_SHA256" = "$CIPHERTEXT_SHA256" ] || die "artifact checksum changed before restore verification"

CONTAINER_SUFFIX="$(printf '%s' "$BACKUP_ID" | openssl dgst -sha256 | awk '{print substr($NF,1,12)}')"
VERIFY_CONTAINER="witself-pgverify-$CONTAINER_SUFFIX"
if [ "$RESTORE_IMAGE_MAJOR" -ge 18 ]; then
  # PostgreSQL 18+ images place a major-versioned data directory beneath this
  # parent. Mounting the parent keeps the whole disposable cluster in tmpfs.
  RESTORE_TMPFS='/var/lib/postgresql:rw,nosuid,nodev,noexec,size=4g'
else
  RESTORE_TMPFS='/var/lib/postgresql/data:rw,nosuid,nodev,noexec,size=4g'
fi
docker run --detach --pull=never \
  --name "$VERIFY_CONTAINER" \
  --label "com.witwave.witself.backup-id=$BACKUP_ID" \
  --network none \
  --tmpfs "$RESTORE_TMPFS" \
  --env POSTGRES_HOST_AUTH_METHOD=trust \
  --env POSTGRES_DB=witself_restore_verify \
  "$RESTORE_IMAGE_ID" >/dev/null
VERIFY_CONTAINER_CREATED=true

ready=false
for ((attempt = 1; attempt <= 60; attempt++)); do
  # pg_isready can report accepting connections while the entrypoint's
  # temporary initialization server is up but before POSTGRES_DB exists.
  if docker exec "$VERIFY_CONTAINER" psql --username=postgres \
      --dbname=witself_restore_verify --tuples-only --no-align \
      --command='SELECT 1;' >/dev/null 2>&1; then
    ready=true
    break
  fi
  sleep 1
done
[ "$ready" = true ] || die "disposable PostgreSQL did not become ready"

RESTORE_VERSION_NUM="$(
  docker exec "$VERIFY_CONTAINER" psql --username=postgres \
    --dbname=witself_restore_verify --tuples-only --no-align \
    --set=ON_ERROR_STOP=1 --command='SHOW server_version_num;' |
    tr -d '[:space:]'
)"
[[ "$RESTORE_VERSION_NUM" =~ ^[0-9]{5,6}$ ]] || die "restore PostgreSQL returned an invalid version"
RESTORE_MAJOR=$((RESTORE_VERSION_NUM / 10000))
[ "$RESTORE_MAJOR" -eq "$SOURCE_MAJOR" ] ||
  die "restore image PostgreSQL major $RESTORE_MAJOR does not match source major $SOURCE_MAJOR"
PGVECTOR_AVAILABLE="$(
  docker exec "$VERIFY_CONTAINER" psql --username=postgres \
    --dbname=witself_restore_verify --tuples-only --no-align \
    --set=ON_ERROR_STOP=1 \
    --command="SELECT count(*) FROM pg_available_extensions WHERE name = 'vector';" |
    tr -d '[:space:]'
)"
[ "$PGVECTOR_AVAILABLE" = 1 ] || die "restore image does not expose the pgvector extension"

printf 'Restore-verifying %s in disposable PostgreSQL...\n' "$BACKUP_ID"
if ! {
  age --decrypt -i "$AGE_IDENTITY_FILE" "$ARTIFACT" |
    docker exec -i "$VERIFY_CONTAINER" pg_restore \
      --username=postgres --dbname=witself_restore_verify \
      --exit-on-error --single-transaction --no-owner --no-privileges
}; then
  die "restore verification failed; encrypted artifact remains marked pending"
fi

VERIFY_ROW="$(
  docker exec "$VERIFY_CONTAINER" psql --username=postgres \
    --dbname=witself_restore_verify --tuples-only --no-align --field-separator='|' \
    --set=ON_ERROR_STOP=1 --command="
      SELECT
        (SELECT COALESCE(MAX(version_id) FILTER (WHERE is_applied), 0)
           FROM public.goose_db_version),
        (SELECT count(*) FROM pg_catalog.pg_tables WHERE schemaname = 'public'),
        (SELECT count(*) FROM public.accounts),
        (SELECT count(*) FROM pg_catalog.pg_index WHERE NOT indisvalid),
        (SELECT count(*) FROM pg_catalog.pg_constraint WHERE NOT convalidated),
        (SELECT count(*) FROM pg_catalog.pg_extension WHERE extname = 'vector');
    " | tr -d '[:space:]'
)" || die "could not verify restored catalog invariants"
IFS='|' read -r RESTORED_SCHEMA_VERSION PUBLIC_TABLE_COUNT ACCOUNT_COUNT INVALID_INDEX_COUNT UNVALIDATED_CONSTRAINT_COUNT PGVECTOR_RESTORED <<<"$VERIFY_ROW"
for number in "$RESTORED_SCHEMA_VERSION" "$PUBLIC_TABLE_COUNT" "$ACCOUNT_COUNT" "$INVALID_INDEX_COUNT" "$UNVALIDATED_CONSTRAINT_COUNT" "$PGVECTOR_RESTORED"; do
  [[ "$number" =~ ^[0-9]+$ ]] || die "restore verification returned an invalid count"
done
[ "$RESTORED_SCHEMA_VERSION" = "$SOURCE_SCHEMA_VERSION" ] ||
  die "restored schema version $RESTORED_SCHEMA_VERSION does not match source $SOURCE_SCHEMA_VERSION"
[ "$PUBLIC_TABLE_COUNT" -gt 0 ] || die "restored database has no public tables"
[ "$INVALID_INDEX_COUNT" -eq 0 ] || die "restored database contains invalid indexes"
[ "$UNVALIDATED_CONSTRAINT_COUNT" -eq 0 ] || die "restored database contains unvalidated constraints"
[ "$PGVECTOR_RESTORED" -eq "$SOURCE_PGVECTOR_INSTALLED" ] ||
  die "restored pgvector extension state does not match the source"

cleanup_verify_container || die "could not clean up disposable restore container"
if docker inspect "$VERIFY_CONTAINER" >/dev/null 2>&1; then
  die "disposable restore container still exists after cleanup"
fi

VERIFIED_AT="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
write_manifest verified "$VERIFIED_AT" "$RESTORED_SCHEMA_VERSION" "$PUBLIC_TABLE_COUNT" "$ACCOUNT_COUNT" "$INVALID_INDEX_COUNT" "$UNVALIDATED_CONSTRAINT_COUNT" "$PGVECTOR_RESTORED"

printf 'Verified pre-migration backup:\n'
printf '  backup_id: %s\n' "$BACKUP_ID"
printf '  cell: %s\n' "$CELL"
printf '  source_schema_version: %s\n' "$SOURCE_SCHEMA_VERSION"
printf '  ciphertext_sha256: %s\n' "$CIPHERTEXT_SHA256"
printf '  artifact: %s\n' "$ARTIFACT"
printf '  checksum: %s\n' "$CHECKSUM_FILE"
printf '  manifest: %s\n' "$MANIFEST_FILE"
