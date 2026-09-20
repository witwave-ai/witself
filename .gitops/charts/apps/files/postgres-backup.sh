#!/bin/bash
# Mounted verbatim by the CronJob. Never log command output or secret values.
set -Eeuo pipefail
umask 077
exec 3>&2 2>/dev/null

export PGCONNECT_TIMEOUT=10
export AWS_EC2_METADATA_DISABLED=true AWS_DEFAULT_REGION=auto AWS_REGION=auto
export AWS_CONFIG_FILE=/dev/null AWS_SHARED_CREDENTIALS_FILE=/dev/null
export AWS_MAX_ATTEMPTS=3 AWS_PAGER=""
export AWS_REQUEST_CHECKSUM_CALCULATION=when_required AWS_RESPONSE_CHECKSUM_VALIDATION=when_required
run_started=""
staging=""
object_key=""
bytes=0
failure_stage=configuration

metrics_sql() {
  PGUSER="$METRICS_USER" PGPASSWORD="$METRICS_PASSWORD" \
    PGOPTIONS='-c statement_timeout=15000 -c lock_timeout=5000 -c DateStyle=ISO,YMD' \
    psql --no-psqlrc --no-password --dbname="$METRICS_DATABASE" \
      --set=ON_ERROR_STOP=1 --tuples-only --no-align --quiet "$@"
}
aws_cli() {
  aws --endpoint-url "$R2_ENDPOINT" --cli-connect-timeout 10 --cli-read-timeout 60 "$@"
}
s3() {
  aws_cli s3 "$@" --only-show-errors >/dev/null
}
finish() {
  local status=$?
  trap - EXIT
  if (( status != 0 )); then
    if [[ -n "$run_started" ]]; then
      metrics_sql --set="run_started=$run_started" --set="object_key=$object_key" \
        --set="error_text=$failure_stage" --set="bytes=$bytes" >/dev/null <<'SQL' || true
UPDATE witself_ops.backup_runs
SET finished_at = clock_timestamp(), succeeded = false, bytes = :'bytes'::bigint,
    object_key = :'object_key', error_text = :'error_text'
WHERE started_at = :'run_started'::timestamptz;
SQL
    fi
    if [[ -n "$staging" ]] && command -v aws >/dev/null; then
      s3 rm "$staging" || true
    fi
    echo 'PostgreSQL backup failed; inspect Job status and backup alerts.' >&3
  fi
  exit "$status"
}
trap finish EXIT
trap 'exit 143' TERM
trap 'exit 130' INT

# Operators provision the table and two restricted roles once. The Job never
# carries administrator credentials or creates schemas, tables, or roles.
for required_variable in METRICS_DATABASE METRICS_USER METRICS_PASSWORD PGHOST; do
  [[ -n "${!required_variable:-}" ]] || exit 1
done
run_started="$(metrics_sql <<'SQL'
INSERT INTO witself_ops.backup_runs (started_at, succeeded)
VALUES (clock_timestamp(), false) RETURNING started_at;
SQL
)"
[[ "$run_started" =~ ^[0-9][0-9\ .:+-]+$ ]] || exit 1

for required_variable in PGDATABASE PGUSER PGPASSWORD AGE_RECIPIENT AWS_ACCESS_KEY_ID AWS_SECRET_ACCESS_KEY R2_ENDPOINT R2_BUCKET R2_PREFIX CELL_NAME POD_UID; do
  [[ -n "${!required_variable:-}" ]] || exit 1
done
[[ "$R2_ENDPOINT" =~ ^https://[a-zA-Z0-9.-]+\.r2\.cloudflarestorage\.com/?$ ]] || exit 1
[[ "$R2_BUCKET" =~ ^[a-z0-9][a-z0-9.-]+[a-z0-9]$ ]] || exit 1
[[ "$R2_PREFIX" =~ ^[a-zA-Z0-9][a-zA-Z0-9/_.-]*$ ]] || exit 1
[[ "$CELL_NAME" =~ ^[a-zA-Z0-9][a-zA-Z0-9.-]*$ ]] || exit 1
[[ "$POD_UID" =~ ^[a-zA-Z0-9][a-zA-Z0-9-]*$ ]] || exit 1

# The public PostgreSQL image already contains psql/pg_dump/bash/gzip. Installing
# from its signed Alpine repositories avoids an unpublished private image gate.
failure_stage=package_setup
apk add --no-cache age aws-cli >/dev/null

backup_id="$(date -u +%Y%m%dT%H%M%SZ)-${POD_UID}"
object_key="${R2_PREFIX%/}/${CELL_NAME}/${backup_id}.sql.gz.age"
destination="s3://${R2_BUCKET}/${object_key}"
staging="${destination}.incomplete"

# pipefail prevents successful encryption/upload of a truncated dump from
# publishing a final object. No plaintext dump file is created.
failure_stage=stream_upload
PGOPTIONS='-c default_transaction_read_only=on' \
  pg_dump --no-password --no-owner --no-privileges --lock-wait-timeout=60s \
  | gzip -c \
  | age -r "$AGE_RECIPIENT" \
  | s3 cp - "$staging"

failure_stage=object_verification
head_bytes="$(aws_cli s3api head-object --bucket "$R2_BUCKET" --key "${object_key}.incomplete" \
  --query ContentLength --output text)"
[[ "$head_bytes" =~ ^[1-9][0-9]*$ ]] || exit 1
bytes="$head_bytes"

# Promote only a complete pipeline, within the same destination. Unique pod IDs
# prevent retries from overwriting prior artifacts.
failure_stage=object_promotion
# Promote with one server-side CopyObject: R2 answers NotImplemented to the
# multipart copy (UploadPartCopy) that `aws s3 cp` selects above 8 MiB, and a
# single CopyObject covers objects up to 5 GiB.
aws_cli s3api copy-object --bucket "$R2_BUCKET" \
  --copy-source "${R2_BUCKET}/${object_key}.incomplete" --key "$object_key" >/dev/null
failure_stage=staging_cleanup
s3 rm "$staging"
staging=""
failure_stage=success_record
metrics_sql --set="run_started=$run_started" --set="object_key=$object_key" \
  --set="bytes=$bytes" >/dev/null <<'SQL'
UPDATE witself_ops.backup_runs
SET finished_at = clock_timestamp(), succeeded = true, bytes = :'bytes'::bigint,
    object_key = :'object_key', error_text = ''
WHERE started_at = :'run_started'::timestamptz;
SQL
echo 'PostgreSQL backup completed.' >&3
