-- +goose Up
-- Normalize every pre-schema-79 row using the same conservative physical
-- accounting applied to old replicas during a rolling update. Existing raw
-- MIME is retained; persisted body text starts empty because the migration
-- deliberately does not interpret tenant content. Old-writer rows inserted
-- after schema 79 are already normalized to retained but remain unaccounted,
-- so the predicate promotes those rows too.
--
-- Fence supported account-first writers before taking any message-row locks,
-- then fence direct/manual writers that bypass the account row. The backfill
-- fires the schema-79 counter trigger, so running it message-first without
-- these NOWAIT locks could deadlock with ingestion or retention. Plain reads
-- remain available, and a busy migration fails promptly for a clean retry.
LOCK TABLE accounts IN EXCLUSIVE MODE NOWAIT;
LOCK TABLE agent_email_messages IN SHARE MODE NOWAIT;

UPDATE agent_email_messages
   SET payload_retention_state = 'retained',
       attachment_storage_bytes =
         CASE
           WHEN attachment_count > 0 OR parse_state = 'error'
             THEN raw_size_bytes
           ELSE 0
         END,
       retained_attachment_storage_bytes =
         CASE
           WHEN attachment_count > 0 OR parse_state = 'error'
             THEN raw_size_bytes
           ELSE 0
         END,
       attachment_storage_accounted = true
 WHERE payload_retention_state = 'legacy_pending'
    OR NOT attachment_storage_accounted;

ALTER TABLE accounts
  VALIDATE CONSTRAINT accounts_retained_agent_email_attachment_bytes_range;

ALTER TABLE agent_email_messages
  VALIDATE CONSTRAINT agent_email_messages_raw_storage_shape;
ALTER TABLE agent_email_messages
  VALIDATE CONSTRAINT agent_email_messages_body_projection_shape;
ALTER TABLE agent_email_messages
  VALIDATE CONSTRAINT agent_email_messages_attachment_storage_shape;

-- +goose Down
-- The data is already valid at schema 79 and the projection remains useful.
SELECT 1;
