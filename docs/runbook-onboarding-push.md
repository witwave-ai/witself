# The onboarding push: invite waves, then the open push

Status: not rehearsed. No step below has been run as a drill or timed end to end. Verified against `main` at `c165480b8564` on 2026-10-21; re-check every command against the current tree before relying on it.

[FOUNDER CHECKPOINT]: the founder decides; he runs the step himself or says go first.

[GAP] Gn: an unresolved gap numbered Gn in this file's `## Gaps` list.

[PROPOSED]: a number or rule this runbook suggests; the founder confirms or changes it.

All times below are UTC. Commands describe future operator procedures; the verification banner is source verification, not a live-state check.

## 1. What this runbook covers

| Tier | Shape | Volume |
|---|---|---|
| T1: invite waves | Invite codes capped at the wave size and pinned to `civo-prod-use1-serving`. Invite-only is approved; lowering the global quota for the wave window is the fallback. | Waves of about 100, up to about 300 accounts **[PROPOSED]** |
| T2: open push | Public entry point and quickstart. Quotas sized from a measured load test. | About 1,000 accounts **[PROPOSED]** |
| T3: beyond | Needs a funded scale-out. Out of scope here. | 2,000 or more, or more than about 500 active **[PROPOSED]** |

The founder's answers of 2026-10-10 that bind this runbook:

1. Open signup continues at its committed quotas (10 per source IP and 500 a day globally) on the sandbox serving cell until production accepts ([Worker configuration](../infra/cloudflare/control-plane/wrangler.template.jsonc); [Launch record](launch-readiness.md#launch-record)). Daily quotas apply to invited signups too; without invite-only, the ceiling is the committed daily global quota.
2. Heavy onboarding waits until the production cell is provisioned, monitored, load-tested and accepting signups.
3. Undelivered paid features are marked "coming soon" (built, slice 118).
4. The move rehearsal loads on production.

Out of scope: provisioning the production cell, moving accounts onto it and retiring the old cell. **[GAP] G16** That procedure has no public runbook. This runbook starts when signups switch to production (gate 4.1 item 1).

T3 needs a third node or a second cell, a backup scheduler sized for the fleet, more frequent dumps and a second responder. The node-count mechanism is [Change a Civo cell's node count](runbooks.md#change-a-civo-cells-node-count).

## 2. How signup admission works today

Read this once before the first wave. Several controls behave in ways that are easy to miss.

### 2.1 The gates, in the order a signup meets them

1. **Edge request limits.** `PUBLIC_IP_LIMITER` allows 300 requests per 60 s per source IP. `SIGNUP_IP_LIMITER` allows 5 signup requests per 60 s per source IP and runs before the signup Durable Object. A denial is `429` ([Cloudflare edge rate limits](signup-abuse-hardening.md#cloudflare-edge-rate-limits); [Worker configuration](../infra/cloudflare/control-plane/wrangler.template.jsonc)).
2. **Open-signup gate.** `CP_SIGNUP_OPEN` is committed `"true"`. A request counts as open signup only when the gate is exactly `"true"` and the request carries no invite. Any other gate value requires an invite for a new signup: a request without one fails with `403 invite code required`. A retry of an invite-less signup that had already passed its legal phase keeps going after the gate closes; one still in its legal phase fails with `503 open signup is unavailable`. Setting the gate to `false` is the documented rollback to invite-only ([signup runtime](../infra/cloudflare/control-plane/src/account-signup-runtime.mjs); [Open-signup gate](signup-abuse-hardening.md#open-signup-gate); [Launch record](launch-readiness.md#launch-record)).
3. **Open signup fails closed when misconfigured.** An invite-less signup returns `503 open signup configuration is invalid` unless Turnstile is enabled and both daily limits are positive ([signup runtime](../infra/cloudflare/control-plane/src/account-signup-runtime.mjs)).
4. **Turnstile applies to every new signup, invited or not.** When enabled, the check runs before quotas and invite reservation. An invalid or missing token gives `403 turnstile challenge required` with a challenge URL. A verifier failure gives `503 turnstile verification unavailable`. Invitees also see the challenge URL and must rerun with `--challenge` ([signup runtime](../infra/cloudflare/control-plane/src/account-signup-runtime.mjs); [Create an account on Witself Cloud](runbooks.md#create-an-account-on-witself-cloud)).
5. **Daily quotas apply to invited signups too.** Both counters run for every new signup when their limit is positive, before its legal phase. The committed limits are 10 per source IP and 500 globally per UTC day. A denial is `429 signup rate limit exceeded` ([Durable daily signup quotas](signup-abuse-hardening.md#durable-daily-signup-quotas); [Worker configuration](../infra/cloudflare/control-plane/wrangler.template.jsonc)).
6. **The quota day is the UTC day.** It resets at 00:00 UTC. A wave window that crosses 00:00 UTC gets two days of quota ([Durable daily signup quotas](signup-abuse-hardening.md#durable-daily-signup-quotas)).
7. **The value `0` disables a quota.** Only an exact positive integer enables it. With open signup on, `0` makes every invite-less signup fail closed. For invited signups, `0` removes that cap ([signup counters](../infra/cloudflare/control-plane/src/signup-counters.mjs)).
8. **Signups that record legal consent pin the abuse policy.** With `CP_SIGNUP_LEGAL_ENFORCEMENT` `"true"` and `--accept-terms`, the signup saves the Turnstile state and both limits before the legal read. If, before the signup is admitted past its legal phase, the gate closes (invite-less signups only) or the Turnstile state or either limit changes, it fails with `503 open signup is unavailable` or `503 signup abuse policy changed; retry when the original policy is available`. Rerunning the same command replays that refusal until the original policy returns. A signup already admitted past its legal phase is not affected. This is why B3 and B4 run between waves (rule R4, which allows B4 inside a wave only as a founder exception; [signup runtime](../infra/cloudflare/control-plane/src/account-signup-runtime.mjs); [signup legal state](../infra/cloudflare/control-plane/src/signup-legal.mjs)).
9. **Invite reservation.** A request carrying an invite always reserves one use, even while open signup is on. The reservation raises the use count. Disabled, not-yet-valid, expired and fully used codes each have a distinct refusal ([signup runtime](../infra/cloudflare/control-plane/src/account-signup-runtime.mjs)).
10. **Placement comes after reservation.** If placement fails, the signup returns the `503` and stays at `invite_reserved`. It still counts against the per-IP and global daily quotas for that UTC day. An invited attempt keeps its invite use reserved; no code path releases either. Rerunning the same command resumes the same provision: the CLI keeps a journal under the local name, so the quota counters and any reservation replay and a retry spends nothing more, even on a later UTC day. A rerun with a new `--name` starts a new provision, counts against both quotas again and, if it carries an invite, reserves one more invite use. A rerun under the same `--name` with a different email or invite is refused on the user's machine and sends nothing (**[GAP] G11**; [signup runtime](../infra/cloudflare/control-plane/src/account-signup-runtime.mjs); [the CLI](../cmd/witself/main.go)).
11. **Placement.** See 2.2.

### 2.2 Placement and the production pin

- Precedence, top wins: invite `cell` (hard pin), invite `region` (hard constraint), fleet `pinned` strategy (soft), then weighted random ([Placement and landing](deployment-cells.md#placement-and-landing); [Worker](../infra/cloudflare/control-plane/src/index.js)).
- **Only eligible cells are candidates, even for a pinned invite.** A cell is eligible when `accepting` is not false, it is not a backup-validation target, it has an endpoint and it has a provisioning token. While account backups are enabled it also needs a distinct backup token. Eligibility is filtered before the invite pin ([placement](../infra/cloudflare/control-plane/src/placement.mjs)).
- **A pinned invite to a drained or ineligible cell fails with `503 no capacity: invite-pinned cell <name> unavailable`.** It never falls back to another cell ([Worker](../infra/cloudflare/control-plane/src/index.js)).
- **No accepting cell at all gives `503 no capacity: no accepting cells`.**
- Placement failures count as `provision_failed`, with no per-request log line of their own (**[GAP] G1**; section 5.3).
- The invite `cell` field is checked for name format, not existence. A typo in `--cell` makes every signup on that code fail with 503. Always run the canary in section 9 ([Worker](../infra/cloudflare/control-plane/src/index.js)).
- **[GAP] G7** Placement ignores capacity. Nothing drains a cell automatically at a memory or disk threshold. Draining is manual; the founder holds and confirms the documented capacity thresholds **[FOUNDER CHECKPOINT]**.
- Draining changes only `accepting`; existing account routes are untouched ([cell coordinator](../infra/cloudflare/control-plane/src/target-cell-coordinator.mjs)).
- **Restore also needs an accepting cell.** `cells restore` picks only eligible cells. An evacuated account not yet restored stays archived and offline while no eligible cell exists. Rule R2 keeps moves away from waves ([Worker](../infra/cloudflare/control-plane/src/index.js); [Account moves](witself-admin.md#account-moves)).
- Placement reads the cell registry from KV on each signup. Directory reads use a 60-second cache and KV propagation ([Directory routing after a same-name re-endpoint](runbooks.md#directory-routing-after-a-same-name-re-endpoint)). **[GAP] G12** The time from a drain to the last signup placed on that cell has not been measured.
- The sandbox serving cell is registered under the legacy name `civo-sandbox-usw2-dev`. After the switch it must stay drained. An undrain, or a `witself-infra up` of `civo-sandbox-use1-serving` that has a control plane and no `register_draining`, registers it as accepting again, and signups can land on it ([Cell identity after a same-name re-endpoint](runbooks.md#cell-identity-after-a-same-name-re-endpoint); [Registration in a drained state](../infra/pulumi/README.md#registration-in-a-drained-state)).

### 2.3 Where each setting lives, and how it changes

Every write below is a **[FOUNDER CHECKPOINT]**.

| Setting | Lives in | How to change | Time to effect | Reference |
|---|---|---|---|---|
| `CP_SIGNUP_OPEN`, `CP_SIGNUP_DAILY_LIMIT_PER_IP`, `CP_SIGNUP_DAILY_LIMIT_GLOBAL` | Committed plain-text Worker vars | Reviewed PR, release tag, control-plane deploy (B3). The verifier pins `"10"`, `"500"`, `"true"`, so it changes too. | Hours; never measured **[GAP] G12** | [Worker configuration](../infra/cloudflare/control-plane/wrangler.template.jsonc), [deploy verifier](../infra/cloudflare/control-plane/scripts/verify-deployment.mjs), [Production activation and rollback](signup-abuse-hardening.md#production-activation-and-rollback) |
| `SIGNUP_IP_LIMITER`, `PUBLIC_IP_LIMITER` | Committed rate-limit bindings | Same as above; the verifier pins these too. | Hours | [Worker configuration](../infra/cloudflare/control-plane/wrangler.template.jsonc), [deploy verifier](../infra/cloudflare/control-plane/scripts/verify-deployment.mjs) |
| `CP_SIGNUP_TURNSTILE_ENABLED`, `CP_SIGNUP_TURNSTILE_SECRET_KEY`, `CP_SIGNUP_TURNSTILE_SITE_KEY` | Runtime-only Worker secrets | The founder performs every Turnstile secret put: `npm run secret:put:break-glass -- NAME`, under the provider-mutation freeze, from a clean tag (8.1 prerequisites). | Unmeasured | [Worker configuration](../infra/cloudflare/control-plane/wrangler.template.jsonc), [package scripts](../infra/cloudflare/control-plane/package.json) |
| Account-backup settings (`CP_ACCOUNT_BACKUPS_ENABLED`, `CP_ACCOUNT_BACKUPS_INTERVAL_MINUTES`, `CP_ACCOUNT_BACKUPS_PAGE_SIZE`, `CP_ACCOUNT_BACKUPS_CONCURRENCY`) | Operator-controlled Worker secrets | Break-glass secret put, performed by the founder. Procedure in section 8.1. | Unmeasured | [Periodic logical account snapshots](backup-and-recovery.md#periodic-logical-account-snapshots) |
| Invite codes | Control-plane KV, written through the fleet API | `witself-admin invite create`, `enable`, `disable` or `delete` | Unmeasured | [Signup invites](witself-admin.md#signup-invites) |
| Cell `accepting` | Cell coordinator Durable Object, projected to KV | `witself-admin cells drain NAME` or `undrain NAME`. Every `witself-infra up` that has a control plane also writes it: the cell registers as accepting unless `register_draining` is set. | Unmeasured; KV propagation applies | [Cells](witself-admin.md#cells), [cell coordinator](../infra/cloudflare/control-plane/src/target-cell-coordinator.mjs) |
| Fleet placement strategy, placement runner, reaper | Control-plane KV | `witself-admin settings placement`, `placement-runner` or `reaper` (writes need `--yes`) | Unmeasured | [Settings](witself-admin.md#settings) |

Every control-plane deploy attests open signup: `scripts/release-deploy-control-plane.sh --attest-signup-open-active` exports `CP_DEPLOY_SIGNUP_OPEN_ACTIVE=true`. The attestation only frees the three Turnstile secrets from the dark-secret check ([deploy script](../infra/cloudflare/control-plane/scripts/deploy-release.mjs)). **[GAP] G15** Under invite-only the Turnstile secrets stay set, so every deploy still passes `--attest-signup-open-active`, whose documented meaning is open signup; no attestation names that state. Leaving it out makes the deploy refuse while the secrets exist.

### 2.4 Rules that hold for the whole push

**R1. Never delete the PostgreSQL PVC or StatefulSet outside a verified-backup step.** Treat both as destructive: deleting the PVC can delete the provider volume and its data. The same rule is in [Rules that apply in every case](runbook-serving-cell-lost.md#2-rules-that-apply-in-every-case). The repository does not set the storage class's reclaim policy; check the cell's storage class before relying on volume retention or expansion (section 7.5).

**R2. No account moves near a wave.** **[FOUNDER CHECKPOINT]** to override. From wave launch until 24 h after the wave code closes, run no `cells evacuate` and no `cells restore`, never undrain the legacy cell, and run no `witself-infra up` with a control plane on `civo-sandbox-use1-serving` unless its own inventory entry sets `register_draining: true`. B2 would strand an account mid-move (2.2), and an account-move rollback undrains the legacy cell, which reopens it to signups mid-wave. Plan account moves into the gaps between waves; every account move needs the founder's go **[FOUNDER CHECKPOINT]**. The account-move conditions and optional weekly-drill hold in section 9 also apply.

**R3. During T1, every control-plane deploy and rollback is also a signup-setting change.** Each release tag carries its own signup values, and the [verifier](../infra/cloudflare/control-plane/scripts/verify-deployment.mjs) pins them. Once a B3 change is merged, every later control-plane deploy ships it, including a security release. Redeploying a tag older than the B3 tag restores that tag's signup values. Hazard 6 (committed configuration reverts) in [Compatibility hazards](runbook-control-plane-rollback.md#compatibility-hazards-check-every-one-every-time) says the same. Any difference is a stop. Any control-plane rollback is a **[FOUNDER CHECKPOINT]**: the operator acts only after the founder's go and a bold notice ([Founder checkpoints](runbook-control-plane-rollback.md#founder-checkpoints)). After any control-plane deploy or rollback, rerun B3 step 10.

**R4. B3 and B4 run between waves, never inside a wave window or the T2 window, with two founder exceptions: during Turnstile verifier trouble inside a wave, the founder may pull B4 **[FOUNDER CHECKPOINT]**; inside the T2 window, the founder may raise the quota with B3 after the day's quota is used up (section 11 step 5) **[FOUNDER CHECKPOINT]**.** A signup still in its legal phase can fail when the gate (invite-less signups only), Turnstile state or either limit changes (2.1 item 8). Anyone who hits `503 signup abuse policy changed` or `503 open signup is unavailable` cannot clear it while the new policy holds: rerunning the same command replays the refusal. They start a fresh signup with a new `--name`. The refused signup already counted against both daily quotas for its UTC day; it had not reserved an invite use, because reservation comes after the legal phase. The fresh signup counts against both quotas again and, if it carries an invite, reserves one more invite use.

**R5. Privacy.** Never put invitee names or email addresses in an invite `--note`. Use a wave label only. The note is stored in control-plane KV and printed by `invite list` ([Signup invites](witself-admin.md#signup-invites)). Treat Worker log output as personal data (5.4). Never paste raw output into tickets, memory, chat or files.

If the founder turns weekly restore drills on (default Wednesday 16:00-20:00Z), B3's train and control-plane deploy, and any account move, follow [Ship a fix and close signups without an AI agent](runbooks.md#ship-a-fix-and-close-signups-without-an-ai-agent) and [Operate periodic account backups](runbooks.md#operate-periodic-account-backups): no chain or train from 13:30Z, no by-hand control-plane deploy from 15:30Z, and no move from 16:00Z, until 21:32Z. Waves themselves are not held. A drill never runs on a moving account ([Scheduled restore drills (off by default)](backup-and-recovery.md#scheduled-restore-drills-off-by-default)).

## 3. Founder checkpoints in this runbook (summary)

Each item is a **[FOUNDER CHECKPOINT]**. The founder performs Worker secret puts, Turnstile changes, the plans Worker deploy, any production preview/apply/destroy (including `witself-infra up` of either serving cell), DNS or R2 changes, and public incident posts himself. The operator runs registry or directory writes, paging changes, account moves, and control-plane rollbacks or rehearsals only after the founder's explicit go; every rollback or rehearsal starts with a bold notice.

1. The T1 signup posture is invite-only through B3 (decided 2026-10-13; lowering the global quota to the wave size is the fallback). Approve the B3 merge, the tag and the deploy.
2. Approve each wave: size, dates, code style.
3. Create, raise, disable or delete invite codes, including the canary code.
4. Drain or undrain any cell, or run `witself-infra up` with a control plane on a registered cell.
5. Every B3 step that changes the world: values, merge (the point of no return), tag, control-plane deploy.
6. Any control-plane rollback or rehearsal (rule R3).
7. Any Worker secret change: Turnstile (B4) or backup page size (8.1).
8. Any paging severity or urgency change, and any new alert rule that pages on a live cell. The approved severities are in section 5 and in the rules files ([Alerts And Dashboards](observability-and-operations.md#alerts-and-dashboards)).
9. Reading Worker logs: `wrangler tail` or Workers Observability in the Cloudflare dashboard.
10. Production `kubectl` reads during waves, in the founder-approved access form (ask first).
11. Any account move, including any override of the R2 freeze.
12. Raising quotas for the T2 window, and closing it.
13. PVC expansion on production (7.5).

## 4. Entry gates

### 4.1 Before the first T1 wave (all must be true)

T1 starts once every item below is closed. Each line names the evidence that proves it.

1. **Production accepts and the legacy cell is drained.** **[FOUNDER CHECKPOINT]** `witself-admin cells undrain civo-prod-use1-serving`, then `witself-admin cells drain civo-sandbox-usw2-dev`. Evidence: `witself-admin cells list` shows `accepting` true for `civo-prod-use1-serving` and false for `civo-sandbox-usw2-dev`; the backup-validation cell stays outside placement. The legacy entry stays drained only while nothing registers it again (rule R2).
2. **Production is monitored and paging.** Monitoring phases 1-3 of [Founder open-plane monitoring](runbooks.md#founder-open-plane-monitoring) are done on production; phase 3 enables paging and needs the founder's go **[FOUNDER CHECKPOINT]**. The alert canary fires and resolves. The production receivers are confirmed current with the founder before paging moves to production **[FOUNDER CHECKPOINT]**. **[GAP] G14** Today the production cell keeps monitoring, alerting and probes off ([cell catalog](../.gitops/cells/catalog.yaml); [production values](../.gitops/cells/civo-prod-use1-serving/values.yaml)). Turning production monitoring on leaves `control_plane_alerts` false; control-plane alert ownership stays on the sandbox serving cell until the separate ownership change in section 5.1.
3. **Load test done on production hardware at 32, 128 and 256 principals**, with the nightly account-export scan and pg_dump CronJob running. Its result sets the wave size and push quotas. The [harness](memory-load-quality.md#concurrent-agents-and-tenant-isolation-slice) runs 32 principals by default and at most 256, against a dedicated test database or disposable schema. Running it against the production cell is a **[FOUNDER CHECKPOINT]**, and no public procedure covers it (**[GAP] G17**). **[PROPOSED]** Wave 1 size = the smaller of 100 and half of (principals held ÷ P). "Principals held" is the largest principal count the load test held without crossing a pause line in section 6. P is the number of active principals per account. **[PROPOSED]** P = 2 until wave 1 measures it; this is an assumption, not a measured or documented number. From wave 2 on, size each wave from measured per-account growth (section 10 step 4).
4. **Node memory below 60% at first sync [PROPOSED].** First sync on the release pins committed in production's values, then roll to the current release. Re-read node memory the day before wave 1.
5. **PostgreSQL volume size set in values before first sync.** `civoPostgres.storage` must hold the chosen size; 20-30Gi suggested **[PROPOSED]**. The committed value is `8Gi`. Growing it later has no documented procedure (**[GAP] G9**; 7.5). Complete monitoring phases 1-3, confirm receivers and paging **[FOUNDER CHECKPOINT]**, enable pg_dump and restore-verify the first dump before completing production activation; then roll to the current release ([production values](../.gitops/cells/civo-prod-use1-serving/values.yaml)).
6. **pg_dump enabled on production and the first dump restore-verified.** Off today: `civoPostgres.backup.enabled: false` and the `postgres_backup` catalog switch false ([production values](../.gitops/cells/civo-prod-use1-serving/values.yaml); [catalog](../.gitops/cells/catalog.yaml)).
7. **PostgreSQL OOM, restart, near-limit and node working-set alerts live on production.** The rules are on `main`: `WitselfPostgreSQLOOMKilled` and `WitselfPostgreSQLRestarted` are critical; `WitselfPostgreSQLVolumeFillingUp`, `WitselfPostgreSQLMemoryNearLimit` and `WitselfNodeMemoryWorkingSetHigh` are warning. Production renders them only after its monitoring and alerting switches are on, with PostgreSQL metrics, cAdvisor and node-exporter enabled (section 5.1).
8. **Customer-impact alerts page at high urgency.** Approved 2026-10-13, not yet built: the 5xx-ratio and OOM alerts page critical, PagerDuty re-notifies unacknowledged incidents, and a named backup covers absences. On `main`, `WitselfServerHighErrorRatio`, `WitselfServerOOMKilled` and `WitselfWorkerOOMKilled` are warning. The gate closes when the rules page critical and a canary proves a high-urgency page **[FOUNDER CHECKPOINT]**.
9. **Ticket-open notification live.** On `main`: `WitselfSupportTicketOpened`, `WitselfSupportFirstResponseSlow` and `WitselfSupportUrgentTicketWaiting` (warning). Production renders them once its alerting is on ([Support ticket alerts](runbooks.md#support-ticket-alerts)).
10. **"Coming soon" plan features live: met.** Approved 2026-10-10 and built by slice 118. The plans Worker carrying them is deployed from a release containing slice 118 ([Billing and limits](billing-and-limits.md)).
11. **Paging continuity [FOUNDER CHECKPOINT].** The paging service subscription is current, coverage is written down for the founder, and a named backup covers absences. The public promise is a first response within 1 business day ([The promise](support-policy.md#the-promise)).
12. **Key escrow [FOUNDER CHECKPOINT].** The dump decryption identity and the control-plane edge token are available from founder-held escrow outside the cell.
13. **Legal wording and backup handling match the published documents [FOUNDER CHECKPOINT].**
14. **Cross-tenant route matrix in CI: met.** The matrix and its runtime families run in CI on `main` (slice 122 parts A to D2).
15. **One real nonzero charge and refund**, per the [refund runbook](refund-runbook.md) **[FOUNDER CHECKPOINT]**. The billing alert gaps are met on `main`: `WitselfStripeWebhookProcessingFailures` (warning), `WitselfBillingMutationRecoveryFailing` and `WitselfEntitlementDeliveryStuck` (critical), on the cell that scrapes the control plane ([Billing failure alerts](runbooks.md#billing-failure-alerts)).
16. **Recommended for T1, required for T2: signup funnel metrics and alerts.** On `main`: counters on the control plane's `/metrics/probes` and four warning rules where `uptimeProbes` is on, today the sandbox serving cell ([Signup funnel and verification email observations](observability-and-operations.md#signup-funnel-and-verification-email-observations)).

### Decisions recorded (2026-10-10 and 2026-10-13)

- **Heavy onboarding** starts only after the production cell is provisioned, monitored, load-tested and accepting signups. Until then, open signup stays on at its committed quotas (10 per source IP and 500 a day globally) on the sandbox serving cell. Status: the production cell is onboarded, not provisioned.
- **T1 is invite-only** (decided; not yet applied). B3 sets it at least a day before the first wave: `CP_SIGNUP_OPEN` is committed `"true"` and the deploy verifier pins it, so B3 is a reviewed code change, a release and a control-plane deploy. B3 is a release, not a security fix, so it runs before the release freeze starts (more than 48 h before the wave) unless the founder exempts it **[FOUNDER CHECKPOINT]**. In the section 9 procedure, that means finishing it before step 4 (start the release freeze). Lowering the global quota to the wave size for the wave window is the fallback; it changes the same pins. Invite codes are pinned to `civo-prod-use1-serving` with `witself-admin invite create --cell` (built). A global-quota denial raises `WitselfSignupGlobalQuotaDenied` (warning, built), which renders only on the cell that scrapes the control plane. Quotas rise only for a set T2 window, after the load test, through a reviewed PR.
- **Release freeze** (approved): security fixes only, from 48 h before to 72 h after each wave. Merge chains pause. No script enforces it; the operator does. Under rule R3, a security release during T1 also carries the signup values of `main`.
- **Paging** (approved, not yet built): the 5xx-ratio and OOM alerts page as critical, and PagerDuty re-notifies unacknowledged incidents. Today `WitselfServerHighErrorRatio`, `WitselfServerOOMKilled` and `WitselfWorkerOOMKilled` are warning; `WitselfPostgreSQLOOMKilled` and `WitselfPostgreSQLRestarted` are already critical. A named backup covers absences of several days. This runbook states no coverage hours; the public promise is in [The promise](support-policy.md#the-promise).
- **Backup page size** (approved): raise it from 4 to 20 before about 800 directory entries, across all cells **[PROPOSED]**. The founder sets it himself as a Worker secret **[FOUNDER CHECKPOINT]** (8.1).
- **The plans Worker:** Approved (2026-10-13): the release chain will deploy the plans Worker right after the control plane, from the same clean tag. This is not yet built. Until then, deploy it by hand from the same clean tag, as [Release and build](release-and-build.md) describes. A control-plane rollback leaves the plans Worker at CUR, and a `web/plans/plans.json` difference between the tags is a stop (hazard 5) **[FOUNDER CHECKPOINT]**. The founder performs the plans deploy himself.
- **Support** (approved): the ticket-open notification is built (`WitselfSupportTicketOpened`, warning). The parked AI responder (read-only triage) is activated above about 5 tickets a day. Personal (free) users go to GitHub Discussions once it is enabled; it is not enabled.
- **Third node** (pre-approved): at sustained node memory above 75%, or at about 500 active accounts, through [Change a Civo cell's node count](runbooks.md#change-a-civo-cells-node-count).
- **Status page** (approved, not built): a hosted status page goes up before T2. Until then the GitHub `incident` label is the canonical channel ([Support Policy](support-policy.md)).
- **Ask first [FOUNDER CHECKPOINT]:** who may pull B1 and B2 without asking first; the brake and pause levels; production `kubectl` reads during waves and their access form; who reads Worker logs during a wave, and with which Cloudflare credential. Numbers marked **[PROPOSED]** stay marked.

## 5. What to watch during a wave

### 5.1 Alerts that exist today

These render only on a cell whose values turn them on. Production has monitoring and alerting off until phases 1-3 (gate 4.1, item 2). The control-plane groups (synthetic uptime, signup, billing, entitlement delivery) render only on the cell that scrapes the control plane.

| Alert | Fires when | Severity | Rules file |
|---|---|---|---|
| `WitselfServerHighErrorRatio` | Server 5xx above 5% of requests on 5-minute rates, while traffic exceeds 0.05 requests a second, for 10 min | warning | [Founder open-plane](../.gitops/charts/platform/files/founder-open-plane.rules.yaml) |
| `WitselfServerOOMKilled`, `WitselfWorkerOOMKilled` | Server or worker container OOM-killed and restarted within 30 min | warning | [Founder open-plane](../.gitops/charts/platform/files/founder-open-plane.rules.yaml) |
| `WitselfPostgresPVCApproachingCapacity` | PostgreSQL PVC at or above 80% used for 15 min | critical | [Founder open-plane](../.gitops/charts/platform/files/founder-open-plane.rules.yaml) |
| `WitselfPostgresPVCMetricsUnavailable` | PVC metrics absent for 10 min | critical | [Founder open-plane](../.gitops/charts/platform/files/founder-open-plane.rules.yaml) |
| `WitselfPostgreSQLDown`, `WitselfPostgreSQLExporterUnavailable` | `pg_up` 0 for 5 min; exporter absent for 10 min | critical | [PostgreSQL](../.gitops/charts/platform/files/postgresql.rules.yaml) |
| `WitselfPostgreSQLConnectionsHigh` | Connections above 80% of `max_connections` for 10 min | warning | [PostgreSQL](../.gitops/charts/platform/files/postgresql.rules.yaml) |
| `WitselfPostgreSQLTransactionAgeHigh` | Oldest transaction other than export and import transactions above 5 min, for 5 min | warning | [PostgreSQL](../.gitops/charts/platform/files/postgresql.rules.yaml) |
| `WitselfPostgreSQLExportTransactionAgeHigh`, `WitselfPostgreSQLImportTransactionAgeHigh` | An export (account, backup, evacuation or self) or import (account, evacuation or validation) transaction open above 45 min, for 5 min | warning | [PostgreSQL](../.gitops/charts/platform/files/postgresql.rules.yaml) |
| `WitselfPostgreSQLDeadlocks` | Any deadlock in 10 min | warning | [PostgreSQL](../.gitops/charts/platform/files/postgresql.rules.yaml) |
| `WitselfAccountBackupStale`, `WitselfAccountBackupHealthUnavailable` | Account backups stale or unreadable. An overrunning pass counts accounts not yet reached as stale; an account with no commit is overdue once its earliest known job is older than two intervals. | critical | [Founder open-plane](../.gitops/charts/platform/files/founder-open-plane.rules.yaml) |
| `WitselfPostgresBackupStale`, `WitselfPostgresBackupFailed` | Cell pg_dump stale or failed | critical | [Postgres backup](../.gitops/charts/platform/files/postgres-backup.rules.yaml) |
| `WitselfProbeTargetDown`, `WitselfProbeSchedulerStale` | Public probe down 10 min; control-plane probe scheduler stale | critical | [Uptime probes](../.gitops/charts/platform/files/uptime-probes.rules.yaml) |
| `WitselfStripeWebhookRejections`, `WitselfBillingReconciliationStalled` | Billing path failing | critical | [Uptime probes](../.gitops/charts/platform/files/uptime-probes.rules.yaml) |
| `WitselfStripeWebhookProcessingFailures`, `WitselfBillingMutationRecoveryFailing` | Webhook processing fails; billing mutation recovery fails | warning; critical, respectively | [Uptime probes](../.gitops/charts/platform/files/uptime-probes.rules.yaml) |
| `WitselfEntitlementDeliverySchedulerStale`, `WitselfEntitlementDeliveryStuck`; `WitselfEntitlementDeliveryMetricsUnavailable`, `WitselfEntitlementDeliveryCycleIncomplete`, `WitselfEntitlementDeliveryGap` | Entitlement delivery stalled, stuck or unobservable | first two critical; last three warning | [Entitlement delivery](../.gitops/charts/platform/files/entitlement-delivery.rules.yaml) |
| `WitselfSupportFirstResponseBreach` | Oldest unanswered ticket above 24 h | critical | [Founder open-plane](../.gitops/charts/platform/files/founder-open-plane.rules.yaml) |
| `WitselfSupportTicketOpened` | Newest unanswered ticket under 30 min old, for 1 min | warning | [Founder open-plane](../.gitops/charts/platform/files/founder-open-plane.rules.yaml) |
| `WitselfSupportFirstResponseSlow` | Oldest unanswered ticket over 4 h old, for 15 min | warning | [Founder open-plane](../.gitops/charts/platform/files/founder-open-plane.rules.yaml) |
| `WitselfSupportUrgentTicketWaiting` | An urgent ticket waits, for 1 min | warning | [Founder open-plane](../.gitops/charts/platform/files/founder-open-plane.rules.yaml) |
| `WitselfIdentityCapacityAtLimit` | An account at its plan identity limit for 30 min | info; no `witself_alert` label, never pages | [Founder open-plane](../.gitops/charts/platform/files/founder-open-plane.rules.yaml) |
| `NodeMemoryHighUtilization` (upstream) | Host memory above 90% for 15 min (host memory, not working set) | upstream default | [Alerts And Dashboards](observability-and-operations.md#alerts-and-dashboards) |
| `WitselfPostgreSQLOOMKilled`, `WitselfPostgreSQLRestarted` | Any PostgreSQL OOM kill or restart within 30 min | critical | [PostgreSQL](../.gitops/charts/platform/files/postgresql.rules.yaml) |
| `WitselfPostgreSQLMemoryNearLimit` | Working set above 90% of the limit for 10 min | warning | [Platform](../.gitops/charts/platform/files/platform.rules.yaml) |
| `WitselfNodeMemoryWorkingSetHigh` | Node working set above 90% of allocatable for 15 min | warning | [Platform](../.gitops/charts/platform/files/platform.rules.yaml) |
| `WitselfPostgreSQLVolumeFillingUp` | The 24 h trend fills the volume within 7 days, for 1 h | warning | [PostgreSQL](../.gitops/charts/platform/files/postgresql.rules.yaml) |
| `WitselfSignupGlobalQuotaDenied` | Any global-quota denial in 15 min; fires at once | warning | [Uptime probes](../.gitops/charts/platform/files/uptime-probes.rules.yaml) |
| `WitselfSignupProvisioningFailureRatioHigh` | More than 20% of provisioning attempts failed over 30 min, at least 3 failures, for 5 min | warning | [Uptime probes](../.gitops/charts/platform/files/uptime-probes.rules.yaml) |
| `WitselfVerificationEmailFailureRatioHigh` | More than 20% of verification-email attempts (signup and resend summed) failed over 30 min, at least 2 failures, for 5 min | warning | [Uptime probes](../.gitops/charts/platform/files/uptime-probes.rules.yaml) |
| `WitselfSignupMetricsUnavailable` | Observations missing, invalid or older than 30 min, for 15 min | warning | [Uptime probes](../.gitops/charts/platform/files/uptime-probes.rules.yaml) |
| `WitselfRestoreDrillStale`, `WitselfRestoreDrillFailed` | No validated restore drill for more than 8 days, for 30 min; latest finished drill failed, for 10 min | warning | [Founder open-plane](../.gitops/charts/platform/files/founder-open-plane.rules.yaml) |
| `WitselfArgoCDControllerOOMKilled` | Argo CD application controller OOM-killed and restarted within 30 min | warning | [Founder open-plane](../.gitops/charts/platform/files/founder-open-plane.rules.yaml) |

The PostgreSQL group needs monitoring alerting and PostgreSQL metrics. The platform groups need alerting and default rules, plus cAdvisor for container memory or node-exporter for node memory. These are independent of the `memory_alerts` switch, which gates agent-memory curation rules. The upstream host-memory rule stays enabled beside the working-set rules. The identity-capacity collector group renders only where the collector-alerts catalog switch is on, and no cell turns it on. The weekly restore-drill scheduler is off by default; its warnings concern account-snapshot drills, not a timed R2 restore of pg_dump. See [Alerts And Dashboards](observability-and-operations.md#alerts-and-dashboards), [Support ticket alerts](runbooks.md#support-ticket-alerts), [Signup funnel alerts](runbooks.md#signup-funnel-alerts), and [Operate periodic account backups](runbooks.md#operate-periodic-account-backups).

The control-plane gauges (account backups, Stripe, entitlement delivery and the signup funnel) are scraped from `https://self.witwave.ai/metrics/probes` by the cell whose `platform.monitoring.uptimeProbes` is on: today only `civo-sandbox-use1-serving`. Turning production monitoring on (gate 4.1, item 2) leaves `control_plane_alerts` false, and ownership stays on the sandbox serving cell. Ownership moves only in the PR that sets production's `control_plane_alerts` to true. A test allows one owner, and the sandbox serving overlay owns these groups whenever its monitoring is on, so that same PR ends the sandbox serving cell's ownership ([GitOps values generation](gitops-values-generation.md)). With the current overlay, that PR either changes the sandbox serving overlay so the two groups follow that cell's own `control_plane_alerts` switch, or sets that cell's `monitoring` to false. Turning monitoring off also ends that cell's own PostgreSQL, backup, platform and Argo CD alerts, its paging and its dead-man heartbeat. Turn it off only after the cell holds no accounts **[FOUNDER CHECKPOINT]**. The synthetic-uptime group (including signup and billing) and entitlement group move with it. During T1 the sandbox serving cell's Prometheus holds these gauges and pages. **[GAP] G13** Move the scrape and paging ownership before destroying that cell; retire it after the soak.

### 5.3 Observations and remaining gaps

The control plane exports aggregate signup observations on `/metrics/probes`:

- `witself_signup_events_total`: `started`, `provisioned`, `provision_failed`, `verified`.
- `witself_signup_denials_total`: `quota_per_ip`, `quota_global`, `turnstile_invalid`, `turnstile_unavailable`.
- `witself_signup_verification_email_total`: context `signup` or `resend`, outcome `sent` or `failed`.
- `witself_signup_pending_accounts`, `witself_signup_pending_oldest_age_seconds`, `witself_signup_pending_scan_complete`.
- `witself_signup_metrics_up`, `witself_signup_observed_timestamp_seconds`.

`provision_failed` includes placement failures. These are five-minute observations, not a ledger ([Signup funnel and verification email observations](observability-and-operations.md#signup-funnel-and-verification-email-observations)). Pending counts are complete only when `witself_signup_pending_scan_complete` is 1.

The verification-email failure log line carries the account id and a fixed reason code (`reason=email_send_or_record_error`), with no provider error or address. The per-IP denial line prints `signup-counter:ip:` plus a SHA-256 of the client IP (64 hex characters). Treat the Worker log stream as personal data. Before quoting a line, redact every 64-hex value and every account id.

- **[GAP] G1** A `503 no capacity` and other placement failures count as `provision_failed` and page through `WitselfSignupProvisioningFailureRatioHigh` on the cell that scrapes the control plane, but they have no per-request log line, and the counter names no cell or code. Further signals:
  1. Invitee reports (support tickets, or email to `support@witwave.ai`).
  2. The gap between `invite show` uses and production account growth in `cells list`. Uses above new accounts can mean stranded reservations or pending retries.
  3. **[FOUNDER CHECKPOINT]** Workers Observability in the Cloudflare dashboard, enabled on the control-plane Worker ([Worker configuration](../infra/cloudflare/control-plane/wrangler.template.jsonc)); whether its invocation logs show the `/v1/signup` response status is not verified from the repository.
- **[GAP] G2** No endpoint reports how much of the day's quota is used. Only denials show: in the Worker log and in `witself_signup_denials_total`.
- **[GAP] G3** No dashboards; Grafana is off. Watching means PromQL through the Kubernetes API proxy, `kubectl top`, and the admin CLI ([Alerts And Dashboards](observability-and-operations.md#alerts-and-dashboards)).
- **[GAP] G4** No PostgreSQL CPU rule: none of the [PostgreSQL rules](../.gitops/charts/platform/files/postgresql.rules.yaml) watches CPU. Read it with `kubectl top` (5.4).
- **[GAP] G7** No automatic capacity drain (2.2).

The maintenance cron runs every 5 minutes; probes run at minutes 1, 6, 11 and so on ([Worker configuration](../infra/cloudflare/control-plane/wrangler.template.jsonc)). Watch `WitselfProbeSchedulerStale`, `WitselfEntitlementDeliverySchedulerStale` and `witself_probe_scheduler_last_run_timestamp_seconds`. The maintenance cron also runs the reaper, placement runner, account-backup scan and plan lifecycle ([Worker](../infra/cloudflare/control-plane/src/index.js)); a missed maintenance tick costs one backup page.

### 5.4 Watch commands

These commands only read. They authenticate with the admin token or the fleet token (for example, `cells list` uses the admin token; `cells show`, `settings show` and `invite show` use the fleet token), resolved as [Connection, credentials, and output](witself-admin.md#connection-credentials-and-output) describes. During a wave use them only for these reads; every write made with the same tokens (drain, undrain, settings, invite changes) stays a **[FOUNDER CHECKPOINT]**. `witself-admin dashboard` also checks for a newer release at startup and every 6 h, upgrades itself and re-executes ([Dashboard and TUI](witself-admin.md#dashboard-and-tui)).

Run before, during and after a wave:

```bash
witself-admin cells list                                # accepting, accounts, archived per cell
witself-admin cells show civo-prod-use1-serving          # accepting, has_provision_token, has_backup_token
witself-admin cells show civo-sandbox-usw2-dev           # must stay accepting=false
witself-admin settings show
witself-admin invite show WAVE_CODE
witself-admin ticket list
witself-admin ticket watch
witself-admin dashboard                                 # self-upgrades
witself-infra health --json -cell civo-prod-use1-serving # same form as the two-wave roll
```

Production `kubectl` reads during waves are reads only and need the founder's go on the access form first (ask first) **[FOUNDER CHECKPOINT]**. `PROD_KUBECONFIG` stands for the founder-held production kubeconfig; never print it. Never create a default `~/.kube` entry for production. Where the founder approves it, `witself-infra cell-health -cell civo-prod-use1-serving` reports apiserver readiness, node Ready/total and Argo health without an operator kube context. Production enables metrics-server ([production values](../.gitops/cells/civo-prod-use1-serving/values.yaml)).

```bash
kubectl --kubeconfig "$PROD_KUBECONFIG" top nodes
kubectl --kubeconfig "$PROD_KUBECONFIG" -n witself top pods --containers # PostgreSQL CPU vs its limit
```

Prometheus queries go through the Kubernetes API service proxy. The production Prometheus exists only after monitoring phase 1; confirm its service name then.

```bash
P='/api/v1/namespaces/monitoring/services/http:witself-monitoring-kube-pr-prometheus:9090/proxy/api/v1/query'
enc() { jq -rn --arg q "$1" '$q|@uri'; }
qprod() { kubectl --kubeconfig "$PROD_KUBECONFIG" get --raw "$P?query=$(enc "$1")" | jq -r '.data.result[] | "\(.metric) \(.value[1])"'; }
qsand() { kubectl --context witself-civo-sandbox-use1-serving get --raw "$P?query=$(enc "$1")" | jq -r '.data.result[] | "\(.metric) \(.value[1])"'; }

# PostgreSQL working set fraction (adapted from WitselfPostgreSQLMemoryNearLimit, without the threshold)
qprod 'max by (pod) (container_memory_working_set_bytes{job="kubelet",metrics_path="/metrics/cadvisor",namespace="witself",container="postgresql"}) / on (pod) max by (pod) (kube_pod_container_resource_limits{job="kube-state-metrics",namespace="witself",container="postgresql",resource="memory"})'

# PostgreSQL volume used fraction (selector from WitselfPostgresPVCApproachingCapacity)
qprod 'max(1 - kubelet_volume_stats_available_bytes{namespace="witself",persistentvolumeclaim=~".*postgresql.*"} / kubelet_volume_stats_capacity_bytes{namespace="witself",persistentvolumeclaim=~".*postgresql.*"})'

# Server 5xx ratio over 15 minutes (adapted from WitselfServerHighErrorRatio's 5-minute ratio)
qprod 'sum(rate(witself_http_requests_total{status_class="5xx"}[15m])) / clamp_min(sum(rate(witself_http_requests_total[15m])), 0.001)'

# Everything firing on the cell
qprod 'ALERTS{alertstate="firing"}'

# Control-plane backup gauges: sandbox serving during T1; use production after ownership moves
qsand 'max(witself_control_plane_account_backup_stale_accounts)'
qsand 'max(witself_control_plane_account_backup_never_committed_accounts)'
```

**[FOUNDER CHECKPOINT]** Worker logs, only as the founder's decision on who reads Worker logs allows. [Runbooks](runbooks.md) uses `wrangler tail` on the control-plane Worker the same way; the Worker is `witself-control-plane` ([Worker configuration](../infra/cloudflare/control-plane/wrangler.template.jsonc)). Run from a clean checkout of the deployed tag, with dependencies installed:

```bash
cd infra/cloudflare/control-plane && npm ci
PATH="$PWD/node_modules/.bin:$PATH" wrangler tail witself-control-plane
```

Treat the whole stream as personal data. Watch it on screen only; never redirect it to a file or log. The verification-email failure line carries an account id and a fixed reason code, not a provider error or address. Before quoting any line, redact every 64-hex value and every account id (5.3).

## 6. Stop conditions

Two levels. **Pause** means: do not start the next wave, keep the current one open, look closer. **Brake** means: stop new accounts now, using section 7. Before any B2, check rule R2 and the `archived` column (B2 step 1).

The production PostgreSQL uses the `medium` resources preset of PostgreSQL chart 18.8.0 ([production values](../.gitops/cells/civo-prod-use1-serving/values.yaml)). Confirm the live limits before wave 1. The founder holds the pause and brake thresholds **[FOUNDER CHECKPOINT]**.

| Signal | Pause | Brake | Source |
|---|---|---|---|
| PostgreSQL working set vs its limit | The documented capacity threshold **[PROPOSED]** | The documented threshold: B2. `WitselfPostgreSQLMemoryNearLimit` (warning) or any PostgreSQL OOM kill or restart (critical): B1 and B2 | Founder-confirmed thresholds; [platform rules](../.gitops/charts/platform/files/platform.rules.yaml), [PostgreSQL rules](../.gitops/charts/platform/files/postgresql.rules.yaml) |
| Node working set vs allocatable | The documented capacity threshold **[PROPOSED]** | The documented threshold: B2 | Baseline below 60% at first sync **[PROPOSED]**; `WitselfNodeMemoryWorkingSetHigh` (warning) sits above the brake |
| PostgreSQL volume used | The documented capacity threshold **[PROPOSED]**, or `WitselfPostgreSQLVolumeFillingUp` fires | The documented threshold: B2, then section 7.5 | `WitselfPostgresPVCApproachingCapacity` (80% for 15 min, critical) sits above the brake |
| PostgreSQL CPU vs its limit | Above the documented CPU threshold in `kubectl top` **[PROPOSED]** **[GAP] G4** | Judgment, with the 5xx and transaction-age signals | No rule watches PostgreSQL CPU (**[GAP] G4**); read it with `kubectl top` (5.4) |
| PostgreSQL connections and transaction age | `WitselfPostgreSQLConnectionsHigh` or `WitselfPostgreSQLTransactionAgeHigh` firing | Either one firing together with a 5xx brake signal | [PostgreSQL rules](../.gitops/charts/platform/files/postgresql.rules.yaml) |
| Server 5xx ratio | The documented capacity threshold **[PROPOSED]** | `WitselfServerHighErrorRatio` (5% for 10 min) | `WitselfServerHighErrorRatio` in the [founder open-plane rules](../.gitops/charts/platform/files/founder-open-plane.rules.yaml) |
| Signup provisioning failures | Invite uses exceed new production accounts by 1 or more **[PROPOSED]** | Any confirmed `503 no capacity`, or `WitselfSignupProvisioningFailureRatioHigh` firing | Placement failures count as `provision_failed`, with no per-request log line (**[GAP] G1**); `WitselfSignupProvisioningFailureRatioHigh` is in the [uptime-probe rules](../.gitops/charts/platform/files/uptime-probes.rules.yaml) |
| Turnstile verifier trouble | One report of `503 turnstile verification unavailable` | Repeated reports during a wave: B4, an inside-wave exception to rule R4 **[FOUNDER CHECKPOINT]** (see section 7) | [Production activation and rollback](signup-abuse-hardening.md#production-activation-and-rollback); also `witself_signup_denials_total{reason="turnstile_unavailable"}` |
| Verification email failures | 1 failure **[PROPOSED]** | `WitselfVerificationEmailFailureRatioHigh` firing | Sending degrades gracefully; users can recover with resend |
| Global quota denial | Any denial under invite-only or the lowered-quota fallback means the cap was hit. Confirm it was invitees, not strangers. | Repeated denials of invitees | `WitselfSignupGlobalQuotaDenied` (warning) |
| Support | More than 5 new tickets in one day; any ticket unanswered 4 h during wave hours | Any ticket reporting data loss, another tenant's data, or a security problem | About 5 a day: the approved staffing trigger. 4 h: `WitselfSupportFirstResponseSlow`. The promise is 1 business day ([The promise](support-policy.md#the-promise)). |
| Backups | `never_committed_accounts` above the count created since the current 00:00 UTC interval began | `WitselfAccountBackupStale`, `WitselfPostgresBackupStale` or `WitselfPostgresBackupFailed` firing | [Founder open-plane rules](../.gitops/charts/platform/files/founder-open-plane.rules.yaml); [Postgres backup rules](../.gitops/charts/platform/files/postgres-backup.rules.yaml) |
| Control-plane schedulers | The probe-scheduler or entitlement-delivery timestamp stops advancing for more than 12 min **[PROPOSED]** | `WitselfProbeSchedulerStale` or `WitselfEntitlementDeliverySchedulerStale` (critical) firing | `WitselfProbeSchedulerStale` in the [uptime-probe rules](../.gitops/charts/platform/files/uptime-probes.rules.yaml); `WitselfEntitlementDeliverySchedulerStale` in the [entitlement-delivery rules](../.gitops/charts/platform/files/entitlement-delivery.rules.yaml) |
| Paging path | The founder unreachable for part of the wave window **[PROPOSED]** | Dead-man heartbeat lapsed, or the paging service lapsed | Paging continuity **[FOUNDER CHECKPOINT]**; [Alerts And Dashboards](observability-and-operations.md#alerts-and-dashboards) |

## 7. The brakes, fastest first

Use the lightest brake that stops the problem. All four are setting changes. All are **[FOUNDER CHECKPOINT]** until the founder rules who may pull B1 and B2 without asking first. None has been timed (**[GAP] G12**).

### B1. Disable the wave's invite code

Stops new uses of that code. Does not reset the count or remove history. Open signup, if on, keeps running. A signup that reserved a use before the disable can still finish: its retry replays the stored reservation before the code's state is checked ([signup runtime](../infra/cloudflare/control-plane/src/account-signup-runtime.mjs)).

```bash
witself-admin invite disable WAVE_CODE
witself-admin invite show WAVE_CODE
```

Undo **[FOUNDER CHECKPOINT]**: `witself-admin invite enable WAVE_CODE`.

### B2. Drain the production cell

Stops every new signup that would land on production. Pinned invites fail with `503 ... invite-pinned cell ... unavailable`. Open signups fail with `503 no capacity: no accepting cells` once no other cell accepts. Existing accounts keep their routes ([Worker](../infra/cloudflare/control-plane/src/index.js); [cell coordinator](../infra/cloudflare/control-plane/src/target-cell-coordinator.mjs); [Close signups](runbooks.md#close-signups)). **[GAP] G12** Drain-to-effect latency is unmeasured; the directory cache is not a bound on registry propagation (2.2).

1. Check that no account is mid-move. The `archived` column must read 0 for every cell. If it does not, an account is evacuated but not restored. Draining production leaves it offline until an eligible cell exists. Prefer B1 then, and ask the founder.
2. **[FOUNDER CHECKPOINT]** Drain and check:

   ```bash
   witself-admin cells list                           # archived column 0 everywhere
   witself-admin cells drain civo-prod-use1-serving
   witself-admin cells show civo-prod-use1-serving
   witself-admin cells show civo-sandbox-usw2-dev      # accepting=false
   ```

   **While production must stay drained, no `witself-infra up` may register it as accepting.** Every `up` that has a control plane, including one taken from the inventory's `control_plane` default and the `up` in [Change a Civo cell's node count](runbooks.md#change-a-civo-cells-node-count), registers the cell again with `accepting=true` unless `register_draining` is set. A re-registration with the same endpoint and provisioning token counts as the same instance, so it lifts B2 without any drain command. **[FOUNDER CHECKPOINT]** Set `register_draining: true` on the cell's own inventory entry (a per-cell key; defaults refuse it), or pass `-control-plane "" -register-draining=false -backup-validation-target=false` on every `up`. See [Registration in a drained state](../infra/pulumi/README.md#registration-in-a-drained-state) and [Close signups](runbooks.md#close-signups). The founder performs the serving-cell apply himself.
3. Tell invitees: rerun the exact same command later, with the same `--name`, email and code. That resumes the same provision and spends neither quota nor reservation again, even on a later UTC day. Each new pinned attempt during the drain already counts against both daily quotas for its UTC day and keeps one invite use reserved; nothing releases them. A new `--name` counts again and, if it carries an invite, reserves one more use (2.1 item 10).

Never undrain the legacy cell as a brake: that sends signups back to the sandbox serving cell. Undo **[FOUNDER CHECKPOINT]**: `witself-admin cells undrain civo-prod-use1-serving`, and remove `register_draining` from the cell record in the same step, before the next `up`. Undrain refuses a backup-validation target, or a cell without its provisioning credential (and, while account backups are on, without a distinct backup credential).

### B3. Reviewed control-plane deploy: change the signup gate or quotas (hours)

This is the only way to change `CP_SIGNUP_OPEN` or the daily limits: they are committed vars, not secrets (2.3). Run B3 between waves, never inside a wave window or the T2 window (rule R4; its one B3 exception is the founder's mid-window quota raise in section 11 step 5). Stay outside the backup window, 23:30Z-00:45Z (the default `--avoid-window` of the [release chain](../scripts/release-chain.sh)). Once weekly restore drills are on, also keep clear of the drill window as [Ship a fix and close signups without an AI agent](runbooks.md#ship-a-fix-and-close-signups-without-an-ai-agent) describes.

1. **[FOUNDER CHECKPOINT]** The founder picks the exact values:
   - Invite-only (the approved T1 posture): `CP_SIGNUP_OPEN` `"false"`, `CP_SIGNUP_DAILY_LIMIT_PER_IP` `"10"`, `CP_SIGNUP_DAILY_LIMIT_GLOBAL` `"500"` (or lower positive values).
   - Today's committed values, to restore: `"true"`, `"10"`, `"500"` ([Worker configuration](../infra/cloudflare/control-plane/wrangler.template.jsonc)).
   - The [renderer](../infra/cloudflare/control-plane/scripts/render-wrangler.mjs) refuses `CP_SIGNUP_OPEN` `"true"` with a zero limit; it allows zero limits when the gate is `"false"`. Keep both quotas positive for T1 as a backstop.
2. The operator prepares one PR that changes all holders together ([Production activation and rollback](signup-abuse-hardening.md#production-activation-and-rollback)):
   - The vars in the [Worker template](../infra/cloudflare/control-plane/wrangler.template.jsonc).
   - Both pin sites in the [deploy verifier](../infra/cloudflare/control-plane/scripts/verify-deployment.mjs).
   - The [control-plane deploy-config test pins](../infra/cloudflare/control-plane/test/deploy-config.test.mjs).
   - The [agent-email relay-key test fixture](../infra/cloudflare/agent-email/test/provision-relay-signing-key.test.mjs) and [script](../infra/cloudflare/agent-email/scripts/provision-relay-signing-key.mjs), which imports the verifier.
   - [Signup abuse hardening](signup-abuse-hardening.md), [Launch record](launch-readiness.md#launch-record) and the opening sentence and signup values in [Create an account on Witself Cloud](runbooks.md#create-an-account-on-witself-cloud).
   - All three signup entries in [the feature catalog](../featurestatus/catalog.json): the feature summary, entitlement-policy gate and bounds gate. Then regenerate [feature status](feature-status.md) with `make feature-status`.
   - **[FOUNDER CHECKPOINT]** The account step on the public product page ([builder](../web/product/build.mjs), which also generates `product.md`) runs an invite-less signup. The founder decides what witself.com says during T1. A brochure edit inside the B3 PR makes it a mixed change, published by hand after full CI; it can instead ship as its own brochure-only PR timed with the deploy.
   - Find every place with whole-repository searches:

     ```bash
     git grep -n -e CP_SIGNUP_OPEN -e CP_SIGNUP_DAILY_LIMIT
     git grep -n -E '500 global|10 per IP|10 signups per source IP'
     git grep -n -i -E 'invite-less signup|self-service signup is live|Start free'
     ```

3. Run the gate and read its verdict from the log ([repository gate rules](../CLAUDE.md#gates)):

   ```bash
   scripts/run-gate.sh --log /tmp/check.log check
   ```

4. **[FOUNDER CHECKPOINT] Merge. This is the point of no return.** From here, every later control-plane deploy ships the new values, including a security release (R3). Follow the [repository merge rule](../CLAUDE.md#change-discipline).
5. Before tagging, list what the tag would carry. `vLAST` is the newest release tag:

   ```bash
   git fetch -q origin --tags
   git log --oneline vLAST..origin/main
   git diff --stat vLAST..origin/main -- web/plans/plans.json infra/cloudflare/control-plane/wrangler.template.jsonc internal/store/migrations
   git diff vLAST..origin/main -- infra/cloudflare/control-plane/wrangler.template.jsonc | grep -n '"tag"'
   ```

   The last command shows any new Durable Object migration tag in the [Worker configuration](../infra/cloudflare/control-plane/wrangler.template.jsonc). Also run the read-only schema check: `scripts/release-schema-check.sh VERSION --serving-url PROD_SERVING_URL --execute`. `PROD_SERVING_URL` is the endpoint of `civo-prod-use1-serving`, not the sandbox `SERVING_URL`. Its last line names the gate.
6. **[FOUNDER CHECKPOINT]** Decide on what the tag carries. If it holds anything besides the signup change, stop and decide. Stop in particular for a database schema migration (any file under `internal/store/migrations` in the step 5 diff, or release-schema-check printing `gate=--backup-evidence`), a Durable Object migration, a `web/plans/plans.json` change or a control-plane/cell protocol change ([Decide: roll back or roll forward](runbook-control-plane-rollback.md#decide-roll-back-or-roll-forward) lists these as roll-forward cases). A `web/plans/plans.json` difference is a stop **[FOUNDER CHECKPOINT]**. Choose one:
   - Wait for a `main` that holds only the signup change on top of the last release, or
   - Release in the normal order, control plane first and then the cells. Begin with `scripts/release-schema-check.sh`. Read its last line. Roll the cells with `scripts/roll-train.sh VERSION --no-schema-change` only when it printed `gate=--no-schema-change`. If it printed `gate=--backup-evidence`, stop: this is a schema release. It needs current verified backups of both databases in the pair (civo-sandbox-use1-backup and civo-prod-use1-serving), as [Back up both reviewed Civo databases before a migration](runbooks.md#back-up-both-reviewed-civo-databases-before-a-migration) describes. It also needs a window and database settings the founder approves **[FOUNDER CHECKPOINT]**. The founder runs it with the [two-wave roll](runbooks.md#two-wave-roll-automated) and `--backup-evidence` for each cell. During T1 the founder's plan also covers the database of `civo-sandbox-use1-serving`, which still holds accounts.

     For an approved release, continue with `scripts/release-tag.sh`, `scripts/release-deploy-control-plane.sh`; the founder deploys the plans Worker by hand from the same clean tag; then roll the cells as [Two-wave roll (automated)](runbooks.md#two-wave-roll-automated) describes (add `--cells civo-sandbox-use1-backup,civo-prod-use1-serving` to the selected `scripts/roll-train.sh` gate; its default pair uses the sandbox serving cell). During T1 the drained sandbox serving cell, `civo-sandbox-use1-serving`, still holds accounts and is not in that pair. Bringing it to the same release is a separate step the founder decides and runs **[FOUNDER CHECKPOINT]** ([Two-wave roll (automated)](runbooks.md#two-wave-roll-automated)); it must not run `witself-infra up` without `register_draining` (rule R2). Until it runs, record that cell's version as release skew (step 9); then prove a restore drill with `scripts/release-drill-proof.sh --account-id DRILL_ACCOUNT_ID --fleet-token-file FLEET_TOKEN_FILE --execute` (the drill cell defaults to `civo-sandbox-use1-backup`), after confirming `witself_control_plane_account_backup_restore_drill_running` is 0, as [Ship a fix and close signups without an AI agent](runbooks.md#ship-a-fix-and-close-signups-without-an-ai-agent) describes. An absent or unavailable gauge is not zero; establish that no drill is running before proceeding ([Operate periodic account backups](runbooks.md#operate-periodic-account-backups)). `scripts/release-chain.sh` refuses to run while `civo-prod-use1-serving` is registered, so it cannot carry B3. Steps 7-8 detail the tag and control-plane stages of this sequence; do not run them twice.
7. **[FOUNDER CHECKPOINT]** Tag with `scripts/release-tag.sh VERSION --execute` (without `--execute` it prints the plan). It waits for CI on the commit and for the release run on the tag ([Ship a fix](runbooks.md#ship-a-fix)).
8. **[FOUNDER CHECKPOINT]** Deploy the control plane from that tag: `scripts/release-deploy-control-plane.sh VERSION --edge-token-file EDGE_TOKEN_FILE --deploy-env-file DEPLOY_ENV_FILE --attest-canonical-email-active --attest-signup-open-active --execute`, with the inputs that [Prepare the inputs](runbooks.md#prepare-the-inputs) describes (add `--cloudflare-token-file`, or set `CLOUDFLARE_API_TOKEN`). Without `--execute` it prints the plan only. The deploy changes live signup behaviour at once. It refuses a version below the live control plane. It deploys neither the plans Worker nor local tools: the founder deploys the plans Worker from the same tag, and the operator then runs `brew upgrade witself witself-admin witself-infra`. The signup attestation under invite-only is **[GAP] G15**.
9. If only the control plane was deployed, or a cell (during T1, `civo-sandbox-use1-serving`) was not rolled, those cells stay on the older release. Record that skew (control-plane version, cell versions, date) in a protected operational record; the next normal release closes it.
10. Check the result:
    - `curl -fsS https://self.witwave.ai/v1/version` shows the new version; the deploy script polls the same route before it prints PASS.
    - The [deploy verifier](../infra/cloudflare/control-plane/scripts/verify-deployment.mjs) passes only if live bindings equal the new pins.
    - For an invite-only switch, a new invite-less signup must fail with `403 invite code required`, before quota or Turnstile steps. Use a dedicated local name, because a taken name is refused before any remote call ([signup runtime](../infra/cloudflare/control-plane/src/account-signup-runtime.mjs); [the CLI](../cmd/witself/main.go); [Runbooks](runbooks.md)):

      ```bash
      witself account create --email TEST_ADDRESS --name b3-check
      ```

      A provision journal is left under `b3-check` after refusal. Use the same name and email for later B3 checks; if it is stranded, follow [Forget a stranded local name](runbooks.md#forget-a-stranded-local-name).
11. **Rollback.** A rollback to any tag older than the B3 tag also reverts the signup values: see rule R3 and hazard 6 in [Compatibility hazards](runbook-control-plane-rollback.md#compatibility-hazards-check-every-one-every-time). Any such difference is a stop; prefer a forward fix that keeps the brake. `scripts/release-deploy-control-plane.sh` refuses a version below the live one, so [Procedure A](runbook-control-plane-rollback.md#procedure-a-redeploy-the-previous-clean-release-tag-preferred) deploys from the previous tag's worktree. **[FOUNDER CHECKPOINT]** The founder gives the go; the operator runs it after a bold notice. Afterwards, rerun step 10. **[GAP] G8** That runbook is public but not rehearsed.
12. **[GAP] G8** Nobody has timed B3 end to end. Assume several hours. Use B1 or B2 first in an emergency.

### B4. Turnstile break-glass secret (minutes, unmeasured)

The documented use is to keep invited signups flowing during Turnstile verifier trouble after open signup is closed ([Production activation and rollback](signup-abuse-hardening.md#production-activation-and-rollback)).

- **[FOUNDER CHECKPOINT]** The founder performs this secret put himself: set `CP_SIGNUP_TURNSTILE_ENABLED` to a value other than `true` with `npm run secret:put:break-glass -- CP_SIGNUP_TURNSTILE_ENABLED`, under the provider-mutation freeze ([Worker configuration](../infra/cloudflare/control-plane/wrangler.template.jsonc); prerequisites in 8.1).
- Effect: invited signups continue without a challenge. If open signup were still enabled, invite-less signups would fail closed with `503 open signup configuration is invalid`; under invite-only, new invite-less signups are already refused with `403 invite code required` ([signup runtime](../infra/cloudflare/control-plane/src/account-signup-runtime.mjs)).
- Under invite-only, open signup is already closed, so B4 applies directly. Under the lowered-quota fallback, the docs close open signup first (B3, hours). The code would fail invite-less signups closed anyway, but departing from the documented order is the founder's call **[FOUNDER CHECKPOINT]**.
- B4 changes the pinned Turnstile state. An affected legal-phase signup cannot clear its refusal while the new policy holds: rerunning the same command replays it. A fresh signup with a new `--name` counts against both daily quotas again and, if it carries an invite, reserves one more invite use. The refused signup counted against both quotas for its UTC day but had not reserved a use. Prefer to run B4 between waves (R4). Inside a wave it is the founder's exception **[FOUNDER CHECKPOINT]**: weigh the invitees stalled by the verifier against the legal-phase signups that B4 will refuse. The founder restores the setting through the same secret-put path once the verifier is healthy **[FOUNDER CHECKPOINT]**.

### 7.5 Growing the PostgreSQL volume **[GAP] G9** **[FOUNDER CHECKPOINT]**

The repository has no PVC expansion procedure. It does not set the storage class's reclaim policy or establish its expansion support. Check the production cell's storage class, in the founder-approved access form, before relying on volume retention or expansion. Until a procedure exists:

1. Pull B2 first, and keep it pulled until the volume is grown and checked. While it is pulled, B2's `witself-infra up` rule applies.
2. Take a pg_dump the same day and restore-verify it before touching anything.
3. Follow R1. Do not delete the PVC or StatefulSet, not even with an orphaning delete, outside that verified-backup step.
4. The founder decides and performs the production change. Write the method down as a runbook before its first use.

## 8. Backup scheduler capacity vs account count

- The maintenance cron runs every 5 minutes. Each tick dispatches one directory page of account backups through the asynchronous `/start` path and waits only for acknowledgements ([Periodic logical account snapshots](backup-and-recovery.md#periodic-logical-account-snapshots)).
- Defaults: one generation per 1,440-minute interval, page size 4, concurrency 2. The page size accepts 1 to 20; any other value silently falls back to 4 ([backup runtime](../infra/cloudflare/control-plane/src/account-backup-runtime.mjs)).
- Each one-day interval starts at 00:00 UTC (the nightly 00:00Z backup export). The pass resumes across intervals in directory order. **[GAP] G5** Capacity is 288 × page size directory entries per interval (about 1,152 at 4, about 5,760 at 20), reduced by pending, archived and unrouted entries, which also use page slots. Above capacity the pass falls further behind each interval.
- When a pass overruns its interval, the accounts it has not reached count as stale, so `WitselfAccountBackupStale` (critical) pages. `witself_control_plane_account_backup_pass_failed_accounts` counts dispatches that were not accepted, not failed backups.

This table gives a lower bound for dispatch only. Export duration adds load; passes take longer when additional non-account entries use slots. Use the total directory across all cells, not the production account count.

| Directory entries (all cells) | Ticks needed at page 4 | Dispatch length at page 4 | Dispatch ends (UTC, from 00:00Z) | Dispatch length at page 20 |
|---|---|---|---|---|
| 100 | 25 | 2 h 05 min | 02:05Z | 25 min |
| 300 | 75 | 6 h 15 min | 06:15Z | 1 h 15 min |
| 500 | 125 | 10 h 25 min | 10:25Z | 2 h 05 min |
| 800 | 200 | 16 h 40 min | 16:40Z | 3 h 20 min |
| 1,000 | 250 | 20 h 50 min | 20:50Z | 4 h 10 min |

Rules for the push:

1. T1 adds up to about 300 accounts **[PROPOSED]** to a directory that already holds the sandbox serving cell's accounts. Read the pass length from the total directory count, not from production's count.
2. **[FOUNDER CHECKPOINT]** Before the directory passes about 800 entries **[PROPOSED]**, across all cells, the founder raises the page size to 20 (approved; 8.1). At about 800 entries the default pass dispatches until about 16:40Z, into wave hours.
3. The pg_dump CronJob runs at 03:00 UTC with a one-hour deadline ([Scheduled PostgreSQL backups](backup-and-recovery.md#scheduled-postgresql-backups)), inside the account-export dispatch window. The load test must cover that overlap.
4. A stalled maintenance cron costs one page per missed tick (5.3).

### 8.1 Set the backup page size **[GAP] G6** **[FOUNDER CHECKPOINT]**

This procedure is not rehearsed. [Periodic logical account snapshots](backup-and-recovery.md#periodic-logical-account-snapshots) documents the override.

1. **[FOUNDER CHECKPOINT]** The founder performs this Worker secret change himself, under the provider-mutation freeze ([Worker configuration](../infra/cloudflare/control-plane/wrangler.template.jsonc)), between waves.
2. Work from a clean worktree of the tag the control plane runs now (`curl -fsS https://self.witwave.ai/v1/version`); the [renderer](../infra/cloudflare/control-plane/scripts/render-wrangler.mjs) needs a clean, exactly tagged checkout.
3. Load the reviewed deploy environment file (the four deploy identifiers that `scripts/release-deploy-control-plane.sh --deploy-env-file` reads) and the Cloudflare credential into the shell. Never copy them into notes.
4. Install dependencies and put the secret. The [package script](../infra/cloudflare/control-plane/package.json) renders the config first. The secret prompt asks for the value; enter `20`:

   ```bash
   cd infra/cloudflare/control-plane && npm ci
   npm run secret:put:break-glass -- CP_ACCOUNT_BACKUPS_PAGE_SIZE
   ```

5. A value outside 1-20, or not an integer, silently falls back to 4 ([backup runtime](../infra/cloudflare/control-plane/src/account-backup-runtime.mjs)). Nothing reports the fallback.
6. Confirm the effect through the backup gauges after the next interval start (00:00 UTC): the pass finishes sooner and `never_committed_accounts` falls (5.4). The time to effect is unmeasured; compare with the whole directory's expected pass length.
7. Remove the temporary worktree.

## 9. Procedure: one T1 invite wave

Placeholders: `WAVE_CODE` (lowercase letters, digits, hyphens, 3-64 characters), `N` (wave size), `START_ISO`, `END_ISO` (UTC timestamps), `TEST_ADDRESS` (an address the founder controls). See [Signup invites](witself-admin.md#signup-invites).

### Three days before

1. Confirm every line of 4.1.
2. **[FOUNDER CHECKPOINT]** Approve the wave: `N`, start and end, and code style:
   - One shared code with `--max-uses N`, or
   - One single-use code per invitee (`--max-uses 1`, no `--code`, so the server generates it). Single-use codes limit what a forwarded code admits. **[GAP] G10** There is no bulk create. N codes mean N separate `invite create` calls, each a **[FOUNDER CHECKPOINT]** ([Signup invites](witself-admin.md#signup-invites)).
3. **[FOUNDER CHECKPOINT]** If invite-only is not yet live (the B3 step 10 check still admits an invite-less signup), run B3 now with the exact values from B3 step 1, at least a day before the wave and never inside a wave window (rule R4). B3 is a release, not a security fix, so it runs before the release freeze starts (more than 48 h before the wave) unless the founder exempts it **[FOUNDER CHECKPOINT]**. In this checklist that means finishing it before step 4.
4. Start the release freeze (approved: security fixes only, 48 h before to 72 h after each wave). Pause merge chains.
5. Check the move calendar. No evacuate or restore may fall inside the R2 freeze.

### The day before

6. Record baselines with the 5.4 commands: PostgreSQL memory fraction, node memory, volume fraction, PostgreSQL CPU, 5xx ratio, accounts and archived per cell, `never_committed_accounts`, open tickets.
7. Check placement and write down what was observed:

   ```bash
   witself-admin cells list
   witself-admin cells show civo-prod-use1-serving   # accepting true, has_provision_token true, has_backup_token true, backup_validation_target false
   witself-admin cells show civo-sandbox-usw2-dev    # accepting false
   witself-admin settings show
   ```

   Expected from [Settings](witself-admin.md#settings) and [Placement](witself-admin.md#placement):
   - Placement strategy: `weighted` (default), or `pinned` with `pinned_cell` `civo-prod-use1-serving`.
   - Placement runner: disabled, or restore-only with rebalance off, as the founder decides **[FOUNDER CHECKPOINT]**. Account moves run only when every cell is on a supported evacuation protocol ([Account evacuation transport rollout](deployment-cells.md#account-evacuation-transport-rollout)), the placement runner is disabled, the account has no open backup job, no train or control-plane deploy is running, and the time is outside 00:00-00:30Z ([Account moves in a chosen order](runbooks.md#account-moves-in-a-chosen-order)). If weekly restore drills are enabled, moves are also held from Wednesday 16:00Z until 21:32Z for the default window; follow the configured window otherwise. R2 still applies.
   - Reaper: record whether enabled and its TTL.
8. **[FOUNDER CHECKPOINT]** Canary through a pinned code. It counts once against each daily quota for that UTC day, reserves the canary code's one use, and leaves a closed-account tombstone ([Close an account](runbooks.md#close-an-account)). Choose a fresh wave-specific canary code and local name for each new wave: recreating an existing code preserves its use count and disabled state. Keep that same name, email and code for retries of the same canary. The block below is the wave-1 example. Follow [Create an account on Witself Cloud](runbooks.md#create-an-account-on-witself-cloud) and [Check account status](runbooks.md#check-account-status):

   ```bash
   witself-admin invite create --code wave1-canary --max-uses 1 --cell civo-prod-use1-serving --note "wave 1 canary"
   witself account create --email TEST_ADDRESS --invite wave1-canary --name wave1-canary --accept-terms
   # complete the challenge at the printed URL, then repeat with --challenge TOKEN
   witself-admin cells list                         # production count rose by one
   # confirm the verification email arrives at TEST_ADDRESS, then click its link
   witself account status --account wave1-canary   # must read active
   witself account close --yes --account wave1-canary
   witself-admin invite disable wave1-canary
   ```

   A `503 ... invite-pinned cell ... unavailable` means production is ineligible (2.2). A missing email can indicate a sender problem; verification email is a public-beta service ([Worker configuration](../infra/cloudflare/control-plane/wrangler.template.jsonc)). Stop and resolve either before the wave.

### Launch

9. **[FOUNDER CHECKPOINT]** Create the wave code or codes. The note is a wave label only (R5):

   ```bash
   witself-admin invite create --code WAVE_CODE --max-uses N \
     --not-before START_ISO --expires END_ISO \
     --cell civo-prod-use1-serving --note "T1 wave 1"
   witself-admin invite show WAVE_CODE
   ```

   Check that `cell` reads exactly `civo-prod-use1-serving`.
10. **[GAP] G10** Send the codes. Witself has no invite-delivery feature; the founder sends the codes by email. The message should say:
    - The command: `witself account create --email ADDRESS --invite WAVE_CODE --accept-terms`.
    - A browser challenge comes first, then the same command with `--challenge TOKEN`.
    - A verification email follows; check spam.
    - After any `503`, wait and rerun the exact same command, with the same `--name`, email and code. A rerun under a new `--name` starts another signup and uses another invite slot; changing the email or code under the same name is refused on their machine. For the legal-policy refusals in R4, that retry keeps refusing while the changed policy holds; the operator must explain the fresh-name recovery and its extra quota use.
    - People behind one office network share the 10-a-day per-IP quota and the 5-a-minute edge limit (2.1).
11. Start no earlier than 15:00 UTC on a weekday **[PROPOSED]**. Check the total-directory pass length in section 8; this start time alone does not prove that backup dispatch has finished.

### During the wave

12. **[PROPOSED]** Read the section 6 signals at +15 min, +1 h and +4 h, then at 15:00Z and 22:00Z each day until the code closes. Compare `invite show` uses with production account growth at each read (5.3). If the founder's ruling on who reads Worker logs during a wave allows it, keep `wrangler tail` open on screen for the first hour **[FOUNDER CHECKPOINT]**.
13. On a pause signal: do not plan the next wave; keep reading every hour.
14. On a brake signal: B1, then B2 if needed (check `archived` first), then tell invitees. Use B3 only for a lasting change, and only after the wave, and inside the release freeze only with the founder's exemption **[FOUNDER CHECKPOINT]**.
15. **[FOUNDER CHECKPOINT]** To raise the cap mid-wave, rerun `invite create --code WAVE_CODE --max-uses NEW_N`. The POST upserts the code and keeps its use count. Repeat the other flags to keep; fields omitted keep their stored values ([Signup invites](witself-admin.md#signup-invites)).

### Close

16. **[FOUNDER CHECKPOINT]** Disable the code at `END_ISO`, or when fully used:

    ```bash
    witself-admin invite disable WAVE_CODE
    ```

17. Run the after-wave checklist (section 10).

Space waves at least 72 h apart **[PROPOSED]** (the approved release freeze runs to 72 h after each wave). The freeze also starts 48 h before each wave, so with waves less than 120 h apart (close to start) the freezes overlap. No non-security release, B3 included, then fits between waves. That covers R4's between-wave B3, step 14's after-wave B3 and the lowered-quota fallback. The founder rules on spacing or a B3 exemption **[FOUNDER CHECKPOINT]**. Stop T1 at about 300 accounts **[PROPOSED]**.

## 10. After-wave checklist

1. **[FOUNDER CHECKPOINT]** The wave code is disabled or expired. Read `invite show` uses against the cap.
2. Read account counts per cell from `witself-admin cells list`. New accounts are all on production; the legacy cell's count did not rise; `archived` reads 0.
3. **Uses vs accounts.** Uses minus new production accounts indicates stranded reservations and abandoned signups (2.1 item 10). Compare before sizing a cap raise for the next wave; allow for canaries, closures and any organic signups under the lowered-quota fallback.
4. **Capacity after vs before.** Compare PostgreSQL memory, node memory, volume fraction and CPU with baselines. Work out growth per new account: (volume used after - before) / new accounts. **[PROPOSED]** Size the next wave so the forecast keeps every signal under its pause line in section 6 through that wave.
5. **Backups reached the new accounts.** After the next interval start at 00:00 UTC and completion of its pass (section 8), `never_committed_accounts` falls back and `WitselfAccountBackupStale` is quiet. The next pg_dump at 03:00 UTC succeeded.
6. The 5xx ratio over the wave window stayed under the pause line.
7. Signup failures: read `increase(witself_signup_events_total{job="witself-probes",event="provision_failed"}[WINDOW])` (placement failures included) and the verification-email failure counter on the cell that scrapes the control plane, and compare with step 3. Replace `WINDOW` with the wave's PromQL duration. Workers Observability only with the founder **[FOUNDER CHECKPOINT]**.
8. Pending (unverified) accounts: read `witself_signup_pending_accounts` and `witself_signup_pending_oldest_age_seconds` (exported, with no paging rule) only while `witself_signup_pending_scan_complete` is 1. Check the reaper setting once (read-only):

   ```bash
   witself-admin settings reaper show
   ```

9. Support: every wave ticket got a first response within 1 business day ([The promise](support-policy.md#the-promise)). Count tickets per day against the support staffing trigger (about 5 tickets a day).
10. Billing: any upgrades from the wave delivered their entitlements; billing and entitlement alerts are quiet (5.1).
11. Retention preview counts on production read and noted for the founder's retention-enforcement decision **[FOUNDER CHECKPOINT]**. Production runs transcript, message and agent-email retention in preview ([production values](../.gitops/cells/civo-prod-use1-serving/values.yaml)).
12. Signup posture unchanged. If any control-plane deploy or rollback ran since the last check, rerun B3 step 10 (R3).
13. The approved release freeze ends 72 h after the wave closes. The R2 move freeze ends 24 h after the code closes.
14. Record a value-free wave summary in a protected operational record: dates, `N`, uses, peak numbers, incidents. No customer identities, email addresses or log lines.
15. Go or no-go for the next wave: every section 6 signal below its pause line, and the backup rule in section 8 still holds.

## 11. Procedure: the T2 open push

### Entry gate

Soak 3-7 days, then retire the old serving cell. In parallel, enforce retention after the preview counts, ship the caps and finish backup hardening. T2 opens after that. All T1 gates, plus:

1. The old serving cell is retired after the soak. Paging ownership and the control-plane scrape move to production with the `control_plane_alerts` catalog switch before the old cell is destroyed. Turning production monitoring on (gate 4.1, item 2) leaves `control_plane_alerts` false, and ownership stays on the sandbox serving cell. Ownership moves only in the PR that sets production's `control_plane_alerts` to true. A test allows one owner, and the sandbox serving overlay owns these groups whenever its monitoring is on, so that same PR ends the sandbox serving cell's ownership ([GitOps values generation](gitops-values-generation.md)). With the current overlay, that PR either changes the sandbox serving overlay so the two groups follow that cell's own `control_plane_alerts` switch, or sets that cell's `monitoring` to false. Turning monitoring off also ends that cell's own PostgreSQL, backup, platform and Argo CD alerts, its paging and its dead-man heartbeat. Turn it off only after the cell holds no accounts **[FOUNDER CHECKPOINT]**. Reconfirm receivers and paging **[FOUNDER CHECKPOINT]**.
2. Retention enforced for transcripts and messages after preview counts are read **[FOUNDER CHECKPOINT]**; a per-plan transcript storage cap live. Not met: the cap is not on `main`; its per-account storage counter is (schema 101), and nothing enforces it yet. The counter's reconcile job is off by default ([Cell worker](cell-worker.md)). Agent-email retention also stays in preview until the founder reads its counts and approves enforcement.
3. Per-account request and write budget live: available on `main`, off until a cell's `request_budget` catalog switch turns it on, with chart and image v0.0.334 or newer. Per-plan tiers exist in the server chart (`requestBudget.plans`, empty by default); their cell passthrough is not on `main`, and values remain a founder decision ([Production Defaults](helm-chart.md#production-defaults)).
4. Signup and verification-email metrics and rules live on the cell that scrapes the control plane (on `main`; they move with item 1). Aggregate placement-failure counts do not close the missing per-request detail in **[GAP] G1**.
5. Backup scheduler fixed (met: slice 121) and page size 20 **[FOUNDER CHECKPOINT]** (section 8).
6. Backups hardened: more frequent pg_dump with matching stale alerts (planned), a dump-expiry rule, an off-Cloudflare copy and a timed R2 restore ([Scheduled PostgreSQL backups](backup-and-recovery.md#scheduled-postgresql-backups)). Optional account-snapshot restore-drill warnings (`WitselfRestoreDrillStale`, `WitselfRestoreDrillFailed`) and the weekly scheduler are on `main`; the scheduler is off by default. Neither is a timed R2 restore of pg_dump.
7. Load test at 256 principals (**[GAP] G17**).
8. Public entry point and quickstart: met by [witself.com](https://witself.com), its setup section (install the CLI; create an account and agent; connect an AI tool), and its linked plain-Markdown product and setup guide. The repository has no quickstart document. Codex and Claude Code are certified; Cursor, Grok Build and DeepSeek Harness are labeled preview ([live runtime acceptance](memory-runtime-acceptance.md)). Before reopening, restore the public product account step and every B3 holder for open signup.
9. A control-plane probe or an external signup check: still open; the self-probe is off in committed [configuration](../infra/cloudflare/control-plane/wrangler.template.jsonc), and no external signup check exists. The [control-plane rollback runbook](runbook-control-plane-rollback.md) is public but not rehearsed; rehearse it **[FOUNDER CHECKPOINT]**, only after the founder's go and a bold notice ([Founder checkpoints](runbook-control-plane-rollback.md#founder-checkpoints)).
10. A hosted status page **[FOUNDER CHECKPOINT]**. Until one exists, the GitHub `incident` label is the canonical channel ([Support Policy](support-policy.md)). The founder performs public incident posts himself.
11. Support staffing: approved (AI responder, read-only triage, above about 5 tickets a day). Personal users go to GitHub Discussions once enabled; it is not enabled.
12. Scale trigger: a third node is pre-approved at sustained node memory above 75% or about 500 active accounts ([Change a Civo cell's node count](runbooks.md#change-a-civo-cells-node-count)).
13. A written PVC growth procedure (7.5, **[GAP] G9**).

### Steps

1. **[FOUNDER CHECKPOINT]** Set the T2 window (start and end dates) and quotas from the load test. Quotas rise only for a set T2 window, after the load test, through a reviewed PR (decided 2026-10-13).
2. **[FOUNDER CHECKPOINT]** Run B3 before the window opens, with `CP_SIGNUP_OPEN` `"true"` and the chosen positive limits. Restore the same holders listed in B3 step 2, including the product account step, all three feature-catalog entries and the runbooks opening sentence. Regenerate feature status. Coordinate brochure publication with the deploy. The global counter is shared with any invite codes still open.
3. The day before: baselines and placement checks as in section 9 steps 6-7. After ownership moves, query production for the control-plane gauges instead of the sandbox serving cell.
4. **[FOUNDER CHECKPOINT]** Publish the entry point; the founder performs any public incident post himself.
5. Watch as in section 9 steps 12-14. `WitselfSignupGlobalQuotaDenied` firing means the day's quota is gone; under T2 that signals demand. Prefer letting the day reset at 00:00 UTC. A mid-window B3 is R4's second founder exception: affected legal-phase signups replay the refusal while the changed policy holds and need a fresh `--name`. The refused signup already counted against both quotas for its UTC day but had not reserved an invite use. The fresh signup counts against both quotas again and, if it carries an invite, reserves one more invite use. **[FOUNDER CHECKPOINT]** If the founder still raises the quota mid-window, deploy after the day's quota is used up, when few signups are in flight.
6. The daily global quota bounds how fast a push lands accounts. At the committed 500 a day, 2,000 accounts need at least 4 UTC days; with the quota chosen for the window, divide the target by that quota. Plan the T2 window length from both ([Worker configuration](../infra/cloudflare/control-plane/wrangler.template.jsonc); [Launch record](launch-readiness.md#launch-record)).
7. **[FOUNDER CHECKPOINT]** Close the window with B3, back to the post-window quotas, after the window ends.
8. Run the after-wave checklist (section 10) for the whole window.

## Gaps

- **G1.** A placement failure (`503 no capacity` and other refusals) counts as `provision_failed` but has no per-request log line, and the counter names no cell or code.
- **G2.** No read of the day's quota use; only denials are counted.
- **G3.** No dashboards; Grafana is off.
- **G4.** No PostgreSQL CPU rule.
- **G5.** The backup scheduler's daily capacity is 288 × page size directory entries, reduced by non-account entries; above it the pass falls behind (and `WitselfAccountBackupStale` pages).
- **G6.** The page-size procedure (8.1) is not rehearsed, and an out-of-range value falls back to 4 silently.
- **G7.** No automatic capacity drain; placement ignores capacity.
- **G8.** The [control-plane rollback runbook](runbook-control-plane-rollback.md) is not rehearsed, and the B3 path has never been timed.
- **G9.** No documented PVC expansion procedure (7.5).
- **G10.** No invite-delivery feature, and no bulk invite creation.
- **G11.** Invite uses and quota units spent by a failed placement are never released.
- **G12.** The time to effect of drain, invite and secret changes is unmeasured.
- **G13.** The control-plane scrape and its alerts stay on the sandbox serving cell until the `control_plane_alerts` switch moves to production; if that cell went away first, the control-plane gauges would stop. Its overlay turns these groups on whenever its monitoring is on, so the move needs an overlay change or turns off every alert on that cell.
- **G14.** Production monitoring and alerting are off in its values and catalog until monitoring phases 1-3.
- **G15.** Under invite-only every control-plane deploy still attests open signup (2.3).
- **G16.** Provisioning the production cell, moving accounts onto it and retiring the old cell have no public runbook.
- **G17.** No public procedure runs the load harness on the production cell with backup and export load running.
