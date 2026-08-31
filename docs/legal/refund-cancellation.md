# Refunds & Cancellation

**Version 2026-08-31 · Effective 2026-08-31**

- **Auto-renewal, stated plainly.** Paid plans renew automatically each
  month at the price shown when you purchased (plus tax where applicable)
  until you cancel.
- **Cancel anytime.** Run `witself plan downgrade free` or use the billing
  portal. Cancellation takes effect at the end of the paid period; you keep
  paid features until then, then the account continues on the free plan. No
  partial-period proration.
- **14-day refund, first charge.** The charge that starts a paid
  subscription is refundable in full for 14 days from the charge date, on
  request — email legal@witwave.ai from your account email. Billing and
  refund requests are accepted from **every** account, including accounts
  already back on the free plan. A refund ends paid service immediately; the
  account continues on the free plan and content is retained per the
  [data-retention policy](../data-retention-policy.md).
- **A note on `witself plan cancel`**: that command does not cancel your
  subscription — it undoes a pending scheduled plan change. If the pending
  change was your cancellation, running it resumes monthly renewal. To stop
  renewal, use `witself plan downgrade free`.
- **Renewals** are not automatically refundable; ask — goodwill refunds of a
  recent renewal are at our discretion.
- **Failed payments**: renewals are retried automatically; if payment
  ultimately fails the subscription is cancelled and the account continues
  on the free plan. Non-payment itself deletes nothing; free-plan retention
  windows then apply to content as described in the data-retention policy.
- One refund per account per plan. Disputes/chargebacks supersede refunds.

Operator-side procedure: [refund runbook](../refund-runbook.md).
