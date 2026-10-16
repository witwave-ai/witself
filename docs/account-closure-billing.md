# Account closure and billing

This describes the automatic cancellation implementation and its release
requirements. Source and local tests are not evidence of production activation.
The brochure must retain its current cancellation instructions until the
control plane and every managed serving cell have this implementation deployed
and a disposable sandbox account has exercised the complete flow.

## Customer behavior

`witself account close --yes` is owner-only and permanent. Export first. On a
managed cell, it confirms billing cancellation before revoking account access:

- Open checkout links are expired before subscriptions are cancelled, so a
  previously issued checkout cannot start another subscription afterward.
- Managed paid subscriptions are cancelled immediately, including subscriptions
  already scheduled to cancel and those paused or awaiting payment. Closure
  does not issue a refund, a final invoice, or a prorated charge.
- An unavailable provider, ambiguous payment operation, unrecognized provider
  object, or incomplete provider listing refuses closure. Credentials and
  routing remain available for retry. A failed response is not proof of closure.
- Once billing closure starts, new purchases, payment setup, pending-change
  cancellation, and new portal links are disabled. This fence is durable across
  process restarts. Retry account closure to finish; do not try to resume billing.
- The cell checks owner/default-account/vault eligibility both before the
  provider call and in the final closing transaction. If a vault operation starts
  in between, billing can be cancelled while account closure is still blocked.
  Settle the vault operation and retry. No database transaction is held over a
  network call.

Free accounts with no provider state also close through the managed safeguard.
Providerless self-hosted installations retain local account closure. Uninstalling
Witself alone still does not close the account or cancel billing. `witself plan
cancel` cancels a *pending plan change* and must not be presented as subscription
cancellation; it can restore renewal by undoing a scheduled downgrade.

The Stripe adapter uses the [immediate subscription cancellation
API](https://docs.stripe.com/api/subscriptions/cancel) with `invoice_now=false`
and `prorate=false`, followed by provider readback. Previously incurred charges
and separately retained billing records are not erased or refunded by closure.

## Trust and concurrency

The cell calls `POST /v1/accounts/{id}/billing:close` with its provision
credential and the owner's bearer. The Worker checks the routed cell credential,
strips it, and injects the private internal bridge proof. Go requires that proof
and independently checks owner eligibility at the cell. A customer cannot use
this callback to cancel billing while deliberately retaining paid account access.
The purchase cohort gate is not a cancellation gate.

Closure permanently reserves the existing per-account durable billing lane. An
unresolved purchase receipt must settle before closure can own that lane. Even
a crash before the lifecycle record is updated cannot reopen that lane for a
purchase. The durable closure marker also blocks late webhook/reconciler
entitlement application. Only confirmed provider cancellation
sets the completed marker. Retry uses the same provider operation identity.

## Release and verification

1. Pass the full application and infrastructure gates. Exercise provider failure,
   lost replies, checkout completion during expiry, direct cell requests, owner
   authorization, and vault eligibility races in tests.
2. Deploy the new Worker and Go control plane before enabling the new cell
   callback. Stop and drain all previous Go API and reconciliation writers before
   the first closure marker is written: old writers do not understand this fence
   and can discard it when rewriting a record. Do not roll back to an old writer
   after any marker exists; forward-fix. Existing Worker container activation
   must actually restart the Go container on the reviewed image/environment.
3. Upgrade every managed serving cell. Keep `WITSELF_BILLING_ENDPOINT` configured
   and its provision credential available. A new cell with an old/unavailable
   control plane must refuse closure. An old cell still has the former unsafe
   behavior, so mixed cell versions are not complete protection.
4. In an isolated Stripe test-mode environment, use a disposable account and
   verify paid closure, pending checkout closure, lost-response retry, no future
   renewal, revoked tokens, and removed routing. Do not close a real customer
   account or charge a card as a deployment probe.
5. Separately audit accounts closed before this release for surviving provider
   subscriptions or checkout links. This implementation does not retroactively
   cancel them. Review exact account/customer ownership before any remediation;
   report counts only outside the restricted operator workflow.
6. After the deployed proof, update the brochure and `product.md` together to say
   closing an account automatically cancels its paid subscription. Keep the
   distinction that uninstalling alone does not cancel it.
