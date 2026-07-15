-- +goose Up
-- One immutable message may target either one agent, a bounded explicit set,
-- or the token-derived realm.  The delivery rows are the authoritative
-- send-time recipient snapshot for non-direct audiences.
ALTER TABLE agent_messages
  ALTER COLUMN to_agent_id DROP NOT NULL,
  ADD COLUMN audience_kind TEXT NOT NULL DEFAULT 'agent',
  ADD COLUMN audience_fingerprint TEXT NOT NULL DEFAULT '';

ALTER TABLE agent_messages
  ADD CONSTRAINT agent_messages_audience_shape
  CHECK (
    (audience_kind = 'agent' AND
     to_agent_id IS NOT NULL AND audience_fingerprint = '')
    OR
    (audience_kind IN ('agents', 'realm') AND
     to_agent_id IS NULL AND audience_fingerprint ~ '^[0-9a-f]{64}$')
  );

-- Recipient mailbox queries are delivery-driven for every audience.  This
-- index supplies message activity ordering after the recipient delivery hot
-- index has selected the bounded mailbox prefix.
CREATE INDEX agent_messages_by_realm_activity
  ON agent_messages (account_id, realm_id, created_at, id);

-- +goose Down
DROP INDEX agent_messages_by_realm_activity;
ALTER TABLE agent_messages
  DROP CONSTRAINT agent_messages_audience_shape,
  DROP COLUMN audience_fingerprint,
  DROP COLUMN audience_kind,
  ALTER COLUMN to_agent_id SET NOT NULL;
