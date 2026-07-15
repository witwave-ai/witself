# Witself Inter-Agent Messaging

Status: direct messaging and autonomous-processing slice. Last reviewed
2026-07-14. This document is
the authority for the durable messaging model, message shape, delivery and
ordering semantics, the anti-spoofing trust boundary, rate limits, scopes,
audit, and metering. It binds the message shapes pinned in
[json-contracts.md](json-contracts.md) and conforms to the master spec in
[requirements.md](requirements.md).

Inter-agent messaging is **fully in scope for v0**, not a stub. It ships a
durable mailbox/queue with at-least-once delivery, per-recipient ordering, and
explicit read/acknowledgement state.

The direct autonomous client runner is implemented. Explicit-recipient-set,
realm-audience, and open request/claim extensions are specified in
[autonomous-realm-messaging.md](autonomous-realm-messaging.md). That document
extends this mailbox; it does not replace the message, delivery, ordering, or
trust contracts here.

## Implementation Status

The current checkout ships direct agent-to-agent messaging and fenced
text-only autonomous processing inside one realm:

- Postgres-backed immutable messages and per-recipient delivery/read/ack state.
- Token-derived sender, account, and realm; caller-supplied actor fields are
  rejected.
- Idempotent send, recipient-only reply with a validated parent, metadata-only
  cursor-paginated inbox/outbox list, and stateless long-poll listen for oldest
  unacknowledged inbound metadata.
- Recipient read and recipient ack are separate operations through API, CLI,
  and MCP.
- Recipient processing claim, renew, release, and atomic complete are separate
  operations through API, CLI, and MCP. Migration `0034` adds a monotonic
  generation fence, database-time lease, and unique durable result-message link
  to each direct delivery without overloading read or acknowledgement.
- Migration `0035` adds backend-derived causal depth. Direct sends start at one;
  every validated reply advances exactly one from its durable parent. Clients
  cannot submit or reset this field.
- Migration `0036` adds a durable, per-delivery `failure_count` independent of
  the processing-generation fence. Only an exact-fence release explicitly
  marked as a deterministic message failure increments it.
- A client-owned runner can listen, claim, read, invoke a capability-probed
  text-only provider, renew, atomically complete, and then acknowledge. Its
  local lifecycle is
  `message runner enable|disable|status|notifications|run|serve|start`, with
  per-user launchd/systemd supervision.
- Enable captures only recognized authentication environment values for the
  selected native provider in a separate mode-0600 local file. Configuration,
  service definitions, and account export remain provider-credential-free.
- Runner status exposes content-free cycle health: last cycle/success time,
  bounded status/error class, and consecutive failures. It never stores an
  error string, message id/content, provider output, or credential.
- Content-free send/deliver/read/ack audit events and complete account
  archive/restore coverage, including reply-parent causality, causal depth, and
  processing state. Import interrupts active claims while preserving completed
  result links.

Explicit-list/realm fan-out, group fan-out, open multi-assignee request claims,
cross-realm delivery, dry-run, operator metadata inspection, policy scopes,
metering, and rate limits remain later slices of this v0 design. These
implementation statements describe the current checkout, not a deployment or
release.

## Goal

Let an agent send a durable message to another agent, a bounded explicit set of
agents, the current realm, or a security group inside the same realm, and let
each recipient list, read, and acknowledge that message — through the same
one-core, multi-adapter spine that backs every other Witself surface. Messaging
is the channel through which agents coordinate; it is also a new attack surface,
because a message can carry instructions and data into a receiving agent.

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
- Agent, bounded explicit-agent-list, realm, and security-group audiences. List,
  realm, and group delivery use a send-time membership snapshot with per-agent
  delivery and ack state. The current implementation supports only one direct
  agent. The realm is the built-in team/room for the autonomous messaging use
  case; no separate named messaging-team resource is required.
- Text `body` plus an optional small structured `payload`. Large attachments and
  object/blob-backed payloads are post-v0.
- A message never carries authority. Receiving a message that asks for a
  cross-agent write does not authorize that write; writes still require policy
  (see [access-policy.md](access-policy.md)).

Out of scope for v0 here: large attachments, presence/typing indicators,
message editing, and message recall/unsend.
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
  is no fire-and-forget path. Delivery means that the message is durably
  available in the mailbox; it does not mean that a model was invoked or that an
  idle runtime was woken.
- **Read/ack.** Read and acknowledgement are explicit, per-recipient state
  transitions. Listing a message does not read it; reading it does not ack it
  (see [Delivery, ordering, read/ack](#delivery-ordering-readack)).
- **Processing.** An unacknowledged direct delivery may be independently
  `available`, `claimed`, or `completed`. Claim ownership is a bounded lease
  protected by a monotonically increasing generation. Atomic completion creates
  exactly one server-routed result reply and links it before the runner acks.
- **Target group fan-out is snapshot-at-send.** A group message is delivered to the
  members of the group **at send time**. Agents added to the group later do not
  retroactively receive earlier messages; agents removed before delivery do not
  receive them. The deciding membership is captured for audit.
- **Target list/realm fan-out is snapshot-at-send.** The target list and realm
  audiences resolve atomically inside the token-derived realm. The sender is
  excluded from a realm audience by default because the outbox already records
  the send. New agents never retroactively receive an older message.

## Runtime Receive And Autonomy Boundary

Mailbox delivery and model execution are deliberately separate:

- The current installed guidance asks an active agent to list unread metadata at
  the beginning of non-trivial work. This is model guidance, not a deterministic
  mailbox poll.
- Supported session/task hooks may inject bounded unread metadata. They do not
  inject the body, mark it read, acknowledge it, or invoke a model.
- The implemented `message listen` operation is a stateless, waitable,
  metadata-only mailbox query. It returns the oldest unacknowledged inbound
  metadata so a crash after read but before ack remains recoverable. Timeout or
  disconnect changes no state and loses no delivery.
- The implemented autonomous client-owned runner performs the bounded loop:
  `listen`, deduplicate, claim direct-delivery work, read untrusted content,
  invoke the configured text-only provider, atomically persist/link the
  reply/result and complete processing (or release), then ack.
- Bounded payload history helps reconstruct provider context but is advisory.
  The runner enforces turns from backend-derived message `causal_depth` and
  cross-machine deterministic failure attempts from backend-owned
  `failure_count`; processing generation is only the stale-writer fence.
- The current provider-invoking kinds are `request`, `question`, and `reply`.
  Other kinds are written to the private content-free runner notification
  ledger before acknowledgement and never invoke the provider. Full MCP lists
  those pointers and consumes one through canonical read/verification plus an
  exact local clear; every failure retains it. Injecting that result into a
  human-facing sender task remains a client integration responsibility rather
  than a backend push.
- The local notification ledger is newest-first, explicitly clearable, and
  bounded to 1,024 pointers. At capacity the runner does not evict or ack the
  next non-provider delivery; it fails closed until pointers are inspected and
  cleared. A recorded delivery is then acknowledged globally, but its pointer
  remains only in that runtime's `WITSELF_HOME`, is not account-exported, and is
  invisible to another host/runtime for the same agent. Canonical PostgreSQL
  messages remain exportable.
- MCP, HTTP, and the backend never wake an AI. A runner or already-active
  foreground client must own the call that waits for messages.

Read, acknowledgement, direct-delivery processing claim, and completion are
distinct operations. MCP mirrors the CLI and API. Full runner, future
realm-request, and lease semantics are canonical in
[autonomous-realm-messaging.md](autonomous-realm-messaging.md).

## Message shape

The canonical JSON Message shape is shared by CLI, MCP, and API and is pinned in
[json-contracts.md](json-contracts.md). Fields:

```json
{
  "id": "msg_8sk3...",
  "account_id": "acc_1f2a...",
  "realm_id": "realm_2af9...",
  "from": {
    "kind": "agent",
    "agent_id": "agent_archivist",
    "agent_name": "archivist"
  },
  "to": {
    "kind": "agent",
    "agent_id": "agent_coordinator",
    "agent_name": "coordinator"
  },
  "subject": "handoff",
  "kind": "request",
  "body": "Please pick up the ingest run for batch 42.",
  "payload": { "batch": 42, "owner": "archivist" },
  "thread_id": "thr_91bd...",
  "reply_to_message_id": null,
  "causal_depth": 1,
  "created_at": "2026-06-26T17:04:11Z",
  "delivery": {
    "state": "delivered",
    "delivered_at": "2026-06-26T17:04:11Z"
  },
  "read_state": {
    "state": "unread",
    "read_at": null,
    "acked_at": null
  },
  "processing": {
    "state": "available",
    "generation": 0,
    "failure_count": 0
  }
}
```

Field rules:

- `id` — stable, `msg_`-prefixed, realm-unique. The dedup key for at-least-once
  delivery.
- `account_id` / `realm_id` — the enclosing token-derived scope.
- `from` — the **sender agent, always derived from the authenticated token,
  never from input.** A `from` supplied in a request body is rejected, not
  honored (see [Trust boundary](#trust-boundary-and-anti-spoofing)).
- `to` — currently one direct agent reference. The target recipient vocabulary
  adds a bounded explicit-agent-list and the token-derived realm alongside the
  existing group design. Every recipient resolves inside the sender's realm;
  caller-supplied account/realm fields are rejected. Direct selectors beginning
  with lowercase `agent_` are exact ID-only references and never fall back to a
  same-text agent name; this prevents stale generated IDs from being retargeted.
  Other selectors use exact ID-or-name resolution with ID precedence. Exact
  target wire shapes are pinned during the implementation slice in
  [autonomous-realm-messaging.md](autonomous-realm-messaging.md).
- Inbox list/listen sender filters use the same selector namespace: lowercase
  `agent_` matches only the exact sender ID, while ordinary values use exact
  ID-or-name matching with ID precedence.
- `subject` — short human-readable classification (free text, length-capped).
- `kind` — convention-driven message type such as `note`, `request`, `reply`,
  `event`, or `handoff`. Like memory `kind`, it is a label for filtering, not a
  closed enum; unknown kinds are allowed. Omitted kind normalizes to actionable
  `request` on CLI, MCP, and API/store writes. Explicit `note` is FYI-only to
  the runner and goes through its notification/ack path without inference.
- `body` — free-form text, the primary payload. Length-capped (see
  [Limits and rate limits](#limits-and-rate-limits)). **Untrusted on receipt.**
- `payload` — optional small structured object for machine-readable data.
  Length-capped. **Untrusted on receipt.**
- `thread_id` / conversation id — `thr_`-prefixed correlation metadata. Raw
  send accepts an existing thread id, but knowing it neither proves causality
  nor grants membership. Ordering is guaranteed per thread and per recipient
  (see below).
- `reply_to_message_id` — nullable causal parent. The recipient-only reply
  action validates that the caller received the parent, then derives the reply
  recipient from the parent sender and derives both this field and `thread_id`.
  The reply request cannot supply routing or identity fields.
- `causal_depth` — positive backend-derived depth in the reply graph. Direct
  sends start at one and replies advance exactly one from their validated
  parent. Callers cannot set it. It is the runner's cross-machine turn-limit
  input; payload history and payload turn hints are advisory only.
- `created_at` — server-assigned send timestamp.
- `delivery` — per-recipient delivery state and timestamp. In a group fan-out,
  each recipient has its own delivery row; the shape above is the recipient's
  view.
- `read_state` — per-recipient `unread` / `read` / `acked` state with timestamps.
- `processing` — value-free recipient processing state. `available` has no live
  claim; `claimed` carries `claim_id`, positive `generation`, and
  `lease_expires_at`; `completed` carries `completed_at` and the unique
  `result_message_id`. Every state carries non-negative `failure_count`, the
  durable migration-0036 count of deterministic message failures. Idempotency
  keys and hashes are never exposed.

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

Three explicit per-recipient read states, each a distinct transition:

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

### Processing claim and completion

Direct processing is a second, independent recipient lifecycle:

- `message claim` acquires an unexpired 30–900 second lease on an unacknowledged
  delivery. An exact idempotent retry returns the same claim; a different live
  claimant receives a conflict. Taking an available or expired delivery
  increments the generation fence.
- `message renew` and `message release` require the exact `claim_id` and
  generation. Renewal uses database time; release makes the delivery available
  and immediately invalidates the old fence. The HTTP release contract accepts
  optional `deterministic_failure`; only true atomically increments
  `failure_count`. Ordinary CLI/MCP release leaves it false.
- `message complete` requires the exact unexpired fence and a completion
  idempotency key. In one PostgreSQL transaction it derives the recipient,
  thread, and causal parent from the claimed message, inserts one result reply,
  links it to the delivery, and marks processing completed. It deliberately
  does not ack.
- A runner that crashes after completion observes the completed result link,
  skips provider invocation, and acks. A stale runner cannot complete a newer
  generation. External model or tool effects remain at-least-once and need
  their own idempotency.
- Processing generation is solely the stale-writer fence. Release/expiry
  takeover advances it without consuming failure budget; exact live-claim retry
  retains it. The runner counts only deterministic, message-specific failures.
  Provider-wide or unavailable-provider errors, invalid runner configuration,
  cancellation/timeouts, and claim-lease maintenance failures release without
  incrementing `failure_count`. Under the current default, the first four
  deterministic failures are released and counted; the fifth deterministic
  attempt is completed as a durable escalation. Payload, generation, and the
  client-local health field `consecutive_failures` cannot reset or substitute
  for this backend-owned count.

## Surfaces

One model, three thin adapters over the same core, with authorization enforced
below every frontend (identical result across CLI, MCP, and API).

### CLI — the `message` group

- `message send` — send to one same-realm agent. Sender is the authenticated
  agent; there is no `--from`. Flags include `--to <agent>`, `--subject`,
  `--kind`, `--body` (or `--body-file`/stdin), `--payload-file`, and
  `--thread <id>`. A lowercase `agent_` recipient selector is always an exact
  ID, never a fallback name.
- `message reply <msg_id>` — recipient-only reply. Flags provide content and an
  idempotency key; there is deliberately no recipient or thread flag because
  the server derives recipient, thread, and parent from the inbound message.
- `message list` — list the caller's mailbox. Filters: `--unread`, `--thread`,
  `--from`, and `--kind`, with cursor pagination. Does not change read state.
- `message listen` — wait 20 seconds by default (bounded to 0–20) for the oldest
  unacknowledged inbound metadata. Filters are `--from`, `--thread` (or
  `--conversation`), `--kind`, and `--limit`. It does not read or ack.
- `message read <msg_id>` — read one message; returns `body`/`payload`;
  transitions to `read`.
- `message ack <msg_id>` — acknowledge one message; transitions to `acked` and
  returns metadata only, never the body or payload.
- `message claim <msg_id>` / `renew` / `release` — acquire and coordinate one
  direct-delivery processing fence.
- `message complete <msg_id>` — atomically create the derived result reply and
  complete processing; acknowledgement remains separate.
- `message runner enable|disable|status|notifications|run|serve|start` —
  configure, inspect, execute, and supervise one client-owned runtime-bound
  text-only runner and its content-free notification handoff.

The target surface adds explicit-list/realm send and separate multi-assignee
request coordination. Those are design targets, not commands in the current
binary.

The target `--dry-run send` validates recipient existence, scopes, rate-limit
headroom, and quotas and does **not** persist or deliver anything (consistent
with the dry-run rule in [requirements.md](requirements.md)).

The full CLI surface is enumerated in
[cli-command-surface.md](cli-command-surface.md).

### MCP — `witself.message.*`

- `witself.message.send`, `reply`, `list`, `listen`, `read`, `ack`, `claim`,
  `renew`, `release`, and `complete`, plus local bridge tools
  `notification.list` and `notification.consume`.
- stdio-first; same authorization as the CLI; honors `--read-only` mode. List
  listen, and notification list remain available there because they do not
  change state; send, reply, read, ack, claim, renew, release, complete, and
  notification consume are unavailable. Curator profiles expose neither local
  bridge tool.
- The agent-token MCP session can only send **as** the token-bound agent.
- Tool inputs/outputs and exposure rules are pinned in
  [mcp-tools.md](mcp-tools.md).

At the start of non-trivial work, the MCP instructions have an active agent
call non-blocking `message.listen(wait_seconds=0)` and
`message.notification.list`. Listen checks canonical unacknowledged mailbox
metadata. Notification list checks content-free pointers created before the
local runner globally acknowledged terminal/non-actionable deliveries. Consume
reads and verifies the canonical message, rechecks the runtime/identity binding,
and then clears only the exact local pointer; any failure retains it. Because
the ledger lives only in that runtime's `WITSELF_HOME`, another machine/runtime
for the same agent cannot see those pointers or rediscover the acknowledged
deliveries as unread. This is an intentional MVP locality limit, not cross-host
wake delivery. The local ledger is not account-exported; the canonical messages
are.

Future MCP work adds multi-assignee request coordination. A read is never proof
that autonomous work completed; processing completion and acknowledgement are
their own explicit tools.

### API — `/v1/messages` (+ colon actions)

- `POST /v1/messages` — send. `from` is taken from the token; a `from` in the
  body is rejected. Supports `Idempotency-Key`; dry-run remains a target.
- `GET /v1/messages` — list the caller's mailbox (cursor-paginated, filterable).
- `POST /v1/messages:listen` — stateless long-poll for oldest unacknowledged
  inbound metadata. Body fields are `wait_seconds` (0–20, default 20),
  `from_agent`, `thread_id`, `kind`, and `limit`. Returns `messages` and
  `timed_out`; it has no cursor and changes no state. Each server process bounds
  concurrent admission; saturation returns HTTP 429 with `Retry-After`. The
  sender filter, like list, treats lowercase `agent_` as exact ID-only and never
  falls back to a same-text name; ordinary values use exact ID-or-name matching
  with ID precedence.
- `POST /v1/messages/{message_id}:reply` — recipient-only reply. The request
  carries content; the server validates the inbound parent and derives
  recipient, thread, and `reply_to_message_id`.
- `POST /v1/messages/{message_id}:read` — mark read (recipient only).
- `POST /v1/messages/{message_id}:ack` — acknowledge (recipient only); its
  response is metadata-only and never contains the body or payload.
- `POST /v1/messages/{message_id}:claim` — acquire or idempotently replay a
  bounded direct-processing lease.
- `POST /v1/messages/{message_id}:renew` — renew one exact live claim fence.
- `POST /v1/messages/{message_id}:release` — release one exact claim fence.
- `POST /v1/messages/{message_id}:complete` — atomically persist one derived
  result reply, link it, and mark processing completed; does not ack.

All action subroutes use `POST`, never `GET`. Responses use the shared envelope
`{schema_version, ok, data, warnings}` and the error-code↔HTTP↔exit-code table.
Route style is pinned in [api-routes.md](api-routes.md) and the contract in
[api-contract.md](api-contract.md).

## Authorization

- `message:send` — required to send. Constrainable by realm and by recipient
  agent/group where practical.
- `message:read` — required to list, listen, read, ack, and coordinate
  claim/renew/release/complete processing on one's own mailbox.
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
- Maximum `subject` length: 256 UTF-8 bytes.
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

Every send, deliver, read, ack, and processing transition is audited with the
stable dotted event names defined in [requirements.md](requirements.md):

- `message.sent` — on send. Records the token-derived `from`, the recipient
  (agent or group), `msg_` id, `kind`, `thread_id`, and — for group sends — the
  membership snapshot used for fan-out.
- `message.delivered` — per recipient on delivery.
- `message.read` — per recipient on read.
- `message.acked` — per recipient on ack.
- `message.processing.claimed`, `.renewed`, `.released`, and `.completed` —
  value-free claim lifecycle events. Completion may record the result message
  id but never its body or payload.

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
- [autonomous-realm-messaging.md](autonomous-realm-messaging.md) — client-owned
  inference, deterministic receive, direct delegation, realm audiences, open
  requests, offers, claims, leases, and autonomous loop bounds.
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
