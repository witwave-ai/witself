-- +goose Up
-- Avatar creative payloads are intentionally bounded independently of their
-- immutable version envelopes. Automatic compaction may clear only the SVG,
-- description, and visual specification. Identity, hashes, provenance, and
-- lifecycle ledgers remain durable and portable.

ALTER TABLE agent_avatar_profiles
  ADD COLUMN retained_payload_count_limit INTEGER NOT NULL DEFAULT 20,
  ADD COLUMN retained_payload_byte_limit  BIGINT  NOT NULL DEFAULT 2097152,
  ADD CONSTRAINT agent_avatar_profiles_retained_payload_count_limit_check
    CHECK (retained_payload_count_limit BETWEEN 4 AND 1000),
  ADD CONSTRAINT agent_avatar_profiles_retained_payload_byte_limit_check
    CHECK (retained_payload_byte_limit BETWEEN 524288 AND 67108864);

ALTER TABLE agent_avatar_versions
  ADD COLUMN payload_state             TEXT        NOT NULL DEFAULT 'full',
  ADD COLUMN payload_bytes             BIGINT,
  ADD COLUMN payload_compacted_at      TIMESTAMPTZ,
  ADD COLUMN payload_compaction_reason TEXT,
  -- Filled by the startup backfill immediately after Goose commits schema 51.
  -- Keeping this nullable during the SQL phase lets the application derive
  -- the normalized locked-layer projection with the exact domain algorithm.
  ADD COLUMN locked_layers_sha256      TEXT,
  -- Reserved for the bounded perceptual continuity projection. The domain
  -- rollout computes it; quota compaction may retain it only while a full,
  -- same-style, agent-authored direct child still needs the boundary proof.
  ADD COLUMN continuity_fingerprint    BYTEA;

-- Schema 50 writers do not name payload_bytes. Keep those writers valid for
-- the entire mixed-version rollout by deriving the exact retained byte count
-- before the schema-51 NOT NULL constraint is checked. The trigger remains in
-- place until a later contract migration explicitly retires schema-50 writers.
-- +goose StatementBegin
CREATE FUNCTION witself_fill_avatar_payload_bytes()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
  IF NEW.payload_state = 'full' AND NEW.payload_bytes IS NULL THEN
    NEW.payload_bytes := octet_length(NEW.svg)
                       + octet_length(NEW.description)
                       + octet_length(NEW.visual_spec::text);
  END IF;
  RETURN NEW;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER agent_avatar_versions_fill_payload_bytes
BEFORE INSERT ON agent_avatar_versions
FOR EACH ROW
EXECUTE FUNCTION witself_fill_avatar_payload_bytes();

UPDATE agent_avatar_versions
   SET payload_bytes = octet_length(svg)
                     + octet_length(description)
                     + octet_length(visual_spec::text);

ALTER TABLE agent_avatar_versions
  ALTER COLUMN payload_bytes SET NOT NULL,
  ALTER COLUMN svg DROP NOT NULL,
  ALTER COLUMN description DROP NOT NULL,
  ALTER COLUMN visual_spec DROP NOT NULL,
  DROP CONSTRAINT agent_avatar_versions_svg_check,
  DROP CONSTRAINT agent_avatar_versions_description_check,
  DROP CONSTRAINT agent_avatar_versions_visual_spec_check,
  ADD CONSTRAINT agent_avatar_versions_payload_state_check
    CHECK (payload_state IN ('full', 'compacted')),
  ADD CONSTRAINT agent_avatar_versions_payload_bytes_check
    CHECK (payload_bytes BETWEEN 1 AND 131072),
  ADD CONSTRAINT agent_avatar_versions_locked_layers_sha256_check
    CHECK (locked_layers_sha256 IS NULL OR
           locked_layers_sha256 ~ '^[0-9a-f]{64}$'),
  ADD CONSTRAINT agent_avatar_versions_continuity_fingerprint_check
    CHECK (continuity_fingerprint IS NULL OR
           (payload_state = 'compacted' AND
            octet_length(continuity_fingerprint) = 38092)),
  ADD CONSTRAINT agent_avatar_versions_svg_check
    CHECK (svg IS NULL OR octet_length(svg) BETWEEN 1 AND 65536),
  ADD CONSTRAINT agent_avatar_versions_description_check
    CHECK (description IS NULL OR octet_length(description) BETWEEN 1 AND 4096),
  ADD CONSTRAINT agent_avatar_versions_visual_spec_check
    CHECK (visual_spec IS NULL OR
           (jsonb_typeof(visual_spec) = 'object' AND
            octet_length(visual_spec::text) <= 16384)),
  ADD CONSTRAINT agent_avatar_versions_payload_shape_check
    CHECK (
      (payload_state = 'full' AND svg IS NOT NULL AND description IS NOT NULL AND
       visual_spec IS NOT NULL AND payload_compacted_at IS NULL AND
       payload_compaction_reason IS NULL)
      OR
      (payload_state = 'compacted' AND svg IS NULL AND description IS NULL AND
       visual_spec IS NULL AND payload_compacted_at IS NOT NULL AND
       payload_compaction_reason = 'quota')
    );

CREATE INDEX agent_avatar_versions_full_payloads
    ON agent_avatar_versions
       (account_id, realm_id, agent_id, lineage_generation, version)
    WHERE payload_state = 'full';

ALTER TABLE avatar_mutation_receipts
  DROP CONSTRAINT avatar_mutation_receipts_operation_check,
  ADD CONSTRAINT avatar_mutation_receipts_operation_check
    CHECK (operation IN
      ('propose', 'activate', 'reject', 'rollback', 'reset', 'set_policy',
       'set_quota', 'set_style', 'fail'));

-- +goose Down
-- A compacted payload cannot be reconstructed. Refuse an unsafe downgrade
-- rather than deleting its immutable metadata or fabricating creative data.
-- +goose StatementBegin
DO $$
BEGIN
  IF EXISTS (
    SELECT 1 FROM agent_avatar_versions WHERE payload_state = 'compacted'
  ) THEN
    RAISE EXCEPTION 'cannot downgrade schema 51 while compacted avatar payloads exist';
  END IF;
END $$;
-- +goose StatementEnd

ALTER TABLE avatar_mutation_receipts
  DROP CONSTRAINT avatar_mutation_receipts_operation_check,
  ADD CONSTRAINT avatar_mutation_receipts_operation_check
    CHECK (operation IN
      ('propose', 'activate', 'reject', 'rollback', 'reset', 'set_policy',
       'set_style', 'fail'));

DROP INDEX agent_avatar_versions_full_payloads;

DROP TRIGGER agent_avatar_versions_fill_payload_bytes
  ON agent_avatar_versions;
DROP FUNCTION witself_fill_avatar_payload_bytes();

ALTER TABLE agent_avatar_versions
  DROP CONSTRAINT agent_avatar_versions_payload_shape_check,
  DROP CONSTRAINT agent_avatar_versions_continuity_fingerprint_check,
  DROP CONSTRAINT agent_avatar_versions_locked_layers_sha256_check,
  DROP CONSTRAINT agent_avatar_versions_visual_spec_check,
  DROP CONSTRAINT agent_avatar_versions_description_check,
  DROP CONSTRAINT agent_avatar_versions_svg_check,
  DROP CONSTRAINT agent_avatar_versions_payload_bytes_check,
  DROP CONSTRAINT agent_avatar_versions_payload_state_check,
  ALTER COLUMN svg SET NOT NULL,
  ALTER COLUMN description SET NOT NULL,
  ALTER COLUMN visual_spec SET NOT NULL,
  ADD CONSTRAINT agent_avatar_versions_svg_check
    CHECK (octet_length(svg) BETWEEN 1 AND 65536),
  ADD CONSTRAINT agent_avatar_versions_description_check
    CHECK (octet_length(description) BETWEEN 1 AND 4096),
  ADD CONSTRAINT agent_avatar_versions_visual_spec_check
    CHECK (jsonb_typeof(visual_spec) = 'object' AND
           octet_length(visual_spec::text) <= 16384),
  DROP COLUMN payload_compaction_reason,
  DROP COLUMN payload_compacted_at,
  DROP COLUMN payload_bytes,
  DROP COLUMN payload_state,
  DROP COLUMN locked_layers_sha256,
  DROP COLUMN continuity_fingerprint;

ALTER TABLE agent_avatar_profiles
  DROP CONSTRAINT agent_avatar_profiles_retained_payload_byte_limit_check,
  DROP CONSTRAINT agent_avatar_profiles_retained_payload_count_limit_check,
  DROP COLUMN retained_payload_byte_limit,
  DROP COLUMN retained_payload_count_limit;
