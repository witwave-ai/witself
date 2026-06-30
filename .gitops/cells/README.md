# cells/ — per-cell GitOps control files

Each directory is named by its composed cell name (for example,
`aws-sandbox-usw2-dev/`). `witself-infra -argocd` points the root Argo
Application at the shared bootstrap chart and passes this cell's `values.yaml`
as the chart values file. The rendered `platform` and `apps` tier Applications
also use this same values file.

```text
cells/<cell>/
  values.yaml   # chart pins, app pins, region, namespaces, secret references
```

This file is the Git-owned control surface for a cell. Change chart versions or
cell-specific settings here, then let Argo reconcile the rendered child
Applications.
