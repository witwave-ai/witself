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
- `platform.externalDNS` enables the ExternalDNS chart and limits it to the cell
  zone with `domainFilters` and `txtOwnerId`. AWS uses Route 53; GCP uses Cloud
  DNS with a Workload Identity-bound Google service account. Azure uses Azure
  DNS with an AKS Workload Identity-bound managed identity; Pulumi enables the
  live Azure ExternalDNS values at runtime after it creates the zone, identity,
  and federated credential.
- `apps.witselfServer.awsAlbIngress` is the AWS ALB path. `gcpIngress` is the
  GKE-native path: GKE Ingress, BackendConfig health checks, a reserved global
  static IP, FrontendConfig HTTP-to-HTTPS redirects, and a Google-managed
  certificate. `azureGateway` is the Azure Application Gateway for Containers
  path. It renders Gateway API companion manifests plus cert-manager Gateway
  HTTP-01 issuer/certificate resources for HTTPS when enabled; Pulumi turns it
  on at runtime after enabling the AKS-managed ALB Controller add-on and
  injecting the delegated association subnet.
- `platform.externalSecrets` points ESO at the cell secret store. AWS uses EKS
  Pod Identity with no auth block in the store, GCP uses a GSA annotation, and
  Azure uses AKS Workload Identity plus a Key Vault `ClusterSecretStore`.

`witself-infra` still owns the durable cloud side: Route 53, Cloud DNS, or Azure
DNS zone creation, Cloudflare parent-zone delegation, certificate/static-IP or
gateway-association cloud resources, and the cloud IAM role/service account
ExternalDNS uses. Pulumi injects generated cloud outputs, such as the ACM
certificate ARN, GCP static IP name, Azure ALB subnet ID, and cloud identity
annotations for GitOps-managed add-ons, into the root app at deploy time.
