-- +goose Up
-- A reply is causally attached to one earlier message in the same account and
-- realm. The service additionally requires the replying principal to be the
-- parent's recipient and derives the reply recipient/thread from that parent.
ALTER TABLE agent_messages
  ADD COLUMN reply_to_message_id TEXT;

ALTER TABLE agent_messages
  ADD CONSTRAINT agent_messages_reply_parent_fk
  FOREIGN KEY (reply_to_message_id, account_id, realm_id)
  REFERENCES agent_messages (id, account_id, realm_id)
  DEFERRABLE INITIALLY DEFERRED;

ALTER TABLE agent_messages
  ADD CONSTRAINT agent_messages_reply_not_self
  CHECK (reply_to_message_id IS NULL OR reply_to_message_id <> id);

CREATE INDEX agent_messages_by_reply_parent
    ON agent_messages (account_id, realm_id, reply_to_message_id)
    WHERE reply_to_message_id IS NOT NULL;

-- Listen repeatedly asks for one agent's unacknowledged deliveries. Keep
-- acknowledged history out of that hot path while retaining message_id for
-- the join to immutable message metadata.
CREATE INDEX agent_message_deliveries_unacked_recipient
    ON agent_message_deliveries
       (account_id, realm_id, recipient_agent_id, message_id)
    WHERE acked_at IS NULL;

-- +goose Down
DROP INDEX agent_message_deliveries_unacked_recipient;
DROP INDEX agent_messages_by_reply_parent;
ALTER TABLE agent_messages DROP CONSTRAINT agent_messages_reply_not_self;
ALTER TABLE agent_messages DROP CONSTRAINT agent_messages_reply_parent_fk;
ALTER TABLE agent_messages DROP COLUMN reply_to_message_id;
