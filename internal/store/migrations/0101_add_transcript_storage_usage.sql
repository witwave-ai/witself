-- +goose Up
SELECT set_config('statement_timeout', '120s', true),
       set_config('lock_timeout', '2s', true),
       set_config('idle_in_transaction_session_timeout', '5s', true),
       set_config('client_connection_check_interval', '1s', true);

-- This comes before catalog writes so retries hold no strong relation locks.
-- Use lock_timeout, not NOWAIT, so later attempts can outwait deadlock_timeout
-- and let PostgreSQL cancel a conflicting non-wraparound autovacuum. The probe
-- avoids queuing behind long client transactions and stalling newer writers.
-- +goose StatementBegin
DO $$
DECLARE
    max_attempts   int := COALESCE(NULLIF(current_setting('witself.migration_0101_lock_attempts', true), '')::int, 20);
    long_holder_ms int := COALESCE(NULLIF(current_setting('witself.migration_0101_long_holder_ms', true), '')::int, 10000);
    late_ms        int := LEAST(GREATEST(
                            (extract(epoch FROM current_setting('deadlock_timeout')::interval) * 1000)::int + 1000,
                            2000), 4000);
    deadline       timestamptz := clock_timestamp() + interval '90 seconds';
    attempt        int := 0;
BEGIN
    LOOP
        attempt := attempt + 1;
        IF attempt > max_attempts OR clock_timestamp() > deadline THEN
            RAISE EXCEPTION USING ERRCODE = 'lock_not_available',
                MESSAGE = format('migration 0101: transcript_entries stayed busy for %s lock attempts', attempt - 1);
        END IF;
        -- pg_stat_activity is cached for the transaction, even after a
        -- subtransaction rollback; refresh it to see holders that started later.
        PERFORM pg_stat_clear_snapshot();
        IF EXISTS (
            SELECT 1
              FROM pg_locks l JOIN pg_stat_activity a ON a.pid = l.pid
             WHERE l.locktype = 'relation'
               AND l.database = (SELECT oid FROM pg_database WHERE datname = current_database())
               AND l.relation = 'transcript_entries'::regclass
               AND l.granted AND l.pid <> pg_backend_pid()
               AND l.mode IN ('RowExclusiveLock','ShareUpdateExclusiveLock','ShareLock',
                              'ShareRowExclusiveLock','ExclusiveLock','AccessExclusiveLock')
               AND a.backend_type = 'client backend'
               AND a.xact_start < clock_timestamp() - long_holder_ms * interval '1 millisecond'
        ) THEN
            PERFORM pg_sleep(1);
            CONTINUE;
        END IF;
        PERFORM set_config('lock_timeout',
            (CASE WHEN attempt <= 3 THEN 1000 ELSE late_ms END)::text || 'ms', true);
        BEGIN
            LOCK TABLE transcript_entries IN SHARE ROW EXCLUSIVE MODE;
            EXIT;
        EXCEPTION WHEN lock_not_available THEN
            PERFORM pg_sleep(0.1 + random() * 0.2);
        END;
    END LOOP;
    PERFORM set_config('lock_timeout', '2s', true);
END
$$;
-- +goose StatementEnd

CREATE SEQUENCE transcript_storage_usage_epoch_seq AS BIGINT;

-- No REFERENCES accounts: that would strongly lock accounts during installation,
-- and cascading removal would be a non-linear write. Orphan rows are harmless.
CREATE TABLE transcript_storage_usage (
    account_id      TEXT        NOT NULL,
    shard           SMALLINT    NOT NULL CHECK (shard BETWEEN 0 AND 15),
    retained_bytes  BIGINT      NOT NULL DEFAULT 0
      CHECK (retained_bytes BETWEEN -4611686018427387903 AND 4611686018427387903),
    exact           BOOLEAN     NOT NULL DEFAULT true,
    reconcile_epoch BIGINT      NOT NULL DEFAULT nextval('transcript_storage_usage_epoch_seq'),
    verified_at     TIMESTAMPTZ,
    PRIMARY KEY (account_id, shard)
);

ALTER SEQUENCE transcript_storage_usage_epoch_seq OWNED BY transcript_storage_usage.reconcile_epoch;

CREATE INDEX transcript_storage_usage_unverified ON transcript_storage_usage (account_id, shard) WHERE NOT exact;

-- The entry half of transcript_storage_byte, matching transcriptEntryLogicalBytes.
-- +goose StatementBegin
CREATE FUNCTION witself_transcript_entry_storage_bytes(
    external_id TEXT, role TEXT, body TEXT, payload JSONB, model TEXT,
    reply_to_entry_id TEXT, artifacts JSONB
) RETURNS BIGINT
LANGUAGE sql IMMUTABLE PARALLEL SAFE
AS $$
    SELECT (COALESCE(octet_length(external_id),0) + octet_length(role) + octet_length(body)
          + COALESCE(octet_length(payload::text),0) + COALESCE(octet_length(model),0)
          + COALESCE(octet_length(reply_to_entry_id),0) + octet_length(artifacts::text))::bigint;
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION witself_transcript_storage_shard(transcript_id TEXT) RETURNS SMALLINT
LANGUAGE sql STABLE PARALLEL SAFE
AS $$
    SELECT (get_byte(sha256(convert_to(transcript_id, 'UTF8')), 0) % 16)::smallint;
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION witself_transcript_storage_usage_insert() RETURNS trigger
LANGUAGE plpgsql
AS $$
DECLARE
    delta RECORD;
BEGIN
    IF current_setting('witself.transcript_storage_usage_bulk', true) = 'on' THEN
        RETURN NULL;
    END IF;
    FOR delta IN
        SELECT account_id, witself_transcript_storage_shard(transcript_id) AS shard,
               sum(witself_transcript_entry_storage_bytes(
                   external_id, role, body, payload, model, reply_to_entry_id, artifacts
               ))::bigint AS retained_bytes
          FROM new_rows
         GROUP BY 1, 2
        HAVING sum(witself_transcript_entry_storage_bytes(
                   external_id, role, body, payload, model, reply_to_entry_id, artifacts
               )) <> 0
         ORDER BY 1, 2
    LOOP
        INSERT INTO transcript_storage_usage AS u (account_id, shard, retained_bytes, exact)
        VALUES (delta.account_id, delta.shard, delta.retained_bytes, delta.retained_bytes > 0)
        ON CONFLICT (account_id, shard) DO UPDATE
           SET retained_bytes = u.retained_bytes + EXCLUDED.retained_bytes,
               exact = u.exact AND u.retained_bytes + EXCLUDED.retained_bytes >= 0;
    END LOOP;
    RETURN NULL;
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION witself_transcript_storage_usage_delete() RETURNS trigger
LANGUAGE plpgsql
AS $$
DECLARE
    delta RECORD;
BEGIN
    IF current_setting('witself.transcript_storage_usage_bulk', true) = 'on' THEN
        RETURN NULL;
    END IF;
    FOR delta IN
        SELECT account_id, witself_transcript_storage_shard(transcript_id) AS shard,
               -sum(witself_transcript_entry_storage_bytes(
                   external_id, role, body, payload, model, reply_to_entry_id, artifacts
               ))::bigint AS retained_bytes
          FROM old_rows
         GROUP BY 1, 2
        HAVING sum(witself_transcript_entry_storage_bytes(
                   external_id, role, body, payload, model, reply_to_entry_id, artifacts
               )) <> 0
         ORDER BY 1, 2
    LOOP
        INSERT INTO transcript_storage_usage AS u (account_id, shard, retained_bytes, exact)
        VALUES (delta.account_id, delta.shard, delta.retained_bytes, delta.retained_bytes > 0)
        ON CONFLICT (account_id, shard) DO UPDATE
           SET retained_bytes = u.retained_bytes + EXCLUDED.retained_bytes,
               exact = u.exact AND u.retained_bytes + EXCLUDED.retained_bytes >= 0;
    END LOOP;
    RETURN NULL;
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION witself_transcript_storage_usage_update() RETURNS trigger
LANGUAGE plpgsql
AS $$
DECLARE
    delta RECORD;
BEGIN
    IF current_setting('witself.transcript_storage_usage_bulk', true) = 'on' THEN
        RETURN NULL;
    END IF;
    -- Apply one signed net delta per account/shard, in stable lock order.
    FOR delta IN
        SELECT account_id, shard, sum(retained_bytes)::bigint AS retained_bytes
          FROM (
              SELECT account_id, witself_transcript_storage_shard(transcript_id) AS shard,
                     -witself_transcript_entry_storage_bytes(
                         external_id, role, body, payload, model, reply_to_entry_id, artifacts
                     ) AS retained_bytes
                FROM old_rows
              UNION ALL
              SELECT account_id, witself_transcript_storage_shard(transcript_id) AS shard,
                     witself_transcript_entry_storage_bytes(
                         external_id, role, body, payload, model, reply_to_entry_id, artifacts
                     ) AS retained_bytes
                FROM new_rows
          ) AS changes
         GROUP BY 1, 2
        HAVING sum(retained_bytes) <> 0
         ORDER BY 1, 2
    LOOP
        INSERT INTO transcript_storage_usage AS u (account_id, shard, retained_bytes, exact)
        VALUES (delta.account_id, delta.shard, delta.retained_bytes, delta.retained_bytes > 0)
        ON CONFLICT (account_id, shard) DO UPDATE
           SET retained_bytes = u.retained_bytes + EXCLUDED.retained_bytes,
               exact = u.exact AND u.retained_bytes + EXCLUDED.retained_bytes >= 0;
    END LOOP;
    RETURN NULL;
END
$$;
-- +goose StatementEnd

CREATE TRIGGER transcript_entries_storage_usage_insert
AFTER INSERT ON transcript_entries
REFERENCING NEW TABLE AS new_rows
FOR EACH STATEMENT EXECUTE FUNCTION witself_transcript_storage_usage_insert();

CREATE TRIGGER transcript_entries_storage_usage_delete
AFTER DELETE ON transcript_entries
REFERENCING OLD TABLE AS old_rows
FOR EACH STATEMENT EXECUTE FUNCTION witself_transcript_storage_usage_delete();

CREATE TRIGGER transcript_entries_storage_usage_update
AFTER UPDATE ON transcript_entries
REFERENCING OLD TABLE AS old_rows NEW TABLE AS new_rows
FOR EACH STATEMENT EXECUTE FUNCTION witself_transcript_storage_usage_update();

INSERT INTO transcript_storage_usage (account_id, shard, retained_bytes, exact)
SELECT a.id, g.s::smallint, 0, false
  FROM accounts a CROSS JOIN generate_series(0, 15) AS g(s)
 WHERE EXISTS (SELECT 1 FROM transcript_entries e WHERE e.account_id = a.id)
 ORDER BY 1, 2;

-- +goose Down
LOCK TABLE transcript_entries IN ACCESS EXCLUSIVE MODE NOWAIT;

DROP TRIGGER transcript_entries_storage_usage_insert ON transcript_entries;
DROP TRIGGER transcript_entries_storage_usage_delete ON transcript_entries;
DROP TRIGGER transcript_entries_storage_usage_update ON transcript_entries;
DROP FUNCTION witself_transcript_storage_usage_insert();
DROP FUNCTION witself_transcript_storage_usage_delete();
DROP FUNCTION witself_transcript_storage_usage_update();
DROP FUNCTION witself_transcript_entry_storage_bytes(TEXT, TEXT, TEXT, JSONB, TEXT, TEXT, JSONB);
DROP FUNCTION witself_transcript_storage_shard(TEXT);
DROP TABLE transcript_storage_usage;
