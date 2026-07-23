-- +goose Up
-- Validation scans existing rows while holding SHARE UPDATE EXCLUSIVE rather
-- than the ACCESS EXCLUSIVE lock that a one-step validated ADD CHECK would
-- retain. The legacy constraint remains active throughout this migration.
ALTER TABLE memory_curation_run_inputs
  VALIDATE CONSTRAINT memory_curation_run_inputs_retention_check;

-- +goose Down
-- Return to the schema-62 staged state: the widened constraint remains
-- enforced for new writes but is again marked not-yet-validated.
ALTER TABLE memory_curation_run_inputs
  DROP CONSTRAINT memory_curation_run_inputs_retention_check;
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
