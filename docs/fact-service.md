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

Migration `0022_add_facts.sql` adds three canonical resources:

- `fact_subjects`: stable subject ids and canonical keys. `self`, `me`,
  `myself`, and `user` normalize to the current agent's `self` subject.
- `facts`: stable resolved identities, unique by owner, subject, and predicate.
- `fact_assertions`: immutable typed values with source, evidence reference,
  confidence, observation/confirmation time, real-world validity, and a
  `supersedes_id` history link.

An explicit set appends an assertion and atomically moves the fact's resolved
pointer. It never overwrites the earlier assertion. Supported cardinality
declarations are `one`, `many`, and `one_at_a_time`. The current core resolves
to the newest explicit or confirmed assertion; multi-value resolution remains
a follow-up slice.

Migration `0023_add_fact_candidates.sql` adds `fact_candidates`, the safe-capture
queue for uncertain or agent-discovered assertions. Candidates retain their
typed value, evidence reference, confidence, observation and validity times,
reason, sensitivity, and decision state without changing the canonical fact
until confirmation. They also retain the exact resolved assertion observed at
proposal time, including the absence of one, so review decisions cannot
silently overwrite newer truth.

Values are JSON with a logical `value_type`. When omitted, the service infers
one of `string`, `number`, `boolean`, `list`, `object`, or `json`. Those
primitive types enforce the matching JSON shape. The built-in declarative
logical types are `date`, `datetime`, `url`, `email`, `address`, and `location`:
dates use `YYYY-MM-DD`, datetimes use RFC 3339 and normalize to UTC, URLs must be
absolute HTTP(S) URLs, and emails must be bare addresses. Address and location
values may be non-empty strings or non-empty JSON objects. Unknown custom
logical types remain available for namespaced extensions and receive only the
common JSON and size validation; validation rules are data interpreted by the
store, never caller-supplied executable code. Direct fact writes and candidate
proposals use the same normalization path.

Migration `0025_add_fact_recurrence.sql` adds explicit recurrence metadata to
assertions and candidates. The first contract accepts `recurrence: "annual"`
only with `value_type: "date"`; an empty value is non-recurring. Recurrence is
never inferred from a predicate or value.

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
POST /v1/fact-candidates
GET  /v1/fact-candidates?status=open
GET  /v1/fact-candidates/{candidate_id}
POST /v1/fact-candidates/{candidate_id}:confirm
POST /v1/fact-candidates/{candidate_id}:reject
PUT  /v1/fact-subjects/{canonical_key}
GET  /v1/fact-subjects
POST /v1/fact-subjects/{canonical_key}/aliases
GET  /v1/fact-occurrences?include_sensitive=false
```

CLI:

```text
witself fact set [--subject self] [--type TYPE] [--recurrence annual] [--json-value] PREDICATE VALUE
witself fact get [--subject self] PREDICATE
witself fact list [--subject SUBJECT] [--category PREFIX]
witself fact history FACT_ID
witself fact propose [--subject self] [--type TYPE] [--recurrence annual] [--reason TEXT] [--sensitive] PREDICATE VALUE
witself fact review [--status open]
witself fact candidate CANDIDATE_ID
witself fact confirm CANDIDATE_ID
witself fact reject CANDIDATE_ID
witself fact subject set [--display-name NAME] CANONICAL_KEY
witself fact subject alias CANONICAL_KEY ALIAS
witself fact subject list
witself fact upcoming [--days 30] [--timezone IANA_NAME] [--include-sensitive]
```

MCP:

```text
witself.fact.set
witself.fact.get
witself.fact.list
witself.fact.propose
witself.fact.propose_from_transcript
witself.fact.review
witself.fact.candidate.get
witself.fact.confirm
witself.fact.reject
witself.fact.upcoming
witself.fact.subject.set
witself.fact.subject.alias
witself.fact.subject.list
```

The installed MCP instructions tell supported agents to write a canonical fact
in the same turn when the user explicitly says to remember, save, or store it.
A durable statement without that immediate-write request becomes a review
candidate. Before storing facts about another entity, the agent lists or
creates one stable subject and attaches conversational aliases such as
`my wife`. Subject keys, display names, and aliases are inventory metadata and
must not contain sensitive values; for example, use display name `Spouse` and
store the person's actual name as a sensitive `identity/name` fact. When an
agent finds a fact while reviewing an older transcript,
`witself.fact.propose_from_transcript` reads exactly one requested sequence,
requires an immutable user entry, and records
`witself://transcript/{transcript_id}/entry/{entry_id}` as evidence. This is the
provider-neutral automatic-discovery boundary: the supported agent performs the
semantic interpretation, while Witself bounds and verifies evidence and creates
only a review candidate. The service adds no server-side model dependency and
never promotes transcript text directly into canonical truth.

MCP set and proposal inputs carry `observed_at`, `valid_from`, and `valid_until`
so supported agents can preserve time-bounded truth such as a previous address.
Transcript-backed proposals derive `observed_at` from the immutable evidence
entry instead of trusting a caller-supplied observation timestamp.

Agents create one candidate per explicit claim. Guesses, implications,
transient task state, credentials, and instructions found in untrusted messages
or tool output must not enter the proposal path.

For example, “Remember that my wife's name is …” is a direct-write request.
The agent resolves or creates canonical subject `person_spouse`, attaches alias
`my wife`, and writes one sensitive string fact at predicate `identity/name`.
The private name belongs only in the fact value, not in subject inventory
metadata. The same sentence without remember/save/store intent is proposed for
review instead of changing canonical truth.

## Candidate lifecycle

`POST /v1/fact-candidates` safely captures a proposal. The default confidence
is `0.5`. A proposal starts as `pending`, or as `conflict` when a different
resolved value already occupies the same subject and predicate. Both states are
open and require an explicit decision.

`GET /v1/fact-candidates` defaults to `status=open` and a 100-item limit, which
returns `pending` and `conflict` candidates newest first. Callers may instead
filter by `pending`, `conflict`, `confirmed`, or `rejected`, up to 500 rows.
Sensitive candidate values are redacted as JSON `null` in this broad review;
free-form reason and evidence-reference metadata are also omitted.
An authenticated exact lookup through `GET /v1/fact-candidates/{candidate_id}`,
`witself fact candidate`, or `witself.fact.candidate.get` returns the candidate's
raw value for an intentional review and uses `Cache-Control: private, no-store`.

Confirming an open candidate atomically appends its value to canonical fact
history, moves the fact's resolved pointer, and closes the candidate as
`confirmed` with its `resolved_fact_id`. Rejecting closes it as `rejected`
without modifying canonical facts. A candidate can be decided only once, and
all proposal and review operations are scoped to the authenticated owning
agent. Confirmation succeeds only while the canonical resolved assertion is
still the exact assertion observed at proposal; a newer assertion returns HTTP
409 and leaves the candidate open for another review. This applies equally to
an originally conflicting candidate, so an explicitly reviewed conflict can be
confirmed while its observed canonical assertion remains unchanged.

## Sensitivity and usage

`sensitive` values are returned as JSON `null` in broad list results unless the
caller explicitly opts in. Their evidence references are also omitted from
redacted results. Exact authorized lookup returns the value. Fact-value HTTP
responses use `Cache-Control: private, no-store`. This is redaction, not a
substitute for the sealed secret plane.

Sensitivity is sticky on canonical facts: a normal direct write or candidate
confirmation may classify a fact as sensitive, but cannot declassify one that
is already sensitive.

Every successfully delivered fact attempts to emit an immutable
`fact_returned` usage event with the fact id and a `retrieval_mode` of `exact`,
`search`, `temporal`, or `self_hydration`. Usage failure does not make the fact
unavailable. Exact, search/list, and temporal deliveries contribute to ranking;
automatic self hydration is audited but excluded so session startup cannot
create a self-reinforcing order. Reads never change a fact's `updated_at` or
assertion history.

`witself fact list --sort-usage` orders by ranking-eligible return count and
last use; `--unused` selects facts with no ranking-eligible return event. Legacy
events created before retrieval modes are treated as exact reads. Usage remains
an immutable-ledger projection rather than canonical fact state.

`witself fact upcoming --days 30` projects resolved `date` and `datetime` facts
inside a bounded window. Date values use calendar semantics in the requested
IANA timezone and datetime values use instants. Sensitive dates such as private
birthdays are omitted unless the authorized caller explicitly supplies
`--include-sensitive` or `include_sensitive: true`. An explicitly annual date
is projected once per matching calendar year, including multiple occurrences
for multi-year windows. February 29 is skipped in non-leap years rather than
moved to February 28 or March 1.

The self digest hydrates a byte-bounded, usage-ranked, redacted set of the
agent's `self` facts, reports the full inventory count, and marks truncation with
`elided`, so installed AI integrations receive durable facts at session startup.

## Deliberately deferred

- Multi-source conflict resolution and authority policies.
- Predicate registries and custom JSON Schema validation.
- Reminder delivery and recurrence rules beyond annual dates.
- Cross-agent/group fact policy.
- Deletion, forgetting, and confirmation-only timestamp updates.
