# Witself Context Hydration, Teaching, and the File Bridge

Status: draft. Decision: Witself is a **service the agent must be taught to
call**, not a file the harness auto-loads. To make a self/identity store
reliable for agents, v0 ships three things this doc pins: an always-injected,
bounded **self-digest**; a three-surface **teaching layer** that installs the
recall-before-act / write-after-learn habit; and a two-way **file bridge**
(`digest emit` / `ingest`) that makes Witself a good citizen of the
CLAUDE.md/AGENTS.md ecosystem. Last reviewed 2026-06-26.

This doc is the canonical home for hydration, teaching, and file interop. The
underlying payloads are pinned in [memory-model.md](memory-model.md) (memories,
recall, salience) and [facts-model.md](facts-model.md) (deterministic,
name-addressed facts). The tool surface is pinned in [mcp-tools.md](mcp-tools.md)
and [cli-command-surface.md](cli-command-surface.md). The master decision log is
[requirements.md](requirements.md).

## Why This Exists

CLAUDE.md and AGENTS.md are read automatically by the harness at session start.
Witself is not: it is reached over MCP, CLI, or API, and an agent that is never
told to call it will simply not call it. A persistent self/identity store that is
never loaded is worse than no store, because it silently diverges from what the
agent actually believes about itself.

So Witself closes the gap from both ends:

- **Pull** — a cheap, always-available `self show` digest the runtime can inject
  at session start, the MCP analogue of an auto-loaded CLAUDE.md head.
- **Teach** — a standing protocol (MCP server `instructions`), trigger-laden tool
  descriptions, and a paste-able file stanza, all saying the same thing, so the
  habit installs whether the agent learns via MCP or via the file ecosystem.
- **Bridge** — `digest emit` writes a Witself-backed fragment into the files the
  harness already loads, and `ingest` pulls existing CLAUDE.md/AGENTS.md content
  back into facts and memories. Witself participates in the AGENTS.md ecosystem
  rather than competing with it.

## The Self-Digest (`self show`)

The self-digest is the bounded, session-start snapshot of who this agent is. It
is the one call a runtime should make (alongside `session start`) before a
non-trivial task. It is **cheap by design**: it never requires the embedding
provider, so it works even when semantic recall is degraded (see
[memory-model.md](memory-model.md#recall-degradation)).

The digest is **open-plane only.** It is built from primary facts and salient
memories; sealed-plane material — secrets and TOTP seeds — is **never** part of
it. See [The Sealed-Plane Carve-Out](#the-sealed-plane-carve-out) below.

### Shape

`witself self show` (MCP `witself.self.show`, API `GET /v1/self`) returns:

```json
{
  "schema": "witself.v0",
  "identity": {
    "agent": "agent_atlas",
    "realm": "realm_acme",
    "display_name": "Atlas"
  },
  "primary_facts": [
    { "id": "fact_001", "name": "display-name", "value": "Atlas", "primary": true, "source": "self" },
    { "id": "fact_002", "name": "package-manager", "value": "pnpm", "primary": true, "source": "self" }
  ],
  "salient_memories": [
    { "id": "mem_120", "snippet": "Prefers terse, decision-led writing.", "kind": "profile", "salience": 0.9, "source": "self" },
    { "id": "mem_133", "snippet": "Mid-migration to the v0 storage adapter; resume at the pgvector step.", "kind": "session", "salience": 0.8, "source": "self" }
  ],
  "index": {
    "kinds": ["profile", "session", "semantic", "note"],
    "tags": ["preferences", "project:witself"],
    "counts": { "facts": 12, "memories": 84 }
  },
  "elided": false
}
```

Field rules:

- `identity` — the token-derived agent, its realm, and the `display-name`
  primary fact if present. Never caller-supplied.
- `primary_facts` — the agent's identity anchors first, in primary order. Omit
  with `--no-facts` / `include_facts:false`.
- `salient_memories` — the top-N salient memories as `{ id, snippet, kind,
  salience, source }`. `snippet` is a bounded excerpt, not full `content`; the
  full record is fetched by id via `memory.read`. Omit with `--no-salient` /
  `include_salient:false`; size with `--salient-limit N` (default 10).
- `index` — a one-line summary of available kinds, tags, and counts so the agent
  knows what else exists and can `recall`/`list` for it.
- `elided` — see the hard cap below.

### Hard Cap and the `elided` Flag

The digest has a hard byte cap (default ~8 KiB / ~200 lines, configurable via
`--max-bytes` / `max_bytes`). **There is no silent truncation.** When the
selected content would exceed the cap, the digest is trimmed to fit *and*
`elided` is set to `true`. An elided digest is a contract signal: it tells the
agent that the self-digest is not the whole self, and that
`witself.memory.recall` / `witself.fact.list` are the way to reach the rest.

```json
{
  "elided": true,
  "elided_hint": "Digest capped at 8192 bytes; 41 memories and 4 facts omitted. Use memory.recall / fact.list."
}
```

This is deliberate: a digest that silently drops half the agent's identity would
poison every downstream decision. Capping is honest; truncation is a bug.

### Salient-Memory Selection

The salient set is the top-N memories by a **blended salience-plus-recency
score**, with pinned kinds (`profile`, `session`) weighted up, excluding
archived/forgotten records. It is **deterministic** and **never calls the
embedding provider** — the digest must hold up when embeddings are degraded. The
exact scoring formula is defined once, canonically, in
[memory-model.md](memory-model.md); this doc only consumes it. `self show` is
that selection rendered into a bounded, session-start shape.

## The Sealed-Plane Carve-Out

Hydration, teaching, and the file bridge are **open-plane only.** Witself's
sealed plane — secrets and TOTP seeds — is **never** surfaced through any of the
mechanisms in this doc. Concretely:

- **Not in the self-digest.** `self show` (and `witself.self.show` / `GET /v1/self`)
  draws only on primary facts and salient memories. No secret value, secret field,
  reference target, or TOTP seed is ever selected into the digest, the `index`
  summary, or the `elided_hint`.
- **Not in `digest emit`.** The outbound file bridge renders the same open-plane
  selection to Markdown. It **never** writes a secret or seed into CLAUDE.md /
  AGENTS.md / GEMINI.md. You do not put secrets in the files the harness
  auto-loads; doing so would leak plaintext into an unencrypted, version-controlled
  surface — the opposite of the sealed plane's contract.
- **Not from `ingest`.** The inbound file bridge composes only the fact-create and
  memory-create paths. It never creates secrets or TOTP enrollments from file
  content. A credential found in a CLAUDE.md/AGENTS.md file is **not** ingested as a
  secret; promoting a credential into the sealed plane is an explicit
  `witself secret create` / `witself totp enroll`, never a side effect of hydration.

This is the hydration-layer face of the cross-cutting sealed-plane invariant:
secrets and TOTP seeds are never embedded, never returned by semantic recall,
never in the self-digest, and never ingested or plaintext-exported. Sealed-plane
values are reachable only through the explicit, audited **reveal** path
(`witself secret reveal` / `witself totp code`). For the full secret data model
and that reveal ceremony, see [secret-model.md](secret-model.md); for the recall /
embeddings half of the same invariant, see
[memory-model.md](memory-model.md#recall-and-embeddings).

## The Teaching Layer

Three reinforcing surfaces, all carrying the same heuristics:
recall-before-act, write-after-learn, fix-don't-contradict, consolidate-when-noisy,
assume-interruption, and listen-before-reply. Redundancy is the point — the agent
should be taught whether it connects over MCP or only ever reads project files.

Collaboration rides this same teaching layer. The `message.listen`/`message.send`
habit — how an agent hears from and replies to other agents, including agents in other realms —
is taught through exactly these three surfaces, not a separate channel. Cross-realm
comms are the same teaching layer carrying the same instinct out across realm
boundaries; the underlying substrate is pinned in
[agent-collaboration.md](agent-collaboration.md). The sealed-plane carve-out still
applies: no secret value or TOTP seed rides a collaboration message by default, the
same way it never enters the self-digest or the file bridge (see
[The Sealed-Plane Carve-Out](#the-sealed-plane-carve-out)).

### 1. MCP Server `instructions` (the canonical standing protocol)

The MCP server returns an `instructions` string on connect (emitted by
`witself mcp serve`). This is the canonical standing protocol. The exact string
is pinned **verbatim** here and in [mcp-tools.md](mcp-tools.md); the two copies
must stay byte-identical. Do not paraphrase it in code:

```text
You have a persistent self/identity store (Witself). At the START of a non-trivial task, call `witself.self.show` to load your primary facts and salient memories, and `witself.memory.recall` before acting on anything you may have learned before. AFTER you learn a durable fact, preference, decision, or reusable context, call `witself.remember`. If a memory is wrong or outdated, `adjust` or `forget` it rather than adding a contradicting one. Assume your context may be cleared at any moment — flush state with `witself.session.end` / `witself.remember` before long operations. To hear from other agents, call `witself.message.listen` each loop; reply with `witself.message.send`. Memory work is not a substitute for doing the task.
```

It is modeled on Anthropic's memory-tool protocol and Letta's block protocol:
short, standing, and behavioral rather than a feature list.

### 2. Trigger-Laden Tool Descriptions

Every memory/fact/self tool description embeds an explicit **when-to-call trigger
list** (the LangMem pattern), so the description does the teaching even when the
server `instructions` are ignored. Signatures stay tiny so the trigger prose
dominates. The canonical per-tool wording lives in
[mcp-tools.md](mcp-tools.md); the triggers, in summary:

- `self.show` / `memory.recall` — "call at task start, and whenever the user
  references the past or something you may have learned before."
- `remember` / `memory.add` / `fact.set` — "call when you 1) learn a durable fact
  about yourself or the user, 2) are asked to remember something, 3) discover
  reusable context, or 4) find an existing memory wrong or outdated."
- `adjust` / `forget` — "update or forget the stale memory instead of adding a
  contradicting one."
- `memory.consolidate` — "call when memory feels noisy or after a large session."

### 3. The Bootstrap Stanza (paste-able file teaching)

`witself bootstrap-instructions [--format agents-md|claude-md|text]` prints a
paste-able block that carries the same heuristics into the file ecosystem, so the
habit installs even for agents taught only through AGENTS.md/CLAUDE.md.
`witself setup --write-agents-md` installs it into the project AGENTS.md. Paste
this verbatim:

```markdown
## Self / Memory (Witself)

You have a persistent self/identity store reached through the `ws` MCP
tools (or the `witself` CLI). Use it:

- **Recall before acting.** At the start of a non-trivial task, call
  `witself.self.show` to load your primary facts and salient memories, then
  `witself.memory.recall <topic>` for anything you may have learned before.
  Resuming work? `witself.session.start` hydrates identity, open goals, and last
  progress in one call.
- **Write after learning.** When you learn a durable fact, preference, decision,
  or reusable bit of context — or are asked to remember something — call
  `witself.remember "<text>"`. It auto-routes: a name→value assertion becomes a
  fact (upsert), anything else becomes a memory.
- **Fix, don't contradict.** If a memory is wrong or outdated, `adjust` or
  `forget` it. Do not add a new memory that contradicts an old one.
- **Assume interruption.** Your context may be cleared at any moment. Before long
  operations, flush state with `witself.remember` or `witself.session.end`.
- **Tidy when noisy.** After a large session, or when memory feels cluttered,
  run `witself.memory.consolidate` (dry-run first).
- **Hear before you reply.** To hear from other agents, call the
  `witself.message.listen` tool each loop; reply with `witself.message.send`. This
  is how you collaborate — in your own realm and, when allowed, with agents in
  other realms (cross-realm sends are realm-qualified, e.g.
  `witself.message.send --to witself://<realm-handle>/agent/<name>`).

Memory work is not a substitute for doing the task.
```

## Digest Emit (the outbound file bridge)

`witself digest emit --format claude-md|agents-md|markdown [--max-bytes N] [-o PATH]`
(MCP `witself.digest.emit`, API `GET /v1/self?format=`) renders the self-digest
as a fragment suitable for the file-load harnesses. It is the same selection as
`self show` (primary facts, then salient memories, then the index), rendered to
Markdown rather than JSON, and respects the same hard cap and `elided`
semantics.

The emitted fragment is wrapped in **provenance HTML comments** so it can be
identified, refreshed, and (on re-`ingest`) round-tripped without duplication:

```markdown
<!-- witself:begin generated=2026-06-26T17:04:00Z agent=agent_atlas realm=realm_acme schema=witself.v0 -->
## Self (Witself)

- display-name: Atlas
- package-manager: pnpm

### Salient memories
- Prefers terse, decision-led writing. <!-- witself:mem_120 kind=profile -->
- Mid-migration to the v0 storage adapter; resume at the pgvector step. <!-- witself:mem_133 kind=session -->

<!-- witself:elided=false -->
<!-- witself:end -->
```

Rules:

- The `witself:begin`/`witself:end` markers delimit Witself-owned content.
  Anything outside them is human-owned and is never touched by a later emit.
- `generated`, `agent`, `realm`, and `schema` are recorded so a reader (human or
  agent) can tell the fragment is machine-generated and how fresh it is.
- Per-record `witself:mem_…` / `witself:fact_…` comments preserve identity so a
  re-`ingest` upserts rather than duplicates.
- `claude-md` and `agents-md` differ only in heading conventions; both produce
  the same delimited, provenance-tagged block. `markdown` is the neutral form.

The point: file-load harnesses get Witself-backed identity for free, and the
generated block is unambiguously distinguishable from hand-authored notes.

The emitted fragment is **open-plane only.** It contains primary facts and
salient memories; it never contains a secret value, secret field, or TOTP seed
(see [The Sealed-Plane Carve-Out](#the-sealed-plane-carve-out)). Sealed-plane
material stays out of CLAUDE.md/AGENTS.md entirely.

## Ingest (the inbound file bridge)

`witself ingest <PATH ...> [--source-label L] [--dry-run] [--json]` parses
existing CLAUDE.md / AGENTS.md / GEMINI.md files into Witself records. There is
**no new resource**: ingest composes the existing fact-create and memory-create
paths (`POST /v1/facts` + `POST /v1/memories`) with dedup. It is CLI-first; an
MCP tool is optional.

Parser rules:

- **kv-shaped lines → facts (upsert).** A line that reads as a clear name→value
  assertion — `display-name: Atlas`, `package-manager = pnpm`, `Email: a@b.com`,
  bulleted `- key: value` — upserts a fact by name. Idempotent: re-ingesting the
  same line updates rather than duplicates.
- **prose paragraphs → memories.** Free-form paragraphs and non-kv bullets become
  memories (verbatim; `infer=false`, matching `remember`). Dedup/supersede on
  write applies (see [memory-model.md](memory-model.md)).
- **never secrets.** Ingest only ever creates facts and memories. A credential in
  a file is **not** promoted into the sealed plane — there is no secret-create or
  TOTP-enroll path through ingest (see
  [The Sealed-Plane Carve-Out](#the-sealed-plane-carve-out)). Move a credential
  into the sealed plane explicitly with `witself secret create` /
  `witself totp enroll`.
- **provenance tag.** Every imported record is tagged
  `source=import:<file>` (overridable with `--source-label`). This keeps
  imported records distinguishable from `self`-authored ones, so the digest and
  `memory.consolidate` can prioritize and never silently overwrite human intent
  (see [requirements.md](requirements.md#memory-self-management-and-hydration)).
- **dedup / upsert.** Facts upsert by name; memories run the standard
  near-duplicate check and return the existing `mem_…` id with a
  `memory_duplicate` warning rather than creating a near-dup. Re-importing an
  unchanged file is a no-op.
- **Witself-generated blocks round-trip.** Content inside `witself:begin`/`end`
  markers (from a prior `digest emit`) is matched back to its
  `witself:mem_…` / `witself:fact_…` ids and upserted in place, never
  re-created.
- `--dry-run` reports created / updated / duplicate / conflicting records without
  persisting. Ingest is audited (`fact.imported`, `memory.imported`).

## How This Maps Onto CLAUDE.md and AGENTS.md Auto-Load

| Concern | CLAUDE.md / AGENTS.md | Witself |
| --- | --- | --- |
| Loaded automatically | Yes, by the harness at session start | No — must be called; `self show` / `session start` are the explicit hydration calls |
| Authority | Static file, edited by hand | Live store, mutated through an audited, versioned lifecycle |
| Recall | Whole file injected every time | Bounded digest + on-demand semantic `recall` |
| Provenance | None (flat text) | First-class `source` on every fact and memory |
| Multi-session state | None | `session start` / `session end` carry open goals and last progress |
| Cross-agent / policy | None | Policy-gated cross-agent access ([access-policy.md](access-policy.md)) |

These are complementary, not competing. The file ecosystem is the
zero-configuration on-ramp; Witself is the durable, auditable, queryable store
behind it. `digest emit` and `ingest` keep the two in sync: emit so a
file-only harness still gets Witself identity, ingest so an existing file project
adopts Witself without rewriting its notes.

## Read-Only Mode

In `witself mcp serve --read-only`, the inbound and mutating verbs are excluded
(`remember`, `session.end`, `memory.consolidate`, `ingest`). The pull and outbound
verbs remain available: `self.show`, `session.start`, `memory.recall`, and
`digest.emit`. Hydration is always safe; teaching the agent to read its self
never depends on write access. See [mcp-tools.md](mcp-tools.md) for the full
read-only matrix.

## Related Docs

- [memory-model.md](memory-model.md) — memory shape, recall, salience scoring,
  dedup/supersede, the `source` provenance field.
- [facts-model.md](facts-model.md) — deterministic name→value facts and upsert.
- [secret-model.md](secret-model.md) — the sealed plane (secrets, TOTP seeds) that
  hydration, `digest emit`, and `ingest` deliberately exclude; the reveal path is
  the only way sealed-plane values are surfaced.
- [mcp-tools.md](mcp-tools.md) — the pinned server `instructions` string,
  trigger-laden tool descriptions, and the read-only matrix.
- [cli-command-surface.md](cli-command-surface.md) — `self show`, `remember`,
  `session start/end`, `memory consolidate`, `digest emit`, `ingest`,
  `bootstrap-instructions`.
- [requirements.md](requirements.md#memory-self-management-and-hydration) — the
  master decision log for self-management and hydration.
