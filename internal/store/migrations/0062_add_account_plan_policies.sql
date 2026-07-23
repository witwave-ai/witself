-- +goose Up
-- Cells store the resolved behavioral policy snapshot, not the plan matrix.
-- Missing transcript_retention_days means indefinite retention.
ALTER TABLE accounts
  ADD COLUMN IF NOT EXISTS plan_policies JSONB NOT NULL DEFAULT '{}'::jsonb,
  ADD COLUMN IF NOT EXISTS plan_applied_at TIMESTAMPTZ,
  ADD COLUMN IF NOT EXISTS plan_snapshot_revision BIGINT NOT NULL DEFAULT 0,
  ADD COLUMN IF NOT EXISTS plan_snapshot_hash TEXT NOT NULL DEFAULT '';

-- One cell-local row serializes sweep selection across replicas and carries a
-- restart-safe wraparound account cursor. It is operational state, not account
-- data, so it is intentionally outside account archives.
CREATE TABLE IF NOT EXISTS transcript_retention_sweep_state (
    singleton               BOOLEAN     PRIMARY KEY DEFAULT TRUE,
    preview_account_cursor  TEXT        NOT NULL DEFAULT '',
    enforce_account_cursor  TEXT        NOT NULL DEFAULT '',
    generation              BIGINT      NOT NULL DEFAULT 0,
    next_run_at             TIMESTAMPTZ NOT NULL DEFAULT '-infinity'::timestamptz,
    updated_at              TIMESTAMPTZ NOT NULL DEFAULT statement_timestamp(),
    CHECK (singleton),
    CHECK (octet_length(preview_account_cursor) <= 512),
    CHECK (octet_length(enforce_account_cursor) <= 512),
    CHECK (generation BETWEEN 0 AND 4611686018427387903)
);
INSERT INTO transcript_retention_sweep_state (singleton)
VALUES (TRUE)
ON CONFLICT (singleton) DO NOTHING;

-- A cursor per policy account bounds candidate examination even when the
-- oldest expired conversations are all protected by provenance holds.
-- Cursors are cell-local worker state and can be rebuilt after evacuation.
CREATE TABLE IF NOT EXISTS transcript_retention_account_scan_state (
    mode                  TEXT        NOT NULL,
    account_id            TEXT        NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    retention_days        INTEGER     NOT NULL,
    cycle_cutoff          TIMESTAMPTZ NOT NULL,
    last_activity_at      TIMESTAMPTZ,
    last_conversation_id  TEXT,
    generation            BIGINT      NOT NULL DEFAULT 0,
    updated_at            TIMESTAMPTZ NOT NULL DEFAULT statement_timestamp(),
    PRIMARY KEY (mode, account_id),
    CHECK (mode IN ('preview', 'enforce')),
    CHECK (retention_days BETWEEN 1 AND 36500),
    CHECK (
      (last_activity_at IS NULL AND last_conversation_id IS NULL)
      OR
      (last_activity_at IS NOT NULL AND last_conversation_id IS NOT NULL)
    ),
    CHECK (
      last_conversation_id IS NULL
      OR octet_length(last_conversation_id) <= 512
    ),
    CHECK (generation BETWEEN 0 AND 4611686018427387903)
);

-- Terminal curation runs retain their immutable input rows, ordinals, and
-- receipt counters after the source transcript ages out. The nullable marker
-- distinguishes an intentional retention detach from malformed input: direct
-- transcript inputs lose transcript_id, and transcript cursor inputs lose
-- cursor_stream_id, while their value-free range/coverage metadata remains.
ALTER TABLE memory_curation_run_inputs
  ADD COLUMN IF NOT EXISTS transcript_pruned_at TIMESTAMPTZ;

-- Keep the legacy validated constraint in force while the widened retention
-- shape is installed. NOT VALID takes only a brief metadata lock and still
-- enforces the new shape for all writes; migration 0063 validates existing
-- rows without holding ACCESS EXCLUSIVE, and 0064 performs the brief swap.
ALTER TABLE memory_curation_run_inputs
  ADD CONSTRAINT memory_curation_run_inputs_retention_check
  CHECK (
    (input_kind = 'memory' AND memory_id IS NOT NULL AND memory_version IS NOT NULL AND
     evidence_id IS NULL AND transcript_id IS NULL AND sequence_from IS NULL AND sequence_until IS NULL AND
     cursor_source_kind IS NULL AND cursor_stream_id IS NULL AND
     cursor_expected_prior IS NULL AND cursor_upper IS NULL AND
     coverage_counts IS NULL AND transcript_pruned_at IS NULL)
    OR
    (input_kind = 'evidence' AND memory_id IS NULL AND memory_version IS NULL AND
     evidence_id IS NOT NULL AND transcript_id IS NULL AND sequence_from IS NULL AND sequence_until IS NULL AND
     cursor_source_kind IS NULL AND cursor_stream_id IS NULL AND
     cursor_expected_prior IS NULL AND cursor_upper IS NULL AND
     coverage_counts IS NULL AND transcript_pruned_at IS NULL)
    OR
    (input_kind = 'transcript' AND memory_id IS NULL AND memory_version IS NULL AND
     evidence_id IS NULL AND
     ((transcript_id IS NOT NULL AND transcript_pruned_at IS NULL) OR
      (transcript_id IS NULL AND transcript_pruned_at IS NOT NULL)) AND
     sequence_from IS NOT NULL AND sequence_until IS NOT NULL AND sequence_until >= sequence_from AND
     cursor_source_kind IS NULL AND cursor_stream_id IS NULL AND
     cursor_expected_prior IS NULL AND cursor_upper IS NULL AND
     coverage_counts IS NULL)
    OR
    (input_kind = 'cursor' AND memory_id IS NULL AND memory_version IS NULL AND
     evidence_id IS NULL AND transcript_id IS NULL AND sequence_from IS NULL AND sequence_until IS NULL AND
     cursor_source_kind IS NOT NULL AND
     ((cursor_stream_id IS NOT NULL AND transcript_pruned_at IS NULL) OR
      (cursor_source_kind = 'transcript' AND cursor_stream_id IS NULL AND
       transcript_pruned_at IS NOT NULL)) AND
     cursor_expected_prior IS NOT NULL AND cursor_upper IS NOT NULL AND
     cursor_upper >= cursor_expected_prior AND coverage_counts IS NULL)
    OR
    (input_kind = 'transcript_coverage' AND memory_id IS NULL AND memory_version IS NULL AND
     evidence_id IS NULL AND
     ((transcript_id IS NOT NULL AND transcript_pruned_at IS NULL) OR
      (transcript_id IS NULL AND transcript_pruned_at IS NOT NULL)) AND
     sequence_from IS NOT NULL AND sequence_until IS NOT NULL AND sequence_until >= sequence_from AND
     cursor_source_kind IS NULL AND cursor_stream_id IS NULL AND
     cursor_expected_prior IS NULL AND cursor_upper IS NULL AND
     coverage_counts IS NOT NULL)
  ) NOT VALID;

-- +goose Down
-- Detachment intentionally discards the live transcript identity. Refuse to
-- downgrade a used cell because the old constraint cannot represent retained
-- pruned input metadata.
-- +goose StatementBegin
DO $$
BEGIN
  IF EXISTS (
    SELECT 1
      FROM memory_curation_run_inputs
     WHERE transcript_pruned_at IS NOT NULL
  ) THEN
    RAISE EXCEPTION
      'cannot remove transcript retention while pruned curation inputs exist';
  END IF;
END
$$;
-- +goose StatementEnd

ALTER TABLE memory_curation_run_inputs
  DROP CONSTRAINT memory_curation_run_inputs_retention_check;
ALTER TABLE memory_curation_run_inputs
  DROP COLUMN IF EXISTS transcript_pruned_at;
DROP TABLE IF EXISTS transcript_retention_account_scan_state;
DROP TABLE IF EXISTS transcript_retention_sweep_state;
ALTER TABLE accounts
  DROP COLUMN IF EXISTS plan_snapshot_hash,
  DROP COLUMN IF EXISTS plan_snapshot_revision,
  DROP COLUMN IF EXISTS plan_applied_at,
  DROP COLUMN IF EXISTS plan_policies;
