# Witself Facts Model

Status: core service implemented; advanced fact policy documented. Reconciled with
the CLI, fact routes, and store on 2026-09-11. Cross-agent and group fact access
remain deferred to the [access-policy rock](access-policy.md).

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
and occupy at most 255 bytes. The server validates this syntax and declared
cardinality only; there is no server-side predicate registry service. See
[Predicate registry](#predicate-registry) and
[`validFactPredicate`](../internal/store/fact.go).

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

## Conflict authority

A canonical Witself fact outranks any narrative memory or transcript. Client
routing treats recalled memories and transcripts as advisory input, never as
authority over a stored fact address.
[Agent memory routing](agent-memory-routing.md) and
[MCP fact descriptions](../cmd/witself/mcp.go).

Within facts, the newest confirmed assertion at a subject/predicate address is
authoritative. Direct `witself.fact.set` and `POST /v1/facts` append an
assertion and move `resolved_assertion_id` atomically.
[`SetFact`](../internal/store/fact.go). Unconfirmed candidates never override a
confirmed value; they remain `pending` or `conflict` until explicit review.
[`ProposeFact`](../internal/store/fact_candidate.go). Confirmation refuses when
the resolved assertion changed since proposal, returning
[`ErrFactConflict`](../internal/store/fact.go).
[`ConfirmFactCandidate`](../internal/store/fact_candidate.go). A rejected
candidate is retained as history with status `rejected` and cannot become
authoritative without a new confirmation.
[`RejectFactCandidate`](../internal/store/fact_candidate.go). A correction
("X instead of Y") is a new confirmed assertion via set or confirm, never a
delete. HTTP set rejects caller-claimed non-agent `source_kind`.
[`setFactHandler`](../internal/server/fact.go).

Cross-agent or operator precedence hierarchies are not implemented. Those belong
to the access-policy rock.

## Predicate registry

There is no server-side predicate registry service. The store validates predicate
shape through [`validFactPredicate`](../internal/store/fact.go), accepts
`cardinality` values [`one`](../internal/store/fact.go),
[`many`](../internal/store/fact.go), and
[`one_at_a_time`](../internal/store/fact.go), and validates built-in
`value_type` identifiers through
[`builtInFactValueTypes`](../internal/store/fact_value_type.go). Custom logical
types remain caller-declared JSON with common size validation only.

The table below documents predicates and namespaces referenced by the shipped
client contracts today. It is documentation, not an enforced server registry.
Callers may use any syntactically valid predicate name. The store persists only
the caller-supplied `sensitive` flag on writes and redacts broad lists from that
stored flag; it does not apply predicate defaults.
[`SetFact`](../internal/store/fact.go), [list redaction](../internal/store/fact_usage.go).

| Predicate | Value type | Cardinality | Client routing convention | Referenced in |
| --- | --- | --- | --- | --- |
| `identity/name` | `string` | `one` | mark `sensitive: true` | [MCP routing instructions](../cmd/witself/cursor_instructions.go), [MCP tool contract](mcp-tools.md), [`witself.fact.delete` example](mcp-tools.md) |
| `preferences/editor` | `string` | `one` | `sensitive: false` | [MCP tool contract](mcp-tools.md), [`witself.fact.set` schema](../cmd/witself/mcp.go) |
| `identity/birth-date` | `date` | `one` | `sensitive: false` | [fact service examples](fact-service.md) |
| `resources/repository` | `string` or `url` | `one` | `sensitive: false` | [fact service examples](fact-service.md) |

Built-in `value_type` identifiers accepted by the store:

| Value type | JSON shape | Notes |
| --- | --- | --- |
| `string` | string | |
| `number` | number | |
| `boolean` | boolean | |
| `list` | array | |
| `object` | object | |
| `json` | any JSON value | |
| `date` | `YYYY-MM-DD` string | pairs with [`FactRecurrenceAnnual`](../internal/store/fact.go) only |
| `datetime` | RFC 3339 string, normalized to UTC | |
| `url` | absolute `http`/`https` URL string | |
| `email` | bare email address string | |
| `address` | non-empty string or non-empty object | |
| `location` | non-empty string or non-empty object | |

Omitted `value_type` is inferred from JSON shape during normalization.
[`normalizeSetFactInput`](../internal/store/fact.go). A governed cross-agent
predicate registry remains out of scope until the access-policy rock defines one.

## Reminders

Dated facts may carry `valid_from`, `valid_until`, and explicit
`recurrence: "annual"` on `value_type: "date"` assertions and candidates.
Recurrence is never inferred from a predicate or value.
[`normalizeSetFactInput`](../internal/store/fact.go),
[`UpcomingFacts`](../internal/store/fact_temporal.go).

`witself fact upcoming`, `witself.fact.upcoming`, and `GET /v1/fact-occurrences`
project resolved date and datetime facts in a bounded window through
[`UpcomingFacts`](../internal/store/fact_temporal.go) and
[`upcomingFactsHandler`](../internal/server/fact.go). February 29 annual
occurrences are skipped in non-leap years. Sensitive temporal facts are omitted
unless explicitly requested because an occurrence timestamp is itself the value.

Reminders are a read-side query, not a delivery feature. There is no scheduler,
notification worker, or agent-waking workflow in the fact service. Delivery
remains out of scope until the messaging lane owns it.

## Cross-agent fact access

Facts are owner-agent scoped today. Every fact, subject, candidate, history, and
occurrence operation requires an agent principal and is bound to the
authenticated agent's account, realm, and owner-agent id. HTTP handlers reject
operator principals with `403` and the message that only an agent token may use
the surface: [`setFactHandler`](../internal/server/fact.go),
[`factsReadHandler`](../internal/server/fact.go),
[`deleteFactHandler`](../internal/server/fact.go),
[`proposeFactHandler`](../internal/server/fact.go),
[`factCandidateActionHandler`](../internal/server/fact.go),
[`upcomingFactsHandler`](../internal/server/fact.go), and the subject handlers
[`upsertFactSubjectHandler`](../internal/server/fact_subject.go),
[`listFactSubjectsHandler`](../internal/server/fact_subject.go),
[`addFactSubjectAliasHandler`](../internal/server/fact_subject.go). Store reads
and writes scope queries to `account_id`, `realm_id`, and `owner_agent_id`.
[`getFactTx`](../internal/store/fact.go),
[`ProposeFact`](../internal/store/fact_candidate.go),
[`UpcomingFacts`](../internal/store/fact_temporal.go).

A subject describing another person, place, or project is still owned by the
caller's agent; there is no `owner` argument or policy grant to read, write,
curate, or delete another agent's collection. Cross-agent and group-owned facts
are deferred to the access-policy rock and the open gates of the
`access-policy-security-groups` feature in
[access-policy.md](access-policy.md).

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
these targets belongs to the access-policy rock.

## Related Docs

- [fact-service.md](fact-service.md)
- [access-policy.md](access-policy.md)
- [security-groups.md](security-groups.md)
- [api-contract.md](api-contract.md)
- [agent-memory-routing.md](agent-memory-routing.md)
- [billing-and-limits.md](billing-and-limits.md)
