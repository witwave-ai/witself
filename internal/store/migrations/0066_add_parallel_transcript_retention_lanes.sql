-- +goose NO TRANSACTION
-- +goose Up
-- The legacy singleton remains the mixed-version compatibility fence. New
-- workers take it FOR SHARE, so they may run together while an older worker's
-- FOR UPDATE claim remains mutually exclusive with every lane batch.
CREATE TABLE IF NOT EXISTS transcript_retention_worker_lanes (
    mode            TEXT        NOT NULL,
    lane_id         SMALLINT    NOT NULL,
    account_cursor  TEXT        NOT NULL DEFAULT '',
    generation      BIGINT      NOT NULL DEFAULT 0,
    next_run_at     TIMESTAMPTZ NOT NULL DEFAULT '-infinity'::timestamptz,
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT statement_timestamp(),
    PRIMARY KEY (mode, lane_id),
    CHECK (mode IN ('preview', 'enforce')),
    CHECK (lane_id BETWEEN 0 AND 15),
    CHECK (octet_length(account_cursor) <= 512),
    CHECK (generation BETWEEN 0 AND 4611686018427387903)
);

-- Account ids are immutable. This stable digest prefix therefore assigns every
-- policy account to exactly one durable lane without rewriting the accounts
-- table. The expression must stay byte-for-byte aligned with the worker query.
-- A failed concurrent build may leave a same-named invalid index. Remove only
-- that unusable artifact on retry; never drop an already-valid live index.
-- +goose StatementBegin
DO $$
BEGIN
  IF EXISTS (
    SELECT 1
      FROM pg_index
     WHERE indexrelid = to_regclass(
             format('%I.%I', current_schema(),
                    'accounts_transcript_retention_worker_lane_idx')
           )
       AND (NOT indisvalid OR NOT indisready)
  ) THEN
    EXECUTE format(
      'DROP INDEX %I.%I',
      current_schema(),
      'accounts_transcript_retention_worker_lane_idx'
    );
  END IF;
END
$$;
-- +goose StatementEnd

CREATE INDEX CONCURRENTLY IF NOT EXISTS accounts_transcript_retention_worker_lane_idx
  ON accounts ((get_byte(decode(md5(id), 'hex'), 0) % 16), id)
  WHERE plan_policies ? 'transcript_retention_days';

-- Lock the legacy singleton, seed all lanes from that exact locked state, and
-- park its scheduled cadence in one atomic statement. Building the index first
-- is deliberate: old workers may continue normally during a long concurrent
-- build, and their final durable cursor/due time is what the locked handoff
-- copies. Once this statement commits, an older binary's positive-interval
-- scheduled batch is a clean no-op. The singleton row remains as the
-- mixed-version in-flight lock, while interval-zero direct/operator batches
-- continue to bypass cadence.
-- +goose StatementBegin
DO $$
DECLARE
  legacy_state transcript_retention_sweep_state%ROWTYPE;
  durable_lane_count INTEGER;
BEGIN
  SELECT *
    INTO legacy_state
    FROM transcript_retention_sweep_state
   WHERE singleton
   FOR UPDATE;

  IF NOT FOUND THEN
    RAISE EXCEPTION
      'cannot seed transcript retention worker lanes without singleton state';
  END IF;

  INSERT INTO transcript_retention_worker_lanes
    (mode, lane_id, account_cursor, generation, next_run_at, updated_at)
  SELECT mode.value,
         lane.value,
         CASE mode.value
           WHEN 'preview' THEN legacy_state.preview_account_cursor
           ELSE legacy_state.enforce_account_cursor
         END,
         0,
         legacy_state.next_run_at,
         statement_timestamp()
    FROM (VALUES ('preview'), ('enforce')) AS mode(value)
   CROSS JOIN generate_series(0, 15) AS lane(value)
  ON CONFLICT (mode, lane_id) DO NOTHING;

  SELECT count(*)
    INTO durable_lane_count
    FROM transcript_retention_worker_lanes;
  IF durable_lane_count <> 32 THEN
    RAISE EXCEPTION
      'transcript retention worker lane set is incomplete: got %, want 32',
      durable_lane_count;
  END IF;

  UPDATE transcript_retention_sweep_state
     SET next_run_at = 'infinity'::timestamptz,
         updated_at = statement_timestamp()
   WHERE singleton;
END
$$;
-- +goose StatementEnd

-- +goose Down
-- Refuse before removing worker-lane state when the underlying transcript
-- retention schema is already irreversible for this cell.
-- +goose StatementBegin
DO $$
BEGIN
  IF EXISTS (
    SELECT 1
      FROM memory_curation_run_inputs
     WHERE transcript_pruned_at IS NOT NULL
  ) THEN
    RAISE EXCEPTION
      'cannot remove transcript retention worker lanes while pruned curation inputs exist';
  END IF;
END
$$;
-- +goose StatementEnd

DROP INDEX CONCURRENTLY IF EXISTS accounts_transcript_retention_worker_lane_idx;

-- Restore the old scheduler conservatively from the earliest durable lane due
-- time before removing the lanes. Keeping both changes in one statement makes
-- the handoff atomic even though this migration must otherwise run outside a
-- transaction for the concurrent index operations.
-- +goose StatementBegin
DO $$
DECLARE
  earliest_lane_due TIMESTAMPTZ;
BEGIN
  IF to_regclass(
       format('%I.%I', current_schema(), 'transcript_retention_worker_lanes')
     ) IS NOT NULL THEN
    EXECUTE format(
      'SELECT min(next_run_at) FROM %I.%I',
      current_schema(),
      'transcript_retention_worker_lanes'
    )
    INTO earliest_lane_due;

    UPDATE transcript_retention_sweep_state
       SET next_run_at = COALESCE(
             earliest_lane_due,
             transcript_retention_sweep_state.next_run_at
           ),
           updated_at = statement_timestamp()
     WHERE singleton;

    EXECUTE format(
      'DROP TABLE %I.%I',
      current_schema(),
      'transcript_retention_worker_lanes'
    );
  END IF;
END
$$;
-- +goose StatementEnd
