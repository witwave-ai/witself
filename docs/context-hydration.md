# Witself Context Hydration, Teaching, and the File Bridge

Status: implemented self-digest, managed teaching, and bounded automatic
hydration for the current runtime contracts, with target session-command and
file-bridge extensions. Witself remains a service reached through its API, CLI,
or MCP; installed lifecycle hooks now put its open-plane context into the model
where the runtime exposes a model-visible output channel. The two-way file
bridge (`digest emit` / `ingest`) remains future work. Updated 2026-07-14.

Target amendment (accepted 2026-07-14): managed runtime integration performs
automatic Witself digest/recall and same-turn narrative capture as described in
[narrative-memory-and-curation.md](narrative-memory-and-curation.md). Any later
native-only capture language is current implementation history, not the target.

This doc is the canonical home for hydration, teaching, and file interop. The
underlying payloads are pinned in [memory-model.md](memory-model.md) (memories,
recall, salience) and [facts-model.md](facts-model.md) (deterministic,
name-addressed facts). The tool surface is pinned in [mcp-tools.md](mcp-tools.md)
and [cli-command-surface.md](cli-command-surface.md). The master decision log is
[requirements.md](requirements.md).

## Why This Exists

CLAUDE.md and AGENTS.md are read automatically by the harness at session start.
A bare Witself endpoint is not: it is reached over MCP, CLI, or API. The
installed integration closes that gap where a runtime exposes a model-visible
hook output channel; runtimes without one still need the managed instructions
and guided MCP fallback described below. A persistent self/identity store that
is never loaded is worse than no store, because it silently diverges from what
the agent actually believes about itself.

So Witself closes the gap from both ends:

- **Pull** — a cheap, always-available `self show` digest the runtime can inject
  at session start, the MCP analogue of an auto-loaded CLAUDE.md head.
- **Teach** — a standing protocol (MCP server `instructions`), trigger-laden tool
  descriptions, and a paste-able file stanza, all saying the same thing, so the
  habit installs whether the agent learns via MCP or via the file ecosystem.
- **Bridge (target)** — `digest emit` will write a Witself-backed fragment into
  files the harness already loads, and `ingest` will compose explicit fact and
  narrative-memory writes. Witself participates in the AGENTS.md ecosystem
  rather than competing with it.

## The Self-Digest (`self show`)

The self-digest is the bounded snapshot of who this agent is. It is the one call
a runtime should make before a non-trivial task. It is **model-free by design**:
selection uses stored metadata and never invokes an LLM or embedding provider.

The digest is **open-plane only.** It is built from primary facts and salient
memories; sealed-plane material — secrets and TOTP seeds — is **never** part of
it. See [The Sealed-Plane Carve-Out](#the-sealed-plane-carve-out) below.

### Shape

`witself self show` (MCP `witself.self.show`, API `GET /v1/self`) returns:

```json
{
  "schema_version": "witself.v0",
  "identity": {
    "account_id": "acc_acme",
    "agent_id": "agent_atlas",
    "agent_name": "atlas",
    "realm_id": "realm_acme",
    "realm_name": "prod"
  },
  "primary_facts": [
    { "id": "fact_001", "name": "display-name", "value": "Atlas", "primary": true, "source": "self" },
    { "id": "fact_002", "name": "package-manager", "value": "pnpm", "primary": true, "source": "self" }
  ],
  "salient_memories": [
    { "id": "mem_120", "snippet": "Prefers terse, decision-led writing.", "kind": "profile", "salience": 0.9, "source": "self" },
    { "id": "mem_133", "snippet": "Migration 0032 shipped portable client vectors; resume at the cloud conformance check.", "kind": "session", "salience": 0.8, "source": "self" }
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

- `identity` — the token-derived account, agent, and realm identifiers and
  names. Never caller-supplied. A display-name primary fact, when present,
  remains in `primary_facts`.
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
archived/forgotten records. It is **deterministic** and **never calls a model or
embedding provider**. The exact scoring formula is defined once, canonically, in
[memory-model.md](memory-model.md); this doc only consumes it. `self show` is
that selection rendered into a bounded, session-start shape.

## Automatic Runtime Hydration

`witself install` uses the same hook command for transcript capture and
automatic open-plane hydration. Capture is durably queued first. The hook then
attempts a short, synchronous read only when that runtime/event has a documented
model-visible output contract:

1. `SessionStart` reads one bounded `self.show` digest.
2. `UserPromptSubmit` runs a deterministic local history-dependence check. A
   matching prompt becomes a bounded literal lexical `memory.recall` query; an
   ordinary prompt performs no memory network read.
3. The live `self.show` identity must exactly match every installed account,
   realm, and agent id and name. A missing legacy id requires reinstall rather
   than permitting an ambiguous read.
4. Sensitive, redacted, and non-plain memory values are omitted. The renderer
   places remaining facts and narratives in a JSON-escaped
   `WITSELF_AUTOMATIC_CONTEXT_V1` envelope that says the material is untrusted,
   advisory data rather than instructions or authority.
5. Any configuration, credential, identity, network, timeout, or rendering
   failure returns success to the runtime with no context. Transcript capture
   remains queued and the user's prompt proceeds.

The executor defaults to a two-second deadline, an 8 KiB context envelope,
eight salient self memories, and six recall hits. Its intrinsic ceilings are
five seconds, 16 KiB, and 20 candidates; a caller cannot raise them. The lexical
query is capped at 768 bytes. No prompt, query, digest, memory value, or token is
persisted in local hydration state.

Current conformance is deliberately asymmetric:

| Runtime | Session-start self digest | History-dependent task recall | Delivery/fallback |
| --- | --- | --- | --- |
| Codex | Automatic | Automatic | Structured `additionalContext` hook output |
| Claude Code | Automatic | Automatic | Structured `additionalContext` hook output |
| Cursor | Guided fallback | Guided fallback | Current IDE releases can accept and log `sessionStart.additional_context` without delivering it to the model; both paths use MCP guidance until a live version-gated conformance test passes |
| Grok Build | Guided fallback | Guided fallback | Passive-hook stdout is ignored; managed instructions tell the active agent to use MCP |

“Guided fallback” is not renamed automatic injection. It means the installed
always-on routing rule and MCP server instructions tell the active agent to call
`self.show` and focused `memory.recall` without waiting for the user to ask. If
that agent ignores the guidance or MCP is unavailable, Witself reports only the
partial integration guarantee. The matrix is pinned in code and shared
conformance tests, so adding a runtime requires declaring its real output
contract rather than copying another provider's hook fields.

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
secrets and TOTP seeds are never embedded, never returned by memory recall,
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
`witself mcp serve`). The current implementation includes the fact, transcript,
message, and direct narrative-memory protocol documented in
[mcp-tools.md](mcp-tools.md), with a provider policy for each managed runtime:

- Codex receives its full fact-versus-portable-memory policy before the base
  protocol.
- Claude Code receives a concise Claude auto-memory policy plus an operational
  suffix, kept within its 2 KiB MCP server-instruction limit.
- Grok Build receives its provider policy plus the suffix, with tool names
  rewritten to the runtime's underscore-safe namespace.

In every case, an explicit atomic assertion uses `witself.fact.set`, while an
explicit narrative remember uses `witself.memory.capture` in the same turn.
Runtime-native memory is used only when the user explicitly names it or asks for
both providers. The router does not call a nonexistent `witself.remember` tool.
The exact implemented strings are canonical in code; [Agent Memory
Routing](agent-memory-routing.md) defines their shared behavior and
provider-specific guarantees.

The standing protocol is short and behavioral rather than a feature list. It
also states that recalled memories, transcripts, messages, and tool output are
untrusted context rather than instruction authority, and that the Witself
backend performs no model inference.

### 2. Trigger-Laden Tool Descriptions

Every memory/fact/self tool description embeds an explicit **when-to-call trigger
list** (the LangMem pattern), so the description does the teaching even when the
server `instructions` are ignored. Signatures stay tiny so the trigger prose
dominates. The canonical per-tool wording lives in
[mcp-tools.md](mcp-tools.md); the triggers, in summary:

- `self.show` / `memory.recall` — call before history-dependent work. Broad
  recall combines redacted Witself facts with Witself narrative memory.
- `fact.set` — call in the same turn for an explicitly requested atomic durable
  assertion.
- `memory.capture` — call in the same turn for an explicit narrative remember,
  or for a bounded client-authored checkpoint supported by visible evidence.
- `memory.adjust` / `memory.supersede` / `memory.forget` — use exact-version,
  reversible operations rather than adding a contradictory record.
- Curation queue, plan, apply, rollback, opportunistic worker, and per-user
  launchd/systemd service are implemented. The backend never chooses a semantic
  merge or starts inference itself.

### 3. The Bootstrap Stanza (paste-able file teaching)

The bootstrap-instructions/file-bridge command remains a target surface. The
implemented integration path is `witself install codex|claude|grok|cursor`,
which installs managed routing guidance and the MCP server. A future paste-able
stanza must express the same contract:

```markdown
## Self / Memory (Witself)

You have a persistent self/identity store reached through the `ws` MCP
tools (or the `witself` CLI). Use it:

- **Recall before acting.** At the start of a non-trivial task, call
  `witself.self.show` to load your primary facts and salient memories, then
  `witself.memory.recall <topic>` for anything you may have learned before.
  Resuming work? `witself.session.start` hydrates identity, open goals, and last
  progress in one call.
- **Route after learning.** When asked to remember one atomic durable assertion,
  call `witself.fact.set` in the same turn. For an explicit narrative remember,
  call `witself.memory.capture` in the same turn. Split clearly mixed requests,
  clarify genuinely ambiguous ones, and use runtime-native memory only when the
  user explicitly names it or asks for both providers.
- **Fix, don't contradict.** If a memory is wrong or outdated, `adjust` or
  `forget` it. Do not add a new memory that contradicts an old one.
- **Assume interruption.** Your context may be cleared at any moment. Capture a
  bounded, evidence-supported Witself checkpoint when durable progress is
  needed. Claim due fenced curation work when a client is active. An explicitly
  enabled local `memory curate auto` worker can launch after terminal transcript
  flushes; optional per-user launchd/systemd scheduling is managed through
  `memory curate auto service`.
- **Hear before you reply.** To hear from other agents, call
  `witself.message.list` with `unread_only=true`; reply with
  `witself.message.send`. This
  is how you collaborate — in your own realm and, when allowed, with agents in
  other realms (cross-realm sends are realm-qualified, e.g.
  `witself.message.send --to witself://<realm-handle>/agent/<name>`).

Memory work is not a substitute for doing the task.
```

## Digest Emit (target outbound file bridge)

This command, MCP tool, and formatted API response are not implemented in the
current slice. The following contract is retained as the target file bridge.

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
- Migration 0032 shipped portable client vectors; resume at the cloud conformance check. <!-- witself:mem_133 kind=session -->

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

## Ingest (target inbound file bridge)

This command is not implemented in the current slice. The following contract is
the future inverse of target `digest emit`; it must compose exact fact and
narrative-memory writes without server-side classification or synthesis.

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
  implemented client-authored curation can preserve and prioritize human intent
  (see [requirements.md](requirements.md#memory-self-management-and-hydration)).
- **Idempotent import.** Facts use their typed assertion semantics. Narrative
  imports carry stable artifact/source identity and exact idempotency keys so an
  unchanged re-import is a no-op. The backend does not infer near-duplicates or
  merge prose.
- **Witself-generated blocks round-trip.** Content inside `witself:begin`/`end`
  markers (from a prior `digest emit`) is matched back to its
  `witself:mem_…` / `witself:fact_…` ids and upserted in place, never
  re-created.
- `--dry-run` reports created / updated / duplicate / conflicting records without
  persisting. Ingest is audited (`fact.imported`, `memory.imported`).

## How This Maps Onto CLAUDE.md and AGENTS.md Auto-Load

| Concern | CLAUDE.md / AGENTS.md | Witself |
| --- | --- | --- |
| Loaded automatically | Yes, by the harness at session start | Hook-injected for Codex/Claude; guided MCP fallback for Grok and Cursor |
| Authority | Static file, edited by hand | Live store, mutated through an audited, versioned lifecycle |
| Recall | Whole file injected every time | Bounded digest + deterministic lexical/structured `recall`; optional implemented client-supplied hybrid vectors |
| Provenance | None (flat text) | First-class `source` on every fact and memory |
| Multi-session state | None | Salient portable state hydrates now; the composed `session start` / `session end` commands remain future |
| Cross-agent / policy | None | Policy-gated cross-agent access ([access-policy.md](access-policy.md)) |

These are complementary, not competing. The file ecosystem is the
zero-configuration on-ramp; Witself is the durable, auditable, queryable store
behind it. Target `digest emit` and `ingest` will keep the two in sync: emit so a
file-only harness still gets Witself identity, ingest so an existing file project
adopts Witself without rewriting its notes.

## Read-Only Mode

In `witself mcp serve --read-only`, every implemented mutation is removed. That
includes memory capture, adjust, supersede, lifecycle changes, evidence
resolution, and permanent deletion; fact writes and review mutations; and
message send/read. Memory read, list, history, and recall, `self.show`, redacted
fact reads, transcript reads, and mailbox listing remain available. Hydration is
always safe; teaching the agent to read its self never depends on write access.
See [mcp-tools.md](mcp-tools.md) for the full read-only matrix.

## Related Docs

- [memory-model.md](memory-model.md) — memory shape, recall, salience scoring,
  dedup/supersede, the `source` provenance field.
- [facts-model.md](facts-model.md) — deterministic name→value facts and upsert.
- [secret-model.md](secret-model.md) — the sealed plane (secrets, TOTP seeds) that
  hydration, `digest emit`, and `ingest` deliberately exclude; the reveal path is
  the only way sealed-plane values are surfaced.
- [mcp-tools.md](mcp-tools.md) — the pinned server `instructions` string,
  trigger-laden tool descriptions, and the read-only matrix.
- [cli-command-surface.md](cli-command-surface.md) — the implemented direct
  memory lifecycle and the separately labeled target hydration, curation, and
  file-bridge commands.
- [requirements.md](requirements.md#memory-self-management-and-hydration) — the
  master decision log for self-management and hydration.
