-- +goose NO TRANSACTION
-- +goose Up
-- Existing avatar-profile tables may be large. Build the lazy projection
-- index without holding a write-blocking table lock for the duration.
ALTER TABLE agent_avatar_profiles
    VALIDATE CONSTRAINT agent_avatar_profiles_style_revision_positive;

-- A concurrent build can finish while Goose is interrupted before recording
-- the version, or leave an invalid same-named index when PostgreSQL cancels
-- it. Cleanup makes either retry state deterministic.
DROP INDEX CONCURRENTLY IF EXISTS agent_avatar_profiles_by_style_revision;
CREATE INDEX CONCURRENTLY agent_avatar_profiles_by_style_revision
    ON agent_avatar_profiles
       (account_id, realm_id, (COALESCE(style_revision, 0)), agent_id);

-- +goose Down
DROP INDEX CONCURRENTLY IF EXISTS agent_avatar_profiles_by_style_revision;
