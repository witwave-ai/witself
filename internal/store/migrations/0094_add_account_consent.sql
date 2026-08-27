-- +goose Up
-- Schema 0094: dark ToS/privacy consent capture at account creation. When a
-- signup request carries consent versions, the control plane binds them to the
-- durable provision request and the cell stores them here beside the account;
-- when the request carries none, every byte of today's behavior — including
-- the provision request fingerprint — is unchanged. The columns are additive
-- and nullable (no CHECK), so existing rows, mixed-version replicas, and
-- archives written before this schema need no transformation; recording
-- begins only when a signup explicitly opts in.
ALTER TABLE accounts
  ADD COLUMN consent_terms_version TEXT,
  ADD COLUMN consent_privacy_version TEXT,
  ADD COLUMN consent_recorded_at TIMESTAMPTZ;

-- +goose Down
ALTER TABLE accounts
  DROP COLUMN consent_terms_version,
  DROP COLUMN consent_privacy_version,
  DROP COLUMN consent_recorded_at;
