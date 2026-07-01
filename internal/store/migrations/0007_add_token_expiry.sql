-- +goose Up
-- Bootstrap tokens expire; durable operator and agent tokens leave this null
-- until explicit TTL support lands for those token kinds.
ALTER TABLE tokens ADD COLUMN expires_at TIMESTAMPTZ;

-- +goose Down
ALTER TABLE tokens DROP COLUMN expires_at;
