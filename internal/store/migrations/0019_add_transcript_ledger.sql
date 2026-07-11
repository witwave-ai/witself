-- +goose Up
-- Enterprise-visible conversation capture. This ledger is deliberately
-- separate from addressed inter-agent messages: owner_agent_id identifies the
-- authenticated agent integration that recorded the conversation, while each
-- entry's role is asserted transcript data (user/assistant/system/tool).
CREATE TABLE transcript_conversations (
    id             TEXT        PRIMARY KEY,
    account_id     TEXT        NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    realm_id       TEXT        NOT NULL REFERENCES realms(id),
    owner_agent_id TEXT        NOT NULL REFERENCES agents(id),
    external_id    TEXT,
    title          TEXT,
    metadata       JSONB       NOT NULL DEFAULT '{}'::jsonb,
    next_sequence  BIGINT      NOT NULL DEFAULT 1,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (account_id, owner_agent_id, external_id),
    CHECK (external_id IS NULL OR octet_length(external_id) <= 512),
    CHECK (title IS NULL OR octet_length(title) <= 256),
    CHECK (jsonb_typeof(metadata) = 'object'),
    CHECK (octet_length(metadata::text) <= 16384),
    CHECK (next_sequence >= 1)
);

CREATE INDEX transcript_conversations_by_account_activity
    ON transcript_conversations (account_id, updated_at DESC, id);

CREATE INDEX transcript_conversations_by_agent_activity
    ON transcript_conversations (owner_agent_id, updated_at DESC, id);

-- Entries are immutable. A row-locked next_sequence allocation on the parent
-- provides stable ordering under concurrent appends. artifacts is reserved as
-- an empty array until the portable object-store path lands; bounded structured
-- output belongs in payload today.
CREATE TABLE transcript_entries (
    id                   TEXT        PRIMARY KEY,
    account_id           TEXT        NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    transcript_id        TEXT        NOT NULL REFERENCES transcript_conversations(id) ON DELETE CASCADE,
    realm_id             TEXT        NOT NULL REFERENCES realms(id),
    recorded_by_agent_id TEXT        NOT NULL REFERENCES agents(id),
    sequence             BIGINT      NOT NULL,
    external_id          TEXT,
    role                 TEXT        NOT NULL,
    body                 TEXT        NOT NULL DEFAULT '',
    payload              JSONB,
    model                TEXT,
    reply_to_entry_id    TEXT        REFERENCES transcript_entries(id),
    artifacts            JSONB       NOT NULL DEFAULT '[]'::jsonb,
    created_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (transcript_id, sequence),
    UNIQUE (transcript_id, external_id),
    CHECK (sequence >= 1),
    CHECK (external_id IS NULL OR octet_length(external_id) <= 512),
    CHECK (role IN ('user', 'assistant', 'system', 'tool')),
    CHECK (octet_length(body) <= 65536),
    CHECK (payload IS NULL OR jsonb_typeof(payload) = 'object'),
    CHECK (payload IS NULL OR octet_length(payload::text) <= 16384),
    CHECK (model IS NULL OR octet_length(model) <= 256),
    CHECK (jsonb_typeof(artifacts) = 'array'),
    CHECK (octet_length(artifacts::text) <= 16384),
    CHECK (body <> '' OR payload IS NOT NULL)
);

CREATE INDEX transcript_entries_by_transcript
    ON transcript_entries (transcript_id, sequence, id);

CREATE INDEX transcript_entries_by_account_time
    ON transcript_entries (account_id, created_at DESC, id);

-- +goose Down
DROP INDEX transcript_entries_by_account_time;
DROP INDEX transcript_entries_by_transcript;
DROP TABLE transcript_entries;
DROP INDEX transcript_conversations_by_agent_activity;
DROP INDEX transcript_conversations_by_account_activity;
DROP TABLE transcript_conversations;
