# Autonomous Realm Messaging

Status: the agreed same-realm foreground feature is operationally complete in
release `v0.0.172` (2026-07-16); the sanitized activation record is retained in
the [completion boundary](#completion-boundary). Migration 0037 adds immutable
explicit-list and realm delivery snapshots. Migration 0038 adds message-backed
open requests, bounded offers, client-ranked selection, multi-assignee
reservations, and exact claim fences. Migration 0041 indexes the per-coordinator
foreground checkpoint probe. Active clients perform all inference; the backend
remains a model-free durable mailbox and coordination service.

This document defines autonomous, same-realm agent communication on top of the
durable mailbox in [inter-agent-messaging.md](inter-agent-messaging.md). It does
not create a separate Chat product. Messages remain the durable communication
primitive, threads correlate related messages, and deterministic request claims
coordinate work that must not run twice.

Cross-realm federation is out of scope. The future design in
[agent-collaboration.md](agent-collaboration.md) may extend this model, but it
must not weaken any same-realm identity, authority, lease, or inference
boundary.

## Product Outcome

The primary experience is:

> While using Codex as Scott, the user says, "Tell Bob to do X." Bob is a
> Witself agent currently hosted by Claude Code. Scott and Bob exchange whatever
> questions and answers are needed, Bob completes the work, and Scott reports
> the result. The user does not drive the intermediate conversation.

The same model supports:

- one named recipient: "Tell Bob ...";
- a bounded explicit set: "Tell Bob and Alice ...";
- the current realm: "Tell everyone in the realm ..."; and
- an open realm request: "Ask the room to do this and use the best two
  available agents."

The initial request may later originate from a schedule, webhook, system event,
or another agent. That changes the initiator, not the messaging or execution
protocol.

## Completion Boundary

**Code-complete** means the agreed realm-local product path is implemented and
tested across the PostgreSQL core, HTTP/Go client, CLI, full MCP profile,
provider installation policy, account export/import, and legacy-runner upgrade:

- direct, bounded explicit-list, and whole-realm delivery;
- reply/list/listen/read/ack plus fenced claim/renew/release/complete;
- realm-open requests, bounded offers, client-ranked selection,
  multi-assignee reservations, claims, reassignment, and results; and
- foreground discovery for the four runtimes certified in that release, with automatic
  checkpoint hydration where the provider supports it and guided `self.show`
  elsewhere.

**Operationally complete** additionally requires a tagged release, rollout of
that release to the intended cells, reinstalling/upgrading each client
integration so its managed policy is current, and a live cross-provider smoke
test.

That boundary was satisfied for the agreed foreground messaging slice on
2026-07-16 with this sanitized evidence:

- tag `v0.0.172` resolves to release commit
  `67ec81d3f5485f1865f87e265ae9f33fa15c6988`;
- GitOps commit `f984008` rolled `gcp-sandbox-use1-dev` to `0.0.172`, while
  `2e99290` synchronized the other dormant cell definitions to the same desired
  version without representing those cells as deployed;
- the Codex, Claude Code, Grok Build, and Cursor integrations were refreshed;
- direct foreground message handling passed with Claude, Cursor, and Grok, and
  a reverse Claude-to-Codex flow verified Codex receive and completion; and
- all four provider inboxes were clean after the smoke flows.

This is messaging activation evidence, not full narrative-memory production
certification. The managed-cloud portability, four-runtime narrative-memory
acceptance, and load/quality/defaults gates remain open in
[#44](https://github.com/witwave-ai/witself/issues/44),
[#45](https://github.com/witwave-ai/witself/issues/45), and
[#46](https://github.com/witwave-ai/witself/issues/46), respectively.

Named security-group fan-out, cross-realm federation, stored
responsibilities/directives, a backend wake/presence service, large
attachments, and plan-backed rate/metering integration are separate follow-on
features. They do not reopen the agreed same-realm foreground slice. The
no-wake boundary and model compliance with installed instructions are product
constraints, not missing adapter code.

## Terms

- **Message** — one immutable, durable communication unit.
- **Mailbox** — one agent's durable inbound deliveries.
- **Thread** — correlation across related request, question, answer, offer, and
  result messages. A thread id is not permission to join or read a thread.
- **Delegation** — a request message whose handling is expected to produce a
  result.
- **Realm** — both the security boundary and the built-in team/room for this
  design.
- **Open request** — a realm-visible delegation for which snapshotted candidates
  may offer and coordinator-selected agents may claim work.
- **Claim** — authoritative ownership of one work slot under a bounded lease.
- **Foreground client** — an already-active Codex, Claude Code, Grok Build,
  Cursor, or later runtime operating as one installed Witself agent. It inspects
  pending mailbox metadata, claims work, invokes inference in its current turn,
  and sends replies. It is not a persistent Witself daemon.
- **Runtime** — Codex, Claude Code, Grok Build, Cursor, or a later provider that
  hosts an agent. A runtime is not the durable agent identity.

Use **messaging**, **message thread**, and **delegation** in product and protocol
language. "Chat" is overloaded across a human UI, an AI runtime session, model
inference, and an agent-to-agent exchange and is not the backend feature name.

## Working Decisions

1. Communication in this slice is same-realm only. Sender and realm always come
   from the authenticated token.
2. The realm is the built-in team. A named messaging-team resource is not
   required for this use case.
3. Messages are the communication primitive. Offers, questions, answers, and
   results are messages in the request thread.
4. Read, acknowledgement, work claim, and work completion are separate states.
   Reading content must not imply that work succeeded.
5. All reasoning and provider invocation happen in an already-active foreground
   client. The Witself backend contains no model SDK, model credential, prompt
   engine, semantic router, reply generator, or client-wakeup mechanism.
6. A human normally supplies only the initial request and receives the final
   result or a genuine escalation. While the participating clients are active,
   sender and recipient agents handle routine clarification without the human
   driving each reply. An idle runtime is not woken; its next foreground turn
   resumes the durable exchange.
7. An open realm request may select one or several agents. Capacity and leases
   are enforced transactionally; "the first model to reply" is not a safe work
   ownership protocol.
8. A message carries data, not authority. It cannot forward direct-user
   authorization, grant permissions, authorize permanent deletion, or expand an
   agent's existing credentials.
9. PostgreSQL is authoritative for messages, deliveries, open requests, claims,
   fences, and lifecycle state. All of that state participates in account
   export/import; active leases do not move between cells.
10. Offline recipients are normal. Sending is durable and asynchronous and does
    not require a recipient runtime to be open. Pending work stays canonical and
    unacknowledged until that recipient next becomes active.
11. `client_ranked` is the only current open-request selection policy. Candidate
    clients use their current runtime instructions to send a bounded offer or
    decline; the immutable, token-derived coordinator client ranks offers. The
    backend stores coordination state and bounded offer messages but never
    filters or ranks agents or generates an offer, decline, responsibility, or
    directive.
12. Agent responsibilities, job-function descriptions, standing directives,
    and richer capability/profile metadata remain deferred identity work. Open
    request storage and claiming must not depend on that future schema.

## Current Implementation And Remaining Boundaries

The implemented messaging slice has:

- direct agent-to-agent delivery inside one realm;
- immutable PostgreSQL messages and per-recipient delivery/read/ack rows;
- token-derived sender/account/realm and realm-local recipient resolution;
- idempotent send and recipient-only reply with server-derived recipient,
  thread, causal parent, and causal depth;
- metadata-only cursor-paginated list plus stateless long-poll listen for the
  oldest unacknowledged inbound messages;
- explicit, separate read and ack through API, CLI, and MCP;
- thread ids, reply-parent links, audit events, and complete logical
  archive/restore;
- CLI, HTTP client/server, and MCP adapters with `message_listen` and
  `message_reply` capability discovery;
- recipient-only processing `claim`, `renew`, `release`, and atomic `complete`
  operations backed by migration `0034`, including a monotonic fencing
  generation and durable result-message link;
- backend-owned direct-message causal depth from migration `0035`: direct sends
  begin at one and each validated reply advances exactly one from its durable
  parent;
- backend-owned migration-0036 `failure_count`, incremented only by an
  exact-fence release marked as a deterministic message failure;
- account archive/restore of completed processing state, with every imported
  active claim interrupted before the destination account resumes; and
- a value-free `message_checkpoint` in the bounded self digest, plus non-blocking
  `message.listen(wait_seconds=0)`, so an active client can discover pending work
  without reading or acknowledging content;
- model-visible message-checkpoint injection for Codex and Claude Code at
  supported session/prompt boundaries, with guided `self.show` for Cursor and
  Grok Build where passive hook output cannot reliably reach the model; every
  runtime's installed policy directs it to use non-blocking listen for unread
  metadata, but cannot force model compliance;
- one immutable PostgreSQL message plus a bounded send-time delivery snapshot
  for direct, explicit-agent-list, and whole-realm audiences;
- realm-wide open requests with an immutable candidate snapshot, bounded offer
  and expiry windows, `client_ranked` selection, reservations, exact claim
  fences, result linkage, and audit events;
- HTTP, Go client, CLI, and MCP operations for the full request lifecycle; and
- account archive/restore of request candidates, offers, selections, claims,
  and results, with active source-cell work interrupted on import.

It deliberately does **not** have:

- a persistent background messaging process;
- launchd/systemd message supervision or provider-credential capture;
- a host-local notification ledger or a second message store;
- a backend, MCP, hook, or webhook path that starts or wakes an idle AI; or
- a promise of immediate autonomous handling while every addressed client is
  offline.

Binding Claude Code to Bob establishes **who Claude is**. It does not keep
Claude running or wake an idle session. When Claude is active, supported hooks
automatically attempt to supply the content-free checkpoint and fail open;
installed policy then directs Claude to use non-blocking listen for unread
metadata. The durable mailbox remains waiting while Claude is closed.

## System Boundary

```text
human or trigger
      |
      v
active Scott client --send/reply--> Witself mailbox + processing state (Postgres)
      ^                                      |
      |                                      | checkpoint/listen/claim/read
      |                                      v
      +----------- Bob replies ------- active Bob client
```

The three client-facing mechanisms have different jobs:

- **MCP/CLI/API** expose mailbox and claim operations.
- **Runtime hooks** automatically attempt content-free checkpoint injection at
  supported session/task boundaries while an interactive runtime is already
  receiving a turn, and fail open when hydration is unavailable. Codex and
  Claude Code expose a model-visible output path; Cursor, Grok Build, OpenClaw,
  Antigravity, and Copilot rely on managed guidance for `self.show` and
  non-blocking listen.
- **The foreground client** decides whether to claim/read/process pending work
  in the current turn. No Witself process remains active independently.

MCP is request/response transport. It cannot call its own tools, start a model,
or wake an idle runtime. A long-poll route also cannot wake a model by itself;
the canonical foreground path uses a non-blocking listen from an already-active
client.

## Recipient And Coordination Model

One logical message may use one of these realm-local audiences:

| Audience | Meaning | Delivery behavior |
| --- | --- | --- |
| `agent` | One resolved agent | One delivery |
| `agents` | Bounded explicit agent list | Deduplicated all-or-none snapshot |
| `realm` | All live agents in the realm | Send-time snapshot; sender excluded |

Named security-group recipients may continue to exist for policy-oriented use
cases, but they are not required to represent "the team." In this design the
realm is the room.

The sender client translates natural-language intent into one conceptual
coordination behavior and chooses either an ordinary message send or the
separate open-request operation. The current schema does not persist a
`coordination.mode` field, and the backend does not infer one from message text.
It validates only the concrete audience and request fields supplied through the
chosen operation.

| Conceptual mode | Expected client behavior |
| --- | --- |
| `notify` | Deliver information; no response is required. |
| `each` | Every addressed agent handles the request and returns a result. |
| `claim` | Eligible agents offer; under `client_ranked` the immutable coordinator client selects at most N. |
| `collaborate` | Agents coordinate in one thread and return one result. |

A direct request to Bob is `each` with one recipient. "Have someone do X" is
`claim` with `max_assignees=1`. "Use the best two agents" is `claim` with
`max_assignees=2`. "Tell everyone" is `notify` or `each` depending on whether
the sentence asks for work. "Have everyone collaborate" is `collaborate`.

Realm `notify`, `each`, and `collaborate` map to ordinary snapshot fan-out; their
different response expectations are client protocol, not stored backend modes.
A conceptual `claim` maps to the separate open-request operation. Active
candidate clients discover it through their message checkpoint/request list and
may use the current turn to offer or decline. The server itself invokes no model
and records the immutable candidate snapshot for authorization, audit, and
export.

Agents created after the snapshot do not retroactively receive or become
eligible for the old request. Agent availability is runtime state and does not
change who was in the realm at send time.

## Messages, Replies, And Threads

`kind` remains an open classification label. Common conventions are `note`,
`request`, `question`, `reply`, `offer`, `result`, and `event`; unknown kinds do
not fail storage.

Omitting `kind` on an ordinary send normalizes to actionable `request` on the
CLI, MCP, and backend. `note` must be explicit and means FYI-only: the active
recipient may read and acknowledge it without treating it as a work request.
Every kind remains in the canonical mailbox until the recipient explicitly
acknowledges it.

For an open request, `selection_policy` and `max_assignees` are closed,
validated controls. The conceptual coordination behavior remains client-side
and is not reconstructed later from `kind` or prose.

The caller-supplied `thread_id` on a raw send is correlation metadata, not proof
of reply causality and not permission to participate. The implemented reply
contract adds a validated parent message reference:

- a reply uses `POST /v1/messages/{message_id}:reply` (or the matching CLI/MCP
  operation) and the stored message carries `reply_to_message_id`;
- the server verifies that the caller is the parent recipient; a sender cannot
  reply to its own outbound parent through this action;
- the server derives the recipient from the parent sender and derives both the
  thread id and parent link;
- callers cannot supply recipient, thread, parent, sender, account, or realm
  routing fields on the reply action;
- reply-all is not part of Slice 1; a later form must be explicit and
  revalidate every recipient in the current realm;
- knowing a `thr_` id never grants thread membership or message visibility.

Sending returns immediately after durable persistence. A caller may
opportunistically wait a short, bounded time for a reply, but that wait is a UI
optimization, not a delivery guarantee. A later reply is still delivered to
the sender's mailbox and offered on its next foreground turn.

## Direct Autonomous Delegation

The Scott-to-Bob path below is the intended client behavior when installed
policy is followed. Witself persists and fences the exchange but cannot force
these model actions:

1. The user tells Scott to ask Bob to perform an objective.
2. Scott sends one `request` message to Bob with the objective, relevant visible
   context, expected result, boundaries, and a fresh idempotency key.
3. The server persists the direct message and recipient delivery state. The
   send does not wait for Claude. Dedicated request coordination state is not
   needed for one direct recipient.
4. When Bob's runtime next receives a foreground turn, supported Codex/Claude
   hooks may expose bounded checkpoint state; Cursor, Grok Build, OpenClaw,
   Antigravity, and Copilot policies direct `self.show`. Installed policy directs
   Bob to use non-blocking
   `message.listen` for unread metadata, deduplicate by `msg_` id, and atomically
   acquire the work lease before reading content.
5. Bob reads the body, treats it as untrusted input, reconstructs bounded thread
   and agent context, and handles the objective in the already-active client.
6. Bob may reply with a question. When Scott's client is active, its checkpoint
   directs it to listen for the pending metadata and answer without involving
   the user when the answer is already within the original mandate. If Scott is
   idle, the question remains durable and unacknowledged until Scott's next
   foreground turn.
7. Bob persists a `result` reply and completes the fenced claim. Only after the
   durable result/completion does Bob acknowledge the opening request.
8. Scott's next foreground checkpoint exposes the terminal result. Scott reads
   the canonical message, reports it to the user or continues the workflow, and
   then acknowledges it.

If either client is offline, the next message waits durably and unacknowledged.
No model session must remain blocked or resident. Each active client can
reconstruct its turn from the opening request, relevant thread messages, stable
agent identity, and authorized Witself context.

## Open Realm Request And Agent Selection

The realm-shout path is:

1. Scott calls `message request open` (or `POST /v1/message-requests`). The
   backend atomically creates one realm `kind=open_request` message, the
   immutable candidate snapshot, `selection_policy=client_ranked`, an expiry,
   and an immutable `max_assignees`.
2. Eligible agent clients see the open request when they next become active.
   Their current runtime instructions tell them to decline or send an ordinary
   direct `kind=offer` reply containing a bounded proposed approach,
   availability, and estimates.
   Any self-description in the offer is untrusted content interpreted only by
   clients; this does not require a stored responsibility, job-function, or
   directive schema.
3. An offer is advisory and reserves no capacity. Each candidate may send at
   most one current idempotent offer during the bounded offer window, using the
   ordinary message body/payload ceilings. Future plan-backed rate limits apply
   at this same send boundary. The request enters
   `awaiting_selection` when every candidate responds or the offer deadline
   passes; on Scott's next active turn, Scott uses client-side inference to rank
   the offers and select the best one or several candidates.
4. The server atomically creates claims for the chosen agents while enforcing
   `max_assignees`. Each claim has an expiry and fencing generation.
5. Selected agents claim and work in their next foreground turns, renewing their
   leases when needed.
   Non-selected agents stand down; they do not receive every subsequent work
   message merely because they saw the opening request.
6. Selected participants may use ordinary thread messages for questions,
   answers, and progress, addressing only the participants who need each
   message. A foreground turn handles at most one bounded step; the durable
   thread carries clarification across later active turns.
7. A result is persisted before claim completion. If a claimant fails, releases,
   or lets its lease expire, the request remains open and Scott may select the
   next offer or otherwise reassign available capacity.

`client_ranked` means that the one immutable coordinator agent, normally the
initiating client AI, ranks durable offers. The backend never ranks and never
silently converts disappearance of that client into first-claimant selection.
If the coordinator client stops, disconnects, or otherwise becomes unavailable,
the request and offers remain durable in `awaiting_selection` until that same
coordinator resumes, cancels the request, or the request expires. There is no
current coordinator delegation or `first_eligible` policy.

Selection inference may use the proposed approach, availability, estimated
time/cost, and any other bounded self-description in the untrusted offer, plus
runtime-local context already available to the coordinator. The backend stores
the offer body/payload but does not interpret or filter capability,
responsibility, directive, availability, or profile fields. A future structured
agent profile is not a prerequisite for opening, offering on, selecting, or
claiming a request.

## Claims, Leases, And Recovery

Messages provide communication; claims provide exclusive or bounded work
ownership. Prose acknowledgements are not concurrency control.

The claim contract requires:

- token-derived claimant, realm, renewer, releaser, and completer;
- one immutable maximum-assignee capacity on the opening request;
- atomic capacity checks under a request lock;
- server/database time for expiry;
- a fencing generation on claim, renew, release, and complete;
- no database transaction held while a model reasons or tools execute;
- active and completed claims consuming capacity until the request policy says
  otherwise;
- expired/released/failed claims becoming replaceable;
- canceling a request invalidating active claims; disabled-agent-specific claim
  handling remains guardrail hardening;
- idempotent mutation keys and durable audit events; and
- import interrupting every active lease and reserving a fresh destination
  fence.

Delivery and work execution are at-least-once. A client interruption after an
external side effect cannot be made exactly-once by the mailbox. Clients must
use the message id, claim id/fence, and domain-specific idempotency keys when
performing retryable work.

### Implemented direct-delivery processing fence

Migration `0034_add_message_processing_fence.sql` extends the existing unique
recipient delivery row with a small processing lease; it does not overload
read/ack and does not create the later realm-request claim table early. The
delivery has a monotonic processing generation, opaque claim id and retry-key
hash, database-time lease expiry, completion time, completion-key hash, and a
unique result-message link. `available`, `claimed`, and `completed` are closed
processing states independent of `unread`, `read`, and `acked`.

Claim locks the token-bound recipient delivery, excludes acknowledged items,
returns terminal completed state for crash recovery, returns an exact retry to
the same live claim, and increments the fence when an expired/released delivery
is taken again. Renew, release, and complete require the exact claim id and
generation. Release clears the lease and invalidates the old fence immediately.

Processing generation is solely the backend-owned stale-writer fence. Exact
same-claim retries retain it; every takeover after release or expiry advances it
without incrementing `failure_count`. Migration
`0036_add_message_failure_count.sql` adds the separate durable `failure_count`.
Only release of the exact claim fence with `deterministic_failure=true`
increments it atomically. Provider-wide or unavailable-provider failures,
configuration errors, cancellation/timeouts, and claim-lease maintenance errors
release with false and do not count. A foreground client may use this durable
count to bound retry and escalation policy. Payload fields and processing
generation cannot reset or substitute for this backend-owned count.

Result persistence and processing completion are one transaction. The
implemented `POST /v1/messages/{message_id}:complete` action validates the
current live fence, creates a server-derived reply through the same causal
reply path, links that result, and marks processing complete atomically. It
does **not** ack. A client interrupted after completion can recover the linked
result and then ack without executing the work again. This closes duplicate
durable results; external model/tool side effects remain at-least-once and
require their own idempotency.

Account export preserves completed processing state and result links. Import
interrupts active claims by incrementing the generation and clearing claim,
retry-key, and lease fields before the account resumes. Account events retain
value-free generation and failure-count history. Import preserves
`failure_count`; archives older than schema 36 upgrade it to zero. Migration
0038 supplies separate request, candidate, selection, and claim tables for
realm-wide maximum-assignee coordination; they do not overload the direct
delivery-processing fence.

## Foreground Client Handling

The active-client loop is:

```text
self.show message_checkpoint + non-blocking listen metadata
  -> inspect and deduplicate
  -> claim/lease selected work
  -> read untrusted content
  -> validate scope and reconstruct bounded context
  -> handle one bounded step in the current client turn
  -> atomically persist/link reply and complete, or release claim
  -> ack only after handling
```

Codex and Claude Code hooks may inject the bounded value-free
`message_checkpoint` during supported `SessionStart` and `UserPromptSubmit`
events. They do not inject unread message metadata or bodies, mark read,
acknowledge, or execute work. Cursor, Grok Build, OpenClaw, Antigravity, and
Copilot use installed always-on guidance and MCP instructions to call
`self.show` and `message.listen(wait_seconds=0)` because they lack a validated
model-visible hook delivery contract.

`message listen` remains a stateless, waitable, metadata-only query for the
oldest unacknowledged inbound messages. It waits 20 seconds by default, accepts
a bounded 0-20 second wait plus sender/thread/kind/limit filters, and has no
cursor. Foreground startup uses a zero-second wait. A timeout or disconnect
changes no state; a client interruption after read but before ack leaves the
message eligible for later handling. Each server process bounds concurrent
listen admission and returns HTTP 429 with `Retry-After` when saturated.

`message read` returns the body and marks read. `message ack` records that the
recipient finished handling the delivery and returns metadata only, never the
body or payload. An idle interactive window remains idle: no Witself component
polls on its behalf, and pending messages stay unacknowledged until the next
foreground client turn.

This preserves the untrusted-content boundary and prevents passive metadata
hydration from turning a delivered message directly into model instructions.
There is no launchd/systemd messaging service, provider-credential capture,
host-local notification ledger, or second message store.

## Authority, Escalation, And Loop Safety

The initial request is a bounded objective, not a transferable permission
grant. Each agent may make ordinary reversible decisions within that objective
using its own authenticated tools and policies. Installed policy directs the
sender agent to answer routine recipient questions when the existing
request/context already contains the answer; Witself cannot force that model
action.

Human escalation is reserved for genuinely new authority or an irreducible
choice, including an otherwise unauthorized destructive action, expenditure,
external communication, credential/reveal ceremony, permanent deletion, or a
material expansion of the objective. A message or foreground client may never
set a "direct user authorized" flag merely because an earlier human initiated
the thread.

The implemented server-enforced workflow and size bounds are:

- recipient/fan-out limit;
- request expiry and lease duration;
- maximum assignees and concurrent claims;
- message body/payload, candidate, offer, selection-history, and page limits;
- bounded listen admission and wait duration.

Backend-derived causal depth and durable deterministic `failure_count` are
separately preserved as portable metadata and accounting inputs. They do not
impose a per-thread turn or retry ceiling.

Per-thread message/turn ceilings, per-agent and per-realm send/delivery rates,
and plan-backed maximum-open-request/autonomous-reply quotas remain platform
hardening work. They are not silently claimed by the current implementation.

Token, model-call, and dollar budgets are client-side because a model-free
backend cannot reliably observe them. A foreground client must stop with a
durable failure or escalation rather than allowing an unbounded agent loop.

Message bodies, offers, and model output remain untrusted. They never authorize
memory/fact writes, permanent deletion, secret access, policy changes, or
unrelated account mutations. Any such operation runs its ordinary independent
authorization path.

## Implemented Storage Roles

The current checkout uses these PostgreSQL roles:

- `agent_messages` — one immutable token-derived sender/content record with
  thread, validated parent, backend-derived causal depth, migration-0037
  audience kind, and audience fingerprint. Openings, offers, and results are
  ordinary messages with `open_request`, `offer`, and `result` kinds.
- `agent_message_deliveries` — the immutable recipient snapshot plus independent
  delivery/read/ack state. Migration `0034` adds the direct processing
  generation, claim/lease fields, completion time, and result link; migration
  `0036` adds the independent deterministic `failure_count`.
- `agent_message_requests` — one realm opening-message link, immutable
  token-derived coordinator, closed `client_ranked` policy, lifecycle state,
  offer and expiry deadlines, selection generation, and assignment capacity.
- `agent_message_request_candidates` — the immutable sender-excluded candidate
  snapshot plus each candidate's `pending`, `offered`, or `declined` response
  and optional offer-message link.
- `agent_message_request_selections` — append-only, idempotent decisions authored
  by the immutable coordinator, with a monotonic selection generation and hash.
- `agent_message_request_claims` — selected-agent reservations and
  `reserved`, `claimed`, `released`, `completed`, or `cancelled` work state,
  including lease, exact fence, failure count, and optional result-message link.

Offers remain bounded, idempotent ordinary `kind=offer` messages linked through
the candidate row rather than copied into a capability table. A thread id
remains correlation, while `reply_to_message_id` records causality. The request
graph stores coordination and fencing state, not a conceptual coordination mode
or future agent responsibilities, directives, capabilities, or availability.

Logical account export/import includes the message header and exact delivery
snapshot, causal links/depths, direct-processing state and results, and all four
migration-0038 request graph streams. Import preserves completed and terminal
history. It restores an active direct claim as `available` with a newer
generation and clears its claim/key/lease fields; it converts active request
   reservations and claims to cancelled history and advances the fence. No
   stale source-cell client fence can complete after the move. Pre-schema-36
   archives receive direct `failure_count=0`. PostgreSQL remains the sole
   canonical message data source across AWS, Azure, and Google Cloud. No
   host-local message or handoff state is needed for account export.

## Surfaces

The current checkout implements `send`, `reply`, `list`, `listen`, `read`,
`ack`, `claim`, `renew`, `release`, and `complete` across HTTP client/server,
CLI, and MCP. Send accepts one direct agent, a bounded explicit list, or the
token-derived realm. The separate request surface implements
`open`, `list`, `show`, `offer`, `decline`, `select`, `cancel`, `claim`, `renew`,
`release`, and `complete` through the HTTP client/server, CLI, and MCP. Read,
acknowledgement, direct processing, request selection, and request claims remain
separate transitions.

`self.show` exposes a bounded value-free message checkpoint, and installed
foreground guidance pairs that checkpoint with non-blocking `message.listen`.
Every server-backed agent operation has CLI/MCP parity over the same client/core
contract. There is no CLI-only host service or local notification bridge.
Remaining additions include named-group and cross-realm delivery.

### Upgrade from the retired client runner

Normal CLI startup first checks the private all-runtime completion marker. When
that marker is absent, a filesystem-only fast path checks for legacy service or
state paths; a host with no artifacts returns without contacting launchd or
systemd and without writing the marker. Otherwise startup performs the same
fail-closed all-runtime retirement as `disable --all`. Only a successful
all-runtime cleanup records the marker; a targeted `--runtime` cleanup never
does.

All-runtime cleanup verifies ownership of every service definition before any
mutation. It then disables each owned service, removes its definition, and stops
the loaded unit before inspecting that runtime's state. This prevents a
`KeepAlive` or `Restart=always` unit from repeatedly invoking a removed command
even when local state must be preserved. A loaded unit without an owned
definition is never stopped by ordinary cleanup. The exact retired
`message runner serve --runtime RUNTIME` invocation is recognized only as a
tombstone, allowing that already-running unit to disable and retire itself when
its definition has already vanished; it does not restore a runner command.

Without `--force`, valid notification pointers are preserved while other
private files, including credentials, are scrubbed. A malformed or unreadable
`state.json` is also preserved; other files are still scrubbed when its enclosing
directory is private, while unsafe directory state is preserved without
mutation. Either condition makes the cleanup fail without writing the completion
marker, but the owned service has already been retired. `--force` is required to
discard those pointers or malformed/non-private state. Canonical Postgres
messages are never deleted. Inspect any recoverable message IDs before
explicitly discarding local state:

```sh
witself message runner disable --all
# If warned, inspect ~/.witself/message-runners/*/state.json and read needed IDs.
witself message read msg_... --agent AGENT --json
witself message runner disable --all --force
```

`message runner disable` is a temporary, hidden upgrade-cleanup surface, not a
messaging execution mode. Normal CLI help does not advertise a runner. The
marker represents a successful all-runtime cleanup; the no-artifact startup fast
path and per-runtime cleanup do not write it. Per-runtime cleanup remains
available for diagnosis without suppressing the next startup-wide pass.

## Implementation Slices

1. **Receive correctness (complete)** — metadata-only long-poll listen, separate
   MCP read and ack, parent-validated reply, archive coverage, capability flags,
   and contract tests.
2. **Direct processing fence (complete)** — add delivery claim/renew/release and
   atomic result completion with a monotonic fence, archive interruption, audit,
   and competing-client race tests.
3. **Foreground discovery and processing (complete in the current checkout)** —
   add a bounded message checkpoint, non-blocking startup listen, active-client
   handling guidance, backend-owned causal-depth metadata, durable deterministic
   failure accounting, and provider-accurate hook behavior.
4. **Provider conformance** — supported Codex and Claude Code hooks automatically
   attempt bounded checkpoint injection and fail open; Cursor, Grok Build,
   OpenClaw, Antigravity, and Copilot use guided `self.show`. Every runtime's
   installed policy directs it to use MCP
   listen for unread metadata; that model action is not forced.
5. **Recipient fan-out (complete in the current checkout)** — bounded explicit
   lists and realm snapshots with per-recipient delivery/read/ack and archive
   coverage.
6. **Open realm requests (complete in the current checkout)** — coordination
   state, bounded ordinary offers, client-ranked selection with durable
   `awaiting_selection`, atomic max-assignee reservations/claims, leases/fences,
   renewal, expiry, release, completion, reassignment, and archive interruption.
   This slice consumes current runtime instructions and does not wait for a
   future responsibility/directive schema.
7. **Platform hardening (deferred; not a core completion blocker)** — add
   plan-backed rate/meter enforcement, disabled-agent-specific policy,
   additional metrics, and broader live three-cloud conformance. Core
   competing-client races, interruption recovery, audit, and export/import are
   already covered.

Installed policy directs an active client to handle direct deliveries and
open-request roles: offer or decline, rank durable offers as the immutable
coordinator, claim selected work, renew its lease, and atomically complete a
linked result. It cannot force model compliance. Direct question/reply
continuation uses canonical thread messages; backend causal depth records the
chain but does not enforce a turn ceiling. If a client is not active, every
pending step remains durable; no wake or immediate-processing claim is made.

## Acceptance Scenarios

These scenarios assume active clients follow installed policy. Backend
guarantees concern durable state and fencing, not model action.

- A direct request sent while Bob is offline remains unacknowledged and becomes
  discoverable on Bob's next foreground turn; processing depends on client
  compliance with installed policy.
- Two active Bob clients racing the same request produce one current claim and
  no duplicate durable result.
- Moving a conversation or retry between machines cannot reset durable causal
  metadata or failure accounting: Postgres causal depth and message
  `failure_count` remain authoritative values, while processing generation
  remains the stale-writer fence. No fixed turn or retry threshold is
  backend-enforced.
- A terminal result remains a canonical unacknowledged delivery until Scott's
  active client reads, handles, and acknowledges it.
- Bob asks Scott a question; Scott answers from the original visible context;
  Bob resumes and completes without human intervention.
- A question requiring genuinely new authority escalates instead of inventing
  permission.
- A realm request with `max_assignees=2` can receive many offers but has at most
  two current claims. The coordinator may select fewer than two, and one
  completed result closes the request once that selected batch has no other live
  reservation or claim.
- A `client_ranked` request whose ranking client disappears remains durably
  `awaiting_selection`; no candidate becomes the winner until the same immutable
  coordinator resumes.
- A selected agent that stops renewing expires; the coordinator can select a
  replacement without accepting a stale completion fence.
- Realm fan-out is a send-time snapshot and never crosses a realm boundary.
- `listen` never exposes a body or changes read/ack state.
- Last activity may be displayed as historical recency but never labels an
  agent online, available, or willing to accept work.
- Message, request, and claim state round-trip through account export/import;
  no live lease survives import.
- The backend can pass every test without any provider credential or model
  network call.

## Deferred Extensions And Tunables

- Default short reply wait, offer window, request expiry, lease duration, and
  turn/message limits.
- Whether a future agent responsibility/directive/profile model should be
  exposed as advisory client ranking context. The current backend has no such
  fields and performs no responsibility/capability filtering.
