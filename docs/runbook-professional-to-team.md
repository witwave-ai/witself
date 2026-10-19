# Move a Professional account to Team by hand

Status: not rehearsed. No step below has been run as a drill or timed end to end. Verified against `main` at `1209a8c0ecf9` on 2026-10-18; re-check every command against the current tree before relying on it.

[FOUNDER CHECKPOINT]: the founder decides; he runs the step himself or says go first.

[GAP] Gn: an unresolved gap numbered Gn in this file's `## Gaps` list.

This is a manual entitlement exception for an existing Professional subscription.
It is not a self-service paid-to-paid subscription change. Use placeholders only
in shared examples; keep customer details in the customer's support ticket.

## What the implementation does

- **F1:** Self-service purchase supports free-to-paid upgrades. An attempted
  Professional-to-Team upgrade records contact interest instead of changing the
  subscription.
- **F2:** Customer `witself plan status` exposes pending contact interest. Operator
  account-policy output has no pending-contact field or contact queue: **[GAP] G1**.
- **F3:** A plan override changes the effective plan without changing the billing
  subscription. Admin JSON is wrapped in `account_policy`; a saved override is
  not proof that the cell has applied it.
- **F4:** A one-off invoice does not grant Team entitlement. An invoice without
  the managed subscription reference is ignored by entitlement processing.
- **F5:** Never edit the managed subscription to perform this transition.
  Subscription metadata is required; nonempty lookup-key and product identities
  must agree; at least one identity must match. Optional fields need not both be
  present. Subscription-updated events do not implement a paid-to-paid switch.
- **F6:** If the subscription ends, the override outlives it, with no dedicated
  alert: **[GAP] G2**. Clearing the override is durable first and applied second.
  When the effective plan's rank decreases (Team to Professional), the cell runs
  the atomic fit check. If usage does not fit, application is blocked: the cell
  keeps the previously applied plan, `apply_pending` stays true, and reconciliation
  retries until usage fits.
- **F7:** Setting an override does not clear pending contact interest. The
  customer must clear that stale request after verifying the new entitlement.

**[FOUNDER CHECKPOINT]** The founder performs any plans Worker deployment himself.

Approved (2026-10-13): the release chain will deploy the plans Worker right after the control plane, from the same clean tag. This is not yet built. Until then, deploy it by hand from the same clean tag, as [Release and build](release-and-build.md) describes.

## Procedure

1. **[FOUNDER CHECKPOINT]** Trigger: a ticket from a Professional account.
   The customer opens the ticket and includes the request and current
   `witself plan status` output:

   ```sh
   witself account support open --subject 'Professional to Team' --body 'Please review my request to move to Team.'
   witself plan status
   ```

   Contact interest alone does not open a support ticket. Use `TICKET_ID` to
   correlate the request and later decisions; see **[GAP] G1**.

2. **[FOUNDER CHECKPOINT]** Read the requesting customer's account:

   ```sh
   witself-admin account plan-override get --account ACCOUNT_ID --json
   ```

   Require `.account_policy.billing_plan` to be `standard` (Professional),
   `.account_policy.plan` to be `standard`, `.account_policy.plan_override` to
   be null, and `.account_policy.apply_pending` to be false. In the billing
   provider, the founder confirms exactly one live Professional subscription
   with no scheduled cancellation. Resolve conflicting changes before proceeding;
   do not overwrite an unrelated exception.

3. **[FOUNDER CHECKPOINT]** Agree billing for the price difference in the ticket.
   Never change the subscription's price, items or metadata (F5).

4. **[FOUNDER CHECKPOINT]** Set the override:

   ```sh
   witself-admin account plan-override set --account ACCOUNT_ID --plan team --reason 'manual Professional to Team, ticket TICKET_ID' --json
   ```

   While `apply_pending` is true, `plan-override set|get|clear --json` prints
   the JSON, exits 1, and prints this exact line to stderr:

   ```text
   witself-admin: policy is saved but still pending cell application; verify convergence before relying on it
   ```

   This is expected right after `set` and `clear`: the policy is saved and still
   converging. Pending is not a failure. The founder must not re-issue the `set`.
   Re-run the read command until it exits 0 with `.account_policy.apply_pending`
   false and `.account_policy.plan` equal to `team`:

   ```sh
   witself-admin account plan-override get --account ACCOUNT_ID --json
   ```

   Confirm `.account_policy.billing_plan` stays `standard`,
   `.account_policy.applied` is `team`, and the Team override is present.

5. **[FOUNDER CHECKPOINT]** The customer checks with `witself plan status`.
   Its plan and billing lines should be:

   ```text
   plan:     Team (team)
   billing:  Professional (standard)   (effective plan override: Team (team))
   ```

   A stale contact line can still appear at this point; clear it in the next step.

6. **[FOUNDER CHECKPOINT]** The customer confirms that the pending item is the
   stale Team contact request, previews cancellation, then cancels that request
   and checks status again:

   ```sh
   witself plan cancel --dry-run
   witself plan cancel --idempotency-key KEY --yes
   witself plan status
   ```

   Use a fresh operation key for `KEY`, retaining it for retries of this one
   operation. The preview must say `allowed:      yes` and
   `effect:       cancel the current pending plan change`. Stop if the pending
   item is a different billing change. Successful ordinary cancellation prints:

   ```text
   cancelled the pending plan change — your current subscription continues and will keep renewing monthly
   (to stop renewal: witself plan downgrade free)
   ```

   A request already resolved prints
   `pending change was already resolved; no cancellation was applied`.
   Do not run the suggested downgrade as part of this cleanup. Ordinary contact
   cancellation clears the interest without cancelling the provider subscription.
   Confirm Team remains effective, Professional remains the billing plan, and
   there is no pending contact line.

7. **[FOUNDER CHECKPOINT]** Record the outcome in the ticket.

## Exit

1. **[FOUNDER CHECKPOINT]** Read the blocked-application risk first: compare the
   account's current usage with the Professional limits. `witself plan list --json`
   reports the Professional catalog limits; `witself plan status --full` reports
   the account's current effective limits. No single command reports complete
   current account usage against the prospective Professional limits:
   `witself usage` is historical per-agent usage, and admin `account_policy`
   reports limits without usage. Review available capacity reports with the
   customer and reassess the target if the billing plan is no longer Professional.
   The atomic fit check in step 3 decides whether the lower plan can apply.
2. **[FOUNDER CHECKPOINT]** Clear the exception:

   ```sh
   witself-admin account plan-override clear --account ACCOUNT_ID --reason 'End Team exception; support ticket TICKET_ID' --json
   ```

   The same pending note from procedure step 4 applies: JSON can be printed with
   exit 1 after the clear has already been saved.
3. **[FOUNDER CHECKPOINT]** Re-run
   `witself-admin account plan-override get --account ACCOUNT_ID --json` until
   `.account_policy.apply_pending` is false and the command exits 0. The
   `get --json` command itself exits 1 and prints the same stderr line from
   procedure step 4 while `apply_pending` is true; pending is not a failure.
   Confirm `plan_override` is null and both `plan` and `applied` match the
   intended billing plan.
4. **[FOUNDER CHECKPOINT]** If it stays pending, ask the customer for
   `witself plan status --json`. A fit refusal is reported in its top-level
   `apply_blocked` field (also printed as `blocked:` in human status). Admin
   `account_policy` JSON has no blocked-report field. If the report says usage
   exceeds Professional limits, the cell keeps Team until usage fits; the
   override is already cleared. Resolve this with the customer by reducing
   usage or restoring Team with the procedure's `plan-override set` command.
   Pending alone can also mean a delivery error; investigate that separately.
   Never edit the subscription to force the transition (F5).
5. **[FOUNDER CHECKPOINT]** For refunds, follow the
   [refund runbook](refund-runbook.md). Reassess any remaining override as part
   of that review; a subscription ending does not remove it.

## Gaps

1. **[GAP] G1** — Customer status exposes pending contact interest; operator
   account-policy output does not. There is no operator pending-contact queue
   or automatic support-ticket creation. The customer must open the ticket.
2. **[GAP] G2** — An override can outlive the subscription, with no dedicated
   alert tying that exception to the subscription ending. Its owner must track
   the agreed end and review it manually.

Pending delivery does have repository monitoring coverage:
`witself_cp_entitlement_apply_blocked_total` counts blocked reconciliation visits,
and `WitselfEntitlementDeliveryGap` has severity `warning` and covers pending or
unverified delivery across completed scans. These are aggregate observations,
not an individual account's pending age. Its alert capability is disabled by
default and requires each cell's `platform.monitoring.entitlementDelivery.enabled`
setting. This is implementation evidence, not proof of deployed alerting. See
[entitlement delivery monitoring](billing-and-limits.md#entitlement-delivery-monitoring).
