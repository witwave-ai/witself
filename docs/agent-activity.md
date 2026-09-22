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

Exact retries do not add another operation. A send to several recipients
counts once; delivery fanout does not add sender operations. An email write
means the queue accepted the request, not that the recipient received it.

Passive console refreshes, self hydration, status and usage queries, mailbox
listening, claim and lease maintenance, provider callbacks, and internal
bookkeeping do not count. A deliberate fact reveal or message preview can
count without changing its existing read-acknowledgment or fact-ranking
behavior. A curation plan that changes nothing does not add a write.

This catalog covers core data operations, not every administrative endpoint.
The activity-intent signal is cooperative, nonbilling telemetry; it never
grants authorization or changes entitlements. New clients supply an opaque
identifier for each bounded read. Callers can preserve that identifier across
exact retries; a new independent call receives a fresh identifier. Legacy
reads without that identifier are not included. Supported durable writes use
their existing mutation identities to recognize retries.

Fact history is bounded to 1,000 assertions. Larger histories return an error
and do not record a successful read; the endpoint never silently returns a
partial assertion chain. Other paged APIs retain their existing page limits.

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
Existing usage storage and account archive handling carry these additional
dimensions; existing usage and billing behavior is unchanged.
