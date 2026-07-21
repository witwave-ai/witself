-- +goose Up
-- Synthetic provider-retry proof. Arm/proof columns store only a SHA-256
-- challenge digest; delivery_fingerprint_sha256 binds the first temporary
-- result to the exact signed raw body plus normalized SMTP envelope. The
-- opaque UUID header remains ordinary accepted synthetic raw_mime under the
-- mailbox/archive retention policy. At most one unused armed challenge exists
-- per mailbox. Retained tempfailed proofs remain independently retryable
-- through their grace window and do not wedge later runs. Accepted rows remain
-- beside their message so a lost success response cannot create a duplicate
-- on a later provider replay.

CREATE TABLE agent_email_retry_canary_arms (
    account_id                  TEXT        NOT NULL,
    realm_id                    TEXT        NOT NULL,
    mailbox_id                  TEXT        NOT NULL,
    owner_agent_id              TEXT        NOT NULL,
    challenge_sha256            TEXT        NOT NULL,
    state                       TEXT        NOT NULL DEFAULT 'armed',
    delivery_fingerprint_sha256 TEXT,
    accepted_message_id         TEXT,
    tempfail_count              SMALLINT    NOT NULL DEFAULT 0,
    row_version                 BIGINT      NOT NULL DEFAULT 1,
    armed_at                    TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    expires_at                  TIMESTAMPTZ NOT NULL,
    tempfailed_at               TIMESTAMPTZ,
    retry_expires_at            TIMESTAMPTZ,
    accepted_at                 TIMESTAMPTZ,
    PRIMARY KEY (account_id, realm_id, mailbox_id, challenge_sha256),
    FOREIGN KEY (account_id, realm_id, owner_agent_id, mailbox_id)
      REFERENCES agent_email_mailboxes
        (account_id, realm_id, owner_agent_id, id)
      ON DELETE CASCADE,
    FOREIGN KEY (account_id, realm_id, mailbox_id, owner_agent_id,
                 accepted_message_id)
      REFERENCES agent_email_messages
        (account_id, realm_id, mailbox_id, owner_agent_id, id)
      ON DELETE CASCADE,
    CHECK (challenge_sha256 ~ '^[0-9a-f]{64}$'),
    CHECK (delivery_fingerprint_sha256 IS NULL OR
           delivery_fingerprint_sha256 ~ '^[0-9a-f]{64}$'),
    CHECK (state IN ('armed', 'tempfailed', 'accepted', 'expired')),
    CHECK (tempfail_count BETWEEN 0 AND 1),
    CHECK (row_version BETWEEN 1 AND 4611686018427387903),
    CHECK (expires_at > armed_at),
    CHECK (tempfailed_at IS NULL OR tempfailed_at >= armed_at),
    CHECK (retry_expires_at IS NULL OR
           (tempfailed_at IS NOT NULL AND retry_expires_at > tempfailed_at)),
    CHECK (accepted_at IS NULL OR
           (tempfailed_at IS NOT NULL AND accepted_at >= tempfailed_at)),
    CHECK (
      (state = 'armed' AND delivery_fingerprint_sha256 IS NULL AND
       accepted_message_id IS NULL AND tempfail_count = 0 AND
       tempfailed_at IS NULL AND retry_expires_at IS NULL AND accepted_at IS NULL)
      OR
      (state = 'tempfailed' AND delivery_fingerprint_sha256 IS NOT NULL AND
       accepted_message_id IS NULL AND tempfail_count = 1 AND
       tempfailed_at IS NOT NULL AND retry_expires_at IS NOT NULL AND
       accepted_at IS NULL)
      OR
      (state = 'accepted' AND delivery_fingerprint_sha256 IS NOT NULL AND
       accepted_message_id IS NOT NULL AND tempfail_count = 1 AND
       tempfailed_at IS NOT NULL AND retry_expires_at IS NOT NULL AND
       accepted_at IS NOT NULL)
      OR
      (state = 'expired' AND accepted_message_id IS NULL AND accepted_at IS NULL AND
       ((delivery_fingerprint_sha256 IS NULL AND tempfail_count = 0 AND
         tempfailed_at IS NULL AND retry_expires_at IS NULL)
        OR
        (delivery_fingerprint_sha256 IS NOT NULL AND tempfail_count = 1 AND
         tempfailed_at IS NOT NULL AND retry_expires_at IS NOT NULL)))
    )
);

CREATE UNIQUE INDEX agent_email_retry_canary_one_live_arm
    ON agent_email_retry_canary_arms
       (account_id, realm_id, mailbox_id)
 WHERE state = 'armed';

-- +goose Down
DROP TABLE agent_email_retry_canary_arms;
