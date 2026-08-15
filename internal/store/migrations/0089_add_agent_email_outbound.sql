-- +goose Up
-- Durable, provider-neutral outbound agent email. Callers can enqueue only one
-- plain-text recipient. The authenticated agent's canonical mailbox supplies
-- both the managed sending identity and Reply-To; neither is caller input.

CREATE TABLE agent_email_realm_send_controls (
    account_id    TEXT        NOT NULL,
    realm_id      TEXT        NOT NULL,
    send_state    TEXT        NOT NULL DEFAULT 'enabled',
    row_version   BIGINT      NOT NULL DEFAULT 1,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    disabled_at   TIMESTAMPTZ,
    PRIMARY KEY (account_id, realm_id),
    FOREIGN KEY (account_id, realm_id)
      REFERENCES realms (account_id, id) ON DELETE CASCADE,
    CHECK (send_state IN ('enabled', 'disabled')),
    CHECK (row_version BETWEEN 1 AND 4611686018427387903),
    CHECK (updated_at >= created_at),
    CHECK (
      (send_state = 'enabled' AND disabled_at IS NULL) OR
      (send_state = 'disabled' AND disabled_at IS NOT NULL)
    )
);

CREATE TABLE agent_email_send_controls (
    account_id     TEXT        NOT NULL,
    realm_id       TEXT        NOT NULL,
    owner_agent_id TEXT        NOT NULL,
    send_state     TEXT        NOT NULL DEFAULT 'enabled',
    row_version    BIGINT      NOT NULL DEFAULT 1,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    disabled_at    TIMESTAMPTZ,
    PRIMARY KEY (account_id, realm_id, owner_agent_id),
    FOREIGN KEY (account_id, realm_id)
      REFERENCES realms (account_id, id) ON DELETE CASCADE,
    FOREIGN KEY (realm_id, owner_agent_id)
      REFERENCES agents (realm_id, id) ON DELETE CASCADE,
    CHECK (send_state IN ('enabled', 'disabled')),
    CHECK (row_version BETWEEN 1 AND 4611686018427387903),
    CHECK (updated_at >= created_at),
    CHECK (
      (send_state = 'enabled' AND disabled_at IS NULL) OR
      (send_state = 'disabled' AND disabled_at IS NOT NULL)
    )
);

CREATE TABLE agent_email_outbound_messages (
    id                          TEXT        PRIMARY KEY,
    account_id                  TEXT        NOT NULL,
    realm_id                    TEXT        NOT NULL,
    owner_agent_id              TEXT        NOT NULL,
    address_id                  TEXT        NOT NULL,
    from_address                TEXT        NOT NULL,
    reply_to_address            TEXT        NOT NULL,
    to_address                  TEXT        NOT NULL,
    subject                     TEXT        NOT NULL DEFAULT '',
    body_text                   TEXT        NOT NULL,
    request_kind                TEXT        NOT NULL,
    reply_to_inbound_message_id TEXT,
    thread_key                  TEXT        NOT NULL,
    in_reply_to_header          TEXT,
    references_headers          TEXT[]      NOT NULL DEFAULT '{}'::text[],
    idempotency_key_hash        TEXT        NOT NULL,
    request_hash                TEXT        NOT NULL,
    state                       TEXT        NOT NULL DEFAULT 'queued',
    provider_state              TEXT        NOT NULL DEFAULT '',
    provider                    TEXT        NOT NULL DEFAULT '',
    provider_message_id         TEXT        NOT NULL DEFAULT '',
    last_error_code             TEXT        NOT NULL DEFAULT '',
    attempt_count               BIGINT      NOT NULL DEFAULT 0,
    claim_generation            BIGINT      NOT NULL DEFAULT 0,
    claim_id                    TEXT,
    lease_expires_at            TIMESTAMPTZ,
    next_attempt_at             TIMESTAMPTZ,
    queued_at                   TIMESTAMPTZ NOT NULL DEFAULT statement_timestamp(),
    provider_started_at         TIMESTAMPTZ,
    accepted_at                 TIMESTAMPTZ,
    delivered_at                TIMESTAMPTZ,
    deferred_at                 TIMESTAMPTZ,
    failed_at                   TIMESTAMPTZ,
    ambiguous_at                TIMESTAMPTZ,
    canceled_at                 TIMESTAMPTZ,
    created_at                  TIMESTAMPTZ NOT NULL DEFAULT statement_timestamp(),
    updated_at                  TIMESTAMPTZ NOT NULL DEFAULT statement_timestamp(),
    UNIQUE (account_id, id),
    UNIQUE (account_id, realm_id, owner_agent_id, id),
    UNIQUE (account_id, realm_id, owner_agent_id, idempotency_key_hash),
    FOREIGN KEY (account_id, realm_id)
      REFERENCES realms (account_id, id) ON DELETE CASCADE,
    FOREIGN KEY (realm_id, owner_agent_id)
      REFERENCES agents (realm_id, id) ON DELETE CASCADE,
    FOREIGN KEY (account_id, realm_id, owner_agent_id, address_id)
      REFERENCES agent_email_addresses
        (account_id, realm_id, provisioned_agent_id, id),
    CHECK (id ~ '^esnd_[a-z2-7]{16}$'),
    CHECK (octet_length(from_address) BETWEEN 3 AND 320 AND
           from_address = lower(from_address) AND
           from_address ~ '^[^[:space:]<>@]+@send[.]witmail[.]net$'),
    CHECK (octet_length(reply_to_address) BETWEEN 3 AND 320 AND
           reply_to_address = lower(reply_to_address) AND
           reply_to_address ~ '^[^[:space:]<>@]+@witmail[.]net$'),
    CHECK (split_part(from_address, '@', 1) =
           split_part(reply_to_address, '@', 1)),
    CHECK (octet_length(to_address) BETWEEN 3 AND 320 AND
           to_address !~ '[[:space:]<>]' AND
           to_address ~ '^[^@]+@[^@]+$'),
    CHECK (octet_length(subject) <= 4096 AND
           subject !~ '[[:cntrl:]]'),
    CHECK (octet_length(body_text) BETWEEN 1 AND 262144),
    CHECK (request_kind IN ('direct', 'reply')),
    CHECK (reply_to_inbound_message_id IS NULL OR
           reply_to_inbound_message_id ~ '^emsg_[a-z2-7]{16}$'),
    CHECK ((request_kind = 'direct' AND reply_to_inbound_message_id IS NULL) OR
           (request_kind = 'reply' AND reply_to_inbound_message_id IS NOT NULL)),
    CHECK (octet_length(thread_key) BETWEEN 1 AND 128),
    CHECK (in_reply_to_header IS NULL OR
           (octet_length(in_reply_to_header) BETWEEN 3 AND 998 AND
            in_reply_to_header ~ '^<[^<>[:space:]]+>$')),
    CHECK (cardinality(references_headers) <= 16),
    CHECK (idempotency_key_hash ~ '^[0-9a-f]{64}$'),
    CHECK (request_hash ~ '^[0-9a-f]{64}$'),
    CHECK (state IN (
      'queued','claimed','provider_started','accepted','delivered','deferred',
      'bounced','rejected','failed','ambiguous','canceled'
    )),
    CHECK (provider_state IN (
      '','accepted','delivered','deferred','bounced','rejected','failed'
    )),
    CHECK (provider = '' OR provider ~ '^[a-z][a-z0-9_.-]{0,63}$'),
    CHECK (octet_length(provider_message_id) <= 512),
    CHECK (last_error_code = '' OR
           last_error_code IN (
             'provider_unavailable','provider_rate_limited','provider_rejected',
             'provider_failed','provider_timeout','provider_connection_reset',
             'provider_response_invalid','recipient_hard_bounce',
             'recipient_complained','dispatch_canceled','worker_lease_expired'
           )),
    CHECK (attempt_count BETWEEN 0 AND 4611686018427387903),
    CHECK (claim_generation BETWEEN 0 AND 4611686018427387903),
    CHECK (claim_id IS NULL OR claim_id ~ '^escl_[a-z2-7]{16}$'),
    CHECK (updated_at >= created_at AND queued_at >= created_at),
    CHECK (
      (state = 'queued' AND provider_state = '' AND claim_id IS NULL AND
       lease_expires_at IS NULL AND next_attempt_at IS NOT NULL AND
       provider_started_at IS NULL AND accepted_at IS NULL AND
       delivered_at IS NULL AND deferred_at IS NULL AND failed_at IS NULL AND
       ambiguous_at IS NULL AND canceled_at IS NULL)
      OR
      (state = 'claimed' AND provider_state = '' AND claim_id IS NOT NULL AND
       claim_generation >= 1 AND lease_expires_at IS NOT NULL AND
       next_attempt_at IS NULL AND provider_started_at IS NULL AND
       accepted_at IS NULL AND delivered_at IS NULL AND deferred_at IS NULL AND
       failed_at IS NULL AND ambiguous_at IS NULL AND canceled_at IS NULL)
      OR
      (state = 'provider_started' AND provider_state = '' AND
       claim_id IS NOT NULL AND claim_generation >= 1 AND
       lease_expires_at IS NOT NULL AND next_attempt_at IS NULL AND
       provider_started_at IS NOT NULL AND accepted_at IS NULL AND
       delivered_at IS NULL AND deferred_at IS NULL AND failed_at IS NULL AND
       ambiguous_at IS NULL AND canceled_at IS NULL)
      OR
      (state IN ('accepted','delivered','deferred','bounced','rejected','failed') AND
       provider_state = state AND provider <> '' AND claim_id IS NULL AND
       lease_expires_at IS NULL AND next_attempt_at IS NULL AND
       provider_started_at IS NOT NULL AND ambiguous_at IS NULL AND
       canceled_at IS NULL)
      OR
      (state = 'ambiguous' AND provider_state = '' AND claim_id IS NULL AND
       lease_expires_at IS NULL AND next_attempt_at IS NULL AND
       provider_started_at IS NOT NULL AND ambiguous_at IS NOT NULL AND
       canceled_at IS NULL)
      OR
      (state = 'canceled' AND provider_state = '' AND claim_id IS NULL AND
       lease_expires_at IS NULL AND next_attempt_at IS NULL AND
       provider_started_at IS NULL AND canceled_at IS NOT NULL AND
       accepted_at IS NULL AND delivered_at IS NULL AND deferred_at IS NULL AND
       failed_at IS NULL AND ambiguous_at IS NULL)
    ),
    CHECK (state NOT IN ('accepted','delivered','deferred') OR
           (provider_message_id <> '' AND accepted_at IS NOT NULL)),
    CHECK (state <> 'delivered' OR delivered_at IS NOT NULL),
    CHECK (state <> 'deferred' OR deferred_at IS NOT NULL),
    CHECK (state NOT IN ('bounced','rejected','failed') OR
           (failed_at IS NOT NULL AND last_error_code <> ''))
);

CREATE UNIQUE INDEX agent_email_outbound_by_provider_message
    ON agent_email_outbound_messages (provider, provider_message_id)
 WHERE provider_message_id <> '';
CREATE INDEX agent_email_outbound_by_owner
    ON agent_email_outbound_messages
       (account_id, realm_id, owner_agent_id, created_at DESC, id DESC);
CREATE INDEX agent_email_outbound_claimable
    ON agent_email_outbound_messages (next_attempt_at, queued_at, id)
 WHERE state = 'queued';
CREATE INDEX agent_email_outbound_expired_claims
    ON agent_email_outbound_messages (lease_expires_at, id)
 WHERE state IN ('claimed', 'provider_started');
CREATE INDEX agent_email_outbound_provider_started_by_account
    ON agent_email_outbound_messages (account_id)
 WHERE state = 'provider_started';

-- Event ids are hashed before persistence. The row is an idempotency receipt
-- and normalized lifecycle fact, never a copy of provider payload or text.
CREATE TABLE agent_email_outbound_provider_events (
    account_id         TEXT        NOT NULL,
    provider           TEXT        NOT NULL,
    event_id_hash      TEXT        NOT NULL,
    event_request_hash TEXT        NOT NULL,
    outbound_id        TEXT        NOT NULL,
    event_class        TEXT        NOT NULL,
    occurred_at        TIMESTAMPTZ NOT NULL,
    received_at        TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (provider, event_id_hash),
    FOREIGN KEY (account_id, outbound_id)
      REFERENCES agent_email_outbound_messages (account_id, id) ON DELETE CASCADE,
    CHECK (provider ~ '^[a-z][a-z0-9_.-]{0,63}$'),
    CHECK (event_id_hash ~ '^[0-9a-f]{64}$'),
    CHECK (event_request_hash ~ '^[0-9a-f]{64}$'),
    CHECK (event_class IN
      ('delivered','deferred','bounced','failed','rejected','complained')),
    CHECK (occurred_at <= received_at + interval '5 minutes')
);
CREATE INDEX agent_email_outbound_provider_events_by_send
    ON agent_email_outbound_provider_events
       (outbound_id, occurred_at, provider, event_id_hash);

-- Suppression stores only an account-scoped SHA-256 recipient key. A hard
-- bounce or complaint cannot be bypassed by plan/account overrides; a later
-- operator workflow may add explicit review, but ordinary queue calls cannot.
CREATE TABLE agent_email_outbound_recipient_suppressions (
    account_id       TEXT        NOT NULL,
    recipient_sha256 TEXT        NOT NULL,
    reason           TEXT        NOT NULL,
    source_send_id   TEXT        NOT NULL,
    provider         TEXT        NOT NULL,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (account_id, recipient_sha256),
    FOREIGN KEY (account_id) REFERENCES accounts (id) ON DELETE CASCADE,
    CHECK (recipient_sha256 ~ '^[0-9a-f]{64}$'),
    CHECK (reason IN ('hard_bounce','complained')),
    CHECK (source_send_id ~ '^esnd_[a-z2-7]{16}$'),
    CHECK (provider ~ '^[a-z][a-z0-9_.-]{0,63}$'),
    CHECK (updated_at >= created_at)
);

-- Cell-local GCRA state. These operational buckets are deliberately omitted
-- from account archives; a moved account starts with fresh defensive debt.
CREATE TABLE agent_email_outbound_rate_buckets (
    account_id                       TEXT        NOT NULL,
    realm_id                         TEXT        NOT NULL,
    lane                             TEXT        NOT NULL,
    scope                            TEXT        NOT NULL,
    scope_id                         TEXT        NOT NULL,
    theoretical_arrival_microseconds BIGINT      NOT NULL,
    updated_at                       TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (account_id, realm_id, lane, scope, scope_id),
    FOREIGN KEY (account_id) REFERENCES accounts (id) ON DELETE CASCADE,
    CHECK (lane IN ('admission','dispatch')),
    CHECK (scope IN ('account','agent','realm')),
    CHECK ((scope = 'account' AND realm_id = '' AND scope_id = account_id) OR
           (scope = 'agent' AND realm_id <> '' AND
            octet_length(scope_id) BETWEEN 1 AND 128) OR
           (scope = 'realm' AND realm_id <> '' AND scope_id = realm_id)),
    CHECK (theoretical_arrival_microseconds > 0)
);
CREATE INDEX agent_email_outbound_rate_buckets_by_update
    ON agent_email_outbound_rate_buckets (updated_at);

-- +goose StatementBegin
CREATE FUNCTION witself_consume_agent_email_outbound_rate_bucket(
    p_account_id TEXT,
    p_realm_id TEXT,
    p_lane TEXT,
    p_scope TEXT,
    p_scope_id TEXT,
    p_interval_microseconds BIGINT,
    p_limit BIGINT
)
RETURNS TABLE (
    admitted BOOLEAN,
    current_tat BIGINT,
    now_microseconds BIGINT
)
LANGUAGE plpgsql
AS $$
DECLARE
    v_current_tat BIGINT;
    v_candidate_tat BIGINT;
BEGIN
    IF p_limit <= 0 THEN
        SELECT bucket.theoretical_arrival_microseconds
          INTO v_current_tat
          FROM agent_email_outbound_rate_buckets AS bucket
         WHERE bucket.account_id = p_account_id
           AND bucket.realm_id = p_realm_id
           AND bucket.lane = p_lane
           AND bucket.scope = p_scope
           AND bucket.scope_id = p_scope_id;
        SELECT floor(extract(epoch FROM clock_timestamp()) * 1000000)::bigint
          INTO now_microseconds;
        admitted := FALSE;
        current_tat := COALESCE(v_current_tat, now_microseconds);
        RETURN NEXT;
        RETURN;
    END IF;

    LOOP
        SELECT bucket.theoretical_arrival_microseconds
          INTO v_current_tat
          FROM agent_email_outbound_rate_buckets AS bucket
         WHERE bucket.account_id = p_account_id
           AND bucket.realm_id = p_realm_id
           AND bucket.lane = p_lane
           AND bucket.scope = p_scope
           AND bucket.scope_id = p_scope_id
         FOR UPDATE;
        EXIT WHEN FOUND;

        INSERT INTO agent_email_outbound_rate_buckets
          (account_id,realm_id,lane,scope,scope_id,
           theoretical_arrival_microseconds)
        VALUES (p_account_id,p_realm_id,p_lane,p_scope,p_scope_id,1)
        ON CONFLICT (account_id,realm_id,lane,scope,scope_id) DO NOTHING;
    END LOOP;

    SELECT floor(extract(epoch FROM clock_timestamp()) * 1000000)::bigint
      INTO now_microseconds;
    v_candidate_tat := GREATEST(v_current_tat, now_microseconds) +
        p_interval_microseconds;
    IF v_candidate_tat <= now_microseconds + p_limit * p_interval_microseconds THEN
        UPDATE agent_email_outbound_rate_buckets AS bucket
           SET theoretical_arrival_microseconds = v_candidate_tat,
               updated_at = clock_timestamp()
         WHERE bucket.account_id = p_account_id
           AND bucket.realm_id = p_realm_id
           AND bucket.lane = p_lane
           AND bucket.scope = p_scope
           AND bucket.scope_id = p_scope_id;
        admitted := TRUE;
        current_tat := v_candidate_tat;
    ELSE
        admitted := FALSE;
        current_tat := v_current_tat;
    END IF;
    RETURN NEXT;
END;
$$;
-- +goose StatementEnd

-- Every durable tenant table added after schema 0070 attaches the same cell
-- evacuation barrier. Operational rate buckets are intentionally excluded.
CREATE TRIGGER account_evacuation_fence
BEFORE INSERT OR UPDATE OR DELETE ON agent_email_realm_send_controls
FOR EACH ROW EXECUTE FUNCTION witself_tenant_evacuation_fence();
CREATE TRIGGER account_evacuation_fence
BEFORE INSERT OR UPDATE OR DELETE ON agent_email_send_controls
FOR EACH ROW EXECUTE FUNCTION witself_tenant_evacuation_fence();
CREATE TRIGGER account_evacuation_fence
BEFORE INSERT OR UPDATE OR DELETE ON agent_email_outbound_messages
FOR EACH ROW EXECUTE FUNCTION witself_tenant_evacuation_fence();
CREATE TRIGGER account_evacuation_fence
BEFORE INSERT OR UPDATE OR DELETE ON agent_email_outbound_provider_events
FOR EACH ROW EXECUTE FUNCTION witself_tenant_evacuation_fence();
CREATE TRIGGER account_evacuation_fence
BEFORE INSERT OR UPDATE OR DELETE ON agent_email_outbound_recipient_suppressions
FOR EACH ROW EXECUTE FUNCTION witself_tenant_evacuation_fence();

-- +goose Down
-- Schema 0088 cannot represent durable outbound controls, queued or delivered
-- sends, provider receipts, or recipient suppressions. Fence every writer and
-- refuse before dropping any of that state. Rate buckets are cell-local,
-- reconstructible abuse-control debt and may be discarded on an otherwise
-- empty downgrade.
LOCK TABLE agent_email_realm_send_controls,
           agent_email_send_controls,
           agent_email_outbound_messages,
           agent_email_outbound_provider_events,
           agent_email_outbound_recipient_suppressions
    IN ACCESS EXCLUSIVE MODE;

-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM agent_email_realm_send_controls)
       OR EXISTS (SELECT 1 FROM agent_email_send_controls)
       OR EXISTS (SELECT 1 FROM agent_email_outbound_messages)
       OR EXISTS (SELECT 1 FROM agent_email_outbound_provider_events)
       OR EXISTS (SELECT 1 FROM agent_email_outbound_recipient_suppressions)
    THEN
        RAISE EXCEPTION USING
            ERRCODE = '55000',
            MESSAGE = 'cannot downgrade schema 0089 while durable outbound agent-email state exists';
    END IF;
END;
$$;
-- +goose StatementEnd

DROP TRIGGER account_evacuation_fence
    ON agent_email_outbound_recipient_suppressions;
DROP TRIGGER account_evacuation_fence
    ON agent_email_outbound_provider_events;
DROP TRIGGER account_evacuation_fence ON agent_email_outbound_messages;
DROP TRIGGER account_evacuation_fence ON agent_email_send_controls;
DROP TRIGGER account_evacuation_fence ON agent_email_realm_send_controls;
DROP FUNCTION witself_consume_agent_email_outbound_rate_bucket(
    TEXT, TEXT, TEXT, TEXT, TEXT, BIGINT, BIGINT
);
DROP TABLE agent_email_outbound_rate_buckets;
DROP TABLE agent_email_outbound_recipient_suppressions;
DROP TABLE agent_email_outbound_provider_events;
DROP TABLE agent_email_outbound_messages;
DROP TABLE agent_email_send_controls;
DROP TABLE agent_email_realm_send_controls;
