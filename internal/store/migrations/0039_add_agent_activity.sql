-- +goose Up
-- Agent activity is a latest-only, server-observed projection. A row represents
-- the latest accepted client event for one agent/runtime/installation tuple;
-- it is deliberately not an availability or presence lease.
CREATE TABLE agent_activity (
    agent_id                  TEXT        NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
    runtime                   TEXT        NOT NULL,
    location_id               TEXT        NOT NULL,
    location                  TEXT        NOT NULL DEFAULT '',
    last_event                TEXT        NOT NULL,
    last_event_id             TEXT        NOT NULL,
    last_event_occurred_at    TIMESTAMPTZ NOT NULL,
    last_activity_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (agent_id, runtime, location_id),
    CHECK (octet_length(runtime) BETWEEN 1 AND 128),
    CHECK (octet_length(location_id) BETWEEN 1 AND 128),
    CHECK (octet_length(location) <= 256),
    CHECK (octet_length(last_event) BETWEEN 1 AND 128),
    CHECK (octet_length(last_event_id) BETWEEN 1 AND 128),
    CHECK (last_activity_at >= last_event_occurred_at)
);

CREATE INDEX agent_activity_by_agent_latest
    ON agent_activity (agent_id, last_activity_at DESC, runtime, location_id);

-- +goose Down
DROP TABLE agent_activity;
