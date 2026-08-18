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

The exact reader/canceller capability binds cleanup to one provider object and
refuses targetless cancellation. Because normal pull requests are squash-
merged, ancestry of a pre-squash branch commit is not durable release evidence.
The deployed Git tree must instead contain the exact reviewed capability marker
`internal/billing/lifecycle/compatibility/exact-provider-target-v1`; a version
number or marker copied outside that reviewed implementation is not proof.

Consequences:

1. Keep the billing mutation cohort empty.
2. Fully stop and drain **all** `v0.0.254` API replicas and **all**
   `v0.0.254` reconciliation/tick workers before the first new writer starts.
3. Do not run `v0.0.254` concurrently with a prepared-downgrade writer, even
   for a one-replica rolling overlap.
4. Do not roll back to `v0.0.254`; forward-fix on an exact-aware release.

This is an exclusive stop-and-start cutover, not an ordinary rolling update.
The marker proves this exact reader/canceller capability, not arbitrary future
lifecycle, receipt, or bridge compatibility. Any future rolling pair requires
an independent protocol review and a new or updated rollout guard.

## Count-only inventory

Take the inventory after the source fleet is drained and before the first
target writer starts. The privileged collector may read the billing R2
registry, but its shared output must contain counts only:

The repository includes a complete, fenced, read-only collector. It binds an
immutable target release and the exact production Cloudflare/R2 authority,
requires a stopped source-fleet observation before and after the complete R2
scan, and emits this exact shared schema. A hand-authored JSON file or the
bounded reconciler sample is not inventory evidence.

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

### Fenced production collection

Run every step from one immutable tagged release snapshot. Its repository tree
must be read-only, and `PRIVATE_WRANGLER_CONFIG` must be the frozen private
`wrangler.generated.jsonc` created inside that snapshot, with immutable
release-snapshot metadata and mode `0400`. A mutable checkout config is
rejected. Use fresh, normalized absolute paths on a private filesystem for
`INITIAL_SOURCE_FENCE`, `SOURCE_FENCE_BEFORE`, `PROVISIONAL_INVENTORY`,
`SOURCE_FENCE_AFTER`, and `FINAL_INVENTORY`; none may be a symlink or already
exist. The source fences must be regular mode-`0600` files. The inventory
command creates its provisional and final outputs atomically at mode `0600`
and refuses overwrite.

The Cloudflare inspection process must carry a dedicated read-only
`CLOUDFLARE_API_TOKEN` and the exact
`CLOUDFLARE_ACCOUNT_ID=8f0bf04a4e7aab3a8cc60f02cc8c8fdb` identity. Verify
the token's read-only policy outside this command; the collector can validate
identity and observed state, not the token's provider-side grant. Set the
target values from reviewed release evidence, not from the currently returned
provider object:

- `TARGET_APPLICATION_ID`: exact lowercase Cloudflare Container application
  UUID;
- `TARGET_APPLICATION_VERSION`: exact positive Container application version;
- `TARGET_IMAGE_DIGEST`: exact lowercase `sha256:<64-hex>` image digest;
- `TARGET_RELEASE_VERSION`: canonical semantic version without a leading `v`;
- `TARGET_RELEASE_COMMIT`: exact lowercase 40-hex Git commit.

Take the first private lifecycle-disabled attestation with all required target
bindings. `SOURCE_FENCE_SCRIPT` must name the copy in the same immutable release
snapshot as the private config. `umask 077` plus shell no-clobber makes the
redirected source-fence files private and create-only:

```sh
umask 077
set -o noclobber

source_fence() {
  node "$SOURCE_FENCE_SCRIPT" \
    --config "$PRIVATE_WRANGLER_CONFIG" \
    --expected-account-id 8f0bf04a4e7aab3a8cc60f02cc8c8fdb \
    --expected-target-application-id "$TARGET_APPLICATION_ID" \
    --expected-target-application-version "$TARGET_APPLICATION_VERSION" \
    --expected-target-image-digest "$TARGET_IMAGE_DIGEST" \
    --expected-target-release-version "$TARGET_RELEASE_VERSION" \
    --expected-target-release-commit "$TARGET_RELEASE_COMMIT" \
    "$@"
}

source_fence > "$INITIAL_SOURCE_FENCE"
```

The initial artifact is a self-hashed absence attestation, not a usable scan
fence. It must prove an empty mutation cohort, absent lifecycle gate, the exact
target application current, and zero Container rows with a non-null version.
It deliberately reports one possible reconciler until the drain bound has
elapsed. A stopped row with a retained version is still a possible writer and
blocks the ceremony; only an inactive/version-null tombstone is non-writing.
The current target application may remain spawnable with zero rows because a
new instance would receive the currently attested absent bindings.

Wait at least four minutes after that successful initial observation. Then
take `BEFORE`, perform the complete scan, immediately take `AFTER`, and
finalize, in that order:

```sh
source_fence \
  --prior-lifecycle-disabled-attestation "$INITIAL_SOURCE_FENCE" \
  > "$SOURCE_FENCE_BEFORE"

export WITSELF_BILLING_INVENTORY_R2_ENDPOINT=\
'https://8f0bf04a4e7aab3a8cc60f02cc8c8fdb.r2.cloudflarestorage.com'
export WITSELF_BILLING_INVENTORY_R2_BUCKET='witself-control-plane'
export WITSELF_BILLING_INVENTORY_R2_PREFIX='registry/'
: "${WITSELF_BILLING_INVENTORY_R2_ACCESS_KEY:?dedicated read-only key required}"
: "${WITSELF_BILLING_INVENTORY_R2_SECRET_KEY:?dedicated read-only secret required}"

witself-control-plane billing-rollout-inventory scan \
  --source-fence-before "$SOURCE_FENCE_BEFORE" \
  --provisional "$PROVISIONAL_INVENTORY"

source_fence \
  --prior-lifecycle-disabled-attestation "$INITIAL_SOURCE_FENCE" \
  > "$SOURCE_FENCE_AFTER"

witself-control-plane billing-rollout-inventory finalize \
  --source-fence-before "$SOURCE_FENCE_BEFORE" \
  --provisional "$PROVISIONAL_INVENTORY" \
  --source-fence-after "$SOURCE_FENCE_AFTER" \
  --output "$FINAL_INVENTORY"
```

The two R2 credential variables must come from a separately provisioned,
read-only inventory principal and must not reuse
`WITSELF_CP_R2_ACCESS_KEY`/`WITSELF_CP_R2_SECRET_KEY`. Verify and retain its
provider-side read-only policy; the command rejects the ordinary credential
values when they are present but cannot introspect the grant. The endpoint,
bucket, and prefix above are the only accepted production authority.
The scan follows the complete paginated registry listing with strict cursor,
object, and snapshot checks and fails closed instead of truncating if either
the account-object or mutation-receipt class exceeds 1,000,000 objects.

The source helper also accepts `--reviewed-env-file` for a frozen reviewed
empty Wrangler environment file (the default is the operating-system null
device) and `--wrangler-cwd` for an absolute pinned Wrangler working directory.
If either is used, pass the same reviewed value to all three observations.

`BEFORE` and `AFTER` must independently prove the empty cohort, absent
lifecycle gate, zero API/reconciler sources, and zero non-null-version
Container rows. Finalization requires strict `BEFORE < scan start <= scan
completion <= AFTER` ordering and stable account, config, Worker deployment,
binding/secret inventory, Container application, target app/version/image, and
release version/commit/date identity. Inactive tombstone count/hash changes are
allowed only because both endpoints separately prove zero possible writers.
Any failed timing or identity check requires fresh artifact paths and a new
ceremony; never repair an attestation or provisional file.

Retain the initial attestation, both source fences, provisional inventory,
exact private config/release evidence, dedicated credential-policy evidence,
and final inventory in the access-controlled operator case. Only
`FINAL_INVENTORY` is a count-only shared artifact; even though the command
creates it privately, its exact JSON content may be copied into the shared
rollout report. Do not share the source fences or provisional artifact.

This collector closes the implementation gap only. Billing remains dark and
conditional. Activation is still blocked until this ceremony has produced and
retained real zero-hazard production evidence, all configured success, cancel,
and portal-return routes are live on owned HTTPS surfaces, and the complete
Stripe sandbox canary below has been retained.

Do not put account ids, operation ids, customer ids, provider object ids,
emails, URLs, reasons, claim tokens, object keys, ETags, or raw errors in this
file. Record those only in an access-controlled operator case when a nonzero
count must be investigated. Counts must come from a complete registry scan,
not the bounded reconciler sample or its rotating shard window.
Record the exact `captured_at` value in the reviewed cutover ticket immediately
after the scan. The preflight requires that independently supplied value to
match the artifact, preventing an operator from accidentally selecting a
different old zero-count file. This is an exact artifact fence, not a wall-clock
freshness oracle; the operator must still prove no writer ran after capture.

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

Any targetless, malformed, post-retry-horizon, or prepared count blocks this
one-time activation from `v0.0.254`.

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
carrying the exact reader/canceller capability. Rolling back to `v0.0.254` is
not an emergency recovery option.

## Hermetic preflight

The preflight reads only the local Git graph and the count-only JSON above. It
does not contact or change Cloudflare, Kubernetes, R2, Stripe, or an account.
For the first tagged release carrying the capability marker:

```sh
scripts/billing-transition-rollout-preflight.sh \
  --mode activate \
  --from-version v0.0.254 --from-ref v0.0.254 \
  --to-version "$TARGET_VERSION" --to-ref "$TARGET_VERSION" \
  --inventory billing-rollout-inventory.json \
  --expected-captured-at "$CAPTURED_AT"
```

The command fails unless version tags bind to the requested commits, the safe
side carries the exact reader/canceller capability marker, the incompatible-
transition cohort and source fleet are zero, every hazardous count is zero, and
the inventory timestamp equals the operator-supplied exact capture fence. The
explicit untagged flags exist only for a reviewed pre-release drill; never use
them to deploy an unbound image.
Separately bind the immutable image digest and chart revision to the same target
commit before rollout.

## Activation sequence

1. Confirm Stripe test mode, the reviewed hosted-portal configuration, test
   webhook secret, and production-live key absence. Prove all three configured
   HTTPS success, cancel, and portal-return routes exist on an owned surface and
   safely converge back to read-only billing status; a syntactically valid dead
   URL is a blocker. Confirm Team and Enterprise remain unavailable in the
   catalog. If period-boundary acceleration is part of the retained canary, set
   `WITSELF_CP_STRIPE_TEST_CLOCK_ID` only for the fresh disposable test customer
   and record a value-free configuration hash.
2. Set the billing account allowlist to empty and verify mutation previews and
   applies fail closed before receipt, provider, or account writes.
3. Stop every `v0.0.254` API and plan-lifecycle reconciliation process. Verify
   both source replica counts are zero; stopping only the HTTP listener is not
   sufficient.
4. Complete the private initial-attestation, four-minute drain, `BEFORE`, R2
   scan, `AFTER`, and finalize ceremony above. Quarantine any nonzero hazard;
   retain all private fence/provisional evidence and the final count-only
   inventory plus capture time.
5. Run `activate` preflight against the exact release tag and retain its output.
6. Start only target replicas whose image provenance carries the capability
   marker. Keep the allowlist empty; verify health, billing reads, and pending-
   recovery metrics. Startup may perform bounded Stripe product/price catalog
   reconciliation even with an empty account cohort: retain that catalog-write
   evidence separately and prove that no customer, subscription, Checkout,
   invoice, or payment mutation occurred.
7. Before enabling the disposable account, verify the Cloudflare bridge Worker
   is exactly the reviewed target version at 100%, including both alias/domain
   Durable Object bindings and the atomic `:plan-fit-apply` route. Verify the
   account routes to a cell on the target image digest and that the direct
   `:plan` and atomic `:plan-fit-apply` protocols return the reviewed strict
   envelopes under a non-mutating refusal/replay probe. An old Worker or cell is
   a hard abort, not a compatibility mode.
8. After every API and reconciler is on the target, add one disposable sandbox
   account to the allowlist. Exercise setup, Personal to Professional checkout,
   signed webhook replay, exact idempotent retry, Professional to Personal fit
   rejection and fit success, period-boundary scheduling, test-clock advance
   when selected, and exact pending cancellation. Retain value-free results and
   access-controlled Stripe sandbox object evidence.
9. Remove the account from the allowlist immediately after the canary, clear
   `WITSELF_CP_STRIPE_TEST_CLOCK_ID`, and prove the cohort is empty before any
   broader test cohort. Production live mode and production webhooks remain
   disabled.

Abort and return to the empty dark cohort on any unknown record, incomplete
scan, stale inventory, unexpected provider object, response-loss ambiguity,
fit-authority disagreement, webhook signature/replay anomaly, old replica,
test clock outside the single disposable cohort, nonzero recovery backlog, or
attempted Team/Enterprise/refund mutation. Keep the cohort empty and forward-
fix; this guard intentionally provides no rollback mode to `v0.0.254`.

## Required retained evidence

Retain the release/tag/commit, control-plane and cell image digests, cell
version/protocol probes, the exact Cloudflare Worker version or script ETag and
Durable Object binding inventory, the immutable private config identity, the
initial lifecycle-disabled attestation, both source fences, private provisional
inventory, dedicated read-only credential-policy evidence, the value-free final
inventory and capture time, source and target API/reconciler replica counts,
preflight output, empty-cohort proof before and after the run, bounded Stripe
catalog-bootstrap result, test-mode/provider configuration hashes, webhook
replay result, plan-fit result, and the final zero-hazard inventory. Never
retain secret values or customer/provider identifiers in the shared rollout
report, and never attach private fence/provisional artifacts to it.
