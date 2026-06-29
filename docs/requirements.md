# Witself Requirements

Status: pre-implementation draft. Last reviewed 2026-06-26.

## Product Summary

Witself is the durable-self platform for agents AND the trust fabric agents
collaborate over. Every agent gets a durable, attributable self — memories, facts,
and sealed credentials — and, on the go-forward path, a verified, loop-safe channel
to work with other agents across machines, realms, and accounts. The identity and
memory store is what makes that channel trustworthy: a cross-realm message is only
worth acting on because the sender has a durable, attributable self behind it.
Cross-realm agent collaboration is the flagship of the go-forward architecture. It
is sequenced as the first post-v0 epic, built on top of the realm-local core (see
[V0 Scope](#v0-scope) and [Post-v0 Roadmap](#post-v0-roadmap)); you cannot extend a
collaboration substrate that is not built yet.

Witself gives AI agents (and the humans who operate them) a safe, auditable CLI,
MCP, and API surface across two planes:

- **Open plane** (memories + facts): plaintext at rest, semantically indexed,
  recallable, cross-agent readable/curatable under declarative **policy**,
  organized into **security groups**, exchanged as durable **messages**, and
  plaintext-exportable. This is the identity store and the headline feature.
- **Sealed plane** (secrets + TOTP): KMS-backed envelope-encrypted credential
  material — passwords, API keys, SSH/TLS keys, TOTP seeds, recovery codes — that
  is reveal-gated and **never embedded, recalled, in the self-digest, ingested, or
  plaintext-exported**. This is the credential vault and authenticator, folded in
  from Witpass.

Witself absorbs the former Witpass product. There is now ONE product, ONE CLI
(`witself`), one backend, and one account/realm/agent/token model spanning both
planes. The open plane protects the *integrity and authenticity* of identity
data; the sealed plane protects the *confidentiality* of credential material. The
two planes share the platform spine — one core domain service behind thin
CLI/MCP/API adapters, the same tenancy and token model, the same
managed/self-hosted/local deployment shape, the same authorization, audit,
observability, release, and billing apparatus — and differ only in their
data-at-rest posture and value-handling ceremony.

The sealed-plane carve-out is a load-bearing invariant repeated throughout this
spec: secret and TOTP-seed material is never embedded, never returned by semantic
recall, never present in the self-digest or `digest emit`, never ingested from
`CLAUDE.md`/`AGENTS.md`, and never written to a plaintext export. See
[Sealed-Plane Invariants](#sealed-plane-invariants).

The managed cloud backend is the default product path. Witself expects to store
memories, facts, policies, groups, messages, and envelope-encrypted secrets
remotely in the cloud when offered as a service, while keeping the backend API
code public and self-hostable for operators who want to run Witself in their own
cloud, network, or compliance boundary.

The local backend may start as development scaffolding, but it should graduate
into a real backend adapter behind the same service boundary used by the managed
and self-hosted API. Local mode remains a mock/development deployment profile,
not the production model.

## Naming

- Product name: Witself.
- Avoid spelling the product as WitSelf.
- Module path: `github.com/witwave-ai/witself`.
- Binaries: `witself` (CLI and `witself mcp serve`) and `witself-server` (backend
  API). There is no `server` subcommand on the main CLI.
- CLI command name (decided, go-forward): the invoked command is `ws`. The
  mechanical rename from `witself <cmd>` to `ws <cmd>` is a separate follow-up
  sweep; examples in these docs stay on `witself <cmd>` until that sweep lands. The
  `witself://` reference scheme is unaffected and never becomes `ws://` (which
  collides with the WebSocket scheme).
- Environment prefix: `WITSELF_` (for example `WITSELF_TOKEN_FILE`,
  `WITSELF_ENDPOINT`, `WITSELF_METRICS_ENABLED`). The sealed plane adds
  `WITSELF_KMS_PROVIDER`, `WITSELF_KMS_KEY_ID`, and `WITSELF_PASSPHRASE_FILE` (the
  local-dev passphrase that wraps the local KEK/DEK; see
  [Encryption (Two-Tier)](#encryption-two-tier)).
- JSON schema version string: `witself.v0` (the rename of the former
  `witpass.v0`).
- Raw agent-token prefix: `witself_at_` (the rename of the former `wp_at_`).
- Reference URI scheme: `witself://` (never `ws://`, which collides with the
  WebSocket scheme). Open-plane reference forms are pinned in
  [Identity References](#identity-references); sealed-plane secret reference forms
  are pinned in [Secret References](#secret-references). The former Witpass `wp://`
  scheme is fully retired.
- ID prefixes: `acct_`, `realm_`, `agent_`, `opr_`, `tok_`, `mem_`, `fact_`,
  `grp_`, `pol_`, `msg_`, `thr_`, `aud_`, and the sealed-plane prefixes `sec_`
  (secret), `fld_` (secret field), `grt_` (secret grant), `totp_` (TOTP
  enrollment), `kek_` (per-realm key-encryption key), `dek_` (per-secret/field
  data-encryption key), `att_` (attachment), `usg_` (usage counter), and `idem_`
  (idempotency record).

## Core Goals

- Let agents add, adjust, read, recall, list, forget, and restore their own
  memories.
- Let agents set, get, list, and delete their own facts, and promote a fact to
  primary.
- Let agents recall memories **semantically by default** through embedding-backed
  similarity search, combined with keyword, tag, kind, and time filters.
- Let agents access other agents' memories and facts only under explicit,
  evaluable policy, with a default-deny stance.
- Let agents and operators organize agents into named security groups that act as
  both policy subjects and policy targets, and that can own group-scoped shared
  memories and facts.
- Let agents exchange durable messages with other agents and groups through a
  mailbox/queue model with delivery, ordering, and acknowledgement.
- Let agents create, store, reveal, and inject their own sealed-plane secrets
  (passwords, API keys, SSH/TLS keys, recovery codes, env vars, connection
  strings) under envelope encryption, and act as their own TOTP authenticator —
  with secret material never embedded, recalled, in the self-digest, ingested, or
  plaintext-exported.
- Let humans use the same product from a clear, safe CLI, with or without AI
  assistance, and with no privileged AI-only path.
- Make first-class structured/plaintext identity export and round-trippable
  import a headline feature, not a forbidden one.
- Drive managed-service account administration through the CLI, including customer
  account details, operators, billing, payments, usage, and support.
- Provide a complete CLI bootstrap path to create a managed realm and issue tokens
  for named agents.
- Implement the CLI, MCP server, and shared core in Go using Go modules.
- Implement the backend API server in the same public repository.
- Support a self-hosted deployment mode for operators who want to run the backend
  in their own cloud.
- Instrument `witself-server` with Prometheus-compatible metrics,
  Kubernetes-compatible health probes, and structured logs from the beginning.
- Support universal installation paths, including Homebrew and `curl | sh`.
- Provide an MCP-compatible surface for agent runtimes.
- Keep CLI, MCP, managed API, self-hosted API, and local development behavior
  consistent through one shared core, with authorization enforced below every
  frontend.
- Keep the source repository, backend code, release artifacts, and published
  packages public.
- Expose backend capabilities so the CLI can report which managed, self-hosted, or
  local features are available before running an operation.
- Use CLI-initiated human/operator authentication for managed account setup, with
  browser and device-code flows rather than raw password collection.

## V0 Scope

Decision: v0 ships in two sequenced slices on one platform. The **open-plane core
ships first**; the **sealed credential plane is a defined v0 slice that may be
staged after the core**. Both are in v0 scope; the sequencing exists so the
identity store can prove itself without blocking on KMS.

Open-plane core (ships first). A usable cloud-shaped slice that proves the CLI,
MCP stdio, the backend API boundary, a local development adapter, the agent token
lifecycle, the memory model with semantic recall, the facts model with primary
promotion, agent self-managed memory and context hydration (the always-loaded
self-digest, quick-add capture, session bootstrap, memory consolidation, the
two-way CLAUDE.md/AGENTS.md file bridge, and the teaching layer that gets agents
to call Witself), the cross-agent access policy engine, security groups, full
inter-agent messaging, identity export/import, audit events, Prometheus metrics,
Kubernetes health probes, the Postgres storage path with pgvector, public images,
a Helm chart skeleton, a Terraform AWS skeleton, CI, linting, and release
automation. The open plane has **no KMS dependency**: pgvector is a hard readiness
gate, and the embedding provider is degradable.

Sealed credential plane (a defined v0 slice, may stage after the core). The
secret data model and lifecycle, secret references, TOTP/2FA, password generation,
runtime injection (`witself run`), two-tier envelope encryption (CMK → per-realm
KEK → per-secret/field DEK), the reveal ceremony, secret grants and realm roles,
and the sealed-plane carve-outs. This slice adds a **KMS dependency** that is a
readiness gate only when the sealed plane is enabled (see
[Encryption (Two-Tier)](#encryption-two-tier) and
[Sealed-Plane Invariants](#sealed-plane-invariants)). An open-plane-only
deployment requires no KMS. The crypto subset for v0 is tracked in
[key-hierarchy.md](key-hierarchy.md).

Billing, support, payment, crypto payment, and broader managed-account operations
may appear as command, API, and JSON contract shapes in v0, but they can be
capability-gated until the managed service is ready. Unsupported operations must
return deterministic `unsupported_operation` responses with capability context.

Inter-agent messaging is **fully in scope** for v0 (durable mailbox, delivery,
ordering, and acknowledgement), not a stub.

The v0 decision is tracked in [v0-scope.md](v0-scope.md).

## Decision Log

### Principal Model

The primary principal model is per agent. Each agent has its own identity,
permissions, audit trail, and default ownership rules for the memories, facts, and
secrets it creates.

By default, a named agent can only read, adjust, forget, or delete the memories
and facts it created, and can only show, reveal, update, or delete the secrets it
created (and generate TOTP codes for its own enrollments). One agent cannot list,
read, recall, contribute to, curate, or forget another agent's memories or facts
unless an operator grants access or a policy permits it, and cannot show, reveal,
or use another agent's secrets unless a secret grant or realm role permits it.
Open-plane cross-agent access is governed by policy; sealed-plane cross-agent
access is governed by grants plus realm roles. Neither is default-shared.

Agent-created memories, facts, and secrets default to the creating agent's
ownership inside the realm. Group-scoped memories, facts, and secrets are owned by
a security group and are an explicit choice (`--group`), not the default. There is
no separate "shared" scope: `owner_kind ∈ {agent, group}` uniformly (see
[Ownership Unification](#ownership-unification)).

Operators can manage the full lifecycle of named agents inside a realm. Required
lifecycle operations include creating, renaming, copying, disabling, enabling,
archiving, and deleting agents.

Agent lifecycle operations are operator/admin actions by default. An agent must
not be able to create, rename, copy, disable, enable, archive, or delete agents
unless it has been explicitly granted that permission.

Copying an agent should be explicit about what is copied. At minimum, Witself
should support copying agent metadata, policy bindings, and group membership.
Copying memories and facts should be a separate explicit option because it
duplicates identity material and must be audited.

Agent identity comes from the authenticated token, never from a caller-supplied
agent name. This is load-bearing for cross-agent access and messaging: the sender
or actor on every operation is derived server-side from the token. Passing an
agent name must never let a caller act as that agent unless the authenticated
token or operator credential has explicit permission.

### Realm Model

A realm is the operator-owned container for a group of named agents. It is the
scope that holds agents, agent-owned and group-owned memories, facts, and secrets,
security groups, policies, messages, secret grants, the per-realm KEK (in
`realm_keys`), audit records, and billing or usage limits. The realm is the rename
of the Witpass "vault" and is the top-level managed object in Witself; the former
per-vault KEK is now the per-realm KEK (see [Key Hierarchy](#key-hierarchy)).

In the managed service, billing attaches at the account level and usage rolls up
by realm: plans, usage limits, agent caps, stored-memory and stored-fact limits,
recall/embedding limits, message limits, and rate limits are measured per realm.

An operator or realm admin can inspect identity state across all agents in the
realm. This includes listing all agent-owned and group-owned memories and facts,
viewing metadata, reviewing policies and group membership, and, when authorized,
reading, curating, forgetting, or exporting identity data. Operator access is
audited the same way agent access is audited (operator override; see
[Cross-Agent Access Policy](#cross-agent-access-policy)).

### Operator Multi-Agent Management

The same CLI used by agents and humans must support operator-level management
across every named agent in a realm. Operators should not need a separate admin
console to inspect or manage multi-agent identity state.

Operator workflows should include:

- Realm-wide inventory of memories and facts across all agents and groups.
- Scanning every memory and fact in the realm, with `sensitive` facts redacted by
  default.
- Filtering by owning agent, owning group, memory kind, tag, fact name, primary
  flag, source, or access state.
- Showing one agent-owned or group-owned memory or fact by specifying the owner.
- Reading, curating, forgetting, restoring, exporting, or re-homing a specific
  memory or fact when operator permissions allow it.
- Managing policies and security-group membership across the realm.
- Auditing every operator action with the same rigor as agent actions.

Broad destructive actions should require explicit targeting and confirmation. For
example, an operator may list or scan `--all-agents`, but curate/forget/delete
operations should require a specific `--owner-agent` or `--owner-group`, or an
explicitly group-shared target. Cross-agent forget and curate require an audit
`--reason` and confirmation unless `--yes` is supplied.

### CLI-First Service Administration

Witself managed-service administration should be CLI-first. Operators should not
need to log in to a web dashboard for ordinary account, billing, payment,
customer, usage, or support workflows.

This is a core product stance, not only an implementation detail. Account
management through a browser dashboard should be optional and secondary. The CLI
should be the normal way to create an account, configure billing, inspect usage,
manage realms, rotate agents, open support tickets, export identity data, and
recover from operational issues.

The CLI should work well with or without AI help. A human operator can run
commands directly. An AI assistant can inspect structured output, propose
commands, run authorized commands, and explain results without a separate web-only
admin path. AI assistance must use the same authentication, permissions,
confirmations, audit reasons, redaction, and payment-safety rules as a human
operator.

CLI-managed service workflows should include:

- Showing and updating customer account details.
- Inviting, removing, and changing roles for human operators/admins.
- Creating, renaming, archiving, and deleting realms.
- Managing realm membership.
- Viewing current plan, limits, and usage.
- Managing subscription state.
- Managing payment methods.
- Applying promo codes during setup, account creation, or subscription changes.
- Managing crypto payment flows alongside traditional payment methods when
  supported.
- Viewing and downloading invoices.
- Opening, listing, commenting on, and closing support tickets.
- Exporting account, realm, identity, usage, invoice, and support data where
  policy allows.

Billing and payment flows should be driven from the CLI, but Witself should not
collect raw card numbers or other high-risk payment details directly in CLI flags,
environment variables, logs, or config files. When payment-provider or regulatory
requirements demand a hosted checkout, secure payment setup, bank authorization,
or SCA-style browser approval, the CLI should initiate and track that flow rather
than becoming a payment-data collection surface.

When an external browser or hosted provider page is unavoidable, the CLI should
still own the workflow. It should create the session, show the URL or open it on
request, return a resumable session ID, poll or watch status, and emit
machine-readable completion state.

Crypto payment support should sit alongside traditional payment methods rather
than replacing them. The initial posture is provider-mediated checkout, invoice,
or subscription payment with no Witself-managed wallet custody. Witself should not
collect wallet seed phrases, private keys, or raw wallet credentials in CLI flags,
environment variables, config files, logs, support tickets, or billing metadata.

Candidate crypto rails include stablecoins such as USDC or USDT where a payment
provider supports them, and native ETH as a source asset only when a provider can
safely quote, confirm, and settle the payment. Witself should prefer fiat or
provider-managed stablecoin settlement over direct treasury management until there
is a deliberate finance, tax, and compliance design.

Witself should not introduce a Witself utility token for v0 or v1. A utility token
may remain a future research topic tracked in
[post-v0-roadmap.md](post-v0-roadmap.md), but it must not be required for account
setup, billing, agent access, memory recall, fact reads, messaging, CLI use, or
MCP use.

The CLI is the primary control plane. A future web UI may exist for convenience as
a post-v0 option, but it should not be required for managed-service
administration.

Service-admin command requirements:

- All account, realm, billing, payment, support, and usage commands should support
  `--json`.
- Commands whose availability may differ by backend should check backend
  capabilities and fail with a deterministic `unsupported_operation` error when
  unsupported.
- Risky account, realm, agent, token, policy, group, billing, payment, and support
  mutations should support preview or `--dry-run` behavior where practical.
- Destructive or integrity-impacting identity mutations should support `--dry-run`
  for v0 where practical, specifically memory forget/restore/delete, cross-agent
  curate/forget, fact delete, primary promotion, policy delete, group member
  removal, and group deletion.
- Dry runs should validate inputs, authorization, conflicts, quotas, and provider
  prerequisites, but should not persist state, generate tokens, create hosted
  provider sessions, charge payment methods, send messages, or send
  customer/support notifications.
- Destructive, billing-impacting, or support-sensitive actions should require
  explicit confirmation unless `--yes` is supplied by an authorized caller.
- Billing-impacting payment changes should require or prompt for an audit
  `--reason`; read-only billing commands should not require one.
- Sensitive operator actions and cross-agent mutations should require an audit
  `--reason`.
- Output should include stable IDs and next-step commands so humans and AI
  assistants can continue a workflow deterministically.
- There should be no privileged AI-only path. AI-assisted administration should
  compose the same public CLI and backend API that human operators use.

### Memory Model

A memory is free-form self-content owned by an agent (or, in the group case, by a
security group). It is one of the two first-class identity payload types
alongside facts. The memory model is tracked in [memory-model.md](memory-model.md).

A memory has:

- A stable `id` with the `mem_` prefix.
- An owning agent (or owning group), and the enclosing realm.
- `content`: free-form text. This is the primary payload.
- `kind`: a convention-driven type such as `episodic`, `semantic`, `profile`, or
  `note`. Kind is a label used for filtering and ranking, not a fixed storage
  column with a closed enum. Unknown kinds are allowed.
- `tags[]`: an array of short string tags for filtering.
- `source`: first-class provenance/authorship for the memory. Canonical values are
  `self` (the owning agent authored it), `agent:<name>` (a cross-agent
  contribution), `operator`, and `import:<file>` (ingested from a file). Makes
  human-, agent-, and import-authored records distinguishable so the self-digest
  and `memory consolidate` can prioritize and never silently overwrite human or
  imported intent.
- `salience`: an importance/weight hint (numeric) used in recall ranking.
- `links[]`: references to other memories or facts, expressed as `witself://`
  references (see [Identity References](#identity-references)).
- `sensitive`: an optional PII marker. Sensitive memory content is redacted in
  list/scan output by default and is handled carefully in audit and logs.
- Timestamps: `created_at`, `updated_at`, `last_accessed_at`.
- Versioned edit history (see below).

Memory lifecycle:

- `add` — create a memory. The creating agent is the owner unless an authorized
  caller targets a group via `--group`.
- `adjust` — edit the content, kind, tags, source, salience, links, or sensitive
  marker. Adjust appends to versioned edit history; prior versions are retained.
- `read`/`get` — deterministic retrieval by `id`. Reading updates
  `last_accessed_at`.
- `recall` — semantic-by-default search over the caller's accessible memories
  (see [Memory Recall and Embeddings](#memory-recall-and-embeddings)).
- `list` — enumerate memories with metadata and filters (kind, tag, owner, source,
  time, sensitive), with cursor pagination.
- `forget` — soft delete (tombstone). Reversible within a retention window. This
  is the default destructive path; it is audited and reversible.
- `restore` — undo a forget within the retention window.
- `delete` — hard delete. Explicit, guarded, audited, and irreversible. Requires
  confirmation and, for cross-agent or operator deletes, an audit `--reason`.

Edit history:

- Every `adjust` records a new version with a monotonically increasing version
  number, the actor (derived from token), a timestamp, and the changed fields.
- History is retained for audit, conflict detection, safe operator recovery, and
  export. History is included in identity export.
- History entries inherit the `sensitive` posture of the memory.

Size limits (v0 defaults):

- Maximum memory `content` size: 256 KiB before storage overhead.
- Maximum `tags` per memory: 64.
- Maximum tag length: 128 characters.
- Maximum `links` per memory: 256.
- Maximum `source` length: 1 KiB.
- Memory names are not unique within an agent; identity is by `id`. Memories are
  addressed by id, recalled semantically, and filtered by metadata, not by a
  unique name. (Facts, by contrast, are name-unique; see
  [Facts Model](#facts-model).)

### Memory Recall and Embeddings

Recall is **semantic by default** and is the core Witself differentiator. The
recall and embedding model is tracked in [memory-model.md](memory-model.md).

Recall posture:

- Each memory carries an embedding vector computed from its `content` (and
  optionally tags/kind) at write time.
- `memory recall <query>` performs vector similarity search over the caller's
  accessible memories, then blends in keyword, tag, kind, and time filters.
- Hybrid ranking combines, at minimum: vector similarity, lexical/keyword match,
  tag and kind match, recency (favoring recent `created_at`/`last_accessed_at`),
  and `salience`. Weights are tunable; defaults are documented in
  [memory-model.md](memory-model.md).
- Plain `read`/`get` by `id` and `list` by metadata are always available and do
  not require the embedding provider.
- Recall over another agent's memories requires a policy granting `read` on that
  target (see [Cross-Agent Access Policy](#cross-agent-access-policy)) and is
  metered as a cross-agent access.

Embedding provider abstraction (mirrors the way Witpass abstracted KMS):

- Providers: `voyage` (default; the Anthropic-recommended embedding family),
  `openai`, and `local-dev`.
- Provider and model are selectable via `WITSELF_EMBEDDINGS_PROVIDER` and
  `WITSELF_EMBEDDINGS_MODEL`.
- `local-dev` is for tests, demos, and `witself-server serve --dev`; it is a
  deterministic or low-cost local embedder and is not a production provider.
- Provider choice is capability-gated. The capabilities contract reports the
  active provider, model, and vector dimensionality.

Storage and degradation:

- Embedding vectors are stored in Postgres via pgvector. Vector storage size is a
  metered dimension.
- If the embedding provider is unavailable or disabled, recall degrades
  deterministically to keyword/tag/kind/time ranking, and the capability contract
  reports that semantic recall is degraded. Recall never silently returns
  unranked or empty results without surfacing the degraded state.
- Re-embedding on provider/model change is an explicit, audited maintenance
  operation, not an automatic side effect.

Sealed-plane carve-out (#4): the embedding and recall path is **open-plane only**.
Secret field values and TOTP seeds are never embedded and are never returned by
semantic recall, `self show`, or any ranking path. There is no vector
representation of sealed material. See
[Sealed-Plane Invariants](#sealed-plane-invariants).

### Memory Self-Management and Hydration

Decision: agent self-managed memory and context hydration are **in v0**. Witself is
a *service* the agent must be taught to call, unlike `CLAUDE.md`/`AGENTS.md`, which
a harness auto-loads. Without an always-loaded self-digest, a teaching layer, a
file bridge, and a few convergent capture/maintenance primitives, the core product
is unreliable for the agents it exists to serve. The canonical bridge and teaching
doc is [context-hydration.md](context-hydration.md).

Hydration and self-management verbs (each a thin, tested path over the existing
memory/fact core — never a bypass of validation, limits, scopes, or audit):

- `remember` — the dead-simple quick-add. It **auto-routes**: a clear name→value
  assertion upserts a fact (idempotent by name); anything else adds a verbatim
  memory with dedup/supersede. It is the tested primary capture path, not a thin
  alias to be left to rot; `fact set` and `memory add` remain for explicit control.
- `self show` — the always-loaded self-digest: primary facts first, then top-N
  salient memories, then a one-line index of kinds/tags/counts. It is the MCP
  analogue of an auto-loaded `CLAUDE.md` head: cheap, never requiring the embedding
  provider, and hard-capped (default ~8 KiB / ~200 lines, configurable). When
  capped it sets `elided=true` and points to `memory recall` rather than silently
  truncating.
- `session start` / `session end` — multi-session bootstrap. `start` hydrates
  identity, open goals, and last progress in one round-trip; `end` persists a
  progress memory (kind `session`) and updates open goals. Resuming is one call,
  not a list-then-recall crawl.
- `memory consolidate` — the garbage-collection verb. It merges near-duplicate
  memories, supersedes stale ones, **surfaces** (does not auto-pick) conflicting
  facts, and trims the digest/index. It is guarded (`--dry-run` defaults true),
  audited, excluded in `--read-only` MCP mode, and respects provenance — it never
  silently overwrites human- or import-authored records.
- `digest emit` / `ingest` — the two-way file bridge. `emit` renders the
  self-digest as a `CLAUDE.md`/`AGENTS.md` fragment with provenance comments so
  file-load harnesses get Witself-backed identity for free. `ingest` parses
  `CLAUDE.md`/`AGENTS.md`/`GEMINI.md`: name→value lines become facts (upsert),
  prose paragraphs become memories, everything tagged `source=import:<file>`, with
  dedup/upsert preventing re-import duplication. This makes Witself a good citizen
  of the `AGENTS.md` ecosystem, not a competitor to it.
- `bootstrap-instructions` — prints the paste-able teaching stanza; `witself setup
  --write-agents-md` installs it into the project `AGENTS.md`.

Teaching layer (three reinforcing surfaces that all say the same thing, because a
service the agent forgets to call is worthless):

1. **MCP server `instructions` field** — returned on connect by `witself mcp
   serve`. This is the canonical standing protocol (pinned verbatim in
   [mcp-tools.md](mcp-tools.md) and [context-hydration.md](context-hydration.md)):
   call `self show` and `memory recall` before acting; `remember` after learning a
   durable fact, preference, decision, or reusable context; `adjust`/`forget`
   rather than adding a contradicting memory; and flush state with `session end` /
   `remember` before long operations, assuming context may be cleared at any moment.
2. **Tool descriptions as instruction** — every memory/fact/self tool's prose
   embeds an explicit when-to-call trigger list (recall/`self show` at task start
   and whenever the past is referenced; `remember` on learning/being asked/finding
   reusable context/finding a record wrong; `adjust`/`forget` instead of
   contradicting; `consolidate` when memory feels noisy). Signatures stay tiny so
   the trigger text dominates.
3. **Bootstrap stanza** — a paste-able `AGENTS.md`/`CLAUDE.md` block (emitted by
   `bootstrap-instructions`) carrying the same recall-before-act,
   write-after-learn, consolidate-when-noisy heuristics, so the habit installs
   whether the agent is taught via MCP or via the file ecosystem.

Cross-cutting contract changes that make these verbs self-verifiable and safe:

- **Echo on every mutation.** The mutation result gains a deterministic,
  human-readable `echo` string (for example, `Remembered fact display-name=Atlas`
  or `Added mem_123 (kind=profile, salience=0.6)`) so the model can self-verify and
  chain operations.
- **Dedup/supersede on write.** `memory add` (and `remember` when it routes to a
  memory) checks for near-duplicates; on a hit it returns the existing `mem_` id
  plus a `memory_duplicate`/`memory_merged` warning in `warnings[]` and a
  `duplicate_of` reference instead of silently creating a near-duplicate. `fact set`
  is already upsert.
- **Provenance/authorship as first-class.** The memory and fact `source` field (see
  [Memory Model](#memory-model) and [Facts Model](#facts-model)) carries
  `self`/`agent:<name>`/`operator`/`import:<file>` so the digest and consolidate can
  prioritize and never silently overwrite human intent. This is the lightweight v0
  seed of the post-v0 provenance-and-lineage feature.

Salient-digest selection: the salient set for `self show` is the top-N memories by
a blended score of salience and recency (favoring pinned kinds such as `profile`
and `session`), excluding archived/forgotten records. It is deterministic and never
calls the embedding provider, so the digest works even when embeddings are degraded.
The selection rule is defined once in [memory-model.md](memory-model.md).

Scopes and read-only: these verbs reuse the existing scopes (no new scope). `self
show`/`session start`/`digest emit` need `memory:read` + `fact:read`;
`remember`/`session end`/`ingest` need `memory:create`/`fact:create`;
`consolidate` needs `memory:update` (plus `memory:forget` for supersede).
`--read-only` MCP mode excludes the mutating verbs (`remember`, `session end`,
`consolidate`, `ingest`) while keeping `self show`, `session start`, `recall`, and
`digest emit`.

### Facts Model

A fact is a name→value pair: the canonical, queryable identity card for an agent.
Facts are deterministic and addressable by name, in contrast to memories, which
are addressed by id and recalled semantically. The facts model is tracked in
[facts-model.md](facts-model.md).

Example facts: `email`, `account-number`, `created-date`, `display-name`,
`home-region`.

A fact has:

- A stable `id` with the `fact_` prefix.
- An owning agent (or owning group), and the enclosing realm.
- `name`: unique per owner, optionally namespaced (for example `aws/account-id`).
- `value`: the fact's value.
- `primary`: boolean. Marks the canonical/identity-defining value of its logical
  kind.
- `sensitive`: boolean. Marks PII that should be redacted by default in
  list/scan/show output.
- `format`: an optional type hint such as `string`, `email`, `url`, `date`, or
  `number`, used for validation and display.
- `source`: first-class provenance/authorship, sharing the memory `source`
  vocabulary: `self`, `agent:<name>`, `operator`, and `import:<file>`. Lets the
  digest and `memory consolidate` prioritize and avoid overwriting human- or
  import-authored facts.
- Timestamps: `created_at`, `updated_at`.
- Versioned edit history (same shape as memory edit history).

Lookup and lifecycle:

- Lookup is **deterministic by name**: `fact get email` returns the one true value
  for the caller's identity.
- `fact set NAME VALUE` creates or updates a fact (upsert by name within the
  owner). `fact list` enumerates with filters. `fact delete NAME` removes a fact
  (guarded, audited).
- Fact names are unique within the owning agent (or owning group). Different
  agents may reuse the same fact name because ownership disambiguates them.

Primary flag, promotion, and uniqueness:

- `primary` marks the canonical value of a logical kind. A logical kind is the
  fact's base name (or an explicitly declared kind), so setting a second
  `email --primary` demotes the prior primary `email`.
- Setting a fact `--primary` is an atomic promotion that demotes any prior primary
  of the same logical kind for the same owner. At most one primary per logical
  kind per owner.
- Primary facts are the agent's identity anchors and are surfaced first in
  `whoami`, profile, and export output.

Readability and sensitivity:

- Facts are ordinary readable identity data and are returned in `show`/`list` by
  default. Only `sensitive` facts are redacted by default; reading a sensitive
  fact's value is an ordinary authorized read, not a reveal ceremony.
- There is **no** secret-style reveal ceremony, no separate sensitive value-size
  budget, and no value redaction state machine. Sensitivity is a display/PII flag,
  not an encryption boundary. Optional field-level encryption of `sensitive` fact
  values is a capability, not the default; see
  [Data-At-Rest Note](#data-at-rest-note).

### Cross-Agent Access Policy

Authorization for cross-agent identity access moves from ad-hoc grants to
evaluable **Policy** objects with a default-deny stance. The policy engine is
tracked in [access-policy.md](access-policy.md); it reuses the authorization and
audit scaffolding that Witpass kept in its encryption-model doc.

A Policy binds:

- `id` with the `pol_` prefix, and the enclosing realm.
- `subject`: an agent or a security group.
- `permission`: one verb (see below).
- `target`: an agent or a security group.
- `scope`: memories, facts, or both.
- Optional `filter`: by memory `kind`/`tag`, fact `name`/namespace, or `sensitive`
  flag.
- `effect`: `allow` (v0 default and only effect for granted policies; absence of a
  matching allow is deny).
- Metadata: created/updated timestamps, creating principal, and an optional
  human-readable description.

Permission verbs, escalating in danger:

- `read` — read another agent's or group's memories and facts, including semantic
  recall over them and reading sensitive facts.
- `contribute` — add new memories or facts to another agent's or group's store.
- `curate` — adjust/merge/re-tag existing memories or facts owned by another agent
  or group, including editing salience, links, and primary flags.
- `forget` — soft-delete or prune another agent's or group's memories and facts
  (reversible window). Hard delete across agents is a further-guarded step and is
  never the default.

Default deny:

- With no matching `allow` policy, cross-agent access is denied. An agent always
  retains full access to its own memories and facts subject to its own token
  scopes.

Guardrails (reuse Witpass patterns):

- `curate` and `forget` across agents require an audit `--reason`, support
  `--dry-run`, and require confirmation unless `--yes` is supplied by an
  authorized caller.
- `contribute` across agents is attributed and audited; the resulting memory/fact
  records the contributing agent in `source`.
- Cross-agent deletes are soft/tombstoned by default and reversible within the
  retention window. Hard cross-agent delete requires an explicit, separately
  guarded step with `--reason` and confirmation.
- Every cross-agent mutation is fully attributed in audit, for example "memory
  `mem_…` of agent A was pruned by agent B under policy `pol_…`".

Policy evaluation and tooling:

- `policy test` evaluates whether a given subject/permission/target/scope would be
  allowed under current policy, returning the deciding policy id or a deny reason.
  This is the canonical dry-run for access decisions and is available via CLI and
  MCP.
- Operators can always manage and access identity data within their realm
  (operator override). Operator override is audited like any agent action and is
  subject to the same `--reason` requirements on destructive/cross-agent actions.

Threat framing:

- Witself's threat model flips from confidentiality (Witpass) to **integrity and
  authenticity** of identity data. Headline risks are memory-poisoning,
  unauthorized curation or forgetting, cross-agent write abuse, and sender
  spoofing in messaging. The threat model is tracked in
  [threat-model.md](threat-model.md).

### Security Groups

A security group is a named set of agents within a realm. It is both a policy
subject and a policy target, and it can own group-scoped shared memories and
facts. Security groups are tracked in [security-groups.md](security-groups.md).

A group has:

- A stable `id` with the `grp_` prefix, and the enclosing realm.
- `name`: unique within the realm.
- `members[]`: the agents in the group.
- `admins[]`/owner: agents allowed to manage membership under `group:manage`.
- Policies bound to it as subject and/or target.
- Timestamps and an optional description.

Group behavior:

- Membership is managed by operators and by agents holding `group:manage`.
- As a policy **subject**, a group grants every member the policy's permission on
  the target (for example, group `analysts` may `read` agent `archivist`).
- As a policy **target**, a group is the recipient of a permission (for example,
  agent `coordinator` may `contribute` to group `shared-context`).
- v0 supports **group-scoped shared memories and facts** (collective memory): a
  memory or fact may be owned by a group rather than a single agent. Group members
  access group-owned identity data according to the group's policies. Group-owned
  records use the same `mem_`/`fact_` shapes with a group owner.
- Group-owned destructive actions follow the same guardrails as cross-agent
  actions: `--reason`, `--dry-run`, confirmation, and soft-delete by default.

### Inter-Agent Messaging

Agents exchange durable messages with other agents and groups. Messaging is fully
in scope for v0. The messaging model is tracked in
[inter-agent-messaging.md](inter-agent-messaging.md).

Model:

- Mailbox/queue per recipient. Messages are durable and survive process and pod
  churn.
- Delivery with at-least-once semantics, per-recipient (and per-conversation)
  ordering, and explicit read/acknowledgement state.
- A message addressed to a group is fanned out to current group members, with
  per-member delivery and ack state.

A message has:

- A stable `id` with the `msg_` prefix, and the enclosing realm.
- `from`: the sender agent. **Always derived from the authenticated token, never
  from input.** Sender forgery is structurally impossible through the API.
- `to`: a recipient agent or group.
- `subject`/`kind`: a short classification.
- `body`: free-form text.
- `payload`: an optional structured payload.
- `thread_id` (a.k.a. conversation id): optional, for ordered conversations.
- `created_at`, delivery state, and per-recipient read/ack state.

Trust boundary and threats:

- Messages can carry instructions and data into another agent, so message content
  is **untrusted input** to the receiving agent. Receiving agents and their
  runtimes must treat message bodies and payloads as untrusted, especially when a
  message would drive a memory or fact write.
- Threats: spoofing (mitigated by token-derived `from`), interception, injection,
  and memory-poisoning via message-driven writes. A message granting no policy
  cannot itself authorize a cross-agent write; writes still require policy.
- Rate limits apply to send and delivery. `message:send` and `message:read`
  scopes gate the surface. Send, deliver, and read events are audited.

Surfaces:

- CLI: the `message` group (`send`/`list`/`read`/`ack`).
- MCP: `witself.message.send/list/read`.
- API: `/v1/messages` plus colon actions (for example
  `/v1/messages/{message_id}:ack`).
- A stable JSON Message shape shared by all frontends.
- Messages sent and delivered are metered billing dimensions.

### Identity Export and Import

First-class structured/plaintext identity export and round-trippable import is a
headline Witself feature and the inverse of Witpass's encrypted-only export
stance. Export/import is tracked in [backup-and-recovery.md](backup-and-recovery.md).

Export posture:

- `witself export` produces a structured, human-readable, plaintext export of an
  agent's self: all memories (with content, kind, tags, source, salience, links,
  timestamps, and **edit history**), all facts (with values, `primary` flags,
  `sensitive` flags, format hints, and edit history), and the agent's identity
  anchors.
- For operators, export can include realm-level context: policies, security-group
  membership, and group-owned memories and facts.
- Export is round-trippable: `witself import` restores an exported self into the
  same or a different agent/realm, preserving primary flags, sensitive markers,
  links, and (where chosen) edit history.
- Export defaults to JSON using the `witself.v0` schema version; a directory/file
  layout suitable for diffing and version control is also supported.
- `sensitive` facts and memories are exported in clear by default (Witself
  embraces plaintext export), but the CLI requires an audit `--reason` and warns
  when exporting `sensitive` records. Operators may scope exports to non-sensitive
  records.
- Identity references (`witself://…`) are preserved on export and re-resolved on
  import; dangling references are reported, not silently dropped.

Sealed-plane carve-out (#2): `witself export` excludes the sealed plane. Secret
field values and TOTP seeds are **never** present in the plaintext export, the
self-digest, `digest emit`, or `ingest`. Secret backup is a separate,
encrypted-only path (envelope ciphertext + KMS key identity, never plaintext)
behind an explicit, audited flag, covered in
[backup-and-recovery.md](backup-and-recovery.md). A secret reference may appear in
a memory or fact, but it resolves to plaintext only through the reveal ceremony,
never through export.

Import posture:

- Import is idempotent by stable id where ids are preserved, and supports a
  rename/remap mode when importing into a different agent or realm.
- Import is audited and supports `--dry-run` to preview created/updated/conflicting
  records without persisting.

### Identity References

Witself supports stable references so memories, facts, messages, scripts, config
files, and MCP tools can point at identity data without copying it. The reference
scheme is `witself://`. Reference parsing and resolution are tracked in
[json-contracts.md](json-contracts.md).

Reference forms (the final path component is the leaf; URL-encode if needed):

- Current authenticated agent's memory by id or path:
  `witself://memory/<path-or-id>`
- Current authenticated agent's fact by name: `witself://fact/<name>`
- A specific agent's fact (cross-agent, policy-gated):
  `witself://agent/<agent>/fact/<name>`
- A specific agent's memory (cross-agent, policy-gated):
  `witself://agent/<agent>/memory/<id>`
- Group-scoped memory or fact: `witself://group/<group>/memory/<id>` and
  `witself://group/<group>/fact/<name>`

Reference rules:

- References resolve only through authorized commands or MCP tools. Resolution
  enforces the same authorization as a direct read; a cross-agent or cross-group
  reference resolves only when policy permits.
- References used in memory `links[]` are validated at write time and re-checked at
  resolve time.
- `witself reference parse` and `witself reference resolve` (CLI and MCP) expose
  reference handling deterministically.
- Sealed-plane secret references are a separate, value-gated family with their own
  forms and resolution rules; see [Secret References](#secret-references). Open
  identity references never resolve secret material, and secret references never
  resolve open-plane memories or facts.

### Sealed-Plane Invariants

Decision: the sealed plane (secrets + TOTP seeds) is governed by a small set of
non-negotiable carve-outs that hold everywhere in this spec. These are the
consolidation invariants from folding Witpass into Witself. Anything touching
secret material must honor all of them.

- **Never embedded, never recalled.** Secret field values and TOTP seeds are never
  embedded and are never returned by semantic recall. The embedding/recall path is
  open-plane only. (Carve-out #4; see
  [Memory Recall and Embeddings](#memory-recall-and-embeddings).)
- **Never in the digest, never ingested.** Secrets and TOTP seeds are never in the
  self-digest (`self show`), never rendered by `digest emit`, and never created by
  `ingest` from `CLAUDE.md`/`AGENTS.md`/`GEMINI.md`. Credentials in those files are
  ignored, not absorbed. (Carve-out #4; see
  [context-hydration.md](context-hydration.md).)
- **Never plaintext-exported.** `witself export` excludes the sealed plane.
  Identity (memory + fact) plaintext export stays the headline feature; secret
  backup is encrypted-only (envelope ciphertext + KMS key identity, never
  plaintext) behind an explicit, audited, separate flag. The self-digest, `digest
  emit`, and `ingest` never carry secrets. (Carve-out #2; see
  [Backup, Export, And Recovery](#backup-export-and-recovery) and
  [backup-and-recovery.md](backup-and-recovery.md).)
- **Reveal-gated values.** Secret field values and TOTP codes are returned only by
  the explicit, audited value-returning operations (`witself secret reveal`,
  `witself totp code`, and value-returning `reference resolve`) under the reveal
  ceremony. The "no reveal ceremony / data is plainly readable" stance applies to
  the OPEN plane only; memories and facts have no reveal, secrets do. (Carve-out
  #3; see [Reveal and Value-Returning Operations](#reveal-and-value-returning-operations).)
- **Confidentiality dual posture.** The threat model protects both the integrity
  and authenticity of open-plane identity and the confidentiality of sealed-plane
  credentials. Both planes' assets, attackers, and controls are in scope. (Carve-out
  #5; see [threat-model.md](threat-model.md).)

### Secret Model

A secret is sealed-plane credential material owned by an agent (or, in the group
case, by a security group). It is the sealed-plane sibling of the open-plane
memory and fact payloads. The secret model is tracked in
[secret-model.md](secret-model.md) and its data shapes in
[data-model.md](data-model.md).

Witself is a general-purpose credential vault. The data model is field-based, not
fixed columns such as `username`/`password`. Supported credential material
includes usernames and passwords, API keys, access/refresh tokens, recovery codes,
TOTP seeds and 2FA metadata, OAuth client IDs and secrets, SSH private keys, TLS
private keys and certificates, signing keys, environment variables, service
account credentials, database URLs and connection strings, login URLs and account
metadata, and arbitrary structured fields.

A secret has:

- A stable `id` with the `sec_` prefix, an owning agent (or owning group), and the
  enclosing realm. Owner is `agent` or `group` — there is no separate "shared"
  scope; the former Witpass vault-shared secret is a **group-owned** secret (see
  [Ownership Unification](#ownership-unification)).
- A stable name or path, unique within the owning agent (or owning group).
- A required human-readable description.
- An optional template: `login`, `api-key`, `ssh-key`, `certificate`, `env`, or
  `generic`. Templates suggest conventional field names and validation; they are
  usability conventions, not storage columns. Every secret may carry arbitrary
  named fields.
- A flat set of named **fields** (`fld_` prefix), each with a `sensitive` marker.
  Non-sensitive fields (for example `username`, `url`, `issuer`) display in `secret
  show`. Sensitive fields (for example `password`, `api-key`, `private-key`,
  `totp-seed`, `recovery-code`) are redacted by default and returned only through
  the reveal ceremony.
- Tags, access grants (`grt_` prefix), timestamps, and version/audit history.

Only `name` and `description` are required; all credential fields are optional and
context-dependent. Sensitive field values are stored as **envelope ciphertext**
under the per-secret/field DEK — never as plaintext database columns, never
base64-as-security (see [Encryption (Two-Tier)](#encryption-two-tier)). Plaintext
secret material never enters ordinary databases, logs, audit records, analytics,
or errors.

Default v0 size limits (tracked in
[secret-size-and-attachments.md](secret-size-and-attachments.md)):

- Maximum sensitive field value size: 64 KiB before encoding.
- Maximum non-sensitive field value size: 16 KiB.
- Maximum total inline secret payload: 256 KiB before storage overhead.
- Maximum field count per secret: 100.
- Maximum secret name length: 255 characters.
- Maximum description length: 4 KiB.

Attachments (`att_` prefix) are not required for the first sealed-plane slice.
When added, attachments use encrypted object/blob storage rather than large
ordinary database columns.

### Secret Lifecycle

Secrets support a complete, audited lifecycle. Every value-bearing step honors the
[Sealed-Plane Invariants](#sealed-plane-invariants).

- `create` — create a secret. The creating agent is the owner unless an authorized
  caller targets a group via `--group`.
- `show` — redacted details: metadata and non-sensitive fields, sensitive fields
  masked. This is the ordinary read; it is not a reveal.
- `reveal` — return one sensitive field's plaintext under the reveal ceremony.
  Explicit, audited, value-returning (see
  [Reveal and Value-Returning Operations](#reveal-and-value-returning-operations)).
- `update` — edit fields and description; keeps version/history.
- `rename` — rename within the owning agent (or group).
- `copy` — copy within an agent, to another agent, or to group-owned scope when
  allowed. Copying sensitive values requires confirmation and an audit `--reason`.
- `archive` / `restore` — soft archive and restore.
- `delete` — permanent delete when allowed; guarded, audited, confirmation
  required.
- `grant` / `revoke` — grant or revoke another agent's or group's access to a
  secret (see [Secret Authorization & Roles](#secret-authorization--roles)).
- `list` / `scan` — enumerate secrets with metadata and filters; `scan` is the
  realm-wide redacted inventory for operators, sensitive values never returned.

Mutations keep enough version/history for audit, conflict detection, and safe
operator recovery. Sensitive historical values stay protected by the same reveal
rules as current values. Broad delete/copy operations that touch sensitive values
require confirmation and an audit `--reason`.

### Secret References

Witself supports stable sealed-plane references so scripts, config files, MCP
tools, and `witself run` can point at a secret field without embedding plaintext.
The scheme is `witself://` (the rename of the former Witpass `wp://`), kept
distinct from open-plane identity references by the `secret` segment.

Reference forms (the final path component is the field; URL-encode if needed):

- Current authenticated agent (or a granted view):
  `witself://secret/<secret-path>/<field>`
- A specific owning agent (operator/admin, or granted):
  `witself://agent/<agent>/secret/<secret-path>/<field>`
- Group-owned secret (the rename of Witpass `wp://shared/…`):
  `witself://group/<group>/secret/<secret-path>/<field>`

Examples:

- `witself://secret/github/builder/password`
- `witself://agent/browser-agent/secret/github/builder/password`
- `witself://group/platform/secret/org-readonly-token/token`

Reference rules:

- References resolve only through explicit value-returning paths — `secret reveal`,
  `totp code`, `witself run`, or an authorized MCP `reference.resolve` — and
  resolution is audited as a secret read.
- Resolution enforces the same authorization as a direct reveal: a cross-agent or
  group reference resolves only when a grant or realm role permits it.
- References never let a caller cross agent boundaries without permission, and the
  `--no-value-tools` MCP mode disables value-returning resolution (see
  [MCP Boundary](#mcp-boundary)).

### TOTP and 2FA

Witself acts as the authenticator application for agents. It stores TOTP setup
material and returns a current one-time code at login time. The TOTP model is
tracked in [totp-2fa.md](totp-2fa.md).

Model and flow:

- A TOTP enrollment has a stable `id` with the `totp_` prefix and belongs to a
  secret (or stands alone as sealed material). Metadata includes issuer, account
  label, algorithm, digit count, and period.
- Enrollment accepts an `otpauth://` URL, a manual Base32 seed, a seed file, or a
  QR-code image when image parsing is available.
- `totp code` returns the current generated code through the authorized CLI/MCP
  flow. The normal agent-facing surface returns **codes, not seeds**.
- The TOTP seed is high-value sealed material: it is envelope-encrypted, never
  embedded, never recalled, never in the digest, and never plaintext-exported.
  Seed import, setup, backup, reveal, or export requires the more privileged
  `totp:enroll` path; ordinary login use needs only `totp:code`.
- `totp show` returns redacted enrollment metadata (no seed); `totp delete`
  removes an enrollment.

External-approval 2FA (SMS, email, push, passkey, hardware key) is post-v0; see
[post-v0-roadmap.md](post-v0-roadmap.md). V0 makes the TOTP authenticator role
work well inside Witself.

### Password Generation

Witself includes built-in password generation for agent signup and rotation
workflows. The generator supports length, character classes, special-character
inclusion, ambiguous-character avoidance, human-readable passphrase-style
generation, and machine-readable JSON output for unattended use.

- Surface: `witself password generate` (CLI) and `witself.password.generate` (MCP,
  where policy allows).
- Generated material is offered for storage as a sealed secret field; the
  generator does not write a plaintext value into the open plane.

### Runtime Injection (`witself run`)

`witself run` resolves sealed-plane secret references and injects the resolved
values into a child process's environment or arguments without printing plaintext.

- It resolves `witself://secret/…` references the same way `secret reveal` does,
  subject to the same authorization, grants, and audit.
- Injected values are passed to the child process only; they are never written to
  logs, audit records, or the open plane. Runtime injection is a value-returning,
  reveal-gated operation and is metered as `runtime_injection`.
- `--no-value-tools` MCP mode and `--read-only` posture both disable
  value-returning resolution; runtime injection is a CLI-side capability.

### Encryption (Two-Tier)

Decision: Witself runs a **two-tier** data-at-rest model, one tier per plane. This
is the encryption reconciliation from the consolidation (carve-out #1).

- **Open plane** uses ordinary data-at-rest protection (managed RDS/disk
  encryption, or self-host-owned disk encryption). Memory content, fact values,
  and embedding vectors are ordinary identity data. There is no KMS dependency, no
  envelope, and no reveal for the open plane. Optional field-level encryption of
  `sensitive` facts remains a capability, not a pillar (see
  [Data-At-Rest Note](#data-at-rest-note)).
- **Sealed plane** uses KMS-backed envelope encryption: a root CMK wraps a
  per-realm KEK (`kek_`), which wraps a per-secret/field DEK (`dek_`). Field
  ciphertext uses an AEAD (`XCHACHA20_POLY1305` or `AES_256_GCM`). Plaintext
  secret material never lands in ordinary database columns.
- **Hybrid custody behind one capability switch.** `client_side_decrypt` is the
  default where the client (CLI, local MCP runtime, local agent) can hold or derive
  key material; the server returns ciphertext + envelope metadata and the client
  unwraps. `server_side_decrypt` is the capability-gated path for managed
  token-only ephemeral pods that hold only a bearer token and cannot reach KMS — on
  that path the server transiently sees the DEK and plaintext, advertised honestly
  via the capability flag and flagged on the reveal/code audit event. The trade-off
  is analyzed in [key-hierarchy.md](key-hierarchy.md) and
  [encryption-model.md](encryption-model.md).
- **KMS as a conditional dependency.** KMS providers are `aws-kms`, `gcp-kms`,
  `azure-key-vault`, and `local-dev` (the local-dev provider wraps the same
  KEK/DEK structure with a `WITSELF_PASSPHRASE_FILE`-derived key, for tests and
  `witself-server serve --dev` only). KMS is required and a readiness gate **only
  when the sealed plane is enabled**; an open-plane-only deployment requires no
  KMS. pgvector remains a hard gate for the open plane regardless.
- **Crypto-shred posture.** Loss of the CMK/KMS access renders sealed secret
  values unrecoverable (crypto-shred). This does **not** affect the open plane,
  whose plaintext identity data is recoverable from ordinary backups and export.

The encryption model is tracked in [encryption-model.md](encryption-model.md), the
key custody analysis in [key-hierarchy.md](key-hierarchy.md), and the storage and
KMS provider configuration in [storage.md](storage.md).

### Key Hierarchy

Decision: the sealed plane uses a three-level key hierarchy, scoping who can call
unwrap and bounding per-realm blast radius.

```
CMK (KMS Customer Master Key)
  └─ wraps ─> per-realm KEK (kek_...)            one per realm, in realm_keys
       └─ wraps ─> per-secret/field DEK (dek_...)  wrapped_dek in secret_deks
```

- **Root CMK.** A KMS Customer Master Key under the `WITSELF_KMS_PROVIDER`
  abstraction (`aws-kms` via an ARN in `WITSELF_KMS_KEY_ID` for managed v0; the
  other providers for self-hosting/BYOK; `local-dev` for dev). In managed mode the
  CMK lives in Witself's KMS; in self-hosted/BYOK mode it lives in the operator's
  own KMS.
- **Per-realm KEK** (`kek_` prefix). A 256-bit symmetric KEK generated per realm
  and stored wrapped in `realm_keys` (the rename of the Witpass per-vault KEK in
  `vault_keys`). Per-realm KEKs give per-tenant cryptographic separation and bound
  the blast radius of a compromised DEK.
- **Per-secret/field DEK** (`dek_` prefix). The DEK that the AEAD uses on field
  ciphertext, stored wrapped in `secret_deks`. V0 ships per-secret DEKs;
  per-field DEKs are opt-in/deferred.

`key.rotated` (KEK rotation) is an audited maintenance event. The full custody
analysis, v0 crypto subset, and BYOK/per-realm-CMK deferrals are in
[key-hierarchy.md](key-hierarchy.md).

### Reveal and Value-Returning Operations

Decision: the sealed plane has an explicit, audited reveal ceremony; the open
plane does not. This is carve-out #3.

- The value-returning operations are `witself secret reveal` (one sensitive field),
  `witself totp code` (a current code), value-returning `reference resolve`, and
  `witself run` injection. Each is explicit, audited, and metered (`secret_read`,
  `totp_code`, `runtime_injection`).
- The reveal ceremony requires the `secret:reveal` (or `totp:code`) scope, honors
  grants and realm roles, records a `secret.reveal` / `totp.code` audit event, and
  carries the `server_side_decrypt` flag on that event when the server-mediated
  path was used.
- The open plane has **no reveal**. `memory read` and `fact get` return values
  through an ordinary authorized read; `sensitive` memories/facts use lightweight
  display redaction, not encryption and not a reveal. The "data is plainly
  readable" statements in [memory-model.md](memory-model.md) and
  [facts-model.md](facts-model.md) are scoped to the open plane. A credential
  belongs in the sealed plane (a secret), not in a `sensitive` fact.

### Secret Authorization & Roles

Decision: sealed-plane access composes with the same authorization layer as the
open plane, adding secret/TOTP scopes, secret grants, and realm roles. The full
role/scope model and resolution algorithm are tracked in
[authorization-and-roles.md](authorization-and-roles.md).

- **Secret scopes** (added to the scope set; see
  [Authorization and Scopes](#authorization-and-scopes)): `secret:create`,
  `secret:show`, `secret:reveal`, `secret:update`, `secret:delete`,
  `secret:grant`, `totp:enroll`, `totp:code`.
- **Grants** (`grt_` prefix) extend a specific secret to another agent or group
  with a bounded permission. Cross-agent/operator secret access uses **grants +
  realm roles**, not the open-plane cross-agent read/curate/forget verbs; secrets
  are not subject to the open cross-agent policy engine (see
  [access-policy.md](access-policy.md)).
- **Realm roles** (the rename of Witpass vault roles): `realm:admin`,
  `realm:operator`, `realm:auditor`, `realm:member`. The elevated roles bundle both
  open-plane scopes (memory/fact/policy/group/message) and sealed-plane scopes
  (secret/totp + grants) per
  [authorization-and-roles.md](authorization-and-roles.md). Account roles
  (`account_owner`/`account_admin`/`account_billing`/`account_member`) are
  unchanged.
- The **default agent token bundle** grants own-data access across both planes:
  memory create/read/update/forget, fact create/read/update/delete/primary, secret
  create/show/reveal/update/delete, totp enroll/code, and message send/read — over
  the agent's OWN data only. It excludes `secret:grant`, the `-others` cross-agent
  scopes, `group:manage`, `policy:manage`, `agent:manage`, `token:manage`,
  `audit:read`, `realm:admin`, and all `account:*`/`billing:*`/`support:*`.

### Ownership Unification

Decision: all three data types — memory, fact, and secret — are owned by an
**agent** or a **group**. `owner_kind ∈ {agent, group}` is uniform across the open
and sealed planes.

- The former Witpass `owner_kind ∈ {agent, shared}` is retired. A Witpass
  vault-shared secret becomes a **group-owned** secret. There is no separate
  "shared" scope.
- CLI: the former Witpass `--shared` becomes `--group <name>`; the former
  `wp://shared/…` becomes `witself://group/<group>/secret/…`.
- Group-owned secrets use the same `sec_`/`fld_` shapes with a group owner and
  follow the same grant and realm-role rules as agent-owned secrets.

### Public Backend And Self-Hosting

Witself should be an inspectable public-code product. The CLI, MCP adapter,
backend API server, storage adapters, authorization and policy logic, audit
model, embedding-provider abstraction, and release/deployment definitions should
live in the same public repository unless a clear security or operational reason
requires a split later.

The managed Witself cloud service remains the default endpoint and the default
commercial product. Self-hosting is a first-class deployment option for operators
who want to run the backend API inside their own cloud, network, or compliance
boundary.

Public backend goals:

- Let customers and security reviewers inspect the code that stores, authorizes,
  audits, embeds, recalls, and serves identity material.
- Let operators choose managed convenience or self-hosted control without changing
  the agent-facing CLI, MCP tools, identity references, or JSON contracts.
- Keep the managed service, self-hosted service, and local development backend on
  the same domain model and authorization path.
- Make the backend deployable through public release artifacts and public
  container images.
- Publish a first-class Helm chart for self-hosted Kubernetes deployments before
  claiming production self-host support.
- Include public Terraform modules and example stacks for AWS, GCP, and Azure
  substrate provisioning.
- Document the production dependencies required for self-hosting (Postgres with
  pgvector, an embedding provider, object/blob storage) rather than hiding them
  behind the managed service.

Self-hosted backend requirements:

- Provide a separate `witself-server` backend API binary/process from the public
  repository.
- Support configuration by environment variables and files suitable for
  containers, Kubernetes, and ordinary service managers.
- Use Postgres with the pgvector extension as the first production storage adapter.
- Add object/blob storage for large exports, diagnostic bundles, support
  attachments, and backup artifacts.
- Treat an embedding provider (`voyage` by default, `openai`, or `local-dev`) as a
  configurable production dependency behind a capability boundary.
- Expose health endpoints and structured logs that do not leak identity content.
- Expose Kubernetes-compatible liveness, readiness, and startup probes.
- Expose Prometheus-compatible metrics for HTTP traffic, auth, memory operations,
  recall and embedding operations, fact operations, policy decisions, group
  operations, messaging, audit events, usage, limits, storage, migrations, and
  runtime health.
- Support database migrations, backup/restore guidance, upgrade notes, and (when
  enabled) embedding re-index guidance before claiming production self-host
  support.
- Provide a Helm chart that deploys `witself-server` and supports externally
  managed production dependencies.
- Include Helm chart values and templates for health probes, metrics scraping,
  ServiceMonitor, PodMonitor, resources, autoscaling, disruption budgets, security
  context, and network policy where practical.
- Provide Terraform under `infra/terraform` for AWS, GCP, and Azure
  infrastructure, including Kubernetes, Postgres with pgvector, object/blob
  storage, workload identity, and networking where practical.
- Implement AWS first for managed cloud and self-hosted Terraform. Keep GCP and
  Azure as planned provider targets with visible repo structure and
  provider-neutral interfaces.
- Keep billing/provider integrations optional or replaceable for self-hosted
  deployments. Self-hosting should not require Witself-managed billing.
- Treat production self-host support as a paid or contracted support path after
  backup/restore, migrations, upgrade, observability, disaster recovery,
  production Helm values, and Terraform state guidance are real.

The local mock/development backend should be pushed upstream into this
architecture as a real adapter behind the same backend interface. It can power
tests, demos, `witself setup --local`, and a future `witself-server serve --dev`
mode, but it should remain clearly labeled as local development scaffolding.

### Observability And Operations

Witself should expose enough operational telemetry for managed Witself Cloud and
self-hosted operators to understand service health, customer adoption, load,
limits, failures, and security-sensitive activity without leaking identity
content.

Required operational surfaces:

- Prometheus text-format metrics through `/metrics`.
- Kubernetes liveness, readiness, and startup probes.
- Separate default listeners for API traffic, health probes, and metrics: `:8080`,
  `:8081`, and `:9090`.
- Metrics enablement controlled through server config, environment variables, CLI
  flags, and Helm values, with `WITSELF_METRICS_ENABLED` as the canonical
  environment variable.
- Structured JSON logs with request IDs and strict redaction.
- Low-cardinality metric labels that use route templates rather than raw paths.
- Metrics for core customer and agent activity, including memory operations,
  recall and embedding operations, fact operations, policy decisions (allow/deny),
  cross-agent accesses, group operations, message send/deliver/read,
  authentication, token lifecycle, audit events, usage metering, limit decisions,
  storage, vector storage, migrations, and HTTP latency.
- Helm chart support for metrics scraping, ServiceMonitor, PodMonitor, probes,
  resources, autoscaling, disruption budgets, security context, and network
  policy.

Metrics, logs, health responses, dashboards, and alerts must never include memory
content, fact values, message bodies or payloads, embedding vectors, raw tokens,
raw payment details, wallet credentials, database URLs, provider credentials,
provider secrets, raw request paths, query strings, request bodies, or arbitrary
user input.

The observability model is tracked in
[observability-and-operations.md](observability-and-operations.md).

### Self-Hosted Support Boundary

Decision: self-hosting is available from the public repo, but production self-host
support is paid or contracted once the hardening path is real.

Support posture:

- Managed Witself Cloud is the default supported product.
- Local development self-hosting is community or best-effort only.
- Self-host preview receives best-effort public issue triage and no SLA.
- Production self-hosted support is paid or contracted only.
- Public repo availability should not imply a production SLA.
- Billing, payment, support-ticket, and managed-service admin features may be
  unsupported in self-hosted deployments unless explicitly configured.

The self-hosted support model is tracked in
[self-host-support.md](self-host-support.md).

### Backup, Export, And Recovery

Decision: v0 supports first-class structured/plaintext identity export and
round-trippable import, in addition to operational backups. This is the deliberate
inverse of the Witpass encrypted-only stance.

Backup and recovery posture:

- Production backups preserve all identity data (memories, facts, policies, groups,
  messages, audit) and the embedding vectors needed to restore semantic recall
  without re-embedding.
- Backups include Postgres (with pgvector data), object/blob storage when used,
  migration version, embedding-provider/model identity, and server configuration
  needed to reconnect to storage and the embedding provider.
- Identity export is a supported plaintext feature, not a forbidden one (see
  [Identity Export and Import](#identity-export-and-import)). `witself export`
  produces structured, round-trippable identity data; `witself import` restores
  it.
- Self-hosted operators are responsible for backing up Postgres (including vector
  data), object/blob storage when used, Terraform state, deployment configuration,
  and embedding-provider configuration.
- If the embedding provider or model changes, recall can be restored from backed-up
  vectors; re-embedding is only required when intentionally changing the embedding
  model.
- Managed cloud recovery restores customer identity data and service availability.

Because export is plaintext by default, exports of `sensitive` open-plane records
are warned-on, require an audit `--reason`, and are least-privilege authorized;
operators may restrict exports to non-sensitive records. Recovery and export are
audited.

Sealed plane (carve-out #2): the sealed plane is excluded from the plaintext
export entirely. Secret backup is encrypted-only — envelope ciphertext plus the
KMS key identity and rotation metadata, never plaintext — and is the only path
that backs up secret material. Loss of the CMK/KMS access crypto-shreds secret
values but does not affect the recoverable plaintext open plane. The dual backup
posture is carried in [backup-and-recovery.md](backup-and-recovery.md).

The backup, export, and recovery model is tracked in
[backup-and-recovery.md](backup-and-recovery.md).

### Realm And Agent Bootstrap

Witself must include everything needed to set up a realm and give tokens to agents
from the CLI. A fresh operator should be able to install `witself`, point it at
managed Witself Cloud or a self-hosted endpoint, create or connect an
account/operator context, create a realm, create named agents, write agent token
files, and hand those token files to agent runtimes without using a web dashboard.

Required bootstrap outcomes:

- Create or select the managed-service customer account or self-hosted operator
  context.
- Create or select the realm.
- Create one or more named agents.
- Issue durable agent tokens bound to the realm and named agent.
- Write tokens directly to files suitable for secret mounts.
- Print or emit machine-readable instructions for `WITSELF_TOKEN_FILE`.
- Optionally emit Kubernetes Secret manifests or commands for token delivery.
- Verify each issued token can authenticate as the intended agent.

Setup should be idempotent by default for account, realm, and agent creation. When
a requested account, realm, or agent name already exists and is visible to the
operator, setup should select that resource instead of creating a duplicate.

Token handling should not be silently idempotent. If setup detects an existing
token file or active token for a requested agent, it must require an explicit
choice: `--reuse-existing-token` to verify and reuse it, or
`--rotate-existing-tokens` to issue replacements and invalidate or phase out the
old tokens according to rotation policy. Without one of those choices,
non-interactive setup should fail with a deterministic conflict and interactive
setup should ask.

Setup-created token files should be written with owner-only permissions where the
operating system supports them. On POSIX-style systems, token files should use
mode `0600`, and directories that Witself creates for token files should use mode
`0700`. Setup should write token files atomically and refuse to overwrite an
existing token file unless the caller explicitly chose token reuse or rotation.
Reuse verifies and keeps the existing file unchanged. Rotation is the explicit
path that may replace the token file after the replacement token has been issued
successfully.

The remote bootstrap flow is the canonical product flow. It bootstraps the
managed-service customer account or self-hosted operator context, remote realm,
named agents, and token files.

`witself setup` should default to managed Witself Cloud when no target flag is
provided. `--endpoint` should target a specific remote managed, staging, private,
or self-hosted endpoint. `--local` should be reserved for local mock/development
mode and should not be presented as a production setup path.

Remote setup should call backend capability discovery before attempting account,
realm, agent, token, billing, payment, or support operations. The command should
use that capability response to decide which steps can run and which steps must
return `unsupported_operation`.

Managed operator authentication should be browser/device-code based and
CLI-initiated. The CLI should not collect raw account passwords. Self-hosted
first-operator bootstrap should use a one-time bootstrap token or equivalent
deployment-owned mechanism, not a default admin password. The operator auth model
is tracked in [operator-auth.md](operator-auth.md).

Agents should not need an interactive login. Once the token file is mounted or
mapped and `WITSELF_TOKEN_FILE` points at it, the agent should be able to call
Witself immediately as the token-bound named agent.

Local mode may provide a similar-looking setup path for development and tests, but
it does not need to carry the full production agent bootstrap promise.

### Billing And Limits

Billing attaches at the account level, and usage rolls up by realm. One paying
customer can support multiple realms, each holding multiple agents.

Decision: v0 billing is plan-based and usage-aware, not raw per-call billing at
launch. The full managed billing apparatus is retained from Witpass, including
crypto payment rails.

Billing posture:

- The account is the billing target; usage is measured per realm.
- Plans define included quantities, soft limits, hard limits, and rate limits.
- V0 meters meaningful usage internally from the beginning.
- V0 charges primarily by plan tier first.
- V0 should not require raw per-call billing or per-agent invoice line items.
- Overage behavior should be configurable by plan and dimension: `warn`,
  `throttle`, or `block`.
- The full managed apparatus is retained: plans, usage-aware metering, soft/hard
  limits, payment methods, invoices, crypto payment rails (USDC/USDT/ETH via
  provider, no wallet custody), support tickets, CLI-first service admin, and
  capability-gating with `unsupported_operation`.

V0 metered dimensions span both planes:

Open plane:

- Active named agents.
- Stored memories.
- Stored facts.
- Memory recalls and reads.
- Memory writes (add/adjust).
- Embedding operations.
- Vector storage size (`vector_storage_byte`).
- General data-at-rest storage size (`storage_byte`).
- Cross-agent accesses.
- Security groups.
- Messages sent and delivered.

Sealed plane:

- Stored secrets (`stored_secret`).
- Secret reads (`secret_read`, covering reveal and reference resolution).
- TOTP code generation (`totp_code`).
- Runtime injection (`runtime_injection`, via `witself run`).
- Encrypted storage size (`encrypted_storage_byte`).

Platform:

- Audit retention and stored audit volume.
- General managed-service API request volume.

Recalls, embedding operations, cross-agent accesses, messages, secret reads, TOTP
code generation, and runtime injection should be metered even when pricing remains
tiered because they represent real service load and security-relevant use.

The billing and limits model is tracked in
[billing-and-limits.md](billing-and-limits.md).

### Authorization and Scopes

Witself should enforce authorization below every frontend. CLI, MCP, and the HTTP
API must call the same authorization layer, with the same result across frontends.

Default permissions:

- Agent tokens can add, adjust, read, recall, list, forget, restore, and delete
  memories, and set, get, list, and delete facts, only for identity data owned by
  that agent.
- Agent tokens can create, show, reveal, update, and delete secrets and enroll
  TOTP / generate TOTP codes, only for sealed-plane material owned by that agent.
- Agent tokens cannot read, contribute to, curate, or forget another agent's
  identity data unless a policy grants it, and cannot show, reveal, or use another
  agent's secrets unless a secret grant or realm role permits it.
- Agent tokens cannot manage other agents, manage realm membership, manage
  billing, inspect realm-wide inventory, manage policies, manage groups, or manage
  tokens unless explicitly granted.
- Operator/admin tokens can manage realm-wide state according to their role
  (operator override).
- Cross-agent actions require an explicit policy or operator/admin permission.

Permissions are expressed as scopes spanning both planes:

Open-plane scopes:

- `memory:create`
- `memory:read`
- `memory:update`
- `memory:forget`
- `memory:read-others` (cross-agent READ for memories AND facts, policy-gated)
- `memory:manage-others` (contribute/curate/forget across agents, policy-gated)
- `fact:create`
- `fact:read`
- `fact:update`
- `fact:delete`
- `fact:primary`
- `policy:read`
- `policy:manage`
- `group:read`
- `group:manage`
- `group:member`
- `message:send`
- `message:read`

Sealed-plane scopes:

- `secret:create`
- `secret:show`
- `secret:reveal`
- `secret:update`
- `secret:delete`
- `secret:grant`
- `totp:enroll`
- `totp:code`

Platform/admin scopes:

- `agent:manage`
- `token:manage`
- `audit:read`
- `account:read`
- `account:manage`
- `billing:read`
- `billing:manage`
- `support:read`
- `support:manage`
- `realm:admin`

`memory:read-others` and `memory:manage-others` are the ONLY open-plane
cross-agent scopes, and each spans both memories and facts; there are no
`fact:read-others` or `fact:manage-others` scopes. Sealed-plane cross-agent access
is mediated by `secret:grant` plus realm roles, not by the open-plane cross-agent
scopes (see [Secret Authorization & Roles](#secret-authorization--roles)).

Scopes should be constrainable by realm, owning agent, owning group, memory kind
or tag, fact name, or secret name/field where practical. Realm roles
(`realm:admin`/`realm:operator`/`realm:auditor`/`realm:member`) bundle these scopes
across both planes; the full bundles and resolution algorithm are in
[authorization-and-roles.md](authorization-and-roles.md).

### Agent Authentication

Agents should authenticate to Witself with scoped agent tokens. The token
identifies the realm, the named agent, and the agent's effective permissions. In
v0, the token is a bearer token bound server-side to one realm and one named
agent.

Agents may be ephemeral runtime instances, such as Kubernetes pods. The named
agent identity is durable; the pod or process instance is not. A restarted agent
should be able to mount or map the same token file and immediately continue using
Witself as the same named agent, subject to token validity and policy.

Agent tokens are allowed to outlive any one agent process, container, or pod by
default. Token rotation and revocation are explicit lifecycle operations on the
named agent identity, not automatic side effects of runtime churn.

Default v0 agent tokens do not expire automatically. Operators may set expiration
explicitly with `--ttl` or `--expires-at`, but the default favors durable
mounted-token operation for ephemeral agents.

Agent identity must come from the token, not from a caller-supplied agent name.
This is critical for cross-agent access and messaging: the actor and message
sender are derived server-side from the token. Passing an agent name should never
let a caller become that agent unless the authenticated token or operator
credential has explicit permission to act on behalf of that agent.

The preferred unattended delivery mechanism is a token file, not a raw token in an
environment variable. Production deployments should prefer deployment-native
secret mounts and pass the exact path through `WITSELF_TOKEN_FILE` or a CLI flag.

Recommended token locations:

- Production container or service: an explicit secret mount such as
  `/run/secrets/witself-agent-token`.
- Local development fallback:
  `${XDG_CONFIG_HOME:-~/.config}/witself/tokens/<profile-or-agent>.token`.

Witself should not require the default local path. Explicit token paths should be
the normal, documented production pattern.

When Witself itself writes a token file, it should create it with owner-only
permissions where the operating system supports them and should refuse to
overwrite an existing path unless the caller explicitly requested token reuse or
rotation.

Token lifecycle requirements:

- Raw token values are returned only once during create or rotate.
- Server-side storage keeps token hashes and token metadata, not raw tokens.
- Token files are plain token text in v0.
- Revocation is immediate.
- Disabled agents cannot authenticate with existing tokens.
- Agent deletion invalidates that agent's tokens.
- Rotation may revoke the old token immediately or after an explicit grace period.

The token lifecycle is tracked in [token-lifecycle.md](token-lifecycle.md).

Authentication source precedence should be:

1. Explicit CLI flag, such as `--token-file`.
2. `WITSELF_TOKEN_FILE`.
3. `WITSELF_TOKEN`.
4. Stored local profile auth, when available for human/operator use.

`WITSELF_TOKEN` is convenient for tests and short-lived local use, but should be
documented as the least-safe unattended option.

### CLI, MCP, And JSON Contracts

The CLI should be useful to humans by default and deterministic for agents when
`--json` or `--no-input` is used.

Requirements:

- Commands that return data should support `--json`.
- Mutating commands should be non-interactive when all required flags are provided
  and `--no-input` is set.
- JSON outputs should use stable field names and the `witself.v0` schema version.
- Structured errors should include a stable machine-readable code and human
  message.
- CLI and MCP should share request/response structs or an equivalent generated
  contract to prevent drift.
- Memory content, fact values, message bodies/payloads, raw tokens, and plaintext
  secret/TOTP-seed material must not appear in errors. `sensitive` memory content
  and `sensitive` fact values are redacted in list/scan output by default; an
  authorized read of a single open-plane record returns the value (there is no
  open-plane reveal ceremony). Sealed-plane secret field values and TOTP codes are
  returned only by the explicit reveal ceremony (`secret reveal`, `totp code`); see
  [Reveal and Value-Returning Operations](#reveal-and-value-returning-operations).

The CLI noun surface spans both planes:

Open plane:

- `memory` (`add`/`adjust`/`read`/`recall`/`list`/`forget`/`restore`/`delete`/
  `consolidate`).
- `fact` (`set`/`get`/`list`/`delete`, with `--primary`).
- `remember` (the auto-routing quick-add over `fact set`/`memory add`).
- `self` (`show`, the always-loaded self-digest).
- `session` (`start`/`end`, multi-session bootstrap).
- `digest` (`emit`, the self-digest rendered as a `CLAUDE.md`/`AGENTS.md` fragment).
- `ingest` (parse `CLAUDE.md`/`AGENTS.md`/`GEMINI.md` into facts and memories).
- `bootstrap-instructions` (print the paste-able teaching stanza).
- `policy` (`create`/`list`/`show`/`delete`/`test`).
- `group` (`create`/`list`/`show`/`add-member`/`remove-member`/`delete`).
- `message` (`send`/`list`/`read`/`ack`).

Sealed plane:

- `secret` (`create`/`show`/`list`/`scan`/`reveal`/`update`/`rename`/`copy`/
  `archive`/`restore`/`delete`/`grant`/`revoke`). `--group <name>` targets a
  group-owned secret; `--owner-agent <name>` targets a specific agent's secret for
  operator use.
- `totp` (`enroll`/`code`/`show`/`delete`).
- `password generate` (built-in password/passphrase generator).
- `run` (resolve `witself://secret/…` references and inject values into a child
  process; see [Runtime Injection](#runtime-injection-witself-run)).

Inherited: `version`, `capabilities`, `whoami`, `auth`, `setup`, `account`,
`realm`, `agent`, `token`, `audit`, `billing`, `support`, `export`, `import`,
`reference`, `mcp`, `config`, `completion`.

The MCP tool catalog spans both planes:

- `witself.version`, `witself.whoami`, `witself.capabilities`.
- `witself.memory.add/adjust/read/recall/list/forget/consolidate`.
- `witself.fact.set/get/list/delete`.
- `witself.remember` (auto-routing quick-add).
- `witself.self.show` (the always-loaded self-digest; never includes secrets).
- `witself.session.start/end` (multi-session bootstrap).
- `witself.digest.emit` (ingest is CLI-first; an MCP `ingest` tool is optional;
  neither carries secrets).
- `witself.policy.test` (plus operator `policy.list`/`policy.show`).
- `witself.group.list/show`.
- `witself.message.send/list/read`.
- `witself.secret.create/list/show/reveal/update` (sealed plane).
- `witself.totp.enroll/code/show` (sealed plane).
- `witself.password.generate` (sealed plane).
- `witself.reference.parse/resolve` (value-returning resolution of both open
  identity and sealed secret references; gated by `--no-value-tools`).
- Operator candidates: `witself.agent.list/show`, `witself.audit.list/show`.

Value-returning sealed-plane tools (`secret.reveal`, `totp.code`, and
value-returning `reference.resolve`) are policy-gated and disabled by
`--no-value-tools`; mutating tools are excluded in `--read-only` mode (see
[MCP Boundary](#mcp-boundary)).

Detailed JSON shapes are tracked in [json-contracts.md](json-contracts.md). The
CLI command surface is tracked in
[cli-command-surface.md](cli-command-surface.md). The MCP tool catalog and
tool-level risk boundaries are tracked in [mcp-tools.md](mcp-tools.md).

### Go And Module Baseline

Witself should be implemented in Go and should use the latest stable Go release
available at the time implementation or release work is performed.

Current baseline as of June 26, 2026:

- Latest stable Go release: `go1.26.4`.
- Initial module path: `github.com/witwave-ai/witself`.
- Initial `go.mod` language version: `go 1.26`.
- Initial `go.mod` toolchain directive: `toolchain go1.26.4`.

This baseline should be refreshed before the first implementation pass and before
each release. The intent is to stay on the latest stable Go toolchain, not to pin
the project permanently to the snapshot above.

Module and dependency requirements:

- Use Go modules only; do not rely on GOPATH mode.
- Start with one Go module for the repository unless a real build or ownership
  boundary justifies multiple modules.
- Build the `witself` CLI, `witself mcp serve`, the separate `witself-server`
  backend API binary, and shared core from the same module so command behavior
  does not drift across frontends.
- Commit `go.mod` and `go.sum`.
- Keep dependencies pinned by the module files and refreshed deliberately.
- Run `go mod tidy` and `go mod verify` in CI.
- Do not vendor dependencies by default; vendor only if release or supply-chain
  requirements make that necessary.

### MCP Boundary

The MCP surface should be useful but intentionally scoped.

Defaults:

- Local stdio transport first.
- HTTP/network transport is post-v0, explicit, authenticated, and higher risk.
- MCP tools must respect the same authorization checks as CLI commands.
- Agent-token MCP sessions should act only as the token-bound agent.
- Operator-token MCP sessions may bind to or inspect agents only when explicitly
  permitted.
- A `--read-only` MCP mode should exist for inspection-only deployments; it
  disables all mutating tools across both planes.
- A `--no-value-tools` MCP mode should exist for the sealed plane; it disables the
  value-returning tools (`witself.secret.reveal`, `witself.totp.code`, and
  value-returning `witself.reference.resolve`) while leaving redacted/metadata
  tools available. `--no-value-tools` and `--read-only` are independent switches.

MCP should expose agent-useful actions such as memory add/adjust/read/recall/list/
forget, fact set/get/list/delete, policy test, group list/show, message
send/list/read, reference parse/resolve, secret create/list/show/reveal/update,
totp enroll/code/show, and password generate. High-risk admin actions such as
realm deletion, broad agent lifecycle operations, policy mutation, secret grant,
and token management should be operator-only and may be excluded from MCP v0 even
if they exist in the CLI. Sealed-plane reveal tools carry an explicit reveal
framing and are gated by `--no-value-tools`; the self-digest and `digest emit`
never carry secret material.

### Audit and Identity Handling

Witself should write structured audit records for security-relevant events,
including:

- Authentication success and failure.
- Memory add, adjust, read, recall, forget, restore, and delete.
- Memory consolidation, session start/end, and file ingest of memories and facts.
  (`remember` needs no event of its own; it routes to `memory.added` /
  `fact.created` / `fact.updated`.)
- Fact set, get (of `sensitive` facts), delete, and primary promotion.
- Cross-agent read, contribute, curate, and forget actions, attributed to the
  acting agent and the deciding policy.
- Policy create, update, delete, and `policy test` decisions where configured.
- Group create, delete, member add/remove, and group-owned record changes.
- Message send, deliver, read, and ack.
- Identity export and import.
- Secret create, update, rename, copy, archive, restore, delete, reveal, grant,
  and revoke (the `server_side_decrypt` flag is recorded on reveal).
- TOTP enroll, code generation, seed reveal, and delete (the `server_side_decrypt`
  flag is recorded on code generation).
- Per-realm KEK rotation (`key.rotated`).
- Agent lifecycle operations.
- Token create, rotate, revoke, and failed token use.
- Operator/admin override actions.
- Account profile, account member, and role changes.
- Billing plan, subscription, payment-method, invoice, refund, and payment
  provider actions.
- Crypto payment quote, checkout/session, confirmation, failure, refund, and
  provider reconciliation actions.
- Support ticket create, comment, close, and diagnostic-bundle actions.

Audit records must not contain memory content, fact values, message bodies or
payloads, embedding vectors, raw tokens, raw payment details, or any sealed-plane
plaintext (secret field values, TOTP seeds, generated TOTP codes, or DEK/KEK key
material). Error messages, logs, and JSON responses must follow the same rule.
Audit may include non-sensitive context such as record ids (including `sec_`,
`fld_`, `grt_`, `totp_`, `kek_`), owner agent/group, kinds, tags, fact and secret
names, field names, policy and grant ids, message ids, recipient, the
`server_side_decrypt` flag, and decision outcome.

Audit records for billing and payment actions may include non-sensitive context
such as account ID, realm ID, invoice ID, subscription ID, payment provider,
payment method type, crypto asset, network, amount, currency, status, and provider
event ID. They must not contain raw payment details, card numbers, provider
tokens, wallet seed phrases, wallet private keys, raw wallet credentials, or full
wallet identifiers.

Identity and payment audit event names should be stable enough for support,
reconciliation, and customer exports. Initial event names include:

- `memory.added`
- `memory.adjusted`
- `memory.recalled`
- `memory.forgotten`
- `memory.restored`
- `memory.deleted`
- `memory.consolidated`
- `memory.imported`
- `session.started`
- `session.ended`
- `fact.created` (the `fact set` / `remember` upsert emits this for a new fact)
- `fact.updated` (the `fact set` / `remember` upsert emits this for an existing fact)
- `fact.imported`
- `self.digest.emitted` (optional)
- `fact.primary_changed`
- `fact.deleted`
- `policy.created`
- `policy.deleted`
- `policy.access_denied`
- `crossagent.read`
- `crossagent.contributed`
- `crossagent.curated`
- `crossagent.forgotten`
- `group.created`
- `group.member_added`
- `group.member_removed`
- `message.sent`
- `message.delivered`
- `message.read`
- `message.acked`
- `identity.exported`
- `identity.imported`
- `secret.created`
- `secret.updated`
- `secret.renamed`
- `secret.copied`
- `secret.archived`
- `secret.restored`
- `secret.deleted`
- `secret.reveal` (carries the `server_side_decrypt` flag)
- `secret.grant`
- `secret.revoke`
- `totp.enrolled`
- `totp.code` (carries the `server_side_decrypt` flag)
- `totp.seed_revealed`
- `totp.deleted`
- `key.rotated` (per-realm KEK rotation)
- `billing.subscription.created`
- `billing.subscription.updated`
- `billing.subscription.canceled`
- `billing.payment_method.added`
- `billing.payment_method.removed`
- `billing.payment_method.default_changed`
- `billing.invoice.created`
- `billing.invoice.paid`
- `billing.invoice.payment_failed`
- `billing.refund.created`
- `billing.crypto.quote.created`
- `billing.crypto.checkout.started`
- `billing.crypto.payment.confirmed`
- `billing.crypto.payment.failed`
- `billing.crypto.refund.created`
- `billing.crypto.provider_event.reconciled`

Support actions and diagnostic bundles must redact identity content by default.

Operator/admin override, destructive, and cross-agent actions should require an
audit reason unless a higher-level policy explicitly waives that requirement.

Default managed v0 audit retention is 365 days. Self-hosted Helm values should
also default to 365 days while allowing operators to configure retention.
Local-development audit retention can be best-effort. Audit retention is a metered
usage dimension and is tracked in [audit-retention.md](audit-retention.md).

### Local Mock/Development Backend

The v0 local backend should be a local store suitable for development, demos,
tests, and offline prototyping. It is scaffolding for implementation and developer
ergonomics, not a full product mode and not the production agent runtime contract.

The local backend should still be a real backend adapter, not a parallel toy
system. CLI, MCP, API handlers, tests, and demos should be able to exercise the
same domain model, authorization and policy checks, audit paths, JSON contracts,
and storage interface against the local adapter.

Local backend requirements:

- Persist the serialized identity store at rest with ordinary data-at-rest
  protection.
- Write store files atomically.
- Restrict local file permissions where the OS supports it.
- Keep tokens out of config files by default.
- Support export/import for test fixtures, demos, backup, and migration (using the
  same `witself export`/`witself import` paths).
- Use the `local-dev` embedding provider so semantic recall can be exercised
  offline without a paid provider.

The local backend is not the managed-service or production self-hosted model. It
should be kept behind the same backend interface so it helps build and test the
CLI, MCP, API server, and data-model contracts without becoming the production
architecture.

Local bootstrap decision:

- `witself realm init` creates the local store and the first local operator/admin
  context when the store is empty.
- The first local operator can create named agents and write token files for local
  development runtimes.
- Recommended agent bootstrap: `witself agent create NAME --token-out PATH`.
- Local agent token files can be passed to Witself through `WITSELF_TOKEN_FILE`.

### Managed Cloud Backend

Witself should design the managed cloud backend as the default hosted product
backend. When Witself is offered as a service, memories, facts, embedding vectors,
policies, security groups, messages, agent metadata, envelope-encrypted secrets
and TOTP enrollments, the per-realm KEK and wrapped DEKs, secret grants, audit
records, and usage counters are expected to be stored remotely in Witself-operated
cloud infrastructure. The CMK lives in the managed KMS; plaintext secret material
is never stored.

Managed backend requirements:

- Use Postgres with pgvector as the managed cloud system of record for account,
  realm, agent, token, memory, fact, embedding vector, policy, group, message,
  grant, audit, usage counter, idempotency record, and backend metadata.
- Use `voyage` as the default embedding provider, behind the same provider boundary
  used by the local backend.
- Apply ordinary data-at-rest protection (managed RDS/disk encryption). Optional
  field-level encryption of `sensitive` facts is a capability, not a core
  dependency (see [Data-At-Rest Note](#data-at-rest-note)).
- Keep storage and embedding providers behind the same backend boundaries used by
  the local mock/development backend.
- Preserve the same CLI, MCP, JSON, and authorization contracts across the managed
  backend and any local mock/development backend where practical.
- Support per-realm usage metering for agent count, stored memories, stored facts,
  recalls/reads, writes, embedding operations, vector storage size
  (`vector_storage_byte`), general data-at-rest storage size (`storage_byte`),
  cross-agent accesses, security groups, messages, audit retention, and general
  API load.
- Keep memory content, fact values, message bodies/payloads, embedding vectors,
  and raw tokens out of logs, audit records, analytics, and errors.
- Design for backup, restore, and disaster recovery from the beginning, including
  vector data.

### Self-Hosted Backend

The self-hosted backend should run the same public API server used by the managed
service, with deployment-owned configuration, storage, embedding-provider
selection, networking, and observability.

Self-hosting should preserve the same external contracts:

- Same CLI command behavior when pointed at a self-hosted endpoint.
- Same `WITSELF_ENDPOINT` and `--endpoint` targeting model.
- Same token, realm, agent, memory, fact, policy, group, message, audit, and JSON
  response contracts.
- Same MCP behavior when the MCP server is configured against the self-hosted
  endpoint.
- Same redaction and audit rules.

Self-hosting is not a promise that every managed-service feature is available
immediately. Managed billing, hosted payment flows, Witself support workflows, and
internal service administration may be disabled, stubbed, or replaced by
self-host-owned integrations in self-hosted deployments. The embedding provider is
deployment-owned; a self-hosted operator may run `local-dev`, `voyage`, or
`openai` per their own contracts.

Backend feature discovery is required. Managed, self-hosted, and local development
backends should expose a capabilities contract so the CLI can show which features
are supported, which are unavailable, and why (including the active embedding
provider and whether semantic recall is degraded). Unsupported commands should
fail predictably with `unsupported_operation`, not with vague provider, route, or
config errors.

### Backend API Contract

The public backend API should use an explicit versioned contract. The initial
route base is `/v1`.

API contract requirements:

- Use the shared JSON response envelope `{schema_version, ok, data, warnings}` and
  the error-code-to-HTTP-to-exit-code table.
- Authenticate remote calls with bearer tokens loaded by the CLI or MCP adapter.
- Expose `/v1/capabilities` for backend feature discovery, including the active
  embedding provider/model and, when the sealed plane is enabled, the KMS provider
  and the decrypt-custody capability flags `client_side_decrypt` /
  `server_side_decrypt` (see [Encryption (Two-Tier)](#encryption-two-tier)).
- Use resource-oriented `/v1` REST-ish routes with plural resources, including
  the open-plane `/v1/memories`, `/v1/facts`, `/v1/policies`, `/v1/groups`, and
  `/v1/messages`, and the sealed-plane `/v1/secrets` and `/v1/totp`.
- Use colon action subroutes for sensitive or workflow operations, such as
  `/v1/memories:recall` (a query over the collection),
  `/v1/memories/{memory_id}:forget`, `/v1/memories/{memory_id}:restore`,
  `/v1/memories:consolidate`, `/v1/facts/{fact_id}:primary`, `/v1/policies:test`,
  `/v1/messages/{message_id}:ack`, and `/v1/tokens/{token_id}:rotate`.
- Use the sealed-plane action subroutes
  `/v1/secrets/{secret_id}:reveal` (returns one sensitive field; carries the
  `client_side_decrypt` ciphertext-and-envelope shape or the `server_side_decrypt`
  plaintext shape per the active capability),
  `/v1/secrets/{secret_id}:rotate`, `/v1/secrets/{secret_id}:archive`,
  `/v1/secrets/{secret_id}:restore`, `/v1/secrets/{secret_id}:grant`,
  `/v1/secrets/{secret_id}:revoke`, `/v1/totp/{totp_id}:code` (returns a current
  code), and `/v1/password:generate` (password/passphrase generation). All use
  `POST`, never `GET`.
- Expose the self-management and hydration routes: `POST /v1/remember` (the
  convenience action that the core routes to a fact or memory), `GET /v1/self` (the
  self-digest; `?format=` renders the `digest emit` fragment), and
  `POST /v1/sessions:start` / `POST /v1/sessions:end`. `ingest` composes the
  existing `POST /v1/facts` and `POST /v1/memories` create paths (with dedup) rather
  than introducing a new resource.
- Use `POST`, never `GET`, for sensitive/action routes.
- Support idempotency keys for retryable mutating operations.
- Support `dry_run` on mutating operations where practical.
- Support cursor pagination for list endpoints.
- Avoid putting memory content, fact values, message bodies/payloads, raw tokens,
  payment details, wallet credentials, or provider secrets in URL paths or query
  strings.
- Generate or publish OpenAPI from the Go structs before the first public server
  release.

The API contract is tracked in [api-contract.md](api-contract.md). The API route
style is tracked in [api-routes.md](api-routes.md).

### Distribution And Release

Install and release ergonomics are part of the product, not polish.

Requirements:

- The primary repository should be public at `github.com/witwave-ai/witself`.
- GitHub Release assets should be public.
- Published packages should be public, including container images and any future
  package artifacts needed for installation.
- Homebrew installation for macOS and Linux.
- Homebrew releases must use the `witwave-ai/homebrew-tap` repository. Release
  automation should create that repository under the `witwave-ai` organization if
  it does not already exist before the first Homebrew release.
- Universal `curl | sh` installer for macOS and Linux.
- A public Docker image published from an image definition under `images/*`.
- A public backend server Docker image published from an image definition under
  `images/*` once `witself-server` exists.
- A public Helm chart published for Kubernetes self-hosting once `witself-server`
  exists.
- Public Terraform modules and example stacks for AWS, GCP, and Azure under
  `infra/terraform`.
- Checksums for release artifacts.
- Signed release archives and checksum manifests.
- Signed container images.
- SBOMs for binaries and container images.
- Build provenance or equivalent release attestations.
- A documented verification path for install scripts and binaries.
- A GitHub Actions release workflow from the beginning of the project.
- Shell completions.
- Stable `witself version` output.
- Machine-readable release metadata where practical.

Homebrew install path:

- Tap repository: `github.com/witwave-ai/homebrew-tap`.
- Tap name: `witwave-ai/tap`.
- Formula: `Formula/witself.rb`.
- Expected install command: `brew install witwave-ai/tap/witself`.

The universal installer should install the same release artifacts as Homebrew and
verify checksums plus signatures where available before placing the `witself`
binary on PATH.

Container image requirements:

- Initial CLI/MCP Dockerfile path: `images/witself/Dockerfile`.
- Initial CLI/MCP image package: `ghcr.io/witwave-ai/images/witself`.
- Backend server Dockerfile path once the server exists:
  `images/witself-server/Dockerfile`.
- Backend server image package: `ghcr.io/witwave-ai/images/witself-server`.
- Supported platforms: `linux/amd64` and `linux/arm64`.
- Tags should include immutable release versions and `latest`.
- Images should run as a non-root user where practical.
- The CLI/MCP image entrypoint should be the `witself` binary so it can run both
  normal CLI commands and `witself mcp serve`.
- The backend image entrypoint should be the separate `witself-server` process.
- Image builds must not require private base images or private package registries.
- Container images should be signed before publication.
- Container image releases should include an SBOM and provenance attestation.

Helm chart requirements:

- Initial chart path: `charts/witself`.
- Public chart package: `ghcr.io/witwave-ai/charts/witself`.
- The chart should deploy `witself-server`, not the customer/operator CLI.
- External Postgres with pgvector should be the production default.
- External embedding-provider configuration and object/blob storage configuration
  should be first-class chart values.
- Chart values must support referencing existing Kubernetes Secrets rather than
  placing raw secrets in `values.yaml`.
- The chart should include Service, Deployment, ServiceAccount, ConfigMap,
  optional Ingress, optional NetworkPolicy, and migration Job templates.
- The chart should include liveness, readiness, startup, metrics scraping,
  ServiceMonitor, PodMonitor, resources, autoscaling, disruption budget, security
  context, and network policy support where practical.
- The chart should use separate named container ports for API, health, and
  metrics, and should not expose health or metrics through public ingress by
  default.
- Automatic migrations should be opt-in and explicit. Production operators should
  be able to run migrations as a controlled job before upgrades.
- Helm docs must clearly label any dev-only convenience values.
- Chart releases should be linted, rendered, schema-validated, signed or
  provenance-attested, and published publicly.

Terraform infrastructure requirements:

- Initial Terraform path: `infra/terraform`.
- Module paths: `infra/terraform/modules/aws`, `infra/terraform/modules/gcp`, and
  `infra/terraform/modules/azure`.
- Example stack paths: `infra/terraform/stacks/self-hosted/aws`,
  `infra/terraform/stacks/self-hosted/gcp`, and
  `infra/terraform/stacks/self-hosted/azure`.
- Managed Witself Cloud stack examples may live under
  `infra/terraform/stacks/witself-cloud`, but real state, credentials, and
  environment-specific secrets must stay outside the public repo.
- Terraform should provision cloud substrate (including Postgres with pgvector) and
  output the values or references needed by the Helm chart.
- Terraform must not render raw secrets into Helm values files.
- Terraform state files, state credentials, real `.tfvars`, cloud credentials,
  database passwords, embedding-provider credentials, private keys, raw Witself
  tokens, payment provider credentials, and wallet credentials must not be
  committed.

Release workflow requirements:

- Workflow path: `.github/workflows/release.yml`.
- Trigger on version tags such as `v0.1.0`.
- Support manual `workflow_dispatch` release dry runs or snapshots.
- Build public release archives, checksums, signatures, SBOMs, provenance,
  Homebrew formula updates, and the public container image.
- Verify the Homebrew tap exists before publishing. If
  `github.com/witwave-ai/homebrew-tap` is missing, the workflow should create it
  when configured with an org token that can create public repositories, or fail
  with a clear setup error.
- Smoke test the generated artifacts before marking the release complete.

The build and release model is tracked in
[release-and-build.md](release-and-build.md).

### CI And Quality Gates

Witself should have strong CI and linting before implementation starts to grow.
Required checks should run on pull requests and pushes to `main`, with release
checks also running on version tags.

Required CI coverage:

- Root docs validation for `README.md`, `SECURITY.md`, and `CONTRIBUTING.md`.
- Go formatting with `gofmt`.
- Go module cleanliness with `go mod tidy` and a clean `go.mod`/`go.sum` diff.
- Go module verification with `go mod verify`.
- Go build for all packages.
- Go tests with race detection on Linux.
- `go vet ./...`.
- `golangci-lint`.
- Vulnerability scanning with `govulncheck`.
- Markdown linting for docs.
- Shell linting for install and release scripts.
- GitHub Actions workflow linting with `actionlint`.
- Dockerfile linting with `hadolint`.
- Docker image build smoke tests for every Dockerfile under `images/*`.
- Helm chart linting and template rendering for every chart under `charts/*`.
- Kubernetes manifest schema validation for rendered Helm templates.
- Terraform formatting, validation, linting, and static security checks for
  `infra/terraform`.
- GoReleaser release-configuration checks.
- Homebrew install smoke tests on macOS and Linux.
- Universal installer smoke tests on macOS and Linux where practical.

CI should use concurrency cancellation for stale branches and should keep
permissions minimal. Release jobs that publish packages need `packages: write`,
but ordinary CI should default to read-only repository permissions.

### Account on Self-Host

Decision: one data model everywhere — `account -> realm -> agent`. Self-hosting does
not get a different shape; it gets the same shape with the commercial capabilities
gated off.

- **Managed.** The account is the customer/billing root: signup, plan, payment,
  support, and managed-admin all attach to the account, and usage rolls up per realm
  to the account (see [Billing And Limits](#billing-and-limits)).
- **Self-host.** The account is a single, implicit deployment/org root created at
  bootstrap — no signup, no plan, no payment. Billing, payment, support-ticket, and
  managed-admin features are capability-gated **off** and return
  `unsupported_operation` (see [Self-Hosted Backend](#self-hosted-backend)). The
  operator works in realms and agents; the account is plumbing.
- **Multi-account self-host** is possible but **off by default** — single-tenant is
  the self-host norm. An operator can opt into multiple deployment-root accounts, but
  nothing requires it.
- **Unit of trust is the realm, not the account.** For cross-realm collaboration and
  federation, the realm (handle + signing key) is the unit that is allow-listed and
  verified (see [Agent Collaboration Substrate](#agent-collaboration-substrate-cross-realm)),
  not the account.

This decision keeps managed and self-host on the same `account/realm/agent/token`
plumbing so the CLI, MCP, JSON contracts, and authorization path do not fork. The
follow-up sweep propagates the capability-gating detail into
[operator-auth.md](operator-auth.md) and [self-hosting.md](self-hosting.md).

### Agent Collaboration Substrate (cross-realm)

Decision (go-forward): cross-realm / cross-account agent collaboration is the
flagship epic. Witself is the channel — a trust fabric, not a Slack/Discord bridge —
where each agent's durable, attributable self makes a cross-realm message worth
acting on. It **extends** the realm-local mailbox (see
[Inter-Agent Messaging](#inter-agent-messaging)); it does not replace it. Sequenced
as the **first post-v0 epic**, after the realm-local core. The model is tracked in
[agent-collaboration.md](agent-collaboration.md).

Model:

- **Addressing.** Extend `witself://` with a realm-authority handle:
  `witself://<realm-handle>/agent/<name>` and `.../group/<name>`. The wire `to`/`from`
  gain an optional `realm`; an absent `realm` means local (unchanged v0 behavior).
- **Discovery.** Each realm publishes a **signed** well-known realm/agent card
  (capabilities/skills, endpoint, accepted auth, signing public key/JWKS, delivery
  modes, TTL). Signing is mandatory; verify before trust; resolution is separated
  from the signing key.
- **Rendezvous / relay.** Witself Cloud is a **blind relay**: it routes by realm
  handle, carries end-to-end-signed envelopes, and cannot read, forge, or alter the
  body/payload. Self-hosts federate by registering an FQDN + key. The relay shares
  the global directory with the cell control plane (see
  [Deployment Cells & Tenant Placement](#deployment-cells--tenant-placement)).
- **Participants.** *Directed* agents (human-guided, e.g. Claude Code / Codex via
  MCP) default `auto_reply=false` — the human gates replies and inbound surfaces via
  list/read. *Autonomous* (cloud) agents may set `auto_reply=true` but only within a
  finite per-conversation reply budget under the loop caps.
- **Conversations.** Promote `thread_id` to a first-class cross-realm
  `conversation_id` with an A2A-style task state machine
  (`submitted -> working -> input_required/auth_required -> completed | failed |
  canceled`). The durable mailbox is the source of truth; any live stream is only a
  latency accelerator, and a conversation resumes from the mailbox.
- **1:1 default; 1:many** via cross-realm channels (group fan-out generalized across
  realms with mutual consent; snapshot-at-send; fan-out cap).

Loop & safety defaults (enforced on the wire):

- Do-not-auto-reply across a trust boundary by default.
- `hop_count` with `max_hops=8`; per-conversation `turn_budget=24`; TTL/`expires_at`
  default 1h (up to 24h).
- Idempotency + dedup on `id`+`nonce`; repeat-hash loop detection (same message ×3 ->
  suspend + notify).
- Shared cost kill-switch enforced **before each model call** (soft $5 warn / hard
  $25 fail per conversation); adaptive rate limits + new-sender quarantine; circuit
  breaker + wait timeouts.
- `remaining_turns`/budget exposed to agents. Human gates fire only at
  trust-boundary auto-reply, over-threshold spend, and `auth_required`.

Trust & consent:

- Keep the token-derived sender for **routing** and in-realm anti-spoofing; add a
  cross-realm **signature** (realm key, ideally agent keypair) verified against the
  published JWKS for **trust**.
- Deny-by-default federation: allow-list which realm handles + keys you accept; first
  contact is quarantined and consented. Trust is anchored, not transitive; real-time
  revocation is required.
- Content stays untrusted and carries **no authority** across realms: a cross-realm
  message can never author a write without a standing allow policy in the receiving
  realm (see [Cross-Agent Access Policy](#cross-agent-access-policy)).

Interface invariants (see also [Interface Invariants](#interface-invariants)):

- Everything works via MCP with **full parity**, not just CLI; CLI is
  primary/canonical.
- Add a `listen`/`recv` verb (long-poll: block up to N seconds, return inbound) to
  **both** CLI and MCP next to send/list/read.
- **No agent-run HTTP servers** for normal I/O — agents are outbound clients; the
  durable mailbox is the source of truth; offline recipients are the default; v0
  transport is polling-first.

Open decisions (document, do not resolve):

- **Identity root** — per-realm signing key for v1 vs per-agent keypair now.
- **Self-host federation** — cloud-relay-first vs peer-to-peer.
- **Auto-reply default** — recommended: OFF-by-default + budgeted opt-in.
- **A2A interop** — native A2A at the boundary vs Witself-native + an A2A gateway.

Cross-links: [inter-agent-messaging.md](inter-agent-messaging.md) (realm-local
authority), [security-groups.md](security-groups.md),
[access-policy.md](access-policy.md), [threat-model.md](threat-model.md),
[deployment-cells.md](deployment-cells.md),
[post-v0-roadmap.md](post-v0-roadmap.md).

### Deployment Cells & Tenant Placement

Decision (go-forward): both managed and self-host run on a **cell-based**
architecture — a fleet of independent cells, each authoritative for its own tenants,
plus a thin global control plane for routing. Sequenced as go-forward, alongside the
collaboration epic, after the realm-local core. The model is tracked in
[deployment-cells.md](deployment-cells.md).

Model:

- **Cell.** One complete, independent Witself stack (`witself-server` +
  Postgres/pgvector + KMS + blob) in a cloud account/region (AWS account #1, AWS
  account #2, a GCP project, Azure, …). Cells are isolated; a cell outage affects only
  its tenants (blast-radius containment). An independent second AWS account is simply
  another cell.
- **Control plane.** A thin, HA, globally-replicated service holding **only** routing
  metadata (`realm/account -> home cell + endpoint + signing key`). It does
  **placement** (which cell a new tenant lands on) and **resolution** (where to
  route), and holds **no tenant data**. It extends the existing
  `--endpoint`/token model to "resolve my home cell." Keep it thin so its blast
  radius is tiny; it is the one new always-on global component.
- **Placement / landing.** At account/realm creation the control plane picks a cell
  by region/data-residency, capacity, provider preference, or rollout wave; records
  the mapping; clients resolve their home cell.
- **Multi-cloud.** AWS / GCP / Azure across multiple accounts, reusing the per-cloud
  Terraform modules already planned (see
  [terraform-infrastructure.md](terraform-infrastructure.md),
  [cloud-targets.md](cloud-targets.md)).
- **Cells at different versions.** Cells may run different software versions (canary /
  wave rollout); capability discovery lets clients adapt. This is a strength of the
  cell model, not a problem.
- **Fleet, not multi-master.** Many independent live cells, each authoritative for its
  own tenants. There is no shared-data multi-master across clouds in v1; that is a
  much harder, deferred thing.
- **Shared global directory.** The collaboration relay needs `realm-handle ->
  where-it-lives + signing-key` — the **same** registry the control plane maintains.
  Cells and collaboration share the global directory.
- **Billing.** Account-level billing; if one account's realms span cells, per-realm
  usage aggregates across cells at the account level (see
  [Billing And Limits](#billing-and-limits)).

Tenant migration (move a realm/account between cells):

- Export from cell A -> import into cell B -> repoint the control-plane mapping ->
  cut over.
- **Open plane** (memories/facts) moves via the existing first-class export/import
  (embeddings recomputed at the destination, or moved if the model matches; see
  [Identity Export and Import](#identity-export-and-import)).
- **Sealed plane** (secrets) is KMS-rooted per cell/cloud, so migration **re-wraps**
  keys under the destination KMS (audited decrypt-at-source / re-encrypt-at-dest; see
  [Key Hierarchy](#key-hierarchy)). Bounded, but not free.

Open decisions (document, do not resolve):

- **Placement unit** — account vs realm. Recommended: the realm is the placement /
  migration unit, account-level default, with realms individually re-homeable.
- **Self-host topology** — single-cell vs multi-cell self-host.
- **Migration cutover** — brief read-only freeze vs dual-write + reconcile.

Cross-links: [backend-architecture.md](backend-architecture.md),
[cloud-targets.md](cloud-targets.md),
[terraform-infrastructure.md](terraform-infrastructure.md),
[storage.md](storage.md), [billing-and-limits.md](billing-and-limits.md),
[backup-and-recovery.md](backup-and-recovery.md),
[agent-collaboration.md](agent-collaboration.md).

### Interface Invariants

Decision (go-forward): the agent-facing surface holds a small set of invariants so
collaboration, cells, and the realm-local core all present the same shape.

- **MCP everywhere, full parity.** Every agent-facing capability works via MCP with
  full parity, not just via CLI. CLI and MCP share request/response contracts so they
  cannot drift (see [CLI, MCP, And JSON Contracts](#cli-mcp-and-json-contracts)).
- **CLI is primary/canonical.** The CLI is the canonical control plane and the
  reference behavior; MCP mirrors it.
- **No agent-run HTTP servers for normal I/O.** Agents are **outbound clients**. The
  only HTTP server is the backend (`witself-server` / relay). An optional
  wake-webhook exists **only** for already-hosted cloud autonomous agents and is never
  required.
- **The `listen` verb.** Add `listen`/`recv` (long-poll: block up to N seconds, return
  inbound) to both CLI (`message listen`) and MCP (`witself.message.listen`) next to
  send/list/read. The durable mailbox is the source of truth; **offline recipients are
  the default** (send never needs the recipient online; drains on next listen); v0
  transport is **polling-first**. Agents are told to listen in the agent directive
  (the context-hydration teaching stanza: "to hear, call the witself listen tool each
  loop"; see [context-hydration.md](context-hydration.md)).
- **Any live stream is a latency accelerator only**, never the system of record.

These invariants apply to the realm-local messaging core today and extend unchanged
to the cross-realm substrate (see
[Agent Collaboration Substrate](#agent-collaboration-substrate-cross-realm)).
Detailed contracts land in a follow-up pass.

## Managed Service Decisions

### Data-At-Rest Note

Decision: Witself runs a two-tier data-at-rest model (see
[Encryption (Two-Tier)](#encryption-two-tier)). This note covers the **open
plane**; the sealed plane is the encryption-pillar plane.

Open-plane posture:

- The open plane (memories, facts, embedding vectors) is identity data protected
  for integrity and authenticity, not a secret vault. Use ordinary data-at-rest
  encryption (managed RDS/disk, or self-host-owned disk encryption). The open plane
  has no KMS/envelope/client-side-decrypt pillar, no reveal ceremony, and no
  end-to-end secret model.
- Optional field-level encryption for `sensitive` facts is a capability an operator
  can enable, not the default behavior. `sensitive` is a PII/redaction display
  flag, not an encryption boundary; there is no value-size split between sensitive
  and non-sensitive open-plane values.

Sealed-plane posture:

- The sealed plane (secrets, TOTP seeds) IS the encryption pillar: KMS-backed
  envelope encryption (CMK → per-realm KEK → per-secret/field DEK) with the reveal
  ceremony. KMS is a required dependency and readiness gate **only when the sealed
  plane is enabled**; an open-plane-only deployment needs no KMS. The model is
  tracked in [encryption-model.md](encryption-model.md) and
  [key-hierarchy.md](key-hierarchy.md), with KMS provider config and the
  `realm_keys`/`secret_deks` tables in [storage.md](storage.md).

This is tracked in [storage.md](storage.md).

### Production Storage

Decision: use Postgres with pgvector first, an embedding-provider abstraction
(`voyage` default), a KMS-provider abstraction for the sealed plane, object/blob
storage where the data shape needs it, and Goose for database migrations.

Storage posture:

- Postgres with the pgvector extension is the production system of record. Identity
  records and their embedding vectors live in Postgres; sealed-plane envelope
  ciphertext, the `realm_keys` (wrapped per-realm KEK), and `secret_deks` (wrapped
  per-secret/field DEK) live in Postgres too.
- Object/blob storage is for large exports, diagnostic bundles, support
  attachments, encrypted secret attachments, backup artifacts, and future
  import/export jobs.
- Memory content and fact values are ordinary identity data protected by
  data-at-rest encryption; there is no requirement to keep them out of database
  columns. Optional field-level encryption of `sensitive` facts may wrap those
  specific values when enabled.
- Sealed-plane secret field values and TOTP seeds are stored only as envelope
  ciphertext; plaintext secret material never becomes an ordinary database column.

KMS posture (sealed plane only):

- KMS providers are `aws-kms` (managed v0 first), `gcp-kms`, `azure-key-vault`, and
  `local-dev`. The provider is selected by `WITSELF_KMS_PROVIDER` and the key by
  `WITSELF_KMS_KEY_ID`. KMS is required and a readiness gate only when the sealed
  plane is enabled; pgvector remains a hard gate for the open plane regardless. The
  KMS provider config and key tables are detailed in [storage.md](storage.md).

Embedding posture:

- The embedding provider is a configurable boundary: `voyage` (default), `openai`,
  `local-dev`. Vectors are stored via pgvector. The provider and model are reported
  by the capabilities contract.

Migration posture:

- Database migrations use Goose.
- Migrations are run through `witself-server migrate`, with an advisory lock,
  rather than through the public customer/operator CLI.
- Helm should expose an explicit migration Job path before rolling
  `witself-server`.

The storage model is tracked in [storage.md](storage.md). The separate backend
server command surface is tracked in
[server-command-surface.md](server-command-surface.md).

### First Cloud Target

Decision: implement AWS first.

AWS is the first implementation target for managed Witself Cloud, the first
self-hosted Terraform module and stack, the first production Postgres/pgvector
integration (RDS), and the first production-shaped Helm values example.

GCP and Azure remain planned provider targets. Their directories, docs, and
interfaces should exist early enough to prevent AWS-only assumptions, but their
full implementations should follow AWS.

The cloud target decision is tracked in [cloud-targets.md](cloud-targets.md).

## Post-v0 Roadmap

The following features are intentionally outside the v0 scope and are tracked in
[post-v0-roadmap.md](post-v0-roadmap.md):

- **Cross-realm agent collaboration substrate (the first post-v0 epic; the
  go-forward flagship).** Built on the realm-local messaging core, it adds
  cross-realm addressing, signed discovery, a blind relay, the loop/safety stack, and
  the `listen` verb (see
  [Agent Collaboration Substrate](#agent-collaboration-substrate-cross-realm) and
  [agent-collaboration.md](agent-collaboration.md)). This supersedes the former bare
  "cross-realm federation" line.
- **Deployment cells & tenant placement (go-forward).** A fleet of independent cells,
  each authoritative for its own tenants, plus a thin global control plane and tenant
  migration (see [Deployment Cells & Tenant Placement](#deployment-cells--tenant-placement)
  and [deployment-cells.md](deployment-cells.md)).
- MCP HTTP or other network transport.
- Web dashboard.
- Private Witself admin CLI.
- Witself utility token.
- Additional embedding providers and on-cluster local embedding services.
- Policy `deny` effects and richer policy expressions beyond v0 default-deny/allow.
- Message attachments and large structured payloads in object/blob storage.
- Field-level encryption of `sensitive` facts as a managed default.
- Automated re-embedding and embedding-model migration tooling.

These are not first-release blockers. Each one needs a threat-model update, clear
permissions, audit events, CLI/API/MCP contracts where applicable, capability
flags, support boundaries, tests, and rollout controls before it moves into an
active release plan.

## Initial Architecture Direction

Witself should follow a one-core, multiple-frontends architecture:

- The core service owns the memory, fact, recall/embedding, policy, security-group,
  messaging, export/import, and audit behavior.
- The CLI is a thin adapter over the core service.
- The MCP server is a thin adapter over the same core service.
- The backend HTTP API handlers are thin adapters over the same core service.
- Managed cloud and self-hosted deployments use the same backend API server where
  practical.

The local mock backend can start as a file-backed store, but it should live behind
the same storage/provider boundary as production adapters. That lets the local
adapter exercise the real service path (including the `local-dev` embedding
provider) while managed cloud and self-hosted deployments use production storage,
embedding providers, and operational controls.

The proposed CLI command surface is tracked in
[cli-command-surface.md](cli-command-surface.md).

JSON output and shared resource contracts are tracked in
[json-contracts.md](json-contracts.md).

The public backend API contract is tracked in [api-contract.md](api-contract.md).
The public backend route style is tracked in [api-routes.md](api-routes.md).

The memory model and semantic recall are tracked in
[memory-model.md](memory-model.md).

The facts model is tracked in [facts-model.md](facts-model.md).

The sealed-plane secret model and lifecycle are tracked in
[secret-model.md](secret-model.md). The TOTP/2FA authenticator is tracked in
[totp-2fa.md](totp-2fa.md). Secret field size limits and attachments are tracked in
[secret-size-and-attachments.md](secret-size-and-attachments.md).

The full data model for both planes (realm/account/operator/agent/token tables;
open-plane memories with the pgvector embedding column, facts, policies,
security_groups, group_members, messages, audit, usage; and sealed-plane secrets,
secret_fields, secret_grants, totp_enrollments, realm_keys, secret_deks,
attachments) is tracked in [data-model.md](data-model.md).

The sealed-plane confidentiality and envelope-encryption model is tracked in
[encryption-model.md](encryption-model.md), and the CMK → per-realm KEK → DEK key
hierarchy in [key-hierarchy.md](key-hierarchy.md).

The role/scope model spanning both planes (open-plane memory/fact/policy/group/
message scopes plus sealed-plane secret/totp scopes, secret grants, and realm
roles) is tracked in [authorization-and-roles.md](authorization-and-roles.md).

The cross-agent access policy engine governs the OPEN plane and is tracked in
[access-policy.md](access-policy.md). Sealed-plane cross-agent/operator access uses
grants plus realm roles per
[authorization-and-roles.md](authorization-and-roles.md), not the open cross-agent
read/curate/forget verbs.

Security groups are tracked in [security-groups.md](security-groups.md).

Inter-agent messaging is tracked in
[inter-agent-messaging.md](inter-agent-messaging.md).

The production storage and migration decisions are tracked in
[storage.md](storage.md).

The cloud provider implementation order is tracked in
[cloud-targets.md](cloud-targets.md).

The agent token lifecycle is tracked in [token-lifecycle.md](token-lifecycle.md).

The billing, usage, and limits model is tracked in
[billing-and-limits.md](billing-and-limits.md).

The backup, export, and recovery model is tracked in
[backup-and-recovery.md](backup-and-recovery.md).

Backend architecture and self-hosting are tracked in
[backend-architecture.md](backend-architecture.md) and
[self-hosting.md](self-hosting.md).

The self-hosted support boundary is tracked in
[self-host-support.md](self-host-support.md).

The first self-hosting chart is tracked in [helm-chart.md](helm-chart.md).

Terraform infrastructure modules and stacks are tracked in
[terraform-infrastructure.md](terraform-infrastructure.md).

The separate backend server binary command surface is tracked in
[server-command-surface.md](server-command-surface.md).

MCP tool names, inputs, outputs, and exposure rules are tracked in
[mcp-tools.md](mcp-tools.md).

Build, release, Go module, Homebrew, and installer expectations are tracked in
[release-and-build.md](release-and-build.md).

The implementation sequence is tracked in
[implementation-plan.md](implementation-plan.md).

Security posture and vulnerability handling are tracked in
[threat-model.md](threat-model.md) and [security-policy.md](security-policy.md).

Public-code, contribution, license, package, and support boundaries are tracked in
[governance-and-support.md](governance-and-support.md).

Competitive research and market patterns are tracked in
[competitive-analysis.md](competitive-analysis.md).
