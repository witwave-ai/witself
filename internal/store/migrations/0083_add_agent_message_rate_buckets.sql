-- +goose Up
-- Cell-local GCRA state for realm-local message admission. One row represents
-- one bounded sender, realm, or recipient scope; the row is updated in the
-- same transaction that creates the message and its delivery snapshot.
--
-- The theoretical-arrival time is stored as Unix microseconds so weighted
-- fan-out debits use exact integer arithmetic. This state is operational and
-- intentionally absent from account archives; moving an account to another
-- cell starts fresh platform-protection buckets there.
CREATE TABLE agent_message_rate_buckets (
    account_id                       TEXT        NOT NULL,
    realm_id                         TEXT        NOT NULL,
    dimension                        TEXT        NOT NULL,
    scope                            TEXT        NOT NULL,
    scope_id                         TEXT        NOT NULL,
    theoretical_arrival_microseconds BIGINT      NOT NULL,
    updated_at                       TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (account_id, realm_id, dimension, scope, scope_id),
    FOREIGN KEY (account_id, realm_id)
      REFERENCES realms (account_id, id) ON DELETE CASCADE,
    CHECK (dimension IN ('message_sent', 'message_delivered')),
    CHECK (scope IN ('agent', 'realm', 'recipient')),
    CHECK (
      (dimension = 'message_sent' AND scope = 'agent') OR
      (dimension = 'message_delivered' AND scope IN ('realm', 'recipient'))
    ),
    CHECK (octet_length(scope_id) BETWEEN 1 AND 128),
    CHECK (theoretical_arrival_microseconds > 0)
);

CREATE INDEX agent_message_rate_buckets_by_update
    ON agent_message_rate_buckets (updated_at);

-- Serialize one weighted GCRA debit entirely inside PostgreSQL so every API
-- replica uses a clock captured after the bucket row lock is acquired. The
-- function is invoker-rights and returns only value-free rate state.
-- +goose StatementBegin
CREATE FUNCTION witself_consume_agent_message_rate_bucket(
    p_account_id TEXT,
    p_realm_id TEXT,
    p_dimension TEXT,
    p_scope TEXT,
    p_scope_id TEXT,
    p_interval_microseconds BIGINT,
    p_limit BIGINT,
    p_quantity BIGINT
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
    -- A zero/negative limit, non-positive debit, or debit larger than total
    -- capacity can never succeed unchanged. Keep this path read-only.
    IF p_limit <= 0 OR p_quantity <= 0 OR p_quantity > p_limit THEN
        SELECT bucket.theoretical_arrival_microseconds
          INTO v_current_tat
          FROM agent_message_rate_buckets AS bucket
         WHERE bucket.account_id = p_account_id
           AND bucket.realm_id = p_realm_id
           AND bucket.dimension = p_dimension
           AND bucket.scope = p_scope
           AND bucket.scope_id = p_scope_id;

        SELECT floor(extract(epoch FROM clock_timestamp()) * 1000000)::bigint
          INTO now_microseconds;
        admitted := FALSE;
        current_tat := COALESCE(v_current_tat, now_microseconds);
        RETURN NEXT;
        RETURN;
    END IF;

    -- Select-before-insert avoids evaluating the rate clock until after an
    -- existing row is locked. If two first writers race, the loser loops after
    -- INSERT ON CONFLICT waits for the winner.
    LOOP
        SELECT bucket.theoretical_arrival_microseconds
          INTO v_current_tat
          FROM agent_message_rate_buckets AS bucket
         WHERE bucket.account_id = p_account_id
           AND bucket.realm_id = p_realm_id
           AND bucket.dimension = p_dimension
           AND bucket.scope = p_scope
           AND bucket.scope_id = p_scope_id
         FOR UPDATE;
        EXIT WHEN FOUND;

        INSERT INTO agent_message_rate_buckets
          (account_id, realm_id, dimension, scope, scope_id,
           theoretical_arrival_microseconds)
        VALUES
          (p_account_id, p_realm_id, p_dimension, p_scope, p_scope_id, 1)
        ON CONFLICT (account_id, realm_id, dimension, scope, scope_id)
        DO NOTHING;
    END LOOP;

    SELECT floor(extract(epoch FROM clock_timestamp()) * 1000000)::bigint
      INTO now_microseconds;
    v_candidate_tat := GREATEST(v_current_tat, now_microseconds) +
        p_quantity * p_interval_microseconds;

    IF v_candidate_tat <= now_microseconds + p_limit * p_interval_microseconds THEN
        UPDATE agent_message_rate_buckets AS bucket
           SET theoretical_arrival_microseconds = v_candidate_tat,
               updated_at = to_timestamp(now_microseconds::double precision / 1000000.0)
         WHERE bucket.account_id = p_account_id
           AND bucket.realm_id = p_realm_id
           AND bucket.dimension = p_dimension
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

-- +goose Down
DROP FUNCTION witself_consume_agent_message_rate_bucket(
    TEXT, TEXT, TEXT, TEXT, TEXT, BIGINT, BIGINT, BIGINT
);
DROP INDEX agent_message_rate_buckets_by_update;
DROP TABLE agent_message_rate_buckets;
