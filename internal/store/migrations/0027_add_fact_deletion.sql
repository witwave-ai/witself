-- +goose Up
-- A deleted fact keeps only its stable address and a value-free receipt. The
-- assertion history and candidates are hard-deleted by the store transaction;
-- usage events intentionally continue to reference the stable fact id.
-- Phase A deliberately retains the schema-26 full address UNIQUE constraint.
-- The partial index and columns can roll out beneath old binaries, while fact
-- deletion/recreation remains transport-gated until a later migration removes
-- the full constraint after every writer uses predicate-qualified upserts.
ALTER TABLE facts
    ADD COLUMN deleted_at TIMESTAMPTZ,
    ADD COLUMN deleted_by_agent_id TEXT REFERENCES agents(id),
    ADD COLUMN delete_receipt_id TEXT NOT NULL DEFAULT '',
    ADD COLUMN delete_idempotency_key_hash TEXT NOT NULL DEFAULT '',
    ADD COLUMN deleted_prior_assertion_id TEXT NOT NULL DEFAULT '',
    ADD COLUMN deleted_assertion_count BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN deleted_candidate_count BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN deleted_usage_count BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN deleted_mutation_key_count BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN deleted_candidate_revision TEXT NOT NULL DEFAULT '',
    ADD COLUMN recreated_at TIMESTAMPTZ,
    ADD COLUMN replacement_fact_id TEXT;

CREATE UNIQUE INDEX facts_one_active_address
    ON facts (owner_agent_id, subject_id, predicate)
    WHERE deleted_at IS NULL;

CREATE UNIQUE INDEX facts_by_owner_delete_idempotency
    ON facts (owner_agent_id, delete_idempotency_key_hash)
    WHERE delete_idempotency_key_hash <> '';

CREATE UNIQUE INDEX facts_delete_receipt_id
    ON facts (delete_receipt_id)
    WHERE delete_receipt_id <> '';

ALTER TABLE facts
    ADD CONSTRAINT facts_delete_receipt_shape CHECK (
        (deleted_at IS NULL AND deleted_by_agent_id IS NULL AND
         delete_receipt_id = '' AND delete_idempotency_key_hash = '' AND
         deleted_prior_assertion_id = '' AND deleted_assertion_count = 0 AND
         deleted_candidate_count = 0 AND deleted_usage_count = 0 AND
         deleted_mutation_key_count = 0 AND deleted_candidate_revision = '') OR
        (deleted_at IS NOT NULL AND deleted_by_agent_id IS NOT NULL AND
         delete_receipt_id LIKE 'fdel\_%' ESCAPE '\' AND
         delete_idempotency_key_hash ~ '^[0-9a-f]{64}$' AND
         deleted_prior_assertion_id LIKE 'fas\_%' ESCAPE '\' AND
         deleted_assertion_count > 0 AND deleted_candidate_count >= 0 AND
         deleted_usage_count >= 0 AND deleted_mutation_key_count >= 0 AND
         deleted_candidate_revision ~ '^[0-9a-f]{64}$')
    ),
    ADD CONSTRAINT facts_deleted_resolution CHECK (
        deleted_at IS NULL OR resolved_assertion_id IS NULL
    ),
    ADD CONSTRAINT facts_replacement_shape CHECK (
        (recreated_at IS NULL AND replacement_fact_id IS NULL) OR
        (deleted_at IS NOT NULL AND recreated_at IS NOT NULL AND
         replacement_fact_id LIKE 'fact\_%' ESCAPE '\' AND
         replacement_fact_id <> id AND recreated_at >= deleted_at)
    );

-- When assertion/candidate rows are erased, their mutation retry keys must
-- remain blocked. Only a one-way key hash is retained: no raw key, request
-- fingerprint, value, source reference, or candidate reason survives.
CREATE TABLE fact_mutation_tombstones (
    id                   TEXT        PRIMARY KEY,
    account_id           TEXT        NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    realm_id             TEXT        NOT NULL REFERENCES realms(id),
    owner_agent_id       TEXT        NOT NULL REFERENCES agents(id),
    fact_id              TEXT        NOT NULL REFERENCES facts(id) ON DELETE CASCADE,
    surface              TEXT        NOT NULL,
    idempotency_key_hash TEXT        NOT NULL,
    deleted_at           TIMESTAMPTZ NOT NULL,
    CHECK (id LIKE 'fmt\_%' ESCAPE '\'),
    CHECK (surface IN ('set', 'proposal')),
    CHECK (idempotency_key_hash ~ '^[0-9a-f]{64}$'),
    UNIQUE (owner_agent_id, surface, idempotency_key_hash)
);

CREATE INDEX fact_mutation_tombstones_by_fact
    ON fact_mutation_tombstones (fact_id, surface, id);

-- +goose Down
DROP INDEX fact_mutation_tombstones_by_fact;
DROP TABLE fact_mutation_tombstones;

ALTER TABLE facts
    DROP CONSTRAINT facts_replacement_shape,
    DROP CONSTRAINT facts_deleted_resolution,
    DROP CONSTRAINT facts_delete_receipt_shape;

DROP INDEX facts_by_owner_delete_idempotency;
DROP INDEX facts_delete_receipt_id;
DROP INDEX facts_one_active_address;

-- A rollback cannot represent deletion receipts. Remove only already
-- content-free tombstones so schema 26 is not left with address-blocking rows
-- whose resolved assertion is NULL.
DELETE FROM facts WHERE deleted_at IS NOT NULL;

ALTER TABLE facts
    DROP COLUMN replacement_fact_id,
    DROP COLUMN recreated_at,
    DROP COLUMN deleted_candidate_revision,
    DROP COLUMN deleted_mutation_key_count,
    DROP COLUMN deleted_usage_count,
    DROP COLUMN deleted_candidate_count,
    DROP COLUMN deleted_assertion_count,
    DROP COLUMN deleted_prior_assertion_id,
    DROP COLUMN delete_idempotency_key_hash,
    DROP COLUMN delete_receipt_id,
    DROP COLUMN deleted_by_agent_id,
    DROP COLUMN deleted_at;
