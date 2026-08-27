# Support Policy

Status: the published managed-cloud support policy for the general
self-service launch. This is the customer-facing promise; the operator's
procedures live beside it ([refund-runbook.md](refund-runbook.md), the
[Support Boundary](governance-and-support.md#support-boundary), and
[self-host-support.md](self-host-support.md) for everything self-hosted).
Where a mechanism is still activating at cutover, the *Launch status* note in
that section says so — the policy never claims a channel that is not live.

## Who has support

Support is a plan entitlement (the catalog's `support` feature):

| Plan | Support |
|---|---|
| Personal (free) | None included. Public docs and community resources. |
| Professional | Included. |
| Team | Included. |
| Enterprise | Included (plan not yet purchasable). |

Fleet operations can additionally enable or disable ticketing per account
(the account's `support_policy`); abuse of the support channel is grounds for
disabling it. Entitlement is enforced at ticket creation: an applied plan
without the support feature cannot open tickets (accounts predating plan
snapshots are not locked out), and the per-account switch remains an
independent operator control.

## Channels

- **In-product tickets** — the durable, authoritative channel:
  `witself support create` / `list` / `show` / `comment` / `close`. Tickets
  and their messages are account-scoped, retained per the
  [data-retention policy](data-retention-policy.md), and included in account
  export.
- **Email** — support@witwave.ai reaches the same queue. *Launch status: the
  intake bridge activates at cutover; until then the in-product channel is
  the supported path.* Email intake requires a DMARC-authenticated sender whose
  address matches the account contact email.

## The promise

**First response within 1 business day** (Monday–Friday, excluding US federal
holidays). Responses usually arrive much sooner, at any hour — the first
responder is an AI assistant on duty around the clock.

## How it is staffed: AI-first, humans for what matters

First-line support — triage, questions, diagnostics, how-to, known-issue
answers — is handled by an AI assistant. Three commitments come with that:

1. **Labeled, always.** Assistant replies are attributed to the assistant in
   the ticket thread, never presented as a human.
2. **Read-only.** The assistant can read your ticket, your account's plan and
   status, and service health. It cannot change anything: no plan changes, no
   refunds, no deletions, no settings. Any action that would change account
   state is done by a human after the assistant hands the ticket over.
3. **A fixed escalation set.** These always go to a human, immediately and
   automatically:
   - security incidents,
   - billing and refund disputes,
   - legal matters,
   - data-deletion requests,
   - anything requiring a live change to an account.

You can also just ask for a human at any point; the assistant will not argue.

## Severity

Ticket priority (`low` / `normal` / `high` / `urgent`) is set at creation and
may be adjusted during triage. Reserve `urgent` for service unavailability or
suspected security issues; those are worked first regardless of arrival order.

## What managed support covers

Account and billing help; service availability issues; managed backend
incidents; hosted payment-flow issues; managed identity export, import, and
recovery assistance; and the support-ticket workflow itself.

Not covered: your own agent/client code, prompt engineering, third-party
tools, and anything on the self-hosted side (see
[self-host-support.md](self-host-support.md)).

## Specific paths

- **Refunds** — the published policy is a 14-day full refund on the
  subscription-creating charge; see the customer-facing refund/cancellation
  terms. Refund *disputes* are human-escalated by rule.
- **Security reports** — do not open a public issue; see
  [Security Reporting](governance-and-support.md#security-reporting).
  Security reports through support are treated as `urgent` and
  human-escalated.
- **Data deletion** — human-escalated by rule; the erasure posture is
  documented in the [data-retention policy](data-retention-policy.md).

## Incident communications

When a managed-service incident affects your account, the durable public
record is the incident log: GitHub issues labeled
[`incident`](https://github.com/witwave-ai/witself/issues?q=label%3Aincident)
on the public repository. Each incident issue carries timeline updates and
the resolution note; watch the repository (or that label) to be notified as
updates post.

During an incident, the in-product support channel remains the right place to
report impact and ask about your account; `urgent` tickets are worked first.
A full hosted status page is deliberately deferred until after launch — the
incident label is the canonical channel until one exists.

Security incidents follow the private path in
[Security Reporting](governance-and-support.md#security-reporting). Public
incident issues never carry account data, tokens, or any open- or
sealed-plane content — value-free operational language only.
