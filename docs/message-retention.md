# Message Entitlement And Retention

Status: implemented; production activation is staged.

Messaging availability and message retention are two independent account-level
behaviors. They are unrelated to inactive-account cleanup, account closure,
transcript retention, narrative-memory retention, and audit retention.

## Plan defaults

| Plan | Messaging | Message retention |
|---|---:|---:|
| Personal | Disabled | 30-day downgrade cleanup |
| Professional | Enabled | 90 days |
| Team | Enabled | 365 days |
| Enterprise | Enabled | 365-day safe default; contractual override |
| Founder account | Enabled override | Explicit indefinite override |

Enterprise does not become indefinite by accident. An administrator must
create an attributed account exception. The founder account uses that same
explicit exception rather than changing the Enterprise plan default.

The resolved cell snapshot carries:

- feature `messaging` when mailbox operations are enabled;
- policy `message_retention_days` for a finite window;
- no `message_retention_days` for explicit indefinite retention; and
- internal policy `messaging_entitlement_version: 1` to distinguish a new
  entitlement-aware snapshot from a legacy snapshot during rollout.

The version marker remains present when retention is indefinite. That lets an
administrator disable messaging independently without an absent retention key
being mistaken for a legacy account.

## Disabled behavior

Every message and message-request store operation takes an account-row share
lock and checks the current resolved snapshot. A concurrent plan change and
operation therefore have one database-defined order.

When messaging is disabled:

- send, receive/listen, list, read, ack, reply, processing, checkpoint polling,
  and open-request operations are unavailable;
- the server returns HTTP 403 with `code: "feature_not_enabled"`,
  `feature: "messaging"`, `retryable: false`, and the friendly message
  `Sorry, this feature is not enabled on this account.`;
- message body and payload are rejected before persistence;
- no recipient notification message is created; and
- the authenticated self checkpoint reports `enabled: false`, allowing an
  installed client to stop polling without treating the service as unhealthy.

Existing mailbox data becomes inaccessible immediately. A finite retention
policy can then remove it on schedule. Personal keeps a 30-day policy even
though its feature is disabled, so downgrade cleanup cannot become accidentally
indefinite.

The complete MCP tool catalog and managed instruction set remain installed on
every client. Entitlement is checked on each backend operation, and the
authenticated self/hook state is refreshed dynamically. Changing plan or
account overrides never requires reinstalling the client integration.

## Admin overrides

Availability and retention overrides are independent from each other and from
plan, price, subscription dates, and invoice history:

```sh
witself-admin account messaging get --account ACCOUNT_ID
witself-admin account messaging set --account ACCOUNT_ID --enabled --reason "..."
witself-admin account messaging set --account ACCOUNT_ID --disabled --reason "..."
witself-admin account messaging clear --account ACCOUNT_ID --reason "..."

witself-admin account message-retention get --account ACCOUNT_ID
witself-admin account message-retention set --account ACCOUNT_ID --days 365 --reason "..."
witself-admin account message-retention set --account ACCOUNT_ID --indefinite --reason "..."
witself-admin account message-retention clear --account ACCOUNT_ID --reason "..."
```

The matching control-plane resources are:

- `GET|PUT|DELETE /v1/admin/accounts/{id}/messaging`;
- `GET|PUT|DELETE /v1/admin/accounts/{id}/message-retention`.

Mutations use the existing compare-and-swap account lifecycle record, retain
actor/reason/time attribution, advance the desired snapshot revision, and are
applied to the cell through the same hash-acknowledged plan-snapshot path as
other account behavior.

## Whole-thread retention

Migration `0068` adds a value-free, rebuildable activity projection with one
row per `(account, realm, thread)`. A `BEFORE INSERT` trigger advances the
thread's last-message time. This has two purposes:

1. candidate scans use a small indexed row rather than grouping the complete
   message table every five minutes;
2. the row lock is a concurrency fence, so a new send and a whole-thread delete
   cannot pass each other.

The general-purpose `witself-worker` owns the cleanup loop. Preview and
enforcement each have 16 durable logical lanes and independent per-account
keyset scan cursors. `FOR UPDATE SKIP LOCKED` lets two or more replicas claim
different lanes without duplicate deletion. Held or busy old threads advance
the cursor and are revisited on the next finite scan cycle, so they cannot
starve later eligible threads.

A thread is eligible only when its last message is older than the account
cutoff. Retention bounds, locks, and deletes its complete message, delivery,
reply, open-request, candidate, selection, and claim graph in one transaction.
Every graph relation uses `SKIP LOCKED` with exact row-count completeness
checks. If foreground work owns even one row, cleanup defers the whole thread
without waiting behind that work. Individual graph rows are never trimmed
independently.

Deletion is deferred when:

- a live direct-delivery processing claim exists;
- an unexpired open request or live request claim exists; or
- any message in the thread is referenced by resolved memory evidence;
- foreground work owns part of the thread graph; or
- a thread exceeds the bounded cleanup graph ceiling; or
- admitting the thread would exceed the cumulative graph budget for this batch.

Message retention does not delete a narrative memory. A memory-evidence hold is
an explicit exception to the ordinary plan window: it is reported through a
value-free counter and remains until a separate, authorized memory lifecycle
operation removes that provenance. This mirrors transcript provenance holds
and must remain visible operationally; a future product policy may place its
own bound on holds without changing ordinary message retention.

The activity projection is not trusted as the sole deletion proof. After
locking the bounded graph, cleanup rechecks the newest actual message timestamp.
A stale projection is repaired and the thread is retained. Oversize graphs are
durably quarantined for 24 hours and exposed through a value-free
`deferred_oversize` metric, while the lane and account cursor advance so other
threads continue. The current technical ceiling is 4,096 messages, 4,096 rows
in each associated graph relation, and 4,096 total graph rows per thread.
Eligible threads are admitted in cursor order until a cumulative 65,536-row
batch graph budget is reached; later candidates are reported through the
value-free `deferred_budget` metric and remain available for a later pass.
These are operational safety boundaries, not plan message-count limits; an
oversize alert requires operator review rather than silent partial deletion.

Lane selection commits a short durable lease before the heavier graph
transaction begins. If a database statement reaches its deadline or the
worker crashes, that exact lane remains backed off until the lease expires,
allowing another replica to continue on a different due lane instead of
immediately retrying the same expensive graph.

Message idempotency replay lasts no longer than the retained source thread.
After retention removes a thread, reusing an old idempotency key is not a
promise to recover its deleted result.

## Worker configuration and metrics

The job is disabled by default:

- `WITSELF_MESSAGE_RETENTION_ENABLED` (`false`);
- `WITSELF_MESSAGE_RETENTION_MODE` (`preview` or `enforce`);
- `WITSELF_MESSAGE_RETENTION_BATCH_SIZE` (`25`, range 1-100 threads);
- `WITSELF_MESSAGE_RETENTION_INTERVAL` (`5m`, range 1m-24h);
- `WITSELF_MESSAGE_RETENTION_BATCH_TIMEOUT` (`2m`, range 10s-5m).

Deletion requires both `enabled=true` and `mode=enforce`. The chart exposes the
same values at `worker.messageRetention`. Worker health remains on `/livez`,
`/startupz`, and `/readyz`; Prometheus exposes separate
`witself_worker_message_retention_batches_total`,
`witself_worker_message_retention_items_total`, and
`witself_worker_message_retention_last_success_timestamp_seconds` families.
The item family includes value-free `deferred_locked`, `deferred_oversize`,
`deferred_budget`, and `repaired_activity` counters. No account, realm, agent,
thread, message, or content value is a metric label.

## Rollout

1. Deploy the cell image and migration with message retention disabled.
2. Legacy applied snapshots without `messaging_entitlement_version` continue to
   allow messaging; this prevents a cell-first rollout from disabling existing
   accounts.
3. Before fleet reconciliation, set the founder account's explicit indefinite
   retention override and explicit enabled override.
4. Reconcile the new catalog. Every new snapshot carries entitlement version
   1, after which the explicit `messaging` feature is authoritative.
5. Enable retention in `preview`, review value-free eligible/deferred counts,
   then switch to `enforce` in a config-only worker rollout.

After a version-1 snapshot is accepted, do not roll that account back to a
pre-entitlement control plane or cell binary. Old software does not understand
the new enforcement boundary.

## Fair-use boundary

“Unlimited messages” means the plan has no stored-message count cap. It does
not remove the existing 64 KiB body, 16 KiB payload, 64-recipient audience,
lease, causal-depth, request-expiry, and API abuse protections. Plan-backed
send/delivery rate controls remain a separate limit dimension; they do not
change the retention algorithm.
