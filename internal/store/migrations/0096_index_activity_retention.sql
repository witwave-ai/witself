-- +goose NO TRANSACTION
-- +goose Up
-- Global bounded retention cannot use the agent/account-prefixed usage indexes.
-- Retry safely after an interrupted concurrent build.
DROP INDEX CONCURRENTLY IF EXISTS usage_events_activity_retention;
CREATE INDEX CONCURRENTLY usage_events_activity_retention ON usage_events (occurred_at, id)
 WHERE dimension IN ('operation_read','operation_read_record','operation_write','operation_write_record',
 'memory_created','memory_revised','memory_archived','memory_restored','memory_deleted');

-- +goose Down
DROP INDEX CONCURRENTLY IF EXISTS usage_events_activity_retention;
