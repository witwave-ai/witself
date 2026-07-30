-- +goose Up
-- Every supported Phase-A active-memory mutation touches this table before it
-- changes canonical memory heads. EXCLUSIVE blocks those DML and FOR UPDATE
-- paths while ordinary status reads continue. NOWAIT makes a busy startup
-- fail and retry cleanly instead of queueing a migration lock ahead of writes.
LOCK TABLE memory_change_clocks IN EXCLUSIVE MODE NOWAIT;

-- Repair a defensive legacy/manual missing clock before rebuilding counts.
-- Existing clocks retain their monotonic sequence position.
WITH owner_positions AS (
  SELECT account_id, realm_id, owner_kind, owner_id,
         max(position) AS last_change_seq
    FROM (
      SELECT account_id, realm_id, owner_kind, owner_id,
             change_seq AS position
        FROM memory_versions
      UNION ALL
      SELECT account_id, realm_id, owner_kind, owner_id,
             evidence_change_seq AS position
        FROM memory_evidence
    ) positions
   GROUP BY account_id, realm_id, owner_kind, owner_id
)
INSERT INTO memory_change_clocks
  (account_id, realm_id, owner_kind, owner_id, last_change_seq)
SELECT account_id, realm_id, owner_kind, owner_id, last_change_seq
  FROM owner_positions
ON CONFLICT (account_id, realm_id, owner_kind, owner_id) DO NOTHING;

WITH active_counts AS (
  SELECT m.account_id, m.realm_id, m.owner_kind, m.owner_id,
         count(*)::bigint AS active_memory_count
    FROM memories m
    JOIN memory_versions v
      ON v.memory_id = m.id
     AND v.version = m.current_version
   WHERE m.current_version IS NOT NULL
     AND v.state = 'active'
   GROUP BY m.account_id, m.realm_id, m.owner_kind, m.owner_id
),
desired_counts AS (
  SELECT clock.account_id, clock.realm_id, clock.owner_kind, clock.owner_id,
         COALESCE(active.active_memory_count, 0)::bigint
           AS active_memory_count
    FROM memory_change_clocks clock
    LEFT JOIN active_counts active
      ON active.account_id = clock.account_id
     AND active.realm_id = clock.realm_id
     AND active.owner_kind = clock.owner_kind
     AND active.owner_id = clock.owner_id
)
UPDATE memory_change_clocks clock
   SET active_memory_count = desired.active_memory_count
  FROM desired_counts desired
 WHERE clock.account_id = desired.account_id
   AND clock.realm_id = desired.realm_id
   AND clock.owner_kind = desired.owner_kind
   AND clock.owner_id = desired.owner_id
   AND clock.active_memory_count IS DISTINCT FROM
       desired.active_memory_count;

-- +goose StatementBegin
DO $$
DECLARE
  missing_clock_count bigint;
  mismatched_count bigint;
BEGIN
  WITH active_counts AS (
    SELECT m.account_id, m.realm_id, m.owner_kind, m.owner_id,
           count(*)::bigint AS active_memory_count
      FROM memories m
      JOIN memory_versions v
        ON v.memory_id = m.id
       AND v.version = m.current_version
     WHERE m.current_version IS NOT NULL
       AND v.state = 'active'
     GROUP BY m.account_id, m.realm_id, m.owner_kind, m.owner_id
  )
  SELECT
    count(*) FILTER (WHERE clock.account_id IS NULL),
    count(*) FILTER (
      WHERE clock.account_id IS NOT NULL
        AND clock.active_memory_count IS DISTINCT FROM
            COALESCE(active.active_memory_count, 0)
    )
    INTO missing_clock_count, mismatched_count
    FROM memory_change_clocks clock
    FULL OUTER JOIN active_counts active
      ON active.account_id = clock.account_id
     AND active.realm_id = clock.realm_id
     AND active.owner_kind = clock.owner_kind
     AND active.owner_id = clock.owner_id;

  IF missing_clock_count <> 0 OR mismatched_count <> 0 THEN
    RAISE EXCEPTION
      'active memory reconciliation failed: % active owners lack clocks and % clocks mismatch',
      missing_clock_count, mismatched_count;
  END IF;
END
$$;
-- +goose StatementEnd

-- +goose Down
-- This reconciliation changes derived data and has no meaningful inverse.
-- Retaining exact counts is safe at schema 74; rolling down through migration
-- 74 still removes the column.
SELECT 1;
