-- +goose Up
CREATE TABLE fact_candidates (
    id                  TEXT        PRIMARY KEY,
    account_id          TEXT        NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    realm_id            TEXT        NOT NULL REFERENCES realms(id),
    owner_agent_id      TEXT        NOT NULL REFERENCES agents(id),
    subject_key         TEXT        NOT NULL,
    predicate           TEXT        NOT NULL,
    value_type          TEXT        NOT NULL,
    value               JSONB       NOT NULL,
    cardinality         TEXT        NOT NULL DEFAULT 'one',
    sensitive           BOOLEAN     NOT NULL DEFAULT FALSE,
    source_ref          TEXT        NOT NULL DEFAULT '',
    confidence          DOUBLE PRECISION NOT NULL DEFAULT 0.5,
    observed_at         TIMESTAMPTZ NOT NULL,
    valid_from          TIMESTAMPTZ,
    valid_until         TIMESTAMPTZ,
    reason              TEXT        NOT NULL DEFAULT '',
    status              TEXT        NOT NULL DEFAULT 'pending',
    conflict_fact_id    TEXT        REFERENCES facts(id) ON DELETE CASCADE,
    observed_assertion_id TEXT      REFERENCES fact_assertions(id) ON DELETE CASCADE,
    resolved_fact_id    TEXT        REFERENCES facts(id) ON DELETE CASCADE,
    proposed_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    decided_at          TIMESTAMPTZ,
    CHECK (id LIKE 'fcand\_%' ESCAPE '\'),
    CHECK (subject_key ~ '^[a-z][a-z0-9_.-]{0,254}$'),
    CHECK (predicate ~ '^[a-z][a-z0-9_.-]*(/[a-z0-9_.-]+){0,7}$'),
    CHECK (octet_length(predicate) <= 255),
    CHECK (value_type ~ '^[a-z][a-z0-9_.-]{0,63}$'),
    CHECK (status IN ('pending', 'conflict', 'confirmed', 'rejected')),
    CHECK (cardinality IN ('one', 'many', 'one_at_a_time')),
    CHECK (confidence >= 0 AND confidence <= 1),
    CHECK (valid_until IS NULL OR valid_from IS NULL OR valid_until >= valid_from),
    CHECK (decided_at IS NULL OR decided_at >= proposed_at),
    CHECK (octet_length(value::text) <= 65536),
    CHECK (octet_length(source_ref) <= 1024),
    CHECK (octet_length(reason) <= 1024),
    CHECK (
        (status = 'pending' AND conflict_fact_id IS NULL AND resolved_fact_id IS NULL AND decided_at IS NULL) OR
        (status = 'conflict' AND conflict_fact_id IS NOT NULL AND observed_assertion_id IS NOT NULL AND resolved_fact_id IS NULL AND decided_at IS NULL) OR
        (status = 'confirmed' AND resolved_fact_id IS NOT NULL AND decided_at IS NOT NULL) OR
        (status = 'rejected' AND resolved_fact_id IS NULL AND decided_at IS NOT NULL)
    )
);

CREATE INDEX fact_candidates_by_owner_status
    ON fact_candidates (owner_agent_id, status, proposed_at DESC, id);

-- +goose Down
DROP INDEX fact_candidates_by_owner_status;
DROP TABLE fact_candidates;
