# Witself Inter-Agent Messaging

Status: first implementation slice. Last reviewed 2026-07-11. This document is
the authority for the durable messaging model, message shape, delivery and
ordering semantics, the anti-spoofing trust boundary, rate limits, scopes,
audit, and metering. It binds the message shapes pinned in
[json-contracts.md](json-contracts.md) and conforms to the master spec in
[requirements.md](requirements.md).

Inter-agent messaging is **fully in scope for v0**, not a stub. It ships a
durable mailbox/queue with at-least-once delivery, per-recipient ordering, and
explicit read/acknowledgement state.

## Implementation Status

The first slice ships direct agent-to-agent messaging inside one realm:

- Postgres-backed immutable messages and per-recipient delivery/read/ack state.
- Token-derived sender, account, and realm; caller-supplied actor fields are
  rejected.
- Idempotent send, metadata-only cursor-paginated inbox/outbox list, recipient
  read, and recipient ack through API, CLI, and MCP.
- Content-free send/deliver/read/ack audit events and complete account
  archive/restore coverage.
- MCP read acknowledges after a successful read; CLI read and ack remain
  separate explicit commands.

Group fan-out, cross-realm delivery, long-poll listen, dry-run, operator
metadata inspection, policy scopes, metering, and rate limits remain later
slices of this v0 design.

## Goal

Let an agent send a durable message to another agent (or to a security group)
inside the same realm, and let the recipient list, read, and acknowledge that
message — through the same one-core, multi-adapter spine that backs every other
Witself surface. Messaging is the channel through which agents coordinate; it is
also a new attack surface, because a message can carry instructions and data into
a receiving agent.

The central security stance: **the sender is always derived from the
authenticated token, never from input.** Sender forgery is structurally
impossible through the API. The receiving side treats every message body and
payload as **untrusted input**.

## Scope

- Realm-local messaging only here. A message's `from`, `to`, and the message
  itself all live in one realm. Cross-realm and cross-account collaboration is
  the documented go-forward extension of this model: it is specified in
  [agent-collaboration.md](agent-collaboration.md), which builds on this
  realm-local mailbox rather than replacing it. This document stays the
  authority for the realm-local model.
- Agent-to-agent and agent-to-group delivery. Group sends fan out to current
  group members with per-member delivery and ack state (see
  [security-groups.md](security-groups.md)).
- Text `body` plus an optional small structured `payload`. Large attachments and
  object/blob-backed payloads are post-v0.
- A message never carries authority. Receiving a message that asks for a
  cross-agent write does not authorize that write; writes still require policy
  (see [access-policy.md](access-policy.md)).

Out of scope for v0 here: large attachments, broadcast to all agents,
presence/typing indicators, message editing, and message recall/unsend.
Cross-realm messaging is not handled in this document; it is the go-forward
extension specified in [agent-collaboration.md](agent-collaboration.md).

## Model

- **Mailbox/queue per recipient.** Each recipient agent has a durable mailbox.
  Messages are persisted in Postgres (system of record) and survive process and
  pod churn; an ephemeral recipient pod can restart, re-mount its token file, and
  resume reading its mailbox as the same named agent.
- **Send.** A send creates one message record and the per-recipient delivery rows
  needed to fan it out. For an agent recipient that is one delivery; for a group
  recipient it is one delivery per current member.
- **Delivery.** Delivery is **at-least-once**. A recipient (or its runtime) may
  observe the same message more than once across retries, so recipients should
  treat `msg_` ids as the dedup key. Delivery is server-driven and durable; there
  is no fire-and-forget path.
- **Read/ack.** Read and acknowledgement are explicit, per-recipient state
  transitions. Listing a message does not read it; reading it does not ack it
  (see [Delivery, ordering, read/ack](#delivery-ordering-readack)).
- **Group fan-out is snapshot-at-send.** A group message is delivered to the
  members of the group **at send time**. Agents added to the group later do not
  retroactively receive earlier messages; agents removed before delivery do not
  receive them. The deciding membership is captured for audit.

## Message shape

The canonical JSON Message shape is shared by CLI, MCP, and API and is pinned in
[json-contracts.md](json-contracts.md). Fields:

```json
{
  "schema_version": "witself.v0",
  "id": "msg_8sk3...",
  "realm": "realm_2af9...",
  "from": "agent_archivist",
  "to": { "kind": "agent", "id": "agent_coordinator" },
  "subject": "handoff",
  "kind": "request",
  "body": "Please pick up the ingest run for batch 42.",
  "payload": { "batch": 42, "owner": "archivist" },
  "thread_id": "thr_91bd...",
  "created_at": "2026-06-26T17:04:11Z",
  "delivery": {
    "state": "delivered",
    "delivered_at": "2026-06-26T17:04:11Z"
  },
  "read": {
    "state": "unread",
    "read_at": null,
    "acked_at": null
  }
}
```

Field rules:

- `id` — stable, `msg_`-prefixed, realm-unique. The dedup key for at-least-once
  delivery.
- `realm` — the enclosing realm. Always derived server-side from the token.
- `from` — the **sender agent, always derived from the authenticated token,
  never from input.** A `from` supplied in a request body is rejected, not
  honored (see [Trust boundary](#trust-boundary-and-anti-spoofing)).
- `to` — a recipient reference: `{ "kind": "agent" | "group", "id": "..." }`.
  Resolved within the sender's realm. A group recipient is expressed as
  `witself://group/<group>` in references; see [requirements.md](requirements.md).
- `subject` — short human-readable classification (free text, length-capped).
- `kind` — convention-driven message type such as `note`, `request`, `reply`,
  `event`, or `handoff`. Like memory `kind`, it is a label for filtering, not a
  closed enum; unknown kinds are allowed.
- `body` — free-form text, the primary payload. Length-capped (see
  [Limits and rate limits](#limits-and-rate-limits)). **Untrusted on receipt.**
- `payload` — optional small structured object for machine-readable data.
  Length-capped. **Untrusted on receipt.**
- `thread_id` / conversation id — optional, `thr_`-prefixed. Groups messages
  into an ordered conversation; ordering is guaranteed per thread and per
  recipient (see below).
- `created_at` — server-assigned send timestamp.
- `delivery` — per-recipient delivery state and timestamp. In a group fan-out,
  each recipient has its own delivery row; the shape above is the recipient's
  view.
- `read` — per-recipient `unread` / `read` / `acked` state with timestamps.

Body and payload never appear in audit records, logs, metrics, or error
messages (see [Audit](#audit) and the redaction rules in
[requirements.md](requirements.md)).

## Trust boundary and anti-spoofing

Messaging introduces a new trust boundary: a message can carry instructions and
data **into** another agent. Two rules bound it.

### Sender is derived from the token

- The `from` agent on every message is resolved server-side from the
  authenticated bearer token, which is bound to one realm and one named agent.
- A caller-supplied `from`, `realm`, or actor field is ignored or rejected.
  Passing an agent name in a request body never lets a caller send **as** that
  agent. This mirrors the actor-from-token rule that governs every other Witself
  mutation (see [access-policy.md](access-policy.md)).
- Result: sender forgery is structurally impossible through the API. The
  authenticated sender recorded in audit is the real sender.

### Message content is untrusted input

- Receiving agents and their runtimes must treat `body` and `payload` as
  **untrusted**. A message is data, not a command, and not a grant.
- A message that asks the receiver to write to, curate, or forget another
  agent's identity data does **not** authorize that action. The write still
  requires a matching `allow` policy evaluated at write time (see
  [access-policy.md](access-policy.md)). A message cannot escalate privilege.
- The headline messaging threats — **spoofing** (mitigated by token-derived
  `from`), **injection** (prompt-injection / instruction-smuggling in `body` or
  `payload`), **memory-poisoning via message-driven writes**, and
  **interception** — are enumerated and owned in
  [threat-model.md](threat-model.md). This document defers to it for the full
  attacker model and mitigations.

## Delivery, ordering, read/ack

### Delivery semantics

- **At-least-once.** A durable delivery row is created per recipient at send
  time. The server retries until the recipient has the message available;
  duplicates are possible across retries and recipients dedup by `msg_` id.
- **Durable.** Messages and delivery rows persist in Postgres. Delivery survives
  `witself-server` restarts and recipient pod churn.
- **Group fan-out.** One send produces N delivery rows for the N members at send
  time; each member's delivery, read, and ack state is independent.

### Ordering

- Ordering is guaranteed **per recipient and per thread**: within one
  `thread_id`, a given recipient observes messages in send order
  (`created_at`, with `msg_` id as the deterministic tiebreaker).
- There is **no** total ordering across threads or across recipients. Messages in
  different threads, or to different recipients, may interleave.
- `message list` returns a recipient's mailbox newest-first by default, with
  cursor pagination and filters; thread order is recoverable with
  `--thread <id>`.

### Read and acknowledgement

Three explicit per-recipient states, each a distinct transition:

- `unread` → the message has been delivered but not opened.
- `read` → the recipient opened it via `message read`. Sets `read_at`. Read does
  not imply ack.
- `acked` → the recipient confirmed handling via `message ack`. Sets `acked_at`.
  Ack implies read.

Rules:

- `message list` shows metadata and does **not** change read state.
- `message read <id>` returns `body` + `payload` and transitions `unread` → `read`
  (idempotent; reading an already-read message is a no-op on state).
- `message ack <id>` transitions to `acked` (idempotent). Ack is the recipient's
  durable signal that the message was handled, distinct from merely having read
  it.
- Read and ack are recipient-only actions, gated by `message:read`. The sender
  cannot read or ack on the recipient's behalf.

## Surfaces

One model, three thin adapters over the same core, with authorization enforced
below every frontend (identical result across CLI, MCP, and API).

### CLI — the `message` group

- `message send` — send to an agent or group. Sender is the authenticated agent;
  there is no `--from`. Flags include `--to <agent|group>`, `--subject`,
  `--kind`, `--body` (or `--body-file`/stdin), `--payload` (JSON), and
  `--thread <id>`. Supports `--json`, `--dry-run`, and `--no-input`.
- `message list` — list the caller's mailbox. Filters: `--unread`, `--thread`,
  `--from`, `--kind`, time, with cursor pagination. Does not change read state.
- `message read <msg_id>` — read one message; returns `body`/`payload`;
  transitions to `read`.
- `message ack <msg_id>` — acknowledge one message; transitions to `acked`.

`--dry-run send` validates recipient existence, scopes, rate-limit headroom, and
quotas, and does **not** persist or deliver anything (consistent with the
dry-run rule in [requirements.md](requirements.md)).

The full CLI surface is enumerated in
[cli-command-surface.md](cli-command-surface.md).

### MCP — `witself.message.*`

- `witself.message.send`, `witself.message.list`, `witself.message.read`.
- stdio-first; same authorization as the CLI; honors `--read-only` mode (in which
  `send` is unavailable and only `list`/`read` are exposed; there is no separate
  `ack` MCP tool — ack is a side effect of `read` on MCP).
- The agent-token MCP session can only send **as** the token-bound agent.
- Tool inputs/outputs and exposure rules are pinned in
  [mcp-tools.md](mcp-tools.md).

### API — `/v1/messages` (+ colon actions)

- `POST /v1/messages` — send. `from` is taken from the token; a `from` in the
  body is rejected. Supports `Idempotency-Key` and `dry_run`.
- `GET /v1/messages` — list the caller's mailbox (cursor-paginated, filterable).
- `GET /v1/messages/{message_id}` — fetch one message; metadata only unless the
  caller is the recipient.
- `POST /v1/messages/{message_id}:read` — mark read (recipient only).
- `POST /v1/messages/{message_id}:ack` — acknowledge (recipient only).

All action subroutes use `POST`, never `GET`. Responses use the shared envelope
`{schema_version, ok, data, warnings}` and the error-code↔HTTP↔exit-code table.
Route style is pinned in [api-routes.md](api-routes.md) and the contract in
[api-contract.md](api-contract.md).

## Authorization

- `message:send` — required to send. Constrainable by realm and by recipient
  agent/group where practical.
- `message:read` — required to list, read, and ack one's own mailbox.
- Sending to a group requires `message:send`; it does **not** require group
  membership unless an operator constrains the scope. Group-recipient resolution
  follows [security-groups.md](security-groups.md).
- Messaging carries no implicit data-access authority. A message can reference
  identity data via `witself://` URIs, but resolving those references still runs
  the full policy check (see [access-policy.md](access-policy.md)); an
  unauthorized reference does not resolve just because it arrived in a message.
- Operators may inspect realm-wide message metadata under realm admin
  permissions (operator override), audited like any agent action.

The complete scope list lives in [requirements.md](requirements.md).

## Limits and rate limits

v0 defaults (tunable per plan; final values pinned in
[billing-and-limits.md](billing-and-limits.md)):

- Maximum `body` size: 64 KiB.
- Maximum `payload` size: 16 KiB serialized.
- Maximum `subject` length: 256 characters.
- Maximum group fan-out per send: capped (large groups are throttled, not
  silently truncated).

Rate limits apply to **both send and delivery** to bound abuse and
memory-poisoning amplification:

- Per-agent send rate limit (messages per interval).
- Per-realm aggregate send rate limit.
- Per-recipient delivery/inbound rate limit, to protect a single recipient from
  flooding.

Overage follows the plan's configured behavior — `warn`, `throttle`, or `block`
— per the model in [billing-and-limits.md](billing-and-limits.md). A blocked or
throttled send returns a deterministic error, never a silent drop.

## Audit

Every send, deliver, read, and ack is audited with the stable dotted event names
defined in [requirements.md](requirements.md):

- `message.sent` — on send. Records the token-derived `from`, the recipient
  (agent or group), `msg_` id, `kind`, `thread_id`, and — for group sends — the
  membership snapshot used for fan-out.
- `message.delivered` — per recipient on delivery.
- `message.read` — per recipient on read.
- `message.acked` — per recipient on ack.

Audit records carry non-sensitive context only — `msg_` id, sender/recipient
ids, `kind`, `subject` presence, `thread_id`, decision outcome. They **never**
contain `body`, `payload`, raw tokens, or any identity-content values. The same
redaction rule applies to logs, metrics, and error messages. Retention follows
[audit-retention.md](audit-retention.md).

## Metered dimensions

Messaging contributes two metered dimensions (see
[billing-and-limits.md](billing-and-limits.md)):

- **Messages sent** — counted at send (one per send request).
- **Messages delivered** — counted per recipient delivery (a group send of N
  members counts as N deliveries).

Both are metered even when pricing remains plan-tiered, because each send and
delivery is real service load and security-relevant use. Metrics expose
send/deliver/read counts with low-cardinality, route-template labels and never
include message content (see
[observability-and-operations.md](observability-and-operations.md)).

## Cross-references

- [requirements.md](requirements.md) — master spec; binds shapes, ids, scopes,
  and naming.
- [json-contracts.md](json-contracts.md) — canonical JSON Message shape and
  reference forms.
- [access-policy.md](access-policy.md) — why a message carries no authority;
  policy still gates every cross-agent write.
- [security-groups.md](security-groups.md) — group recipients and fan-out.
- [agent-collaboration.md](agent-collaboration.md) — the go-forward cross-realm /
  cross-account collaboration substrate that extends this realm-local mailbox.
- [threat-model.md](threat-model.md) — spoofing, injection, poisoning, and
  interception in messaging.
- [billing-and-limits.md](billing-and-limits.md) — limits, rate limits, and the
  metered send/deliver dimensions.
- [cli-command-surface.md](cli-command-surface.md),
  [mcp-tools.md](mcp-tools.md), [api-contract.md](api-contract.md),
  [api-routes.md](api-routes.md) — the three frontends.
- [audit-retention.md](audit-retention.md),
  [observability-and-operations.md](observability-and-operations.md) — audit and
  metrics handling.
