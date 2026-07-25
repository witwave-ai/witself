-- +goose Up
-- Cell-local signup idempotency. The provision id is minted by the control
-- plane before it contacts a cell. A receipt binds that opaque operation to
-- one exact normalized request, pending account, root operator, and currently
-- unclaimed bootstrap-token row. Only token hashes live in `tokens`; plaintext
-- bootstrap credentials are never persisted.
--
-- This table is deliberately excluded from account archives. It describes the
-- source cell's response-recovery state, not portable tenant data.
CREATE TABLE account_provision_receipts (
    provision_id            TEXT PRIMARY KEY,
    -- Deliberately no FK: the receipt must outlive source-account/token purge
    -- so a delayed retry cannot recreate the same operation as a new account.
    account_id              TEXT NOT NULL UNIQUE,
    -- SHA-256 of an unambiguous canonical encoding of provision_id plus the
    -- normalized email/display name. The durable receipt carries no contact
    -- address or display-name plaintext after account deletion.
    request_fingerprint     TEXT NOT NULL,
    bootstrap_token_id      TEXT NOT NULL UNIQUE,
    issue_count             BIGINT NOT NULL DEFAULT 1,
    created_at              TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_issued_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT account_provision_receipts_id_check CHECK (
        provision_id ~ '^[A-Za-z0-9_-]{1,128}$'
    ),
    CONSTRAINT account_provision_receipts_fingerprint_check CHECK (
        request_fingerprint ~ '^[0-9a-f]{64}$'
    ),
    CONSTRAINT account_provision_receipts_issue_count_check CHECK (
        issue_count >= 1
    )
);

-- +goose Down
-- A receipt is the durable idempotency shield for a caller-stable signup
-- operation. Removing a nonempty table would allow a delayed retry after a
-- later re-upgrade to create a second account, because duplicate contact
-- emails are intentionally valid. Refuse that irreversible downgrade.
LOCK TABLE account_provision_receipts IN ACCESS EXCLUSIVE MODE;

-- +goose StatementBegin
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM account_provision_receipts) THEN
    RAISE EXCEPTION 'cannot remove account provision receipts while provision receipts exist';
  END IF;
END;
$$;
-- +goose StatementEnd

DROP TABLE account_provision_receipts;
