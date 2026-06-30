# .gitops — GitOps source of truth

Argo CD (installed per cell by `witself-infra up -argocd`) watches this
directory. `witself-infra` creates a single root Argo `Application` named
`bootstrap` that renders the reusable [`charts/cell-bootstrap`](charts/cell-bootstrap)
chart with one cell-specific [`cells/<cell>/values.yaml`](cells) file.

## Layout

```text
.gitops/
  charts/
    cell-bootstrap/ # root app-of-apps chart; renders platform + apps tiers
    cell-platform/  # platform tier; renders platform add-ons/config
    cell-apps/      # app tier; renders app prerequisites + app Applications
  cells/                     # per-cell control files, by composed cell name
    aws-sandbox-usw2-dev/
      values.yaml
    aws-sandbox-use1-dev/
      values.yaml
```

`witself-infra up -argocd` creates one root Argo `Application` (`bootstrap`)
pointing at [`charts/cell-bootstrap`](charts/cell-bootstrap), with values loaded
from `cells/<cell>/values.yaml`. That bootstrap chart renders the child Argo
Applications for the `platform` and `apps` tiers. Those tier Applications render
their own Helm charts with the same cell values file. The cell values file is the
single Git-owned place to pin chart versions, target revisions, regions,
namespaces, and secret references for that cell.

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
