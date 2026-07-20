-- +goose Up
-- Per-agent dashboard UI preferences (ADR 0004). One row per agent, written
-- only by the agent itself through the dashboard's single deliberate write
-- path. The prefs document is a strictly validated, size-capped JSON object
-- ({"schema":"witself.dashboard-prefs.v1","theme":...}); the store-side
-- validator owns the key contract while these CHECKs are the defense-in-depth
-- floor. No revision machinery: UI preferences are low-stakes last-write-wins.

CREATE TABLE agent_dashboard_preferences (
    agent_id   TEXT        PRIMARY KEY REFERENCES agents (id) ON DELETE CASCADE,
    account_id TEXT        NOT NULL,
    realm_id   TEXT        NOT NULL,
    prefs      JSONB       NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    -- Denormalized scope columns prove the complete account -> realm -> agent
    -- relationship through the scoped keys added by migration 0055, matching
    -- the sealed-table house pattern.
    FOREIGN KEY (account_id, realm_id)
      REFERENCES realms (account_id, id) ON DELETE CASCADE,
    FOREIGN KEY (realm_id, agent_id)
      REFERENCES agents (realm_id, id) ON DELETE CASCADE,
    CHECK (jsonb_typeof(prefs) = 'object'),
    CHECK (octet_length(prefs::text) <= 4096)
);

-- +goose Down
DROP TABLE agent_dashboard_preferences;
