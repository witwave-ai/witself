# Autonomous Realm Messaging

Status: accepted design under active implementation (2026-07-14). Direct
receive correctness, the migration-0034 delivery-processing fence,
migration-0035 server-derived causal depth, migration-0036 deterministic
per-message failure counting, and the client-owned text-only autonomous runner
are implemented in the current checkout. This is not a deployment or release
statement. Explicit-list/realm fan-out, open realm requests, multi-assignee
claims, and their budget controls remain future slices.

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

## Terms

- **Message** — one immutable, durable communication unit.
- **Mailbox** — one agent's durable inbound deliveries.
- **Thread** — correlation across related request, question, answer, offer, and
  result messages. A thread id is not permission to join or read a thread.
- **Delegation** — a request message whose handling is expected to produce a
  result.
- **Realm** — both the security boundary and the built-in team/room for this
  design.
- **Open request** — a realm-visible delegation for which eligible agents may
  offer or claim work.
- **Claim** — authoritative ownership of one work slot under a bounded lease.
- **Runner** — a client-owned process that listens, claims, invokes the locally
  configured AI, and sends messages as one installed Witself agent.
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
5. All reasoning and provider invocation happen in a client runner. The Witself
   backend contains no model SDK, model credential, prompt engine, semantic
   router, or reply generator.
6. A human normally supplies only the initial request and receives the final
   result or a genuine escalation. Sender and recipient agents handle routine
   clarification automatically.
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
    not require a recipient runtime to be open.

## Current Baseline And Missing Behavior

The implemented direct slice has:

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
- a client-owned direct runner with identity pinning, lease renewal, bounded
  continuation context, provider isolation, retry/recovery, local singleton
  locking, a private metadata-only notification ledger, content-free cycle
  health, and per-user launchd/systemd supervision; and
- full-profile MCP notification list/consume bridging that local ledger to a
  canonical read, with list retained in read-only and both tools absent from
  curator profiles.

It does **not** yet have:

- deterministic mailbox checks in runtime lifecycle hooks;
- explicit-recipient-set or whole-realm delivery;
- open requests, offers, multi-assignee request claims, or reassignment;
- tool-capable autonomous execution;
- native autonomous execution through Codex or Cursor, whose current CLIs do
  not pass the required text-only isolation probe; and
- automatic injection of an asynchronous final result into a foreground sender
  task when no task is open. The current runner invokes only `request`,
  `question`, and `reply`; it durably indexes terminal
  result/decline/escalation metadata before acknowledging the delivery. A
  client or user can list those pointers and read the original message content.

Binding Claude Code to Bob establishes **who Claude is**. It does not keep
Claude running, wake an idle session, or make mailbox retrieval automatic.

## System Boundary

```text
human or trigger
      |
      v
Scott client/runner --send/reply--> Witself mailbox + processing state (Postgres)
      ^                                      |
      |                                      | listen/claim/read
      |                                      v
      +----------- Bob replies ------- Bob client/runner --> Claude Code
                             Codex <-- Scott client/runner
```

The three client-facing mechanisms have different jobs:

- **MCP/CLI/API** expose mailbox and claim operations.
- **Runtime hooks** provide deterministic checks at supported session/task
  boundaries while an interactive runtime is already receiving a turn.
- **The runner** remains active independently, long-polls for work, and starts a
  bounded provider turn when a message arrives.

MCP is request/response transport. It cannot call its own tools, start a model,
or wake an idle Codex or Claude session. A long-poll route also cannot wake a
model by itself; an already-running runner must own the poll.

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

The sender client translates natural-language intent into one closed
coordination mode. The backend validates and enforces the selected mode but
never infers it from message text.

| Mode | Expected behavior |
| --- | --- |
| `notify` | Deliver information; no response is required. |
| `each` | Every addressed agent handles the request and returns a result. |
| `claim` | Eligible agents offer; the coordinator selects at most N. |
| `collaborate` | Agents coordinate in one thread and return one result. |

A direct request to Bob is `each` with one recipient. "Have someone do X" is
`claim` with `max_assignees=1`. "Use the best two agents" is `claim` with
`max_assignees=2`. "Tell everyone" is `notify` or `each` depending on whether
the sentence asks for work. "Have everyone collaborate" is `collaborate`.

Realm `notify`, `each`, and `collaborate` create snapshot delivery state for the
agents present when the message is sent. A realm `claim` creates one indexed,
realm-visible open request rather than triggering inference in every agent.
Eligible runners discover that opportunity through the same listen loop and
only invoke inference when their local deterministic filters or declared
capabilities make the request relevant. The server records the audience
snapshot or equivalent membership version for audit and export.

Agents created after the snapshot do not retroactively receive or become
eligible for the old request. Agent availability is runtime state and does not
change who was in the realm at send time.

## Messages, Replies, And Threads

`kind` remains an open classification label. Common conventions are `note`,
`request`, `question`, `reply`, `offer`, `result`, and `event`; unknown kinds do
not fail storage.

Omitting `kind` on an ordinary direct send normalizes to actionable `request`
on the CLI, MCP, and backend. `note` must be explicit and means FYI-only to the
implemented runner: it records the content-free notification pointer and acks
without provider invocation. The runner's provider-invoking kinds are
`request`, `question`, and `reply`; other labels use the notification path.

Coordination mode and `max_assignees` are closed, validated request controls and
must not be inferred later from `kind` or prose.

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
the sender's mailbox and processed by its runner.

## Direct Autonomous Delegation

The Scott-to-Bob path is:

1. The user tells Scott to ask Bob to perform an objective.
2. Scott sends one `request` message to Bob with the objective, relevant visible
   context, expected result, boundaries, and a fresh idempotency key.
3. The server persists the direct message and recipient delivery state. The
   send does not wait for Claude. Dedicated request coordination state is not
   needed for one direct recipient.
4. Bob's runner receives metadata through `listen`, deduplicates by `msg_` id,
   and atomically acquires the work lease before invoking inference.
5. The runner reads the body, treats it as untrusted input, reconstructs bounded
   thread and agent context, and invokes Claude as Bob.
6. Bob may reply with a question. Scott's runner receives it, invokes its
   configured safe provider with the original objective and relevant bounded
   continuation history, and answers without involving the user when the
   answer is already within the original mandate. A native Codex runner is
   currently fail-closed; this side requires an active Codex task or an
   explicitly integrated safe command adapter until Codex exposes the required
   headless isolation controls.
7. Bob persists a `result` reply and completes the fenced claim. Only after the
   durable result/completion does the runner acknowledge the opening request.
8. Scott's runner records a private content-free pointer to the terminal result
   and acknowledges the delivery. An active client can inspect the notification,
   consume it through a canonical read/verify/exact-local-clear operation,
   report it to the user, or continue the workflow.

The metadata handoff in step 8 is implemented. Automatic injection into a
foreground AI task is still a client-integration requirement: the backend does
not push into a runtime, and the terminal message body remains in Witself until
an authorized `message read` retrieves it.

If either runner is offline, the next message waits durably. No model session
must remain blocked or resident: a runner can reconstruct each turn from the
opening request, relevant thread messages, the stable agent identity, and
authorized Witself context.

## Open Realm Request And Agent Selection

The realm-shout path is:

1. Scott sends a realm `request` with `coordination.mode=claim`, an expiry, and
   an immutable `max_assignees`.
2. Eligible agent runners see the open request. They may reject it locally or
   send an ordinary direct `offer` reply containing a bounded proposed approach,
   relevant capability labels, availability, and estimates.
3. Offers are advisory messages and reserve no capacity. The initiating Scott
   runner waits through a short bounded offer window and uses client-side
   inference to select the best one or several candidates.
4. The server atomically creates claims for the chosen agents while enforcing
   `max_assignees`. Each claim has an expiry and fencing generation.
5. Selected agents work and renew their leases while inference or tools run.
   Non-selected agents stand down; they do not receive every subsequent work
   message merely because they saw the opening request.
6. Selected participants send questions, answers, progress, and results in the
   same thread, addressing only the participants who need each message.
7. A result is persisted before claim completion. If a claimant fails, releases,
   or lets its lease expire, Scott may select the next offer or reopen the
   request.

The default selector is the initiating client AI, not the backend and not the
first claimant. An explicit `first_eligible` policy may be added for cheap queue
work, but it is not the default meaning of "most capable."

Selection may use declared role/capabilities, available tools and permissions,
runner availability, the proposed approach, estimated time/cost, and later
observed reliability. Every self-reported value remains advisory. The backend
stores and filters those values but performs no semantic ranking.

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
- disabling an agent or canceling a request invalidating active claims;
- idempotent mutation keys and durable audit events; and
- import interrupting every active lease and reserving a fresh destination
  fence.

Delivery and work execution are at-least-once. A runner crash after an external
side effect cannot be made exactly-once by the mailbox. Runners must use the
message id, claim id/fence, and domain-specific idempotency keys when performing
retryable work.

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
without consuming a failure budget. Migration
`0036_add_message_failure_count.sql` adds the separate durable `failure_count`.
Only release of the exact claim fence with `deterministic_failure=true`
increments it atomically. Provider-wide or unavailable-provider failures,
configuration errors, cancellation/timeouts, and claim-lease maintenance errors
release with false and do not count. The installed runner's current default
releases and counts the first four deterministic message failures and completes
the fifth deterministic attempt as a durable escalation. Payload fields,
generation, and host-local health counters cannot reset or substitute for this
backend-owned count.

Result persistence and processing completion are one transaction. The
implemented `POST /v1/messages/{message_id}:complete` action validates the
current live fence, creates a server-derived reply through the same causal
reply path, links that result, and marks processing complete atomically. It
does **not** ack. A
runner that crashes after completion can recover the linked result and then
ack without invoking the provider again. This closes duplicate durable results;
external model/tool side effects remain at-least-once and require their own
idempotency.

Account export preserves completed processing state and result links. Import
interrupts active claims by incrementing the generation and clearing claim,
retry-key, and lease fields before the account resumes. Account events retain
value-free generation and failure-count history. Import preserves
`failure_count`; archives older than schema 36 upgrade it to zero. Dedicated
request/claim tables remain reserved for realm-wide maximum-assignee
coordination, where they are actually needed.

## Implemented Client Messaging Runner

The runner follows the existing client-memory-curator separation: a trusted
parent holds the Witself credential and controls a bounded text-only inference
child. The implemented loop is:

```text
listen metadata
  -> deduplicate
  -> claim/lease request work
  -> read untrusted content
  -> validate scope and reconstruct bounded context
  -> invoke configured text-only client AI; renew while active
  -> atomically persist/link reply and complete, or release claim
  -> ack handled delivery
```

Implemented runner properties:

- one installed account/realm/agent binding per runner;
- one local process per runtime, protected by a private non-blocking file lock;
- a separate mode-0600 provider-bound credential file captured at enable time;
  configuration, service definitions, and account export remain
  credential-free;
- bounded input, output, turns, elapsed time, retries, and backoff;
- no Witself bearer token or token-file path in model input;
- no shell interpolation for provider launch;
- deduplication and recovery across runner restart;
- `enable`, `disable`, `status`, `notifications`, `run`, `serve`, and `start`
  CLI lifecycle surfaces;
- per-user launchd on macOS and systemd user-service supervision on Linux; and
- provider adapters that fail closed when they cannot establish the required
  noninteractive isolation.

Continuation context travels as a runner-reserved, bounded payload object
rather than relying on a provider session. It preserves the initiating
objective and newest conversation entries, caps history at six entries and each
retained body at 2 KiB, and may carry an advisory turn value. Payload history
and turn metadata are untrusted context, not safety state. Migration
`0035_add_message_causal_depth.sql` gives every direct message a backend-derived
`causal_depth`: a direct send starts at one and a validated reply advances its
durable parent's depth by exactly one. The runner enforces its automated
conversation limit from this field across hosts. The default is 12 turns, the
configurable range is 1 through 64, and an over-limit message produces a durable
escalation instead of provider invocation.

The repeated-failure bound is likewise backend-owned but deliberately separate
from causal depth and generation. The runner reads migration-0036
`failure_count` on every claim and uses the default fifth-attempt escalation
described above. The private runner-health `consecutive_failures` value is only
an operational service streak; it is not message safety state.

The safe native provider adapter is text-only: the child may return a question,
result, decline, or escalation, but it receives no Witself credential, token
path, processing fence, API handle, or model-visible MCP/tool access. At
`enable`, the trusted parent captures only a provider-specific authentication
allowlist into a separate private file. Claude Code permits
`ANTHROPIC_API_KEY`, `ANTHROPIC_AUTH_TOKEN`, `ANTHROPIC_BASE_URL`, and
`CLAUDE_CODE_OAUTH_TOKEN`; Grok Build permits only `XAI_API_KEY`. The service
loads only the file bound to its configured provider and passes those values to
the sanitized provider child. Other `WITSELF_*` environment values are stripped
apart from a reserved value-free runner-session marker, message content never
enters argv, no shell is used, and one strict result JSON object is validated.
Claude Code and Grok Build have implemented native adapters whose installed CLI
must advertise every required isolation control before enablement. Grok receives
the private prompt as a mode-0600 plain-text `turn.txt` inside the mode-0700
per-call workspace because its strict sandbox reads only beneath `--cwd`; it
runs with strict sandboxing, plan permission mode, tools/MCP, memory, web search,
and subagents disabled, and the entire scratch root is removed on return. Claude
receives the prompt on stdin with its corresponding no-tools, no-persistence,
strict-empty-MCP controls. Codex and Cursor are recognized but deliberately fail
closed because their current CLIs do not expose an equivalent provable contract.

Local conformance verification for this checkout invoked the isolated
`NativeTextProvider` against installed Claude Code 2.1.202 and Grok Build
0.2.101 and passed fixed, content-free result assertions for both. This is a
local implementation check, not a release, deployment, or general platform
compatibility claim; enable still probes the installed CLI on each host.

The runner core also provides a strict generic command-adapter protocol for a
separately configured wrapper: one bounded JSON envelope on stdin and one
content-only result on stdout, with the same token-free child boundary. The
current persisted `message runner` CLI selects only capability-probed native
providers; making an arbitrary wrapper user-configurable requires a separate
configuration surface and security review. Tool-capable execution is a
different adapter class and requires an explicit workspace/OS sandbox plus
credential isolation. Gemini and GitHub Copilot remain deferred.

For terminal and other non-provider message kinds, the trusted runner records a
private content-free pointer before it acknowledges the delivery. `message
runner status` reports the pending notification count, and `message runner
notifications` lists the newest pointers first; the ordinary `message read`
operation remains the content boundary. The ledger is bounded to 1,024 entries
and fails closed at capacity: the runner does not silently evict a pointer or
acknowledge a message it could not record. Explicit
`notifications --clear MESSAGE_ID` removes inspected local pointers;
`--clear-all` removes every pointer. Neither deletes the durable Witself
messages.

The full MCP profile adds `witself.message.notification.list` and
`witself.message.notification.consume`. List exposes only bounded local pointers
and is retained in read-only mode. Consume performs the canonical message read,
verifies it and the runtime/account/realm/agent binding against the pointer,
rechecks the binding, and removes only that exact local entry. A failure at any
stage keeps the pointer for retry. Curator profiles expose neither bridge tool.
Grok receives underscore-safe names:
`witself_message_notification_list` and
`witself_message_notification_consume`.

Acknowledgement is canonical and global for the agent delivery, while the
handoff pointer exists only in that runtime's local `WITSELF_HOME`. A different
machine or runtime bound to the same agent therefore cannot see the pointer and
will not rediscover the acknowledged delivery through unread mailbox state.
This is an intentional MVP locality limitation, not cross-host wake delivery.
The host-local ledger is excluded from account export; the canonical PostgreSQL
message remains included.

The same private state records content-free runner health for operational
inspection: last cycle and last success timestamps, the last bounded status or
error class, and consecutive failure count. It never retains raw error text,
message ids/content, provider output, or credentials. `message runner status`
returns that health alongside service state and notification count.

## Active-Session Hooks And Long Polling

The deterministic active-session experience and the autonomous experience are
related but distinct:

- The full and read-only MCP startup instructions tell an already-active agent
  to call `message.listen(wait_seconds=0)` and
  `message.notification.list` at the beginning of non-trivial work. These are
  non-blocking, metadata-only checks for canonical unacknowledged work and
  already-acknowledged local handoff pointers respectively; they do not wake a
  model or expose content.
- On supported runtimes, `SessionStart` and `UserPromptSubmit` hooks check
  unread **metadata** and may inject a bounded notification such as "one unread
  message from Scott." They do not inject bodies, mark read, acknowledge, or
  execute work.
- `message listen` is a stateless, waitable, metadata-only query for the oldest
  unacknowledged inbound messages. It waits 20 seconds by default, accepts a
  bounded 0–20 second wait plus sender/thread/kind/limit filters, and has no
  cursor. A timeout returns normally with no state change. Disconnecting loses
  no message, and a crash after read but before ack makes the message eligible
  for listen again. Each server process bounds concurrent listen admission;
  saturation returns HTTP 429 with `Retry-After` and the runner retries.
- `message read` returns the body and marks read.
- `message ack` records that the recipient finished handling the delivery and
  returns metadata only, never the body or payload.
- The autonomous runner owns repeated `listen` calls. An idle interactive
  window without a runner remains idle.

This preserves the untrusted-content boundary and prevents a passive hook from
turning a delivered message directly into model instructions.

## Authority, Escalation, And Loop Safety

The initial request is a bounded objective, not a transferable permission
grant. Each agent may make ordinary reversible decisions within that objective
using its own authenticated tools and policies. The sender agent answers routine
recipient questions automatically when the existing request/context already
contains the answer.

Human escalation is reserved for genuinely new authority or an irreducible
choice, including an otherwise unauthorized destructive action, expenditure,
external communication, credential/reveal ceremony, permanent deletion, or a
material expansion of the objective. A message or autonomous runner may never
set a "direct user authorized" flag merely because an earlier human initiated
the thread.

Every autonomous request is bounded by server-enforceable limits where the
server has the necessary information:

- recipient/fan-out limit;
- request expiry and lease duration;
- maximum assignees and concurrent claims;
- maximum message count/turns per thread;
- per-agent and per-realm send/delivery rates; and
- maximum open requests and autonomous replies.

Token, model-call, and dollar budgets are runner-side because a model-free
backend cannot reliably observe them. The runner records value-free outcomes
and stops with `failed`, `expired`, or `escalated` rather than allowing an
unbounded agent loop.

Message bodies, offers, and model output remain untrusted. They never authorize
memory/fact writes, permanent deletion, secret access, policy changes, or
unrelated account mutations. Any such operation runs its ordinary independent
authorization path.

## Target Storage Roles

The direct-delivery processing shape is pinned above; realm-request tables
remain target work. The storage roles are:

- `agent_messages` — immutable sender, content, kind, thread, parent reply,
  backend-derived causal depth, and coordination reference.
  Sender/account/realm are token-derived. Migration `0035` adds the depth field.
- `agent_message_deliveries` — snapshot recipient plus delivery/read/ack state.
  Migration `0034` adds the direct processing generation, claim/lease fields,
  completion time, and result link; migration `0036` adds the independent
  deterministic `failure_count`. Neither overloads read/ack as a lease.
- `agent_message_requests` — opening message, audience snapshot/version,
  coordination mode, requester/coordinator, state, expiry, and immutable
  assignment capacity.
- `agent_message_claims` — selected agent, state, lease expiry, fence,
  attempts, and completion outcome.

Offers remain ordinary `kind=offer` messages rather than a separate table. A
thread id remains correlation, while `reply_to_message_id` records causality.
The service validates parent participation rather than trusting a caller that
knows a thread id.

Logical account export/import currently includes messages, deliveries, causal
links and depths, completed direct-processing state, result links, and
deterministic failure counts plus audit-safe lifecycle data. Import validates
account/realm/agent, recomputes or validates depth against the reply graph, and
validates result-link closure and the non-negative failure count. A `claimed`
delivery is restored as `available` with its generation incremented, its failure
count unchanged, and its claim id, key hash, and lease cleared, so no source-cell
runner can complete on the destination. Completed processing and its result link
are preserved. Pre-schema-36 archives receive `failure_count=0`. Future
request/claim rows must follow the same rule. PostgreSQL
remains the sole canonical message data source across AWS, Azure, and Google
Cloud. Client-local runner configuration, notification pointers, and provider
credentials are host state and are not account-exported.

## Surfaces

The implemented direct same-realm surface is `send`, `reply`, `list`, `listen`,
`read`, `ack`, `claim`, `renew`, `release`, and `complete` across HTTP
client/server, CLI, and MCP. MCP also exposes local notification list/consume;
these have no backend route because the pointer ledger is host-local. Read,
acknowledgement, processing claim, and completion are separate on every
server-backed surface. The CLI additionally implements
`message runner enable|disable|status|notifications|run|serve|start`; this local
process lifecycle is not a backend operation. Target additions are:

- send to repeated explicit agents or the token-derived realm;
- coordination mode, expiry, offer window, and maximum assignees on a request;
- request list/show/cancel plus separate multi-assignee request-claim
  operations;
- hook-delivered unread metadata at supported runtime boundaries; and
- tool-capable, explicitly sandboxed runner adapters.

Every server-backed agent operation has CLI/MCP parity over the same
client/core contract. Host-local runner configuration and launchd/systemd
service lifecycle remain CLI-only because they are not backend resources.

## Implementation Slices

1. **Receive correctness (complete)** — metadata-only long-poll listen, separate
   MCP read and ack, parent-validated reply, archive coverage, capability flags,
   and contract tests.
2. **Direct processing fence (complete)** — add delivery claim/renew/release and
   atomic result completion with a monotonic fence, archive interruption, audit,
   and two-runner race tests.
3. **Text-only autonomous runner (complete in the current checkout)** — add a
   sibling `messagerunner` core with private value-free state, local
   singleflight, identity pinning, bounded retries, launchd/systemd supervision,
   fake-provider recovery tests, backend-owned causal-depth and deterministic
   failure-count bounds, advisory bounded continuation context, and native
   Claude Code/Grok Build text-only adapters.
4. **Provider conformance (partially complete)** — Claude Code and Grok Build
   are capability-probed and supported; Codex and Cursor stay unsupported until
   they pass. Tool-capable execution remains a separately sandboxed contract.
5. **Recipient fan-out** — add bounded explicit lists and realm snapshots with
   per-recipient delivery/read/ack and archive coverage.
6. **Open realm requests** — add coordination state, offers as messages, atomic
   max-assignee claims, leases/fences, selection, renewal, expiry, and
   reassignment.
7. **Guardrails and conformance** — add rates/budgets, cancellation, disabled
   agent behavior, multi-runner races, restart recovery, metrics, audit, and
   three-cloud/export-import tests.

The implemented backend/runner vertical is direct processing, not realm
bidding: one durable request is automatically claimed, read, passed to a safe
text-only provider, completed with one atomically linked reply, and
acknowledged. Multi-turn question/reply continuation uses bounded advisory
payload context while backend causal depth enforces the turn limit. A fully
native Codex-to-Claude unattended pair remains blocked specifically on the Codex
native isolation contract. Automatic injection of the final asynchronous
terminal result into a foreground task is also still a client-integration
slice; the durable metadata handoff is implemented and the remaining work is
not a mailbox or fence gap.

## Acceptance Scenarios

- A direct request sent while Bob is offline is processed after Bob's runner
  starts.
- Two Bob runners racing the same request produce one current claim and no
  duplicate durable result.
- Moving a conversation or retry between machines cannot reset the automated
  turn or failure bounds: Postgres causal depth and message `failure_count`
  remain authoritative, while processing generation remains the stale-writer
  fence.
- Scott's background runner records a terminal result pointer before ack; the
  user can list the pointer and use MCP consume to read/verify the durable result
  before clearing only that exact local notification.
- A full 1,024-entry local notification ledger leaves the next non-provider
  delivery unacknowledged instead of silently losing its pointer.
- Bob asks Scott a question; Scott answers from the original visible context;
  Bob resumes and completes without human intervention.
- A question requiring genuinely new authority escalates instead of inventing
  permission.
- A realm request with `max_assignees=2` can receive many offers but has at most
  two current claims.
- A selected agent that stops renewing expires; the coordinator can select a
  replacement without accepting a stale completion fence.
- Realm fan-out is a send-time snapshot and never crosses a realm boundary.
- `listen` never exposes a body or changes read/ack state.
- Message, request, and claim state round-trip through account export/import;
  no live lease survives import.
- The backend can pass every test without any provider credential or model
  network call.

## Open Implementation Choices

- Default short reply wait, offer window, request expiry, lease duration, and
  turn/message limits.
- Exact wire names for explicit recipient lists and the token-derived realm.
- The first restricted runner credential profile and its exact message/request
  permissions.
- Which declared capability and availability fields are deterministic enough to
  filter before client inference.
- Which client/UI should turn the implemented terminal-notification ledger into
  an automatic foreground task or user alert.
- A user-facing configuration contract for the generic command adapter.
- A separately sandboxed tool-capable execution adapter.
