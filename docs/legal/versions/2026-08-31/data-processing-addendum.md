# Witself Data Processing Addendum

**Version 2026-08-31 · Effective 2026-08-31**

This DPA applies where Witwave LLC processes personal data in your content
on your behalf. It is incorporated into the
[Terms of Service](terms-of-service.md) for every account. Customers who
require EU Standard Contractual Clauses or a countersigned DPA should
contact legal@witwave.ai.

## Processing details

- **Subject matter/duration**: operation of the Service for the account
  term.
- **Nature/purpose**: storage, retrieval, replication, backup, and serving
  of agent state on documented instructions (your API/CLI/MCP calls and
  configuration).
- **Data categories**: whatever personal data your agents store (you
  control this) — including memories, facts, transcripts, messages, support
  tickets, and agent email received from third parties; plus
  account/operator contact data.
- **Security measures**: sealed-plane content is client-encrypted (we hold
  ciphertext only and cannot decrypt); all traffic is encrypted in transit;
  backups are encrypted with keys held offline by the operator; access is
  audited in an append-style audit ledger with access controls. Open-plane
  content is stored on managed provider infrastructure.
- **Deletion on termination**: on account closure, stored content is
  deleted within 30 days as described in the
  [Privacy Policy](privacy-policy.md); encrypted backups expire on the
  backup retention schedule (up to 90 days).
- **Breach notice**: we notify affected customers without undue delay after
  confirming a breach affecting their personal data.
- **Audit**: we make available the information reasonably necessary to
  demonstrate compliance with this DPA on written request.

## Subprocessors

Witself runs on independent deployment cells and is multi-cloud by design:
Witwave may operate cells on additional infrastructure providers (for
example GCP, AWS, or Azure). This table is the authoritative list of
subprocessors **as of this version's date**; any new provider that will host
tenant content is added here, with notice, before use.

| Subprocessor | Entity / region | Role |
|---|---|---|
| Cloudflare, Inc. | US (global network) | Control plane (Workers/KV/R2/Durable Objects), account metadata, encrypted backups and archives, inbound/outbound agent-email edge, signup Turnstile |
| Civo Ltd | US regions (Phoenix, AZ; New York, NY) | Current deployment-cell infrastructure: compute and in-cell PostgreSQL holding agent content |
| Stripe, Inc. | US | Billing and payments (card data goes directly to Stripe) |
| GitHub, Inc. | US | Source hosting, CI, and public incident-comms issues (no tenant content) |
| PagerDuty, Inc. | US | Operational alerting (alert metadata only, no tenant content) |
| SIA Monkey See Monkey Do (Healthchecks.io) | EU | Dead-man heartbeat monitoring (ping timestamps only, no tenant content) |

AWS is used for isolated infrastructure tooling in a sandbox account only
and has no production role and no tenant content.

**Subprocessor changes**: we will update this list and notify account
emails at least 30 days before adding a subprocessor that processes tenant
content.
