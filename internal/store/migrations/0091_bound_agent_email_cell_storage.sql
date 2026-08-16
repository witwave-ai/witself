-- +goose Up
-- This is a cell-level safety boundary, not a commercial account allowance.
-- Founder and other explicitly-unlimited accounts remain commercially
-- unlimited, while every writer in this database shares one bounded retained
-- email budget.  The database trigger is the authority so rolling old writers,
-- archive imports, direct maintenance writes, and every API replica cannot
-- bypass the same linearization point.

CREATE TABLE agent_email_cell_storage_capacity (
    singleton             SMALLINT    PRIMARY KEY DEFAULT 1,
    retained_bytes        BIGINT      NOT NULL DEFAULT 0,
    root_rows             BIGINT      NOT NULL DEFAULT 0,
    counted_rows          BIGINT      NOT NULL DEFAULT 0,
    admission_bytes       BIGINT      NOT NULL DEFAULT 3221225472,
    admission_root_rows   BIGINT      NOT NULL DEFAULT 25000,
    hard_bytes            BIGINT      NOT NULL DEFAULT 4294967296,
    hard_counted_rows     BIGINT      NOT NULL DEFAULT 100000,
    updated_at            TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    CHECK (singleton = 1),
    CHECK (retained_bytes BETWEEN 0 AND 4611686018427387903),
    CHECK (root_rows BETWEEN 0 AND 4611686018427387903),
    CHECK (counted_rows BETWEEN 0 AND 4611686018427387903),
    CHECK (root_rows <= counted_rows),
    CHECK (admission_bytes BETWEEN 1 AND 4611686018427387903),
    CHECK (admission_root_rows BETWEEN 1 AND 4611686018427387903),
    CHECK (hard_bytes BETWEEN 1 AND 4611686018427387903),
    CHECK (hard_counted_rows BETWEEN 1 AND 4611686018427387903),
    CHECK (admission_bytes < hard_bytes),
    CHECK (admission_root_rows < hard_counted_rows)
);

-- Twenty-five thousand admitted roots leave seventy-five thousand hard-reserve
-- rows: three lifecycle children per admitted root on average. Inbound normally
-- consumes one delivery; outbound normally consumes one provider event and may
-- consume one suppression. Repeated provider events remain bounded by the hard
-- cap instead of receiving an unbounded per-root promise.

INSERT INTO agent_email_cell_storage_capacity (singleton) VALUES (1);

-- Eight KiB per durable row conservatively charges tuple, visibility-map,
-- free-space-map, TOAST-pointer, index fan-out, and every small bounded
-- lifecycle field. Variable-length identity and customer-content values are
-- charged separately because PostgreSQL retains both raw MIME and its bounded
-- parsed projections. Mutable claim/provider/status fields are deliberately
-- omitted: ordinary claim, release, and terminalization must remain
-- charge-neutral at the hard boundary. The two otherwise-unbounded claim ids
-- are capped below so they also fit comfortably inside the fixed charge.
-- These functions intentionally use actual retained content: an attachment
-- omitted by the commercial account pool has a NULL raw_mime and must not be
-- charged as if its discarded bytes remained. Large parsed headers/body are
-- charged as content and therefore must be present at root admission; the
-- current ingestion path parses before its atomic message+delivery insert.

-- +goose StatementBegin
CREATE FUNCTION witself_agent_email_message_cell_storage_bytes(
    candidate agent_email_messages
)
RETURNS BIGINT
LANGUAGE SQL
IMMUTABLE
STRICT
AS $$
    SELECT 8192::BIGINT
         + octet_length(candidate.id)::BIGINT
         + octet_length(candidate.account_id)::BIGINT
         + octet_length(candidate.realm_id)::BIGINT
         + octet_length(candidate.mailbox_id)::BIGINT
         + octet_length(candidate.owner_agent_id)::BIGINT
         + octet_length(candidate.address_id)::BIGINT
         + octet_length(candidate.provider)::BIGINT
         + COALESCE(octet_length(candidate.provider_message_id), 0)::BIGINT
         + octet_length(candidate.envelope_sender)::BIGINT
         + octet_length(candidate.envelope_recipient)::BIGINT
         + octet_length(candidate.agent_segment)::BIGINT
         + octet_length(candidate.realm_label)::BIGINT
         + octet_length(candidate.recipient_route_kind)::BIGINT
         + COALESCE(octet_length(candidate.recipient_realm_alias_claim_id), 0)::BIGINT
         + COALESCE(octet_length(candidate.recipient_custom_domain_request_id), 0)::BIGINT
         + COALESCE(octet_length(candidate.subaddress_tag), 0)::BIGINT
         + COALESCE(octet_length(candidate.raw_mime), 0)::BIGINT
         + octet_length(candidate.raw_sha256)::BIGINT
         + COALESCE(octet_length(candidate.header_from), 0)::BIGINT
         + COALESCE(octet_length(candidate.header_to), 0)::BIGINT
         + COALESCE(octet_length(candidate.header_subject), 0)::BIGINT
         + COALESCE(octet_length(candidate.mime_message_id), 0)::BIGINT
         + COALESCE(octet_length(candidate.body_text), 0)::BIGINT
         + COALESCE(octet_length(candidate.body_text_kind), 0)::BIGINT
         + octet_length(candidate.duplicate_group_sha256)::BIGINT
         + COALESCE(octet_length(candidate.possible_duplicate_of_message_id), 0)::BIGINT
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION witself_agent_email_delivery_cell_storage_bytes(
    candidate agent_email_deliveries
)
RETURNS BIGINT
LANGUAGE SQL
IMMUTABLE
STRICT
AS $$
    SELECT 8192::BIGINT
         + octet_length(candidate.message_id)::BIGINT
         + octet_length(candidate.account_id)::BIGINT
         + octet_length(candidate.realm_id)::BIGINT
         + octet_length(candidate.mailbox_id)::BIGINT
         + octet_length(candidate.owner_agent_id)::BIGINT
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION witself_agent_email_outbound_cell_storage_bytes(
    candidate agent_email_outbound_messages
)
RETURNS BIGINT
LANGUAGE SQL
IMMUTABLE
STRICT
AS $$
    SELECT 8192::BIGINT
         + octet_length(candidate.id)::BIGINT
         + octet_length(candidate.account_id)::BIGINT
         + octet_length(candidate.realm_id)::BIGINT
         + octet_length(candidate.owner_agent_id)::BIGINT
         + octet_length(candidate.address_id)::BIGINT
         + octet_length(candidate.from_address)::BIGINT
         + octet_length(candidate.reply_to_address)::BIGINT
         + octet_length(candidate.to_address)::BIGINT
         + octet_length(candidate.subject)::BIGINT
         + octet_length(candidate.body_text)::BIGINT
         + octet_length(candidate.request_kind)::BIGINT
         + COALESCE(octet_length(candidate.reply_to_inbound_message_id), 0)::BIGINT
         + octet_length(candidate.thread_key)::BIGINT
         + COALESCE(octet_length(candidate.in_reply_to_header), 0)::BIGINT
         + COALESCE((
               SELECT sum(octet_length(header))::BIGINT
                 FROM unnest(candidate.references_headers) AS header
           ), 0)::BIGINT
         + octet_length(candidate.idempotency_key_hash)::BIGINT
         + octet_length(candidate.request_hash)::BIGINT
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION witself_agent_email_provider_event_cell_storage_bytes(
    candidate agent_email_outbound_provider_events
)
RETURNS BIGINT
LANGUAGE SQL
IMMUTABLE
STRICT
AS $$
    SELECT 8192::BIGINT
         + octet_length(candidate.account_id)::BIGINT
         + octet_length(candidate.provider)::BIGINT
         + octet_length(candidate.event_id_hash)::BIGINT
         + octet_length(candidate.event_request_hash)::BIGINT
         + octet_length(candidate.outbound_id)::BIGINT
         + octet_length(candidate.event_class)::BIGINT
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION witself_agent_email_suppression_cell_storage_bytes(
    candidate agent_email_outbound_recipient_suppressions
)
RETURNS BIGINT
LANGUAGE SQL
IMMUTABLE
STRICT
AS $$
    SELECT 8192::BIGINT
         + octet_length(candidate.account_id)::BIGINT
         + octet_length(candidate.recipient_sha256)::BIGINT
$$;
-- +goose StatementEnd

-- One AFTER-trigger function serves all five table-specific triggers. Existing
-- account-evacuation and account-attachment triggers run before this `zz_`
-- trigger, preserving the global account -> cell lock order. INSERT of a new
-- correspondence root must
-- fit both the admission and hard boundaries.  Child/lifecycle writes and
-- positive updates consume the reserved space up to the hard boundary.  Every
-- delete is allowed and releases its exact old charge.
-- +goose StatementBegin
CREATE FUNCTION witself_maintain_agent_email_cell_storage_capacity()
RETURNS trigger
LANGUAGE plpgsql
AS $$
DECLARE
    old_charge BIGINT := 0;
    new_charge BIGINT := 0;
    byte_delta BIGINT := 0;
    row_delta BIGINT := 0;
    root_delta BIGINT := 0;
    root_insert BOOLEAN := false;
BEGIN
    IF TG_TABLE_NAME = 'agent_email_messages' THEN
        IF TG_OP <> 'INSERT' THEN
            old_charge := witself_agent_email_message_cell_storage_bytes(OLD);
        END IF;
        IF TG_OP <> 'DELETE' THEN
            new_charge := witself_agent_email_message_cell_storage_bytes(NEW);
        END IF;
        root_insert := TG_OP = 'INSERT';
        IF TG_OP = 'INSERT' THEN
            root_delta := 1;
        ELSIF TG_OP = 'DELETE' THEN
            root_delta := -1;
        END IF;
    ELSIF TG_TABLE_NAME = 'agent_email_deliveries' THEN
        IF TG_OP <> 'INSERT' THEN
            old_charge := witself_agent_email_delivery_cell_storage_bytes(OLD);
        END IF;
        IF TG_OP <> 'DELETE' THEN
            new_charge := witself_agent_email_delivery_cell_storage_bytes(NEW);
        END IF;
    ELSIF TG_TABLE_NAME = 'agent_email_outbound_messages' THEN
        IF TG_OP <> 'INSERT' THEN
            old_charge := witself_agent_email_outbound_cell_storage_bytes(OLD);
        END IF;
        IF TG_OP <> 'DELETE' THEN
            new_charge := witself_agent_email_outbound_cell_storage_bytes(NEW);
        END IF;
        root_insert := TG_OP = 'INSERT';
        IF TG_OP = 'INSERT' THEN
            root_delta := 1;
        ELSIF TG_OP = 'DELETE' THEN
            root_delta := -1;
        END IF;
    ELSIF TG_TABLE_NAME = 'agent_email_outbound_provider_events' THEN
        IF TG_OP <> 'INSERT' THEN
            old_charge := witself_agent_email_provider_event_cell_storage_bytes(OLD);
        END IF;
        IF TG_OP <> 'DELETE' THEN
            new_charge := witself_agent_email_provider_event_cell_storage_bytes(NEW);
        END IF;
    ELSIF TG_TABLE_NAME = 'agent_email_outbound_recipient_suppressions' THEN
        IF TG_OP <> 'INSERT' THEN
            old_charge := witself_agent_email_suppression_cell_storage_bytes(OLD);
        END IF;
        IF TG_OP <> 'DELETE' THEN
            new_charge := witself_agent_email_suppression_cell_storage_bytes(NEW);
        END IF;
    ELSE
        RAISE EXCEPTION 'unexpected agent-email cell-storage table %', TG_TABLE_NAME
            USING ERRCODE = '55000';
    END IF;

    byte_delta := new_charge - old_charge;
    IF TG_OP = 'INSERT' THEN
        row_delta := 1;
    ELSIF TG_OP = 'DELETE' THEN
        row_delta := -1;
    END IF;

    IF byte_delta = 0 AND row_delta = 0 AND root_delta = 0 THEN
        IF TG_OP = 'DELETE' THEN
            RETURN OLD;
        END IF;
        RETURN NEW;
    END IF;

    IF byte_delta > 0 OR row_delta > 0 OR root_delta > 0 THEN
        UPDATE agent_email_cell_storage_capacity
           SET retained_bytes = retained_bytes + byte_delta,
               root_rows = root_rows + root_delta,
               counted_rows = counted_rows + row_delta,
               updated_at = clock_timestamp()
         WHERE singleton = 1
           AND byte_delta >= 0
           AND row_delta >= 0
           AND root_delta >= 0
           AND byte_delta <= hard_bytes
           AND retained_bytes <= hard_bytes - byte_delta
           AND retained_bytes <= 4611686018427387903 - byte_delta
           AND counted_rows <= hard_counted_rows - row_delta
           AND counted_rows <= 4611686018427387903 - row_delta
           AND root_rows <= 4611686018427387903 - root_delta
           AND (
             NOT root_insert OR (
               byte_delta <= admission_bytes
               AND retained_bytes <= admission_bytes - byte_delta
               AND root_rows <= admission_root_rows - root_delta
             )
           );
        IF NOT FOUND THEN
            IF NOT EXISTS (
                SELECT 1 FROM agent_email_cell_storage_capacity
                 WHERE singleton = 1
            ) THEN
                RAISE EXCEPTION 'agent-email cell storage capacity state is missing'
                    USING ERRCODE = '55000';
            END IF;
            RAISE EXCEPTION 'agent-email cell storage capacity reached'
                USING ERRCODE = 'WE001',
                      CONSTRAINT = 'agent_email_cell_storage_capacity';
        END IF;
    ELSE
        -- Deletes and charge-reducing updates remain possible above either
        -- configured threshold.  Refuse only invariant corruption/underflow.
        UPDATE agent_email_cell_storage_capacity
           SET retained_bytes = retained_bytes + byte_delta,
               root_rows = root_rows + root_delta,
               counted_rows = counted_rows + row_delta,
               updated_at = clock_timestamp()
         WHERE singleton = 1
           AND retained_bytes >= -byte_delta
           AND root_rows >= -root_delta
           AND counted_rows >= -row_delta;
        IF NOT FOUND THEN
            RAISE EXCEPTION 'agent-email cell storage capacity underflow or missing state'
                USING ERRCODE = '55000';
        END IF;
    END IF;

    IF TG_OP = 'DELETE' THEN
        RETURN OLD;
    END IF;
    RETURN NEW;
END
$$;
-- +goose StatementEnd

-- Fence every supported writer before measuring the existing rows.  Accounts
-- are locked first, matching application order; NOWAIT makes deployment retry
-- rather than waiting behind an unexpectedly long live transaction.  Direct
-- child-table writers are fenced by the following table locks.
LOCK TABLE accounts IN EXCLUSIVE MODE NOWAIT;
LOCK TABLE agent_email_deliveries,
           agent_email_outbound_messages
    IN ACCESS EXCLUSIVE MODE NOWAIT;
LOCK TABLE agent_email_messages,
           agent_email_outbound_provider_events,
           agent_email_outbound_recipient_suppressions
    IN SHARE ROW EXCLUSIVE MODE NOWAIT;

ALTER TABLE agent_email_deliveries
    ADD CONSTRAINT agent_email_deliveries_claim_id_storage_bound
    CHECK (claim_id IS NULL OR octet_length(claim_id) <= 128) NOT VALID;
ALTER TABLE agent_email_deliveries
    VALIDATE CONSTRAINT agent_email_deliveries_claim_id_storage_bound;

ALTER TABLE agent_email_outbound_messages
    ADD CONSTRAINT agent_email_outbound_claim_id_storage_bound
    CHECK (claim_id IS NULL OR octet_length(claim_id) <= 128) NOT VALID;
ALTER TABLE agent_email_outbound_messages
    VALIDATE CONSTRAINT agent_email_outbound_claim_id_storage_bound;

WITH measured AS (
    SELECT
      COALESCE((SELECT sum(witself_agent_email_message_cell_storage_bytes(row_value))
                  FROM agent_email_messages AS row_value), 0)::BIGINT
      + COALESCE((SELECT sum(witself_agent_email_delivery_cell_storage_bytes(row_value))
                    FROM agent_email_deliveries AS row_value), 0)::BIGINT
      + COALESCE((SELECT sum(witself_agent_email_outbound_cell_storage_bytes(row_value))
                    FROM agent_email_outbound_messages AS row_value), 0)::BIGINT
      + COALESCE((SELECT sum(witself_agent_email_provider_event_cell_storage_bytes(row_value))
                    FROM agent_email_outbound_provider_events AS row_value), 0)::BIGINT
      + COALESCE((SELECT sum(witself_agent_email_suppression_cell_storage_bytes(row_value))
                    FROM agent_email_outbound_recipient_suppressions AS row_value), 0)::BIGINT
        AS retained_bytes,
      ((SELECT count(*) FROM agent_email_messages)
       + (SELECT count(*) FROM agent_email_outbound_messages))::BIGINT
        AS root_rows,
      ((SELECT count(*) FROM agent_email_messages)
       + (SELECT count(*) FROM agent_email_deliveries)
       + (SELECT count(*) FROM agent_email_outbound_messages)
       + (SELECT count(*) FROM agent_email_outbound_provider_events)
       + (SELECT count(*) FROM agent_email_outbound_recipient_suppressions))::BIGINT
        AS counted_rows
)
UPDATE agent_email_cell_storage_capacity AS capacity
   SET retained_bytes = measured.retained_bytes,
       root_rows = measured.root_rows,
       counted_rows = measured.counted_rows,
       updated_at = clock_timestamp()
  FROM measured
 WHERE capacity.singleton = 1;

CREATE TRIGGER zz_agent_email_cell_storage_capacity
AFTER INSERT OR UPDATE OR DELETE ON agent_email_messages
FOR EACH ROW EXECUTE FUNCTION witself_maintain_agent_email_cell_storage_capacity();

CREATE TRIGGER zz_agent_email_cell_storage_capacity
AFTER INSERT OR UPDATE OR DELETE ON agent_email_deliveries
FOR EACH ROW EXECUTE FUNCTION witself_maintain_agent_email_cell_storage_capacity();

CREATE TRIGGER zz_agent_email_cell_storage_capacity
AFTER INSERT OR UPDATE OR DELETE ON agent_email_outbound_messages
FOR EACH ROW EXECUTE FUNCTION witself_maintain_agent_email_cell_storage_capacity();

CREATE TRIGGER zz_agent_email_cell_storage_capacity
AFTER INSERT OR UPDATE OR DELETE ON agent_email_outbound_provider_events
FOR EACH ROW EXECUTE FUNCTION witself_maintain_agent_email_cell_storage_capacity();

CREATE TRIGGER zz_agent_email_cell_storage_capacity
AFTER INSERT OR UPDATE OR DELETE ON agent_email_outbound_recipient_suppressions
FOR EACH ROW EXECUTE FUNCTION witself_maintain_agent_email_cell_storage_capacity();

-- Row triggers do not fire for TRUNCATE. Refuse it rather than letting a
-- privileged maintenance shortcut silently corrupt the ledger; bounded batch
-- DELETE remains available and releases capacity transactionally.
-- +goose StatementBegin
CREATE FUNCTION witself_forbid_agent_email_cell_storage_truncate()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    RAISE EXCEPTION 'truncate would bypass agent-email cell storage accounting'
        USING ERRCODE = '55000';
END
$$;
-- +goose StatementEnd

CREATE TRIGGER zz_agent_email_cell_storage_no_truncate
BEFORE TRUNCATE ON agent_email_messages
FOR EACH STATEMENT EXECUTE FUNCTION witself_forbid_agent_email_cell_storage_truncate();

CREATE TRIGGER zz_agent_email_cell_storage_no_truncate
BEFORE TRUNCATE ON agent_email_deliveries
FOR EACH STATEMENT EXECUTE FUNCTION witself_forbid_agent_email_cell_storage_truncate();

CREATE TRIGGER zz_agent_email_cell_storage_no_truncate
BEFORE TRUNCATE ON agent_email_outbound_messages
FOR EACH STATEMENT EXECUTE FUNCTION witself_forbid_agent_email_cell_storage_truncate();

CREATE TRIGGER zz_agent_email_cell_storage_no_truncate
BEFORE TRUNCATE ON agent_email_outbound_provider_events
FOR EACH STATEMENT EXECUTE FUNCTION witself_forbid_agent_email_cell_storage_truncate();

CREATE TRIGGER zz_agent_email_cell_storage_no_truncate
BEFORE TRUNCATE ON agent_email_outbound_recipient_suppressions
FOR EACH STATEMENT EXECUTE FUNCTION witself_forbid_agent_email_cell_storage_truncate();

-- +goose Down
-- Removing the trigger while durable mail remains would silently fail open.
-- Lock in the same account -> child order and refuse before changing anything.
LOCK TABLE accounts IN EXCLUSIVE MODE;
LOCK TABLE agent_email_messages,
           agent_email_deliveries,
           agent_email_outbound_messages,
           agent_email_outbound_provider_events,
           agent_email_outbound_recipient_suppressions
    IN ACCESS EXCLUSIVE MODE;

-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM agent_email_messages)
       OR EXISTS (SELECT 1 FROM agent_email_deliveries)
       OR EXISTS (SELECT 1 FROM agent_email_outbound_messages)
       OR EXISTS (SELECT 1 FROM agent_email_outbound_provider_events)
       OR EXISTS (SELECT 1 FROM agent_email_outbound_recipient_suppressions)
       OR EXISTS (
         SELECT 1 FROM agent_email_cell_storage_capacity
          WHERE retained_bytes <> 0 OR root_rows <> 0 OR counted_rows <> 0
       )
    THEN
        RAISE EXCEPTION USING
            ERRCODE = '55000',
            MESSAGE = 'cannot downgrade schema 0091 while durable agent-email storage exists';
    END IF;
END
$$;
-- +goose StatementEnd

DROP TRIGGER zz_agent_email_cell_storage_no_truncate
    ON agent_email_outbound_recipient_suppressions;
DROP TRIGGER zz_agent_email_cell_storage_no_truncate
    ON agent_email_outbound_provider_events;
DROP TRIGGER zz_agent_email_cell_storage_no_truncate
    ON agent_email_outbound_messages;
DROP TRIGGER zz_agent_email_cell_storage_no_truncate
    ON agent_email_deliveries;
DROP TRIGGER zz_agent_email_cell_storage_no_truncate
    ON agent_email_messages;

DROP TRIGGER zz_agent_email_cell_storage_capacity
    ON agent_email_outbound_recipient_suppressions;
DROP TRIGGER zz_agent_email_cell_storage_capacity
    ON agent_email_outbound_provider_events;
DROP TRIGGER zz_agent_email_cell_storage_capacity
    ON agent_email_outbound_messages;
DROP TRIGGER zz_agent_email_cell_storage_capacity
    ON agent_email_deliveries;
DROP TRIGGER zz_agent_email_cell_storage_capacity
    ON agent_email_messages;

DROP FUNCTION witself_forbid_agent_email_cell_storage_truncate();
DROP FUNCTION witself_maintain_agent_email_cell_storage_capacity();
DROP FUNCTION witself_agent_email_suppression_cell_storage_bytes(
    agent_email_outbound_recipient_suppressions
);
DROP FUNCTION witself_agent_email_provider_event_cell_storage_bytes(
    agent_email_outbound_provider_events
);
DROP FUNCTION witself_agent_email_outbound_cell_storage_bytes(
    agent_email_outbound_messages
);
DROP FUNCTION witself_agent_email_delivery_cell_storage_bytes(
    agent_email_deliveries
);
DROP FUNCTION witself_agent_email_message_cell_storage_bytes(
    agent_email_messages
);
ALTER TABLE agent_email_outbound_messages
    DROP CONSTRAINT agent_email_outbound_claim_id_storage_bound;
ALTER TABLE agent_email_deliveries
    DROP CONSTRAINT agent_email_deliveries_claim_id_storage_bound;
DROP TABLE agent_email_cell_storage_capacity;
