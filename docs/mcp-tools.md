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
  delete`, and `witself.message.send` are unavailable. Read, recall, list,
  get, show, parse, resolve, and `policy.test` remain available subject to
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
- `witself.memory.add`
- `witself.memory.adjust`
- `witself.memory.read`
- `witself.memory.recall`
- `witself.memory.list`
- `witself.memory.forget`
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
| `witself.memory.add` | yes | no | Requires `memory:create`; `contribute` policy for cross-agent. |
| `witself.memory.adjust` | yes | no | Requires `memory:update`; `curate` policy for cross-agent. |
| `witself.memory.read` | yes | yes | Cross-agent read requires `read` policy. |
| `witself.memory.recall` | yes | yes | Semantic by default; cross-agent recall requires `read` policy. |
| `witself.memory.list` | yes | yes | Redacts `sensitive` content; cross-agent/scan is policy/operator gated. |
| `witself.memory.forget` | yes | no | Soft delete; cross-agent requires `forget` policy, `reason`. |
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

### `witself.memory.add`

Create a memory owned by the current agent, by a specified agent, or by a
security group when policy or operator/admin permission allows. Cross-agent and
group adds are `contribute` actions and record the contributing agent in
`source`.

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

### `witself.memory.forget`

Soft-delete (tombstone) a memory. Reversible within the retention window. This is
the default destructive path; hard delete and restore are CLI-first in v0.
Cross-agent or group forget requires a `forget` policy and a `reason`.

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

### `witself.fact.set`

Create or update a fact by name (upsert within the owner). Setting `primary`
atomically demotes any prior primary of the same logical kind. Cross-agent or
group set is a `contribute`/`curate` action and requires a `reason`.

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
