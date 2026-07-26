-- +goose NO TRANSACTION
-- +goose Up
-- PostgreSQL's md5(text) is unavailable when OpenSSL runs in FIPS mode. The
-- three retention lane expression indexes made every accounts insert evaluate
-- md5(id), so a FIPS cell could migrate successfully but could never provision
-- or import an account. Account ids are restricted to ASCII; hashing their
-- bytea representation with PostgreSQL's built-in SHA-256 keeps the assignment
-- immutable and FIPS-safe. These expressions must stay byte-for-byte aligned
-- with the worker queries.
--
-- Build replacements before retiring the canonical indexes. A failed
-- concurrent build may leave a same-named invalid artifact, so remove only
-- that unusable temporary index on retry.
-- +goose StatementBegin
DO $$
BEGIN
  IF EXISTS (
    SELECT 1
      FROM pg_index
     WHERE indexrelid = to_regclass(
             format('%I.%I', current_schema(),
                    'accounts_transcript_retention_worker_lane_sha256_idx')
           )
       AND (NOT indisvalid OR NOT indisready)
  ) THEN
    EXECUTE format(
      'DROP INDEX %I.%I',
      current_schema(),
      'accounts_transcript_retention_worker_lane_sha256_idx'
    );
  END IF;
END
$$;
-- +goose StatementEnd

CREATE INDEX CONCURRENTLY IF NOT EXISTS
  accounts_transcript_retention_worker_lane_sha256_idx
  ON accounts ((get_byte(sha256(id::bytea), 0) % 16), id)
  WHERE plan_policies ? 'transcript_retention_days';

-- +goose StatementBegin
DO $$
BEGIN
  IF EXISTS (
    SELECT 1
      FROM pg_index
     WHERE indexrelid = to_regclass(
             format('%I.%I', current_schema(),
                    'accounts_message_retention_worker_lane_sha256_idx')
           )
       AND (NOT indisvalid OR NOT indisready)
  ) THEN
    EXECUTE format(
      'DROP INDEX %I.%I',
      current_schema(),
      'accounts_message_retention_worker_lane_sha256_idx'
    );
  END IF;
END
$$;
-- +goose StatementEnd

CREATE INDEX CONCURRENTLY IF NOT EXISTS
  accounts_message_retention_worker_lane_sha256_idx
  ON accounts ((get_byte(sha256(id::bytea), 0) % 16), id)
  WHERE plan_policies ? 'message_retention_days';

-- +goose StatementBegin
DO $$
BEGIN
  IF EXISTS (
    SELECT 1
      FROM pg_index
     WHERE indexrelid = to_regclass(
             format('%I.%I', current_schema(),
                    'accounts_agent_email_retention_worker_lane_sha256_idx')
           )
       AND (NOT indisvalid OR NOT indisready)
  ) THEN
    EXECUTE format(
      'DROP INDEX %I.%I',
      current_schema(),
      'accounts_agent_email_retention_worker_lane_sha256_idx'
    );
  END IF;
END
$$;
-- +goose StatementEnd

CREATE INDEX CONCURRENTLY IF NOT EXISTS
  accounts_agent_email_retention_worker_lane_sha256_idx
  ON accounts ((get_byte(sha256(id::bytea), 0) % 16), id)
  WHERE plan_policies ? 'agent_email_retention_days';

-- Replace one canonical index at a time. If the migration is interrupted
-- after a rename, rerunning it may rebuild that replacement once, but it never
-- leaves a canonical MD5 index after the migration is recorded complete.
DROP INDEX CONCURRENTLY IF EXISTS
  accounts_transcript_retention_worker_lane_idx;
ALTER INDEX accounts_transcript_retention_worker_lane_sha256_idx
  RENAME TO accounts_transcript_retention_worker_lane_idx;

DROP INDEX CONCURRENTLY IF EXISTS
  accounts_message_retention_worker_lane_idx;
ALTER INDEX accounts_message_retention_worker_lane_sha256_idx
  RENAME TO accounts_message_retention_worker_lane_idx;

DROP INDEX CONCURRENTLY IF EXISTS
  accounts_agent_email_retention_worker_lane_idx;
ALTER INDEX accounts_agent_email_retention_worker_lane_sha256_idx
  RENAME TO accounts_agent_email_retention_worker_lane_idx;

-- +goose Down
-- Fail before removing the usable SHA-256 indexes when this PostgreSQL runtime
-- cannot execute the legacy md5 expression. Without this guard an empty FIPS
-- cell could appear to downgrade successfully and then reject its next account
-- insert.
-- +goose StatementBegin
DO $$
BEGIN
  BEGIN
    PERFORM md5('witself-retention-lane-downgrade-probe');
  EXCEPTION WHEN OTHERS THEN
    RAISE EXCEPTION
      'cannot downgrade schema 73 while PostgreSQL md5 is unavailable'
      USING DETAIL = SQLERRM;
  END;
END
$$;
-- +goose StatementEnd

-- Build the legacy replacements first so a normal non-FIPS downgrade keeps
-- every canonical index available until its replacement is ready.
-- +goose StatementBegin
DO $$
BEGIN
  IF EXISTS (
    SELECT 1
      FROM pg_index
     WHERE indexrelid = to_regclass(
             format('%I.%I', current_schema(),
                    'accounts_transcript_retention_worker_lane_md5_idx')
           )
       AND (NOT indisvalid OR NOT indisready)
  ) THEN
    EXECUTE format(
      'DROP INDEX %I.%I',
      current_schema(),
      'accounts_transcript_retention_worker_lane_md5_idx'
    );
  END IF;
END
$$;
-- +goose StatementEnd

CREATE INDEX CONCURRENTLY IF NOT EXISTS
  accounts_transcript_retention_worker_lane_md5_idx
  ON accounts ((get_byte(decode(md5(id), 'hex'), 0) % 16), id)
  WHERE plan_policies ? 'transcript_retention_days';

-- +goose StatementBegin
DO $$
BEGIN
  IF EXISTS (
    SELECT 1
      FROM pg_index
     WHERE indexrelid = to_regclass(
             format('%I.%I', current_schema(),
                    'accounts_message_retention_worker_lane_md5_idx')
           )
       AND (NOT indisvalid OR NOT indisready)
  ) THEN
    EXECUTE format(
      'DROP INDEX %I.%I',
      current_schema(),
      'accounts_message_retention_worker_lane_md5_idx'
    );
  END IF;
END
$$;
-- +goose StatementEnd

CREATE INDEX CONCURRENTLY IF NOT EXISTS
  accounts_message_retention_worker_lane_md5_idx
  ON accounts ((get_byte(decode(md5(id), 'hex'), 0) % 16), id)
  WHERE plan_policies ? 'message_retention_days';

-- +goose StatementBegin
DO $$
BEGIN
  IF EXISTS (
    SELECT 1
      FROM pg_index
     WHERE indexrelid = to_regclass(
             format('%I.%I', current_schema(),
                    'accounts_agent_email_retention_worker_lane_md5_idx')
           )
       AND (NOT indisvalid OR NOT indisready)
  ) THEN
    EXECUTE format(
      'DROP INDEX %I.%I',
      current_schema(),
      'accounts_agent_email_retention_worker_lane_md5_idx'
    );
  END IF;
END
$$;
-- +goose StatementEnd

CREATE INDEX CONCURRENTLY IF NOT EXISTS
  accounts_agent_email_retention_worker_lane_md5_idx
  ON accounts ((get_byte(decode(md5(id), 'hex'), 0) % 16), id)
  WHERE plan_policies ? 'agent_email_retention_days';

DROP INDEX CONCURRENTLY IF EXISTS
  accounts_transcript_retention_worker_lane_idx;
ALTER INDEX accounts_transcript_retention_worker_lane_md5_idx
  RENAME TO accounts_transcript_retention_worker_lane_idx;

DROP INDEX CONCURRENTLY IF EXISTS
  accounts_message_retention_worker_lane_idx;
ALTER INDEX accounts_message_retention_worker_lane_md5_idx
  RENAME TO accounts_message_retention_worker_lane_idx;

DROP INDEX CONCURRENTLY IF EXISTS
  accounts_agent_email_retention_worker_lane_idx;
ALTER INDEX accounts_agent_email_retention_worker_lane_md5_idx
  RENAME TO accounts_agent_email_retention_worker_lane_idx;
