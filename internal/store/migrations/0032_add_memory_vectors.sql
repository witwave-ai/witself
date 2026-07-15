-- +goose Up
-- Narrative-memory vectors are optional, client-authored derived indexes.
-- JSONB keeps the portable lexical baseline independent of pgvector; a later
-- deployment optimization may mirror these rows into a pgvector index without
-- changing this canonical contract or requiring backend model credentials.
CREATE TABLE memory_vector_profiles (
    id                 TEXT        PRIMARY KEY,
    account_id         TEXT        NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    realm_id           TEXT        NOT NULL REFERENCES realms(id),
    owner_kind         TEXT        NOT NULL DEFAULT 'agent',
    owner_id           TEXT        NOT NULL REFERENCES agents(id),
    provider           TEXT        NOT NULL,
    model              TEXT        NOT NULL,
    recipe             TEXT        NOT NULL,
    recipe_version     TEXT        NOT NULL,
    dimensions         INTEGER     NOT NULL,
    distance_metric    TEXT        NOT NULL,
    normalization      TEXT        NOT NULL,
    contract_hash      TEXT        NOT NULL,
    created_by_agent_id TEXT       NOT NULL REFERENCES agents(id),
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (id, account_id, realm_id, owner_kind, owner_id),
    UNIQUE (account_id, realm_id, owner_kind, owner_id, contract_hash),
    CHECK (id ~ '^mvp_[a-z2-7]{16}$'),
    CHECK (owner_kind = 'agent'),
    CHECK (octet_length(provider) BETWEEN 1 AND 128),
    CHECK (octet_length(model) BETWEEN 1 AND 256),
    CHECK (octet_length(recipe) BETWEEN 1 AND 128),
    CHECK (octet_length(recipe_version) BETWEEN 1 AND 128),
    CHECK (dimensions BETWEEN 1 AND 4096),
    CHECK (distance_metric IN ('cosine', 'dot', 'euclidean')),
    CHECK (normalization IN ('none', 'l2')),
    CHECK (distance_metric <> 'dot' OR normalization = 'l2'),
    CHECK (contract_hash ~ '^[0-9a-f]{64}$'),
    CHECK (created_by_agent_id = owner_id)
);

CREATE TABLE memory_vectors (
    profile_id          TEXT        NOT NULL,
    memory_id           TEXT        NOT NULL,
    memory_version      INTEGER     NOT NULL,
    account_id          TEXT        NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    realm_id            TEXT        NOT NULL REFERENCES realms(id),
    owner_kind          TEXT        NOT NULL DEFAULT 'agent',
    owner_id            TEXT        NOT NULL REFERENCES agents(id),
    content_hash        TEXT        NOT NULL,
    vector              JSONB       NOT NULL,
    vector_hash         TEXT        NOT NULL,
    created_by_agent_id TEXT        NOT NULL REFERENCES agents(id),
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (profile_id, memory_id, memory_version),
    FOREIGN KEY (profile_id, account_id, realm_id, owner_kind, owner_id)
      REFERENCES memory_vector_profiles
        (id, account_id, realm_id, owner_kind, owner_id)
      ON DELETE CASCADE DEFERRABLE INITIALLY DEFERRED,
    FOREIGN KEY
      (memory_id, memory_version, account_id, realm_id, owner_kind, owner_id)
      REFERENCES memory_versions
        (memory_id, version, account_id, realm_id, owner_kind, owner_id)
      ON DELETE CASCADE DEFERRABLE INITIALLY DEFERRED,
    CHECK (owner_kind = 'agent'),
    CHECK (memory_version >= 1),
    CHECK (content_hash ~ '^[0-9a-f]{64}$'),
    CHECK (jsonb_typeof(vector) = 'array'),
    CHECK (jsonb_array_length(vector) BETWEEN 1 AND 4096),
    CHECK (octet_length(vector::text) <= 262144),
    CHECK (vector_hash ~ '^[0-9a-f]{64}$'),
    CHECK (created_by_agent_id = owner_id)
);

CREATE INDEX memory_vectors_profile_lookup
    ON memory_vectors
      (account_id, realm_id, owner_kind, owner_id, profile_id,
       memory_id, memory_version, created_at);

-- +goose Down
DROP TABLE IF EXISTS memory_vectors;
DROP TABLE IF EXISTS memory_vector_profiles;
