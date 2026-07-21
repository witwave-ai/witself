-- +goose Up
-- Realm receive is a durable aggregate, not replicated mailbox state. The row
-- survives a zero-live-mailbox interval so a disabled realm cannot silently
-- re-enable when a mailbox is later provisioned. Mailbox receive_state remains
-- the independent per-agent control.

CREATE TABLE agent_email_realm_receive_controls (
    account_id    TEXT        NOT NULL,
    realm_id      TEXT        NOT NULL,
    receive_state TEXT        NOT NULL DEFAULT 'enabled',
    row_version   BIGINT      NOT NULL DEFAULT 1,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    disabled_at   TIMESTAMPTZ,
    PRIMARY KEY (account_id, realm_id),
    FOREIGN KEY (account_id, realm_id)
      REFERENCES realms (account_id, id) ON DELETE CASCADE,
    CHECK (receive_state IN ('enabled', 'disabled')),
    CHECK (row_version BETWEEN 1 AND 4611686018427387903),
    CHECK (updated_at >= created_at),
    CHECK (disabled_at IS NULL OR disabled_at >= created_at),
    CHECK (
      (receive_state = 'enabled' AND disabled_at IS NULL) OR
      (receive_state = 'disabled' AND disabled_at IS NOT NULL)
    )
);

INSERT INTO agent_email_realm_receive_controls (account_id, realm_id)
SELECT DISTINCT account_id, realm_id
FROM agent_email_mailboxes;

-- +goose Down
DROP TABLE agent_email_realm_receive_controls;
