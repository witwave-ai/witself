# witself-infra

`witself-infra` provisions and manages **Witself cells**. A cell is one complete,
isolated Witself stack in a single cloud account/region. The same cell program
provisions a self-hoster's single cell and each cell in the Witself Cloud fleet —
only the stack config and who runs it (a human vs CI) differ.

## Why this is a separate module

This directory is its own Go module
(`github.com/witwave-ai/witself/infra/pulumi`), independent of the repo root. It
is built on the [Pulumi Automation API](https://www.pulumi.com/docs/iac/using-pulumi/automation-api/)
in inline-program mode: the cell definition is a Go closure compiled into the
`witself-infra` binary, so there is no project directory and you never invoke
`pulumi` yourself. The Automation API does drive the `pulumi` engine binary under
the hood (see Prerequisites).

Pulumi's provider SDKs are a large dependency tree. Keeping them in this nested
module means they never touch the lean `ws` and `witself-server` binaries, which
build from the repo-root module.

## Layout

```text
infra/pulumi/
  cmd/witself-infra/    # the CLI: up | preview | destroy | refresh | outputs
  internal/backend/      # state backend bootstrap/lookup (AWS S3, GCP GCS, Azure Blob)
  internal/cell/        # the inline Pulumi program — the cell definition
```

## Prerequisites

The Automation API drives the `pulumi` engine binary, so it must be on `PATH`
(`brew install pulumi`). A planned follow-up has `witself-infra` install and pin
its own engine on first run (via `auto.NewPulumiCommand`), so the end user
installs only `witself-infra` — the engine is fetched like a provider plugin.

## Run it

```sh
# build
go build -o bin/witself-infra ./cmd/witself-infra

# the cell name is composed from components: <cloud>-<account-alias>-<region-code>-<role>
# e.g. these flags -> cell aws-sandbox-usw2-dev, resources witself-aws-sandbox-usw2-dev-*
# creds come from -aws-profile (or the ambient AWS chain / OIDC).
F="-cloud aws -account-alias sandbox -region us-west-2 -role dev -aws-profile witwave-sandbox"

# state lives in S3 by default — create the per-account+region backend once:
./bin/witself-infra bootstrap -cloud aws -region us-west-2 -aws-profile witwave-sandbox

# then the cell loop (S3 is the default; add -backend local for a no-AWS dev run)
./bin/witself-infra preview $F
./bin/witself-infra up      $F
./bin/witself-infra outputs $F
./bin/witself-infra destroy $F
```

Inputs split two ways: **functional** (`-cloud`, `-region`, `-profile`) drive
behavior; **labels** (`-account-alias`, `-role`) are free text used only in the
name. Credentials are a name, not a secret — `-aws-profile` (or the ambient
`AWS_PROFILE`/OIDC when omitted); `-account-alias` does **not** select creds.
State is stored in **S3 by default**; `up` errors with "run bootstrap first" if
the backend is missing. Pass `-backend local` for a zero-setup local file backend
(dev/experiments), which uses a tool-managed passphrase under `~/.witself-infra/state`.

## State backend

State is stored in a cloud object-storage backend for real cells — shared,
durable, cloud-KMS-encrypted secrets (no passphrase), **one object-store backend
and one cloud key per account/project/subscription + region**. (`-backend local`
is the dev opt-out.)

AWS uses S3 + AWS KMS and remains the default backend:

```sh
# once per account+region: create the bucket + KMS key (idempotent — reuses if present)
witself-infra bootstrap -cloud aws -region us-west-2 -aws-profile witwave-sandbox

# then point cells at it
witself-infra up --backend s3 -cloud aws -account-alias sandbox -region us-west-2 -role dev -aws-profile witwave-sandbox
```

`bootstrap` creates `witself-state-<account-id>-<region-code>` (versioned,
SSE-KMS, public-access-blocked, TLS-only) and `alias/witself-state-<region-code>`,
and prints the `s3://…` backend + `awskms://…` secrets provider. `up --backend s3`
**uses** that backend (KMS-encrypted secrets, no passphrase) and errors if it is
missing; pass `-bootstrap` to create it on first use.

GCP uses GCS + Cloud KMS. The GCP project is a shared substrate boundary, not the
cell boundary: one project can host multiple cell stacks. The current GCP cell
program provisions a dedicated custom VPC, regional subnet, GKE pod/service
secondary ranges, an internal firewall rule, private services access for
private-IP Cloud SQL, a regional GKE Autopilot cluster, a minimal private-IP
Cloud SQL Postgres instance, Secret Manager JSON secrets for DB/bootstrap/
provision material, a regional Cloud Router + Public Cloud NAT with a reserved
outbound IPv4 address, a public Cloud DNS zone, and a reserved global IPv4
address for the GKE Ingress. With `-argocd`, it also installs Argo CD and
bootstraps the GCP cell values file. It grants Workload Identity paths for
External Secrets Operator to read the cell secrets and for ExternalDNS to manage
only the cell Cloud DNS zone. The GCP cell values enable `witself-server`,
ExternalDNS, GKE Ingress, BackendConfig health checks, FrontendConfig
HTTP-to-HTTPS redirects, and a Google-managed certificate.

GCP profile sizing follows the same operator intent as AWS. `-profile minimal`
is the low-cost, one-shot test profile: zonal Cloud SQL, small disk, and no
retained database backups. `-profile prod` raises Cloud SQL to regional
availability, enables PITR plus retained/final backups, and increases disk
headroom; it is meant for persistent cells rather than nightly save-money
teardown loops.

```sh
# Pulumi's GCS backend and gcpkms secrets provider use Application Default
# Credentials, not only `gcloud auth login`.
gcloud auth application-default login --project witself-sandbox
gcloud config set container/use_application_default_credentials true

# prepare state only
witself-infra bootstrap \
  -backend gcs \
  -cloud gcp \
  -gcp-project witself-sandbox \
  -region us-west2

# or one-shot: prepare state if missing, then create/update the GCP substrate
witself-infra up \
  -account-alias sandbox \
  -argocd \
  -backend gcs \
  -bootstrap \
  -cloud gcp \
  -cidr 10.20.0.0/16 \
  -control-plane https://self.witwave.ai \
  -gcp-project witself-sandbox \
  -gitops-path .gitops/charts/bootstrap \
  -gitops-repo https://github.com/witwave-ai/witself \
  -gitops-revision main \
  -gitops-values-path .gitops/cells/gcp-sandbox-use1-dev/values.yaml \
  -profile minimal \
  -region us-east1 \
  -restore-archives \
  -restore-any-region \
  -role dev
```

Azure uses Blob Storage + Key Vault. The Azure subscription is a shared
substrate boundary, not the cell boundary: one subscription+region backend can
hold many cell stacks. The current Azure cell program provisions a dedicated
resource group, VNet, workload subnet, PostgreSQL-delegated DB subnet, NAT
Gateway, static public IPv4 address, private DNS zone/link, private Azure
Database for PostgreSQL Flexible Server, the logical `witself` database, and a
per-cell Key Vault containing DB/bootstrap/provision JSON secrets. It also
creates an AKS cluster in the workload subnet with Azure CNI overlay, controlled
egress through the cell NAT Gateway, and OIDC/workload identity enabled. ESO,
DNS/ingress, and GitOps parity are later slices.

```sh
# Pulumi's azblob backend and azurekeyvault secrets provider can use Azure CLI
# auth. The backend bootstrap stores a storage account key in the local process
# environment for Pulumi, but it never prints that key.
az login --tenant a18639f4-1eb4-4810-ab3b-5717aa935e27
az account set --subscription witwave-sandbox

# prepare state only
witself-infra bootstrap \
  -azure-subscription witwave-sandbox \
  -backend azblob \
  -cloud azure \
  -region eastus2

# create/update the Azure network and controlled-egress substrate
witself-infra up \
  -account-alias sandbox \
  -azure-subscription witwave-sandbox \
  -backend azblob \
  -cloud azure \
  -db-version 18 \
  -ingress none \
  -k8s-version 1.36 \
  -profile minimal \
  -region eastus2 \
  -role dev
```

`bootstrap` registers the required Azure resource providers if needed, creates
`witself-state-<region-code>`, a private versioned Blob container named
`pulumi-state`, and a Key Vault key named `pulumi-secrets`. It prints the
`azblob://...` backend and `azurekeyvault://...` secrets provider. In RBAC-mode
vaults, bootstrap grants the signed-in user `Key Vault Crypto Officer` on the
state vault so Pulumi can create and use the state encryption key.

`up` registers the required workload resource providers if needed before running
Pulumi, so a fresh subscription can create the VNet, NAT resources, database,
Key Vault, and AKS cluster in the same command. The workload subnet has default
outbound access disabled and egresses through the NAT Gateway; the DB subnet has
default outbound access disabled and is delegated to
`Microsoft.DBforPostgreSQL/flexibleServers`. The PostgreSQL server uses password
auth, public network access disabled, the delegated DB subnet, and a private DNS
zone named `privatelink.postgres.database.azure.com`.

The cell Key Vault is separate from the state-backend Key Vault. It stores the
same app material as AWS Secrets Manager and GCP Secret Manager: `db`,
`bootstrap-operator-token`, and `provision-token`. The current operator gets an
access policy so Pulumi can write the secrets during `up`; Azure Workload
Identity read access is added with the ESO slice.

For GCP cells with `-argocd`, `up` does not stop at Pulumi success. After the
cell is registered and reachable through the control-plane probe, and after any
archive restore completes, the CLI reads the GKE API directly with ADC and waits
for every Argo CD Application to report `Synced/Healthy`. That makes Google
ManagedCertificate status lag visible as a normal waiter instead of a surprise
operator caveat after the command exits.

## GitOps (Argo CD)

Pass `-argocd` to install the Argo CD control plane into the cell's cluster from
its upstream Helm chart (`argo-cd` 10.0.1). This is **universal** — the chart,
not the AWS-only managed EKS capability — so the same install works on EKS, GKE,
or a self-hosted cluster. It is **opt-in** and off by default.

```sh
witself-infra up -argocd $F

# reach the UI (ClusterIP):
kubectl -n argocd port-forward svc/argocd-server 8080:443   # https://localhost:8080, user: admin
kubectl -n argocd get secret argocd-initial-admin-secret -o jsonpath='{.data.password}' | base64 -d
```

The Kubernetes provider authenticates with an exec kubeconfig (`aws eks
get-token` on AWS, `gcloud auth application-default print-access-token` on GCP),
so the first install can outlast a static token while managed Kubernetes
provisions capacity.

`-argocd` also creates a root Argo `Application` (`bootstrap`) that renders the
shared `.gitops/charts/bootstrap` chart with this cell's
`.gitops/cells/<cell>/values.yaml` file. That Git-owned values file pins the
platform/app chart versions and cell-specific settings. The repo is public, so
Argo needs **no credentials** (private-repo creds: issue #7). Point Argo at a
self-hosted fork with the `-gitops-*` flags:

```sh
witself-infra up -argocd \
  -gitops-repo https://github.com/you/your-config \
  -gitops-path .gitops/charts/bootstrap \
  -gitops-values-path .gitops/cells/aws-sandbox-usw2-dev/values.yaml \
  -gitops-revision main $F
```

SSO, ingress polish, and production hardening are later slices.

## Fleet (control plane)

Pass `-control-plane` to make cell lifecycle changes known to the Witself Cloud
control plane (`https://self.witwave.ai`). Omit it and no registration happens —
that is the self-hosted path, same command.

```sh
# up: provision, then REGISTER the cell with the fleet (post-step).
# The registered endpoint is the cell's apiHost output (api.<cell>.<domain>).
witself-infra up -control-plane https://self.witwave.ai $F

# destroy: DRAIN the cell (placement stops), EVACUATE every account to a
# Cloudflare R2 archive (per-account file, integrity-checked), then REMOVE the
# empty registry entry and tear down:
witself-infra destroy -control-plane https://self.witwave.ai $F
#   evacuated acc_… from <cell>
#   ...
#   cell <cell>: N accounts evacuated to Cloudflare R2
#   cell <cell> removed from fleet

# sandbox/dev teardown where the cell's data genuinely dies — SKIP evacuation
# and force-purge account entries from the control plane:
witself-infra destroy -control-plane https://self.witwave.ai -destroy-accounts $F
```

Authorization is the **fleet token**, read from `-fleet-token-file` if given,
else `WITSELF_FLEET_TOKEN`, else `~/.witself/tokens/fleet.token` (minted when the control plane was deployed; its
counterpart lives as the `FLEET_TOKEN` Worker secret). One token per fleet — all
cells registering to the same control plane use the same token.

Registration is deliberately **outside the Pulumi resource graph**: fleet
membership is bookkeeping on the control plane, not a cloud resource. `up`
registers after a successful provision; `destroy` drains/removes before teardown.
The control plane never touches infrastructure — Pulumi destroys things, the
control plane forgets them.

| Flag | Applies to | Effect |
|---|---|---|
| `-control-plane URL` | `up` | register the cell (upsert) after provisioning |
| `-control-plane URL` | `destroy` | drain, evacuate every account to R2, then remove the cell from the fleet before teardown |
| `-fleet-token-file PATH` | both | read the fleet token from this file (default: `WITSELF_FLEET_TOKEN` env, then `~/.witself/tokens/fleet.token`) |
| `-destroy-accounts` | `destroy` | with `-control-plane`: SKIP evacuation and force-purge accounts — sandbox/dev override, the data dies with the cell |
| `-restore-archives` | `up` | after registration, restore archived accounts whose stored region matches this cell's region |
| `-restore-any-region` | `up` | with `-restore-archives`: explicit operator override that restores every archived account, ignoring stored region |

## Roadmap (one slice at a time)

1. **[done]** module + CLI + Automation API loop.
2. **[done]** AWS substrate: cell VPC (NAT egress) + EKS Auto Mode + RDS Postgres.
3. **[done]** S3 + KMS state backend (`bootstrap`).
4. **[done]** Argo CD (GitOps control plane) via Helm — opt-in `-argocd`.
5. **[done]** Wire Argo at the bootstrap app-of-apps chart + per-cell values.
6. **[done]** Metrics Server in the GitOps platform tier (resource metrics API
   for `kubectl top` and HPA CPU/memory signals).
7. **[done]** Fleet registration: `-control-plane` on `up`/`destroy` registers /
   drains+removes the cell against the control plane (`-destroy-accounts` to
   purge); fleet token from `~/.witself/tokens/fleet.token`.
8. **[done]** GCP GCS + Cloud KMS state backend and empty stack lifecycle.
9. **[done]** GCP network substrate: custom VPC, regional subnet, secondary
   ranges for future GKE pods/services, internal firewall, and private services
   access for future Cloud SQL.
10. **[done]** GCP GKE Autopilot substrate with VPC-native pod/service ranges
    and Workload Identity.
11. **[done]** GCP Cloud SQL Postgres over private services access plus Secret
    Manager DB connection JSON.
12. **[done]** GCP Argo CD control plane and GCP cell GitOps values scaffold.
13. **[done]** GCP ESO → Secret Manager via GKE Workload Identity for DB,
    bootstrap, and provision secrets.
14. **[done]** GCP `witself-server` GitOps app as an internal ClusterIP
    workload, backed by the ESO-synced Cloud SQL DSN.
15. **[done]** GCP Cloud DNS + Cloudflare delegation + ExternalDNS Workload
    Identity + GKE Ingress/BackendConfig/FrontendConfig + Google-managed
    certificate + HTTP-to-HTTPS redirect.
16. **[done]** GCP controlled egress with regional Cloud Router, reserved
    outbound IPv4 address, and Public Cloud NAT over the cell subnet ranges.
17. **[done]** Azure Blob Storage + Key Vault state backend and empty stack
    lifecycle in `eastus2`.
18. **[done]** Azure network substrate: resource group, VNet, workload subnet,
    PostgreSQL-delegated DB subnet, NAT Gateway, and static outbound IP.
19. **[done]** Azure private PostgreSQL Flexible Server plus logical `witself`
    database on the delegated DB subnet.
20. **[done]** Azure Key Vault app secrets for DB, bootstrap, and provision
    material.
21. **[done]** Azure AKS with Azure CNI overlay, controlled egress through the
    cell NAT Gateway, and OIDC/workload identity enabled.
22. Azure ESO Workload Identity, DNS/ingress, and GitOps parity.
23. SSO; sealed-plane KMS (prod); deletion-protection break-glass flow, and
    remaining production hardening.
