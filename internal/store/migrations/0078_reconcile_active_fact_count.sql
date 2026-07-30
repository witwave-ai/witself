-- +goose Up
-- Every Phase-A current-fact mutation locks its owner row in agents before it
-- changes canonical facts. EXCLUSIVE blocks those DML and FOR UPDATE paths
-- while ordinary status/auth reads continue. NOWAIT makes a busy startup fail
-- and retry cleanly instead of queueing a migration lock ahead of live writes.
-- A direct/manual or legacy fact-table writer may not touch agents first.
-- SHARE blocks fact INSERT/UPDATE/DELETE while preserving ordinary fact reads.
LOCK TABLE agents IN EXCLUSIVE MODE NOWAIT;
LOCK TABLE facts IN SHARE MODE NOWAIT;

-- Rebuild every owner projection, including explicit zeroes. Migration 77's
-- initial backfill intentionally tolerated mixed-version drift while all
-- effective stored_fact limits remained unlimited.
WITH desired_counts AS (
  SELECT owner.id AS owner_agent_id,
         count(fact.id)::BIGINT AS active_fact_count
    FROM agents owner
    LEFT JOIN facts fact
      ON fact.owner_agent_id = owner.id
     AND fact.deleted_at IS NULL
     AND fact.resolved_assertion_id IS NOT NULL
   GROUP BY owner.id
)
UPDATE agents owner
   SET active_fact_count = desired.active_fact_count
  FROM desired_counts desired
 WHERE owner.id = desired.owner_agent_id
   AND owner.active_fact_count IS DISTINCT FROM desired.active_fact_count;

-- +goose StatementBegin
DO $$
DECLARE
  mismatched_count BIGINT;
BEGIN
  SELECT count(*)
    INTO mismatched_count
    FROM agents owner
   WHERE owner.active_fact_count IS DISTINCT FROM (
     SELECT count(*)::BIGINT
       FROM facts fact
      WHERE fact.owner_agent_id = owner.id
        AND fact.deleted_at IS NULL
        AND fact.resolved_assertion_id IS NOT NULL
   );

  IF mismatched_count <> 0 THEN
    RAISE EXCEPTION
      'active fact reconciliation failed: % owner counters mismatch',
      mismatched_count;
  END IF;
END
$$;
-- +goose StatementEnd

-- +goose Down
-- This reconciliation changes derived data and has no meaningful inverse.
-- Retaining exact counts is safe at schema 77; rolling down through migration
-- 76 still removes the column.
SELECT 1;
