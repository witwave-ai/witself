-- +goose Up
-- Closed accounts retain a 30-day recovery/export window before their tenant
-- content is erased. The account row then remains as a value-free tombstone;
-- purged_at is the durable fence that makes that transition idempotent.
ALTER TABLE accounts
  ADD COLUMN purged_at TIMESTAMPTZ,
  ADD CONSTRAINT accounts_purged_requires_closed_check CHECK (
    purged_at IS NULL OR closed_at IS NOT NULL
  );

CREATE INDEX accounts_pending_purge_by_closed_at
  ON accounts (closed_at, id)
  WHERE status = 'closed' AND purged_at IS NULL;

-- The closed-and-due population is expected to stay tiny, so one cell-local
-- cursor is sufficient to serialize preview/enforcement selection across
-- replicas. The 16-lane retention schemes exist for large policy scans and
-- would add needless durable coordination here.
CREATE TABLE account_purge_sweep_state (
    singleton               BOOLEAN     PRIMARY KEY DEFAULT TRUE,
    preview_account_cursor  TEXT        NOT NULL DEFAULT '',
    enforce_account_cursor  TEXT        NOT NULL DEFAULT '',
    generation              BIGINT      NOT NULL DEFAULT 0,
    next_run_at             TIMESTAMPTZ NOT NULL DEFAULT '-infinity'::timestamptz,
    updated_at              TIMESTAMPTZ NOT NULL DEFAULT statement_timestamp(),
    CHECK (singleton),
    CHECK (octet_length(preview_account_cursor) <= 512),
    CHECK (octet_length(enforce_account_cursor) <= 512),
    CHECK (generation BETWEEN 0 AND 4611686018427387903)
);

INSERT INTO account_purge_sweep_state (singleton)
VALUES (TRUE)
ON CONFLICT (singleton) DO NOTHING;

-- +goose Down
DROP TABLE account_purge_sweep_state;
DROP INDEX accounts_pending_purge_by_closed_at;
ALTER TABLE accounts
  DROP CONSTRAINT accounts_purged_requires_closed_check,
  DROP COLUMN purged_at;
