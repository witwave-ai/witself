# Witself Transcript Ledger

Status: first implementation slice. Decision: Witself stores an append-only
ledger of the visible conversation an agent runtime chooses to record. The
ledger is distinct from addressed inter-agent messaging.

## Goal

Give agent runtimes and enterprise operators a portable record of what crossed
the model boundary without pretending that Witself can or should capture a
model's private reasoning.

A normal exchange is two immutable entries in one transcript:

1. A `user` entry records the prompt delivered to the model.
2. An `assistant` entry records the finalized response and points to the prompt
   through `reply_to_entry_id`.

Additional turns share the transcript id and receive a monotonically increasing
sequence number. Explicit `system` and `tool` entries may be recorded when an
integration needs an execution trace. Streaming token chunks are not entries;
the integration records the finalized visible result.

## Boundary From Messaging And Memory

- A **transcript** is an append-only record of visible interaction. The
  authenticated agent token identifies the recorder, while `role` describes
  the recorded participant and is treated as asserted data.
- A **message** is an addressed delivery between authenticated agents. Its
  sender is always the token-bound agent and can never be supplied as data. See
  [inter-agent-messaging.md](inter-agent-messaging.md).
- A **memory** is curated durable identity state. Transcript entries do not
  automatically become memories; an explicit extraction or curation step is
  required. This keeps conversational noise and prompt injection out of the
  agent's durable self.
- An **audit event** proves that an operation occurred without copying prompt or
  response bodies into the audit ledger.

Keeping these resources separate preserves the anti-spoofing boundary for A2A
messages and lets each resource have independent retention and access policy.

## First-Slice Model

`transcript_conversations` owns the ordered thread:

- `trn_` id, account, realm, and owning/recording agent.
- Optional external conversation id, title, and small JSON metadata object.
- `next_sequence`, created time, and last-updated time.

`transcript_entries` is append-only:

- `ent_` id, transcript id, sequence, and token-derived recorder agent.
- Optional external message id, unique within the transcript, for idempotent
  capture when an integration retries a write.
- `role`: `user`, `assistant`, `system`, or `tool`.
- Visible text body, optional small JSON payload, optional model label, and an
  optional same-transcript `reply_to_entry_id`.
- A reserved `artifacts` array. The first slice accepts only an empty array; the
  field exists so file references can be added without changing the entry
  envelope.

The first-slice limits are 256 characters for a title, 512 characters for an
external id, 64 KiB for body text, and 16 KiB serialized for metadata or
payload. At least one of body or payload must be present on an entry.

Agent tokens may create, append, list, and read their own transcripts. Account
operator tokens may list and read every transcript in their account for audit
and compliance, but they cannot manufacture entries on an agent's behalf.

## Reasoning And Execution Traces

Witself does not request, expose, or store raw hidden chain-of-thought. When an
enterprise needs to understand a complex action, an integration may record:

- The visible prompt and final response.
- Explicit tool calls and sanitized tool results as `tool` entries.
- A concise decision or rationale summary in a visible assistant/system entry.
- Model, timing, citation, and cost metadata in the small JSON payload.

This provides an inspectable execution record without treating private model
reasoning or every internal token as durable customer data.

## Structured Objects And File Artifacts

Postgres `jsonb` is the right first-slice home for small structured objects:
tool arguments, result summaries, citations, decisions, and other bounded JSON.
It remains queryable and travels with the account archive.

Postgres can technically store arbitrary bytes in `bytea`, but Witself will not
use ordinary rows as a general file store. Generated documents, images,
archives, and other binary artifacts require a cell-local object-store adapter.
When that slice lands:

- Postgres stores `art_` metadata, ownership, content type, size, checksum,
  source entry, version, retention, and object key.
- Object storage holds the bytes.
- Account export/import carries both the metadata and blob manifest/content so
  moving an account never leaves its artifacts behind.
- Revisions create new immutable versions rather than overwriting prior output.

Until then, an integration may store a bounded structured result in `payload`,
but non-empty file attachments are refused instead of creating a non-portable
reference.

## First-Slice Surfaces

API:

```text
GET  /v1/transcripts
POST /v1/transcripts
GET  /v1/transcripts/{transcript_id}
POST /v1/transcripts/{transcript_id}/entries
```

CLI:

```text
witself transcript create
witself transcript append TRANSCRIPT_ID
witself transcript list
witself transcript show TRANSCRIPT_ID
```

Transcript rows are included in the account's logical export/import stream in
foreign-key order. Bodies and payloads must never appear in logs, metrics,
errors, or account-event metadata.

The first slice has no edit or delete endpoint: entries remain immutable and
are retained with the account. Configurable retention, legal holds, redaction,
and an explicitly audited deletion workflow are later enterprise-policy work,
not implicit side effects of reading or moving an account.
