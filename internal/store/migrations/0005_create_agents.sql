-- +goose Up
-- agents: machine principals bound to one realm. They own content (memories,
-- facts) and are addressable in messaging. Names are unique per realm.
CREATE TABLE agents (
    id         TEXT        PRIMARY KEY,
    realm_id   TEXT        NOT NULL REFERENCES realms(id),
    name       TEXT        NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (realm_id, name)
);

-- +goose Down
DROP TABLE agents;
