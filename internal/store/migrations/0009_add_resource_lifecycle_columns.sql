-- +goose Up
-- Lifecycle metadata for operator-managed resources. Deleted resources stay in
-- the database as tombstones so future audit/history surfaces can explain who
-- existed and when credentials were revoked.
ALTER TABLE operators ADD COLUMN updated_at TIMESTAMPTZ NOT NULL DEFAULT now();
ALTER TABLE operators ADD COLUMN deleted_at TIMESTAMPTZ;

ALTER TABLE realms ADD COLUMN updated_at TIMESTAMPTZ NOT NULL DEFAULT now();
ALTER TABLE realms ADD COLUMN deleted_at TIMESTAMPTZ;

ALTER TABLE agents ADD COLUMN updated_at TIMESTAMPTZ NOT NULL DEFAULT now();
ALTER TABLE agents ADD COLUMN deleted_at TIMESTAMPTZ;

-- +goose Down
ALTER TABLE agents DROP COLUMN deleted_at;
ALTER TABLE agents DROP COLUMN updated_at;

ALTER TABLE realms DROP COLUMN deleted_at;
ALTER TABLE realms DROP COLUMN updated_at;

ALTER TABLE operators DROP COLUMN deleted_at;
ALTER TABLE operators DROP COLUMN updated_at;
