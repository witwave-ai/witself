-- +goose Up
-- The same evacuation id exists on both sides of a move. Persist the side's
-- role so a delayed or misrouted source-finalization request can never purge
-- the imported target. NULL remains accepted only for an in-flight marker
-- written by a schema-70 server during a rolling upgrade; exact protocol-v1
-- operations require a non-NULL role in store code and fail closed otherwise.
ALTER TABLE accounts
    ADD COLUMN evacuation_role TEXT,
    ADD CONSTRAINT accounts_evacuation_role_chk CHECK (
        evacuation_role IS NULL OR
        evacuation_role IN ('source', 'target')
    ),
    ADD CONSTRAINT accounts_evacuation_role_marker_chk CHECK (
        evacuation_role IS NULL OR evacuation_id IS NOT NULL
    );

-- Source finalization removes the source account row, so its retry receipt
-- cannot reference accounts. This is cell-local coordination state, excluded
-- from logical account archives, and intentionally survives a later restore
-- of the same account id so a stale finalize retry cannot touch that new row.
CREATE TABLE account_evacuation_finalizations (
    account_id       TEXT        NOT NULL,
    evacuation_id    TEXT        NOT NULL,
    source_status    TEXT        NOT NULL,
    finalized_at     TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (account_id, evacuation_id),
    CHECK (evacuation_id ~ '^[A-Za-z0-9_-]{1,128}$'),
    CHECK (source_status IN ('suspended', 'closed'))
);

CREATE INDEX account_evacuation_finalizations_by_account
    ON account_evacuation_finalizations (account_id, finalized_at DESC);

-- +goose Down
-- The source-finalization receipt is the stale-request shield for a later
-- restored account with the same id, while evacuation_role distinguishes the
-- source row from the imported target during one live epoch. Serialize with
-- lifecycle writers and refuse to erase either kind of durable authority.
LOCK TABLE account_evacuation_finalizations IN ACCESS EXCLUSIVE MODE;
LOCK TABLE accounts IN ACCESS EXCLUSIVE MODE;

-- +goose StatementBegin
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM account_evacuation_finalizations) THEN
    RAISE EXCEPTION 'cannot remove account evacuation finalizations while finalization receipts exist';
  END IF;
  IF EXISTS (SELECT 1 FROM accounts WHERE evacuation_id IS NOT NULL) THEN
    RAISE EXCEPTION 'cannot remove account evacuation roles while evacuations are active';
  END IF;
END;
$$;
-- +goose StatementEnd

DROP TABLE account_evacuation_finalizations;

ALTER TABLE accounts
    DROP CONSTRAINT accounts_evacuation_role_marker_chk,
    DROP CONSTRAINT accounts_evacuation_role_chk,
    DROP COLUMN evacuation_role;
