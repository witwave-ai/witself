# Data retention policy

This is the customer-facing statement of how long Witself keeps the data an
account produces, and what happens at the end of each window. It **describes the
behavior the code already enforces** — it introduces no new retention product
policy. The authoritative per-domain mechanics live in
[transcript-retention.md](transcript-retention.md),
[message-retention.md](message-retention.md),
[audit-retention.md](audit-retention.md), and the plan matrix in
[billing-and-limits.md](billing-and-limits.md); this page consolidates them.

> **Status:** reflects the implemented behavior as of the commit that adds this
> file. The mechanics below are verified against the cell code and the canonical
> plan matrix; the customer-facing wording is pending owner ratification before
> it is cited as a binding published policy.

## How retention works, in one model

Every retained data class has an independent **account retention policy** — an
age window in days, or *indefinite*. A window is resolved in this order:

1. an explicit **account override** set by an operator, then
2. the **plan default**, then
3. **absent → indefinite retention.**

An absent policy key always means "keep indefinitely"; a window of zero days is
never valid. Overrides are independent of billing: an operator may set a finite
or indefinite window without changing the plan, price, subscription, or invoice
history.

Enforcement is a **bounded background sweep inside the account's own cell**. Each
retention worker runs on a fixed cadence (a pass roughly every 5 minutes, across
16 fair lanes, in small batches), selects records older than the window, and
**hard-deletes** them — the data is removed, not hidden. Sweeps are idempotent,
advisory-locked, and emit **count-only** operational records; they never log
retained content. A record newer than the window is untouched; changing a window
takes effect on the next sweep. An account whose policy is *indefinite* (an
absent window) has nothing to sweep, regardless of worker state.

## Enforcement activation

Retention enforcement is **activated per cell**, not globally. The platform
chart ships every retention worker **disabled by default** and offers a
**preview** stage (which computes and reports what *would* be deleted, deleting
nothing) before an operator switches a class to **enforce**. Consequently the
windows below are actively applied on a given cell only once that cell has the
corresponding worker enabled in enforce mode; elsewhere the same windows are the
declared policy awaiting activation. This is an operational rollout control, not
a change to the retention windows themselves.

## Windows by data class

Defaults follow the plan; every finite window is an operator-overridable account
policy. Values below are the plan defaults from
[billing-and-limits.md](billing-and-limits.md).

| Data class | Personal | Professional | Team | Enterprise | End-of-window action |
|---|---|---|---|---|---|
| **Agent transcripts** | 30 days | 90 days | 365 days | Indefinite (configurable) | Hard delete of the conversation |
| **Agent-to-agent messages** | 30 days¹ | 90 days | 365 days | 365 days (contract-overridable) | Hard delete of the whole inactive thread |
| **Account audit trail** | 365 days² | 365 days² | 365 days² | 365 days² (contract-overridable) | Hard delete by default; see modes below |
| **Raw inbound email (MIME + attachments)** | Not stored¹ | 90 days | 365 days | 365 days (contract-overridable) | Hard delete of the stored message |

¹ Personal includes neither messaging nor inbound email, but still carries
30-day message- and email-retention policies so that a downgrade makes the
mailbox unavailable immediately and then cleans up on a finite grace schedule;
because Personal cannot receive mail, no raw email is stored in the first place.

² Audit retention default is 365 days, set in the platform Helm values; it is a
per-realm operator setting (plan-overridable in managed mode), not currently a
per-plan product tier.

Two safety notes that are part of the enforced behavior, not marketing:

- **Message retention respects memory-provenance holds.** When a message is the
  evidentiary source for a retained memory, the sweep reports the hold and does
  **not** delete the message or its source graph until the hold clears.
- **Defensive maximum.** Finite windows are capped at 36,500 days (100 years) as
  a representation bound only. This is not a product cap: "indefinite" is
  expressed by an absent policy, not by a large number.

## Audit-trail retention modes

The account audit trail supports three operator-selected end-of-window modes
(see [audit-retention.md](audit-retention.md)):

- **`delete`** (default): audit rows older than the window are hard-deleted on
  the scheduled sweep.
- **`archive`**: older rows are exported to the configured object/blob store —
  a redacted, machine-readable archive — before deletion, and no longer count
  toward live stored-audit metering.
- **`hold`**: retention sweeps are suspended (for an active operational
  investigation). A hold is itself an audited operator action.

The sweep emits an `audit.retention.swept` record that summarizes counts only,
never content.

## Legal hold is out of scope at launch

Witself does **not** offer legal hold as a customer-facing feature or guarantee
at launch. The operator `hold` audit-retention mode above exists for internal
operational pauses and is not a customer-requestable legal-hold product; it
carries no eDiscovery, preservation-obligation, or chain-of-custody commitment.
Customers who need a formal legal-hold arrangement should treat it as an
out-of-band contractual matter, not an in-product capability.

## What this page does not cover

- **Memories and facts** are not on an age sweep. They are user-controlled:
  soft-deleted (tombstoned) and reversible within their own retention window,
  and separately subject to permanent deletion on explicit request. See
  [access-policy.md](access-policy.md) and
  [authorization-and-roles.md](authorization-and-roles.md).
- **Backups and pre-migration evidence** follow their own retention, described
  in [backup-and-recovery.md](backup-and-recovery.md); a configured retention
  policy on live data is not a statement about backup lifetime.
- **Billing records** (invoices, subscription history) are retained
  independently of these windows and are not removed by a retention override.
