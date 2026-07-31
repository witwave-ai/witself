-- +goose Up
-- Fence both supported account-first writers and direct/manual message-table
-- writers, then rebuild exact totals for rows already admitted by a Phase-A
-- writer or the schema-80 backfill. EXCLUSIVE conflicts with the ROW SHARE
-- lock taken by
-- SELECT ... FOR NO KEY UPDATE in supported ingestion without blocking plain
-- reads. The second lock catches writers that bypass the account row. NOWAIT
-- makes a busy startup fail and retry cleanly instead of creating an
-- account-row/message-table lock inversion.
LOCK TABLE accounts IN EXCLUSIVE MODE NOWAIT;
LOCK TABLE agent_email_messages IN SHARE MODE NOWAIT;

WITH desired_counts AS (
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
  FROM desired_counts desired
 WHERE account.id = desired.account_id
   AND account.retained_agent_email_attachment_bytes
         IS DISTINCT FROM desired.retained_bytes;

-- +goose StatementBegin
DO $$
DECLARE
  mismatched_count BIGINT;
  pending_count BIGINT;
BEGIN
  SELECT count(*)
    INTO pending_count
    FROM agent_email_messages
   WHERE payload_retention_state = 'legacy_pending';

  IF pending_count <> 0 THEN
    RAISE EXCEPTION
      'agent-email storage reconciliation failed: % legacy rows remain',
      pending_count;
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
        AND message.attachment_storage_accounted
   );

  IF mismatched_count <> 0 THEN
    RAISE EXCEPTION
      'agent-email storage reconciliation failed: % account counters mismatch',
      mismatched_count;
  END IF;
END
$$;
-- +goose StatementEnd

-- +goose Down
-- This reconciliation changes derived data and has no meaningful inverse.
SELECT 1;
