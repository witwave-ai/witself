-- +goose Up
-- Phase B runs only after every schema-78 process has drained. Phase-A
-- writers already take the account row before writing an explicitly accounted
-- message, while archive import promotes its compatibility rows before commit.
-- Fence those supported account-first writers before taking the message-table
-- fence. Plain reads remain available, and a busy migration fails promptly for
-- a clean startup retry.
LOCK TABLE accounts IN EXCLUSIVE MODE NOWAIT;
LOCK TABLE agent_email_messages IN SHARE MODE NOWAIT;

-- Repair the projection for already-accounted rows before promoting any
-- compatibility rows. This makes the counter trigger's promotion delta safe
-- even if a harmless Phase-A counter drift was introduced operationally.
WITH accounted_counts AS (
  SELECT account.id AS account_id,
         COALESCE(
           sum(message.retained_attachment_storage_bytes),
           0
         )::BIGINT AS retained_bytes
    FROM accounts account
    LEFT JOIN agent_email_messages message
      ON message.account_id = account.id
     AND message.attachment_storage_accounted
   GROUP BY account.id
)
UPDATE accounts account
   SET retained_agent_email_attachment_bytes = desired.retained_bytes
  FROM accounted_counts desired
 WHERE account.id = desired.account_id
   AND account.retained_agent_email_attachment_bytes
         IS DISTINCT FROM desired.retained_bytes;

-- Schema 79 normalized every rolling old-writer row into the retained storage
-- shape without accounting it. Promoting the marker fires the counter trigger
-- in this transaction; the table fences prevent another compatibility row from
-- appearing between promotion and final verification.
UPDATE agent_email_messages
   SET attachment_storage_accounted = true
 WHERE NOT attachment_storage_accounted;

-- Rebuild every account from all retained message rows after promotion. Do not
-- retain the Phase-A accounted predicate here: zero unaccounted rows is now a
-- required invariant, and the complete message set is authoritative.
WITH desired_counts AS (
  SELECT account.id AS account_id,
         COALESCE(
           sum(message.retained_attachment_storage_bytes),
           0
         )::BIGINT AS retained_bytes
    FROM accounts account
    LEFT JOIN agent_email_messages message
      ON message.account_id = account.id
   GROUP BY account.id
)
UPDATE accounts account
   SET retained_agent_email_attachment_bytes = desired.retained_bytes
  FROM desired_counts desired
 WHERE account.id = desired.account_id
   AND account.retained_agent_email_attachment_bytes
         IS DISTINCT FROM desired.retained_bytes;

-- +goose StatementBegin
DO $$
DECLARE
  legacy_count BIGINT;
  unaccounted_count BIGINT;
  mismatched_count BIGINT;
BEGIN
  SELECT count(*) FILTER (
           WHERE payload_retention_state = 'legacy_pending'
         ),
         count(*) FILTER (
           WHERE NOT attachment_storage_accounted
         )
    INTO legacy_count, unaccounted_count
    FROM agent_email_messages;

  IF legacy_count <> 0 THEN
    RAISE EXCEPTION
      'agent-email storage finalization failed: % legacy rows remain',
      legacy_count;
  END IF;

  IF unaccounted_count <> 0 THEN
    RAISE EXCEPTION
      'agent-email storage finalization failed: % unaccounted rows remain',
      unaccounted_count;
  END IF;

  SELECT count(*)
    INTO mismatched_count
    FROM accounts account
   WHERE account.retained_agent_email_attachment_bytes IS DISTINCT FROM (
     SELECT COALESCE(
       sum(message.retained_attachment_storage_bytes),
       0
     )::BIGINT
       FROM agent_email_messages message
      WHERE message.account_id = account.id
   );

  IF mismatched_count <> 0 THEN
    RAISE EXCEPTION
      'agent-email storage finalization failed: % account counters mismatch',
      mismatched_count;
  END IF;
END
$$;
-- +goose StatementEnd

-- +goose Down
-- Finalization changes only derived data and has no meaningful inverse.
-- Exact counters and promoted rows remain valid at schema 81.
SELECT 1;
