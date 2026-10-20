# Roll the control plane back to the previous release

Status: not rehearsed. No step below has been run as a drill or timed end to end. Verified against `main` at `8f5545dc6548` on 2026-10-19; re-check every command against the current tree before relying on it.

[FOUNDER CHECKPOINT]: the founder decides; he runs the step himself or says go first.

[GAP] Gn: an unresolved gap numbered Gn in this file's `## Gaps` list.

The control plane is the `witself-control-plane` Cloudflare Worker and a Go
container. The Worker fronts the container through a Durable Object. A source
deploy builds the container image using the tagged
[Dockerfile](../images/witself-control-plane/Dockerfile) and
[Worker configuration](../infra/cloudflare/control-plane/wrangler.template.jsonc).

The container rolls separately from the outer Worker. The
[deploy verifier](../infra/cloudflare/control-plane/scripts/verify-deployment.mjs)
checks the outer Worker first, then polls the container's `/v1/version` for the
same release identity. During rollback, PREV's Worker can front CUR's container
while it converges. Normal forward releases exercise the opposite pairing.
The configuration permits up to two container instances; the
[Backend class](../infra/cloudflare/control-plane/src/index.js) also permits
idle instances to sleep, so recovery can include cold starts. The polling
configuration is not a measured bound on rollback or on the mixed-version window.

This procedure rolls back only the control-plane Worker and its container. Cells,
the receive, send, legal and plans Workers, DNS, secrets and stored state have
separate lifecycles.

## Founder checkpoints

- **[FOUNDER CHECKPOINT]** The operator may run a control-plane rollback or
  rehearsal only after the founder's explicit go and a bold notice naming this
  runbook, CUR, PREV and the expected impact. A routine forward deploy is not
  rollback authorization.
- **[FOUNDER CHECKPOINT]** Every stop in Procedure A goes back to the founder.
- **[FOUNDER CHECKPOINT]** The founder performs Worker secret changes himself,
  including `wrangler secret put` and `npm run secret:put:break-glass`.
  This procedure changes no secret.
- **[FOUNDER CHECKPOINT]** The founder performs any deploy or rollback of the
  receive, send, legal or plans Worker himself. Neither `npm run deploy:plans`
  nor `npm run deploy:legal` is part of this rollback.
- **[FOUNDER CHECKPOINT]** The founder performs Procedure B himself, including
  any Wrangler version deployment or dashboard rollback.
- **[FOUNDER CHECKPOINT]** Handling the control-plane edge token outside the
  forward wrapper is part of the execution go in Procedure A step 7; without
  that go, the operator does not read the token. Deciding whether to hold or
  revert the newer brew tap formula also requires an explicit decision.
- **[FOUNDER CHECKPOINT]** The founder performs DNS, R2, Turnstile, production
  preview/apply/destroy and public incident posts himself. Registry or directory
  writes, paging changes and account moves require his explicit go before an
  operator acts. None is a step in this rollback.
- Preserve PostgreSQL PVCs and StatefulSets: never delete them as a rollback
  remedy. Do not move accounts near a rollout wave.

## Decide: roll back or roll forward

Roll back only when all of these hold:

1. The fault is in the control plane, began at its deploy, and cells still answer
   normally at their own `/v1/version`.
2. It hurts customers or blocks fleet work: for example directory resolution,
   signup, billing webhooks, the fleet API or scheduled work. The
   [client directory code](../internal/client/directory.go) resolves cloud
   account endpoints; the [Worker](../infra/cloudflare/control-plane/src/index.js)
   implements the edge routes and scheduled dispatch.
3. A fix cannot ship soon through the normal tagged release path. Use the
   [release procedure](runbooks.md#ship-a-fix-and-close-signups-without-an-ai-agent),
   including its explicit exceptions to control-plane-first order.
4. PREV was deployed and verified on this control plane, with retained evidence
   from Procedure A step 2. A tag alone is insufficient.
5. Every compatibility and in-flight-work check below passes.

Roll forward if CUR wrote state PREV cannot read, changed the plan catalog,
added a Durable Object migration, changed committed configuration, establishes a
security floor, or supplies a contract the cells or email edge still need.
A configuration fault also requires the founder: committed plain variables
change through a reviewed deploy; break-glass runtime gates change through the
founder's secret workflow.

The [deploy script](../infra/cloudflare/control-plane/scripts/deploy-release.mjs)
attests the email edge, takes its operations lease and checks dark secrets before
the Wrangler mutation. If it stopped before any mutation, there is no new
deployment to undo. If Wrangler deployed but verification failed, redeploying a
previous clean tagged release still requires all the checks in this runbook.

## Compatibility hazards (check every one, every time)

1. **Durable Object migrations.** The
   [template](../infra/cloudflare/control-plane/wrangler.template.jsonc) contains
   seven bindings and migration tags v1 through v8. Treat migrations as a
   forward-only boundary. If CUR added a migration, stop and roll forward.
   How an older configuration lacking a live migration behaves in a rollback
   has not been rehearsed here.

2. **Control plane and cells.** The cell
   [version response](../internal/server/server.go) attests
   `account_evacuation_protocol`, `account_provision_protocol`,
   `store_schema_version` and `backup_validation_protocol`.
   The current [lifecycle runtime](../infra/cloudflare/control-plane/src/account-lifecycle-runtime.mjs)
   accepts evacuation protocols 1, 2 and 3 and checks archive schemas against
   the destination cell. The [signup runtime](../infra/cloudflare/control-plane/src/account-signup-runtime.mjs)
   requires provision protocol 1. The [backup runtime](../infra/cloudflare/control-plane/src/account-backup-runtime.mjs)
   supports asynchronous validation when the target attests backup validation
   protocol 2 or later. Inspect PREV's implementations, not only the current ones.

   Rolling the control plane back after training cells pairs newer cells with
   an older control plane. Any changed control-plane-to-cell contract is a
   stop unless the documented rollout establishes compatibility with PREV.
   Cells-first examples include the backup-cell echo header and
   [validation archive pulls](backup-and-recovery.md#restore-drill-archive-pull-protocol).
   Protocol 3 needs the opposite order: an older control plane can reject the
   new probe. Follow the [protocol rollout rules](backup-and-recovery.md#evacuation-archive-transport-protocols-2-and-3).
   If the protocol probe fails, start no drain, move, restore or
   `witself-infra destroy` until compatibility is restored.

3. **Control plane and email edge.** The control plane signs route rows consumed
   by the receive edge and serves the operations lease and managed-delivery
   readiness routes. Review the [email contract](../infra/cloudflare/agent-email/README.md)
   and [lease implementation](../infra/cloudflare/control-plane/src/agent-email-operations-lease.mjs).
   A rollback acquires its lease on CUR and completes/releases it on PREV.
   Marked entitlement snapshots introduce a shared forward-only boundary across
   cell, control plane and edge; see [agent-email entitlement](billing-and-limits.md#agent-email-availability-and-retention).

   The deploy reads the live edge release and enforces the historical v0.0.241
   cohort transition; it does not generally reject a control plane older than
   the edge. **[FOUNDER CHECKPOINT]** Stop if the edge is newer than PREV and
   route-signing, lease, readiness or cohort code changed, or if documentation
   names a shared boundary PREV cannot satisfy.

4. **State written by CUR stays.** Durable Object storage, KV and R2 do not
   revert. Older code may reject an unknown phase or key, or ignore a new key
   and fail open. For example, [signup legal state](../infra/cloudflare/control-plane/src/signup-legal.mjs)
   uses exact key sets. Review the documented floors for abort phases in
   [Runbooks](runbooks.md), audited dimensions and alias activation in
   [Billing and limits](billing-and-limits.md), the
   [billing cutover](billing-transition-rollout.md#non-rolling-compatibility-boundary),
   custom-domain lifecycle journals and the forward-only verification refresh
   in [the dark custom-domain authority](runbooks.md#deploy-checkpoint-and-drill-the-dark-custom-domain-authority),
   and refresh state in [Agent email](agent-email.md).

   These examples are not a complete inventory. Any changed stored key,
   `schema_version`, phase or enum is a stop unless documentation establishes
   that PREV reads it. Procedure A searches both tags' documentation and
   reviews code changes to catch undocumented boundaries.

5. **The plan catalog is compiled into the container.** The
   [Go embed](../plans.go) includes `web/plans/plans.json`. A control-plane
   rollback leaves the public plans Worker at CUR while reverting the
   container's catalog. **[FOUNDER CHECKPOINT]** Any difference in that file
   between the tags is a stop.

   Approved (2026-10-13): the release chain will deploy the plans Worker right
   after the control plane, from the same clean tag. This is not yet built.
   Until then, deploy it by hand from the same clean tag, as
   [Release and build](release-and-build.md) describes. The
   [manual release procedure](runbooks.md#ship-a-fix-and-close-signups-without-an-ai-agent)
   also covers the separate plans-page deployment. The founder performs that
   deployment himself; it is outside this rollback.

6. **Committed configuration reverts.** PREV restores its plain variables,
   crons, routes, KV/R2 and service bindings, required secrets, Durable Object
   bindings and rate limiters. Relevant variables include `CP_SIGNUP_OPEN`,
   `CP_SIGNUP_DAILY_LIMIT_PER_IP`, `CP_SIGNUP_DAILY_LIMIT_GLOBAL`,
   `CP_SIGNUP_LEGAL_ENFORCEMENT`, `CP_PUBLIC_ORIGIN`, queue ceilings and probe
   configuration. **[FOUNDER CHECKPOINT]** Any such configuration difference
   is a stop. Reverting a signup brake can reopen signup; ship a forward fix
   that preserves the brake.

   Three [render inputs](../infra/cloudflare/control-plane/scripts/render-wrangler.mjs)
   come from the deploying shell: `EMAIL_DIRECTORY_KV_ID`,
   `AGENT_EMAIL_ROUTE_SIGNING_KEY_ID` and
   `CP_AGENT_EMAIL_MANAGED_DELIVERY_ACCOUNT_ALLOWLIST`. Use reviewed current
   values. The template requires the cohort to match the edge byte for byte;
   the deploy also refuses a cohort that drops any account present at the edge.

7. **Secrets stay as they are.** Procedure A writes no secret. Existing Worker
   secrets carry into the deployment. PREV may ignore a newer runtime gate;
   its dark-secret check may instead refuse a gate that is now present. Such
   refusal is a stop: never remove a secret to pass it.
   `CP_DEPLOY_CANONICAL_EMAIL_ACTIVE` and `CP_DEPLOY_SIGNUP_OPEN_ACTIVE`
   are deploy attestations, not commands that activate their corresponding
   features. PREV must support the attestations required by reviewed live
   state. Inspect its [deploy script](../infra/cloudflare/control-plane/scripts/deploy-release.mjs).

8. **Security floor.** **[FOUNDER CHECKPOINT]** Never roll below a required
   security release. The tagged Dockerfile, `go.mod`, `go.sum` and npm lockfile
   determine the toolchain and dependencies. Review dependency-only changes
   and the release notes too; release notes identifying a security fix are a
   stop. Do not assume unchanged application code means a safe downgrade.

9. **The deploy path depends on the live control plane.** `npm run deploy`
   acquires the shared email operations lease from `https://self.witwave.ai`.
   The lease client needs `CONTROL_PLANE_EDGE_TOKEN`; Wrangler needs
   `CLOUDFLARE_ACCOUNT_ID` and `CLOUDFLARE_API_TOKEN`; Docker must be running
   to build the image. The narrowly scoped historical v0.0.242 lease bootstrap
   exception is not a recovery bypass for other targets. If CUR broke the
   lease routes, Procedure A cannot run. **[FOUNDER CHECKPOINT]** Only the
   untested Procedure B remains: **[GAP] G1**, **[GAP] G6**.

10. **Targets need retained verification evidence.** The
    [source-identity check](../infra/cloudflare/control-plane/scripts/source-identity.mjs)
    requires a clean checkout at exactly one semantic-version tag. The
    [release snapshot](../infra/cloudflare/control-plane/scripts/control-plane-release-snapshot.mjs)
    has bounded file/byte inventories; older tags can have lower limits.
    A snapshot refusal is a stop. Never infer that a release deployed merely
    because its tag exists: **[GAP] G2**.

11. **Customer CLIs do not roll back.** The
    [release workflow](../.github/workflows/release.yml) publishes the CLI
    formulae to the brew tap. A newer CLI may call a route PREV cannot handle;
    unmatched Worker routes reach the Go container, which may reject them.
    Review [API routes](api-routes.md) and [operator commands](witself-admin.md).
    **[FOUNDER CHECKPOINT]** Decide whether to hold or revert the tap formula;
    no repository command implements that operation: **[GAP] G4**.

## Procedure A: redeploy the previous clean release tag (preferred)

**[FOUNDER CHECKPOINT] The operator runs this control-plane rollback only after
the founder's explicit go. Before starting, issue a bold notice naming
`docs/runbook-control-plane-rollback.md`, CUR, PREV and the expected impact.
The same rule applies to a rehearsal.**

The forward wrapper [release-deploy-control-plane.sh](../scripts/release-deploy-control-plane.sh)
refuses a version below the live control plane by design. Run `npm run deploy`
directly from PREV's worktree below. Use bash for the shell steps. Use
`CUR=<release live now>` and `PREV=<release before it>`, without the `v` prefix;
replace all placeholders before running any command.

1. **Record the live version.**

   ```bash
   curl -fsS -m 10 https://self.witwave.ai/v1/version | jq -c '{version,commit}'
   ```

2. **Set the versions and prove PREV ran.**

   ```bash
   REPO=/path/to/a/witself/checkout
   CUR='<release live now>'
   PREV='<release before it>'
   git -C "$REPO" fetch -q origin --tags
   git -C "$REPO" rev-parse -q --verify "refs/tags/v$PREV^{commit}"
   ```

   Retain PREV's successful deploy output, including both verifier lines:
   `verified outer Worker <version-id> (<etag>)` and
   `verified witself-control-plane <PREV> (<commit>)`.
   Keep the outer Worker version ID for Procedure B. Verify that those lines
   belong to the same completed deployment. Missing logs are **[GAP] G2**;
   take the missing evidence to the founder and stop.

3. **Prepare a clean PREV worktree.** These steps install local dependencies
   and do not deploy anything.

   ```bash
   umask 022
   WT="$(mktemp -d)/witself-v$PREV"
   git -C "$REPO" worktree add --detach "$WT" "v$PREV"
   test -z "$(git -C "$WT" status --porcelain)"
   git -C "$WT" tag --points-at HEAD --list 'v*'
   cd "$WT/infra/cloudflare/control-plane"
   npm ci
   ```

   Require each command to succeed and the tag list to contain exactly
   `v$PREV`. Stop on any failure.

4. **Review repository compatibility.** These reads do not change the fleet.

   ```bash
   # Hazards 1 and 6: must be empty.
   git -C "$REPO" diff "v$PREV" "v$CUR" -- infra/cloudflare/control-plane/wrangler.template.jsonc
   # Hazard 5: must be empty.
   git -C "$REPO" diff --stat "v$PREV" "v$CUR" -- web/plans/plans.json
   # Hazard 8: review all changed toolchain/dependency inputs and release notes.
   git -C "$REPO" diff "v$PREV" "v$CUR" -- images/witself-control-plane/Dockerfile go.mod
   git -C "$REPO" diff --stat "v$PREV" "v$CUR" -- go.sum infra/cloudflare/control-plane/package.json infra/cloudflare/control-plane/package-lock.json
   # Inventory the whole tree so new imports and deploy helpers are not omitted.
   git -C "$REPO" diff --name-status "v$PREV" "v$CUR"
   # Hazards 2-4 and the Worker/container contract: review these code changes.
   git -C "$REPO" diff --stat "v$PREV" "v$CUR" -- infra/cloudflare/control-plane cmd internal regions plans.go
   # Hazard 3: review both sides of the email contract.
   git -C "$REPO" diff --stat "v$PREV" "v$CUR" -- infra/cloudflare/agent-email infra/cloudflare/agent-email-send
   # Read the whole docs diff; the following searches are aids, not completeness checks.
   git -C "$REPO" diff "v$PREV" "v$CUR" -- docs
   git -C "$REPO" grep -nEi 'rollback floor|forward-only|control plane first|cells first|roll forward|roll back to v0' "v$CUR" -- docs
   git -C "$REPO" grep -nEi 'rollback floor|forward-only|roll back to v0' "v$PREV" -- docs
   # Inspect the exact acceptance logic in PREV, including surrounding branches.
   git -C "$REPO" show "v$PREV:infra/cloudflare/control-plane/src/account-lifecycle-runtime.mjs" | grep -n 'account_evacuation_protocol'
   git -C "$REPO" show "v$PREV:infra/cloudflare/control-plane/src/account-signup-runtime.mjs" | grep -n 'account_provision_protocol'
   git -C "$REPO" show "v$PREV:infra/cloudflare/control-plane/src/account-backup-runtime.mjs" | grep -n 'backup_validation_protocol'
   ```

   A stat or search result is an inventory, not a compatibility verdict. Read
   every relevant changed file, including newly imported packages, embedded
   files and deploy helpers from the whole-tree list. Inspect stored keys,
   schema strings, phases, enums, cell protocols, email signing/lease/readiness/
   cohort logic, container routes and the `Backend` environment bridge. Avatar
   continuity uses unrelated rollback-floor terminology; classify each match.
   A missing file or unfamiliar contract is a stop, not proof of safety.

5. **Read the live fleet and email edge.** `cells list --json` wraps rows in
   `cells`; each row has a name and optional endpoint, as defined by the
   [fleet CLI](../cmd/witself-admin/fleet_cmd.go) and
   [admin client](../internal/client/admin.go).

   ```bash
   witself-admin cells list --json | jq -r '.cells[] | "\(.name) \(.endpoint // "NO-ENDPOINT")"'
   witself-admin cells list --json | jq -r '.cells[] | select(.endpoint) | .endpoint' |
     while read -r ep; do
       printf '%s ' "$ep"
       curl -fsS -m 20 "$ep/v1/version" | jq -c '{version,account_evacuation_protocol,account_provision_protocol,store_schema_version,backup_validation_protocol}'
     done
   ```

   Missing endpoints, failed reads or absent attestation fields require
   resolution before continuing. Inspect the edge from PREV's worktree with
   its pinned Wrangler. Use only the reviewed Cloudflare account/token and a
   clean inspection environment following
   [wrangler-environment.mjs](../infra/cloudflare/agent-email/scripts/wrangler-environment.mjs);
   remove its unsafe environment variables and suppress file logging and
   telemetry. `/dev/null` prevents loading local dotenv files.

   ```bash
   cd "$WT/infra/cloudflare/control-plane"
   npx wrangler deployments status --name witself-agent-email-receive --json --env-file /dev/null
   npx wrangler versions view '<edge-version-id>' --name witself-agent-email-receive --json --env-file /dev/null |
     grep -n -A2 'WITSELF_EDGE_RELEASE_VERSION'
   ```

   Require one edge version serving all traffic and use its ID in the second
   command. **[FOUNDER CHECKPOINT]** Stop on a migration/config/catalog change,
   security downgrade, incompatible stored state, an unsupported cell protocol,
   or a newer edge with changed shared contracts. Reconcile live state with
   PREV's exact acceptance logic before requesting the execution go.

6. **Check work in flight.** Follow the current deploy/backup exclusion windows
   and weekly-drill rules in the
   [manual release procedure](runbooks.md#ship-a-fix-and-close-signups-without-an-ai-agent)
   and [periodic backup procedure](runbooks.md#operate-periodic-account-backups).
   The scheduler can continue a traversal across slots; the nightly start time
   alone is not a sufficient check. Read the current backup status with
   `GET /v1/backups/status`. No restore drill may be running. At the verified
   commit the control plane exports no fleet-wide signal that a drill is
   running (**[GAP] G3**), so for every account with a known or possible
   drill, read `GET /v1/backups/status?account_id=<account-id>` and require
   that `account.restore_drill.state` is not `running`, as in
   [Operate periodic account backups](runbooks.md#operate-periodic-account-backups).
   If a later release adds a drill-running gauge on `/metrics/probes`, also
   require it to be 0; an absent gauge is not zero. If drill state is
   uncertain, wait or take it to the founder. **[FOUNDER CHECKPOINT]** An
   emergency exception to a scheduling window needs an explicit decision and a
   notice describing the risk.

   Read every account known to be moving or restoring:

   ```bash
   witself-admin placement lifecycle --account-id '<account-id>' --json
   ```

   There is no fleet-wide lifecycle-operation list: **[GAP] G3**. Account-level
   checks cannot prove fleet-wide idleness. If an operation is unfinished or
   its state is uncertain, wait or take the unresolved state to the founder.
   Do not begin an account move near this rollback or a rollout wave.

7. **[FOUNDER CHECKPOINT] Get the execution go.** Present CUR, PREV, symptom,
   start time and the checks above. Issue the bold notice before the deploy.
   Explain that PREV's Worker can temporarily front CUR's container, cold
   starts can follow, and features unique to CUR become unavailable.
   A forward deploy has taken about 9 minutes to its verdict; no rollback has been timed.

8. **Deploy from PREV.** Use a session that will survive until completion;
   interruption can leave a Wrangler mutation in progress. Lease expiry is
   not proof that the deployment settled. Use a protected directory outside
   the checkout for private files and logs. The control-plane edge token must
   be available from founder-held escrow: **[GAP] G5**. Disable shell tracing,
   never type or print credential values, and protect the token file with mode
   0600. Supply the reviewed Cloudflare credentials through the private
   operator environment. The following names are placeholders, not locations
   of any actual credential.

   ```bash
   : "${PREV:?}" "${WT:?}"
   cd "$WT/infra/cloudflare/control-plane"
   git -C "$WT" tag --points-at HEAD --list "v$PREV" | grep -qx "v$PREV"
   set +x
   umask 077
   : "${EMAIL_DIRECTORY_KV_ID:?}" "${AGENT_EMAIL_ROUTE_SIGNING_KEY_ID:?}" \
     "${CLOUDFLARE_ACCOUNT_ID:?}" "${CLOUDFLARE_API_TOKEN:?}"
   test -n "${CP_AGENT_EMAIL_MANAGED_DELIVERY_ACCOUNT_ALLOWLIST+set}"
   export EMAIL_DIRECTORY_KV_ID AGENT_EMAIL_ROUTE_SIGNING_KEY_ID CP_AGENT_EMAIL_MANAGED_DELIVERY_ACCOUNT_ALLOWLIST
   export CLOUDFLARE_ACCOUNT_ID CLOUDFLARE_API_TOKEN
   EDGE_TOKEN_FILE=/path/to/protected/edge-token-file
   CONTROL_PLANE_EDGE_TOKEN="$(cat "$EDGE_TOKEN_FILE")"
   test -n "$CONTROL_PLANE_EDGE_TOKEN"
   export CONTROL_PLANE_EDGE_TOKEN
   # Set each attestation only if it matches the reviewed live state.
   export CP_DEPLOY_CANONICAL_EMAIL_ACTIVE=true CP_DEPLOY_SIGNUP_OPEN_ACTIVE=true
   docker info >/dev/null 2>&1
   LOG="/path/to/protected/cp-rollback-$PREV.log"
   # The deploy's release snapshot compares extracted file modes with Git, so
   # it must run under umask 022 (as the forward wrapper does); 077 stays for
   # the token and the log only.
   (umask 022 && npm run deploy) 2>&1 | sed -E 's/[0-9a-fA-F]{64}/<hex64>/g' | tee "$LOG"
   deploy_status=("${PIPESTATUS[@]}")
   printf 'npm run deploy exit=%s; redactor exit=%s; log exit=%s\n' "${deploy_status[@]}" | tee -a "$LOG"
   unset CONTROL_PLANE_EDGE_TOKEN
   ```

   Run the preconditions one at a time and stop on failure; do not paste through
   a failed `test`, credential read or Docker check. The example hex filter
   is not a general credential scrubber: keep logs private and review/redact
   them before sharing. Set attestations only when independently confirmed;
   a mismatched attestation is not a workaround for a refusal.

   `npm run deploy` requires clean tagged source, deploys with `--strict`, sets
   release annotations and verifies under the lease. If lease acquisition,
   edge attestation, dark-secret, cohort or snapshot checks refuse, stop.
   If Wrangler mutated but verification failed, also stop.
   **[FOUNDER CHECKPOINT]** Do not retry blindly or bypass the refusal.

9. **Require the complete verdict.** The private log must show
   `verified outer Worker <version-id> (<etag>)`,
   `verified witself-control-plane <PREV> (<commit>)`, and zero exit status for
   npm, the redactor and log writer. Both identities must match PREV. Neither
   an exit code alone nor only the outer Worker line establishes recovery.

   ```bash
   grep -nE 'verified outer Worker|verified witself-control-plane|npm run deploy exit=' "$LOG"
   curl -fsS -m 10 https://self.witwave.ai/v1/version | jq -c '{version,commit}'
   ```

10. **Retain the evidence, then remove the temporary worktree.**

    ```bash
    git -C "$REPO" worktree remove "$WT"
    ```

    If Git refuses cleanup, inspect what remains before removing anything.
    Preserve the private deployment log outside the worktree.

## Verify

1. Read `/v1/version` again and require PREV's container version and commit.
2. Check the directory hot path and an end-to-end account read:

   ```bash
   curl -fsS -m 10 'https://self.witwave.ai/v1/directory/<account-id>' | jq -c .
   witself account status
   ```

   The [Worker directory route](../infra/cloudflare/control-plane/src/index.js)
   reads KV without reaching the container or requiring a token. The CLI then
   uses the directory result to reach its cell. Neither check substitutes for
   container verification.
3. Read fleet health and registry state:

   ```bash
   witself-infra health
   witself-admin cells list --json
   ```

   The [infra health targets](../infra/pulumi/cmd/witself-infra/health_probe.go)
   include the control plane, but that check lists cells through the outer
   Worker; the admin cell list also terminates there. These are fleet/Worker
   checks, not container probes. Use the intended infrastructure config as
   described in the [infra README](../infra/pulumi/README.md), and keep the
   separate `/v1/version` container check.
4. Repeat version and health reads through the observation period agreed in
   the notice, and check scheduler progress:

   ```bash
   curl -fsS -m 10 https://self.witwave.ai/v1/version | jq -c '{version,commit}'
   curl -fsS -m 10 https://self.witwave.ai/metrics/probes | grep '^witself_probe_scheduler_last_run_timestamp_seconds'
   witself-infra health
   ```

   The [probe code](../infra/cloudflare/control-plane/src/uptime-probes.mjs)
   supports an optional control-plane target. `/metrics/probes` terminates in
   the outer Worker. An advancing scheduler timestamp shows scheduled work;
   it does not by itself prove that the container is healthy: **[GAP] G7**.
   Check the reviewed probe configuration and the target results as well as
   direct version/health reads.
5. **[FOUNDER CHECKPOINT]** An optional restore drill is a separate rehearsal
   requiring explicit go and its own bold notice. Only use it when PREV
   supports the alarm-driven drill. It writes to the drill cell; follow
   [Operate periodic account backups](runbooks.md#operate-periodic-account-backups).
   The public [release-drill-proof.sh](../scripts/release-drill-proof.sh)
   accepts an account ID and fleet token file, defaults to a plan without
   `--execute`, and proves validation plus an empty drill database when run.
   Read its usage and prerequisites; run the approved drill in a durable session.
6. Re-read `witself-admin placement lifecycle` for every account inspected
   before the rollback.

## Procedure B: Wrangler version rollback (UNTESTED, founder only)

**[FOUNDER CHECKPOINT] The founder performs this untested rollback himself,
after a bold notice naming this runbook and its unresolved risks.**

Use this only when Procedure A cannot run, for example because CUR broke the
outer Worker or lease routes. There is no fenced control-plane rollback tool:
**[GAP] G1**. The email edge's [rollback tool](../infra/cloudflare/agent-email/scripts/rollback.mjs)
is evidence for the Wrangler argument shape; it is not a control-plane tool
and does not prove this procedure.

Procedure A's retained PREV evidence, compatibility review and in-flight-work
checks still apply. If a required check cannot be completed, stop; inability to
run the deploy wrapper is not permission to skip its safety review.

- This bypasses deploy guards: lease, edge attestation, dark-secret check,
  cohort preflight and release snapshot. The existing
  [email operations rules](runbooks.md) freeze unleased mutations during a
  token rotation. Do not use this to bypass that freeze.
- Versions carry secret state. Never accept a secret difference, including
  restoration of an older value. The control-plane edge token must remain
  consistent across the control plane and email edge.
- A secret-only version may lack release annotations. The immediately preceding
  version is not necessarily PREV's verified release.
- Worker and container roll separately. This repository has no rehearsal
  proving whether a version rollback restores the container image.

Use a clean PREV worktree after `npm ci`, as in Procedure A step 3. Use the
reviewed account/token and a clean Wrangler inspection environment, following
[wrangler-environment.mjs](../infra/cloudflare/agent-email/scripts/wrangler-environment.mjs).
Remove the unsafe variables it names; disable file logging and telemetry.
Do not pass `--yes`; the founder must inspect any secret-state warning and
stop if a difference is proposed.

```bash
# UNTESTED for witself-control-plane; the founder runs this himself.
cd "$WT/infra/cloudflare/control-plane"
KNOWN_GOOD_VERSION='<outer Worker version id from PREV verification>'
npx wrangler deployments status --name witself-control-plane --json --env-file /dev/null
npx wrangler versions view "$KNOWN_GOOD_VERSION" --name witself-control-plane --json --env-file /dev/null
# Review the identity and secret state before continuing.
npx wrangler versions deploy "$KNOWN_GOOD_VERSION@100" --name witself-control-plane \
  --message "control-plane rollback to v$PREV" --env-file /dev/null
npx wrangler deployments status --name witself-control-plane --json --env-file /dev/null
```

Before deployment, require `versions view` to show tag `v$PREV` and message
`witself-control-plane v$PREV <commit>`, matching retained PREV evidence.
Those annotations come from the [source identity](../infra/cloudflare/control-plane/scripts/source-identity.mjs)
and deploy script. Afterwards, check the container's `/v1/version` and every
Verify step above. Do not declare recovery from the outer Worker version alone.
When lease routes work again, seek the execution go for Procedure A at the
same tag to restore a verified source deployment. Whether `deploy --strict`
accepts this version-deployed starting state remains untested; stop on refusal.

## What this does not roll back

- Durable Object storage or class migrations.
- KV directories, routing records and probe scheduler state.
- R2 archives, backups and authority journals, including the Go control plane's
  [R2 registry](../cmd/witself-control-plane/main.go).
- Worker secrets and runtime gates.
- Cells, database schemas or GitOps pins. The [release train](release-and-build.md#current-automated-release-and-rollout-boundary)
  has its own downgrade guard. Preserve PostgreSQL PVCs and StatefulSets.
- The receive, send, legal and plans Workers. The control plane reads the live
  legal consent manifest through its service binding, so a rollback does not
  restore earlier legal text.
- Customer/operator CLIs, DNS, Turnstile, billing-provider objects, monitoring
  rules or paging.

## After the rollback

1. Give the founder CUR, PREV, observed times, retained verdict lines and verify
   results. Record uncertainty about any incomplete checks.
2. **[FOUNDER CHECKPOINT]** If customers were affected, the founder performs
   the public [incident communications](runbooks.md#incident-communications).
3. Fix forward on main and ship a new release through the normal reviewed
   order. Do not redeploy the faulty CUR.
4. Retain both verifier lines and the full outcome after every deploy. They
   provide PREV evidence for the next rollback. The built-in verdict lines do
   not timestamp the Worker/container transition: **[GAP] G8**.

## Gaps

- **G1.** The control plane has no fenced rollback tool. The repository deploy
  wrapper refuses a version below the live control plane by design. Procedure B
  is untested.
- **G2.** No repository command lists earlier control-plane Worker versions
  together with verified release identities. Selecting PREV depends on retained
  deployment evidence; raw Wrangler version inspection is not that proof.
- **G3.** No fleet-wide list of in-flight lifecycle operations or restore drills
  exists. Per-account lifecycle and backup-status reads do not establish
  fleet-wide idleness.
- **G4.** No repository command holds or reverts the brew tap formula.
- **G5.** No repository command writes the edge token from founder-held escrow to
  a private file.
- **G6.** The forward wrapper `scripts/release-deploy-control-plane.sh` refuses a
  version below the live control plane, so a rollback runs `npm run deploy`
  directly from the PREV tag worktree (Procedure A). That deploy takes its lease
  from the live control plane (hazard 9): if CUR broke the lease routes,
  Procedure A cannot run and only the untested Procedure B remains.
- **G7.** The outer Worker's `/metrics/probes` and scheduler timestamp alone
  cannot confirm container recovery. The implementation supports an optional
  control-plane probe; review its configuration/results and independently verify
  the container version and health.
- **G8.** The deploy's Worker and container verdict lines have no built-in
  timestamps between them, so they alone cannot measure the mixed-version window.
