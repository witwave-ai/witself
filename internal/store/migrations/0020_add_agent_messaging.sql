-- +goose Up
-- Realm-local, direct agent messaging. Sender and realm are always derived
-- from the authenticated token; to_agent_id is resolved inside that realm.
CREATE TABLE agent_messages (
    id                 TEXT        PRIMARY KEY,
    account_id         TEXT        NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    realm_id           TEXT        NOT NULL REFERENCES realms(id),
    from_agent_id      TEXT        NOT NULL REFERENCES agents(id),
    to_agent_id        TEXT        NOT NULL REFERENCES agents(id),
    subject            TEXT        NOT NULL DEFAULT '',
    kind               TEXT        NOT NULL DEFAULT 'note',
    body               TEXT        NOT NULL,
    payload            JSONB,
    thread_id          TEXT        NOT NULL,
    idempotency_key    TEXT,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (account_id, from_agent_id, idempotency_key),
    CHECK (octet_length(subject) <= 256),
    CHECK (octet_length(kind) BETWEEN 1 AND 64),
    CHECK (octet_length(body) BETWEEN 1 AND 65536),
    CHECK (payload IS NULL OR jsonb_typeof(payload) = 'object'),
    CHECK (payload IS NULL OR octet_length(payload::text) <= 16384),
    CHECK (thread_id LIKE 'thr\_%' ESCAPE '\' AND octet_length(thread_id) <= 128),
    CHECK (idempotency_key IS NULL OR octet_length(idempotency_key) <= 512)
);

CREATE INDEX agent_messages_by_sender_activity
    ON agent_messages (account_id, from_agent_id, created_at DESC, id DESC);

CREATE INDEX agent_messages_by_thread
    ON agent_messages (account_id, realm_id, thread_id, created_at, id);

-- Delivery/read/ack state is per recipient even though this first slice has
-- one direct agent recipient. The shape supports later snapshot group fan-out.
CREATE TABLE agent_message_deliveries (
    message_id          TEXT        NOT NULL REFERENCES agent_messages(id) ON DELETE CASCADE,
    account_id          TEXT        NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    realm_id            TEXT        NOT NULL REFERENCES realms(id),
    recipient_agent_id  TEXT        NOT NULL REFERENCES agents(id),
    state               TEXT        NOT NULL DEFAULT 'delivered',
    delivered_at        TIMESTAMPTZ,
    read_at             TIMESTAMPTZ,
    acked_at            TIMESTAMPTZ,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (message_id, recipient_agent_id),
    CHECK (state IN ('queued', 'delivered', 'failed')),
    CHECK (acked_at IS NULL OR read_at IS NOT NULL)
);

CREATE INDEX agent_message_deliveries_by_recipient
    ON agent_message_deliveries
       (account_id, realm_id, recipient_agent_id, read_at, message_id);

-- +goose Down
DROP INDEX agent_message_deliveries_by_recipient;
DROP TABLE agent_message_deliveries;
DROP INDEX agent_messages_by_thread;
DROP INDEX agent_messages_by_sender_activity;
DROP TABLE agent_messages;
