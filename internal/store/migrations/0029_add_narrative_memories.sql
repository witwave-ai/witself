-- +goose Up
-- Narrative memory keeps a stable resource identity while every mutation
-- appends a complete immutable version. Client runtimes provide all semantic
-- judgment; this schema only enforces deterministic persistence boundaries.
CREATE TABLE memory_change_clocks (
    account_id      TEXT        NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    realm_id        TEXT        NOT NULL REFERENCES realms(id),
    owner_kind      TEXT        NOT NULL DEFAULT 'agent',
    owner_id        TEXT        NOT NULL REFERENCES agents(id),
    last_change_seq BIGINT      NOT NULL DEFAULT 0,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (account_id, realm_id, owner_kind, owner_id),
    CHECK (owner_kind = 'agent'),
    CHECK (last_change_seq BETWEEN 0 AND 4611686018427387903)
);

CREATE TABLE memories (
    id                         TEXT        PRIMARY KEY,
    account_id                 TEXT        NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    realm_id                   TEXT        NOT NULL REFERENCES realms(id),
    owner_kind                 TEXT        NOT NULL DEFAULT 'agent',
    owner_id                   TEXT        NOT NULL REFERENCES agents(id),
    origin                     TEXT        NOT NULL,
    capture_reason             TEXT        NOT NULL DEFAULT 'manual',
    authored_by_agent_id       TEXT        NOT NULL REFERENCES agents(id),
    current_version            INTEGER,
    permanently_deleted_at     TIMESTAMPTZ,
    permanently_deleted_by_id  TEXT        REFERENCES agents(id),
    permanent_delete_reason    TEXT,
    delete_receipt_id          TEXT        NOT NULL DEFAULT '',
    delete_idempotency_key_hash TEXT       NOT NULL DEFAULT '',
    deleted_prior_version      INTEGER     NOT NULL DEFAULT 0,
    deleted_scrub_set_revision TEXT        NOT NULL DEFAULT '',
    deleted_version_count      BIGINT      NOT NULL DEFAULT 0,
    deleted_evidence_count     BIGINT      NOT NULL DEFAULT 0,
    deleted_relation_count     BIGINT      NOT NULL DEFAULT 0,
    deleted_retry_shield_count BIGINT      NOT NULL DEFAULT 0,
    deleted_retry_shield_digest TEXT       NOT NULL DEFAULT '',
    created_at                 TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at                 TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (id, account_id, realm_id, owner_kind, owner_id),
    CHECK (id ~ '^mem_[a-z2-7]{16}$'),
    CHECK (owner_kind = 'agent'),
    CHECK (origin ~ '^[a-z][a-z0-9_.-]{0,63}$'),
    CHECK (capture_reason ~ '^[a-z][a-z0-9_.-]{0,63}$'),
    CHECK (current_version IS NULL OR current_version >= 1),
    CHECK (permanent_delete_reason IS NULL OR
           permanent_delete_reason ~ '^[a-z][a-z0-9_.-]{0,63}$'),
    CHECK (
      (current_version IS NOT NULL AND permanently_deleted_at IS NULL AND
       permanently_deleted_by_id IS NULL AND permanent_delete_reason IS NULL AND
       delete_receipt_id = '' AND delete_idempotency_key_hash = '' AND
       deleted_prior_version = 0 AND deleted_scrub_set_revision = '' AND
       deleted_version_count = 0 AND deleted_evidence_count = 0 AND
       deleted_relation_count = 0 AND deleted_retry_shield_count = 0 AND
       deleted_retry_shield_digest = '')
      OR
      (current_version IS NULL AND permanently_deleted_at IS NOT NULL AND
       permanently_deleted_by_id IS NOT NULL AND permanent_delete_reason IS NOT NULL AND
       delete_receipt_id ~ '^mdel_[a-z2-7]{16}$' AND
       delete_idempotency_key_hash ~ '^[0-9a-f]{64}$' AND
       deleted_prior_version >= 1 AND
       deleted_scrub_set_revision ~ '^[0-9a-f]{64}$' AND
       deleted_version_count = deleted_prior_version AND
       deleted_evidence_count >= 1 AND deleted_relation_count >= 0 AND
       deleted_retry_shield_count >= deleted_version_count AND
       deleted_retry_shield_digest ~ '^[0-9a-f]{64}$')
    )
);

CREATE UNIQUE INDEX memories_by_owner_delete_idempotency
    ON memories (account_id, realm_id, owner_kind, owner_id,
                 delete_idempotency_key_hash)
    WHERE delete_idempotency_key_hash <> '';
CREATE UNIQUE INDEX memories_delete_receipt_id
    ON memories (delete_receipt_id)
    WHERE delete_receipt_id <> '';

CREATE TABLE memory_versions (
    memory_id              TEXT        NOT NULL,
    version                INTEGER     NOT NULL,
    account_id             TEXT        NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    realm_id               TEXT        NOT NULL REFERENCES realms(id),
    owner_kind             TEXT        NOT NULL DEFAULT 'agent',
    owner_id               TEXT        NOT NULL REFERENCES agents(id),
    previous_version       INTEGER,
    change_seq             BIGINT      NOT NULL,
    content                TEXT        NOT NULL,
    content_encoding       TEXT        NOT NULL DEFAULT 'plain',
    kind                   TEXT        NOT NULL,
    tags                   JSONB       NOT NULL DEFAULT '[]'::jsonb,
    links                  JSONB       NOT NULL DEFAULT '[]'::jsonb,
    salience               DOUBLE PRECISION NOT NULL DEFAULT 0.5,
    sensitive              BOOLEAN     NOT NULL DEFAULT FALSE,
    occurred_from          TIMESTAMPTZ,
    occurred_until         TIMESTAMPTZ,
    state                  TEXT        NOT NULL DEFAULT 'active',
    prior_state            TEXT,
    lifecycle_reason       TEXT        NOT NULL DEFAULT '',
    content_hash           TEXT        NOT NULL,
    actor_kind             TEXT        NOT NULL DEFAULT 'agent',
    actor_id               TEXT        NOT NULL REFERENCES agents(id),
    operation              TEXT        NOT NULL,
    idempotency_key        TEXT        NOT NULL,
    request_hash           TEXT        NOT NULL,
    client_runtime         TEXT        NOT NULL DEFAULT '',
    client_model           TEXT        NOT NULL DEFAULT '',
    client_recipe          TEXT        NOT NULL DEFAULT '',
    client_recipe_version  TEXT        NOT NULL DEFAULT '',
    -- A superseded source version commits to the complete replacement set.
    -- Relations may later be removed by authorized permanent deletion, so
    -- replay must verify them against this immutable value-free receipt.
    supersession_set_id              TEXT        NOT NULL DEFAULT '',
    supersession_set_revision        INTEGER     NOT NULL DEFAULT 0,
    supersession_replacement_count   INTEGER     NOT NULL DEFAULT 0,
    supersession_replacement_digest  TEXT        NOT NULL DEFAULT '',
    curation_run_id        TEXT,
    curation_action_id     TEXT,
    search_document        TSVECTOR GENERATED ALWAYS AS
                             (to_tsvector('simple'::regconfig, content)) STORED,
    created_at             TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (memory_id, version),
    UNIQUE (memory_id, version, account_id, realm_id, owner_kind, owner_id),
    UNIQUE (account_id, realm_id, owner_kind, owner_id, change_seq),
    UNIQUE (account_id, realm_id, owner_kind, owner_id, idempotency_key),
    FOREIGN KEY (memory_id, account_id, realm_id, owner_kind, owner_id)
      REFERENCES memories (id, account_id, realm_id, owner_kind, owner_id)
      ON DELETE CASCADE DEFERRABLE INITIALLY DEFERRED,
    FOREIGN KEY (memory_id, previous_version)
      REFERENCES memory_versions (memory_id, version)
      DEFERRABLE INITIALLY DEFERRED,
    CHECK (version >= 1),
    CHECK (owner_kind = 'agent'),
    CHECK (previous_version IS NULL OR
           (previous_version >= 1 AND previous_version < version)),
    CHECK (change_seq >= 1),
    CHECK (octet_length(content) BETWEEN 1 AND 262144),
    CHECK (content_encoding IN ('plain', 'base64')),
    CHECK (kind ~ '^[a-z][a-z0-9_.-]{0,63}$'),
    CHECK (jsonb_typeof(tags) = 'array' AND octet_length(tags::text) <= 16384),
    CHECK (jsonb_typeof(links) = 'array' AND octet_length(links::text) <= 32768),
    CHECK (salience >= 0 AND salience <= 1),
    CHECK (occurred_until IS NULL OR occurred_from IS NULL OR
           occurred_until >= occurred_from),
    CHECK (state IN ('active', 'superseded', 'forgotten')),
    CHECK ((state = 'forgotten' AND prior_state IN
             ('active', 'superseded')) OR
           (state <> 'forgotten' AND prior_state IS NULL)),
    CHECK (octet_length(lifecycle_reason) <= 2048),
    CHECK (content_hash ~ '^[0-9a-f]{64}$'),
    CHECK (actor_kind = 'agent'),
    CHECK (operation IN
      ('added', 'adjusted', 'superseded', 'forgotten', 'restored',
       'reactivated')),
    CHECK (octet_length(idempotency_key) BETWEEN 1 AND 512),
    CHECK (request_hash ~ '^[0-9a-f]{64}$'),
    CHECK (octet_length(client_runtime) <= 128),
    CHECK (octet_length(client_model) <= 256),
    CHECK (octet_length(client_recipe) <= 128),
    CHECK (octet_length(client_recipe_version) <= 128),
    CHECK (
      (operation = 'superseded' AND state = 'superseded' AND
       supersession_set_id ~ '^mset_[a-z2-7]{16}$' AND
       supersession_set_revision >= 1 AND
       supersession_replacement_count BETWEEN 1 AND 32 AND
       supersession_replacement_digest ~ '^[0-9a-f]{64}$')
      OR
      (operation <> 'superseded' AND supersession_set_id = '' AND
       supersession_set_revision = 0 AND
       supersession_replacement_count = 0 AND
       supersession_replacement_digest = '')
    )
);

-- This deferrable pointer closes the head/version cycle. Store mutations insert
-- the version and move the head in one transaction, so no inconsistent live
-- materialized head can commit.
ALTER TABLE memories
  ADD CONSTRAINT memories_current_version_fk
  FOREIGN KEY (id, current_version, account_id, realm_id, owner_kind, owner_id)
  REFERENCES memory_versions
    (memory_id, version, account_id, realm_id, owner_kind, owner_id)
  DEFERRABLE INITIALLY DEFERRED;

-- Redundant scoped uniqueness on globally unique source ids lets evidence use
-- composite foreign keys that prove account/realm ownership in the database,
-- not only in the store authorization check.
CREATE UNIQUE INDEX transcript_conversations_memory_evidence_scope
    ON transcript_conversations (id, account_id, realm_id, owner_agent_id);
CREATE UNIQUE INDEX agent_messages_memory_evidence_scope
    ON agent_messages (id, account_id, realm_id);

CREATE TABLE memory_evidence (
    id                         TEXT        PRIMARY KEY,
    account_id                 TEXT        NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    realm_id                   TEXT        NOT NULL REFERENCES realms(id),
    owner_kind                 TEXT        NOT NULL DEFAULT 'agent',
    owner_id                   TEXT        NOT NULL REFERENCES agents(id),
    memory_id                  TEXT        NOT NULL,
    target_version             INTEGER     NOT NULL,
    evidence_change_seq        BIGINT      NOT NULL,
    evidence_type              TEXT        NOT NULL,
    role                       TEXT        NOT NULL DEFAULT 'supports',
    resolution_state           TEXT        NOT NULL,
    external_locator           TEXT,
    pending_evidence_id        TEXT,
    resolved_kind              TEXT,
    source_transcript_id       TEXT,
    source_sequence_from       BIGINT,
    source_sequence_until      BIGINT,
    source_memory_id           TEXT,
    source_memory_version      INTEGER,
    source_message_id          TEXT,
    source_import_locator      TEXT,
    artifact_excerpt           BYTEA,
    artifact_sensitive         BOOLEAN     NOT NULL DEFAULT TRUE,
    terminal_reason_code       TEXT,
    source_digest              TEXT,
    actor_id                   TEXT        NOT NULL REFERENCES agents(id),
    idempotency_key            TEXT,
    request_hash               TEXT,
    created_at                 TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (id, memory_id, target_version),
    UNIQUE (account_id, realm_id, owner_kind, owner_id, evidence_change_seq),
    FOREIGN KEY
      (memory_id, target_version, account_id, realm_id, owner_kind, owner_id)
      REFERENCES memory_versions
        (memory_id, version, account_id, realm_id, owner_kind, owner_id)
      ON DELETE CASCADE DEFERRABLE INITIALLY DEFERRED,
    FOREIGN KEY (pending_evidence_id, memory_id, target_version)
      REFERENCES memory_evidence (id, memory_id, target_version)
      DEFERRABLE INITIALLY DEFERRED,
    FOREIGN KEY (source_transcript_id, account_id, realm_id, owner_id)
      REFERENCES transcript_conversations
        (id, account_id, realm_id, owner_agent_id)
      DEFERRABLE INITIALLY DEFERRED,
    FOREIGN KEY (source_transcript_id, source_sequence_from)
      REFERENCES transcript_entries (transcript_id, sequence)
      DEFERRABLE INITIALLY DEFERRED,
    FOREIGN KEY (source_transcript_id, source_sequence_until)
      REFERENCES transcript_entries (transcript_id, sequence)
      DEFERRABLE INITIALLY DEFERRED,
    FOREIGN KEY
      (source_memory_id, source_memory_version, account_id, realm_id,
       owner_kind, owner_id)
      REFERENCES memory_versions
        (memory_id, version, account_id, realm_id, owner_kind, owner_id)
      ON DELETE CASCADE DEFERRABLE INITIALLY DEFERRED,
    FOREIGN KEY (source_message_id, account_id, realm_id)
      REFERENCES agent_messages (id, account_id, realm_id)
      DEFERRABLE INITIALLY DEFERRED,
    CHECK (id LIKE 'mev\_%' ESCAPE '\'),
    CHECK (owner_kind = 'agent'),
    CHECK (target_version >= 1),
    CHECK (evidence_change_seq >= 1),
    CHECK (evidence_type ~ '^[a-z][a-z0-9_.-]{0,63}$'),
    CHECK (role IN ('supports', 'contradicts', 'context')),
    CHECK (resolution_state IN
      ('pending', 'resolved', 'unresolvable', 'unavailable')),
    CHECK (external_locator IS NULL OR octet_length(external_locator) <= 2048),
    CHECK (resolved_kind IS NULL OR resolved_kind IN
      ('transcript', 'memory', 'message', 'import_artifact', 'artifact')),
    CHECK (source_sequence_from IS NULL OR source_sequence_from >= 1),
    CHECK (source_sequence_until IS NULL OR source_sequence_until >= 1),
    CHECK (source_sequence_until IS NULL OR source_sequence_from IS NULL OR
           source_sequence_until >= source_sequence_from),
    CHECK (source_import_locator IS NULL OR
           octet_length(source_import_locator) <= 2048),
    CHECK (artifact_excerpt IS NULL OR octet_length(artifact_excerpt) <= 65536),
    CHECK (terminal_reason_code IS NULL OR
           octet_length(terminal_reason_code) BETWEEN 1 AND 128),
    CHECK (source_digest IS NULL OR source_digest ~ '^[0-9a-f]{64}$'),
    CHECK ((idempotency_key IS NULL AND request_hash IS NULL) OR
           (idempotency_key IS NOT NULL AND request_hash IS NOT NULL AND
            octet_length(idempotency_key) BETWEEN 1 AND 512 AND
            request_hash ~ '^[0-9a-f]{64}$')),
    CHECK (
      (resolution_state = 'pending' AND external_locator IS NOT NULL AND
       pending_evidence_id IS NULL AND resolved_kind IS NULL AND
       source_transcript_id IS NULL AND source_sequence_from IS NULL AND
       source_sequence_until IS NULL AND source_memory_id IS NULL AND
       source_memory_version IS NULL AND source_message_id IS NULL AND
       source_import_locator IS NULL AND artifact_excerpt IS NULL AND
       terminal_reason_code IS NULL)
      OR
      (resolution_state = 'unresolvable' AND external_locator IS NULL AND
       pending_evidence_id IS NOT NULL AND resolved_kind IS NULL AND
       source_transcript_id IS NULL AND source_sequence_from IS NULL AND
       source_sequence_until IS NULL AND source_memory_id IS NULL AND
       source_memory_version IS NULL AND source_message_id IS NULL AND
       source_import_locator IS NULL AND artifact_excerpt IS NULL AND
       terminal_reason_code IS NOT NULL)
      OR
      (resolution_state = 'unavailable' AND external_locator IS NULL AND
       pending_evidence_id IS NULL AND resolved_kind IS NULL AND
       source_transcript_id IS NULL AND source_sequence_from IS NULL AND
       source_sequence_until IS NULL AND source_memory_id IS NULL AND
       source_memory_version IS NULL AND source_message_id IS NULL AND
       source_import_locator IS NULL AND artifact_excerpt IS NULL AND
       terminal_reason_code IS NOT NULL)
      OR
      (resolution_state = 'resolved' AND external_locator IS NULL AND
       resolved_kind IS NOT NULL AND terminal_reason_code IS NULL AND
       (
         (resolved_kind = 'transcript' AND source_transcript_id IS NOT NULL AND
          source_sequence_from IS NOT NULL AND source_sequence_until IS NOT NULL AND
          source_memory_id IS NULL AND source_memory_version IS NULL AND
          source_message_id IS NULL AND source_import_locator IS NULL AND
          artifact_excerpt IS NULL)
         OR
         (resolved_kind = 'memory' AND source_transcript_id IS NULL AND
          source_sequence_from IS NULL AND source_sequence_until IS NULL AND
          source_memory_id IS NOT NULL AND source_memory_version IS NOT NULL AND
          source_message_id IS NULL AND source_import_locator IS NULL AND
          artifact_excerpt IS NULL)
         OR
         (resolved_kind = 'message' AND source_transcript_id IS NULL AND
          source_sequence_from IS NULL AND source_sequence_until IS NULL AND
          source_memory_id IS NULL AND source_memory_version IS NULL AND
          source_message_id IS NOT NULL AND source_import_locator IS NULL AND
          artifact_excerpt IS NULL)
         OR
         (resolved_kind = 'import_artifact' AND source_transcript_id IS NULL AND
          source_sequence_from IS NULL AND source_sequence_until IS NULL AND
          source_memory_id IS NULL AND source_memory_version IS NULL AND
          source_message_id IS NULL AND source_import_locator IS NOT NULL AND
          artifact_excerpt IS NULL)
         OR
         (resolved_kind = 'artifact' AND source_transcript_id IS NULL AND
          source_sequence_from IS NULL AND source_sequence_until IS NULL AND
          source_memory_id IS NULL AND source_memory_version IS NULL AND
          source_message_id IS NULL AND source_import_locator IS NULL AND
          artifact_excerpt IS NOT NULL)
       ))
    )
);

CREATE UNIQUE INDEX memory_evidence_one_terminal_resolution
    ON memory_evidence (pending_evidence_id)
    WHERE pending_evidence_id IS NOT NULL AND
          resolution_state IN ('resolved', 'unresolvable');
CREATE UNIQUE INDEX memory_evidence_resolution_idempotency
    ON memory_evidence
       (account_id, realm_id, owner_kind, owner_id, idempotency_key)
    WHERE idempotency_key IS NOT NULL;
CREATE INDEX memory_evidence_by_target
    ON memory_evidence (memory_id, target_version, created_at, id);

-- A deferred constraint allows capture to insert head, version, and evidence
-- in their natural order while rejecting a live memory with no durable source
-- capsule at transaction commit.
-- +goose StatementBegin
CREATE FUNCTION check_live_memory_has_evidence()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
DECLARE
    checked_memory_id TEXT;
BEGIN
    IF TG_TABLE_NAME = 'memory_evidence' THEN
        checked_memory_id := OLD.memory_id;
    ELSE
        checked_memory_id := NEW.id;
    END IF;
    IF EXISTS (
        SELECT 1 FROM memories m
        WHERE m.id = checked_memory_id AND m.current_version IS NOT NULL
    ) AND NOT EXISTS (
        SELECT 1 FROM memory_evidence e
        WHERE e.memory_id = checked_memory_id
    ) THEN
        RAISE EXCEPTION 'live memory % requires evidence', checked_memory_id
            USING ERRCODE = '23514';
    END IF;
    RETURN NULL;
END;
$$;
-- +goose StatementEnd

CREATE CONSTRAINT TRIGGER memories_require_evidence
AFTER INSERT OR UPDATE ON memories
DEFERRABLE INITIALLY DEFERRED
FOR EACH ROW EXECUTE FUNCTION check_live_memory_has_evidence();

CREATE CONSTRAINT TRIGGER memory_evidence_preserves_live_source
AFTER DELETE OR UPDATE ON memory_evidence
DEFERRABLE INITIALLY DEFERRED
FOR EACH ROW EXECUTE FUNCTION check_live_memory_has_evidence();

-- +goose StatementBegin
CREATE FUNCTION check_memory_evidence_transcript_range()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
DECLARE
    actual_entries BIGINT;
BEGIN
    IF NEW.resolution_state = 'resolved' AND NEW.resolved_kind = 'transcript' THEN
        SELECT count(*) INTO actual_entries
        FROM transcript_entries e
        WHERE e.transcript_id = NEW.source_transcript_id
          AND e.sequence BETWEEN NEW.source_sequence_from
                             AND NEW.source_sequence_until;
        IF actual_entries <> NEW.source_sequence_until - NEW.source_sequence_from + 1 THEN
            RAISE EXCEPTION 'transcript evidence range is not contiguous'
                USING ERRCODE = '23514';
        END IF;
    END IF;
    RETURN NULL;
END;
$$;
-- +goose StatementEnd

CREATE CONSTRAINT TRIGGER memory_evidence_requires_contiguous_transcript
AFTER INSERT OR UPDATE ON memory_evidence
DEFERRABLE INITIALLY DEFERRED
FOR EACH ROW EXECUTE FUNCTION check_memory_evidence_transcript_range();

-- The scoped message foreign key proves tenant placement. This deferred owner
-- check additionally proves that the memory owner actually participated in
-- the referenced same-realm message.
-- +goose StatementBegin
CREATE FUNCTION check_memory_evidence_message_participant()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
    IF NEW.resolution_state = 'resolved' AND NEW.resolved_kind = 'message' AND
       NOT EXISTS (
           SELECT 1 FROM agent_messages msg
           WHERE msg.id = NEW.source_message_id
             AND msg.account_id = NEW.account_id
             AND msg.realm_id = NEW.realm_id
             AND (msg.from_agent_id = NEW.owner_id OR
                  msg.to_agent_id = NEW.owner_id)
       ) THEN
        RAISE EXCEPTION 'memory owner did not participate in source message'
            USING ERRCODE = '23514';
    END IF;
    RETURN NULL;
END;
$$;
-- +goose StatementEnd

CREATE CONSTRAINT TRIGGER memory_evidence_requires_message_participant
AFTER INSERT OR UPDATE ON memory_evidence
DEFERRABLE INITIALLY DEFERRED
FOR EACH ROW EXECUTE FUNCTION check_memory_evidence_message_participant();

-- +goose StatementBegin
CREATE FUNCTION protect_memory_evidence_transcript_entries()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
    IF EXISTS (
        SELECT 1 FROM memory_evidence e
        WHERE e.resolution_state = 'resolved'
          AND e.resolved_kind = 'transcript'
          AND e.source_transcript_id = OLD.transcript_id
          AND OLD.sequence BETWEEN e.source_sequence_from
                               AND e.source_sequence_until
    ) THEN
        RAISE EXCEPTION 'transcript entry is pinned by memory evidence'
            USING ERRCODE = '23503';
    END IF;
    RETURN OLD;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER transcript_entries_memory_evidence_pin
BEFORE DELETE ON transcript_entries
FOR EACH ROW EXECUTE FUNCTION protect_memory_evidence_transcript_entries();

CREATE TABLE memory_relations (
    id                         TEXT        PRIMARY KEY,
    account_id                 TEXT        NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    realm_id                   TEXT        NOT NULL REFERENCES realms(id),
    owner_kind                 TEXT        NOT NULL DEFAULT 'agent',
    owner_id                   TEXT        NOT NULL REFERENCES agents(id),
    from_memory_id             TEXT        NOT NULL,
    from_version               INTEGER     NOT NULL,
    to_memory_id               TEXT        NOT NULL,
    to_version                 INTEGER     NOT NULL,
    relation_type              TEXT        NOT NULL,
    supersession_set_id        TEXT,
    supersession_set_revision  INTEGER,
    curation_run_id            TEXT,
    curation_action_id         TEXT,
    reverted_by_run_id         TEXT,
    reverted_at                TIMESTAMPTZ,
    created_at                 TIMESTAMPTZ NOT NULL DEFAULT now(),
    FOREIGN KEY
      (from_memory_id, from_version, account_id, realm_id, owner_kind, owner_id)
      REFERENCES memory_versions
        (memory_id, version, account_id, realm_id, owner_kind, owner_id)
      ON DELETE CASCADE DEFERRABLE INITIALLY DEFERRED,
    FOREIGN KEY
      (to_memory_id, to_version, account_id, realm_id, owner_kind, owner_id)
      REFERENCES memory_versions
        (memory_id, version, account_id, realm_id, owner_kind, owner_id)
      ON DELETE CASCADE DEFERRABLE INITIALLY DEFERRED,
    CHECK (id ~ '^mrel_[a-z2-7]{16}$'),
    CHECK (owner_kind = 'agent'),
    CHECK (from_version >= 1 AND to_version >= 1),
    CHECK (relation_type = 'supersedes'),
    CHECK (supersession_set_id ~ '^mset_[a-z2-7]{16}$' AND
           supersession_set_revision IS NOT NULL AND
           supersession_set_revision >= 1),
    CHECK (reverted_by_run_id IS NULL OR reverted_at IS NOT NULL),
    UNIQUE (from_memory_id, from_version, to_memory_id, to_version,
            relation_type, supersession_set_id)
);

CREATE INDEX memory_relations_by_source
    ON memory_relations (from_memory_id, from_version, relation_type, created_at, id);
CREATE INDEX memory_relations_by_target
    ON memory_relations (to_memory_id, to_version, relation_type, created_at, id);
CREATE INDEX memory_relations_by_supersession_set
    ON memory_relations (supersession_set_id, supersession_set_revision)
    WHERE supersession_set_id IS NOT NULL;

-- A superseded version can have multiple replacement edges, but all live
-- edges must belong to one exact set id/revision. Locking the target head makes
-- the deferred check safe against concurrent curators.
-- +goose StatementBegin
CREATE FUNCTION check_memory_one_active_supersession_set()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
DECLARE
    checked_memory_id TEXT;
    checked_version INTEGER;
    active_sets BIGINT;
BEGIN
    IF TG_OP = 'DELETE' THEN
        checked_memory_id := OLD.to_memory_id;
        checked_version := OLD.to_version;
    ELSE
        checked_memory_id := NEW.to_memory_id;
        checked_version := NEW.to_version;
    END IF;

    PERFORM 1 FROM memories
    WHERE id = checked_memory_id
    FOR UPDATE;

    SELECT count(*) INTO active_sets
    FROM (
        SELECT r.supersession_set_id, r.supersession_set_revision
        FROM memory_relations r
        WHERE r.to_memory_id = checked_memory_id
          AND r.to_version = checked_version
          AND r.relation_type = 'supersedes'
          AND r.reverted_at IS NULL
        GROUP BY r.supersession_set_id, r.supersession_set_revision
    ) sets;

    IF active_sets > 1 THEN
        RAISE EXCEPTION 'memory version belongs to multiple active supersession sets'
            USING ERRCODE = '23514';
    END IF;
    RETURN NULL;
END;
$$;
-- +goose StatementEnd

CREATE CONSTRAINT TRIGGER memory_relations_one_active_supersession_set
AFTER INSERT OR UPDATE OR DELETE ON memory_relations
DEFERRABLE INITIALLY DEFERRED
FOR EACH ROW EXECUTE FUNCTION check_memory_one_active_supersession_set();

-- This table deliberately has no version foreign key and no value-bearing
-- columns. It is the only durable trace allowed to outlive version purge.
CREATE TABLE memory_deleted_references (
    id                    TEXT        PRIMARY KEY,
    account_id            TEXT        NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    realm_id              TEXT        NOT NULL REFERENCES realms(id),
    owner_kind            TEXT        NOT NULL DEFAULT 'agent',
    owner_id              TEXT        NOT NULL REFERENCES agents(id),
    deleted_memory_id     TEXT        NOT NULL,
    former_reference_kind TEXT        NOT NULL,
    related_resource_id   TEXT,
    curation_run_id       TEXT,
    curation_request_id   TEXT,
    reason_code           TEXT        NOT NULL,
    created_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
    FOREIGN KEY (deleted_memory_id, account_id, realm_id, owner_kind, owner_id)
      REFERENCES memories (id, account_id, realm_id, owner_kind, owner_id)
      ON DELETE CASCADE DEFERRABLE INITIALLY DEFERRED,
    CHECK (id ~ '^mdr_[a-z2-7]{16}$'),
    CHECK (owner_kind = 'agent'),
    -- The first slice preserves only hashed mutation retry shields. Keeping
    -- the future curation columns null prevents a crafted archive from using
    -- this value-free tombstone table as a payload side channel.
    CHECK (former_reference_kind IN
      ('idempotency.added', 'idempotency.adjusted',
       'idempotency.superseded', 'idempotency.forgotten',
       'idempotency.restored', 'idempotency.reactivated',
       'idempotency.evidence_resolution')),
    CHECK (related_resource_id ~ '^[0-9a-f]{64}$'),
    CHECK (curation_run_id IS NULL AND curation_request_id IS NULL),
    CHECK (reason_code = 'permanent_delete')
);

-- +goose StatementBegin
CREATE FUNCTION check_memory_deleted_reference_target()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    PERFORM 1 FROM memories
    WHERE id = NEW.deleted_memory_id
      AND account_id = NEW.account_id
      AND realm_id = NEW.realm_id
      AND owner_kind = NEW.owner_kind
      AND owner_id = NEW.owner_id
      AND current_version IS NULL;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'deleted-memory reference requires a matching tombstone'
            USING ERRCODE = '23514';
    END IF;
    RETURN NULL;
END;
$$;
-- +goose StatementEnd

CREATE CONSTRAINT TRIGGER memory_deleted_references_require_tombstone
AFTER INSERT OR UPDATE ON memory_deleted_references
DEFERRABLE INITIALLY DEFERRED
FOR EACH ROW EXECUTE FUNCTION check_memory_deleted_reference_target();

CREATE INDEX memory_deleted_references_by_memory
    ON memory_deleted_references (deleted_memory_id, created_at, id);
CREATE UNIQUE INDEX memory_deleted_references_retry_shield
    ON memory_deleted_references
       (account_id, realm_id, owner_kind, owner_id,
        former_reference_kind, related_resource_id)
    WHERE related_resource_id IS NOT NULL AND
          former_reference_kind LIKE 'idempotency.%';

CREATE INDEX memory_versions_by_owner_state_activity
    ON memory_versions
       (account_id, realm_id, owner_kind, owner_id, state, created_at DESC,
        memory_id, version);
CREATE INDEX memory_versions_by_owner_change
    ON memory_versions
       (account_id, realm_id, owner_kind, owner_id, change_seq);
CREATE INDEX memory_versions_by_tags ON memory_versions USING GIN (tags);
CREATE INDEX memory_versions_search ON memory_versions USING GIN (search_document);
CREATE INDEX memories_by_owner_activity
    ON memories
       (account_id, realm_id, owner_kind, owner_id, updated_at DESC, id);

-- +goose Down
DROP INDEX memories_by_owner_activity;
DROP INDEX memories_delete_receipt_id;
DROP INDEX memories_by_owner_delete_idempotency;
DROP INDEX memory_versions_search;
DROP INDEX memory_versions_by_tags;
DROP INDEX memory_versions_by_owner_change;
DROP INDEX memory_versions_by_owner_state_activity;
DROP INDEX memory_deleted_references_by_memory;
DROP INDEX memory_deleted_references_retry_shield;
DROP TRIGGER memory_deleted_references_require_tombstone ON memory_deleted_references;
DROP FUNCTION check_memory_deleted_reference_target;
DROP TABLE memory_deleted_references;
DROP TRIGGER memory_relations_one_active_supersession_set ON memory_relations;
DROP FUNCTION check_memory_one_active_supersession_set;
DROP INDEX memory_relations_by_supersession_set;
DROP INDEX memory_relations_by_target;
DROP INDEX memory_relations_by_source;
DROP TABLE memory_relations;
DROP TRIGGER transcript_entries_memory_evidence_pin ON transcript_entries;
DROP FUNCTION protect_memory_evidence_transcript_entries;
DROP TRIGGER memory_evidence_requires_message_participant ON memory_evidence;
DROP FUNCTION check_memory_evidence_message_participant;
DROP TRIGGER memory_evidence_requires_contiguous_transcript ON memory_evidence;
DROP FUNCTION check_memory_evidence_transcript_range;
DROP TRIGGER memory_evidence_preserves_live_source ON memory_evidence;
DROP TRIGGER memories_require_evidence ON memories;
DROP FUNCTION check_live_memory_has_evidence;
DROP INDEX memory_evidence_by_target;
DROP INDEX memory_evidence_resolution_idempotency;
DROP INDEX memory_evidence_one_terminal_resolution;
DROP TABLE memory_evidence;
DROP INDEX agent_messages_memory_evidence_scope;
DROP INDEX transcript_conversations_memory_evidence_scope;
ALTER TABLE memories DROP CONSTRAINT memories_current_version_fk;
DROP TABLE memory_versions;
DROP TABLE memories;
DROP TABLE memory_change_clocks;
