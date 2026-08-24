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
| **Billing** | Dark Stripe stack complete: lifecycle, mutations, receipts, adapter, paid-to-paid guard (#247), Stripe Tax wiring (#248), GA gate (#249), dunning contract test, refund runbook | Live cutover only | 🔑 live keys/products/webhook/portal/Tax activation/cutover |
| **Support** | Policy published (#251); assistant author-kind + reserved handle (#252); re-triage (#253); dark AI runner + notification labeling (#255); entitlement sync (#256); scoped support_ai credential (#257); support@ intake bridge implemented dark | SLO metric + breach alert, keyed intake enablement | 🔨 code · 🔑 support@ DNS+routing, runner host + API key, mint scoped credential, enable flag |
| **Monitoring** | kube-prometheus-stack templated, default-off | PagerDuty receiver (dark), dead-man watchdog, 3-phase rollout overlay, runbook | 🔨 config · 🔑 PagerDuty acct+key, dead-man monitor, cluster secrets, apply |
| **Edge DMARC** | Inbound worker runs full SMTP txn; cell records spf/dkim/dmarc | Authenticity parser module, hard-DMARC-fail SMTP rejection (dark flag), value-free authenticity metadata (dark), migration | 🔨 code · 🔑 authserv-id from live header, worker deploy |
| **Capacity** | Civo has no prod profile (hardcoded 1 node) | Fixed 2-node Civo profile (vary only NodeCount → in-place update), lift minimal-only gate, TUI, topology spread | 🔨 code · 🔑 billable 2-node apply (gates monitoring) |
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
- **Second Civo node before monitoring rollout** (Scott's sequencing; monitoring
  needs the headroom).
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

**Capacity (gates monitoring)**
- `witself-infra preview -cell civo-sandbox-usw2-dev -profile prod` → confirm
  in-place NodeCount 1→2 (abort on any cluster/network/PVC replace) → `up` to
  create the 2nd billable node. Sequence before monitoring.

**Monitoring accounts + secrets**
- One PagerDuty free service + Events API v2 key; one external dead-man monitor.
- Pre-create the two immutable K8s secrets (PagerDuty key, dead-man URL).
- Apply the 3-phase rollout; run the alert canary; retain evidence.

**Stripe**
- Live account + secret key; webhook endpoint + secret; Customer Portal config;
  Stripe Tax activation; Revenue Recovery (Smart Retries → cancel → Personal);
  14-day refund window; verify auto-provisioned prices; set launch env flags;
  execute the cutover; one live end-to-end proof before GA.

**Email edge + support**
- Capture Cloudflare's real authserv-id; enable DMARC reject + deploy worker;
  apply the authenticity migration; support@ DNS + Email Routing; mint the
  support-AI credential + Claude API key; stand up the runner host.

**Release**
- One coordinated push + CI + tag + production deploy for the whole dark batch.
