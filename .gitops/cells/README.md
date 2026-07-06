# cells/ — per-cell GitOps control files

Each directory is named by its composed cell name (for example,
`aws-sandbox-usw2-dev/`). `witself-infra -argocd` points the root Argo
Application at the shared bootstrap chart and passes this cell's `values.yaml`
as the chart values file. The rendered `platform` and `apps` tier Applications
also use this same values file.

```text
cells/<cell>/
  values.yaml   # chart pins, app pins, region, DNS names, namespaces, secret references
```

This file is the Git-owned control surface for a cell. Change chart versions or
cell-specific settings here, then let Argo reconcile the rendered child
Applications.

For DNS, keep the stable names here:

- `cell.domain` is the cell's public DNS zone, usually
  `<cell>.cells.witself.witwave.ai`.
- `cell.apiHost` is the Witself API hostname under that zone.
- `platform.externalDNS` enables the ExternalDNS chart for AWS cells and limits
  it to the cell zone with `domainFilters` and `txtOwnerId`. Keep it disabled on
  GCP until the GCP DNS/ingress slice lands.
- GCP cells run `witself-server` as an internal ClusterIP workload before the
  public ingress slice lands. ESO syncs the DB secret from Google Secret Manager
  first; keep GCP ExternalDNS and ingress-specific values disabled until the GCP
  DNS/ingress path exists.

`witself-infra` still owns the durable cloud side: Route 53 zone creation,
Cloudflare parent-zone delegation, ACM certificate validation, and the AWS Pod
Identity role ExternalDNS uses. Pulumi injects generated cloud outputs, such as
the ACM certificate ARN, into the root app at deploy time.
