-- +goose NO TRANSACTION
-- +goose Up
-- Inbound agent-email retention is account-policy driven. Missing
-- agent_email_retention_days means indefinite retention. Preview and enforce
-- keep independent account cursors so an observation rollout never advances
-- destructive cleanup state.
CREATE TABLE IF NOT EXISTS agent_email_retention_account_scan_state (
    mode             TEXT        NOT NULL,
    account_id       TEXT        NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    retention_days   INTEGER     NOT NULL,
    cycle_cutoff     TIMESTAMPTZ NOT NULL,
    last_received_at TIMESTAMPTZ,
    last_message_id  TEXT,
    generation       BIGINT      NOT NULL DEFAULT 0,
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT statement_timestamp(),
    PRIMARY KEY (mode, account_id),
    CHECK (mode IN ('preview', 'enforce')),
    CHECK (retention_days BETWEEN 1 AND 36500),
    CHECK (
      (last_received_at IS NULL AND last_message_id IS NULL)
      OR
      (last_received_at IS NOT NULL AND last_message_id IS NOT NULL)
    ),
    CHECK (last_message_id IS NULL OR
           (last_message_id LIKE 'emsg\_%' ESCAPE '\' AND
            octet_length(last_message_id) <= 128)),
    CHECK (generation BETWEEN 0 AND 4611686018427387903)
);

-- A fixed schema-owned lane set lets any number of worker replicas cooperate
-- with FOR UPDATE SKIP LOCKED. Lane cardinality is deliberately not a pod
-- setting, and preview/enforce never share a cadence or cursor.
CREATE TABLE IF NOT EXISTS agent_email_retention_worker_lanes (
    mode           TEXT        NOT NULL,
    lane_id        SMALLINT    NOT NULL,
    account_cursor TEXT        NOT NULL DEFAULT '',
    generation     BIGINT      NOT NULL DEFAULT 0,
    next_run_at    TIMESTAMPTZ NOT NULL DEFAULT '-infinity'::timestamptz,
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT statement_timestamp(),
    PRIMARY KEY (mode, lane_id),
    CHECK (mode IN ('preview', 'enforce')),
    CHECK (lane_id BETWEEN 0 AND 15),
    CHECK (octet_length(account_cursor) <= 512),
    CHECK (generation BETWEEN 0 AND 4611686018427387903)
);

INSERT INTO agent_email_retention_worker_lanes
  (mode, lane_id, account_cursor, generation, next_run_at, updated_at)
SELECT mode.value, lane.value, '', 0, '-infinity'::timestamptz,
       statement_timestamp()
  FROM (VALUES ('preview'), ('enforce')) AS mode(value)
 CROSS JOIN generate_series(0, 15) AS lane(value)
ON CONFLICT (mode, lane_id) DO NOTHING;

-- Remove only unusable artifacts left by an interrupted concurrent build.
-- +goose StatementBegin
DO $$
DECLARE
  index_name TEXT;
BEGIN
  FOREACH index_name IN ARRAY ARRAY[
    'accounts_agent_email_retention_worker_lane_idx',
    'agent_email_messages_account_received_idx',
    'agent_email_messages_possible_duplicate_idx',
    'agent_email_retry_canary_accepted_message_idx'
  ]
  LOOP
    IF EXISTS (
      SELECT 1
        FROM pg_index
       WHERE indexrelid = to_regclass(
               format('%I.%I', current_schema(), index_name)
             )
         AND (NOT indisvalid OR NOT indisready)
    ) THEN
      EXECUTE format('DROP INDEX %I.%I', current_schema(), index_name);
    END IF;
  END LOOP;
END
$$;
-- +goose StatementEnd

-- Only finite-policy accounts participate in the lane walk.
CREATE INDEX CONCURRENTLY IF NOT EXISTS
  accounts_agent_email_retention_worker_lane_idx
  ON accounts ((get_byte(decode(md5(id), 'hex'), 0) % 16), id)
  WHERE plan_policies ? 'agent_email_retention_days';

-- Age-first keyset pagination uses the database-recorded receive timestamp.
CREATE INDEX CONCURRENTLY IF NOT EXISTS
  agent_email_messages_account_received_idx
  ON agent_email_messages (account_id, received_at, id);

-- PostgreSQL does not automatically index the referencing side of the
-- suspected-duplicate self-reference. Retention clears these links before
-- deleting their target.
CREATE INDEX CONCURRENTLY IF NOT EXISTS
  agent_email_messages_possible_duplicate_idx
  ON agent_email_messages (account_id, possible_duplicate_of_message_id, id)
  WHERE possible_duplicate_of_message_id IS NOT NULL;

-- Accepted synthetic retry proofs cascade with their retained email. This
-- index makes the worker's explicit proof lock and cascade count bounded by
-- the selected message page.
CREATE INDEX CONCURRENTLY IF NOT EXISTS
  agent_email_retry_canary_accepted_message_idx
  ON agent_email_retry_canary_arms (account_id, accepted_message_id)
  WHERE accepted_message_id IS NOT NULL;

-- +goose Down
-- Removing the lane table first is the fail-closed fence. ACCESS EXCLUSIVE
-- waits for an in-flight batch; afterward no new worker can claim work.
DROP TABLE IF EXISTS agent_email_retention_worker_lanes;

DROP INDEX CONCURRENTLY IF EXISTS
  agent_email_retry_canary_accepted_message_idx;
DROP INDEX CONCURRENTLY IF EXISTS
  agent_email_messages_possible_duplicate_idx;
DROP INDEX CONCURRENTLY IF EXISTS
  agent_email_messages_account_received_idx;
DROP INDEX CONCURRENTLY IF EXISTS
  accounts_agent_email_retention_worker_lane_idx;

DROP TABLE IF EXISTS agent_email_retention_account_scan_state;
