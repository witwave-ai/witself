# Witself Terraform Infrastructure

Status: draft. This document describes where Terraform should live in the
Witself repository and what it should provision for managed and self-hosted
deployments.

Narrative-memory amendment (accepted 2026-07-14): infrastructure provisions no
memory inference or embedding-provider service/credential. The deterministic
PostgreSQL-backed target is specified by
[narrative-memory-and-curation.md](narrative-memory-and-curation.md).

Sealed-plane custody amendment (accepted 2026-07-18):
[ADR 0003](decisions/0003-client-custodied-agent-vault.md) and the
[client-custodied vault contract](client-custodied-agent-vault.md) define
client-side custody for agent secrets. The backend holds no AVK key material,
calls no KMS for agent secrets, and never decrypts secret values.
Ordinary infrastructure KMS and
storage-encryption references are unaffected.

## Decision

Witself should include public Terraform under `infra/terraform` for AWS, GCP,
and Azure infrastructure.

Terraform owns cloud substrate. Helm owns application deployment.

Terraform should provision Kubernetes, ordinary PostgreSQL, object/blob
storage, workload identity, networking, and the
Kubernetes integration points that the Helm chart
needs. The Helm chart should deploy `witself-server` onto that substrate and wire
probes, metrics, and optional Prometheus Operator resources.

Witself has two planes. The open plane (memories and facts) is ordinary
data-at-rest in Postgres. In the sealed plane (secrets and TOTP), the active
client encrypts and decrypts sensitive values with per-field DEKs wrapped by
its agent vault key (AVK). Terraform provisions no agent-vault key or server
permission to unwrap those keys. The backend stores ciphertext, wrapped DEKs,
and public vault metadata. Secret values are never embedded, recalled, placed
in the self-digest, or plaintext-exported. See
[encryption-model.md](encryption-model.md) and [key-hierarchy.md](key-hierarchy.md).

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
identifiers, production credentials, and
environment-specific `.tfvars` files must not be committed.

## AWS Target

The AWS module is the first implementation target. It should support:

- EKS cluster or integration with an existing EKS cluster.
- RDS/Aurora PostgreSQL with full-text and JSONB support. Migration `0032`
  stores optional client-supplied vectors as portable JSONB, so no extension is
  required for lexical or deterministic hybrid recall.
- S3 bucket for object/blob storage when needed (large exports, diagnostic
  bundles, support attachments, backup artifacts).
- IAM roles for service accounts (IRSA) for `witself-server` workload identity.
- Agent secrets use client-held AVKs; no application KMS key or unwrap IAM
  grant is required.

- Security groups and network policy prerequisites.
- Optional Route 53 and ACM integration.
- Networking inputs sized for inter-agent messaging if a future transport needs
  cross-pod or cross-AZ delivery paths (see [Messaging
  Networking](#messaging-networking)).
- Outputs for the Helm chart, such as service account annotations, database
  secret reference, bucket name, and public URL.

### Optional ANN projection

No vector extension is part of the current infrastructure contract. The same
migration `0032` and JSONB hybrid-recall behavior runs on AWS, GCP, Azure, and
ordinary self-hosted PostgreSQL. A future pgvector/ANN projection may be offered
as an explicit optional accelerator. If added, Terraform may provision extension
support and report its state, but the chart, readiness, capabilities, archive,
and correctness tests must continue to work without it.

### Sealed-plane custody

The sealed plane requires no Terraform key-management module. Authorized
clients hold the AVK and wrap per-field DEKs locally; the server stores only
the resulting encrypted material and public metadata. The server has no
sealed-plane KMS provider setting or KMS readiness dependency.

AVK enrollment, recovery, and rotation are client operations. Infrastructure
provisioning does not recover a lost AVK or grant access to sensitive values.
See [key-hierarchy.md](key-hierarchy.md),
[encryption-model.md](encryption-model.md), and [storage.md](storage.md).

## GCP Target

The GCP module is a planned follow-up target. It should eventually support:

- GKE cluster or integration with an existing GKE cluster.
- Cloud SQL for PostgreSQL with full-text and JSONB support; no vector extension
  is required.
- Cloud Storage bucket for object/blob storage when needed.
- Workload Identity for `witself-server`.
- Agent secrets retain the same client-held AVK custody as every other cloud.

- Network and firewall prerequisites.
- Optional Cloud DNS and certificate integration.
- Outputs for the Helm chart, such as service account annotations, database
  secret reference, bucket name, and public URL.

## Azure Target

The Azure module is a planned follow-up target. It should eventually support:

- AKS cluster or integration with an existing AKS cluster.
- Azure Database for PostgreSQL with full-text and JSONB support; no vector
  extension is required.
- Azure Blob Storage for object/blob storage when needed.
- Azure Workload Identity for `witself-server`.
- Agent secrets retain the same client-held AVK custody as every other cloud.

- Network security groups and private networking prerequisites.
- Optional Azure DNS and certificate integration.
- Outputs for the Helm chart, such as service account annotations, database
  secret reference, storage account/container name, and public URL.

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

## Deployment Cells And The Control Plane

Witself's go-forward topology is a fleet of independent cells under a thin global
control plane (see [deployment-cells.md](deployment-cells.md)). Terraform is how a
cell is built. A **cell** is one complete, isolated Witself stack in a single cloud
account/region: one instantiation of a `stacks/` composition over the existing
per-provider `modules/` — `witself-server`, ordinary PostgreSQL, and object/blob
storage. The repository layout above
does not change for cells; a cell is one stack instance, and the fleet is many such
instances stood up across multiple accounts and regions.

Multi-account is the normal case, not a special one. An independent second AWS
account in the same region is simply another cell — another instantiation of the AWS
stack against that account's provider configuration. The same holds for additional
GCP projects and Azure subscriptions. Each cell keeps its own state and credentials;
cells share nothing at the data layer (see [State And Secret
Policy](#state-and-secret-policy) — per-cell state and credential isolation is a hard
requirement, never a shared backend across cells).

Alongside the per-cell stacks, the fleet adds one thin global **control-plane**
component. It is the only always-on global piece and it holds routing metadata only —
the realm/account -> home-cell + endpoint + signing-key mapping — never tenant
memories, facts, secrets, or messages. It can be modeled as its own small stack
(globally replicated, HA) distinct from the per-cell stacks; keeping it thin and
metadata-only keeps its blast radius tiny. New cells register with the control plane;
clients resolve their home cell through it and then talk directly to that cell (see
[deployment-cells.md](deployment-cells.md)).

Tenant migration between cells copies sealed ciphertext, wrapped DEKs, and
public vault metadata unchanged. The matching AVK remains with the authorized
client; neither cell unwraps keys or receives plaintext. Terraform provisions
the destination storage and identity, without an agent-vault KMS dependency.
The open plane moves via the existing first-class export/import. See
[key-hierarchy.md](key-hierarchy.md) and [storage.md](storage.md).

Open decisions (tracked in [deployment-cells.md](deployment-cells.md), not resolved
here): whether the placement/migration unit is the account or the realm, and whether a
self-host deployment is always a single cell or may itself be a multi-cell fleet with
its own control plane.

## Helm Integration

Terraform should output the values or references needed by the Helm chart.

Example output categories:

- Kubernetes namespace.
- Service account name and annotations.
- Database connection secret name and key.
- Confirmation that PostgreSQL meets the migration/full-text/JSONB baseline.
- Object/blob storage bucket/container.
- Public URL or ingress host.
- Required cloud identity metadata.
- Optional Prometheus, ServiceMonitor, PodMonitor, or managed monitoring
  integration references when the platform provides them.

Terraform should not render raw credential values into Helm values files. It
should create or reference database, KMS, and other infrastructure credentials
through deployment-native mechanisms such as:

- Existing Kubernetes Secrets.
- External Secrets Operator.
- Secret Store CSI driver.
- Cloud workload identity.
- Cloud secret managers.

There is no backend embedding provider or model API key. Clients may submit
vectors under immutable profiles; Terraform provisions only ordinary PostgreSQL
storage for the canonical JSONB rows. Any future optional ANN projection must
remain an accelerator rather than a production dependency.

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
- Agent vault keys and unwrapped sealed-plane data keys.

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
- Do not require application KMS configuration for either plane. Agent-secret
  encryption and decryption belong to the active client.
- Do not grant the `witself-server` deployment identity KMS administration
  (key deletion, key-policy edits). Key administration stays with operators;
  agent-secret data keys are wrapped and unwrapped only by the active client.

## Related Docs

- [helm-chart.md](helm-chart.md)
- [self-hosting.md](self-hosting.md)
- [backend-architecture.md](backend-architecture.md)
- [api-contract.md](api-contract.md)
- [observability-and-operations.md](observability-and-operations.md)
- [storage.md](storage.md)
- [encryption-model.md](encryption-model.md)
- [key-hierarchy.md](key-hierarchy.md)
- [inter-agent-messaging.md](inter-agent-messaging.md)
- [cloud-targets.md](cloud-targets.md)
- [deployment-cells.md](deployment-cells.md)
- [release-and-build.md](release-and-build.md)
- [requirements.md](requirements.md)
- [implementation-plan.md](implementation-plan.md)
- [threat-model.md](threat-model.md)
```
