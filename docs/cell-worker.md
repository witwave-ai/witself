# Witself Cell Worker

Status: implemented.

## Purpose

`witself-worker` is the long-running background-work process for one Witself
cell. It is separate from the public API process:

- `witself-server` serves product API traffic;
- `witself-worker` runs bounded, cell-local maintenance jobs.

Both executables ship in the same signed, versioned cell-runtime image. Keeping
them together guarantees that the API, worker, embedded schema, and store code
come from one release while still allowing Kubernetes to deploy, scale, and
restart them independently.

The worker is not a second control plane. The control plane resolves plans and
account policy. The worker applies cell-local behavior to data stored in the
cell.

## Process Model

The worker runs continuously as a Kubernetes Deployment. It does not expose a
product API or require an Ingress. Its only HTTP listeners are:

- health on `:8081`;
- Prometheus metrics on `:9090`.

Production starts with two worker replicas. Each replica runs the same
explicit registry of background jobs. Jobs have independent loops, so a slow
job does not stop unrelated jobs in the same process. A second replica keeps
making progress if one process or node is unavailable.

Worker jobs must be:

- bounded rather than "drain everything";
- safe to retry;
- cancellable with a deadline;
- coordinated through durable PostgreSQL state;
- free of tenant identifiers or payload values in metrics and logs.

The registered jobs are transcript retention, message retention, agent-email
retention, outbound agent-email dispatch, avatar-style rollout, message-rate
bucket cleanup, and agent-email-rate bucket cleanup. Both cleanup jobs are
enabled whenever the worker runs unless an operator explicitly disables one.
Outbound dispatch is independently default-off. New job types must opt in
explicitly; the worker is not an arbitrary command runner.

## Cooperative Scaling

Adding replicas must add useful capacity without allowing duplicate work.
Every scalable job divides its work into durable logical units and claims one
unit with PostgreSQL locking. A losing worker skips the busy unit and claims a
different one.

Transcript retention uses a fixed set of logical lanes independent of the pod
count. Accounts have a stable lane assignment, and preview and enforcement
keep separate progress. A worker locks one due lane with
`FOR UPDATE SKIP LOCKED` and retains that fence in the same transaction that
rechecks policy, advances cursors, handles provenance holds, and deletes any
eligible conversations. Therefore:

- two replicas can process two different lanes concurrently;
- two replicas cannot delete the same conversation;
- a blocked lane does not block other lanes;
- a crash rolls back the transaction and releases the lane;
- changing the replica count does not reshuffle accounts or lose progress.

The configured batch timeout bounds a stuck database operation. The existing
account and conversation locks, PostgreSQL clock cutoff, evidence and active
curation holds, and whole-conversation atomic delete remain the final safety
boundary.

Message retention uses its own fixed preview/enforcement lanes, account
cursors, and per-account thread-scan cursors. A small rebuildable activity
projection supplies an indexed last-message timestamp for each thread. Its
`BEFORE INSERT` trigger is also the synchronization fence: a new send cannot
materialize content while retention owns that thread, and retention cannot
classify the thread while a send is advancing its activity. The batch locks the
complete message/request graph and deletes only whole inactive threads.
Memory-evidence references and live delivery/request claims defer deletion.
Per-thread and cumulative batch graph ceilings keep work bounded. Lane
selection first records a short durable lease, so a timeout or crash backs off
that exact lane while another replica can claim a different due lane. Message
and transcript retention never share a lane or cadence.

Message-rate bucket cleanup removes only expired GCRA coordination rows. Each
replica attempts one batch immediately and then at the configured interval.
One statement orders and locks at most 10,000 stale rows with `FOR UPDATE SKIP
LOCKED`, clamps the cutoff to a full idle minute on the PostgreSQL clock, and
deletes only the selected expired rows. Two replicas therefore divide
available rows without waiting for or deleting the same bucket. A ten-second
default batch deadline bounds a stuck attempt; recoverable failures are
reported and retried on the next interval.

Agent-email-rate bucket cleanup follows the same bounded loop against its
independent email safety table. PostgreSQL `SKIP LOCKED` divides stale rows
across replicas, while the store-level database-clock cutoff is the final
authority on when accumulated email debt has fully expired.

Outbound agent-email dispatch uses the durable cell outbox rather than a
process-local queue or timer. Every enabled replica may claim ready rows. A
claim id, monotonic generation, and database-time lease fence one provider
attempt; a losing replica skips or fails the stale transition instead of
sending concurrently. The worker marks the durable provider boundary before
calling the adapter, but only after rechecking the active account, current send
entitlement, live agent, both operator controls, recipient suppression, and
the independent provider-attempt rate lane. It signs one immutable
dispatch with the cell's Ed25519 key. Exact retries preserve the same `esnd_`
id so the adapter's Durable Object
receipt can return the prior result. Transport uncertainty schedules only that
exact receipt replay; the worker never creates a fresh id and guesses that
resend is safe. Work closes after 12 attempts or 72 hours, and an outcome that
still cannot be proved after crossing the provider boundary becomes
`ambiguous`.

The job processes a bounded batch (default 10), waits two seconds between
attempts, bounds a batch at 30 seconds, and bounds one adapter request at 20
seconds. Provider credentials remain at the Cloudflare adapter. The worker has
only the HTTPS endpoint, audience, key id, and private dispatch key. Scaling
from two replicas can increase throughput when multiple ready rows and database
capacity exist; one slow provider request occupies only its bounded job loop,
not the API service or unrelated worker jobs.

### Operator-only accepted-receipt proof

The worker image also contains one bounded, non-scheduled operator command for
the first production outbound canary:

```sh
witself-worker agent-email receipt-replay \
  --account-id acc_... \
  --send-id esnd_... \
  --expected-accepted-at 2026-08-15T18:00:00.000000Z \
  --expected-attempt-count 1 \
  --json
```

Do not exec that command in a live worker pod. Use the repository's transient
operator helper, pinning the exact image and worker ConfigMap checksum already
verified for the rollout:

```sh
scripts/run-agent-email-receipt-proof.sh \
  --cell civo-sandbox-usw2-dev \
  --kubeconfig /absolute/private/path/kubeconfig \
  --context witself-civo-sandbox-usw2-dev \
  --namespace witself \
  --expected-image ghcr.io/witwave-ai/images/witself-server:0.0.249 \
  --expected-config-checksum 0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef \
  --expected-replicas 2 \
  --account-id acc_... \
  --send-id esnd_... \
  --expected-accepted-at 2026-08-15T18:00:00.000000Z
```

Replace the checksum example with the exact 64-character `checksum/config`
annotation approved for that rollout. The helper also verifies the active
server's checksummed managed-cell identity, requires exactly two fully ready
worker replicas, validates the exact Deployment-to-ReplicaSet-to-Pod ownership
chain for both ready worker Pods, and rereads every source before its fixed
lock and Job are created. It first snapshots the private kubeconfig at mode
0400, so a concurrent rewrite of the operator's original file cannot redirect
later reads, writes, logs, or cleanup. It copies only the dispatch endpoint,
dispatch key ID, provider timeout, database Secret reference, and immutable
dispatch-key Secret reference. Secret values are never read. The database
Secret remains compatible with ExternalSecret rotation: its explicit mutable
state and exact UID/resourceVersion must remain unchanged through the initial,
pre-lock, pre-Job, post-proof-Pod-start, and post-proof-read fences. The
dispatch Secret is immutable and receives the same UID/resourceVersion
rechecks. That metadata fence is paired with an exact
account/send/accepted-at/attempt/provider row check and the edge receipt's
digest-and-signer match. The expected attempt count is fixed at one; it is not
an operator-adjustable flag.

The Job is fixed-name, backoff-free, deadline bounded, tokenless, non-root,
read-only-root, and deleted with foreground propagation. The helper captures
the API-assigned lock and Job UIDs, accepts logs only from the exact Pod whose
controller owner is that Job UID, and rereads the exact lock, Job, and Pod
after the log read. Cleanup sends Kubernetes `DeleteOptions` with the captured
UID as a precondition, so a same-name replacement cannot be read or deleted.
It then polls the exact Pod name to absence while enforcing the captured Pod
UID and separately requires the Job-label selector to be empty. If exact
deletion or owned-Pod absence cannot be proved, the immutable fixed-name lock
remains for explicit operator cleanup. On success stdout is only the locally
revalidated closed receipt proof. It is not an API, worker job, retry loop, or
agent command. It does not run migrations. Its PostgreSQL transaction is
explicitly repeatable-read and read-only.

The command reconstructs the dispatch through the same production projection
and deterministic JSON serializer used by the live worker. Before signing, it
requires the exact account and send IDs, exact accepted timestamp, local
`accepted` state, Cloudflare provider, nonempty private provider receipt ID,
attempt count exactly one, and an acceptance age no greater than the adapter's
fixed seven-day receipt lifetime. Any mismatch fails closed without contacting
the edge.

The proof request uses only `POST /v1/dispatch:receipt-replay` and the distinct
`witself-agent-email-send-receipt-replay` audience. Redirects are forbidden. A
successful result must be the exact closed
`witself.agent-email-dispatch-receipt-proof.v1` schema, match the send ID and
accepted receipt, attest both digest and signer matches, report exactly one
provider-call start, and report `route_pending=false`. Any non-200 response,
missing or extra field, unresolved receipt, digest/signer conflict, second
provider-call start, or unsettled route fails closed. Standard output contains
only that value-free proof; it never contains the body, recipient, subject,
provider receipt ID, digest, key material, or token.

Agent-email retention has its own 16 preview lanes and 16 enforcement
lanes. The worker briefly leases one lane, then takes an exclusive account row
with `SKIP LOCKED`; foreground email ingress and mailbox operations use a
share lock on that same row. This makes a busy account a deferral instead of a
queue behind live work and prevents new claims, duplicate links, or retry
proofs from racing deletion. Account-local age cursors use PostgreSQL
`received_at`, reset when the retention policy changes, and advance past
temporarily claimed messages so one hold cannot starve later mail.

The same account fence and retention policy also govern the outbox. Eligible
outbound rows age from `created_at`; `queued`, `claimed`, and
`provider_started` rows are unresolved-work holds and are never selected.
Outbound scanning is oldest-first and uses `SKIP LOCKED` inside the existing
account transaction, while the established inbound cursor remains unchanged.
Provider-event receipts are deleted in bounded batches before a parent send so
parent deletion cannot hide an unbounded cascade.

One batch selects at most 100 messages and deletes at most 32 MiB of raw MIME.
A live processing lease defers its message; unread, unacknowledged, completed,
available, and expired-claim messages are otherwise age eligible. Enforcement
clears suspected-duplicate backlinks before deleting the message. Delivery
rows and accepted retry-canary proofs cascade, while address reservations,
mailboxes, account audit events, and usage data remain.

Scaling is not guaranteed to be perfectly linear. Two replicas can approach
twice the throughput when at least two lanes have work and PostgreSQL has
capacity. A single hot account, provenance holds, or database saturation can
limit that gain.

Replica count is manual initially. A future autoscaler should use ready-work
count or oldest-ready-work age rather than CPU alone, and it must retain a
minimum of two replicas for availability.

## Health

The health listener exposes:

- `/livez`: the process and supervisor are alive;
- `/startupz`: configuration, schema, job registration, and initial database
  setup completed;
- `/readyz`: PostgreSQL is reachable and the worker can safely attempt work;
- `/healthz`: liveness alias.

An empty queue is healthy. A transient PostgreSQL failure affects readiness,
not liveness. Kubernetes should restart a genuinely wedged process, while
ordinary job failures remain visible through metrics and retries.

## Metrics

`/metrics` serves Prometheus text format. The initial worker telemetry includes
bounded job names and result classes only:

- process up and registered-job running gauges;
- recoverable job failures and supervised job-loop exits;
- retention batch success, no-work, and error counts;
- retention last-success time; and
- retention scanned, eligible, deleted, capped, and deferred counts.

Message retention has separate
`witself_worker_message_retention_*` batch, item, and last-success families.
Its item kinds distinguish eligible/deleted threads, deleted messages,
evidence holds, active-work holds, lock deferrals, oversize quarantine, and
cumulative-budget deferrals.

Agent email has separate
`witself_worker_agent_email_retention_*` batch, item, and last-success
families. The common configured batch budget is spent across selected inbound
messages, outbound messages, provider-event receipts, and recipient
suppressions. Item kinds therefore distinguish inbound counts from the closed
`outbound_message`, `provider_event`, and `recipient_suppression` families,
along with deleted raw/body bytes, live-work holds, lock/oversize/budget
deferrals, duplicate-link repairs, and bounded retry-canary cleanup.

Outbound dispatch exposes
`witself_worker_agent_email_outbound_batches_total{result}`,
`witself_worker_agent_email_outbound_items_total{kind}`, and
`witself_worker_agent_email_outbound_last_success_timestamp_seconds`. The
closed item kinds are claimed, accepted, delivered, retried, bounced,
rejected, ambiguous, canceled, and `expired_reconciled` outcomes. Send id,
account, realm, agent, recipient, subject, provider message id, and provider
error text are forbidden labels.

Message-rate bucket cleanup has separate
`witself_worker_message_rate_bucket_cleanup_*` batch-result, deleted-row, and
last-success metrics. Its only result label values are `success`, `no_work`,
and `error`; it has no tenant-derived labels.

Inbound- and outbound-email rate-bucket cleanup has an independent
`witself_worker_agent_email_rate_bucket_cleanup_*` batch-result, deleted-row,
and last-success family with the same closed result labels and no
tenant-derived labels. One scheduled sweep drains consecutive 10,000-row
batches from both rate tables until each is caught up or the shared sweep
timeout expires. The metrics intentionally aggregate the two value-free row
counts; full-batch throughput and timeout failures remain directly visible.

Account, realm, agent, conversation, task, transcript, memory, and secret
identifiers must never be metric labels. Error text and stored content must
never enter metrics.

The worker has its own metrics Service and monitoring selector. API Services,
Ingress, and monitors select only `witself-server`; worker monitoring selects
only `witself-worker`.

## Shutdown And Rollout

On `SIGTERM`, the worker stops beginning new attempts, cancels its job
contexts, lets PostgreSQL roll back unfinished transactions, shuts down its
health and metrics listeners, and exits.

The initial migration is deliberately overlap-safe:

1. deploy the new worker while old API replicas may still run their embedded
   retention loop;
2. migration 66 atomically copies the singleton's latest cadence into the lane
   rows and parks the legacy scheduled cadence; the retained singleton then
   remains a mixed-version in-flight fence—old workers take its exclusive lock
   and new lane workers take a shared lock, so those two scheduling models
   cannot overlap while new workers can still cooperate with each other;
3. replace all API replicas with the API-only process;
4. verify worker progress, health, and metrics before considering the handoff
   complete.

The old singleton retention state remains schema-compatible during this
transition. It is not the scheduling path for `witself-worker`.

Message retention has a separate activation sequence:

1. migrate the activity projection, per-account scan state, and 16
   preview/enforcement lanes while the job is disabled;
2. enable `preview` and review only value-free counts;
3. switch to `enforce` in a config-only worker rollout.

Agent-email retention uses the same sequence with its own schema,
lanes, and metrics. Changing `worker.agentEmailRetention` alters only the
worker ConfigMap checksum; API pods are not restarted.

Outbound agent-email activation is a separate dark rollout:

1. migrate the outbox, send controls, suppressions, provider-event receipts,
   and rate buckets to schema 89 while dispatch remains disabled;
2. onboard and authenticate `send.witmail.net`, provision the adapter Email
   Sending/receipt/route/Queue bindings, configure one cell signing key and the
   matching adapter public-key/account allowlist, and install the independent
   provider-event bearer/target map while every gate remains off;
3. deploy two compatible worker replicas, enable and verify the adapter for an
   exact canary cohort, then enable `worker.agentEmailOutbound` only in that
   cell and its resolved account policy;
4. prove queue-to-provider idempotent replay, timeout-to-ambiguous handling,
   provider-event folding, suppression, health, metrics, and rollback before
   widening either cohort.

Schema 89 is a forward-only convergence barrier. The first compatible process
that starts against a cell applies the migration automatically. After that,
any pre-0.0.245 binary fails startup with `ErrMigrationSchemaAhead`; it is not
a viable rollback even if its Deployment manifest still exists. Keep all
outbound gates off, freeze account export/import and moves, and converge every
API and worker replica on a schema-89-compatible image before accepting an
outbound row. Roll back with account, worker, and adapter gates or deploy a
forward fix. Never down-migrate the database or restart an older image.

A populated source-to-destination move canary is also a release gate, not just
an archive unit test. Before moving any account with outbound-email history,
exercise source suspend and export, archive validation, destination import,
and activation with fixtures containing at least an accepted/delivered send,
a provider event, a recipient suppression, and a `claimed` send. Verify that
tenant links and safety rows survive, `claimed` becomes `queued` with its fence
consumed, its destination-authored retry timestamps use the destination
database import clock, and no provider call occurs during the move. Source
history remains bounded by the manifest's exact `exported_at`; only the exact
timestamps created by claim normalization may cross that boundary. Also prove
that source evacuation is refused while any send is `provider_started`, leaves
the account active, and succeeds only after the worker has resolved that
exact-replay window. Import still normalizes a legacy or manually constructed
`provider_started` archive to terminal `ambiguous` as a defensive fallback;
the supported move path never creates such an archive. An empty new cell is
the preferred migration target, but it does not replace this populated canary
before the first email-active account moves.

Enabling a catalog feature, account override, realm/agent switch, worker job,
or adapter gate never implicitly enables the others.

The API process never runs this loop. Multiple worker replicas claim different
database lanes with `SKIP LOCKED`; replica count is not encoded in the lane
cardinality.
