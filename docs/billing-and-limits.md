# Witself Billing And Limits

Status: draft. Decision: v0 billing is account-level, plan-based, usage-aware,
and not raw per-call billing at launch.

This document and `web/plans/plans.json` define packaging and resolved account
behavior, not feature readiness. The canonical [Feature Status](feature-status.md)
scorecard owns implementation, managed rollout, evidence, and open gates while
referencing every plan feature, limit, and policy key exactly once.

Narrative-memory amendment (accepted 2026-07-14): Witself may meter vector
storage/search and curation records, but it performs no billable backend model
or re-embedding inference. Client model cost stays with the client under
[narrative-memory-and-curation.md](narrative-memory-and-curation.md).

## Decision

The account is the billing target, and usage rolls up by realm. One paying
customer can run many realms, each holding many named agents, and the plan
attaches to the account.

Transcript retention is the first implemented behavioral plan policy. Personal,
Professional, Team, and Enterprise default to 30, 90, 365, and indefinite
retention respectively. Account-specific admin exceptions are independent of
billing and follow `account override > plan default > missing/indefinite`; see
[transcript-retention.md](transcript-retention.md).

Messaging is an independent feature entitlement plus an independent retention
policy. Personal does not include messaging; Professional, Team, and Enterprise
include it with 90-, 365-, and 365-day message-retention defaults. Personal
still carries a 30-day message-retention policy so a downgrade can make the
mailbox unavailable immediately and then clean up on a finite grace schedule.
An explicit account override may independently enable or disable messaging and
may set finite or indefinite retention without changing plan, price,
subscription, or invoice history. Finite windows are subject to explicit
memory-provenance holds; the worker reports those holds without deleting the
memory or its source graph.

Agent email follows the same account-policy model with independent receive and
send entitlements. Personal includes neither direction and carries a 30-day
downgrade-cleanup window. Professional includes receive but not send and
retains email for 90 days. Team and Enterprise include both directions with
365-day defaults; Enterprise retention remains contract-overridable. Receive,
send, and retention can each be overridden without changing plan, price,
subscription, or invoice history. The Founder account has explicit unlimited
commercial entitlement and indefinite retention, while independent platform
limits continue to apply.

V0 should meter meaningful usage internally, but charge primarily by plan tier.
This gives Witself enough data to understand real service load without making
the first pricing model feel like nickel-and-dime metering.

The first v0 release does not need live payment collection or full subscription
management. The current checkout implements a dark provider-neutral lifecycle
and an account-scoped billing read/setup contract: status, actual provider next
charge, redacted payment-method summary, bounded invoice/payment history, and
provider-hosted setup/portal actions. Its presence does not enable live Stripe,
charging, or production webhooks. Billing, payment, crypto payment, and invoice
commands may otherwise remain contract-shaped and capability-gated while the
core product matures across both planes — the open plane (realm, agent, memory,
fact, policy, group, message, and audit) and the sealed plane (secret and TOTP).
The metered payload spans identity usage and credential usage; the sealed-plane
dimensions count events only and never carry secret or seed values.

The dark lifecycle requires every setup, upgrade, downgrade, and pending-change
cancel apply request to carry an audit reason, `confirmed: true`, and an
account-scoped `Idempotency-Key`. The control plane derives actor id and role
from `billing:manage` authority, persists a mutation receipt before the
provider call, and returns the operation id and replay state without exposing
the reason or raw retry key. Retry keys are 16-128 printable ASCII characters;
reasons are normalized single-line text from 1-512 bytes and reject control or
bidirectional-override characters. The shared `billing:preview` route accepts
no confirmation or key and performs policy/capability reads only: it creates
no receipt, provider session, customer, subscription, or account row.

Receipt attribution keeps the initiating actor and role immutable, but those
fields are not retry payload: any currently authorized `billing:manage` actor
may resume the exact request after operator or role rotation. A strict cancel
whose targeted pending state was already resolved or changed during provider
disarm completes with value-minimal `resolved`, not a false `cancelled` claim.
Target-aware provider capability checks refuse unsupported downgrade targets
before creating a receipt. The initiating account-email digest is immutable
audit metadata, not mutation semantics, so an account-email change cannot
strand an otherwise exact pending retry.

Value-minimal account tombstones bridge the crash gap between folding account
truth and completing the receipt. They retain the exact terminal result kind
for both successful and raced cancellation, so recovery never touches a
replacement pending change. If a period-end webhook resolves a downgrade after
the provider accepted it but before its response was received, the same
tombstone retains the exact operation, target, and effective time; retry
completes the original `scheduled` receipt without rescheduling anything.

The lifecycle also persists an operation id before subscription checkout,
uses that id as the provider idempotency key, and durably records authenticated
provider-event identities with a signed-body hash before folding them. Exact
mutation replay returns the original terminal result without another provider
call; changed semantics under one key fail with a non-retryable conflict, and
an exact operation held by another live worker returns a retryable in-progress
conflict. A durable per-account operation generation serializes provider work
across replicas. Lease expiry permits the same operation to resume. A different
operation cannot enter while the prior receipt exists and remains nonterminal,
because an expired client lease cannot prove that an earlier provider call
stopped. The narrow exception is an expired reservation with no receipt: the
provider boundary cannot have been reached, so a different operation may
advance. A late prior worker must revalidate its exact account generation and
claim token after creating the receipt and is superseded before any provider
call if that advance won.
Event redelivery is suppressed, reuse of an event identity with
different content or normalization fails closed, and unresolved provider-event
receipt work remains indexed for bounded reconciliation. Outbound mutation
receipts now enter one of 16 fixed global recovery shards before any provider
call is allowed. Each shard holds at most 256 operation ids and carries a CAS
rotation cursor; a five-minute pass selects up to 64 receipts across all shards
and processes them with bounded concurrency. Two control-plane replicas may run
the pass together: shard cursor CAS spreads their selected windows, while the
account-generation and receipt claims remain the authoritative duplicate-effect
fences. Index saturation or an ambiguous index write fails closed before the
provider boundary. Exact caller replay repairs missing membership.

Only a validated terminal receipt can remove an index reference. A missing,
malformed, or mismatched target remains visible and makes the batch unhealthy
without starving valid work later in the rotating shard. Completion makes the
receipt terminal first and treats index removal as retryable cleanup. Automatic
provider execution stops 23 hours after receipt creation, before the assumed
minimum 24-hour provider idempotency horizon; exact durable account evidence
may still terminalize the receipt, but otherwise an operator must reconcile it.

Receipt schema 2 also pins the server-approved execution class before provider
work: hosted setup, contact-only upgrade, self-serve upgrade, scheduled
downgrade, or pending cancellation. Self-serve upgrade and downgrade receipts
pin approved monthly cents and lowercase currency, and fail closed if the
deployed catalog no longer matches. Recovery never turns a contact request into
a purchase because availability changed. Legacy schema-1 pending receipts can
complete only from exact account-fold or tombstone evidence; an unproved legacy
receipt is never re-sent to a provider. A short durable processing lease admits
one normal folder at a time, and its immutable account/event/decision resolution
is pinned before account mutation. Crash recovery therefore reuses that
resolution without consulting provider state again. These durability controls
make retries safer; they are not a claim that every Stripe transition is ready
for production.

The hosted plan-lifecycle tick runs this global billing pass before its bounded
account-directory page and still processes that page when mutation recovery has
partial failures. Its overall success flag includes both lanes, while the
Worker advances the account-directory cursor after any structurally valid
acknowledgement so a recovery failure cannot starve later accounts. Tick and
status JSON expose a nested, value-free
`billing_mutations` summary. Authenticated
`GET /v1/plan-lifecycle/metrics` exports closed-label Prometheus counters and
gauges for batch success, selected items, terminal-index cleanup, capped scans,
last success, and the oldest pending timestamp observed in that bounded sample.
Health remains liveness-only; no account, operation, provider, plan, customer,
actor, digest, URL, or raw error becomes a metric label.

Production payment activation remains gated on an explicit operational and
security review. Account billing reads and mutations now derive distinct
`billing:read` and `billing:manage` authority from the cell-authenticated
account role; older cells that omit the role fail closed. At minimum,
activation must replace timestamp-only entitlement ordering with an
authoritative subscription projection, scope dunning to the exact managed
subscription, reconcile restored control-plane state against provider truth,
provide a real downgrade fit checker, and implement the Team usage-billing
policy. It must also compensate partial multi-subscription downgrades, define
deterministic ordering for conflicting provider events with equal timestamps,
and retain completed receipts under an explicit bounded policy without ever
deleting unresolved work. The implemented reconciler now stops unsafe automatic
provider retries before the idempotency horizon, but operations that reach that
guard still need an operator/provider-object resolution path. Hosted-action
receipts also need explicit expiry/refresh behavior rather than replaying a
stale URL forever.

This mutation surface remains dark until a rolling-writer floor is enforced:
every control-plane replica capable of receiving a billing write must preserve
the receipt/envelope version, actor, confirmation, request digest, and
idempotency fence. Old writers must be drained or blocked before billing is
enabled; route availability alone is not an activation signal. Webhook and
reconciler writers must cut over exclusively with that floor because an older
binary can bypass or erase the new fences. The account fold also needs a durable
equal-time event fence. The cross-replica mutation fence is implemented, but
production replacement still requires authoritative subscription projection
and exact provider-object cancellation rather than discovery from a bounded
list. Completed mutation receipts still need explicit bounded retention that
never removes unresolved work, plus an operator path that can terminalize a
provider-declared deterministic failure without guessing that an ambiguous
failure had no side effect. Provider secrets, webhook registration, deployment
gates, and a production rollback exercise are separate human-controlled rollout
steps.

The implemented transcript-usage slice is deliberately upstream of this
billing design: immutable `usage_events` plus hourly/daily `usage_rollups` move
with the account and power `GET /v1/usage`. No Stripe object is the usage source
of truth. Realm/account billing aggregation and conversion into plan charges
remain deferred.

## Working Plan Direction

The following table records the current product direction as of 2026-08-16. It
is a working packaging decision, not a claim that every runtime is already
enabled. Receive and send entitlements are implemented in the canonical plan
catalog and resolved cell policy, but both retain independent cell/edge rollout
gates. Committed templates remain dark by default; the exact Founder production
cohort currently has receive, outbound dispatch, and lifecycle-event delivery
enabled.
The realm, agent, active-memory, current-fact, inbound-agent-email byte, and
custom-inbound-domain values are the Phase B canonical defaults described
below; the other rows remain subject to their own implementation and rollout
gates.
The three message-rate rows are active paid-tier defaults in the `v0.0.225`
Phase-B catalog; Personal deliberately omits those keys.
The six inbound-agent-email rate keys are intentionally absent for every tier;
their independent platform breakers plus the non-optional account-wide count
and byte breakers described below apply instead.

| Capability | Personal — $0 | Professional — $30/month | Team — $250/month | Enterprise — contact us |
|---|---:|---:|---:|---:|
| Realms | 1 | 1 | 25 | Contracted |
| Agents per realm | 10 | 100 | 100 | Contracted |
| Active memories per agent | 1,000 | 10,000 | 50,000 | Contracted; 250,000 default |
| Current facts per agent | 1,000 | 10,000 | 50,000 | Contracted; 250,000 default |
| Transcript retention | 30 days | 90 days | 365 days | Configurable, including indefinite |
| Secrets per agent | 0 | 100 | 250 | 1,000 |
| Agent messaging | Disabled; 30-day downgrade cleanup | Enabled; retained 90 days | Enabled; retained 365 days | Enabled; retained 365 days by default, contract override |
| Agent message sends per rolling minute | Not applicable | 30 | 120 | 600 |
| Agent message deliveries per realm per rolling minute | Not applicable | 500 | 5,000 | 25,000 |
| Agent message deliveries per recipient per rolling minute | Not applicable | 60 | 300 | 1,000 |
| Receive agent email | No | Included; retained 90 days; safety breakers apply | Included; retained 365 days; safety breakers apply | Enabled; retained 365 days by default, contract override; safety breakers apply |
| Raw MIME and attachment retention | None stored | 90 days | 365 days | Configurable, including indefinite |
| Maximum raw email size | 0 (email disabled) | 10 MiB | 25 MiB | Contracted; 25 MiB default |
| Retained attachment storage per account | 0 | 5 GiB | 100 GiB | Contracted; 100 GiB default |
| Send agent email | No | No | Included | Included |
| Permanent Realm ID email address | Reserved; delivery disabled | Active on `witmail.net` | Active on `witmail.net` | Active on `witmail.net` |
| Realm email aliases | 0 | 0 | 1 active alias per realm | Contracted; 3 active aliases per realm by default |
| Custom inbound email domains | No | No | 1 per account | Contracted; disabled until an explicit account limit is set |

The permanent Witself-provided address format is
`agent-name.realm-id@witmail.net`. It is reserved on every plan but Personal
cannot receive mail. Team and Enterprise can add, rather than replace it with,
`agent-name.realm-email-alias@witmail.net`. Custom domains remain a separate
feature: `agent-name.realm-email-alias@customer-domain`. A realm label remains
part of the address on custom domains.

The retired `agent-mail.witwave.ai` pilot domain is not another plan benefit.
Compatibility there is limited to canonical local parts that were actually
issued before retirement. No plan may request a new legacy-domain canonical
address or alias; all new Witself-managed addresses use `witmail.net`.
This matrix and `witself plan list` describe catalog entitlements, not service
readiness. `witself plan status` describes the account's effective policy after
any audited override; it also does not prove that an edge route or cell cohort
is live. Canonical inventory and delivery remain behind independent
default-off gates, and custom-domain request, verification, routing, provider
onboarding, and delivery are all still dark.
The [Feature Status](feature-status.md) scorecard is the authoritative progress
view for those separately gated capabilities.

In this table, an included feature does not imply unbounded throughput or a
per-message charge. Message rate values and agent-email safety breakers are
shared rolling one-minute GCRA/token-bucket-equivalent budgets, not wall-clock
minute buckets. Inbound hostile traffic must not create recipient charges.
"Included" confirms catalog entitlement to outbound agent email, not that the
worker or provider adapter is active. Its commercial allowance and overage
treatment remain to be decided. Independent platform rate breakers still
apply. "Contracted" means the quantity or policy is negotiated for the
Enterprise account.

The two agent-email byte allowances are hard limits in the resolved account
snapshot, not retention policies:

- `agent_email_max_raw_bytes` is the maximum raw-MIME size of each inbound
  message. Personal resolves to `0`, Professional to `10,485,760`, and Team
  and Enterprise to `26,214,400` bytes.
- `agent_email_attachment_storage_bytes` is one account-wide pool for retained
  attachment-bearing MIME. Personal resolves to `0`, Professional to
  `5,368,709,120`, and Team and Enterprise to `107,374,182,400` bytes.

A missing limit key means unlimited and zero remains a real cap. The ordinary
audited account limit override can replace either default without changing the
plan, price, subscription, or invoice history. Explicit unlimited is stored as
an override and represented by omitting the key from the resolved cell
snapshot. The Founder account uses that explicit-unlimited override for
`agent_email_attachment_storage_bytes`; its per-message raw-MIME maximum
remains 25 MiB.

Capacity activation is intentionally split across two releases. Phase A
shipped the schema-81 projection, cell enforcement, status surfaces, and
audited admin override support while both catalog keys remained absent
(temporarily unlimited). Rows accepted by a rolling old replica were
normalized but marked unaccounted, so its pre-existing shared account lock was
never upgraded inside a trigger. After every old process was gone, the control
plane was upgraded and the Founder attachment-storage override was written and
verified.

Phase B first rolls the schema-82 post-convergence migration that promotes any
compatibility rows and rebuilds exact counters under account-first `NOWAIT`
fences. Only after every target cell reports schema 82 does the Phase-B catalog
in `web/plans/plans.json` activate the exact finite values above. Republishing
that catalog and reconciling every hosted account completes activation. The
Founder account's explicit-unlimited attachment-storage override must remain
present before and after reconciliation. Personal email remains disabled
throughout; the edge Worker retains its independent 25 MiB technical ceiling
during both phases.

### Agent-email cell storage safety boundary

Account byte allowances and age retention are commercial policy. They do not
bound the sum of several accounts on one cell, and an explicit-unlimited Founder
override intentionally removes those commercial caps. The live schema-91
deployment therefore has a separate, non-billable platform safety boundary that
no plan or account override can raise or bypass.

One transactionally maintained logical ledger covers all retained inbound and
outbound roots, inbound deliveries, outbound provider-event receipts, and
recipient suppressions in the cell. The default root-admission boundary is
3 GiB of charged storage or 25,000 inbound-plus-outbound roots. The default hard
boundary is 4 GiB or 100,000 counted rows. Every counted row is charged 8 KiB of
fixed overhead plus retained immutable identity and customer-content fields.
The fixed charge absorbs bounded mutable lifecycle metadata; inbound-delivery
and outbound claim IDs have independent 128-byte database caps. Claim, release,
and terminalization updates are charge-neutral even at the hard boundary. The
lower root threshold leaves 75,000 rows—three lifecycle children per fully
admitted root on average—for lifecycle headroom. Repeated provider events are
still bounded by the hard cap, not guaranteed unlimited capacity. Database
triggers apply the hard threshold to all positive writes, including rolling-old
binaries, imports, and direct maintenance. Deletes and cascades release their
exact logical charge, so retention can reopen admission without waiting for
PostgreSQL's physical file size to shrink.

At the boundary, an enabled inbound delivery returns HTTP 507 with the closed
`storage_full` verdict. The receive edge converts that refusal into a sanitized
permanent SMTP rejection instead of discarding the message or asking the sender
provider to retry forever. A new outbound message returns HTTP 507 with
`agent_email_storage_full`, `retryable: false`, and no `Retry-After`. These are
platform refusals, not billable overages, plan downgrades, or changes to the
Founder's indefinite retention. The v0.0.253/schema-91 production cell has this
ledger live, and a point-in-time scrape verified
`witself_agent_email_cell_storage_metrics_up 1` plus all seven usage/threshold
gauges. Database triggers remain authoritative without a
collector. Continuous Prometheus scraping, PVC metrics collection, Alertmanager
routing, and a tested external receiver are not yet installed end to end.
Those controls, logical-ledger and physical-PVC alerts, and provider
backpressure block cohort expansion.

### Agent-email ingress rate breakers

Inbound agent email has six account-adjustable rolling one-minute safety limits
plus two non-optional account-wide breakers across all realms. They are
service-protection controls, not customer usage or overage dimensions:

| Resolved limit key | Bucket scope | Platform GCRA refill rate |
|---|---|---:|
| `agent_email_received_per_sender_minute` | One normalized, unverified envelope-sender and enrolled-recipient pair | 30 messages |
| `agent_email_received_per_recipient_minute` | One receiving agent | 300 messages |
| `agent_email_received_per_realm_minute` | One realm | 5,000 messages |
| `agent_email_received_bytes_per_sender_minute` | The same sender/recipient pair | 64 MiB |
| `agent_email_received_bytes_per_recipient_minute` | One receiving agent | 512 MiB |
| `agent_email_received_bytes_per_realm_minute` | One realm | 4 GiB |
| Platform-only (not a resolved plan key) | All realms in one account | 5,000 messages/minute; burst 100 |
| Platform-only (not a resolved plan key) | Raw MIME across all realms in one account | 1 GiB/minute; burst 64 MiB |

All six keys are intentionally absent from every current plan in
`web/plans/plans.json`: there is no commercial tier default yet. Missing or
explicit-unlimited removes only a commercial cap; it never disables the
independent platform maximum. The generic audited account override accepts a
finite value only at or below that key's platform maximum, so an administrator
can lower a breaker for one account but cannot raise or bypass it. Clearing the
override restores plan inheritance; with the current catalog that again means
the platform maximum. Neither account-wide breaker is a plan key or override
surface. Personal remains protected first by its disabled receive entitlement.

The sender label is not an authenticated identity. The cell hashes the
normalized envelope sender together with the exact enrolled recipient and
keeps that hash only in operational bucket state. Recipient, realm, and account
breakers remain necessary because an external sender can spoof or rotate
envelope addresses or multiply realms. All eight debits and the message insert
share one PostgreSQL transaction, so a refusal rolls back earlier debits and
stores no message. It
returns the exact value-free `rate_limited` verdict with HTTP 429 when waiting
can make the debit succeed; the edge surfaces only a sanitized temporary
provider result. A zero cap or a single message larger than its effective byte
bucket cannot become admissible by waiting, so the cell instead returns the
existing value-free permanent verdict and the edge rejects it without retry.

Accepted and refused attempts affect only these operational safety buckets.
They emit no billable `email_received` usage event and create no inbound
overage. The owning cell's PostgreSQL limiter is the sole authoritative
admission point across replicas. Keeping admission below the feature check
also preserves Personal's accept-and-drop behavior without requiring any edge
reconfiguration when an account changes plans.

These are GCRA rates, not wall-clock counters. The new account aggregates have
smaller burst tolerances than their refill rates, so one rolling minute is
bounded by 5,100 messages and 1,088 MiB. Existing sender, recipient, and realm
lanes retain their established full-rate burst tolerance.

### Agent-email outbound rate breakers

Outbound email has two account-adjustable rolling one-minute safety limits plus
three non-optional platform-only breakers. They are admission and
sender-reputation protections; provider activation remains a separate cohort
decision and the resulting observations are not customer billing:

| Resolved limit key | Bucket scope | Refill window | Platform refill rate / burst |
|---|---|---:|---:|
| `agent_email_sent_per_agent_minute` | One sending agent | 1 minute | 30 messages |
| `agent_email_sent_per_realm_minute` | Aggregate senders in one realm | 1 minute | 300 messages |
| Platform-only (not a resolved plan key) | All realms in one account | 1 minute | 1,000 messages |
| Platform-only (not a resolved plan key) | All realms in one account | 24 hours | 10,000 messages / 1,000 |
| Platform-only (not a resolved plan key) | One normalized recipient across the account | 24 hours | 100 messages / 10 |

Both keys are intentionally absent from every current plan. Missing or
explicit unlimited removes only a commercial cap; it never removes the
platform maximum. The generic audited account limit override may lower either
value, but rejects a finite value above the corresponding maximum. The Founder
account's unlimited commercial entitlement therefore still resolves through
the same 30/300 service-protection rates plus the 1,000-per-account-minute,
10,000-per-account-day, and 100-per-recipient-day refill rates. The daily burst
tolerances bound any rolling day to 11,000 per account and 110 per recipient.
The recipient bucket stores only an account-domain-separated SHA-256
identifier, never the address.

The owning cell admits all five buckets and inserts the unique durable outbox
row in one PostgreSQL transaction. An exact idempotent replay returns the
existing row without consuming another debit. Reusing a key for changed send
semantics conflicts. Immediately before a provider attempt, the worker applies
an independent dispatch lane with the same five platform maxima so retries and
multiple replicas cannot burst the provider. Agent and realm operator kill
switches, hard-bounce/complaint recipient suppression, and the platform breakers
remain effective regardless of plan or account override.

### Messaging availability and retention

The resolved cell snapshot uses the feature key `messaging` and the behavioral
policy key `message_retention_days`. Their meanings are deliberately separate:

- `messaging_entitlement_version: 1` activates explicit entitlement
  enforcement for the snapshot;
- absence of `messaging` disables all message send, receive, mailbox, reply,
  processing, and open-request operations;
- presence of `messaging` enables those operations;
- a finite `message_retention_days` value is the age window for whole inactive
  message threads; and
- an absent `message_retention_days` key means explicit indefinite retention.

The client integration is installed once. Plan changes and account overrides do
not add or remove tools, rewrite managed instructions, or require runtime
reinstallation. The control plane pushes a new monotonic, hash-acknowledged
account snapshot to the cell. The cell is the authoritative enforcement point,
while capability/checkpoint hints let installed clients avoid useless retries.

If messaging is disabled, a message operation fails before content is stored
with a stable, non-retryable `feature_not_enabled` refusal for feature
`messaging`. No recipient notification message is created. Only value-free
refusal telemetry may be retained. Messages that already exist become
inaccessible immediately and are then eligible for cleanup under the effective
retention policy.

Account policy resolution is `account override > effective-plan default`.
Availability and retention each have their own attributed override and their
own clear-to-inheritance operation. Administrator mutations are
compare-and-swap persisted, append an actor/reason/timestamp audit transition,
and advance the same desired/applied snapshot revision fence used by plan
changes. The operator surface is:

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

The matching authenticated control-plane resources are
`/v1/admin/accounts/{id}/messaging` and
`/v1/admin/accounts/{id}/message-retention` with `GET`, `PUT`, and `DELETE`.
Owner plan status exposes inherited and effective values but never admin
attribution or audit history.

Messaging throughput is represented by three ordinary integer limit keys in
the same resolved account snapshot:

- `message_sent_per_agent_minute` charges one unit for each new logical send;
- `message_delivered_per_realm_minute` charges the resolved fan-out count to
  the sender's realm; and
- `message_delivered_per_recipient_minute` charges one unit to each resolved
  recipient.

Personal omits all three keys because messaging is disabled. The `v0.0.225`
Phase-B catalog activates Professional `30 / 500 / 60`, Team `120 / 5,000 /
300`, and Enterprise `600 / 25,000 / 1,000` in the order above.
The independent platform ceilings are `2,000 / 100,000 / 5,000`. A send
evaluates all applicable budgets in the same PostgreSQL transaction used to
insert the message and delivery rows. Exact idempotent replay is resolved
before charging. Admission debits every resolved target, while the durable
`message_delivered` usage event counts only targets whose committed delivery
state is `delivered`. A fan-out either commits the admission debits, one send,
and the complete delivery set, or stores nothing and consumes nothing.

The existing generic account limit override surface changes these effective
plan limits without changing plan or price:

```sh
witself-admin account limit-override get \
  --account ACCOUNT_ID --dimension message_sent_per_agent_minute --json
witself-admin account limit-override set \
  --account ACCOUNT_ID --dimension message_sent_per_agent_minute --max 60 \
  --reason "Temporary account-specific messaging allowance"
witself-admin account limit-override set \
  --account ACCOUNT_ID --dimension message_sent_per_agent_minute --unlimited \
  --reason "Founder messaging plan allowance is unlimited"
witself-admin account limit-override clear \
  --account ACCOUNT_ID --dimension message_sent_per_agent_minute \
  --reason "Return to the plan default"
```

The same commands accept the two delivery keys. `--unlimited` removes the plan
cap from the resolved snapshot; it does not bypass the platform ceiling.

Activation uses two phases. Phase A shipped schema 83, the cell-side
store/API/client/metrics implementation, and the control-plane/edge allow-list,
while leaving all three keys absent from `web/plans/plans.json`; platform
ceilings protected the service while no finite plan default was active. After
both cells and the control plane converged, operators set and verified
explicit-unlimited Founder overrides for all three keys with equal
desired/applied snapshot revisions. Release `v0.0.225` is Phase B: its catalog
activates Professional `30 / 500 / 60`, Team `120 / 5,000 / 300`, and
Enterprise `600 / 25,000 / 1,000`. Deploy that activation from a clean exact
`v0.0.225` checkout and update both catalog surfaces:

```sh
export EMAIL_DIRECTORY_KV_ID="${EMAIL_DIRECTORY_KV_ID:?set the dedicated 32-character agent-email KV namespace id}"
npm run deploy:plans
npm run deploy
```

The control-plane container embeds `web/plans/plans.json`; deploying only the
public plans Worker does not update account snapshots. Run lifecycle
reconciliation only after both deployments succeed, then verify the effective
values and Founder overrides. The independent platform ceilings remain
effective after activation.

Rate-bucket debt is cell-local operational state and is not exported with an
account. A cell evacuation therefore starts fresh rate buckets on the target,
while the immutable message usage events and their checked rollups do migrate.

Activation is cell-first. A new cell treats an already-applied snapshot that
does not contain `messaging_entitlement_version: 1` as a legacy
pre-entitlement snapshot and keeps messaging enabled. Once the control plane
applies a snapshot containing that marker, the explicit `messaging` feature
becomes authoritative. The dedicated marker remains present when
`message_retention_days` is absent for explicit indefinite retention. This
avoids both disabling existing mailboxes between the cell rollout and catalog
reconciliation and accidentally re-enabling a disabled account whose retention
is indefinite.

Before catalog reconciliation is activated, the founder account receives an
explicit indefinite message-retention override and an explicit enabled
messaging override. The indefinite override is applied first, so the first new
founder snapshot never temporarily inherits Enterprise's 365-day default.
After any new-policy snapshot is accepted, pre-feature cell or control-plane
rollback is prohibited: an old cell ignores the entitlement and an old control
plane can recompute a legacy snapshot without the new policy.

### Agent-email availability and retention

The resolved cell snapshot uses independent features `agent_email_receive` and
`agent_email_send`, policy `agent_email_retention_days`, and rollout marker
`agent_email_entitlement_version: 1`. Personal has neither feature;
Professional has receive only; Team and Enterprise have both. A marked
snapshot without receive accepts and discards verified inbound relay traffic
before storage. A send/reply/list/show operation without send fails before
queueing with stable `feature_not_enabled` for `agent_email_send`. Retention
remains independent so records already stored before a downgrade can age out
on schedule. An absent retention key means explicit indefinite retention.

The integration stays installed across plan and override changes. The
administrator surface is:

```sh
witself-admin account email-receive get --account ACCOUNT_ID
witself-admin account email-receive set --account ACCOUNT_ID --enabled --reason "..."
witself-admin account email-receive set --account ACCOUNT_ID --disabled --reason "..."
witself-admin account email-receive clear --account ACCOUNT_ID --reason "..."

witself-admin account email-send get --account ACCOUNT_ID
witself-admin account email-send set --account ACCOUNT_ID --enabled --reason "..."
witself-admin account email-send set --account ACCOUNT_ID --disabled --reason "..."
witself-admin account email-send clear --account ACCOUNT_ID --reason "..."

witself-admin account email-retention get --account ACCOUNT_ID
witself-admin account email-retention set --account ACCOUNT_ID --days 365 --reason "..."
witself-admin account email-retention set --account ACCOUNT_ID --indefinite --reason "..."
witself-admin account email-retention clear --account ACCOUNT_ID --reason "..."
```

The matching authenticated resources are
`/v1/admin/accounts/{id}/email-receive`,
`/v1/admin/accounts/{id}/email-send`, and
`/v1/admin/accounts/{id}/email-retention`, each with `GET`, `PUT`, and
`DELETE`. These overrides change effective behavior, not the billed plan or
price. Owner plan status exposes only inherited and effective values.

Within an entitled account, value-free `GET`/`PATCH
/v1/agents/{agent}/email-send` and `GET`/`PATCH
/v1/realms/{realm}/email-send` controls are independent kill switches.
Effective send requires an active account, the account feature, a live agent,
and both layers enabled. A suspended account may inspect or disable a layer as
a harm-reducing action, but it cannot enable one.

Before catalog reconciliation, the Founder account receives an explicit
indefinite email-retention override plus explicit enabled receive and send
overrides. This keeps its first marked snapshot from temporarily inheriting
Enterprise's finite default. It also receives and verifies explicit-unlimited
commercial overrides, including `agent_email_attachment_storage_bytes` and
the outbound rate dimensions. Those unlimited values do not bypass the 25 MiB
inbound transport ceiling; the 5,000-message/1-GiB account-wide inbound refill
rates and 100-message/64-MiB bursts; or any outbound 30-per-agent-minute,
300-per-realm-minute, 1,000-per-account-minute, 10,000-per-account-day, or
100-per-recipient-day breaker. They also do not bypass the live schema-91
cell's 3-GiB/25,000-root logical admission boundary or its
4-GiB/100,000-counted-row hard boundary.

Production keeps two agent-email retention workers in enforcement mode even
while the Founder policy is indefinite. Batch 100, a
one-minute interval, a two-minute timeout, a 32-productive-pass ceiling, and a
48-total-pass ceiling allow one sparse 16-lane sweep without creating an
unbounded drain loop. One replica can scan at most 3,200 rows and delete at most
1,024 MiB of raw MIME per enforce attempt; two replicas can scan at most 6,400
rows and delete at most 2,048 MiB. Maximum-sized 25 MiB rows yield 800/1,600 MiB
because each database batch fits only one. Those are work ceilings, not
database-throughput guarantees, reserved inbound shares, or a multi-account
cell bound. Schema 91's live independent database-triggered ledger supplies
that cell bound; the retention sweep does not. Durable
per-account kind rotation prevents one continuously full kind from starving the
others over repeated visits. A finite plan policy takes effect without a
worker-mode change. A wider cohort remains blocked until continuous logical/PVC
alerts reach a tested external receiver and provider-wide outbound backpressure
is in place.

Rollout is cell-and-edge first: deploy a cell that understands the marked
snapshot and an agent-email edge Worker that accepts the cell's
`feature_disabled` verdict before the control plane publishes or reconciles
the new catalog. After any marked snapshot is accepted, the cell, control
plane, and edge Worker share a forward-only compatibility boundary. Rolling
the edge Worker back to a pre-entitlement version would treat an intentional
accept-and-drop verdict as a transient relay failure and cause sender retries.

### Realm email aliases

The canonical `agent-name.realm-id@witmail.net` identity is permanent and is
reserved even when inbound email is disabled. The resolved feature
`agent_email_realm_alias` and limit
`agent_email_realm_aliases_per_realm` govern only additional memorable labels.
Personal and Professional have a zero alias limit; Team has one per realm;
Enterprise defaults to three per realm and can be contracted or explicitly
overridden. As with other numeric plan dimensions, an absent/null resolved
limit means unlimited only when the feature itself is enabled; zero is a real
cap. The feature does not bypass `agent_email_receive`: Personal keeps
its canonical reservation but accepts no mail.

The Founder account is an Enterprise-classified account with an audited
explicit-unlimited override for this dimension. Set and verify that override
before the catalog containing the finite Enterprise default is published:

```sh
witself-admin account limit-override set \
  --account "$FOUNDER_ACCOUNT_ID" \
  --dimension agent_email_realm_aliases_per_realm \
  --unlimited \
  --reason "Founder realm email aliases are unlimited"
witself-admin account limit-override get \
  --account "$FOUNDER_ACCOUNT_ID" \
  --dimension agent_email_realm_aliases_per_realm
```

Activation is forward-only and deliberately split across two releases. The
Phase-A release rolls schema 85 to every target cell and deploys the
control-plane registry plus Email Worker with the alias feature and limit still
absent from `web/plans/plans.json`,
`CP_REALM_EMAIL_ALIAS_ACTIVATION_ENABLED` absent, and
`REALM_EMAIL_ALIAS_DELIVERY_ENABLED=false`. After that runtime accepts the new
dimension, write and verify the Founder override and deploy the edge directory
binding plus shared fallback credential. A later catalog-only Phase-B release
adds the Personal 0, Professional 0, Team 1, and Enterprise 3 defaults and
publishes the public plan catalog. This prevents the enabled plan lifecycle
from applying Enterprise's finite default to the Founder account before the
new override can exist. The activation gate remains
off until full-coverage SMTP delivery, canonical-route backfill, and immediate
route republishing across account moves have each passed a reviewed live
canary. Publishing the plan entitlement by itself never enables alias requests
or delivery; both exact-`true` gates are required, and the edge gate can
immediately tempfail alias delivery fleet-wide without affecting canonical
Realm ID addresses.

The globally scarce alias namespace is authoritative in the control plane.
Customers submit requests, while a Witself platform administrator reviews
them and manages the versioned reserved-name registry. Customer account and
realm roles cannot modify or override reserved entries. The initial protected
set includes Witself and Witwave brands, mail/protocol terms, operational role
addresses, and normalized confusable forms. Approved state is projected to the
owning cell first and then to the edge directory. The edge KV is never used to
decide ownership.

The commercial per-realm allowance is not a review-queue allowance. A separate
technical guard permits at most eight open (`pending_review` or
`provisioning`) requests per realm and 64 per account, even when the resolved
plan limit is explicitly unlimited. Durable membership-backed counters track
open requests and allocated customer aliases without scanning an unlimited
realm. Approval does not release its open slot until cell and edge projection
finish; rejection, terminal abort, and completed approval release it exactly
once, and retirement releases the allocated slot exactly once. Runtime
configuration may lower these two ceilings but cannot raise the compiled
maxima. A ceiling refusal uses the stable, value-free
`technical_pending_limit_reached` error with its `realm` or `account` scope and
effective limit; it does not consume durable audit space. Exact membership and
aggregate integrity checks make ordinary transitions fail closed on drift. A
platform administrator can request an idempotent, audited recovery that fences
count-changing writes, clears only derived counter state in bounded pages,
rebuilds from canonical claims, and verifies a second bounded pass before
reopening writes. This recovery is operational maintenance and is not gated by
the customer alias activation switch.

An activated alias survives loss of entitlement as a reserved assignment: it
receives during a 30-day downgrade grace period, then becomes suspended. A
later upgrade reactivates it without reinstalling an integration. Activated,
suspended, retired, and tombstoned aliases are never reassigned to another
account or realm. Newly reserved words do not automatically revoke existing
active aliases; they create an explicit platform-admin conflict for review.

### Custom inbound email domains

Organization-owned inbound domains are a separate account-level entitlement,
not another realm alias. The feature key is `agent_email_custom_domain`; the
hard-cap key is `agent_email_custom_domains_per_account`. The Phase-B canonical
catalog gives Personal and Professional a zero cap and no feature, Team the
feature with one domain per account, and Enterprise the feature with an
explicit zero default. Its negotiated quantity must be installed as an audited
account limit override before a request is accepted. A missing effective limit
means unlimited only while the feature is enabled; zero remains a real cap.

The implementation remains deliberately dark. Customer request creation needs
the exact-`true` runtime-only
`CP_AGENT_EMAIL_CUSTOM_DOMAIN_REQUESTS_ENABLED` gate, the requesting account in
`CP_AGENT_EMAIL_CUSTOM_DOMAIN_REQUEST_ACCOUNT_ALLOWLIST`, and exact-`true`
`CP_AGENT_EMAIL_CUSTOM_DOMAIN_AUTHORITY_READY` after journal and recovery
acceptance. DNS observation has the separate exact-`true`
`CP_AGENT_EMAIL_CUSTOM_DOMAIN_VERIFICATION_ENABLED` gate. All four controls are
absent from committed release configuration.

Each request receives a stable TXT challenge that expires after seven days.
Several accounts may hold pending challenges for the same domain: an
unverified request temporarily consumes one account allowance and one of the
eight technical open-request slots, but does not create the global domain
allocation or a permanent tombstone. Rejection, pre-proof retirement, and
expiry release that reservation. The first exact TXT proof wins the atomic,
permanent allocation; only then does retirement retain a non-reusable domain
tombstone.

When verification is enabled, a new pending challenge is due immediately;
verified ownership is rechecked every 24 hours. Authoritative absence suspends
availability and retries hourly; a temporary resolver failure preserves the
last authoritative result and retries after 15 minutes. A plan downgrade
suspends excess pending requests immediately, gives excess verified domains a
30-day grace period, then suspends them. Restoring entitlement removes plan
suspension without a client reinstall. Account lifecycle fences suspend during
move/archive work, republish on resume, and permanently retire on account
close.

The deployed v0.0.238 verifier does not create unbounded storage when a
scheduled check's durable evidence outcome is unchanged. Each request has at
most one overwrite-only, journal-local verification refresh plus one derived
due entry. The refresh advances the effective clocks, retry counter, recursive
TTL, and schedule shown by request list/show without mutating the authority
request/allocation, audit history, journal head, capacity count, or R2 stream.
A first or changed outcome and every newly executed manual check remain audited
authority commits. Recovery discards the local refresh and conservatively
resumes from the last journaled request. Through that deployed release, none of
these states writes a cell or edge route.

The schema-88 dark foundation adds routing with no new commercial
dimension. A custom route always reuses one existing realm-alias claim and has
the form `agent.realm-alias@customer-domain`; it does not create another alias
or consume extra domain allowance. The cell projection is an exact join of the
verified domain request/allocation, realm-alias claim, realm, and account, with
all source revisions preserved. One verified domain may therefore support
several already-authorized realm aliases while `(domain, realm_label)` remains
globally unambiguous.

Routing is independently disabled unless
`CP_AGENT_EMAIL_CUSTOM_DOMAIN_ROUTING_ENABLED=true` and the account appears
exactly in `CP_AGENT_EMAIL_CUSTOM_DOMAIN_ROUTING_ACCOUNT_ALLOWLIST`. Delivery
has the separate exact-true `AGENT_EMAIL_CUSTOM_DOMAIN_DELIVERY_ENABLED` edge
gate. All three are absent from committed configuration. They do not alter the
domain entitlement or limit, and request creation, verification, routing,
provider onboarding, and delivery remain separate activation decisions. This
slice performs no DNS, MX, Email Routing, provider, customer-zone, or live-mail
mutation; only fake/offline acceptance is authorized.

The control-plane authority is protected by a separate create-only R2 journal.
After the existing registry has been bootstrapped and a sealed empty-target
restore drill succeeds, operators enable only
`CP_AGENT_EMAIL_DOMAIN_AUTHORITY_JOURNAL_ENABLED=true`; this does not enable
the customer request feature. Journal maintenance and recovery currently fail
closed above 10,000 authority keys. Requests, first or changed observations,
manual checks, plan/lifecycle fences, audit history, and permanent
verified-domain tombstones grow that global authority over time, so the bound
is an explicit request-gate activation blocker rather than a plan limit.

Activation follows the established two-phase limit rollout. Phase A adds the
closed feature/limit vocabulary, control-plane Durable Object, APIs, and admin
client while intentionally leaving both keys absent from
`web/plans/plans.json`. Roll Phase A to every cell and the control plane first,
then write and verify the Founder account's explicit-unlimited limit override.
This Phase-B catalog release adds the Personal 0, Professional 0, Team 1, and
Enterprise 0 values plus the Team/Enterprise feature. The split keeps an old
cell from receiving an unknown key and prevents the Founder snapshot from
briefly inheriting Enterprise's zero default. The request gate stays off in
both phases. For this catalog-only promotion, deploy the Phase-B control-plane
container first, wait for a complete plan-lifecycle pass, and verify the
Founder explicit-unlimited snapshot before publishing the public plans Worker.
Phase-A cells already understand the new dimension, so they do not need a
Phase-B image rollout when the second release changes only catalog bytes,
catalog tests, and rollout documentation.

The commercial allowance is reserved by pending requests and becomes an
allocation only after exact ownership proof. A separate technical ceiling
allows at most eight open requests per account, including explicitly unlimited
accounts. Terminal request records remain durable for audit, but an unverified
terminal request does not reserve the name. Only a verified allocation becomes
a permanent, non-reassignable tombstone on retirement; a later transfer policy
must explicitly preserve that tenant boundary.

An Enterprise contract can be installed without changing the account's plan:

```sh
witself-admin account limit-override set \
  --account "$ACCOUNT_ID" \
  --dimension agent_email_custom_domains_per_account \
  --max 3 \
  --reason "Contract includes three custom inbound domains"
witself-admin account limit-override get \
  --account "$ACCOUNT_ID" \
  --dimension agent_email_custom_domains_per_account
```

That override changes only the account allowance. It does not enable the dark
request gate and cannot activate DNS or mail delivery.

Before Phase B, preserve the Founder exception explicitly:

```sh
witself-admin account limit-override set \
  --account "$FOUNDER_ACCOUNT_ID" \
  --dimension agent_email_custom_domains_per_account \
  --unlimited \
  --reason "Founder custom inbound domains are unlimited"
witself-admin account limit-override get \
  --account "$FOUNDER_ACCOUNT_ID" \
  --dimension agent_email_custom_domains_per_account
```

### Realm and agent limits

`realms` is the maximum live realm count for an account.
`agents_per_realm` is the maximum live agent count independently within each
live realm. A missing key means unlimited, while zero is a real cap. Soft-deleted
realm and agent tombstones do not consume capacity, although their names remain
reserved. Lowering either maximum never deletes or disables existing resources;
it blocks only a later create until live usage is below the maximum again.
Account import remains exempt so migration and disaster recovery can preserve
an over-limit account exactly.

The previously deployed `agents` key retains its original account-wide meaning
for old snapshots and archives. It is not reinterpreted. Cells accept and
enforce both keys during migration; if both are present, both gates apply.
Ordinary creates serialize on the stable account plan row, so concurrent
requests reaching different server replicas cannot overshoot either maximum.
Duplicate or tombstone-reserved names and invalid realms are resolved before
the capacity gate so callers are not incorrectly told to upgrade.

Catalog activation uses two phases:

1. Phase A deploys cells and the control plane that understand
   `agents_per_realm`,
   retain legacy `agents`, expose the audited override through the strict edge
   allow-list, and leave `web/plans/plans.json` unchanged.
2. Phase B promotes the working table's values in a separate catalog release
   after every cell is converged and founder explicit-unlimited overrides exist
   for `realms`, `agents`, and `agents_per_realm`; every hosted account is then
   reconciled.

The compatibility Phase A implementation did not modify
`web/plans/plans.json`; this Phase B catalog release activates the defaults.
After `agents_per_realm` audit data exists, Phase A is the control-plane rollback
floor: an older control plane rejects the unknown audited dimension. Old cells
reject a newly pushed snapshot containing the closed key, but rolling an old
cell binary back onto a snapshot already stored by Phase A can ignore the new
key and fail open. Pre-Phase-A cell and control-plane rollback is therefore
prohibited once the founder override or any new snapshot is written.

Phase B keeps a derived legacy `agents` account-wide compatibility ceiling next
to `agents_per_realm`: 10 for Personal, 100 for Professional, and 2,500 for
Team. Those totals equal each plan's maximum realms multiplied by agents per
realm, so a Phase A cell enforces the product matrix while a mistakenly rolled
back old cell still has a safe cap. Removing the legacy key is a separate future
migration after old cell artifacts can no longer be selected.

Realm and agent create routes retain their existing non-retryable HTTP 403
contract in this compatibility slice. The error text uses the customer-facing
phrase “agents per realm”; the closed `agents_per_realm` key remains available
as typed internal detail for bounded metrics and administration. A unified
structured billing-limit status/error surface remains separate work.

The `stored_secret` allowance is stored in the account's resolved plan snapshot
but enforced independently for each owner agent. One retained top-level secret
bundle consumes one slot, regardless of its field count, TOTP fields, revisions,
or vault-key rotations. Active and archived secrets count; a guarded tombstone
delete frees its slot. A missing `stored_secret` key means unlimited, while zero
is a real cap. Resolution is `account override > catalog/plan default >
missing/unlimited`. An audited account override may set a finite maximum,
including zero, or explicit unlimited behavior without changing the account's
plan, price, subscription, or invoice history. An explicit-unlimited override
wins over a finite catalog default and is represented by omitting
`stored_secret` from the resolved snapshot.

Lowering the maximum never deletes existing data. An agent already at or above
its maximum keeps read, list, access, archive, restore, export, and delete
operations, but creation of another top-level secret is refused until deletion
brings retained usage below the maximum or an administrator raises the
allowance. Account import remains exempt so migration and disaster recovery can
preserve an over-limit account exactly; subsequent ordinary creates use the
current resolved maximum.

### Implemented stored-secret limit

The Phase A implementation counts `active + archived` top-level bundles with no
`deleted_at` tombstone in the authenticated owner-agent scope. Status is
available through `GET /v1/secrets:status`, `witself secret status`, and the
read-only, idempotent, value-free `witself.secret.status` MCP tool. It reports
`used`, `max`, `remaining`, `unlimited`, and `over_limit`; unlimited status uses
`null` for `max` and `remaining`. At `used == max`, `over_limit` is false but a
new create is still blocked. `over_limit` becomes true only after a maximum is
lowered below retained usage.

A refused create returns HTTP 403 with
`code: "stored_secret_limit_reached"`, `retryable: false`, and the same
value-free `limit` object. This stored-inventory code is the feature-specific
form of the generic HTTP 403, non-retryable hard-cap behavior below.
Idempotent create replay is resolved before the gate, so replaying the exact
already-completed request still succeeds when the owner is at or over the
current maximum.

Create and tombstone-delete transactions serialize on the stable owner-agent row
after the account/plan fence. This prevents concurrent requests on different
cell replicas from overshooting one agent's maximum while allowing unrelated
agents to proceed independently. `POST /v1/secrets/{secret_id}:delete`,
`witself secret delete`, and the destructive, idempotent, value-free
`witself.secret.delete` MCP tool perform an exact-row-version, retry-keyed
tombstone delete. The transaction scrubs secret metadata and deletes every
field and wrapped-DEK row, while append-only usage history, a minimal
value-free secret tombstone, the `secret.deleted` event, and mutation receipt
remain for retry and recovery bookkeeping. Ordinary list/show/access paths
exclude the tombstone and retained capacity is released. Irreversible purge of
the minimal tombstone is a separate future operation.

Migration `0067_add_secret_delete_receipts.sql` widens the receipt constraints
for `secret_delete` using add/validate/swap. Its down migration refuses to run
while any delete receipt exists because the legacy constraint cannot represent
that durable evidence. The guard runs before any constraint change, so a refusal
leaves the migration version and schema checks intact. Operational rollback
therefore requires a backup and a decision about those receipts; it must not
silently discard them. Schema-66 archives upgrade by pass-through because they
cannot contain delete receipts, and direct account import remains exempt from
the create gate so an over-limit encrypted archive round-trips unchanged.

Catalog promotion is intentionally two-phase:

1. Deploy converged control-plane and cell code that understands overrides,
   resolves `stored_secret`, and enforces the owner-agent gate while leaving
   the canonical catalog unchanged.
2. Only after convergence, update and publish the canonical catalog as a
   separate rollout. Verify that the founder account has an explicit-unlimited
   override both immediately before and after catalog promotion; do not rely on
   a plan label or a missing catalog entry to make the founder unlimited.

The stored-secret compatibility Phase A did not modify
`web/plans/plans.json`; its catalog promotion was a separate release.

### Implemented current-fact limit

`stored_fact` is an account plan/override dimension enforced independently for
each owner agent. It counts resolved, non-deleted current facts across all of
the agent's subjects. Assertion versions, candidates, subjects, aliases,
evidence, usage history, tombstones, and deleted facts do not consume another
slot. A missing key means unlimited and zero is a real cap. Resolution is
`account override > catalog/plan default > missing/unlimited`; an audited
explicit-unlimited override omits the key from the resolved cell snapshot
without changing the account's plan, price, subscription, or invoice history.

Phase A exposes `GET /v1/facts:status`, `witself fact status`, the read-only
idempotent `witself.fact.status` MCP tool, `self.show.fact_capacity`, actionable
hook hydration, and the local dashboard. The shared value-free projection is
`used`, nullable `max` and `remaining`, `unlimited`, `near_limit`, `at_limit`,
and `over_limit`. For a finite maximum, `near_limit` starts at 90 percent,
rounded up. It contains no ids, subjects, predicates, fact values, assertion
history, or candidate data.

Setting an already-current fact preserves the count and remains available at
the cap. Creating or explicitly recreating another current address adds one;
confirming a candidate adds one only when its subject/predicate address is not
already current. A refused count-growing write returns HTTP 403 with
`code: "stored_fact_limit_reached"`, `retryable: false`, and the current
value-free `limit` object. Exact idempotent replay is resolved before the gate.
Reads, history, export, existing-fact updates, and permanent deletion under its
separate direct-user authorization rule remain available. Capacity never
authorizes deleting or rewriting an unrelated fact to make room.

Catalog activation follows the same compatibility discipline as active memory:

1. Phase A deploys the cell counter/gate, client surfaces, control-plane
   `stored_fact` override handling, and strict edge allow-list while leaving
   `web/plans/plans.json` unchanged. Migration
   `0076_add_active_fact_count.sql` performs only the short column DDL so its
   `ACCESS EXCLUSIVE` lock is not held across a mature fact-table scan.
   `0077_backfill_active_fact_count.sql` then backfills nonzero canonical
   counts and validates the range guard with read-compatible locks, leaving
   agent/auth reads available. A pre-Phase-A pod can still write after that
   initial backfill without
   maintaining the derived counter, so every account must remain
   missing/unlimited and no finite override may be set during the mixed-writer
   rollout.
2. Set and verify the Founder account's explicit-unlimited override before any
   later catalog promotion:

   ```sh
   witself-admin account limit-override set \
     --account FOUNDER_ACCOUNT_ID \
     --dimension stored_fact \
     --unlimited \
     --reason "Founder current facts are unlimited"

   witself-admin account limit-override get \
     --account FOUNDER_ACCOUNT_ID \
     --dimension stored_fact \
     --json
   ```

   The read must report `overridden: true`, `effective_max: null`,
   `apply_pending: false`, and equal desired/applied snapshot revisions.
3. This Phase B release ships
   `0078_reconcile_active_fact_count.sql`. After every old writer is gone, it
   takes `LOCK TABLE agents IN EXCLUSIVE MODE NOWAIT` followed by
   `LOCK TABLE facts IN SHARE MODE NOWAIT`. The first fence blocks supported
   Phase-A writers that lock the owner row; the second blocks direct/manual or
   legacy fact-table writes that bypass that row. Ordinary reads continue.
   The migration recomputes every agent's count from resolved, non-deleted
   canonical facts (including explicit zeroes), validates exact equality before
   commit, and has a data-only no-op down migration. Either lock's contention
   fails startup promptly for retry instead of queueing ahead of a live writer.
4. The Phase B catalog in `web/plans/plans.json` supplies Personal `1,000`,
   Professional `10,000`, Team `50,000`, and Enterprise `250,000`. Do not deploy
   the catalog-serving Worker, deploy the control-plane container that embeds
   the catalog, reconcile accounts, or set a finite override until every target
   cell is at migration 0078 and every older ReplicaSet is at zero. Re-read the
   Founder explicit-unlimited override immediately before and after catalog
   reconciliation.

Keeping migration and catalog changes in one release tag does not collapse the
operational fence: cells migrate first, catalog/control-plane promotion comes
only after cell convergence. The code is safe before promotion because a
missing `stored_fact` key remains unlimited.

### Implemented active-memory limit

`stored_memory` is an account plan/override dimension enforced independently for
each owner agent. It counts only current heads in the `active` lifecycle state.
Forgotten, superseded, and deleted heads, immutable prior versions, evidence,
relations, vectors, and curation history do not consume another active slot. A
missing key means unlimited and zero is a real cap. Resolution is
`account override > catalog/plan default > missing/unlimited`; an audited
explicit-unlimited override omits the key from the resolved cell snapshot
without changing the account's plan, price, subscription, or invoice history.

The implemented value-free status surfaces are `GET /v1/memories:status`,
`witself memory status`, the read-only idempotent
`witself.memory.status` MCP tool, and `self.show.memory_capacity`. They report
`used`, `max`, `remaining`, `unlimited`, `near_limit`, `at_limit`, and
`over_limit`. Unlimited capacity uses `null` for `max` and `remaining`. For a finite maximum,
`near_limit` becomes true at 90 percent, rounded up to the next whole memory,
and remains true at or beyond the maximum. At `used == max`, `over_limit` is
false, `at_limit` is true, and a net-growing write is blocked. `over_limit`
becomes true only when
an administrator lowers the maximum below current active usage. The
authenticated curation preflight carries the same `memory_capacity` projection
so a restricted curator does not need authority for an ordinary memory route.

Capacity is checked against the complete active-memory effect, not the number of
API calls. Capture adds one active head. Adjust preserves the count. Forgetting
an active head releases one slot; restore or reactivate consumes one. Direct
supersession computes replacements minus the one active source. A curation plan
reports count-only `active_memory_delta` and `projected_active_memories`, then
recomputes them from locked live heads during atomic apply. At or above the
maximum, zero- or negative-delta correction and consolidation remain allowed;
positive-delta plans are refused. This permits a client-authored merge while
preventing a concurrent pair of writers on different server replicas from
overshooting one agent's maximum.

A refused net-growing write returns HTTP 403 with
`code: "stored_memory_limit_reached"`, `retryable: false`, and the current
value-free `limit` object. It never includes memory content, ids, evidence,
plan text, or account/agent labels. The same structured refusal covers capture,
restore, reactivate, direct supersession, and curation apply. Exact idempotent
replay is resolved before the capacity gate. A client must not loop on the same
refused intent; it may read, recall, inspect history, export, correct in place,
or submit a safe non-growing consolidation plan.

Lowering the maximum never deletes, forgets, merges, or disables existing
memories. Witself preserves reads and provenance while the agent is at or over
capacity. Semantic consolidation belongs to the active client agent: it reviews
evidence and authors a reversible plan. The backend only counts, validates,
versions, fences, and applies that exact plan; it performs no semantic inference
and does not wake or launch a model. Permanent deletion is never automatic
capacity management. Account import remains exempt so migration and disaster
recovery can reproduce an over-limit account exactly; subsequent ordinary
net-growing writes use the effective maximum.

Catalog activation is intentionally two-phase:

1. Phase A deploys compatible cells, control-plane override handling, and the
   strict edge dimension allow-list for `stored_memory`, while leaving
   `web/plans/plans.json` unchanged.
   The control-plane binary embeds that file, so merely withholding
   `npm run deploy:plans` is not sufficient: build and deploy Phase A from a
   commit/tag that still has the old catalog. Put the catalog edit in a later
   Phase-B commit/tag. During a rolling Phase-A cell deployment, a pre-Phase-A
   pod can write after migration 74's initial backfill without maintaining the
   new derived counter. This is harmless only while `stored_memory` remains
   missing/unlimited; do not set any finite override or publish a finite
   default in Phase A.
2. Before catalog promotion, set and verify the Founder account's explicit
   unlimited override:

   ```sh
   witself-admin account limit-override set \
     --account FOUNDER_ACCOUNT_ID \
     --dimension stored_memory \
     --unlimited \
     --reason "Founder active memories are unlimited"

   witself-admin account limit-override get \
     --account FOUNDER_ACCOUNT_ID \
     --dimension stored_memory \
     --json
   ```

   The read must report `overridden: true`, `effective_max: null`,
   `apply_pending: false`, and equal desired/applied snapshot revisions.
3. Phase B first rolls every target cell onto the release containing migration
   75. That transactional migration takes an `EXCLUSIVE ... NOWAIT` fence on
   the counter table, repairs missing owner clocks, recomputes every derived
   active-memory count from canonical current heads, and validates exact
   equality. Ordinary reads remain available. Contention fails the startup
   attempt cleanly for retry instead of queueing ahead of live writers. Run it
   only after every remaining writer is Phase-A counter-aware. Wait for all
   replacement server and worker pods to become ready and all older
   ReplicaSets to reach zero before activating any finite cap.
4. From that same Phase-B tag, promote the canonical defaults: Personal
   `1,000`, Professional `10,000`, Team `50,000`, and Enterprise `250,000`
   unless its contract supplies another value. Deploy both the public plan
   Worker and the control-plane container, whose binary embeds the catalog,
   then reconcile every hosted account.
5. Re-read the Founder override after reconciliation and run a finite-account
   canary covering the 90-percent boundary, exact-at-cap refusal, idempotent
   replay, non-growing consolidation, and lowered-cap over-limit behavior.

Once a `stored_memory` override or resolved snapshot has been written, Phase A
is the rollback floor. An older control plane rejects the audited dimension and
an older cell can ignore a stored key and fail open. Roll forward or restore a
compatible snapshot; do not roll either component back below Phase A.

Memories are durable knowledge and do not expire by age. Memory writes and
revisions are included rather than metered as customer-facing monthly usage.
Per-record size, vector, evidence, relationship, revision-history,
curation-frequency, and API bounds remain internal service protections.

Inbound and outbound email records use the same account-level age policy.
Inbound age is measured from `received_at`; an eligible outbound record is
measured from `created_at`. Queued, claimed, and `provider_started` outbound
work is held regardless of age so retention never deletes unresolved provider
work. Once eligible, deletion removes the outbound subject/body and its
metadata; provider-event idempotency receipts are removed in bounded batches
before their parent so a cascade cannot become unbounded.

Inbound attachments remain inside the stored raw MIME; they are not extracted
into separately retained blobs. Deleting an inbound email therefore deletes
its raw MIME and the attachment bytes within it, and that payload expires no
later than the plan's email-retention window. Personal stores no new raw MIME
or attachment payloads while receipt is disabled and queues no outbound body
while send is disabled.

Hard-bounce and complaint suppressions are value-minimal safety state rather
than customer correspondence. They retain only an account-scoped recipient
digest and outlive the finite email window by one year, capped at ten years.
An indefinite/missing email-retention policy still uses the ten-year suppression
safety lifetime. A plan or account override never bypasses a live suppression.

`agent_email_attachment_storage_bytes` is deliberately defined in terms of
that storage model: a retained message with one or more attachments consumes
its full raw-MIME byte length from the account pool. An attachment-free
message consumes no bytes from this particular pool, although its raw MIME
still contributes to the internal `email_storage_byte` observation. This is a
conservative retained-attachment-bearing-MIME allowance, not a sum of
separately extracted attachment blob sizes.

Per-message byte limits, header and part-count limits, nesting-depth limits,
and the retained attachment-bearing-MIME allowance are service protections
rather than billable overages. The storage allowance is pooled at the account
level because the account is the billing boundary; it does not multiply by the
number of agents or realms. Inbound traffic must never create a surprise
charge. If a new attachment-bearing message would exceed the account pool,
Witself retains its bounded text and metadata while declining to retain the
raw MIME containing those attachment bytes, and marks that state explicitly.
It must not evict an existing in-window message merely because a hostile sender
delivered another one.

Still open for packaging decisions:

- Team and Enterprise outbound-email allowances and overages.
- Audit retention by plan.
- Internal storage, vector, fan-out, and API service-protection limits.
- Annual pricing, support boundaries, human seats, and downgrade behavior.

## Billing Model

V0 billing posture:

- Plan-based first.
- Usage-aware from the beginning.
- Account-level billing target; usage measured per realm.
- No required per-realm or per-agent invoice line items.
- No raw per-call billing at launch.
- Overage behavior is configurable by plan and dimension.

Plans should define soft and hard limits for:

- Active named agents.
- Stored memories.
- Stored facts.
- Memory recalls and reads.
- Memory writes (add/adjust).
- Embedding operations.
- Vector storage size.
- General data-at-rest storage size.
- Cross-agent accesses.
- Security groups.
- Messages sent and delivered.
- Agent-email addresses, received/sent events, and inline raw-MIME storage.
- Stored secrets (sealed plane).
- Secret reads, including reveal events and reference resolution (sealed plane).
- TOTP code generation (sealed plane).
- Runtime injection through `witself run` (sealed plane).
- Total encrypted storage size for envelope-encrypted secret material (sealed
  plane).
- General managed-service API request volume.
- Audit retention and stored audit volume.

The five sealed-plane dimensions meter the credential plane only. They never
count toward, and are never derived from, the open-plane recall, embedding, or
digest paths: secrets and TOTP seeds are never embedded, recalled, placed in the
self-digest, or plaintext-exported, and their values surface only through the
reveal-gated paths (see [secret-model.md](secret-model.md) and
[encryption-model.md](encryption-model.md)).

Managed v0 should default to 365 days of audit retention. Longer retention can
be plan-based later, and self-hosted operators can configure retention according
to their own policy (see [audit-retention.md](audit-retention.md)).

## Metered Dimensions

Witself should meter these dimensions internally in v0:

| Dimension | Why it matters |
|---|---|
| `active_agent` | Principal count, plan shape, support burden. |
| `stored_memory` | Storage footprint and recall corpus size. |
| `stored_fact` | Identity-card inventory size. |
| `memory_recall` | Semantic search load and security-relevant access. |
| `memory_write` | Add/adjust load and integrity-relevant mutation. |
| `vector_write` | Validation and persistence load for client-supplied vectors. |
| `vector_storage_byte` | Client-vector JSONB storage and backup size. |
| `crossagent_access` | Cross-agent read/write load and security signal. |
| `security_group` | Group count, policy-evaluation surface. |
| `message_sent` | Outbound mailbox load and abuse control. |
| `message_delivered` | Fan-out delivery load (group fan-out multiplies this). |
| `email_received` | Inbound agent-email volume and abuse accounting; never a victim-billed inbound charge. |
| `email_sent` | Non-billable outbound agent-email volume and sender-reputation observation; one idempotent event is emitted after provider acceptance, while invoice and overage conversion remain disabled. |
| `email_address` | Provisioned live agent-email address count. |
| `email_storage_byte` | Internal observation of inline raw-MIME and backup footprint; not a customer quota or overage dimension. |
| `storage_byte` | General open-plane data-at-rest footprint and backup size. |
| `stored_secret` | Sealed-plane inventory size and storage footprint. |
| `secret_read` | Sealed-plane sensitive access risk and service load (reveal + reference resolution). |
| `totp_code` | Sealed-plane sensitive login assistance and service load. |
| `runtime_injection` | Sealed-plane secret use without printing, service load. |
| `encrypted_storage_byte` | Sealed-plane envelope-encrypted storage cost and backup size. |
| `api_request` | General API burden and abuse control. |
| `audit_event` | Audit retention size and compliance cost. |

Recalls, embedding operations, cross-agent accesses, and messages must be metered
even if v0 pricing stays tiered. They create real backend load and
security-relevant usage signals — the integrity-and-authenticity signals of the
open plane.

On the sealed plane, secret reads, TOTP code generation, and runtime injection
must likewise be metered even when v0 pricing stays tiered. They create real
backend load and are the confidentiality-relevant usage signals of the
credential plane. Metering counts the event; it never records secret or seed
values, and it does not cause secrets to be embedded, recalled, or placed in the
self-digest (see [secret-model.md](secret-model.md) and
[audit-retention.md](audit-retention.md)).

Notes on a few dimensions:

- `memory_recall` covers semantic recall and plain read/get. Recall that runs
  over another agent's memories also increments `crossagent_access`.
- `vector_write` counts accepted client-supplied vector writes. The backend does
  not generate or regenerate vectors and therefore meters no model inference.
- `vector_storage_byte` is a sub-dimension of overall storage; it is metered
  separately because vector storage scales with corpus size and vector
  dimensionality and is the dominant storage cost driver for active realms.
- `message_delivered` can exceed `message_sent` because a message addressed to a
  security group fans out to current group members (see
  [inter-agent-messaging.md](inter-agent-messaging.md)).
- Cross-realm messages (post-v0 cross-realm collaboration) are metered on the
  same existing `message_sent` and `message_delivered` dimensions; a
  realm-qualified destination does not introduce a new billing dimension (see
  [agent-collaboration.md](agent-collaboration.md)).
- Agent email uses its own dimensions because external inbound abuse, outbound
  reputation, address allocation, and MIME storage have different controls from
  the realm-local mailbox. The six resolved ingress-rate keys map internally to
  the closed operational dimensions `email_received` and
  `email_received_bytes`; the two platform-only account aggregates use those
  same dimensions rather than creating billable keys. They are not eight new
  billable dimensions.
  `email_received` remains accounting-only for any gated receive-only
  activation: provisioning and ingestion emit no billable usage event or
  overage, and hostile inbound volume can never bill the recipient. The
  canonical dimension and unit names
  exist in the cell usage contract so later production metering cannot invent
  incompatible keys; inbound emission remains disabled until authoritative
  abuse classification and production pricing are both pinned. For outbound
  mail, successful provider acceptance emits exactly one idempotent
  `email_sent` usage observation for the logical send. That observation is
  operational and non-billable: invoice, overage, and payment-provider
  conversion remain disabled until pricing policy is explicitly approved.
  `email_address` counts live provisioned addresses. `email_storage_byte`
  observes inline raw-MIME footprint independently so mail does not silently
  consume the ordinary `storage_byte` allowance, but it is not exposed as a
  customer quota or overage dimension. Inline raw MIME, including attachment
  bytes, expires by the plan's age-based email-retention window. The separate
  `agent_email_max_raw_bytes` and account-wide
  `agent_email_attachment_storage_bytes` plan limits remain service
  protections. The latter counts the complete retained raw MIME of each
  attachment-bearing message; it does not imply separately stored attachment
  blobs.
  Production pricing and abuse exclusions must be pinned before either receive
  or send becomes billable; see
  [agent-email.md](agent-email.md).
- `storage_byte` measures ordinary open-plane data-at-rest footprint (memories,
  facts, and the rest of the open plane on RDS/disk), not envelope-encrypted
  secret material (see [storage.md](storage.md)).
- `encrypted_storage_byte` is the sealed-plane companion to `storage_byte`. It
  measures the envelope-encrypted secret bytes (ciphertext, wrapped DEKs, and
  attachments) governed by the CMK→per-realm KEK→per-secret/field DEK hierarchy.
  It is metered separately because the sealed plane is a distinct storage and
  KMS cost driver (see [encryption-model.md](encryption-model.md),
  [key-hierarchy.md](key-hierarchy.md), and
  [secret-size-and-attachments.md](secret-size-and-attachments.md)).
- `secret_read` increments on the reveal-gated value-returning paths only —
  `witself secret reveal` and value-returning reference resolution — never on
  plain metadata listing. `totp_code` increments on `witself totp code`, and
  `runtime_injection` increments when a secret is injected into a child process
  by `witself run` without being printed. None of these dimensions cause secret
  values to be embedded, recalled, placed in the self-digest, or
  plaintext-exported (see [secret-model.md](secret-model.md) and
  [totp-2fa.md](totp-2fa.md)).

## Canonical Dimension Names

The metered-dimension names above are the single canonical identifier for each
usage dimension. They are reused verbatim as:

- the keys of the `/v1/capabilities` `limits` object,
- the dimension/item keys in `/v1/billing/usage` output, and
- the `limit_dimension` Prometheus metric label (see
  [observability-and-operations.md](observability-and-operations.md)).

Whether a dimension is a point-in-time cap (`active_agent`, `stored_memory`,
`stored_fact`, `security_group`, `vector_storage_byte`, `email_address`,
`email_storage_byte`, `storage_byte`,
`stored_secret`, `encrypted_storage_byte`) or a rate (`memory_recall`,
`memory_write`, `vector_write`, `crossagent_access`, `message_sent`,
`message_delivered`, `email_received`, `email_sent`, `secret_read`, `totp_code`,
`runtime_injection`, `api_request`, `audit_event`) is conveyed by the limit
object's fields (`max`/`used` for caps; `unit`, `included`, `soft_limit`,
`hard_limit` for rates), not by the key name. Using one key across
all three surfaces lets a client join capability limits, usage items, and metrics
directly. Field shapes are pinned in [json-contracts.md](json-contracts.md).

## Cross-Cell Aggregation

Managed Witself runs as a fleet of independent cells, and an account may span
more than one cell — for example when its realms are placed in different regions
or residency zones (see [deployment-cells.md](deployment-cells.md)). Billing
stays account-level and the canonical dimensions above are unchanged. When an
account spans cells, per-realm usage is summed across the cells that hold the
account's realms, and those per-realm rollups aggregate into the single
account-level total that the plan attaches to.

Each cell meters its own realms locally on the canonical dimensions; the
account-level view is the sum of those per-cell contributions. A realm has a
single home cell, so per-realm usage is never double-counted across cells. The
control plane holds only the account/realm → home-cell mapping needed to drive
this aggregation; it carries routing metadata, not tenant usage data (see
[deployment-cells.md](deployment-cells.md)). Tenant migration moves a realm
between cells without changing the account it bills to.

## Overage Behavior

Overage behavior should be configurable per plan and dimension:

- `warn`: allow the action and emit a warning (no error).
- `throttle`: apply service-protection rate limiting. The action may be delayed
  and still succeed; when it must be rejected, return `rate_limited`
  (HTTP 429, `retryable: true`) with a `retry_after` hint.
- `block`: deny the action with `limit_exceeded` (HTTP 403, `retryable: false`).
  Retrying does not succeed until the plan is raised or the window resets.

Recommended defaults:

| Dimension | Default overage behavior |
|---|---|
| Active agents | `block` for hard plan cap, `warn` near cap. |
| Stored memories | `block` for hard cap, `warn` near cap. |
| Stored facts | `block` for hard cap, `warn` near cap. |
| Memory recalls/reads | `throttle` or `warn`; block only for abuse or hard caps. |
| Memory writes | `throttle` or `warn`; block only for abuse or hard caps. |
| Embedding operations | `throttle` or `warn`; block only for abuse or hard caps. |
| Vector storage size | `warn` near cap, `block` at hard cap. |
| General data-at-rest storage size | `warn` near cap, `block` at hard cap. |
| Cross-agent accesses | `throttle` or `warn`; block only for abuse or hard caps. |
| Security groups | `block` for hard cap, `warn` near cap. |
| Messages sent/delivered | `throttle` or `warn`; block only for abuse or hard caps. |
| Agent-email addresses | `block` for the hard address cap, `warn` near cap. |
| Agent email received | Apply the non-billable temporary platform breakers above, with no plan overage or usage charge in any gated receive-only activation. A production billing default is blocked on authoritative spam/abuse classification; aggregate recipient traffic must never become a victim-billing or mailbox-starvation lever. |
| Agent email sent | Enforce the 30-per-agent, 300-per-realm, and 1,000-per-account one-minute GCRA refill rates, plus platform-only 10,000-per-account/day and 100-per-recipient/day refill rates with 1,000/10 burst tolerances. Successful provider acceptance emits one idempotent, non-billable `email_sent` observation; invoice and overage conversion remain disabled until explicit pricing approval. |
| Agent-email raw-MIME and attachment storage | Expire inline raw MIME by the plan's age-based retention window and reject messages over `agent_email_max_raw_bytes`. Charge the full retained raw-MIME size of each attachment-bearing message to the account-wide `agent_email_attachment_storage_bytes` pool. When that pool lacks room, preserve bounded text and metadata, explicitly mark the raw attachment-bearing payload unretained, and never create an inbound overage charge. |
| Stored secrets | `block` for hard cap, `warn` near cap. |
| Secret reads | `throttle` or `warn`; block only for abuse or hard caps. |
| TOTP code generation | `throttle` or `warn`; block only for abuse or hard caps. |
| Runtime injection | `throttle` or `warn`; block only for abuse or hard caps. |
| Encrypted storage size | `warn` near cap, `block` at hard cap. |
| API requests | `throttle`. |
| Audit retention | `warn` and require plan/config change before retention loss. |

Limit responses should be deterministic and machine-readable so agents can
recover or ask for operator help. The error envelope, error codes, HTTP status,
and exit-code mapping are defined in [api-contract.md](api-contract.md); the
limit-error JSON shape is pinned in [json-contracts.md](json-contracts.md).

A `limit_exceeded` or `rate_limited` response should carry, at minimum: the
canonical `limit_dimension`, the `overage_behavior` in force, `used`,
`included`/`max`, the `soft_limit`/`hard_limit` that tripped, the `reset_at`
window when applicable, a `retry_after` hint for `rate_limited`, and a
recommended next command for the CLI/agent to surface.

## Crypto Payment Rails

Witself retains the full Witpass payment apparatus, including crypto rails, with
no Witself-managed wallet custody. Crypto payment support sits alongside
traditional payment methods rather than replacing them.

Posture:

- Provider-mediated checkout, invoice, or subscription payment only. There is no
  Witself-held wallet, no treasury management, and no on-chain custody in v0.
- Candidate rails: stablecoins such as USDC or USDT where a payment provider
  supports them, and native ETH as a source asset only when a provider can safely
  quote, confirm, and settle the payment.
- Witself prefers fiat or provider-managed stablecoin settlement over direct
  treasury management until there is a deliberate finance, tax, and compliance
  design.
- Witself must not collect wallet seed phrases, private keys, or raw wallet
  credentials in CLI flags, environment variables, config files, logs, support
  tickets, or billing metadata.
- A crypto quote has a finite validity window; settlement must reconcile against
  the provider event, and under/over/late payment is handled by the provider's
  reconciliation flow, surfaced through billing status.
- There is no Witself utility token for v0 or v1. A utility token is not required
  for account setup, billing, agent access, memory recall, fact reads, messaging,
  CLI use, or MCP use.

CLI-owned hosted flows:

- When a payment-provider or regulatory requirement demands hosted checkout,
  secure payment setup, bank authorization, SCA-style browser approval, or a
  crypto checkout page, the CLI owns the workflow rather than collecting payment
  data. It creates the session, shows or opens the URL on request, returns a
  resumable session ID, polls or watches status, and emits machine-readable
  completion state.
- Crypto checkout sessions are tracked through the same session surface as other
  hosted payment flows (`witself billing sessions show`).

## CLI Requirements

The CLI should expose the following complete target surface. The current
implemented slice is `billing show`, `billing invoices`, `billing payments`,
`billing portal`, and `billing setup`; all other entries below remain target
work unless documented separately:

- `witself billing show`
- `witself billing usage`
- `witself billing limits`
- `witself billing plans`
- `witself billing subscribe --promo-code`
- `witself billing payment-methods` (list/add/remove/default; hosted-flow
  initiation, never raw card or wallet collection)
- `witself billing payments`
- `witself billing portal`
- `witself billing setup`
- `witself billing crypto` (quote/checkout/status; provider-mediated, no wallet
  custody)
- `witself billing invoices` (list/show/download)
- `witself billing sessions show`
- `witself capabilities`

The full noun/verb surface and flag conventions are defined in
[cli-command-surface.md](cli-command-surface.md). Billing-impacting payment
changes require or prompt for an audit `--reason`; read-only billing commands do
not. Risky billing/payment mutations support `--dry-run`, which validates inputs,
authorization, conflicts, quotas, and provider prerequisites without persisting
state, creating hosted provider sessions, or charging payment methods.

Usage and limits output should include:

- Current plan.
- Account and per-realm rollup.
- Metered dimensions (canonical names).
- Used quantity.
- Included quantity.
- Soft/hard limit status.
- Overage behavior.
- Reset window when applicable.
- Recommended next command.

## Capability Requirements

Backends should report billing capabilities:

- Managed Witself Cloud: billing is expected to be supported as the product
  matures.
- Self-hosted: billing may be disabled, local-only, or wired to the operator's
  own billing system. Self-hosting must not require Witself-managed billing (see
  [self-hosting.md](self-hosting.md)).
- Local development: billing is normally unsupported or mocked.

Unsupported billing operations should return `unsupported_operation` with
capability context. Crypto payment is independently capability-gated: a backend
may support fiat billing while reporting crypto rails as unsupported.

V0 clients should treat billing capability discovery as authoritative. A command
shape can exist before the backend supports live billing or crypto settlement.

## Pricing Follow-Up

The exact plan names, prices, included quantities, and overage policy are still
business decisions. V0 should preserve pricing flexibility while collecting
enough usage data to make those decisions responsibly. The embedding-operation
and vector-storage dimensions in particular carry real provider cost and should
be observed before fixed inclusions are set. On the sealed plane, `secret_read`,
`totp_code`, and `encrypted_storage_byte` carry real KMS and envelope-storage
cost and should be observed on the same basis (see
[key-hierarchy.md](key-hierarchy.md)).

## Related Docs

- [requirements.md](requirements.md)
- [v0-scope.md](v0-scope.md)
- [cli-command-surface.md](cli-command-surface.md)
- [json-contracts.md](json-contracts.md)
- [api-contract.md](api-contract.md)
- [memory-model.md](memory-model.md)
- [inter-agent-messaging.md](inter-agent-messaging.md)
- [secret-model.md](secret-model.md)
- [totp-2fa.md](totp-2fa.md)
- [encryption-model.md](encryption-model.md)
- [key-hierarchy.md](key-hierarchy.md)
- [secret-size-and-attachments.md](secret-size-and-attachments.md)
- [storage.md](storage.md)
- [deployment-cells.md](deployment-cells.md)
- [agent-collaboration.md](agent-collaboration.md)
- [self-hosting.md](self-hosting.md)
- [implementation-plan.md](implementation-plan.md)
- [audit-retention.md](audit-retention.md)
- [observability-and-operations.md](observability-and-operations.md)
