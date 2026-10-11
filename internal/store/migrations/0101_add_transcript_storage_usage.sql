-- +goose Up
-- Fence account-first, like 0080: EXCLUSIVE on accounts blocks the FOR SHARE
-- that appends take first. SHARE ROW EXCLUSIVE on transcript_entries blocks
-- every entry write, including writers that skip the account row, and is the
-- mode CREATE TRIGGER needs, so no lock is upgraded. Plain reads continue;
-- NOWAIT makes a busy startup fail and retry.
LOCK TABLE accounts IN EXCLUSIVE MODE NOWAIT;
LOCK TABLE transcript_entries IN SHARE ROW EXCLUSIVE MODE NOWAIT;

-- A cell-local derived projection, never archived; import recomputes it.
-- Sixteen shards per account, chosen by transcript, mean appends to different
-- conversations rarely wait on the same row.
CREATE TABLE transcript_storage_usage (
    account_id     TEXT     NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    shard          SMALLINT NOT NULL CHECK (shard BETWEEN 0 AND 15),
    retained_bytes BIGINT   NOT NULL
      CHECK (retained_bytes BETWEEN 0 AND 4611686018427387903),
    PRIMARY KEY (account_id, shard)
);

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
         ORDER BY 1, 2
    LOOP
        INSERT INTO transcript_storage_usage (account_id, shard, retained_bytes)
        VALUES (delta.account_id, delta.shard, delta.retained_bytes)
        ON CONFLICT (account_id, shard) DO UPDATE
          SET retained_bytes = transcript_storage_usage.retained_bytes + EXCLUDED.retained_bytes;
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
               sum(witself_transcript_entry_storage_bytes(
                   external_id, role, body, payload, model, reply_to_entry_id, artifacts
               ))::bigint AS retained_bytes
          FROM old_rows
         GROUP BY 1, 2
         ORDER BY 1, 2
    LOOP
        -- Subtraction never raises or inserts: a drifted counter must never
        -- block retention or purge. Leave a missing shard missing.
        UPDATE transcript_storage_usage
           SET retained_bytes = GREATEST(retained_bytes - delta.retained_bytes, 0)
         WHERE account_id = delta.account_id AND shard = delta.shard;
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
    -- Apply one net delta per account/shard. Clamping the old-row subtraction
    -- before adding new rows would make a shrinking UPDATE inflate a drifted
    -- shard. One ordered pass also covers updates that move between shards.
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
         ORDER BY 1, 2
    LOOP
        IF delta.retained_bytes > 0 THEN
            INSERT INTO transcript_storage_usage (account_id, shard, retained_bytes)
            VALUES (delta.account_id, delta.shard, delta.retained_bytes)
            ON CONFLICT (account_id, shard) DO UPDATE
              SET retained_bytes = transcript_storage_usage.retained_bytes + EXCLUDED.retained_bytes;
        ELSE
            -- A decrease never inserts: leave a missing shard missing.
            UPDATE transcript_storage_usage
               SET retained_bytes = GREATEST(retained_bytes + delta.retained_bytes, 0)
             WHERE account_id = delta.account_id AND shard = delta.shard;
        END IF;
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

INSERT INTO transcript_storage_usage (account_id, shard, retained_bytes)
SELECT account_id, witself_transcript_storage_shard(transcript_id),
       sum(witself_transcript_entry_storage_bytes(
           external_id, role, body, payload, model, reply_to_entry_id, artifacts
       ))::bigint
  FROM transcript_entries
 GROUP BY 1, 2;

-- +goose Down
LOCK TABLE accounts IN ACCESS EXCLUSIVE MODE NOWAIT;
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
