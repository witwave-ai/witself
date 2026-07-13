-- +goose Up
-- Fact mutations keep only retry keys and one-way request fingerprints beside
-- the durable resource they created. Raw fact values are never copied into a
-- separate idempotency record.
ALTER TABLE fact_assertions
    ADD COLUMN idempotency_key TEXT NOT NULL DEFAULT '',
    ADD COLUMN idempotency_fingerprint TEXT NOT NULL DEFAULT '';

ALTER TABLE fact_candidates
    ADD COLUMN idempotency_key TEXT NOT NULL DEFAULT '',
    ADD COLUMN idempotency_fingerprint TEXT NOT NULL DEFAULT '',
    ADD COLUMN decision_idempotency_key TEXT NOT NULL DEFAULT '',
    ADD COLUMN decision_assertion_id TEXT REFERENCES fact_assertions(id) ON DELETE CASCADE;

CREATE UNIQUE INDEX fact_assertions_by_owner_idempotency
    ON fact_assertions (asserted_by_agent_id, idempotency_key)
    WHERE idempotency_key <> '';

CREATE UNIQUE INDEX fact_candidates_by_owner_idempotency
    ON fact_candidates (owner_agent_id, idempotency_key)
    WHERE idempotency_key <> '';

CREATE UNIQUE INDEX fact_candidates_by_owner_decision_idempotency
    ON fact_candidates (owner_agent_id, decision_idempotency_key)
    WHERE decision_idempotency_key <> '';

ALTER TABLE fact_assertions
    ADD CONSTRAINT fact_assertions_idempotency_key_length
        CHECK (octet_length(idempotency_key) <= 512),
    ADD CONSTRAINT fact_assertions_idempotency_fingerprint_shape
        CHECK (idempotency_fingerprint = '' OR idempotency_fingerprint ~ '^[0-9a-f]{64}$'),
    ADD CONSTRAINT fact_assertions_idempotency_pair
        CHECK ((idempotency_key = '') = (idempotency_fingerprint = '')),
    ADD CONSTRAINT fact_assertions_idempotency_actor
        CHECK (idempotency_key = '' OR asserted_by_agent_id IS NOT NULL);

ALTER TABLE fact_candidates
    ADD CONSTRAINT fact_candidates_idempotency_key_length
        CHECK (octet_length(idempotency_key) <= 512),
    ADD CONSTRAINT fact_candidates_decision_idempotency_key_length
        CHECK (octet_length(decision_idempotency_key) <= 512),
    ADD CONSTRAINT fact_candidates_idempotency_fingerprint_shape
        CHECK (idempotency_fingerprint = '' OR idempotency_fingerprint ~ '^[0-9a-f]{64}$'),
    ADD CONSTRAINT fact_candidates_idempotency_pair
        CHECK ((idempotency_key = '') = (idempotency_fingerprint = '')),
    ADD CONSTRAINT fact_candidates_decision_idempotency_state
        CHECK (
            (decision_idempotency_key = '' AND decision_assertion_id IS NULL) OR
            (decision_idempotency_key <> '' AND status = 'confirmed' AND decision_assertion_id IS NOT NULL) OR
            (decision_idempotency_key <> '' AND status = 'rejected' AND decision_assertion_id IS NULL)
        );

-- +goose Down
DROP INDEX fact_candidates_by_owner_decision_idempotency;
DROP INDEX fact_candidates_by_owner_idempotency;
DROP INDEX fact_assertions_by_owner_idempotency;

ALTER TABLE fact_candidates
    DROP CONSTRAINT fact_candidates_decision_idempotency_state,
    DROP CONSTRAINT fact_candidates_idempotency_pair,
    DROP CONSTRAINT fact_candidates_idempotency_fingerprint_shape,
    DROP CONSTRAINT fact_candidates_decision_idempotency_key_length,
    DROP CONSTRAINT fact_candidates_idempotency_key_length,
    DROP COLUMN decision_assertion_id,
    DROP COLUMN decision_idempotency_key,
    DROP COLUMN idempotency_fingerprint,
    DROP COLUMN idempotency_key;

ALTER TABLE fact_assertions
    DROP CONSTRAINT fact_assertions_idempotency_actor,
    DROP CONSTRAINT fact_assertions_idempotency_pair,
    DROP CONSTRAINT fact_assertions_idempotency_fingerprint_shape,
    DROP CONSTRAINT fact_assertions_idempotency_key_length,
    DROP COLUMN idempotency_fingerprint,
    DROP COLUMN idempotency_key;
