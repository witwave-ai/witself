-- +goose Up
-- Agent-owned sealed credentials. Sensitive values and their DEKs are
-- encrypted by the client before this schema sees them. The backend persists
-- only public metadata, non-sensitive field values, ciphertext, wrapped DEKs,
-- and public AVK identity; there is deliberately no server-held decrypt key.

-- These scoped keys let every sealed foreign key prove the complete
-- account -> realm -> agent relationship instead of relying on globally
-- unique ids plus application validation.
CREATE UNIQUE INDEX realms_account_id_id_unique
    ON realms (account_id, id);
CREATE UNIQUE INDEX agents_realm_id_id_unique
    ON agents (realm_id, id);

CREATE TABLE agent_vault_keys (
    id              TEXT        PRIMARY KEY,
    account_id      TEXT        NOT NULL,
    realm_id        TEXT        NOT NULL,
    owner_agent_id  TEXT        NOT NULL,
    key_version     BIGINT      NOT NULL,
    algorithm       TEXT        NOT NULL,
    fingerprint     TEXT        NOT NULL,
    lifecycle_state TEXT        NOT NULL DEFAULT 'current',
    row_version     BIGINT      NOT NULL DEFAULT 1,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    retired_at      TIMESTAMPTZ,
    UNIQUE (account_id, fingerprint),
    UNIQUE (account_id, realm_id, owner_agent_id, id, key_version),
    UNIQUE (account_id, realm_id, owner_agent_id, key_version),
    FOREIGN KEY (account_id, realm_id)
      REFERENCES realms (account_id, id) ON DELETE CASCADE,
    FOREIGN KEY (realm_id, owner_agent_id)
      REFERENCES agents (realm_id, id) ON DELETE CASCADE,
    CHECK (id ~ '^avk_[a-z2-7]{16}$'),
    CHECK (key_version >= 1),
    CHECK (algorithm = 'AES_256_GCM_RANDOM_NONCE_V1'),
    CHECK (fingerprint ~ '^[0-9a-f]{64}$'),
    CHECK (lifecycle_state IN ('pending', 'current', 'retired')),
    CHECK (row_version >= 1),
    CHECK ((lifecycle_state = 'retired' AND retired_at IS NOT NULL) OR
           (lifecycle_state <> 'retired' AND retired_at IS NULL))
);

CREATE UNIQUE INDEX agent_vault_keys_one_current
    ON agent_vault_keys (account_id, realm_id, owner_agent_id)
 WHERE lifecycle_state = 'current';

CREATE TABLE secrets (
    id             TEXT        PRIMARY KEY,
    account_id     TEXT        NOT NULL,
    realm_id       TEXT        NOT NULL,
    owner_agent_id TEXT        NOT NULL,
    name           TEXT        NOT NULL,
    description    TEXT        NOT NULL DEFAULT '',
    template       TEXT        NOT NULL DEFAULT 'generic',
    tags           JSONB       NOT NULL DEFAULT '[]'::jsonb,
    row_version    BIGINT      NOT NULL DEFAULT 1,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    archived_at    TIMESTAMPTZ,
    deleted_at     TIMESTAMPTZ,
    search_document TSVECTOR GENERATED ALWAYS AS (
      to_tsvector('simple', name || ' ' || description || ' ' ||
        template || ' ' || tags::text)
    ) STORED,
    UNIQUE (account_id, realm_id, owner_agent_id, id),
    FOREIGN KEY (account_id, realm_id)
      REFERENCES realms (account_id, id) ON DELETE CASCADE,
    FOREIGN KEY (realm_id, owner_agent_id)
      REFERENCES agents (realm_id, id) ON DELETE CASCADE,
    CHECK (id ~ '^sec_[a-z2-7]{16}$'),
    CHECK (octet_length(name) BETWEEN 1 AND 256),
    CHECK (octet_length(description) <= 4096),
    CHECK (template ~ '^[a-z][a-z0-9_.-]{0,127}$'),
    CHECK (jsonb_typeof(tags) = 'array' AND octet_length(tags::text) <= 8192),
    CHECK (row_version >= 1),
    CHECK (updated_at >= created_at),
    CHECK (archived_at IS NULL OR archived_at >= created_at),
    CHECK (deleted_at IS NULL OR deleted_at >= created_at)
);

CREATE UNIQUE INDEX secrets_one_live_name_per_agent
    ON secrets (account_id, realm_id, owner_agent_id, name COLLATE "C")
 WHERE archived_at IS NULL AND deleted_at IS NULL;
CREATE INDEX secrets_by_agent_updated
    ON secrets (account_id, realm_id, owner_agent_id, updated_at DESC, id DESC)
 WHERE deleted_at IS NULL;
CREATE INDEX secrets_search_document
    ON secrets USING GIN (search_document);

CREATE TABLE secret_fields (
    id              TEXT        PRIMARY KEY,
    account_id      TEXT        NOT NULL,
    realm_id        TEXT        NOT NULL,
    owner_agent_id  TEXT        NOT NULL,
    secret_id       TEXT        NOT NULL,
    name            TEXT        NOT NULL,
    field_kind      TEXT        NOT NULL DEFAULT 'text',
    sensitive       BOOLEAN     NOT NULL,
    value_encoding  TEXT        NOT NULL DEFAULT 'utf8',
    value_version   BIGINT      NOT NULL DEFAULT 1,
    public_value    TEXT,
    envelope_version BIGINT,
    ciphertext      BYTEA,
    aead_algorithm  TEXT,
    aad_version     BIGINT,
    dek_id          TEXT,
    dek_generation  BIGINT,
    row_version     BIGINT      NOT NULL DEFAULT 1,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    public_search_document TSVECTOR GENERATED ALWAYS AS (
      to_tsvector('simple', name || ' ' || field_kind || ' ' ||
        coalesce(public_value, ''))
    ) STORED,
    UNIQUE (account_id, realm_id, owner_agent_id, secret_id, id),
    UNIQUE (account_id, realm_id, owner_agent_id, secret_id, id,
            dek_id, dek_generation),
    UNIQUE (secret_id, name),
    FOREIGN KEY (account_id, realm_id, owner_agent_id, secret_id)
      REFERENCES secrets (account_id, realm_id, owner_agent_id, id)
      ON DELETE CASCADE,
    CHECK (id ~ '^fld_[a-z2-7]{16}$'),
    CHECK (name ~ '^[a-z][a-z0-9_.-]{0,127}$'),
    CHECK (field_kind IN
      ('text', 'username', 'password', 'url', 'api_key', 'token',
       'private_key', 'totp', 'recovery_code', 'note')),
    CHECK (value_encoding IN ('utf8', 'json', 'binary')),
    CHECK (value_version >= 1),
    CHECK (row_version >= 1),
    CHECK (updated_at >= created_at),
    CHECK (
      (NOT sensitive AND value_encoding = 'utf8' AND public_value IS NOT NULL AND
       envelope_version IS NULL AND ciphertext IS NULL AND
       aead_algorithm IS NULL AND aad_version IS NULL AND
       dek_id IS NULL AND dek_generation IS NULL) OR
      (sensitive AND public_value IS NULL AND envelope_version = 1 AND
       ciphertext IS NOT NULL AND
       octet_length(ciphertext) BETWEEN 29 AND 65564 AND
       aead_algorithm = 'AES_256_GCM_RANDOM_NONCE_V1' AND aad_version = 1 AND
       dek_id IS NOT NULL AND dek_id ~ '^dek_[a-z2-7]{16}$' AND
       dek_generation >= 1)
    ),
    CHECK (sensitive OR field_kind NOT IN
      ('password', 'api_key', 'token', 'private_key', 'totp', 'recovery_code')),
    CHECK (public_value IS NULL OR octet_length(public_value) <= 65536)
);

CREATE INDEX secret_fields_by_secret
    ON secret_fields
       (account_id, realm_id, owner_agent_id, secret_id, name, id);
CREATE INDEX secret_fields_public_search_document
    ON secret_fields USING GIN (public_search_document);

CREATE TABLE secret_deks (
    id                   TEXT        PRIMARY KEY,
    account_id           TEXT        NOT NULL,
    realm_id             TEXT        NOT NULL,
    owner_agent_id       TEXT        NOT NULL,
    secret_id            TEXT        NOT NULL,
    field_id             TEXT        NOT NULL,
    dek_generation       BIGINT      NOT NULL,
    wrapped_dek          BYTEA       NOT NULL,
    wrap_algorithm       TEXT        NOT NULL,
    aad_version          BIGINT      NOT NULL,
    wrap_revision        BIGINT      NOT NULL DEFAULT 1,
    wrapping_key_id      TEXT        NOT NULL,
    wrapping_key_version BIGINT      NOT NULL,
    row_version          BIGINT      NOT NULL DEFAULT 1,
    created_at           TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    retired_at           TIMESTAMPTZ,
    UNIQUE (account_id, realm_id, owner_agent_id, secret_id, field_id,
            id, dek_generation),
    UNIQUE (account_id, realm_id, owner_agent_id, secret_id, field_id,
            dek_generation),
    FOREIGN KEY (account_id, realm_id, owner_agent_id, secret_id, field_id)
      REFERENCES secret_fields
        (account_id, realm_id, owner_agent_id, secret_id, id)
      ON DELETE CASCADE DEFERRABLE INITIALLY DEFERRED,
    FOREIGN KEY (account_id, realm_id, owner_agent_id,
                 wrapping_key_id, wrapping_key_version)
      REFERENCES agent_vault_keys
        (account_id, realm_id, owner_agent_id, id, key_version),
    CHECK (id ~ '^dek_[a-z2-7]{16}$'),
    CHECK (dek_generation >= 1),
    CHECK (octet_length(wrapped_dek) = 60),
    CHECK (wrap_algorithm = 'AES_256_GCM_RANDOM_NONCE_V1'),
    CHECK (aad_version = 1),
    CHECK (wrap_revision >= 1),
    CHECK (wrapping_key_version >= 1),
    CHECK (row_version >= 1),
    CHECK (retired_at IS NULL OR retired_at >= created_at)
);

ALTER TABLE secret_fields
  ADD CONSTRAINT secret_fields_current_dek_fk
  FOREIGN KEY (account_id, realm_id, owner_agent_id, secret_id, id,
               dek_id, dek_generation)
  REFERENCES secret_deks
    (account_id, realm_id, owner_agent_id, secret_id, field_id,
     id, dek_generation)
  DEFERRABLE INITIALLY DEFERRED;

CREATE UNIQUE INDEX secret_deks_one_current_per_field
    ON secret_deks
       (account_id, realm_id, owner_agent_id, secret_id, field_id)
 WHERE retired_at IS NULL;

-- Receipts contain retry and request hashes plus value-free result coordinates.
-- They never duplicate public values, ciphertext, wrapped keys, or plaintext.
CREATE TABLE secret_mutation_receipts (
    account_id             TEXT        NOT NULL,
    realm_id               TEXT        NOT NULL,
    owner_agent_id         TEXT        NOT NULL,
    actor_kind             TEXT        NOT NULL,
    actor_id               TEXT        NOT NULL,
    operation              TEXT        NOT NULL,
    idempotency_key_hash   TEXT        NOT NULL,
    request_hash           TEXT        NOT NULL,
    target_kind            TEXT        NOT NULL,
    target_id              TEXT        NOT NULL,
    result_revision        BIGINT      NOT NULL,
    result_value_version   BIGINT,
    created_at             TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (account_id, realm_id, owner_agent_id, actor_kind, actor_id,
                 operation, idempotency_key_hash),
    FOREIGN KEY (account_id, realm_id)
      REFERENCES realms (account_id, id) ON DELETE CASCADE,
    FOREIGN KEY (realm_id, owner_agent_id)
      REFERENCES agents (realm_id, id) ON DELETE CASCADE,
    CHECK (actor_kind IN ('agent', 'operator')),
    CHECK (octet_length(actor_id) BETWEEN 1 AND 128),
    CHECK (operation IN
      ('key_register', 'secret_create', 'secret_update', 'secret_archive',
       'secret_restore', 'dek_rewrap', 'field_access')),
    CHECK (idempotency_key_hash ~ '^[0-9a-f]{64}$'),
    CHECK (request_hash ~ '^[0-9a-f]{64}$'),
    CHECK (target_kind IN ('key_epoch', 'secret', 'field', 'dek')),
    CHECK (target_id ~ '^(avk|sec|fld|dek)_[a-z2-7]{16}$'),
    CHECK (
      (operation = 'key_register' AND target_kind = 'key_epoch' AND target_id ~ '^avk_') OR
      (operation IN ('secret_create', 'secret_update', 'secret_archive', 'secret_restore') AND
       target_kind = 'secret' AND target_id ~ '^sec_') OR
      (operation = 'dek_rewrap' AND target_kind = 'dek' AND target_id ~ '^dek_') OR
      (operation = 'field_access' AND target_kind = 'field' AND target_id ~ '^fld_')
    ),
    CHECK (result_revision >= 1),
    CHECK (result_value_version IS NULL OR result_value_version >= 1),
    CHECK (operation IN ('secret_update', 'field_access') OR result_value_version IS NULL),
    CHECK (operation <> 'field_access' OR result_value_version IS NOT NULL)
);

-- +goose Down
DROP TABLE secret_mutation_receipts;
DROP INDEX secret_deks_one_current_per_field;
ALTER TABLE secret_fields DROP CONSTRAINT secret_fields_current_dek_fk;
DROP TABLE secret_deks;
DROP INDEX secret_fields_public_search_document;
DROP INDEX secret_fields_by_secret;
DROP TABLE secret_fields;
DROP INDEX secrets_search_document;
DROP INDEX secrets_by_agent_updated;
DROP INDEX secrets_one_live_name_per_agent;
DROP TABLE secrets;
DROP INDEX agent_vault_keys_one_current;
DROP TABLE agent_vault_keys;
DROP INDEX agents_realm_id_id_unique;
DROP INDEX realms_account_id_id_unique;
