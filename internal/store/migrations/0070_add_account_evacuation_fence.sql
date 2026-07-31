-- +goose Up
-- An account move needs a database-local write barrier that survives process
-- restarts and does not depend on every application mutation remembering to
-- check status='suspended'. The opaque evacuation id is minted by the control
-- plane Durable Object. It is both the move epoch and the exact authority
-- required by export/import/resume maintenance transactions.
ALTER TABLE accounts
    ADD COLUMN evacuation_id TEXT,
    ADD COLUMN evacuation_started_at TIMESTAMPTZ,
    ADD COLUMN last_evacuation_id TEXT,
    ADD COLUMN last_evacuation_completed_at TIMESTAMPTZ,
    ADD COLUMN last_evacuation_outcome TEXT,
    ADD CONSTRAINT accounts_evacuation_pair_chk CHECK (
        (evacuation_id IS NULL) = (evacuation_started_at IS NULL)
    ),
    ADD CONSTRAINT accounts_last_evacuation_pair_chk CHECK (
        (last_evacuation_id IS NULL) =
        (last_evacuation_completed_at IS NULL)
        AND
        (last_evacuation_id IS NULL) =
        (last_evacuation_outcome IS NULL)
    ),
    ADD CONSTRAINT accounts_last_evacuation_outcome_chk CHECK (
        last_evacuation_outcome IS NULL OR
        last_evacuation_outcome IN ('completed', 'aborted')
    ),
    ADD CONSTRAINT accounts_evacuation_id_chk CHECK (
        evacuation_id IS NULL OR
        evacuation_id ~ '^[A-Za-z0-9_-]{1,128}$'
    ),
    ADD CONSTRAINT accounts_last_evacuation_id_chk CHECK (
        last_evacuation_id IS NULL OR
        last_evacuation_id ~ '^[A-Za-z0-9_-]{1,128}$'
    );

-- +goose StatementBegin
CREATE FUNCTION witself_check_account_evacuation_fence(target_account_id TEXT)
RETURNS void
LANGUAGE plpgsql
AS $$
DECLARE
    authority TEXT := NULLIF(
        current_setting('witself.evacuation_id', true), ''
    );
    marker TEXT;
BEGIN
    SELECT evacuation_id
      INTO marker
      FROM accounts
     WHERE id = target_account_id
     FOR SHARE;
    IF marker IS NOT NULL AND marker IS DISTINCT FROM authority THEN
        RAISE EXCEPTION 'account evacuation is in progress'
            USING ERRCODE = '55006';
    END IF;
END;
$$;
-- +goose StatementEnd

-- Child-table mutations take a shared lock on the owning account row. The
-- lifecycle transition takes that row FOR UPDATE, so it cannot install the
-- marker until every mutation that observed the pre-marker state commits.
-- Conversely, a mutation arriving after the lifecycle transition waits for
-- the marker and then fails closed. This is the snapshot linearization point.
-- +goose StatementBegin
CREATE FUNCTION witself_tenant_evacuation_fence()
RETURNS trigger
LANGUAGE plpgsql
AS $$
DECLARE
    old_account_id TEXT;
    new_account_id TEXT;
    candidate_account_id TEXT;
BEGIN
    IF TG_OP <> 'INSERT' THEN
        old_account_id := to_jsonb(OLD)->>'account_id';
    END IF;
    IF TG_OP <> 'DELETE' THEN
        new_account_id := to_jsonb(NEW)->>'account_id';
    END IF;

    -- Deterministic order avoids inverse account-id updates deadlocking.
    FOR candidate_account_id IN
        SELECT DISTINCT candidate
          FROM unnest(ARRAY[old_account_id, new_account_id]) candidate
         WHERE candidate IS NOT NULL
         ORDER BY candidate
    LOOP
        PERFORM witself_check_account_evacuation_fence(candidate_account_id);
    END LOOP;

    IF TG_OP = 'DELETE' THEN
        RETURN OLD;
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

-- `agents` is tenant-scoped indirectly through realms.
-- +goose StatementBegin
CREATE FUNCTION witself_agents_evacuation_fence()
RETURNS trigger
LANGUAGE plpgsql
AS $$
DECLARE
    old_realm_id TEXT;
    new_realm_id TEXT;
    candidate_realm_id TEXT;
    candidate_account_id TEXT;
    candidate_account_ids TEXT[] := ARRAY[]::TEXT[];
BEGIN
    IF TG_OP <> 'INSERT' THEN
        old_realm_id := OLD.realm_id;
    END IF;
    IF TG_OP <> 'DELETE' THEN
        new_realm_id := NEW.realm_id;
    END IF;

    -- Lock every dependency before consulting its account mapping. This
    -- serializes a concurrent realm move with the account-row barrier. A NEW
    -- realm that is not visible yet may be an uncommitted cross-transaction
    -- insert; fail closed instead of letting its FK wake after the evacuation
    -- marker has already committed. Missing OLD rows are possible only during
    -- a protected parent cascade.
    FOR candidate_realm_id IN
        SELECT DISTINCT candidate
          FROM unnest(ARRAY[old_realm_id, new_realm_id]) candidate
         WHERE candidate IS NOT NULL
         ORDER BY candidate
    LOOP
        SELECT r.account_id
          INTO candidate_account_id
          FROM realms r
         WHERE r.id = candidate_realm_id
         FOR SHARE;
        IF NOT FOUND THEN
            IF TG_OP <> 'DELETE' THEN
                RAISE EXCEPTION 'account evacuation dependency is not visible'
                    USING ERRCODE = '55006';
            END IF;
            CONTINUE;
        END IF;
        candidate_account_ids := array_append(
            candidate_account_ids, candidate_account_id
        );
    END LOOP;

    FOR candidate_account_id IN
        SELECT DISTINCT candidate
          FROM unnest(candidate_account_ids) candidate
         ORDER BY candidate
    LOOP
        PERFORM witself_check_account_evacuation_fence(candidate_account_id);
    END LOOP;
    IF TG_OP = 'DELETE' THEN
        RETURN OLD;
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

-- `agent_activity` is tenant-scoped through agent -> realm -> account.
-- +goose StatementBegin
CREATE FUNCTION witself_agent_activity_evacuation_fence()
RETURNS trigger
LANGUAGE plpgsql
AS $$
DECLARE
    old_agent_id TEXT;
    new_agent_id TEXT;
    candidate_agent_id TEXT;
    candidate_realm_id TEXT;
    candidate_account_id TEXT;
    candidate_realm_ids TEXT[] := ARRAY[]::TEXT[];
    candidate_account_ids TEXT[] := ARRAY[]::TEXT[];
BEGIN
    IF TG_OP <> 'INSERT' THEN
        old_agent_id := OLD.agent_id;
    END IF;
    IF TG_OP <> 'DELETE' THEN
        new_agent_id := NEW.agent_id;
    END IF;

    -- Lock agent mappings first, then realm mappings, then account rows. The
    -- fixed dependency order prevents a concurrent agent/realm reassignment
    -- from moving this activity write behind an already-committed marker.
    FOR candidate_agent_id IN
        SELECT DISTINCT candidate
          FROM unnest(ARRAY[old_agent_id, new_agent_id]) candidate
         WHERE candidate IS NOT NULL
         ORDER BY candidate
    LOOP
        SELECT a.realm_id
          INTO candidate_realm_id
          FROM agents a
         WHERE a.id = candidate_agent_id
         FOR SHARE;
        IF NOT FOUND THEN
            IF TG_OP <> 'DELETE' THEN
                RAISE EXCEPTION 'account evacuation dependency is not visible'
                    USING ERRCODE = '55006';
            END IF;
            CONTINUE;
        END IF;
        candidate_realm_ids := array_append(
            candidate_realm_ids, candidate_realm_id
        );
    END LOOP;

    FOR candidate_realm_id IN
        SELECT DISTINCT candidate
          FROM unnest(candidate_realm_ids) candidate
         ORDER BY candidate
    LOOP
        SELECT r.account_id
          INTO candidate_account_id
          FROM realms r
         WHERE r.id = candidate_realm_id
         FOR SHARE;
        IF NOT FOUND THEN
            IF TG_OP <> 'DELETE' THEN
                RAISE EXCEPTION 'account evacuation dependency is not visible'
                    USING ERRCODE = '55006';
            END IF;
            CONTINUE;
        END IF;
        candidate_account_ids := array_append(
            candidate_account_ids, candidate_account_id
        );
    END LOOP;

    FOR candidate_account_id IN
        SELECT DISTINCT candidate
          FROM unnest(candidate_account_ids) candidate
         ORDER BY candidate
    LOOP
        PERFORM witself_check_account_evacuation_fence(candidate_account_id);
    END LOOP;
    IF TG_OP = 'DELETE' THEN
        RETURN OLD;
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

-- The accounts row needs a separate trigger because its tenant key is `id`.
-- Starting and completing a move are allowed only when the transaction-local
-- authority equals every non-null old/new epoch involved in the update.
-- +goose StatementBegin
CREATE FUNCTION witself_account_evacuation_fence()
RETURNS trigger
LANGUAGE plpgsql
AS $$
DECLARE
    authority TEXT := NULLIF(
        current_setting('witself.evacuation_id', true), ''
    );
    old_marker TEXT;
    new_marker TEXT;
BEGIN
    IF TG_OP <> 'INSERT' THEN
        old_marker := OLD.evacuation_id;
    END IF;
    IF TG_OP <> 'DELETE' THEN
        new_marker := NEW.evacuation_id;
    END IF;

    IF old_marker IS NOT NULL AND old_marker IS DISTINCT FROM authority THEN
        RAISE EXCEPTION 'account evacuation is in progress'
            USING ERRCODE = '55006';
    END IF;
    IF new_marker IS NOT NULL AND new_marker IS DISTINCT FROM authority THEN
        RAISE EXCEPTION 'account evacuation authority does not match'
            USING ERRCODE = '55006';
    END IF;

    IF TG_OP = 'DELETE' THEN
        RETURN OLD;
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER account_evacuation_fence
BEFORE INSERT OR UPDATE OR DELETE ON accounts
FOR EACH ROW EXECUTE FUNCTION witself_account_evacuation_fence();

-- Attach the barrier to every current directly tenant-scoped archive table.
-- Cell-local retention coordination is excluded because it is not snapshot
-- content; its workers separately skip fenced accounts.
-- A schema contract test requires future account_id tables to attach the same
-- trigger, so additive migrations cannot silently reopen this race.
-- +goose StatementBegin
DO $$
DECLARE
    table_name TEXT;
BEGIN
    FOR table_name IN
        SELECT c.table_name
          FROM information_schema.columns c
          JOIN information_schema.tables t
            ON t.table_schema = c.table_schema
           AND t.table_name = c.table_name
         WHERE c.table_schema = current_schema()
           AND c.column_name = 'account_id'
           AND t.table_type = 'BASE TABLE'
           AND c.table_name NOT IN (
             'account_evacuation_finalizations',
             'account_provision_receipts',
             'agent_email_retention_account_scan_state',
             'agent_email_retention_worker_lanes',
             'message_retention_thread_activity',
             'message_retention_account_scan_state',
             'message_retention_worker_lanes',
             'transcript_retention_account_scan_state',
             'transcript_retention_sweep_state',
             'transcript_retention_worker_lanes'
           )
         ORDER BY c.table_name
    LOOP
        EXECUTE format(
            'CREATE TRIGGER account_evacuation_fence
             BEFORE INSERT OR UPDATE OR DELETE ON %I
             FOR EACH ROW EXECUTE FUNCTION witself_tenant_evacuation_fence()',
            table_name
        );
    END LOOP;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER account_evacuation_fence
BEFORE INSERT OR UPDATE OR DELETE ON agents
FOR EACH ROW EXECUTE FUNCTION witself_agents_evacuation_fence();

CREATE TRIGGER account_evacuation_fence
BEFORE INSERT OR UPDATE OR DELETE ON agent_activity
FOR EACH ROW EXECUTE FUNCTION witself_agent_activity_evacuation_fence();

-- +goose Down
-- Active markers and completed/aborted ids are exact retry authority. Removing
-- them could reopen writes during a move or let a delayed lifecycle operation
-- run as new after re-upgrade. Lock out lifecycle writers and fail closed.
LOCK TABLE accounts IN ACCESS EXCLUSIVE MODE;

-- +goose StatementBegin
DO $$
BEGIN
  IF EXISTS (
    SELECT 1
      FROM accounts
     WHERE evacuation_id IS NOT NULL
        OR last_evacuation_id IS NOT NULL
  ) THEN
    RAISE EXCEPTION 'cannot remove account evacuation fences while evacuation state exists';
  END IF;
END;
$$;
-- +goose StatementEnd

DROP TRIGGER IF EXISTS account_evacuation_fence ON agent_activity;
DROP TRIGGER IF EXISTS account_evacuation_fence ON agents;

-- +goose StatementBegin
DO $$
DECLARE
    table_name TEXT;
BEGIN
    FOR table_name IN
        SELECT c.table_name
          FROM information_schema.columns c
          JOIN information_schema.tables t
            ON t.table_schema = c.table_schema
           AND t.table_name = c.table_name
         WHERE c.table_schema = current_schema()
           AND c.column_name = 'account_id'
           AND t.table_type = 'BASE TABLE'
         ORDER BY c.table_name
    LOOP
        EXECUTE format(
            'DROP TRIGGER IF EXISTS account_evacuation_fence ON %I',
            table_name
        );
    END LOOP;
END;
$$;
-- +goose StatementEnd

DROP TRIGGER IF EXISTS account_evacuation_fence ON accounts;
DROP FUNCTION witself_agent_activity_evacuation_fence();
DROP FUNCTION witself_agents_evacuation_fence();
DROP FUNCTION witself_account_evacuation_fence();
DROP FUNCTION witself_tenant_evacuation_fence();
DROP FUNCTION witself_check_account_evacuation_fence(TEXT);

ALTER TABLE accounts
    DROP CONSTRAINT accounts_last_evacuation_outcome_chk,
    DROP CONSTRAINT accounts_last_evacuation_id_chk,
    DROP CONSTRAINT accounts_evacuation_id_chk,
    DROP CONSTRAINT accounts_last_evacuation_pair_chk,
    DROP CONSTRAINT accounts_evacuation_pair_chk,
    DROP COLUMN last_evacuation_outcome,
    DROP COLUMN last_evacuation_completed_at,
    DROP COLUMN last_evacuation_id,
    DROP COLUMN evacuation_started_at,
    DROP COLUMN evacuation_id;
