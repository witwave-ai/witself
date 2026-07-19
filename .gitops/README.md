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

Do not treat a committed pin as deployment proof. For every provisioned cell,
verify Argo health/sync, replacement-pod readiness, and the public
`/v1/version` response before advancing the wave. With a database DSN,
`witself-server serve` applies its embedded migrations before serving, so a
migration failure prevents the replacement pod from becoming Ready. Complete
release-specific API and client smoke tests before calling a feature
operational. The full procedure is in
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
