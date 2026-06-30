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

The cell values file is the single Git-owned place to pin chart versions, target
revisions, regions, namespaces, and secret references for that cell. For the
current sandbox cell, that file is
[`cells/aws-sandbox-usw2-dev/values.yaml`](cells/aws-sandbox-usw2-dev/values.yaml).

The Application manifests here reference this public repo by URL; a self-hosted
fork (`-gitops-repo`) would adjust the root source, and the per-cell values file
would set `gitops.repoURL` for child Applications. A central fleet controller
can move this to ApplicationSet later; for one Argo CD per cell, app-of-apps is
the simpler control plane.

## Notes

- This repo is **public**, and the root app points at the `main` branch, so Argo
  needs **no credentials** to read `.gitops/`.
- **No secrets live here.** Application secrets (DB credentials, …) are delivered
  into the cluster by the External Secrets Operator from AWS Secrets Manager;
  `.gitops/` only references them by name.
