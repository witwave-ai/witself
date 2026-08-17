# Managed agent-email sending adapter

This Worker is the provider boundary for outbound Witself agent email. Cell
workers retain all account policy and mailbox authority. The adapter holds the
Cloudflare Email Sending binding, verifies a signed immutable dispatch, and
keeps one Durable Object receipt per logical send.

The default configuration is dark:

- `DISPATCH_ENABLED=false` rejects no mail permanently; it returns a bounded
  retryable response without calling the provider.
- `RECEIPT_REPLAY_ENABLED=false` keeps the operator receipt-proof endpoint
  unavailable.
- `EVENT_DELIVERY_ENABLED=false` leaves lifecycle events on the Queue retry
  path.
- The lifecycle event subscription is provisioned disabled.

That is the committed deployment default, not the current production status.
As of 2026-08-16, v0.0.253 serves the exact Founder cohort with adapter dispatch
and lifecycle event delivery enabled, the
`witself-agent-email-send-lifecycle` subscription enabled, and receipt replay
disabled. The main lifecycle Queue is
`witself-agent-email-send-events`; its retained incident path is
`witself-agent-email-send-events-dlq`. Always inspect live gates, cohort,
subscription, Queue consumer, and deployment version before an operation. A
catalog entitlement is never proof of provider readiness. The live Worker binds
Rate Limiter namespace `2301` at 1,000 requests per 60 seconds, has version
preview URLs disabled (`preview_urls=false`), and has Cloudflare Workers
Observability enabled. Its production `workers.dev` endpoint remains public.

## Safety model

The cell marks its outbox row `provider_started` before making the HTTPS call.
The adapter then stores its own `provider_started` receipt before calling the
Email Sending binding. An exact replay uses the same send ID and digest. It
never performs a second provider call after an uncertain provider boundary.
The receipt also durably increments `provider_call_started_count` as the final
awaited operation before every real Email Sending call.
An older retryable receipt created before this counter shipped is treated as
having crossed at least one provider boundary; its next attempt is persisted as
call two or later, so an upgraded receipt can never manufacture a one-call
proof.

`POST /v1/dispatch:receipt-replay` is a separate, default-off operational
proof. It accepts the byte-identical dispatch body under the distinct
`witself-agent-email-send-receipt-replay` signature audience. Its Durable
Object method can only read and update the content-free receipt: it has no
Email Sending or provider-route call path. A proof succeeds only for an exact
digest and original signer match against a fully accepted receipt. It then
increments `verified_replay_count` without changing
`provider_call_started_count`. Missing receipts return 404; conflicts and
unresolved or non-accepted receipts return 409. The successful closed response
contains only the schema, send ID, accepted state, two match booleans, the two
bounded counters, and `route_pending`; it never returns message content,
addresses, a digest, signer key, provider ID, or provider response.
The proof read/increment is one storage-only transaction. Route finalizers also
reread and merge the fresh exact receipt lineage in short transactions after
provider-route I/O. They preserve concurrent proof counters and unrelated
fields, never change `route_pending` from false back to true, and never
resurrect a missing, expired, or replaced receipt.

The cell, not this adapter, owns sender admission. Its GCRA lanes refill at 30
per agent, 300 per realm, and 1,000 per account each minute. Long-horizon lanes
refill at 10,000 per account and 100 per normalized recipient each day, with
1,000/10 burst tolerances and rolling-day upper bounds of 11,000/110.
Immediately before dispatch, the worker applies an independent provider-attempt
lane with those same rates and burst tolerances so retries and multiple replicas
cannot bypass this boundary. Recipient limiter state contains only an
account-domain-separated SHA-256 bucket identifier.

If Email Sending returns a message ID but the content-free provider route
cannot be registered immediately, the known acceptance is returned with that
message ID and a Durable Object alarm repairs the route. This does not resend
the message.

Adapter receipts and provider routes contain no message content and have
separate lifetimes. A receipt expires seven days after its latest provider
attempt, exceeding the maximum 24-hour interval to the next retry. A provider
route expires after 400 days, covering the Team plan's 365-day mail window plus
delivery-event delay. These are bounded edge idempotency/routing windows, not
customer email retention: the cell remains the authoritative store for
plan-governed sent-mail history. Terminal cell rows are never replayed. A cell
may repeat the exact signed envelope to read this receipt, but the adapter never
starts another provider call after its own boundary became ambiguous. Exact
provider-route re-registration does not extend the fixed 400-day expiry.

The public dispatch endpoint first consumes only a domain-separated,
SHA-256-hashed connecting-IP Rate Limiter lane. It then authenticates the
Ed25519 signature over the declared body digest without reading the body.
Only a request with valid signed headers may stream a body, and that stream is
bounded at a 2 MiB JSON envelope and must match the signed digest. After JSON
validation and signer-to-account authorization, the request consumes fixed
shared aggregate and verified-signer lanes before it can reach receipt storage
or Email Sending. An anonymous caller can therefore spend only its own source
lane; it cannot drain either shared lane and deny valid cell traffic. A caller
replaying a captured signed header with a different body also cannot consume a
shared lane. The decoded text field still has its independent 256 KiB UTF-8
cap. The larger envelope limit is necessary because valid one-byte text
characters can expand to six-byte JSON escapes.

All three front-door lanes use the one committed `DISPATCH_FRONTDOOR_LIMITER`
binding at 1,000 requests per 60 seconds per key. That fixed ceiling is sized
for the current cell deployment: two replicas, a batch of 10, and one poll
every two seconds can make at most 600 dispatch attempts per minute; a rolling
update with one surge replica can make at most 900. Changing replica count,
`maxSurge`, batch size, or poll interval requires recalculating this ceiling
and reviewing the config, tests, and bundle gate before rollout. A missing
binding, binding exception, malformed result, or missing connecting IP fails
closed with `503` and `Retry-After: 60`; an exhausted lane returns `429` with
the same retry hint. Source-lane failures happen without reading the body and
use only the fixed `esnd_invalid` identifier. Shared-lane failures occur only
after the exact signed dispatch has been authenticated and may safely echo its
validated send ID for an exact retry.

The current cell client requires an exact send ID in every closed adapter
response, while the send ID intentionally exists only inside the unread body.
It therefore treats a pre-body source-lane refusal conservatively as an
uncertain result and exact-replays the same logical dispatch; it does not use
the response body's 60-second delay. The shared lanes return the authenticated
send ID, so their delay is honored normally. The 100-request headroom above the
900-attempt surge ceiling, plus the cell's authoritative provider-attempt GCRA
lane, keeps that conservative source retry bounded. Do not lower the
front-door ceiling below the reviewed surge capacity without changing and
revalidating this response contract.

Cloudflare's [Workers Rate Limiting API](https://developers.cloudflare.com/workers/runtime-apis/bindings/rate-limit/)
is deliberately a coarse abuse circuit breaker: its counters are local to a
Cloudflare location, eventually consistent, and may be permissive. It is not
global quota accounting and must not replace the cell's authoritative
Postgres GCRA lanes. The deterministic header-first body-read boundary remains
the primary cost defense. Worker code emits no custom per-refusal log
containing an IP, digest, signer, or send ID; use Workers
HTTP-status/invocation metrics for aggregate `401`, `429`, and `503` pressure
and exception monitoring. Status alone does not identify the front-door lane:
provider throttling also returns `429`, while a provider-ambiguous result or a
dark gate may return `503`. Use the bounded response `error_code` from a
controlled, signed probe to distinguish those cases; do not add per-denial
request logs. Cloudflare Workers Observability is enabled and version preview
URLs are disabled (`preview_urls=false`). The production `workers.dev` endpoint
is still public. Cloudflare
Access with a cell-held service token is the next stronger ingress boundary
before a broad cohort; a Service Binding by itself cannot be called directly
by the Kubernetes cell.

Lifecycle events enter a Queue, are reduced to identifiers, class, and time,
and are forwarded to the authorized cell. Sender, recipient, subject, SMTP
responses, complaint text, and bounce reasons are not forwarded.

## Required resources

1. `send.witmail.net` must be an onboarded Email Sending domain in the same
   production Cloudflare account as this Worker. Cloudflare DNS is required.
2. The Worker must have the `EMAIL` send binding, `RECEIPTS` and
   `PROVIDER_ROUTES` Durable Objects, the configured Queue consumer, and the
   `DISPATCH_FRONTDOOR_LIMITER` Rate Limiter binding with namespace `2301` and
   exactly 1,000 requests per 60 seconds.
3. The lifecycle Queue and dead-letter Queue must exist before deployment.
4. A domain-scoped `email.sending` event subscription must target the lifecycle
   Queue.

## Operator setup

Use the production Cloudflare account that owns `witmail.net`; a successful
login to another account is not sufficient. Complete the Email Service domain
onboarding for `send.witmail.net` before this procedure. The commands below
assume Wrangler `4.123.0` from this directory's lockfile.

Set only non-secret operator context in the shell, verify the selected account,
and run the local checks:

```bash
cd infra/cloudflare/agent-email-send
export CLOUDFLARE_ACCOUNT_ID='replace-with-32-character-production-account-id'
export CLOUDFLARE_ZONE_ID='replace-with-32-character-lowercase-zone-id'
export WITSELF_EMAIL_SENDING_DOMAIN=send.witmail.net

wrangler_prod() {
  npx --no-install wrangler --config wrangler.template.jsonc "$@"
}

npm ci
npm test
npm run bundle:check

auth_json="$(npx --no-install wrangler whoami \
  --account "$CLOUDFLARE_ACCOUNT_ID" --json)"
printf '%s' "$auth_json" | jq -e --arg id "$CLOUDFLARE_ACCOUNT_ID" '
  .loggedIn == true and
  ([.accounts[]? | select(.id == $id)] | length == 1)
' >/dev/null
```

`whoami` does not accept Wrangler's `--profile` flag. The explicit account ID
and membership assertion above therefore fence both the selected deployment
account and the authenticated operator without relying on a named profile.

The v0.0.253 deployment completed an account-wide read-only inventory and proved
that no other Worker used namespace `2301` before binding it to
`witself-agent-email-send`. Rate Limiter namespace IDs can be shared across
Workers, so repeat that inventory before every future deployment or binding
change and stop if `2301` is unexpectedly reused. The three keys are prefixed
with `witself-agent-email-send.frontdoor.v1` to prevent accidental cross-Worker
counter overlap.

Do not export, echo, paste into command arguments, or save either JSON secret
described below. Wrangler prompts for those values without putting them in
shell history or process arguments.

### 1. Provision the event path first

The lifecycle Queue and dead-letter Queue are deployment dependencies, so
create them **before** the Worker. Provision also creates the Email Service
subscription in its safe disabled state:

```bash
./scripts/manage-events.sh provision
./scripts/manage-events.sh status
wrangler_prod queues info witself-agent-email-send-events
wrangler_prod queues info witself-agent-email-send-events-dlq
```

`provision` is safe to repeat and creates a missing subscription, but it does
not rewrite an existing subscription. Before continuing, inspect `status` and
confirm the exact subscription name `witself-agent-email-send-lifecycle`,
source `email.sending`, six configured lifecycle event classes, domain
`send.witmail.net`, target Queue, and `enabled=false`. Stop on any drift.

### 2. Deploy dark

Use this helper for the first deployment and every gate change. It supplies
all eight plaintext variables explicitly, so a toggle cannot accidentally omit
one while Wrangler replaces the `vars` binding set:

```bash
deploy_gates() {
  local dispatch_enabled="${1:?dispatch gate is required}"
  local receipt_replay_enabled="${2:?receipt-replay gate is required}"
  local event_enabled="${3:?event gate is required}"
  for gate in "$dispatch_enabled" "$receipt_replay_enabled" "$event_enabled"; do
    case "$gate" in
      false|true) ;;
      *) echo "all gates must be true or false" >&2; return 2 ;;
    esac
  done
  wrangler_prod deploy --strict \
    --var DISPATCH_AUDIENCE:witself-agent-email-send \
    --var RECEIPT_REPLAY_AUDIENCE:witself-agent-email-send-receipt-replay \
    --var DISPATCH_REPLAY_WINDOW_SECONDS:300 \
    --var DISPATCH_ENABLED:"$dispatch_enabled" \
    --var RECEIPT_REPLAY_ENABLED:"$receipt_replay_enabled" \
    --var EVENT_DELIVERY_ENABLED:"$event_enabled" \
    --var SEND_DOMAIN:send.witmail.net \
    --var REPLY_DOMAIN:witmail.net
}

deploy_gates false false false
wrangler_prod deployments status --json
```

Record the version ID printed by the deploy. Verify its plaintext bindings,
especially all three `false` gates, before installing secrets. Also require
exactly one `DISPATCH_FRONTDOOR_LIMITER` binding at namespace `2301` with
limit `1000` and period `60`, and require `.metadata.has_preview=false`:

```bash
export DEPLOYED_VERSION_ID='replace-with-version-id-from-deploy-output'
wrangler_prod versions view "$DEPLOYED_VERSION_ID" --json
```

`versions view` does not expose the Workers Observability setting. Verify
observability separately in the Worker settings/API and stop unless it is
enabled; the local bundle gate enforces the committed value but is not evidence
of live state.

## Secrets

Set these through the interactive prompts; never place them in `vars`, Git,
logs, environment variables, files, or deployment output:

- `DISPATCH_SIGNERS_JSON`: one to eight Ed25519 public-key entries, each with
  the exact account-ID cohort that signer may dispatch for.
- `EVENT_TARGETS_JSON`: a bounded `cells` registry plus an independent
  `account_targets` map. Each cell contains its exact HTTPS callback, bearer
  token, and one to 32 current or historical signer key IDs it accepts as
  provenance. The account map contains at most 100 exact
  account-to-current-cell assignments, and the complete UTF-8 JSON value is
  capped at 5 KiB to match the Worker binding limit.

Example shapes with non-secret placeholders:

```json
{
  "founder-cell": {
    "public_key": "<base64-raw-ed25519-public-key>",
    "account_ids": ["acc_aaaaaaaaaaaaaaaa"]
  }
}
```

```json
{
  "cells": {
    "founder-cell": {
      "url": "https://cell.example/v1/internal/agent-email-send:provider-event",
      "token": "<random-callback-token>",
      "accepted_signer_key_ids": ["founder-cell"]
    }
  },
  "account_targets": {
    "acc_aaaaaaaaaaaaaaaa": "founder-cell"
  }
}
```

Provider routes record the account and the signer key that authorized the
original dispatch, but not a fixed callback. At event time,
`account_targets[account_id]` chooses the account's current cell independently
of that historical signer. The chosen cell must still list the recorded key in
`accepted_signer_key_ids`; this is a provenance check, not the routing choice.
The corresponding old public key may leave `DISPATCH_SIGNERS_JSON` once it no
longer authorizes new sends, but its key ID must follow moved accounts in
`EVENT_TARGETS_JSON` until every route it signed has passed the 400-day expiry.
Likewise, retain an account's target through that horizon after its last send;
a plan downgrade or disabled send feature does not make historical complaints
safe to discard.

The 32-key per-cell bound supports more than two signer transitions per month
across the complete route horizon, subject to the 5 KiB total secret bound.
Treat either limit as a rollout blocker: do not drop an unexpired provenance
key or move its account to a cell that omits it. Wait for the oldest route
horizon or deploy a separately reviewed storage/capacity change. Cloudflare's
current [Workers limits](https://developers.cloudflare.com/workers/platform/limits/#environment-variables)
document the 5 KiB environment-variable size.

Install and verify secret **names only** while the Worker remains dark:

```bash
wrangler_prod secret put DISPATCH_SIGNERS_JSON
wrangler_prod secret put EVENT_TARGETS_JSON
wrangler_prod secret list --format json
wrangler_prod deployments status --json
```

Stop unless both names are present, the current version still has all three
gates false, every dispatch signer has the exact intended account-ID cohort,
every account target names its current cell, each cell carries all unexpired
signer provenance for its accounts, and every callback is HTTPS. Never print
the JSON values to verify them.

For an account move, first import the suspended archive and make the destination
callback healthy. In one replacement `EVENT_TARGETS_JSON` value, add the
destination cell if needed, carry every unexpired signer key ID for that
account into the destination cell's `accepted_signer_key_ids`, and repoint only
that account's `account_targets` entry. Install the replacement interactively
with `wrangler_prod secret put EVENT_TARGETS_JSON`. A secret update can create a
new Worker version, so reapply the intended values with `deploy_gates`, verify
that version and the Queue state, and only then activate the imported account.
Keep the source cell entry while any other account still targets it.

## Rollout order

1. Finish Email Service domain setup, then create the Queue, dead-letter Queue,
   and disabled subscription as described above.
2. Deploy the Worker with all three gates false. Install the two secrets and
   verify their names, the exact Founder account cohort, the exact front-door
   binding, disabled version preview URLs, enabled Cloudflare Workers
   Observability, and the dark
   deployment. Stop if namespace `2301` is unexpectedly present on another
   Worker in the Cloudflare account.
3. Deploy the schema-91-compatible cell
   server and two worker replicas with cell outbound dispatch still disabled.
   Observe their health, the logical cell-storage gauges, and the independent
   PostgreSQL/PVC headroom signal before enabling dispatch. Before expanding a
   cohort, require continuous Prometheus scraping, PVC metrics collection,
   Alertmanager routing, and a tested external receiver for those logical and
   physical signals, plus provider-wide backpressure. The v0.0.253 production
   rollout verified the gauges only at a point in time; those continuous
   controls are not installed yet.
4. Enable adapter dispatch only, leaving the receipt proof, event delivery, and
   the subscription disabled:

   ```bash
   deploy_gates true false false
   wrangler_prod deployments status --json
   export DEPLOYED_VERSION_ID='replace-with-new-version-id-from-deploy-output'
   wrangler_prod versions view "$DEPLOYED_VERSION_ID" --json
   ```

5. Enable `worker.agentEmailOutbound` and account policy for only the Founder
   cohort. Send one controlled canary. Verify exactly one durable cell outbox
   transition to accepted and one provider message ID. Open only the temporary
   proof surface:

   ```bash
   deploy_gates true true false
   wrangler_prod deployments status --json
   ```

   Run the released `scripts/run-agent-email-receipt-proof.sh` helper from an
   operator workstation with an explicit kubeconfig, context, and cell. The
   helper creates a separate, short-lived Kubernetes Job from the fully
   converged worker deployment; never run this command with `kubectl exec` in
   a live worker pod. Supply the exact account ID, send ID, and canonical
   `accepted_at` fence. Require an accepted proof with
   `provider_call_started_count=1`. Run the helper a second time with the same
   fence and require the provider count to remain one while
   `verified_replay_count` increments. Then remove the temporary proof surface
   and verify its gate is false:

   ```bash
   deploy_gates true false false
   wrangler_prod deployments status --json
   ```

   A 404, 409, malformed response, counter other than one, or changing provider
   count is a rollout blocker. Never use the ordinary dispatch endpoint as the
   proof: that path owns the real Email Sending boundary.

6. While adapter event delivery and the Queue subscription are still disabled,
   and within 15 minutes of the first canary's `accepted_at`, run the released
   `witself-server agent-email provider-event-canary` command for that exact
   fence against the exact cell. This bounded operator command intentionally
   and permanently changes the disposable canary from `accepted` to `delivered`.
   Require its internal `204`/`204`/`409` replay proof and final exact
   `delivered` state with one receipt. A continuation run with the same fence
   must converge on that same result. Do not use customer mail, and do not
   enable either real event path until this proof is complete.

7. Enable adapter event delivery, verify that version's gates, and only then
   enable the subscription:

   ```bash
   deploy_gates true false true
   wrangler_prod deployments status --json
   export DEPLOYED_VERSION_ID='replace-with-new-version-id-from-deploy-output'
   wrangler_prod versions view "$DEPLOYED_VERSION_ID" --json
   ./scripts/manage-events.sh enable
   ./scripts/manage-events.sh status
   ```

8. Send a **new** second canary. Events emitted while the subscription was
   disabled are not a lifecycle-delivery test. Verify its delivered event is
   folded into the cell exactly once and that both the main Queue and dead-letter
   Queue drain. Do not improvise a replay of a real Queue callback; the isolated
   provider-event canary in step 6 is the idempotency proof.
9. Keep platform and plan rate breakers active before widening either the
   adapter cohort or account policy. During each widening step, watch aggregate
   `401`, `429`, and `503` rates plus worker exceptions. If `429` or `503`
   pressure rises, use a controlled, signed probe and its bounded `error_code`
   to distinguish front-door admission from provider throttling, ambiguity, or
   a dark gate. A confirmed front-door `503` means binding/edge identity failure
   and is a rollout blocker. Do not infer that classification from status-only
   metrics or add unbounded per-request denial logs as a diagnostic shortcut.

## Rollback and shutdown

Rollback is gate-first and forward-only at the cell database:

1. Disable outbound policy for the affected account/cohort and disable
   `worker.agentEmailOutbound` in that cell. Confirm that no new rows are being
   claimed.
2. Stop new provider calls but keep lifecycle delivery available for sends the
   provider already accepted:

   ```bash
   deploy_gates false false true
   wrangler_prod deployments status --json
   ```

3. Let the main Queue drain and reconcile all accepted sends. Inspect the
   Queue, consumer, subscription, and dead-letter Queue as shown below.
4. When no accepted send can still produce an expected event, disable the
   subscription and then make the adapter fully dark:

   ```bash
   ./scripts/manage-events.sh disable
   ./scripts/manage-events.sh status
   deploy_gates false false false
   wrangler_prod deployments status --json
   ```

5. If the adapter code itself must be reverted, first inspect a known-good
   version and confirm it retains header-first verification and the exact
   fail-closed front-door binding. An older version without both protections is
   not safe to receive public traffic; keep dispatch dark and deploy a forward
   fix instead. Then move all traffic to the reviewed version:

   ```bash
   export KNOWN_GOOD_VERSION='replace-with-reviewed-version-id'
   wrangler_prod versions view "$KNOWN_GOOD_VERSION" --json
   wrangler_prod versions deploy "$KNOWN_GOOD_VERSION@100%" --yes \
     --message "agent-email adapter rollback"
   wrangler_prod deployments status --json
   ```

   Keep the account and cell gates off until the reverted version is verified.
   Once a cell reaches schema 91, do not deploy a schema 90 or earlier cell binary
   or attempt a schema downgrade; recover with gates and a forward fix.

### Queue and dead-letter handling

These commands expose configuration and consumer state; use the Cloudflare
Queue metrics/dashboard to confirm backlog depth and retry/dead-letter counts:

```bash
wrangler_prod queues info witself-agent-email-send-events
wrangler_prod queues consumer list witself-agent-email-send-events
wrangler_prod queues info witself-agent-email-send-events-dlq
wrangler_prod queues consumer list witself-agent-email-send-events-dlq
./scripts/manage-events.sh status
```

Wrangler `4.123.0` has no safe command to peek, replay, or drain individual
dead-letter messages. A non-empty dead-letter Queue is an incident: keep it,
reconcile the related value-free cell/provider evidence, and repair or replay
through an explicitly reviewed incident procedure. `queues purge` is deletion,
not draining or replay. It is irreversible and must never be part of normal
rollback. A lifecycle event with no provider route or an event that cannot be
normalized is always retried; the same rule applies to every non-204 cell
response, including validation and idempotency conflicts. After the Queue's
configured retry ceiling it becomes DLQ evidence rather than being silently
acknowledged. Missing routes,
provider schema drift, and malformed lifecycle events therefore require
incident reconciliation instead of destroying possible bounce or complaint
evidence.

Each Queue attempt emits one structured
`witself.agent-email-provider-event-consume-log.v1` record. Its `outcome` is a
fixed low-cardinality reason code: `delivery_disabled`, `normalize_invalid`,
`route_lookup_error`, `route_missing`, `target_config_invalid`,
`target_account_unmapped`, `target_signer_unauthorized`, `cell_fetch_error`,
`cell_http_4xx`, `cell_http_5xx`, `cell_http_other`, `unexpected_error`, or
`acked`. The record contains only the schema, component, outcome, and
`ack`/`retry` disposition. It never contains event or message identifiers,
account/realm/send identifiers, addresses, message content, target URLs,
tokens, HTTP bodies, or raw errors. Use these codes with Queue backlog and DLQ
metrics to locate the failing boundary without exposing mail or tenancy data.

Only after durable reconciliation and explicit operator approval may the DLQ
be discarded:

```bash
wrangler_prod queues purge witself-agent-email-send-events-dlq --force
```

Never purge the main lifecycle Queue during rollout or rollback.

Cloudflare Email Service is currently beta. Its sending domain setup and event
subscription model are documented in the official
[Email Service sending guide](https://developers.cloudflare.com/email-service/get-started/send-emails/)
and
[event subscription reference](https://developers.cloudflare.com/email-service/platform/event-subscriptions/).
