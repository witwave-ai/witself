# Witself V0 Scope

Status: draft. Decision: v0 is a usable cloud-shaped slice, not only a local
mock and not yet the full commercial managed service.

Open-plane amendment (accepted 2026-07-14): the memory slice now follows
[narrative-memory-and-curation.md](narrative-memory-and-curation.md). Narrative
capture is a Witself capability, inference is client-side, PostgreSQL lexical
recall is the universal baseline, and migration-0032 client-supplied vector
profiles/JSONB rows plus bounded hybrid recall are implemented but optional to
use.
Conflicting native-only or server-embedding language later in this draft is
superseded.

## Sequencing

Decision: v0 ships in two sequenced slices on one platform. The **open-plane core
(memory/fact/identity) ships first**; the **sealed credential plane
(secrets/TOTP/encryption/KMS/reveal) is a defined v0 slice that may be staged
after the core**. Both are in v0 scope; the sequencing exists so the identity
store can prove itself without blocking on KMS. This mirrors
[requirements.md](requirements.md#v0-scope).

- **Open-plane core (ships first).** The CLI, MCP stdio, the backend API
  boundary, the local development adapter, the agent token lifecycle, the memory
  model with lexical/structured recall and fenced client-side curation, the facts model with primary promotion, agent
  self-managed memory and context hydration, the cross-agent access policy
  engine, security groups, full inter-agent messaging, identity export/import,
  audit, metrics, health probes, and the delivery scaffolding below. The open
  plane has **no KMS or model-provider dependency**: PostgreSQL is the readiness
  gate and optional client-vector indexes are derived, rebuildable data.
- **Sealed credential plane (a defined v0 slice, may stage after the core).** The
  secret data model and lifecycle, secret references, TOTP/2FA, password
  generation, runtime injection (`witself run`), two-tier envelope encryption
  (CMK → per-realm KEK → per-secret/field DEK), the reveal ceremony, secret
  grants and realm roles, and the sealed-plane carve-outs. This slice adds a
  **KMS dependency** that is a readiness gate **only when the sealed plane is
  enabled**; an open-plane-only deployment requires no KMS. Sealed-plane carve-out: secrets and
  TOTP seeds are never embedded, recalled, in the self-digest, or
  plaintext-exported, and are reachable only through the audited reveal ceremony.
  See [secret-model.md](secret-model.md), [totp-2fa.md](totp-2fa.md),
  [encryption-model.md](encryption-model.md), and
  [key-hierarchy.md](key-hierarchy.md).

## Decision

V0 should prove the end-to-end product boundary:

- A human or AI agent can install `ws`.
- A human operator can authenticate through a CLI-initiated browser or
  device-code flow without giving the CLI a raw account password.
- An operator can bootstrap a realm and named agents.
- A named agent can authenticate from a token file or token environment.
- The CLI can add, adjust, read, recall, list, forget, restore, and delete an
  agent's own memories, and can drive an agent's fenced curation queue through
  plan, apply, and rollback.
- The CLI can set, get, list, and delete an agent's own facts, and promote a
  fact to primary.
- An agent integration routes natural-language capture without disabling the
  runtime's own memory: an explicitly requested atomic assertion goes to a
  Witself fact, narrative context goes to portable Witself memory, and a native
  write is added only when explicitly requested. Current self-management also
  includes the `self show` digest and direct narrative-memory lifecycle. Session
  helpers and the file bridge remain explicit future work. Per-user launchd and
  systemd scheduling is available through `memory curate auto service`. The
  opportunistic request/run/plan/apply/
  rollback curation protocol and opt-in client-owned terminal-flush launcher are
  implemented.
- `memory recall` performs deterministic PostgreSQL lexical/structured ranking
  over the caller's accessible memories. Optional vectors are supplied by the
  client under an immutable profile and may add bounded hybrid ranking; the
  backend generates none.
- The CLI can create, list, show, delete, and `test` declarative cross-agent
  access policies under a default-deny stance.
- The CLI can create, list, show, and manage membership of security groups,
  including group-scoped shared memories and facts.
- The CLI can create, append, list, and show visible interaction transcripts;
  agents write their own ledger and account operators have read-only audit
  visibility.
- The CLI can send, list, read, and acknowledge durable inter-agent messages,
  with sender identity derived from the token.
- The CLI can export an agent's self as structured plaintext and import it back
  round-trippably.
- `witself mcp serve` can expose the safe v0 MCP stdio tool surface, including
  the self-management and fourteen curation tools, and teach the always-loaded
  recall-before-act / write-after-learn protocol through its server
  `instructions` field.
- `witself-server serve --dev` can exercise the same core through HTTP.
- `witself-server` exposes Prometheus metrics, structured logs, and Kubernetes
  health probes.
- The Postgres path exists early enough to keep the backend honest and remains
  the sole authoritative memory data source.
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
- Memory capture/show/list/recall/history/adjust/supersede/forget/restore/
  reactivate/evidence-resolution/delete for the caller's own memories, with
  reversible lifecycle operations and a separately guarded permanent delete.
- `memory curate request|requests|start|renew|show|plan|apply|cancel|abandon|
  rollback|status` for client-side inference over frozen agent-self inputs.
- Fact set/get/list plus guarded permanent deletion by canonical
  subject/predicate. Deletion previews a value-free receipt, requires explicit
  confirmation, purges assertions/history/evidence and candidates, and retains
  only a value-free integrity tombstone and immutable usage history. Explicit
  recreation receives a new fact id and zero inherited usage rank.
- Provider-aware agent routing for natural-language `remember`, `save`, and
  `store`: explicit atomic assertions use `fact set`; narrative context uses
  portable Witself narrative memory by default; runtime-native memory is used
  only when explicitly named; clearly mixed requests split; and the same
  content is never silently duplicated. A future unified `witself remember`
  command may compose the existing Witself operations, but it is not a backend
  inference router.
- `self show` bounded session-start digest of primary facts, top-N salient
  memories, and a one-line kinds/tags/counts index, hard-capped with an
  `elided` signal instead of silent truncation, and never requiring a model or
  embedding provider.
- Target `session start`/`session end` helpers for multi-session bootstrap remain
  future; current clients capture bounded evidence-backed checkpoints directly.
- Direct `memory supersede` applies one exact client-authored replacement set.
  Deeper client-run curation is implemented through deterministic due requests,
  lease/fence claims, immutable input pages, strict hashed plans, atomic apply,
  cancellation/abandonment, and guarded rollback. The backend never chooses a
  semantic merge or split and never launches inference. An opt-in local
  `memory curate auto` worker now records value-free terminal-flush wakes and
  launches the trusted client driver with explicit provider, transcript consent,
  and preview/apply policy; optional persistent per-user launchd/systemd
  scheduling is implemented through the `auto service` lifecycle.
- Target `digest emit`, `ingest`, and `bootstrap-instructions` file-bridge
  helpers remain future. Current `witself install` integrations install the
  managed routing guidance and MCP server directly.
- Policy create/list/show/delete/test under default deny.
- Group create/list/show/add-member/remove-member/delete, including
  group-scoped shared memories and facts.
- Message send/list/read/ack over a durable mailbox with per-recipient ordering
  and acknowledgement.
- Identity reference parsing and resolution.
- `witself export` and `witself import` for round-trippable structured
  identity data, including the portable curation graph and value-free receipts.
- `witself mcp serve` over stdio, exposing the self-management tools and the
  pinned server-instructions teaching protocol. The curation tools are
  `request`, `start`, `renew`, `get`, `plan`, `apply`, `cancel`, `rollback`, and
  `status`; read-only mode retains only `get` and `status`.

Sealed credential-plane capabilities (the defined v0 slice that may stage after
the open-plane core; secrets and TOTP seeds are never embedded, recalled, in the
self-digest, or plaintext-exported, and values are reachable only through the
audited reveal ceremony):

- `secret create/show/list/scan/reveal/update/rename/copy/archive/restore/delete`
  for the caller's own secrets, with `--group` for group-owned secrets and
  `--owner-agent` for operator-targeted access. `secret reveal` is the explicit,
  audited value-returning op with the reveal ceremony.
- `secret grant/revoke` to delegate sealed-plane access through secret grants,
  composed with realm roles (sealed-plane cross-agent access is governed by
  grants plus realm roles, not the open-plane cross-agent policy engine).
- `totp enroll/code/show/delete`, where the TOTP seed is high-value sealed
  material and `totp code` is a value-returning, audited op distinct from
  `totp enroll`.
- `password generate`, offering generated material for storage as a sealed secret
  field without writing a plaintext value into the open plane.
- `witself run` to resolve sealed-plane secret references and inject resolved
  values into a child process environment, never into logs, audit records, or the
  open plane.
- `witself mcp serve --no-value-tools` to disable the value-returning sealed-plane
  tools (`secret.reveal`, `totp.code`, value-returning `reference.resolve`),
  separate from `--read-only` which disables mutations.

Core backend capabilities:

- Shared Go core below CLI, MCP, API, and local development adapters.
- `witself-server serve --dev`.
- `/v1/version`, `/livez`, `/readyz`,
  `/startupz`, `/metrics`, `/v1/whoami`, and `/v1/capabilities`.
- Initial `/v1/realms`, `/v1/agents`, `/v1/tokens`, `/v1/memories`,
  `/v1/facts`, `/v1/policies`, `/v1/groups`, `/v1/transcripts`, `/v1/messages`, and `/v1/audit`
  route groups.
- Self-management routes including `/v1/self`, `/v1/memories`, bounded memory
  reads/history/list/recall, exact lifecycle actions, evidence resolution, and
  guarded permanent deletion. The 13 curation routes under
  `/v1/memory-curation-requests`, `/v1/memory-curation-runs`, and
  `/v1/memory-curation-status` are implemented. Unified remember, session, and
  file-bridge routes remain future work.
- Colon action subroutes including `/v1/memories:recall`,
  `/v1/memories/{memory_id}:forget`, `/v1/memories/{memory_id}:restore`,
  `/v1/memories/{memory_id}:reactivate`, `/v1/memories/{memory_id}/supersede`,
  `/v1/facts/{fact_id}:primary`, `/v1/policies:test`,
  `/v1/messages/{message_id}:ack`, and `/v1/tokens/{token_id}:rotate`.
- Sealed credential-plane route groups for the sealed-plane slice:
  `/v1/secrets` (with `:reveal`, `:rotate`, `:archive`, `:restore`, `:grant`,
  `:revoke`), `/v1/totp` (with `:code`), and `/v1/password:generate`, all behind
  the capability flags `client_side_decrypt` / `server_side_decrypt` and the KMS
  readiness gate that applies only when the sealed plane is enabled.
- Prometheus metrics for HTTP traffic, auth, token operations, memory
  operations, recall and optional vector-index operations, fact operations, policy
  decisions, cross-agent accesses, group operations, messaging, audit events,
  usage, limits, storage, vector storage, and migrations. When the sealed plane
  is enabled, secret, reveal, TOTP, and KMS metric families
  (`witself_secret_reveals_total`, `witself_totp_*`,
  `witself_kms_operations_total`) and the sealed-plane metered dimensions
  (`stored_secret`, `secret_read`, `totp_code`, `runtime_injection`,
  `encrypted_storage_byte`) are added.
- Local development adapter behind the same backend interface.
- Postgres storage adapter with Goose migrations; it is the authoritative open-
  plane source and readiness gate. Migration `0030` adds the durable curation
  lanes, cursors, requests, runs, inputs, actions, and mutation receipts;
  migration `0032` adds portable JSONB vector profiles/rows. A future optional
  pgvector/ANN projection is rebuildable and never required for baseline or
  JSONB hybrid recall. When the sealed
  plane is enabled, the schema gains `realm_keys` and `secret_deks` and a KMS
  provider binding (`aws-kms`, `gcp-kms`, `azure-key-vault`, `local-dev`) becomes
  a readiness gate for that slice only.
- Deterministic lexical/structured recall with no backend model dependency;
  capabilities independently report optional client-vector support/coverage.
- Object/blob storage interface for exports and large artifacts, even if v0
  stores most data in Postgres.
- Declarative policy engine with default deny and a `policy test` decision path.
- Audit events for auth, token lifecycle, direct memory and fact changes, recall,
  supersession, cross-agent read/contribute/curate/forget, policy
  decisions, group changes, message send/deliver/read, identity export/import,
  operator actions, limits, and billing/support stubs when invoked. When the
  sealed plane is enabled, secret create/update/rename/copy/archive/restore/
  delete, `secret.reveal`, secret grant/revoke, `totp.enrolled`/`totp.code`/
  seed-revealed/deleted, and `key.rotated` (KEK) events are emitted, with a
  `server_side_decrypt` flag recorded on reveal and code.
- Deterministic resource/receipt mutation results, exact idempotent retries,
  complete immutable memory versions, evidence, and token-derived provenance.
  The backend never invents semantic duplicate/merge decisions.
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
- Helm chart skeleton under `charts/witself-server`.
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
- Server-side vector generation or regeneration. Clients own model inference
  and may explicitly populate a new immutable profile while lexical recall
  remains available.
- Field-level encryption of `sensitive` facts as a default (it remains an
  optional open-plane capability). `sensitive` facts use lightweight redaction,
  not the sealed-plane reveal ceremony; a credential belongs in the sealed plane
  as a secret, not in a `sensitive` fact.
- Production support promises for arbitrary self-hosted installations before
  hardening docs, backup guidance, upgrade guidance, and support contracts are
  ready.
- Cross-realm agent collaboration and federation, including the signed realm
  card, the blind relay, the federation allow-list/trust registry, and the
  cross-realm conversation/task lifecycle. Realm-local inter-agent messaging is
  fully in v0 (see above); only **cross-realm** collaboration is deferred, and
  it is the flagship post-v0 epic. See
  [agent-collaboration.md](agent-collaboration.md) and
  [post-v0-roadmap.md](post-v0-roadmap.md).

## V0 Exit Criteria

V0 is credible when:

- A clean checkout can build and test the CLI and server.
- The CLI can run the core memory, fact, policy, group, message, token, agent,
  and identity-reference flows against local development mode.
- The same flows can be exercised through `witself-server serve --dev`.
- Lexical/structured recall returns deterministic ranked results without a model
  provider; optional vector coverage never changes PostgreSQL's authority.
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
- Agent integrations route an explicitly requested atomic assertion to a fact
  upsert and narrative context to portable Witself memory, adding a native write
  only when explicitly requested; `self show`
  returns a bounded digest that sets `elided` when
  capped and works without a model provider; direct lifecycle and exact
  client-authored supersession round-trip through CLI/MCP/API; client-authored
  curation requests, leases/fences, frozen inputs, strict plans, atomic apply,
  cancel/abandon, and rollback round-trip through CLI/HTTP, with fourteen MCP
  tools; the optional client-owned terminal-flush worker is configurable through
  `memory curate auto` and keeps credentials out of inference;
  `opportunistic_curation` is supported while `automatic_capture` and
  `scheduled_curation` remain explicitly unsupported backend capabilities; and
  `--read-only` MCP mode excludes the mutating self-management and curation
  tools while retaining curation `preflight`, request list/get, run get, input
  `get`, and `status`.
- `witself export` produces structured, round-trippable identity data and
  `witself import` restores it, preserving primary flags, sensitive markers,
  links, edit history, curation lanes/cursors/requests/runs/inputs/actions/
  receipts, and curation attribution. Import terminates active leases and
  advances the fence rather than reviving in-flight work in another cell.
- Managed recovery restores authoritative customer identity data and service
  availability; schema-32 vector profiles/rows round-trip, while FTS and any
  future ANN projection are rebuilt.
- The Postgres adapter and lexical/structured recall path pass integration tests
  in a controlled environment.
- Release dry runs produce verifiable artifacts, images, checksums, SBOMs, and
  provenance.
- Helm and Terraform scaffolding lint and render/validate without embedding raw
  tokens or provider credentials.
- Unsupported billing, payment, crypto payment, support, and self-hosted
  production operations fail deterministically with capability details.

When the sealed credential-plane slice ships, it is additionally credible when:

- An open-plane-only deployment passes readiness with **no KMS** configured;
  enabling the sealed plane makes KMS a readiness gate while PostgreSQL remains
  the open-plane gate.
- `secret reveal` and `totp code` are the only value-returning sealed-plane ops,
  each emits an audited event with the `server_side_decrypt` flag, and
  `--no-value-tools` disables them in MCP while `--read-only` disables mutations.
- Secrets and TOTP seeds never appear in embeddings, `memory recall`, `self show`,
  `digest emit`, `ingest`, or `witself export`; `witself export` excludes the
  sealed plane and secret backup is encrypted-only (envelope plus KMS key
  identity, never plaintext).
- Loss of KMS access renders sealed secret values unrecoverable (crypto-shred)
  without affecting the open plane.

## Related Docs

- [requirements.md](requirements.md)
- [implementation-plan.md](implementation-plan.md)
- [cli-command-surface.md](cli-command-surface.md)
- [api-contract.md](api-contract.md)
- [data-model.md](data-model.md)
- [memory-model.md](memory-model.md)
- [facts-model.md](facts-model.md)
- [secret-model.md](secret-model.md)
- [totp-2fa.md](totp-2fa.md)
- [secret-size-and-attachments.md](secret-size-and-attachments.md)
- [encryption-model.md](encryption-model.md)
- [key-hierarchy.md](key-hierarchy.md)
- [authorization-and-roles.md](authorization-and-roles.md)
- [context-hydration.md](context-hydration.md)
- [access-policy.md](access-policy.md)
- [storage.md](storage.md)
- [security-groups.md](security-groups.md)
- [inter-agent-messaging.md](inter-agent-messaging.md)
- [agent-collaboration.md](agent-collaboration.md)
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
