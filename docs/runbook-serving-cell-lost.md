# Serving cell lost

Status: not rehearsed. No step below has been run as a drill or timed end to end. Verified against `main` at `bc2d5a5bb990` on 2026-10-21; re-check every command against the current tree before relying on it.

[FOUNDER CHECKPOINT]: the founder decides; he runs the step himself or says go first.

[GAP] Gn: an unresolved gap numbered Gn in this file's `## Gaps` list.

Use this runbook when a serving cell stops answering and may not come back.
It covers one node lost, the PostgreSQL volume lost, the whole cluster lost,
and the whole provider region lost.

It does not cover:

- A provider notice that a region or cluster will go away on a date. While
  the cell still answers, use
  [Account moves in a chosen order](runbooks.md#account-moves-in-a-chosen-order)
  or [Decommission a cell and preserve its accounts](runbooks.md#decommission-a-cell-and-preserve-its-accounts).
  A planned move loses no data.
- A control-plane outage. Start with
  [Roll the control plane back to the previous release](runbook-control-plane-rollback.md)
  to assess whether rollback applies.

## 0. Before you start

Do these while nothing is broken. Most cannot be fixed during an incident.

**Preconditions:**

- **P1. Prove the dead-man page.** **[FOUNDER CHECKPOINT]** Prove that a dead-man
  lapse produces a high-urgency page on the current paging service, and record
  the date privately. Re-prove it end to end after any paging-service change.
  The external dead-man is the detection route that survives the monitored
  cell's loss; see
  [Alerts and dashboards](observability-and-operations.md#alerts-and-dashboards)
  and **[GAP] G11**.
- **P2. Escrow.** **[FOUNDER CHECKPOINT]** The dump decryption identity must be
  available from founder-held escrow outside the cell.
  A control-plane deploy needs the edge token file (`--edge-token-file`); the token must be available from founder-held escrow outside the cell.
  See the [deploy wrapper](../scripts/release-deploy-control-plane.sh).
  See section 4 step 7 and **[GAP] G9**.
- **P3. Record registry identities.** Record each cell's registry name,
  endpoint and `registration_id` in a protected operational record.
  The registry lives in the control plane. No supported command prints
  `registration_id` (**[GAP] G1**); obtaining it needs a separately reviewed
  authenticated API read.
- **P4. Pick the fallback for a NYC1 loss.** **[FOUNDER CHECKPOINT]**
  The founder chooses the region or provider in advance where possible;
  see section 13 and **[GAP] G15**.

**Operator workstation:**

1. Tools on PATH, at minimum: `witself-admin`, `witself-infra`, `kubectl`, `aws`,
   `age`, `sops`, `op`, `jq`, `git`, `curl`, `shasum`, `make`, and a Go toolchain
   that satisfies the root `go.mod` (`make gitops-cell-values` runs `go run`;
   `scripts/roll-cell.sh` and `scripts/resolve-server-image-digest.sh` check
   for `git`, `jq`, `curl` and `shasum`). Wherever the founder-approved restore
   transport runs them (G2), section 9 steps 4 and 7 also need `gzip` and a
   `psql` client compatible with the cell's PostgreSQL major. The SOPS helper
   also requires Bash and Ruby.
   See [Cell operator Secrets](runbooks.md#cell-operator-secrets-sops).
   The founder needs the public-issue tooling for section 15. Directory
   writes have no supported command (G4); any directory write needs a
   founder-approved, reviewed request (section 12 step 3).
2. The 1Password CLI signed in for authorized secret handling.
3. Check these inputs are present without printing their contents:
   - the fleet token file or `WITSELF_FLEET_TOKEN`;
   - the configured Civo token file;
   - the witself-infra inventory (`infra.yaml`; see
     [witself-infra](../infra/pulumi/README.md));
   - the cell's local Pulumi state directory, or the backend variables for
     an R2-backed cell;
   - the `witself-<cell>` kube context.

**Who performs what:**

| Action | Performer and checkpoint |
|---|---|
| Serving-cell `witself-infra preview`, `up` or `destroy` | Founder-performed: a cell serving customers is production regardless of its inventory alias. Destroy remains prohibited during this incident by R1 and R3. |
| List or fetch nightly dumps with the dump bucket credential (R7: list and fetch only) | Operator only after the founder's explicit go and a bold notice naming the cell and read operation. |
| Kubernetes Secret restore with `decrypt-apply` | Operator only after the founder's explicit go and a bold notice naming the cell and affected Secret names. |
| Merge the hold PR | Operator only after the founder's explicit go and a bold notice naming the cell and expected outage. |
| `POST /v1/cells`, drain, undrain, directory writes or paging changes | Operator only after the founder's explicit go. |
| Public incident posts and updates | Founder-performed. |
| Worker secret changes; legal, receive, send or plans Worker deploys or rollbacks; DNS; R2 changes; Turnstile | Founder-performed. The dump exception is limited to listing and fetching. |
| Control-plane rollback or rehearsal | Operator only after the founder's explicit go and the bold notice required by the [rollback runbook](runbook-control-plane-rollback.md). |

## 1. What happened on 2026-09-19 (PHX1)

A provider region decommission destroyed the serving cell and its in-cluster
PostgreSQL.

Lessons this runbook turns into rules:

1. Hold the server and worker before restoring. Startup applies forward
   migrations, and Argo CD self-heals applications.
2. `kubectl exec -i` can hang after a restore finishes. Verify with a query
   instead of waiting; see
   [Restore transport](runbooks.md#restore-transport-kubectl-exec--i-can-hang-after-pg_restore-exits).
3. A restore can leave control-plane authorities ahead of the cell.
   [Entitlement delivery must re-converge](runbooks.md#after-restoring-a-cell-from-backup-entitlement-re-delivery).
4. Keep the registry name after a
   [same-name re-endpoint](runbooks.md#cell-identity-after-a-same-name-re-endpoint).
5. Write Secret values byte-exact, without an added trailing newline.
6. List Secrets by name only; never print Secret data into a session or log.
7. Provider notices need an independent watcher.
8. This runbook names each checkpoint instead of assuming a standing grant.

## 2. Rules that apply in every case

**R1. Never delete the PostgreSQL PVC or StatefulSet outside a verified-backup step.**
Treat both as destructive. The repository does not establish the live storage
class's reclaim policy or prove that a StatefulSet deletion preserves its PVC.
This rule covers:

- `kubectl delete pvc` or `kubectl delete statefulset` in namespace `witself`;
- deleting the Argo CD Application `witself-postgresql`: its resource
  finalizer can cascade deletion;
- a GitOps change that stops rendering that Application, including
  `apps.civoPostgres.enabled: false`;
- `witself-infra destroy` of the cell.

Both the root and PostgreSQL Applications prune automatically. See the
[PostgreSQL Application](../.gitops/charts/apps/templates/civo-postgres.yaml)
and [Argo CD bootstrap](../infra/pulumi/internal/cell/civo_argocd.go).
A verified-backup step requires a dump of this exact database, a matching
ciphertext checksum, and a verified restore into a disposable target.
See [Scheduled PostgreSQL backups](backup-and-recovery.md#scheduled-postgresql-backups).
Even then, **[FOUNDER CHECKPOINT]** before any deletion. Preserve the
repository's separate [deletion-protection procedure](runbooks.md#deletion-protection-and-break-glass).

**R2. Hold the server and worker before PostgreSQL comes back empty.**
Startup applies forward migrations. Against an empty database it creates the
schema, and a restore then collides with it. Use a GitOps hold because Argo CD
self-heals; section 9 step 1 describes it. **[GAP] G12**.

**R3. Do not destroy the dead stack.** Its volume may still exist at the
provider. Deletion can remove the last recovery source. Retiring local state
is a separate, reversible decision in section 11, not an instruction to
destroy resources.

**R4. Freeze everything else.** No release train, control-plane deploy,
account move, `POST /v1/backups:run` or restore drill until the cell is back
and verified, except the explicit recovery steps below. Do not move accounts
near a rollout wave. Stop any automation that merges, releases or rolls cells
(for example `scripts/release-chain.sh` or `scripts/roll-train.sh`).

**R5. Value-free language everywhere.** Incident issues, tickets and logs
never carry account data, tokens or identifiers beyond cell names. Keep any
necessary recovery artifact references in a protected operational record,
outside public issues and session output. See
[Incident communications](runbooks.md#incident-communications).

**R6. Gate-rule deviations need the founder.** The
[repository gate policy](../CLAUDE.md#gates) starts with `make check-local`,
which selects the required local checks. A recovery overlay change requires
the full application path. **[FOUNDER CHECKPOINT]** for any proposed deviation;
time pressure does not grant one.

**R7. Handle secrets without printing them.**

- Never print or describe Secret data or list output columns containing it.
  Use `scripts/cell-secrets.sh decrypt-apply <cell> --diff-names` for the
  names-only comparison.
- Load authorized credentials from founder-held escrow into an owner-only
  temporary file in a protected directory outside the checkout, or into the
  environment of the one process that needs them. Remove the file afterward.
  The SOPS helper handles its own ephemeral identity file.
- Never pass a secret value as a command argument.
- Write values byte-exact (`jq -j` or `printf '%s'`), without an added newline.
- Never decrypt a SOPS artifact to the terminal.
- Use the dump bucket credential only to list and fetch.

## 3. Detection

At the verified tree, the
[cell catalog](../.gitops/cells/catalog.yaml) enables monitoring on
`civo-sandbox-use1-serving`; the backup cell is unmonitored and the
`civo-prod-use1-serving` monitoring rollout is disabled. The control plane
probes registered cells' `/v1/version` from Cloudflare, but the Prometheus
scraping those results runs on the monitored serving cell.

When that cell is lost, its own alerts disappear. The external dead-man route
must detect the missing check-ins. Re-prove the route after changes (P1);
do not infer paging delivery from a healthy Watchdog alone.

| Failure | Signal and severity | Source |
|---|---|---|
| Monitored cluster or region lost | `WitselfWatchdog` has severity `none`; its absence is detected externally after the configured missed-check-in window. The documented recommendation is about 15 minutes. No local Prometheus alert survives the loss. | [Founder rules](../.gitops/charts/platform/files/founder-open-plane.rules.yaml), [monitoring setup](runbooks.md#founder-open-plane-monitoring) |
| Node lost, Prometheus survives, PostgreSQL pod cannot start | `WitselfPostgreSQLExporterUnavailable`, critical after 10m, includes `absent(pg_up)`. `WitselfServerMetricsUnavailable`, critical after 3m, and `WitselfProbeTargetDown`, critical after 10m, may also fire. If the cell was accepting placements, `WitselfSignupProvisioningFailureRatioHigh`, warning (more than 20% and at least 3 failed provisioning attempts over 30m, for 5m), may fire if signups are attempted. | [PostgreSQL rules](../.gitops/charts/platform/files/postgresql.rules.yaml), [founder rules](../.gitops/charts/platform/files/founder-open-plane.rules.yaml), [probe rules](../.gitops/charts/platform/files/uptime-probes.rules.yaml) |
| Exporter up, database refusing connections | `WitselfPostgreSQLDown`, critical after 5m. It requires a live `pg_up` series. If the serving Prometheus survives and the cell was accepting placements, `WitselfSignupProvisioningFailureRatioHigh`, warning (more than 20% and at least 3 failed provisioning attempts over 30m, for 5m), may fire if signups are attempted. | [PostgreSQL rules](../.gitops/charts/platform/files/postgresql.rules.yaml), [probe rules](../.gitops/charts/platform/files/uptime-probes.rules.yaml) |
| Node lost, Prometheus on the lost node | External dead-man until Prometheus recovers and resumes check-ins. | [Monitoring setup](runbooks.md#founder-open-plane-monitoring) |
| PostgreSQL volume lost, cluster alive | `WitselfPostgreSQLExporterUnavailable` and `WitselfPostgresPVCMetricsUnavailable`, both critical after 10m; `WitselfProbeTargetDown`, critical after 10m. If the cell was accepting placements, `WitselfSignupProvisioningFailureRatioHigh`, warning (more than 20% and at least 3 failed provisioning attempts over 30m, for 5m), may fire if signups are attempted. | [PostgreSQL rules](../.gitops/charts/platform/files/postgresql.rules.yaml), [founder rules](../.gitops/charts/platform/files/founder-open-plane.rules.yaml), [probe rules](../.gitops/charts/platform/files/uptime-probes.rules.yaml) |
| Non-monitored cell lost, serving Prometheus survives | `WitselfProbeTargetDown`, critical after 10m, with the registry name as target. If that cell was accepting placements, `WitselfSignupProvisioningFailureRatioHigh`, warning (more than 20% and at least 3 failed provisioning attempts over 30m, for 5m), may fire if signups are attempted. | [Probe rules](../.gitops/charts/platform/files/uptime-probes.rules.yaml) |
| Backup stops succeeding, Prometheus survives | `WitselfPostgresBackupStale`, critical after 5m with no success for over 26 hours or a missing/nonpositive metric. `WitselfPostgresBackupFailed`, critical without a `for` delay on terminal failure or an unfinished attempt older than 7,200 seconds. | [Backup rules](../.gitops/charts/platform/files/postgres-backup.rules.yaml) |

The PostgreSQL exporter is a sidecar in the PostgreSQL pod, so it cannot
report when that pod cannot start. All these in-cluster rules depend on their
cell's monitoring rollout. Collector rules additionally require the catalog's
`collector_alerts` switch; their presence in the chart does not enable them
on another cell. `WitselfSignupProvisioningFailureRatioHigh` renders only
where uptime probes are enabled and needs signup traffic, so its absence
proves nothing. Other signup-funnel alerts are described in
[Signup funnel alerts](runbooks.md#signup-funnel-alerts), and the support
alert in [Support ticket alerts](runbooks.md#support-ticket-alerts).

Users may see CLI or MCP connection failures before an alert. Once the cell
is drained, new placements fail if no other cell accepts (section 4).

Read-only checks; retain only value-free conclusions in the incident log:

```bash
# Control plane
curl -sS -m 20 https://self.witwave.ai/v1/version

# Cell version route
curl -sS -m 20 "https://<cell api host>/v1/version"

# Inventory cells through the control plane; nonzero if any is not healthy
witself-infra health --json

# Kubernetes health through the stack's exported kubeconfig
witself-infra cell-health -cell <cell>

# Registry entry; credential presence only
witself-admin cells show <registry-name>

# Kubernetes, if its API answers
kubectl --context witself-<cell> get nodes
kubectl --context witself-<cell> -n witself get pods,pvc
```

See [health probes](../infra/pulumi/README.md#health-probes-and-alerting)
and [registry CLI](../cmd/witself-admin/cells_cmd.go).
The serving cell's registry name remains `civo-sandbox-usw2-dev`, while
GitOps and the inventory use `civo-sandbox-use1-serving`; see
[cell identity](runbooks.md#cell-identity-after-a-same-name-re-endpoint).
`witself-infra cell-health -cell <cell>` reports apiserver readiness, node
Ready/total and Argo Application health from the stack's exported kubeconfig,
without an operator kube context. No repository command checks provider
(Civo API) cluster, node or volume state. **[GAP] G11**.

## 4. First 30 minutes

1. Confirm the control plane answers. If it does not, stop this procedure and
   assess [control-plane recovery](runbook-control-plane-rollback.md).
2. Record value-free evidence: which alerts fired and when, registry health,
   and Kubernetes availability. Keep endpoint and resource details private.
   Record the UTC start time of the loss in the protected incident record;
   it bounds dump selection in section 9 step 2.
3. Apply R4. Stop any automation that merges, releases or rolls cells
   (for example `scripts/release-chain.sh` or `scripts/roll-train.sh`).
4. **[FOUNDER CHECKPOINT]** **Drain the lost cell.** The operator acts only after
   the founder's explicit go. Drain stops new placements and leaves existing
   account routes alone:

   ```bash
   witself-admin cells drain <registry-name>
   ```

   **While the cell must stay drained, no `witself-infra up` may register it as accepting.** Every `up` that has a control plane, including one taken from the inventory's `control_plane` default, registers the cell again. That registration sets `accepting=true` unless `register_draining` is set, and a re-registration with the same endpoint and provisioning token counts as the same instance, so it reopens a drained cell. This also covers an `up` run by [Change a Civo cell's node count](runbooks.md#change-a-civo-cells-node-count). **[FOUNDER CHECKPOINT]** Set `register_draining: true` on the cell's own inventory entry (a per-cell key; defaults refuse it), or pass `-control-plane "" -register-draining=false -backup-validation-target=false` on every `up`, exactly as section 11 step 4 does. Remove `register_draining` in the same step as an approved `witself-admin cells undrain`, before the next `up`. See [Registration in a drained state](../infra/pulumi/README.md#registration-in-a-drained-state) and [Close signups](runbooks.md#close-signups).

   If no cell accepts, signup placement returns HTTP 503; see
   [Close signups](runbooks.md#close-signups), including the registry-cache
   wait and invite-specific refusal variants. Setting `CP_SIGNUP_OPEN=false`
   needs a reviewed change, a release and a control-plane deploy.
   A control-plane deploy needs the edge token file (`--edge-token-file`);
   the token must be available from founder-held escrow outside the cell.
   `WitselfSignupProvisioningFailureRatioHigh` can keep firing after the drain
   while no cell accepts. The response is to close signups
   ([Close signups](runbooks.md#close-signups)), never to undrain the lost cell.
5. **[FOUNDER CHECKPOINT]** **Incident communication.** The founder classifies
   the incident and performs public posts (section 15). A security incident
   takes the private path first.
6. Walk the decision tree and select a branch.
7. **[FOUNDER CHECKPOINT]** **Recovery credentials.** Confirm the dump decryption
   identity and the control-plane edge token are available from founder-held
   escrow outside the cell. Without the dump identity, that encrypted dump
   is unusable; the founder must select another recovery artifact for which
   the matching identity is available.
   A control-plane deploy needs the edge token file (`--edge-token-file`); the token must be available from founder-held escrow outside the cell.
   Never assume a cell restore will remove the need for control-plane repair.

## 5. Decision tree

```text
Control plane healthy? ── no ──> assess control-plane recovery
        │ yes
Cell answering and a provider notice exists? ── yes ──> planned account move
        │ no
Kubernetes API answers?
        ├── yes ── all nodes Ready? ── no ──> A. NODE LOSS
        │            │ yes
        │          PostgreSQL pod Running, PVC Bound, data present?
        │            ├── no ──> B. PVC LOSS
        │            └── yes ──> diagnose the server
        └── no ── provider region still serves new clusters?
                     ├── yes ──> C. CLUSTER LOSS (same region)
                     └── no  ──> D. REGION LOSS

Old cluster answers after rebuild starts? ──> section 14, split brain
```

Use [Roll the control plane back to the previous release](runbook-control-plane-rollback.md)
to assess a control-plane rollback,
[Account moves](runbooks.md#account-moves-in-a-chosen-order) for a planned move,
and [Observability and operations](observability-and-operations.md) for a server fault.
A non-answering cluster may return. The founder sets the time box before
declaring it lost. **[FOUNDER CHECKPOINT]**

| Branch | Data at risk | Recovery source | Re-registration | Customer impact |
|---|---|---|---|---|
| A. Node loss | None if the volume reattaches intact | Existing volume | None | Outage while service and storage recover |
| B. PVC loss | Writes after the recovery point | Nightly dump | None if endpoint and registration survive | Outage and data loss |
| C. Cluster loss | Writes after the recovery point | Nightly dump | Same registry name; new credentials, and a new endpoint unless the host is preserved (load-balancer cells keep `api.<cell>.<domain>`) (G1) | Outage and data loss |
| D. Region loss | Writes after the recovery point across affected cells | Nightly dump | As C, in the chosen region/provider | Multi-cell outage, data loss and location review (G15) |

## 6. Recovery sources and their RPO

| Source | Storage and encryption | Recovery point | Can it restore a lost cell? |
|---|---|---|---|
| Nightly PostgreSQL dump | The operator-selected bucket. Key shape `<prefix>/<cell>/<UTC-timestamp>-<pod-uid>.sql.gz.age`; the cell is the inventory name. `pg_dump --no-owner --no-privileges`, gzip, then age encryption. | Scheduled at 03:00 UTC daily; a successful daily schedule can still lose nearly 24 hours of writes. No PITR. | Primary source for a cell whose dump is enabled and verified. The transport and restore automation remain G2. |
| Per-account snapshots | R2 logical archives, with provider-managed encryption at rest and sealed content remaining ciphertext. | One generation per 1,440-minute interval from 00:00 UTC; a five-minute cron scans in directory order and resumes an unfinished pass across intervals. This is not a promise that all snapshots complete at midnight. | No committed disaster restore endpoint; rollback-only drills prove restorability (G3). |
| Pre-migration dumps | Age-encrypted artifacts and matching identities in a protected directory outside the checkout. | Taken before schema-advancing trains; use the artifact's actual timestamp. | A separately reviewed manual recovery can use a restore-verified dump. Its custom format needs `pg_restore`, not the nightly plain-SQL pipeline. |
| Evacuation archives | R2 logical archives; checksums prove integrity, not authentication. | Created by an evacuation while the source is available. | Not a replacement for whole-cell disaster backup; evacuation needs a live source. |

Sources:
[Scheduled PostgreSQL backups](backup-and-recovery.md#scheduled-postgresql-backups),
[Periodic logical account snapshots](backup-and-recovery.md#periodic-logical-account-snapshots),
[Civo pre-migration backup](backup-and-recovery.md#civo-postgresql-pre-migration-backup),
and [Decommission a cell](runbooks.md#decommission-a-cell-and-preserve-its-accounts).

- At the verified tree, `civo-prod-use1-serving` has `postgres_backup: false`
  and `monitoring: false` in the [catalog](../.gitops/cells/catalog.yaml).
  Before admitting accounts there, enable backups and verify the first dump
  by a disposable restore.
- Dumps and account snapshots share a Cloudflare account. They do not provide
  recovery from loss of that account. The nightly dumps have no copy outside
  the Cloudflare account, and the repository configures no expiry for them.
  **[GAP] G13**. List available objects; do not assume any older dump exists.
- Backup telemetry in `witself_ops.backup_runs`, in the cluster's `postgres`
  database, is lost with that cluster. Inspect the operator-selected bucket
  instead.

## 7. Branch A: node loss

`civo_node_count` in the inventory sets the node count; see
[Change a Civo cell's node count](runbooks.md#change-a-civo-cells-node-count).
That procedure still requires its stated approval and provider-preview proof;
the setting's existence is not approval to use it during recovery.
The PostgreSQL [Application](../.gitops/charts/apps/templates/civo-postgres.yaml)
uses standalone storage, and the serving
[overlay](../internal/gitopsvalues/overlays/civo-sandbox-use1-serving.yaml.tmpl)
configures two server replicas.

1. Apply R1. Do not delete the PVC, StatefulSet or Argo Application to
   unstick PostgreSQL.
2. Read cluster state using section 3's Kubernetes checks.
3. Inspect surviving server replicas. The pod disruption budget governs
   voluntary eviction; do not treat it as node-failure recovery. The overlay
   spreads replicas by hostname with `whenUnsatisfiable: DoNotSchedule`;
   whether a replacement schedules depends on the available topology.
4. If the lost node held PostgreSQL, recovery needs pod rescheduling and
   volume reattachment. No repository procedure establishes the Civo
   recovery behavior or timing. **[GAP] G5**. **[FOUNDER CHECKPOINT]** before
   forced pod deletion, Node removal or any volume operation.
5. Verify the endpoint independently of database recovery. A running database
   alone does not prove customer access.
6. The inventory node-count setting does not provide a node-replacement
   command. **[GAP] G5**. The founder reviews any infrastructure change.
   Keep any `up` from reopening the drained cell (section 4 step 4).
7. If the volume is gone, switch to branch B.
8. **[FOUNDER CHECKPOINT]** **Undrain** only if the cell was accepting before and
   verification passes. Coordinate removal of `register_draining` as in
   section 4 step 4. The operator needs the founder's explicit go:

   ```bash
   witself-admin cells undrain <registry-name>
   ```

## 8. Branch B: PVC loss (cluster alive)

If the cluster, Secrets, endpoint and registration survive and only the
database is lost, no re-registration is needed.

1. **[FOUNDER CHECKPOINT]** **Merge the hold PR first** (section 9 step 1), after
   the founder's explicit go and a bold notice. This scales the server to
   zero and removes the worker. If startup already created a schema, the
   restore requires a new empty target. Database recreation is destructive
   and has no repository command. **[GAP] G2**. **[FOUNDER CHECKPOINT]**
2. Establish a fresh empty PostgreSQL target. PVC replacement behavior has
   not been rehearsed here. If a PVC object exists but its volume is gone,
   deletion remains a **[FOUNDER CHECKPOINT]** under R1; this runbook supplies
   no deletion command.
3. Run section 9 steps 2 and 4–7. Step 3 is unnecessary if Secrets survived.
   Backup roles still need verification or recreation because cluster-level
   roles are not in a database dump.
4. Release the hold (section 9 step 8).
5. Complete section 10.
6. **[FOUNDER CHECKPOINT]** **Undrain** only after the founder's explicit go, if
   the cell should accept new accounts again. Coordinate removal of
   `register_draining` as in section 4 step 4.

## 9. Common restore procedure (branches B, C and D)

**Step 1.** **[FOUNDER CHECKPOINT]** **Hold the server and worker.**

Edit `internal/gitopsvalues/overlays/<cell>.yaml.tmpl`: set the server's
`replicaCount: 0` and the worker's `enabled: false`, then regenerate:

```bash
make gitops-cell-values
```

For a fresh cluster, stage monitoring back to phase 1 before bootstrap.
Monitoring CRDs must exist before ServiceMonitors can sync; follow
[Founder open-plane monitoring](runbooks.md#founder-open-plane-monitoring).
The checked-in [recovery fixture test](../scripts/testdata/test-monitoring-recovery.rb)
exercises the phased configuration, not a live restore.

Open a PR with the source configuration and generated values. For C and D
it must merge before `witself-infra up`, because Argo CD bootstraps from
`main`. The operator merges only after the founder's explicit go and a
**bold notice naming the held cell and the service interruption**. Apply R6.
For a same-name rebuild, the hold targets any surviving old cluster too.
A partitioned cluster may not receive it: independently verify convergence
or fence that cluster before releasing the replacement (section 14).

After the hold has synced, verify its actual effect before restoring. The
bootstrap convergence wait requires `bootstrap`, `apps` and `platform` at
the expected target revision and all returned Applications Synced/Healthy;
it does not check Deployment replicas or prove workloads are held.
Inspect Argo sync status/revisions and Kubernetes workload state privately:

```bash
kubectl --context witself-<cell> -n argocd get applications
kubectl --context witself-<cell> -n witself get deployments
kubectl --context witself-<cell> -n witself get pods
```

Confirm the root/apps revision includes the merged hold, the server
Application has synced its zero-replica configuration, and the server
Deployment controller has observed its current generation with desired and
actual replicas zero. Confirm autoscaling is disabled and no HPA can raise
the replica count. No server pods may remain, including terminating pods.
The worker Deployment and pods must be absent. Query only the
needed status fields if more detail is required; never inspect Secret data.
For an absent server, verify absence at both the controller and pod levels.
On C/D, do this once the kube context is available in section 11 step 5.
Do not use HTTPS success to verify the hold: `/v1/version` needs a running
server and is checked only after release.

**Step 1b.** **[FOUNDER CHECKPOINT]** **Pin images to the lost cell's release.**

Check this for C; it is required for D. Select and inspect the candidate dump
under step 2 before finalizing any pin attestation; its actual schema is an
input to this decision, even though the hold must precede provisioning.

- The [generator](../internal/gitopsvalues/pins.go) preserves an existing
  values file's server pins. A new file starts from chart defaults, which
  must not be assumed suitable for the restored database.
- Determine the lost cell's release from its retained generated values and
  release evidence. Inspect `chartVersion`, `imageTag`, the server digest,
  backup image and PostgreSQL image privately; record only equality results
  in session output.
- For a never-provisioned cell, use `scripts/roll-cell.sh --first-sync` as
  described in [Back up both reviewed Civo databases before a migration](runbooks.md#back-up-both-reviewed-civo-databases-before-a-migration).
  It requires the catalog's `unprovisioned: true` marker, no kube context and
  no resolvable API host. It cannot be combined with `--no-schema-change`.
  Remove the marker in its own change after the first-sync pins merge and
  before the first infrastructure preview.
- For an existing cell, the reviewed pin command shape is:

  ```bash
  scripts/roll-cell.sh <cell> <lost-cell-release> --no-schema-change
  ```

  The [roll script](../scripts/roll-cell.sh) requires a values file and local
  release tag, and performs release-artifact verification. The founder must
  approve the attestation that this release cannot advance the database
  schema, using the actual dump schema and target build. A held server alone
  does not prove that. If startup would migrate forward, do not use that
  attestation; return to the founder for a restore-verified backup gate.
- Preserve the PostgreSQL selection and backup image pin. Do not use
  `--postgres-image` with `--no-schema-change`; it requires backup evidence
  (or the separately guarded first-sync path). Review every pin difference
  before merging; stop on an unexplained difference. A newly generated cell
  does not inherit the lost cell's backup image pin. Where the selected
  release provides that image, add `--backup-image` to the appropriate
  guarded roll command and compare the resulting pin with the intended
  recovery configuration.
- Pinning the release also avoids an unintended evacuation-protocol change.
  The cell's version response advertises its protocol; do not guess it from
  a stale release number.

**Step 2.** **[FOUNDER CHECKPOINT]** **Choose the recovery point.**

Default to the newest complete nightly dump of this cell whose key timestamp
(UTC, taken when that backup run started) is earlier than the start of the
loss recorded in the protected incident record. Skip keys ending in
`.incomplete`. The key's pod UID tells objects apart but does not show which
cluster wrote them: a rebuilt cluster with restored backup roles, a cluster
whose roles survived a partial loss, and a returning old cluster all upload
under the same `<cell>/` prefix. Do not select any object newer than the loss
start, including objects written during or after this restore and on a retry.
Treat their presence as a sign that another cluster may be running, and
follow section 14 before choosing.

The founder accepts the data-loss window, from the dump's UTC timestamp to
the loss, in the protected incident record.

The operator may list and fetch only after the founder's explicit go and a
**bold notice naming the cell and the read operation**. Supply the dump bucket
credential privately to the AWS CLI from founder-held escrow under R7, and
use it only to list and fetch; the
`witself-postgresql-backup-r2` Kubernetes Secret name identifies the backup
configuration, not a password-manager item. Do not decrypt its SOPS artifact
to display credentials. Use the operator-selected bucket:

```bash
aws --endpoint-url "$R2_ENDPOINT" s3 ls "s3://$R2_BUCKET/$R2_PREFIX/<cell>/"
```

See [Scheduled PostgreSQL backups](backup-and-recovery.md#scheduled-postgresql-backups).

**Step 3.** **[FOUNDER CHECKPOINT]** **Restore operator Secrets (C and D only).**

For the same inventory name, use the
[SOPS rebuild procedure](runbooks.md#cell-operator-secrets-sops).
The private SOPS identity is the password-manager item
`witself-cell-secrets-age`. The operator needs the founder's explicit go and a
**bold notice naming the cell and Secret names** before these operations:

```bash
scripts/cell-secrets.sh decrypt-apply <cell> --diff-names
scripts/cell-secrets.sh decrypt-apply <cell> --dry-run
scripts/cell-secrets.sh decrypt-apply <cell>
```

Before re-arming alert receivers on a rebuilt cell, confirm with the founder
that their credentials are current. Match the overlay's selected Secret
names. The helper accepts selected `<namespace>/<name>` arguments; restoring
every stored artifact is not a substitute for reviewing that selection.

For a new inventory name, ciphertext is bound to the old cell through
`witself.io/cell`. The [copy helper](../scripts/copy-cell-secrets.sh) reads
the source cluster, so it cannot recover from a lost source. **[GAP] G8**.
The founder must approve a new Secret preparation path from escrow.

**Step 4.** **[FOUNDER CHECKPOINT]** **Restore into the empty database.**

The [documented pipeline](backup-and-recovery.md#scheduled-postgresql-backups)
first restores into a new, empty, disposable database. Using the held cell's
database as that disposable target is a departure requiring the founder's
explicit approval. Nothing may serve from it until verification passes.
If verification fails, stop; recreation is destructive and remains G2.

Preflight checklist:

- server at zero replicas or absent;
- worker absent;
- target has zero public tables;
- cell drained and the hold preventing customer requests from reaching the
  restore target; for C and D, the replacement endpoint not yet registered,
  or, when the registry endpoint is unchanged, the hold verified as in step 1;
- compatible PostgreSQL major and pgvector available;
- selected dump and matching decryption identity available.

The nightly dump is compressed plain SQL. The documented pipeline is:

```bash
set -o pipefail
aws --endpoint-url "$R2_ENDPOINT" s3 cp "$ENCRYPTED_BACKUP_S3_URI" - \
  | age --decrypt -i "$AGE_IDENTITY_FILE" \
  | gzip --decompress \
  | psql --no-psqlrc --set=ON_ERROR_STOP=1 --single-transaction "$RESTORE_DATABASE_URL"
```

The fetch needs the explicit go and bold read notice from step 2. Keep the
identity in an owner-only file and remove it afterward. Keep authentication
out of the command arguments: `RESTORE_DATABASE_URL` must not embed a password;
supply authentication privately through the client environment or protected
credential file. Connect as the application user `witself` to database
`witself`, so restored objects have the intended owner; the dump carries no
owners or grants.

The serving configuration disallows external PostgreSQL access. The
repository supplies no checked-in transport and preflight-enforcing restore
Job for this pipeline into a cell. **[GAP] G2**. The founder must approve that
transport before execution. Do not substitute a custom-format `pg_restore`
pipeline for the nightly SQL dump. See the
[exec transport caveat](runbooks.md#restore-transport-kubectl-exec--i-can-hang-after-pg_restore-exits).
Avoid overlapping the 03:00 UTC backup Job while its roles are unavailable.

**Step 5. Verify the restore before anything boots.**

Use the six-column query from the
[pre-migration backup script](../scripts/civo-pre-migration-backup.sh).
Record its result privately; publish only pass/fail conclusions.

```sql
SELECT
  (SELECT COALESCE(MAX(version_id) FILTER (WHERE is_applied), 0) FROM public.goose_db_version),
  (SELECT count(*) FROM pg_catalog.pg_tables WHERE schemaname = 'public'),
  (SELECT count(*) FROM public.accounts),
  (SELECT count(*) FROM pg_catalog.pg_index WHERE NOT indisvalid),
  (SELECT count(*) FROM pg_catalog.pg_constraint WHERE NOT convalidated),
  (SELECT count(*) FROM pg_catalog.pg_extension WHERE extname = 'vector');
```

| Column | Expected |
|---|---|
| Applied Goose version | The dump's recorded schema, compatible with the pinned release. Determine migrations from that release's `internal/store/migrations/`, not today's main branch. |
| Public tables | Compare with verified source evidence or a healthy cell at the same schema; otherwise mark unverified. |
| Accounts | Reconcile with the control plane's count for this registry entry; a count comparison alone cannot identify missing or resurrected accounts (section 10 step 6). |
| Invalid indexes | Zero. |
| Unvalidated constraints | Zero. |
| Installed vector extension | Match the source's installed-extension state; availability of pgvector in the image does not mean it was installed. |

Nightly dumps have no restore-verified manifest contract. A successful upload
does not prove a good dump. Establish the dump's applied Goose version from
its source evidence or its Goose history inspected in a protected environment;
do not infer it from the selected release. The restored version must equal
that dump version. Compare it separately with the pinned build's newest
migration: a restored schema above the build's version is a stop; an equal
version permits the no-schema-change attestation; a lower version requires
explicit approval of forward migration and its backup gate before startup.
If the dump version or compatibility cannot be established, remain held and
escalate G2.

Before releasing the hold, check the whole-database invariants from
[Restore Checklist](backup-and-recovery.md#restore-checklist) step 8:

- fact-name uniqueness per owner and at-most-one primary per logical kind
  per owner;
- group membership/admins and default-deny policy bindings; curation
  request/run/action/receipt ownership and attribution;
- memory edit-history continuity;
- message delivery/read/ack state, completed processing/result links and
  preserved deterministic failure counts;
- for schema 87, original agent-email domain routes, globally reserved
  additive `(domain, local_part)` pairs and no synthesized legacy route for
  a post-cutover mailbox;
- for schema 89 or newer, realm/agent send controls, outbound messages,
  provider-event receipts, recipient suppressions, reply provenance and
  terminal versus unresolved state;
- for schema 91, exactly one `agent_email_cell_storage_capacity` row with
  the configured thresholds, reconciling `root_rows`/`counted_rows` and the
  logical byte charge against the five counted mail tables.

Record the import-only clauses as not applicable: inactive imported leases
with reserved fences, interrupted claims with advanced generations, schema-90
rate debt omitted for fresh limiter state, and an account archive never
carrying or overwriting the capacity singleton. A whole-database dump restores
leases, claims, cell-local rate buckets and the capacity singleton as they
were at dump time; account-import transformations do not run.
Do not edit or truncate restored rows to make them pass; restored leases
expire and buckets drain under normal operation.
Checklist steps 9–11 require a running server and follow afterward.
List the pinned release's migration files recursively:

```bash
git ls-tree -r --name-only "v<release>" -- internal/store/migrations/
```

**Step 6. Record the result.**

Keep the selected dump key, UTC timestamp, verification row, and start/end
times in a protected operational record outside the checkout. Keep public
logs and session summaries value-free.

**Step 7.** **[FOUNDER CHECKPOINT]** **Re-provision backup roles.**

Roles are cluster-level and absent from the database dump. Inspect existing
roles and telemetry schema first, especially after partial database loss:
the dump script's `CREATE ROLE` refuses an existing dump role; the metrics
script refuses an existing metrics role or telemetry schema. They are
one-time provisioning scripts, not reconciliation tools. Do not rerun them
against existing objects or drop those objects to make the scripts pass.
For retained roles, verify their privileges on the restored objects and
default privileges, then obtain a separate reviewed repair if needed (G2).
With an approved
administrator connection and a target needing initial provisioning, run:

```bash
psql --no-psqlrc --set=ON_ERROR_STOP=1 --single-transaction --dbname=witself \
  --file=.gitops/charts/apps/files/postgres-backup-dump-role.sql
psql --no-psqlrc --set=ON_ERROR_STOP=1 --single-transaction --dbname=postgres \
  --file=.gitops/charts/apps/files/postgres-backup-metrics-role.sql
```

Then set passwords interactively with `\password witself_backup_dump` and
`\password witself_backup_metrics`, following
[Scheduled PostgreSQL backups](backup-and-recovery.md#scheduled-postgresql-backups).
Use escrowed values matching the backup Secret's `dump-password` and
`metrics-password`; never print that Secret or put values in arguments.
The transport gap from step 4 also applies. **[GAP] G2**.

**Step 8.** **[FOUNDER CHECKPOINT]** **Release the hold.**

The server can apply forward migrations, which cannot be undone. The founder
approves only with:

- the step-5 verification result and step-6 record;
- the step-5 scoped Restore Checklist result;
- the target build's migration compatibility with the dump, compared in
  step 5 (for C and D, also the step-1b pin attestation);
- section 14 checked, including fencing any surviving old cluster.

Drain stops new placements; existing accounts remain.
**[FOUNDER CHECKPOINT]** Decide whether to release before the reconciliation
in section 10 step 6 or to stay held until it is done. Until it is done,
existing customers can reach data that was deleted after the recovery point:
from the hold release for B, and for a C rebuild whose registry endpoint is
unchanged (a `civo_ingress: loadbalancer` cell keeps `api.<cell>.<domain>`);
from the section 12 registry write only when the endpoint changes (a Civo
DNS-entry host, or D).

Restore the intended server and worker settings in the overlay, regenerate
with `make gitops-cell-values`, and merge the reviewed PR only after the
founder's go. Watch for clean startup/migrations and the intended replicas
Ready. Keep placement drained until the later checks pass, with
`register_draining: true` kept on the inventory entry (section 4 step 4).

## 10. After the restore (all branches that restored)

1. **Entitlement delivery.** Observe a fresh complete lifecycle scan. Require
   `witself_entitlement_delivery_last_cycle_accounts{outcome="failed"} == 0`
   plus scheduled tick logs with `failed=0` and `succeeded=true`. A stale
   metric or one successful page is insufficient. Until monitoring is back,
   inspect the control-plane tick logs. Follow
   [entitlement re-delivery](runbooks.md#after-restoring-a-cell-from-backup-entitlement-re-delivery);
   do not clear authority fences or force an unconditional apply.
2. **Backups.** After roles are ready, run a manual Job:

   ```bash
   kubectl --context witself-<cell> -n witself create job \
     --from=cronjob/witself-postgresql-backup "postgres-backup-initial-$(date -u +%s)"
   ```

   Avoid overlap with the scheduled Job. Verify the object, telemetry and a
   disposable restore using
   [Scheduled PostgreSQL backups](backup-and-recovery.md#scheduled-postgresql-backups).
   Check account snapshots through `GET /v1/backups/status`. If periodic
   account backups are enabled and the serving Prometheus survives,
   `WitselfAccountBackupStale` (critical, for 5m) is expected once any account
   on the lost cell passes twice the configured backup interval since its
   last commit. The default interval is 1,440 minutes. It clears only after
   that account's backups commit again and the health snapshot reflects the
   recovery; other stale accounts can keep the alert firing.
   **[FOUNDER CHECKPOINT]** Lift R4 for a restore drill only after the
   restored cell is verified. Use the public
   [drill proof script](../scripts/release-drill-proof.sh):

   ```bash
   scripts/release-drill-proof.sh --account-id ID --fleet-token-file PATH --execute
   ```

   `--drill-cell` defaults to `civo-sandbox-use1-backup`; it must be available
   and configured for validation. This is a rollback-only account drill,
   not proof of whole-cell disaster restoration.
3. **Restore Checklist steps 9–11.** Verify recall, health/readiness probes,
   metrics and restore audit events; see
   [Restore Checklist](backup-and-recovery.md#restore-checklist).
4. **Monitoring (C and D).** **[FOUNDER CHECKPOINT]** Complete phases 2 and 3 of
   [Founder open-plane monitoring](runbooks.md#founder-open-plane-monitoring).
   Phase 3 enables paging; the operator needs an explicit go. Prove a dead-man
   lapse and recovery plus firing/resolved alert delivery. When monitoring
   returns on a rebuilt cell that is still drained and no other cell accepts,
   `WitselfSignupProvisioningFailureRatioHigh` may fire. This is expected
   until section 12 undrains the cell or signups are closed. While R4 freezes
   drills or no validation target exists, `WitselfRestoreDrillStale`
   (warning) may fire once the last validated drill is more than 8 days old;
   it clears after the next approved drill.
5. **Agent email.** Confirm the selected agent-email Secrets are present
   using the names-only path. Check startup's value-free configuration
   counts. The email audience and dispatch key identity remain tied to the
   registry name; see
   [cell identity](runbooks.md#cell-identity-after-a-same-name-re-endpoint).
6. **Accounts that do not match the dump.** **[GAP] G10**. Accounts created after
   the recovery point may have directory rows but no cell data. Deletions
   and closures after that point may be undone by the restore. No command
   reconciles them. Compare control-plane and database evidence privately.
   From the hold release (B, or C with an unchanged registry endpoint) or the
   registry write (C with a changed endpoint, or D), existing customers can
   reach data deleted after the recovery point until this reconciliation is
   complete.
   **[FOUNDER CHECKPOINT]** for reconciliation and re-applying deletions;
   use the [data-subject procedure](runbooks.md#handle-a-data-subject-request).
7. **Clients and objects inside accounts.** Verify these separately:
   - Each operator runtime: `witself self show`, then
     `witself transcript flush --runtime <runtime>`. Captured events may
     remain held if their binding no longer matches; follow the
     [CLI reference](cli-command-surface.md) rather than rebinding blindly.
   - Agents and tokens created after the recovery point may need to be
     re-created or re-issued through authorized account recovery.
   - Restored sealed data requires the matching client vault key. Use
     [Client-key loss and recovery](backup-and-recovery.md#client-key-loss-and-recovery);
     never generate a replacement blindly.
   - Data written after the recovery point is lost. This includes memories,
     facts, tickets and email; client-side capture does not reconstruct every
     kind of write.

## 11. Branch C: cluster loss (same region)

1. Complete section 4; keep the cell drained.
2. Merge the hold and reviewed pins (section 9 steps 1 and 1b).
3. **[FOUNDER CHECKPOINT]** **Decide the stack and state treatment.**
   Neither recovery choice is rehearsed:
   - **Same inventory name, fresh local stack.** Keep the cell entry and its
     `registry_name` in the witself-infra inventory (`infra.yaml`; see
     [witself-infra](../infra/pulumi/README.md)). Retain the cell's local
     Pulumi state directory in a protected directory outside the checkout
     before any new stack work. Do not run `config remove-cell`: the
     registry and remaining-resource checks protect a still-occupied cell.
     Whether a fresh same-name stack can safely replace this lost stack
     requires a separately reviewed plan. Hidden surviving resources can
     continue incurring charges. **[GAP] G7**.
   - **Existing stack, reapplied.** State may preserve hostname and
     provisioning credentials. Matching these can satisfy the registry's
     same-instance fallback, but do not infer safety from that possibility.
     Dead resources still in state can make preview show replacements.
     Whether refresh/apply works with an unavailable Kubernetes API requires
     a founder-approved plan. **[FOUNDER CHECKPOINT]**
4. **[FOUNDER CHECKPOINT]** **Founder-performed preview and apply.**
   A serving cell is production even if named sandbox. The founder runs:

   ```bash
   witself-infra whoami -cell <cell>
   witself-infra preview -cell <cell>
   witself-infra up -cell <cell> -control-plane "" \
     -register-draining=false -backup-validation-target=false
   ```

   See [Civo cells](../infra/pulumi/README.md#civo-production-cells).
   Preview acceptance is creates only, with zero deletes and zero replaces.
   Any delete, replace or collision with a leftover resource stops this
   procedure for a separate founder decision. Resource names derive from
   the cell name; deleting a collision may remove the last data copy.
   **[GAP] G16**.

   Apply without registration to avoid changing the occupied registry entry.
   Explicit flags override the inventory. With `-control-plane ""`, both
   `register_draining` and `backup_validation_target` must be false or the
   CLI refuses before cloud work. These overrides affect registration and
   do not change the stack's validation role or edit the inventory. Preserve
   the intended role when explicitly registering later.

   Inspect any partial apply before deciding to retry. With no control plane,
   `up` still waits for Argo CD convergence but skips the public HTTPS wait.
   A held server cannot answer `/v1/version`; verify that route after section
   9 step 8, before re-registration:

   ```bash
   curl -sS -m 20 "https://<new cell api host>/v1/version"
   ```

5. Restore the `witself-<cell>` kube context without printing kubeconfig.
   No repository command builds it from stack outputs. **[GAP] G6**.
6. Restore operator Secrets (section 9 step 3).
7. Restore the dump selected under section 9 step 2 (section 9 steps 4–7).
8. Release the hold only after verification (section 9 step 8), then check
   HTTPS and the target release identity.
9. Re-register drained (section 12).
10. Complete section 10.
11. **[FOUNDER CHECKPOINT]** **Undrain or remain drained** according to section 12
    step 5.

## 12. Re-registration and placement (branches C and D)

Directory rows name the registry entry and carry `cell_registration_id`,
which corresponds to the registry's `registration_id`. A replacement
may have a new endpoint and new provisioning/backup credentials. The
[coordinator](../infra/cloudflare/control-plane/src/target-cell-coordinator.mjs)
treats a request as the same instance if it carries the existing
`registration_id`. Without that field, the endpoint must be unchanged and
any supplied provisioning token must match. Otherwise occupied registrations
refuse replacement with HTTP 409. Same-instance registration preserves the
existing identity.

**[GAP] G1**. No supported command sends or reads `registration_id`:
the [cells CLI](../cmd/witself-admin/cells_cmd.go),
[fleet client](../internal/client/fleet.go),
[admin client](../internal/client/admin.go) and
[infrastructure CLI](../infra/pulumi/cmd/witself-infra/main.go) do not expose
it. The [control-plane API](../infra/cloudflare/control-plane/src/index.js)
accepts it on `POST /v1/cells` and includes it in the authenticated registry
read.

1. **[FOUNDER CHECKPOINT]** **Registry write.** This reroutes every account on the
   cell. Until G1 is closed, no supported command sends `registration_id`;
   the control plane accepts it on `POST /v1/cells`. The founder decides how
   the request is made; the operator needs an explicit go before executing
   it. Preserve the existing registry name and registration identity,
   supply the new endpoint/credentials, and send the intended complete
   metadata. Omitting `cloud` or `region` blanks them. Register drained
   (`accepting: false`) until section 10 passes. When the endpoint changes,
   this write restores existing customers' routes even while placement stays
   drained; when it is unchanged, the routes returned at the hold release
   (section 9 step 8) and this write only updates the credentials and
   metadata. Section 9 step 8 makes reconciliation timing a founder decision.
2. Verify the registry entry privately:

   ```bash
   witself-admin cells show <registry-name>
   ```

   Check the endpoint, cloud/region fields, channel, weight, validation role,
   `accepting false` and both credential-presence booleans. This command
   cannot verify `registration_id` (G1).
3. Verify an authorized account's routing:

   ```bash
   curl -sS -m 20 "https://self.witwave.ai/v1/directory/<account-id>"
   ```

   The [directory](runbooks.md#directory-routing-after-a-same-name-re-endpoint)
   follows the live registry endpoint with a 60-second cache. Rewriting each
   stored `acct:` fallback is no longer needed for live routing but remains
   recommended by that procedure. **[GAP] G4**. **[FOUNDER CHECKPOINT]** for
   any directory write; no supported command performs it.
4. Record `registry_name: <old-registry-name>` in the inventory so existing
   health and teardown lookups address the retained entry. Inventory-selected
   `witself-infra up -cell <cell>` also registers under that name, as
   implemented by the [infrastructure CLI](../infra/pulumi/cmd/witself-infra/main.go).
   Keep automatic registration suppressed while the cell stays drained
   (section 4 step 4): the mapping does not add the missing `registration_id`
   field or authorize an occupied re-endpoint.
5. **[FOUNDER CHECKPOINT]** **Placement.** Undrain only if this cell should take
   new accounts again, after the founder's explicit go:

   ```bash
   witself-admin cells undrain <registry-name>
   ```

   Remaining drained is a valid outcome. If it remains drained, keep
   `register_draining: true` on the inventory entry (section 4 step 4).
   Coordinate its removal with an approved undrain as section 4 step 4
   requires; do not infer admission approval from restored service.
6. Before any later account move, inspect its lifecycle state:

   ```bash
   witself-admin placement lifecycle --account-id <account-id>
   ```

   See [Diagnose an interrupted restore](runbooks.md#diagnose-an-interrupted-restore-or-source-finalization).
   Resolve any disagreement between lifecycle state, directory and registry
   before moving the account.

## 13. Branch D: region loss

The [catalog](../.gitops/cells/catalog.yaml) places the serving, backup and
production Civo cells in `nyc1`. Loss of that region affects the current
serving and drill targets together.

The [GitOps identity mapping](../internal/gitopsvalues/identity.go) maps only
`nyc1` for Civo. The [infrastructure CLI](../infra/pulumi/cmd/witself-infra/main.go)
also knows `fra1`, `lon1` and `mum1`, but those are outside the US. The
[DPA](legal/data-processing-addendum.md) lists NYC1 for Civo. Other clouds'
catalog entries alone do not prove recovery readiness.

**[GAP] G15**. A different Civo region needs a reviewed code change to the
identity mapping and a catalog entry before values can be generated.

In addition to section 11:

1. **[FOUNDER CHECKPOINT]** **Choose the region or provider.** The founder
   approves capacity/quota checks and the location decision. Review any
   cross-border transfer or DPA change with the founder before provisioning.
2. Add the region mapping if necessary, then prepare the catalog entry,
   overlay, generated values, tests and first-sync pins (section 9 step 1b).
   The inventory name is composed from cloud, account alias, region code and
   role. Retiring old catalog entries is a later separate change.
3. Keep the old registry name and identity (section 12). A region change does
   not grant permission to rename occupied registry entries.
4. **[GAP] G8**. **[FOUNDER CHECKPOINT]** Prepare operator Secrets from escrow
   under the new inventory name; old ciphertext cannot simply be relabeled.
   Paging changes require an explicit go.
5. **[FOUNDER CHECKPOINT]** **Legal publication.** The founder decides whether
   the sub-processor location requires revised public documents and performs
   any legal Worker publication.
6. Rebuild a validation target before running a restore drill. Its absence
   does not waive the restore verification requirement.

## 14. If the old cluster comes back (split brain)

A returning database may contain writes absent from the dump. A same-name
rebuild shares a values file with the old cluster, so releasing the hold may
start both. Both can upload into the same configured cell key space in the
operator-selected bucket; the backup script adds a pod UID to distinguish
objects. The registry routes to one endpoint, but that does not fence writes
on the other cluster.

1. **Freeze both.** Keep the hold; do not release it, re-register or undrain.
   If any already happened, stop further recovery steps and escalate.
2. **[FOUNDER CHECKPOINT]** **Fence serving and worker activity.** The same-name
   hold targets both clusters; prove it actually converged or establish an
   independent fence on the returning cluster. For different names, hold
   the old cell's overlay too. Prove only one server set can run before any
   release.
3. Delete nothing (R1 and R3).
4. Compare the old database's writes with the recovery point privately.
   No repository command compares or reconciles these histories.
   **[GAP] G18**.
5. **[FOUNDER CHECKPOINT]** **Choose which data wins.** The founder chooses the
   restored cluster or the returning one. A switch back preserves the
   registry identity and requires an explicitly authorized registry write.
   Handle the losing side's writes through the reconciliation decision in
   section 10 step 6.
6. Verify fencing before re-registration or release. An unanswered version
   route is a reachability check, not proof that a cluster cannot return:

   ```bash
   curl -sS -m 20 "https://<unchosen cell api host>/v1/version"
   ```

   If write fencing cannot be established, keep the hold and escalate G18.

## 15. Customer communication

Use the public repository's `incident` label as described in
[Incident communications](runbooks.md#incident-communications) and the
[support policy](support-policy.md#incident-communications). There is no
separate status page.

In-product tickets depend on the cell's PostgreSQL. Email intake is parked;
support@witwave.ai does not feed the ticket queue. During a cell outage the
public incident issue carries value-free updates. **[GAP] G17**: no
out-of-band account-specific support intake.

1. **[FOUNDER CHECKPOINT]** **Classify first.** For a security incident, use the
   private security@witwave.ai path first. The founder decides public
   disclosure.
2. **[FOUNDER CHECKPOINT]** **Founder-performed public issue.** Open
   `Incident: <short impact summary>` with label `incident`. State confirmed
   impact, cell scope and start time in UTC. Explain that in-product support
   is unavailable and that the issue carries status updates; do not solicit
   account data in comments.
3. **[FOUNDER CHECKPOINT]** **Founder-performed updates.** Post hourly or better
   for urgent impact. After restoration, copy the same value-free update to
   affected tickets as needed.
4. **[FOUNDER CHECKPOINT]** **Founder-performed recovery-point update.** Once
   chosen, explain the confirmed data-loss window in UTC without listing
   accounts or content.
5. **[FOUNDER CHECKPOINT]** **Breach notice.** The
   [Privacy Policy](legal/privacy-policy.md) and
   [DPA](legal/data-processing-addendum.md) provide the notice obligations.
   The founder and legal adviser decide whether the event requires individual
   notice and what it must say.
6. For a region change, also complete section 13 step 5.
7. **[FOUNDER CHECKPOINT]** **Founder-performed resolution.** Publish a brief
   root cause, user-visible effect and follow-up work, then close the issue
   while retaining the label. Explain confirmed loss of tickets, replies and
   agent email after the recovery point without disclosing content. Receive
   failures can depend on sender retry behavior; follow
   [Agent email](agent-email.md) and do not promise eventual delivery.

## 16. RTO: measured and unmeasured

| Stage | Measurement scope | Value |
|---|---|---|
| Detection by dead-man | Configured window; re-prove delivery under P1 | About 15 minutes recommended in [monitoring setup](runbooks.md#founder-open-plane-monitoring); not an end-to-end recovery bound |
| Detection to work starting | PHX1 incident | measured once, in the 2026-09-19 incident; not published |
| Provision a Civo cluster with `witself-infra up` | PHX1 incident | measured once, in the 2026-09-19 incident; not published |
| Restore a pre-migration dump (custom format) | PHX1 incident | measured once, in the 2026-09-19 incident; not published |
| Restore the nightly R2 dump | No repository timing record | Unknown |
| Core service | PHX1 incident | measured once, in the 2026-09-19 incident; not published |
| Full service: monitoring, backups, entitlement delivery | PHX1 incident | measured once, in the 2026-09-19 incident; not published |
| Node loss: PostgreSQL volume reattachment | Not rehearsed | Unknown |
| Production cell rebuild, including load balancer, DNS and HTTP-01 | Not rehearsed | The infrastructure CLI's convergence timeout is not a measured recovery duration |
| Region loss | Not rehearsed | Requires the region/provider preparation in G15 |

No RTO or RPO target is written. PHX1's restore time is not a prediction for
today; data has grown since. **[GAP] G14**.

## 17. Founder checkpoints in this runbook

Every action below uses the performer rules in section 0; the list grants no
standing authorization.

Before an incident:

1. Prove paging delivery on the current service (P1).
2. Confirm founder-held escrow outside the cell (P2).
3. Pick the fallback region or provider (P4).

During an incident:

4. Drain; approve any signup-mode change (section 4).
5. Classify the incident and perform public posts and updates (section 15).
6. Approve any gate deviation and the actual pin-roll attestation (R6,
   section 9 step 1b).
7. Authorize credential handling and dump listing and fetching (sections 4 and 9).
8. Approve the hold merge with a bold notice (sections 8 and 9).
9. Choose the recovery point and accept its data-loss window (section 9).
10. Approve transport and any departure from disposable-first restoration
    (section 9 step 4).
11. Release the hold only with verified integrity and schema compatibility
    (section 9 step 8).
12. Approve any forced pod/Node action, volume operation or destructive
    database/PVC step under R1 (sections 7 and 8).
13. Approve state treatment and inventory edits (section 11).
14. Perform production preview/apply and any DNS or R2 changes (section 11).
15. Authorize Secret restoration and paging changes with the required notice
    (section 9 step 3; section 10 step 4).
16. Authorize the registry re-point (section 12).
17. Authorize any directory-row write (section 12).
18. Authorize undrain; restored service alone is insufficient (section 12).
19. Choose the region/provider and perform any legal publication (section 13).
20. Fence a returning cluster and choose the surviving history (section 14).
21. Decide breach notices and authorize deletion reconciliation (sections 10
    and 15).
22. Never destroy the lost stack during this incident (R1 and R3).

## Gaps

- **G1.** No supported command sends or reads `registration_id`; occupied
  re-endpointing and identity verification require a separately reviewed API
  procedure.
- **G2.** No checked-in tool restores nightly dumps into cell PostgreSQL with
  transport, empty-target preflight, verification, role provisioning and safe
  recreation of a failed target.
- **G3.** Per-account snapshots have no committed disaster-restore endpoint;
  see [Periodic logical account snapshots](backup-and-recovery.md#periodic-logical-account-snapshots)
  and [Rollback-only restore drills](backup-and-recovery.md#rollback-only-restore-drills).
- **G4.** No supported command rewrites stored `acct:` directory fallbacks.
- **G5.** No node-loss procedure or node-replacement command; volume
  reattachment timing is unmeasured.
- **G6.** No command writes the `witself-<cell>` kube context from stack outputs.
- **G7.** No procedure retires dead-stack state or removes leftover Civo
  resources, including recovery of R2-backed state.
- **G8.** No recovery procedure rebinds SOPS operator Secrets to a new cell
  name when the source cluster is lost.
- **G9.** No repository procedure checks that the dump decryption identity
  and the control-plane edge token are available from founder-held escrow
  outside the cell.
- **G10.** No reconciliation of accounts created, closed or deleted after the
  recovery point, or of agents, tokens, vault bindings and capture state
  inside those accounts.
- **G11.** Detection of the monitored cell's loss rests on one external
  dead-man route; re-prove it end to end after any change to the paging
  service. The backup cell is unmonitored, and no Civo state check exists.
- **G12.** The server hold needs an overlay change and merged PR; on rebuild,
  it must land before provisioning.
- **G13.** The nightly dumps have no copy outside the Cloudflare account, and
  the repository configures no expiry for them.
- **G14.** No written RTO/RPO target or repository timing record for a nightly
  whole-database restore.
- **G15.** No prepared fallback region: the GitOps Civo mapping and the DPA
  cover NYC1; the infrastructure CLI's other Civo regions are outside the US.
- **G16.** Same-region collisions between a new stack and dead-cluster
  leftovers have not been rehearsed.
- **G17.** No out-of-band account-specific support intake while the cell is down.
- **G18.** Nothing fences a returning old cluster's writes, and no command
  compares and reconciles the old and restored databases.
