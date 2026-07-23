-- +goose Up
-- Both constraints are validated before this point. The only
-- ACCESS EXCLUSIVE work is this metadata-only drop/rename swap; there is no
-- table scan while the strongest lock is held.
ALTER TABLE memory_curation_run_inputs
  DROP CONSTRAINT memory_curation_run_inputs_check;
ALTER TABLE memory_curation_run_inputs
  RENAME CONSTRAINT memory_curation_run_inputs_retention_check
  TO memory_curation_run_inputs_check;

-- +goose Down
-- A used retention schema cannot be represented by the legacy constraint.
-- Refuse before changing names or rescanning so a failed downgrade leaves the
-- current validated constraint intact.
-- +goose StatementBegin
DO $$
BEGIN
  IF EXISTS (
    SELECT 1
      FROM memory_curation_run_inputs
     WHERE transcript_pruned_at IS NOT NULL
  ) THEN
    RAISE EXCEPTION
      'cannot restore legacy curation input constraint while pruned inputs exist';
  END IF;
END
$$;
-- +goose StatementEnd

-- Reconstruct the exact schema-63 state: the widened validated constraint has
-- its staging name and the legacy validated constraint is canonical. Rollback
-- is operator-controlled; unlike the forward rollout, this validation may hold
-- the transaction's metadata lock while it verifies the downgrade precondition.
ALTER TABLE memory_curation_run_inputs
  ADD CONSTRAINT memory_curation_run_inputs_legacy_check
  CHECK (
    (input_kind = 'memory' AND memory_id IS NOT NULL AND memory_version IS NOT NULL AND
     evidence_id IS NULL AND transcript_id IS NULL AND sequence_from IS NULL AND sequence_until IS NULL AND
     cursor_source_kind IS NULL AND cursor_stream_id IS NULL AND
     cursor_expected_prior IS NULL AND cursor_upper IS NULL AND coverage_counts IS NULL)
    OR
    (input_kind = 'evidence' AND memory_id IS NULL AND memory_version IS NULL AND
     evidence_id IS NOT NULL AND transcript_id IS NULL AND sequence_from IS NULL AND sequence_until IS NULL AND
     cursor_source_kind IS NULL AND cursor_stream_id IS NULL AND
     cursor_expected_prior IS NULL AND cursor_upper IS NULL AND coverage_counts IS NULL)
    OR
    (input_kind = 'transcript' AND memory_id IS NULL AND memory_version IS NULL AND
     evidence_id IS NULL AND transcript_id IS NOT NULL AND
     sequence_from IS NOT NULL AND sequence_until IS NOT NULL AND sequence_until >= sequence_from AND
     cursor_source_kind IS NULL AND cursor_stream_id IS NULL AND
     cursor_expected_prior IS NULL AND cursor_upper IS NULL AND coverage_counts IS NULL)
    OR
    (input_kind = 'cursor' AND memory_id IS NULL AND memory_version IS NULL AND
     evidence_id IS NULL AND transcript_id IS NULL AND sequence_from IS NULL AND sequence_until IS NULL AND
     cursor_source_kind IS NOT NULL AND cursor_stream_id IS NOT NULL AND
     cursor_expected_prior IS NOT NULL AND cursor_upper IS NOT NULL AND
     cursor_upper >= cursor_expected_prior AND coverage_counts IS NULL)
    OR
    (input_kind = 'transcript_coverage' AND memory_id IS NULL AND memory_version IS NULL AND
     evidence_id IS NULL AND transcript_id IS NOT NULL AND
     sequence_from IS NOT NULL AND sequence_until IS NOT NULL AND sequence_until >= sequence_from AND
     cursor_source_kind IS NULL AND cursor_stream_id IS NULL AND
     cursor_expected_prior IS NULL AND cursor_upper IS NULL AND coverage_counts IS NOT NULL)
  ) NOT VALID;
ALTER TABLE memory_curation_run_inputs
  VALIDATE CONSTRAINT memory_curation_run_inputs_legacy_check;
ALTER TABLE memory_curation_run_inputs
  RENAME CONSTRAINT memory_curation_run_inputs_check
  TO memory_curation_run_inputs_retention_check;
ALTER TABLE memory_curation_run_inputs
  RENAME CONSTRAINT memory_curation_run_inputs_legacy_check
  TO memory_curation_run_inputs_check;
