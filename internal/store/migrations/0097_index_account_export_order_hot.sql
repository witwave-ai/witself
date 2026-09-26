-- +goose NO TRANSACTION
-- +goose Up
-- Account archives keep their established row ordering. These account-prefixed
-- indexes avoid sorting wide JSON payloads before the first chunk (#555).
-- Keep the two hot-table builds in their own migration so a later migration
-- retry does not rebuild these completed indexes.
-- Rebuild on retry so an interrupted concurrent build cannot leave an invalid index.
DROP INDEX CONCURRENTLY IF EXISTS transcript_entries_export_order;
CREATE INDEX CONCURRENTLY transcript_entries_export_order ON transcript_entries (account_id, transcript_id, sequence, id);

DROP INDEX CONCURRENTLY IF EXISTS usage_events_export_order;
CREATE INDEX CONCURRENTLY usage_events_export_order ON usage_events (account_id, occurred_at, id);

-- +goose Down
DROP INDEX CONCURRENTLY IF EXISTS usage_events_export_order;
DROP INDEX CONCURRENTLY IF EXISTS transcript_entries_export_order;
