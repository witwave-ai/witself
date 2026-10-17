-- +goose NO TRANSACTION
-- +goose Up
-- Support referential-integrity checks and cascades on row deletion without
-- repeatedly scanning transcript history or curation bookkeeping.
-- Keep the three hot-table builds in their own migration so a later migration
-- retry does not rebuild these completed indexes.
-- Rebuild on retry so an interrupted concurrent build cannot leave an invalid index.
DROP INDEX CONCURRENTLY IF EXISTS transcript_entries_by_reply_to;
CREATE INDEX CONCURRENTLY transcript_entries_by_reply_to ON transcript_entries (reply_to_entry_id) WHERE reply_to_entry_id IS NOT NULL;

DROP INDEX CONCURRENTLY IF EXISTS memory_curation_mutations_by_run_request;
CREATE INDEX CONCURRENTLY memory_curation_mutations_by_run_request ON memory_curation_mutations (run_id, request_id);

DROP INDEX CONCURRENTLY IF EXISTS memory_curation_mutations_by_request_scope;
CREATE INDEX CONCURRENTLY memory_curation_mutations_by_request_scope ON memory_curation_mutations (request_id, account_id, realm_id, owner_kind, owner_id);

-- +goose Down
DROP INDEX CONCURRENTLY IF EXISTS memory_curation_mutations_by_request_scope;
DROP INDEX CONCURRENTLY IF EXISTS memory_curation_mutations_by_run_request;
DROP INDEX CONCURRENTLY IF EXISTS transcript_entries_by_reply_to;
