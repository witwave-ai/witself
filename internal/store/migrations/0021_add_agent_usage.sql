-- +goose Up
-- Immutable product-usage facts. The event ledger is the portable source of
-- truth; hourly and daily rollups are transactionally maintained projections
-- used by the user-facing usage API.
CREATE TABLE usage_events (
    id               TEXT        PRIMARY KEY,
    account_id       TEXT        NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    realm_id         TEXT        NOT NULL REFERENCES realms(id),
    agent_id         TEXT        NOT NULL REFERENCES agents(id),
    dimension        TEXT        NOT NULL,
    quantity         BIGINT      NOT NULL,
    unit             TEXT        NOT NULL,
    subject_type     TEXT        NOT NULL,
    subject_id       TEXT        NOT NULL,
    idempotency_key  TEXT        NOT NULL,
    metadata         JSONB       NOT NULL DEFAULT '{}'::jsonb,
    occurred_at      TIMESTAMPTZ NOT NULL,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (account_id, idempotency_key),
    CHECK (id LIKE 'usg\_%' ESCAPE '\'),
    CHECK (dimension ~ '^[a-z][a-z0-9_]{0,63}$'),
    CHECK (quantity > 0),
    CHECK (unit ~ '^[a-z][a-z0-9_]{0,31}$'),
    CHECK (subject_type ~ '^[a-z][a-z0-9_]{0,31}$'),
    CHECK (octet_length(subject_id) BETWEEN 1 AND 256),
    CHECK (octet_length(idempotency_key) BETWEEN 1 AND 512),
    CHECK (jsonb_typeof(metadata) = 'object'),
    CHECK (octet_length(metadata::text) <= 4096)
);

CREATE INDEX usage_events_by_agent_time
    ON usage_events (agent_id, occurred_at DESC, id);

CREATE INDEX usage_events_by_account_dimension_time
    ON usage_events (account_id, dimension, occurred_at DESC, id);

CREATE TABLE usage_rollups (
    account_id    TEXT        NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    realm_id      TEXT        NOT NULL REFERENCES realms(id),
    agent_id      TEXT        NOT NULL REFERENCES agents(id),
    dimension     TEXT        NOT NULL,
    unit          TEXT        NOT NULL,
    bucket        TEXT        NOT NULL,
    bucket_start  TIMESTAMPTZ NOT NULL,
    quantity      BIGINT      NOT NULL,
    event_count   BIGINT      NOT NULL,
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (agent_id, dimension, unit, bucket, bucket_start),
    CHECK (dimension ~ '^[a-z][a-z0-9_]{0,63}$'),
    CHECK (unit ~ '^[a-z][a-z0-9_]{0,31}$'),
    CHECK (bucket IN ('hour', 'day')),
    CHECK (quantity > 0),
    CHECK (event_count > 0)
);

CREATE INDEX usage_rollups_by_account_time
    ON usage_rollups (account_id, bucket, bucket_start DESC, agent_id);

-- +goose Down
DROP INDEX usage_rollups_by_account_time;
DROP TABLE usage_rollups;
DROP INDEX usage_events_by_account_dimension_time;
DROP INDEX usage_events_by_agent_time;
DROP TABLE usage_events;
