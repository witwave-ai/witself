-- +goose Up
-- Cloud account provisioning: signup accounts carry the owner's email and a
-- lifecycle status. The seeded default account keeps a null email. Status is
-- 'active' in v0; email-verification and billing slices add pending states.
ALTER TABLE accounts ADD COLUMN email TEXT;
ALTER TABLE accounts ADD COLUMN status TEXT NOT NULL DEFAULT 'active';

-- One account per email per cell (case-insensitive).
CREATE UNIQUE INDEX accounts_email_unique ON accounts (lower(email)) WHERE email IS NOT NULL;

-- +goose Down
DROP INDEX accounts_email_unique;
ALTER TABLE accounts DROP COLUMN status;
ALTER TABLE accounts DROP COLUMN email;
