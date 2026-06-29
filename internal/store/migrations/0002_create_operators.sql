-- +goose Up
-- operators: the human/admin principals on an account (distinct from agents,
-- which are machine principals bound to a realm). The bootstrap seeds one root
-- account_owner; more operators can be added later.
CREATE TABLE operators (
    id           TEXT        PRIMARY KEY,
    account_id   TEXT        NOT NULL REFERENCES accounts(id),
    role         TEXT        NOT NULL,
    is_root      BOOLEAN     NOT NULL DEFAULT FALSE,
    display_name TEXT        NOT NULL DEFAULT '',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- At most one root (seeded) operator per account.
CREATE UNIQUE INDEX operators_one_root_per_account ON operators (account_id) WHERE is_root;

-- +goose Down
DROP TABLE operators;
