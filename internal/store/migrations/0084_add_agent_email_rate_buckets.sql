-- +goose Up
-- Cell-local GCRA state for signed inbound agent-email attempts. These rows
-- are defensive coordination state, not billable usage. Sender scope ids are
-- client-generated hashes so unverified envelope addresses never become
-- bucket-table identifiers.
CREATE TABLE agent_email_rate_buckets (
    account_id                     TEXT        NOT NULL,
    realm_id                       TEXT        NOT NULL,
    dimension                      TEXT        NOT NULL,
    scope                          TEXT        NOT NULL,
    scope_id                       TEXT        NOT NULL,
    theoretical_arrival_nanoseconds BIGINT      NOT NULL,
    updated_at                     TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (account_id, realm_id, dimension, scope, scope_id),
    FOREIGN KEY (account_id, realm_id)
      REFERENCES realms (account_id, id) ON DELETE CASCADE,
    CHECK (dimension IN ('email_received', 'email_received_bytes')),
    CHECK (scope IN ('realm', 'recipient', 'sender')),
    CHECK (octet_length(scope_id) BETWEEN 1 AND 128),
    CHECK (theoretical_arrival_nanoseconds > 0)
);

CREATE INDEX agent_email_rate_buckets_by_update
    ON agent_email_rate_buckets (updated_at);

-- Serialize one weighted debit entirely inside PostgreSQL. Nanosecond units
-- preserve useful precision for the byte-weighted 4 GiB/minute realm breaker;
-- PostgreSQL remains the shared clock and lock point for every cell replica.
-- +goose StatementBegin
CREATE FUNCTION witself_consume_agent_email_rate_bucket(
    p_account_id TEXT,
    p_realm_id TEXT,
    p_dimension TEXT,
    p_scope TEXT,
    p_scope_id TEXT,
    p_interval_nanoseconds BIGINT,
    p_limit BIGINT,
    p_quantity BIGINT
)
RETURNS TABLE (
    admitted BOOLEAN,
    current_tat BIGINT,
    now_nanoseconds BIGINT
)
LANGUAGE plpgsql
AS $$
DECLARE
    v_current_tat BIGINT;
    v_candidate_tat BIGINT;
BEGIN
    -- Zero limits and debits larger than the complete bucket can never fit.
    -- Keep this refusal read-only so a disabled account cannot create rows.
    IF p_limit <= 0 OR p_quantity <= 0 OR p_quantity > p_limit THEN
        SELECT bucket.theoretical_arrival_nanoseconds
          INTO v_current_tat
          FROM agent_email_rate_buckets AS bucket
         WHERE bucket.account_id = p_account_id
           AND bucket.realm_id = p_realm_id
           AND bucket.dimension = p_dimension
           AND bucket.scope = p_scope
           AND bucket.scope_id = p_scope_id;

        SELECT floor(extract(epoch FROM clock_timestamp()) * 1000000000)::bigint
          INTO now_nanoseconds;
        admitted := FALSE;
        current_tat := COALESCE(v_current_tat, now_nanoseconds);
        RETURN NEXT;
        RETURN;
    END IF;

    -- Select-before-insert captures the database clock only after an existing
    -- bucket lock is acquired. First-writer races converge through ON CONFLICT.
    LOOP
        SELECT bucket.theoretical_arrival_nanoseconds
          INTO v_current_tat
          FROM agent_email_rate_buckets AS bucket
         WHERE bucket.account_id = p_account_id
           AND bucket.realm_id = p_realm_id
           AND bucket.dimension = p_dimension
           AND bucket.scope = p_scope
           AND bucket.scope_id = p_scope_id
         FOR UPDATE;
        EXIT WHEN FOUND;

        INSERT INTO agent_email_rate_buckets
          (account_id, realm_id, dimension, scope, scope_id,
           theoretical_arrival_nanoseconds)
        VALUES
          (p_account_id, p_realm_id, p_dimension, p_scope, p_scope_id, 1)
        ON CONFLICT (account_id, realm_id, dimension, scope, scope_id)
        DO NOTHING;
    END LOOP;

    SELECT floor(extract(epoch FROM clock_timestamp()) * 1000000000)::bigint
      INTO now_nanoseconds;
    v_candidate_tat := GREATEST(v_current_tat, now_nanoseconds) +
        p_quantity * p_interval_nanoseconds;

    IF v_candidate_tat <= now_nanoseconds + p_limit * p_interval_nanoseconds THEN
        UPDATE agent_email_rate_buckets AS bucket
           SET theoretical_arrival_nanoseconds = v_candidate_tat,
               updated_at = clock_timestamp()
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
DROP FUNCTION witself_consume_agent_email_rate_bucket(
    TEXT, TEXT, TEXT, TEXT, TEXT, BIGINT, BIGINT, BIGINT
);
DROP INDEX agent_email_rate_buckets_by_update;
DROP TABLE agent_email_rate_buckets;
