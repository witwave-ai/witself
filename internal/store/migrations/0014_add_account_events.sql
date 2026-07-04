-- +goose Up
-- Account security audit trail: an append-only ledger of security-relevant
-- events on each account. Written inline with the mutating transaction so
-- an event never lands without its cause and vice versa. Owner-readable
-- only — audit is a trust-boundary artifact, not a general operator view.
--
-- verb is namespaced ("email.changed", "recovery.completed", etc.) so a
-- future consumer can filter by category without a table redesign.
-- actor_kind distinguishes owner-initiated from control-plane-forwarded
-- from system events; actor_id is the operator or agent id when the actor
-- is a principal, NULL for system/control_plane events.
--
-- metadata carries per-verb context (masked emails, ids, etc.). PII lives
-- ONLY in masked form — no plaintext emails, no token values or hashes.
-- The registry in internal/store/events.go validates each verb's expected
-- shape at write time so drift is caught before it lands.
--
-- retain_until is left NULL for MVP (meaning: keep forever). A future
-- retention slice can set it and add a sweep — the column existing now
-- avoids a schema change later.
CREATE TABLE account_events (
  id           TEXT PRIMARY KEY,
  account_id   TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  occurred_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
  actor_kind   TEXT NOT NULL,
  actor_id     TEXT,
  verb         TEXT NOT NULL,
  metadata     JSONB NOT NULL DEFAULT '{}'::jsonb,
  retain_until TIMESTAMPTZ
);

-- Read pattern: the owner's paginated view, filtered by account_id and
-- often bounded by a time range — DESC because the interesting entries
-- are the most recent.
CREATE INDEX account_events_by_account_time
  ON account_events (account_id, occurred_at DESC);

-- Verb filter: a later query pattern will be "show me every
-- recovery.requested since <date>". Small compared to (account, time)
-- but cheap given the row width.
CREATE INDEX account_events_by_verb
  ON account_events (verb, occurred_at DESC);

-- +goose Down
DROP INDEX account_events_by_verb;
DROP INDEX account_events_by_account_time;
DROP TABLE account_events;
