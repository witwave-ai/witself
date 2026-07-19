-- +goose Up
-- Old writers omit this column and therefore land safely as legacy. New
-- writers explicitly claim perceptual-v1 only after validating the exact SVG
-- bytes at the renderer boundary.
ALTER TABLE agent_avatar_versions
  ADD COLUMN renderer_profile TEXT NOT NULL DEFAULT 'legacy',
  ADD CONSTRAINT agent_avatar_versions_renderer_profile_check
    CHECK (renderer_profile IN ('legacy', 'perceptual-v1'));

-- Fingerprints written before renderer provenance cannot prove that their
-- source bytes satisfied perceptual-v1. Quarantine those compacted rows under
-- the durable locked-layer digest contract and discard the untrusted cache.
UPDATE agent_avatar_versions
   SET continuity_fingerprint = NULL
 WHERE continuity_fingerprint IS NOT NULL;

-- A continuity fingerprint is evidence produced by perceptual-v1 and must
-- never be attached to bytes whose renderer provenance is legacy.
ALTER TABLE agent_avatar_versions
  DROP CONSTRAINT agent_avatar_versions_continuity_fingerprint_check,
  ADD CONSTRAINT agent_avatar_versions_continuity_fingerprint_check
    CHECK (continuity_fingerprint IS NULL OR
           (renderer_profile = 'perceptual-v1' AND
            payload_state = 'compacted' AND
            octet_length(continuity_fingerprint) = 38092));

-- +goose Down
-- Removing the column while v1 rows exist would silently erase the immutable
-- proof that separates renderer-compatible baselines from legacy bytes.
-- +goose StatementBegin
DO $$
BEGIN
  -- Serialize the safety check with every writer so a perceptual-v1 row
  -- cannot race into the table between the check and column removal.
  LOCK TABLE agent_avatar_versions IN ACCESS EXCLUSIVE MODE;
  IF EXISTS (
    SELECT 1 FROM agent_avatar_versions
     WHERE renderer_profile = 'perceptual-v1'
  ) THEN
    RAISE EXCEPTION 'cannot downgrade schema 54 while perceptual-v1 avatar versions exist';
  END IF;
  -- Up discards pre-profile WAPF because it cannot prove renderer provenance,
  -- and schema 54 can preserve legacy continuity with locked digests alone.
  -- Schema 53 cannot safely reconstruct any compacted SVG or its old boundary
  -- proof, so refuse the downgrade while compacted history remains.
  IF EXISTS (
    SELECT 1 FROM agent_avatar_versions
     WHERE payload_state = 'compacted'
  ) THEN
    RAISE EXCEPTION 'cannot downgrade schema 54 while compacted avatar versions exist';
  END IF;
END $$;
-- +goose StatementEnd

ALTER TABLE agent_avatar_versions
  DROP CONSTRAINT agent_avatar_versions_continuity_fingerprint_check,
  ADD CONSTRAINT agent_avatar_versions_continuity_fingerprint_check
    CHECK (continuity_fingerprint IS NULL OR
           (payload_state = 'compacted' AND
            octet_length(continuity_fingerprint) = 38092)),
  DROP CONSTRAINT agent_avatar_versions_renderer_profile_check,
  DROP COLUMN renderer_profile;
