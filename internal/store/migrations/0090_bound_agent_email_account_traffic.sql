-- +goose Up
-- Inbound email originally stopped at realm scope, so an account with many
-- realms could multiply the platform breaker. Keep the established realm
-- bucket table and its composite realm foreign key unchanged; account debt
-- lives in a separate account-keyed table so neither integrity boundary is
-- weakened. These are operational safety buckets outside account archives.
CREATE TABLE agent_email_account_rate_buckets (
    account_id                       TEXT        NOT NULL,
    dimension                        TEXT        NOT NULL,
    theoretical_arrival_nanoseconds BIGINT      NOT NULL,
    updated_at                       TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (account_id, dimension),
    FOREIGN KEY (account_id) REFERENCES accounts (id) ON DELETE CASCADE,
    CHECK (dimension IN ('email_received', 'email_received_bytes')),
    CHECK (theoretical_arrival_nanoseconds > 0)
);

CREATE INDEX agent_email_account_rate_buckets_by_update
    ON agent_email_account_rate_buckets (updated_at);

-- Serialize one account-wide weighted debit entirely inside PostgreSQL. This
-- mirrors the realm limiter while retaining an account-only relational key.
-- +goose StatementBegin
CREATE FUNCTION witself_consume_agent_email_account_rate_bucket(
    p_account_id TEXT,
    p_dimension TEXT,
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
    IF p_limit <= 0 OR p_quantity <= 0 OR p_quantity > p_limit THEN
        SELECT bucket.theoretical_arrival_nanoseconds
          INTO v_current_tat
          FROM agent_email_account_rate_buckets AS bucket
         WHERE bucket.account_id = p_account_id
           AND bucket.dimension = p_dimension;

        SELECT floor(extract(epoch FROM clock_timestamp()) * 1000000000)::bigint
          INTO now_nanoseconds;
        admitted := FALSE;
        current_tat := COALESCE(v_current_tat, now_nanoseconds);
        RETURN NEXT;
        RETURN;
    END IF;

    LOOP
        SELECT bucket.theoretical_arrival_nanoseconds
          INTO v_current_tat
          FROM agent_email_account_rate_buckets AS bucket
         WHERE bucket.account_id = p_account_id
           AND bucket.dimension = p_dimension
         FOR UPDATE;
        EXIT WHEN FOUND;

        INSERT INTO agent_email_account_rate_buckets
          (account_id, dimension, theoretical_arrival_nanoseconds)
        VALUES (p_account_id, p_dimension, 1)
        ON CONFLICT (account_id, dimension) DO NOTHING;
    END LOOP;

    SELECT floor(extract(epoch FROM clock_timestamp()) * 1000000000)::bigint
      INTO now_nanoseconds;
    v_candidate_tat := GREATEST(v_current_tat, now_nanoseconds) +
        p_quantity * p_interval_nanoseconds;

    IF v_candidate_tat <= now_nanoseconds + p_limit * p_interval_nanoseconds THEN
        UPDATE agent_email_account_rate_buckets AS bucket
           SET theoretical_arrival_nanoseconds = v_candidate_tat,
               updated_at = clock_timestamp()
         WHERE bucket.account_id = p_account_id
           AND bucket.dimension = p_dimension;
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

-- Add long-horizon account and recipient reputation lanes. Minute lanes keep
-- their existing names so rolling replicas coordinate on the same debt while
-- this migration is deployed.
ALTER TABLE agent_email_outbound_rate_buckets
  DROP CONSTRAINT agent_email_outbound_rate_buckets_lane_check,
  DROP CONSTRAINT agent_email_outbound_rate_buckets_scope_check,
  DROP CONSTRAINT agent_email_outbound_rate_buckets_check;

ALTER TABLE agent_email_outbound_rate_buckets
  ADD CHECK (lane IN (
    'admission','dispatch','admission_daily','dispatch_daily'
  )),
  ADD CHECK (scope IN ('account','recipient','agent','realm')),
  ADD CHECK (
    (lane IN ('admission','dispatch')
      AND scope IN ('account','agent','realm'))
    OR
    (lane IN ('admission_daily','dispatch_daily')
      AND scope IN ('account','recipient'))
  ),
  ADD CHECK (
    (scope = 'account' AND realm_id = '' AND scope_id = account_id)
    OR
    (scope = 'recipient' AND realm_id = ''
      AND scope_id ~ '^[0-9a-f]{64}$')
    OR
    (scope = 'agent' AND realm_id <> ''
      AND octet_length(scope_id) BETWEEN 1 AND 128)
    OR
    (scope = 'realm' AND realm_id <> '' AND scope_id = realm_id)
  );

-- +goose Down
-- Rate debt is disposable coordination state. Remove only the new outbound
-- shapes before restoring the schema-89 constraints.
DELETE FROM agent_email_outbound_rate_buckets
 WHERE lane IN ('admission_daily','dispatch_daily') OR scope = 'recipient';

ALTER TABLE agent_email_outbound_rate_buckets
  DROP CONSTRAINT agent_email_outbound_rate_buckets_lane_check,
  DROP CONSTRAINT agent_email_outbound_rate_buckets_scope_check,
  DROP CONSTRAINT agent_email_outbound_rate_buckets_check,
  DROP CONSTRAINT agent_email_outbound_rate_buckets_check1;

ALTER TABLE agent_email_outbound_rate_buckets
  ADD CHECK (lane IN ('admission','dispatch')),
  ADD CHECK (scope IN ('account','agent','realm')),
  ADD CHECK (
    (scope = 'account' AND realm_id = '' AND scope_id = account_id)
    OR
    (scope = 'agent' AND realm_id <> ''
      AND octet_length(scope_id) BETWEEN 1 AND 128)
    OR
    (scope = 'realm' AND realm_id <> '' AND scope_id = realm_id)
  );

DROP FUNCTION witself_consume_agent_email_account_rate_bucket(
    TEXT, TEXT, BIGINT, BIGINT, BIGINT
);
DROP INDEX agent_email_account_rate_buckets_by_update;
DROP TABLE agent_email_account_rate_buckets;
