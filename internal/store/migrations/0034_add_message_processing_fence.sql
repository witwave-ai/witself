-- +goose Up
-- A delivery may be leased by exactly one recipient-side runner. Generation is
-- a monotonically increasing fencing token: an expired claim can be replaced,
-- but the old runner can never renew, release, or complete the new generation.
ALTER TABLE agent_message_deliveries
  ADD COLUMN processing_state TEXT NOT NULL DEFAULT 'available',
  ADD COLUMN processing_generation BIGINT NOT NULL DEFAULT 0,
  ADD COLUMN claim_id TEXT,
  ADD COLUMN claim_key_hash TEXT NOT NULL DEFAULT '',
  ADD COLUMN lease_expires_at TIMESTAMPTZ,
  ADD COLUMN completed_at TIMESTAMPTZ,
  ADD COLUMN complete_key_hash TEXT NOT NULL DEFAULT '',
  ADD COLUMN result_message_id TEXT;

ALTER TABLE agent_message_deliveries
  ADD CONSTRAINT agent_message_deliveries_processing_generation
  CHECK (processing_generation BETWEEN 0 AND 4611686018427387903),
  ADD CONSTRAINT agent_message_deliveries_processing_shape
  CHECK (
    (processing_state = 'available' AND
     claim_id IS NULL AND claim_key_hash = '' AND lease_expires_at IS NULL AND
     completed_at IS NULL AND complete_key_hash = '' AND result_message_id IS NULL)
    OR
    (processing_state = 'claimed' AND processing_generation >= 1 AND
     claim_id ~ '^mcl_[A-Za-z0-9_-]+$' AND
     claim_key_hash ~ '^[0-9a-f]{64}$' AND lease_expires_at IS NOT NULL AND
     completed_at IS NULL AND complete_key_hash = '' AND result_message_id IS NULL)
    OR
    (processing_state = 'completed' AND processing_generation >= 1 AND
     claim_id ~ '^mcl_[A-Za-z0-9_-]+$' AND
     claim_key_hash ~ '^[0-9a-f]{64}$' AND lease_expires_at IS NULL AND
     completed_at IS NOT NULL AND complete_key_hash ~ '^[0-9a-f]{64}$' AND
     result_message_id IS NOT NULL)
  );

-- The result must be an account/realm-local message. It is inserted in the
-- same transaction as completion, so the deferred scope check also supports
-- full-account archive restoration without weakening ownership constraints.
ALTER TABLE agent_message_deliveries
  ADD CONSTRAINT agent_message_deliveries_result_message_fk
  FOREIGN KEY (result_message_id, account_id, realm_id)
  REFERENCES agent_messages (id, account_id, realm_id)
  DEFERRABLE INITIALLY DEFERRED,
  ADD CONSTRAINT agent_message_deliveries_result_message_unique
  UNIQUE (result_message_id, account_id, realm_id)
  DEFERRABLE INITIALLY DEFERRED;

CREATE INDEX agent_message_deliveries_claimable_recipient
    ON agent_message_deliveries
       (account_id, realm_id, recipient_agent_id, processing_state,
        lease_expires_at, message_id)
    WHERE acked_at IS NULL;

-- +goose Down
DROP INDEX agent_message_deliveries_claimable_recipient;
ALTER TABLE agent_message_deliveries
  DROP CONSTRAINT agent_message_deliveries_result_message_unique,
  DROP CONSTRAINT agent_message_deliveries_result_message_fk,
  DROP CONSTRAINT agent_message_deliveries_processing_shape,
  DROP CONSTRAINT agent_message_deliveries_processing_generation,
  DROP COLUMN result_message_id,
  DROP COLUMN complete_key_hash,
  DROP COLUMN completed_at,
  DROP COLUMN lease_expires_at,
  DROP COLUMN claim_key_hash,
  DROP COLUMN claim_id,
  DROP COLUMN processing_generation,
  DROP COLUMN processing_state;
