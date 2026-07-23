# Transcript Retention

Status: implemented account-policy slice.

Transcript retention is an account-level managed-service policy. It is
independent of inactive-account cleanup, account closure, audit retention, and
narrative-memory retention. Expiring a transcript does not expire a memory.

## Plan defaults and precedence

The control-plane plan catalog owns the defaults:

| Display tier | Stable plan id | Monthly price | Transcript default |
|---|---|---:|---:|
| Personal | `free` | $0 | 30 days |
| Professional | `standard` | $30 | 90 days |
| Team | `team` | $250 | 365 days |
| Enterprise | `enterprise` | Custom | Indefinite |

The resolved value follows one rule:

```text
account transcript-retention override
  > current effective plan default
  > missing key (indefinite)
```

An account override changes only transcript retention. It does not change the
plan, price, provider customer, subscription, invoice, or renewal dates.
Clearing it immediately restores the current plan default, including a plan
default changed by a later catalog deployment.

The control plane stores the override's immutable `actor_id`, display
`actor_handle`, reason, and time in the account's compare-and-swap lifecycle
record, together with append-only transition history. Handles can be reused
after an administrator record is retired, so attribution always keys on
`actor_id`; the handle is display metadata only. `GET /v1/accounts/{id}/plan`
and the admin read return both `default_days` and `effective_days`; JSON `null`
means indefinite.

## Admin operations

The Cloudflare Worker authenticates the caller's `witself_adm_*` credential
against the admin registry, strips it, and forwards only the private bridge
credential plus the verified immutable admin id and non-secret display handle
to Go. The Go control-plane rejects a bridge request without all three pieces.
Account-owner credentials can read their resolved plan status but cannot mint
exceptions.

Set an account-specific retention window:

```sh
curl -fsS -X PUT \
  -H "Authorization: Bearer $WITSELF_ADMIN_TOKEN" \
  -H "Content-Type: application/json" \
  "$WITSELF_CONTROL_PLANE/v1/admin/accounts/$ACCOUNT_ID/transcript-retention" \
  -d '{"days":60,"reason":"approved account exception"}'
```

Set an explicit indefinite exception:

```json
{"indefinite":true,"reason":"contractual retention exception"}
```

Clear the exception and resume plan inheritance:

```sh
curl -fsS -X DELETE \
  -H "Authorization: Bearer $WITSELF_ADMIN_TOKEN" \
  -H "Content-Type: application/json" \
  "$WITSELF_CONTROL_PLANE/v1/admin/accounts/$ACCOUNT_ID/transcript-retention" \
  -d '{"reason":"restore current plan default"}'
```

The founder/personal-account Enterprise backfill is deliberately a separate
operation. It changes the effective plan classification without fabricating a
billing relationship:

```sh
curl -fsS -X PUT \
  -H "Authorization: Bearer $WITSELF_ADMIN_TOKEN" \
  -H "Content-Type: application/json" \
  "$WITSELF_CONTROL_PLANE/v1/admin/accounts/$ACCOUNT_ID/plan-override" \
  -d '{"plan":"enterprise","reason":"founder account backfill"}'
```

The lifecycle record continues to report `billing_plan: "free"` while the
effective `plan` is `enterprise`. `DELETE` on the same route clears the
classification override. Operators should always read the response and verify
`plan`, `billing_plan`, `default_days`, and `effective_days` before considering
a backfill complete.

## Control-plane lifecycle rollout

The Go lifecycle routes stay absent unless
`WITSELF_CP_PLAN_LIFECYCLE_ENABLED=true` is set explicitly. An enabled
production container also requires:

- `WITSELF_CP_BRIDGE_URL`, the directory-owning Worker base URL;
- `WITSELF_CP_BRIDGE_TOKEN`, the Worker/container shared credential; and
- the existing `WITSELF_CP_R2_*` lifecycle-record store configuration.

`WITSELF_CP_BILLING_PROVIDER` is optional. With no provider, the control plane
runs in manual mode: plan status, catalog defaults, admin overrides, account
seeding, and cell enforcement work, while upgrade, downgrade, cancel, and
webhook routes are absent. Manual mode never creates a provider customer,
subscription, invoice, or charge. Stripe can be enabled later without changing
the already-stored Personal/free baseline. In the Cloudflare deployment, set
the runtime Worker bindings `CP_BILLING_PROVIDER=stripe`,
`CP_STRIPE_SECRET_KEY`, and `CP_STRIPE_WEBHOOK_SECRET`; optional hosted return
URLs use `CP_STRIPE_SUCCESS_URL` and `CP_STRIPE_CANCEL_URL`. The Backend
allowlist projects those bindings to their corresponding `WITSELF_CP_*`
container variables. The fake provider is refused on the production bridge.

Cloudflare deployment is intentionally separate from the GitHub tag workflow.
Run it only from the same clean checkout whose `vMAJOR.MINOR.PATCH` tag finished
the release workflow:

```sh
cd infra/cloudflare/control-plane
npm ci

# Public catalog deployment uses this package's committed Wrangler lock too.
npm run deploy:plans

# Render a release-stamped container configuration. The renderer refuses a
# dirty checkout or an untagged HEAD.
npm run config

# Configure all prerequisites while the lifecycle gate remains false. Each
# command reads the value from stdin; never put a credential on the command line.
printf '%s' "$INTERNAL_BRIDGE_TOKEN" |
  npm run secret:put -- INTERNAL_BRIDGE_TOKEN
printf '%s' "$CP_R2_ENDPOINT" |
  npm run secret:put -- CP_R2_ENDPOINT
printf '%s' "$CP_R2_BUCKET" |
  npm run secret:put -- CP_R2_BUCKET
printf '%s' "$CP_R2_ACCESS_KEY" |
  npm run secret:put -- CP_R2_ACCESS_KEY
printf '%s' "$CP_R2_SECRET_KEY" |
  npm run secret:put -- CP_R2_SECRET_KEY
printf '%s' false |
  npm run secret:put -- CP_PLAN_LIFECYCLE_ENABLED

# This builds VERSION/COMMIT/DATE into the Go container and refuses success
# unless the deployed /v1/version reports that exact identity.
npm run deploy
```

The committed `package-lock.json` pins Wrangler for the control-plane and public
plan deployments. Do not substitute a floating `npx wrangler`. Keep
`CP_PLAN_LIFECYCLE_ENABLED=false` through the initial cell rollout below. After
cell convergence and the provision-authenticated plan-fence read succeed,
activate lifecycle as its own Worker deployment, with the gate written last:

```sh
printf '%s' true |
  npm run secret:put -- CP_PLAN_LIFECYCLE_ENABLED
# The activation command reads the private credential from the environment,
# never argv. It force-replaces the singleton Go process with the fresh Worker
# secret projection and succeeds only after the private lifecycle status route
# reports enabled.
export INTERNAL_BRIDGE_TOKEN
npm run activate:plan-lifecycle
unset INTERNAL_BRIDGE_TOKEN
npm run verify
```

Changing a Worker secret does not update the environment of an already-running
container. Do not omit `activate:plan-lifecycle`: it calls the
`INTERNAL_BRIDGE_TOKEN`-authenticated Worker activation endpoint, destroys and
restarts the singleton Backend container, waits for port 8080, and then probes
the restarted Go process's authenticated `GET /v1/plan-lifecycle/status`
response. The command fails closed unless that response is valid and reports
`plan_lifecycle.enabled=true`. It never puts the bridge credential in argv,
logs, or response bodies. Set `WITSELF_CONTROL_PLANE` only when activating a
non-production HTTPS endpoint.

Each Worker maintenance cron loads one opaque directory cursor from
`DIRECTORY` KV, selects one bounded page (100 active accounts), and sends that
page to Go through an authenticated, value-free lifecycle tick. The tick stores
a Personal/`free` lifecycle record for every previously unseen account and
pushes the exact revision-and-hash-fenced snapshot through the Worker to its
current cell. Only a structurally valid Go acknowledgement advances the cursor.
Individual account failures advance with the rest of the page and retry on the
next complete directory cycle, so one unavailable account cannot pin the fleet
behind it.

Reaching the end stores an empty cursor and restarts the scan on the next cron.
The cursor belongs to KV rather than the Go process: a scale-to-zero container
is woken by the cron call and container sleep, restart, or replacement cannot
reset fleet progress. Existing accounts are therefore backfilled and newly
activated accounts join without making signup depend on R2 or the billing
provider. The hosted tick never lists and rereads every lifecycle object from
R2.

Before applying, Go reads only the cell's current snapshot revision and hash
through the Worker bridge. If an R2 restore lost or rolled back a lifecycle
record while the cell retained a higher revision, Go advances above both
fences and reapplies its own catalog-and-override-derived snapshot. It never
adopts plan, limit, policy, or feature payload from the cell. Conversely, an
expired provider-pinned checkout remains pending if its provider is not
configured, because reconciliation must cancel provider state before clearing
the local marker.

`GET /v1/plan-lifecycle/status`, authenticated with the private bridge token,
reports only aggregate run, scanned, seeded, pending-apply, and failed counts
plus timestamps and billing availability. Logs carry the same aggregate
counts—never account ids, cursors, endpoints, customer ids, or raw errors.

## Cell snapshot and enforcement

Cells never load or interpret the plan matrix. The control plane resolves the
effective plan, limits, policies, and features, hashes that exact snapshot, and
pushes it through the provision-token-gated account `:plan` endpoint. The cell
stores:

- `accounts.plan`;
- `accounts.plan_limits`;
- `accounts.plan_policies`;
- `accounts.plan_features`;
- `accounts.plan_applied_at`.

Before a push, the production bridge reads only the cell's current
revision/hash fence. If the R2 lifecycle record was restored behind the cell,
the control plane advances above the greater observed revision and reapplies
its own resolved snapshot. It never adopts plan or policy payload from the
cell, so recovery preserves control-plane entitlement authority while avoiding
a permanent stale-revision loop.

`transcript_retention_days` must be an integer from 1 through 36,500. A missing
key is indefinite; zero is rejected rather than being treated as “delete
immediately” or “unlimited.” Account export/import carries the complete applied
snapshot and timestamp.

The cell worker selects whole conversations only when `updated_at` is strictly
older than the account cutoff. It never partially truncates a conversation.
The cutoff comes from PostgreSQL's statement clock rather than a cell host
clock.

Each sweep first divides the configured batch into exact per-account quotas,
then reads at most that many raw conversation keys before checking evidence,
curation holds, or row locks. Hold-heavy and write-locked backlogs therefore
cannot turn a batch of 100 into an unbounded transcript or provenance scan.
The worker reports the raw `scanned` count, candidates skipped because they
could not be locked and revalidated, and whether any account filled its scan
quota. All are aggregate, value-free counts.

Preview and enforcement keep separate durable per-account keyset cursors.
Each cursor uses a fixed, finite cycle cutoff and advances across held or busy
candidates as well as deletable ones. This lets later rows make progress,
ensures released holds are revisited after wraparound, and prevents a long
preview rollout from advancing enforcement past its first deletion candidates.
A policy-window change resets that account's cycle, while every candidate is
still rechecked against the current live cutoff before deletion. Appending and
enforcement lock the same conversation row.

Retention is disabled by default and uses an explicit three-stage rollout:

1. leave the worker disabled while schema and code converge;
2. enable it in `preview` mode and review several intervals of counts;
3. change only the mode to `enforce` after the counts and holds are understood.

The control-plane lifecycle gate must also remain off through the first cell
rollout. Old cell pods accept the older unfenced plan request shape, so this is
a mandatory two-phase compatibility boundary:

1. deploy the new cell image with transcript retention disabled and
   `WITSELF_CP_PLAN_LIFECYCLE_ENABLED` still unset/false in the control plane;
2. wait for the cell Deployment rollout to complete, verify every ready pod is
   on the new version, and verify the old ReplicaSet has zero pods;
3. verify the provision-authenticated plan snapshot GET endpoint through the
   cell Service; only then enable the control-plane lifecycle in a separate
   control-plane deployment.

Do not overlap steps 1 and 3. A malformed acknowledgement detects an old pod
after the fact, but it cannot undo an unfenced write that the old pod already
performed.

The worker defaults to previewing 100 conversations every five minutes and can
be tuned with:

- `WITSELF_TRANSCRIPT_RETENTION_ENABLED` (default `false`);
- `WITSELF_TRANSCRIPT_RETENTION_MODE` (`preview`, the default, or `enforce`);
- `WITSELF_TRANSCRIPT_RETENTION_BATCH_SIZE` (1-1000);
- `WITSELF_TRANSCRIPT_RETENTION_INTERVAL` (1 minute-24 hours).

Enabling the worker without setting a mode is therefore non-destructive.
Deletion requires both `WITSELF_TRANSCRIPT_RETENTION_ENABLED=true` and
`WITSELF_TRANSCRIPT_RETENTION_MODE=enforce`.

The interval is enforced by a durable cell-local `next_run_at` fence, not only
by each process's ticker. A successful worker attempt advances that fence in
the same transaction as its scan progress. Staggered replicas and restarted
pods can attempt work, but they cannot multiply configured batch throughput;
only one worker batch is admitted per interval. Explicit store preview/process
operations bypass the cadence fence for tests and manual operator runs without
moving `next_run_at`.

Non-zero runs log only counts and the selected mode: eligible conversations,
deleted conversations, evidence-deferred conversations, curation-deferred
conversations, raw candidates scanned, candidates skipped at the lock/recheck
boundary, and whether the candidate scan was capped. In preview, `eligible` is
the bounded number that preview sweep would classify as deletable,
`scan_capped` and the compatibility cap fields say more may remain, and
`deleted` remains zero. Transcript ids, titles, bodies, payloads, and account
ids never enter those logs.

## Archive and import

Account export is not a retention-enforcement path. The archive's existing
`REPEATABLE READ` transaction streams the account rows physically present in
its database snapshot. It does not recalculate the retention window, detach
inputs, delete conversations, or omit an expired row merely because the
background worker has not reached it. Disabled mode and preview mode are
therefore non-destructive during account movement as well as normal serving.
A bounded worker backlog can carry an expired conversation to the destination,
where ordinary enforcement eventually removes it.

The enforce worker is the only path in this slice that applies the policy. It
deletes an expired unheld whole conversation and its entries, and that already
enforced physical state is what a later archive observes. Resolved memory
evidence and active `open` or `planned` curation inputs prevent that deletion,
so those held conversations, entries, and live cursors remain portable.

Terminal curation history is not a hold. When enforcement deletes its expired
source conversation, the same database statement preserves every input row,
ordinal, interval, count, and receipt while detaching the payload pointer:

- `transcript` and `transcript_coverage` inputs export with
  `transcript_id: null`;
- transcript `cursor` inputs keep `cursor_source_kind: "transcript"` and their
  prior/upper interval but export with `cursor_stream_id: null`;
- all detached inputs carry `transcript_pruned_at`; and
- the corresponding live `memory_curation_cursors` row is deleted.

Import accepts those detached shapes only for terminal runs and only with the
pruning marker. Active runs must remain attached, and attached transcript
references must match the exact archived owner and realm.

Immutable value-free transcript usage events and rollups survive payload
retention. An imported usage event may name a well-formed transcript id absent
from the archive because enforcement deleted that conversation. If the
conversation is present, import still requires the exact agent/realm match, so
the exception cannot graft a usage event onto another archived conversation.

## Memory and evidence integrity

Memories are a separate durable product surface and do not disappear with
transcripts. A conversation referenced by resolved memory evidence is retained
until Witself can materialize the already-specified immutable bounded evidence
artifact or an authorized memory operation replaces/removes that evidence.
Inputs frozen into an active `open` or `planned` memory-curation run also defer
deletion. Once a run is applied, rolled back, abandoned, interrupted, or in
conflict, retention removes that run's transcript-input pointer in the same
transaction as the conversation. The run's value-free input counters and
receipts remain as history; any durable resolved memory evidence remains an
independent hold. Holds consume only the bounded raw-candidate budget for that
sweep, and the durable keyset cursor advances across them. They therefore
cannot cause unbounded work or permanently starve later deletable rows.

Deferred counts are operationally visible and intentionally bounded. Releasing
resolved-evidence holds through evidence-artifact materialization is a later
slice; this worker never breaks provenance merely to satisfy a storage
deadline. Enforce-mode logs also report the count of terminal curation input
pointers released in each batch.
