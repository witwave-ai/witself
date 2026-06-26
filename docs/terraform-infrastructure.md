# Witself Terraform Infrastructure

Status: draft. This document describes where Terraform should live in the
Witself repository and what it should provision for managed and self-hosted
deployments.

## Decision

Witself should include public Terraform under `infra/terraform` for AWS, GCP,
and Azure infrastructure.

Terraform owns cloud substrate. Helm owns application deployment.

Terraform should provision Kubernetes, the PostgreSQL database with the
`pgvector` extension, object/blob storage, workload identity, networking, and
the Kubernetes integration points that the Helm chart needs. The Helm chart
should deploy `witself-server` onto that substrate and wire probes, metrics, and
optional Prometheus Operator resources. The split mirrors Witpass; the payload
under it is self-identity, not secrets, so there is no KMS pillar to provision.

AWS is the first implementation target. GCP and Azure remain planned provider
targets and should keep visible module/stack placeholders, but AWS should get
the first complete managed-cloud and self-hosted implementation.

## Repository Layout

Initial target layout:

```text
infra/terraform/
  modules/
    aws/
    gcp/
    azure/
  stacks/
    self-hosted/
      aws/
      gcp/
      azure/
    witself-cloud/
      aws/
      gcp/
      azure/
  examples/
    values/
      aws.yaml
      gcp.yaml
      azure.yaml
```

The `modules/` tree should hold reusable provider-specific modules. The
`stacks/` tree should show composed deployments. The `examples/values/` tree can
show Helm values generated from, or aligned with, Terraform outputs.

Actual Terraform state, cloud credentials, private account IDs, customer
identifiers, production credentials, embedding-provider API keys, and
environment-specific `.tfvars` files must not be committed.

## AWS Target

The AWS module is the first implementation target. It should support:

- EKS cluster or integration with an existing EKS cluster.
- RDS/Aurora PostgreSQL with the `pgvector` extension enabled, because Witself
  stores memory embedding vectors in Postgres and semantic recall depends on it.
- S3 bucket for object/blob storage when needed (large exports, diagnostic
  bundles, support attachments, backup artifacts).
- IAM roles for service accounts (IRSA) for `witself-server` workload identity.
- Security groups and network policy prerequisites.
- Optional Route 53 and ACM integration.
- Optional AWS KMS key for data-at-rest or optional field-level encryption of
  `sensitive` facts. KMS is demoted to an optional input, not a core
  dependency; see [storage.md](storage.md).
- Networking inputs sized for inter-agent messaging if a future transport needs
  cross-pod or cross-AZ delivery paths (see [Messaging
  Networking](#messaging-networking)).
- Outputs for the Helm chart, such as service account annotations, database
  secret reference, bucket name, public URL, and the optional KMS key ID when
  enabled.

### pgvector enablement

The AWS module must make `pgvector` a first-class concern, not an afterthought:

- Use an RDS/Aurora PostgreSQL engine version that ships `pgvector`.
- Add `vector` to `shared_preload_libraries` where the engine requires it via a
  managed parameter group.
- Run `CREATE EXTENSION IF NOT EXISTS vector;` as a provisioning step (a
  bootstrap SQL run, an init Job, or `witself-server migrate` on first start).
- Surface the vector extension state and dimensionality expectations as outputs
  so the Helm chart and capability contract can confirm semantic recall is
  available. `pgvector` is a hard prerequisite of the backend; its absence is a
  deployment error, not a recall-degrade trigger. Recall degrades only when the
  embedding provider is unavailable.

## GCP Target

The GCP module is a planned follow-up target. It should eventually support:

- GKE cluster or integration with an existing GKE cluster.
- Cloud SQL for PostgreSQL with the `pgvector` extension enabled.
- Cloud Storage bucket for object/blob storage when needed.
- Workload Identity for `witself-server`.
- Network and firewall prerequisites.
- Optional Cloud DNS and certificate integration.
- Optional Cloud KMS key for data-at-rest or optional field-level encryption of
  `sensitive` facts.
- Outputs for the Helm chart, such as service account annotations, database
  secret reference, bucket name, public URL, and the optional KMS key ID.

## Azure Target

The Azure module is a planned follow-up target. It should eventually support:

- AKS cluster or integration with an existing AKS cluster.
- Azure Database for PostgreSQL with the `pgvector` extension enabled.
- Azure Blob Storage for object/blob storage when needed.
- Azure Workload Identity for `witself-server`.
- Network security groups and private networking prerequisites.
- Optional Azure DNS and certificate integration.
- Optional Azure Key Vault key for data-at-rest or optional field-level
  encryption of `sensitive` facts.
- Outputs for the Helm chart, such as service account annotations, database
  secret reference, storage account/container name, public URL, and the
  optional Key Vault key reference.

## Messaging Networking

Inter-agent messaging is fully in scope for v0 and is served by
`witself-server` over the same `/v1` API, so v0 needs no special network
substrate beyond the cluster and database. Terraform should still leave room for
messaging-driven networking:

- The mailbox/queue model is backed by Postgres in v0; the AWS module's database
  and cluster networking already cover it.
- If a post-v0 transport adds dedicated delivery paths (a broker, a streaming
  backend, or direct cross-pod fan-out for group messages), the module should
  expose security-group/firewall and subnet inputs to allow that traffic without
  re-architecting the stack.
- Keep messaging egress and ingress controllable through network policy and
  cloud firewall inputs so operators can constrain cross-agent message flow.

The messaging model is tracked in
[inter-agent-messaging.md](inter-agent-messaging.md).

## Self-Hosted Vs Witself Cloud

Self-hosted stacks and Witself Cloud stacks should use the same public modules
where practical.

Differences:

- Self-hosted stacks should be examples and reference deployments that operators
  can copy or adapt.
- Witself Cloud stacks may describe the public infrastructure shape, but real
  state backends, credentials, environment-specific variables, production
  account IDs, and sensitive topology values must live outside the public repo.
- Managed Witself Cloud may use stricter defaults, additional observability,
  abuse controls, deployment pipelines, or private environment overlays.

The public repo should show enough infrastructure code for reviewers to
understand the security posture without exposing live credentials or state.

## Helm Integration

Terraform should output the values or references needed by the Helm chart.

Example output categories:

- Kubernetes namespace.
- Service account name and annotations.
- Database connection secret name and key.
- Confirmation that the database has the `pgvector` extension and the expected
  vector dimensionality.
- Object/blob storage bucket/container.
- Public URL or ingress host.
- Required cloud identity metadata.
- Optional KMS provider and key ID when data-at-rest or field-level encryption
  is enabled.
- Optional Prometheus, ServiceMonitor, PodMonitor, or managed monitoring
  integration references when the platform provides them.

Terraform should not render raw credential values into Helm values files. It
should create or reference database credentials, embedding-provider API keys,
and any other secrets through deployment-native mechanisms such as:

- Existing Kubernetes Secrets.
- External Secrets Operator.
- Secret Store CSI driver.
- Cloud workload identity.
- Cloud secret managers.

The embedding provider (`voyage` by default, `openai`, or `local-dev`) is a
configurable production dependency; its API key is a deployment-native secret,
never a Terraform-rendered Helm value.

## State And Secret Policy

Terraform state can contain sensitive values. The public repo must not include:

- Local state files.
- State backend credentials.
- Real `.tfvars` files with customer or production data.
- Database passwords.
- Cloud access keys.
- Private keys.
- Raw Witself tokens.
- Embedding-provider API keys.
- Payment provider credentials.
- Wallet credentials.
- Optional KMS credentials when KMS is enabled.

The repo should include `.gitignore` rules, examples, and validation checks that
make accidental state or secret commits difficult.

## CI Requirements

Required checks once Terraform exists:

- `terraform fmt -check -recursive infra/terraform`.
- `terraform init -backend=false` for modules and examples where practical.
- `terraform validate` for modules and examples.
- `tflint` for provider-specific linting.
- Static security checks such as `checkov` or equivalent.
- Secret scanning for Terraform examples.
- Documentation checks for required inputs and outputs.

Provider credentials should not be required for ordinary CI validation.

## Consumption

Initial module consumption can use Git sources pinned to release tags:

```hcl
module "witself_aws" {
  source = "git::https://github.com/witwave-ai/witself.git//infra/terraform/modules/aws?ref=v0.1.0"
}
```

Separate Terraform Registry modules can be considered later if the module
surface becomes stable enough to deserve independent versioning.

## Non-Goals

- Do not make Terraform deploy application pods directly when Helm should own
  the application deployment.
- Do not commit real Terraform state or production `.tfvars`.
- Do not require Terraform for every self-hosted user. Operators with existing
  clusters and managed services should be able to use only the Helm chart.
- Do not hide managed Witself Cloud production secrets or state in the public
  repo.
- Do not treat KMS as a required pillar. Witself protects the integrity and
  authenticity of identity data, not the confidentiality of secret material, so
  KMS provisioning stays optional.

## Related Docs

- [helm-chart.md](helm-chart.md)
- [self-hosting.md](self-hosting.md)
- [backend-architecture.md](backend-architecture.md)
- [api-contract.md](api-contract.md)
- [observability-and-operations.md](observability-and-operations.md)
- [storage.md](storage.md)
- [inter-agent-messaging.md](inter-agent-messaging.md)
- [cloud-targets.md](cloud-targets.md)
- [release-and-build.md](release-and-build.md)
- [requirements.md](requirements.md)
- [implementation-plan.md](implementation-plan.md)
- [threat-model.md](threat-model.md)
```
