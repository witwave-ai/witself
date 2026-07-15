-- +goose Up
-- Curation is a client-inference protocol. These tables provide only the
-- deterministic PostgreSQL queue, snapshot, fencing, plan, receipt, cursor,
-- and rollback-attribution boundaries; no backend process makes a semantic
-- decision.

ALTER TABLE memories
    ADD COLUMN deleted_curation_run_count BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN deleted_curation_action_count BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN deleted_curation_input_count BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN deleted_curation_mutation_count BIGINT NOT NULL DEFAULT 0,
    DROP CONSTRAINT memories_check;

ALTER TABLE memories ADD CONSTRAINT memories_check CHECK (
  (current_version IS NOT NULL AND permanently_deleted_at IS NULL AND
   permanently_deleted_by_id IS NULL AND permanent_delete_reason IS NULL AND
   delete_receipt_id = '' AND delete_idempotency_key_hash = '' AND
   deleted_prior_version = 0 AND deleted_scrub_set_revision = '' AND
   deleted_version_count = 0 AND deleted_evidence_count = 0 AND
   deleted_relation_count = 0 AND deleted_retry_shield_count = 0 AND
   deleted_retry_shield_digest = '' AND deleted_curation_run_count = 0 AND
   deleted_curation_action_count = 0 AND deleted_curation_input_count = 0 AND
   deleted_curation_mutation_count = 0)
  OR
  (current_version IS NULL AND permanently_deleted_at IS NOT NULL AND
   permanently_deleted_by_id IS NOT NULL AND permanent_delete_reason IS NOT NULL AND
   delete_receipt_id ~ '^mdel_[a-z2-7]{16}$' AND
   delete_idempotency_key_hash ~ '^[0-9a-f]{64}$' AND deleted_prior_version >= 1 AND
   deleted_scrub_set_revision ~ '^[0-9a-f]{64}$' AND
   deleted_version_count = deleted_prior_version AND deleted_evidence_count >= 1 AND
   deleted_relation_count >= 0 AND deleted_retry_shield_count >= deleted_version_count AND
   deleted_retry_shield_digest ~ '^[0-9a-f]{64}$' AND
   deleted_curation_run_count >= 0 AND deleted_curation_action_count >= 0 AND
   deleted_curation_input_count >= 0 AND deleted_curation_mutation_count >= 0)
);

CREATE TABLE memory_curation_lanes (
    account_id          TEXT        NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    realm_id            TEXT        NOT NULL REFERENCES realms(id),
    owner_kind          TEXT        NOT NULL DEFAULT 'agent',
    owner_id            TEXT        NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
    request_generation  BIGINT      NOT NULL DEFAULT 0,
    fencing_generation  BIGINT      NOT NULL DEFAULT 0,
    active_run_id       TEXT,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (account_id, realm_id, owner_kind, owner_id),
    CHECK (owner_kind = 'agent'),
    CHECK (request_generation BETWEEN 0 AND 4611686018427387903),
    CHECK (fencing_generation BETWEEN 0 AND 4611686018427387903),
    CHECK (active_run_id IS NULL OR active_run_id ~ '^mrun_[a-z2-7]{16}$')
);

-- The lane is the sole agent-owned root of a curation graph. Descendants
-- repeat owner and actor columns for scoped indexes and audit receipts, but
-- ownership is enforced transitively by their composite parent keys. Keeping
-- redundant direct agent foreign keys off descendants avoids competing delete
-- paths; actor_id is constrained to the owner wherever it is present. Forward
-- ownership edges cascade, while cyclic active/claimed/replay back-references
-- remain deferred and non-cascading.
CREATE TABLE memory_curation_cursors (
    account_id        TEXT        NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    realm_id          TEXT        NOT NULL REFERENCES realms(id),
    owner_kind        TEXT        NOT NULL DEFAULT 'agent',
    owner_id          TEXT        NOT NULL,
    source_kind       TEXT        NOT NULL,
    source_stream_id  TEXT        NOT NULL,
    position          BIGINT      NOT NULL DEFAULT 0,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY
      (account_id, realm_id, owner_kind, owner_id, source_kind, source_stream_id),
    FOREIGN KEY (account_id, realm_id, owner_kind, owner_id)
      REFERENCES memory_curation_lanes
        (account_id, realm_id, owner_kind, owner_id)
      ON DELETE CASCADE DEFERRABLE INITIALLY DEFERRED,
    CHECK (owner_kind = 'agent'),
    CHECK (source_kind IN ('memory', 'evidence', 'transcript')),
    CHECK (octet_length(source_stream_id) BETWEEN 1 AND 512),
    CHECK (position BETWEEN 0 AND 4611686018427387903)
);

CREATE TABLE memory_curation_requests (
    id                    TEXT        PRIMARY KEY,
    account_id            TEXT        NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    realm_id              TEXT        NOT NULL REFERENCES realms(id),
    owner_kind            TEXT        NOT NULL DEFAULT 'agent',
    owner_id              TEXT        NOT NULL,
    scope                 JSONB       NOT NULL DEFAULT '{}'::jsonb,
    coalescing_key        TEXT        NOT NULL,
    trigger_reason        TEXT        NOT NULL,
    request_generation    BIGINT      NOT NULL,
    priority              INTEGER     NOT NULL DEFAULT 0,
    due_at                TIMESTAMPTZ NOT NULL DEFAULT now(),
    state                 TEXT        NOT NULL DEFAULT 'queued',
    attempt_count         INTEGER     NOT NULL DEFAULT 0,
    max_attempts          INTEGER     NOT NULL DEFAULT 8,
    claimed_run_id        TEXT,
    fulfilled_generation  BIGINT      NOT NULL DEFAULT 0,
    replay_run_id         TEXT,
    read_only_replay      BOOLEAN     NOT NULL DEFAULT FALSE,
    actor_kind            TEXT        NOT NULL DEFAULT 'agent',
    actor_id              TEXT        NOT NULL,
    idempotency_key       TEXT        NOT NULL,
    request_hash          TEXT        NOT NULL,
    claimed_at            TIMESTAMPTZ,
    fulfilled_at          TIMESTAMPTZ,
    cancelled_at          TIMESTAMPTZ,
    dead_lettered_at      TIMESTAMPTZ,
    created_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (id, account_id, realm_id, owner_kind, owner_id),
    UNIQUE (id, claimed_run_id),
    FOREIGN KEY (account_id, realm_id, owner_kind, owner_id)
      REFERENCES memory_curation_lanes
        (account_id, realm_id, owner_kind, owner_id)
      ON DELETE CASCADE DEFERRABLE INITIALLY DEFERRED,
    CHECK (id ~ '^mcrq_[a-z2-7]{16}$'),
    CHECK (owner_kind = 'agent' AND actor_kind = 'agent' AND actor_id = owner_id),
    CHECK (jsonb_typeof(scope) = 'object' AND octet_length(scope::text) <= 32768),
    CHECK (octet_length(coalescing_key) BETWEEN 1 AND 256),
    CHECK (trigger_reason ~ '^[a-z][a-z0-9_.-]{0,63}$'),
    CHECK (request_generation BETWEEN 1 AND 4611686018427387903),
    CHECK (priority BETWEEN -1000 AND 1000),
    CHECK (state IN
      ('queued', 'claimed', 'retry_wait', 'fulfilled', 'cancelled', 'dead_letter')),
    CHECK (attempt_count BETWEEN 0 AND max_attempts AND max_attempts BETWEEN 1 AND 100),
    CHECK (fulfilled_generation BETWEEN 0 AND request_generation),
    CHECK (octet_length(idempotency_key) BETWEEN 1 AND 512),
    CHECK (request_hash ~ '^[0-9a-f]{64}$'),
    CHECK ((replay_run_id IS NULL AND NOT read_only_replay) OR
           (replay_run_id IS NOT NULL AND read_only_replay)),
    CHECK (
      (state IN ('queued', 'retry_wait') AND claimed_run_id IS NULL AND
       claimed_at IS NULL AND fulfilled_generation = 0 AND fulfilled_at IS NULL AND
       cancelled_at IS NULL AND dead_lettered_at IS NULL)
      OR
      (state = 'claimed' AND claimed_run_id IS NOT NULL AND claimed_at IS NOT NULL AND
       fulfilled_generation = 0 AND fulfilled_at IS NULL AND
       cancelled_at IS NULL AND dead_lettered_at IS NULL)
      OR
      (state = 'fulfilled' AND claimed_run_id IS NOT NULL AND claimed_at IS NOT NULL AND
       fulfilled_generation BETWEEN 1 AND request_generation AND fulfilled_at IS NOT NULL AND
       cancelled_at IS NULL AND dead_lettered_at IS NULL)
      OR
      (state = 'cancelled' AND fulfilled_generation = 0 AND fulfilled_at IS NULL AND
       cancelled_at IS NOT NULL AND dead_lettered_at IS NULL)
      OR
      (state = 'dead_letter' AND fulfilled_generation = 0 AND fulfilled_at IS NULL AND
       cancelled_at IS NULL AND dead_lettered_at IS NOT NULL)
    )
);

CREATE UNIQUE INDEX memory_curation_requests_initial_idempotency
    ON memory_curation_requests
       (account_id, realm_id, owner_kind, owner_id, actor_id, idempotency_key);
CREATE UNIQUE INDEX memory_curation_requests_open_coalescing
    ON memory_curation_requests
       (account_id, realm_id, owner_kind, owner_id, coalescing_key)
    WHERE state IN ('queued', 'claimed', 'retry_wait');
CREATE INDEX memory_curation_requests_due
    ON memory_curation_requests
       (state, due_at, priority DESC, created_at, id)
    WHERE state IN ('queued', 'retry_wait');

CREATE TABLE memory_curation_runs (
    id                      TEXT        PRIMARY KEY,
    account_id              TEXT        NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    realm_id                TEXT        NOT NULL REFERENCES realms(id),
    owner_kind              TEXT        NOT NULL DEFAULT 'agent',
    owner_id                TEXT        NOT NULL,
    request_id              TEXT        NOT NULL,
    request_generation      BIGINT      NOT NULL,
    fencing_generation      BIGINT      NOT NULL,
    state                   TEXT        NOT NULL DEFAULT 'open',
    actor_kind              TEXT        NOT NULL DEFAULT 'agent',
    actor_id                TEXT        NOT NULL,
    idempotency_key         TEXT        NOT NULL,
    request_hash            TEXT        NOT NULL,
    lease_expires_at        TIMESTAMPTZ,
    client_runtime          TEXT        NOT NULL DEFAULT '',
    client_model            TEXT        NOT NULL DEFAULT '',
    client_recipe           TEXT        NOT NULL DEFAULT '',
    client_recipe_version   TEXT        NOT NULL DEFAULT '',
    memory_change_upper     BIGINT      NOT NULL DEFAULT 0,
    evidence_change_upper   BIGINT      NOT NULL DEFAULT 0,
    input_count             INTEGER     NOT NULL DEFAULT 0,
    memory_input_count      INTEGER     NOT NULL DEFAULT 0,
    evidence_input_count    INTEGER     NOT NULL DEFAULT 0,
    transcript_input_count  INTEGER     NOT NULL DEFAULT 0,
    cursor_input_count      INTEGER     NOT NULL DEFAULT 0,
    plan_schema             TEXT        NOT NULL DEFAULT '',
    plan_revision           INTEGER     NOT NULL DEFAULT 0,
    plan_hash               TEXT        NOT NULL DEFAULT '',
    apply_receipt_id        TEXT        NOT NULL DEFAULT '',
    rollback_receipt_id     TEXT        NOT NULL DEFAULT '',
    conflict_reason_code    TEXT        NOT NULL DEFAULT '',
    terminal_reason_code    TEXT        NOT NULL DEFAULT '',
    budgets                 JSONB       NOT NULL DEFAULT '{}'::jsonb,
    scrubbed_at             TIMESTAMPTZ,
    scrubbed_reason_code    TEXT        NOT NULL DEFAULT '',
    started_at              TIMESTAMPTZ NOT NULL DEFAULT now(),
    planned_at              TIMESTAMPTZ,
    applied_at              TIMESTAMPTZ,
    rolled_back_at          TIMESTAMPTZ,
    terminal_at             TIMESTAMPTZ,
    created_at              TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at              TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (id, account_id, realm_id, owner_kind, owner_id),
    UNIQUE (id, account_id, realm_id, owner_id),
    UNIQUE (id, request_id),
    FOREIGN KEY (request_id, account_id, realm_id, owner_kind, owner_id)
      REFERENCES memory_curation_requests
        (id, account_id, realm_id, owner_kind, owner_id)
      ON DELETE CASCADE DEFERRABLE INITIALLY DEFERRED,
    CHECK (id ~ '^mrun_[a-z2-7]{16}$'),
    CHECK (owner_kind = 'agent' AND actor_kind = 'agent' AND actor_id = owner_id),
    CHECK (request_generation BETWEEN 1 AND 4611686018427387903),
    CHECK (fencing_generation BETWEEN 1 AND 4611686018427387903),
    CHECK (state IN
      ('open', 'planned', 'applied', 'rolled_back', 'abandoned', 'interrupted', 'conflict')),
    CHECK (octet_length(idempotency_key) BETWEEN 1 AND 512),
    CHECK (request_hash ~ '^[0-9a-f]{64}$'),
    CHECK (octet_length(client_runtime) <= 128),
    CHECK (octet_length(client_model) <= 256),
    CHECK (octet_length(client_recipe) <= 128),
    CHECK (octet_length(client_recipe_version) <= 128),
    CHECK (memory_change_upper BETWEEN 0 AND 4611686018427387903),
    CHECK (evidence_change_upper BETWEEN 0 AND 4611686018427387903),
    CHECK (input_count BETWEEN 0 AND 10000),
    CHECK (memory_input_count BETWEEN 0 AND input_count),
    CHECK (evidence_input_count BETWEEN 0 AND input_count),
    CHECK (transcript_input_count BETWEEN 0 AND input_count),
    CHECK (cursor_input_count BETWEEN 0 AND input_count),
    CHECK (input_count = memory_input_count + evidence_input_count +
                         transcript_input_count + cursor_input_count),
    CHECK (plan_revision >= 0),
    CHECK ((plan_revision = 0 AND plan_schema = '' AND plan_hash = '') OR
           (plan_revision >= 1 AND plan_schema = 'witself.memory-plan.v1' AND
            (plan_hash ~ '^[0-9a-f]{64}$' OR
             (scrubbed_at IS NOT NULL AND plan_hash = '')))),
    CHECK (octet_length(apply_receipt_id) <= 128),
    CHECK (octet_length(rollback_receipt_id) <= 128),
    CHECK (octet_length(conflict_reason_code) <= 128),
    CHECK (octet_length(terminal_reason_code) <= 128),
    CHECK (jsonb_typeof(budgets) = 'object' AND octet_length(budgets::text) <= 16384),
    CHECK ((scrubbed_at IS NULL AND scrubbed_reason_code = '') OR
           (scrubbed_at IS NOT NULL AND scrubbed_reason_code = 'permanent_delete')),
    CHECK (
      (state = 'open' AND lease_expires_at IS NOT NULL AND plan_revision = 0 AND
       planned_at IS NULL AND applied_at IS NULL AND rolled_back_at IS NULL AND terminal_at IS NULL)
      OR
      (state = 'planned' AND lease_expires_at IS NOT NULL AND plan_revision >= 1 AND
       planned_at IS NOT NULL AND applied_at IS NULL AND rolled_back_at IS NULL AND terminal_at IS NULL)
      OR
      (state = 'applied' AND lease_expires_at IS NULL AND plan_revision >= 1 AND
       apply_receipt_id <> '' AND applied_at IS NOT NULL AND rolled_back_at IS NULL AND terminal_at IS NULL)
      OR
      (state = 'rolled_back' AND lease_expires_at IS NULL AND plan_revision >= 1 AND
       apply_receipt_id <> '' AND rollback_receipt_id <> '' AND
       applied_at IS NOT NULL AND rolled_back_at IS NOT NULL AND terminal_at IS NOT NULL)
      OR
      (state IN ('abandoned', 'interrupted') AND lease_expires_at IS NULL AND
       applied_at IS NULL AND rolled_back_at IS NULL AND terminal_at IS NOT NULL)
      OR
      (state = 'conflict' AND lease_expires_at IS NULL AND plan_revision >= 1 AND
       conflict_reason_code <> '' AND applied_at IS NULL AND
       rolled_back_at IS NULL AND terminal_at IS NOT NULL)
    )
);

CREATE UNIQUE INDEX memory_curation_runs_initial_idempotency
    ON memory_curation_runs
       (account_id, realm_id, owner_kind, owner_id, actor_id, idempotency_key);
CREATE INDEX memory_curation_runs_by_request
    ON memory_curation_runs (request_id, created_at, id);
CREATE INDEX memory_curation_runs_by_owner_state
    ON memory_curation_runs
       (account_id, realm_id, owner_kind, owner_id, state, created_at, id);

ALTER TABLE memory_curation_lanes
  ADD CONSTRAINT memory_curation_lanes_active_run_fk
  FOREIGN KEY (active_run_id, account_id, realm_id, owner_kind, owner_id)
  REFERENCES memory_curation_runs
    (id, account_id, realm_id, owner_kind, owner_id)
  DEFERRABLE INITIALLY DEFERRED;

CREATE UNIQUE INDEX memory_curation_lanes_one_active_run
    ON memory_curation_lanes (active_run_id)
    WHERE active_run_id IS NOT NULL;

ALTER TABLE memory_curation_requests
  ADD CONSTRAINT memory_curation_requests_claimed_run_fk
  FOREIGN KEY (claimed_run_id, id)
  REFERENCES memory_curation_runs (id, request_id)
  DEFERRABLE INITIALLY DEFERRED;

ALTER TABLE memory_curation_requests
  ADD CONSTRAINT memory_curation_requests_replay_run_fk
  FOREIGN KEY (replay_run_id, account_id, realm_id, owner_kind, owner_id)
  REFERENCES memory_curation_runs
    (id, account_id, realm_id, owner_kind, owner_id)
  DEFERRABLE INITIALLY DEFERRED;

CREATE UNIQUE INDEX memory_evidence_curation_scope
    ON memory_evidence (id, account_id, realm_id, owner_kind, owner_id);

CREATE TABLE memory_curation_run_inputs (
    run_id                 TEXT        NOT NULL,
    ordinal                BIGINT      NOT NULL,
    account_id             TEXT        NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    realm_id               TEXT        NOT NULL REFERENCES realms(id),
    owner_kind             TEXT        NOT NULL DEFAULT 'agent',
    owner_id               TEXT        NOT NULL,
    input_kind             TEXT        NOT NULL,
    order_key              TEXT        NOT NULL,
    memory_id              TEXT,
    memory_version         INTEGER,
    evidence_id            TEXT,
    transcript_id          TEXT,
    sequence_from          BIGINT,
    sequence_until         BIGINT,
    cursor_source_kind     TEXT,
    cursor_stream_id       TEXT,
    cursor_expected_prior  BIGINT,
    cursor_upper           BIGINT,
    created_at             TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (run_id, ordinal),
    UNIQUE (run_id, order_key),
    FOREIGN KEY (run_id, account_id, realm_id, owner_kind, owner_id)
      REFERENCES memory_curation_runs
        (id, account_id, realm_id, owner_kind, owner_id)
      ON DELETE CASCADE DEFERRABLE INITIALLY DEFERRED,
    FOREIGN KEY
      (memory_id, memory_version, account_id, realm_id, owner_kind, owner_id)
      REFERENCES memory_versions
        (memory_id, version, account_id, realm_id, owner_kind, owner_id)
      ON DELETE CASCADE DEFERRABLE INITIALLY DEFERRED,
    FOREIGN KEY (evidence_id, account_id, realm_id, owner_kind, owner_id)
      REFERENCES memory_evidence (id, account_id, realm_id, owner_kind, owner_id)
      ON DELETE CASCADE DEFERRABLE INITIALLY DEFERRED,
    FOREIGN KEY (transcript_id, account_id, realm_id, owner_id)
      REFERENCES transcript_conversations
        (id, account_id, realm_id, owner_agent_id)
      DEFERRABLE INITIALLY DEFERRED,
    FOREIGN KEY
      (account_id, realm_id, owner_kind, owner_id, cursor_source_kind,
       cursor_stream_id)
      REFERENCES memory_curation_cursors
        (account_id, realm_id, owner_kind, owner_id, source_kind,
         source_stream_id)
      DEFERRABLE INITIALLY DEFERRED,
    CHECK (owner_kind = 'agent'),
    CHECK (ordinal BETWEEN 1 AND 4611686018427387903),
    CHECK (input_kind IN ('memory', 'evidence', 'transcript', 'cursor')),
    CHECK (octet_length(order_key) BETWEEN 1 AND 512),
    CHECK (memory_version IS NULL OR memory_version >= 1),
    CHECK (sequence_from IS NULL OR sequence_from BETWEEN 1 AND 4611686018427387903),
    CHECK (sequence_until IS NULL OR sequence_until BETWEEN 1 AND 4611686018427387903),
    CHECK (cursor_source_kind IS NULL OR cursor_source_kind IN ('memory', 'evidence', 'transcript')),
    CHECK (cursor_stream_id IS NULL OR octet_length(cursor_stream_id) BETWEEN 1 AND 512),
    CHECK (cursor_expected_prior IS NULL OR cursor_expected_prior BETWEEN 0 AND 4611686018427387903),
    CHECK (cursor_upper IS NULL OR cursor_upper BETWEEN 0 AND 4611686018427387903),
    CHECK (
      (input_kind = 'memory' AND memory_id IS NOT NULL AND memory_version IS NOT NULL AND
       evidence_id IS NULL AND transcript_id IS NULL AND sequence_from IS NULL AND sequence_until IS NULL AND
       cursor_source_kind IS NULL AND cursor_stream_id IS NULL AND
       cursor_expected_prior IS NULL AND cursor_upper IS NULL)
      OR
      (input_kind = 'evidence' AND memory_id IS NULL AND memory_version IS NULL AND
       evidence_id IS NOT NULL AND transcript_id IS NULL AND sequence_from IS NULL AND sequence_until IS NULL AND
       cursor_source_kind IS NULL AND cursor_stream_id IS NULL AND
       cursor_expected_prior IS NULL AND cursor_upper IS NULL)
      OR
      (input_kind = 'transcript' AND memory_id IS NULL AND memory_version IS NULL AND
       evidence_id IS NULL AND transcript_id IS NOT NULL AND
       sequence_from IS NOT NULL AND sequence_until IS NOT NULL AND sequence_until >= sequence_from AND
       cursor_source_kind IS NULL AND cursor_stream_id IS NULL AND
       cursor_expected_prior IS NULL AND cursor_upper IS NULL)
      OR
      (input_kind = 'cursor' AND memory_id IS NULL AND memory_version IS NULL AND
       evidence_id IS NULL AND transcript_id IS NULL AND sequence_from IS NULL AND sequence_until IS NULL AND
       cursor_source_kind IS NOT NULL AND cursor_stream_id IS NOT NULL AND
       cursor_expected_prior IS NOT NULL AND cursor_upper IS NOT NULL AND
       cursor_upper >= cursor_expected_prior)
    )
);

CREATE TABLE memory_curation_actions (
    id                    TEXT        PRIMARY KEY,
    run_id                TEXT        NOT NULL,
    account_id            TEXT        NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    realm_id              TEXT        NOT NULL REFERENCES realms(id),
    owner_kind            TEXT        NOT NULL DEFAULT 'agent',
    owner_id              TEXT        NOT NULL,
    ordinal               BIGINT      NOT NULL,
    plan_revision         INTEGER     NOT NULL,
    primitive             TEXT        NOT NULL,
    state                 TEXT        NOT NULL DEFAULT 'draft',
    local_ref             TEXT        NOT NULL DEFAULT '',
    input_refs            JSONB       NOT NULL DEFAULT '[]'::jsonb,
    expected_heads        JSONB       NOT NULL DEFAULT '[]'::jsonb,
    proposed_payload      JSONB       NOT NULL DEFAULT '{}'::jsonb,
    validation_result     JSONB       NOT NULL DEFAULT '{}'::jsonb,
    applied_result        JSONB       NOT NULL DEFAULT '{}'::jsonb,
    rollback_result       JSONB       NOT NULL DEFAULT '{}'::jsonb,
    action_hash           TEXT        NOT NULL DEFAULT '',
    scrubbed_at           TIMESTAMPTZ,
    scrubbed_reason_code  TEXT        NOT NULL DEFAULT '',
    created_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
    validated_at          TIMESTAMPTZ,
    applied_at            TIMESTAMPTZ,
    reverted_at           TIMESTAMPTZ,
    UNIQUE (run_id, ordinal),
    UNIQUE (id, run_id),
    UNIQUE (id, account_id, realm_id, owner_kind, owner_id),
    UNIQUE (id, run_id, account_id, realm_id, owner_kind, owner_id),
    UNIQUE (id, run_id, account_id, realm_id, owner_id),
    FOREIGN KEY (run_id, account_id, realm_id, owner_kind, owner_id)
      REFERENCES memory_curation_runs
        (id, account_id, realm_id, owner_kind, owner_id)
      ON DELETE CASCADE DEFERRABLE INITIALLY DEFERRED,
    CHECK (id ~ '^mact_[a-z2-7]{16}$'),
    CHECK (owner_kind = 'agent'),
    CHECK (ordinal BETWEEN 1 AND 4611686018427387903),
    CHECK (plan_revision >= 1),
    CHECK (primitive IN ('create', 'replace', 'supersede', 'relate', 'propose_fact')),
    CHECK (state IN ('draft', 'validated', 'applied', 'reverted')),
    CHECK (octet_length(local_ref) <= 128),
    CHECK (jsonb_typeof(input_refs) = 'array' AND octet_length(input_refs::text) <= 65536),
    CHECK (jsonb_typeof(expected_heads) = 'array' AND octet_length(expected_heads::text) <= 65536),
    CHECK (jsonb_typeof(proposed_payload) = 'object' AND octet_length(proposed_payload::text) <= 33554432),
    CHECK (jsonb_typeof(validation_result) = 'object' AND octet_length(validation_result::text) <= 65536),
    CHECK (jsonb_typeof(applied_result) = 'object' AND octet_length(applied_result::text) <= 65536),
    CHECK (jsonb_typeof(rollback_result) = 'object' AND octet_length(rollback_result::text) <= 65536),
    CHECK ((state = 'draft' AND action_hash = '') OR
           (state <> 'draft' AND
            (action_hash ~ '^[0-9a-f]{64}$' OR
             (scrubbed_at IS NOT NULL AND action_hash = '')))),
    CHECK ((scrubbed_at IS NULL AND scrubbed_reason_code = '') OR
           (scrubbed_at IS NOT NULL AND scrubbed_reason_code = 'permanent_delete' AND
            local_ref = '' AND
            input_refs = '[]'::jsonb AND expected_heads = '[]'::jsonb AND
            proposed_payload = '{}'::jsonb AND validation_result = '{}'::jsonb AND
            applied_result = '{}'::jsonb AND rollback_result = '{}'::jsonb AND
            action_hash = '')),
    CHECK (
      (state = 'draft' AND validated_at IS NULL AND applied_at IS NULL AND reverted_at IS NULL)
      OR
      (state = 'validated' AND validated_at IS NOT NULL AND applied_at IS NULL AND reverted_at IS NULL)
      OR
      (state = 'applied' AND validated_at IS NOT NULL AND applied_at IS NOT NULL AND reverted_at IS NULL)
      OR
      (state = 'reverted' AND validated_at IS NOT NULL AND applied_at IS NOT NULL AND reverted_at IS NOT NULL)
    )
);

CREATE TABLE memory_curation_mutations (
    id                  TEXT        PRIMARY KEY,
    account_id          TEXT        NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    realm_id            TEXT        NOT NULL REFERENCES realms(id),
    owner_kind          TEXT        NOT NULL DEFAULT 'agent',
    owner_id            TEXT        NOT NULL,
    actor_kind          TEXT        NOT NULL DEFAULT 'agent',
    actor_id            TEXT        NOT NULL,
    operation           TEXT        NOT NULL,
    idempotency_key     TEXT        NOT NULL,
    request_hash        TEXT        NOT NULL,
    request_id          TEXT        NOT NULL,
    run_id              TEXT,
    request_generation  BIGINT      NOT NULL,
    fencing_generation  BIGINT      NOT NULL DEFAULT 0,
    plan_revision       INTEGER     NOT NULL DEFAULT 0,
    plan_hash           TEXT        NOT NULL DEFAULT '',
    lease_expires_at    TIMESTAMPTZ,
    result_state        TEXT        NOT NULL,
    receipt_id          TEXT        NOT NULL DEFAULT '',
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE
      (account_id, realm_id, owner_kind, owner_id, actor_id, operation,
       idempotency_key),
    FOREIGN KEY (request_id, account_id, realm_id, owner_kind, owner_id)
      REFERENCES memory_curation_requests
        (id, account_id, realm_id, owner_kind, owner_id)
      ON DELETE CASCADE DEFERRABLE INITIALLY DEFERRED,
    FOREIGN KEY (run_id, request_id)
      REFERENCES memory_curation_runs (id, request_id)
      ON DELETE CASCADE DEFERRABLE INITIALLY DEFERRED,
    CHECK (id ~ '^mcmu_[a-z2-7]{16}$'),
    CHECK (owner_kind = 'agent' AND actor_kind = 'agent' AND actor_id = owner_id),
    CHECK (operation IN
      ('request', 'start', 'renew', 'plan', 'apply', 'cancel', 'abandon', 'rollback')),
    CHECK (octet_length(idempotency_key) BETWEEN 1 AND 512),
    CHECK (request_hash ~ '^[0-9a-f]{64}$'),
    CHECK (request_generation BETWEEN 1 AND 4611686018427387903),
    CHECK (fencing_generation BETWEEN 0 AND 4611686018427387903),
    CHECK (plan_revision >= 0),
    CHECK ((plan_revision = 0 AND plan_hash = '') OR
           (plan_revision >= 1 AND plan_hash ~ '^[0-9a-f]{64}$')),
    CHECK (octet_length(result_state) BETWEEN 1 AND 64),
    CHECK (receipt_id = '' OR receipt_id ~ '^mrec_[a-z2-7]{16}$'),
    CHECK ((operation = 'request' AND run_id IS NULL AND fencing_generation = 0 AND
            plan_revision = 0 AND lease_expires_at IS NULL) OR
           (operation <> 'request' AND run_id IS NOT NULL AND fencing_generation >= 1)),
    CHECK ((operation IN ('start', 'renew') AND lease_expires_at IS NOT NULL) OR
           (operation NOT IN ('start', 'renew') AND lease_expires_at IS NULL)),
    CHECK ((operation IN ('plan', 'apply') AND plan_revision >= 1) OR
           (operation NOT IN ('plan', 'apply') AND plan_revision = 0))
);

-- Curation attribution is nullable for ordinary direct operations, but when
-- present it is exact and same-run. Rollback adds only the semantic `reverted`
-- operation/state; create/replace/supersede keep their existing operation names.
ALTER TABLE memory_versions
    DROP CONSTRAINT memory_versions_state_check,
    DROP CONSTRAINT memory_versions_operation_check;

ALTER TABLE memory_versions
    ADD CONSTRAINT memory_versions_state_check
      CHECK (state IN ('active', 'superseded', 'forgotten', 'reverted')),
    ADD CONSTRAINT memory_versions_operation_check
      CHECK (operation IN
        ('added', 'adjusted', 'superseded', 'forgotten', 'restored',
         'reactivated', 'reverted')),
    ADD CONSTRAINT memory_versions_curation_pair
      CHECK ((curation_run_id IS NULL) = (curation_action_id IS NULL)),
    ADD CONSTRAINT memory_versions_curation_run_fk
      FOREIGN KEY (curation_run_id, account_id, realm_id, owner_kind, owner_id)
      REFERENCES memory_curation_runs
        (id, account_id, realm_id, owner_kind, owner_id)
      DEFERRABLE INITIALLY DEFERRED,
    ADD CONSTRAINT memory_versions_curation_action_fk
      FOREIGN KEY
        (curation_action_id, curation_run_id, account_id, realm_id,
         owner_kind, owner_id)
      REFERENCES memory_curation_actions
        (id, run_id, account_id, realm_id, owner_kind, owner_id)
      DEFERRABLE INITIALLY DEFERRED;

ALTER TABLE memory_relations
    ADD COLUMN reverted_by_action_id TEXT,
    DROP CONSTRAINT memory_relations_relation_type_check,
    DROP CONSTRAINT memory_relations_check1,
    DROP CONSTRAINT memory_relations_check2;

ALTER TABLE memory_relations
    ADD CONSTRAINT memory_relations_relation_type_check
      CHECK (relation_type IN
        ('supersedes', 'derived_from', 'summarizes', 'merged_from',
         'split_from', 'conflicts_with')),
    ADD CONSTRAINT memory_relations_set_shape
      CHECK ((relation_type = 'supersedes' AND
              supersession_set_id ~ '^mset_[a-z2-7]{16}$' AND
              supersession_set_revision IS NOT NULL AND supersession_set_revision >= 1)
             OR
             (relation_type <> 'supersedes' AND supersession_set_id IS NULL AND
              supersession_set_revision IS NULL)),
    ADD CONSTRAINT memory_relations_curation_shape
      CHECK (((curation_run_id IS NULL) = (curation_action_id IS NULL)) AND
             (relation_type = 'supersedes' OR curation_run_id IS NOT NULL)),
    ADD CONSTRAINT memory_relations_revert_shape
      CHECK ((reverted_at IS NULL AND reverted_by_run_id IS NULL AND
              reverted_by_action_id IS NULL) OR
             (reverted_at IS NOT NULL AND
              ((reverted_by_run_id IS NULL AND reverted_by_action_id IS NULL) OR
               (reverted_by_run_id IS NOT NULL AND reverted_by_action_id IS NOT NULL)))),
    ADD CONSTRAINT memory_relations_curation_run_fk
      FOREIGN KEY (curation_run_id, account_id, realm_id, owner_kind, owner_id)
      REFERENCES memory_curation_runs
        (id, account_id, realm_id, owner_kind, owner_id)
      DEFERRABLE INITIALLY DEFERRED,
    ADD CONSTRAINT memory_relations_curation_action_fk
      FOREIGN KEY
        (curation_action_id, curation_run_id, account_id, realm_id,
         owner_kind, owner_id)
      REFERENCES memory_curation_actions
        (id, run_id, account_id, realm_id, owner_kind, owner_id)
      DEFERRABLE INITIALLY DEFERRED,
    ADD CONSTRAINT memory_relations_reverted_by_action_fk
      FOREIGN KEY
        (reverted_by_action_id, reverted_by_run_id, account_id, realm_id,
         owner_kind, owner_id)
      REFERENCES memory_curation_actions
        (id, run_id, account_id, realm_id, owner_kind, owner_id)
      DEFERRABLE INITIALLY DEFERRED;

-- A curator may only propose a candidate. Rollback uses a distinct withdrawn
-- terminal state and never impersonates a human rejection.
ALTER TABLE fact_candidates
    ADD COLUMN curation_run_id TEXT,
    ADD COLUMN curation_action_id TEXT,
    ADD COLUMN withdrawal_reason TEXT NOT NULL DEFAULT '',
    ADD COLUMN withdrawal_idempotency_key TEXT NOT NULL DEFAULT '',
    ADD COLUMN withdrawal_request_hash TEXT NOT NULL DEFAULT '',
    DROP CONSTRAINT fact_candidates_status_check,
    DROP CONSTRAINT fact_candidates_check2;

ALTER TABLE fact_candidates
    ADD CONSTRAINT fact_candidates_status_check
      CHECK (status IN ('pending', 'conflict', 'confirmed', 'rejected', 'withdrawn')),
    ADD CONSTRAINT fact_candidates_lifecycle_check
      CHECK (
        (status = 'pending' AND conflict_fact_id IS NULL AND
         resolved_fact_id IS NULL AND decided_at IS NULL) OR
        (status = 'conflict' AND conflict_fact_id IS NOT NULL AND
         observed_assertion_id IS NOT NULL AND resolved_fact_id IS NULL AND
         decided_at IS NULL) OR
        (status = 'confirmed' AND resolved_fact_id IS NOT NULL AND decided_at IS NOT NULL) OR
        (status = 'rejected' AND resolved_fact_id IS NULL AND decided_at IS NOT NULL) OR
        (status = 'withdrawn' AND resolved_fact_id IS NULL AND decided_at IS NOT NULL AND
         ((conflict_fact_id IS NULL AND observed_assertion_id IS NULL) OR
          (conflict_fact_id IS NOT NULL AND observed_assertion_id IS NOT NULL)))),
    ADD CONSTRAINT fact_candidates_curation_pair
      CHECK ((curation_run_id IS NULL) = (curation_action_id IS NULL)),
    ADD CONSTRAINT fact_candidates_withdrawal_shape
      CHECK ((status = 'withdrawn' AND curation_run_id IS NOT NULL AND
              withdrawal_reason = 'curation_rollback' AND
              octet_length(withdrawal_idempotency_key) BETWEEN 1 AND 512 AND
              withdrawal_request_hash ~ '^[0-9a-f]{64}$') OR
             (status <> 'withdrawn' AND withdrawal_reason = '' AND
              withdrawal_idempotency_key = '' AND withdrawal_request_hash = '')),
    ADD CONSTRAINT fact_candidates_curation_run_fk
      FOREIGN KEY (curation_run_id, account_id, realm_id, owner_agent_id)
      REFERENCES memory_curation_runs (id, account_id, realm_id, owner_id)
      DEFERRABLE INITIALLY DEFERRED,
    ADD CONSTRAINT fact_candidates_curation_action_fk
      FOREIGN KEY
        (curation_action_id, curation_run_id, account_id, realm_id,
         owner_agent_id)
      REFERENCES memory_curation_actions
        (id, run_id, account_id, realm_id, owner_id)
      DEFERRABLE INITIALLY DEFERRED;

CREATE UNIQUE INDEX fact_candidates_withdrawal_idempotency
    ON fact_candidates
       (account_id, realm_id, owner_agent_id, withdrawal_idempotency_key)
    WHERE withdrawal_idempotency_key <> '';

-- +goose Down
DROP INDEX fact_candidates_withdrawal_idempotency;
ALTER TABLE fact_candidates
    DROP CONSTRAINT fact_candidates_curation_action_fk,
    DROP CONSTRAINT fact_candidates_curation_run_fk,
    DROP CONSTRAINT fact_candidates_withdrawal_shape,
    DROP CONSTRAINT fact_candidates_curation_pair,
    DROP CONSTRAINT fact_candidates_lifecycle_check,
    DROP CONSTRAINT fact_candidates_status_check,
    DROP COLUMN withdrawal_request_hash,
    DROP COLUMN withdrawal_idempotency_key,
    DROP COLUMN withdrawal_reason,
    DROP COLUMN curation_action_id,
    DROP COLUMN curation_run_id,
    ADD CONSTRAINT fact_candidates_status_check
      CHECK (status IN ('pending', 'conflict', 'confirmed', 'rejected')),
    ADD CONSTRAINT fact_candidates_check2
      CHECK (
        (status = 'pending' AND conflict_fact_id IS NULL AND resolved_fact_id IS NULL AND decided_at IS NULL) OR
        (status = 'conflict' AND conflict_fact_id IS NOT NULL AND observed_assertion_id IS NOT NULL AND resolved_fact_id IS NULL AND decided_at IS NULL) OR
        (status = 'confirmed' AND resolved_fact_id IS NOT NULL AND decided_at IS NOT NULL) OR
        (status = 'rejected' AND resolved_fact_id IS NULL AND decided_at IS NOT NULL));

ALTER TABLE memory_relations
    DROP CONSTRAINT memory_relations_reverted_by_action_fk,
    DROP CONSTRAINT memory_relations_curation_action_fk,
    DROP CONSTRAINT memory_relations_curation_run_fk,
    DROP CONSTRAINT memory_relations_revert_shape,
    DROP CONSTRAINT memory_relations_curation_shape,
    DROP CONSTRAINT memory_relations_set_shape,
    DROP CONSTRAINT memory_relations_relation_type_check,
    DROP COLUMN reverted_by_action_id,
    ADD CONSTRAINT memory_relations_relation_type_check
      CHECK (relation_type = 'supersedes'),
    ADD CONSTRAINT memory_relations_check1
      CHECK (supersession_set_id ~ '^mset_[a-z2-7]{16}$' AND
             supersession_set_revision IS NOT NULL AND supersession_set_revision >= 1),
    ADD CONSTRAINT memory_relations_check2
      CHECK (reverted_by_run_id IS NULL OR reverted_at IS NOT NULL);

ALTER TABLE memory_versions
    DROP CONSTRAINT memory_versions_curation_action_fk,
    DROP CONSTRAINT memory_versions_curation_run_fk,
    DROP CONSTRAINT memory_versions_curation_pair,
    DROP CONSTRAINT memory_versions_operation_check,
    DROP CONSTRAINT memory_versions_state_check,
    ADD CONSTRAINT memory_versions_state_check
      CHECK (state IN ('active', 'superseded', 'forgotten')),
    ADD CONSTRAINT memory_versions_operation_check
      CHECK (operation IN
        ('added', 'adjusted', 'superseded', 'forgotten', 'restored',
         'reactivated'));

DROP TABLE memory_curation_mutations;
DROP TABLE memory_curation_actions;
DROP TABLE memory_curation_run_inputs;
DROP INDEX memory_evidence_curation_scope;
ALTER TABLE memory_curation_requests
    DROP CONSTRAINT memory_curation_requests_replay_run_fk,
    DROP CONSTRAINT memory_curation_requests_claimed_run_fk;
DROP INDEX memory_curation_lanes_one_active_run;
ALTER TABLE memory_curation_lanes
    DROP CONSTRAINT memory_curation_lanes_active_run_fk;
DROP INDEX memory_curation_runs_by_owner_state;
DROP INDEX memory_curation_runs_by_request;
DROP INDEX memory_curation_runs_initial_idempotency;
DROP TABLE memory_curation_runs;
DROP INDEX memory_curation_requests_due;
DROP INDEX memory_curation_requests_open_coalescing;
DROP INDEX memory_curation_requests_initial_idempotency;
DROP TABLE memory_curation_requests;
DROP TABLE memory_curation_cursors;
DROP TABLE memory_curation_lanes;

ALTER TABLE memories
    DROP CONSTRAINT memories_check,
    DROP COLUMN deleted_curation_mutation_count,
    DROP COLUMN deleted_curation_input_count,
    DROP COLUMN deleted_curation_action_count,
    DROP COLUMN deleted_curation_run_count;

ALTER TABLE memories ADD CONSTRAINT memories_check CHECK (
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
   delete_idempotency_key_hash ~ '^[0-9a-f]{64}$' AND deleted_prior_version >= 1 AND
   deleted_scrub_set_revision ~ '^[0-9a-f]{64}$' AND
   deleted_version_count = deleted_prior_version AND deleted_evidence_count >= 1 AND
   deleted_relation_count >= 0 AND deleted_retry_shield_count >= deleted_version_count AND
   deleted_retry_shield_digest ~ '^[0-9a-f]{64}$')
);
