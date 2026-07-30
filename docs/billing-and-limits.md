# Witself Billing And Limits

Status: draft. Decision: v0 billing is account-level, plan-based, usage-aware,
and not raw per-call billing at launch.

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

Inbound agent email follows the same account-policy model. Personal does not
include receipt and carries a 30-day downgrade-cleanup window. Professional,
Team, and Enterprise include receipt with 90-, 365-, and 365-day defaults.
Availability and retention can each be overridden without changing billing.

V0 should meter meaningful usage internally, but charge primarily by plan tier.
This gives Witself enough data to understand real service load without making
the first pricing model feel like nickel-and-dime metering.

The first v0 release does not need live payment collection or full subscription
management. Billing, payment, crypto payment, and invoice commands may be
contract-shaped and capability-gated while the core product matures across both
planes — the open plane (realm, agent, memory, fact, policy, group, message, and
audit) and the sealed plane (secret and TOTP). The metered payload spans identity
usage and credential usage; the sealed-plane dimensions count events only and
never carry secret or seed values.

The implemented transcript-usage slice is deliberately upstream of this
billing design: immutable `usage_events` plus hourly/daily `usage_rollups` move
with the account and power `GET /v1/usage`. No Stripe object is the usage source
of truth. Realm/account billing aggregation and conversion into plan charges
remain deferred.

## Working Plan Direction

The following table records the current product direction as of 2026-07-30. It
is a working packaging decision, not a claim that every entitlement is already
implemented or enforced. Each row moves into the canonical plan catalog and
resolved cell policy only through its own implementation and rollout decision.
The realm, agent, active-memory, and current-fact values are the Phase B
canonical defaults described below; the other rows remain subject to their own
implementation and rollout gates.

| Capability | Personal — $0 | Professional — $30/month | Team — $250/month | Enterprise — contact us |
|---|---:|---:|---:|---:|
| Realms | 1 | 1 | 25 | Contracted |
| Agents per realm | 10 | 100 | 100 | Contracted |
| Active memories per agent | 1,000 | 10,000 | 50,000 | Contracted; 250,000 default |
| Current facts per agent | 1,000 | 10,000 | 50,000 | Contracted; 250,000 default |
| Transcript retention | 30 days | 90 days | 365 days | Configurable, including indefinite |
| Secrets per agent | 0 | 100 | 250 | 1,000 |
| Agent messages | Disabled; 30-day downgrade cleanup | Unlimited; retained 90 days | Unlimited; retained 365 days | Enabled; retained 365 days by default, contract override |
| Receive agent email | No | Unlimited; retained 90 days | Unlimited; retained 365 days | Enabled; retained 365 days by default, contract override |
| Raw MIME and attachment retention | None stored | 90 days | 365 days | Configurable, including indefinite |
| Maximum raw email size | Not available | 10 MiB | 25 MiB | Contracted; 25 MiB default |
| Retained attachment storage per account | 0 | 5 GiB | 100 GiB | Contracted |
| Send agent email | No | No | Included | Included |
| Agent email addressing | None | Realm ID on `witmail.ai` | Custom realm designator and custom domain | Custom realm designator and custom domain |

The Witself-provided agent-email address format is
`agent-name.realm-id@witmail.ai`. Team and Enterprise may replace the realm ID
with their custom realm designator and use a configured custom domain:
`agent-name.realm-designator@customer-domain`. A realm designator remains part
of the address on custom domains.

In this table, "unlimited" means that the plan does not expose a per-message or
per-email charge. It remains subject to fair-use, abuse-prevention, and
technical rate limits. Inbound hostile traffic must not create recipient
charges. "Included" confirms that outbound agent email is available, but its
sending allowance and overage treatment remain to be decided. "Contracted"
means the quantity or policy is negotiated for the Enterprise account.

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

The resolved cell snapshot uses feature `agent_email_receive`, policy
`agent_email_retention_days`, and rollout marker
`agent_email_entitlement_version: 1`. A marked snapshot without the feature
rejects inbound email before storage; an unmarked legacy snapshot preserves
the pre-entitlement receive behavior. Retention remains independent so mail
already stored before a downgrade can age out on schedule. An absent retention
key means explicit indefinite retention.

The integration stays installed across plan and override changes. The
administrator surface is:

```sh
witself-admin account email-receive get --account ACCOUNT_ID
witself-admin account email-receive set --account ACCOUNT_ID --enabled --reason "..."
witself-admin account email-receive set --account ACCOUNT_ID --disabled --reason "..."
witself-admin account email-receive clear --account ACCOUNT_ID --reason "..."

witself-admin account email-retention get --account ACCOUNT_ID
witself-admin account email-retention set --account ACCOUNT_ID --days 365 --reason "..."
witself-admin account email-retention set --account ACCOUNT_ID --indefinite --reason "..."
witself-admin account email-retention clear --account ACCOUNT_ID --reason "..."
```

The matching authenticated resources are
`/v1/admin/accounts/{id}/email-receive` and
`/v1/admin/accounts/{id}/email-retention`, each with `GET`, `PUT`, and
`DELETE`. Owner plan status exposes only inherited and effective values.

Before catalog reconciliation, the founder account receives an explicit
indefinite email-retention override followed by an explicit enabled
email-receive override. This keeps its first marked snapshot from temporarily
inheriting Enterprise's finite default.

Rollout is cell-and-edge first: deploy a cell that understands the marked
snapshot and an agent-email edge Worker that accepts the cell's
`feature_disabled` verdict before the control plane publishes or reconciles
the new catalog. After any marked snapshot is accepted, the cell, control
plane, and edge Worker share a forward-only compatibility boundary. Rolling
the edge Worker back to a pre-entitlement version would treat an intentional
accept-and-drop verdict as a transient relay failure and cause sender retries.

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
value-free `limit` object. This stored-inventory refusal is the implemented
exception to the generic draft `limit_exceeded`/HTTP 429 block behavior below.
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

Email records, raw MIME, and extracted attachment payloads use age-based
retention. Attachments are stored separately from the email record so their
bytes can be managed independently, but deleting an email must cascade to its
raw MIME and every attachment. All three expire no later than the plan's
email-retention window. Free stores no raw MIME or attachment payloads; this
remains true even if a later Free feature exposes limited email metadata.

Per-message and per-attachment byte limits, header and part-count limits,
nesting-depth limits, and the retained attachment-storage allowance are service
protections rather than billable overages. Attachment storage is pooled at the
account level because the account is the billing boundary; it does not multiply
by the number of agents or realms.
Inbound traffic must never create a surprise charge. If an agent reaches the
account's attachment-storage ceiling, Witself retains the email's bounded text
and metadata while declining to retain new attachment bytes, and marks that
state explicitly. It must not evict an existing in-window attachment merely
because a hostile sender delivered another message. The current receive-only
pilot remains capped at 5 MiB of raw MIME per message until the production
limits above are implemented and validated.

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
| `email_received` | Inbound agent-email volume and abuse accounting; never a victim-billed pilot charge. |
| `email_sent` | Future outbound agent-email volume and sender-reputation enforcement. |
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
  the realm-local mailbox. `email_received` is accounting-only for the
  authorized Cloudflare pilot: pilot provisioning and ingestion emit no
  billable usage event, have no quota/overage enforcement, and hostile inbound
  volume can never bill the recipient. The canonical dimension and unit names
  exist in the cell usage contract so later production metering cannot invent
  incompatible keys; emission remains disabled until authoritative abuse
  classification and production pricing are both pinned. `email_sent` remains
  dormant until a send slice exists.
  `email_address` counts live provisioned addresses. `email_storage_byte`
  observes inline raw-MIME footprint independently so mail does not silently
  consume the ordinary `storage_byte` allowance, but it is not exposed as a
  customer quota or overage dimension. Raw MIME and separately stored
  attachments expire by the plan's age-based email-retention window, and
  deleting an email cascades to both. Per-message, per-attachment, and pooled
  per-account attachment-storage bounds remain service protections.
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
- `block`: deny the action with `limit_exceeded` (HTTP 429, `retryable: false`).
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
| Agent email received | No plan overage action in the limited pilot. A production default is blocked on authoritative spam/abuse classification and source-scoped enforcement; aggregate recipient traffic must never become a victim-billing or mailbox-starvation lever. |
| Agent email sent | `block` at the hard per-period threshold; sending remains dormant until a send slice exists. |
| Agent-email raw-MIME and attachment storage | Store attachments separately; expire all payloads by the plan's age-based retention window; cascade email deletion to raw MIME and attachments. Reject oversized individual payloads. At an internal per-agent attachment ceiling, preserve bounded email text and metadata, explicitly mark unretained attachments, and never create an inbound overage charge. |
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

The CLI should expose:

- `witself billing show`
- `witself billing usage`
- `witself billing limits`
- `witself billing plans`
- `witself billing subscribe --promo-code`
- `witself billing payment-methods` (list/add/remove/default; hosted-flow
  initiation, never raw card or wallet collection)
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
