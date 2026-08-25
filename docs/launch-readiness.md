# Launch readiness — plan of record

Working plan for the **general self-service** production launch. It is the
decided execution order plus the list of things that need Scott directly. It is
maintained by Claude (production-readiness lead) and updated as slices land.

**Authority model (per Scott, 2026-08-22):** Claude decides reasonable defaults
and executes — code, docs, config, tests, **and deploys** (there are no
customers; Scott is the only internal user, so dogfood deploys are expected and
must not bottleneck). Claude uses tooling already authenticated on the host and
follows release discipline (CI green; pre-migration backup gate before
schema-advancing rolls; `make check` before push; one coordinated tag per arc).
**Only these still need Scott directly:** creating/revealing raw secret values,
provisioning *new* external credentials/accounts (PagerDuty, Stripe live keys,
new DNS/email routing), billable resource creation sign-off, and anything the
safety rules reserve. No AWS Control Tower changes, ever.

Status legend: ✅ done · 🔨 Claude-owned (build/deploy) · 🔑 needs Scott
(credential/account/ship) · ⏳ blocked on a dependency.

## Already shipped this week

- ✅ roll-cell pre-migration backup gate (#235)
- ✅ email-change `expected_current` CAS (#237) — closed the last onboarding P1
- ✅ Codex delegation via the official plugin (#236) + deepest-effort/ultra (#238)
- ✅ consolidated data-retention policy doc (#239)

## Domain summary

| Domain | State | What's left | Owner |
|---|---|---|---|
| **Team activation** | ✅ done — seats set (Personal 1 / Professional 3 / Team 25, ratified 2026-08-24) and Team flipped available at one flat monthly price | — | — |
| **Billing** | Dark Stripe stack complete; live account verified (acct_1TpugQEICTDi58ec); all six CP_STRIPE_* secrets STAGED on witself-control-plane (secret key, webhook, portal bpc_, 3 URLs) — verified 2026-08-25; dunning emails on | Cutover only | 🔑 set CP_BILLING_PROVIDER/CP_STRIPE_MODE/CP_PLAN_LIFECYCLE_ENABLED(+allowlist), activate Tax, live end-to-end proof; (opt) revoke stale Jul-5 secret key |
| **Support** | Policy published (#251); assistant author-kind + reserved handle (#252); re-triage (#253); dark AI runner + notification labeling (#255); entitlement sync (#256); scoped support_ai credential (#257); support@ intake bridge implemented dark | SLO metric + breach alert, keyed intake enablement | 🔨 code · 🔑 support@ DNS+routing, runner host + API key, mint scoped credential, enable flag |
| **Monitoring** | Dark code merged (#231/#242/#244/#259); accounts + secrets DONE 2026-08-25: PagerDuty `witself-prod` (Events API v2) + healthchecks.io `witself-watchdog` (5m period/15m grace, pages witself-prod via native integration); both immutable Secrets banked in `monitoring` ns | 3-phase GitOps rollout + alert canary | 🔨 config/apply only — no more accounts or keys |
| **Edge DMARC** | Inbound worker runs full SMTP txn; cell records spf/dkim/dmarc | Authenticity parser module, hard-DMARC-fail SMTP rejection (dark flag), value-free authenticity metadata (dark), migration | 🔨 code · 🔑 authserv-id from live header, worker deploy |
| **Capacity** | ✅ done | 2-node Civo prod profile applied 2026-08-25: cell `civo-sandbox-usw2-dev` scaled 1→2 `g4s.kube.medium` in place (both ACTIVE, cluster Ready); `profile: prod` pinned in the inventory so it won't revert | — |
| **Retention** | ✅ done (#239) | — | — |

## Newly surfaced launch-critical gaps (from the survey critique)

These were not in the original decision list and matter for a paid, public,
PII-collecting launch:

1. **Legal pages don't exist** — no ToS, Privacy, Acceptable Use, DPA/subprocessor
   list, refund/cancellation terms. Claude drafts skeletons; **final content +
   legal review = 🔑**.
2. **No ToS/consent capture at signup** — accounts schema has no consent column;
   `accountCreate` records no acceptance. Add consent-timestamp capture (🔨) once
   legal text exists.
3. **Signup is invite-gated and CLI-only** — `accountCreate` requires `--invite`;
   there is no web signup. "General self-service" needs a **product decision (🔑):**
   open signup (relax `--invite`) vs waitlist, and web entry point vs CLI-only.
   Claude builds the chosen path.
4. **Open-signup abuse controls landed dark** — Turnstile, per-IP and global
   Durable Object daily counters, and general/signup edge throttles are
   implemented. The committed daily limits are `0` and all three Turnstile
   secrets are absent. Enablement is keyed: 🔑 mint the `self.witwave.ai`
   widget, provide its site and secret keys, choose both daily limits, then land
   the exact template/verifier activation PR. See
   [Signup Abuse Hardening](signup-abuse-hardening.md).
5. ✅ **Closed-account erasure is implemented dark** — posture B gives every
   closed account a 30-day reopen/export grace window, then purges its stored
   content and leaves one value-free anonymized tombstone plus the single
   `account.purged` audit record. Launch enablement remains keyed per cell:
   enable the default-off worker in preview, review count-only results, then
   switch it to enforce.
6. **No public status/incident page** — reasonable default: defer a full status
   page; stand up an incident-comms channel referenced from the support policy.

## Cross-cutting ordering constraints

- **Paid-to-paid guard before Team flip.** The Professional→Team billing guard
  (`billing_mutations.go`) MUST merge before `team.available` flips in
  `web/plans/plans.json`, or `subscribe()` mints a second live subscription
  (double-bill). The `plans.json` edits (Team available, support feature, seat
  counts) are one cluster.
- ✅ **Second Civo node** applied 2026-08-25 (in-place 1→2, both nodes ACTIVE); the
  headroom monitoring needs is now in place.
- **Monitoring receiver before support SLO alert** (support breach + dead-man
  feed the one shared PagerDuty service + dead-man monitor — create exactly one
  of each).
- **Edge DMARC before support@ intake** (support@ trusts DMARC-authenticated
  sender behind the same edge).
- **One release per arc.** All dark code merges first; then ONE coordinated
  CI + tag + production deploy — not a tag per domain.

## Claude-owned execution order

Dark, independently testable slices, delegated to Codex (gpt-5.6-sol/ultra),
adversarially reviewed, gated, merged. Roughly in dependency order:

1. **Legal-page skeletons** (ToS, Privacy incl. erasure posture, AUP, DPA,
   refund/cancellation) — drafts for Scott/legal to finalize. *Drafted under
   docs/legal/; remaining owner decisions are bracketed in-page (governing law,
   entity details, subprocessor confirmation, contact
   addresses).*
2. **Capacity**: Civo prod node profile (fixed 2-node, vary only NodeCount) →
   lift minimal-only gate → TUI + docs → usw2 soft topology-spread values.
3. **Monitoring**: PagerDuty native receiver (dark) → dead-man watchdog (dark) →
   3-phase enablement overlay (committed, not applied) → runbook.
4. **Edge DMARC**: authenticity parser module + tests → hard-fail SMTP rejection
   behind `AGENT_EMAIL_DMARC_REJECT_ENABLED` (dark) → authenticity metadata +
   migration (cell-side inert first).
5. **Team seats**: `operator_seats` dimension (dark) → plan-fit seat check.
6. **Billing (paid-to-paid guard first)** → Stripe Tax wiring → GA gate flag
   (fail-closed) → dunning contract test → refund runbook. *All merged:
   #247, #248, #249, and the dunning-contract/refund-runbook PR; what remains
   is the keyed live cutover.*
7. **Support**: policy doc → assistant author-kind + reserved handle → admin
   re-triage store method → AI support-runner core (dark) → scoped support_ai
   credential/role (dark) → SLO metric + breach alert → support@ intake bridge.
   *Steps 1–5 merged: #251, #252, #253, #255, #257 (+ entitlement sync in
   #256). Remaining: the SLO alert and the support@ intake bridge.*
8. **plans.json cluster** (after the paid-to-paid guard): per-plan
   `operator_seats` values → support entitlement sync → Team billing readiness →
   **flip Team available** with honest flat pricing.
9. **Consent capture** (after legal text) · **post-deploy** feature-status
   reconciliation citing real monitoring evidence.

## Needs-Scott packet

Grouped by the interaction required. Claude does everything up to these.

**Product / legal decisions**
- Open self-service signup (relax `--invite`; web entry point vs CLI-only).
- Approve + publish legal pages after legal review.
- Signup abuse enablement: mint the `self.witwave.ai` Turnstile widget, provide
  its site and secret keys, and choose the per-IP and global daily limits; the
  follow-up template/verifier PR pins those exact values before activation.
- Account-purge enablement: switch the default-off purge worker to preview,
  review its count-only results, then flip it to enforce.
- Status/incident-comms channel.
- Per-plan seat counts; whether Professional/Personal are seat-restricted.
- Paid-to-paid transition policy (contact-guard vs in-place switch).

**Capacity (gates monitoring)** — ✅ DONE 2026-08-25
- Applied: `witself-infra up -cell civo-sandbox-usw2-dev -profile prod`. Preview
  showed the required in-place `~pools` NodeCount 1→2 (1 update, 15 unchanged, no
  replace); apply matched; both `g4s.kube.medium` nodes ACTIVE and cluster Ready
  (Civo API). `profile: prod` persisted in `~/.witself/infra.yaml` and a no-flag
  re-preview shows 16 unchanged, so it will not revert. Monitoring is unblocked.

**Monitoring accounts + secrets** — ✅ DONE 2026-08-25
- PagerDuty: service `witself-prod` (P9RRGZ8) with Events API v2 integration;
  routing key banked as immutable Secret `monitoring/witself-monitoring-pagerduty-v1`
  (key `routing_key`). Login in 1Password.
- Dead-man: healthchecks.io check `witself-watchdog`, period 5m / grace 15m,
  ping URL banked as immutable Secret `monitoring/witself-monitoring-deadman-v1`
  (key `url`); missed check-in pages `witself-prod` via the native PagerDuty
  integration (plus email backup). Passwordless (magic-link) account.
- Remaining: the 3-phase GitOps enablement (stack → targets verified → alerting
  + receiver secret names in values) and the alert canary with retained evidence.

**Stripe** (2026-08-24: account verified for live charges; portal default
configuration saved; webhook `witself-control-plane` →
`https://self.witwave.ai/v1/billing/webhook/stripe` active on the exact five
consumed events; failed-payment + upcoming-renewal customer emails enabled;
incomplete payments auto-cancel at 15 days; live product catalog empty)
- ✅ DONE 2026-08-25: live secret key + webhook signing secret revealed and all
  six CP_STRIPE_* Worker secrets staged dark via
  `scripts/stage-stripe-live-secrets.sh`; the superseded live key was expired.
- At cutover: activate Stripe Tax; set CP_BILLING_PROVIDER/CP_STRIPE_MODE/
  CP_PLAN_LIFECYCLE_ENABLED(+allowlist); container picks env up only at the
  plan-lifecycle activation RPC; verify auto-provisioned prices; one live
  end-to-end proof (also completes the Go-live setup-guide step) before GA.

**Email edge + support**
- Capture Cloudflare's real authserv-id; enable DMARC reject + deploy worker;
  apply the authenticity migration; support@ DNS + Email Routing; mint the
  support-AI credential + Claude API key; stand up the runner host.

**Release**
- One coordinated push + CI + tag + production deploy for the whole dark batch.
