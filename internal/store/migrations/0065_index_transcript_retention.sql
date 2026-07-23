-- +goose NO TRANSACTION
-- +goose Up
-- Finds accounts with an active retention policy without creating a large
-- index over transcript data. Per-account oldest-first probes use the existing
-- (account_id, updated_at DESC, id) index in reverse and select only a bounded
-- set of accounts before locking conversations.
DROP INDEX CONCURRENTLY IF EXISTS accounts_transcript_retention_policy_idx;
CREATE INDEX CONCURRENTLY accounts_transcript_retention_policy_idx
  ON accounts (id)
  WHERE plan_policies ? 'transcript_retention_days';

-- Retention checks these provenance edges before deleting a whole
-- conversation. PostgreSQL does not automatically index the referencing side
-- of a foreign key; without these indexes one held conversation could turn
-- each bounded sweep into a full evidence/curation scan.
DROP INDEX CONCURRENTLY IF EXISTS memory_evidence_by_source_transcript;
CREATE INDEX CONCURRENTLY memory_evidence_by_source_transcript
  ON memory_evidence (source_transcript_id)
  WHERE source_transcript_id IS NOT NULL;

DROP INDEX CONCURRENTLY IF EXISTS memory_curation_run_inputs_by_transcript;
CREATE INDEX CONCURRENTLY memory_curation_run_inputs_by_transcript
  ON memory_curation_run_inputs (transcript_id)
  WHERE transcript_id IS NOT NULL;

DROP INDEX CONCURRENTLY IF EXISTS memory_curation_run_inputs_by_transcript_cursor;
CREATE INDEX CONCURRENTLY memory_curation_run_inputs_by_transcript_cursor
  ON memory_curation_run_inputs (cursor_stream_id)
  WHERE input_kind = 'cursor'
    AND cursor_source_kind = 'transcript'
    AND cursor_stream_id IS NOT NULL;

-- +goose Down
-- Refuse before dropping any retention index when the preceding schema
-- migration is no longer reversible.
-- +goose StatementBegin
DO $$
BEGIN
  IF EXISTS (
    SELECT 1
      FROM memory_curation_run_inputs
     WHERE transcript_pruned_at IS NOT NULL
  ) THEN
    RAISE EXCEPTION
      'cannot remove transcript retention indexes while pruned curation inputs exist';
  END IF;
END
$$;
-- +goose StatementEnd

DROP INDEX CONCURRENTLY IF EXISTS memory_curation_run_inputs_by_transcript_cursor;
DROP INDEX CONCURRENTLY IF EXISTS memory_curation_run_inputs_by_transcript;
DROP INDEX CONCURRENTLY IF EXISTS memory_evidence_by_source_transcript;
DROP INDEX CONCURRENTLY IF EXISTS accounts_transcript_retention_policy_idx;
