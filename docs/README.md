# Witself Docs

Status: evolving architecture and implementation reference. Narrative-memory
sections were reconciled on 2026-07-14; older documents retain explicit
supersession notices where implementation has moved ahead of the original draft.

Current cross-feature implementation, managed rollout, evidence, and open gates
are tracked in the canonical [Feature Status](feature-status.md) scorecard.
Feature documents remain the detailed contracts; their local status paragraphs
do not replace the repository-wide readiness declaration.

Witself is the agent durable-state platform and the trust fabric agents
collaborate over: one open plane (memories + facts + avatars, plaintext at rest,
recallable, in the self-digest, plaintext-exportable) and one sealed plane
(secrets + TOTP, envelope-encrypted, reveal-gated, never embedded, recalled, in
the digest, or plaintext-exported). On top of that durable, attributable self,
Witself adds durable same-realm agent messaging and collaboration requests.
Cross-realm and cross-account federation remain planned work rather than a live
product claim. Witself runs as a multi-cloud-shaped platform of independent
deployment cells under a thin global control plane.

## Product And Architecture

- [feature-status.md](feature-status.md): the generated human view of the
  canonical feature-progress catalog, with separate implementation and rollout
  axes, seven non-averaged gates, repository evidence, and concrete open gates.
- [requirements.md](requirements.md): product requirements, terminology,
  identity posture, billing posture, backend requirements, and settled v0
  decisions.
- [v0-scope.md](v0-scope.md): first release target, required capabilities,
  capability-gated surfaces, non-goals, and exit criteria.
- [memory-model.md](memory-model.md): the earlier memory payload, lifecycle,
  edit history, and recall draft; superseded inference sections are marked.
- [narrative-memory-and-curation.md](narrative-memory-and-curation.md): the
  accepted portable narrative-memory architecture, implemented same-turn
  capture and client-side curation protocol, deterministic backend, retrieval,
  PostgreSQL graph, runtime adapters, export/import, production-readiness
  checklist, and tracked certification gates. It controls wherever an older
  draft still describes server inference or native-only narrative memory.
- [client-memory-curator-recipe.md](client-memory-curator-recipe.md): the
  provider-neutral, client-inference workflow for foreground pending-checkpoint
  handling, claiming due work, paging a frozen snapshot, planning, applying an
  empty or non-empty reversible plan, retrying, rolling back, and operating the
  explicit legacy/manual launcher when deliberately configured.
- [facts-model.md](facts-model.md): planned fact capabilities and the transition
  from the original name/value identity-card draft.
- [fact-service.md](fact-service.md): the implemented subject/predicate core,
  typed assertions, provenance/history, CLI/API/MCP surfaces, and deferred work.
- [agent-memory-routing.md](agent-memory-routing.md): the implemented Codex,
  Claude Code, Grok Build, and Cursor policies plus phase-one previews for
  OpenClaw MCP/workspace, Antigravity rules-plugin/shared-MCP, and GitHub
  Copilot global-instructions/user-MCP routing for portable
  fact-and-narrative-memory routing and explicitly selected provider-native
  coexistence.
- [client-custodied-agent-vault.md](client-custodied-agent-vault.md): the
  authoritative secrets implementation plan: client-held per-agent vault key,
  ciphertext-only backend, local password/TOTP/use paths, archive portability,
  fail-closed bootstrap, and four-runtime/three-cloud delivery slices.
- [secret-model.md](secret-model.md): the earlier sealed-plane secret data model and
  lifecycle (create/show/reveal/update/rename/copy/archive/restore/delete/grant),
  templates, references, password generation, runtime injection, and the
  carve-outs (secrets are never embedded, recalled, in the digest, or
  plaintext-exported; reveal-gated).
- [totp-2fa.md](totp-2fa.md): the sealed-plane TOTP authenticator
  (enroll/code/show/delete), otpauth/Base32/QR handling, the seed as high-value
  sealed material, and `totp:enroll` vs `totp:code`.
- [secret-size-and-attachments.md](secret-size-and-attachments.md): secret field
  size limits and attachment handling for the sealed plane.
- [context-hydration.md](context-hydration.md): the always-loaded self-digest,
  bounded meaningful-keyword `OR` recall, fail-open additive checkpoint state,
  explicit degraded-recall markers, the agent teaching layer, the two-way
  CLAUDE.md/AGENTS.md file bridge, and the session bootstrap protocol.
- [access-policy.md](access-policy.md): the default-deny cross-agent policy
  engine, permission verbs, guardrails, and `policy test` evaluation.
- [authorization-and-roles.md](authorization-and-roles.md): the role/scope model,
  account and realm roles, scope bundles, and the resolution algorithm spanning
  the open plane (memory/fact/policy/group/message) and the sealed plane
  (secret/totp + grants).
- [security-groups.md](security-groups.md): named agent groups as policy
  subjects and targets, membership, and group-owned shared identity data.
- [inter-agent-messaging.md](inter-agent-messaging.md): the durable mailbox
  model, token-derived sender identity, recipient-only replies,
  oldest-unacknowledged listen, separate read/ack state, migration-0034 fenced
  direct processing, migration-0035 server-derived causal depth, migration-0036
  deterministic failure counting, migration-0037 explicit-list/realm fanout,
  migration-0038 client-ranked open requests, atomic result completion,
  foreground message checkpoints, message threats, and the explicit boundary
  between code-complete core behavior and remaining release/rollout work.
- [autonomous-realm-messaging.md](autonomous-realm-messaging.md): the working
  same-realm foreground-client design for direct and client-ranked open-request
  work, with realm/explicit fanout, provider-accurate checkpoint hydration, an
  explicit no-wake boundary, and the canonical completion/follow-on split.
- [transcript-ledger.md](transcript-ledger.md): append-only visible conversation
  capture, its boundary from A2A messaging and memory, and the structured-object
  versus file-artifact storage decision.
- [agent-collaboration.md](agent-collaboration.md): the cross-realm /
  cross-account agent collaboration substrate — realm-authority addressing,
  signed realm/agent discovery, the blind cloud relay, cross-realm conversations,
  the loop and safety stack, and deny-by-default federation; extends
  inter-agent-messaging.md as the first post-v0 epic.
- [agent-email.md](agent-email.md): implemented managed-domain agent mail —
  receiving for verification/OTP mail, service mail, and human-to-agent
  correspondence plus independently gated provider-isolated sending, currently
  live for the exact Founder production cohort; covers the inverted
  (unverified-sender) trust boundary, fenced foreground processing, retention,
  durable lifecycle queues, and default-off expansion controls.
- [agent-console.md](agent-console.md): the local, per-agent, content-read-only
  Console presentation matrix, including received and sent email metadata,
  domain-ownership boundaries, degraded states, and the distinction from the
  fleet-admin TUI and any future hosted console.
- [agent-avatars.md](agent-avatars.md): portable versioned agent portraits,
  deterministic placeholders, realm style packs, client-side AI generation,
  autonomy policy, safe SVG validation, evolution, and CLI/API/MCP routing for
  Codex, Claude Code, Grok Build, Cursor, OpenClaw, Antigravity, and GitHub
  Copilot.
- [operator-auth.md](operator-auth.md): CLI-initiated human/operator auth,
  device-code fallback, self-hosted bootstrap, and unattended token posture.
- [threat-model.md](threat-model.md): assets, principals, trust boundaries,
  the integrity/authenticity attacker model, required controls, and non-goals.
- [backend-architecture.md](backend-architecture.md): public backend,
  self-hosting, storage adapters, process boundaries, and implementation path.
- [cell-worker.md](cell-worker.md): the independently scalable cell background
  runtime, cooperative PostgreSQL work ownership, health, metrics, and rollout
  model.
- [observability-and-operations.md](observability-and-operations.md):
  Prometheus metrics, Kubernetes health probes, structured logs, Helm values,
  and operational checks.
- [self-hosting.md](self-hosting.md): intended operator experience for running
  Witself in a customer-owned cloud.
- [self-host-support.md](self-host-support.md): local, preview, and paid
  production self-hosted support boundary.
- [server-command-surface.md](server-command-surface.md): separate
  `witself-server` API and `witself-worker` background-service command design.
- [api-contract.md](api-contract.md): public `/v1` HTTP API shape,
  authentication, capabilities, idempotency, pagination, and route groups.
- [api-routes.md](api-routes.md): resource-oriented `/v1` route style and
  sensitive action route conventions.
- [data-model.md](data-model.md): the full relational data model across both
  planes — realm/account/operator/agent/token tables, open-plane tables
  (versioned memories, evidence and lineage, facts, avatars, policies, groups,
  messages, audit, usage), and sealed-plane tables (secrets, secret_fields,
  secret_grants, totp_enrollments, realm_keys, secret_deks, attachments).
- [storage.md](storage.md): authoritative PostgreSQL storage, deterministic
  full-text retrieval, optional client-supplied vectors, object/blob usage,
  infrastructure encryption, client-custodied secret envelopes, and Goose
  migrations.
- [encryption-model.md](encryption-model.md): the earlier KMS-rooted sealed-plane
  confidentiality draft, superseded for vault custody by ADR 0003.
- [key-hierarchy.md](key-hierarchy.md): the earlier CMK/realm-KEK hierarchy,
  superseded for vault custody by ADR 0003.
- [cloud-targets.md](cloud-targets.md): AWS-first managed cloud and
  self-hosted Terraform target decision.
- [memory-cloud-conformance.md](memory-cloud-conformance.md): the executable
  AWS/GCP/Azure managed-PostgreSQL and directed account-move certification
  matrix, protected-runner contract, and evidence requirements.
- [memory-runtime-acceptance.md](memory-runtime-acceptance.md): the executable
  six-session acceptance protocol for Codex, Claude Code, Cursor, and Grok
  Build, including delivery-mode and sanitized-evidence boundaries.
- [claude-codex-delegation.md](claude-codex-delegation.md): how Claude Code
  delegates bounded implementation and review work to Codex (GPT-5.6 Sol)
  through OpenAI's official Codex plugin — read-only review lanes, a
  workspace-write task lane that Claude runs only inside its own worktree,
  and the verification and merge ownership Claude keeps.
- [memory-load-quality.md](memory-load-quality.md): the deterministic
  PostgreSQL load/quality harness, current exploratory baseline, evidence
  schema, and remaining production workload/SLO work.
- [deployment-cells.md](deployment-cells.md): the multi-cloud deployment
  topology — a fleet of independent cells, each authoritative for its own
  tenants, under a thin global control plane that does placement and routing
  resolution; cells at different versions, tenant migration between cells, and
  the shared global directory the collaboration relay reuses.
- [token-lifecycle.md](token-lifecycle.md): durable v0 agent token behavior,
  token file format, rotation, revocation, and agent disable effects.
- [billing-and-limits.md](billing-and-limits.md): account-level plan billing,
  realm-rolled metered dimensions, limits, and overage behavior.
- [billing-return-pages.md](billing-return-pages.md): value-free hosted
  checkout and portal return routes that hand control back to the AI or CLI.
- [launch-readiness.md](launch-readiness.md): the working plan of record for
  the general self-service production launch — decided execution order and the
  items that need Scott directly.
- [data-retention-policy.md](data-retention-policy.md): the consolidated
  customer-facing statement of how long transcripts, messages, the audit trail,
  and raw email are kept, end-of-window actions, and the legal-hold scope.
- [audit-retention.md](audit-retention.md): managed, self-hosted, and local
  audit retention defaults and audit content rules.
- [transcript-retention.md](transcript-retention.md): account plan defaults,
  admin overrides, cell snapshots, and whole-conversation enforcement.
- [message-retention.md](message-retention.md): messaging entitlement,
  independent account overrides, and whole-thread retention enforcement.
- legal/ — DRAFT legal-page skeletons for owner/counsel review:
  [Terms](legal/terms-of-service.md) · [Privacy](legal/privacy-policy.md) ·
  [AUP](legal/acceptable-use.md) · [DPA](legal/data-processing-addendum.md) ·
  [Refunds](legal/refund-cancellation.md). Not in force until ratified.
- [backup-and-recovery.md](backup-and-recovery.md): operational backups
  (including vector data), first-class plaintext identity export, and
  round-trippable import.
- [post-v0-roadmap.md](post-v0-roadmap.md): deliberately deferred features,
  including MCP network transport, web dashboard, utility token, cross-realm
  federation, policy `deny` effects, and advanced vector-profile migration.
- [helm-chart.md](helm-chart.md): first self-hosted Kubernetes deployment
  artifact and chart requirements.
- [terraform-infrastructure.md](terraform-infrastructure.md): AWS, GCP, and
  Azure infrastructure modules and stack layout.
- [governance-and-support.md](governance-and-support.md): public-code,
  licensing, contribution, self-hosting, package, and support boundaries.

## Interfaces

- [workflow-scripts.md](workflow-scripts.md): step-by-step CLI workflow scripts
  for install, setup, billing, memories, facts, policy, groups, messaging, MCP,
  self-hosted, and local mode.
- [cli-command-surface.md](cli-command-surface.md): human and agent CLI command
  design, including local runtime discovery and bulk integration lifecycle
  operations.
- [mcp-tools.md](mcp-tools.md): MCP tool surface and safety posture.
- [json-contracts.md](json-contracts.md): shared machine-readable response
  shapes, `witself://` references, and resource contracts for CLI, MCP, managed
  API, self-hosted API, and local development.

## Delivery

- [release-and-build.md](release-and-build.md): Go baseline, CI, release,
  signing, Homebrew, universal installer, and container image requirements.
- [implementation-plan.md](implementation-plan.md): build sequence for the CLI,
  MCP adapter, backend server, images, Helm, Terraform, release, and hardening.
- [scaffold-readiness.md](scaffold-readiness.md): docs freeze boundary and
  initial repo scaffold target.
- [security-policy.md](security-policy.md): vulnerability reporting and
  supported security surfaces.

## Research

- [competitive-analysis.md](competitive-analysis.md): product patterns from
  agent-memory systems, knowledge stores, identity/profile services, and
  MCP-adjacent tools.

## Decisions

- [decisions/0001-consolidate-witpass-into-witself.md](decisions/0001-consolidate-witpass-into-witself.md):
  consolidating the Witpass secrets product into Witself as the sealed plane —
  the two-plane model, naming/ownership unification, and the five reconciliations.
- [ADR 0002](decisions/0002-client-side-narrative-memory.md):
  making Witself the portable narrative-memory system of record while keeping
  all semantic inference and optional embedding generation in clients.
- [ADR 0003](decisions/0003-client-custodied-agent-vault.md): making each named
  agent's separate client-held AVK the sealed-plane decrypt root while the
  backend remains a portable ciphertext store.
- [ADR 0004](decisions/0004-local-agent-dashboard.md):
  serving the loopback-only, content-read-only local Agent Console through
  `witself dashboard`, over the same public API, authorization, and redaction
  rules as every other client — distinct from the fleet-admin TUI and deferred
  operator/admin web dashboard.
