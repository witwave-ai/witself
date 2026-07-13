# Fact Service: Implemented Core

Status: implemented core slice. Last reviewed 2026-07-12.

The fact service is the deterministic durable-knowledge facility beside
semantic memory. Exact lookup never uses embeddings or usage ranking. A fact is
addressed by the token-bound owning agent, a stable subject, and a namespaced
predicate:

```text
(owner_agent, subject, predicate) -> resolved assertion
```

Examples include `self/identity/birth-date`,
`person_spouse/identity/name`, `self/preferences/editor`, and
`project_witself/resources/repository`.

## Storage

Migration `0022_add_facts.sql` adds three resources:

- `fact_subjects`: stable subject ids and canonical keys. `self`, `me`,
  `myself`, and `user` normalize to the current agent's `self` subject.
- `facts`: stable resolved identities, unique by owner, subject, and predicate.
- `fact_assertions`: immutable typed values with source, evidence reference,
  confidence, observation/confirmation time, real-world validity, and a
  `supersedes_id` history link.

An explicit set appends an assertion and atomically moves the fact's resolved
pointer. It never overwrites the earlier assertion. Supported cardinality
declarations are `one`, `many`, and `one_at_a_time`. The current core resolves
to the newest explicit assertion; multi-value resolution and candidate/conflict
workflows are follow-up slices.

Values are JSON with a logical `value_type`. When omitted, the service infers
one of `string`, `number`, `boolean`, `list`, `object`, or `json`. Declarative
logical types such as `date`, `url`, and `address` are accepted; registry-backed
schema validation is a follow-up slice.

Sources are `self`, `operator`, `agent`, `import`, or `inference`. Current
agent-token API, CLI, and MCP writes derive `agent` at the authenticated service
boundary; callers cannot claim operator or import provenance. `source_ref` may
point to a transcript entry or another evidence artifact.

## Surfaces

HTTP:

```text
POST /v1/facts
GET  /v1/facts?subject=self&predicate=preferences/editor
GET  /v1/facts?subject=self&predicate_prefix=preferences&limit=100
GET  /v1/facts/{fact_id}/history
```

CLI:

```text
witself fact set [--subject self] [--type TYPE] [--json-value] PREDICATE VALUE
witself fact get [--subject self] PREDICATE
witself fact list [--subject SUBJECT] [--category PREFIX]
witself fact history FACT_ID
```

MCP:

```text
witself.fact.set
witself.fact.get
witself.fact.list
```

The installed MCP instructions tell supported agents to store explicit durable
facts and preferences, while refusing guesses, transient task state, and
credentials.

## Sensitivity and usage

`sensitive` values are returned as JSON `null` in broad list results unless the
caller explicitly opts in. Exact authorized lookup returns the value. This is
redaction, not a substitute for the sealed secret plane.

Every successful exact `GetFact` attempts to emit an immutable
`fact_returned` usage event with the fact id. Usage failure does not make the
fact unavailable. Usage aggregation and usage-aware browse/hydration ranking
remain separate from canonical fact mutation: reads never change a fact's
`updated_at` or assertion history.

## Deliberately deferred

- Subject alias management beyond the built-in `self` aliases.
- Candidate proposals and explicit confirm/reject flows.
- Multi-source conflict resolution and authority policies.
- Predicate/type registries and JSON Schema validation.
- Temporal occurrence projection and reminders.
- Usage-ranked listing and fact-usage reports.
- Cross-agent/group fact policy and export/import coverage.
- Deletion, forgetting, and confirmation-only timestamp updates.
