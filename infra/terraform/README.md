# Witself Terraform

Public Terraform for provisioning the cloud **substrate** Witself runs on. This
is a skeleton: directories, provider/version pins, variables, outputs, and
**commented placeholders** for the real resources. Concrete resource bodies land
with the implementation pass — the shape is frozen here so reviewers can see the
security posture without any live infrastructure.

Terraform owns cloud substrate. Helm owns the application deployment. See
[`docs/terraform-infrastructure.md`](../../docs/terraform-infrastructure.md) and
[`docs/helm-chart.md`](../../docs/helm-chart.md).

## What Terraform provisions

For each provider the substrate is:

- A Kubernetes cluster (EKS / GKE / AKS) — or integration with an existing one.
- PostgreSQL with the **`pgvector`** extension. `pgvector` is a hard gate for the
  open plane (memories + facts); semantic recall depends on it.
- Object/blob storage for exports, attachments, diagnostic bundles, and backups.
- Workload identity (IRSA / GCP Workload Identity / Azure Workload Identity) so
  `witself-server` reaches Postgres, the embedding provider, KMS, and storage
  without static credentials.
- Networking: VPC/VNet, subnets, security groups / firewall rules.
- **Sealed-plane KMS only:** a customer managed key (CMK) for the sealed plane
  (secrets + TOTP), provisioned **only when `sealed_plane_enabled = true`**. The
  open plane never needs KMS. The deployment identity is granted envelope
  operations (`Encrypt`/`Decrypt`/`GenerateDataKey*`/`DescribeKey`) only — never
  key administration or deletion. See
  [`docs/key-hierarchy.md`](../../docs/key-hierarchy.md) and
  [`docs/encryption-model.md`](../../docs/encryption-model.md).

Terraform then **outputs** the references the Helm chart consumes (namespace,
service account annotations, database secret reference, pgvector confirmation,
storage bucket, public URL, and — when the sealed plane is on — the KMS provider
and key ID mapped by Helm to `WITSELF_KMS_PROVIDER` / `WITSELF_KMS_KEY_ID`).

## Layout

```text
infra/terraform/
  modules/
    aws/            # First implementation target (most real structure)
      modules/kms/  # Sealed-plane CMK submodule (sealed_plane_enabled only)
    gcp/            # Visible skeleton, AWS-first
    azure/          # Visible skeleton, AWS-first
  stacks/
    self-hosted/    # Reference deployments operators can copy/adapt
      aws/
      gcp/
      azure/
    witself-cloud/  # Managed Witself Cloud example shape
      aws/
```

AWS carries the most real structure. GCP and Azure are deliberately visible
skeletons that note AWS is implemented first.

Every directory has `versions.tf`, `variables.tf`, `main.tf`, and `outputs.tf`.

## Provider status

| Provider | Module | Self-hosted stack | Cloud stack | Status        |
| -------- | ------ | ----------------- | ----------- | ------------- |
| AWS      | yes    | yes               | yes         | First target  |
| GCP      | yes    | yes               | —           | Planned       |
| Azure    | yes    | yes               | —           | Planned       |

## Consuming a module

Pin to a release tag with a Git source:

```hcl
module "witself_aws" {
  source = "git::https://github.com/witwave-ai/witself.git//infra/terraform/modules/aws?ref=v0.1.0"

  cluster_name        = "witself-prod"
  region              = "us-east-1"
  sealed_plane_enabled = true
}
```

## Never commit

State, credentials, account IDs, customer identifiers, database passwords,
embedding-provider API keys, raw Witself tokens, KMS key material, or real
`.tfvars`. The repo `.gitignore` blocks `*.tfstate*`, `*.tfvars` (except
`*.tfvars.example`), `**/.terraform/`, and crash logs. Provider credentials are
never required for ordinary CI validation.

## CI

CI runs (see [`docs/terraform-infrastructure.md`](../../docs/terraform-infrastructure.md)):

- `terraform fmt -check -recursive infra/terraform`
- `terraform init -backend=false` then `terraform validate` for modules/stacks
- `tflint` provider linting
- `checkov` (or equivalent) static security checks
- secret scanning of examples

`terraform validate` against the cloud modules can require the provider plugins;
in CI it runs after `init -backend=false`. Locally, `terraform fmt -check
-recursive` is the always-available gate.
