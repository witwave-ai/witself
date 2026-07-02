-- +goose Up
-- Decision (2026-07-02): multiple accounts may share an email. Email is contact
-- info, not identity — identity is tokens and account ids. Future
-- lookup-by-email flows return a list, not a single account.
DROP INDEX accounts_email_unique;

-- +goose Down
CREATE UNIQUE INDEX accounts_email_unique ON accounts (lower(email)) WHERE email IS NOT NULL;
