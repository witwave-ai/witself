-- +goose Up
-- realms: the isolation unit within an account. Agents, memories, facts, and
-- messages all live inside a realm. Names are unique per account.
CREATE TABLE realms (
    id         TEXT        PRIMARY KEY,
    account_id TEXT        NOT NULL REFERENCES accounts(id),
    name       TEXT        NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (account_id, name)
);

-- +goose Down
DROP TABLE realms;
