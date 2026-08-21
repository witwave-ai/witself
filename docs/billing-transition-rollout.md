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
`WITSELF_CP_BILLING_ACCOUNT_ALLOWLIST` means zero customer mutations at
runtime. The stronger rollout steady-state proof requires the corresponding
`CP_BILLING_ACCOUNT_ALLOWLIST` Worker secret to be absent before and after the
canary; a present secret whose value is empty is not accepted by the source
fence. Route availability, a configured provider, or a successful read is
never permission to charge.

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

The canonical operator entry point is the frozen tagged copy of
`scripts/capture-billing-rollout-inventory.sh`. Do not reproduce its timing or
subcommands by hand. Create its input with
`createControlPlaneReleaseSnapshot` from
`infra/cloudflare/control-plane/scripts/control-plane-release-snapshot.mjs`,
using the exact reviewed tag identity and the normal reviewed render/validation
hooks. A hand-built directory or mutable checkout is not a release snapshot.

The operator driver owns the returned snapshot for the entire capture. Keep its
mode-`0700` root, mode-`0555` repository, private work directory, and frozen
mode-`0400` `wrangler.generated.jsonc` in custody; do not call the snapshot's
`cleanup()` while the wrapper is running. Retain the generator's
`inventory.source_sha256` evidence with the successful capture, then clean up
the snapshot only after the retained evidence has been transferred to its
access-controlled case. That source hash binds the frozen tagged tree,
including the wrapper and source-fence helper. A mutable config is rejected.

`CAPTURE_WORK_DIR` must be a fresh, absent, normalized absolute directory path
on a private filesystem. `FINAL_INVENTORY` must be a different fresh, absent,
normalized absolute file path whose real parent already exists. The wrapper
creates the work directory at mode `0700` and every evidence file at mode
`0600`; it refuses overwrite and removes only its own fixed artifacts after a
failed capture. If an output appears during a failed finalize race and the
wrapper cannot prove that it owns that path, quarantine it rather than deleting
or reusing it. Every retry requires new absent work-directory and output paths.

The Cloudflare inspection process must carry a dedicated read-only
`CLOUDFLARE_API_TOKEN` and the exact
`CLOUDFLARE_ACCOUNT_ID=8f0bf04a4e7aab3a8cc60f02cc8c8fdb` identity. Verify
the token's read-only policy outside this command; the collector can validate
identity and observed state, not the token's provider-side grant. Set the
target values from reviewed release evidence, not from the currently returned
provider object. The frozen source helper performs only bounded, no-redirect
bearer-authenticated GETs against the fixed `https://api.cloudflare.com`
production API origin for the exact Worker and Container resources. It does
not resolve or invoke Wrangler from `PATH`:

- `TARGET_APPLICATION_ID`: exact lowercase Cloudflare Container application
  UUID;
- `TARGET_APPLICATION_VERSION`: exact positive Container application version;
- `TARGET_IMAGE_DIGEST`: exact lowercase `sha256:<64-hex>` image digest;
- `TARGET_RELEASE_VERSION`: canonical semantic version without a leading `v`;
- `TARGET_RELEASE_COMMIT`: exact lowercase 40-hex Git commit;
- `CONTROL_PLANE_BINARY`: exact target `witself-control-plane` executable;
- `CONTROL_PLANE_BINARY_SHA256`: separately reviewed lowercase SHA-256 of that
  target binary. Do not derive the expected value from the binary being tested.

Obtain the binary only from the target tag's
`witself-control-plane_<version>_<os>_<arch>.tar.gz` GitHub Release asset. Verify
the archive against the keyless-signed `checksums.txt` and verify its GitHub
build-provenance attestation before extraction. The release contract guarantees
one root `witself-control-plane` executable and stamps it with the full 40-hex
release commit. After those checks, extract into a fresh private directory,
review and retain the executable's SHA-256 independently, and use that retained
value as `CONTROL_PLANE_BINARY_SHA256`. Do not use an untagged build, a binary
copied from a container, or a digest calculated from an unverified archive.
There is intentionally no Homebrew formula for this operator evidence binary.

The review procedure below assumes Bash, a fresh private destination, and a
native macOS/Linux archive. It verifies only the selected archive entry in the
manifest, because the other release payloads are intentionally not downloaded:

```sh
set -euo pipefail
umask 077

TARGET_RELEASE_TAG="v${TARGET_RELEASE_VERSION}"
TARGET_OS="$(go env GOOS)"
TARGET_ARCH="$(go env GOARCH)"
ARCHIVE="witself-control-plane_${TARGET_RELEASE_VERSION}_${TARGET_OS}_${TARGET_ARCH}.tar.gz"
DOWNLOAD_DIR="$(mktemp -d)"
EXTRACT_DIR="${DOWNLOAD_DIR}/extracted"
mkdir -m 0700 "$EXTRACT_DIR"

TAG_COMMIT="$(gh api \
  "repos/witwave-ai/witself/commits/${TARGET_RELEASE_TAG}" --jq .sha)"
test "$TAG_COMMIT" = "$TARGET_RELEASE_COMMIT"

gh release download "$TARGET_RELEASE_TAG" \
  --repo witwave-ai/witself \
  --dir "$DOWNLOAD_DIR" \
  --pattern "$ARCHIVE" \
  --pattern checksums.txt \
  --pattern checksums.txt.sigstore.json

cosign verify-blob \
  --bundle "$DOWNLOAD_DIR/checksums.txt.sigstore.json" \
  --certificate-identity \
  "https://github.com/witwave-ai/witself/.github/workflows/release.yml@refs/tags/${TARGET_RELEASE_TAG}" \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  "$DOWNLOAD_DIR/checksums.txt"

sha256_file() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | awk '{print $1}'
  else
    shasum -a 256 "$1" | awk '{print $1}'
  fi
}
EXPECTED_ARCHIVE_SHA="$(awk -v name="$ARCHIVE" '
  $2 == name { count++; digest=$1 }
  END { if (count != 1) exit 1; print digest }
' "$DOWNLOAD_DIR/checksums.txt")"
test "$EXPECTED_ARCHIVE_SHA" = "$(sha256_file "$DOWNLOAD_DIR/$ARCHIVE")"

gh attestation verify "$DOWNLOAD_DIR/$ARCHIVE" \
  --repo witwave-ai/witself \
  --signer-workflow witwave-ai/witself/.github/workflows/release.yml \
  --signer-digest "$TARGET_RELEASE_COMMIT" \
  --source-ref "refs/tags/${TARGET_RELEASE_TAG}" \
  --source-digest "$TARGET_RELEASE_COMMIT" \
  --deny-self-hosted-runners \
  --format json >"$DOWNLOAD_DIR/archive-provenance.json"
test -s "$DOWNLOAD_DIR/archive-provenance.json"

test "$(tar -tzf "$DOWNLOAD_DIR/$ARCHIVE")" = witself-control-plane
tar -xzf "$DOWNLOAD_DIR/$ARCHIVE" -C "$EXTRACT_DIR"
CONTROL_PLANE_BINARY="$EXTRACT_DIR/witself-control-plane"
test -f "$CONTROL_PLANE_BINARY"
test ! -L "$CONTROL_PLANE_BINARY"
test -x "$CONTROL_PLANE_BINARY"
VERSION_OUTPUT="$("$CONTROL_PLANE_BINARY" version)"
[[ "$VERSION_OUTPUT" =~ ^witself-control-plane\ ([0-9]+\.[0-9]+\.[0-9]+)\ \(commit\ ([0-9a-f]{40}),\ built\ ([0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z)\)$ ]]
test "${BASH_REMATCH[1]}" = "$TARGET_RELEASE_VERSION"
test "${BASH_REMATCH[2]}" = "$TARGET_RELEASE_COMMIT"
TARGET_RELEASE_DATE="${BASH_REMATCH[3]}"
CONTROL_PLANE_BINARY_SHA256="$(sha256_file "$CONTROL_PLANE_BINARY")"
```

Retain `TARGET_RELEASE_DATE` as the exact reported UTC build second. A second
reviewer must compare and retain
`CONTROL_PLANE_BINARY_SHA256` before it is supplied to the capture wrapper; do
not calculate the expected digest inline in the wrapper invocation. Keep the
verified archive, signed manifest, provenance result, extracted binary, and
review record together in the private operator case.

The wrapper checks the binary digest, its reported release version/commit, its
file identity, and its non-group/world-writable executable mode throughout the
capture.
Set the exact production R2 authority and inject the dedicated credentials,
then run:

```sh
export WITSELF_BILLING_INVENTORY_R2_ENDPOINT=\
'https://8f0bf04a4e7aab3a8cc60f02cc8c8fdb.r2.cloudflarestorage.com'
export WITSELF_BILLING_INVENTORY_R2_BUCKET='witself-control-plane'
export WITSELF_BILLING_INVENTORY_R2_PREFIX='registry/'
: "${WITSELF_BILLING_INVENTORY_R2_ACCESS_KEY:?dedicated read-only key required}"
: "${WITSELF_BILLING_INVENTORY_R2_SECRET_KEY:?dedicated read-only secret required}"

"$CAPTURE_SCRIPT" \
  --release-snapshot-config "$PRIVATE_WRANGLER_CONFIG" \
  --control-plane-binary "$CONTROL_PLANE_BINARY" \
  --control-plane-binary-sha256 "$CONTROL_PLANE_BINARY_SHA256" \
  --work-dir "$CAPTURE_WORK_DIR" \
  --output "$FINAL_INVENTORY" \
  --expected-account-id 8f0bf04a4e7aab3a8cc60f02cc8c8fdb \
  --expected-target-application-id "$TARGET_APPLICATION_ID" \
  --expected-target-application-version "$TARGET_APPLICATION_VERSION" \
  --expected-target-image-digest "$TARGET_IMAGE_DIGEST" \
  --expected-target-release-version "$TARGET_RELEASE_VERSION" \
  --expected-target-release-commit "$TARGET_RELEASE_COMMIT"
```

`CAPTURE_SCRIPT` must be the copy inside the same frozen snapshot as
`PRIVATE_WRANGLER_CONFIG`. The wrapper derives that snapshot's source-fence
helper and retains the snapshot config and reviewed empty environment file in
its immutable custody hash. The source phase receives no `PATH`; the helper
reads the frozen config identity and uses its source-owned direct Cloudflare
API inspector. The wrapper takes the initial private lifecycle-disabled
attestation, waits the fixed 240-second in-flight bound, takes `BEFORE` using
that initial artifact as its prior, runs `witself-control-plane
billing-rollout-inventory scan`, immediately takes `AFTER` from the same prior,
and runs
`witself-control-plane billing-rollout-inventory finalize`.

The initial artifact is a self-hashed absence attestation, not a usable scan
fence. It must prove an empty mutation cohort, absent lifecycle gate, the exact
target application current, and zero Container rows with a non-null version.
It deliberately reports one possible reconciler until the drain bound has
elapsed. A stopped row with a retained version is still a possible writer and
blocks the ceremony; only an inactive/version-null tombstone is non-writing.
The current target application may remain spawnable with zero rows because a
new instance would receive the currently attested absent bindings.

The two R2 credential variables must come from a separately provisioned,
read-only inventory principal and must not reuse
`WITSELF_CP_R2_ACCESS_KEY`/`WITSELF_CP_R2_SECRET_KEY`. Verify and retain its
provider-side read-only policy; the command rejects the ordinary credential
values when they are present but cannot introspect the grant. The endpoint,
bucket, and prefix above are the only accepted production authority.
The scan follows the complete paginated registry listing with strict cursor,
object, and snapshot checks and fails closed instead of truncating if either
the account-object or mutation-receipt class exceeds 1,000,000 objects.
Only the source-observation subprocesses receive the Cloudflare token, and only
the scan subprocess receives the dedicated R2 access key and secret. Finalize
receives the exact non-secret endpoint/bucket/prefix authority but no
Cloudflare, R2, or Stripe credential and performs no provider read.

`BEFORE` and `AFTER` must independently prove the empty cohort, absent
lifecycle gate, zero API/reconciler sources, and zero non-null-version
Container rows. Finalization requires strict `BEFORE < scan start <= scan
completion <= AFTER` ordering and stable account, config, Worker deployment,
binding/secret inventory, Container application, target app/version/image, and
release version/commit/date identity. Inactive tombstone count/hash changes do
not invalidate the fence because each endpoint independently proves that no
possible writer exists.
Any failed timing or identity check requires fresh artifact paths and a new
ceremony; never repair an attestation or provisional file.

A successful wrapper run retains these exact private evidence paths:

- `$CAPTURE_WORK_DIR/initial-lifecycle-disabled.json`;
- `$CAPTURE_WORK_DIR/source-fence-before.json`;
- `$CAPTURE_WORK_DIR/registry-provisional.json`;
- `$CAPTURE_WORK_DIR/source-fence-after.json`;
- `$FINAL_INVENTORY`.

Retain them with the tagged snapshot generator's `inventory.source_sha256`,
the reviewed control-plane binary SHA-256, exact private config/release
evidence, and dedicated credential-policy evidence in the access-controlled
operator case. Only `$FINAL_INVENTORY` is a count-only shared artifact; even
though the wrapper creates it privately, its exact JSON content may be copied
into the shared rollout report. Do not share the source fences or provisional
artifact.

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

### Canary activation blocker

The repository does not yet contain a supported command that owns the complete
billing canary secret transition. A safe implementation must hold the global
Cloudflare mutation freeze and, as one operation:

1. prove the active Worker is the exact frozen tagged target at 100%;
2. atomically install exactly one disposable-account cohort and the optional
   canonical Stripe test clock without putting either value in argv, logs, or
   a temporary file;
3. attest the one new secret-generated Worker successor has the same script,
   runtime, non-target bindings, routes, Container application/image contract,
   and release variables with only the intended secret-name delta;
4. redeploy the exact tag, restart the singleton Go Container with its fresh
   environment, and fully verify the final Worker and live Go identity before
   handing control to the canary; and
5. on darkening, first remove the cohort and clock atomically, restore and
   verify the empty-cohort Container, then remove the lifecycle-gate binding,
   stop and drain every Container writer, and complete the canonical
   lifecycle-disabled source-fence and final zero-hazard inventory.

Worker secret mutation immediately creates and deploys a successor version,
while an already-running Go Container retains its old environment. Therefore a
sequence of generic `secret:put:break-glass`, `wrangler secret put`, or
`wrangler secret delete` commands cannot satisfy this boundary. Do not use
those commands for the cohort or test clock, or for the lifecycle gate as part
of this billing canary and darkening ceremony. The separately reviewed
nonbilling lifecycle activation remains governed by its own runbook. Until the
atomic transition and darkening orchestrator exists and is hermetically tested,
stop after step 7 below and keep billing dark.

## Activation sequence

1. Confirm Stripe test mode, the reviewed hosted-portal configuration, test
   webhook secret, and production-live key absence. Prove all three configured
   HTTPS success, cancel, and portal-return routes exist on an owned surface and
   satisfy the [value-free return-page contract](billing-return-pages.md); a
   syntactically valid dead URL is a blocker. Confirm Team and Enterprise remain
   unavailable in the catalog. If period-boundary acceleration is part of the
   retained canary, select and review a Stripe test clock for a future fresh
   disposable test customer and retain only its value-free configuration hash.
   Do not configure `WITSELF_CP_STRIPE_TEST_CLOCK_ID` or write the corresponding
   Worker secret while the activation blocker above remains open.
2. Prove the billing account allowlist and lifecycle-gate Worker secrets are
   absent, and separately prove the optional test-clock binding is absent.
   Verify mutation previews and applies fail closed before receipt, provider,
   or account writes. If any binding is present, abort this attempt and restore
   the reviewed dark source state before starting a fresh capture; do not
   mutate it inside the source-fence interval.
3. Stop every `v0.0.254` API and plan-lifecycle reconciliation process. Verify
   both source replica counts are zero; stopping only the HTTP listener is not
   sufficient.
4. Run the canonical capture wrapper, which owns the private initial
   attestation, fixed four-minute drain, `BEFORE`, R2 scan, `AFTER`, and
   finalize ceremony. Quarantine any nonzero hazard; retain the tagged source
   hash, binary hash, all private fence/provisional evidence, and final
   count-only inventory plus capture time.
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
8. **Blocked on current main.** Only the future reviewed atomic orchestrator may
   install one disposable sandbox account and optional test clock. After it
   proves the exact Worker and refreshed Go Container, exercise setup, Personal
   to Professional checkout, signed webhook replay, exact idempotent retry,
   Professional to Personal fit rejection and fit success, period-boundary
   scheduling, test-clock advance when selected, and exact pending
   cancellation. Retain value-free results and access-controlled Stripe
   sandbox object evidence.
9. That same orchestrator must darken before it returns success: atomically
   remove the allowlist and optional test clock so customer mutations fail
   closed, refresh and verify the empty-cohort Container, then remove
   `CP_PLAN_LIFECYCLE_ENABLED`, stop and drain every remaining writer, and prove
   the cohort and lifecycle-gate state through the canonical source fence. It
   must also retain a separate exact test-clock absence attestation bound to
   those fence observations before the final zero-hazard inventory; the current
   source-fence artifact does not expose that secret name. Production live mode
   and production webhooks remain disabled.

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
tagged snapshot `inventory.source_sha256`, reviewed control-plane binary
SHA-256, initial lifecycle-disabled attestation, both source fences, private
provisional inventory, dedicated read-only credential-policy evidence, the
value-free final inventory and capture time, source and target API/reconciler
replica counts, preflight output, empty-cohort proof before and after the run,
bounded Stripe catalog-bootstrap result, test-mode/provider configuration
hashes, webhook replay result, plan-fit result, and the final zero-hazard
inventory. Never retain secret values or customer/provider identifiers in the
shared rollout report, and never attach private fence/provisional artifacts to
it.
