-- +goose Up
-- tokens: server-validated credential records. Only the SHA-256 of each token is
-- stored; the plaintext is shown once at issuance. Bootstrap tokens are single-
-- use (consumed_at), operator/agent tokens are durable.
CREATE TABLE tokens (
    id          TEXT        PRIMARY KEY,
    account_id  TEXT        NOT NULL REFERENCES accounts(id),
    operator_id TEXT        REFERENCES operators(id),
    kind        TEXT        NOT NULL,        -- 'bootstrap' | 'operator' | 'agent'
    token_hash  TEXT        NOT NULL UNIQUE, -- sha256 hex of the token plaintext
    consumed_at TIMESTAMPTZ,                 -- set when a single-use token is spent
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- +goose Down
DROP TABLE tokens;
