-- +goose Up
-- Active narrative-memory capacity is an account-plan concern, but the exact
-- usage belongs to the agent owner lane that already serializes memory
-- mutations across replicas. Keep this value derived: archives omit it and
-- restore recomputes it from canonical heads.
ALTER TABLE memory_change_clocks
  ADD COLUMN active_memory_count BIGINT NOT NULL DEFAULT 0
    CHECK (active_memory_count BETWEEN 0 AND 4611686018427387903);

-- Old well-formed data already has one clock per memory owner. Repair a
-- missing owner clock defensively before the backfill so a legacy/manual row
-- cannot make capacity reads silently report zero. The highest version or
-- evidence sequence preserves the same monotonic lane position.
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
         count(*)::BIGINT AS active_memory_count
    FROM memories m
    JOIN memory_versions v
      ON v.memory_id = m.id
     AND v.version = m.current_version
   WHERE m.current_version IS NOT NULL
     AND v.state = 'active'
   GROUP BY m.account_id, m.realm_id, m.owner_kind, m.owner_id
)
UPDATE memory_change_clocks clock
   SET active_memory_count = active.active_memory_count
  FROM active_counts active
 WHERE clock.account_id = active.account_id
   AND clock.realm_id = active.realm_id
   AND clock.owner_kind = active.owner_kind
   AND clock.owner_id = active.owner_id;

-- +goose Down
ALTER TABLE memory_change_clocks
  DROP COLUMN active_memory_count;
