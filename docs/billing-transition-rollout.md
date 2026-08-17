# Billing Transition Dark-Rollout Guard

Status: operator runbook for the first Stripe sandbox plan-transition release.
Production charging and production Stripe webhooks remain dark.

## Supported scope

The currently implemented self-serve transition is deliberately narrow:

- Personal (`free`) to Professional (`standard`) through a Stripe test-mode
  hosted checkout;
- Professional to Personal at the exact subscription period boundary, after
  the account fits the Personal snapshot;
- exact cancellation and replay of those pending hosted or period-end effects;
- read-only invoice, payment, and normalized settled-refund history.

Team and Enterprise remain unavailable for self-serve purchase. Paid-to-paid
changes, Team usage billing, Enterprise contracting, dunning, and creating or
managing refunds are not part of this rollout. Reading a settled refund is not
authority to initiate one. The first canary must use a disposable Stripe
sandbox account in the explicit account cohort; it must not use a founder,
employee, or customer account.

`WITSELF_CP_STRIPE_MODE=test` is mandatory for this run. An empty
`WITSELF_CP_BILLING_ACCOUNT_ALLOWLIST` means zero customer mutations and is the
required steady state before and after the canary. Route availability, a
configured provider, or a successful read is never permission to charge.

`WITSELF_CP_STRIPE_TEST_CLOCK_ID` is an optional, temporary sandbox-acceptance
setting. It may attach only a newly created customer for the one disposable
test cohort to the reviewed Stripe test clock. Keep it absent for ordinary
sandbox use, clear it before adding any broader cohort, and never configure it
with `WITSELF_CP_STRIPE_MODE=live`; startup rejects that combination. An
existing Stripe customer is not retroactively attached, so the period-boundary
canary must start with a fresh disposable customer when a test clock is used.

## Non-rolling compatibility boundary

The released predecessor `v0.0.254` is not compatible with a writer that can
persist a prepared downgrade. Its API path and its background account
reconciler see `CancelPrevious: true` but do not understand the exact prepared
target. Either path can call the old broad `CancelPending` behavior and mutate
provider objects other than the one the new operation prepared. The
`prepared_downgrade_fence` marker does not protect against this predecessor:
`v0.0.254` never consults that exact target before issuing broad cleanup.

Commit `d12af5c7384cb443c3f79910c86b2b597a447e85` is the minimum safe
reader/canceller floor. It binds cleanup to an exact provider object and refuses
targetless cancellation. A release version is not proof of that behavior by
itself; the deployed Git object must contain this commit.

Consequences:

1. Keep the billing mutation cohort empty.
2. Fully stop and drain **all** `v0.0.254` API replicas and **all**
   `v0.0.254` reconciliation/tick workers before the first new writer starts.
3. Do not run `v0.0.254` concurrently with a prepared-downgrade writer, even
   for a one-replica rolling overlap.
4. Do not roll back to `v0.0.254` while any prepared downgrade exists.
5. Before an incompatible rollback, empty the cohort, drain every source API
   and reconciler, prove the hazardous inventory is zero, and keep billing dark
   after rollback.

This is an exclusive stop-and-start cutover, not an ordinary rolling update.
Future releases that both contain the floor may roll together only after the
same inventory shows no targetless, malformed, or post-retry-horizon work.

## Count-only inventory

Take the inventory after the source fleet is drained and before the first
target writer starts. The privileged collector may read the billing R2
registry, but its shared output must contain counts only:

```json
{
  "schema": "witself.billing-rollout-inventory.v1",
  "captured_at": "2026-08-17T22:00:00Z",
  "billing_mutation_cohort_accounts": 0,
  "source_fleet": {
    "api_replicas": 0,
    "reconciler_replicas": 0
  },
  "records": {
    "prepared_downgrades": 0,
    "targetless_pending_changes": 0,
    "malformed_pending_changes": 0,
    "malformed_mutation_receipts": 0,
    "post_retry_horizon_receipts": 0
  }
}
```

Do not put account ids, operation ids, customer ids, provider object ids,
emails, URLs, reasons, claim tokens, object keys, ETags, or raw errors in this
file. Record those only in an access-controlled operator case when a nonzero
count must be investigated. Counts must come from a complete registry scan,
not the bounded reconciler sample or its rotating shard window.

Classify records in this order, retaining the raw object unchanged:

- **Malformed**: JSON cannot be decoded or the record fails current structural
  validation, including mismatched operation, provider-object, phase, or
  effective-boundary fields. Count malformed account pending state separately
  from malformed mutation receipts.
- **Targetless**: provider cleanup is indicated, but no valid exact cancellation
  target exists. This includes a legacy `CancelPrevious` without a valid
  `CancelPreviousTarget`, and a hosted or scheduled effect whose exact provider
  object cannot be proven.
- **Prepared**: a structurally valid downgrade has `ProviderPhase == "prepared"`
  and its prepared effective boundary, provider object, operation, and prepared
  fence all agree. It has not yet been confirmed applied by the provider.
- **Post retry horizon**: a valid nonterminal mutation receipt is at least 23
  hours old and lacks exact terminal account or tombstone evidence.

Terminal receipts and coherent applied pending changes do not enter these
hazard counts. The inventory collector must still validate them while scanning;
an invalid supposedly terminal receipt is malformed. The pending-state and
receipt counts describe different object classes and may refer to the same
operation; never sum them into an alleged unique-operation total.

## Quarantine and reconciliation

Any targetless, malformed, or post-retry-horizon count blocks activation and
every rollout mode. A prepared count blocks activation from, or rollback to,
`v0.0.254`; it is permitted only when both source and target contain the safe
reader/canceller floor.

For every blocked record:

1. Keep `WITSELF_CP_BILLING_ACCOUNT_ALLOWLIST` empty and keep all incompatible
   writers stopped. Do not delete, rewrite, re-index, or manufacture terminal
   state.
2. Open an access-controlled operator case containing the object key, version
   or ETag, exact operation and provider object references, and the read-only
   provider observation. Keep those values out of logs and the count-only
   artifact.
3. Compare the immutable receipt, account fold/tombstone, current cell snapshot,
   and exact Stripe object. Do not infer success from a broad subscription list
   or from absence in a bounded page.
4. Within the automatic retry horizon, retry only the original exact operation
   and idempotency identity. If exact terminal account or tombstone evidence
   already exists, allow the current safe binary to fold that evidence.
5. At or beyond the horizon, or when evidence conflicts, leave the record
   pending and escalate. The current product has no general operator
   terminalization command; a manual R2 edit is not a substitute.
6. Re-run a complete read-only inventory after the case is resolved. Never
   decrement the shared count by hand.

If a prepared downgrade exists during an emergency, forward-fix on a release
at or above the safe floor. Rolling back to `v0.0.254` is not an emergency
recovery option.

## Hermetic preflight

The preflight reads only the local Git graph and the count-only JSON above. It
does not contact or change Cloudflare, Kubernetes, R2, Stripe, or an account.
For the first tagged release containing the floor:

```sh
scripts/billing-transition-rollout-preflight.sh \
  --mode activate \
  --from-version v0.0.254 --from-ref v0.0.254 \
  --to-version "$TARGET_VERSION" --to-ref "$TARGET_VERSION" \
  --inventory billing-rollout-inventory.json
```

Before an explicitly approved rollback:

```sh
scripts/billing-transition-rollout-preflight.sh \
  --mode rollback \
  --from-version "$CURRENT_VERSION" --from-ref "$CURRENT_VERSION" \
  --to-version v0.0.254 --to-ref v0.0.254 \
  --inventory billing-rollout-inventory.json
```

The command fails unless version tags bind to the requested commits, the safe
side contains `d12af5c`, the incompatible-transition cohort and source fleet
are zero, and every hazardous count is zero. The explicit untagged flags exist
only for a reviewed pre-release drill; never use them to deploy an unbound
image. Separately bind the immutable image digest and chart revision to the
same target commit before rollout.

## Activation sequence

1. Confirm Stripe test mode, the reviewed hosted-portal configuration, test
   webhook secret, and production-live key absence. Confirm Team and Enterprise
   remain unavailable in the catalog. If period-boundary acceleration is part
   of the retained canary, set `WITSELF_CP_STRIPE_TEST_CLOCK_ID` only for the
   fresh disposable test customer and record a value-free configuration hash.
2. Set the billing account allowlist to empty and verify mutation previews and
   applies fail closed before receipt, provider, or account writes.
3. Stop every `v0.0.254` API and plan-lifecycle reconciliation process. Verify
   both source replica counts are zero; stopping only the HTTP listener is not
   sufficient.
4. Produce the complete count-only inventory, quarantine any nonzero hazard,
   and retain the source snapshot plus capture time.
5. Run `activate` preflight against the exact release tag and retain its output.
6. Start only target replicas whose image provenance contains the floor. Keep
   the allowlist empty; verify health, billing reads, pending-recovery metrics,
   and that no provider write occurred.
7. After every API and reconciler is on the target, add one disposable sandbox
   account to the allowlist. Exercise setup, Personal to Professional checkout,
   signed webhook replay, exact idempotent retry, Professional to Personal fit
   rejection and fit success, period-boundary scheduling, test-clock advance
   when selected, and exact pending cancellation. Retain value-free results and
   access-controlled Stripe sandbox object evidence.
8. Remove the account from the allowlist immediately after the canary, clear
   `WITSELF_CP_STRIPE_TEST_CLOCK_ID`, and prove the cohort is empty before any
   broader test cohort. Production live mode and production webhooks remain
   disabled.

Abort and return to the empty dark cohort on any unknown record, incomplete
scan, stale inventory, unexpected provider object, response-loss ambiguity,
fit-authority disagreement, webhook signature/replay anomaly, old replica,
test clock outside the single disposable cohort, nonzero recovery backlog, or
attempted Team/Enterprise/refund mutation. Do not choose rollback until the
rollback preflight independently passes.

## Required retained evidence

Retain the release/tag/commit and image digests, the value-free inventory and
capture time, source and target API/reconciler replica counts, preflight output,
empty-cohort proof before and after the run, test-mode/provider configuration
hashes, webhook replay result, plan-fit result, and the final zero-hazard
inventory. Never retain secret values or customer/provider identifiers in the
shared rollout report.
