# Witself Backend Architecture

Status: draft. This document captures the public backend and self-hosting
architecture direction before implementation. Last reviewed 2026-06-26.

## Decision

Witself should keep the backend API server in the public `witself` repository.
The code that stores, embeds, recalls, authorizes, audits, and serves identity
material should be inspectable alongside the CLI and MCP adapter.

The managed Witself Cloud service is the default commercial deployment, but the
same backend code should be usable by operators who want to self-host Witself in
their own cloud, network, or compliance boundary.

The backend API server should ship as a separate `witself-server` binary, not as
a public `witself server` subcommand. Keeping the server process separate makes
production and self-hosted deployment packaging, container entrypoints, service
permissions, and operational docs clearer while leaving `witself` focused on
human, agent, and MCP workflows.

## Deployment Modes

| Mode | Purpose | Storage | Production posture |
|---|---|---|---|
| Managed Witself Cloud | Default paid service operated by Witwave | Cloud production adapters (Postgres + pgvector) | Production |
| Self-hosted Witself Server | Customer-operated backend in their own cloud | Production self-host adapters (Postgres + pgvector) | Production once supported |
| Local development backend | Tests, demos, offline prototyping, early CLI work | Local file adapter with the `local-dev` embedder | Development only |

The local backend should be a real adapter behind the same service boundary, but
it is not the production model. It uses the `local-dev` embedding provider so
semantic recall can be exercised offline; see
[memory-model.md](memory-model.md).

## Repository Shape

Initial target layout once code starts:

```text
cmd/witself/                  # CLI, including mcp serve
cmd/witself-server/           # Backend API server
internal/core/                # Domain service and use cases
internal/api/                 # HTTP handlers and request/response adapters
internal/auth/                # Token validation and principal resolution
internal/policy/              # Cross-agent policy engine and security groups
internal/messaging/           # Mailbox/queue, delivery, ordering, ack state
internal/embeddings/          # Embedding-provider abstraction and recall ranking
internal/audit/               # Audit event generation and sinks
internal/observability/       # Metrics, logs, request IDs, and health probes
internal/store/               # Storage interfaces
internal/store/local/         # Local development file adapter
internal/store/postgres/      # Production relational + pgvector adapter
internal/store/blob/          # Object/blob storage adapter when needed
internal/server/              # Server config, lifecycle, health, migrations
images/witself/               # CLI/MCP container image
images/witself-server/        # Backend API server container image
charts/witself/               # Self-hosted Kubernetes Helm chart
infra/terraform/              # AWS/GCP/Azure infrastructure modules and stacks
docs/backend-architecture.md
docs/self-hosting.md
docs/helm-chart.md
```

This layout is a starting point, not a promise that every package name is final.
The important boundary is one core domain service with multiple adapters.

## Process Boundaries

Witself should have these public entrypoints:

- `witself`: human and agent CLI.
- `witself mcp serve`: local-first MCP adapter over the same core behavior.
- `witself-server`: separate backend API server for managed and self-hosted
  deployments.

The CLI can operate in two broad ways:

- Local mode: use the local development adapter directly.
- Remote mode: call a managed or self-hosted `witself-server` endpoint through
  the same public API contract.

Remote mode should use the versioned HTTP API contract described in
[api-contract.md](api-contract.md), with the route style in
[api-routes.md](api-routes.md). Every backend should expose capability discovery
so clients can distinguish managed, self-hosted, and local behavior — including
the active embedding provider and whether semantic recall is degraded — without
guessing.

## Core Boundary

Core behavior should not live in CLI handlers or HTTP handlers. It should live
in shared services that own:

- Realm, agent, token, and audit behavior.
- Memory behavior: add, adjust, read, recall, list, forget, restore, delete, and
  versioned edit history.
- Fact behavior: set, get, list, delete, and atomic primary promotion.
- Semantic recall: embedding generation and hybrid ranking (see the
  [Embeddings Boundary](#embeddings-boundary)).
- The cross-agent policy engine and security-group evaluation (see the
  [Authorization Boundary](#authorization-boundary)).
- Inter-agent messaging: send, deliver, list, read, and ack (see the
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
- Memory embedding vectors.
- Fact records, including name, value, `primary` flag, `sensitive` flag, format
  hint, source, timestamps, and edit history.
- Security groups and group membership.
- Policy objects.
- Messages, mailbox/queue state, and per-recipient delivery/read/ack state.
- Audit events.
- Usage counters and rate-limit state.
- Idempotency records.

The production storage adapter starts with Postgres as the system of record, with
the pgvector extension for embedding vectors. Memory content and fact values are
ordinary identity data living in normal columns under data-at-rest protection;
there is no requirement to keep them out of the database (unlike Witpass secrets).
Large exports, diagnostic bundles, support attachments, and backup artifacts may
use an object/blob adapter when needed.

The local development adapter should use a serialized file store and exercise the
same storage interface, including the vector path through the `local-dev`
embedder. It should not become a separate data model. The storage and migration
model is tracked in [storage.md](storage.md).

## Data-At-Rest Note

Witself does not treat encryption as a product pillar. It stores identity data,
protected for integrity and authenticity, not secret material.

- Use ordinary data-at-rest encryption (managed RDS/disk, or self-host-owned disk
  encryption). There is no KMS/envelope/client-side-decrypt pillar, no reveal
  ceremony, and no end-to-end secret model.
- KMS is **optional and demoted**, not a core dependency. Storage decisions live
  in [storage.md](storage.md).
- Optional field-level encryption for `sensitive` fact values is a capability an
  operator can enable, not the default behavior.
- `sensitive` is a PII/redaction display flag, not an encryption boundary. There
  is no value-size split between sensitive and non-sensitive values.

The authorization and audit scaffolding once associated with a Witpass
encryption model is reused by the policy engine in
[access-policy.md](access-policy.md).

## Embeddings Boundary

Semantic recall is the core Witself differentiator, so the backend treats the
embedding provider as a first-class, capability-gated boundary (mirroring the way
Witpass abstracted KMS). The recall and embedding model is tracked in
[memory-model.md](memory-model.md).

Provider abstraction:

- Providers are `voyage` (default; the Anthropic-recommended embedding family),
  `openai`, and `local-dev`.
- Provider and model are selectable via `WITSELF_EMBEDDINGS_PROVIDER` and
  `WITSELF_EMBEDDINGS_MODEL`, behind one interface in `internal/embeddings`.
- `local-dev` is a deterministic or low-cost local embedder for tests, demos, and
  `witself-server serve --dev`. It is not a production provider.

Storage and recall:

- Each memory carries an embedding vector computed at write time. Vectors are
  stored in Postgres via pgvector. Vector storage size is a metered dimension; see
  [billing-and-limits.md](billing-and-limits.md).
- `memory recall` performs vector similarity search blended with keyword, tag,
  kind, time, and salience signals. Plain `read`/`get` by id and `list` by
  metadata never require the provider.

Degradation and capability reporting:

- If the provider is unavailable or disabled, recall degrades deterministically
  to keyword/tag/kind/time ranking, and the capabilities contract reports that
  semantic recall is degraded, plus the active provider, model, and vector
  dimensionality. Recall never silently returns unranked or empty results without
  surfacing the degraded state.
- Re-embedding on a provider/model change is an explicit, audited maintenance
  operation run through `witself-server`, not an automatic side effect; see
  [server-command-surface.md](server-command-surface.md).

The backend must never put embedding vectors, memory content, or fact values in
logs, audit metadata, analytics values, or support-ticket content.

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

Inter-agent messaging is fully in scope for v0 and lives in the core service
behind `internal/messaging`. The messaging model is tracked in
[inter-agent-messaging.md](inter-agent-messaging.md).

- Durable mailbox/queue per recipient. Messages (`msg_…`) survive process and
  pod churn because mailbox and per-recipient delivery/read/ack state are
  persisted through the storage boundary.
- Delivery is at-least-once with per-recipient (and per-conversation) ordering
  and explicit read/acknowledgement state. A message addressed to a group is
  fanned out to current group members with per-member delivery and ack state.
- `from` is **always derived from the authenticated token, never from input**, so
  sender forgery is structurally impossible through the API. `message:send` and
  `message:read` scopes gate the surface, and send/deliver/read events are
  audited.
- Message bodies and payloads are **untrusted input** to the receiving agent. A
  message can carry data toward a memory or fact write, but it cannot itself
  authorize a cross-agent write — writes still require a matching policy through
  the [Authorization Boundary](#authorization-boundary).
- Rate limits apply to send and delivery. Messages sent and delivered are metered
  dimensions; see [billing-and-limits.md](billing-and-limits.md).
- Surfaces: CLI `message` group, MCP `witself.message.send/list/read`, and
  `/v1/messages` plus colon actions such as `/v1/messages/{message_id}:ack`.

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
operations, recall and embedding operations, fact operations, policy decisions
(allow/deny), cross-agent accesses, group operations, message send/deliver/read,
audit events, usage metering, limit decisions, storage, vector storage,
migrations, and runtime health. Metric labels should be low cardinality and use
route templates rather than raw paths. The observability model is tracked in
[observability-and-operations.md](observability-and-operations.md).

## Capability Discovery

Managed, self-hosted, and local development backends must expose a capabilities
contract through `/v1/capabilities` so the CLI can show which features are
supported, which are unavailable, and why, before running an operation.

- The contract reports the deployment mode, the active embedding provider and
  model, vector dimensionality, whether semantic recall is degraded, and which
  managed-service surfaces (billing, payment, support, operator administration)
  are available.
- Unsupported commands fail predictably with `unsupported_operation` and
  capability context, not with vague provider, route, or config errors.
- Remote setup calls capability discovery before attempting account, realm,
  agent, token, billing, payment, or support operations.

## Backend Endpoint Set

The public backend API uses an explicit versioned contract with a `/v1` base, the
shared response envelope `{schema_version, ok, data, warnings}`, plural resources,
and colon-action subroutes using `POST` for sensitive or workflow operations.
Bootstrap and platform endpoints include:

- `/v1/version`
- `/v1/health/live`, `/v1/health/ready`, `/v1/health/startup`
- `/metrics`
- `/v1/whoami`
- `/v1/capabilities`

Identity and platform resource routes include:

- `/v1/memories` and actions `/v1/memories:recall`,
  `/v1/memories/{memory_id}:forget`, `/v1/memories/{memory_id}:restore`.
- `/v1/facts` and action `/v1/facts/{fact_id}:primary`.
- `/v1/policies` and action `/v1/policies:test`.
- `/v1/groups` (membership management).
- `/v1/messages` and action `/v1/messages/{message_id}:ack`.
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
2. Implement the local file storage adapter, including the vector path through the
   `local-dev` embedder.
3. Build CLI commands against the core service and local adapter.
4. Add a minimal `witself-server serve --dev` path using the local adapter.
5. Add `/v1/version`, `/v1/health/live`, `/v1/health/ready`,
   `/v1/health/startup`, `/metrics`, `/v1/whoami`, and `/v1/capabilities`.
6. Add HTTP API handlers over the same core service.
7. Add Postgres-backed production storage with pgvector for embeddings.
8. Wire the embedding-provider abstraction with `voyage` as default.
9. Add Goose migrations, server config validation, health checks, metrics, and
   container image.
10. Add the `charts/witself` Helm chart as the first self-hosting artifact.
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
- Do not let the local development backend define production behavior or the
  production embedding path.
- Do not treat encryption, KMS, or a reveal ceremony as core backend pillars.
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
- [token-lifecycle.md](token-lifecycle.md)
- [operator-auth.md](operator-auth.md)
- [audit-retention.md](audit-retention.md)
- [billing-and-limits.md](billing-and-limits.md)
- [threat-model.md](threat-model.md)
- [cli-command-surface.md](cli-command-surface.md)
- [mcp-tools.md](mcp-tools.md)
- [implementation-plan.md](implementation-plan.md)
