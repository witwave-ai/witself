-- +goose Up
-- The accounts table: the top of the tenancy tree (account -> realm -> agent).
-- Local and self-managed seed exactly one default (root) account; Cloud has many.
CREATE TABLE accounts (
    id           TEXT PRIMARY KEY,
    is_default   BOOLEAN     NOT NULL DEFAULT FALSE,
    display_name TEXT        NOT NULL DEFAULT '',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- At most one default/root account per deployment.
CREATE UNIQUE INDEX accounts_one_default ON accounts (is_default) WHERE is_default;

-- +goose Down
DROP TABLE accounts;
