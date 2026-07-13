-- +goose Up
-- Recurrence is explicit assertion metadata. This first contract intentionally
-- supports only annual projection for date values; empty means non-recurring.
ALTER TABLE fact_assertions
    ADD COLUMN recurrence TEXT NOT NULL DEFAULT ''
    CHECK (recurrence = '' OR (recurrence = 'annual' AND value_type = 'date'));

ALTER TABLE fact_candidates
    ADD COLUMN recurrence TEXT NOT NULL DEFAULT ''
    CHECK (recurrence = '' OR (recurrence = 'annual' AND value_type = 'date'));

-- +goose Down
ALTER TABLE fact_candidates DROP COLUMN recurrence;
ALTER TABLE fact_assertions DROP COLUMN recurrence;
