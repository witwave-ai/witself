# cells/ — per-cell GitOps roots

Each directory is named by its composed cell name (for example,
`aws-sandbox-usw2-dev/`). A cell can point `witself-infra -gitops-path` at its
own `bootstrap/` directory so Argo reconciles only that cell's platform and app
trees.

```text
cells/<cell>/
  bootstrap/    # root Argo app target for this cell
    platform.yaml
    apps.yaml
  values.yaml   # cell-specific pins and secret references
```

The per-cell bootstrap files intentionally point at `cells/<cell>/platform` and
`cells/<cell>/apps`. Those trees are populated as each shared manifest is moved
behind the cell boundary.
