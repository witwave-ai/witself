-- +goose Up
-- Support tickets: the customer-facing thread between an account's operators
-- and Witwave fleet admins. Tickets live on the tenant's cell (not centrally)
-- so they travel through evacuate → R2 → restore alongside the rest of the
-- audit trail. Both the opening description and every subsequent reply are
-- rows in support_ticket_messages; the parent row carries only the state
-- machine and the pointers a lister needs.
--
-- Visibility: any operator token on the account can list/read/reply to any
-- of the account's tickets (not owner-only, not filer-only) — matches how
-- team-based support systems work. Enforced at the server layer via
-- requireOperator (which already checks account membership).
--
-- state is a namespaced Go const set enforced by the store, not a Postgres
-- CHECK, so states can be added later without a migration. Slice 1 legal
-- values: 'open', 'awaiting_admin', 'awaiting_customer', 'resolved',
-- 'closed'. Transitions: open→awaiting_admin (first admin sees it),
-- awaiting_admin→awaiting_customer (admin replies), awaiting_customer→
-- awaiting_admin (customer replies), any→resolved (admin says done),
-- resolved→closed (customer confirms) OR resolved→awaiting_admin (customer
-- reopens).
--
-- category is coarse ('technical' | 'billing' | 'security' | 'other');
-- fine-grained taxonomy is a later slice.
--
-- priority + first_response_at + resolved_at are present from day one but
-- NOT ENFORCED in slice 1 — the enterprise SLA slice will populate and
-- enforce them. Bake-in-now avoids a non-additive migration when SLA lands.
--
-- correlation is a JSONB array of {kind, id} — the soft join back to the
-- audit ledger or other resources. Not a hard FK because account_events
-- may be pruned by a later retention sweep; correlation entries dangle
-- gracefully. GIN index makes reverse-lookup ("what tickets reference
-- this event?") cheap.
--
-- retain_until mirrors account_events: NULL means keep forever; a future
-- retention slice can populate and sweep. Closed tickets stay by default.
CREATE TABLE support_tickets (
  id                 TEXT PRIMARY KEY,
  account_id         TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  opened_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
  opened_by_kind     TEXT NOT NULL,
  opened_by_id       TEXT NOT NULL,
  subject            TEXT NOT NULL,
  category           TEXT NOT NULL DEFAULT 'other',
  state              TEXT NOT NULL DEFAULT 'open',
  priority           TEXT NOT NULL DEFAULT 'normal',
  first_response_at  TIMESTAMPTZ,
  resolved_at        TIMESTAMPTZ,
  closed_at          TIMESTAMPTZ,
  last_activity_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
  last_message_id    TEXT,
  correlation        JSONB NOT NULL DEFAULT '[]'::jsonb,
  metadata           JSONB NOT NULL DEFAULT '{}'::jsonb,
  retain_until       TIMESTAMPTZ
);

-- Read patterns: the operator's list of their account's tickets, and the
-- fleet admin's per-account drill-down. Both want newest activity first.
CREATE INDEX support_tickets_by_account_activity
  ON support_tickets (account_id, last_activity_at DESC);

-- Fleet-wide "show me all open tickets on this cell" — the control plane
-- fans out one query per cell to build the fleet-wide admin queue.
CREATE INDEX support_tickets_open_by_activity
  ON support_tickets (state, last_activity_at DESC)
  WHERE state IN ('open', 'awaiting_admin', 'awaiting_customer');

-- Reverse lookup: "what tickets reference this event or resource id?"
CREATE INDEX support_tickets_correlation_gin
  ON support_tickets USING GIN (correlation jsonb_path_ops);

-- Messages: every ticket has ≥1 message (the opener's description).
-- Replies from the customer AND from fleet admins live in the same table
-- so ordering is a single monotonic thread.
--
-- author_kind = 'owner' | 'operator' | 'fleet_admin' | 'system'. When
-- author_kind is 'fleet_admin', author_id is the admin's chosen HANDLE
-- (e.g. "sarah") not a machine id — the admin is not a principal on the
-- tenant's cell so we record only what the control plane asserted.
-- 'system' entries have author_id NULL (state transitions logged as
-- messages for the timeline view).
--
-- body is TEXT, capped to 64 KiB at the store layer for slice 1.
-- attachments is a reserved JSONB column so a later slice can add file
-- refs without a migration; the store layer refuses non-empty
-- attachments in slice 1 writes.
CREATE TABLE support_ticket_messages (
  id            TEXT PRIMARY KEY,
  ticket_id     TEXT NOT NULL REFERENCES support_tickets(id) ON DELETE CASCADE,
  account_id    TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  posted_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
  author_kind   TEXT NOT NULL,
  author_id     TEXT,
  body          TEXT NOT NULL,
  attachments   JSONB NOT NULL DEFAULT '[]'::jsonb,
  metadata      JSONB NOT NULL DEFAULT '{}'::jsonb
);

-- Thread-read pattern: fetch one ticket's messages in chronological order
-- (ASC, oldest-first — the opposite of the events feed).
CREATE INDEX support_ticket_messages_by_ticket_time
  ON support_ticket_messages (ticket_id, posted_at, id);

-- Account-scoped denormalization for the archive stream and for
-- "find all messages by author X on this account" audit queries.
CREATE INDEX support_ticket_messages_by_account
  ON support_ticket_messages (account_id, posted_at DESC);

-- Account-level policy for whether operators may open support tickets.
-- Default 'enabled' so slice 1 works out of the box. Future free-tier
-- gating (#16) can flip this to 'disabled' at plan-tier assignment
-- time. 'disabled' means POST /v1/support/tickets refuses; existing
-- open tickets remain readable.
ALTER TABLE accounts ADD COLUMN support_policy TEXT NOT NULL DEFAULT 'enabled';

-- +goose Down
ALTER TABLE accounts DROP COLUMN support_policy;
DROP INDEX support_ticket_messages_by_account;
DROP INDEX support_ticket_messages_by_ticket_time;
DROP TABLE support_ticket_messages;
DROP INDEX support_tickets_correlation_gin;
DROP INDEX support_tickets_open_by_activity;
DROP INDEX support_tickets_by_account_activity;
DROP TABLE support_tickets;
