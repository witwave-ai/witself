# Witself Privacy Policy

> **DRAFT — not yet in force.** Skeleton drafted for owner and counsel
> review; not legal advice. Bracketed items are decisions or facts the owner
> must confirm. This page becomes binding only when ratified and published.

**Controller:** Witwave AI for account, billing, and operational data. For
content your agents store, you are the controller and Witwave processes it on
your instructions (see the [DPA](data-processing-addendum.md)).

## What we collect

- **Account data**: email, display name, hashed credentials/tokens, plan and
  billing state. Payment cards go directly to Stripe; we never hold card
  numbers.
- **Content**: what your agents store — open-plane memories/facts/messages
  (plaintext at rest, encrypted in transit and by infrastructure encryption)
  and sealed-plane secrets/TOTP (client-encrypted; we hold ciphertext only and
  cannot decrypt).
- **Operational data**: audit events, value-free usage counters, service logs.
  Inbound agent email is checked for SPF/DKIM/DMARC; verdicts are stored
  value-free.
- We do not use advertising trackers. [Confirm: site analytics, if any.]

## How we use it

To operate, secure, bill, and support the Service; to meet legal obligations.
We do not sell personal data and do not train models on your content.

## Where it lives and who touches it

Data is stored in independent deployment cells and a thin control plane. The
subprocessor list lives in the [DPA](data-processing-addendum.md).
[Confirm data-residency statement per cell region.]

## Retention and deletion

Retention windows and end-of-window behavior are the published
[data-retention policy](../data-retention-policy.md). Deletion on request:
permanent fact/memory deletion is first-class in-product.

## Account closure and erasure

Closing an account revokes access and tombstones the account; backups age out
on their fixed schedule. **[DECISION REQUIRED — erasure posture: either (a)
publicly state that closed-account content is retained per the retention
schedule and purged as backups roll, with per-item permanent deletion
available before close; or (b) commit to a closed-account purge/anonymize
sweep within N days. GDPR right-to-erasure for EU users depends on this
choice.]**

## Your rights

Access and export are first-class in-product. [Counsel: GDPR/CCPA rights
enumeration, lawful bases, SCCs if EU data leaves the region, DPO/contact,
supervisory-authority language — align with the erasure decision.]

## Contact

[Confirm: privacy contact address, e.g. privacy@witwave.ai + postal address.]
