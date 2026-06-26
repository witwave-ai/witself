# Witself Scaffold Readiness

Status: draft. Decision: the v0 product docs are ready to freeze for initial
repo scaffolding.

## Decision

The docs are sufficient to start scaffolding the repository. Future product
questions should be captured as issues or follow-up docs unless they block the
first implementation slice.

Witself reuses the Witpass platform spine — one core domain service behind thin
CLI/MCP/API adapters, the same tenancy and token model, the same
observability/release/billing apparatus — but swaps the secret payload for
self-identity. The scaffold therefore mirrors the Witpass repository shape, with
the realm/agent/memory/fact/policy/group/message domain replacing the
vault/agent/secret domain, a pgvector + embeddings-provider package added for
semantic recall, a dedicated policy-engine package added for cross-agent access,
and the crypto package demoted to token hashing and transport only.

The initial scaffold should create:

- Go module `github.com/witwave-ai/witself`.
- `cmd/witself`.
- `cmd/witself-server`.
- Internal package layout for shared core, CLI adapter, MCP adapter, API
  adapter, storage adapters, embeddings/pgvector, policy engine, audit,
  observability, token-hashing/transport crypto, and JSON contracts.
- `.gitignore`.
- `.github/workflows/ci.yml`.
- `.github/workflows/release.yml`.
- `images/witself/Dockerfile`.
- `images/witself-server/Dockerfile`.
- `charts/witself`.
- `infra/terraform/modules/aws`.
- `infra/terraform/modules/gcp`.
- `infra/terraform/modules/azure`.
- `infra/terraform/stacks/self-hosted/aws`.
- `infra/terraform/stacks/self-hosted/gcp`.
- `infra/terraform/stacks/self-hosted/azure`.
- `infra/terraform/stacks/witself-cloud/aws`.
- Initial docs, lint, build, and release smoke checks.

## Module And Binaries

- Single Go module `github.com/witwave-ai/witself`, `go 1.26`, toolchain
  `go1.26.4`, refreshed before the first implementation pass.
- `cmd/witself` builds the `witself` CLI and `witself mcp serve`. There is no
  `server` subcommand on the main CLI.
- `cmd/witself-server` builds the separate backend API binary, including its
  `migrate` and `serve --dev` subcommands.
- Both binaries and the shared core build from the same module so CLI, MCP, and
  API behavior do not drift.

## Package Layout

Starting internal layout. Names will evolve during implementation, but the
boundaries are frozen:

```text
cmd/witself/                  # CLI, including mcp serve
cmd/witself-server/           # Backend API server (serve, migrate, serve --dev)
internal/core/                # Domain service and use cases (memory/fact/policy/group/message)
internal/api/                 # HTTP handlers and request/response adapters
internal/mcp/                 # MCP stdio adapter over the core service
internal/auth/                # Token validation and principal (realm+agent) resolution
internal/policy/              # Policy engine: default-deny evaluation, verbs, policy test
internal/embeddings/          # Embedding-provider abstraction (voyage/openai/local-dev)
internal/recall/              # Semantic recall: hybrid ranking and degradation
internal/audit/               # Audit event generation and sinks
internal/observability/       # Metrics, logs, request IDs, and health probes
internal/crypto/              # Token hashing and transport only (no envelope/KMS pillar)
internal/store/               # Storage interfaces
internal/store/local/         # Local development adapter (file-backed, local-dev embedder)
internal/store/postgres/      # Production relational adapter, including pgvector
internal/store/blob/          # Object/blob storage adapter for exports and bundles
internal/server/              # Server config, lifecycle, health, migrations
```

This layout is a starting point, not a promise that every package name is final.

Notes on the domain-specific packages:

- `internal/policy/` is a first-class package, not a thin grant table. It owns
  the evaluable Policy objects (subject × permission × target × scope), the
  default-deny stance, the escalating verbs (`read`, `contribute`, `curate`,
  `forget`), and the `policy test` decision path. It reuses the authorization and
  audit scaffolding that Witpass kept in its encryption-model package; Witself
  has no encryption-model package. Tracked in
  [access-policy.md](access-policy.md).
- `internal/embeddings/` is the embedding-provider abstraction, the structural
  mirror of how Witpass abstracted KMS. It carries the `voyage` (default),
  `openai`, and `local-dev` providers behind a capability boundary and reports
  active provider, model, and vector dimensionality. Tracked in
  [memory-model.md](memory-model.md) and [storage.md](storage.md).
- `internal/recall/` owns semantic-by-default recall: vector similarity blended
  with keyword, tag, kind, and time filters, hybrid ranking, and deterministic
  degradation to keyword/tag/time when the provider is unavailable.
- `internal/store/postgres/` holds the pgvector integration. Identity records
  and their embedding vectors live in Postgres; migrations run through
  `witself-server migrate` (Goose, advisory lock). Tracked in
  [storage.md](storage.md).
- `internal/crypto/` is demoted to token hashing and transport concerns only.
  There is no envelope handling, no KMS-as-pillar, and no reveal machinery.
  Optional field-level encryption of `sensitive` facts is a capability, not a
  core dependency; see [storage.md](storage.md).

## CI And Release Workflows

- `.github/workflows/ci.yml` runs on pull requests and pushes to `main`:
  root docs validation, `gofmt`, `go mod tidy`/`go mod verify` cleanliness, build,
  race tests on Linux, `go vet`, `golangci-lint`, `govulncheck`, markdownlint,
  shellcheck, `actionlint`, `hadolint`, image build smoke tests for `images/*`,
  Helm lint/render/schema-validate for `charts/*`, and Terraform
  fmt/validate/lint/security for `infra/terraform`.
- `.github/workflows/release.yml` triggers on `v*` tags with
  `workflow_dispatch` dry runs, builds public archives, checksums, signatures,
  SBOMs, provenance, the Homebrew formula update, and the public images, verifies
  the `witwave-ai/homebrew-tap` repository, and smoke tests artifacts.
- CI uses concurrency cancellation and minimal permissions; only publishing jobs
  get `packages: write`. Tracked in [release-and-build.md](release-and-build.md).

## Images

- `images/witself/Dockerfile` → `ghcr.io/witwave-ai/images/witself`. Entrypoint
  is the `witself` binary so the image runs both CLI commands and
  `witself mcp serve`.
- `images/witself-server/Dockerfile` →
  `ghcr.io/witwave-ai/images/witself-server`. Entrypoint is the separate
  `witself-server` process.
- `linux/amd64` and `linux/arm64`, non-root where practical, signed, with SBOM
  and provenance.

## Helm Chart

- `charts/witself` → `ghcr.io/witwave-ai/charts/witself`. Deploys
  `witself-server`, not the CLI.
- External Postgres with pgvector is the production default. External
  embedding-provider configuration and object/blob storage configuration are
  first-class values; values reference existing Secrets rather than embedding raw
  secrets.
- Service, Deployment, ServiceAccount, ConfigMap, optional Ingress, optional
  NetworkPolicy, and a migration Job, with separate named ports for API
  (`:8080`), health (`:8081`), and metrics (`:9090`). Tracked in
  [helm-chart.md](helm-chart.md).

## Terraform

- `infra/terraform` with `modules/{aws,gcp,azure}` and
  `stacks/self-hosted/{aws,gcp,azure}`, plus
  `stacks/witself-cloud/aws` for managed examples.
- Provisions cloud substrate (including Postgres with pgvector) and outputs the
  references the Helm chart needs. AWS is implemented first; GCP and Azure are
  visible but follow. No state, credentials, real `.tfvars`, database passwords,
  embedding-provider credentials, or raw tokens are committed. Tracked in
  [terraform-infrastructure.md](terraform-infrastructure.md).

## Freeze Boundary

Frozen enough for scaffolding:

- Product name and public repo posture.
- Realm/agent/memory/fact/policy/group/message model.
- Semantic-by-default recall with an embedding-provider abstraction.
- Cross-agent access via an evaluable default-deny policy engine.
- Security groups as policy subjects and targets, with group-scoped records.
- Full inter-agent messaging in v0 (durable mailbox, delivery, ordering, ack).
- CLI-first setup and account management.
- Managed-default setup target.
- Token lifecycle and token-file handling (token identity bound to realm+agent).
- First-class structured/plaintext identity export and round-trippable import.
- MCP stdio as the v0 MCP target.
- Backend API and route style (`/v1`, plural resources, colon actions).
- Prometheus metrics, Kubernetes health probes, and structured server logs.
- Postgres with pgvector as the first production storage path; AWS first.
- Helm under `charts/*`.
- Terraform under `infra/terraform`.
- Strong CI and release action from the beginning.
- Post-v0 deferral list.

Still expected to evolve during implementation:

- Exact Go package names.
- Exact policy-evaluation and recall-ranking structs.
- Exact embedding-provider interface and vector dimensionality handling.
- Exact OpenAPI generation approach.
- Exact Helm values schema.
- Exact Terraform variables and outputs.
- Exact CI tool versions.

## Related Docs

- [v0-scope.md](v0-scope.md)
- [implementation-plan.md](implementation-plan.md)
- [release-and-build.md](release-and-build.md)
- [requirements.md](requirements.md)
- [backend-architecture.md](backend-architecture.md)
- [memory-model.md](memory-model.md)
- [access-policy.md](access-policy.md)
- [storage.md](storage.md)
- [observability-and-operations.md](observability-and-operations.md)
