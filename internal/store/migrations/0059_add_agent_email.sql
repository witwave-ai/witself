-- +goose Up
-- Receive-only agent email. Address reservations deliberately outlive their
-- agent principal, while mailboxes and their content remain realm-scoped and
-- cascade with the owning agent. Raw MIME is retained inline under the
-- capability-limited pilot's 5 MiB ceiling.

CREATE TABLE agent_email_addresses (
    id                     TEXT        PRIMARY KEY,
    account_id             TEXT        NOT NULL,
    realm_id               TEXT        NOT NULL,
    provisioned_agent_id   TEXT        NOT NULL,
    domain                 TEXT        NOT NULL,
    agent_segment          TEXT        NOT NULL,
    realm_label            TEXT        NOT NULL,
    local_part             TEXT        NOT NULL,
    provisioning_kind      TEXT        NOT NULL,
    created_at             TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    retired_at             TIMESTAMPTZ,
    retirement_reason_code TEXT,
    UNIQUE (account_id, realm_id, id),
    UNIQUE (account_id, realm_id, provisioned_agent_id, id),
    UNIQUE (domain, local_part),
    FOREIGN KEY (account_id, realm_id)
      REFERENCES realms (account_id, id) ON DELETE CASCADE,
    CHECK (id ~ '^eaddr_[a-z2-7]{16}$'),
    CHECK (octet_length(provisioned_agent_id) BETWEEN 1 AND 128),
    CHECK (octet_length(domain) BETWEEN 1 AND 253 AND domain = lower(domain)),
    CHECK (domain ~ '^[a-z0-9]([a-z0-9.-]{0,251}[a-z0-9])?$' AND
           position('..' IN domain) = 0),
    CHECK (agent_segment ~ '^[a-z0-9]([a-z0-9-]{0,45}[a-z0-9])?$'),
    CHECK (realm_label ~ '^[a-z2-7]{16}$'),
    CHECK (realm_id = 'realm_' || realm_label),
    CHECK (local_part = agent_segment || '.' || realm_label AND
           octet_length(local_part) <= 64),
    CHECK (provisioning_kind IN ('derived', 'operator_override')),
    CHECK (
      (retired_at IS NULL AND retirement_reason_code IS NULL) OR
      (retired_at IS NOT NULL AND retired_at >= created_at AND
       retirement_reason_code ~ '^[a-z][a-z0-9_.-]{0,63}$')
    )
);

-- Mailbox uniqueness below selects at most one current reservation for an
-- agent. This partial lookup also makes the live-or-tombstoned collision
-- check cheap; global domain/local-part uniqueness permanently prevents
-- address reuse.
CREATE UNIQUE INDEX agent_email_addresses_live_by_agent
    ON agent_email_addresses (account_id, realm_id, provisioned_agent_id)
 WHERE retired_at IS NULL;

CREATE TABLE agent_email_mailboxes (
    id             TEXT        PRIMARY KEY,
    account_id     TEXT        NOT NULL,
    realm_id       TEXT        NOT NULL,
    owner_agent_id TEXT        NOT NULL,
    address_id     TEXT        NOT NULL,
    receive_state  TEXT        NOT NULL,
    row_version    BIGINT      NOT NULL DEFAULT 1,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    disabled_at    TIMESTAMPTZ,
    retired_at     TIMESTAMPTZ,
    UNIQUE (account_id, realm_id, id),
    UNIQUE (account_id, realm_id, owner_agent_id, id),
    UNIQUE (account_id, realm_id, owner_agent_id, id, address_id),
    UNIQUE (account_id, realm_id, owner_agent_id),
    UNIQUE (account_id, realm_id, address_id),
    FOREIGN KEY (account_id, realm_id)
      REFERENCES realms (account_id, id) ON DELETE CASCADE,
    FOREIGN KEY (realm_id, owner_agent_id)
      REFERENCES agents (realm_id, id) ON DELETE CASCADE,
    FOREIGN KEY (account_id, realm_id, owner_agent_id, address_id)
      REFERENCES agent_email_addresses
        (account_id, realm_id, provisioned_agent_id, id),
    CHECK (id ~ '^emb_[a-z2-7]{16}$'),
    CHECK (receive_state IN ('enabled', 'disabled', 'retired')),
    CHECK (row_version BETWEEN 1 AND 4611686018427387903),
    CHECK (updated_at >= created_at),
    CHECK (disabled_at IS NULL OR disabled_at >= created_at),
    CHECK (retired_at IS NULL OR retired_at >= created_at),
    CHECK (disabled_at IS NULL OR retired_at IS NULL OR retired_at >= disabled_at),
    CHECK (
      (receive_state = 'enabled' AND disabled_at IS NULL AND retired_at IS NULL) OR
      (receive_state = 'disabled' AND disabled_at IS NOT NULL AND retired_at IS NULL) OR
      (receive_state = 'retired' AND retired_at IS NOT NULL)
    )
);

CREATE TABLE agent_email_messages (
    id                               TEXT        PRIMARY KEY,
    account_id                       TEXT        NOT NULL,
    realm_id                         TEXT        NOT NULL,
    mailbox_id                       TEXT        NOT NULL,
    owner_agent_id                   TEXT        NOT NULL,
    address_id                       TEXT        NOT NULL,
    provider                         TEXT        NOT NULL,
    provider_message_id              TEXT,
    envelope_sender                  TEXT        NOT NULL,
    envelope_recipient               TEXT        NOT NULL,
    agent_segment                    TEXT        NOT NULL,
    realm_label                      TEXT        NOT NULL,
    subaddress_tag                   TEXT,
    raw_mime                         BYTEA       NOT NULL,
    raw_size_bytes                   BIGINT      NOT NULL,
    raw_sha256                       TEXT        NOT NULL,
    parse_state                      TEXT        NOT NULL DEFAULT 'pending',
    parse_error                      TEXT,
    header_from                      TEXT,
    header_to                        TEXT,
    header_subject                   TEXT,
    mime_message_id                  TEXT,
    message_date                     TIMESTAMPTZ,
    attachment_count                 BIGINT      NOT NULL DEFAULT 0,
    spf_result                       TEXT,
    dkim_result                      TEXT,
    dmarc_result                     TEXT,
    spam_verdict                     TEXT,
    sender_verification_state        TEXT        NOT NULL DEFAULT 'unverified',
    duplicate_group_sha256           TEXT        NOT NULL,
    possible_duplicate_of_message_id TEXT,
    received_at                      TIMESTAMPTZ NOT NULL,
    created_at                       TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    UNIQUE (account_id, realm_id, mailbox_id, owner_agent_id, id),
    FOREIGN KEY (account_id, realm_id, owner_agent_id, mailbox_id, address_id)
      REFERENCES agent_email_mailboxes
        (account_id, realm_id, owner_agent_id, id, address_id)
      ON DELETE CASCADE,
    FOREIGN KEY (account_id, realm_id, owner_agent_id, address_id)
      REFERENCES agent_email_addresses
        (account_id, realm_id, provisioned_agent_id, id),
    FOREIGN KEY (account_id, realm_id, mailbox_id, owner_agent_id,
                 possible_duplicate_of_message_id)
      REFERENCES agent_email_messages
        (account_id, realm_id, mailbox_id, owner_agent_id, id)
      DEFERRABLE INITIALLY DEFERRED,
    CHECK (id ~ '^emsg_[a-z2-7]{16}$'),
    CHECK (provider ~ '^[a-z][a-z0-9_.-]{0,63}$'),
    CHECK (provider_message_id IS NULL OR
           octet_length(provider_message_id) BETWEEN 1 AND 512),
    CHECK (octet_length(envelope_sender) <= 320),
    CHECK (octet_length(envelope_recipient) BETWEEN 1 AND 320 AND
           envelope_recipient = lower(envelope_recipient)),
    CHECK (agent_segment ~ '^[a-z0-9]([a-z0-9-]{0,45}[a-z0-9])?$'),
    CHECK (realm_label ~ '^[a-z2-7]{16}$'),
    CHECK (subaddress_tag IS NULL OR octet_length(subaddress_tag) BETWEEN 1 AND 64),
    CHECK (raw_size_bytes BETWEEN 1 AND 5242880 AND
           raw_size_bytes = octet_length(raw_mime)),
    CHECK (raw_sha256 ~ '^[0-9a-f]{64}$'),
    CHECK (parse_state IN ('pending', 'parsed', 'error')),
    CHECK (
      (parse_state IN ('pending', 'parsed') AND parse_error IS NULL) OR
      (parse_state = 'error' AND octet_length(parse_error) BETWEEN 1 AND 1024)
    ),
    CHECK (header_from IS NULL OR octet_length(header_from) <= 4096),
    CHECK (header_to IS NULL OR octet_length(header_to) <= 4096),
    CHECK (header_subject IS NULL OR octet_length(header_subject) <= 4096),
    CHECK (mime_message_id IS NULL OR octet_length(mime_message_id) <= 998),
    CHECK (attachment_count BETWEEN 0 AND 10000),
    CHECK (spf_result IS NULL OR spf_result IN
      ('unknown', 'none', 'neutral', 'pass', 'fail', 'softfail', 'temperror', 'permerror')),
    CHECK (dkim_result IS NULL OR dkim_result IN
      ('unknown', 'none', 'neutral', 'pass', 'fail', 'policy', 'temperror', 'permerror')),
    CHECK (dmarc_result IS NULL OR dmarc_result IN
      ('unknown', 'none', 'pass', 'fail', 'temperror', 'permerror')),
    CHECK (spam_verdict IS NULL OR spam_verdict IN
      ('unknown', 'clean', 'spam', 'suspected_spam')),
    CHECK (sender_verification_state IN ('unverified', 'verified')),
    CHECK (
      provider <> 'cloudflare_email_routing' OR
      (provider_message_id IS NULL AND
       spf_result IS NOT DISTINCT FROM 'unknown' AND
       dkim_result IS NOT DISTINCT FROM 'unknown' AND
       dmarc_result IS NOT DISTINCT FROM 'unknown' AND
       spam_verdict IS NOT DISTINCT FROM 'unknown' AND
       sender_verification_state = 'unverified')
    ),
    CHECK (duplicate_group_sha256 ~ '^[0-9a-f]{64}$'),
    CHECK (possible_duplicate_of_message_id IS NULL OR
           possible_duplicate_of_message_id <> id)
);

-- Provider identity is optional in the Cloudflare pilot. When a future edge
-- supplies one authoritatively, it is an idempotency key for that provider
-- and normalized envelope recipient. The pilot's digest grouping remains
-- deliberately non-unique so suspected duplicates are preserved.
CREATE UNIQUE INDEX agent_email_messages_provider_dedupe
    ON agent_email_messages
       (account_id, realm_id, provider, provider_message_id, envelope_recipient)
 WHERE provider_message_id IS NOT NULL;
CREATE INDEX agent_email_messages_duplicate_group
    ON agent_email_messages
       (account_id, realm_id, mailbox_id, duplicate_group_sha256,
        received_at, id);
CREATE INDEX agent_email_messages_by_mailbox_received
    ON agent_email_messages
       (account_id, realm_id, mailbox_id, received_at DESC, id DESC);

CREATE TABLE agent_email_deliveries (
    message_id            TEXT        NOT NULL,
    account_id            TEXT        NOT NULL,
    realm_id              TEXT        NOT NULL,
    mailbox_id            TEXT        NOT NULL,
    owner_agent_id        TEXT        NOT NULL,
    folder                TEXT        NOT NULL DEFAULT 'inbox',
    delivered_at          TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    read_at               TIMESTAMPTZ,
    acked_at              TIMESTAMPTZ,
    code_consumed_at      TIMESTAMPTZ,
    processing_state      TEXT        NOT NULL DEFAULT 'available',
    processing_generation BIGINT      NOT NULL DEFAULT 0,
    failure_count         BIGINT      NOT NULL DEFAULT 0,
    claim_id              TEXT,
    claim_key_hash        TEXT        NOT NULL DEFAULT '',
    lease_expires_at      TIMESTAMPTZ,
    completed_at          TIMESTAMPTZ,
    complete_key_hash     TEXT        NOT NULL DEFAULT '',
    created_at            TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (message_id, mailbox_id),
    FOREIGN KEY (account_id, realm_id, mailbox_id, owner_agent_id, message_id)
      REFERENCES agent_email_messages
        (account_id, realm_id, mailbox_id, owner_agent_id, id)
      ON DELETE CASCADE,
    FOREIGN KEY (account_id, realm_id, owner_agent_id, mailbox_id)
      REFERENCES agent_email_mailboxes
        (account_id, realm_id, owner_agent_id, id)
      ON DELETE CASCADE,
    CHECK (folder IN ('inbox', 'quarantine')),
    CHECK (read_at IS NULL OR read_at >= delivered_at),
    CHECK (acked_at IS NULL OR (read_at IS NOT NULL AND acked_at >= read_at)),
    CHECK (code_consumed_at IS NULL OR
           (read_at IS NOT NULL AND code_consumed_at >= read_at)),
    CHECK (processing_generation BETWEEN 0 AND 4611686018427387903),
    CHECK (failure_count BETWEEN 0 AND 4611686018427387903),
    CONSTRAINT agent_email_deliveries_processing_shape CHECK (
      (processing_state = 'available' AND
       claim_id IS NULL AND claim_key_hash = '' AND lease_expires_at IS NULL AND
       completed_at IS NULL AND complete_key_hash = '')
      OR
      (processing_state = 'claimed' AND processing_generation >= 1 AND
       claim_id ~ '^ecl_[A-Za-z0-9_-]+$' AND
       claim_key_hash ~ '^[0-9a-f]{64}$' AND lease_expires_at IS NOT NULL AND
       completed_at IS NULL AND complete_key_hash = '')
      OR
      (processing_state = 'completed' AND processing_generation >= 1 AND
       claim_id ~ '^ecl_[A-Za-z0-9_-]+$' AND
       claim_key_hash ~ '^[0-9a-f]{64}$' AND lease_expires_at IS NULL AND
       completed_at IS NOT NULL AND complete_key_hash ~ '^[0-9a-f]{64}$')
    )
);

CREATE INDEX agent_email_deliveries_by_owner
    ON agent_email_deliveries
       (account_id, realm_id, owner_agent_id, delivered_at DESC, message_id DESC);
CREATE INDEX agent_email_deliveries_checkpoint
    ON agent_email_deliveries
       (account_id, realm_id, owner_agent_id, delivered_at, message_id)
 WHERE folder = 'inbox' AND acked_at IS NULL;
CREATE INDEX agent_email_deliveries_claimable
    ON agent_email_deliveries
       (account_id, realm_id, owner_agent_id, processing_state,
        lease_expires_at, delivered_at, message_id)
 WHERE folder = 'inbox' AND acked_at IS NULL;

-- +goose Down
DROP TABLE agent_email_deliveries;
DROP TABLE agent_email_messages;
DROP TABLE agent_email_mailboxes;
DROP TABLE agent_email_addresses;
