-- +goose Up
-- Agent tokens reference the agent they authenticate (operator/bootstrap tokens
-- leave it null).
ALTER TABLE tokens ADD COLUMN agent_id TEXT REFERENCES agents(id);

-- +goose Down
ALTER TABLE tokens DROP COLUMN agent_id;
