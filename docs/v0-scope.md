# Witself V0 Scope

Status: draft. Decision: v0 is a usable cloud-shaped slice, not only a local
mock and not yet the full commercial managed service.

## Decision

V0 should prove the end-to-end product boundary:

- A human or AI agent can install `witself`.
- A human operator can authenticate through a CLI-initiated browser or
  device-code flow without giving the CLI a raw account password.
- An operator can bootstrap a realm and named agents.
- A named agent can authenticate from a token file or token environment.
- The CLI can add, adjust, read, recall, list, forget, restore, and delete an
  agent's own memories.
- The CLI can set, get, list, and delete an agent's own facts, and promote a
  fact to primary.
- `memory recall` performs semantic-by-default similarity search over the
  caller's accessible memories, blended with keyword, tag, kind, and time
  filters.
- The CLI can create, list, show, delete, and `test` declarative cross-agent
  access policies under a default-deny stance.
- The CLI can create, list, show, and manage membership of security groups,
  including group-scoped shared memories and facts.
- The CLI can send, list, read, and acknowledge durable inter-agent messages,
  with sender identity derived from the token.
- The CLI can export an agent's self as structured plaintext and import it back
  round-trippably.
- `witself mcp serve` can expose the safe v0 MCP stdio tool surface.
- `witself-server serve --dev` can exercise the same core through HTTP.
- `witself-server` exposes Prometheus metrics, structured logs, and Kubernetes
  health probes.
- The Postgres path with pgvector and an embedding provider exists early enough
  to keep the backend honest.
- Public release artifacts, images, Helm chart scaffolding, Terraform AWS
  scaffolding, CI, linting, and release automation exist from the beginning.

V0 should not wait for a polished web dashboard, live billing provider, live
support desk, crypto settlement, MCP network transport, or a managed web admin
console.

## Required V0 Product Capabilities

Core user-facing capabilities:

- `witself version`.
- `witself capabilities`.
- `witself whoami`.
- `witself setup`, defaulting to managed Witself Cloud.
- `witself setup --endpoint URL` for self-hosted, staging, private, or other
  explicit remote endpoints.
- `witself setup --local` for dev/test-only local mode.
- Idempotent setup for account, realm, and agent resources by name.
- Explicit setup token handling through `--reuse-existing-token` or
  `--rotate-existing-tokens` when token material already exists.
- Owner-only setup-created token files with refusal to overwrite existing token
  files unless token reuse or rotation was explicitly selected.
- Managed operator auth through browser/device-code flow, with self-hosted
  first-operator bootstrap through a one-time bootstrap token.
- Realm create/status for remote-shaped use.
- Local store initialization (`witself realm init`) for early development and
  tests.
- Agent create/show/list/rename/copy/disable/enable/delete where operator
  policy allows.
- Token create/list/show/rotate/revoke with raw token returned only once.
- Memory add/adjust/read/recall/list/forget/restore/delete for the caller's own
  memories, with soft-delete-by-default forget and a guarded hard delete.
- Fact set/get/list/delete with `--primary` promotion that demotes the prior
  primary of the same logical kind.
- Policy create/list/show/delete/test under default deny.
- Group create/list/show/add-member/remove-member/delete, including
  group-scoped shared memories and facts.
- Message send/list/read/ack over a durable mailbox with per-recipient ordering
  and acknowledgement.
- Identity reference parsing and resolution.
- `witself export` and `witself import` for round-trippable structured
  identity data.
- `witself mcp serve` over stdio.

Core backend capabilities:

- Shared Go core below CLI, MCP, API, and local development adapters.
- `witself-server serve --dev`.
- `/v1/version`, `/v1/health/live`, `/v1/health/ready`,
  `/v1/health/startup`, `/metrics`, `/v1/whoami`, and `/v1/capabilities`.
- Initial `/v1/realms`, `/v1/agents`, `/v1/tokens`, `/v1/memories`,
  `/v1/facts`, `/v1/policies`, `/v1/groups`, `/v1/messages`, and `/v1/audit`
  route groups.
- Colon action subroutes including `/v1/memories:recall`,
  `/v1/memories/{memory_id}:forget`, `/v1/memories/{memory_id}:restore`,
  `/v1/facts/{fact_id}:primary`, `/v1/policies:test`,
  `/v1/messages/{message_id}:ack`, and `/v1/tokens/{token_id}:rotate`.
- Prometheus metrics for HTTP traffic, auth, token operations, memory
  operations, recall and embedding operations, fact operations, policy
  decisions, cross-agent accesses, group operations, messaging, audit events,
  usage, limits, storage, vector storage, and migrations.
- Local development adapter behind the same backend interface.
- Postgres storage adapter with pgvector and Goose migrations.
- Embedding-provider abstraction with `voyage` as default and `openai` and
  `local-dev` selectable through `WITSELF_EMBEDDINGS_PROVIDER` /
  `WITSELF_EMBEDDINGS_MODEL`.
- Deterministic recall degradation to keyword/tag/kind/time ranking when the
  embedding provider is unavailable, surfaced through the capability contract.
- Object/blob storage interface for exports and large artifacts, even if v0
  stores most data in Postgres.
- Declarative policy engine with default deny and a `policy test` decision path.
- Audit events for auth, token lifecycle, memory and fact changes, recall,
  cross-agent read/contribute/curate/forget, policy decisions, group changes,
  message send/deliver/read, identity export/import, operator actions, limits,
  and billing/support stubs when invoked.
- Managed v0 audit retention default of 365 days.
- Default per-memory content and per-fact size limits with reasonable v0 caps.

Delivery capabilities:

- Public Go module using the latest stable Go baseline selected before
  implementation.
- Public GitHub repository.
- Public release workflow.
- Public Homebrew tap automation.
- Universal installer script.
- Public CLI/MCP image under `images/witself`.
- Public backend image under `images/witself-server`.
- Helm chart skeleton under `charts/witself`.
- AWS Terraform skeleton under `infra/terraform`.
- CI for Go, linting, docs, Dockerfiles, Helm, Terraform, release config,
  security scanning, SBOM/provenance, and install smoke paths as each surface
  appears.

## Capability-Gated V0 Surfaces

The v0 command, API, and JSON contracts should include the shape of these
managed-service surfaces, but they do not need to be fully live at first:

- Billing.
- Traditional payment setup.
- Crypto payment rails.
- Support tickets.
- Managed account lifecycle beyond the minimum needed for bootstrap.
- Production self-host support claims.

When a backend cannot perform one of these operations, it must return a stable
`unsupported_operation` error with capability context. This is true for local,
self-hosted, preview managed, and partially configured managed deployments.

Inter-agent messaging is **not** capability-gated for v0. The durable mailbox,
delivery, ordering, and acknowledgement are fully in scope, not a stub.

## V0 Non-Goals

V0 does not include:

- Live card charging or bank payment collection inside Witself.
- Witself-managed wallet custody.
- Crypto settlement owned directly by Witself.
- A production support desk workflow.
- A production web dashboard.
- A private Witself staff admin CLI.
- MCP HTTP or network transport.
- A Witself utility token for account setup, billing, or access.
- Automatic re-embedding on provider/model change (re-embedding is an explicit,
  audited maintenance operation).
- Field-level encryption of `sensitive` facts as a default (it remains an
  optional capability).
- Production support promises for arbitrary self-hosted installations before
  hardening docs, backup guidance, upgrade guidance, and support contracts are
  ready.

## V0 Exit Criteria

V0 is credible when:

- A clean checkout can build and test the CLI and server.
- The CLI can run the core memory, fact, policy, group, message, token, agent,
  and identity-reference flows against local development mode.
- The same flows can be exercised through `witself-server serve --dev`.
- Semantic recall returns ranked results with the embedding provider active, and
  degrades deterministically to keyword/tag/kind/time ranking with the degraded
  state surfaced when the provider is unavailable.
- Cross-agent access obeys default deny: with no matching `allow` policy,
  cross-agent read/contribute/curate/forget are denied, and `policy test`
  reports the deciding policy or deny reason.
- Cross-agent curate and forget require an audit `--reason`, support `--dry-run`,
  and are soft-delete/tombstoned and reversible by default.
- Message sender identity is always derived from the token; passing a `from`
  field cannot spoof another agent.
- Health and metrics endpoints work without leaking memory content, fact values,
  message bodies/payloads, embedding vectors, raw tokens, sensitive
  configuration, raw paths, or user input.
- `sensitive` memory content and `sensitive` fact values are redacted by default
  in list/scan output across CLI, MCP, API, logs, errors, and audit records;
  an authorized read of a single record returns the value with no reveal
  ceremony.
- `witself export` produces structured, round-trippable identity data and
  `witself import` restores it, preserving primary flags, sensitive markers,
  links, and edit history.
- Managed recovery restores customer identity data and service availability,
  including the vector data needed to restore semantic recall.
- The Postgres adapter and the pgvector recall path pass at least a smoke or
  integration test in a controlled environment.
- Release dry runs produce verifiable artifacts, images, checksums, SBOMs, and
  provenance.
- Helm and Terraform scaffolding lint and render/validate without embedding raw
  tokens or provider credentials.
- Unsupported billing, payment, crypto payment, support, and self-hosted
  production operations fail deterministically with capability details.

## Related Docs

- [requirements.md](requirements.md)
- [implementation-plan.md](implementation-plan.md)
- [cli-command-surface.md](cli-command-surface.md)
- [api-contract.md](api-contract.md)
- [memory-model.md](memory-model.md)
- [facts-model.md](facts-model.md)
- [access-policy.md](access-policy.md)
- [security-groups.md](security-groups.md)
- [inter-agent-messaging.md](inter-agent-messaging.md)
- [observability-and-operations.md](observability-and-operations.md)
- [mcp-tools.md](mcp-tools.md)
- [json-contracts.md](json-contracts.md)
- [operator-auth.md](operator-auth.md)
- [audit-retention.md](audit-retention.md)
- [backup-and-recovery.md](backup-and-recovery.md)
- [billing-and-limits.md](billing-and-limits.md)
- [release-and-build.md](release-and-build.md)
- [scaffold-readiness.md](scaffold-readiness.md)
- [post-v0-roadmap.md](post-v0-roadmap.md)
