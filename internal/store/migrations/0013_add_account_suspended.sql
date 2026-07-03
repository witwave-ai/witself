-- +goose Up
-- Account suspension: a REVERSIBLE freeze that stops every write while
-- preserving reads and credentials. Owner-initiated for now (suspended_for
-- = 'owner_request'); the same columns will accept 'fleet_admin',
-- 'migration', 'billing', 'policy' when those slices land — the store's
-- resume path refuses anything but the category that matches the actor.
ALTER TABLE accounts ADD COLUMN suspended_at TIMESTAMPTZ;
ALTER TABLE accounts ADD COLUMN suspended_for TEXT;
ALTER TABLE accounts ADD COLUMN suspended_reason TEXT;

-- +goose Down
ALTER TABLE accounts DROP COLUMN suspended_reason;
ALTER TABLE accounts DROP COLUMN suspended_for;
ALTER TABLE accounts DROP COLUMN suspended_at;
