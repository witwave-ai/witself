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

The public dispatch endpoint streams at most a 2 MiB signed JSON envelope
before authentication and rejects a larger body. The decoded text field still
has its independent 256 KiB UTF-8 cap. The larger envelope limit is necessary
because valid one-byte text characters can expand to six-byte JSON escapes.

Lifecycle events enter a Queue, are reduced to identifiers, class, and time,
and are forwarded to the authorized cell. Sender, recipient, subject, SMTP
responses, complaint text, and bounce reasons are not forwarded.

## Required resources

1. `send.witmail.net` must be an onboarded Email Sending domain in the same
   production Cloudflare account as this Worker. Cloudflare DNS is required.
2. The Worker must have the `EMAIL` send binding, `RECEIPTS` and
   `PROVIDER_ROUTES` Durable Objects, and the configured Queue consumer.
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
especially all three `false` gates, before installing secrets:

```bash
export DEPLOYED_VERSION_ID='replace-with-version-id-from-deploy-output'
wrangler_prod versions view "$DEPLOYED_VERSION_ID" --json
```

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
   verify their names, the exact Founder account cohort, and the dark
   deployment.
3. Deploy the schema-89-compatible cell server and two worker replicas with
   cell outbound dispatch still disabled. Observe their health and metrics.
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
   adapter cohort or account policy.

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
   version and confirm its bindings are safe, then move all traffic to it:

   ```bash
   export KNOWN_GOOD_VERSION='replace-with-reviewed-version-id'
   wrangler_prod versions view "$KNOWN_GOOD_VERSION" --json
   wrangler_prod versions deploy "$KNOWN_GOOD_VERSION@100%" --yes \
     --message "agent-email adapter rollback"
   wrangler_prod deployments status --json
   ```

   Keep the account and cell gates off until the reverted version is verified.
   Once a cell reaches schema 89, do not deploy an older cell binary or attempt
   a schema downgrade; recover with gates and a forward fix.

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
