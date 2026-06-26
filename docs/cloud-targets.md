# Witself Cloud Targets

Status: draft. Decision: AWS is the first implementation target for managed
Witself Cloud and the first self-hosted Terraform stack.

## Decision

Witself should implement AWS first.

AWS is first for:

- Managed Witself Cloud infrastructure.
- The first self-hosted Terraform module and stack.
- The first production Postgres-with-pgvector integration.
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
- S3 for object/blob storage (identity exports, diagnostic bundles, support
  attachments, backups) when needed.
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

## Provider Portability

AWS-first should not mean AWS-only.

Shared contracts should stay provider-neutral:

- CLI behavior.
- API contracts.
- JSON contracts.
- Memory, fact, policy, group, and message model.
- Embedding-provider interface.
- Object/blob storage interface.
- Helm chart values shape.
- Observability and health probe semantics.
- Terraform stack conventions.

Provider-specific behavior should live behind storage, object/blob, identity, and
Terraform boundaries.

## Related Docs

- [requirements.md](requirements.md)
- [storage.md](storage.md)
- [terraform-infrastructure.md](terraform-infrastructure.md)
- [self-hosting.md](self-hosting.md)
- [helm-chart.md](helm-chart.md)
- [observability-and-operations.md](observability-and-operations.md)
- [implementation-plan.md](implementation-plan.md)
