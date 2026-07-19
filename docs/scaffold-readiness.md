# Witself Scaffold Readiness

Status: draft. Decision: the v0 product docs are ready to freeze for initial
repo scaffolding.

Narrative-memory amendment (accepted 2026-07-14): there is no server-side
embedding-provider scaffold. The memory slice starts with PostgreSQL
versions/evidence and lexical recall, then may accept optional client-supplied
vectors under
[narrative-memory-and-curation.md](narrative-memory-and-curation.md).

Sealed-plane custody amendment (accepted 2026-07-18):
[ADR 0003](decisions/0003-client-custodied-agent-vault.md) and the
[client-custodied vault contract](client-custodied-agent-vault.md) supersede
KMS-rooted agent-secret, realm-KEK, and server-side-decrypt language below. The
backend holds no AVK key material, calls no KMS for agent secrets, and exposes
no decrypt or `server_side_decrypt` path. Ordinary infrastructure KMS and
storage-encryption references are unaffected.

## Decision

The docs are sufficient to start scaffolding the repository. Future product
questions should be captured as issues or follow-up docs unless they block the
first implementation slice.

Witself reuses the Witpass platform spine — one core domain service behind thin
CLI/MCP/API adapters, the same tenancy and token model, the same
observability/release/billing apparatus — and now carries **both planes** in one
product: the **open plane** (memories + facts) and the **sealed plane** (secrets
+ TOTP). The scaffold therefore mirrors the Witpass repository shape, with the
realm/agent/memory/fact/policy/group/message domain joined by the
secret/TOTP/grant domain, PostgreSQL lexical recall plus an optional
client-vector index boundary, a dedicated policy-engine package added for
cross-agent identity access, and the crypto + KMS-provider packages re-added for the
sealed-plane envelope (CMK → per-realm KEK → per-secret/field DEK; see
[key-hierarchy.md](key-hierarchy.md) and [encryption-model.md](encryption-model.md)).
The sealed plane is reveal-gated and is never embedded, never recalled, never in
the self-digest, and never plaintext-exported; KMS is a required dependency only
when the sealed plane is enabled.

The initial scaffold should create:

- Go module `github.com/witwave-ai/witself`.
- `cmd/witself`.
- `cmd/witself-server`.
- Internal package layout for shared core, CLI adapter, MCP adapter, API
  adapter, storage adapters, lexical recall/optional client vectors, policy
  engine, sealed-plane envelope crypto, KMS provider abstraction, audit,
  observability, and JSON
  contracts.
- `.gitignore`.
- `.github/workflows/ci.yml`.
- `.github/workflows/release.yml`.
- `images/witself/Dockerfile`.
- `images/witself-server/Dockerfile`.
- `charts/witself-server`.
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
  `go1.26.5`, refreshed before the first implementation pass.
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
cmd/ws/                  # CLI, including mcp serve
cmd/witself-server/           # Backend API server (serve, migrate, serve --dev)
internal/core/                # Domain service and use cases (memory/fact/policy/group/message + secret/totp/grant)
internal/api/                 # HTTP handlers and request/response adapters
internal/mcp/                 # MCP stdio adapter over the core service
internal/auth/                # Token validation and principal (realm+agent) resolution
internal/policy/              # Policy engine: default-deny evaluation, verbs, policy test
internal/recall/              # Open plane: PostgreSQL lexical ranking; optional client-vector math
internal/audit/               # Audit event generation and sinks
internal/observability/       # Metrics, logs, request IDs, and health probes
internal/crypto/              # Sealed plane: envelope (CMK->per-realm KEK->DEK), AEAD, reveal; plus token hashing/transport
internal/kms/                 # Sealed plane: KMS-provider abstraction (aws-kms/gcp-kms/azure-key-vault/local-dev)
internal/store/               # Storage interfaces
internal/store/local/         # Local development adapter (file-backed, lexical recall)
internal/store/postgres/      # Production relational adapter; FTS + JSONB vectors
internal/store/blob/          # Object/blob storage adapter for exports and bundles
internal/server/              # Server config, lifecycle, health, migrations
```

This layout is a starting point, not a promise that every package name is final.

### Post-v0 go-forward packages (collaboration + cells fast-follow)

The realm-local core above is frozen and unchanged. The cross-realm
collaboration substrate and the multi-cloud cells arrive as a **fast-follow**
after v0 and add packages alongside the frozen core — additive, not a refactor:

- `internal/relay/` — durable cross-realm message relay (the outbound-client
  transport behind `POST /v1/messages:listen` and cross-realm send; agents run
  no HTTP servers). Tracked in [agent-collaboration.md](agent-collaboration.md).
- `internal/federation/` — the deny-by-default peer allow-list / trust registry,
  realm-card signing and verification, and the `federation:manage` surface.
  Tracked in [agent-collaboration.md](agent-collaboration.md).
- `internal/conversation/` — the cross-realm conversation/task resource
  (`thr_`-prefixed conversations, participant set, turn/cost budgets, loop and
  flood governors). Tracked in [agent-collaboration.md](agent-collaboration.md).
- A thin **control-plane** component (a separate surface, not a per-cell `/v1`
  route) that holds the realm/account → home-cell mapping and resolves placement
  and migration. It carries routing metadata only — no tenant data. Tracked in
  [deployment-cells.md](deployment-cells.md).

Cells are **deployment-shaped, not a package boundary**: a cell is one stack
instance per cloud account/region (reusing the existing
`infra/terraform/modules/*` + `stacks/*` layout, see
[terraform-infrastructure.md](terraform-infrastructure.md)), and Witself Cloud
runs a fleet of independent cells fronted by the thin control plane. Adding a
cell (including a second account in the same cloud) provisions another stack
instance; the realm-local core packages are identical in every cell. This is
post-v0 go-forward and does not change the realm-local core freeze.

Notes on the domain-specific packages:

- `internal/policy/` is a first-class package, not a thin grant table. It owns
  the evaluable Policy objects (subject × permission × target × scope), the
  default-deny stance, the escalating verbs (`read`, `contribute`, `curate`,
  `forget`), and the `policy test` decision path. It governs the **open plane
  only** — cross-agent identity access to memories/facts. Sealed-plane secret
  access is not a policy verb; it uses grants plus realm roles (see
  [authorization-and-roles.md](authorization-and-roles.md)). Tracked in
  [access-policy.md](access-policy.md).
- `internal/recall/` owns deterministic PostgreSQL lexical recall across
  keyword, tag, kind, time, recency, and salience signals. A future optional
  profile boundary accepts finite memory and query vectors supplied by an
  authorized client; the backend validates profile/dimension compatibility and
  performs similarity math but never calls a model. **Sealed-plane carve-out:**
  secret values and TOTP seeds are never submitted for vector generation.
  Tracked in [memory-model.md](memory-model.md) and [storage.md](storage.md).
- `internal/store/postgres/` holds identity records, lexical indexes, and the
  implemented migration-0032 immutable profiles/JSONB vector rows. Bounded
  deterministic similarity uses ordinary PostgreSQL; a future pgvector/ANN
  projection may accelerate candidate generation;
  migrations run through `witself-server migrate` (Goose, advisory lock).
  Tracked in [storage.md](storage.md).
- `internal/crypto/` owns the sealed-plane envelope: the CMK → per-realm KEK →
  per-secret/field DEK hierarchy, AEAD seal/open (`XCHACHA20_POLY1305`,
  `AES_256_GCM`), AAD binding, DEK wrap/unwrap, and the reveal machinery for
  `secret reveal` / `totp code`, alongside its existing token hashing and
  transport concerns. The envelope is the sealed plane's confidentiality
  boundary; the open plane (memories/facts) uses ordinary data-at-rest, not this
  package. Tracked in [encryption-model.md](encryption-model.md) and
  [key-hierarchy.md](key-hierarchy.md).
- `internal/kms/` is the KMS-provider abstraction that roots the envelope: the
  `aws-kms`, `gcp-kms`, `azure-key-vault`, and `local-dev` providers behind a
  capability boundary, supporting client-side and server-side decrypt. It is
  required only when the sealed plane is enabled (an open-plane-only deployment
  runs without KMS). Unlike optional vector profiles, KMS is a real backend
  provider boundary because it protects sealed data. KMS loss crypto-shreds
  secret values without affecting the open plane. Tracked in
  [key-hierarchy.md](key-hierarchy.md) and [storage.md](storage.md).

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
  is the `ws` binary so the image runs both CLI commands and
  `witself mcp serve`.
- `images/witself-server/Dockerfile` →
  `ghcr.io/witwave-ai/images/witself-server`. Entrypoint is the separate
  `witself-server` process.
- `linux/amd64` and `linux/arm64`, non-root where practical, signed, with SBOM
  and provenance.

## Helm Chart

- `charts/witself-server` → `ghcr.io/witwave-ai/charts/witself-server`. Deploys
  `witself-server`, not the CLI.
- External PostgreSQL is the production default. Object/blob storage
  configuration is a first-class value; pgvector is not required. There is no backend
  model-provider credential or model-egress value in the chart.
- Service, Deployment, ServiceAccount, ConfigMap, optional Ingress, optional
  NetworkPolicy, and a migration Job, with separate named ports for API
  (`:8080`), health (`:8081`), and metrics (`:9090`). Tracked in
  [helm-chart.md](helm-chart.md).

## Terraform

- `infra/terraform` with `modules/{aws,gcp,azure}` and
  `stacks/self-hosted/{aws,gcp,azure}`, plus
  `stacks/witself-cloud/aws` for managed examples.
- Provisions cloud substrate (including PostgreSQL) and outputs the
  references the Helm chart needs. AWS is implemented first; GCP and Azure are
  visible but follow. No state, credentials, real `.tfvars`, database passwords,
  KMS credentials, or raw tokens are committed. There is no server model secret
  to provision. Tracked in
  [terraform-infrastructure.md](terraform-infrastructure.md).

## Freeze Boundary

Frozen enough for scaffolding:

- Product name and public repo posture.
- Realm/agent model spanning two planes: the open plane
  (memory/fact/policy/group/message) and the sealed plane
  (secret/TOTP/grant). Ownership is unified — `owner_kind ∈ {agent, group}`
  across memories, facts, and secrets.
- The two-plane Postgres data model is frozen for the first Goose migration,
  including the sealed-plane `secrets`, `secret_fields`, `secret_grants`,
  `totp_enrollments`, `realm_keys`, and `secret_deks` tables and their envelope
  columns (CMK → per-realm KEK → per-secret/field DEK). See
  [data-model.md](data-model.md).
- Sealed-plane invariants: secret values and TOTP seeds are reveal-gated and are
  never embedded, never recalled, never in the self-digest, and never
  plaintext-exported. KMS is required only when the sealed plane is enabled.
- PostgreSQL lexical recall as the always-available baseline, with no backend
  inference and implemented optional client-supplied vector profiles/JSONB rows.
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
- PostgreSQL as the first production storage path; AWS first. A future ANN
  projection is optional and never a gate for the open plane; AWS KMS is the first production
  key path for the sealed plane (see [storage.md](storage.md)).
- Helm under `charts/*`.
- Terraform under `infra/terraform`.
- Strong CI and release action from the beginning.
- Post-v0 deferral list.

The realm-local core freeze above stands as written. The collaboration
substrate and multi-cloud cells are **post-v0 go-forward**: they add the
`internal/relay`, `internal/federation`, and `internal/conversation` packages
plus a thin control-plane component, and they treat a cell as one stack instance
per cloud account/region. They extend the scaffold additively and do not reopen
the realm-local core freeze. See [agent-collaboration.md](agent-collaboration.md)
and [deployment-cells.md](deployment-cells.md).

Still expected to evolve during implementation:

- Exact Go package names.
- Exact policy-evaluation and recall-ranking structs.
- Exact optional client-vector profile, validation, and dimensionality handling.
- Exact cryptographic envelope structs and KMS-provider interface.
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
- [data-model.md](data-model.md)
- [encryption-model.md](encryption-model.md)
- [key-hierarchy.md](key-hierarchy.md)
- [authorization-and-roles.md](authorization-and-roles.md)
- [storage.md](storage.md)
- [observability-and-operations.md](observability-and-operations.md)
