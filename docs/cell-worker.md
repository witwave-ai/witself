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

The registered jobs are transcript retention, message retention, inbound
agent-email retention, avatar-style rollout, and message-rate bucket cleanup.
The cleanup job is enabled whenever the worker runs unless an operator
explicitly disables it. New job types must opt in explicitly; the worker is not
an arbitrary command runner.

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

Inbound agent-email retention has its own 16 preview lanes and 16 enforcement
lanes. The worker briefly leases one lane, then takes an exclusive account row
with `SKIP LOCKED`; foreground email ingress and mailbox operations use a
share lock on that same row. This makes a busy account a deferral instead of a
queue behind live work and prevents new claims, duplicate links, or retry
proofs from racing deletion. Account-local age cursors use PostgreSQL
`received_at`, reset when the retention policy changes, and advance past
temporarily claimed messages so one hold cannot starve later mail.

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

Inbound email has separate
`witself_worker_agent_email_retention_*` batch, item, and last-success
families. Item kinds cover selected and deleted messages, deleted raw bytes,
live-claim holds, lock/oversize/budget deferrals, duplicate-link repairs, and
cascaded retry-canary proofs.

Message-rate bucket cleanup has separate
`witself_worker_message_rate_bucket_cleanup_*` batch-result, deleted-row, and
last-success metrics. Its only result label values are `success`, `no_work`,
and `error`; it has no tenant-derived labels.

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

Inbound agent-email retention uses the same sequence with its own schema,
lanes, and metrics. Changing `worker.agentEmailRetention` alters only the
worker ConfigMap checksum; API pods are not restarted.

The API process never runs this loop. Multiple worker replicas claim different
database lanes with `SKIP LOCKED`; replica count is not encoded in the lane
cardinality.
