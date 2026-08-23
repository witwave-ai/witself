# Witself Data Processing Addendum

> **DRAFT — not yet in force.** Skeleton drafted for owner and counsel
> review; not legal advice. Bracketed items are decisions or facts the owner
> must confirm. This page becomes binding only when ratified and published.

This DPA applies where Witwave processes personal data in your content on
your behalf. [Counsel: full GDPR Art. 28 terms, SCC incorporation, audit
rights, breach-notice window (72h to customer), deletion-on-termination
aligned with the published erasure posture.]

## Processing details

- **Subject matter/duration**: operation of the Service for the account term.
- **Nature/purpose**: storage, retrieval, replication, backup, serving of
  agent state on documented instructions.
- **Data categories**: whatever personal data your agents store (you control
  this); account/operator contact data.
- **Security measures**: per-plane encryption posture (sealed plane
  client-encrypted, ciphertext-only at the server), infrastructure
  encryption, audit ledger, access controls, bounded retention sweeps.

## Subprocessors

| Subprocessor | Role | Confirm |
|---|---|---|
| Cloudflare | Control plane (Workers/KV/R2), inbound/outbound agent email edge | ✔ in use |
| Civo | Kubernetes cells (compute + in-cell Postgres) | ✔ in use |
| Stripe | Billing and payments | ✔ in use |
| GitHub | Source hosting and CI | ✔ in use (no tenant content) |
| PagerDuty | Operational alerting (no tenant content) | [pending activation] |
| [AWS] | [confirm: any production role, or sandbox only] | [confirm] |

[Confirm the list, add legal entity names + regions; notice mechanism for
subprocessor changes (email, 30 days).]
