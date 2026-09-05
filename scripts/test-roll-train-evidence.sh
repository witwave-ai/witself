#!/usr/bin/env bash
set -euo pipefail

SOURCE_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
TEST_ROOT_RAW="$(mktemp -d "${TMPDIR:-/tmp}/witself-roll-train-evidence-test.XXXXXX")"
TEST_ROOT="$(cd "$TEST_ROOT_RAW" && pwd -P)"
TRAIN="$SOURCE_ROOT/scripts/roll-train.sh"
BACKUP=civo-sandbox-use1-backup
SERVING=civo-sandbox-usw2-dev
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
command -v go >/dev/null 2>&1 || fail 'go is required'
command -v shasum >/dev/null 2>&1 || fail 'shasum is required'
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
    grep -Fq -- '--backup-evidence requires --cells civo-sandbox-use1-backup,civo-sandbox-usw2-dev' "$TEST_ROOT/output" ||
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

printf 'roll train evidence tests passed\n'
