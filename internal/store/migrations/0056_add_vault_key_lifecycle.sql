-- +goose Up
-- Multi-installation AVK enrollment and crash-resumable AVK rotation. The
-- enrollment relay stores only a recipient-bound, short-lived ciphertext; no
-- plaintext AVK or recipient private key crosses the client boundary. Rotation
-- stages replacement DEK wrappers away from the live secret_deks rows and
-- exposes them only in one atomic epoch flip.

-- Schema 55 made every historical logical key version unique. That means a
-- cancelled rotation would consume source+1 forever even though its pending
-- candidate never became an epoch. Preserve the retired candidate as history,
-- but allow a fresh id to retry that logical version. Pending/current rows
-- remain unique so there is never more than one live occupant of a version.
-- +goose StatementBegin
DO $$
DECLARE
    version_constraint TEXT;
BEGIN
    SELECT c.conname
      INTO version_constraint
      FROM pg_constraint c
     WHERE c.conrelid = 'agent_vault_keys'::regclass
       AND c.contype = 'u'
       AND pg_get_constraintdef(c.oid) =
           'UNIQUE (account_id, realm_id, owner_agent_id, key_version)';
    IF version_constraint IS NULL THEN
        RAISE EXCEPTION 'schema 55 vault key version constraint is missing';
    END IF;
    EXECUTE format('ALTER TABLE agent_vault_keys DROP CONSTRAINT %I',
                   version_constraint);
END $$;
-- +goose StatementEnd

-- Schema 55 permitted pending AVK rows but had no rotation aggregate capable
-- of owning or completing them. Retire every such orphan deterministically
-- before schema 56 introduces real pending epochs. Preserve row_version: this
-- is migration normalization, not a client-visible lifecycle mutation.
UPDATE agent_vault_keys
   SET lifecycle_state = 'retired',
       retired_at = created_at
 WHERE lifecycle_state = 'pending';

CREATE UNIQUE INDEX agent_vault_keys_one_live_version
    ON agent_vault_keys
       (account_id, realm_id, owner_agent_id, key_version)
 WHERE lifecycle_state IN ('pending', 'current');

CREATE TABLE agent_vault_key_enrollments (
    id                         TEXT        PRIMARY KEY,
    account_id                 TEXT        NOT NULL,
    realm_id                   TEXT        NOT NULL,
    owner_agent_id             TEXT        NOT NULL,
    vault_key_id               TEXT        NOT NULL,
    vault_key_version          BIGINT      NOT NULL,
    target_location_id         TEXT        NOT NULL,
    target_location_name       TEXT        NOT NULL DEFAULT '',
    target_public_key          TEXT        NOT NULL,
    target_key_algorithm       TEXT        NOT NULL,
    pairing_commitment         TEXT        NOT NULL,
    lifecycle_state            TEXT        NOT NULL DEFAULT 'pending',
    source_location_id         TEXT,
    source_ephemeral_public_key TEXT,
    transfer_ciphertext        BYTEA,
    transfer_algorithm         TEXT,
    consume_commitment         TEXT,
    row_version                BIGINT      NOT NULL DEFAULT 1,
    created_at                 TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    expires_at                 TIMESTAMPTZ NOT NULL,
    approved_at                TIMESTAMPTZ,
    consumed_at                TIMESTAMPTZ,
    cancelled_at               TIMESTAMPTZ,
    expired_at                 TIMESTAMPTZ,
    UNIQUE (account_id, realm_id, owner_agent_id, id),
    FOREIGN KEY (account_id, realm_id)
      REFERENCES realms (account_id, id) ON DELETE CASCADE,
    FOREIGN KEY (realm_id, owner_agent_id)
      REFERENCES agents (realm_id, id) ON DELETE CASCADE,
    FOREIGN KEY (account_id, realm_id, owner_agent_id,
                 vault_key_id, vault_key_version)
      REFERENCES agent_vault_keys
        (account_id, realm_id, owner_agent_id, id, key_version),
    CHECK (id ~ '^enr_[a-z2-7]{16}$'),
    CHECK (vault_key_version >= 1),
    CHECK (target_location_id ~ '^loc_[a-z2-7]{16}$'),
    CHECK (octet_length(target_location_name) <= 256),
    CHECK (target_public_key ~ '^[A-Za-z0-9_-]{43}$'),
    CHECK (target_key_algorithm = 'X25519_RAW_32_BASE64URL_V1'),
    CHECK (pairing_commitment ~ '^[0-9a-f]{64}$'),
    CHECK (lifecycle_state IN
      ('pending', 'approved', 'consumed', 'cancelled', 'expired')),
    CHECK (row_version >= 1),
    CHECK (expires_at > created_at),
    CHECK (source_location_id IS NULL OR
           source_location_id ~ '^loc_[a-z2-7]{16}$'),
    CHECK (source_ephemeral_public_key IS NULL OR
           source_ephemeral_public_key ~ '^[A-Za-z0-9_-]{43}$'),
    CHECK (transfer_ciphertext IS NULL OR
           octet_length(transfer_ciphertext) BETWEEN 64 AND 4096),
    CHECK (transfer_algorithm IS NULL OR
           transfer_algorithm = 'X25519_HKDF_SHA256_AES_256_GCM_V1'),
    CHECK (consume_commitment IS NULL OR
           consume_commitment ~ '^[0-9a-f]{64}$'),
    CHECK (
      (lifecycle_state = 'pending' AND
       source_location_id IS NULL AND source_ephemeral_public_key IS NULL AND
       transfer_ciphertext IS NULL AND transfer_algorithm IS NULL AND
       consume_commitment IS NULL AND approved_at IS NULL AND
       consumed_at IS NULL AND cancelled_at IS NULL AND expired_at IS NULL) OR
      (lifecycle_state = 'approved' AND
       source_location_id IS NOT NULL AND source_ephemeral_public_key IS NOT NULL AND
       transfer_ciphertext IS NOT NULL AND transfer_algorithm IS NOT NULL AND
       consume_commitment IS NOT NULL AND approved_at IS NOT NULL AND
       consumed_at IS NULL AND cancelled_at IS NULL AND expired_at IS NULL) OR
      (lifecycle_state = 'consumed' AND
       source_location_id IS NOT NULL AND source_ephemeral_public_key IS NULL AND
       transfer_ciphertext IS NULL AND transfer_algorithm IS NULL AND
       consume_commitment IS NULL AND approved_at IS NOT NULL AND
       consumed_at IS NOT NULL AND cancelled_at IS NULL AND expired_at IS NULL) OR
      (lifecycle_state = 'cancelled' AND
       source_ephemeral_public_key IS NULL AND transfer_ciphertext IS NULL AND
       transfer_algorithm IS NULL AND consume_commitment IS NULL AND
       consumed_at IS NULL AND cancelled_at IS NOT NULL AND expired_at IS NULL) OR
      (lifecycle_state = 'expired' AND
       source_ephemeral_public_key IS NULL AND transfer_ciphertext IS NULL AND
       transfer_algorithm IS NULL AND consume_commitment IS NULL AND
       consumed_at IS NULL AND cancelled_at IS NULL AND expired_at IS NOT NULL)
    )
);

CREATE INDEX agent_vault_key_enrollments_active
    ON agent_vault_key_enrollments
       (account_id, realm_id, owner_agent_id, expires_at, created_at, id)
 WHERE lifecycle_state IN ('pending', 'approved');

CREATE TABLE vault_key_enrollment_receipts (
    account_id             TEXT        NOT NULL,
    realm_id               TEXT        NOT NULL,
    owner_agent_id         TEXT        NOT NULL,
    operation              TEXT        NOT NULL,
    idempotency_key_hash   TEXT        NOT NULL,
    request_hash           TEXT        NOT NULL,
    enrollment_id          TEXT        NOT NULL,
    result_revision        BIGINT      NOT NULL,
    created_at             TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (account_id, realm_id, owner_agent_id,
                 operation, idempotency_key_hash),
    FOREIGN KEY (account_id, realm_id)
      REFERENCES realms (account_id, id) ON DELETE CASCADE,
    FOREIGN KEY (realm_id, owner_agent_id)
      REFERENCES agents (realm_id, id) ON DELETE CASCADE,
    CONSTRAINT vault_key_enrollment_receipts_parent_fk
      FOREIGN KEY (account_id, realm_id, owner_agent_id, enrollment_id)
      REFERENCES agent_vault_key_enrollments
        (account_id, realm_id, owner_agent_id, id) ON DELETE CASCADE,
    CHECK (operation IN
      ('enrollment_request', 'enrollment_approve',
       'enrollment_consume', 'enrollment_cancel')),
    CHECK (idempotency_key_hash ~ '^[0-9a-f]{64}$'),
    CHECK (request_hash ~ '^[0-9a-f]{64}$'),
    CHECK (enrollment_id ~ '^enr_[a-z2-7]{16}$'),
    CHECK (result_revision >= 1)
);

CREATE TABLE agent_vault_key_rotations (
    id                  TEXT        PRIMARY KEY,
    account_id          TEXT        NOT NULL,
    realm_id            TEXT        NOT NULL,
    owner_agent_id      TEXT        NOT NULL,
    source_key_id       TEXT        NOT NULL,
    source_key_version  BIGINT      NOT NULL,
    target_key_id       TEXT        NOT NULL,
    target_key_version  BIGINT      NOT NULL,
    lifecycle_state     TEXT        NOT NULL DEFAULT 'open',
    recovery_disposition_mode TEXT,
    recovery_artifact_sha256 TEXT,
    item_count          BIGINT      NOT NULL,
    staged_count        BIGINT      NOT NULL DEFAULT 0,
    row_version         BIGINT      NOT NULL DEFAULT 1,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    committed_at        TIMESTAMPTZ,
    cancelled_at        TIMESTAMPTZ,
    UNIQUE (account_id, realm_id, owner_agent_id, id),
    FOREIGN KEY (account_id, realm_id)
      REFERENCES realms (account_id, id) ON DELETE CASCADE,
    FOREIGN KEY (realm_id, owner_agent_id)
      REFERENCES agents (realm_id, id) ON DELETE CASCADE,
    FOREIGN KEY (account_id, realm_id, owner_agent_id,
                 source_key_id, source_key_version)
      REFERENCES agent_vault_keys
        (account_id, realm_id, owner_agent_id, id, key_version),
    FOREIGN KEY (account_id, realm_id, owner_agent_id,
                 target_key_id, target_key_version)
      REFERENCES agent_vault_keys
        (account_id, realm_id, owner_agent_id, id, key_version),
    CHECK (id ~ '^vkr_[a-z2-7]{16}$'),
    CHECK (source_key_version >= 1 AND target_key_version > source_key_version),
    CHECK (source_key_id <> target_key_id),
    CHECK (lifecycle_state IN ('open', 'committed', 'cancelled')),
    CHECK (
      (lifecycle_state <> 'committed' AND
       recovery_disposition_mode IS NULL AND recovery_artifact_sha256 IS NULL) OR
      (lifecycle_state = 'committed' AND recovery_disposition_mode = 'risk_accepted' AND
       recovery_artifact_sha256 IS NULL) OR
      (lifecycle_state = 'committed' AND recovery_disposition_mode = 'recovery_artifact' AND
       recovery_artifact_sha256 ~ '^[0-9a-f]{64}$')
    ),
    CHECK (item_count >= 0 AND staged_count >= 0 AND staged_count <= item_count),
    CHECK (row_version >= 1),
    CHECK (updated_at >= created_at),
    CHECK (
      (lifecycle_state = 'open' AND committed_at IS NULL AND cancelled_at IS NULL) OR
      (lifecycle_state = 'committed' AND committed_at IS NOT NULL AND cancelled_at IS NULL) OR
      (lifecycle_state = 'cancelled' AND committed_at IS NULL AND cancelled_at IS NOT NULL)
    )
);

CREATE UNIQUE INDEX agent_vault_key_rotations_one_open
    ON agent_vault_key_rotations (account_id, realm_id, owner_agent_id)
 WHERE lifecycle_state = 'open';

CREATE TABLE agent_vault_key_rotation_items (
    rotation_id                TEXT        NOT NULL,
    account_id                 TEXT        NOT NULL,
    realm_id                   TEXT        NOT NULL,
    owner_agent_id             TEXT        NOT NULL,
    secret_id                  TEXT        NOT NULL,
    field_id                   TEXT        NOT NULL,
    dek_id                     TEXT        NOT NULL,
    dek_generation             BIGINT      NOT NULL,
    source_dek_row_version     BIGINT      NOT NULL,
    source_wrap_revision       BIGINT      NOT NULL,
    source_wrapped_dek         BYTEA       NOT NULL,
    source_wrap_algorithm      TEXT        NOT NULL,
    source_aad_version         BIGINT      NOT NULL,
    source_wrapping_key_id     TEXT        NOT NULL,
    source_wrapping_key_version BIGINT     NOT NULL,
    target_wrapped_dek         BYTEA,
    target_wrap_revision       BIGINT,
    target_wrapper_sha256      TEXT,
    staged_at                  TIMESTAMPTZ,
    PRIMARY KEY (rotation_id, dek_id),
    FOREIGN KEY (account_id, realm_id, owner_agent_id, rotation_id)
      REFERENCES agent_vault_key_rotations
        (account_id, realm_id, owner_agent_id, id) ON DELETE CASCADE,
    FOREIGN KEY (account_id, realm_id, owner_agent_id, secret_id, field_id,
                 dek_id, dek_generation)
      REFERENCES secret_deks
        (account_id, realm_id, owner_agent_id, secret_id, field_id,
         id, dek_generation),
    CHECK (dek_generation >= 1),
    CHECK (source_dek_row_version >= 1),
    CHECK (source_wrap_revision >= 1),
    CHECK (octet_length(source_wrapped_dek) = 60),
    CHECK (source_wrap_algorithm = 'AES_256_GCM_RANDOM_NONCE_V1'),
    CHECK (source_aad_version = 1),
    CHECK (source_wrapping_key_version >= 1),
    CHECK (
      (target_wrapped_dek IS NULL AND target_wrap_revision IS NULL AND
       target_wrapper_sha256 IS NULL AND staged_at IS NULL) OR
      (octet_length(target_wrapped_dek) = 60 AND
       target_wrap_revision > source_wrap_revision AND
       target_wrapper_sha256 ~ '^[0-9a-f]{64}$' AND staged_at IS NOT NULL)
    )
);

CREATE INDEX agent_vault_key_rotation_items_page
    ON agent_vault_key_rotation_items (rotation_id, dek_id);

CREATE TABLE vault_key_rotation_receipts (
    account_id             TEXT        NOT NULL,
    realm_id               TEXT        NOT NULL,
    owner_agent_id         TEXT        NOT NULL,
    operation              TEXT        NOT NULL,
    idempotency_key_hash   TEXT        NOT NULL,
    request_hash           TEXT        NOT NULL,
    rotation_id            TEXT        NOT NULL,
    result_revision        BIGINT      NOT NULL,
    created_at             TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (account_id, realm_id, owner_agent_id,
                 operation, idempotency_key_hash),
    FOREIGN KEY (account_id, realm_id)
      REFERENCES realms (account_id, id) ON DELETE CASCADE,
    FOREIGN KEY (realm_id, owner_agent_id)
      REFERENCES agents (realm_id, id) ON DELETE CASCADE,
    CONSTRAINT vault_key_rotation_receipts_parent_fk
      FOREIGN KEY (account_id, realm_id, owner_agent_id, rotation_id)
      REFERENCES agent_vault_key_rotations
        (account_id, realm_id, owner_agent_id, id) ON DELETE CASCADE,
    CHECK (operation IN
      ('rotation_start', 'rotation_stage',
       'rotation_commit', 'rotation_cancel')),
    CHECK (idempotency_key_hash ~ '^[0-9a-f]{64}$'),
    CHECK (request_hash ~ '^[0-9a-f]{64}$'),
    CHECK (rotation_id ~ '^vkr_[a-z2-7]{16}$'),
    CHECK (result_revision >= 1)
);

-- +goose Down
-- Schema 55 cannot represent an in-flight key lifecycle or more than one
-- historical row for a logical key version. Lock the complete downgrade
-- surface before checking it so a concurrent client cannot create work after
-- the preflight and have that work silently discarded by the table drops.
LOCK TABLE agent_vault_key_enrollments,
           agent_vault_key_rotations,
           agent_vault_keys,
           secret_deks
  IN ACCESS EXCLUSIVE MODE;

-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (
        SELECT 1
          FROM agent_vault_key_enrollments
         WHERE lifecycle_state IN ('pending', 'approved')
    ) OR EXISTS (
        SELECT 1
          FROM agent_vault_key_rotations
         WHERE lifecycle_state = 'open'
    ) THEN
        RAISE EXCEPTION USING
            ERRCODE = '55000',
            MESSAGE = 'cannot downgrade schema 56 with active vault key enrollment or rotation';
    END IF;

    -- A pending epoch must belong to the one open rotation. The open-rotation
    -- check above handles valid state; this separate guard catches imported or
    -- manually damaged state before any lifecycle table is dropped.
    IF EXISTS (
        SELECT 1
          FROM agent_vault_keys
         WHERE lifecycle_state = 'pending'
    ) THEN
        RAISE EXCEPTION USING
            ERRCODE = '55000',
            MESSAGE = 'cannot downgrade schema 56 with an orphan pending vault key epoch';
    END IF;
END $$;
-- +goose StatementEnd

DROP TABLE vault_key_rotation_receipts;
DROP INDEX agent_vault_key_rotation_items_page;
DROP TABLE agent_vault_key_rotation_items;
DROP INDEX agent_vault_key_rotations_one_open;
DROP TABLE agent_vault_key_rotations;
DROP TABLE vault_key_enrollment_receipts;
DROP INDEX agent_vault_key_enrollments_active;
DROP TABLE agent_vault_key_enrollments;

-- A cancelled rotation may leave a retired source+1 candidate, and a retry may
-- create another row for that same logical version. Schema 55 has no lifecycle
-- history capable of distinguishing those candidates. Preserve the current
-- epoch first, otherwise an epoch still referenced by a DEK, otherwise the
-- newest deterministic retired row. Refuse the downgrade rather than deleting
-- any losing row that is still referenced by a DEK.
-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (
        WITH ranked AS (
            SELECT k.account_id,
                   k.realm_id,
                   k.owner_agent_id,
                   k.id,
                   k.key_version,
                   row_number() OVER (
                       PARTITION BY k.account_id, k.realm_id,
                                    k.owner_agent_id, k.key_version
                       ORDER BY
                           CASE WHEN k.lifecycle_state = 'current' THEN 0 ELSE 1 END,
                           CASE WHEN EXISTS (
                               SELECT 1
                                 FROM secret_deks d
                                WHERE d.account_id = k.account_id
                                  AND d.realm_id = k.realm_id
                                  AND d.owner_agent_id = k.owner_agent_id
                                  AND d.wrapping_key_id = k.id
                                  AND d.wrapping_key_version = k.key_version
                           ) THEN 0 ELSE 1 END,
                           k.created_at DESC,
                           k.id DESC
                   ) AS keep_rank
              FROM agent_vault_keys k
        )
        SELECT 1
          FROM ranked r
          JOIN secret_deks d
            ON d.account_id = r.account_id
           AND d.realm_id = r.realm_id
           AND d.owner_agent_id = r.owner_agent_id
           AND d.wrapping_key_id = r.id
           AND d.wrapping_key_version = r.key_version
         WHERE r.keep_rank > 1
    ) THEN
        RAISE EXCEPTION USING
            ERRCODE = '55000',
            MESSAGE = 'cannot downgrade schema 56 because a duplicate retired vault key epoch is still referenced';
    END IF;
END $$;
-- +goose StatementEnd

WITH ranked AS (
    SELECT k.account_id,
           k.realm_id,
           k.owner_agent_id,
           k.id,
           k.key_version,
           k.lifecycle_state,
           row_number() OVER (
               PARTITION BY k.account_id, k.realm_id,
                            k.owner_agent_id, k.key_version
               ORDER BY
                   CASE WHEN k.lifecycle_state = 'current' THEN 0 ELSE 1 END,
                   CASE WHEN EXISTS (
                       SELECT 1
                         FROM secret_deks d
                        WHERE d.account_id = k.account_id
                          AND d.realm_id = k.realm_id
                          AND d.owner_agent_id = k.owner_agent_id
                          AND d.wrapping_key_id = k.id
                          AND d.wrapping_key_version = k.key_version
                   ) THEN 0 ELSE 1 END,
                   k.created_at DESC,
                   k.id DESC
           ) AS keep_rank
      FROM agent_vault_keys k
)
DELETE FROM agent_vault_keys k
 USING ranked r
 WHERE r.keep_rank > 1
   AND r.lifecycle_state = 'retired'
   AND k.account_id = r.account_id
   AND k.realm_id = r.realm_id
   AND k.owner_agent_id = r.owner_agent_id
   AND k.id = r.id
   AND k.key_version = r.key_version;

DROP INDEX agent_vault_keys_one_live_version;
ALTER TABLE agent_vault_keys
    ADD CONSTRAINT agent_vault_keys_scope_version_unique
    UNIQUE (account_id, realm_id, owner_agent_id, key_version);
