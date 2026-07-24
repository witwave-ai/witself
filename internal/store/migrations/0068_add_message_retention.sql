-- +goose NO TRANSACTION
-- +goose Up
-- Message retention deletes whole inactive threads. Keep one value-free,
-- rebuildable activity row per thread so every bounded worker batch can use an
-- age-first index instead of grouping the full message table.
CREATE TABLE IF NOT EXISTS message_retention_thread_activity (
    account_id       TEXT        NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    realm_id         TEXT        NOT NULL,
    thread_id        TEXT        NOT NULL,
    last_message_at  TIMESTAMPTZ NOT NULL,
    retry_after      TIMESTAMPTZ NOT NULL DEFAULT '-infinity'::timestamptz,
    defer_reason     TEXT        NOT NULL DEFAULT '',
    defer_count      BIGINT      NOT NULL DEFAULT 0,
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT statement_timestamp(),
    PRIMARY KEY (account_id, realm_id, thread_id),
    CHECK (thread_id LIKE 'thr\_%' ESCAPE '\' AND octet_length(thread_id) <= 128),
    CHECK (defer_reason IN ('', 'oversize')),
    CHECK (defer_count BETWEEN 0 AND 4611686018427387903)
);

-- Held or busy old threads must not starve later eligible threads. This
-- per-account keyset state advances across a fixed age cycle even when the
-- current page is entirely deferred, then wraps for the next cycle.
CREATE TABLE IF NOT EXISTS message_retention_account_scan_state (
    mode              TEXT        NOT NULL,
    account_id        TEXT        NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    retention_days    INTEGER     NOT NULL,
    cycle_cutoff      TIMESTAMPTZ NOT NULL,
    last_activity_at  TIMESTAMPTZ,
    last_realm_id     TEXT,
    last_thread_id    TEXT,
    generation        BIGINT      NOT NULL DEFAULT 0,
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT statement_timestamp(),
    PRIMARY KEY (mode, account_id),
    CHECK (mode IN ('preview', 'enforce')),
    CHECK (retention_days BETWEEN 1 AND 36500),
    CHECK (
      (last_activity_at IS NULL AND last_realm_id IS NULL AND last_thread_id IS NULL)
      OR
      (last_activity_at IS NOT NULL AND last_realm_id IS NOT NULL AND last_thread_id IS NOT NULL)
    ),
    CHECK (last_realm_id IS NULL OR octet_length(last_realm_id) <= 512),
    CHECK (last_thread_id IS NULL OR octet_length(last_thread_id) <= 128),
    CHECK (generation BETWEEN 0 AND 4611686018427387903)
);

-- Run before the message insert itself. The upsert's row lock is the
-- synchronization fence between new thread activity and whole-thread
-- retention: a sender waits before materializing content while a retention
-- batch owns the activity row, and retention waits for an already-started
-- sender before deciding that the thread is inactive.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION witself_track_message_thread_activity()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
  INSERT INTO message_retention_thread_activity
    (account_id, realm_id, thread_id, last_message_at, updated_at)
  VALUES
    (NEW.account_id, NEW.realm_id, NEW.thread_id, NEW.created_at,
     statement_timestamp())
  ON CONFLICT (account_id, realm_id, thread_id) DO UPDATE
    SET last_message_at = GREATEST(
          message_retention_thread_activity.last_message_at,
          EXCLUDED.last_message_at
        ),
        retry_after = '-infinity'::timestamptz,
        defer_reason = '',
        defer_count = 0,
        updated_at = statement_timestamp();
  RETURN NEW;
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
DO $$
BEGIN
  IF NOT EXISTS (
    SELECT 1
      FROM pg_trigger
     WHERE tgrelid = 'agent_messages'::regclass
       AND tgname = 'agent_messages_track_retention_activity'
       AND NOT tgisinternal
  ) THEN
    CREATE TRIGGER agent_messages_track_retention_activity
    BEFORE INSERT ON agent_messages
    FOR EACH ROW
    EXECUTE FUNCTION witself_track_message_thread_activity();
  END IF;
END
$$;
-- +goose StatementEnd

-- The trigger is live before backfill, so concurrent inserts cannot be lost.
-- GREATEST also makes this statement idempotent after an interrupted migration.
INSERT INTO message_retention_thread_activity
  (account_id, realm_id, thread_id, last_message_at, updated_at)
SELECT account_id, realm_id, thread_id, max(created_at), statement_timestamp()
  FROM agent_messages
 GROUP BY account_id, realm_id, thread_id
ON CONFLICT (account_id, realm_id, thread_id) DO UPDATE
  SET last_message_at = GREATEST(
        message_retention_thread_activity.last_message_at,
        EXCLUDED.last_message_at
      ),
      updated_at = statement_timestamp();

-- A fixed, schema-owned lane assignment lets any number of worker replicas
-- take different due lanes with SKIP LOCKED. Lane count is not a pod setting.
CREATE TABLE IF NOT EXISTS message_retention_worker_lanes (
    mode         TEXT        NOT NULL,
    lane_id      SMALLINT    NOT NULL,
    account_cursor TEXT      NOT NULL DEFAULT '',
    generation   BIGINT      NOT NULL DEFAULT 0,
    next_run_at  TIMESTAMPTZ NOT NULL DEFAULT '-infinity'::timestamptz,
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT statement_timestamp(),
    PRIMARY KEY (mode, lane_id),
    CHECK (mode IN ('preview', 'enforce')),
    CHECK (lane_id BETWEEN 0 AND 15),
    CHECK (octet_length(account_cursor) <= 512),
    CHECK (generation BETWEEN 0 AND 4611686018427387903)
);

INSERT INTO message_retention_worker_lanes
  (mode, lane_id, account_cursor, generation, next_run_at, updated_at)
SELECT mode.value, lane.value, '', 0, '-infinity'::timestamptz,
       statement_timestamp()
  FROM (VALUES ('preview'), ('enforce')) AS mode(value)
 CROSS JOIN generate_series(0, 15) AS lane(value)
ON CONFLICT (mode, lane_id) DO NOTHING;

-- Remove only unusable artifacts left by a failed concurrent build.
-- +goose StatementBegin
DO $$
BEGIN
  IF EXISTS (
    SELECT 1
      FROM pg_index
     WHERE indexrelid = to_regclass(
             format('%I.%I', current_schema(),
                    'message_retention_activity_account_age_idx')
           )
       AND (NOT indisvalid OR NOT indisready)
  ) THEN
    EXECUTE format(
      'DROP INDEX %I.%I',
      current_schema(),
      'message_retention_activity_account_age_idx'
    );
  END IF;
END
$$;
-- +goose StatementEnd

CREATE INDEX CONCURRENTLY IF NOT EXISTS message_retention_activity_account_age_idx
  ON message_retention_thread_activity
     (account_id, last_message_at, realm_id, thread_id);

-- Only finite-policy accounts enter the lane walk. Indefinite accounts may
-- retain a rebuildable activity projection for a future downgrade, but they
-- never inflate a finite-retention candidate scan.
-- +goose StatementBegin
DO $$
BEGIN
  IF EXISTS (
    SELECT 1
      FROM pg_index
     WHERE indexrelid = to_regclass(
             format('%I.%I', current_schema(),
                    'accounts_message_retention_worker_lane_idx')
           )
       AND (NOT indisvalid OR NOT indisready)
  ) THEN
    EXECUTE format(
      'DROP INDEX %I.%I',
      current_schema(),
      'accounts_message_retention_worker_lane_idx'
    );
  END IF;
END
$$;
-- +goose StatementEnd

CREATE INDEX CONCURRENTLY IF NOT EXISTS accounts_message_retention_worker_lane_idx
  ON accounts ((get_byte(decode(md5(id), 'hex'), 0) % 16), id)
  WHERE plan_policies ? 'message_retention_days';

-- Memory evidence is a provenance hold. PostgreSQL does not automatically
-- index the referencing side of its message foreign key.
-- +goose StatementBegin
DO $$
BEGIN
  IF EXISTS (
    SELECT 1
      FROM pg_index
     WHERE indexrelid = to_regclass(
             format('%I.%I', current_schema(),
                    'memory_evidence_by_source_message')
           )
       AND (NOT indisvalid OR NOT indisready)
  ) THEN
    EXECUTE format(
      'DROP INDEX %I.%I',
      current_schema(),
      'memory_evidence_by_source_message'
    );
  END IF;
END
$$;
-- +goose StatementEnd

CREATE INDEX CONCURRENTLY IF NOT EXISTS memory_evidence_by_source_message
  ON memory_evidence (source_message_id)
  WHERE source_message_id IS NOT NULL;

-- +goose Down
-- Fail closed before removing the activity trigger. This ACCESS EXCLUSIVE
-- drop waits for every in-flight retention batch, and any batch started after
-- it commits cannot acquire a durable lane. An interrupted downgrade can
-- therefore leave retention unavailable, but can never leave it running from
-- a stale activity projection.
DROP TABLE IF EXISTS message_retention_worker_lanes;

DROP TRIGGER IF EXISTS agent_messages_track_retention_activity
  ON agent_messages;
DROP FUNCTION IF EXISTS witself_track_message_thread_activity();

DROP INDEX CONCURRENTLY IF EXISTS memory_evidence_by_source_message;
DROP INDEX CONCURRENTLY IF EXISTS accounts_message_retention_worker_lane_idx;
DROP INDEX CONCURRENTLY IF EXISTS message_retention_activity_account_age_idx;

DROP TABLE IF EXISTS message_retention_account_scan_state;
DROP TABLE IF EXISTS message_retention_thread_activity;
