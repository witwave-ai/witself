-- +goose Up
-- Open message requests are message-backed, same-realm coordination records.
-- Candidate offers and ranking remain client-owned; PostgreSQL owns only the
-- immutable audience snapshot, selection capacity, leases, fences, and result
-- linkage.  No backend process performs semantic matching or inference.
CREATE TABLE agent_message_requests (
    id                     TEXT        PRIMARY KEY,
    account_id             TEXT        NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    realm_id               TEXT        NOT NULL REFERENCES realms(id),
    opening_message_id     TEXT        NOT NULL,
    coordinator_agent_id   TEXT        NOT NULL REFERENCES agents(id),
    selection_policy       TEXT        NOT NULL DEFAULT 'client_ranked',
    state                  TEXT        NOT NULL DEFAULT 'open',
    max_assignees          INTEGER     NOT NULL DEFAULT 1,
    offer_window_seconds   INTEGER     NOT NULL,
    expires_in_seconds     INTEGER     NOT NULL,
    offer_deadline         TIMESTAMPTZ NOT NULL,
    expires_at             TIMESTAMPTZ NOT NULL,
    selection_generation   BIGINT      NOT NULL DEFAULT 0,
    completed_at           TIMESTAMPTZ,
    cancelled_at           TIMESTAMPTZ,
    expired_at             TIMESTAMPTZ,
    created_at             TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at             TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (id, account_id, realm_id),
    UNIQUE (opening_message_id, account_id, realm_id),
    FOREIGN KEY (opening_message_id, account_id, realm_id)
      REFERENCES agent_messages (id, account_id, realm_id)
      ON DELETE CASCADE DEFERRABLE INITIALLY DEFERRED,
    CHECK (id ~ '^mrq_[a-z2-7]{16}$'),
    CHECK (selection_policy = 'client_ranked'),
    CHECK (state IN ('open', 'completed', 'cancelled', 'expired')),
    CHECK (max_assignees BETWEEN 1 AND 8),
    CHECK (offer_window_seconds BETWEEN 1 AND 900),
    CHECK (expires_in_seconds BETWEEN 2 AND 604800),
    CHECK (expires_in_seconds > offer_window_seconds),
    CHECK (offer_deadline > created_at AND expires_at > offer_deadline),
    CHECK (selection_generation BETWEEN 0 AND 4611686018427387903),
    CHECK (
      (state = 'open' AND completed_at IS NULL AND cancelled_at IS NULL AND expired_at IS NULL)
      OR
      (state = 'completed' AND completed_at IS NOT NULL AND cancelled_at IS NULL AND expired_at IS NULL)
      OR
      (state = 'cancelled' AND completed_at IS NULL AND cancelled_at IS NOT NULL AND expired_at IS NULL)
      OR
      (state = 'expired' AND completed_at IS NULL AND cancelled_at IS NULL AND expired_at IS NOT NULL)
    )
);

CREATE INDEX agent_message_requests_open_by_realm
    ON agent_message_requests
       (account_id, realm_id, state, offer_deadline, expires_at, created_at, id)
    WHERE state = 'open';

-- This is the immutable send-time realm snapshot.  Later agent creation never
-- adds a candidate to an existing request.  Response state is deliberately
-- separate from assignment/claim state.
CREATE TABLE agent_message_request_candidates (
    request_id          TEXT        NOT NULL,
    account_id          TEXT        NOT NULL,
    realm_id            TEXT        NOT NULL,
    agent_id            TEXT        NOT NULL REFERENCES agents(id),
    response_state      TEXT        NOT NULL DEFAULT 'pending',
    offer_message_id    TEXT,
    offer_key_hash      TEXT        NOT NULL DEFAULT '',
    offer_request_hash  TEXT        NOT NULL DEFAULT '',
    responded_at        TIMESTAMPTZ,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (request_id, agent_id),
    FOREIGN KEY (request_id, account_id, realm_id)
      REFERENCES agent_message_requests (id, account_id, realm_id)
      ON DELETE CASCADE DEFERRABLE INITIALLY DEFERRED,
    FOREIGN KEY (offer_message_id, account_id, realm_id)
      REFERENCES agent_messages (id, account_id, realm_id)
      DEFERRABLE INITIALLY DEFERRED,
    UNIQUE (offer_message_id, account_id, realm_id),
    CHECK (response_state IN ('pending', 'offered', 'declined')),
    CHECK (
      (response_state = 'pending' AND offer_message_id IS NULL AND
       offer_key_hash = '' AND offer_request_hash = '' AND responded_at IS NULL)
      OR
      (response_state = 'offered' AND offer_message_id IS NOT NULL AND
       offer_key_hash ~ '^[0-9a-f]{64}$' AND
       offer_request_hash ~ '^[0-9a-f]{64}$' AND responded_at IS NOT NULL)
      OR
      (response_state = 'declined' AND offer_message_id IS NULL AND
       offer_key_hash = '' AND offer_request_hash = '' AND responded_at IS NOT NULL)
    )
);

CREATE INDEX agent_message_request_candidates_by_agent
    ON agent_message_request_candidates
       (account_id, realm_id, agent_id, response_state, request_id);

-- Each selection call is an immutable, idempotent coordinator decision.  A
-- later replacement selection gets a fresh generation and selection record.
CREATE TABLE agent_message_request_selections (
    id                    TEXT        PRIMARY KEY,
    request_id            TEXT        NOT NULL,
    account_id            TEXT        NOT NULL,
    realm_id              TEXT        NOT NULL,
    coordinator_agent_id  TEXT        NOT NULL REFERENCES agents(id),
    generation            BIGINT      NOT NULL,
    idempotency_key_hash  TEXT        NOT NULL,
    selection_hash        TEXT        NOT NULL,
    created_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (id, request_id, account_id, realm_id),
    UNIQUE (request_id, generation),
    UNIQUE (request_id, idempotency_key_hash),
    FOREIGN KEY (request_id, account_id, realm_id)
      REFERENCES agent_message_requests (id, account_id, realm_id)
      ON DELETE CASCADE DEFERRABLE INITIALLY DEFERRED,
    CHECK (id ~ '^msel_[a-z2-7]{16}$'),
    CHECK (generation BETWEEN 1 AND 4611686018427387903),
    CHECK (idempotency_key_hash ~ '^[0-9a-f]{64}$'),
    CHECK (selection_hash ~ '^[0-9a-f]{64}$')
);

-- A selected agent receives one bounded reservation.  Claim and completion
-- use database-time leases plus a monotonic generation so stale workers cannot
-- publish a result after replacement or cancellation.
CREATE TABLE agent_message_request_claims (
    id                   TEXT        PRIMARY KEY,
    request_id           TEXT        NOT NULL,
    selection_id         TEXT        NOT NULL,
    account_id           TEXT        NOT NULL,
    realm_id             TEXT        NOT NULL,
    agent_id             TEXT        NOT NULL REFERENCES agents(id),
    state                TEXT        NOT NULL DEFAULT 'reserved',
    generation           BIGINT      NOT NULL DEFAULT 0,
    claim_key_hash       TEXT        NOT NULL DEFAULT '',
    lease_expires_at     TIMESTAMPTZ,
    failure_count        BIGINT      NOT NULL DEFAULT 0,
    complete_key_hash    TEXT        NOT NULL DEFAULT '',
    result_message_id    TEXT,
    selected_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    claimed_at           TIMESTAMPTZ,
    released_at          TIMESTAMPTZ,
    completed_at         TIMESTAMPTZ,
    cancelled_at         TIMESTAMPTZ,
    updated_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (id, request_id, account_id, realm_id),
    UNIQUE (selection_id, agent_id),
    FOREIGN KEY (selection_id, request_id, account_id, realm_id)
      REFERENCES agent_message_request_selections
        (id, request_id, account_id, realm_id)
      ON DELETE CASCADE DEFERRABLE INITIALLY DEFERRED,
    FOREIGN KEY (request_id, account_id, realm_id)
      REFERENCES agent_message_requests (id, account_id, realm_id)
      ON DELETE CASCADE DEFERRABLE INITIALLY DEFERRED,
    FOREIGN KEY (result_message_id, account_id, realm_id)
      REFERENCES agent_messages (id, account_id, realm_id)
      DEFERRABLE INITIALLY DEFERRED,
    UNIQUE (result_message_id, account_id, realm_id),
    CHECK (id ~ '^mrc_[a-z2-7]{16}$'),
    CHECK (state IN ('reserved', 'claimed', 'released', 'completed', 'cancelled')),
    CHECK (generation BETWEEN 0 AND 4611686018427387903),
    CHECK (failure_count BETWEEN 0 AND 4611686018427387903),
    CHECK (
      (state = 'reserved' AND generation = 0 AND claim_key_hash = '' AND
       lease_expires_at IS NOT NULL AND claimed_at IS NULL AND released_at IS NULL AND
       completed_at IS NULL AND cancelled_at IS NULL AND complete_key_hash = '' AND
       result_message_id IS NULL)
      OR
      (state = 'claimed' AND generation >= 1 AND
       claim_key_hash ~ '^[0-9a-f]{64}$' AND lease_expires_at IS NOT NULL AND
       claimed_at IS NOT NULL AND released_at IS NULL AND completed_at IS NULL AND
       cancelled_at IS NULL AND complete_key_hash = '' AND result_message_id IS NULL)
      OR
      (state = 'released' AND generation >= 1 AND
       claim_key_hash ~ '^[0-9a-f]{64}$' AND lease_expires_at IS NULL AND
       claimed_at IS NOT NULL AND released_at IS NOT NULL AND completed_at IS NULL AND
       cancelled_at IS NULL AND complete_key_hash = '' AND result_message_id IS NULL)
      OR
      (state = 'completed' AND generation >= 1 AND
       claim_key_hash ~ '^[0-9a-f]{64}$' AND lease_expires_at IS NULL AND
       claimed_at IS NOT NULL AND released_at IS NULL AND completed_at IS NOT NULL AND
       cancelled_at IS NULL AND complete_key_hash ~ '^[0-9a-f]{64}$' AND
       result_message_id IS NOT NULL)
      OR
      (state = 'cancelled' AND lease_expires_at IS NULL AND released_at IS NULL AND
       completed_at IS NULL AND cancelled_at IS NOT NULL AND complete_key_hash = '' AND
       result_message_id IS NULL AND
       ((generation = 0 AND claim_key_hash = '' AND claimed_at IS NULL) OR
        (generation >= 1 AND claim_key_hash ~ '^[0-9a-f]{64}$' AND claimed_at IS NOT NULL)))
    )
);

CREATE INDEX agent_message_request_claims_by_request_capacity
    ON agent_message_request_claims
       (request_id, state, lease_expires_at, agent_id, id);

CREATE INDEX agent_message_request_claims_by_agent
    ON agent_message_request_claims
       (account_id, realm_id, agent_id, state, lease_expires_at, id);

-- +goose Down
DROP INDEX agent_message_request_claims_by_agent;
DROP INDEX agent_message_request_claims_by_request_capacity;
DROP TABLE agent_message_request_claims;
DROP TABLE agent_message_request_selections;
DROP INDEX agent_message_request_candidates_by_agent;
DROP TABLE agent_message_request_candidates;
DROP INDEX agent_message_requests_open_by_realm;
DROP TABLE agent_message_requests;
