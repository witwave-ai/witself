-- +goose Up
-- Phase B is intentionally non-additive. Migration 0027 first had to reach
-- every writer while retaining this legacy conflict target; only then is it
-- safe to leave address uniqueness to the predicate-qualified partial index.
-- The application migration preflight prevents a populated schema 1..26 from
-- reaching this statement in the same binary invocation.
-- +goose StatementBegin
DO $$
DECLARE
    current_version BIGINT;
BEGIN
    -- Goose installations may either delete a version row on Down or append a
    -- newer is_applied=false row. Reduce each version to its latest recorded
    -- state before finding the current applied version; MAX over all historic
    -- true rows would incorrectly report 28 after a 28 -> 27 rollback.
    SELECT COALESCE(MAX(version_id) FILTER (WHERE is_applied), 0)
      INTO current_version
      FROM (
          SELECT DISTINCT ON (version_id) version_id, is_applied
            FROM goose_db_version
           ORDER BY version_id, id DESC
      ) AS latest_version_state;
    IF current_version <> 27 THEN
        RAISE EXCEPTION USING
            ERRCODE = '55000',
            MESSAGE = format(
                'schema 28 requires schema 27 as its immediate predecessor; current Goose version is %s',
                current_version
            );
    END IF;

    IF to_regclass('facts') IS NULL THEN
        RAISE EXCEPTION USING
            ERRCODE = '55000',
            MESSAGE = 'schema 28 requires the schema-27 facts table';
    END IF;

    IF NOT EXISTS (
        SELECT 1
          FROM pg_constraint AS c
         WHERE c.conrelid = to_regclass('facts')
           AND c.conname = 'facts_owner_agent_id_subject_id_predicate_key'
           AND c.contype = 'u'
           AND NOT c.condeferrable
           AND (
               SELECT array_agg(a.attname::text ORDER BY key_column.ordinality)
                 FROM unnest(c.conkey) WITH ORDINALITY AS key_column(attnum, ordinality)
                 JOIN pg_attribute AS a
                   ON a.attrelid = c.conrelid
                  AND a.attnum = key_column.attnum
           ) = ARRAY['owner_agent_id', 'subject_id', 'predicate']::text[]
    ) THEN
        RAISE EXCEPTION USING
            ERRCODE = '55000',
            MESSAGE = 'schema 28 requires the schema-27 legacy full-address UNIQUE constraint';
    END IF;

    IF NOT EXISTS (
        SELECT 1
          FROM pg_index AS i
         WHERE i.indexrelid = to_regclass('facts_one_active_address')
           AND i.indrelid = to_regclass('facts')
           AND i.indisunique
           AND i.indisvalid
           AND i.indisready
           AND i.indnkeyatts = 3
           AND (
               SELECT array_agg(a.attname::text ORDER BY key_column.ordinality)
                 FROM unnest(i.indkey::smallint[]) WITH ORDINALITY AS key_column(attnum, ordinality)
                 JOIN pg_attribute AS a
                   ON a.attrelid = i.indrelid
                  AND a.attnum = key_column.attnum
                WHERE key_column.ordinality <= i.indnkeyatts
           ) = ARRAY['owner_agent_id', 'subject_id', 'predicate']::text[]
           AND i.indpred IS NOT NULL
           AND pg_get_expr(i.indpred, i.indrelid) = '(deleted_at IS NULL)'
    ) THEN
        RAISE EXCEPTION USING
            ERRCODE = '55000',
            MESSAGE = 'schema 28 requires the valid schema-27 active-address partial UNIQUE index';
    END IF;

    IF NOT EXISTS (
        SELECT 1
          FROM pg_class
         WHERE oid = to_regclass('fact_mutation_tombstones')
           AND relkind = 'r'
    ) OR (
        SELECT COUNT(*)
          FROM pg_attribute
         WHERE attrelid = to_regclass('facts')
           AND attname = ANY (ARRAY[
               'deleted_at',
               'deleted_by_agent_id',
               'delete_receipt_id',
               'delete_idempotency_key_hash',
               'deleted_prior_assertion_id',
               'deleted_assertion_count',
               'deleted_candidate_count',
               'deleted_usage_count',
               'deleted_mutation_key_count',
               'deleted_candidate_revision',
               'recreated_at',
               'replacement_fact_id'
           ])
           AND NOT attisdropped
    ) <> 12 OR (
        SELECT COUNT(*)
          FROM pg_constraint
         WHERE conrelid = to_regclass('facts')
           AND conname = ANY (ARRAY[
               'facts_delete_receipt_shape',
               'facts_deleted_resolution',
               'facts_replacement_shape'
           ])
           AND contype = 'c'
    ) <> 3 THEN
        RAISE EXCEPTION USING
            ERRCODE = '55000',
            MESSAGE = 'schema 28 requires the complete schema-27 fact-deletion shape';
    END IF;
END
$$;
-- +goose StatementEnd

ALTER TABLE facts
    DROP CONSTRAINT facts_owner_agent_id_subject_id_predicate_key;

-- +goose Down
-- Rollback is deliberately non-destructive. A recreated address has both its
-- value-free tombstone and its replacement row, which schema 27 cannot place
-- under the legacy full UNIQUE constraint. Refuse instead of erasing receipts.
-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (
        SELECT 1
          FROM facts
         GROUP BY owner_agent_id, subject_id, predicate
        HAVING COUNT(*) > 1
    ) THEN
        RAISE EXCEPTION USING
            ERRCODE = '55000',
            MESSAGE = 'cannot downgrade schema 28 while recreated fact addresses exist; no fact rows were removed';
    END IF;
END
$$;
-- +goose StatementEnd

ALTER TABLE facts
    ADD CONSTRAINT facts_owner_agent_id_subject_id_predicate_key
    UNIQUE (owner_agent_id, subject_id, predicate);
