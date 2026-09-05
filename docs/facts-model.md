# Witself Facts Model

Status: core service implemented; advanced policy deferred. Reconciled with the
CLI, fact routes, and store on 2026-09-04. The access-policy rock retains the open
gate `advanced-fact-policy`; see [Resolve advanced fact policy](#resolve-advanced-fact-policy).

A current fact is one resolved assertion at a stable subject/predicate address
inside the authenticated agent's account and realm. Exact lookup returns that
assertion; a write appends assertion history and changes the resolved pointer.
This replaces the former owner/name-only draft. The implementation is in
[the fact store](../internal/store/fact.go) and
[the public fact types and handlers](../internal/server/fact.go).

The MCP contract uses `witself.fact.set` for an explicitly requested atomic
assertion and `witself.fact.propose` for an observation awaiting review.
Narrative context uses `witself.memory.capture`. Credentials do not belong in
facts, and private personal values belong in sensitive facts, never subject
metadata. These are client routing requirements in
[the MCP tool descriptions](../cmd/witself/mcp.go) and
[the narrative capture description](../cmd/witself/mcp_memory.go), not a backend
inference or authority-ranking engine.

## Fact Shape

The implemented public `Fact` representation contains:

- `id`, `subject_id`, `subject`, and `predicate` for identity and addressing.
- `cardinality` and `sensitive` on the fact.
- `resolved_assertion_id`, `value_type`, and a typed JSON `value` from the
  current assertion.
- Provenance and temporal fields: `source_kind`, optional `source_ref`,
  `confidence`, `observed_at`, optional `confirmed_at`, `valid_from`,
  `valid_until`, and `recurrence`.
- `created_at`, `updated_at`, `usage_count`, and optional `last_used_at`.

Account, realm, and owner-agent scope come from the principal and store query;
there is no caller-selectable `owner` field in the fact request. The public
shape has no fact `name`, `format`, `primary`, tags, links, or embedding field.
See [Fact and SetFactRequest](../internal/server/fact.go) and
[the scoped lookup](../internal/store/fact.go).

For example, this is an implemented `POST /v1/facts` request body:

```json
{
  "subject": "self",
  "predicate": "preferences/editor",
  "value_type": "string",
  "value": "vim",
  "cardinality": "one",
  "sensitive": false
}
```

## Naming and Uniqueness

A live address is unique by owner agent, subject, and predicate. Repeated writes
to that address preserve its fact id and append a new assertion. Permanent
deletion and explicit recreation are the exception described below.
[Store evidence](../internal/store/fact.go).

Subjects have a canonical key, display name, and aliases. The default subject is
`self`; `me`, `myself`, and `user` normalize to it. The CLI exposes
`witself fact subject set|list|alias` for stable subject management, and aliases
resolve to the canonical subject instead of creating another fact collection.
[Subject wire shape](../internal/server/fact_subject.go),
[normalization](../internal/store/fact.go), and
[CLI dispatch](../cmd/witself/main.go).

Predicates are case-sensitive lowercase identifiers, optionally namespaced with
`/`, for example `preferences/editor`. They start with a letter, use lowercase
letters, digits, `_`, `-`, and `.`, allow at most eight non-empty path segments,
and occupy at most 255 bytes. This is syntax validation, not a registry of
approved predicate meanings. [Predicate validation](../internal/store/fact.go).

`value_type` has built-in validation for `string`, `number`, `boolean`, `list`,
`object`, `json`, `date`, `datetime`, `url`, `email`, `address`, and `location`.
An unknown syntactically valid type keeps its JSON value; it is not coerced to a
string. Omitted types are inferred from JSON shape. A `cardinality` of `one`,
`many`, or `one_at_a_time` is accepted, but current writes still resolve one
assertion at the address; there is no automatic multi-value merge or
validity-based assertion selection.
[Value validation](../internal/store/fact_value_type.go) and
[normalization and resolution](../internal/store/fact.go).

## Lookup and Lifecycle

The shipped CLI dispatches
`witself fact status|set|get|list|history|delete|propose|review|candidate|confirm|reject|upcoming|subject`.
Flags precede positional arguments, for example:

```sh
witself fact set --subject self preferences/editor vim
witself fact get --subject self preferences/editor
witself fact list --subject self --limit 100
```

`fact set` takes a string value unless `--json-value` is supplied. It supports
`--type`, `--cardinality`, `--sensitive`, provenance/validity flags, recurrence,
and retry keys; it does not expose the draft `--primary`, `--kind`, `--format`,
or `--source` flags. [CLI dispatch and flags](../cmd/witself/main.go).

The implemented HTTP surfaces are:

| Operation | Route |
| --- | --- |
| Value-free capacity | `GET /v1/facts:status` |
| Set | `POST /v1/facts` |
| Exact get | `GET /v1/facts?subject=SUBJECT&predicate=PREDICATE` |
| Bounded list | `GET /v1/facts` with optional `subject`, `predicate_prefix`, `limit`, `sort=usage`, `unused`, and `include_sensitive` |
| Assertion history | `GET /v1/facts/{fact}/history` |
| Deletion preview/apply | `DELETE /v1/facts` or `DELETE /v1/facts/{fact}` |
| Propose/list candidates | `POST /v1/fact-candidates`, `GET /v1/fact-candidates` |
| Exact candidate detail | `GET /v1/fact-candidates/{candidate}` |
| Confirm/reject candidate | `POST /v1/fact-candidates/{candidate}:confirm` or `:reject` |
| Temporal projection | `GET /v1/fact-occurrences` |
| Subject set/list/alias | `PUT /v1/fact-subjects/{subject}`, `GET /v1/fact-subjects`, `POST /v1/fact-subjects/{subject}/aliases` |

[Route registration](../internal/server/server.go) and
[query/action handling](../internal/server/fact.go) define these implemented
routes. Fact lists return `{schema_version,facts}` with a bounded `limit`, not
the generic `items`/`next_cursor` pagination target elsewhere in
[api-contract.md](api-contract.md).

Permanent deletion uses a value-free preview, then the preview's resolved
assertion id and candidate-set revision plus `Idempotency-Key` for apply. It
removes values, assertion/evidence history, and candidates at the address;
a non-restorable value-free tombstone and immutable usage events remain.
The CLI requires `--yes` to apply; `--dry-run` only previews. An ordinary set
cannot resurrect a deleted fact. Explicit `--recreate-deleted` creates a new
fact id with a retry key. The MCP deletion/recreation tools additionally fence
client authorization to the current user's direct request.
[Deletion wire contract](api-contract.md#dry-runs),
[CLI deletion and recreation](../cmd/witself/main.go),
[store recreation](../internal/store/fact.go), and
[MCP boundaries](../cmd/witself/mcp.go).

## Sensitivity and Redaction

Facts store JSON values in the open plane. `sensitive` controls response
redaction; it does not turn a fact into a sealed secret. Broad fact lists redact
both `value` (JSON `null`) and `source_ref` unless `include_sensitive` is
explicitly selected. An authorized exact fact get returns its value, and
assertion history is an authorized detail read. Broad candidate review always
redacts sensitive values; one exact candidate can be read for review.
[Fact storage and detail reads](../internal/store/fact.go),
[list redaction](../internal/store/fact_usage.go),
[candidate reads](../internal/store/fact_candidate.go), and
[open-plane contract](api-contract.md#pagination-and-filtering).

Sensitivity is sticky on an existing address: set and candidate confirmation
combine the existing and incoming flags with logical OR. Recreation also
inherits a sensitive tombstone's flag.
[Set/recreation](../internal/store/fact.go) and
[candidate confirmation](../internal/store/fact_candidate.go).

## Edit History

Set appends an assertion identified by `fas_…`, links the prior assertion with
`supersedes_id`, and atomically changes `resolved_assertion_id`. History follows
that chain newest first and includes value, provenance, confidence, observation,
confirmation, and validity fields. It is assertion history, not numbered
field-diff versions or primary-flag change events.
[Assertion writes and history](../internal/store/fact.go).

## Size and Count Limits

The implemented limits are 65,536 bytes for the input and normalized JSON value,
255 bytes for a predicate, eight predicate segments, 64 characters for a type
identifier, and 1,024 bytes for `source_ref`. Confidence must be between zero
and one. Fact lists default to 100 results and accept limits from 1 through 500.
[Input validation](../internal/store/fact.go) and
[list bounds](../internal/store/fact_usage.go).

The account's applied `stored_fact` limit governs each agent's resolved,
non-deleted current facts across subjects. Assertions, candidates, aliases,
history, and tombstones do not consume additional fact slots. An update to an
existing current address is allowed at the cap; a growing set, recreation, or
candidate confirmation is refused with `stored_fact_limit_reached` when it
would exceed the limit. Capacity exposes `used`, nullable `max` and `remaining`,
`unlimited`, `near_limit`, `at_limit`, and `over_limit`; the finite warning starts
at 90 percent. [Capacity wire contract](api-contract.md#action-and-colon-routes),
[limit enforcement](../internal/store/fact_limit.go),
[set accounting](../internal/store/fact.go), and
[candidate accounting](../internal/store/fact_candidate.go).

## Resolve advanced fact policy

The following boundaries are reconciled; the unimplemented policies remain
**explicitly deferred to the [access-policy rock](access-policy.md)** under
**`advanced-fact-policy`**. This gate remains open and does not define a new
permission or grant access.

| Topic | Implemented today | Deferred target under `advanced-fact-policy` |
| --- | --- | --- |
| Conflict authority | Direct set resolves the new assertion. Proposal stores a separate `pending` or `conflict` candidate; explicit confirmation refuses if the resolved assertion changed since proposal. HTTP set rejects caller-claimed non-agent `source_kind`. | An authority hierarchy across self, operator, imports, inference, or competing agents; automatic conflict arbitration. Source and confidence metadata do not implement such a hierarchy. |
| Predicate registries | Predicate syntax and built-in value-type validation; callers may use custom predicate names and valid custom type identifiers. | A governed registry assigning predicate meanings, ownership, cardinality rules, or authority. The built-in type table is not that registry. |
| Reminders | `fact upcoming` / `witself.fact.upcoming` project resolved dates and datetimes in a bounded window. Annual recurrence must be explicit and only applies to dates; February 29 is skipped in non-leap years. Sensitive occurrences are omitted unless requested. | Reminder scheduling, notification delivery, or waking an agent. The temporal projection does not provide those workflows. |
| Cross-agent facts | Fact, subject, candidate, and occurrence operations require an agent principal and are bound to its account, realm, and owner-agent id. A subject describing another agent is still owned by the caller. | Reading, contributing to, curating, or deleting another agent's collection; group-owned facts and the draft per-verb access rules. No `owner` argument or policy grant enables those operations today. |

Evidence: [direct set and scoped reads](../internal/store/fact.go),
[candidate conflict fencing](../internal/store/fact_candidate.go),
[agent-only HTTP handlers](../internal/server/fact.go),
[subject handlers](../internal/server/fact_subject.go),
[value types](../internal/store/fact_value_type.go),
[temporal projection](../internal/store/fact_temporal.go), and
[MCP fact descriptions](../cmd/witself/mcp.go).

## Primary Flag

The former per-kind `primary` flag, atomic promotion/demotion rules,
`fact:primary` scope, and `/v1/facts/{fact_id}:primary` action are **deferred
targets**, not current fact CRUD. The fact request/response and CLI set flags
have no such field, and the server registers no promotion route. The self-digest
still calls its projection `primary_facts` and marks projected entries
`primary: true`; that response label is not a stored promotion contract.
[Fact shape](../internal/server/fact.go), [CLI](../cmd/witself/main.go), and
[routes and self-digest](../internal/server/server.go).

The earlier group/cross-agent `witself://fact/...` reference-resolution promises
and automatic file-ingest workflow are also removed from the implemented model.
They do not appear in the shipped fact dispatch or routes. Any future policy for
these targets belongs to the access-policy rock and `advanced-fact-policy`.

## Related Docs

- [fact-service.md](fact-service.md)
- [access-policy.md](access-policy.md)
- [security-groups.md](security-groups.md)
- [api-contract.md](api-contract.md)
- [agent-memory-routing.md](agent-memory-routing.md)
- [billing-and-limits.md](billing-and-limits.md)
