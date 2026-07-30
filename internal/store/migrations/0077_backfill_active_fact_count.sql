-- +goose Up
-- Run the initial derived-count scan after migration 76 has committed its
-- short ACCESS EXCLUSIVE schema change. This ordinary UPDATE does not block
-- agent/auth reads while a mature fact table is scanned. Phase A remains
-- unlimited, so mixed-version writes may create harmless temporary drift that
-- the separately released fenced reconciliation repairs before finite limits
-- activate.
WITH active_counts AS (
  SELECT owner_agent_id, count(*)::BIGINT AS active_fact_count
    FROM facts
   WHERE deleted_at IS NULL
     AND resolved_assertion_id IS NOT NULL
   GROUP BY owner_agent_id
)
UPDATE agents agent
   SET active_fact_count = active.active_fact_count
  FROM active_counts active
 WHERE agent.id = active.owner_agent_id;

ALTER TABLE agents
  VALIDATE CONSTRAINT agents_active_fact_count_range;

-- +goose Down
-- This backfill changes only derived data. Rolling down through migration 76
-- removes the projection column.
SELECT 1;
