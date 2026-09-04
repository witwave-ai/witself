# Launch readiness — post-launch record

Witself is in **general self-service production**. This document records the
launch that completed on 2026-08-31 and the short list of follow-through that
still needs Scott directly. It is no longer a pre-launch execution packet.

The operating authority model remains unchanged: Claude decides reasonable
defaults and executes code, docs, config, tests, and deploys with the existing
release discipline. Scott is needed for product decisions, billable resource
approval, new external credentials or accounts, and actions reserved by the
safety rules.

## Launch record

- **Open signup — 2026-08-29 (#295).** The reviewed activation set
  `CP_SIGNUP_OPEN=true` together with daily quotas of 10 signups per source IP
  and 500 globally. Invite-less CLI signup is live with Turnstile and explicit
  `--accept-terms`; setting the gate back to `false` remains the invite-only
  rollback.
- **Billing general availability — 2026-08-31 (#307, v0.0.267).** The control
  plane projects the general-availability flag into the billing service, with
  the fail-closed refusal of a simultaneous cohort allowlist or Stripe test
  clock. Open signup plus open paid billing made Witself fully self-service.
- **Customer contract — 2026-08-31 (#309/#311).** The five Version 2026-08-31
  legal pages were ratified and published, the CLI can read them with
  `witself legal`, and signup records the accepted Terms and Privacy versions.
- **Erasure posture — 2026-08-31 (#312/#313).** In-product fact deletion is
  enabled and the 30-day closed-account purge runs in enforce mode on both
  production cells after its preview review.

## Current needs-Scott list

- Decide the public web signup entry point. The CLI signup path is already live.
- Approve the billable managed-PostgreSQL resources and spend needed to run the
  protected AWS/GCP/Azure 3-by-3 certification in
  [issue #44](https://github.com/witwave-ai/witself/issues/44).
- Complete the real authenticated Cursor and Grok Build acceptance legs in
  [issue #45](https://github.com/witwave-ai/witself/issues/45). Codex and Claude
  Code are certified; the four-runtime gate remains open until the other two
  legs pass.
