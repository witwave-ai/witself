-- +goose Up
-- Current-fact capacity is enforced per owner agent. Keep this projection
-- derived: account archives omit it and restore recomputes it from canonical
-- resolved, non-deleted facts. Keep this migration to the fast DDL only so the
-- ACCESS EXCLUSIVE table lock is released before the fact-table backfill in
-- migration 77.
ALTER TABLE agents
  ADD COLUMN active_fact_count BIGINT NOT NULL DEFAULT 0;

-- Add the range guard without validating the existing table under the schema
-- lock. PostgreSQL enforces a NOT VALID CHECK for every new write immediately;
-- migration 77 validates the defaulted/backfilled rows with a read-compatible
-- lock.
ALTER TABLE agents
  ADD CONSTRAINT agents_active_fact_count_range
  CHECK (active_fact_count BETWEEN 0 AND 4611686018427387903) NOT VALID;

-- +goose Down
ALTER TABLE agents
  DROP COLUMN active_fact_count;
