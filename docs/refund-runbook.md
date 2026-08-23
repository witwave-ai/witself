# Refund Runbook

Status: operational policy and procedure for the general self-service launch.
The customer-facing wording lives in the refund/cancellation legal page; this
document is the operator's side — who qualifies, how a refund is executed, and
what the platform does on its own afterwards. The launch decision record sets
the policy: a **14-day refund window** on new paid subscriptions.

## Policy

- A customer's **first charge** for a plan (the purchase that created the
  subscription) is refundable in full for **14 days** from the charge date, on
  request, no questions asked.
- **Renewal charges** are not covered by the window. Goodwill refunds of a
  renewal (for example, the customer forgot to cancel and asks within a few
  days) are at the operator's discretion; when granted, refund the most recent
  renewal only.
- A refund ends the service: the subscription is cancelled **immediately**
  (not at period end) as part of issuing it. Money back and continued paid
  service are never combined.
- One refund per account per plan. A customer who refunds, re-subscribes, and
  asks again is outside the policy; decline with a pointer to the legal page.
- Disputes/chargebacks are not refunds: never issue a refund for a charge that
  already has an open dispute — the dispute process supersedes, and refunding
  a disputed charge loses both the money and the dispute.

## Determining eligibility

1. Identify the account and its Stripe customer. `witself billing show
   --account NAME` reports the entitled plan and provider linkage;
   `witself billing payments --account NAME --json` lists the newest charges
   (charges positive, refunds negative, successful refunds `refunded`).
2. The eligible charge is the **subscription-creating** charge, at most 14
   days old (compare the charge's `created` date, UTC, to today).
3. Confirm no prior refund rows exist for the account, and no open dispute on
   the charge (Stripe Dashboard → Payments → the charge).

## Executing the refund (Stripe Dashboard — deliberately manual)

There is **no Witself refund verb**: refund mutations are excluded from the
CLI/API surface on purpose, so every refund is an explicit human action in the
provider's own console with its own audit trail.

1. In the Stripe Dashboard (correct mode — live), open the customer.
2. **Cancel the subscription immediately** (Subscriptions → Cancel →
   *immediately*, not at period end). Do this first: cancelling emits
   `customer.subscription.deleted`, and the platform handles the downgrade
   (next section) without any further operator action.
3. **Refund the charge in full** (Payments → the eligible charge → Refund).
   Stripe Tax was calculated on the charge, so refund the **entire amount
   including tax**; a full refund reverses the tax with it. Choose the reason
   `requested_by_customer`.
4. Reply to the customer confirming the refund and that paid service ended;
   card-network posting time (5–10 business days) is on Stripe, not us.

## What the platform does on its own

Cancelling the subscription is the trigger; the rest is the already-tested
dunning/cancellation fold (`TestDunningContractSmartRetriesThenCancellationFoldsToFree`):

- Stripe emits `customer.subscription.deleted`; the signed webhook folds the
  account back to the free plan (Personal), pushes that downgrade to the cell,
  clears the managed-subscription handle, and clears any dunning marker.
- Data is retained under the free plan's limits per the
  [data-retention policy](data-retention-policy.md); nothing is deleted by a
  downgrade. Over-limit resources become read-only rather than destroyed.
- No proration, credit note, or invoice edit is needed for the 14-day full
  refund; Stripe's refund receipt is the customer's document of record.

## Verification

After the webhook lands (seconds, not minutes):

1. `witself billing show --account NAME` — entitled and applied plan report
   the free plan, no managed subscription.
2. `witself billing payments --account NAME` — a negative `refunded` row for
   the full amount. (A refund against a charge older than the bounded page is
   only visible in the Dashboard; for 14-day-window refunds this cannot
   happen.)
3. If the plan did **not** fold within a few minutes, check the control
   plane's webhook delivery in the Stripe Dashboard (Developers → Webhooks →
   the endpoint): a non-2xx delivery retries on Stripe's schedule; a missing
   endpoint or stale signing secret is an incident, not a refund problem.

## Record-keeping

Refunds are rare enough at launch to log by hand: note account, charge id,
refund id, date, and reason in the support thread that requested it. Revisit
if volume ever makes the manual log the bottleneck.
