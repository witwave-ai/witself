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
-- Recorded consent cannot be discarded while its value-free audit event
-- survives; refuse the downgrade instead of silently erasing the accepted
-- legal versions and timestamp.
LOCK TABLE accounts IN ACCESS EXCLUSIVE MODE;

-- +goose StatementBegin
DO $$
BEGIN
  IF EXISTS (
    SELECT 1 FROM accounts
     WHERE consent_terms_version IS NOT NULL
        OR consent_privacy_version IS NOT NULL
        OR consent_recorded_at IS NOT NULL
  ) THEN
    RAISE EXCEPTION
      'cannot downgrade schema 0094 while recorded account consent exists';
  END IF;
END;
$$;
-- +goose StatementEnd

ALTER TABLE accounts
  DROP COLUMN consent_terms_version,
  DROP COLUMN consent_privacy_version,
  DROP COLUMN consent_recorded_at;
