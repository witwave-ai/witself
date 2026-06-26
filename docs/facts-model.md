# Witself Facts Model

Status: pre-implementation draft. Last reviewed 2026-06-26.

A fact is a `name`→`value` identity attribute: the canonical, queryable identity
card for an agent. Facts are the deterministic counterpart to memories. Where a
[memory](memory-model.md) is free-form self-content addressed by `id` and
recalled semantically, a fact is a named, single-valued attribute addressed by
name and resolved exactly. `fact get email` returns the one true value, every
time, with no ranking and no embedding provider involved.

Facts are one of the two first-class identity payload types in Witself. This
document pins their shape, lookup rules, the `primary` flag and its promotion
semantics, sensitivity/redaction posture, and v0 size and count limits. JSON
shapes are normative in [json-contracts.md](json-contracts.md); cross-agent
access is governed by [access-policy.md](access-policy.md).

## Goals

- Give agents a deterministic, name-addressed identity card distinct from
  semantic memory recall.
- Make exactly one value answer each `name` for a given owner (deterministic
  lookup, name-unique per owner).
- Let an agent declare its identity anchors through a `primary` flag, with safe,
  atomic promotion and at-most-one-primary-per-kind uniqueness.
- Keep facts plainly readable identity data by default, redacting only those
  marked `sensitive`, with no secret-style reveal ceremony.
- Bound storage with simple per-fact and per-owner limits enforced with
  deterministic errors.

## Fact Shape

A fact has:

- `id`: a stable identifier with the `fact_` prefix. Identity is by `id`;
  `name` is the addressing key.
- `owner`: the owning agent (`agent_…`) or, for group-scoped facts, the owning
  security group (`grp_…`). See [Security Groups](security-groups.md).
- `realm`: the enclosing realm (`realm_…`).
- `name`: the attribute name. **Unique per owner**, optionally namespaced (for
  example `aws/account-id`). See [Naming and Uniqueness](#naming-and-uniqueness).
- `value`: the fact's value, stored as text. Interpretation is guided by
  `format`.
- `primary`: boolean. Marks the canonical, identity-defining value of its
  logical kind. See [Primary Flag](#primary-flag).
- `sensitive`: boolean. Marks PII redacted by default in `list`/`scan`/`show`
  output. A display/PII flag, not an encryption boundary. See
  [Sensitivity and Redaction](#sensitivity-and-redaction).
- `format`: an optional type hint such as `string`, `email`, `url`, `date`, or
  `number`, used for validation and display. `format` is advisory; unknown
  formats are allowed and treated as `string`.
- `source`: provenance for the fact, such as `self`, an agent name (for a
  cross-agent contribution), a `msg_…` id, or an import batch id.
- `created_at`, `updated_at`: timestamps.
- Versioned edit history (see [Edit History](#edit-history)).

Example fact (illustrative; normative shape lives in
[json-contracts.md](json-contracts.md)):

```json
{
  "id": "fact_7b3c9a1e",
  "owner": "agent_archivist",
  "realm": "realm_acme",
  "name": "email",
  "value": "archivist@acme.example",
  "primary": true,
  "sensitive": false,
  "format": "email",
  "source": "self",
  "created_at": "2026-06-26T00:00:00Z",
  "updated_at": "2026-06-26T00:00:00Z"
}
```

Facts deliberately have **no** `tags[]`, `salience`, `links[]`, or embedding
vector. Those belong to memories, which are filtered and ranked; facts are
resolved by name. A fact may be referenced from a memory's `links[]` via a
`witself://fact/<name>` reference (see [References](#references)), but a fact
does not itself carry links.

## Naming and Uniqueness

- A fact `name` is unique within its owner. One owner cannot hold two facts with
  the same `name`; a second write to the same `name` upserts the existing fact
  (see [Lookup and Lifecycle](#lookup-and-lifecycle)).
- Names may be namespaced with `/`, for example `aws/account-id` or
  `contact/email`. The namespace is part of the name for uniqueness and lookup;
  it is not a separate field.
- Different owners may reuse the same `name`. Ownership disambiguates: agent
  `archivist` and agent `analyst` may each hold their own `email` fact, and a
  group may hold a group-scoped `email` distinct from any member's.
- Name constraints (v0): a name is a non-empty string of lowercase letters,
  digits, `-`, `_`, `.`, and `/`, must not begin or end with `/`, and must not
  contain empty path segments (`//`). Names are matched case-sensitively but
  callers are encouraged to use lowercase. The final path component is the leaf.
- Memory names, by contrast, are **not** unique; memories are addressed by `id`
  and recalled semantically. See [memory-model.md](memory-model.md).

## Lookup and Lifecycle

Lookup is **deterministic by name**. There is no ranking, scoring, or
fuzzy-matching step.

- `fact get NAME` returns the single fact named `NAME` for the caller's
  identity, or `not_found` if no such fact exists. Reading a `sensitive` fact's
  value is an ordinary authorized read (see
  [Sensitivity and Redaction](#sensitivity-and-redaction)).
- `fact set NAME VALUE` creates or updates a fact (upsert by `name` within the
  owner). Optional flags set `--primary`, `--sensitive`, `--format`, and
  `--source`. Setting `NAME` when it already exists updates `value` and the
  named fields and appends to edit history; it never creates a duplicate.
- `fact list` enumerates the caller's facts with metadata and filters (by
  `name` prefix/namespace, `primary`, `sensitive`, `format`, `source`), with
  cursor pagination. `sensitive` values are redacted in list output by default.
- `fact delete NAME` removes a fact. Delete is guarded: it requires confirmation
  unless `--yes`, supports `--dry-run`, and is audited (`fact.deleted`). Deleting
  a `primary` fact does not auto-promote another fact; promotion is always
  explicit.

Cross-agent and group-owned facts follow the same surface with an explicit
owner. Reading another owner's fact requires a `read` policy; contributing,
curating, or deleting another owner's fact requires the matching policy verb and
the cross-agent guardrails (`--reason`, `--dry-run`, confirmation, audit). See
[access-policy.md](access-policy.md).

The CLI noun is `fact` (`set`/`get`/`list`/`delete`, with `--primary`). The MCP
tools are `witself.fact.set/get/list/delete`. The API resource is `/v1/facts`
with the colon action `/v1/facts/{fact_id}:primary` for promotion. Scopes are
`fact:create`, `fact:read`, `fact:update`, `fact:delete`, and `fact:primary`.

## Primary Flag

The `primary` flag marks the canonical, identity-defining value of a fact's
**logical kind**.

- A logical kind is the fact's base name by default. The base name is the leaf
  path component with any trailing numeric or disambiguating suffix removed by
  convention, so `email`, `email-work`, and `contact/email` may be intended as
  the same kind. To keep v0 unambiguous, the logical kind is the **leaf name**
  (`email`) unless an explicit `--kind` is supplied at set time. An explicit
  `--kind` overrides the derived kind and is recorded with the fact.
- At most one fact may be `primary` per logical kind per owner. This is the
  uniqueness invariant the engine enforces.
- Primary facts are the agent's **identity anchors**. They are surfaced first in
  `whoami`, in profile output, and in [identity export](backup-and-recovery.md),
  ahead of non-primary facts.

### Promotion and demotion

- Setting a fact `--primary` is an **atomic promotion**. In one transaction it
  marks the target fact `primary: true` and demotes any prior primary of the
  same logical kind for the same owner to `primary: false`.
- Promotion never deletes or merges the demoted fact; it only flips its flag.
  Both facts continue to exist and remain readable by name.
- Promotion across owners (promoting another agent's or a group's fact) is a
  `curate`-class action: it requires a `curate` policy, an audit `--reason`,
  supports `--dry-run`, and requires confirmation unless `--yes`. See
  [access-policy.md](access-policy.md).
- Promotion is exposed as the colon action `/v1/facts/{fact_id}:primary`, the
  `--primary` flag on `fact set`, and is gated by the `fact:primary` scope.
- Every promotion writes a `fact.primary_changed` audit event recording the
  promoted fact, the demoted fact (if any), the logical kind, and the actor
  derived from the token. See [audit-retention.md](audit-retention.md).

### Worked example

1. `fact set email a@acme.example` → creates `email`, not primary.
2. `fact set email a@acme.example --primary` → `email` becomes primary (no prior
   primary of kind `email`, so nothing is demoted).
3. `fact set email-2 b@acme.example --primary --kind email` → `email-2` becomes
   primary of kind `email`; `email` is atomically demoted to `primary: false`.
   Both facts persist.

## Sensitivity and Redaction

Facts are **ordinary readable identity data**. Witself is an identity store, not
a secret vault, so there is no reveal ceremony, no separate sensitive value-size
budget, and no value-redaction state machine.

- Non-sensitive facts are returned in full in `show`/`get`/`list` by default.
- Facts marked `sensitive` have their `value` redacted in `list`/`scan` output
  by default. An authorized `fact get NAME` of a single sensitive fact returns
  the value — this is an ordinary authorized read, not a reveal.
- `sensitive` is a display/PII flag, not an encryption boundary. Optional
  field-level encryption of `sensitive` fact values is a capability an operator
  may enable, not the default; see [storage.md](storage.md).
- Sensitive fact values must never appear in errors, logs, audit records, or
  metrics. Audit may record the fact `name`, `id`, owner, and decision outcome,
  but never the value. See [audit-retention.md](audit-retention.md).
- Exporting `sensitive` facts is supported (Witself embraces plaintext export)
  but warned-on and requires an audit `--reason`; operators may scope exports to
  non-sensitive facts. See [backup-and-recovery.md](backup-and-recovery.md).

## Edit History

Every `fact set` that changes a stored fact records a new version. History uses
the same shape as memory edit history (see [memory-model.md](memory-model.md)).

- Each version records a monotonically increasing version number, the actor
  (derived from the token, never from input), a timestamp, and the changed
  fields.
- History is retained for audit, conflict detection, safe operator recovery, and
  export, and is included in [identity export](backup-and-recovery.md).
- History entries inherit the fact's `sensitive` posture; redacted in list/scan,
  readable on an authorized single read.
- `primary` flips are recorded both in history and as a `fact.primary_changed`
  audit event.

## Size and Count Limits

V0 defaults, enforced with deterministic `limit_exceeded` (or `usage_error`)
responses and surfaced through backend capabilities where practical:

- Maximum fact `value` size: 64 KiB before storage overhead. (Facts are
  attribute-sized; large free-form content belongs in a
  [memory](memory-model.md).)
- Maximum fact `name` length: 255 characters, including namespace segments.
- Maximum namespace depth: 8 path segments.
- Maximum `source` length: 1 KiB.
- Maximum `format` length: 64 characters.
- Maximum facts per owner (agent or group): 1024.
- Edit-history versions retained per fact: 256 (oldest pruned beyond the cap,
  pruning is audited).

Stored facts are a metered billing dimension; fact reads and writes roll up into
the memory/fact operation meters. See [billing-and-limits.md](billing-and-limits.md).

## References

A fact is referenceable through the `witself://` scheme so memories, messages,
config, scripts, and MCP tools can point at it without copying the value.

- Current agent's fact by name: `witself://fact/<name>`
- A specific agent's fact (cross-agent, policy-gated):
  `witself://agent/<agent>/fact/<name>`
- Group-scoped fact: `witself://group/<group>/fact/<name>`

The final path component is the leaf name; URL-encode if a name contains
reserved characters. Namespaced names keep their `/` separators inside the leaf
position (for example `witself://fact/aws/account-id`).

Reference rules:

- A reference resolves only through authorized commands or MCP tools, enforcing
  the same authorization as a direct `fact get`. A cross-agent or cross-group
  reference resolves only when policy permits.
- References used in a memory's `links[]` are validated at write time and
  re-checked at resolve time; dangling references are reported, not silently
  dropped.
- `witself reference parse` and `witself reference resolve` (CLI and MCP) handle
  references deterministically. Parsing and resolution are tracked in
  [json-contracts.md](json-contracts.md).

## Facts vs Memories

| Aspect        | Fact                          | Memory                              |
| ------------- | ----------------------------- | ----------------------------------- |
| Id prefix     | `fact_`                       | `mem_`                              |
| Addressing    | by `name` (unique per owner)  | by `id`                             |
| Lookup        | deterministic, exact          | semantic recall + filters           |
| Value         | single attribute value        | free-form `content`                 |
| Embedding     | none                          | embedding vector (pgvector)         |
| Anchors       | `primary` flag per kind       | `salience`, `kind`, `tags`          |
| Size budget   | 64 KiB value                  | 256 KiB content                     |

The memory side of this split is documented in
[memory-model.md](memory-model.md).

## Related Docs

- [requirements.md](requirements.md)
- [memory-model.md](memory-model.md)
- [access-policy.md](access-policy.md)
- [security-groups.md](security-groups.md)
- [json-contracts.md](json-contracts.md)
- [cli-command-surface.md](cli-command-surface.md)
- [mcp-tools.md](mcp-tools.md)
- [api-contract.md](api-contract.md)
- [storage.md](storage.md)
- [audit-retention.md](audit-retention.md)
- [backup-and-recovery.md](backup-and-recovery.md)
- [billing-and-limits.md](billing-and-limits.md)
