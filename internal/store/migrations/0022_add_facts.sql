-- +goose Up
-- Subjects separate who/what a fact describes from the agent that owns the
-- knowledge. The canonical key is stable within one agent's fact collection;
-- aliases support conversational resolution without entering the lookup key.
CREATE TABLE fact_subjects (
    id             TEXT        PRIMARY KEY,
    account_id     TEXT        NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    realm_id       TEXT        NOT NULL REFERENCES realms(id),
    owner_agent_id TEXT        NOT NULL REFERENCES agents(id),
    canonical_key  TEXT        NOT NULL,
    display_name   TEXT        NOT NULL DEFAULT '',
    aliases        JSONB       NOT NULL DEFAULT '[]'::jsonb,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (owner_agent_id, canonical_key),
    CHECK (id LIKE 'sub\_%' ESCAPE '\'),
    CHECK (canonical_key ~ '^[a-z][a-z0-9_.-]{0,254}$'),
    CHECK (jsonb_typeof(aliases) = 'array'),
    CHECK (octet_length(aliases::text) <= 8192)
);

-- A fact is the stable resolved identity addressed by subject + predicate.
-- Source-specific and historical values live in fact_assertions.
CREATE TABLE facts (
    id                    TEXT        PRIMARY KEY,
    account_id            TEXT        NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    realm_id              TEXT        NOT NULL REFERENCES realms(id),
    owner_agent_id        TEXT        NOT NULL REFERENCES agents(id),
    subject_id            TEXT        NOT NULL REFERENCES fact_subjects(id) ON DELETE CASCADE,
    predicate             TEXT        NOT NULL,
    cardinality           TEXT        NOT NULL DEFAULT 'one',
    sensitive             BOOLEAN     NOT NULL DEFAULT FALSE,
    resolved_assertion_id TEXT,
    created_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (owner_agent_id, subject_id, predicate),
    CHECK (id LIKE 'fact\_%' ESCAPE '\'),
    CHECK (predicate ~ '^[a-z][a-z0-9_.-]*(/[a-z0-9_.-]+){0,7}$'),
    CHECK (octet_length(predicate) <= 255),
    CHECK (cardinality IN ('one', 'many', 'one_at_a_time'))
);

-- Assertions preserve provenance and corrections. Superseding an assertion
-- closes its real-world validity window when supplied and never erases it.
CREATE TABLE fact_assertions (
    id                  TEXT        PRIMARY KEY,
    fact_id             TEXT        NOT NULL REFERENCES facts(id) ON DELETE CASCADE,
    account_id          TEXT        NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    realm_id            TEXT        NOT NULL REFERENCES realms(id),
    asserted_by_agent_id TEXT       REFERENCES agents(id),
    value_type          TEXT        NOT NULL,
    value               JSONB       NOT NULL,
    source_kind         TEXT        NOT NULL,
    source_ref          TEXT        NOT NULL DEFAULT '',
    confidence          DOUBLE PRECISION NOT NULL DEFAULT 1,
    observed_at         TIMESTAMPTZ NOT NULL,
    confirmed_at        TIMESTAMPTZ,
    valid_from          TIMESTAMPTZ,
    valid_until         TIMESTAMPTZ,
    supersedes_id       TEXT        REFERENCES fact_assertions(id),
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (id LIKE 'fas\_%' ESCAPE '\'),
    CHECK (value_type ~ '^[a-z][a-z0-9_.-]{0,63}$'),
    CHECK (source_kind IN ('self', 'operator', 'agent', 'import', 'inference')),
    CHECK (octet_length(source_ref) <= 1024),
    CHECK (confidence >= 0 AND confidence <= 1),
    CHECK (valid_until IS NULL OR valid_from IS NULL OR valid_until >= valid_from),
    CHECK (octet_length(value::text) <= 65536)
);

CREATE INDEX facts_by_owner_predicate
    ON facts (owner_agent_id, predicate, updated_at DESC, id);
CREATE INDEX fact_assertions_by_fact_history
    ON fact_assertions (fact_id, created_at DESC, id);
CREATE INDEX fact_assertions_by_validity
    ON fact_assertions (fact_id, valid_from DESC, valid_until);

-- +goose Down
DROP INDEX fact_assertions_by_validity;
DROP INDEX fact_assertions_by_fact_history;
DROP INDEX facts_by_owner_predicate;
DROP TABLE fact_assertions;
DROP TABLE facts;
DROP TABLE fact_subjects;
