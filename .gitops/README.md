# .gitops — Argo CD source of truth

This directory is the Git-owned desired state that Argo CD watches after
`witself-infra up -argocd` installs Argo in a cell. It is not an application by
itself; it is the control tree Argo reads to decide which platform services and
Witself apps should exist in the cluster.

`witself-infra` creates one root Argo `Application` named `bootstrap`. That root
app points at the reusable [`charts/bootstrap`](charts/bootstrap) Helm chart and
loads one cell-specific [`cells/<cell>/values.yaml`](cells) file.

## Layout

```text
.gitops/
  charts/
    bootstrap/  # root app-of-apps chart; renders child Argo Applications
    platform/   # platform tier; renders cert-manager, External Secrets, KEDA, and other add-ons
    apps/       # app tier; renders Witself app prerequisites and apps
  cells/        # per-cell values, keyed by composed cell name
    aws-sandbox-usw2-dev/
      values.yaml
    aws-sandbox-use1-dev/
      values.yaml
    azure-sandbox-use2-dev/
      values.yaml
    azure-sandbox-usw2-dev/
      values.yaml
    gcp-sandbox-use1-dev/
      values.yaml
    gcp-sandbox-usw2-dev/
      values.yaml
```

## How app-of-apps works here

The root Argo app points at `charts/bootstrap`. That chart does not deploy pods.
It renders two child Argo `Application` objects:

- `platform`, which points at `charts/platform`.
- `apps`, which points at `charts/apps`.

Those child Applications then render their own Helm charts with the same
`cells/<cell>/values.yaml` file. In other words, the Helm charts in this folder
are mostly a tidy way to template Argo `Application` YAML and pass the same
per-cell values through each layer.

Each cell values file is the single Git-owned place to pin chart versions,
target revisions, regions, stable DNS names, namespaces, and secret references
for that configured cell. A directory here is desired-state configuration, not
evidence that its cell is provisioned, reachable, or currently reconciled.

Stable DNS intent lives in that values file too: `cell.domain`, `cell.apiHost`,
and `platform.externalDNS` describe what ExternalDNS should manage. Pulumi keeps
ownership of the cloud resources behind that intent, including Route 53, Cloud
DNS, Azure DNS, Cloudflare delegation, ACM or Google-managed TLS, static ingress
IPs or gateway association subnets where the cloud needs them, and the cloud IAM
identity ExternalDNS uses.

The Application manifests here reference this public repo by URL; a self-hosted
fork (`-gitops-repo`) would adjust the root source, and the per-cell values file
would set `gitops.repoURL` for child Applications. A central fleet controller
can move this to ApplicationSet later; for one Argo CD per cell, app-of-apps is
the simpler control plane.

## Rolling A Released Application

Publish and verify the release before changing GitOps. The helper accepts the
released version without the Git tag's `v` prefix and changes exactly the
Witself server chart and image pins for one configured cell:

Before a release that can advance the database schema, create and verify the
cell's pre-migration database backup and record its identifier in the private
rollout record. For managed GCP, follow the hard on-demand Cloud SQL gate in
[`docs/backup-and-recovery.md`](../docs/backup-and-recovery.md#gcp-cloud-sql-pre-migration-backup);
this is mandatory even when a scheduled backup appears recent.

```sh
VERSION="${RELEASE_VERSION:?set RELEASE_VERSION}"
CELL="${ROLLOUT_CELL:?set ROLLOUT_CELL}"
scripts/roll-cell.sh "$CELL" "$VERSION"
git diff -- ".gitops/cells/${CELL}/values.yaml"
```

Run the helper from the repository root. Review the resulting diff, group only
the intended canary or wave in one commit, and push that commit to `main`.
Provisioned cells whose bootstrap application is healthy watch `main`; their
child applications use automated sync, pruning, and self-healing.

The app-of-apps renderer withholds `worker.messageRateBucketCleanup` from child
chart pins older than `0.0.224`; those strict schemas reject the new field.
Advancing a cell to `0.0.224` atomically begins forwarding the cleanup contract.

The same compatibility rule independently withholds
`worker.agentEmailRateBucketCleanup` from child chart pins older than
`0.0.226`. Advancing a cell to `0.0.226` begins forwarding the default-enabled
email-specific cleanup job together with the schema and binary that understand
its configuration.

`apps.witselfServer.billing.endpoint` remains empty and is not forwarded by
default, preserving existing cells' providerless state and compatibility with
older strict child schemas. A non-empty value is accepted only for a managed
cell with matching chart and image `v0.0.255` or newer, and must be a canonical
HTTPS control-plane origin or prefix without credentials, query, fragment,
encoded characters, or unsafe characters. Roll chart and image first; activate
the endpoint in a separate reviewed cell-values commit only after the
control-plane route and authentication fence are verified. The value is public
discovery metadata, not a Stripe secret or provider activation switch.

`apps.witselfServer.agentEmail.receiveProduction` is independently withheld
until both the child chart and image are `0.0.241` or newer. Enabling it on an
older mixed pin fails rendering instead of silently dropping the exact account
cohort. It remains false in fleet defaults and must be activated only for a
reviewed cell after the matching release has converged. API startup performs
bounded read-only cohort validation; run the explicit cell-local mailbox
backfill once, never as a per-replica startup action.

Managed cells set `accountIDsExistingSecret.name` and `.key`; they never commit
the literal `accountIDs` array. Provision an immutable, versioned Kubernetes
Secret in the server namespace through the cell's secret manager/External
Secrets path before enabling receive. Its value is one canonical, byte-sorted
CSV with no spaces or trailing newline. The app-of-apps forwards only the
Secret reference, and the child ConfigMap contains no account IDs. A missing or
malformed value fails closed at pod creation or API startup. In-place Secret
mutation is unsupported: create a new versioned Secret and update the reference
name so the Deployment rolls. Keep the literal `retryCanaryAgentID` empty in
managed mode. Starting with matching chart and image `v0.0.245`, select the
production retry canary through `retryCanaryAgentIDExistingSecret.name` and
`.key`. That distinct Secret must be immutable and versioned, and its value
must be exactly one canonical `agent_*` ID with no whitespace or trailing
newline. The app-of-apps rejects a literal canary even while the gate is dark,
withholds the empty new field from pre-`0.0.245` strict child schemas, and
fails rendering if a nonempty Secret reference is paired with an older chart
or image. Changing the Secret name or key changes both server rollout
checksums and restarts the API pods.

Roll it out in two commits. First converge `v0.0.245`, the Secret-backed
account cohort, and production receive with the retry-canary Secret name empty.
After backfill, export the private canary manifest and choose one eligible
agent. Create its distinct immutable versioned Secret, then set the reference
in a config-only commit and wait for all replacement pods. Re-export and verify
the selected agent is included before any edge/provider activation.

Do not treat a committed pin as deployment proof. For every provisioned cell,
verify Argo health/sync, replacement-pod readiness, and the public
`/v1/version` response before advancing the wave. When the worker is enabled,
also verify both worker replicas are Ready, their health and metrics endpoints
respond, every configured job reports running, and the API pods report no
legacy in-process background loops. With a database DSN, both cell executables
apply embedded migrations before serving, so a migration failure prevents the
replacement pod from becoming Ready. Complete release-specific API and client
smoke tests before calling a feature operational. The full procedure is in
[`docs/deployment-cells.md`](../docs/deployment-cells.md).

Avatar creative-payload compaction uses two GitOps phases. Keep
`apps.witselfServer.avatarPayloadCompactionEnabled: false` while rolling the
new chart and image, freeze avatar mutation/import/export during writer
convergence, and verify that every old writer has drained. Enable compaction
only in a later config-only commit; the nested chart checksum must then restart
every server pod. Do not combine that Phase-B flip with an image or chart pin.

## Notes

- This repo is **public**, and the root app points at the `main` branch, so Argo
  needs **no credentials** to read `.gitops/`.
- **No secrets live here.** Application secrets (DB credentials, …) are delivered
  into the cluster by the External Secrets Operator from the cell's cloud secret
  store: AWS Secrets Manager, GCP Secret Manager, or Azure Key Vault. `.gitops/`
  only references them by name.
