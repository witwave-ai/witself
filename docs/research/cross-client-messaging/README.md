# Cross-client messaging

Status: **research / deferred feature; implementation is not authorized by this
document**. Recorded from the founder discussion on 2026-10-23 Mountain time
(2026-10-24 UTC). Working name: **cross-client messaging**.

This is an extension of WitSelf's internal messaging, not agent email. It lets
one logical agent communicate between the AI clients using its identity: for
example, Scott in Codex asking Scott in Claude or Grok a question. Shared agent
identity, facts and memories remain shared. Separate agents are not required
merely to obtain separate message destinations.

Source assessment baseline: `2daf2f9f2cc6aa6ac1d00e4af1126b99fb5acb9a`.
The findings below come from static code and contract review, not a runtime
experiment or proof that this proposed capability exists. Refresh source
references when implementation begins. Claude's ongoing Local Cell work is
separate; this research changes none of its requirements or implementation.

## Intent and planning premise

The motivating example is asking another active client what it is working on,
instead of reconstructing its progress from a transcript. A response can provide
the relevant context and evidence deliberately, while transcripts remain a
separate interaction record.

Selected product intent:

- Keep one Scott agent across AI clients and locations.
- Address a message to a particular client, optionally qualified by location.
- Support home-to-work communication without treating home and work as different
  agent identities.
- Keep targeted messages from being consumed or acknowledged by another client
  simply because both clients authenticate as Scott.
- Give replies a clear return destination, and preserve pending messages while a
  destination is offline. Delivery alone does not wake an AI or start a task.
- Record the idea and its engineering implications now; choose implementation
  scope and authorization later.

Scott reports that there are currently no external users to migrate. This is a
founder-provided planning premise, not a live account-inventory finding. It gives
us room to choose a clean contract and a coordinated upgrade of controlled
installations rather than promise indefinite support for every old client.
Existing founder data, archives, simultaneous clients and active work still
matter. A coordinated breaking change is an available design option, not
permission to discard data or disrupt running work.

## Examples and terminology

The examples describe user intent, not proposed CLI syntax or a frozen wire API.

| Example | Required distinction |
|---|---|
| Scott in Codex at home asks Scott in Claude at home for a progress report. | Same agent and installation, different clients; Claude's reply returns to the requesting context. |
| Scott in Codex at home sends a question to Scott in Grok at work. | Client and location both constrain the destination. |
| Two Claude sessions are open at home. | A client name alone may be ambiguous; do not choose whichever transcript was most recently updated. |
| Send a notice to all of Scott's clients at work. | Explicit fan-out, with a defined recipient snapshot and separate delivery tracking; different from asking one client to do work. |
| Claude is closed when the question arrives. | The message waits according to a defined lifetime; no implicit launch or inference purchase. |

Keep these concepts separate:

| Concept | Meaning |
|---|---|
| Agent | Durable account/realm identity, such as Scott. This remains the principal for shared state. |
| Runtime/client kind | Codex, Claude Code, Grok Build, etc. This is not the selected model; changing Fable to Opus should not accidentally change an address. |
| Location | An installation identifier with an optional human label such as home or work. A label is not proof of geography, ownership or permission. |
| Client endpoint | Proposed durable, enrolled messaging destination belonging to an agent. Its precise lifetime and credential binding are undecided. |
| Session / run | A particular conversation or execution lifetime. Useful for exact return routing, but neither is automatically a durable endpoint. |

The relationship between an endpoint, an installation and a session is the first
design decision. One possible design has a durable endpoint with optional
session-specific addressing; another makes each session a destination. Compare
restart recovery, closed conversations, multiple windows and user-facing
selection before choosing.

## What the code already provides

These are findings at the stated baseline, not a proposed new contract.

| Area | Observed behavior | Source |
|---|---|---|
| Authentication | The token-derived principal has agent/account/realm/token identity and access profile, but no authenticated runtime, location or session. A token ID alone is not an enrolled client identity. | [Principal and authentication](../../../internal/store/auth.go#L62-L75), [token lookup](../../../internal/store/auth.go#L226-L278) |
| Delivery and retries | Delivery identity is `(message_id, recipient_agent_id)`; send idempotency is scoped to account and sender agent. There is no client component. | [Messaging schema](../../../internal/store/migrations/0020_add_agent_messaging.sql#L1-L48) |
| Self-addressing | The direct-send path resolves a live same-realm agent without excluding the sender, so the source permits Scott-to-Scott messages. These still enter Scott's shared mailbox. Realm fan-out explicitly excludes the sender agent, including every client using it. | [Recipient resolution](../../../internal/store/message.go#L1596-L1642), [delivery insertion](../../../internal/store/message.go#L1035-L1044) |
| Receive and processing | Inbox/read/ack and claim operations identify the recipient by agent. Claims serialize attempts on that shared delivery; they do not establish which client was intended. | [Inbox predicates](../../../internal/store/message.go#L1163-L1177), [read and ack](../../../internal/store/message.go#L1272-L1335), [claim acquisition](../../../internal/store/message.go#L536-L580) |
| Replies and completion | Replies derive the destination from the parent sender agent. Fenced completion creates and links one result reply atomically; no originating client/session is stored for return routing. | [Reply routing](../../../internal/store/message.go#L477-L494), [completion](../../../internal/store/message.go#L785-L861) |
| Checkpoints and listening | The content-free checkpoint is agent-wide and advisory. Long-poll admission allows two concurrent listens per account/realm/agent per server process, with an additional process-wide limit. | [Message checkpoint](../../../internal/store/message_checkpoint.go#L11-L27), [checkpoint queries](../../../internal/store/message_checkpoint.go#L73-L128), [listen limiter](../../../internal/server/message.go#L14-L38) |
| Requests and offers | Coordinators, candidates and selected claims are identified by agent. Multiple Scott clients are not independent candidates; another Scott client can act as the same coordinator. This is more than direct-message delivery. | [Request schema](../../../internal/store/migrations/0038_add_open_message_requests.sql#L1-L143), [coordinator authorization](../../../internal/store/message_request.go#L1703-L1714) |
| Location and activity | A stable local `loc_` identifier and mutable label already exist. Activity collapses to one row per `(agent_id, runtime, location_id)` and deliberately excludes session IDs, models and paths from its hook projection. These are observations, not authenticated destination enrollment. | [Location](../../../internal/transcriptcapture/config.go#L66-L71), [location lifecycle](../../../internal/transcriptcapture/config.go#L163-L195), [activity upsert](../../../internal/store/agent_activity.go#L68-L112), [privacy projection](../../../internal/transcriptcapture/capture.go#L110-L128) |
| Transcript sessions | Transcript external IDs combine runtime, location ID and native session ID. Restart may preserve the session while creating a new run; compaction does not restart the run. These identifiers provide context, not messaging authority or proof of availability. | [Session namespace](../../../internal/transcriptcapture/capture.go#L2301-L2308), [run lifecycle](../../../internal/transcriptcapture/capture.go#L420-L449) |
| Foreground adapters | Codex and Claude support model-visible session/prompt hydration. Grok uses guided `self.show` fallback because passive hook output is ignored. New routing context must reach both paths without assuming a shared hook mechanism. | [Hydration capabilities](../../../internal/memoryhydration/hydration.go#L79-L105) |
| Client inventory | Account Clients scans fixed local installation records and does not invoke a provider. It is not a server-side endpoint directory or proof that a client is accepting work. | [Inventory projection](../../../internal/clientinventory/inventory.go#L87-L105), [static scanner](../../../internal/clientinventory/inventory.go#L143-L165) |
| Version compatibility | Current send/listen structs have no client selector. The shared JSON decoder accepts unknown fields. Adding a `client_id` field beside an otherwise valid old recipient can therefore be silently ignored by an old server. | [Send request](../../../internal/server/server.go#L1689-L1717), [listen request](../../../internal/server/server.go#L1741-L1763), [decoder](../../../internal/server/server.go#L4226-L4236) |

The existing [messaging contract](../../inter-agent-messaging.md) distinguishes
listing, reading, acknowledging, claiming and completing work. Those distinctions
remain useful. An acknowledgement is not proof of a successful task, and a lease
is not proof of an intended destination. No live self-send experiment was run for
this research. Sending a note to the exact same session is a separate optional
use case; the motivating value here is coordination between different clients.

## Proposed design direction

Use the existing message/thread and delivery machinery where it fits, with an
explicit destination identity and return route. Do not model each model choice
as a new agent or use transcript searches as the routing mechanism.

1. **Enroll and resolve destinations.** Define how an endpoint is registered,
   authenticated, named, revoked and discovered. Resolve friendly selectors to
   stable IDs. A runtime plus a location label can match multiple installations
   or sessions: ambiguity must be visible, not resolved by last activity.
2. **Bind the actual caller.** Establish a trusted runtime-to-CLI/MCP context
   path. Session data captured by a hook does not automatically reach a shared
   MCP server; current [MCP serve configuration](../../../cmd/witself/mcp.go#L1181-L1204)
   has runtime/location but no session/run binding. Explicit sender/receiver strings alone cannot prove which client
   is calling. The current [self request](../../../internal/client/self.go#L316-L351)
   and [headers](../../../internal/client/self.go#L375-L401) have no such binding.
3. **Persist target and origin.** Store the resolved audience and originating
   endpoint/session on the message graph. Derive replies and results from that
   origin rather than selecting the sender's currently active client. Define
   what happens if the originating session ends or its endpoint is revoked.
4. **Scope every receive operation.** Discovery, checkpoint, direct-ID peek/read,
   acknowledgement, claims, renewal, release and completion must agree about the
   destination. A filtered inbox with an agent-wide direct-ID acknowledgement
   route is not sufficient. Keep generation fences and atomic result linkage.
5. **Make audience semantics explicit.** One named destination, any one eligible
   destination, and every destination are different operations. A broadcast
   should freeze its audience at send time; a newly enrolled client should not
   silently inherit past messages. Choose bounds and aggregate usage accounting.
6. **Keep execution under client control.** An active client checks its pending
   work and decides how to handle it within the user's authorization. A status
   question is different from permission to edit code, deploy, spend money or
   forward secrets. Presence is advisory; a stored message is not a wake-up.

Keep existing account messaging entitlements and principal scopes in the path.
Addressing a client does not enable messaging for a disabled account or authorize
a new commercial-policy change.

Recommended first scope to evaluate: targeted direct messages, notes and replies
within one account/realm, with explicit destination identity and a proven return
route. Endpoint-aware open requests, bidding and selection require their own
complete coordinator/candidate design; either include that work deliberately or
reject unsupported targeting on those surfaces. Do not imply that ordinary
delivery changes automatically upgrade the request graph.

## Gotchas that must be resolved

### Shared credentials and home/work privacy

A client-supplied label can support cooperative routing, but it cannot stop
another holder of the same broad credential from presenting that label. Decide
whether this feature promises accidental-consumption prevention, enforced
endpoint access control, or both. For enforced targeting, design a credential-
bound endpoint identity and restrict enrollment/impersonation authority; merely
adding an ID to the request is insufficient. Test the unscoped old-token path too.

Targeted message delivery does not make shared Scott facts, memories or transcripts
private between home and work. If a workplace requires separate data access,
use an explicit authorization boundary; a `work` label must not claim to provide
one. A reply should share only relevant authorized context, not automatically
forward a full transcript or sealed data. Address and discovery metadata can
itself disclose installation/location information and needs bounded visibility.

### Sessions, restart and stale destinations

Two windows can use the same runtime and location. A copied installation can
carry a copied location ID. A model switch, app restart, session resume, machine
replacement and clone are different events. Define endpoint identity/lifetime,
duplicate detection, session rebinding, revocation and unavailable-origin rules.
Keep a message pending or return an explicit failure; never silently deliver it
to some other Scott client. Stale presence must not erase durable work.

### Mixed versions and coordinated upgrade

Choose a capability/version contract and an operation shape that an old server
rejects. A new optional field on today's otherwise valid send/listen request is
unsafe because of permissive decoding. A coordinated upgrade may simplify the
transition, but partially upgraded installations, rollback and old archives
still need deterministic behavior. Missing targeting support must fail closed,
not degrade to the agent-wide inbox. This is a migration requirement, not a
promise to maintain old binaries forever.

### Concurrency, replies and loops

Separate delivery identity from the worker attempt that currently holds its
lease. Preserve retry idempotency, lease expiry, stale-writer rejection and
atomic completion, including crashes after read but before acknowledgement.
Decide whether send idempotency stays agent-scoped or gains an endpoint scope.
The guarantee is one durable completion/result link, not exactly-once external
side effects: recipients still need idempotent execution or reconciliation.

Agent identity alone would be inadequate for detecting cross-client reply loops.
Preserve the existing backend-derived causal depth, distinguish notes from requests, and set
explicit reply/hop/turn/cost limits for any future automatic response policy.
Replies must follow the stored origin, including when it is gone. Do not turn
the sender's own receipt or status update into an infinite work request.

### Request coordination is a separate seam

The existing request graph grants one response per candidate agent and
coordinator authority to the coordinating agent. It cannot represent several
independent offers from Scott's different clients. Ordinary processing tools
also [reject graph-linked messages](../../../internal/store/message.go#L1873-L1897).
Preserve that separation. Define endpoint-specific coordinator continuation,
offers, selections, reservations, result routing and cancellation together before
advertising endpoint-aware delegation.

### Archives, retention, limits and Local Cell synchronization

Archive export/import explicitly enumerate message/request columns and current
delivery identity. [Archive tables](../../../internal/store/archive_tables.go#L102-L107),
[export](../../../internal/store/export.go#L1104-L1190) and
[import column rules](../../../internal/store/import.go#L508-L555) all need review.
Historical messages need an explicit agent-wide or migration disposition.
Transferred client credentials must not recreate device authority accidentally.
Existing import [invalidates active ordinary claims](../../../internal/store/import.go#L1371-L1396)
and [request claims/reservations](../../../internal/store/import.go#L1526-L1561);
new endpoint state must preserve that stale-authority protection.

Retention works on complete threads, with active-work and memory-evidence holds;
it [locks delivery rows using the current identity](../../../internal/store/message_retention.go#L884-L902).
Endpoint fan-out changes graph size, lock ordering, row budgets, retention and
usage accounting. Keep account/agent aggregate limits as well as any new endpoint
limits; multiplying endpoints must not bypass them. The current two-listen limit
also needs deliberate treatment rather than unbounded growth.

The [Local Cell plan](../local-cell/implementation-plan.md) separately requires
local messaging and continuous synchronization. Coordinate delivery identities,
request-wide authority, deletion/retention and lease fencing with that work.
An endpoint address is not an execution-authority epoch. Replicating a pending
message must not permit local and hosted copies to execute it independently.
This research neither blocks nor modifies the currently authorized LC-S1 slice.

## Evidence required before implementation is called ready

These are proposed acceptance scenarios, not tests that have been run:

- Three clients share Scott: Codex/home sends to Claude/home while Grok/work
  polls. Only the intended destination can handle targeted work; untargeted
  Scott messages retain their explicitly selected semantics.
- Two Claude sessions at one installation, two installations labeled work, and
  a cloned location ID produce explicit resolution/identity behavior. A model
  change does not redirect a stable endpoint.
- Wrong endpoint, copied broad token, revoked endpoint and direct-ID access are
  exercised for read, ack, claim, renew, complete and reply, not just listen.
- Replies and results reach the recorded origin across restart/resume, compaction,
  offline periods and rename; stale runs cannot complete replacement-run claims.
  Closed sessions and deleted origins follow the chosen
  unavailable-origin policy without silent reassignment.
- Concurrent attempts, repeated send/claim keys, lease expiry and crashes before
  and after result persistence yield no lost work or duplicate durable result.
- Checkpoint and listen agree about pending work through both model-visible
  hydration and guided `self.show`. More than two same-agent clients have defined
  admission/fairness behavior within bounded server load.
- A new client against an old server cannot silently send to the shared inbox;
  old clients cannot consume newly targeted work through an unscoped route.
- Explicit broadcast has bounded per-destination receipts and aggregate rate
  accounting. Same-agent replies terminate under the chosen loop policy.
- Endpoint-aware requests are either fully covered (including coordinator
  restart and independent same-agent offers) or explicitly unsupported.
- New and historical archive round-trips preserve message graphs, retention
  holds and identities while invalidating live claims and device credentials as
  designed. Local/hosted interruption and synchronization cannot create concurrent
  valid execution authority or duplicate durable results; external effects follow
  the chosen idempotency/reconciliation policy.
- An offline destination stays offline. No message, checkpoint, hook or MCP call
  launches a model; an unauthorized work request grants no additional authority.

## Open decisions and next step

We control WitSelf's storage, API, CLI and integration adapters, so we can change
their contracts to support this feature. Provider-specific session and hook
limitations still need verification; owning WitSelf does not grant control of an
AI runtime's execution lifecycle.

Before proposing an implementation slice, settle:

1. Durable endpoint versus session addressing, including exact return routing.
2. Cooperative routing versus enforced endpoint isolation and enrollment rights.
3. Single-recipient default, ambiguity handling and explicit fan-out behavior.
4. Whether endpoint-aware request coordination belongs in the first delivery.
5. Message lifetime, unavailable-origin handling, revocation and archive mapping.
6. Coordinated upgrade/version contract and the Local Cell synchronization seam.

Next safe step: review these choices with Scott one at a time when needed, then
write a bounded implementation proposal with API/schema changes and acceptance
evidence. Saving this research does not select those mechanisms, authorize code
implementation, or start a background messaging service.

[Deferred-feature roadmap](../../post-v0-roadmap.md) ·
[Messaging](../../inter-agent-messaging.md) ·
[Foreground request coordination](../../autonomous-realm-messaging.md)
