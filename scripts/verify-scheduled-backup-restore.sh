#!/usr/bin/env bash
# Restore one encrypted scheduled dump into an empty, disposable PostgreSQL DB.
# Disable inherited tracing before expanding any operator-supplied values.
set +x
set +v
set -euo pipefail

usage() {
  cat <<'EOF'
usage: verify-scheduled-backup-restore.sh \
  (--file PATH | --object s3://BUCKET/KEY --endpoint-url URL) \
  --age-identity-file PATH [--expect-schema-version N] [--keep-workdir]

RESTORE_DATABASE_URL must name a new, empty, disposable database. The identity
must be a private regular file. Nothing cleans or disposes of the database.
--keep-workdir retains private errors.log and any downloaded encrypted object.
EOF
}

fail() {
  printf 'scheduled-backup-restore: FAIL %s\n' "$1" >&2
  exit "${2:-1}"
}

bad_usage() {
  usage >&2
  fail "$1" 2
}

object=''
endpoint=''
input_file=''
identity=''
expected_version=''
keep_workdir=false
while [ "$#" -gt 0 ]; do
  case "$1" in
    --file|--object|--endpoint-url|--age-identity-file|--expect-schema-version)
      [ "$#" -ge 2 ] || bad_usage missing_value
      case "$2" in ''|-*) bad_usage missing_value ;; esac
      case "$1" in
        --file) [ -z "$input_file" ] || bad_usage duplicate_option; input_file=$2 ;;
        --object) [ -z "$object" ] || bad_usage duplicate_option; object=$2 ;;
        --endpoint-url) [ -z "$endpoint" ] || bad_usage duplicate_option; endpoint=$2 ;;
        --age-identity-file) [ -z "$identity" ] || bad_usage duplicate_option; identity=$2 ;;
        --expect-schema-version) [ -z "$expected_version" ] || bad_usage duplicate_option; expected_version=$2 ;;
      esac
      shift 2
      ;;
    --keep-workdir) keep_workdir=true; shift ;;
    -h|--help) usage; exit 0 ;;
    *) bad_usage unknown_option ;;
  esac
done

# Only metadata is inspected: the identity contents are handed solely to age.
[ -n "$identity" ] || bad_usage missing_identity
[ -f "$identity" ] && [ ! -L "$identity" ] || bad_usage invalid_identity
mode=$(stat -c '%a' "$identity" 2>/dev/null || stat -f '%Lp' "$identity" 2>/dev/null) ||
  bad_usage invalid_identity
[[ "$mode" =~ ^[0-7]{3,4}$ ]] || bad_usage invalid_identity
(( (8#$mode & 8#077) == 0 )) || bad_usage identity_permissions

if { [ -n "$object" ] && [ -n "$input_file" ]; } ||
   { [ -z "$object" ] && [ -z "$input_file" ]; }; then
  bad_usage source_required
fi
if [ -n "$object" ]; then
  [[ "$object" =~ ^s3://[^/]+/.+[^/]$ || "$object" =~ ^s3://[^/]+/[^/]$ ]] || bad_usage invalid_object
  [ -n "$endpoint" ] || bad_usage missing_endpoint
else
  [ -z "$endpoint" ] || bad_usage unexpected_endpoint
  [ -f "$input_file" ] || bad_usage invalid_file
  # A mistaken source path (including a hardlink) must never hash the identity.
  [[ ! "$input_file" -ef "$identity" ]] || bad_usage identity_is_source
fi
[ -n "${RESTORE_DATABASE_URL:-}" ] || bad_usage missing_database_url
case "$RESTORE_DATABASE_URL" in *$'\n'*|*$'\r'*) bad_usage invalid_database_url ;; esac
if [ -n "$expected_version" ]; then
  [[ "$expected_version" =~ ^[1-9][0-9]*$ ]] || bad_usage invalid_schema_version
fi

for command_name in age gzip psql mktemp chmod rm wc; do
  command -v "$command_name" >/dev/null 2>&1 || fail missing_command
done
if [ -n "$object" ]; then
  command -v aws >/dev/null 2>&1 || fail missing_command
fi
if command -v sha256sum >/dev/null 2>&1; then
  hash_command=(sha256sum)
elif command -v shasum >/dev/null 2>&1; then
  hash_command=(shasum -a 256)
else
  fail missing_command
fi

umask 077
workdir=''
cleanup() {
  if [ -n "$workdir" ] && [ "$keep_workdir" = false ]; then
    rm -rf -- "$workdir" 2>/dev/null
  fi
}
trap cleanup EXIT
trap 'fail interrupted' HUP INT TERM
workdir=$(mktemp -d "${TMPDIR:-/tmp}/scheduled-backup-restore.XXXXXX" 2>/dev/null) || fail workdir
chmod 0700 "$workdir" 2>/dev/null || fail workdir
errors="$workdir/errors.log"
: >"$errors"
chmod 0600 "$errors" 2>/dev/null || fail workdir
if [ "$keep_workdir" = true ]; then
  printf 'scheduled-backup-restore: workdir=%s\n' "$workdir" >&2
fi

# Scrub the exact connection string before any child diagnostic reaches disk.
# The synchronous pipeline also finishes draining stderr before cleanup runs.
redact_errors() {
  local line
  while IFS= read -r line || [ -n "$line" ]; do
    line=${line//"$RESTORE_DATABASE_URL"/[redacted]}
    printf '%s\n' "$line"
  done
}

private_errors() {
  { "$@" 2>&1 1>&3 | redact_errors >>"$errors"; } 3>&1
}

# Do not load the operator's psql startup file. libpq's PGDATABASE environment
# fallback does not expand connection URIs; --dbname accepts URI and conninfo.
# The URL is visible in psql's argv, but never traced or written to diagnostics.
sql() {
  PGOPTIONS='-c default_transaction_read_only=on -c search_path=pg_catalog -c row_security=off' \
    private_errors psql --no-psqlrc --no-password --set=ON_ERROR_STOP=1 \
      --dbname "$RESTORE_DATABASE_URL" --tuples-only --no-align --command "$1"
}

# System and extension-owned objects are allowed in a fresh database. Include
# views, materialized views, foreign/partitioned tables, sequences and routines.
empty_count=$(sql "/* nrc1:empty */
SELECT
  (SELECT count(*) FROM pg_catalog.pg_class c
   JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
   WHERE n.nspname !~ '^pg_' AND n.nspname <> 'information_schema'
     AND c.relkind IN ('r', 'p', 'f', 'v', 'm', 'S')
     AND NOT EXISTS (SELECT 1 FROM pg_catalog.pg_depend d
       WHERE d.classid = 'pg_catalog.pg_class'::regclass AND d.objid = c.oid
         AND d.refclassid = 'pg_catalog.pg_extension'::regclass AND d.deptype = 'e'))
  + (SELECT count(*) FROM pg_catalog.pg_proc p
   JOIN pg_catalog.pg_namespace n ON n.oid = p.pronamespace
   WHERE n.nspname !~ '^pg_' AND n.nspname <> 'information_schema'
     AND NOT EXISTS (SELECT 1 FROM pg_catalog.pg_depend d
       WHERE d.classid = 'pg_catalog.pg_proc'::regclass AND d.objid = p.oid
         AND d.refclassid = 'pg_catalog.pg_extension'::regclass AND d.deptype = 'e'));") || fail check_failed
[[ "$empty_count" =~ ^[0-9]+$ ]] || fail check_failed
[ "$empty_count" = 0 ] || fail target_not_empty

if [ -n "$object" ]; then
  input_file="$workdir/object.age"
  private_errors aws --endpoint-url "$endpoint" s3 cp "$object" "$input_file" > /dev/null || fail fetch
  object_name=${object##*/}
else
  object_name=${input_file##*/}
fi
[ -s "$input_file" ] || fail empty_object
bytes=$(wc -c <"$input_file" 2>>"$errors") || fail object_metadata
bytes=${bytes//[[:space:]]/}
digest=$("${hash_command[@]}" <"$input_file" 2>>"$errors") || fail object_metadata
digest=${digest%% *}
[[ "$bytes" =~ ^[0-9]+$ && "$digest" =~ ^[a-fA-F0-9]{64}$ ]] || fail object_metadata
digest=${digest:0:12}
# Keep the one-line receipt safe for terminal/log use with unusual object keys.
object_name=${object_name//[^a-zA-Z0-9._-]/_}

# Capture all diagnostics, including COPY data and connection errors. Inspect
# each pipeline status: a downstream failure can also SIGPIPE an upstream stage.
set +e
private_errors age --decrypt -i "$identity" "$input_file" |
  private_errors gzip --decompress |
  private_errors psql --no-psqlrc --no-password --set=ON_ERROR_STOP=1 \
    --dbname "$RESTORE_DATABASE_URL" --single-transaction > /dev/null
pipeline_status=("${PIPESTATUS[@]}")
set -e
if [ "${pipeline_status[2]}" -ne 0 ]; then
  fail restore
elif [ "${pipeline_status[0]}" -ne 0 ] && [ "${pipeline_status[0]}" -ne 141 ]; then
  fail decrypt
elif [ "${pipeline_status[1]}" -ne 0 ]; then
  fail decompress
elif [ "${pipeline_status[0]}" -ne 0 ]; then
  fail decrypt
fi

# Goose uses the latest record for each version and returns the most recently
# applied surviving record (migrate.go delegates to goose.GetDBVersionContext).
# MAX(version_id) alone would incorrectly accept a subsequently rolled-back row.
schema_version=$(sql "/* nrc1:schema */
SELECT version_id FROM (
  SELECT DISTINCT ON (version_id) id, version_id, is_applied
  FROM public.goose_db_version ORDER BY version_id, id DESC
) latest WHERE is_applied ORDER BY id DESC LIMIT 1;") || fail schema_version_missing
[[ "$schema_version" =~ ^[1-9][0-9]*$ ]] || fail schema_version_missing
if [ -n "$expected_version" ] && [ "$schema_version" != "$expected_version" ]; then
  fail schema_version_mismatch
fi

user_tables="FROM pg_catalog.pg_class c
JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
WHERE n.nspname !~ '^pg_' AND n.nspname <> 'information_schema'
  AND c.relkind IN ('r', 'p', 'f')
  AND NOT EXISTS (SELECT 1 FROM pg_catalog.pg_depend d
    WHERE d.classid = 'pg_catalog.pg_class'::regclass AND d.objid = c.oid
      AND d.refclassid = 'pg_catalog.pg_extension'::regclass AND d.deptype = 'e')"
tables=$(sql "/* nrc1:tables */ SELECT count(*) $user_tables;") || fail check_failed
[[ "$tables" =~ ^[0-9]+$ ]] || fail check_failed
[ "$tables" != 0 ] || fail no_user_tables
invalid_indexes=$(sql '/* nrc1:indexes */ SELECT count(*) FROM pg_catalog.pg_index WHERE NOT indisvalid;') || fail check_failed
[[ "$invalid_indexes" =~ ^[0-9]+$ ]] || fail check_failed
[ "$invalid_indexes" = 0 ] || fail invalid_indexes
unvalidated_constraints=$(sql '/* nrc1:constraints */ SELECT count(*) FROM pg_catalog.pg_constraint WHERE NOT convalidated;') || fail check_failed
[[ "$unvalidated_constraints" =~ ^[0-9]+$ ]] || fail check_failed
[ "$unvalidated_constraints" = 0 ] || fail unvalidated_constraints

# ONLY avoids counting inherited/partition rows twice. format's %I quotes both
# identifiers; query_to_xml executes SELECTs without creating helper functions.
rows=$(sql "/* nrc1:rows */
SELECT COALESCE(sum((xpath('/row/n/text()', query_to_xml(
  format('SELECT count(*) AS n FROM ONLY %I.%I', n.nspname, c.relname),
  false, true, '')))[1]::text::numeric), 0) $user_tables;") || fail check_failed
[[ "$rows" =~ ^[0-9]+$ ]] || fail check_failed
printf 'scheduled-backup-restore: PASS object=%s bytes=%s sha256=%s schema_version=%s tables=%s rows=%s\n' \
  "$object_name" "$bytes" "$digest" "$schema_version" "$tables" "$rows"
