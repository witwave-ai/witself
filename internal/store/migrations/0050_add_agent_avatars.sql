-- +goose Up
-- Agent avatars keep a mutable, optimistic-concurrency guarded profile over
-- immutable visual versions. Style packs are realm-scoped and independently
-- versioned so a mixed human/animal/insect team can share one visual grammar
-- without rewriting historical avatars when that grammar evolves.

CREATE TABLE avatar_style_packs (
    account_id      TEXT        NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    realm_id        TEXT        NOT NULL REFERENCES realms(id) ON DELETE CASCADE,
    id              TEXT        NOT NULL,
    current_version INTEGER     NOT NULL,
    revision        BIGINT      NOT NULL DEFAULT 1,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (account_id, realm_id, id),
    CHECK (id ~ '^[a-z][a-z0-9_-]{0,127}$'),
    CHECK (current_version >= 1),
    CHECK (revision >= 1)
);

CREATE TABLE avatar_style_pack_versions (
    account_id         TEXT        NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    realm_id           TEXT        NOT NULL REFERENCES realms(id) ON DELETE CASCADE,
    style_pack_id      TEXT        NOT NULL,
    version            INTEGER     NOT NULL,
    previous_version   INTEGER,
    name               TEXT        NOT NULL,
    description        TEXT        NOT NULL,
    style_spec         JSONB       NOT NULL,
    reference_examples JSONB       NOT NULL DEFAULT '[]'::jsonb,
    provenance         JSONB       NOT NULL DEFAULT '{}'::jsonb,
    created_by_kind    TEXT        NOT NULL,
    created_by_id      TEXT        NOT NULL DEFAULT '',
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (account_id, realm_id, style_pack_id, version),
    FOREIGN KEY (account_id, realm_id, style_pack_id)
      REFERENCES avatar_style_packs (account_id, realm_id, id)
      ON DELETE CASCADE DEFERRABLE INITIALLY DEFERRED,
    FOREIGN KEY (account_id, realm_id, style_pack_id, previous_version)
      REFERENCES avatar_style_pack_versions
        (account_id, realm_id, style_pack_id, version)
      DEFERRABLE INITIALLY DEFERRED,
    CHECK (version >= 1),
    CHECK (previous_version IS NULL OR
           (previous_version >= 1 AND previous_version < version)),
    CHECK (octet_length(name) BETWEEN 1 AND 256),
    CHECK (octet_length(description) BETWEEN 1 AND 8192),
    CHECK (jsonb_typeof(style_spec) = 'object' AND
           octet_length(style_spec::text) <= 65536),
    CHECK (jsonb_typeof(reference_examples) = 'array' AND
           octet_length(reference_examples::text) <= 262144),
    CHECK (jsonb_typeof(provenance) = 'object' AND
           octet_length(provenance::text) <= 16384),
    CHECK (created_by_kind IN ('operator', 'system')),
    CHECK ((created_by_kind = 'system' AND created_by_id = '') OR
           (created_by_kind <> 'system' AND octet_length(created_by_id) BETWEEN 1 AND 128))
);

ALTER TABLE avatar_style_packs
  ADD CONSTRAINT avatar_style_packs_current_version_fk
  FOREIGN KEY (account_id, realm_id, id, current_version)
  REFERENCES avatar_style_pack_versions
    (account_id, realm_id, style_pack_id, version)
  DEFERRABLE INITIALLY DEFERRED;

CREATE TABLE realm_avatar_styles (
    account_id         TEXT        NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    realm_id           TEXT        NOT NULL REFERENCES realms(id) ON DELETE CASCADE,
    style_pack_id      TEXT        NOT NULL,
    style_pack_version INTEGER     NOT NULL,
    revision           BIGINT      NOT NULL DEFAULT 1,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (account_id, realm_id),
    FOREIGN KEY (account_id, realm_id, style_pack_id, style_pack_version)
      REFERENCES avatar_style_pack_versions
        (account_id, realm_id, style_pack_id, version)
      DEFERRABLE INITIALLY DEFERRED,
    CHECK (revision >= 1)
);

-- Seed the built-in style grammar for existing realms. The application owns
-- richer rendering guidance, while this persisted baseline makes the selected
-- pack/version portable and gives every profile a valid durable reference.
INSERT INTO avatar_style_packs
       (account_id, realm_id, id, current_version)
SELECT account_id, id, 'witself-flat-portrait', 1
  FROM realms;

INSERT INTO avatar_style_pack_versions
       (account_id, realm_id, style_pack_id, version, name, description,
        style_spec, reference_examples, provenance, created_by_kind)
SELECT account_id, id, 'witself-flat-portrait', 1,
       'Witself Flat Portrait',
       'A consistent flat-vector portrait system for human, animal, insect, and other agent forms.',
       '{
          "canvas":{"width":512,"height":512,"view_box":"0 0 512 512"},
          "crop":"head_and_shoulders",
          "pose":"front_three_quarter",
          "palette":{"maximum_colors":12},
          "layers":["background","base-identity","expression","attire","experience"],
          "subject_forms":["human","animal","insect","anthropomorphic","hybrid","robot","symbolic"]
        }'::jsonb,
       '[]'::jsonb,
       '{"source":"witself.builtin","revision":"1"}'::jsonb,
       'system'
  FROM realms;

INSERT INTO realm_avatar_styles
       (account_id, realm_id, style_pack_id, style_pack_version)
SELECT account_id, id, 'witself-flat-portrait', 1
  FROM realms;

CREATE TABLE agent_avatar_profiles (
    account_id              TEXT        NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    realm_id                TEXT        NOT NULL REFERENCES realms(id) ON DELETE CASCADE,
    agent_id                TEXT        PRIMARY KEY REFERENCES agents(id) ON DELETE CASCADE,
    status                  TEXT        NOT NULL DEFAULT 'generation_due',
    autonomy_policy         TEXT        NOT NULL DEFAULT 'agent_self_managed',
    style_pack_id           TEXT        NOT NULL DEFAULT 'witself-flat-portrait',
    style_pack_version      INTEGER     NOT NULL DEFAULT 1,
    latest_avatar_version   INTEGER,
    proposed_avatar_version INTEGER,
    active_avatar_version   INTEGER,
    lineage_generation      INTEGER     NOT NULL DEFAULT 1,
    subject_form            TEXT        NOT NULL DEFAULT 'human',
    attempt_count           INTEGER     NOT NULL DEFAULT 0,
    retry_after             TIMESTAMPTZ,
    fallback_seed           TEXT        NOT NULL,
    failure_code            TEXT        NOT NULL DEFAULT '',
    revision                BIGINT      NOT NULL DEFAULT 1,
    created_at              TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at              TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (account_id, realm_id, agent_id),
    FOREIGN KEY (account_id, realm_id, style_pack_id, style_pack_version)
      REFERENCES avatar_style_pack_versions
        (account_id, realm_id, style_pack_id, version),
    CHECK (status IN (
      'placeholder', 'generation_due', 'proposed', 'active',
      'evolution_due', 'rejected', 'generation_failed', 'archived'
    )),
    CHECK (autonomy_policy IN
      ('operator_only', 'agent_proposes', 'agent_self_managed')),
    CHECK (latest_avatar_version IS NULL OR latest_avatar_version >= 1),
    CHECK (proposed_avatar_version IS NULL OR proposed_avatar_version >= 1),
    CHECK (active_avatar_version IS NULL OR active_avatar_version >= 1),
    CHECK (lineage_generation >= 1),
    CHECK (subject_form IN
      ('human', 'animal', 'insect', 'anthropomorphic', 'hybrid',
       'robot', 'symbolic')),
    CHECK (attempt_count BETWEEN 0 AND 1000000),
    CHECK (fallback_seed = agent_id),
    CHECK (octet_length(failure_code) <= 128),
    CHECK (revision >= 1),
    CHECK ((status = 'generation_failed' AND failure_code <> '' AND
            retry_after IS NOT NULL AND attempt_count >= 1) OR
           (status <> 'generation_failed' AND failure_code = '' AND
            retry_after IS NULL))
);

CREATE TABLE agent_avatar_versions (
    account_id         TEXT        NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    realm_id           TEXT        NOT NULL REFERENCES realms(id) ON DELETE CASCADE,
    agent_id           TEXT        NOT NULL,
    id                 TEXT        NOT NULL UNIQUE,
    version            INTEGER     NOT NULL,
    parent_version     INTEGER,
    lineage_generation INTEGER     NOT NULL DEFAULT 1,
    style_pack_id      TEXT        NOT NULL,
    style_pack_version INTEGER     NOT NULL,
    subject_form       TEXT        NOT NULL,
    svg                 TEXT        NOT NULL,
    description         TEXT        NOT NULL,
    visual_spec         JSONB       NOT NULL,
    svg_sha256          TEXT        NOT NULL,
    provenance          JSONB       NOT NULL DEFAULT '{}'::jsonb,
    proposed_by_kind    TEXT        NOT NULL,
    proposed_by_id      TEXT        NOT NULL,
    proposed_at         TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (account_id, realm_id, agent_id, version),
    UNIQUE (account_id, realm_id, agent_id, version, lineage_generation),
    FOREIGN KEY (account_id, realm_id, agent_id)
      REFERENCES agent_avatar_profiles (account_id, realm_id, agent_id)
      ON DELETE CASCADE DEFERRABLE INITIALLY DEFERRED,
    FOREIGN KEY (account_id, realm_id, agent_id, parent_version,
                 lineage_generation)
      REFERENCES agent_avatar_versions
        (account_id, realm_id, agent_id, version, lineage_generation)
      DEFERRABLE INITIALLY DEFERRED,
    FOREIGN KEY (account_id, realm_id, style_pack_id, style_pack_version)
      REFERENCES avatar_style_pack_versions
        (account_id, realm_id, style_pack_id, version),
    CHECK (version >= 1),
    CHECK (lineage_generation >= 1),
    CHECK (id ~ '^avver_[a-z2-7]{16}$'),
    CHECK (parent_version IS NULL OR
           (parent_version >= 1 AND parent_version < version)),
    CHECK (subject_form IN
      ('human', 'animal', 'insect', 'anthropomorphic', 'hybrid',
       'robot', 'symbolic')),
    CHECK (octet_length(svg) BETWEEN 1 AND 65536),
    CHECK (octet_length(description) BETWEEN 1 AND 4096),
    CHECK (jsonb_typeof(visual_spec) = 'object' AND
           octet_length(visual_spec::text) <= 16384),
    CHECK (svg_sha256 ~ '^[0-9a-f]{64}$'),
    CHECK (jsonb_typeof(provenance) = 'object' AND
           octet_length(provenance::text) <= 16384),
    CHECK (proposed_by_kind IN ('agent', 'operator')),
    CHECK (octet_length(proposed_by_id) BETWEEN 1 AND 128)
);

ALTER TABLE agent_avatar_profiles
  ADD CONSTRAINT agent_avatar_profiles_latest_version_fk
  FOREIGN KEY (account_id, realm_id, agent_id, latest_avatar_version)
  REFERENCES agent_avatar_versions (account_id, realm_id, agent_id, version)
  DEFERRABLE INITIALLY DEFERRED;

ALTER TABLE agent_avatar_profiles
  ADD CONSTRAINT agent_avatar_profiles_proposed_version_fk
  FOREIGN KEY (account_id, realm_id, agent_id, proposed_avatar_version,
               lineage_generation)
  REFERENCES agent_avatar_versions
    (account_id, realm_id, agent_id, version, lineage_generation)
  DEFERRABLE INITIALLY DEFERRED;

ALTER TABLE agent_avatar_profiles
  ADD CONSTRAINT agent_avatar_profiles_active_version_fk
  FOREIGN KEY (account_id, realm_id, agent_id, active_avatar_version,
               lineage_generation)
  REFERENCES agent_avatar_versions
    (account_id, realm_id, agent_id, version, lineage_generation)
  DEFERRABLE INITIALLY DEFERRED;

CREATE TABLE agent_avatar_activations (
    id                    TEXT        PRIMARY KEY,
    account_id            TEXT        NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    realm_id              TEXT        NOT NULL REFERENCES realms(id) ON DELETE CASCADE,
    agent_id              TEXT        NOT NULL,
    sequence              BIGINT      NOT NULL,
    lineage_generation    INTEGER     NOT NULL DEFAULT 1,
    avatar_version        INTEGER     NOT NULL,
    prior_active_version  INTEGER,
    action                TEXT        NOT NULL,
    activated_by_kind     TEXT        NOT NULL,
    activated_by_id       TEXT        NOT NULL,
    activated_at          TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    FOREIGN KEY (account_id, realm_id, agent_id, avatar_version,
                 lineage_generation)
      REFERENCES agent_avatar_versions
        (account_id, realm_id, agent_id, version, lineage_generation)
      ON DELETE CASCADE,
    FOREIGN KEY (account_id, realm_id, agent_id, prior_active_version,
                 lineage_generation)
      REFERENCES agent_avatar_versions
        (account_id, realm_id, agent_id, version, lineage_generation),
    CHECK (id ~ '^avact_[a-z2-7]{16}$'),
    CHECK (sequence >= 1),
    CHECK (lineage_generation >= 1),
    CHECK (avatar_version >= 1),
    CHECK (prior_active_version IS NULL OR prior_active_version >= 1),
    CHECK (action IN ('activated', 'rolled_back')),
    CHECK (activated_by_kind IN ('agent', 'operator')),
    CHECK (octet_length(activated_by_id) BETWEEN 1 AND 128)
);

CREATE UNIQUE INDEX agent_avatar_activations_by_sequence
    ON agent_avatar_activations (account_id, realm_id, agent_id, sequence);

CREATE INDEX agent_avatar_activations_by_version
    ON agent_avatar_activations
       (account_id, realm_id, agent_id, avatar_version, activated_at DESC);

CREATE TABLE agent_avatar_rejections (
    id                    TEXT        PRIMARY KEY,
    account_id            TEXT        NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    realm_id              TEXT        NOT NULL REFERENCES realms(id) ON DELETE CASCADE,
    agent_id              TEXT        NOT NULL,
    avatar_version        INTEGER     NOT NULL,
    reason_code           TEXT        NOT NULL DEFAULT '',
    rejected_by_kind      TEXT        NOT NULL,
    rejected_by_id        TEXT        NOT NULL,
    rejected_at           TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    FOREIGN KEY (account_id, realm_id, agent_id, avatar_version)
      REFERENCES agent_avatar_versions
        (account_id, realm_id, agent_id, version)
      ON DELETE CASCADE,
    CHECK (id ~ '^avrej_[a-z2-7]{16}$'),
    CHECK (avatar_version >= 1),
    CHECK (reason_code = '' OR reason_code ~ '^[a-z][a-z0-9_.-]{0,127}$'),
    CHECK (rejected_by_kind IN ('agent', 'operator')),
    CHECK (octet_length(rejected_by_id) BETWEEN 1 AND 128)
);

CREATE UNIQUE INDEX agent_avatar_rejections_one_per_version
    ON agent_avatar_rejections (account_id, realm_id, agent_id, avatar_version);

-- Reset is non-destructive: immutable avatar/version/activation history stays
-- in place while the profile advances to a fresh lineage with no active or
-- proposed pointer. This ledger makes each boundary explicit and auditable.
CREATE TABLE agent_avatar_resets (
    id                           TEXT        PRIMARY KEY,
    account_id                   TEXT        NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    realm_id                     TEXT        NOT NULL REFERENCES realms(id) ON DELETE CASCADE,
    agent_id                     TEXT        NOT NULL,
    sequence                     BIGINT      NOT NULL,
    retired_lineage_generation   INTEGER     NOT NULL,
    new_lineage_generation       INTEGER     NOT NULL,
    retired_active_version       INTEGER,
    retired_proposed_version     INTEGER,
    reset_by_kind                TEXT        NOT NULL,
    reset_by_id                  TEXT        NOT NULL,
    reason_code                  TEXT        NOT NULL DEFAULT '',
    reset_at                     TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    UNIQUE (account_id, realm_id, agent_id, retired_lineage_generation),
    UNIQUE (account_id, realm_id, agent_id, new_lineage_generation),
    FOREIGN KEY (account_id, realm_id, agent_id)
      REFERENCES agent_avatar_profiles (account_id, realm_id, agent_id)
      ON DELETE CASCADE,
    FOREIGN KEY (account_id, realm_id, agent_id, retired_active_version,
                 retired_lineage_generation)
      REFERENCES agent_avatar_versions
        (account_id, realm_id, agent_id, version, lineage_generation),
    FOREIGN KEY (account_id, realm_id, agent_id, retired_proposed_version,
                 retired_lineage_generation)
      REFERENCES agent_avatar_versions
        (account_id, realm_id, agent_id, version, lineage_generation),
    CHECK (id ~ '^avrst_[a-z2-7]{16}$'),
    CHECK (sequence >= 1),
    CHECK (sequence = retired_lineage_generation),
    CHECK (retired_lineage_generation >= 1),
    CHECK (new_lineage_generation = retired_lineage_generation + 1),
    CHECK (retired_active_version IS NULL OR retired_active_version >= 1),
    CHECK (retired_proposed_version IS NULL OR retired_proposed_version >= 1),
    CHECK (retired_active_version IS NOT NULL OR
           retired_proposed_version IS NOT NULL),
    CHECK (reset_by_kind IN ('agent', 'operator')),
    CHECK (octet_length(reset_by_id) BETWEEN 1 AND 128),
    CHECK (reason_code = '' OR reason_code ~ '^[a-z][a-z0-9_.-]{0,127}$')
);

CREATE UNIQUE INDEX agent_avatar_resets_by_sequence
    ON agent_avatar_resets (account_id, realm_id, agent_id, sequence);

-- Mutation receipts give every profile/style mutation a bounded retry key and
-- request fingerprint. They intentionally contain no SVG, prose, visual spec,
-- prompt, or other avatar value.
CREATE TABLE avatar_mutation_receipts (
    account_id       TEXT        NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    realm_id         TEXT        NOT NULL REFERENCES realms(id) ON DELETE CASCADE,
    target_kind      TEXT        NOT NULL,
    target_id        TEXT        NOT NULL,
    actor_kind       TEXT        NOT NULL,
    actor_id         TEXT        NOT NULL,
    operation        TEXT        NOT NULL,
    idempotency_key  TEXT        NOT NULL,
    request_hash     TEXT        NOT NULL,
    result_revision  BIGINT      NOT NULL,
    result_version   INTEGER,
    result_lineage_generation INTEGER,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY
      (account_id, realm_id, target_kind, target_id,
       actor_kind, actor_id, operation, idempotency_key),
    CHECK (target_kind IN ('avatar', 'style_pack')),
    CHECK (actor_kind IN ('agent', 'operator')),
    CHECK (octet_length(actor_id) BETWEEN 1 AND 128),
    CHECK (operation IN
      ('propose', 'activate', 'reject', 'rollback', 'reset', 'set_policy', 'set_style', 'fail')),
    CHECK (octet_length(idempotency_key) BETWEEN 1 AND 512),
    CHECK (request_hash ~ '^[0-9a-f]{64}$'),
    CHECK (result_revision >= 1),
    CHECK (result_version IS NULL OR result_version >= 1),
    CHECK (result_lineage_generation IS NULL OR result_lineage_generation >= 1),
    CHECK ((operation = 'reset' AND result_version IS NULL AND
            result_lineage_generation IS NOT NULL) OR
           (operation <> 'reset' AND result_lineage_generation IS NULL))
);

-- Build operational indexes before the profile backfill. The profile/version
-- pointer constraints are deferred, and PostgreSQL refuses CREATE INDEX while
-- those backfill rows still have pending deferred constraint-trigger events.
CREATE INDEX agent_avatar_profiles_by_realm_status
    ON agent_avatar_profiles (account_id, realm_id, status, updated_at, agent_id);
CREATE INDEX agent_avatar_versions_by_agent_latest
    ON agent_avatar_versions (account_id, realm_id, agent_id, version DESC);

INSERT INTO agent_avatar_profiles
       (account_id, realm_id, agent_id, fallback_seed)
SELECT r.account_id, a.realm_id, a.id, a.id
  FROM agents a
  JOIN realms r ON r.id = a.realm_id
ON CONFLICT (agent_id) DO NOTHING;

-- +goose Down
DROP INDEX agent_avatar_versions_by_agent_latest;
DROP INDEX agent_avatar_profiles_by_realm_status;
DROP TABLE avatar_mutation_receipts;
DROP INDEX agent_avatar_resets_by_sequence;
DROP TABLE agent_avatar_resets;
DROP INDEX agent_avatar_rejections_one_per_version;
DROP TABLE agent_avatar_rejections;
DROP INDEX agent_avatar_activations_by_version;
DROP INDEX agent_avatar_activations_by_sequence;
DROP TABLE agent_avatar_activations;
ALTER TABLE agent_avatar_profiles DROP CONSTRAINT agent_avatar_profiles_active_version_fk;
ALTER TABLE agent_avatar_profiles DROP CONSTRAINT agent_avatar_profiles_proposed_version_fk;
ALTER TABLE agent_avatar_profiles DROP CONSTRAINT agent_avatar_profiles_latest_version_fk;
DROP TABLE agent_avatar_versions;
DROP TABLE agent_avatar_profiles;
DROP TABLE realm_avatar_styles;
ALTER TABLE avatar_style_packs DROP CONSTRAINT avatar_style_packs_current_version_fk;
DROP TABLE avatar_style_pack_versions;
DROP TABLE avatar_style_packs;
