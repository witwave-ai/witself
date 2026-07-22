# ADR 0002: Portable Narrative Memory Uses Client-Side Inference

Status: accepted and implemented (2026-07-14). Direct storage, lifecycle,
archive portability, salient hydration, lexical and optional hybrid recall,
atomic supersede, client-side curation/automation, runtime preflight recall,
and client-supplied vector profiles/rows are implemented end to end in the
current checkout.

## Context

Witself already has canonical facts and an append-only visible transcript
ledger, while the older drafts route narrative context to each runtime's native
memory. That leaves durable context split by product and machine. Other drafts
also assign embeddings and an intelligent `memory.consolidate` operation to the
server, which would require backend inference and make behavior provider- and
cloud-dependent.

The product needs one memory layer that follows an account across the currently
supported Codex, Claude Code, Grok Build, and Cursor integrations and deployment
cells on AWS, Azure, and Google Cloud. Gemini was deferred; GitHub Copilot later
entered scope as a phase-one managed-instructions and stdio-MCP adapter without
transcript hooks. The portable core must preserve facts as canonical
assertions, allow native memories to coexist, use the client's existing
inference, and round-trip through account export/import.

## Decision

Add first-class Witself narrative memory alongside facts.

PostgreSQL is authoritative for memory heads, immutable full versions, evidence,
lineage, curation requests/runs/actions/cursors, and retrieval metadata.
Transcripts remain immutable evidence rather than memories.

All semantic judgment is client-side. A parent agent, native subagent, or
separate local/headless client writes an immediate capture or submits an exact
curation plan. The backend performs deterministic authorization, validation,
optimistic-concurrency checks, atomic application, audit, search, rollback, and
export/import. It does not call an LLM or embedding model.

Explicit narrative remember requests write a client-authored capsule to Witself
in the same turn. Hooks capture transcript evidence and may mark curation due,
but MCP cannot initiate inference and hook paths do not wait for it. A client
supervisor, scheduler, subagent, parent agent, or later session performs deep
curation.

Curation compiles to a small deterministic plan language: `create`, `replace`,
`supersede`, `relate`, and `propose_fact`. Merge and split are compositions of
those primitives. Automatic curation is reversible, fact proposals never
silently become canonical facts, and hard deletion is excluded.

Recall uses PostgreSQL full-text, structured time and metadata filters,
salience, and recency as the universal baseline. Migration `0032` adds optional
client-supplied memory and query vectors under immutable versioned profiles.
The backend stores canonical vectors as portable JSONB and compares them with
deterministic bounded hybrid ranking, but never generates them. A later
pgvector/ANN projection may accelerate candidate generation without changing
the correctness contract or becoming a deployment prerequisite.

Witself is the portable system of record. Native-provider memory remains an
optional advisory convenience. Both are targeted only on an explicit request,
with separate outcomes because the operations cannot be atomic and some native
providers expose no transactional write API. Provider auto-memory may still
capture independently; Witself does not claim control over it.

The protocol requires no native subagent feature. Runtime adapters must support
the main-agent or separate-process path. Runtime, model, machine, session, and
run are provenance; the stable Witself agent is the normal cross-runtime owner.

Account archives carry the narrative graph, curation metadata, immutable vector
profiles, and version/content-hash-bound vector rows. Full-text and any future
ANN indexes are derived and rebuilt after import; active leases never move
between cells. Source freeze or placement-epoch fencing is required at cutover.

## Consequences

- Witself can replace dependence on product-local memory for durable,
  cross-machine context without disabling native memory.
- Explicit and current-agent captures are durable immediately; hook evidence is
  real-time where supported, while deeper synthesis remains asynchronous and
  low-touch.
- The backend has no AI credentials, model cost, inference availability, or
  model-vendor dependency.
- All three cloud targets run the same PostgreSQL-backed protocol.
- Client quality may differ, so provenance, evidence, model/recipe identity,
  versioning, preview, conflict detection, and rollback are first-class.
- Lexical recall works for every runtime; vector recall is an optional
  acceleration rather than a product availability gate.
- Native dual writes and provider-wide native search remain best-effort and
  must be reported honestly.
- Every narrative schema change also changes export/import and its round-trip
  tests.

## Superseded Draft Decisions

This ADR supersedes only the conflicting portions of drafts that:

- route all natural narrative memory exclusively to the current runtime;
- let the server infer whether prose is a fact or memory;
- have the backend call Voyage, OpenAI, or a local embedding model;
- have `memory.consolidate(scope, dry_run)` choose semantic merges or
  supersessions without a caller-authored plan;
- treat session end as the only durable narrative checkpoint;
- model `forgotten` as the only non-active state instead of distinguishing a
  reversible `superseded` representation.

ADR 0001's open-plane/sealed-plane model remains accepted. Facts, access policy,
transcript immutability, and the sealed-plane boundary remain intact.

## Alternatives Considered

- **Use native memory only.** Rejected because it fragments identity by runtime,
  machine, and provider and cannot satisfy portable account export.
- **Run a backend synthesis worker.** Rejected because it violates the
  client-inference boundary, introduces model credentials/cost into every cell,
  and makes self-hosting behavior diverge.
- **Make MCP start curation automatically.** Rejected because MCP is
  request/response transport and cannot wake an inference client.
- **Require a native subagent.** Rejected because runtime capabilities and
  semantics differ. Subagents remain an optional optimization.
- **Store only transcripts and search them.** Rejected because transcripts are
  evidence, not a compact, versioned, intentionally curated self.
- **Require embeddings for recall.** Rejected because the supported agents do
  not all expose compatible embedding APIs and lexical/time/metadata recall is
  a portable deterministic baseline.

## Related

- [narrative-memory-and-curation.md](../narrative-memory-and-curation.md)
- [memory-model.md](../memory-model.md)
- [facts-model.md](../facts-model.md)
- [agent-memory-routing.md](../agent-memory-routing.md)
- [transcript-ledger.md](../transcript-ledger.md)
- [backup-and-recovery.md](../backup-and-recovery.md)
- [0001-consolidate-witpass-into-witself.md](0001-consolidate-witpass-into-witself.md)
