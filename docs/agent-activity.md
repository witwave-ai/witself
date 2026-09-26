# Agent activity metrics

The agent consoles separate **inventory** (what is stored) from **activity**
(what has been recorded over time). Operations measures successful data
operations. It is not a price, a charge, or a database transaction count.

## Operations

One operation is one successfully completed, bounded data API request in the
`core-records.v1` catalog. A page of search results counts as one read; fetching
another page counts as another read. A batch append counts as one write.
Records returned or affected appear separately, so a successful search with
zero results is one read and zero records.

| Area | Reads | Writes |
| --- | --- | --- |
| Facts | Exact lookup, search, history, upcoming occurrences | Set, propose, confirm, reject, delete |
| Memories | Exact lookup, list, recall, history | Capture, revise, supersede, archive, restore, reactivate, delete; curation apply with changes |
| Transcripts | List, entry page | Create, append an entry or batch |
| Messages | List, explicit read or preview | Send or reply |
| Email | Inbox, explicit read, outbox list or detail | Queue a send or reply |
| Secrets | Inventory, metadata, explicit field access | Create, archive, restore, delete |

Exact read retries with the same activity ID do not add another operation
while their events are retained (35 days). Durable write retries use their
domain mutation identities. A send to several recipients
counts once; delivery fanout does not add sender operations. An email write
means the queue accepted the request, not that the recipient received it.

Passive console refreshes, self hydration, status and usage queries, mailbox
listening, claim and lease maintenance, provider callbacks, and internal
bookkeeping do not count. Prompt-time hydration recall and automatic curator
reads explicitly send `X-Witself-Activity-Observation: 1` with no activity ID.
Deliberate CLI/MCP memory recall remains a recorded read. Direct user-invoked
reads are not made observational by the curator adapter. A deliberate fact reveal or message preview can
count without changing its existing read-acknowledgment or fact-ranking
behavior. A curation plan that changes nothing does not add a write.

This catalog covers core data operations, not every administrative endpoint.
The activity-intent signal is cooperative, nonbilling telemetry; it never
grants authorization or changes entitlements. New clients supply an opaque
identifier for each bounded read. Callers can preserve that identifier across
exact retries; a new independent call receives a fresh identifier. Legacy
reads without that identifier are not included. Supported durable writes use
their existing mutation identities to recognize retries.

Fact history returns the newest 1,000 assertions with HTTP 200 and an explicit
`truncated` boolean. When true, older assertions remain; the CLI and consoles
show a truncation notice. Traversal remains capped and this endpoint has no
cursor. The read records only the returned assertions. Other paged APIs retain
their existing page limits.

Malformed or duplicated activity-intent headers are stripped without blocking
the route. Domain write idempotency keys retain their normal validation. MCP
invocations establish a stable client retry scope: exact HTTP retries share an
activity ID, while distinct routes, query strings, bodies, and new invocations
receive distinct IDs. A separately reissued MCP call is a new invocation.

To inspect the same metrics without opening a console:

```sh
witself usage --activity --agent scott
witself usage --activity --agent scott --json
```

The activity report defaults to 24 UTC hour buckets, including the current
partial hour. Use `--since` and `--until` to select a different window, up to
31 days. Bounds select hourly bucket starts: a historical `--until` inside
an hour includes that hour's current rollup, not an event-by-event cutoff.
Reading the report does not add an operation.

## Memory changes

Memories retain their exact active inventory count. Their activity graph
counts changes separately: created, revised, archived, restored, and deleted.
Superseding a memory revises its source and creates its replacements. A
curation operation may change several memories, so its memory-change count
can exceed its operation count. Importing an archive does not replay its
historical changes as new activity.

## Time and availability

The summary shows 24 UTC hourly buckets, including the current partial hour.
These are recorded quantities in their named units; quantities from different
categories must not be added into a general total.

Recording starts for an agent when the first supported operation or memory
change is durably recorded. A separate persistent marker retains that start
across server restarts. This does not certify activity from older clients or
older replicas during a rollout. The activation hour is partially tracked,
and earlier hours remain unknown. A zero means no activity was recorded in
the tracked bucket; it must not stand in for missing history.

- **Not tracked yet:** no recording-start marker exists for the agent.
- **Server update needed:** the cell does not implement the activity API.
- **Unavailable:** the request failed or its response could not be validated.

Existing transcript, fact, secret, email, and message usage graphs remain
independent of this new API. The bounded recent-updates list remains a set of
timestamp observations rather than a complete action audit log.

## Data boundary

`GET /v1/activity` returns an authenticated agent's value-free hourly report,
with schema `witself.agent-activity.v1` and catalog `core-records.v1`. Reads of
that report do not create activity. Events retain fixed operation names,
numeric quantities, and opaque identities only. They do not copy fact or
memory values, secret material, message bodies, email subjects, or addresses.

The event and its hourly and daily projections commit together. Mutation
activity commits in the same database transaction as the mutation itself.
Read metering is best-effort: a failed meter logs only the fixed operation name
and increments `witself_activity_metering_failures_total`; the successful read
still succeeds and its activity can be undercounted. Reads reuse a committed
in-process marker cache and do not repeat the domain's scope verification.
Write metering remains transactional. Operation and record companion events
remain separate because the rollup/summary contract exposes distinct units,
quantities, and event counts and the current ledger row has one dimension.

The existing message-rate-bucket maintenance job retires at most 1,000
`operation_*` and `memory_*` catalog events older than 35 days per sweep.
This requires that existing cleanup job to be enabled (its default); its
configured interval and batch deadline also bound retention. Concurrent workers
skip locked rows. The activation marker, all rollups, and billing events survive.
Archives carry the retained events plus the authoritative activity rollups;
import permits retired activity events, but never rollups below retained event
counts or quantities. Non-activity projections must still match their ledger.
The legacy `/v1/usage` route rejects activity dimensions; use `/v1/activity`.
Existing usage and billing behavior is otherwise unchanged.
