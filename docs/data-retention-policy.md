# Data retention policy

This is the customer-facing statement of how long Witself keeps the data an
account produces, and what happens at the end of each window. It distinguishes
implemented mechanics and production enforcement from documented policy awaiting
implementation or activation. The per-domain mechanics and design live in
[transcript-retention.md](transcript-retention.md),
[message-retention.md](message-retention.md),
[audit-retention.md](audit-retention.md), and the plan matrix in
[billing-and-limits.md](billing-and-limits.md); this page consolidates them.

> **Status:** PUBLISHED as the operator's retention policy for the mechanics
> described here. The checked-in production cell values enable account purge in
> `enforce` mode on both Civo cells and agent-email retention in `enforce` mode on
> the serving cell only; transcript and message retention are inactive. Audit
> retention is documented policy, not yet implemented or enforced. The exact
> activation flags are listed below. Formal ratification/signature of the
> customer-facing wording remains an owner action.

## How retention works, in one model

Transcripts, messages, and agent email each have an independent **account
retention policy** — an age window in days, or *indefinite*. Their policy keys are
`transcript_retention_days`, `message_retention_days`, and
`agent_email_retention_days`. A window is resolved in this order:

1. an explicit **account override** set by an operator, then
2. the **plan default**, then
3. **absent → indefinite retention.**

For these three classes, an absent policy key means "keep indefinitely"; a window
of zero days is never valid. Overrides are independent of billing: an operator
may set a finite or indefinite window without changing the plan, price,
subscription, or invoice history.

When enabled in `enforce` mode, each implemented age-retention worker runs a
**bounded background sweep inside the account's own cell**, across 16 lanes in
small batches. The cadence is configurable: the chart default is 5 minutes;
agent-email retention on the serving production cell uses 1 minute. Eligible
records older than the window are **hard-deleted**, subject to each class's
eligibility and hold checks. Sweeps use database row locks and durable lane
coordination, and emit **count-only** operational records; they never log retained
content.
Changing a window affects subsequent sweeps as they reach eligible records. An
absent window disables age-based content deletion for that class; email
recipient-suppression digests have a separate bounded lifetime.

## Enforcement activation

Retention enforcement is **activated per cell**, not globally. The platform
chart ships each implemented retention worker **disabled by default** and offers
a **preview** stage (which computes and reports what *would* be deleted, deleting
nothing) before an operator switches a class to **enforce**. Consequently the
transcript, message, and agent-email windows below are actively applied on a given
cell only once that cell has the corresponding worker enabled in enforce mode;
elsewhere the same windows are the declared policy awaiting activation. This is
an operational rollout control, not
a change to the retention windows themselves.

The checked-in production configuration is:

| Retention class / chart flags | `civo-sandbox-usw2-dev` (serving) | `civo-sandbox-use1-backup` (rollback) |
|---|---|---|
| Account closure: `worker.accountPurge.enabled` / `.mode` | `true` / `enforce` | `true` / `enforce` |
| Agent email: `worker.agentEmailRetention.enabled` / `.mode` | `true` / `enforce` | `false` / `preview` (inactive defaults) |
| Transcripts: `worker.transcriptRetention.enabled` / `.mode` | `false` / `preview` (inactive defaults) | `false` / `preview` (inactive defaults) |
| Messages: `worker.messageRetention.enabled` / `.mode` | `false` / `preview` (inactive defaults) | `false` / `preview` (inactive defaults) |
| Audit trail | No implemented retention worker or chart flag | No implemented retention worker or chart flag |

Both cells set `worker.enabled: true`. Cell overrides live under
`apps.witselfServer.worker`; omitted settings inherit the apps/server chart
defaults. A disabled worker does not run preview sweeps. The audit window and
modes below are documented policy awaiting implementation, not an active preview
lane. Account purge still removes audit events as described below.

## Account closure and erasure

Account closure is irreversible and revokes credentials; request an export
before closing. Closed accounts become eligible for purge after a 30-day grace
window. The closed-account purge removes the account's stored content regardless
of the age windows below.
The purge covers:

- open-plane content, including memories, facts, messages, and transcripts;
- sealed-plane ciphertext, including encrypted secrets and vault material;
- inbound and outbound agent email and attachments;
- support tickets and their messages;
- usage events; and
- every account audit event except the single value-free `account.purged`
  record written after the other audit events are deleted.

The account row remains only as a value-free tombstone. It retains the stable
account ID, closed/default markers, creation, closure, and purge timestamps,
any historical suspension timestamp and support-policy marker, the plan label
and plan-application snapshot metadata, default placement policy,
last-evacuation bookkeeping, and a zero retained-email-attachment byte count.
Email is null; display name and closure reason are blank; suspension target and
reason are cleared; and plan limits, features, and policies are emptied. Consent
version labels and their recording timestamp are cleared with the rest of the
account content. Stripe retains financial records independently. Backup copies
are not removed by account purge: R2 archive expiration requires separately
configured bucket lifecycle rules, and pre-migration dumps follow the operator's
reviewed backup-retention procedure.

The cell keeps its value-free provisioning retry receipt so a delayed request
cannot recreate the account. Purge replaces the receipt's contact-derived
request fingerprint with a fixed non-contact marker.

Like the retention workers above, the account-purge worker is activated per
cell. The platform chart ships it disabled and in `preview` mode by default;
both production Civo cells override `worker.accountPurge.enabled: true` and
`worker.accountPurge.mode: enforce`, with `worker.accountPurge.grace: 720h`
inherited from the chart. On other cells, preview deletes nothing and does not
anonymize the account; purge requires explicit enforcement activation.

## Windows by data class

Transcript, message, and agent-email defaults follow the plan and are
operator-overridable account policies. Values below come from
[billing-and-limits.md](billing-and-limits.md).

| Data class | Personal | Professional | Team | Enterprise | End-of-window action |
|---|---|---|---|---|---|
| **Agent transcripts** | 30 days | 90 days | 365 days | Indefinite (configurable) | Hard delete of the conversation |
| **Agent-to-agent messages** | 30 days¹ | 90 days | 365 days | 365 days (contract-overridable) | Hard delete of the whole inactive thread |
| **Account audit trail (policy only; inactive)** | 365 days² | 365 days² | 365 days² | 365 days² | Planned hard delete by default; no age sweep is implemented |
| **Raw inbound email (MIME + attachments)** | No new inbound mail¹ | 90 days | 365 days | 365 days (contract-overridable) | Hard delete of the stored message |

¹ Personal includes neither messaging nor inbound email, but still carries
30-day message- and email-retention policies so that a downgrade makes the
mailbox unavailable immediately. Previously stored messages and email remain
subject to those windows when enforcement is active; Personal cannot receive
new inbound mail.

² The documented audit-retention policy calls for a 365-day default and
operator-selected modes. Neither the window nor those modes has an implemented
Helm value or account policy key; production does not enforce this age window.

Two safety notes about the implemented policy and sweep behavior:

- **Message retention respects memory-provenance holds.** When a message is the
  evidentiary source for a retained memory, the sweep reports the hold and does
  **not** delete the message or its source graph until the hold clears.
- **Defensive maximum.** Finite windows are capped at 36,500 days (100 years) as
  a representation bound only. This is not a product cap: "indefinite" is
  expressed by an absent policy, not by a large number.

## Audit-trail retention modes

The documented audit-retention policy defines three planned operator-selected
end-of-window modes (see [audit-retention.md](audit-retention.md)). None is
implemented or enabled in production:

- **`delete`** (planned default): audit rows older than the window would be
  hard-deleted on the scheduled sweep.
- **`archive`**: older rows would be exported to the configured object/blob store —
  a redacted, machine-readable archive — before deletion, and would no longer count
  toward live stored-audit metering.
- **`hold`**: retention sweeps would be suspended (for an active operational
  investigation). The design calls for the hold itself to be audited.

The design calls for an `audit.retention.swept` record summarizing counts only,
never content; no such scheduled sweep runs today.

## Legal hold is out of scope at launch

Witself does **not** offer legal hold as a customer-facing feature or guarantee
at launch. The planned operator `hold` audit-retention mode above is intended for
internal operational pauses and is not a customer-requestable legal-hold product;
it carries no eDiscovery, preservation-obligation, or chain-of-custody commitment.
Customers who need a formal legal-hold arrangement should treat it as an
out-of-band contractual matter, not an in-product capability.

## What this page does not cover

- **Memories and facts** are not on an age sweep. Memories can be forgotten and
  restored without a configured age window; permanent memory or fact deletion
  is a separate explicit action and is not reversible. See
  [access-policy.md](access-policy.md) and
  [authorization-and-roles.md](authorization-and-roles.md).
- **Backups and pre-migration evidence** follow their own retention, described
  in [backup-and-recovery.md](backup-and-recovery.md); a configured retention
  policy on live data is not a statement about backup lifetime.
- **Billing records** (invoices, subscription history) are retained
  independently of these windows and are not removed by a retention override.
