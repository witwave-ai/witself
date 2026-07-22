# Witself Inter-Agent Messaging

Status: the agreed same-realm direct, fan-out, open-request, and foreground
processing feature is operationally complete in release `v0.0.172`. Sanitized
release, rollout, provider-refresh, and live-smoke evidence is retained in the
[autonomous messaging completion boundary](autonomous-realm-messaging.md#completion-boundary).
Last reviewed 2026-07-16. This document is the authority for the durable
messaging model, message shape, delivery and ordering semantics, the
anti-spoofing trust boundary, target rate limits/scopes, audit, and target
metering. It binds the message shapes pinned in
[json-contracts.md](json-contracts.md) and conforms to the master spec in
[requirements.md](requirements.md).

Inter-agent messaging is **fully in scope for v0**, not a stub. It ships a
durable mailbox/queue with at-least-once delivery, per-recipient ordering, and
explicit read/acknowledgement state.

Foreground-client handling, explicit-recipient-set and realm audiences, and
open request/claim coordination are implemented as specified in
[autonomous-realm-messaging.md](autonomous-realm-messaging.md). That document
extends this mailbox; it does not replace the message, delivery, ordering, or
trust contracts here. The only current open-request selection policy is
`client_ranked`, with ranking performed by an authorized client rather than the
backend.

## Implementation Status

The current checkout ships direct and fan-out agent messaging, open-job
coordination, and fenced foreground processing inside one realm:

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
- Active clients discover pending metadata through the bounded self
  `message_checkpoint` plus non-blocking `message.listen`, then explicitly
  claim, read, process, complete/release, and acknowledge. There is no
  background message service or host-local handoff store.
- Content-free send/deliver/read/ack audit events and complete account
  archive/restore coverage, including reply-parent causality, causal depth, and
  processing state. Import interrupts active claims while preserving completed
  result links.
- Migration `0037` adds bounded explicit-list and whole-realm audiences. One
  immutable header owns an all-or-none send-time delivery snapshot; the sender
  is excluded from realm fan-out.
- Migration `0038` adds message-backed open requests with immutable candidates,
  one offer or decline per candidate, client-ranked selections, bounded
  reservations, exact claim fences, and atomic result completion.
- PostgreSQL account archives include audience snapshots and the complete open
  request graph. Active source-cell reservations/claims are interrupted during
  import so their old fences cannot complete in the destination cell.

Named-group fan-out, cross-realm delivery, dry-run, operator metadata
inspection, policy scopes, plan-backed metering, and send/delivery rate limits
are follow-on platform features rather than blockers for the agreed realm-local
core. The tagged and deployed activation record for that core is separate from
the implementation inventory below and makes no claim about dormant cells or
narrative-memory production certification.

## Goal

Let an agent send a durable message to another agent, a bounded explicit set of
agents, or the current realm, and let each recipient list, read, and acknowledge
that message — through the same
one-core, multi-adapter spine that backs every other Witself surface. Messaging
is the channel through which agents coordinate; it is also a new attack surface,
because a message can carry instructions and data into a receiving agent.

Named security-group delivery is a documented follow-on, not part of the
code-complete audience set above.

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
  delivery and ack state. The current implementation supports direct,
  explicit-list, and realm audiences; named security-group fan-out remains
  deferred. The realm is the built-in team/room for the autonomous messaging
  use case; no separate named messaging-team resource is required.
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
  exactly one server-routed result reply and links it before the client acks.
- **Target group fan-out is snapshot-at-send.** A group message is delivered to the
  members of the group **at send time**. Agents added to the group later do not
  retroactively receive earlier messages; agents removed before delivery do not
  receive them. The deciding membership is captured for audit.
- **List/realm fan-out is snapshot-at-send.** Explicit-list and realm
  audiences resolve atomically inside the token-derived realm. The sender is
  excluded from a realm audience by default because the outbox already records
  the send. New agents never retroactively receive an older message.

## Runtime Receive And Autonomy Boundary

Mailbox delivery and model execution are deliberately separate:

- The current installed guidance asks an active agent to inspect its value-free
  message checkpoint and call non-blocking listen for unread metadata at the
  beginning of non-trivial work.
- Supported Codex/Claude session/task hooks may inject the bounded checkpoint
  automatically. Cursor, Grok Build, OpenClaw, Antigravity, and Copilot use
  guided `self.show`. Every runtime's
  installed policy directs it to use listen for unread message metadata, but
  that model action is not forced; no hook injects the metadata. No path injects
  the body, marks it read, acknowledges it, or invokes a model.
- The implemented `message listen` operation is a stateless, waitable,
  metadata-only mailbox query. It returns the oldest unacknowledged inbound
  metadata so a crash after read but before ack remains recoverable. Timeout or
  disconnect changes no state and loses no delivery.
- An active client first scans its open-request roles. A candidate offers or
  declines, the exact coordinator ranks durable offers, and a selected client
  claims/renews/completes a request fence. It then handles selected canonical
  mailbox work in the current foreground turn.
- Backend-derived message `causal_depth` and `failure_count` remain durable
  safety inputs; processing generation is only the stale-writer fence.
- Every message kind remains canonical and unacknowledged until the recipient
  handles it. Explicit `note` is FYI-only and need not invoke work inference.
- MCP, HTTP, hooks, and the backend never wake an AI. An already-active
  foreground client owns every mailbox call and inference turn.
- For an open request, candidate clients use their current runtime
  instructions to send one bounded ordinary `kind=offer` message or decline.
  The backend currently validates and persists those messages but performs no
  semantic ranking and generates no candidate response. A future plan-backed
  layer may rate-limit them at the ordinary send boundary.
- `client_ranked` is the only current open-request policy. If the ranking client
  disappears, the request remains durably `awaiting_selection`; there is no
  automatic first-claimant fallback. Selection resumes only when that immutable
  coordinator agent's client returns, or ends when the coordinator cancels the
  request or it expires.
- Stored agent responsibilities, job-function descriptions, standing
  directives, and richer profile metadata are explicitly deferred. They are not
  a schema or runtime dependency for requests, offers, selection, or
  claims.

Read, acknowledgement, direct-delivery processing claim, and completion are
distinct operations. MCP mirrors the CLI and API. Full foreground,
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
- `to` — one direct agent, a bounded explicit-agent-list, or the token-derived
  realm. Every recipient resolves inside the sender's realm;
  caller-supplied account/realm fields are rejected. Direct selectors beginning
  with lowercase `agent_` are exact ID-only references and never fall back to a
  same-text agent name; this prevents stale generated IDs from being retargeted.
  Other selectors use exact ID-or-name resolution with ID precedence. Fan-out
  output uses `to.kind=agents|realm` plus the immutable delivery `count`; direct
  output retains the resolved agent ID and name.
- Inbox list/listen sender filters use the same selector namespace: lowercase
  `agent_` matches only the exact sender ID, while ordinary values use exact
  ID-or-name matching with ID precedence.
- `subject` — short human-readable classification (free text, length-capped).
- `kind` — convention-driven message type such as `note`, `request`, `reply`,
  `event`, or `handoff`. Like memory `kind`, it is a label for filtering, not a
  closed enum; unknown kinds are allowed. Omitted kind normalizes to actionable
  `request` on CLI, MCP, and API/store writes. Explicit `note` is FYI-only; an
  active recipient may read and acknowledge it without treating it as work.
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
  parent. Callers cannot set it. It is durable causal metadata and a possible
  client-policy safety input; no fixed turn threshold is currently codified.
  Payload history and payload turn hints are advisory only.
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
- Read and ack are recipient-only actions. The sender cannot read or ack on the
  recipient's behalf; granular `message:read` scope gating remains follow-on
  platform integration.

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
  `failure_count`. CLI and MCP expose the same optional input and leave it false
  by default. It is only for repeatable failures attributable to that message,
  never provider-wide or configuration failures, cancellation, timeout, or
  claim-lease maintenance failures.
- `message complete` requires the exact unexpired fence and a completion
  idempotency key. In one PostgreSQL transaction it derives the recipient,
  thread, and causal parent from the claimed message, inserts one result reply,
  links it to the delivery, and marks processing completed. It deliberately
  does not ack.
- A client interrupted after completion observes the completed result link,
  skips duplicate work, and acks. A stale client cannot complete a newer
  generation. External model or tool effects remain at-least-once and need
  their own idempotency.
- Processing generation is solely the stale-writer fence. Release/expiry
  takeover advances it without incrementing `failure_count`; exact live-claim
  retry retains it. The backend counts only deterministic, message-specific
  failures that a client marks on an exact-fence release.
  Provider-wide errors, cancellation/timeouts, and claim-lease maintenance
  failures release without incrementing `failure_count`. Installed policy
  directs clients to release and count the first four deterministic failures and
  complete a durable escalation when `failure_count` is already 4 or greater;
  neither the backend nor model compliance enforces that threshold. Payload and
  generation cannot reset or substitute for the backend-owned count.

## Surfaces

One model, three thin adapters over the same core, with authorization enforced
below every frontend (identical result across CLI, MCP, and API).

### CLI — the `message` group

- `message send` — send to one same-realm agent, a bounded explicit list, or the
  whole realm. Sender is the authenticated agent; there is no `--from`. Flags
  select exactly one audience and include `--subject`,
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

`message request open|list|show|offer|decline|select|cancel|claim|renew|release|complete`
exposes the separate multi-assignee request lifecycle. Active candidate and
coordinator clients perform inference; the CLI is also available for inspection,
recovery, and explicit operation.

The target `--dry-run send` validates recipient existence, scopes, rate-limit
headroom, and quotas and does **not** persist or deliver anything (consistent
with the dry-run rule in [requirements.md](requirements.md)).

The full CLI surface is enumerated in
[cli-command-surface.md](cli-command-surface.md).

### MCP — `witself.message.*`

- `witself.message.send`, `reply`, `list`, `listen`, `read`, `ack`, `claim`,
  `renew`, `release`, and `complete`.
- stdio-first; same authorization as the CLI; honors `--read-only` mode. List
  and listen remain available there because they do not change state; send,
  reply, read, ack, claim, renew, release, and complete are unavailable.
- The agent-token MCP session can only send **as** the token-bound agent.
- Tool inputs/outputs and exposure rules are pinned in
  [mcp-tools.md](mcp-tools.md).

At the start of non-trivial work, the MCP instructions have an active agent
inspect `self.show.message_checkpoint` and call non-blocking
`message.listen(wait_seconds=0)`. Listen checks canonical unacknowledged mailbox
metadata. Message content remains behind explicit read, and the canonical
delivery remains recoverable until explicit acknowledgement. No host-local
notification bridge is involved.

MCP exposes matching `witself.message.request.open|list|show|offer|decline|select|cancel|claim|renew|release|complete`
tools. `client_ranked` is the only current selection policy. Offers remain
bounded ordinary messages; candidate clients use current runtime instructions
to offer or decline, and the backend never ranks them. A vanished ranking
client leaves the request `awaiting_selection` until the coordinator resumes.
This surface does not depend on a responsibility/directive schema. A read is
never proof that autonomous work completed; processing completion and
acknowledgement are explicit operations.

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
- `POST /v1/message-requests` plus `GET /v1/message-requests` and
  `GET /v1/message-requests/{request_id}` — open, list, and inspect realm-local
  requests.
- `POST /v1/message-requests/{request_id}:offer|decline|select|cancel|claim|renew|release|complete`
  — advance the client-ranked request state machine under token-derived actor
  identity and exact claim fences.

All action subroutes use `POST`, never `GET`. Responses use the shared envelope
`{schema_version, ok, data, warnings}` and the error-code↔HTTP↔exit-code table.
Route style is pinned in [api-routes.md](api-routes.md) and the contract in
[api-contract.md](api-contract.md).

## Target authorization integration

- `message:send` — will be required to send and constrainable by realm/recipient
  agent/group where practical.
- `message:read` — will be required to list, listen, read, ack, and coordinate
  claim/renew/release/complete processing on one's own mailbox.
- Target group sending will require `message:send`; it will not require group
  membership unless an operator constrains the scope. Group-recipient resolution
  is tracked in [security-groups.md](security-groups.md).
- Messaging carries no implicit data-access authority. A message can reference
  identity data via `witself://` URIs, but resolving those references still runs
  the full policy check (see [access-policy.md](access-policy.md)); an
  unauthorized reference does not resolve just because it arrived in a message.
- Operator inspection of realm-wide message metadata under an audited realm
  admin override remains follow-on work.

The complete scope list lives in [requirements.md](requirements.md).

## Implemented limits and target rate limits

The current core enforces:

- Maximum `body` size: 64 KiB.
- Maximum `payload` size: 16 KiB serialized.
- Maximum `subject` length: 256 UTF-8 bytes.
- Maximum resolved direct, explicit-list, or realm audience: 64 recipients;
  oversized sends fail atomically rather than truncating.

The following plan-backed rate controls remain target platform integration and
must not be inferred from the size/fan-out limits above:

- Per-agent send rate limit (messages per interval).
- Per-realm aggregate send rate limit.
- Per-recipient delivery/inbound rate limit, to protect a single recipient from
  flooding.

When implemented, overage follows the plan's configured behavior — `warn`,
`throttle`, or `block` — per the model in
[billing-and-limits.md](billing-and-limits.md). A blocked or throttled send must
return a deterministic error, never a silent drop.

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
- `message.request.opened`, `.offered`, `.declined`, `.selected`, `.claimed`,
  `.renewed`, `.released`, `.completed`, and `.cancelled` — value-free
  open-request coordination events. They may record request/opening/selection/
  claim/result IDs, agent IDs, generations, counts, and lifecycle outcomes, but
  never the request, offer, or result body/payload.

Audit records carry non-sensitive context only — `msg_` id, sender/recipient
ids, `kind`, `subject` presence, `thread_id`, decision outcome. They **never**
contain `body`, `payload`, raw tokens, or any identity-content values. The same
redaction rule applies to logs, metrics, and error messages. Retention follows
[audit-retention.md](audit-retention.md).

## Target metered dimensions

Messaging is intended to contribute two metered dimensions (see
[billing-and-limits.md](billing-and-limits.md)):

- **Messages sent** — counted at send (one per send request).
- **Messages delivered** — counted per recipient delivery (an N-recipient fanout
  counts as N deliveries).

These dimensions are the intended billing/operations contract but are not yet
wired into plan-backed messaging metering. When implemented, metrics expose
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
