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
| **Support** | Policy published (#251); assistant author-kind + reserved handle (#252); re-triage (#253); dark AI runner + notification labeling (#255); entitlement sync (#256); scoped support_ai credential (#257); SLO metric + breach alert live (#259 — `witself_support_slo_metrics_up 1` and `WitselfSupportFirstResponseBreach` loaded/ok verified on the serving cell 2026-08-27); support@ intake bridge implemented dark; incident-comms channel published | Keyed intake enablement | 🔑 support@ DNS+routing, runner host + API key, mint scoped credential, enable flag |
| **Monitoring** | LIVE 2026-08-25: 3-phase rollout merged + verified (#264 stack, #265 targets, #266 alerting); PagerDuty `witself-prod` incident route + dead-man heartbeat active; 14 rules, zero eval failures, schema-91 + PVC metrics scraped | — (accepted; observe normal rules/storage growth over the acceptance window) | ✅ |
| **Edge DMARC** | ✅ LIVE 2026-08-29: authserv-id `mx.cloudflare.net` captured from a live Gmail probe; edge deployed from v0.0.262 with `AGENT_EMAIL_DMARC_REJECT_ENABLED=true` + relay v2; production proof — real `dkim=pass` recorded in the cell columns, `dmarc=none` mail correctly not rejected | — | ✅ |
| **Capacity** | ✅ done | 2-node Civo prod profile applied 2026-08-25: cell `civo-sandbox-usw2-dev` scaled 1→2 `g4s.kube.medium` in place (both ACTIVE, cluster Ready); `profile: prod` pinned in the inventory so it won't revert | — |
| **Retention** | ✅ done (#239) | — | — |

## Newly surfaced launch-critical gaps (from the survey critique)

These were not in the original decision list and matter for a paid, public,
PII-collecting launch:

1. **Legal pages don't exist** — no ToS, Privacy, Acceptable Use, DPA/subprocessor
   list, refund/cancellation terms. Claude drafts skeletons; **final content +
   legal review = 🔑**.
2. **ToS/consent capture at signup** — ✅ DEPLOYED dark in v0.0.260
   (2026-08-28, both cells at schema 94 + CP verified): schema 0094 adds
   nullable consent columns, and consent ships behind the optional
   `--accept-terms` CLI flag / `consent_terms_version` +
   `consent_privacy_version` API fields end to end (fingerprint-bound,
   audited as `account.consent.recorded`; consent-less invite signups are
   byte-identical to before). Activation = CLI default-on + web signup once the
   legal text is finalized (still 🔑).
3. ✅ **Open-signup path BUILT DARK** behind committed
   `CP_SIGNUP_OPEN=false`. Invite-less requests fail closed against a missing
   Turnstile enablement/secret or non-positive per-IP/global daily limits, then
   require a valid challenge, both quota allowances, and both consent fields.
   Remaining 🔑: stage the runtime Turnstile values, land one reviewed
   activation PR that pins the chosen positive limits and flips
   `CP_SIGNUP_OPEN=true`, and decide the web entry point. CLI-first works today.
4. **Open-signup abuse controls landed dark** — Turnstile, per-IP and global
   Durable Object daily counters, and general/signup edge throttles are
   implemented. The committed daily limits are `0` and all three Turnstile
   secrets are absent. Enablement is keyed: 🔑 mint the `self.witwave.ai`
   widget, stage its runtime Turnstile values, choose both daily limits, then
   land the single activation PR that pins those limits and flips
   `CP_SIGNUP_OPEN`. See
   [Signup Abuse Hardening](signup-abuse-hardening.md).
5. ✅ **Closed-account erasure is implemented dark** — posture B gives every
   closed account a 30-day reopen/export grace window, then purges its stored
   content and leaves one value-free anonymized tombstone plus the single
   `account.purged` audit record. Launch enablement remains keyed per cell:
   enable the default-off worker in preview, review count-only results, then
   switch it to enforce.
6. ✅ **Incident comms stood up; status page deferred** — the public GitHub
   `incident` label is the canonical incident log, referenced from the support
   policy (Incident communications section) with the operator procedure in
   runbooks.md. A full hosted status page stays deliberately post-launch.

## Cross-cutting ordering constraints

- **Paid-to-paid guard before Team flip.** The Professional→Team billing guard
  (`billing_mutations.go`) MUST merge before `team.available` flips in
  `web/plans/plans.json`, or `subscribe()` mints a second live subscription
  (double-bill). The `plans.json` edits (Team available, support feature, seat
  counts) are one cluster.
- ✅ **Second Civo node** applied 2026-08-25 (in-place 1→2, both nodes ACTIVE); the
  headroom monitoring needs is now in place.
- ✅ **Monitoring receiver before support SLO alert** — satisfied; both live (support breach + dead-man
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
   migration (cell-side inert first). *All merged and deployed dark: #269
   (cells, schema 93) + #270 (edge, 0.0.259). Remaining: keyed enablement.*
5. **Team seats**: `operator_seats` dimension (dark) → plan-fit seat check.
6. **Billing (paid-to-paid guard first)** → Stripe Tax wiring → GA gate flag
   (fail-closed) → dunning contract test → refund runbook. *All merged:
   #247, #248, #249, and the dunning-contract/refund-runbook PR; what remains
   is the keyed live cutover.*
7. **Support**: policy doc → assistant author-kind + reserved handle → admin
   re-triage store method → AI support-runner core (dark) → scoped support_ai
   credential/role (dark) → SLO metric + breach alert → support@ intake bridge.
   *All merged: #251–#253, #255–#257, the SLO metric + breach alert (#259,
   live-verified 2026-08-27), and the dark support@ intake bridge. Remaining:
   keyed enablement only.*
8. **plans.json cluster** (after the paid-to-paid guard): per-plan
   `operator_seats` values → support entitlement sync → Team billing readiness →
   **flip Team available** with honest flat pricing.
9. **Consent capture** (after legal text) · **post-deploy** feature-status
   reconciliation citing real monitoring evidence.

## Needs-Scott packet

Grouped by the interaction required. Claude does everything up to these.

**Product / legal decisions**
- Decide the web signup entry point (CLI-first already works).
- Approve + publish legal pages after legal review.
- Signup abuse enablement: mint the `self.witwave.ai` Turnstile widget, provide
  and stage its runtime values, and choose the per-IP and global daily limits;
  one reviewed activation PR pins those exact values and flips
  `CP_SIGNUP_OPEN=true`.
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
- ✅ 3-phase GitOps enablement DONE 2026-08-25: PR #264 (stack, 12 invariants +
  all stack targets verified), PR #265 (ServiceMonitors + metrics ingress
  narrowed; server/worker/kubelet/kube-state-metrics up across multiple scrape
  intervals; schema-91 gauges fresh), PR #266 (alerting: null root,
  `witself_alert` PagerDuty route via `routing_key_file`, `witself_watchdog`
  dead-man route; 14 alerting rules, zero evaluation failures, PVC
  capacity/available metrics present, Watchdog firing).
- ✅ Alert canary PASSED 2026-08-26 (Scott's go): firing observed, 45s dwell,
  rule flipped false, resolve observed after 310s dwell, canary rule deleted
  by exact UID. PagerDuty receiver evidence retained in `~/.witself/`
  (mode 0600): script artifact `monitoring-acceptance.json`, notification
  counter timeline (firing 0→1 at 04:04:23Z, resolve 1→2 at 04:09:15Z, zero
  failures), and operator receipt — Scott received and resolved the incident
  on PagerDuty.
- ✅ Dead-man lapse/restore proof PASSED 2026-08-26 (Scott's go): Watchdog
  silenced 04:12Z (incident alerting stayed live), heartbeat flatlined 24 min
  at counter 56, healthchecks.io paged via `witself-prod` (operator confirmed
  04:36Z), silence deleted, heartbeat resumed 04:37:20Z with zero failed
  notifications. Receipts + per-minute flatline log in `~/.witself/`
  (mode 0600). Monitoring acceptance evidence is complete.

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
- Capture Cloudflare's real authserv-id (one Gmail→agent-address email), then
  the reviewed enablement flips: DMARC reject, and relay v2 after cells
  dual-accept (cells and worker are already deployed; the authenticity
  migration shipped at schema 93). Support intake token is minted+installed
  dark (sealed sec_k4k7xglepstg6mej). Remaining Scott-keyed: support@ DNS +
  Email Routing (one destination-verification click); mint the support-AI
  Claude API key; stand up the runner host. Turnstile: API token has read but
  not write scope — dashboard mint (or a Turnstile-scoped token) still needed.

**Release** — ✅ v0.0.260 DONE 2026-08-28: consent capture (schema 0094) +
support/incident-comms close-out + feature-status truth-up + gate tooling
released and deployed; dual-cell restore-verified pre-migration backups
(evidence 2/2); both cells rolled in waves to 0.0.260 at schema 94 (8/8
checks each, zero restarts); control plane deployed + deployment-verified
at 0.0.260 (consent binding live, all activation gates dark); edge
unchanged at 0.0.259 (no edge src changes). Previous train — ✅ DONE
2026-08-26/27: v0.0.258 (42-commit dark batch) + v0.0.259
(deploy-tooling fixes) released; dual-cell restore-verified pre-migration
backups; both Civo cells rolled in waves to 0.0.258 at schema 93 (8/8 checks
each, zero restarts, no alerts); control plane deployed+attested at 0.0.259
(canonical-live attestation, SUPPORT_EMAIL_INTAKE_TOKEN minted+sealed+installed
dark); email edge deployed+attested at 0.0.259 (all delivery/DMARC/relay-v2
gates dark).
