-- +goose Up
-- Account close: a permanent tombstone, never a deletion. Closed accounts keep
-- their row (and history) forever; routing and credentials die at close time.
ALTER TABLE accounts ADD COLUMN closed_at TIMESTAMPTZ;
ALTER TABLE accounts ADD COLUMN closed_reason TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE accounts DROP COLUMN closed_reason;
ALTER TABLE accounts DROP COLUMN closed_at;
