-- +goose Up
-- Fast-forward observational coverage (narrative-memory-and-curation.md). A
-- transcript stream deep in tool-event backlog freezes one value-free
-- transcript_coverage input per window — inclusive bounds plus deterministic
-- per-class entry counts — while signal entries still materialize
-- individually, so the stream cursor drains the whole window in one reviewed
-- cycle instead of five hundred entries at a time.
ALTER TABLE memory_curation_run_inputs
    ADD COLUMN coverage_counts JSONB;

ALTER TABLE memory_curation_run_inputs
    DROP CONSTRAINT memory_curation_run_inputs_input_kind_check;
ALTER TABLE memory_curation_run_inputs
    ADD CONSTRAINT memory_curation_run_inputs_input_kind_check
    CHECK (input_kind IN
      ('memory', 'evidence', 'transcript', 'cursor', 'transcript_coverage'));

ALTER TABLE memory_curation_run_inputs
    DROP CONSTRAINT memory_curation_run_inputs_check;
ALTER TABLE memory_curation_run_inputs
    ADD CONSTRAINT memory_curation_run_inputs_check
    CHECK (
      (input_kind = 'memory' AND memory_id IS NOT NULL AND memory_version IS NOT NULL AND
       evidence_id IS NULL AND transcript_id IS NULL AND sequence_from IS NULL AND sequence_until IS NULL AND
       cursor_source_kind IS NULL AND cursor_stream_id IS NULL AND
       cursor_expected_prior IS NULL AND cursor_upper IS NULL AND
       coverage_counts IS NULL)
      OR
      (input_kind = 'evidence' AND memory_id IS NULL AND memory_version IS NULL AND
       evidence_id IS NOT NULL AND transcript_id IS NULL AND sequence_from IS NULL AND sequence_until IS NULL AND
       cursor_source_kind IS NULL AND cursor_stream_id IS NULL AND
       cursor_expected_prior IS NULL AND cursor_upper IS NULL AND
       coverage_counts IS NULL)
      OR
      (input_kind = 'transcript' AND memory_id IS NULL AND memory_version IS NULL AND
       evidence_id IS NULL AND transcript_id IS NOT NULL AND
       sequence_from IS NOT NULL AND sequence_until IS NOT NULL AND sequence_until >= sequence_from AND
       cursor_source_kind IS NULL AND cursor_stream_id IS NULL AND
       cursor_expected_prior IS NULL AND cursor_upper IS NULL AND
       coverage_counts IS NULL)
      OR
      (input_kind = 'cursor' AND memory_id IS NULL AND memory_version IS NULL AND
       evidence_id IS NULL AND transcript_id IS NULL AND sequence_from IS NULL AND sequence_until IS NULL AND
       cursor_source_kind IS NOT NULL AND cursor_stream_id IS NOT NULL AND
       cursor_expected_prior IS NOT NULL AND cursor_upper IS NOT NULL AND
       cursor_upper >= cursor_expected_prior AND
       coverage_counts IS NULL)
      OR
      (input_kind = 'transcript_coverage' AND memory_id IS NULL AND memory_version IS NULL AND
       evidence_id IS NULL AND transcript_id IS NOT NULL AND
       sequence_from IS NOT NULL AND sequence_until IS NOT NULL AND sequence_until >= sequence_from AND
       cursor_source_kind IS NULL AND cursor_stream_id IS NULL AND
       cursor_expected_prior IS NULL AND cursor_upper IS NULL AND
       coverage_counts IS NOT NULL)
    );

-- +goose Down
ALTER TABLE memory_curation_run_inputs
    DROP CONSTRAINT memory_curation_run_inputs_check;
ALTER TABLE memory_curation_run_inputs
    ADD CONSTRAINT memory_curation_run_inputs_check
    CHECK (
      (input_kind = 'memory' AND memory_id IS NOT NULL AND memory_version IS NOT NULL AND
       evidence_id IS NULL AND transcript_id IS NULL AND sequence_from IS NULL AND sequence_until IS NULL AND
       cursor_source_kind IS NULL AND cursor_stream_id IS NULL AND
       cursor_expected_prior IS NULL AND cursor_upper IS NULL)
      OR
      (input_kind = 'evidence' AND memory_id IS NULL AND memory_version IS NULL AND
       evidence_id IS NOT NULL AND transcript_id IS NULL AND sequence_from IS NULL AND sequence_until IS NULL AND
       cursor_source_kind IS NULL AND cursor_stream_id IS NULL AND
       cursor_expected_prior IS NULL AND cursor_upper IS NULL)
      OR
      (input_kind = 'transcript' AND memory_id IS NULL AND memory_version IS NULL AND
       evidence_id IS NULL AND transcript_id IS NOT NULL AND
       sequence_from IS NOT NULL AND sequence_until IS NOT NULL AND sequence_until >= sequence_from AND
       cursor_source_kind IS NULL AND cursor_stream_id IS NULL AND
       cursor_expected_prior IS NULL AND cursor_upper IS NULL)
      OR
      (input_kind = 'cursor' AND memory_id IS NULL AND memory_version IS NULL AND
       evidence_id IS NULL AND transcript_id IS NULL AND sequence_from IS NULL AND sequence_until IS NULL AND
       cursor_source_kind IS NOT NULL AND cursor_stream_id IS NOT NULL AND
       cursor_expected_prior IS NOT NULL AND cursor_upper IS NOT NULL AND
       cursor_upper >= cursor_expected_prior)
    );
ALTER TABLE memory_curation_run_inputs
    DROP CONSTRAINT memory_curation_run_inputs_input_kind_check;
ALTER TABLE memory_curation_run_inputs
    ADD CONSTRAINT memory_curation_run_inputs_input_kind_check
    CHECK (input_kind IN ('memory', 'evidence', 'transcript', 'cursor'));
ALTER TABLE memory_curation_run_inputs
    DROP COLUMN coverage_counts;
