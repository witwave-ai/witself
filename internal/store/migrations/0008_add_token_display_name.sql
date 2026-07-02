-- +goose Up
-- Human-readable token label for list/show surfaces. Raw token values are still
-- returned once and only the token hash is stored.
ALTER TABLE tokens ADD COLUMN display_name TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE tokens DROP COLUMN display_name;
