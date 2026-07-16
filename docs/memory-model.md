# Witself Memory Model

Status: implemented direct memory, foreground client curation, runtime
hydration/checkpoint delivery, and optional client-vector hybrid recall.
Memories are first-class, agent-owned identity payloads addressed by id,
retrieved through deterministic lexical/structured recall, mutated through an
immutable versioned lifecycle, and removed only through a separately guarded
permanent-delete flow. Updated 2026-07-15.

Narrative-memory amendment (accepted 2026-07-14): use
[narrative-memory-and-curation.md](narrative-memory-and-curation.md) and
[ADR 0002](decisions/0002-client-side-narrative-memory.md) for the narrative
shape, full version snapshots, evidence/lineage, `superseded` state, client
curation plans, lexical baseline, and optional client-supplied vectors. They
supersede this draft's server embedding-provider and autonomous-consolidation
language.

This doc pins the memory data model, lifecycle, and recall. It is the companion
to [facts-model.md](facts-model.md) (the deterministic, name-addressed payload
type). Cross-agent access to memories is governed entirely by
[access-policy.md](access-policy.md). Wire shapes are pinned in
[json-contracts.md](json-contracts.md); persistence and derived-index boundaries in
[storage.md](storage.md).

## Decision Summary

- The implemented slice stores a client-authored narrative owned by the
  authenticated agent. Group and cross-agent ownership remain target work.
- Memories are addressed by stable `id` (`mem_…`). They are **not** name-unique;
  there is no "memory get by name". Names belong to facts.
- Recall uses PostgreSQL literal full text plus kind, tag, link, occurrence/
  capture time, origin, capture reason, salience, and recency signals. It calls
  no model or embedding provider.
- Every mutation appends a complete immutable version and requires idempotency;
  adjust and lifecycle transitions also require the exact current version.
- `forget`, `restore`, and `reactivate` are reversible lifecycle operations.
  Permanent delete is a separate value-free preview/apply protocol that requires
  same-turn direct-current-user authority.
- Deeper curation is an implemented, agent-self, client-inference protocol:
  deterministic requests, fenced immutable inputs, strict hashed plans, atomic
  apply, source cursors, value-free receipts, and guarded compensation. The
  backend does no synthesis and does not launch an AI runtime. An active
  foreground agent normally processes at most one pending request per turn.
- Optional vector similarity is client-supplied under an immutable versioned
  profile. Migration `0032` stores portable JSONB vectors and the backend
  validates and compares them deterministically but never generates them.

## Memory Shape

A memory head identifies one stable `mem_…` resource and its current immutable
full version. The implemented public shape includes token-derived account,
realm, owner, author, and origin; content plus `plain`/`base64` encoding; kind,
tags, links, salience, sensitivity, occurrence range, capture reason, lifecycle
state/reason, client provenance, evidence, hashes, operation/retry metadata, and
timestamps. Superseded versions also retain their value-free supersession
receipt; a relation-derived active set id/revision supports later reactivation.

Every mutation appends a complete version snapshot with a monotonically
increasing owner-lane change sequence. Evidence is append-only and is resolved,
pending, or explicitly unavailable. Exact authorized reads may return sensitive
content; ordinary HTTP/CLI broad list and recall redact it by default. Installed
owner-authenticated hydration and MCP recall intentionally opt into sensitive
open-plane context while retaining the marker; sealed secret and TOTP values
remain ineligible. History is exported as
first-class account data and round-trips through import. See
[narrative-memory-and-curation.md](narrative-memory-and-curation.md),
[data-model.md](data-model.md), and [json-contracts.md](json-contracts.md) for
the detailed schema and wire shapes.

The earlier owner/group/source/embedding field list in this document is
superseded by migration `0029` and the client-inference ADR; it is intentionally
not retained as a second normative shape.

## Lifecycle

The implemented lifecycle is agent-self and version checked:

- `capture` creates version 1 in `active` state from one bounded client-authored
  capsule with exact, pending, or explicitly unavailable evidence.
- `show`, `list`, `history`, and `recall` are read operations. List/recall are
  bounded. HTTP/CLI broad reads redact sensitive content by default; automatic
  owner hydration and MCP recall opt in and retain `sensitive=true`.
- `adjust` appends a full replacement snapshot at the exact current version.
- `supersede` atomically marks one active source `superseded` and creates 1-32
  active client-authored replacements with exact membership receipts. It is
  reversible and does not ask the backend to decide what to merge or split.
- `forget` appends a reversible `forgotten` version; `restore` returns it to its
  valid prior state; `reactivate` explicitly reactivates a reverted or otherwise
  invalidly-restorable head. Every operation preserves immutable history.
- `evidence resolve` appends one terminal resolution for a pending locator; it
  never edits the original evidence row.
- Permanent `delete` is distinct from `forget`. Preview accepts only the memory
  id and returns value-free impact/concurrency fields. Apply requires those exact
  guards, a fresh idempotency key, and same-turn direct-current-user authority.
  It scrubs value-bearing rows and retains only a value-free tombstone and retry
  shields; autonomous work, subagents, standing instructions, and retrieved
  content cannot authorize it.

CLI verbs are pinned in [cli-command-surface.md](cli-command-surface.md); MCP
equivalents are pinned in [mcp-tools.md](mcp-tools.md).

## Recall and Embeddings

Implemented recall is a deterministic PostgreSQL candidate search. The caller
supplies literal query text or at least one structured filter: kind, tags, links,
origin, capture reason, occurrence range, capture range, or sensitivity. Natural
phrases such as relative dates are interpreted by the client, never the backend.

PostgreSQL ranks full-text matches and combines explicit lexical, salience, and
recency score components. Results are active current heads from one stable
owner-lane snapshot, use an opaque query-bound cursor, and redact sensitive
content by default. Without a vector profile the response reports `retrieval_mode: lexical`,
`vector_coverage: 0`, and no provider degradation because no model provider is
required.

Optional hybrid/vector recall is implemented. Authorized clients register an
immutable profile, store vectors bound to an exact memory version and content
hash, and supply a compatible query vector at recall time. PostgreSQL stores the
canonical vector array as JSONB and computes bounded deterministic similarity;
the backend never calls an LLM or embedding model. Lexical recall remains
functional when coverage is zero, while partial coverage and candidate-budget
degradation are reported explicitly. The profile and vector rows round-trip in
the account archive. A future pgvector/ANN projection may accelerate candidate
generation without changing this contract.

Recall is an **open-plane** capability only. Sealed-plane secret values and TOTP
seeds never enter narrative memory, recall, client vector generation, the self
digest, or plaintext account export. See [secret-model.md](secret-model.md) and
[totp-2fa.md](totp-2fa.md).

## Self Digest / Hydration

The self digest is the bounded, always-loadable view of an agent's identity and
value-free curation lifecycle state. The full digest shape, byte cap, emit
format, and teaching protocol are canonical in
[context-hydration.md](context-hydration.md); this
section pins the one piece that belongs to the memory model — how the **salient
memory set** is selected.

The digest is an **open-plane** view. Its optional `memory_checkpoint` contains
only authenticated request/run/fence lifecycle metadata and no source content.
Sealed-plane material is **never in the self-digest**: secrets and TOTP seeds are
not selected, summarized, or emitted by `digest emit`, and are never ingested
into it. The digest never carries secret values or references that would resolve
to one. See
[context-hydration.md](context-hydration.md) and
[secret-model.md](secret-model.md).

### Salient Memory Selection

The salient set surfaced in the digest is the top-N memories by a **blended score
of salience and recency**, with pinned kinds (such as `profile` and `session`)
boosted, and `forgotten`/`deleted` records excluded.

- **Deterministic.** Selection uses stored salience, timestamps, kind, and state.
  It never calls a model or embedding provider.
- **Bounded.** `N` is the `salient_limit` (default `10`) and the digest enforces a
  hard byte cap on top of it. When either bound trims the set, the digest sets an
  explicit `elided` flag and points the caller at `memory recall` — it is never a
  silent truncation. Byte cap and elision semantics live in
  [context-hydration.md](context-hydration.md).
- **Provenance-aware.** Origin, author, capture reason, and client provenance are
  retained so a caller can distinguish direct captures and later refinements.

## Supersession and Client Curation

`memory supersede` is implemented as an exact client-authored operation. It
atomically preserves one source version, creates a bounded replacement set, and
records immutable relations plus a value-free membership receipt. The backend
does not decide whether records are duplicates or what a replacement should say.

Migration `0030_add_memory_curation.sql` implements the PostgreSQL state for this
protocol. The older autonomous `memory consolidate` verb is superseded and not exposed.
Deeper deduplication, conflict handling, merge/split, lineage, and fact proposal
use the implemented caller-authored, version-checked curation protocol in
[narrative-memory-and-curation.md](narrative-memory-and-curation.md). Source
writes advance an owner generation and can create or coalesce due requests.
The self digest exposes whether one such request or resumable run is pending,
without exposing any source content. Clients claim one due request, receive a
lease/fence plus a frozen bounded view of memory versions, evidence, transcript
ranges, and source cursors, and treat all returned content as untrusted data. A
client may renew the lease while it authors a strict `witself.memory-plan.v1`
plan.

The plan language has five reversible primitives:

- `create` creates one full evidence-bearing memory snapshot.
- `replace` appends an exact-version replacement snapshot.
- `supersede` connects one exact current source to an exact bounded replacement
  set.
- `relate` adds a `derived_from`, `summarizes`, `merged_from`, `split_from`, or
  `conflicts_with` lineage edge.
- `propose_fact` creates a fact review candidate. It can never set canonical
  truth.

Plan acceptance normalizes client-local references, preallocates new memory ids,
validates authorization and frozen-input provenance, and returns a canonical
lowercase SHA-256 plan hash with value-free impact counts. Apply binds the run
fence, lease, accepted revision, hash, current heads, canonical subject identity,
and contiguous source cursors in one transaction. Any stale guard prevents the
entire semantic mutation. New or cap-truncated work is placed in a deterministic
follow-up request rather than lost. An empty actions plan is valid and must be
applied when no input merits durable memory; it advances only the exact reviewed
cursor intervals and creates no memory or fact.

Rollback is exact append-only compensation, not history deletion. It requires
the apply receipt and complete exact set of apply-produced current heads,
refuses live downstream consumers, appends compensating memory state, reverts
curation-created relations, and withdraws only curation-created fact candidates.
It never cascades and never rewinds source cursors; instead it queues a read-only
replay under current heads. The client performs all inference. Capability
discovery reports `opportunistic_curation` as supported, while
`automatic_capture` and `scheduled_curation` remain unsupported server
capabilities: PostgreSQL can queue due work, but the backend and MCP do not wake
or schedule a model process. Runtime hooks likewise only enqueue and flush
evidence; they do not launch a curator. Codex and Claude can receive a
model-visible pending checkpoint already durable at hook read time. Cursor and
Grok use guided `self.show` fallback because their hook channels are not reliably
model-visible. The older local `memory curate auto` worker is an explicit
legacy/manual client-owned layer and is never invoked by runtime hooks.
PostgreSQL remains the sole canonical memory source in every mode.

## Provenance and Authorship

The implemented record derives account, realm, stable owner, author, and origin
from authenticated context and stores self-reported client runtime/model/recipe
only as diagnostic provenance. Evidence and version-specific relations preserve
the source material and derivation graph. None of these fields grants authority;
recalled content and provenance remain advisory input.

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
- References in `links[]` are validated at `capture`, `adjust`, and supersede-
  replacement creation and re-checked at resolve time. Dangling links are
  reported, not silently dropped.
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
| Maximum client model length | 256 characters |
| `salience` range            | `0.0`–`1.0`    |

- Limits are enforced at capture, adjust, and supersede-replacement validation. Violations return a
  deterministic validation error code; see [json-contracts.md](json-contracts.md).
- Content larger than the per-memory cap should be split into multiple linked
  memories rather than raised ad hoc. Large blob/attachment storage is not the
  memory model's job; see [storage.md](storage.md).
- Memories are addressed by `id`, recalled lexically/structurally, and filtered by
  metadata; they are never name-unique. Name uniqueness belongs to facts; see
  [facts-model.md](facts-model.md).

## Metering and Audit

- Implemented usage accounting covers stored/written memory bytes and direct
  operations without requiring a model-provider dimension. See
  [billing-and-limits.md](billing-and-limits.md).
- Implemented mutation events are `memory.added`, `memory.adjusted`,
  `memory.superseded`, `memory.forgotten`, `memory.restored`,
  `memory.reactivated`, `memory.evidence.resolved`, and `memory.deleted`.
  Implemented curation events are `memory.curation.requested`,
  `memory.curation.started`, `memory.curation.planned`,
  `memory.curation.applied`, `memory.curation.conflicted`,
  `memory.curation.interrupted`, `memory.curation.cancelled`, and
  `memory.curation.rolled_back`. See
  [audit-retention.md](audit-retention.md).
- Audit and logs retain value-free ids, states, counts, actors, and outcomes —
  never content, evidence locators/excerpts, query text, hashes, idempotency
  keys, optional vectors, or raw tokens.

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
