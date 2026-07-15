-- +goose Up
-- The backend owns conversation depth. Direct sends always begin at one;
-- replies advance exactly one from their durable parent. A high explicit
-- bound prevents integer exhaustion while leaving client policy free to use
-- much smaller configurable turn limits.
ALTER TABLE agent_messages
  ADD COLUMN causal_depth BIGINT NOT NULL DEFAULT 1;

WITH RECURSIVE message_depth AS (
  SELECT id, account_id, realm_id, 1::BIGINT AS depth
  FROM agent_messages
  WHERE reply_to_message_id IS NULL

  UNION ALL

  SELECT child.id, child.account_id, child.realm_id, parent.depth + 1
  FROM agent_messages child
  JOIN message_depth parent
    ON parent.id = child.reply_to_message_id
   AND parent.account_id = child.account_id
   AND parent.realm_id = child.realm_id
  WHERE parent.depth < 2147483647
)
UPDATE agent_messages message
SET causal_depth = depth.depth
FROM message_depth depth
WHERE depth.id = message.id
  AND depth.account_id = message.account_id
  AND depth.realm_id = message.realm_id;

-- +goose StatementBegin
DO $$
BEGIN
  IF EXISTS (
    SELECT 1
    FROM agent_messages
    WHERE reply_to_message_id IS NOT NULL AND causal_depth = 1
  ) THEN
    RAISE EXCEPTION 'agent message reply graph cannot be assigned a causal depth';
  END IF;
END
$$;
-- +goose StatementEnd

ALTER TABLE agent_messages
  ADD CONSTRAINT agent_messages_causal_depth_range
  CHECK (causal_depth BETWEEN 1 AND 2147483647);

-- Oldest-unacknowledged listen starts from one recipient and needs only a
-- tiny ordered prefix. Keep that path indexable without sorting a realm's
-- entire message history.
CREATE INDEX agent_messages_by_recipient_activity
  ON agent_messages (account_id, realm_id, to_agent_id, created_at, id);

-- +goose Down
DROP INDEX agent_messages_by_recipient_activity;
ALTER TABLE agent_messages DROP CONSTRAINT agent_messages_causal_depth_range;
ALTER TABLE agent_messages DROP COLUMN causal_depth;
