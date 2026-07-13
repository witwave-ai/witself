# Witself Memory Model

Status: draft. Decision: memories are a first-class, agent-owned identity
payload, addressed by id, recalled **semantically by default**, mutated through a
versioned lifecycle, and removed through a reversible soft `forget` before any
guarded hard `delete`. Last reviewed 2026-06-26.

This doc pins the memory data model, lifecycle, and recall. It is the companion
to [facts-model.md](facts-model.md) (the deterministic, name-addressed payload
type). Cross-agent access to memories is governed entirely by
[access-policy.md](access-policy.md). Wire shapes are pinned in
[json-contracts.md](json-contracts.md); storage and the embedding boundary in
[storage.md](storage.md).

## Decision Summary

- A memory is free-form self-content owned by one agent (or, in the group case,
  by one security group; see [security-groups.md](security-groups.md)).
- Memories are addressed by stable `id` (`mem_…`). They are **not** name-unique;
  there is no "memory get by name". Names belong to facts.
- Recall is semantic by default and is the core Witself differentiator. Each
  memory carries an embedding vector; `recall` blends vector similarity with
  keyword, tag, kind, and time filtering, ranked by similarity, recency, and
  salience.
- Plain `read`/`get` by id and `list` by metadata never require the embedding
  provider. If the provider is unavailable, recall degrades deterministically to
  keyword/tag/kind/time ranking and the degraded state is surfaced.
- Mutation keeps a versioned edit history. Removal is soft (`forget`,
  reversible) before hard (`delete`, guarded, audited, irreversible).
- Embedding providers are abstracted (`voyage` default, `openai`, `local-dev`),
  selectable by environment, capability-gated, and reported through the
  capabilities contract.

## Memory Shape

A memory record has the following fields. The canonical JSON encoding (field
names, types, `witself.v0` schema version) lives in
[json-contracts.md](json-contracts.md); this section is the semantic source of
truth.

- `id`: stable identifier with the `mem_` prefix. Server-assigned. Immutable.
- `realm`: the enclosing realm id (`realm_…`). Immutable.
- `owner`: the owning principal. An agent (`agent_…`) by default, or a security
  group (`grp_…`) for group-scoped collective memory. Ownership is set at `add`
  time and changes only through an explicit, audited re-home operation.
- `content`: free-form text. This is the primary payload and the input to
  embedding. Required.
- `kind`: a convention-driven type label such as `episodic`, `semantic`,
  `profile`, or `note`. Kind is an open string used for filtering and ranking,
  not a fixed column with a closed enum. Unknown kinds are accepted. Default
  `note`.
- `tags[]`: short string tags for filtering and ranking.
- `source`: provenance / authorship, first-class. Canonical values: `self` (the
  owning agent authored it), `agent:<name>` (a cross-agent `contribute`),
  `operator` (a human operator), and `import:<file>` (ingested from a
  CLAUDE.md/AGENTS.md/GEMINI.md file). A message id (`msg_…`) may also appear for
  message-driven writes. Cross-agent, operator, and import writes always record
  their true origin here, so the digest and [Consolidation](#consolidation) can
  prioritize human-/agent-/import-authored records and never silently overwrite
  them; see [Provenance and Authorship](#provenance-and-authorship),
  [access-policy.md](access-policy.md) and
  [inter-agent-messaging.md](inter-agent-messaging.md).
- `salience`: a numeric importance/weight hint used in recall ranking. Range
  `0.0`–`1.0`, default `0.5`. Higher is more important.
- `links[]`: references to other memories or facts, expressed as `witself://`
  references (see [References](#references)). Validated at write time and
  re-checked at resolve time.
- `sensitive`: optional boolean PII marker. Sensitive content is redacted in
  list/scan output by default and handled carefully in audit and logs. Default
  `false`.
- `created_at`, `updated_at`, `last_accessed_at`: RFC 3339 timestamps.
- `version`: monotonically increasing integer, starting at `1` on `add`,
  incremented on each `adjust`.
- `history[]`: versioned edit history (see [Edit History](#edit-history)).
- `state`: `active`, `forgotten` (soft-deleted, tombstoned), or `deleted` (hard,
  terminal). Default `active`.

Notes:

- Memory `content` is never an ordinary readable column for unauthorized callers,
  but it is not encrypted-as-product. In the **open plane** (memories + facts)
  Witself protects identity **integrity and authenticity**, not secret
  confidentiality; authorized callers read identity content plainly and there is
  **no reveal ceremony**. This posture is scoped to the open plane: the **sealed
  plane** (secrets, TOTP) is the opposite — values are envelope-encrypted at rest
  and returned only through an explicit, audited reveal, never read plainly. See
  [secret-model.md](secret-model.md) for the sealed counterpart and
  [threat-model.md](threat-model.md).
- Embedding vectors are storage-internal and are not part of the public memory
  shape. They are never returned in API responses, logs, audit, or export. See
  [Recall and Embeddings](#recall-and-embeddings) and [storage.md](storage.md).

## Edit History

- Every `adjust` records a new history entry with the new `version` number, the
  actor (always derived from the authenticated token, never from input), a
  timestamp, the changed fields, and, for cross-agent or operator edits, the
  audit `--reason` and deciding policy id.
- History is retained for audit, conflict detection, safe operator recovery, and
  export. It is included in identity export by default; see
  [backup-and-recovery.md](backup-and-recovery.md).
- History entries inherit the `sensitive` posture of the memory. Redacted views
  redact historical content the same way they redact current content.
- History records changed fields and metadata, never embedding vectors and never
  raw tokens.

## Lifecycle

The memory lifecycle is `add → adjust → read/recall/list → forget → restore →
delete`. CLI verbs are pinned in [cli-command-surface.md](cli-command-surface.md);
MCP equivalents in [mcp-tools.md](mcp-tools.md).

### add

- Creates a memory at `version` `1`, `state` `active`.
- The creating agent is the `owner` unless an authorized caller targets a group
  via `--group` (see [security-groups.md](security-groups.md)).
- Computes and stores the embedding vector at write time when the provider is
  available; otherwise the memory is added without a vector and is flagged for
  embedding, and recall over it degrades until it is embedded.
- Validates size limits (see [Size Limits](#size-limits)) and `links[]`
  references before persisting.
- Checks for near-duplicates before persisting. On a hit, `add` does **not**
  silently create a near-duplicate: it returns the existing `mem_` id and a
  `memory_duplicate` (or `memory_merged`) entry in `warnings[]` so the caller can
  reuse or `adjust` the existing record. Larger-scale duplicate collapse and
  supersede is the job of [Consolidation](#consolidation); `fact set` is already
  upsert and never near-dups (see [facts-model.md](facts-model.md)).
- Runtime-natural `remember` requests do not automatically select Witself
  memory. Atomic durable assertions use Witself facts; narrative context stays
  on the runtime-native memory path unless the user explicitly selects
  Witself. A future explicit Witself memory capture command must still take this
  same validated `add` path. See
  [Agent Memory Routing](agent-memory-routing.md).
- Audited as `memory.added`.

### adjust

- Edits `content`, `kind`, `tags`, `source`, `salience`, `links`, or
  `sensitive`. Appends a new version to edit history; prior versions are
  retained.
- Re-embeds when `content` (or, by configuration, tags/kind that contribute to
  embedding) changes. Metadata-only edits do not force re-embedding.
- Cross-agent `adjust` requires the `curate` permission and an audit `--reason`,
  supports `--dry-run`, and requires confirmation unless `--yes`. See
  [access-policy.md](access-policy.md).
- Audited as `memory.adjusted`.

### read / get

- Deterministic retrieval by `id`. Returns the current version.
- Updates `last_accessed_at` (which feeds recency ranking).
- Does not require the embedding provider.
- Cross-agent read requires the `read` permission; audited as `crossagent.read`.
  Own-agent read is audited as configured.

### recall

- Semantic-by-default search over the caller's accessible memories. Full behavior
  in [Recall and Embeddings](#recall-and-embeddings).
- Recall over another agent's memories requires a `read` policy on the target and
  is metered as a cross-agent access. See [access-policy.md](access-policy.md) and
  [billing-and-limits.md](billing-and-limits.md).
- Audited as `memory.recalled` (and `crossagent.read` for cross-agent recall).

### list

- Enumerates memories with metadata and filters: `kind`, `tag`, `owner`,
  `source`, time range, `sensitive`, and `state`. Cursor-paginated.
- Sensitive content is redacted in list output by default.
- Does not require the embedding provider.
- By default lists `active` memories; `--include-forgotten` includes tombstoned
  records for restore workflows.

### forget

- The default destructive path. Soft delete / tombstone: sets `state` to
  `forgotten`, retains the record (content, history, vector) for the retention
  window, and excludes it from recall and default `list`.
- Reversible within the retention window via `restore`.
- Cross-agent `forget` requires the `forget` permission and an audit `--reason`,
  supports `--dry-run`, and requires confirmation unless `--yes`.
- Audited as `memory.forgotten` (and `crossagent.forgotten` for cross-agent).

### restore

- Undoes a `forget` within the retention window: sets `state` back to `active`
  and returns the memory to recall and `list`.
- Restoring re-validates `links[]` references and re-checks size limits.
- Audited as `memory.restored`.

### delete

- Hard delete. Explicit, guarded, audited, irreversible. Purges the record,
  history, and embedding vector.
- Requires confirmation unless `--yes`. Cross-agent or operator delete requires
  an audit `--reason`. Supports `--dry-run`.
- Hard cross-agent delete is a further-guarded step and is never the default
  outcome of a cross-agent `forget`; see [access-policy.md](access-policy.md).
- Audited as `memory.deleted`.

### Retention Window

- The forget retention window is the period during which a `forgotten` memory can
  be `restore`d before it becomes eligible for cleanup. The window is
  realm/plan-configurable; tombstones older than the window may be purged by a
  maintenance job, which is audited.
- Tombstone storage and purge cadence are covered in [storage.md](storage.md).

## Recall and Embeddings

Recall is the core differentiator. It is semantic by default and capability-aware.

Recall and embeddings are an **open-plane** capability only. Sealed-plane material
— secrets and TOTP seeds — is **never embedded, never returned by semantic recall,
and never ingested** from CLAUDE.md/AGENTS.md/GEMINI.md. Secret values are
envelope-encrypted at rest and reachable only through the explicit, audited reveal
path; they have no embedding vector and never appear in `recall`, `read`, or `list`
results here. See [secret-model.md](secret-model.md) and [totp-2fa.md](totp-2fa.md).

### Recall Inputs

`memory recall <query>` accepts:

- A natural-language `query` string (embedded at query time for similarity).
- Optional filters: `--kind`, `--tag`, `--owner` (cross-agent, policy-gated),
  `--source`, time range (`--since`/`--until`), `--sensitive`, and `--state`.
- Optional ranking knobs: `--limit`, and weight overrides for the ranking
  signals below (defaults documented here).

### Hybrid Ranking

Recall blends, at minimum, these signals into one score per candidate:

- **Similarity**: cosine similarity between the query vector and the memory
  vector (the dominant signal when the provider is available).
- **Lexical/keyword match**: keyword overlap between query and `content`/`tags`.
- **Tag and kind match**: boosts for memories matching requested tags/kind.
- **Recency**: favors recent `created_at` / `last_accessed_at`.
- **Salience**: the per-memory `salience` weight.

Default weights (tunable per query and per deployment): similarity `0.55`,
recency `0.20`, salience `0.15`, lexical/tag/kind `0.10` combined. Weights are
documented here so callers can reason about ranking; the storage/query
implementation lives in [storage.md](storage.md).

### Embedding Provider Abstraction

Embeddings mirror the way Witpass abstracted KMS: one interface, swappable
providers, capability-gated.

- Providers: `voyage` (default; the Anthropic-recommended embedding family),
  `openai`, and `local-dev`.
- Selection: `WITSELF_EMBEDDINGS_PROVIDER` and `WITSELF_EMBEDDINGS_MODEL`.
- `local-dev` is a deterministic or low-cost local embedder for tests, demos,
  and `witself-server serve --dev`. It is not a production provider.
- The capabilities contract reports the active provider, model, and vector
  dimensionality so callers can discover recall behavior before issuing a query.

```text
WITSELF_EMBEDDINGS_PROVIDER=voyage        # voyage (default) | openai | local-dev
WITSELF_EMBEDDINGS_MODEL=voyage-3         # provider-specific model id
```

### Storage and Degradation

- Embedding vectors are stored in Postgres via pgvector. Vector storage size is a
  metered dimension; see [billing-and-limits.md](billing-and-limits.md) and
  [storage.md](storage.md).
- If the embedding provider is unavailable or disabled, recall degrades
  **deterministically** to keyword/tag/kind/time ranking, and the capabilities
  contract reports that semantic recall is degraded. Recall never silently
  returns unranked or empty results without surfacing the degraded state.
- New or adjusted memories written while the provider is unavailable are flagged
  for embedding and are picked up when the provider returns.
- Re-embedding on a provider or model change is an explicit, audited maintenance
  operation, never an automatic side effect. Backed-up vectors restore recall
  without re-embedding; see [backup-and-recovery.md](backup-and-recovery.md).
- Embedding vectors are never logged, returned in API responses, included in
  audit, or included in export. See
  [observability-and-operations.md](observability-and-operations.md).

## Self Digest / Hydration

The self digest is the bounded, always-loadable view of an agent's identity used
at session start. The full digest shape, byte cap, emit format, and teaching
protocol are canonical in [context-hydration.md](context-hydration.md); this
section pins the one piece that belongs to the memory model — how the **salient
memory set** is selected.

The digest is an **open-plane** view. Sealed-plane material is **never in the
self-digest**: secrets and TOTP seeds are not selected, summarized, or emitted by
`digest emit`, and are never ingested into it. The digest never carries secret
values or references that would resolve to one. See
[context-hydration.md](context-hydration.md) and
[secret-model.md](secret-model.md).

### Salient Memory Selection

The salient set surfaced in the digest is the top-N memories by a **blended score
of salience and recency**, with pinned kinds (such as `profile` and `session`)
boosted, and `forgotten`/`deleted` records excluded.

- **Deterministic.** Selection uses stored `salience`, `created_at`/
  `last_accessed_at`, and `kind` only. It **never calls the embedding provider**,
  so the digest works unchanged when semantic recall is degraded (see
  [Storage and Degradation](#storage-and-degradation)). This is the same
  recency/salience input recall uses, minus the similarity signal.
- **Bounded.** `N` is the `salient_limit` (default `10`) and the digest enforces a
  hard byte cap on top of it. When either bound trims the set, the digest sets an
  explicit `elided` flag and points the caller at `memory recall` — it is never a
  silent truncation. Byte cap and elision semantics live in
  [context-hydration.md](context-hydration.md).
- **Provenance-aware.** Source (`self`/`agent:<name>`/`operator`/`import:<file>`)
  is carried through so the digest can prioritize human-/agent-authored content;
  see [Provenance and Authorship](#provenance-and-authorship).

## Consolidation

Consolidation is the memory garbage-collection pass — the fix for the append-only
store's #1 failure mode (unbounded near-duplicate drift). It is the `memory
consolidate` verb; CLI/MCP/API surfaces are pinned in
[cli-command-surface.md](cli-command-surface.md) and
[mcp-tools.md](mcp-tools.md).

- **What it does.** Over the caller's accessible memories (optionally scoped),
  consolidation: **merges** near-duplicate memories, **supersedes** stale ones
  (via the existing `forget`/`adjust` lifecycle, never a side-channel), and
  **surfaces** conflicting facts rather than auto-picking a winner. It also trims
  the digest/index. Merge and supersede flow through the standard lifecycle so
  edit history and reversibility are preserved.
- **Provenance-respecting.** Consolidation never silently overwrites human- or
  import-authored records (`source` = `operator` / `import:<file>` / `agent:<name>`);
  such conflicts are surfaced for a human/agent decision, not resolved
  automatically. See [Provenance and Authorship](#provenance-and-authorship).
- **Guarded.** `--dry-run` defaults to true, the destructive run requires the
  same `--reason`/confirmation guards as other mutating memory verbs, and the
  result reports `merged[]`, `superseded[]`, `conflicts[]`, and the trimmed index.
- **Read-only exclusion.** Consolidation is mutating and is **excluded in
  `--read-only` MCP mode** (alongside the other write verbs); `self show`,
  `session start`, `recall`, and `digest emit` remain available.
- **Audited** as `memory.consolidated`; see
  [audit-retention.md](audit-retention.md).

## Provenance and Authorship

The `source` field (see [Memory Shape](#memory-shape)) makes authorship
first-class so that human-, agent-, and import-authored records are
distinguishable from the owning agent's own writes.

- Canonical values: `self` (the owning agent), `agent:<name>` (a cross-agent
  `contribute`), `operator` (a human operator), and `import:<file>` (ingested
  from a CLAUDE.md/AGENTS.md/GEMINI.md file).
- The digest's [Salient Memory Selection](#salient-memory-selection) and
  [Consolidation](#consolidation) read `source` to prioritize authored content
  and to refuse silent overwrites of human/agent/import intent.
- This is the lightweight v0 seed of the post-v0 provenance and lineage identity
  feature. Facts carry the same `source` field; see
  [facts-model.md](facts-model.md).

## References

Memories participate in the `witself://` reference scheme so `links[]`, scripts,
config, and MCP tools can point at identity data without copying it. Reference
parsing/resolution is pinned in [json-contracts.md](json-contracts.md).

- Current agent's memory by id or path: `witself://memory/<path-or-id>`
- A specific agent's memory (cross-agent, policy-gated):
  `witself://agent/<agent>/memory/<id>`
- Group-scoped memory: `witself://group/<group>/memory/<id>`

Rules:

- References resolve only through authorized commands or MCP tools. Resolution
  enforces the same authorization as a direct read; a cross-agent or cross-group
  reference resolves only when policy permits.
- References in `links[]` are validated at `add`/`adjust`/`restore` and re-checked
  at resolve time. Dangling links are reported, not silently dropped.
- Facts may be linked from a memory via `witself://fact/<name>` and
  `witself://agent/<agent>/fact/<name>`; see [facts-model.md](facts-model.md).

## Size Limits

V0 defaults (refined before implementation):

| Limit                       | Default        |
| --------------------------- | -------------- |
| Maximum `content` size      | 256 KiB        |
| Maximum `tags` per memory   | 64             |
| Maximum tag length          | 128 characters |
| Maximum `links` per memory  | 256            |
| Maximum `source` length     | 1 KiB          |
| `salience` range            | `0.0`–`1.0`    |

- Limits are enforced at `add`, `adjust`, and `restore`. Violations return a
  deterministic validation error code; see [json-contracts.md](json-contracts.md).
- Content larger than the per-memory cap should be split into multiple linked
  memories rather than raised ad hoc. Large blob/attachment storage is not the
  memory model's job; see [storage.md](storage.md).
- Memories are addressed by `id`, recalled semantically, and filtered by
  metadata; they are never name-unique. Name uniqueness belongs to facts; see
  [facts-model.md](facts-model.md).

## Metering and Audit

- Metered memory dimensions: stored memories, memory writes (`add`/`adjust`),
  memory recalls and reads, embedding operations, vector storage size, and
  cross-agent accesses. See [billing-and-limits.md](billing-and-limits.md).
- Audit event names for memories: `memory.added`, `memory.adjusted`,
  `memory.recalled`, `memory.forgotten`, `memory.restored`, `memory.deleted`,
  `memory.consolidated` (see [Consolidation](#consolidation)), plus
  `crossagent.read` / `crossagent.contributed` / `crossagent.curated` /
  `crossagent.forgotten` for cross-agent actions. Audit content rules and
  retention are in [audit-retention.md](audit-retention.md).
- Audit and logs record ids, owner, kind, tags, source, decision outcome, and
  deciding policy id — never `content`, embedding vectors, or raw tokens.

## Related Docs

- [requirements.md](requirements.md)
- [context-hydration.md](context-hydration.md)
- [facts-model.md](facts-model.md)
- [access-policy.md](access-policy.md)
- [security-groups.md](security-groups.md)
- [inter-agent-messaging.md](inter-agent-messaging.md)
- [storage.md](storage.md)
- [json-contracts.md](json-contracts.md)
- [cli-command-surface.md](cli-command-surface.md)
- [mcp-tools.md](mcp-tools.md)
- [billing-and-limits.md](billing-and-limits.md)
- [audit-retention.md](audit-retention.md)
- [backup-and-recovery.md](backup-and-recovery.md)
- [observability-and-operations.md](observability-and-operations.md)
- [threat-model.md](threat-model.md)
