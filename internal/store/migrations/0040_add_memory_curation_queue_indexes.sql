-- +goose Up
-- Owner-scoped foreground curation selects by priority first, then due time.
-- Prefix the partial index with the complete tenant/owner boundary so that a
-- realm with many agents does not scan or sort unrelated due work.
CREATE INDEX memory_curation_requests_due_by_owner
    ON memory_curation_requests
       (account_id, realm_id, owner_kind, owner_id,
        priority DESC, due_at, created_at, id)
    WHERE state IN ('queued', 'retry_wait');

-- +goose Down
DROP INDEX memory_curation_requests_due_by_owner;
