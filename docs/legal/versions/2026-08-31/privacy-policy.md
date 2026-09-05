# Witself Privacy Policy

**Version 2026-08-31 · Effective 2026-08-31**

**Controller:** Witwave LLC for account, billing, and operational data. For
content your agents store, you are the controller and Witwave processes it
on your instructions (see the [DPA](data-processing-addendum.md)).
**Contact for privacy requests and notices:** legal@witwave.ai.

## What we collect

- **Account data**: email, display name, hashed credentials/tokens, plan and
  billing state. Payment details go directly to Stripe; we never hold card
  numbers.
- **Content**: what your agents store — open-plane memories, facts,
  messages, transcripts, support tickets, and agent email (including inbound
  email sent to your agent's address by third parties), and sealed-plane
  secrets/TOTP (client-encrypted; we hold ciphertext only and cannot
  decrypt).
- **Operational data**: audit events, value-free usage counters, and service
  logs. Inbound agent email is checked for SPF/DKIM/DMARC; verdicts are
  stored value-free.
- **Abuse-prevention data**: at signup we process your IP address for rate
  limiting (daily counters) and verify a Cloudflare Turnstile challenge; a
  fingerprint derived from the signup request is kept to prevent duplicate
  account creation and is replaced with a value-free marker when the account
  is purged.
- We do not use advertising trackers or third-party site analytics. The only
  third-party component served to your browser is Cloudflare Turnstile on
  the signup challenge page. If we ever add analytics, we will update this
  policy first.

## How we use it

To operate, secure, bill, and support the Service, and to meet legal
obligations. Our lawful bases are performance of our contract with you,
our legitimate interest in securing and improving the Service, and legal
obligation. We do not sell or share personal data for advertising, and we
do not train models on your content.

## Where it lives and who touches it

Your agent content is stored in United States deployment cells. Witself's
cell architecture is multi-cloud: cells can run on different infrastructure
providers, and the [DPA](data-processing-addendum.md)'s subprocessor list is
the authoritative, change-notified record of which providers host tenant
content. Currently all content cells run on Civo (Phoenix, Arizona, with a
backup cell in New York). Control-plane account metadata, encrypted
backups, and archives are stored with Cloudflare on its global network. If
you are outside the United States, you are transferring your data to the US
by using the Service.

## Retention and deletion

Retention windows and end-of-window behavior are described in the published
[data-retention policy](https://github.com/witwave-ai/witself/blob/main/docs/data-retention-policy.md); some automated
enforcement lanes are still being enabled, and until a lane is active the
affected data is simply retained. You can permanently delete individual
facts and memories using the product's deletion commands where your
deployment has them enabled, and you can request deletion of any of your
data at any time via legal@witwave.ai — requests are honored within 30
days.

## Account closure and erasure

Closing an account **immediately and permanently disables all access** —
closure is irreversible, and credentials are revoked at closure, so request
any export before you close. After closure, the account's stored content is
deleted and the account record is reduced to a value-free tombstone;
deletion completes within 30 days of closure. Stripe retains financial
records independently as required by law. Copies in encrypted service
backups expire on the backup retention schedule (up to 90 days).

## Your rights

You can access your data in-product, and you may request a copy, a
correction, or deletion of your personal data at any time via
legal@witwave.ai; we respond within 30 days. If you are in the EU/UK, these
mechanisms are how we honor access, rectification, erasure, portability,
and objection requests, and you may also complain to your supervisory
authority. If you are a California resident, the rights above are how we
honor CCPA/CPRA requests; we do not sell or share personal information, so
there is nothing to opt out of. We do not discriminate against you for
exercising privacy rights.

## Breach notice

If a breach of security affects your personal data, we will notify affected
accounts without undue delay after confirming the breach.

## Contact

legal@witwave.ai (privacy requests are handled with priority). Formal legal
notices: same address.
