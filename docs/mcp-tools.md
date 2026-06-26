# Witself MCP Tools

Status: draft. This document defines the proposed MCP tool surface before
implementation.

## Goals

- Make Witself usable from MCP-compatible agent runtimes.
- Keep MCP behavior aligned with the CLI and JSON contracts.
- Preserve per-agent authorization, policy evaluation, and audit rules.
- Keep local stdio as the default transport.
- Make cross-agent reads, contributions, curation, and forgetting explicit,
  policy-gated, and auditable.
- Make message send and read explicit, scoped, and auditable, with the sender
  always derived from the token.

Unlike Witpass, Witself has no secret reveal ceremony and no one-time code
generation: facts and memories are ordinary readable identity data, redacted
only when marked `sensitive`. There is therefore no `--no-value-tools` mode and
no reveal-style tool framing. The risk boundary that matters here is the
**integrity and authenticity** of identity data, not value confidentiality.

The JSON shapes used by MCP tool results are defined in
[json-contracts.md](json-contracts.md). The matching CLI surface is defined in
[cli-command-surface.md](cli-command-surface.md). The master requirements live
in [requirements.md](requirements.md).

## Server Instructions (Teaching Layer)

Unlike a harness-loaded `CLAUDE.md`/`AGENTS.md`, Witself is a service the agent
must be **taught** to call. The MCP server therefore returns a canonical
`instructions` string on connect (emitted by `witself mcp serve`). This is the
**primary teaching surface**: it is auto-returned to the client during the MCP
handshake and stands as the always-loaded protocol for every session. It is
reinforced by the trigger-laden tool descriptions below and by the paste-able
bootstrap stanza, but the server `instructions` string is the canonical copy.

The exact string is pinned **verbatim** here and in
[context-hydration.md](context-hydration.md); the two copies must stay
byte-identical, and code must not paraphrase it:

```text
You have a persistent self/identity store (Witself). At the START of a non-trivial task, call `witself.self.show` to load your primary facts and salient memories, and `witself.memory.recall` before acting on anything you may have learned before. AFTER you learn a durable fact, preference, decision, or reusable context, call `witself.remember`. If a memory is wrong or outdated, `adjust` or `forget` it rather than adding a contradicting one. Assume your context may be cleared at any moment — flush state with `witself.session.end` / `witself.remember` before long operations. Memory work is not a substitute for doing the task.
```

It is modeled on Anthropic's memory-tool protocol and Letta's block protocol:
short, standing, and behavioral rather than a feature list. See
[context-hydration.md](context-hydration.md) for the full teaching-layer
treatment and the `bootstrap-instructions` stanza.

## Transport And Session Model

Initial transport:

- `stdio`

Later transport:

- `http`, only post-v0, explicitly enabled, authenticated, scoped, and reviewed
  as a higher-risk network tool surface.

Session identity:

- Agent-token sessions act as the token-bound agent.
- Operator/admin-token sessions may inspect or bind to an agent only when their
  permissions allow it.
- Tool input must not be treated as identity. For example, passing an
  `owner_agent` field cannot impersonate that agent, and a `message.send` call
  cannot set its own `from`.
- The acting principal and the message sender are always derived server-side
  from the authenticated token (see
  [requirements.md](requirements.md#agent-authentication)).
- Ephemeral runtime instances, such as Kubernetes pods, use the token file
  mounted into the process. Restarting the pod preserves the Witself identity as
  long as the mounted token still belongs to the same named agent.
- Disabled agents cannot authenticate through MCP with existing tokens.
- Raw token create, rotate, and revoke operations should stay out of MCP v0
  unless a specific operator/admin use case is deliberately added.

Read-only mode:

- `witself mcp serve --read-only` disables mutating tools.
- In read-only mode, `witself.memory.add/adjust/forget`, `witself.fact.set/
  delete`, `witself.message.send`, `witself.remember`, `witself.session.end`,
  and `witself.memory.consolidate` are unavailable. Read, recall, list,
  get, show, parse, resolve, `policy.test`, `witself.self.show`,
  `witself.session.start`, and `witself.digest.emit` remain available subject to
  policy.
- Cross-agent reads and recalls remain policy-gated even in read-only mode: a
  read tool that targets another agent still requires a matching `read` policy
  (see [access-policy.md](access-policy.md)).

There is no `--no-value-tools` mode. Witself does not split tools by value
sensitivity because there is no reveal operation; `sensitive` facts and memory
content are redacted in list/scan output and returned in clear only on an
authorized single-record read.

## Targeting Model

Tools that operate on memories or facts should support a common target model:

- `owner_kind: "current"` targets the token-bound agent's own identity data.
- `owner_kind: "agent"` targets a specific owning agent through `owner_agent`;
  a policy granting the relevant permission, or operator/admin permission, is
  required.
- `owner_kind: "group"` targets a security group's group-owned identity data
  through `owner_group`; group membership plus the group's policy, or
  operator/admin permission, is required.

Tool input must never use these fields as authentication. The session principal
comes only from the authenticated token. `owner_kind: "agent"` and
`owner_kind: "group"` select a *target*, never an *actor*.

CLI maps this model to default targeting, `--owner-agent`, and `--owner-group`.

Default `owner_kind` is `current`.

Inventory tools (`witself.memory.list`, `witself.fact.list`) may additionally
expose `all_agents` and `include_groups` for operator/admin realm-wide scans.
`all_agents` is mutually exclusive with `owner_kind: "agent"` and
`owner_kind: "group"`. Realm-wide scans redact `sensitive` records by default.

Cross-agent and group-targeted **mutations** (contribute, curate, forget)
carry the same guardrails as the CLI: they require a `reason`, support
`dry_run`, and are fully attributed in audit under the deciding policy (see
[access-policy.md](access-policy.md)).

## Tool Result Contract

Every tool should return the same response envelope used by CLI `--json`:

```json
{
  "schema_version": "witself.v0",
  "ok": true,
  "data": {},
  "warnings": []
}
```

Every **mutation** result must include a deterministic, human-readable `echo`
string in `data` so the model can self-verify and chain on the outcome (the
greppable-return lesson). The `echo` states exactly what changed, for example
`"Remembered fact display-name=Atlas"`, `"Added mem_123 (kind=profile,
salience=0.6)"`, or `"Merged into mem_120 (duplicate)"`. The `echo` is the same
line the CLI prints; it never contains `sensitive` values.

Writes are deduplicated. `witself.memory.add` (and `witself.remember` when it
routes to a memory) check for near-duplicates before creating a record; on a hit
they return the existing `mem_` id and a `memory_duplicate` warning — or
`memory_merged` when the write was folded into the existing memory — in
`warnings[]` instead of silently creating a near-duplicate. `witself.fact.set`
(and `witself.remember` when it routes to a fact) is upsert by name, so it
updates in place rather than warning. The warning codes are defined in
[json-contracts.md](json-contracts.md).

Tool errors use the error envelope from [json-contracts.md](json-contracts.md).
Tool results must never contain memory content, fact values, message bodies or
payloads, embedding vectors, or raw tokens in error or warning fields; that rule
matches the audit and logging posture in
[requirements.md](requirements.md#audit-and-identity-handling).

## Tool Naming

Tool names should use the `witself.` prefix:

- `witself.version`
- `witself.whoami`
- `witself.capabilities`
- `witself.self.show`
- `witself.remember`
- `witself.session.start`
- `witself.session.end`
- `witself.memory.add`
- `witself.memory.adjust`
- `witself.memory.read`
- `witself.memory.recall`
- `witself.memory.list`
- `witself.memory.consolidate`
- `witself.memory.forget`
- `witself.digest.emit`
- `witself.fact.set`
- `witself.fact.get`
- `witself.fact.list`
- `witself.fact.delete`
- `witself.policy.test`
- `witself.group.list`
- `witself.group.show`
- `witself.message.send`
- `witself.message.list`
- `witself.message.read`
- `witself.reference.parse`
- `witself.reference.resolve`

Operator/admin candidate tools:

- `witself.policy.list`
- `witself.policy.show`
- `witself.agent.list`
- `witself.agent.show`
- `witself.audit.list`
- `witself.audit.show`

High-risk admin tools such as realm deletion, broad agent lifecycle operations,
policy mutation, group mutation, and token management should be excluded from
MCP v0 unless there is a specific operator use case. Memory `restore`/`delete`
(hard delete) and identity export/import are CLI-first in v0 and are not exposed
as MCP tools.

## Exposure Matrix

| Tool | Default Agent | Read-Only Mode | Notes |
|---|---:|---:|---|
| `witself.version` | yes | yes | No auth-sensitive data. |
| `witself.whoami` | yes | yes | Shows effective principal, scopes, and primary facts. |
| `witself.capabilities` | yes | yes | Shows backend kind, embedding provider, and supported features. |
| `witself.self.show` | yes | yes | Bounded session-start digest; never calls the embedding provider; sets `elided` when capped. |
| `witself.remember` | yes | no | Quick-add; auto-routes to a fact or memory; requires `memory:create`/`fact:create`. |
| `witself.session.start` | yes | yes | One round-trip hydrate of identity, open goals, and last progress; requires `memory:read`/`fact:read`. |
| `witself.session.end` | yes | no | Persists a `session` progress memory and updates open goals; requires `memory:create`. |
| `witself.memory.add` | yes | no | Requires `memory:create`; `contribute` policy for cross-agent. |
| `witself.memory.adjust` | yes | no | Requires `memory:update`; `curate` policy for cross-agent. |
| `witself.memory.read` | yes | yes | Cross-agent read requires `read` policy. |
| `witself.memory.recall` | yes | yes | Semantic by default; cross-agent recall requires `read` policy. |
| `witself.memory.list` | yes | yes | Redacts `sensitive` content; cross-agent/scan is policy/operator gated. |
| `witself.memory.consolidate` | yes | no | GC verb; `dry_run` defaults true; requires `memory:update` (+`memory:forget` for supersede); audited; respects provenance. |
| `witself.memory.forget` | yes | no | Soft delete; cross-agent requires `forget` policy, `reason`. |
| `witself.digest.emit` | yes | yes | Renders the self-digest as a `CLAUDE.md`/`AGENTS.md` fragment; requires `memory:read`/`fact:read`. |
| `witself.fact.set` | yes | no | Requires `fact:create`/`fact:update`; `--primary` requires `fact:primary`. |
| `witself.fact.get` | yes | yes | Returns the one true value by name; cross-agent requires `read` policy. |
| `witself.fact.list` | yes | yes | Redacts `sensitive` values; cross-agent/scan is policy/operator gated. |
| `witself.fact.delete` | yes | no | Requires `fact:delete`; cross-agent requires `curate`/`forget` policy, `reason`. |
| `witself.policy.test` | yes | yes | Evaluates an access decision; never mutates. |
| `witself.group.list` | yes | yes | Lists groups visible to the session. |
| `witself.group.show` | yes | yes | Shows group metadata and membership where visible. |
| `witself.message.send` | yes | no | Requires `message:send`; `from` is always token-derived. |
| `witself.message.list` | yes | yes | Lists the session's mailbox; requires `message:read`. |
| `witself.message.read` | yes | yes | Reads and acks a message; requires `message:read`. |
| `witself.reference.parse` | yes | yes | Validates reference syntax only. |
| `witself.reference.resolve` | yes | yes | Resolves under the same authz as a direct read. |
| `witself.policy.list` | operator | yes | Operator/admin only. |
| `witself.policy.show` | operator | yes | Operator/admin only. |
| `witself.agent.list` | operator | yes | Operator/admin only. |
| `witself.agent.show` | operator | yes | Operator/admin only. |
| `witself.audit.list` | operator | yes | Operator/admin only. |
| `witself.audit.show` | operator | yes | Operator/admin only. |

Cross-agent and group-targeted access is allowed only when a matching policy
permits it, or when an operator/admin session has realm permission. Absence of a
matching `allow` policy is deny (see [access-policy.md](access-policy.md)).

## Tool Definitions

### `witself.version`

Return CLI/server version and build metadata.

Input:

```json
{}
```

Output data:

```json
{
  "version": "0.1.0",
  "commit": "abc123",
  "date": "2026-06-26T18:00:00Z"
}
```

### `witself.whoami`

Return the current realm, principal, effective scopes, and the agent's primary
facts (identity anchors).

Input:

```json
{
  "show_permissions": true,
  "show_primary_facts": true
}
```

Output data uses the principal shape from
[json-contracts.md](json-contracts.md). Primary facts are surfaced first, in
line with the facts model in
[facts-model.md](facts-model.md).

### `witself.capabilities`

Return backend kind, supported features, unsupported feature reasons, the active
embedding provider/model and vector dimensionality, and visible limits.

Input:

```json
{
  "features": ["semantic_recall", "messaging", "billing"],
  "include_reasons": true,
  "include_limits": true
}
```

Output data uses the capability result from
[json-contracts.md](json-contracts.md). When the embedding provider is
unavailable, the result reports that semantic recall is degraded to
keyword/tag/kind/time ranking (see
[memory-model.md](memory-model.md)).

### `witself.self.show`

Return the bounded, always-loadable self-digest: primary facts first, then the
top-N salient memories (blended salience + recency), then a one-line index of
kinds, tags, and counts. This is the MCP analogue of an auto-loaded `CLAUDE.md`
head. It is cheap and **never** requires the embedding provider, so it works even
when semantic recall is degraded.

**Call this at the start of a non-trivial task and whenever the user references
the past**, then use `witself.memory.recall` to reach anything not in the digest.

Input:

```json
{
  "include_facts": true,
  "include_salient": true,
  "salient_limit": 10,
  "max_bytes": null
}
```

Output data:

```json
{
  "identity": {},
  "primary_facts": [],
  "salient_memories": [
    { "id": "mem_120", "snippet": "", "kind": "profile", "salience": 0.6 }
  ],
  "index": { "kinds": [], "tags": [], "counts": {} },
  "elided": false
}
```

The digest has a hard cap (default ~8 KiB / ~200 lines, configurable). When the
cap is hit the result sets `elided` to `true` and points the caller to
`witself.memory.recall`; it never silently truncates. Salient-memory selection
is defined in [memory-model.md](memory-model.md); the digest shape and cap are
pinned in [context-hydration.md](context-hydration.md).

### `witself.remember`

Quick-add capture path. `remember` **auto-routes**: a clear name→value assertion
(for example `"package manager is pnpm"`, `"display name is Atlas"`) upserts a
fact (idempotent by name); anything else adds a memory (stored verbatim, with
dedup/supersede). It never bypasses validation or limits. This is the tested
primary capture path, not a thin alias — `witself.fact.set` and
`witself.memory.add` remain for explicit control.

**Call this when you** 1) learn a durable fact about yourself or the user,
2) are asked to remember something, 3) discover reusable context, or 4) need to
flush state before a long operation. If something you previously stored is wrong,
prefer `witself.memory.adjust`/`witself.memory.forget` over remembering a
contradicting record.

Input:

```json
{
  "text": "Prefers terse status updates over long reports.",
  "scope": "self",
  "sensitive": false,
  "reason": null
}
```

Output data:

```json
{
  "kind": "memory",
  "id": "mem_123",
  "echo": "Added mem_123 (kind=profile, salience=0.6)",
  "duplicate_of": null
}
```

`kind` reports whether the write routed to a `fact` or a `memory`. When a memory
write hits a near-duplicate, `duplicate_of` names the existing `mem_` id and the
envelope carries a `memory_duplicate`/`memory_merged` warning. `remember` emits
no audit event of its own; it routes to the existing `memory.added` /
`fact.created` / `fact.updated` events.

### `witself.session.start`

Hydrate identity, open goals, and last progress in one round-trip so resuming a
multi-session task is a single call rather than a list-plus-recall crawl. Read
only; available in read-only mode.

**Call this at the start of a session** that continues prior work.

Input:

```json
{}
```

Output data:

```json
{
  "identity": {},
  "open_goals": [],
  "last_progress": null
}
```

Audited as `session.started`. See [context-hydration.md](context-hydration.md).

### `witself.session.end`

Persist a progress memory (kind `session`) and update the open-goal list so the
next session can resume cleanly. Pairs with the assume-interruption /
flush-before-long-operations habit. Requires `memory:create`; unavailable in
read-only mode.

**Call this before a long operation or when wrapping up**, so context survives a
clear.

Input:

```json
{
  "summary": "Wired the digest cap; recall fallback still open.",
  "open_goals": ["finish recall fallback", "document echo contract"]
}
```

Output data:

```json
{
  "saved": true,
  "progress_memory_id": "mem_140",
  "echo": "Saved session progress mem_140 (2 open goals)"
}
```

Audited as `session.ended`.

### `witself.memory.add`

Create a memory owned by the current agent, by a specified agent, or by a
security group when policy or operator/admin permission allows. Cross-agent and
group adds are `contribute` actions and record the contributing agent in
`source`.

**Call this when you** 1) learn a durable fact about yourself or the user,
2) are asked to remember something, 3) discover reusable context worth keeping,
or 4) find an existing memory wrong or outdated (prefer `adjust`/`forget` over
adding a contradicting record). Prefer `witself.remember` for quick capture;
reach for `add` when you need explicit control over `kind`, `tags`, or
`salience`. Writes are deduplicated: a near-duplicate returns the existing
`mem_` id with a `memory_duplicate`/`memory_merged` warning rather than a new
record.

Input:

```json
{
  "content": "Prefers terse status updates over long reports.",
  "kind": "profile",
  "tags": ["preference", "comms"],
  "source": "self",
  "salience": 0.6,
  "links": ["witself://fact/display-name"],
  "sensitive": false,
  "owner_kind": "current",
  "owner_agent": null,
  "owner_group": null,
  "reason": null
}
```

Output data should return memory detail with the new `mem_` id and version,
using the memory detail shape from [json-contracts.md](json-contracts.md).

### `witself.memory.adjust`

Edit a memory's content, kind, tags, source, salience, links, or `sensitive`
marker. Adjust appends a new version to edit history; prior versions are
retained. Cross-agent or group adjust is a `curate` action and requires a
`reason`.

**Call this when** a memory is wrong or outdated: update it in place rather than
adding a contradicting record.

Input:

```json
{
  "id": "mem_123",
  "owner_kind": "current",
  "owner_agent": null,
  "owner_group": null,
  "set_content": null,
  "set_kind": null,
  "add_tags": [],
  "remove_tags": [],
  "set_source": null,
  "set_salience": null,
  "add_links": [],
  "remove_links": [],
  "set_sensitive": null,
  "dry_run": false,
  "reason": null
}
```

Output data uses the mutation result shape from
[json-contracts.md](json-contracts.md), including the new version number.

### `witself.memory.read`

Read one memory deterministically by `id`. Reading updates `last_accessed_at`.
Cross-agent or group reads require a `read` policy and are metered as cross-agent
accesses.

Input:

```json
{
  "id": "mem_123",
  "owner_kind": "current",
  "owner_agent": null,
  "owner_group": null,
  "version": null,
  "include_history": false,
  "show_links": false
}
```

Output data uses the memory detail shape from
[json-contracts.md](json-contracts.md). `sensitive` content is returned in clear
on this authorized single-record read; there is no reveal ceremony.

### `witself.memory.recall`

Semantic-by-default recall over the caller's accessible memories. Performs vector
similarity search blended with keyword, tag, kind, and time filters and hybrid
ranking (similarity, lexical match, tag/kind match, recency, salience). Cross-
agent or group recall requires a `read` policy.

**Call this at the start of a non-trivial task and whenever the user references
the past** — before acting on anything you may have learned before. Pair it with
`witself.self.show`, which loads primary facts and the salient set cheaply;
`recall` is how you reach the rest.

Input:

```json
{
  "query": "how does this agent like to receive updates",
  "owner_kind": "current",
  "owner_agent": null,
  "owner_group": null,
  "kind": null,
  "tag": [],
  "since": null,
  "until": null,
  "limit": 10,
  "min_score": null
}
```

Output data uses the recall result shape from
[json-contracts.md](json-contracts.md): ranked items with scores and a
`degraded` flag when the embedding provider is unavailable and recall fell back
to keyword/tag/kind/time ranking. See
[memory-model.md](memory-model.md).

### `witself.memory.list`

Enumerate memories with metadata and filters, with cursor pagination. `sensitive`
content is redacted by default.

Input:

```json
{
  "owner_kind": "current",
  "owner_agent": null,
  "owner_group": null,
  "all_agents": false,
  "include_groups": false,
  "kind": null,
  "tag": [],
  "source": null,
  "since": null,
  "until": null,
  "include_sensitive": false,
  "limit": 100,
  "cursor": null
}
```

Output data:

```json
{
  "items": [],
  "next_cursor": null
}
```

Each item uses the memory summary shape from
[json-contracts.md](json-contracts.md).

### `witself.memory.consolidate`

The garbage-collection verb. Merges near-duplicate memories, supersedes stale
ones, **surfaces** (does not auto-pick) conflicting facts, and trims the
digest/index. It respects provenance — it never silently overwrites human-,
operator-, or import-authored records, surfacing such conflicts instead. Guarded
and audited (`memory.consolidated`); requires `memory:update` (plus
`memory:forget` for supersede); unavailable in read-only mode.

**Call this when memory feels noisy or after a large session.** `dry_run`
defaults to `true`, so the first call previews the plan without mutating.

Input:

```json
{
  "dry_run": true,
  "scope": null,
  "reason": null
}
```

Output data:

```json
{
  "merged": [],
  "superseded": [],
  "conflicts": [],
  "trimmed_index": {},
  "echo": "Consolidation preview: 3 merge, 1 supersede, 2 conflicts surfaced"
}
```

### `witself.memory.forget`

Soft-delete (tombstone) a memory. Reversible within the retention window. This is
the default destructive path; hard delete and restore are CLI-first in v0.
Cross-agent or group forget requires a `forget` policy and a `reason`.

**Call this when** a memory is no longer true or relevant: forget it rather than
leaving a contradicting record in place.

Input:

```json
{
  "id": "mem_123",
  "owner_kind": "current",
  "owner_agent": null,
  "owner_group": null,
  "dry_run": false,
  "reason": null
}
```

Output data uses the mutation result shape from
[json-contracts.md](json-contracts.md), including the tombstone state and the
restore window.

### `witself.digest.emit`

Render the self-digest as a `CLAUDE.md`/`AGENTS.md`/`markdown` fragment carrying
provenance HTML comments (witself-generated plus timestamp), so file-load
harnesses get Witself-backed identity for free. Read only (it renders from the
same source as `witself.self.show`); available in read-only mode. Requires
`memory:read`/`fact:read`. The companion `ingest` direction is CLI-first in v0;
this MCP tool covers the outbound (emit) direction only.

**Call this when** you need to seed or refresh a project's `CLAUDE.md`/`AGENTS.md`
from Witself identity.

Input:

```json
{
  "format": "claude-md",
  "max_bytes": null
}
```

Output data:

```json
{
  "content": ""
}
```

`format` is one of `claude-md|agents-md|markdown`. Emit format, provenance
comments, and the ingest parser rules are pinned in
[context-hydration.md](context-hydration.md). Optionally audited as
`self.digest.emitted`.

### `witself.fact.set`

Create or update a fact by name (upsert within the owner). Setting `primary`
atomically demotes any prior primary of the same logical kind. Cross-agent or
group set is a `contribute`/`curate` action and requires a `reason`.

**Call this when you** learn or are asked to record a stable name→value
assertion about yourself or the user (a preference, identifier, or configuration
like `package-manager=pnpm`). Because it is upsert by name, setting a fact that
already exists updates it in place — no contradicting duplicate. Prefer
`witself.remember` for quick capture; it auto-routes a clear name→value
assertion to `fact.set`.

Input:

```json
{
  "name": "display-name",
  "value": "Atlas",
  "primary": false,
  "sensitive": false,
  "format": "string",
  "source": "self",
  "owner_kind": "current",
  "owner_agent": null,
  "owner_group": null,
  "dry_run": false,
  "reason": null
}
```

Output data uses the fact detail shape from
[json-contracts.md](json-contracts.md). Setting `primary` requires the
`fact:primary` scope.

### `witself.fact.get`

Deterministic lookup of one fact by name. Returns the one true value for the
target identity. Cross-agent or group get requires a `read` policy.

Input:

```json
{
  "name": "display-name",
  "owner_kind": "current",
  "owner_agent": null,
  "owner_group": null,
  "include_history": false
}
```

Output data uses the fact detail shape from
[json-contracts.md](json-contracts.md). A `sensitive` fact value is returned in
clear on this authorized read; redaction applies only to list/scan output.

### `witself.fact.list`

Enumerate facts with filters and cursor pagination. `sensitive` values are
redacted by default. Primary facts are surfaced first.

Input:

```json
{
  "owner_kind": "current",
  "owner_agent": null,
  "owner_group": null,
  "all_agents": false,
  "include_groups": false,
  "name_prefix": null,
  "primary_only": false,
  "include_sensitive": false,
  "limit": 100,
  "cursor": null
}
```

Output data:

```json
{
  "items": [],
  "next_cursor": null
}
```

Each item uses the fact summary shape from
[json-contracts.md](json-contracts.md).

### `witself.fact.delete`

Delete a fact by name. Guarded and audited. Cross-agent or group delete requires
a `curate`/`forget` policy and a `reason`.

Input:

```json
{
  "name": "display-name",
  "owner_kind": "current",
  "owner_agent": null,
  "owner_group": null,
  "dry_run": false,
  "reason": null
}
```

Output data uses the mutation result shape from
[json-contracts.md](json-contracts.md).

### `witself.policy.test`

Evaluate whether a given subject/permission/target/scope would be allowed under
current policy. This is the canonical dry-run for access decisions. It never
mutates and never resolves the targeted data.

Input:

```json
{
  "subject_kind": "agent",
  "subject": "agent-amy",
  "permission": "read",
  "target_kind": "agent",
  "target": "agent-archivist",
  "scope": ["memory"],
  "filter": {
    "memory_kind": ["profile"],
    "tag": null,
    "fact_name": null,
    "include_sensitive": null
  }
}
```

Output data uses the Policy Test Result shape from
[json-contracts.md](json-contracts.md), echoing `subject`, `permission`,
`target`, and `scope` alongside the decision:

```json
{
  "subject": { "kind": "agent", "id": "agent_123", "name": "agent-amy" },
  "permission": "read",
  "target": { "kind": "agent", "id": "agent_456", "name": "agent-archivist" },
  "scope": ["memory"],
  "decision": "deny",
  "policy_id": null,
  "reason": "no matching allow policy"
}
```

`scope` is an array of singular tokens (`["memory"]`, `["fact"]`, or
`["memory","fact"]`); the CLI `--scope memory|fact|both` flag maps to this array
form.

When allowed, `decision` is `allow` and `policy_id` names the deciding `pol_`
policy. See [access-policy.md](access-policy.md).

### `witself.group.list`

List security groups visible to the session.

Input:

```json
{
  "member_agent": null,
  "limit": 100,
  "cursor": null
}
```

Output data:

```json
{
  "items": [],
  "next_cursor": null
}
```

Each item uses the group summary shape from
[json-contracts.md](json-contracts.md).

### `witself.group.show`

Show a security group's metadata and membership where visible.

Input:

```json
{
  "group": "analysts",
  "show_members": true,
  "show_policies": false
}
```

Output data uses the group detail shape from
[json-contracts.md](json-contracts.md). See
[security-groups.md](security-groups.md).

### `witself.message.send`

Send a durable message to another agent or to a security group. The `from`
sender is always derived from the authenticated token; it cannot be supplied as
input, so sender forgery is structurally impossible.

Input:

```json
{
  "to_kind": "agent",
  "to": "agent-archivist",
  "subject": "handoff",
  "kind": "note",
  "body": "Picking up the indexing task from here.",
  "payload": null,
  "thread_id": null,
  "reason": null
}
```

Output data uses the message detail shape from
[json-contracts.md](json-contracts.md), including the new `msg_` id and delivery
state. A message granting no policy cannot itself authorize a cross-agent write;
writes still require policy. See
[inter-agent-messaging.md](inter-agent-messaging.md).

### `witself.message.list`

List messages in the session's mailbox with filters and cursor pagination.

Input:

```json
{
  "direction": "inbox",
  "thread_id": null,
  "unread_only": false,
  "from_agent": null,
  "since": null,
  "until": null,
  "limit": 100,
  "cursor": null
}
```

Output data:

```json
{
  "items": [],
  "next_cursor": null
}
```

`direction` is one of the `inbox`|`outbox` set defined in
[json-contracts.md](json-contracts.md) (the CLI selector maps to it). Each item
uses the message summary shape from
[json-contracts.md](json-contracts.md). Message bodies and payloads are
untrusted input to the receiving agent and must be treated as such by the
runtime.

### `witself.message.read`

Read one message by `id` and record read/ack state. Reading is audited.

Input:

```json
{
  "id": "msg_123",
  "ack": true
}
```

Output data uses the message detail shape from
[json-contracts.md](json-contracts.md). The body and payload are returned to the
caller; the receiving runtime must treat them as untrusted input, especially
when a message would drive a memory or fact write.

### `witself.reference.parse`

Validate a Witself reference without resolving the target.

Input:

```json
{
  "ref": "witself://agent/agent-archivist/fact/email"
}
```

Output data:

```json
{
  "reference": "witself://agent/agent-archivist/fact/email",
  "scheme": "witself",
  "kind": "fact",
  "owner_kind": "agent",
  "owner": "agent-archivist",
  "leaf": "email",
  "valid": true
}
```

Reference forms are pinned in
[requirements.md](requirements.md#identity-references).

### `witself.reference.resolve`

Resolve a Witself reference to the targeted memory or fact. Resolution enforces
the same authorization as a direct read; a cross-agent or cross-group reference
resolves only when policy permits.

Input:

```json
{
  "ref": "witself://agent/agent-archivist/fact/email",
  "reason": null
}
```

Output data uses the fact detail or memory detail shape from
[json-contracts.md](json-contracts.md), depending on the reference kind.
`sensitive` values follow the same redaction posture as a direct read: returned
in clear on an authorized single-record resolve, with no reveal ceremony.

## Operator/Admin Candidate Tools

Operator/admin tools should be added carefully after the agent-facing v0 tools
are stable.

Initial candidates:

- `witself.policy.list`
- `witself.policy.show`
- `witself.agent.list`
- `witself.agent.show`
- `witself.audit.list`
- `witself.audit.show`

Excluded from MCP v0 by default:

- Realm deletion.
- Agent create/delete/copy/rename and broad agent lifecycle.
- Token create/rotate/revoke.
- Policy create/delete and group create/delete/member mutation.
- Memory hard delete and restore.
- Identity export and import.
- Customer account administration.
- Billing, subscription, invoice, payment-method, and crypto payment
  management.
- Support ticket management.
- Private Witself admin operations for staff or internal AI agents.

Customer/operator operations remain available through the public CLI where they
are defined (see
[cli-command-surface.md](cli-command-surface.md)). Private Witself admin
operations remain a separate post-v0 surface tracked in
[post-v0-roadmap.md](post-v0-roadmap.md).
