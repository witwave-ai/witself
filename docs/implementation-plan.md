# Witself Implementation Plan

Status: historical sequencing draft with current amendments. This document
describes a practical build order for a repo that
contains the CLI, MCP adapter, backend API server, images, Helm chart, Terraform
substrate, release automation, and docs.

Memory-plan amendment (accepted 2026-07-14): the narrative-memory phases and
first coding slice in
[narrative-memory-and-curation.md](narrative-memory-and-curation.md) replace
this draft's server-embedding milestone and autonomous
`memory.consolidate(scope, dry_run)` work. Facts and non-memory platform
milestones remain applicable.

Every server embedding-provider, embed-on-write, local embedder, pgvector
prerequisite, or provider-health fallback below is preserved only as rejected
historical context. It is not a current requirement. Migration `0032` instead
implements optional client-authored profiles/JSONB rows and bounded hybrid
recall on ordinary PostgreSQL; pgvector/ANN can only be a future accelerator.

Witself reuses the Witpass platform spine and folds the former Witpass secret
payload back in as one of two planes of agent durable state. Witself runs an
**open plane** (memories + facts: plaintext at rest, indexed, recallable,
in the self-digest, plaintext-exportable) and a **sealed plane** (secrets +
TOTP: KMS-backed envelope encryption, reveal-gated, never embedded, never
recalled, never in the self-digest, never plaintext-exported). The build
sequences the open-plane domains first (memory, fact, policy, security-group,
and messaging, plus lexical and optional client-vector recall) and then lands
the sealed credential plane (secret model, envelope/KMS, key hierarchy, reveal,
TOTP, grants/roles) as a defined v0 slice on top of the same spine. The platform
milestones (core-behind-adapters, API skeleton, storage boundary, images, Helm,
Terraform, managed slice, hardening) are otherwise the same shape.

## Guiding Sequence

Witself should start small, but the first implementation slice should exercise
the real product boundaries:

- One Go module.
- One shared core service.
- CLI and MCP adapters over that core.
- A separate `witself-server` API process over that core.
- A local development adapter using the same model-free retrieval contract.
- A production storage boundary ready for ordinary PostgreSQL, KMS (required
  only when the sealed plane is enabled), and
  object/blob storage.
- Public release, image, Helm, and Terraform scaffolding from the beginning.

The local adapter is useful scaffolding. It should not become a separate product
architecture.

## V0 Release Target

V0 should be a usable cloud-shaped slice rather than a purely local mock. The
target includes:

- Installable `witself` CLI.
- `witself-server serve --dev`.
- Local development adapter with deterministic model-free recall.
- Agent token lifecycle.
- Memory, fact, recall, policy, security-group, messaging, reference,
  export/import, and audit flows.
- Sealed-plane secret, password, TOTP, reveal, grant, and runtime-injection
  flows, with envelope encryption (CMK → per-realm KEK → per-secret/field DEK)
  behind the KMS-backed capability switch.
- Deterministic lexical recall with optional migration-0032 client-vector hybrid
  ranking.
- Prometheus metrics, structured logs, and Kubernetes health probes.
- MCP stdio tools.
- PostgreSQL storage adapter with full text and JSONB support.
- KMS path (AWS-first) for the sealed plane: `realm_keys` and `secret_deks`
  envelope storage, required only when the sealed plane is enabled.
- Public CLI/MCP and backend images.
- `charts/witself-server` Helm skeleton.
- AWS Terraform skeleton.
- Strong CI, linting, and release automation.

Inter-agent messaging is fully in scope for v0 (durable mailbox, delivery,
ordering, and acknowledgement), not a stub.

Billing, support, payment, crypto payment, and broader managed-account
operations can be present as contract shapes while remaining capability-gated.
They should return deterministic `unsupported_operation` responses until a
backend explicitly supports them.

The open-plane core (memory, fact, identity, recall, policy, group, messaging)
ships first; the sealed credential plane (M6.1–M6.4) is a defined v0 slice that
may be staged after the core. The v0 scope is tracked in
[v0-scope.md](v0-scope.md).

## Milestone 0: Public Repo Foundation

Goal: make the public repository safe to build in.

Deliverables:

- Scaffold according to [scaffold-readiness.md](scaffold-readiness.md).
- Root `README.md`.
- Root `SECURITY.md`.
- Root `CONTRIBUTING.md`.
- Root FSL-1.1-ALv2 `LICENSE`.
- `.gitignore` covering local store files, token files, identity export bundles,
  Terraform state, cloud credentials, kubeconfigs, and local build output.
- Initial docs index under `docs/`.
- Initial GitHub Actions CI skeleton.
- Initial release workflow skeleton.
- Initial Go module:
  - Module path: `github.com/witwave-ai/witself`.
  - `go 1.26`.
  - `toolchain go1.26.5`, refreshed before implementation and release.

Exit criteria:

- Docs links are valid.
- CI can run on an empty or near-empty codebase.
- Root docs identify FSL-1.1-ALv2 as the repo license and reserve Witself
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
  (`memory.added`, `fact.created`, `policy.access_denied`, `crossagent.curated`,
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
- `/livez`.
- `/readyz`.
- `/startupz`.
- `/metrics`.
- Separate API, health, and metrics listeners with default ports `8080`,
  `8081`, and `9090`.
- Metrics enable/disable configuration through config, environment variables,
  CLI flags, and Helm values, with `WITSELF_METRICS_ENABLED` as the canonical
  variable.
- `/v1/whoami`.
- `/v1/capabilities` reporting backend kind, lexical memory support, optional
  client-vector profile/hybrid support, and curation capabilities independently,
  with no backend provider/model field.
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
  `backend.kind: "local"` and explicit model-free memory capabilities.
- Health endpoints report live, ready, and startup status without exposing
  sensitive config.
- `/metrics` emits Prometheus text format without raw paths, user input, memory
  content, fact values, message bodies, embedding vectors, or tokens in labels.
- Health probes and metrics run on their dedicated listeners rather than the
  public API listener.
- Unsupported service-admin commands return deterministic
  `unsupported_operation`.

## Rejected Historical Milestone 3: Server Embeddings And Semantic Recall

Do not implement any deliverable in this archived milestone. Use Phases 1, 2,
and 5 of
[narrative-memory-and-curation.md](narrative-memory-and-curation.md): lexical
recall first, then optional client-supplied vectors.

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

## Milestone 3.5: Agent Self-Management And Hydration

Do not implement the native-only narrative routing or server consolidation
below. Use the immediate capture, automatic retrieval, and client curation
phases in
[narrative-memory-and-curation.md](narrative-memory-and-curation.md).

Goal: make Witself reliable for the agent that owns it without silently
replacing memory supplied by the agent runtime. Witself is a service the agent
must be *taught* to call, unlike CLAUDE.md/AGENTS.md which the harness
auto-loads. This milestone lands provider-aware capture, an always-loaded
self-digest, multi-session bootstrap, consolidation, the file bridge, and the
teaching layer directly on top of the core memory/fact CRUD (M1) and recall (M3)
it composes, before cross-agent policy (M4).

These verbs add no new resources: they route to the existing fact and memory
create/update/recall paths. The work is convergence and teaching, not a new
store.

Deliverables:

- Natural-language provider routing: an explicit request to remember one atomic
  durable assertion calls `witself.fact.set` in the same turn; a merely stated
  fact follows the candidate/review path; narrative context stays eligible for
  runtime-native memory; mixed requests split; and explicit destinations win.
  Codex, Claude Code, Grok Build, and Cursor receive this contract through
  managed file guidance and provider-specific MCP runtime instructions.
  Claude's installed rule is runtime-neutral for compatibility loading while
  its MCP policy carries Claude auto-memory semantics; Grok's installed and MCP
  guidance uses underscore-safe tool names. Cursor's always-applied ancestor MDC
  rule and MCP guidance retain dotted tool names, treat Cursor Memories as
  project-scoped advisory context, and report broad native recall as partial.
  `witself remember`, `witself.remember`, and `POST
  /v1/remember` remain deferred; if implemented, invoking them explicitly
  selects Witself and never masquerades as the runtime's native-memory path.
- `witself self show` / `witself.self.show` / `GET /v1/self`: the bounded,
  always-loaded self-digest — primary facts first, then top-N salient memories
  (blended salience + recency), then a one-line index of kinds/tags/counts.
  Cheap; never requires the embedding provider; hard byte/line cap with
  `elided=true` pointing to `memory.recall` rather than silent truncation.
- `witself session start/end` / `witself.session.start`/`.end` /
  `POST /v1/sessions:start`,`:end`: start hydrates identity + open goals + last
  progress in one round-trip; end persists a progress memory (kind `session`)
  and updates open goals. Pairs with assume-interruption / flush-before-long-ops.
- `witself memory consolidate [--dry-run]` / `witself.memory.consolidate` /
  `POST /v1/memories:consolidate`: merges near-duplicate memories, supersedes
  stale ones, surfaces (does not auto-pick) conflicting facts, trims the
  digest/index. Dry-run defaults true; guarded; respects provenance and never
  silently overwrites human-/import-authored records.
- `witself digest emit --format claude-md|agents-md|markdown` and
  `witself ingest <PATH ...>` (the two-way file bridge): `emit` renders the
  self-digest as a CLAUDE.md/AGENTS.md fragment with witself-generated
  provenance comments; `ingest` parses CLAUDE.md/AGENTS.md/GEMINI.md — kv-shaped
  lines to facts (upsert), prose to memories — tagged `source=import:<file>`,
  with dedup/upsert to prevent re-import duplication. Ingest composes the
  existing fact/memory create paths; no new resource.
- Salient-memory selection per [memory-model.md](memory-model.md): top-N by a
  deterministic blended salience + recency score (with pinned kinds like
  `profile`/`session`), excluding archived/forgotten, never calling the
  embedding provider.
- Cross-cutting contract changes: a deterministic human-readable `echo` string on
  every mutation result; dedup/supersede on `memory.add` (and `remember`→memory)
  surfacing a `memory_duplicate`/`memory_merged` warning with the existing
  `mem_` id instead of silently creating near-dups; a first-class `source`
  provenance field on Memory and Fact records (`self`, `agent:<name>`,
  `operator`, `import:<file>`).
- The teaching layer (three reinforcing surfaces saying the same thing): the MCP
  server `instructions` field returned on connect (the canonical standing
  protocol pinned in [context-hydration.md](context-hydration.md) and
  [mcp-tools.md](mcp-tools.md)); per-tool descriptions rewritten with explicit
  when-to-call trigger lists; and `witself bootstrap-instructions
  [--format agents-md|claude-md|text]` plus `witself setup --write-agents-md`
  to ship the paste-able teaching stanza into a project AGENTS.md.
- Audit events: `memory.consolidated`, `session.started`, `session.ended`,
  `memory.imported`, `fact.imported`, and optional `self.digest.emitted`.
  Natural fact routing uses the existing `fact.created`/`fact.updated` events;
  a future explicit Witself capture action would use the existing fact or memory
  mutation events rather than inventing its own.
- Scopes reuse: `self show`/`session start`/`digest emit` need
  `memory:read` + `fact:read`; routed `fact.set` needs `fact:create`;
  `session end`/`ingest` need their existing write scopes; and `consolidate`
  needs `memory:update` (+ `memory:forget` for supersede). `--read-only` MCP
  mode excludes every mutating verb while `self.show`, `session.start`,
  `recall`, and `digest.emit` remain when implemented.

Exit criteria:

- For Codex, Claude Code, Grok Build, and Cursor, a natural explicit fact-capture
  request reaches `fact.set` in the same turn, a narrative request is not
  converted into a Witself fact or transcript fallback, and merely stated facts
  remain candidates rather than canonical truth. Each runtime's managed file
  and MCP surfaces preserve one behavioral contract, including provider-specific
  native memory availability and confirmation semantics.
- `witself self show` returns a digest under its byte cap without the embedding
  provider, sets `elided=true` when capped, and points to `memory.recall`.
- A session can be started and ended across process restarts so resuming is one
  call, not a list+recall crawl.
- `memory consolidate --dry-run` reports merges/supersessions/conflicts without
  mutating, and a non-dry run never overwrites human-/import-authored records.
- `digest emit` then `ingest` of the emitted file round-trips without creating
  duplicates, and imported records carry `source=import:<file>`.
- The MCP server advertises the standing protocol via `instructions`, and
  `bootstrap-instructions` prints the matching paste-able stanza.
- The surface matches [context-hydration.md](context-hydration.md),
  [memory-model.md](memory-model.md), and [facts-model.md](facts-model.md).

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

## Milestone 5.5: Transcript Ledger

Goal: persist the visible prompt/response record before adding addressed
delivery semantics.

Deliverables:

- Append-only transcripts with ordered `user`, `assistant`, `system`, and
  `tool` entries.
- Token-derived recorder identity; agent write/own-read and account-operator
  audit-read boundaries.
- Reply links, small `jsonb` payloads, model labels, and account archive/restore.
- No raw hidden chain-of-thought or streaming chunks.
- Reserve artifact references while deferring binary bytes to portable object
  storage.
- `witself transcript create/append/list/show` and `/v1/transcripts`.

Exit criteria:

- An agent runtime records a user prompt and finalized assistant response as
  two immutable, ordered entries and can read them after restart/restore.
- An account operator can inspect the account's transcripts but cannot append as
  an agent.
- Structured JSON round-trips through Postgres and account migration; non-empty
  file attachments fail explicitly until object storage exists.

## Milestone 6: Inter-Agent Messaging

Goal: deliver full durable inter-agent messaging in v0 (not a stub).

Status: direct processing and runner slice complete in the current checkout.
Direct same-realm agent delivery now includes the
durable message/delivery schema, token-derived sender, idempotent send,
recipient-only parent-validated reply, metadata-only inbox/outbox listing,
oldest-unacknowledged long-poll listen, separate read/ack transitions on every
surface, migration-0034 claim/renew/release and atomic result completion,
server-derived migration-0035 causal depth, migration-0036 deterministic
per-message failure counting, content-free audit, archive/restore with
active-claim interruption, and API/CLI/MCP adapters. A client-owned text-only
runner adds identity pinning, lease renewal/recovery, bounded advisory
continuation, backend-owned cross-machine turn/failure enforcement, native
Claude Code/Grok Build adapters, fail-closed Codex/Cursor probes, and
launchd/systemd supervision. Provider authentication is captured through a provider-specific
allowlist into a separate mode-0600 client file, never runner config, service
definitions, backend state, or account export.
Group fan-out, policy scopes, explicit-list/realm fan-out, open multi-assignee
request claims, rate/meter enforcement, and dry-run remain before the full
milestone exit criteria are met. This status does not claim deployment or
release. The accepted working product design is tracked in
[autonomous-realm-messaging.md](autonomous-realm-messaging.md).

Deliverables:

- Message object shape (`msg_…`): `from` (always derived from the token, never
  input), `to` (agent, bounded explicit agent list, token-derived realm, or
  group), subject/kind, body, optional structured payload, optional
  thread/correlation id, validated reply parent, backend-derived causal depth,
  and created/delivery/read-ack state.
- Ordinary direct send defaults to actionable `kind=request` consistently in
  CLI, MCP, and backend normalization; explicit `note` is FYI-only and does not
  invoke the runner provider.
- Durable per-recipient mailbox/queue surviving process and pod churn.
- At-least-once delivery, per-recipient (and per-conversation) ordering, and
  explicit read/acknowledgement state.
- Explicit-list, realm, and group fan-out: membership is an atomic send-time
  snapshot with per-member delivery and ack state. The sender is excluded from
  a realm audience by default.
- `witself message send/reply/list/listen/read/ack/claim/renew/release/complete`
  for the direct same-realm slice, plus client-local
  `message runner enable|disable|status|notifications|run|serve|start`.
- `/v1/messages`, `/v1/messages:listen`, and the
  `/v1/messages/{message_id}:reply`, `:read`, `:ack`, `:claim`, `:renew`,
  `:release`, and `:complete` actions, all using `POST` where state or a bounded
  wait is involved.
- `witself.message.send/reply/list/listen/read/ack/claim/renew/release/complete`
  plus client-local `witself.message.notification.list/consume`; read-only keeps
  message list/listen and notification list but omits consume, while curator
  profiles expose neither bridge tool. The four backend lifecycle states remain
  separate.
- Full/read-only MCP startup instructions call non-blocking
  `message.listen(wait_seconds=0)` and notification list. Consume canonically
  reads/verifies before exact local clear and retains the pointer on failure.
  The runner acknowledgement is global, but the `WITSELF_HOME` pointer is
  runtime-local and not account-exported; canonical messages are exported. This
  intentional MVP locality does not provide cross-host wake delivery.
- `message:send` and `message:read` scope enforcement; rate limits on send and
  delivery.
- Trust boundary handling: message bodies/payloads are treated as untrusted
  input; a message alone cannot authorize a cross-agent write (writes still
  require an M4 policy).
- `message.sent`, `message.delivered`, and `message.read` audit events;
  send/deliver/read metrics; messages-sent/delivered metered dimensions.
- Client-owned autonomous runner: long-poll, deduplicate, claim, read, invoke
  a configured capability-probed text-only local provider, atomically persist
  and link the reply/result, complete/release, and ack. Provider children are
  token-free. Payload continuation is advisory; server-derived causal depth
  enforces the turn limit (12 by default, capped at 64), and migration-0036
  `failure_count` enforces the cross-machine deterministic failure bound.
  Processing generation is fence-only. Provider-wide, configuration,
  cancellation, and lease-maintenance failures do not increment the count; the
  current default escalates the fifth deterministic attempt.
  Terminal/non-provider deliveries enter a private content-free notification
  ledger before ack; it exposes pointers, fails closed at capacity, and leaves
  content behind ordinary message read. The backend and MCP do not launch
  inference. Status exposes content-free last-cycle health without raw errors,
  message content, provider output, or credentials.
- Request coordination modes `notify`, `each`, `claim`, and `collaborate`, with
  realm-visible offers represented as messages and authoritative bounded claims
  protected by expiry, renewal, and fencing generations.
- Per-request recipient, assignee, turn/message, time, rate, and retry bounds;
  model/token/dollar budgets remain client-runner settings.

Implementation slices:

1. Receive correctness (**complete**): non-consuming oldest-unacknowledged
   metadata-only listen, separate MCP read/ack, recipient-only parent-validated
   reply, archive coverage, and honest capability/documentation status.
2. Direct-delivery processing fence (**complete**): claim/renew/release plus one
   transaction that validates the fence, persists the derived result reply,
   links it, and completes processing; acknowledgement remains a recoverable
   later step. Processing generation is solely the stale-writer fence; the
   migration-0036 `failure_count` is the separate durable failure bound.
3. Text-only direct runner (**complete in the current checkout**): private
   client state, identity pinning, OS service, fake-provider recovery tests,
   migration-0035 causal-depth turn enforcement, migration-0036 deterministic
   failure enforcement, bounded advisory continuation, provider-scoped private
   authentication capture, native Claude Code/Grok Build adapters, content-free
   cycle health, and explicit fail-closed Codex/Cursor capability results. The
   generic command-adapter core is available for separately integrated wrappers;
   arbitrary wrapper CLI configuration and tool-capable execution are not part
   of this slice.
4. Bounded explicit-list and realm fan-out with archive/audit coverage.
5. Realm open requests, offers, atomic `max_assignees` claims, leases/fences,
   renewal, expiry, release, completion, and reassignment.
6. Multi-runner race, restart, disabled-agent, cancellation, rate, export/import,
   and three-cloud conformance hardening.

Exit criteria:

- An agent can send to another agent, a bounded explicit set, the current realm,
  and a group, and recipients can listen, list, read, and ack with correct
  per-recipient ordering and state.
- A direct request can complete an autonomous clarification loop and durable
  result while either runtime is initially offline; a message send by itself
  never claims that a model was invoked.
- A realm request with `max_assignees=2` may receive many offers but can never
  have more than two current fenced claims; stale claim completion fails.
- Sender forgery is structurally impossible: `from` is always the token-bound
  agent.
- A message-driven write still fails without a matching policy.
- The server passes messaging/request tests with no model credential, provider
  SDK, semantic ranking, or inference network call.
- The model matches [inter-agent-messaging.md](inter-agent-messaging.md) and the
  autonomous extension in
  [autonomous-realm-messaging.md](autonomous-realm-messaging.md), plus the threat
  framing in [threat-model.md](threat-model.md).

## Milestone 6.1: Sealed-Plane Secret Model

Goal: land the sealed plane's data model and non-cryptographic lifecycle on top
of the open-plane core, so that secrets behave like a first-class agent-state
type while the envelope/KMS layer (M6.2) is still local-dev.

This is the analogue of the open-plane memory/fact core (M1), but for the sealed
plane. Secrets and TOTP seeds are a *carve-out* from the open plane: they are
never embedded, never returned by semantic recall, never in the self-digest,
never plaintext-exported, and never ingested from CLAUDE.md/AGENTS.md. Reads of
secret values go through an explicit, audited reveal ceremony — unlike memories
and facts, which are plainly readable by an authorized single-record read.

Deliverables:

- Core service interfaces for secrets, secret fields, TOTP enrollments, grants,
  and password generation, owned by an agent or a group (`owner_kind ∈
  {agent, group}`, unified with the open plane; vault-shared secrets become
  group-owned secrets).
- Secret object shapes (`sec_…`, `fld_…`) per [secret-model.md](secret-model.md)
  and the sealed-plane tables in [data-model.md](data-model.md): templates
  (`login`, `api-key`, `ssh-key`, `certificate`, `env`, `generic`), fields,
  sensitivity, references, and lifecycle metadata.
- Secret lifecycle: create, show, list, scan, update, rename, copy, archive,
  restore, delete — with soft-delete/tombstone defaults consistent with the
  open-plane guardrails, and `--reason`/`--dry-run`/confirmation on destructive
  ops.
- TOTP enrollment object shape (`totp_…`): `otpauth`/Base32 seed import, QR
  enrollment, and code generation per [totp-2fa.md](totp-2fa.md); the seed is
  high-value sealed material distinct from `totp:code`.
- Password generator (`witself password generate`) per
  [secret-model.md](secret-model.md).
- Secret references (`witself://secret/<path>/<field>` for the current agent,
  `witself://agent/<agent>/secret/<path>/<field>` and
  `witself://group/<group>/secret/<path>/<field>` for granted/operator access),
  parsed and resolved through the same authorization as a direct reveal.
- Runtime injection (`witself run`): resolve secret references into a subprocess
  environment without persisting plaintext, audited as a reveal-class read.
- Sealed-plane redaction: secret field values, TOTP seeds, and generated
  passwords never leak into list/scan/errors/log-like output (no plain read;
  reveal only).
- Audit events: `secret.created`/`updated`/`renamed`/`copied`/`archived`/
  `restored`/`deleted`, `totp.enrolled`/`code`/`seed_revealed`/`deleted`.

Early CLI commands:

- `witself secret create`
- `witself secret show`
- `witself secret list`
- `witself secret scan`
- `witself secret update`
- `witself secret rename`
- `witself secret copy`
- `witself secret archive`
- `witself secret restore`
- `witself secret delete`
- `witself password generate`
- `witself totp enroll`
- `witself totp code`
- `witself totp show`
- `witself totp delete`
- `witself run`

Exit criteria:

- Local mode can create a group- or agent-owned secret, store a generated
  password, enroll TOTP, generate a TOTP code, and run a subprocess with a
  resolved secret reference.
- Secret field values, TOTP seeds, and generated passwords are redacted from
  `secret list`/`secret scan`/errors/log-like output; obtaining a value requires
  the reveal ceremony (M6.2), never a plain read.
- No secret value, TOTP seed, or generated password is ever embedded, returned
  by `memory recall`, included in `self show`/`digest emit`, or written to a
  plaintext `witself export`.
- The model matches [secret-model.md](secret-model.md),
  [totp-2fa.md](totp-2fa.md), and
  [secret-size-and-attachments.md](secret-size-and-attachments.md).

## Milestone 6.2: Envelope Encryption, Key Hierarchy, And Reveal

Goal: make the sealed plane actually sealed — envelope encryption behind a
capability-gated KMS boundary, the key hierarchy, and the explicit reveal
ceremony — so no secret value is ever stored as an ordinary database value.

Unlike the open plane, which needs only ordinary PostgreSQL for lexical and
optional client-vector recall, the sealed plane has a capability-gated provider
boundary with a `local-dev` implementation. KMS is **required only when the
sealed plane is enabled**; an open-plane-only deployment does not need it.

Deliverables:

- Provider-shaped KMS abstraction with `aws-kms` (first target), `gcp-kms`,
  `azure-key-vault`, and `local-dev` implementations, selectable via
  `WITSELF_KMS_PROVIDER` and `WITSELF_KMS_KEY_ID` (with `WITSELF_PASSPHRASE_FILE`
  for the local-dev path), per [key-hierarchy.md](key-hierarchy.md).
- The key hierarchy: customer master key (CMK) in KMS → per-realm KEK
  (`kek_…`) → per-secret/field DEK (`dek_…`), with AEAD payloads
  (`XCHACHA20_POLY1305` default, `AES_256_GCM`) per
  [encryption-model.md](encryption-model.md).
- Envelope encryption wired into every secret/field/TOTP-seed write so values
  are stored only as ciphertext + wrapped DEK; the `realm_keys` and
  `secret_deks` storage shapes per [storage.md](storage.md) (introduced ahead of
  the production storage boundary in M7).
- Hybrid decrypt behind one capability switch: `client_side_decrypt` (default
  where the client holds key material) and `server_side_decrypt` (for
  token-only pods), reported through the capabilities contract.
- The reveal ceremony: `witself secret reveal` and `witself totp code` as the
  explicit, audited, value-returning operations; reference resolution and
  `witself run` are reveal-class reads subject to the same authorization and
  audit.
- Capability reporting of the active KMS provider, key identity, and
  `client_side_decrypt`/`server_side_decrypt` support.
- KEK rotation path (`key.rotated`) as an explicit, audited maintenance
  operation (not an automatic side effect).
- Metrics for reveals, TOTP codes, and KMS operations
  (`witself_secret_reveals_total`, `witself_totp_*`,
  `witself_kms_operations_total`), with no secret material in labels.
- Audit events: `secret.reveal`, `totp.code`, `key.rotated`, each carrying the
  `server_side_decrypt` flag when the value crossed the server boundary.

Exit criteria:

- A secret created in M6.1 is stored only as ciphertext + wrapped DEK; no
  plaintext secret value, TOTP seed, or generated password is stored as an
  ordinary database value.
- `witself secret reveal` and `witself totp code` return values only through the
  audited reveal ceremony, against both a real KMS provider and `local-dev`.
- With the sealed plane disabled, KMS is not required and the capabilities
  contract reports the sealed plane as off; the open plane is unaffected.
- Loss of the KMS key renders secret values unrecoverable (crypto-shred) without
  affecting the open plane.
- Ciphertext, wrapped DEKs, and key material never appear in logs, metrics
  labels, audit records, or errors.
- The model matches [encryption-model.md](encryption-model.md),
  [key-hierarchy.md](key-hierarchy.md), and [storage.md](storage.md).

## Milestone 6.3: Secret Grants And Realm Roles

Goal: govern sealed-plane access with grants and realm roles, composed with the
open-plane cross-agent policy engine (M4) without subjecting secrets to the open
read/curate/forget verbs.

Secrets are not governed by the open cross-agent identity policy engine. The
identity policy engine (M4, [access-policy.md](access-policy.md)) governs the
open plane (memories/facts); sealed-plane cross-agent and operator access uses
grants plus realm roles per
[authorization-and-roles.md](authorization-and-roles.md).

Deliverables:

- Secret grant object shape (`grt_…`): subject (agent or group) × secret/path ×
  granted scopes, with `--reason`-audited issuance and revocation, per
  [secret-model.md](secret-model.md) and
  [authorization-and-roles.md](authorization-and-roles.md).
- Sealed-plane scopes enforced below all adapters: `secret:create`,
  `secret:show`, `secret:reveal`, `secret:update`, `secret:delete`,
  `secret:grant`, `totp:enroll`, `totp:code`.
- Realm roles (`realm:admin`, `realm:operator`, `realm:auditor`, `realm:member`)
  with the consolidated scope bundles spanning both planes, and the resolution
  algorithm in [authorization-and-roles.md](authorization-and-roles.md).
- The default agent token bundle includes `secret:create`/`show`/`reveal`/
  `update`/`delete` and `totp:enroll`/`code` over the agent's own data, but
  excludes `secret:grant` and any `*-others`/manage scopes.
- `witself secret grant` and `witself secret revoke`; `--group` for group-owned
  targets and `--owner-agent` for operator targeting.
- Operator override: realm operators manage/reveal sealed-plane data within their
  realm, audited like agent actions and subject to the same `--reason`
  requirements.
- Audit events: `secret.grant` and `secret.revoke`, fully attributed.

Exit criteria:

- A grant lets a named agent or group reveal a secret it does not own, and
  revocation removes that access, both attributed in audit.
- Sealed-plane scopes are enforced independently of the open-plane policy engine;
  a memory/fact `read` policy never authorizes a secret reveal.
- Realm roles resolve to the correct cross-plane scope bundles.
- The model matches [authorization-and-roles.md](authorization-and-roles.md) and
  [secret-model.md](secret-model.md).

## Milestone 6.4: Sealed-Plane MCP, API, And Export Carve-Outs

Goal: expose the sealed plane through the MCP and API adapters with the
value-returning operations gated, and lock in the export/digest carve-outs.

Deliverables:

- MCP tools: `witself.secret.create`/`list`/`show`/`reveal`/`update`,
  `witself.totp.enroll`/`code`/`show`, `witself.password.generate`, and the
  value-returning `witself.reference.resolve`.
- MCP `--no-value-tools` mode (sealed plane only) that disables
  `secret.reveal`/`totp.code`/value-returning `reference.resolve`; `--read-only`
  still disables all mutations across both planes.
- API routes: `/v1/secrets` (+ `:reveal`/`:rotate`/`:archive`/`:restore`/
  `:grant`/`:revoke`), `/v1/totp` (+ `:code`), and `/v1/password:generate`,
  using `POST` for actions, with `client_side_decrypt`/`server_side_decrypt`
  capability flags surfaced in responses.
- Export carve-out: `witself export` excludes the sealed plane from the plaintext
  identity bundle; secret backup is encrypted-only (envelope + KMS key identity,
  never plaintext) behind an explicit, audited, separate flag, per
  [backup-and-recovery.md](backup-and-recovery.md).
- Self-digest carve-out enforced at the adapter boundary: `self show`,
  `digest emit`, and `ingest` never include secrets or TOTP seeds.
- Metered dimensions wired through: `stored_secret`, `secret_read` (reveal +
  reference resolution), `totp_code`, `runtime_injection`, and
  `encrypted_storage_byte`.

Exit criteria:

- The MCP and API adapters expose secret/TOTP/password operations with
  equivalent behavior to the CLI, and `--no-value-tools` removes only the
  value-returning sealed-plane tools.
- `witself export` produces a plaintext identity bundle with no secret material;
  encrypted secret backup is available only behind the explicit audited flag.
- `self show`/`digest emit`/`ingest` never surface a secret value or TOTP seed.
- Sealed-plane usage is metered through the new dimensions without secret
  material in any label or counter name.
- The surface matches [mcp-tools.md](mcp-tools.md),
  [api-contract.md](api-contract.md), [api-routes.md](api-routes.md),
  [json-contracts.md](json-contracts.md), and
  [backup-and-recovery.md](backup-and-recovery.md).

## Milestone 7: Production Storage Boundary

Goal: introduce production-shaped PostgreSQL storage and migrations.

Deliverables:

- PostgreSQL storage adapter with full text and migration-0032 JSONB vectors as
  the production system of record for realm, account, operator, agent, token,
  memory, fact, client-vector profile/row, policy, group, message, audit, usage
  counter, and idempotency records
  (open plane), plus the sealed-plane tables: secrets, secret fields, secret
  grants, TOTP enrollments, `realm_keys`, `secret_deks`, and attachments.
- Goose migration framework.
- `witself-server migrate status`.
- `witself-server migrate up`.
- `witself-server migrate down` with guarded semantics.
- Advisory-locked migrations run through `witself-server migrate`, not the
  customer/operator CLI.
- Object/blob storage interface for large exports, diagnostic bundles, support
  attachments, and backup artifacts, even if most data starts in Postgres.
- Server config validation with redaction.
- Two-tier encryption posture: the open plane uses ordinary data-at-rest
  encryption (RDS/disk), with optional field-level encryption for `sensitive`
  facts as a capability (not the default); the sealed plane uses the
  CMK → per-realm KEK → per-secret/field DEK envelope from M6.2, with `realm_keys`
  and `secret_deks` persisted here. KMS is a required dependency only when the
  sealed plane is enabled.
- Integration tests for the production adapter where infrastructure is
  available.

Exit criteria:

- `witself-server` can run against ordinary PostgreSQL in development.
- Memory/fact records and optional client-vector profiles/JSONB rows persist and
  recall correctly from PostgreSQL.
- Sealed-plane secret values persist only as ciphertext + wrapped DEK; no
  plaintext secret value, TOTP seed, or generated password is stored as an
  ordinary database value.
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
- The CLI/MCP image entrypoint is `ws` (CLI and `witself mcp serve`); the
  backend image entrypoint is `witself-server`.
- The build model matches [release-and-build.md](release-and-build.md).

## Milestone 9: Helm Chart

Goal: make Kubernetes self-hosting the first production-shaped deployment
artifact.

Deliverables:

- `charts/witself-server`.
- Deployment for `witself-server`.
- ServiceAccount.
- Service.
- ConfigMap.
- Secret references by existing Secret names (token, database, KMS
  credentials), never raw secrets in `values.yaml`.
- External PostgreSQL configuration as a first-class value; no model-provider
  or vector-extension setting is required.
- KMS provider configuration (`kms.*` values) as first-class values for the
  sealed plane, required only when the sealed plane is enabled.
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
- OCI chart publication path: `ghcr.io/witwave-ai/charts/witself-server`.

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
  (ordinary PostgreSQL with no model service).
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
  - PostgreSQL endpoint and secret references.
  - KMS key references for the sealed plane (when enabled).
  - Object/blob storage references.
  - Workload identity references.
  - Networking and ingress references.

Exit criteria:

- `terraform fmt` passes.
- `terraform validate` passes for modules and examples.
- Static Terraform checks run in CI.
- No real state, credentials, kubeconfigs, tfvars, or database passwords are
  committed.
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
  sent/delivered, audit retention, and general API volume (open plane); plus
  stored secrets, secret reads (reveal + reference resolution), TOTP codes,
  runtime injections, and encrypted storage size (sealed plane).
- Rate-limit hooks.
- Billing capability discovery.
- Plan, usage, and limit JSON/API shapes.
- Configurable overage behavior for `warn`, `throttle`, and `block`.
- Hosted checkout/session scaffolding, including crypto payment rails
  (USDC/USDT/ETH via provider, no wallet custody).
- Support ticket scaffolding.

Exit criteria:

- A fresh operator can install `ws`, create or connect a managed account,
  create a realm, create named agents, write token files, and verify agents can
  immediately authenticate through `WITSELF_TOKEN_FILE`.
- Billing and support commands either work or return precise
  `unsupported_operation` responses with capability details.
- The billing model matches [billing-and-limits.md](billing-and-limits.md).

## Milestone 12: Hardening Before Production Claims

Goal: make the security and operations story credible before production use.

Deliverables:

- Dual-posture threat-model review: integrity and authenticity of identity data
  (open plane: memory-poisoning, unauthorized curation/forgetting, cross-agent
  write abuse, sender spoofing) and confidentiality of secrets (sealed plane:
  leakage, KMS/role compromise, reveal abuse, server-side-decrypt TCB expansion,
  tenant blast radius).
- Backup and restore docs covering both planes: plaintext identity
  export/import and restoring semantic recall from backed-up vectors without
  re-embedding (open plane); encrypted-only secret backup with KMS key identity,
  rotation metadata, and KMS-loss/crypto-shred implications (sealed plane), with
  secrets excluded from the plaintext export.
- Key-rotation and reveal-ceremony tests for the sealed plane (KEK rotation,
  `client_side_decrypt`/`server_side_decrypt`, audited reveal/code/grant paths).
- Token rotation and revocation tests.
- Migration rollback and forward-upgrade tests.
- Client-vector profile replacement, coverage, and archive-round-trip tests;
  Witself never generates vectors or owns model credentials.
- Rate-limit and quota tests (including messaging send/delivery limits).
- Policy-engine coverage tests (default-deny, cross-agent guardrails, operator
  override attribution).
- Audit coverage tests (memory/fact/policy/group/message/export events).
- Metrics coverage, label-cardinality, and redaction tests.
- Health probe and readiness failure-mode tests.
- Prometheus alert and dashboard examples for self-hosted operators where
  practical.
- Redaction tests for memory content, fact values, message bodies/payloads, and
  embedding vectors (open plane), and for secret field values, TOTP seeds,
  generated passwords, ciphertext, and key material (sealed plane).
- Carve-out tests asserting secrets and TOTP seeds are never embedded, never
  returned by recall, never in `self show`/`digest emit`/`ingest`, and never in
  the plaintext export.
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
- Recall lands before the policy engine (M4) so cross-agent recall has something
  real to authorize. PostgreSQL lexical recall is the universal baseline;
  optional client-supplied vectors add deterministic hybrid ranking without a
  live backend embedding provider.
- Agent self-management and hydration (M3.5) lands right after core memory/fact
  CRUD (M1) and recall (M3), before cross-agent policy (M4): it adds no new
  resources and only composes those paths, but Witself is a service the agent
  must be taught to call, so the always-loaded self-digest, quick-capture
  `remember`, session bootstrap, consolidation, file bridge, and the teaching
  layer are what make the core product reliable for a single agent before
  cross-agent sharing is introduced.
- The policy engine (M4) precedes security groups (M5) and messaging (M6)
  because both depend on default-deny cross-agent evaluation; a message can
  never substitute for a policy.
- The sealed credential plane (M6.1–M6.4) lands after the open-plane core
  (M1–M6) and may be staged separately: it reuses the same realm/account/agent/
  token spine, the unified `owner_kind ∈ {agent, group}` model, and the
  authorization layer, but it is a confidentiality plane with its own
  carve-outs. Secrets and TOTP seeds are never embedded, recalled, in the
  self-digest, or plaintext-exported, and value reads go through an explicit
  audited reveal — distinct from the plainly readable open plane. KMS becomes a
  required dependency only once the sealed plane is enabled; an open-plane-only
  deployment ships without it.
- Helm and Terraform can start as scaffolding, but their CI checks should exist
  early so deployment artifacts do not drift.
- Managed billing, payments, crypto payment rails, and support can be capability
  gated while the core memory/fact/policy/group/message product matures.
- Post-v0 features tracked in [post-v0-roadmap.md](post-v0-roadmap.md) — MCP
  network transport, web dashboard, advanced vector-profile migration,
  cross-realm federation, and policy `deny` effects — should not be
  mixed into the first build slice.

## Related Docs

- [requirements.md](requirements.md)
- [context-hydration.md](context-hydration.md)
- [v0-scope.md](v0-scope.md)
- [scaffold-readiness.md](scaffold-readiness.md)
- [cli-command-surface.md](cli-command-surface.md)
- [server-command-surface.md](server-command-surface.md)
- [mcp-tools.md](mcp-tools.md)
- [api-contract.md](api-contract.md)
- [api-routes.md](api-routes.md)
- [json-contracts.md](json-contracts.md)
- [data-model.md](data-model.md)
- [memory-model.md](memory-model.md)
- [facts-model.md](facts-model.md)
- [access-policy.md](access-policy.md)
- [security-groups.md](security-groups.md)
- [inter-agent-messaging.md](inter-agent-messaging.md)
- [secret-model.md](secret-model.md)
- [totp-2fa.md](totp-2fa.md)
- [secret-size-and-attachments.md](secret-size-and-attachments.md)
- [encryption-model.md](encryption-model.md)
- [key-hierarchy.md](key-hierarchy.md)
- [authorization-and-roles.md](authorization-and-roles.md)
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
