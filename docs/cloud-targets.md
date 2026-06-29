# Witself Cloud Targets

Status: draft. Decision: AWS is the first implementation target for managed
Witself Cloud and the first self-hosted Terraform stack.

## Decision

Witself should implement AWS first.

AWS is first for:

- Managed Witself Cloud infrastructure.
- The first self-hosted Terraform module and stack.
- The first production Postgres-with-pgvector integration.
- The first production KMS integration for the sealed plane.
- The first production-shaped Helm values example.
- The first CI or smoke environment that exercises cloud-shaped infrastructure.

GCP and Azure remain planned provider targets. Their directories, docs, and
interfaces should exist early enough to avoid AWS-only assumptions, but their
full implementations should follow AWS.

## Why AWS First

- It keeps the first production backend path focused.
- It gives one concrete cloud to harden before multiplying provider behavior.
- It lines up with the first production storage path: managed Postgres with the
  pgvector extension for memory embeddings.
- It lines up with the AWS KMS decision for the sealed plane: AWS KMS is the
  first key-management provider, with `gcp-kms` and `azure-key-vault` planned
  (see [storage.md](storage.md), [key-hierarchy.md](key-hierarchy.md)).
- It still leaves the public repo structured for GCP and Azure from the start.

## Terraform Priority

Initial Terraform priority:

1. `infra/terraform/modules/aws`
2. `infra/terraform/stacks/self-hosted/aws`
3. `infra/terraform/stacks/witself-cloud/aws`
4. Skeleton or placeholder structure for GCP and Azure.
5. GCP implementation.
6. Azure implementation.

The AWS module should support:

- EKS or integration with an existing EKS cluster.
- RDS/Aurora PostgreSQL with the pgvector extension available for memory
  embeddings.
- AWS KMS for the sealed plane (secrets and TOTP): the customer master key
  backing the `CMK → per-realm KEK → per-secret/field DEK` envelope hierarchy.
  KMS is required only when the sealed plane is enabled; an open-plane-only
  deployment (memories and facts) does not need it (see
  [encryption-model.md](encryption-model.md), [key-hierarchy.md](key-hierarchy.md)).
- S3 for object/blob storage (identity exports, diagnostic bundles, support
  attachments, backups, and encrypted-only secret backups) when needed.
  Plaintext identity exports use S3; sealed-plane secret values are never
  written in plaintext — secret backup is encrypted-only (envelope plus KMS
  key identity).
- IAM roles for service accounts.
- Networking and security group prerequisites.
- Optional managed Prometheus or platform monitoring integration points.
- Optional Route 53 and ACM integration.
- Outputs consumed by the Helm chart.

## pgvector Requirement

The AWS Postgres target must support the pgvector extension. Semantic recall is
the core Witself differentiator, and memory embeddings are stored in Postgres via
pgvector (see [storage.md](storage.md)). The provisioned RDS/Aurora PostgreSQL
must be a version and configuration that can create and use the `vector`
extension, and the module should surface this as an explicit prerequisite rather
than an implicit assumption.

## KMS Requirement for the Sealed Plane

The sealed plane (secrets and TOTP) is protected by KMS-backed envelope
encryption: a customer master key (CMK) wraps a per-realm KEK, which wraps a
per-secret/field DEK (see [key-hierarchy.md](key-hierarchy.md)). The AWS target
provisions AWS KMS as the first key-management provider; `gcp-kms` and
`azure-key-vault` are planned and follow AWS.

- KMS is a hard dependency only when the sealed plane is enabled. An
  open-plane-only deployment (memories and facts) requires Postgres-with-pgvector
  but not KMS. Readiness gates on KMS only when the sealed plane is enabled;
  pgvector stays a hard gate for the open plane.
- The AWS module surfaces the KMS provider, key id, and IAM grants as explicit
  prerequisites (`witself-server` reads `WITSELF_KMS_PROVIDER` and
  `WITSELF_KMS_KEY_ID`; see [storage.md](storage.md)).
- KMS-loss is unrecoverable for sealed values by design (crypto-shred): without
  the CMK, secret and TOTP values cannot be decrypted. This does not affect the
  open plane, whose data is plaintext at rest in Postgres.
- Sealed-plane carve-outs hold regardless of cloud target: secret and TOTP
  values are never embedded, never returned by semantic recall, never in the
  self-digest, and never written to plaintext export. Only the explicit,
  reveal-gated, audited operations return sealed values.

## Embedding Provider Is Not Cloud Substrate

The embedding provider (`voyage` by default, `openai`, or `local-dev`) is an
external SaaS dependency, not cloud substrate. It is reached over the network as a
configurable provider behind a capability boundary, not provisioned by Terraform
alongside EKS, RDS, and S3.

- Cloud targets provision compute, Postgres-with-pgvector, object/blob storage,
  identity, and networking. They do not provision the embedding provider.
- Provider credentials are supplied to `witself-server` as configuration, the same
  way across managed, self-hosted, and local deployments.
- `local-dev` is the offline fallback embedder for tests, demos, and
  `witself-server serve --dev`; it requires no external SaaS dependency and lets
  semantic recall be exercised without a paid provider.
- If the external provider is unavailable, recall degrades deterministically to
  keyword/tag/kind/time ranking and the capability contract reports the degraded
  state. Cloud-target choice does not change this behavior.

## Multi-Cloud Cells

Cloud targets are not a single-cloud choice made once. Witself deploys as a fleet
of independent cells, where a cell is one complete, isolated Witself stack in a
single cloud account and region (see [deployment-cells.md](deployment-cells.md)).
The fleet spans AWS, GCP, and Azure, across multiple accounts per cloud.

- **A second AWS account is just another cell.** An independent second AWS account
  is not a special case — it is simply another cell. The same applies to a second
  GCP project or Azure subscription. Each cell is one cloud account/region.
- **AWS-first applies per cell.** The AWS-first ordering above is about which
  provider implementation hardens first, not about how many cells exist. Each cell
  is provisioned from the per-cloud Terraform modules and stacks listed here; a
  cell is one instantiation of a stack (see
  [terraform-infrastructure.md](terraform-infrastructure.md)). AWS cells come
  first because the AWS module hardens first; GCP and Azure cells follow as those
  provider implementations land.
- **Placement is by region and data-residency.** A thin global control plane picks
  the home cell for a new tenant by region / data-residency requirement, capacity
  across the fleet, and provider/account preference. The cloud target a tenant
  lands on is a placement outcome, not a global setting.
- **A fleet of independent cells, not a shared substrate.** There is no shared data
  store spanning cells and no shared-data multi-master across clouds. Each cell
  holds the full data and key material for its own tenants; a cell outage affects
  only the tenants homed on that cell. Moving a tenant between clouds or accounts is
  a deliberate, bounded migration (with a sealed-plane KMS re-wrap under the
  destination cell), not continuous cross-cloud replication — see
  [deployment-cells.md](deployment-cells.md).

This is why the provider-neutral contracts below matter: they let any cloud target
serve as a cell without AWS-only assumptions leaking into the fleet.

## Provider Portability

AWS-first should not mean AWS-only.

Shared contracts should stay provider-neutral:

- CLI behavior.
- API contracts.
- JSON contracts.
- Memory, fact, policy, group, and message model.
- Secret, TOTP, grant, and capability model (sealed plane).
- Embedding-provider interface.
- KMS provider interface.
- Object/blob storage interface.
- Helm chart values shape.
- Observability and health probe semantics.
- Terraform stack conventions.

Provider-specific behavior should live behind storage, KMS, object/blob,
identity, and Terraform boundaries.

## Related Docs

- [requirements.md](requirements.md)
- [deployment-cells.md](deployment-cells.md)
- [storage.md](storage.md)
- [encryption-model.md](encryption-model.md)
- [key-hierarchy.md](key-hierarchy.md)
- [terraform-infrastructure.md](terraform-infrastructure.md)
- [self-hosting.md](self-hosting.md)
- [helm-chart.md](helm-chart.md)
- [observability-and-operations.md](observability-and-operations.md)
- [implementation-plan.md](implementation-plan.md)
