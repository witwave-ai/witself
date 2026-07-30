-- +goose Up
-- Agent-email attachment capacity is account-wide and physical: every
-- retained attachment-bearing raw MIME row consumes its complete raw size.
-- Messages that do not fit preserve bounded text and metadata while omitting
-- raw MIME. The account counter is a derived projection maintained in the
-- same transaction as every Phase-A message insert/delete. Rolling old-writer
-- rows are explicitly marked unaccounted until a fenced reconciliation after
-- every old process is gone.

ALTER TABLE accounts
  ADD COLUMN retained_agent_email_attachment_bytes BIGINT NOT NULL DEFAULT 0;

ALTER TABLE accounts
  ADD CONSTRAINT accounts_retained_agent_email_attachment_bytes_range
  CHECK (
    retained_agent_email_attachment_bytes
      BETWEEN 0 AND 4611686018427387903
  ) NOT VALID;

ALTER TABLE agent_email_messages
  ALTER COLUMN raw_mime DROP NOT NULL,
  ADD COLUMN body_text TEXT,
  ADD COLUMN body_text_kind TEXT,
  ADD COLUMN attachment_storage_bytes BIGINT NOT NULL DEFAULT 0,
  ADD COLUMN retained_attachment_storage_bytes BIGINT NOT NULL DEFAULT 0,
  ADD COLUMN payload_retention_state TEXT NOT NULL DEFAULT 'legacy_pending',
  -- Old replicas omit this column. Their rows remain physically retained but
  -- deliberately uncounted until the post-convergence Phase-B reconciliation;
  -- this avoids upgrading their existing accounts FOR SHARE lock inside the
  -- INSERT trigger. Phase-A writers explicitly set it true.
  ADD COLUMN attachment_storage_accounted BOOLEAN NOT NULL DEFAULT false;

-- Migration 59 used an unnamed compound CHECK for both the 5 MiB ceiling and
-- byte-exact raw storage. Locate it by its referenced columns instead of
-- depending on PostgreSQL's generated constraint suffix.
-- +goose StatementBegin
DO $$
DECLARE
  constraint_name TEXT;
BEGIN
  FOR constraint_name IN
    SELECT con.conname
      FROM pg_constraint con
     WHERE con.conrelid = 'agent_email_messages'::regclass
       AND con.contype = 'c'
       AND pg_get_constraintdef(con.oid) LIKE '%raw_size_bytes%'
       AND pg_get_constraintdef(con.oid) LIKE '%raw_mime%'
  LOOP
    EXECUTE format(
      'ALTER TABLE agent_email_messages DROP CONSTRAINT %I',
      constraint_name
    );
  END LOOP;
END
$$;
-- +goose StatementEnd

-- Old replicas omit all new columns. Normalize their insert before checks run,
-- but leave attachment_storage_accounted=false so their pre-existing account
-- FOR SHARE lock is never upgraded by the counter trigger. Parse failures are
-- still treated conservatively as attachment-bearing. Finite catalog limits
-- remain inactive until a later fenced reconciliation promotes these rows.
-- +goose StatementBegin
CREATE FUNCTION normalize_legacy_agent_email_storage_insert()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
  IF NEW.payload_retention_state = 'legacy_pending' THEN
    NEW.payload_retention_state := 'retained';
    IF NEW.attachment_count > 0 OR NEW.parse_state = 'error' THEN
      NEW.attachment_storage_bytes := NEW.raw_size_bytes;
      NEW.retained_attachment_storage_bytes := NEW.raw_size_bytes;
    ELSE
      NEW.attachment_storage_bytes := 0;
      NEW.retained_attachment_storage_bytes := 0;
    END IF;
  END IF;
  RETURN NEW;
END
$$;
-- +goose StatementEnd

CREATE TRIGGER agent_email_messages_normalize_legacy_storage
BEFORE INSERT ON agent_email_messages
FOR EACH ROW
EXECUTE FUNCTION normalize_legacy_agent_email_storage_insert();

-- Maintain the derived projection for Phase-A writers, retention, direct
-- realm/agent cascades, and archive import. Compatibility rows from old
-- writers contribute zero until they are explicitly promoted. A deleting
-- account may already be invisible to its child cascade; in that one case no
-- projection row remains to maintain.
-- +goose StatementBegin
CREATE FUNCTION maintain_agent_email_attachment_counter()
RETURNS trigger
LANGUAGE plpgsql
AS $$
DECLARE
  target_account_id TEXT;
  delta BIGINT;
  old_retained BIGINT;
  new_retained BIGINT;
BEGIN
  IF TG_OP = 'INSERT' THEN
    target_account_id := NEW.account_id;
    IF NEW.attachment_storage_accounted THEN
      delta := NEW.retained_attachment_storage_bytes;
    ELSE
      delta := 0;
    END IF;
  ELSIF TG_OP = 'DELETE' THEN
    target_account_id := OLD.account_id;
    IF OLD.attachment_storage_accounted THEN
      delta := -OLD.retained_attachment_storage_bytes;
    ELSE
      delta := 0;
    END IF;
  ELSE
    IF NEW.account_id <> OLD.account_id THEN
      RAISE EXCEPTION
        'agent-email attachment counter cannot move between accounts';
    END IF;
    target_account_id := NEW.account_id;
    IF OLD.attachment_storage_accounted THEN
      old_retained := OLD.retained_attachment_storage_bytes;
    ELSE
      old_retained := 0;
    END IF;
    IF NEW.attachment_storage_accounted THEN
      new_retained := NEW.retained_attachment_storage_bytes;
    ELSE
      new_retained := 0;
    END IF;
    delta := new_retained - old_retained;
  END IF;

  IF delta > 0 THEN
    UPDATE accounts
       SET retained_agent_email_attachment_bytes =
             retained_agent_email_attachment_bytes + delta
     WHERE id = target_account_id
       AND retained_agent_email_attachment_bytes
             <= 4611686018427387903 - delta;
    IF NOT FOUND THEN
      RAISE EXCEPTION
        'agent-email attachment counter overflow or missing account';
    END IF;
  ELSIF delta < 0 THEN
    UPDATE accounts
       SET retained_agent_email_attachment_bytes =
             retained_agent_email_attachment_bytes + delta
     WHERE id = target_account_id
       AND retained_agent_email_attachment_bytes >= -delta;
    IF NOT FOUND AND EXISTS (
      SELECT 1 FROM accounts WHERE id = target_account_id
    ) THEN
      RAISE EXCEPTION 'agent-email attachment counter underflow';
    END IF;
  END IF;
  IF TG_OP = 'DELETE' THEN
    RETURN OLD;
  END IF;
  RETURN NEW;
END
$$;
-- +goose StatementEnd

CREATE TRIGGER agent_email_messages_maintain_attachment_counter
AFTER INSERT OR DELETE OR UPDATE OF
  retained_attachment_storage_bytes, attachment_storage_accounted, account_id
ON agent_email_messages
FOR EACH ROW
EXECUTE FUNCTION maintain_agent_email_attachment_counter();

ALTER TABLE agent_email_messages
  ADD CONSTRAINT agent_email_messages_raw_storage_shape
  CHECK (
    raw_size_bytes BETWEEN 1 AND 26214400
    AND (
      (payload_retention_state = 'retained'
       AND raw_mime IS NOT NULL
       AND raw_size_bytes = octet_length(raw_mime))
      OR
      (payload_retention_state = 'omitted_capacity'
       AND raw_mime IS NULL)
    )
  ) NOT VALID,
  ADD CONSTRAINT agent_email_messages_body_projection_shape
  CHECK (
    (body_text IS NULL AND body_text_kind IS NULL)
    OR
    (body_text IS NOT NULL
     AND octet_length(body_text) <= 1048576
     AND body_text_kind IN ('text/plain', 'text/html-rendered'))
  ) NOT VALID,
  ADD CONSTRAINT agent_email_messages_attachment_storage_shape
  CHECK (
    attachment_storage_bytes BETWEEN 0 AND raw_size_bytes
    AND retained_attachment_storage_bytes
          BETWEEN 0 AND attachment_storage_bytes
    AND (
      (parse_state = 'parsed' AND attachment_count = 0
       AND attachment_storage_bytes = 0)
      OR
      ((parse_state = 'parsed' AND attachment_count > 0)
       OR parse_state = 'error')
       AND attachment_storage_bytes = raw_size_bytes
    )
    AND (
      (payload_retention_state = 'retained'
       AND retained_attachment_storage_bytes =
             attachment_storage_bytes)
      OR
      (payload_retention_state = 'omitted_capacity'
       AND attachment_storage_bytes > 0
       AND retained_attachment_storage_bytes = 0)
    )
  ) NOT VALID;

-- +goose Down
DROP TRIGGER agent_email_messages_maintain_attachment_counter
  ON agent_email_messages;
DROP FUNCTION maintain_agent_email_attachment_counter();

DROP TRIGGER agent_email_messages_normalize_legacy_storage
  ON agent_email_messages;
DROP FUNCTION normalize_legacy_agent_email_storage_insert();

ALTER TABLE agent_email_messages
  DROP CONSTRAINT agent_email_messages_attachment_storage_shape,
  DROP CONSTRAINT agent_email_messages_body_projection_shape,
  DROP CONSTRAINT agent_email_messages_raw_storage_shape,
  DROP COLUMN payload_retention_state,
  DROP COLUMN retained_attachment_storage_bytes,
  DROP COLUMN attachment_storage_bytes,
  DROP COLUMN attachment_storage_accounted,
  DROP COLUMN body_text_kind,
  DROP COLUMN body_text,
  ALTER COLUMN raw_mime SET NOT NULL,
  ADD CHECK (
    raw_size_bytes BETWEEN 1 AND 5242880
    AND raw_size_bytes = octet_length(raw_mime)
  );

ALTER TABLE accounts
  DROP COLUMN retained_agent_email_attachment_bytes;
