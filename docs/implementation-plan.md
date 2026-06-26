# Witself Implementation Plan

Status: draft. This document describes a practical build order for a repo that
contains the CLI, MCP adapter, backend API server, images, Helm chart, Terraform
substrate, release automation, and docs.

Witself reuses the Witpass platform spine and swaps the secret payload for
self-identity. Where Witpass sequenced the build around secret/TOTP/password/run
flows and a KMS storage boundary, Witself sequences around the memory, fact,
policy, security-group, and messaging domains, plus an embeddings/pgvector
semantic-recall path. The platform milestones (core-behind-adapters, API
skeleton, storage boundary, images, Helm, Terraform, managed slice, hardening)
are otherwise the same shape.

## Guiding Sequence

Witself should start small, but the first implementation slice should exercise
the real product boundaries:

- One Go module.
- One shared core service.
- CLI and MCP adapters over that core.
- A separate `witself-server` API process over that core.
- A local development adapter (file-backed, `local-dev` embedding provider).
- A production storage boundary ready for Postgres with pgvector, an embedding
  provider, and object/blob storage.
- Public release, image, Helm, and Terraform scaffolding from the beginning.

The local adapter is useful scaffolding. It should not become a separate product
architecture.

## V0 Release Target

V0 should be a usable cloud-shaped slice rather than a purely local mock. The
target includes:

- Installable `witself` CLI.
- `witself-server serve --dev`.
- Local development adapter with the `local-dev` embedding provider.
- Agent token lifecycle.
- Memory, fact, recall, policy, security-group, messaging, reference,
  export/import, and audit flows.
- Semantic recall over pgvector with deterministic keyword/tag/time degradation.
- Prometheus metrics, structured logs, and Kubernetes health probes.
- MCP stdio tools.
- Postgres storage adapter with the pgvector extension.
- Public CLI/MCP and backend images.
- `charts/witself` Helm skeleton.
- AWS Terraform skeleton.
- Strong CI, linting, and release automation.

Inter-agent messaging is fully in scope for v0 (durable mailbox, delivery,
ordering, and acknowledgement), not a stub.

Billing, support, payment, crypto payment, and broader managed-account
operations can be present as contract shapes while remaining capability-gated.
They should return deterministic `unsupported_operation` responses until a
backend explicitly supports them.

The v0 scope is tracked in [v0-scope.md](v0-scope.md).

## Milestone 0: Public Repo Foundation

Goal: make the public repository safe to build in.

Deliverables:

- Scaffold according to [scaffold-readiness.md](scaffold-readiness.md).
- Root `README.md`.
- Root `SECURITY.md`.
- Root `CONTRIBUTING.md`.
- Root Apache-2.0 `LICENSE`.
- `.gitignore` covering local store files, token files, identity export bundles,
  Terraform state, cloud credentials, kubeconfigs, embedding-provider
  credentials, and local build output.
- Initial docs index under `docs/`.
- Initial GitHub Actions CI skeleton.
- Initial release workflow skeleton.
- Initial Go module:
  - Module path: `github.com/witwave-ai/witself`.
  - `go 1.26`.
  - `toolchain go1.26.4`, refreshed before implementation and release.

Exit criteria:

- Docs links are valid.
- CI can run on an empty or near-empty codebase.
- Root docs identify Apache-2.0 as the repo license and reserve Witself
  trademark/branding rights.

## Milestone 1: Core Contracts And Local Adapter

Goal: create the domain model and local development path without hard-coding the
CLI as the only frontend.

Deliverables:

- Shared Go structs for the JSON response envelope
  (`{schema_version, ok, data, warnings}`) and core resource shapes (realm,
  agent, memory, fact, policy, group, message, token, audit record).
- Operator auth abstractions for browser/device-code managed auth, self-hosted
  bootstrap token auth, and local development auth. In M1 the managed/remote
  paths are interface-only stubs (see exit criteria); only local development
  auth is functional until the API skeleton (M2) and managed slice (M9) land.
- Core service interfaces for realms, agents, tokens, memories, facts, recall,
  policies, groups, messages, references, export/import, and audit.
- Authorization checks below CLI, MCP, and API adapters, with a default-deny
  stance for cross-agent access and identity derived from the token, never from
  input fields.
- Redaction helpers shared by all frontends (PII-aware; memory content, fact
  values, message bodies/payloads, and embedding vectors never leak into
  errors/logs).
- File-backed local storage adapter behind the production storage interface.
- Local token hashing and token-file bootstrap (raw token shown once; `0600`
  files, `0700` dirs, atomic writes; refuse overwrite without reuse/rotation).
- Audit event generation with redacted payloads and stable dotted event names
  (`memory.added`, `fact.set`, `policy.access_denied`, `crossagent.curated`,
  `message.sent`, `identity.exported`, ...).
- Audit retention configuration with a managed/default Helm value of 365 days.
- Memory and fact size validation for v0 inline limits (content max, tag/link
  caps, fact name uniqueness per owner).
- Versioned edit history for memories and facts.

Early CLI commands:

- `witself version`
- `witself capabilities`
- `witself whoami`
- `witself setup --local`
- `witself auth login`
- `witself realm init`
- `witself agent create`
- `witself token create`
- `witself memory add`
- `witself memory adjust`
- `witself memory read`
- `witself memory list`
- `witself memory forget`
- `witself memory restore`
- `witself fact set`
- `witself fact get`
- `witself fact list`
- `witself reference parse`
- `witself reference resolve`
- `witself export`
- `witself import`
- `witself mcp tools`

Exit criteria:

- Local mode can create a realm, create a named agent, write a token file, add
  and adjust a memory, read it back by id, set a fact, promote a fact to
  primary, forget and restore a memory, and export then re-import an agent's
  self round-trippably.
- Managed or remote auth paths are stubbed behind the same interfaces used for
  browser/device-code login and self-hosted bootstrap token login.
- `sensitive` memory content and fact values are redacted from
  list/scan/errors/log-like output; an authorized single-record read returns
  the value (no reveal ceremony).
- Cross-agent access is denied with no matching policy.
- JSON output follows [json-contracts.md](json-contracts.md).

## Milestone 2: API Skeleton And Dev Server

Goal: prove the same core can run behind the public API contract.

Deliverables:

- `cmd/witself-server`.
- `witself-server version`.
- `witself-server serve --dev`.
- `witself-server config check`.
- `/v1/version`.
- `/v1/health/live`.
- `/v1/health/ready`.
- `/v1/health/startup`.
- `/metrics`.
- Separate API, health, and metrics listeners with default ports `8080`,
  `8081`, and `9090`.
- Metrics enable/disable configuration through config, environment variables,
  CLI flags, and Helm values, with `WITSELF_METRICS_ENABLED` as the canonical
  variable.
- `/v1/whoami`.
- `/v1/capabilities` reporting backend kind, active embedding provider, model,
  vector dimensionality, and whether semantic recall is degraded.
- Prometheus metric registration and label-normalization (route-template
  labels, never raw paths).
- Structured JSON logs with request IDs and redaction.
- First `/v1/memories`, `/v1/facts`, `/v1/agents`, and `/v1/tokens` routes over
  the local adapter.
- Resource-oriented routes with colon action subroutes for sensitive workflows
  (`/v1/memories/{memory_id}:forget`, `:restore`, `/v1/facts/{fact_id}:primary`,
  `/v1/tokens/{token_id}:rotate`), using `POST` for actions.
- Idempotency-Key and `dry_run` support on mutating routes.
- OpenAPI generation or an equivalent single-source schema path.
- CLI remote mode pointed at a dev server through `--endpoint` /
  `WITSELF_ENDPOINT`.

Exit criteria:

- A local dev server and the CLI produce equivalent behavior for the same core
  operations.
- `witself capabilities --endpoint http://127.0.0.1:8080` reports
  `backend.kind: "local"` and the active embedding provider.
- Health endpoints report live, ready, and startup status without exposing
  sensitive config.
- `/metrics` emits Prometheus text format without raw paths, user input, memory
  content, fact values, message bodies, embedding vectors, or tokens in labels.
- Health probes and metrics run on their dedicated listeners rather than the
  public API listener.
- Unsupported service-admin commands return deterministic
  `unsupported_operation`.

## Milestone 3: Embeddings And Semantic Recall

Goal: make semantic recall real — the core Witself differentiator — behind a
provider abstraction and stored in pgvector.

This milestone is the analogue of the Witpass KMS milestone: a capability-gated
provider boundary with a `local-dev` implementation, but for embeddings rather
than envelope encryption. There is no KMS pillar.

Deliverables:

- Embedding-provider abstraction with `voyage` (default), `openai`, and
  `local-dev` implementations, selectable via `WITSELF_EMBEDDINGS_PROVIDER` and
  `WITSELF_EMBEDDINGS_MODEL`.
- Embed-on-write: every `memory add`/`adjust` computes and persists an embedding
  vector from content (and optionally tags/kind).
- pgvector-backed vector column and similarity index (introduced with the
  storage adapter; see [storage.md](storage.md)).
- `witself memory recall <query>` and `/v1/memories:recall`
  (or `/v1/memories/{memory_id}:recall`).
- Hybrid ranking blending vector similarity, lexical/keyword match, tag/kind
  match, recency, and `salience`, with documented default weights.
- Deterministic degradation to keyword/tag/kind/time ranking when the embedding
  provider is unavailable or disabled, surfaced through the capabilities
  contract (recall never silently returns unranked/empty results).
- Capability reporting of active provider, model, and vector dimensionality.
- `local-dev` deterministic/low-cost embedder for tests, demos, and
  `witself-server serve --dev`.
- Metrics for recall and embedding operations and vector storage size.
- `MCP` `witself.memory.recall`.
- Explicit, audited re-embedding maintenance path (not an automatic side
  effect) reserved for provider/model change.

Exit criteria:

- `witself memory recall` returns relevance-ranked results from the caller's
  accessible memories against a real provider and against `local-dev`.
- With the provider disabled, recall degrades deterministically and the
  capabilities contract reports degraded semantic recall.
- Embedding vectors never appear in logs, metrics labels, audit records, or
  errors.
- The recall and embedding model matches [memory-model.md](memory-model.md).

## Milestone 4: Cross-Agent Policy Engine

Goal: replace ad-hoc grants with an evaluable, default-deny policy engine that
gates all cross-agent identity access.

Deliverables:

- Policy object shape (`pol_…`): subject (agent or group) × permission ×
  target (agent or group), scope (memories/facts/both), optional filter
  (kind/tag/name/sensitive), `allow` effect, metadata.
- Permission verbs in escalating danger order: `read`, `contribute`, `curate`,
  `forget`.
- Default-deny evaluation: absence of a matching `allow` denies; an agent always
  retains access to its own identity data subject to its own scopes.
- `witself policy create/list/show/delete` and `witself policy test`.
- `/v1/policies`, `/v1/policies:test`, and policy colon actions, with `POST` for
  actions.
- `witself.policy.test` (plus operator `witself.policy.list/show`).
- Cross-agent read, contribute, curate, and forget paths wired through the
  engine and fully attributed in audit ("memory `mem_…` of agent A was pruned by
  agent B under policy `pol_…`").
- Guardrails reused from Witpass patterns: `curate` and `forget` across agents
  require an audit `--reason`, support `--dry-run`, and require confirmation
  unless `--yes`; cross-agent deletes are soft/tombstoned and reversible by
  default; hard cross-agent delete is a further-guarded step.
- Operator override: operators manage/access identity data within their realm,
  audited like agent actions and subject to the same `--reason` requirements.
- `policy.access_denied` and `crossagent.*` audit events; allow/deny decision
  metrics.
- Reference resolution (`witself://agent/<agent>/...`,
  `witself://group/<group>/...`) enforces the same authorization as a direct
  read, at write time and at resolve time.

Exit criteria:

- `policy test` returns the deciding policy id or a deny reason for any
  subject/permission/target/scope tuple, via CLI and MCP.
- A cross-agent `recall`/`read` succeeds only with a matching `read` policy and
  is metered as a cross-agent access.
- Cross-agent `curate`/`forget` enforce `--reason`, `--dry-run`, and
  confirmation, and produce attributed audit records.
- The engine matches [access-policy.md](access-policy.md) and the threat framing
  in [threat-model.md](threat-model.md).

## Milestone 5: Security Groups

Goal: let agents be organized into named groups that act as policy subjects and
targets and that can own shared identity data.

Deliverables:

- Group object shape (`grp_…`): name (unique per realm), members, admins/owner,
  bound policies, description, timestamps.
- `witself group create/list/show/add-member/remove-member/delete`.
- `/v1/groups` plus member colon actions, with `POST` for actions.
- `witself.group.list/show`.
- Group-as-subject evaluation (a policy on a group grants every member the
  permission on the target) and group-as-target evaluation (a permission whose
  recipient is a group).
- Group-scoped shared memories and facts (collective memory): a memory/fact may
  be owned by a group; group members access it per the group's policies. Group
  ownership uses the same `mem_`/`fact_` shapes with a group owner.
- Membership management gated by `group:manage`; member-scoped access gated by
  `group:member`; operator override applies.
- Group-owned destructive actions follow the cross-agent guardrails:
  `--reason`, `--dry-run`, confirmation, and soft-delete by default.
- `group.created`, `group.member_added`, and `group.member_removed` audit
  events; group operation metrics.

Exit criteria:

- A group can be created, populated, and bound as both a policy subject and a
  policy target, with access decisions flowing through the M4 engine.
- A group-owned memory/fact is readable by group members under policy and not by
  non-members.
- The model matches [security-groups.md](security-groups.md).

## Milestone 6: Inter-Agent Messaging

Goal: deliver full durable inter-agent messaging in v0 (not a stub).

Deliverables:

- Message object shape (`msg_…`): `from` (always derived from the token, never
  input), `to` (agent or group), subject/kind, body, optional structured
  payload, optional thread/conversation id, created/delivery/read-ack state.
- Durable per-recipient mailbox/queue surviving process and pod churn.
- At-least-once delivery, per-recipient (and per-conversation) ordering, and
  explicit read/acknowledgement state.
- Group fan-out: a message to a group is delivered to current members with
  per-member delivery and ack state.
- `witself message send/list/read/ack`.
- `/v1/messages`, `/v1/messages/{message_id}:ack`, with `POST` for actions.
- `witself.message.send/list/read`.
- `message:send` and `message:read` scope enforcement; rate limits on send and
  delivery.
- Trust boundary handling: message bodies/payloads are treated as untrusted
  input; a message alone cannot authorize a cross-agent write (writes still
  require an M4 policy).
- `message.sent`, `message.delivered`, and `message.read` audit events;
  send/deliver/read metrics; messages-sent/delivered metered dimensions.

Exit criteria:

- An agent can send to another agent and to a group, and recipients can list,
  read, and ack with correct per-recipient ordering and state.
- Sender forgery is structurally impossible: `from` is always the token-bound
  agent.
- A message-driven write still fails without a matching policy.
- The model matches [inter-agent-messaging.md](inter-agent-messaging.md) and the
  threat framing in [threat-model.md](threat-model.md).

## Milestone 7: Production Storage Boundary

Goal: introduce production-shaped storage with pgvector and migrations.

Deliverables:

- Postgres storage adapter with the pgvector extension as the production system
  of record for realm, agent, token, memory, fact, embedding vector, policy,
  group, message, grant, audit, usage counter, and idempotency records.
- Goose migration framework.
- `witself-server migrate status`.
- `witself-server migrate up`.
- `witself-server migrate down` with guarded semantics.
- Advisory-locked migrations run through `witself-server migrate`, not the
  customer/operator CLI.
- Object/blob storage interface for large exports, diagnostic bundles, support
  attachments, and backup artifacts, even if most data starts in Postgres.
- Server config validation with redaction.
- Optional field-level encryption for `sensitive` facts as a capability (not the
  default); ordinary data-at-rest encryption (RDS/disk) otherwise. KMS is
  optional and demoted; there is no KMS pillar.
- Integration tests for the production adapter where infrastructure is
  available.

Exit criteria:

- `witself-server` can run against Postgres with pgvector in development.
- Memory/fact records and their embedding vectors persist and recall correctly
  from Postgres.
- Migrations are repeatable and visible in CI or a smoke environment.
- The storage model matches [storage.md](storage.md) and the backend server
  surface matches [server-command-surface.md](server-command-surface.md).

## Milestone 8: Images And Release Scaffolding

Goal: make install and distribution part of the build, not a final-week chore.

Deliverables:

- `images/witself/Dockerfile`.
- `images/witself-server/Dockerfile`.
- Multi-arch image build for `linux/amd64` and `linux/arm64`.
- GoReleaser config.
- Checksums.
- Signing path.
- SBOM and provenance path.
- Universal `curl | sh` installer script skeleton.
- Homebrew tap automation for `github.com/witwave-ai/homebrew-tap`
  (`brew install witwave-ai/tap/witself`).

Exit criteria:

- Release dry run builds the CLI, server, archives, checksums, images, and
  installer artifacts.
- Image smoke tests can run `witself version` and `witself-server version`.
- The CLI/MCP image entrypoint is `witself` (CLI and `witself mcp serve`); the
  backend image entrypoint is `witself-server`.
- The build model matches [release-and-build.md](release-and-build.md).

## Milestone 9: Helm Chart

Goal: make Kubernetes self-hosting the first production-shaped deployment
artifact.

Deliverables:

- `charts/witself`.
- Deployment for `witself-server`.
- ServiceAccount.
- Service.
- ConfigMap.
- Secret references by existing Secret names (token, database, embedding-provider
  credentials), never raw secrets in `values.yaml`.
- External Postgres-with-pgvector and embedding-provider configuration as
  first-class values.
- Optional Ingress.
- Optional NetworkPolicy.
- Liveness, readiness, and startup probes.
- Metrics service configuration.
- Separate named container ports for API, health, and metrics.
- Optional Prometheus Operator ServiceMonitor and PodMonitor.
- Resource requests and limits.
- Optional PodDisruptionBudget.
- Optional HorizontalPodAutoscaler.
- Pod and container security contexts.
- Opt-in migration Job template run before rolling `witself-server`.
- Values schema.
- Helm README and examples.
- OCI chart publication path: `ghcr.io/witwave-ai/charts/witself`.

Exit criteria:

- `helm lint` passes.
- `helm template` renders without secrets in values.
- Rendered manifests pass schema validation.
- Probe and metrics templates render with default and representative production
  values.
- ServiceMonitor and PodMonitor templates render correctly when enabled and do
  not block ordinary installs when disabled.
- Health and metrics ports are not exposed through public ingress by default.
- Dev chart install can boot `witself-server` against a test dependency set
  (Postgres with pgvector and the `local-dev` embedder).
- The chart matches [helm-chart.md](helm-chart.md).

## Milestone 10: Terraform Substrate

Goal: provide reviewable cloud infrastructure for self-hosted and later managed
Witself deployments.

AWS is the first full implementation target. GCP and Azure should keep planned
module/stack structure, but AWS gets the first working self-hosted and
managed-cloud substrate.

Deliverables:

- `infra/terraform/modules/aws`.
- `infra/terraform/modules/gcp`.
- `infra/terraform/modules/azure`.
- `infra/terraform/stacks/self-hosted/aws`.
- `infra/terraform/stacks/self-hosted/gcp`.
- `infra/terraform/stacks/self-hosted/azure`.
- Optional managed-cloud examples under
  `infra/terraform/stacks/witself-cloud` without real state or secrets.
- First working stack under `infra/terraform/stacks/self-hosted/aws`.
- First managed-cloud example under
  `infra/terraform/stacks/witself-cloud/aws`.
- Outputs consumed by the Helm chart:
  - Kubernetes context or cluster identity.
  - Postgres-with-pgvector endpoint and secret references.
  - Embedding-provider credential references.
  - Object/blob storage references.
  - Workload identity references.
  - Networking and ingress references.

Exit criteria:

- `terraform fmt` passes.
- `terraform validate` passes for modules and examples.
- Static Terraform checks run in CI.
- No real state, credentials, kubeconfigs, tfvars, embedding-provider secrets,
  or database passwords are committed.
- The substrate matches [terraform-infrastructure.md](terraform-infrastructure.md)
  and the provider order in [cloud-targets.md](cloud-targets.md).

## Milestone 11: Managed Cloud Product Slice

Goal: connect the public backend code to the managed product path.

Deliverables:

- Managed endpoint profile defaults.
- Account creation and operator bootstrap (browser/device-code auth, no raw
  passwords).
- Realm creation.
- Named agent creation.
- Agent token issuance to token files.
- Usage metering hooks for the Witself dimensions: active agents, stored
  memories, stored facts, recalls/reads, writes, embedding operations, vector
  storage size, cross-agent accesses, security groups, messages
  sent/delivered, audit retention, and general API volume.
- Rate-limit hooks.
- Billing capability discovery.
- Plan, usage, and limit JSON/API shapes.
- Configurable overage behavior for `warn`, `throttle`, and `block`.
- Hosted checkout/session scaffolding, including crypto payment rails
  (USDC/USDT/ETH via provider, no wallet custody).
- Support ticket scaffolding.

Exit criteria:

- A fresh operator can install `witself`, create or connect a managed account,
  create a realm, create named agents, write token files, and verify agents can
  immediately authenticate through `WITSELF_TOKEN_FILE`.
- Billing and support commands either work or return precise
  `unsupported_operation` responses with capability details.
- The billing model matches [billing-and-limits.md](billing-and-limits.md).

## Milestone 12: Hardening Before Production Claims

Goal: make the security and operations story credible before production use.

Deliverables:

- Threat-model review centered on integrity and authenticity of identity data
  (memory-poisoning, unauthorized curation/forgetting, cross-agent write abuse,
  sender spoofing).
- Backup and restore docs, including plaintext identity export/import and
  restoring semantic recall from backed-up vectors without re-embedding.
- Token rotation and revocation tests.
- Migration rollback and forward-upgrade tests.
- Re-embedding maintenance and embedding-model-change tests.
- Rate-limit and quota tests (including messaging send/delivery limits).
- Policy-engine coverage tests (default-deny, cross-agent guardrails, operator
  override attribution).
- Audit coverage tests (memory/fact/policy/group/message/export events).
- Metrics coverage, label-cardinality, and redaction tests.
- Health probe and readiness failure-mode tests.
- Prometheus alert and dashboard examples for self-hosted operators where
  practical.
- Redaction tests for memory content, fact values, message bodies/payloads, and
  embedding vectors.
- Incident and vulnerability handling docs.
- Self-host upgrade guide.
- Production Helm values examples.
- Production self-host support boundary and paid/contracted support language.

Exit criteria:

- Witself can honestly describe which deployment modes are production-ready and
  which remain preview or local development.
- Production self-host support is not claimed until the paid/contracted support
  path and hardening docs are ready.
- Hardening matches [threat-model.md](threat-model.md),
  [security-policy.md](security-policy.md), and
  [backup-and-recovery.md](backup-and-recovery.md).

## Sequencing Notes

- The CLI can be the first visible surface, but it should not own business logic
  that the API and MCP adapter need later.
- `witself-server serve --dev` should arrive early enough to keep the API
  honest.
- Semantic recall (M3) lands before the policy engine (M4) so cross-agent recall
  has something real to authorize, but recall must degrade deterministically so
  the rest of the build does not depend on a live embedding provider.
- The policy engine (M4) precedes security groups (M5) and messaging (M6)
  because both depend on default-deny cross-agent evaluation; a message can
  never substitute for a policy.
- Helm and Terraform can start as scaffolding, but their CI checks should exist
  early so deployment artifacts do not drift.
- Managed billing, payments, crypto payment rails, and support can be capability
  gated while the core memory/fact/policy/group/message product matures.
- Post-v0 features tracked in [post-v0-roadmap.md](post-v0-roadmap.md) — MCP
  network transport, web dashboard, additional embedding providers, cross-realm
  federation, policy `deny` effects, automated re-embedding — should not be
  mixed into the first build slice.

## Related Docs

- [requirements.md](requirements.md)
- [v0-scope.md](v0-scope.md)
- [scaffold-readiness.md](scaffold-readiness.md)
- [cli-command-surface.md](cli-command-surface.md)
- [server-command-surface.md](server-command-surface.md)
- [mcp-tools.md](mcp-tools.md)
- [api-contract.md](api-contract.md)
- [api-routes.md](api-routes.md)
- [json-contracts.md](json-contracts.md)
- [memory-model.md](memory-model.md)
- [facts-model.md](facts-model.md)
- [access-policy.md](access-policy.md)
- [security-groups.md](security-groups.md)
- [inter-agent-messaging.md](inter-agent-messaging.md)
- [backend-architecture.md](backend-architecture.md)
- [self-hosting.md](self-hosting.md)
- [self-host-support.md](self-host-support.md)
- [helm-chart.md](helm-chart.md)
- [terraform-infrastructure.md](terraform-infrastructure.md)
- [release-and-build.md](release-and-build.md)
- [observability-and-operations.md](observability-and-operations.md)
- [storage.md](storage.md)
- [cloud-targets.md](cloud-targets.md)
- [billing-and-limits.md](billing-and-limits.md)
- [backup-and-recovery.md](backup-and-recovery.md)
- [token-lifecycle.md](token-lifecycle.md)
- [audit-retention.md](audit-retention.md)
- [operator-auth.md](operator-auth.md)
- [threat-model.md](threat-model.md)
- [security-policy.md](security-policy.md)
- [post-v0-roadmap.md](post-v0-roadmap.md)
