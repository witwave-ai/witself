# Witself Backend Architecture

Status: draft. This document captures the public backend and self-hosting
architecture direction. Last reviewed 2026-07-14.

Inference-boundary amendment (accepted 2026-07-14): under
[narrative-memory-and-curation.md](narrative-memory-and-curation.md), the
backend stores and deterministically searches/applies client-authored memory
data and optional client-supplied vectors. It never calls an LLM or embedding
model. Later server-provider language is superseded; PostgreSQL and the public
backend boundary remain accepted.

## Decision

Witself should keep the backend API server in the public `witself` repository.
The code that stores, indexes, recalls, authorizes, audits, and serves identity
material should be inspectable alongside the CLI and MCP adapter.

The managed Witself Cloud service is the default commercial deployment, but the
same backend code should be usable by operators who want to self-host Witself in
their own cloud, network, or compliance boundary.

The backend API server should ship as a separate `witself-server` binary, not as
a public `witself server` subcommand. Keeping the server process separate makes
production and self-hosted deployment packaging, container entrypoints, service
permissions, and operational docs clearer while leaving `ws` focused on
human, agent, and MCP workflows.

## Deployment Modes

| Mode | Purpose | Storage | Production posture |
|---|---|---|---|
| Managed Witself Cloud | Default paid service operated by Witwave | PostgreSQL with full text and optional migration-0032 JSONB vectors | Production |
| Self-hosted Witself Server | Customer-operated backend in their own cloud | PostgreSQL with full text and optional migration-0032 JSONB vectors | Production once supported |
| Local development backend | Tests, demos, and early CLI work | Local PostgreSQL using the same schema and deterministic retrieval path | Development only |

The local backend uses the same server and PostgreSQL contract as production.
It does not run or call a local model. Full-text recall works without optional
vectors, so development does not need model credentials or model egress; see
[narrative-memory-and-curation.md](narrative-memory-and-curation.md).

## Repository Shape

Initial target layout once code starts:

```text
cmd/ws/                  # CLI, including mcp serve
cmd/witself-server/           # Backend API server
internal/core/                # Domain service and use cases
internal/api/                 # HTTP handlers and request/response adapters
internal/auth/                # Token validation and principal resolution
internal/policy/              # Cross-agent policy engine and security groups
internal/retrieval/           # Deterministic FTS, filters, and optional vector math
internal/audit/               # Audit event generation and sinks
internal/observability/       # Metrics, logs, request IDs, and health probes
internal/store/               # Canonical Postgres services, including messaging
internal/store/postgres/      # Authoritative relational store; FTS + JSONB vectors
internal/store/blob/          # Object/blob storage adapter when needed
internal/server/              # Server config, lifecycle, health, migrations
images/witself/               # CLI/MCP container image
images/witself-server/        # Backend API server container image
charts/witself-server/               # Self-hosted Kubernetes Helm chart
infra/terraform/              # AWS/GCP/Azure infrastructure modules and stacks
docs/backend-architecture.md
docs/self-hosting.md
docs/helm-chart.md
```

This layout is a starting point, not a promise that every package name is final.
The important boundary is one core domain service with multiple adapters.

## Process Boundaries

Witself should have these public entrypoints:

- `ws`: human and agent CLI.
- `witself mcp serve`: local-first MCP adapter over the same core behavior.
- `witself-server`: separate backend API server for managed and self-hosted
  deployments.

The CLI calls a managed, self-hosted, or locally running `witself-server`
endpoint through the same public API contract. Local development changes the
deployment profile, not the authoritative data model or inference boundary.

Remote mode should use the versioned HTTP API contract described in
[api-contract.md](api-contract.md), with the route style in
[api-routes.md](api-routes.md). Every backend should expose capability discovery
so clients can distinguish managed, self-hosted, and local behavior, full-text
recall availability, and optional vector-profile coverage without guessing.

## Interface Invariant

Two invariants are load-bearing and additive to the one-core/multi-adapter spine.

MCP-everywhere with full parity. Every agent-facing, server-backed operation
reachable through the CLI is also reachable through MCP, and vice versa,
because both adapters translate into the same core commands. The CLI is the
primary, canonical surface; MCP is not a reduced subset of the backend domain.
There is no host-local messaging service exception. New server-backed verbs
land on both surfaces together (for example `message complete` and
`witself.message.complete` landed in the same pass).

Agents run no HTTP servers for normal I/O. Agents are outbound clients only:
they reach a backend by calling out to it (local adapter, managed cell, or
self-hosted cell) and they discover inbound work by polling/long-polling, never by
hosting an inbound listener. The only HTTP server in the system is the backend —
`witself-server`, including the collaboration relay it exposes. Witself does not
provide a model-wake webhook, and a directed agent (for example Claude Code or
Codex driven over MCP) never needs an inbound endpoint. The durable mailbox is
the source of truth, so offline recipients are the default: a send never requires
the recipient to be online, and pending work remains unacknowledged until the
next foreground client turn.

The CLI command is `ws`; the backend binary is `witself-server`. The `witself://`
reference scheme, `WITSELF_` environment variables, and the `witself.*` MCP tool
names are unchanged.

## Core Boundary

Core behavior should not live in CLI handlers or HTTP handlers. It should live
in shared services that own:

- Realm, agent, token, and audit behavior.
- Memory behavior: add, adjust, read, recall, list, forget, restore, delete, and
  versioned edit history.
- Fact behavior: set, get, list, delete, and atomic primary promotion.
- Deterministic recall: PostgreSQL full-text and structured ranking, plus
  optional similarity math over compatible client-supplied vectors (see the
  [Vector Boundary](#vector-boundary)).
- The cross-agent policy engine and security-group evaluation (see the
  [Authorization Boundary](#authorization-boundary)).
- Inter-agent messaging: send, reply, listen, deliver, list, read, ack,
  processing claim/renew/release, and atomic result completion (see the
  [Messaging Boundary](#messaging-boundary)).
- Identity export and import.
- Redaction rules for `sensitive` memories and facts.
- Mutation and dry-run planning.
- JSON contract mapping where practical.

Adapters should translate transport-specific input into core commands and then
render core results back to CLI, MCP, or HTTP responses. The actor on every
command is the token-bound agent or operator resolved by `internal/auth`, never
a caller-supplied agent name; see the [Authorization Boundary](#authorization-boundary).

## Storage Boundary

Storage should be behind interfaces that are shared by managed, self-hosted, and
local development deployments.

Required storage responsibilities:

- Realm metadata.
- Agent metadata.
- Token hashes and token metadata.
- Memory records, including content, kind, tags, source, salience, links,
  `sensitive` markers, timestamps, and versioned edit history.
- Optional version/profile-keyed memory vectors supplied by clients.
- Fact records, including name, value, `primary` flag, `sensitive` flag, format
  hint, source, timestamps, and edit history.
- Security groups and group membership.
- Policy objects.
- Messages, migration-0035 backend-derived causal depth, mailbox/queue state,
  and per-recipient delivery/read/ack plus migration-0034 fenced processing and
  migration-0036 deterministic failure state.
- Audit events.
- Usage counters and rate-limit state.
- Idempotency records.

The production storage adapter starts with PostgreSQL as the system of record.
Full-text search is universal; migration `0032` stores optional client-supplied
vector profiles and arrays in ordinary JSONB without an extension. Memory content and
fact values are ordinary identity data living in normal columns under data-at-rest protection;
there is no requirement to keep them out of the database (unlike Witpass secrets).
Large exports, diagnostic bundles, support attachments, and backup artifacts may
use an object/blob adapter when needed.

Local development should use a local PostgreSQL instance and the same
migrations. A local transcript outbox may buffer delivery, but it is not a
second live memory source. The storage and migration model is tracked in
[storage.md](storage.md).

## Data-At-Rest Note

The two storage planes keep distinct, explicit postures:

- Open-plane memories and facts use ordinary PostgreSQL columns under managed
  disk/database encryption. Their `sensitive` flag controls disclosure and
  redaction; it is not an encrypted-blob or reveal boundary.
- When the sealed secret/TOTP plane is enabled, KMS-backed envelope encryption,
  key rotation, reveal gates, and audit are required. KMS availability gates
  sealed value operations but never open-plane memory capture or recall.
- Model inference and sealed-plane cryptography are unrelated boundaries. The
  server may legitimately hold KMS authority for sealed values while holding no
  LLM or embedding-model credentials.

Storage and key-custody details live in [storage.md](storage.md),
[encryption-model.md](encryption-model.md), and
[key-hierarchy.md](key-hierarchy.md).

## Vector Boundary

PostgreSQL full-text search, structured time/metadata filters, salience, and
recency form the universal recall path. No model or vector provider is required
for capture, storage, recall, export/import, or recovery.

Optional vectors are a derived client-authored index:

- An authorized client selects its own model and submits both memory and query
  vectors under an immutable profile describing model/recipe, dimensions,
  distance metric, and normalization.
- The backend validates authorization, finite values, profile compatibility,
  dimensions, version, and content hash. It stores canonical JSONB vectors in
  PostgreSQL and performs bounded deterministic similarity math; it never generates,
  interprets, or repairs a vector.
- Missing, stale, or incompatible vectors fall back to the fully functional
  full-text path. Capabilities and recall receipts report profile availability
  and coverage, not a server-side provider health state.
- Regeneration after a profile change is a client responsibility. The server
  may rebuild ordinary PostgreSQL indexes, but it never runs re-embedding.

Consequently `witself-server` has no embedding-provider configuration, API key,
model secret, outbound model egress, provider health probe, or inference cost.
The backend must never put vectors, memory content, or fact values in logs,
audit metadata, analytics values, or support-ticket content. The complete
contract lives in
[narrative-memory-and-curation.md](narrative-memory-and-curation.md).

## Authorization Boundary

Authorization must sit below every frontend:

- CLI local mode.
- CLI remote mode.
- MCP tools.
- Managed API handlers.
- Self-hosted API handlers.

The same action should have the same authorization result regardless of whether
it arrives through CLI, MCP, or HTTP. The authorization model and scopes are
specified in [access-policy.md](access-policy.md) and the requirements'
authorization decisions.

Identity from the token:

- Agent identity comes from the authenticated token, never from a caller-supplied
  agent name. `--agent`/`--owner-agent` are operator/admin targeting flags, not
  proof of identity. This is load-bearing for cross-agent access and messaging:
  the actor and the message sender are derived server-side from the token.
- V0 agent tokens are durable bearer tokens bound server-side to one realm and
  one named agent. Raw token values are returned only once during create or
  rotate; server-side storage keeps token hashes and metadata only. Disabled
  agents cannot authenticate, and agent deletion invalidates that agent's tokens.
  The token lifecycle is tracked in [token-lifecycle.md](token-lifecycle.md), and
  operator auth in [operator-auth.md](operator-auth.md).

Declarative cross-agent policy engine:

- An agent always retains full access to its own memories and facts, subject to
  its own token scopes. Cross-agent access is **default deny**: with no matching
  `allow` policy, it is denied.
- A Policy object (`pol_…`) binds a `subject` (agent or security group) ×
  `permission` × `target` (agent or security group), scoped to memories, facts,
  or both, with optional filters by memory kind/tag, fact name/namespace, or
  `sensitive` flag. Permission verbs escalate in danger: `read`, `contribute`,
  `curate`, `forget`.
- The engine resolves the caller's effective policies (including those held via
  group membership), evaluates the requested verb/scope against the target, and
  returns an allow with the deciding policy id or a deterministic deny reason.
  `policy test` exposes this evaluation as the canonical dry-run for access
  decisions via CLI and MCP.
- `curate` and `forget` across agents require an audit `--reason`, support
  `--dry-run`, and require confirmation unless `--yes`. Cross-agent deletes are
  soft/tombstoned by default and reversible within the retention window; hard
  cross-agent delete is a further-guarded step. Every cross-agent mutation is
  fully attributed in audit.
- Operators can always manage and access identity data within their realm
  (operator override), audited like any agent action and subject to the same
  `--reason` requirements on destructive/cross-agent actions.

Security groups:

- A security group (`grp_…`) is a named set of agents within a realm and is both
  a policy subject and a policy target. Membership is managed by operators and by
  agents holding `group:manage`. Groups may own group-scoped shared memories and
  facts, evaluated through the same policy engine. The group model is tracked in
  [security-groups.md](security-groups.md).

The threat model flips from confidentiality (Witpass) to **integrity and
authenticity** of identity data; headline risks are memory-poisoning,
unauthorized curation or forgetting, cross-agent write abuse, and sender
spoofing. See [threat-model.md](threat-model.md).

## Messaging Boundary

Inter-agent messaging is fully in scope for v0 and lives across the core
`internal/store`, `internal/server`, and `internal/client` boundaries. Active
runtime clients handle inference through hooks and MCP guidance. The messaging model is tracked in
[inter-agent-messaging.md](inter-agent-messaging.md).

- Durable mailbox/queue per recipient. Messages (`msg_…`) survive process and
  pod churn because mailbox and per-recipient delivery/read/ack state are
  persisted through the storage boundary.
- Direct autonomous processing uses the same unique recipient delivery row.
  Migration `0034` adds `available`/`claimed`/`completed` state, a monotonic
  generation, opaque claim id and retry-key hash, database-time lease,
  completion-key hash/time, and a unique result-message link. It does not
  overload read or acknowledgement; migration `0038` adds the separate
  multi-assignee request-claim model below.
- Migration `0035` adds backend-derived causal depth to each direct message.
  Direct sends start at one and replies/results advance exactly one from their
  locked durable parent; callers cannot set or reset the value.
- Migration `0036` adds the independent, durable `failure_count` to each direct
  delivery. Only release of the exact fence with `deterministic_failure=true`
  increments it; provider-wide, configuration, cancellation, timeout, and
  lease-maintenance release paths do not.
- Migration `0037` lets one message target a direct agent, a bounded explicit
  agent set, or the authenticated realm. One immutable message header owns the
  send-time audience fingerprint; recipient delivery rows remain the
  authoritative snapshot, and realm fanout excludes the sender.
- Migration `0038` adds message-backed realm open requests, their immutable
  candidate snapshot, append-only client-ranked selections, and bounded
  reservation/claim fences. PostgreSQL owns validation, capacity, deadlines,
  leases, and result linkage only; clients author offers and perform ranking.
- Claim, renew, and release require the token-bound recipient and exact live
  fence. Completion validates that unexpired fence, creates a parent-derived
  result reply, links it, and marks processing complete in one transaction;
  acknowledgement remains a later recovery boundary.
- Processing generation is only the stale-writer fence. Exact live-claim replay
  keeps it; a takeover after release or expiry advances it without consuming
  `failure_count`. Installed policy directs foreground clients to use that count
  for retry/escalation; the backend does not impose a fifth-attempt threshold.
- Delivery is at-least-once with per-recipient (and per-conversation) ordering
  and explicit read/acknowledgement state. Explicit-list and realm audiences
  are atomically resolved into per-member delivery rows. Group fanout remains a
  follow-on policy slice.
- `from` is **always derived from the authenticated token, never from input**, so
  sender forgery is structurally impossible through the API. Send/deliver/read
  events are audited; granular `message:send` and `message:read` policy scopes
  remain target platform integration.
- Ordinary sends normalize omitted kind to actionable `request` on every
  frontend/core path. Explicit `note` is FYI-only and may be read and
  acknowledged without treating it as work. The separate open-request
  state machine uses ordinary `open_request`, `offer`, and `result` messages for
  content while keeping coordination state authoritative and model-free.
- Message bodies and payloads are **untrusted input** to the receiving agent. A
  message can carry data toward a memory or fact write, but it cannot itself
  authorize a cross-agent write — writes still require a matching policy through
  the [Authorization Boundary](#authorization-boundary).
- Plan-backed send/delivery rate limits and metered dimensions are target
  platform integration; they are not implied by the current size, fan-out, or
  listen-admission bounds. See [billing-and-limits.md](billing-and-limits.md).
- Foreground clients are the inference boundary. At active task startup the
  installed policy directs them to inspect `self.show.message_checkpoint`, make
  a zero-wait `message.listen`, and scan open-request roles. It also directs
  candidates to offer/decline, the exact coordinator to rank durable offers, and
  selected agents to execute fenced claims. Supported Codex and Claude Code
  hooks automatically attempt content-free checkpoint injection and fail open;
  Cursor and Grok Build use guided MCP calls. Every runtime's policy
  directs it to obtain unread metadata through listen, but cannot force model
  compliance. No daemon, provider child, or captured provider credential
  participates.
- Surfaces: CLI `message send|reply|list|listen|read|ack|claim|renew|release|complete`,
  `message request open|list|show|offer|decline|select|cancel|claim|renew|release|complete`.
  MCP mirrors those ordinary and request operations. HTTP uses `/v1/messages`,
  `/v1/messages:listen`, the recipient actions through `:complete`, and
  `/v1/message-requests` with its eight action routes.
- Terminal and non-actionable deliveries remain canonical and unacknowledged
  until an active client reads and handles them. PostgreSQL is the sole message
  and handoff store.
- Account export preserves causal depth, fanout audience fingerprints and
  delivery snapshots, completed processing, result links, deterministic
  `failure_count`, and the full request candidate/selection/claim graph. Import
  validates or derives causal depth, validates fanout/request graph integrity,
  interrupts an active direct claim by advancing its generation, and cancels
  and fences active request reservations/claims before the destination account
  resumes. Archives older than schema 36 upgrade direct `failure_count` to zero.
  Canonical PostgreSQL messages are exported; there is no derived host-local
  message state.

## Audit Boundary

Audit events should be emitted by the core service or by a tightly controlled
audit layer immediately below it. Transport adapters should not be responsible
for remembering which sensitive actions need audit events.

Audit events must stay redacted. They can include record ids, owner agent/group,
realm id, memory kinds and tags, fact names, policy ids, message ids, recipient,
reason strings, outcome, actor, target, and timestamps. They must not include
memory content, fact values, message bodies or payloads, embedding vectors, raw
tokens, or raw payment details.

Cross-agent and operator-override mutations are attributed to the acting agent
and the deciding policy (for example "memory `mem_…` of agent A was pruned by
agent B under policy `pol_…`"). Stable event names and the 365-day default
retention are tracked in [audit-retention.md](audit-retention.md).

## Observability Boundary

Observability should be a shared server boundary, not ad hoc handler code.
`witself-server` should expose Prometheus-compatible metrics, Kubernetes
liveness/readiness/startup probes, and structured logs with request IDs. The
API, health, and metrics surfaces should bind to separate listeners by default
(`:8080`, `:8081`, `:9090`) so operators can expose and restrict each one
independently. Metrics enablement is controlled by `WITSELF_METRICS_ENABLED`.

Metrics and logs must follow the same redaction posture as audit events and API
errors. They must not include memory content, fact values, message bodies or
payloads, embedding vectors, raw tokens, raw payment details, wallet credentials,
database URLs, provider credentials, raw request paths, query strings, request
bodies, or arbitrary user input.

Metrics should cover HTTP traffic, authentication, token operations, memory
operations, deterministic recall, optional vector validation/search, curation
queues/plans, fact operations, policy decisions
(allow/deny), cross-agent accesses, group operations, message send/deliver/read,
audit events, usage metering, limit decisions, storage, vector storage,
migrations, and runtime health. Metric labels should be low cardinality and use
route templates rather than raw paths. The observability model is tracked in
[observability-and-operations.md](observability-and-operations.md).

## Capability Discovery

Managed, self-hosted, and local development backends must expose a capabilities
contract through `/v1/capabilities` so the CLI can show which features are
supported, which are unavailable, and why, before running an operation.

- The contract reports the deployment mode, full-text recall availability,
  optional vector-profile support and coverage, curation capabilities, and
  which managed-service surfaces (billing, payment, support, operator
  administration) are available. It never reports a backend embedding provider
  because none exists.
- Unsupported commands fail predictably with `unsupported_operation` and
  capability context, not with vague provider, route, or config errors.
- Remote setup calls capability discovery before attempting account, realm,
  agent, token, billing, payment, or support operations.

## Deployment Cells And Control Plane

The go-forward multi-cloud shape is a fleet of independent cells coordinated by a
thin global control plane. This is additive to the one-core/multi-adapter spine:
a cell runs the same `witself-server` and the same core service described above.
The full model is tracked in [deployment-cells.md](deployment-cells.md).

A cell is one complete, independent Witself stack — `witself-server` plus its
PostgreSQL, optional sealed-plane KMS, and blob storage —
running in a single cloud
account/region (an AWS account, a second AWS account, a GCP project, an Azure
subscription). Cells are isolated and each cell is authoritative for its own
tenants; a cell outage affects only that cell's tenants, which contains blast
radius. Cells may run different software versions during a canary or wave rollout,
and capability discovery (`/v1/capabilities`) lets clients adapt — version skew
across the fleet is a strength of the cell model, not a defect to engineer away.

The control plane is a thin, highly available, globally replicated service that
holds only routing metadata — the mapping from a realm/account handle to its home
cell endpoint and signing key. It has two jobs:

- Placement: at account/realm creation it picks a home cell by data-residency,
  capacity, provider preference, or rollout wave, and records the mapping.
- Resolution: clients resolve "where is my home cell" before talking to a cell.
  This extends the existing `--endpoint`/token model from "talk to this endpoint"
  to "resolve my home cell, then talk to it."

The control plane holds no tenant data — no memories, facts, messages, tokens, or
audit. It is the one new always-on global component, kept deliberately thin so its
own blast radius is tiny. This is a fleet of independent cells, each authoritative
for its own tenants; there is no shared-data multi-master across clouds in v1 (a
harder, deferred problem).

Tenant migration moves a realm/account between cells by exporting from cell A,
importing into cell B, repointing the control-plane mapping, and cutting over.
The open plane (memories/facts) moves through the existing first-class
export/import path (see [Core Boundary](#core-boundary)); the sealed plane, where
an operator has enabled per-cell KMS-rooted field encryption, re-wraps keys under
the destination KMS as an audited decrypt-at-source / re-encrypt-at-dest step.
Migration is bounded but not free; details and the cutover trade-off live in
[deployment-cells.md](deployment-cells.md).

### Open decisions

These forks are deliberately left open; see
[deployment-cells.md](deployment-cells.md).

- Placement unit: account vs realm as the unit of placement and migration.
- Self-host: whether a self-host deployment is single-cell only or may run as a
  multi-cell fleet.
- Migration cutover: brief read-only freeze vs dual-write + reconcile.

## Collaboration Subsystem

Cross-realm agent collaboration is the first post-v0 epic. It extends the
realm-local [Messaging Boundary](#messaging-boundary) — it does not replace it —
and is sequenced after the realm-local core is built. The full design is in
[agent-collaboration.md](agent-collaboration.md); the realm-local authority stays
[inter-agent-messaging.md](inter-agent-messaging.md).

The cross-realm relay is a blind carry. The relay (hosted by `witself-server`, the
only HTTP server in the system — see [Interface Invariant](#interface-invariant))
routes end-to-end-signed envelopes by realm handle. It carries the signed envelope
without reading its body or payload and without the ability to forge or alter it.
Routing identity (the token-derived sender) stays for in-realm anti-spoofing; a
cross-realm signature over the envelope, verified against the sender realm's
published JWKS, is what establishes trust. Content stays untrusted and carries no
authority across a realm boundary — a cross-realm message can never author a write
without a standing allow policy in the receiving realm, exactly as in the
realm-local messaging and [Authorization Boundary](#authorization-boundary) rules.

The relay shares the control plane's global directory. The relay needs
realm-handle → where-it-lives → signing-key resolution. That is the same routing
registry the control plane maintains for cells. Cells and collaboration share one
global directory: placement/resolution metadata and realm signing keys are the
same lookup, so a realm is reachable for collaboration by the same handle that
resolves its home cell.

Cross-realm delivery follows the same invariants as realm-local messaging: the
durable mailbox is the source of truth, offline recipients are the default, and
agents drain inbound work by calling the `listen`/`recv` verb rather than hosting
a listener. Federation is deny-by-default — a realm allow-lists the realm handles
and keys it accepts, and first contact is quarantined pending consent. Loop and
spend safety caps are enforced on the wire; those defaults live in
[agent-collaboration.md](agent-collaboration.md).

### Open decisions

These forks are deliberately left open; see
[agent-collaboration.md](agent-collaboration.md).

- Identity root: per-realm signing key for v1 vs per-agent keypair now.
- Self-host federation: cloud-relay-first vs peer-to-peer.
- Auto-reply default across a trust boundary (off-by-default + budgeted opt-in is
  the recommendation).
- A2A interop: native A2A at the boundary vs Witself-native plus an A2A gateway.

## Backend Endpoint Set

The public backend API uses an explicit versioned contract with a `/v1` base, the
shared response envelope `{schema_version, ok, data, warnings}`, plural resources,
and colon-action subroutes using `POST` for sensitive or workflow operations.
Bootstrap and platform endpoints include:

- `/v1/version`
- `/livez`, `/readyz`, `/startupz` (plus `/healthz` alias)
- `/metrics`
- `/v1/whoami`
- `/v1/capabilities`

Identity and platform resource routes include:

- `/v1/memories` and actions `/v1/memories:recall`,
  `/v1/memories/{memory_id}:forget`, `/v1/memories/{memory_id}:restore`.
- `/v1/facts` and action `/v1/facts/{fact_id}:primary`.
- `/v1/policies` and action `/v1/policies:test`.
- `/v1/groups` (membership management).
- `/v1/messages`, `/v1/messages:listen`, and message actions `:reply`, `:read`,
  `:ack`, `:claim`, `:renew`, `:release`, and `:complete`.
- `/v1/tokens` and action `/v1/tokens/{token_id}:rotate`.

Mutating routes support `Idempotency-Key` and `dry_run` where practical, and list
routes support cursor pagination. Memory content, fact values, message
bodies/payloads, raw tokens, and provider secrets must never appear in URL paths
or query strings. The full contract is tracked in
[api-contract.md](api-contract.md) and the route style in
[api-routes.md](api-routes.md); shared JSON shapes are in
[json-contracts.md](json-contracts.md).

## First Implementation Path

A pragmatic first backend path:

1. Define core domain interfaces and JSON contract structs.
2. Implement PostgreSQL storage and migrations as the sole authoritative data
   path, including universal full-text indexes.
3. Build CLI and MCP commands against the same service contract.
4. Add a minimal `witself-server serve --dev` path using local PostgreSQL.
5. Add `/v1/version`, `/livez`, `/readyz`,
   `/startupz`, `/metrics`, `/v1/whoami`, and `/v1/capabilities`.
6. Add HTTP API handlers over the same core service.
7. Add client-authored narrative capture, lifecycle, deterministic recall, and
   archive round trips.
8. Add optional client-vector profiles, portable JSONB rows, and bounded hybrid
   ranking after the full-text path is production-ready. (Implemented in
   migration `0032`; pgvector/ANN remains only a future accelerator.)
9. Add Goose migrations, server config validation, health checks, metrics, and
   container image.
10. Add the `charts/witself-server` Helm chart as the first self-hosting artifact.
11. Add Terraform modules and stacks under `infra/terraform` for AWS, GCP, and
    Azure substrate provisioning.
12. Wire the managed cloud and self-hosted deployments to the production adapter.

This lets the local mock move upstream without pretending it is production. The
full sequence is tracked in [implementation-plan.md](implementation-plan.md).

## Non-Goals

- Do not create a separate private backend implementation for the managed
  service.
- Do not maintain different domain logic for managed and self-hosted
  deployments.
- Do not let local development define a second storage or inference path.
- Do not add backend model inference, embedding-provider credentials, or model
  egress. Client inference is the only semantic-authoring boundary.
- Do not make sealed-plane KMS or reveal availability a dependency of the
  open-plane memory path.
- Do not require a web dashboard for ordinary account, realm, billing, support,
  or agent administration.

## Related Docs

- [requirements.md](requirements.md)
- [v0-scope.md](v0-scope.md)
- [self-hosting.md](self-hosting.md)
- [helm-chart.md](helm-chart.md)
- [terraform-infrastructure.md](terraform-infrastructure.md)
- [cloud-targets.md](cloud-targets.md)
- [server-command-surface.md](server-command-surface.md)
- [api-contract.md](api-contract.md)
- [api-routes.md](api-routes.md)
- [json-contracts.md](json-contracts.md)
- [observability-and-operations.md](observability-and-operations.md)
- [storage.md](storage.md)
- [memory-model.md](memory-model.md)
- [facts-model.md](facts-model.md)
- [access-policy.md](access-policy.md)
- [security-groups.md](security-groups.md)
- [inter-agent-messaging.md](inter-agent-messaging.md)
- [agent-collaboration.md](agent-collaboration.md)
- [deployment-cells.md](deployment-cells.md)
- [token-lifecycle.md](token-lifecycle.md)
- [operator-auth.md](operator-auth.md)
- [audit-retention.md](audit-retention.md)
- [billing-and-limits.md](billing-and-limits.md)
- [threat-model.md](threat-model.md)
- [cli-command-surface.md](cli-command-surface.md)
- [mcp-tools.md](mcp-tools.md)
- [implementation-plan.md](implementation-plan.md)
