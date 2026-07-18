-- +goose Up
-- Realm style selection is immediate, but large profile projections are
-- reconciled by a durable, restart-safe worker in bounded transactions. A
-- nullable profile revision lets existing rows be stamped lazily by that
-- worker instead of forcing a deployment-time full-table backfill.

CREATE TABLE avatar_style_rollout_jobs (
    account_id              TEXT        NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    realm_id                TEXT        NOT NULL REFERENCES realms(id) ON DELETE CASCADE,
    style_revision          BIGINT      NOT NULL,
    style_pack_id           TEXT        NOT NULL,
    style_pack_version      INTEGER     NOT NULL,
    status                  TEXT        NOT NULL DEFAULT 'pending',
    -- NULL means the open rollout has not finished discovering its bounded
    -- targets. Terminal jobs finalize this to the number of live profiles
    -- actually projected; publish never scans the realm synchronously.
    target_profile_count    BIGINT,
    processed_profile_count BIGINT      NOT NULL DEFAULT 0,
    batch_count             BIGINT      NOT NULL DEFAULT 0,
    last_batch_size         INTEGER     NOT NULL DEFAULT 0,
    failure_count           INTEGER     NOT NULL DEFAULT 0,
    retry_after             TIMESTAMPTZ,
    last_failure_code       TEXT        NOT NULL DEFAULT '',
    created_at              TIMESTAMPTZ NOT NULL DEFAULT statement_timestamp(),
    started_at              TIMESTAMPTZ,
    updated_at              TIMESTAMPTZ NOT NULL DEFAULT statement_timestamp(),
    completed_at            TIMESTAMPTZ,
    superseded_at           TIMESTAMPTZ,
    PRIMARY KEY (account_id, realm_id, style_revision),
    FOREIGN KEY (account_id, realm_id, style_pack_id, style_pack_version)
      REFERENCES avatar_style_pack_versions
        (account_id, realm_id, style_pack_id, version),
    CHECK (style_revision >= 1),
    CHECK (style_pack_version >= 1),
    CHECK (status IN ('pending', 'running', 'completed', 'superseded')),
    CHECK (target_profile_count IS NULL OR target_profile_count >= 0),
    CHECK (processed_profile_count >= 0),
    CHECK (target_profile_count IS NULL OR
           processed_profile_count <= target_profile_count),
    CHECK (batch_count >= 0),
    CHECK (last_batch_size BETWEEN 0 AND 1000),
    CHECK (last_batch_size <= processed_profile_count),
    CHECK (failure_count BETWEEN 0 AND 1000000),
    CHECK (octet_length(last_failure_code) <= 64),
    CHECK ((failure_count = 0 AND retry_after IS NULL AND
            last_failure_code = '') OR
           (failure_count > 0 AND status IN ('pending', 'running') AND
            retry_after IS NOT NULL AND last_failure_code <> '')),
    CHECK ((status = 'pending' AND started_at IS NULL AND
            completed_at IS NULL AND superseded_at IS NULL AND
            target_profile_count IS NULL AND
            processed_profile_count = 0 AND batch_count = 0 AND
            last_batch_size = 0) OR
           (status = 'running' AND started_at IS NOT NULL AND
            completed_at IS NULL AND superseded_at IS NULL AND
            target_profile_count IS NULL AND batch_count >= 1) OR
           (status = 'completed' AND started_at IS NOT NULL AND
            completed_at IS NOT NULL AND superseded_at IS NULL AND
            target_profile_count = processed_profile_count AND
            batch_count >= 1) OR
           (status = 'superseded' AND completed_at IS NULL AND
            superseded_at IS NOT NULL AND
            target_profile_count = processed_profile_count AND
            ((started_at IS NULL AND batch_count = 0 AND
              processed_profile_count = 0 AND last_batch_size = 0) OR
             (started_at IS NOT NULL AND batch_count >= 1))))
);

CREATE INDEX avatar_style_rollout_jobs_worker
    ON avatar_style_rollout_jobs
       (updated_at, created_at, account_id, realm_id, style_revision)
    WHERE status IN ('pending', 'running');

CREATE UNIQUE INDEX avatar_style_rollout_jobs_one_open_per_realm
    ON avatar_style_rollout_jobs (account_id, realm_id)
    WHERE status IN ('pending', 'running');

-- Keep the profile-table ACCESS EXCLUSIVE section metadata-only and last in
-- the transaction. NOT VALID enforces the check for every new/changed row
-- without scanning a potentially large existing table under this lock; the
-- following migration validates it under PostgreSQL's lower-strength
-- SHARE UPDATE EXCLUSIVE lock before building the concurrent lookup index.
ALTER TABLE agent_avatar_profiles
    ADD COLUMN style_revision BIGINT;
ALTER TABLE agent_avatar_profiles
    ADD CONSTRAINT agent_avatar_profiles_style_revision_positive
    CHECK (style_revision IS NULL OR style_revision >= 1) NOT VALID;

-- +goose Down
-- A pre-rollout server cannot finish a partial projection. The table lock is
-- transaction-held and fences publishers/workers across the check-and-drop
-- sequence, so no open job can appear after the safety checks.
LOCK TABLE avatar_style_rollout_jobs IN ACCESS EXCLUSIVE MODE;

-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (
        SELECT 1
          FROM avatar_style_rollout_jobs
         WHERE status IN ('pending', 'running')
    ) THEN
        RAISE EXCEPTION 'cannot remove avatar style rollouts while a rollout is pending or running';
    END IF;

    IF EXISTS (
        SELECT 1
          FROM agent_avatar_profiles p
          JOIN agents a
            ON a.id = p.agent_id
           AND a.realm_id = p.realm_id
           AND a.deleted_at IS NULL
          JOIN realms r
            ON r.id = p.realm_id
           AND r.account_id = p.account_id
           AND r.deleted_at IS NULL
          JOIN realm_avatar_styles ras
            ON ras.account_id = p.account_id
           AND ras.realm_id = p.realm_id
         WHERE (p.style_pack_id, p.style_pack_version) IS DISTINCT FROM
               (ras.style_pack_id, ras.style_pack_version)
    ) THEN
        RAISE EXCEPTION 'cannot remove avatar style rollouts while a live profile differs from its selected realm style';
    END IF;
END $$;
-- +goose StatementEnd

DROP TABLE avatar_style_rollout_jobs;
ALTER TABLE agent_avatar_profiles
    DROP CONSTRAINT agent_avatar_profiles_style_revision_positive;
ALTER TABLE agent_avatar_profiles DROP COLUMN style_revision;
