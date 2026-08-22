<!-- Code generated from featurestatus/catalog.json; DO NOT EDIT. -->

# Feature Status

This scorecard is the canonical reviewed declaration for current commercial capabilities, substantial implemented or actively building product surfaces, and specified security foundations whose contracts constrain delivered behavior. Other deferred candidates stay in the post-v0 roadmap. It separates implementation from managed rollout and applies the same seven non-averaged readiness gates to every tracked feature. Live fleet, control-plane, Cloudflare, provider, and billing health remain authoritative in their operational systems; this document does not turn a point-in-time observation into permanent truth.

The source is [`featurestatus/catalog.json`](../featurestatus/catalog.json). Run `make feature-status` after changing it. Ordinary `go test ./...` also rejects an invalid catalog, missing reference, uncovered plan feature, or stale generated document.

## Reading the scorecard

- **Implementation**: `planned`, `specified`, `building`, `implemented`, or `retired`.
- **Managed rollout**: `not started`, `dark`, `limited`, `general`, `not applicable`, or `retired`. This is a reviewed declaration, not a live-health claim.
- **Readiness**: `accepted` only for an implemented active or not-applicable rollout whose applicable gates all pass and whose release evidence is scoped; `conditional` when an implemented non-dark rollout has work remaining; `not ready` for planned/building or dark work; `blocked` when a gate fails; and `retired` for retired work. We do not average gates into a completion percentage.
- **Seven gates**: behavior, entitlement/policy, bounds/abuse, observability, recovery, rollout/canaries, and docs/support. `N/A` requires a written rationale.

A feature being implemented does not mean it is generally available. A plan entitlement also does not prove that its feature is ready or enabled for a particular account.

## Summary

| Feature | Area | Implementation | Managed rollout | Readiness | Gates | Open gates |
|---|---|---|---|---|---:|---:|
| [Cross-agent access policies and security groups](#access-policy-security-groups) | Authorization | `specified` | `not started` | **not ready** | 0/7 pass | 4 |
| [Account lifecycle and movement](#account-lifecycle) | Platform | `implemented` | `limited` | **conditional** | 4/7 pass | 2 |
| [Managed account onboarding and recovery](#account-onboarding-recovery) | Identity | `implemented` | `limited` | **conditional** | 2/7 pass | 4 |
| [Agent avatars](#agent-avatars) | Identity | `implemented` | `limited` | **conditional** | 5/7 pass | 3 |
| [Agent collaboration requests](#agent-collaboration) | Communication | `implemented` | `limited` | **conditional** | 4/7 pass | 3 |
| [Local Agent Console](#agent-dashboard) | Operator experience | `implemented` | `not applicable` | **conditional** | 5/6 pass | 1 |
| [Agent email receive](#agent-email-receive) | Email | `implemented` | `limited` | **conditional** | 3/7 pass | 5 |
| [Agent email send](#agent-email-send) | Email | `implemented` | `limited` | **conditional** | 3/7 pass | 4 |
| [Agent self, context, and foreground hydration](#agent-self-context) | Agent experience | `implemented` | `limited` | **conditional** | 5/7 pass | 2 |
| [Account audit trail and retention](#audit-trail-retention) | Governance | `building` | `limited` | **not ready** | 0/7 pass | 4 |
| [Billing and plan transitions](#billing-plan-transitions) | Commercial | `building` | `dark` | **not ready** | 2/7 pass | 5 |
| [Custom inbound email domains](#custom-email-domains) | Email | `building` | `dark` | **not ready** | 2/7 pass | 6 |
| [Durable facts](#facts) | Memory | `implemented` | `limited` | **conditional** | 4/7 pass | 3 |
| [Fleet deployment, backup, and recovery](#fleet-deployment-recovery) | Platform | `implemented` | `limited` | **conditional** | 4/7 pass | 3 |
| [Accounts, realms, agents, and tokens](#identity-tenancy) | Identity | `implemented` | `limited` | **conditional** | 5/7 pass | 2 |
| [Managed support](#managed-support) | Commercial | `implemented` | `limited` | **conditional** | 2/7 pass | 5 |
| [Narrative memory and curation](#narrative-memory) | Memory | `implemented` | `limited` | **conditional** | 4/7 pass | 5 |
| [Managed operator authentication](#operator-authentication) | Security | `specified` | `not started` | **not ready** | 0/7 pass | 4 |
| [Plans, limits, and account overrides](#plan-enforcement) | Commercial | `implemented` | `limited` | **conditional** | 5/7 pass | 3 |
| [Realm email aliases](#realm-email-aliases) | Email | `implemented` | `dark` | **not ready** | 2/7 pass | 6 |
| [Realm-local messaging](#realm-messaging) | Communication | `implemented` | `limited` | **conditional** | 5/7 pass | 2 |
| [Agent runtime integrations](#runtime-integrations) | Integration | `implemented` | `limited` | **conditional** | 5/7 pass | 3 |
| [Secrets, vault, passwords, and TOTP](#secrets-vault) | Security | `implemented` | `limited` | **conditional** | 2/7 pass | 4 |
| [Self-hosted Witself](#self-hosting) | Deployment | `implemented` | `not applicable` | **conditional** | 0/7 pass | 5 |
| [Transcripts and retention](#transcripts) | Core data | `implemented` | `limited` | **conditional** | 4/7 pass | 3 |
| [Usage metering and customer reporting](#usage-metering) | Commercial | `implemented` | `limited` | **conditional** | 4/7 pass | 3 |

## Feature details

<a id="access-policy-security-groups"></a>

### Cross-agent access policies and security groups

Default-deny, realm-local cross-agent memory and fact policy plus group-owned identity are specified in detail, but the policy engine, group lifecycle, enforcement, and operational surfaces are not implemented.

- Implementation: `specified`
- Managed rollout: `not started`
- Readiness: **not ready**
- Detailed docs: [access-policy.md](../docs/access-policy.md), [api-contract.md](../docs/api-contract.md), [security-groups.md](../docs/security-groups.md)

| Gate | State | Current evidence and conclusion |
|---|---|---|
| Behavior | **CONDITIONAL** | Policy, permission, group, membership, collective-memory, and default-deny behavior are specified but have no store, API, CLI, or MCP implementation. [access-policy.md](../docs/access-policy.md), [security-groups.md](../docs/security-groups.md), [server.go](../internal/server/server.go) |
| Entitlement / policy | **CONDITIONAL** | The intended realm-local authority and operator override are documented, but no runtime evaluator enforces them. [access-policy.md](../docs/access-policy.md) |
| Bounds / abuse | **CONDITIONAL** | Default deny, non-nesting, guarded membership, filters, and audit requirements are designed but not executable protections. [access-policy.md](../docs/access-policy.md), [api-contract.md](../docs/api-contract.md), [security-groups.md](../docs/security-groups.md) |
| Observability | **CONDITIONAL** | Planned value-free decision and membership audit signals have no metrics, dashboards, SLOs, or alert path. [access-policy.md](../docs/access-policy.md), [observability-and-operations.md](../docs/observability-and-operations.md) |
| Recovery | **CONDITIONAL** | Membership rollback, policy invalidation, archive/import, and crash recovery remain design requirements rather than tested behavior. [access-policy.md](../docs/access-policy.md), [security-groups.md](../docs/security-groups.md) |
| Rollout / canaries | **CONDITIONAL** | No managed cohort, dark gate, migration, or release canary exists for this specified feature. [access-policy.md](../docs/access-policy.md) |
| Docs / support | **CONDITIONAL** | The design is extensive, but contracts must be reconciled with the current client-custodied vault, fact deletion, collaboration, and plan surfaces before implementation. [access-policy.md](../docs/access-policy.md), [security-groups.md](../docs/security-groups.md) |

Open gates:

- `access-policy-contract-reconciliation` (docs / support): Reconcile the drafts with current memory, fact, collaboration, vault, and authorization contracts and pin the versioned wire shapes. ([tracking/evidence](../docs/api-contract.md))
- `access-policy-core-implementation` (behavior, bounds / abuse, entitlement / policy): Implement the policy evaluator, security-group lifecycle, transactional membership, collective ownership, authorization hooks, audit, API, CLI, MCP, and hostile-input tests. ([tracking/evidence](../docs/security-groups.md))
- `access-policy-operations` (observability, recovery): Add bounded decision metrics, invalidation and backlog alerts, archive/import, rollback, and crash-recovery drills without exposing identity content. ([tracking/evidence](../docs/observability-and-operations.md))
- `access-policy-release-canary` (rollout / canaries): Ship behind a dark account cohort and retain allow, deny, membership-revocation, rollback, and cross-agent isolation canaries before widening. ([tracking/evidence](../docs/access-policy.md))

<a id="account-lifecycle"></a>

### Account lifecycle and movement

Activation, suspension, closure, archive/import, and deliberate whole-account evacuation and placement mechanics are implemented; broad movement remains frozen until monitoring and multi-cell drills are complete. Realm-level placement and dual-write cutover are outside this slice.

- Implementation: `implemented`
- Managed rollout: `limited`
- Readiness: **conditional**
- Detailed docs: [backup-and-recovery.md](../docs/backup-and-recovery.md), [deployment-cells.md](../docs/deployment-cells.md), [governance-and-support.md](../docs/governance-and-support.md)

| Gate | State | Current evidence and conclusion |
|---|---|---|
| Behavior | **PASS** | Lifecycle transitions, archive/import, evacuation fences, and resumable control-plane state have transactional and crash-recovery coverage. [deployment-cells.md](../docs/deployment-cells.md), [evacuation_integration_test.go](../internal/store/evacuation_integration_test.go) |
| Entitlement / policy | **PASS** | Owner and fleet-admin operations are separated, suspended accounts fail closed, and placement eligibility is controlled centrally. [authorization-and-roles.md](../docs/authorization-and-roles.md), [deployment-cells.md](../docs/deployment-cells.md) |
| Bounds / abuse | **PASS** | Exact-id idempotency, mutation fences, bounded batches, lifecycle receipts, and target-cell validation constrain concurrent movement. [deployment-cells.md](../docs/deployment-cells.md), [evacuation_integration_test.go](../internal/store/evacuation_integration_test.go) |
| Observability | **CONDITIONAL** | Value-free status and metrics exist, but continuous Prometheus scraping, PVC metrics, Alertmanager routing, and a tested external receiver are not deployed. [observability-and-operations.md](../docs/observability-and-operations.md) |
| Recovery | **CONDITIONAL** | Archive integrity and backup restore are exercised, but directed multi-cell movement and sealed-state recovery still need a retained production drill. [backup-and-recovery.md](../docs/backup-and-recovery.md), [deployment-cells.md](../docs/deployment-cells.md) |
| Rollout / canaries | **CONDITIONAL** | The placement runner is intentionally paused; widening requires every accepting destination to meet schema, headroom, and monitoring gates. [deployment-cells.md](../docs/deployment-cells.md), [runbooks.md](../docs/runbooks.md) |
| Docs / support | **PASS** | Lifecycle, backup, movement, freeze, and operator recovery procedures are documented. [backup-and-recovery.md](../docs/backup-and-recovery.md), [deployment-cells.md](../docs/deployment-cells.md), [runbooks.md](../docs/runbooks.md) |

Open gates:

- `continuous-capacity-alerting` (observability): Deploy continuous capacity scraping and alert delivery before automatic placement resumes. ([tracking/evidence](../docs/observability-and-operations.md))
- `directed-move-drill` (recovery, rollout / canaries): Retain a release-specific multi-cell evacuation, restore, and sealed-state recovery drill. ([tracking/evidence](../docs/deployment-cells.md))

<a id="account-onboarding-recovery"></a>

### Managed account onboarding and recovery

Invite-gated signup, cell placement, email verification and resend, credential recovery, email change and undo, and crash-safe CLI provisioning are implemented for managed accounts; broad abuse protection and release acceptance remain limited.

- Implementation: `implemented`
- Managed rollout: `limited`
- Readiness: **conditional**
- Detailed docs: [cli-command-surface.md](../docs/cli-command-surface.md), [runbooks.md](../docs/runbooks.md)

| Gate | State | Current evidence and conclusion |
|---|---|---|
| Behavior | **CONDITIONAL** | Signup, exact invite consumption, pending-account projection, and resumable CLI setup are tested; verification, resend, recovery, email change, and undo handlers lack direct Worker contract tests. [account_create_recovery_test.go](../cmd/witself/account_create_recovery_test.go), [index.js](../infra/cloudflare/control-plane/src/index.js), [account-signup-runtime.test.mjs](../infra/cloudflare/control-plane/test/account-signup-runtime.test.mjs) |
| Entitlement / policy | **PASS** | Managed signup requires an authorized invite, self-hosted operation has no signup, and account identity and cell placement remain control-plane authoritative. [runbooks.md](../docs/runbooks.md), [account-signup-runtime.mjs](../infra/cloudflare/control-plane/src/account-signup-runtime.mjs) |
| Bounds / abuse | **CONDITIONAL** | Inputs, invite use, verification and recovery state, retries, and pending-account lifetime are bounded, but a durable account-wide signup, resend, and recovery abuse limiter is not implemented. [post-v0-roadmap.md](../docs/post-v0-roadmap.md), [account-signup-runtime.mjs](../infra/cloudflare/control-plane/src/account-signup-runtime.mjs) |
| Observability | **CONDITIONAL** | Durable phases and value-free logs are inspectable without continuous funnel, delivery, pending-age, recovery-failure, or abuse alerts and a tested receiver. [observability-and-operations.md](../docs/observability-and-operations.md), [account-signup-runtime.mjs](../infra/cloudflare/control-plane/src/account-signup-runtime.mjs) |
| Recovery | **CONDITIONAL** | Durable signup phases, exact idempotency, target-cell acknowledgements, and retry-safe CLI state are tested; credential-recovery and email-change undo paths still need direct recovery tests. [account_create_recovery_test.go](../cmd/witself/account_create_recovery_test.go), [index.js](../infra/cloudflare/control-plane/src/index.js), [account-signup-runtime.test.mjs](../infra/cloudflare/control-plane/test/account-signup-runtime.test.mjs) |
| Rollout / canaries | **CONDITIONAL** | Signup remains invite-gated and the repository does not retain a current release-specific end-to-end signup, verification, recovery, and undo canary. [runbooks.md](../docs/runbooks.md) |
| Docs / support | **PASS** | Customer setup, pending verification, resend, credential recovery, email change, undo, and incomplete-signup cleanup are documented. [cli-command-surface.md](../docs/cli-command-surface.md), [runbooks.md](../docs/runbooks.md) |

Open gates:

- `signup-abuse-limits` (bounds / abuse): Add durable account, invite, address, and source-rate controls for signup, verification resend, recovery, and email-change attempts without enabling account enumeration. ([tracking/evidence](../docs/post-v0-roadmap.md))
- `signup-lifecycle-contract-tests` (behavior, recovery): Add direct Worker contract and interruption tests for verification, resend, credential recovery, email change, undo, and expiration behavior. ([tracking/evidence](../infra/cloudflare/control-plane/src/index.js))
- `signup-operations-alerting` (observability): Connect value-free phase, delivery, pending-age, recovery, and abuse signals to continuous dashboards and a tested external receiver. ([tracking/evidence](../docs/observability-and-operations.md))
- `signup-release-canary` (rollout / canaries): Retain a current invite-through-active-account canary plus resend, recovery, email-change undo, crash-resume, and cleanup evidence before widening signup. ([tracking/evidence](../docs/runbooks.md))

<a id="agent-avatars"></a>

### Agent avatars

Versioned portable SVG identity, autonomy policy, style rollout, continuity guards, reset, rollback, archive, and quota accounting are implemented; payload compaction and live acceptance remain staged.

- Implementation: `implemented`
- Managed rollout: `limited`
- Readiness: **conditional**
- Detailed docs: [agent-avatars.md](../docs/agent-avatars.md)

| Gate | State | Current evidence and conclusion |
|---|---|---|
| Behavior | **PASS** | Proposal, activation, evolution, reset, rollback, style, archive, and continuity behavior are implemented across store, API, CLI, and MCP. [agent-avatars.md](../docs/agent-avatars.md), [avatar_quota_integration_test.go](../internal/store/avatar_quota_integration_test.go) |
| Entitlement / policy | **PASS** | Agent-self-managed, agent-proposes, and operator-only autonomy policies are enforced with exact revisions and immutable history. [agent-avatars.md](../docs/agent-avatars.md) |
| Bounds / abuse | **PASS** | SVG sanitization, structural continuity checks, bounded canonical rendering, quota accounting, and fail-closed cleanup protect storage and identity. [agent-avatars.md](../docs/agent-avatars.md), [continuity.go](../internal/avatar/continuity.go) |
| Observability | **CONDITIONAL** | Lifecycle metrics and audit events exist, but no end-to-end production alert path or avatar-specific SLO is retained. [agent-avatars.md](../docs/agent-avatars.md), [observability-and-operations.md](../docs/observability-and-operations.md) |
| Recovery | **PASS** | Immutable versions, rollback, reset, archive/import, and deterministic placeholders provide recovery paths without deleting history. [agent-avatars.md](../docs/agent-avatars.md), [backup-and-recovery.md](../docs/backup-and-recovery.md) |
| Rollout / canaries | **CONDITIONAL** | Bounded style rollout is active with the worker, while payload compaction remains off and no release-specific live acceptance record is retained. [values.yaml](../charts/witself-server/values.yaml), [agent-avatars.md](../docs/agent-avatars.md) |
| Docs / support | **PASS** | The identity model, autonomy rules, lifecycle, continuity, safety boundaries, and operator surfaces are documented. [agent-avatars.md](../docs/agent-avatars.md), [runbooks.md](../docs/runbooks.md) |

Open gates:

- `avatar-operations-alerting` (observability): Connect value-free avatar lifecycle, failure, compaction, and quota metrics to an avatar SLO and tested external alert receiver. ([tracking/evidence](../docs/observability-and-operations.md))
- `live-avatar-acceptance` (rollout / canaries): Retain a release-specific live lifecycle and archive/restore acceptance record. ([tracking/evidence](../docs/agent-avatars.md))
- `payload-compaction` (rollout / canaries): Activate and verify payload compaction before cleanup-dependent mutations can be generally available. ([tracking/evidence](../docs/agent-avatars.md))

<a id="agent-collaboration"></a>

### Agent collaboration requests

Same-realm open requests, offers, assignment, results, claims, and foreground processing are implemented on the durable messaging graph; cross-realm federation and wake behavior are separate roadmap work.

- Implementation: `implemented`
- Managed rollout: `limited`
- Readiness: **conditional**
- Plan feature keys: `collaboration`
- Detailed docs: [agent-collaboration.md](../docs/agent-collaboration.md), [autonomous-realm-messaging.md](../docs/autonomous-realm-messaging.md)

| Gate | State | Current evidence and conclusion |
|---|---|---|
| Behavior | **PASS** | Request, offer, selection, assignment, result, claim, acknowledge, release, and escalation state machines are implemented and tested. [autonomous-realm-messaging.md](../docs/autonomous-realm-messaging.md), [message_request_hardening_integration_test.go](../internal/store/message_request_hardening_integration_test.go) |
| Entitlement / policy | **CONDITIONAL** | Same-realm authority and claim fences are enforced, but request-graph operations currently check messaging rather than the separately cataloged collaboration feature key. [billing-and-limits.md](../docs/billing-and-limits.md), [message_feature_gate_integration_test.go](../internal/store/message_feature_gate_integration_test.go) |
| Bounds / abuse | **PASS** | Bounded payloads, claim leases, deterministic failure escalation, request state transitions, and messaging rate limits constrain work. [autonomous-realm-messaging.md](../docs/autonomous-realm-messaging.md), [inter-agent-messaging.md](../docs/inter-agent-messaging.md) |
| Observability | **CONDITIONAL** | Value-free message and worker metrics exist, but continuous scrape, alert routing, and a tested receiver are absent. [observability-and-operations.md](../docs/observability-and-operations.md) |
| Recovery | **PASS** | Durable canonical mailboxes, exact claim fences, retryable releases, request history, and archive/import preserve work across failures. [autonomous-realm-messaging.md](../docs/autonomous-realm-messaging.md), [backup-and-recovery.md](../docs/backup-and-recovery.md) |
| Rollout / canaries | **CONDITIONAL** | The same-realm substrate has operational evidence, but the request graph lacks a current release-specific live acceptance record. [autonomous-realm-messaging.md](../docs/autonomous-realm-messaging.md) |
| Docs / support | **PASS** | Protocol roles, foreground constraints, untrusted-input boundaries, failure handling, and deferred federation are documented. [agent-collaboration.md](../docs/agent-collaboration.md), [autonomous-realm-messaging.md](../docs/autonomous-realm-messaging.md) |

Open gates:

- `collaboration-entitlement-enforcement` (entitlement / policy): Define collaboration as bundled metadata or independently enforce its plan key on request-graph operations without breaking existing snapshots. ([tracking/evidence](../internal/store/message_feature_gate_integration_test.go))
- `current-live-request-canary` (rollout / canaries): Retain a current-release offer, assignment, result, retry, and escalation canary. ([tracking/evidence](../docs/autonomous-realm-messaging.md))
- `external-alert-path` (observability): Connect message and worker metrics to continuous alerts with a tested receiver. ([tracking/evidence](../docs/observability-and-operations.md))

<a id="agent-dashboard"></a>

### Local Agent Console

The loopback-only per-agent Agent Console is `witself dashboard`, with passive or observational projections for identity, cell-applied plan entitlements, transcripts, memories, facts, messaging, received/sent email metadata, email capacity, secrets metadata, and local preferences.

- Implementation: `implemented`
- Managed rollout: `not applicable`
- Readiness: **conditional**
- Detailed docs: [README.md](../README.md), [agent-console.md](../docs/agent-console.md), [api-routes.md](../docs/api-routes.md), [cli-command-surface.md](../docs/cli-command-surface.md), [0004-local-agent-dashboard.md](../docs/decisions/0004-local-agent-dashboard.md)

| Gate | State | Current evidence and conclusion |
|---|---|---|
| Behavior | **PASS** | Serve, status, stop, live updates, preferences, capability degradation, and all seven advertised panels are implemented, including the live cell-applied entitlement card and independent read-only Received and Sent email projections. [README.md](../README.md), [dashboard_test.go](../cmd/witself/dashboard_test.go), [dashboard_test.go](../internal/dashboard/dashboard_test.go) |
| Entitlement / policy | **PASS** | The Console uses exactly one agent token and projects a closed value-free entitlement schema only from that token account's cell-applied snapshot; it performs no catalog, control-plane, billing, provider, account-selector, privileged cross-agent, admin, or domain-mutation operation. [plan_entitlements_test.go](../cmd/witself-server/plan_entitlements_test.go), [agent-console.md](../docs/agent-console.md), [0004-local-agent-dashboard.md](../docs/decisions/0004-local-agent-dashboard.md), [dashboard_test.go](../internal/dashboard/dashboard_test.go), [self_test.go](../internal/server/self_test.go) |
| Bounds / abuse | **PASS** | Loopback binding, host checks, process-local credentials, method/body caps, CSP, bounded pages, sanitized SVG, a double allow-listed entitlement projection, and strict email projections that remove ids, bodies, provider payloads, billing fields, and action targets constrain the browser boundary. [0004-local-agent-dashboard.md](../docs/decisions/0004-local-agent-dashboard.md), [dashboard_test.go](../internal/dashboard/dashboard_test.go), [self_test.go](../internal/server/self_test.go) |
| Observability | **N/A** | This is an operator-started local foreground process, not an always-on managed service; status, startup errors, and foreground logs are its explicit operator surface. |
| Recovery | **PASS** | Per-process registry claims, stale-process detection, conservative stop behavior, transactional preference persistence, and process-local credentials make crash and restart recovery bounded. [0004-local-agent-dashboard.md](../docs/decisions/0004-local-agent-dashboard.md), [registry_test.go](../internal/dashboard/registry_test.go), [dashboard_preferences_integration_test.go](../internal/store/dashboard_preferences_integration_test.go) |
| Rollout / canaries | **CONDITIONAL** | The command is generally shipped, but no current release-specific macOS, Linux, and Windows acceptance artifact is retained for all seven panels, cell-applied entitlement states, both email directions, and disabled or unavailable states. [release.yml](../.github/workflows/release.yml), [agent-console.md](../docs/agent-console.md), [0004-local-agent-dashboard.md](../docs/decisions/0004-local-agent-dashboard.md) |
| Docs / support | **PASS** | The command, presentation matrix, cell-applied entitlement schema and compatibility states, domain-ownership boundary, local lifecycle, received/sent email projections, and distinction from the fleet-admin TUI and any future hosted console are documented. [README.md](../README.md), [agent-console.md](../docs/agent-console.md), [cli-command-surface.md](../docs/cli-command-surface.md), [0004-local-agent-dashboard.md](../docs/decisions/0004-local-agent-dashboard.md) |

Open gates:

- `dashboard-release-acceptance` (rollout / canaries): Retain current cross-platform release acceptance for serve, status, stop, browser authentication, all seven panels, cell-applied entitlement states, independent received/sent email metadata, live updates, strict redaction, and graceful disabled or unavailable states. ([tracking/evidence](../docs/agent-console.md))

<a id="agent-email-receive"></a>

### Agent email receive

Inbound email on witmail.net is a production service for the exact Founder cohort, with account entitlements, canonical addressing, bounded storage, retention, rate gates, durable claims, and Cloudflare-to-cell route authority.

- Implementation: `implemented`
- Managed rollout: `limited`
- Readiness: **conditional**
- Retained release/cohort evidence: `v0.0.253` — Exact Founder receive deployment and storage postflight; a fresh real-mail canary remains open.
- Plan feature keys: `agent_email_receive`
- Plan limit keys: `agent_email_attachment_storage_bytes`, `agent_email_max_raw_bytes`, `agent_email_received_bytes_per_realm_minute`, `agent_email_received_bytes_per_recipient_minute`, `agent_email_received_bytes_per_sender_minute`, `agent_email_received_per_realm_minute`, `agent_email_received_per_recipient_minute`, `agent_email_received_per_sender_minute`
- Plan policy keys: `agent_email_entitlement_version`, `agent_email_retention_days`
- Detailed docs: [agent-email.md](../docs/agent-email.md), [billing-and-limits.md](../docs/billing-and-limits.md)

| Gate | State | Current evidence and conclusion |
|---|---|---|
| Behavior | **PASS** | Canonical delivery, disabled-account discard, durable storage, listen/claim/read/ack/release, retention, and move-safe routing are implemented and tested. [agent-email.md](../docs/agent-email.md), [test-agent-email-cell-smoke.sh](../scripts/test-agent-email-cell-smoke.sh) |
| Entitlement / policy | **PASS** | Personal is disabled, paid-plan policy is resolved per account, Founder is explicitly allowlisted, and clients need no reinstall when policy changes. [agent-email.md](../docs/agent-email.md), [billing-and-limits.md](../docs/billing-and-limits.md) |
| Bounds / abuse | **CONDITIONAL** | Raw-size, byte, sender, recipient, realm, account, storage-ledger, root-count, and hard-row ceilings are enforced; trusted sender authentication, spam classification, and provider-wide pressure handling remain incomplete. [agent-email.md](../docs/agent-email.md), [threat-model.md](../docs/threat-model.md) |
| Observability | **CONDITIONAL** | Workers Observability, cell metrics, storage gauges, health, logs, and audits exist, but continuous capacity alerts and a tested external receiver do not. [agent-email.md](../docs/agent-email.md), [observability-and-operations.md](../docs/observability-and-operations.md) |
| Recovery | **CONDITIONAL** | Route journals, signed projections, retry-safe ingress, claims, retention, and archive/import are implemented; a populated schema-91 email account move and restore drill remains required. [agent-email.md](../docs/agent-email.md), [backup-and-recovery.md](../docs/backup-and-recovery.md) |
| Rollout / canaries | **CONDITIONAL** | Release v0.0.253 is live for the Founder cohort with exact edge and cell gates, but broad-plan enrollment and a fresh real-mail canary remain open. [agent-email.md](../docs/agent-email.md), [tag #v0.0.253](https://github.com/witwave-ai/witself/releases/tag/v0.0.253) |
| Docs / support | **PASS** | Architecture, account behavior, addresses, bounds, operations, routing, recovery, security, and rollout state are documented as production rather than pilot. [agent-email.md](../docs/agent-email.md), [runbooks.md](../docs/runbooks.md) |

Open gates:

- `continuous-capacity-alerting` (observability): Connect logical and PVC capacity metrics to continuous alerts and a tested external receiver. ([tracking/evidence](../docs/observability-and-operations.md))
- `disabled-account-edge-preflight` (bounds / abuse): Add signed disposition/preflight so mail for disabled accounts is rejected before raw MIME is buffered or relayed. ([tracking/evidence](../docs/agent-email.md))
- `email-move-recovery-canary` (recovery): Move and restore populated schema-91 email state, including inbound, accepted/delivered sends, provider events, suppressions, claims, and provider-started refusal. ([tracking/evidence](../docs/cell-worker.md))
- `fresh-real-mail-canary` (rollout / canaries): Retain a v0.0.253-or-later real inbound mail canary through claim, read, acknowledge, and retention accounting. ([tracking/evidence](../docs/agent-email.md))
- `inbound-abuse-classification` (bounds / abuse): Define and verify sender authentication, spam/reputation policy, and provider-wide backpressure before cohort expansion. ([tracking/evidence](../docs/threat-model.md))

<a id="agent-email-send"></a>

### Agent email send

Outbound agent email is production-deployed for the Founder cohort through a signed cell adapter and isolated Cloudflare dispatch worker with receipts, provider lifecycle events, suppression, and bounded admission.

- Implementation: `implemented`
- Managed rollout: `limited`
- Readiness: **conditional**
- Retained release/cohort evidence: `v0.0.253` — Exact Founder dispatch deployment and lifecycle postflight; a fresh provider-delivery canary remains open.
- Plan feature keys: `agent_email_send`
- Plan limit keys: `agent_email_sent_per_agent_minute`, `agent_email_sent_per_realm_minute`
- Detailed docs: [agent-email.md](../docs/agent-email.md), [billing-and-limits.md](../docs/billing-and-limits.md)

| Gate | State | Current evidence and conclusion |
|---|---|---|
| Behavior | **PASS** | Prepare, enqueue, signed dispatch, provider acceptance, receipts, event delivery, suppression, reply routing, and deterministic disabled responses are implemented and tested. [agent-email.md](../docs/agent-email.md), [test-agent-email-receipt-proof.sh](../scripts/test-agent-email-receipt-proof.sh) |
| Entitlement / policy | **PASS** | Send is independently gated from receive, disabled for Personal and Professional, and enabled only by resolved account policy and exact rollout cohort. [agent-email.md](../docs/agent-email.md), [billing-and-limits.md](../docs/billing-and-limits.md) |
| Bounds / abuse | **CONDITIONAL** | Per-agent, realm, account, recipient, minute, day, edge, database, payload, and suppression limits are enforced; provider-wide reputation and pressure controls remain a widening gate. [agent-email.md](../docs/agent-email.md), [threat-model.md](../docs/threat-model.md) |
| Observability | **CONDITIONAL** | Workers Observability, queue metrics, lifecycle events, cell metrics, storage gauges, logs, and health exist without continuous alert routing to a tested receiver. [agent-email.md](../docs/agent-email.md), [observability-and-operations.md](../docs/observability-and-operations.md) |
| Recovery | **CONDITIONAL** | Idempotent send records, queue retries, receipts, lifecycle reconciliation, DLQ, retention, and archive/import are implemented; populated schema-91 email movement and restore remain unproven. [agent-email.md](../docs/agent-email.md), [README.md](../infra/cloudflare/agent-email-send/README.md) |
| Rollout / canaries | **CONDITIONAL** | Release v0.0.253 is live for the Founder cohort with dispatch and lifecycle events enabled, but broad enrollment and a fresh end-to-end delivery canary remain open. [agent-email.md](../docs/agent-email.md), [tag #v0.0.253](https://github.com/witwave-ai/witself/releases/tag/v0.0.253) |
| Docs / support | **PASS** | Dispatch, provider lifecycle, limits, failure semantics, operations, recovery, and production topology are documented. [agent-email.md](../docs/agent-email.md), [README.md](../infra/cloudflare/agent-email-send/README.md) |

Open gates:

- `continuous-delivery-alerting` (observability): Connect queue, outcome, storage, and worker metrics to continuous alerts and a tested external receiver. ([tracking/evidence](../docs/observability-and-operations.md))
- `email-move-recovery-canary` (recovery): Move and restore populated schema-91 email state, including accepted/delivered sends, provider events, suppressions, claims, and provider-started refusal. ([tracking/evidence](../docs/cell-worker.md))
- `fresh-delivery-canary` (rollout / canaries): Retain a v0.0.253-or-later real send through accepted and delivered provider lifecycle states. ([tracking/evidence](../docs/agent-email.md))
- `provider-wide-backpressure` (bounds / abuse): Add and exercise shared-domain capacity and reputation budgets plus a provider-wide pause, then review the cell, account-policy, adapter, queue, retention, and provider-capacity envelope before widening. ([tracking/evidence](../docs/agent-email.md))

<a id="agent-self-context"></a>

### Agent self, context, and foreground hydration

The bounded self digest, self card, identity and capacity projections, memory, message, email, and avatar checkpoints, plus managed model-visible hook hydration are implemented; automatic delivery remains runtime-dependent and operationally limited.

- Implementation: `implemented`
- Managed rollout: `limited`
- Readiness: **conditional**
- Detailed docs: [context-hydration.md](../docs/context-hydration.md), [mcp-tools.md](../docs/mcp-tools.md)

| Gate | State | Current evidence and conclusion |
|---|---|---|
| Behavior | **PASS** | Self show, self card, bounded facts and memories, capacities, value-free checkpoints, elision, filters, and installed hook hydration are implemented and tested. [context-hydration.md](../docs/context-hydration.md), [self_test.go](../internal/server/self_test.go) |
| Entitlement / policy | **PASS** | Identity is token-derived, broad sensitive values stay redacted, checkpoints are content-free, and disabled messaging or email is represented without changing the installed client toolset. [context-hydration.md](../docs/context-hydration.md), [threat-model.md](../docs/threat-model.md) |
| Bounds / abuse | **PASS** | Fact, memory, snippet, tag, checkpoint, output, hook-time, and rendered-context bounds prevent the digest from becoming an unbounded prompt or value-bearing control channel. [context-hydration.md](../docs/context-hydration.md), [self_test.go](../internal/server/self_test.go) |
| Observability | **CONDITIONAL** | Integration verification and value-free hook state exist without recurring hydration-success, latency, staleness, truncation, or provider-regression alerts. [observability-and-operations.md](../docs/observability-and-operations.md), [provider-integration-certification.md](../docs/provider-integration-certification.md) |
| Recovery | **PASS** | The self projection is rebuilt from canonical rows, hook installation is transactional, unavailable optional sections fail open, and installed guidance provides an MCP fallback. [context-hydration.md](../docs/context-hydration.md), [provider-integration-certification.md](../docs/provider-integration-certification.md) |
| Rollout / canaries | **CONDITIONAL** | Core self reads are released, but automatic model-visible delivery and foreground checkpoint handling lack current signed-in acceptance across every advertised runtime and operating system. [context-hydration.md](../docs/context-hydration.md), [provider-integration-certification.md](../docs/provider-integration-certification.md) |
| Docs / support | **PASS** | Digest shape, privacy boundary, runtime differences, hook behavior, checkpoint semantics, foreground processing, and future file bridge are documented. [context-hydration.md](../docs/context-hydration.md), [mcp-tools.md](../docs/mcp-tools.md) |

Open gates:

- `self-hydration-operations` (observability): Add recurring value-free hydration success, latency, staleness, truncation, and provider-regression evidence with a tested alert path. ([tracking/evidence](../docs/observability-and-operations.md))
- `self-hydration-runtime-acceptance` (rollout / canaries): Retain signed-in acceptance for automatic and guided hydration, every checkpoint, disabled-feature transitions, and MCP fallback across the supported runtime matrix. ([tracking/evidence](../docs/provider-integration-certification.md))

<a id="audit-trail-retention"></a>

### Account audit trail and retention

The value-free account event registry, durable audit rows, owner-facing account events API and CLI, filtering, pagination, and authorization are implemented; retention modes, scheduled cleanup, export, and plan policy remain a building slice.

- Implementation: `building`
- Managed rollout: `limited`
- Readiness: **not ready**
- Detailed docs: [audit-retention.md](../docs/audit-retention.md), [cli-command-surface.md](../docs/cli-command-surface.md)

| Gate | State | Current evidence and conclusion |
|---|---|---|
| Behavior | **CONDITIONAL** | Event append, registry validation, account list, filters, pagination, and authorization work; delete, archive, hold, status, export, and scheduled retention do not. [audit-retention.md](../docs/audit-retention.md), [events_test.go](../internal/store/events_test.go) |
| Entitlement / policy | **CONDITIONAL** | Account-scoped read authorization is enforced, but the specified per-plan retention policy, operator modes, and legal-hold authority are not represented in the canonical plan contract. [audit-retention.md](../docs/audit-retention.md), [plans.json](../web/plans/plans.json) |
| Bounds / abuse | **CONDITIONAL** | Event shape, metadata, labels, filters, time range, and pages are bounded and value-free; live audit volume remains unbounded because no retention worker or storage ceiling is implemented. [audit-retention.md](../docs/audit-retention.md), [events.go](../internal/store/events.go) |
| Observability | **CONDITIONAL** | Audit is itself inspectable, but append failures, registry drift, backlog, oldest-row age, storage growth, export, and retention health have no continuous SLO and alert path. [audit-retention.md](../docs/audit-retention.md), [observability-and-operations.md](../docs/observability-and-operations.md) |
| Recovery | **CONDITIONAL** | Canonical rows survive ordinary database backup and account movement, while the specified archive, hold, export, and retention-failure recovery paths are not implemented or drilled. [audit-retention.md](../docs/audit-retention.md), [backup-and-recovery.md](../docs/backup-and-recovery.md) |
| Rollout / canaries | **CONDITIONAL** | The account events surface is released, but there is no dark retention worker, selected retention cohort, or release-specific append, query, expiry, archive, and hold canary. [audit-retention.md](../docs/audit-retention.md) |
| Docs / support | **CONDITIONAL** | The event registry and intended retention contract are extensive, but the document still presents unimplemented defaults, modes, export, and commands as decisions rather than separating current core from target behavior. [audit-retention.md](../docs/audit-retention.md) |

Open gates:

- `audit-doc-reconciliation` (docs / support): Separate the implemented account-events core from target retention, export, hold, metering, and command behavior throughout the audit contract. ([tracking/evidence](../docs/audit-retention.md))
- `audit-operations-alerting` (observability): Connect append failures, registry drift, backlog, oldest age, storage, export, and retention health to continuous metrics and a tested receiver. ([tracking/evidence](../docs/observability-and-operations.md))
- `audit-retention-enforcement` (behavior, bounds / abuse, entitlement / policy, rollout / canaries): Add a canonical retention policy, bounded worker, delete/archive/hold modes, storage safety, admin override, tests, dark cohort, and expiry canary. ([tracking/evidence](../docs/audit-retention.md))
- `audit-retention-recovery` (recovery): Implement and drill audit export, archive verification, hold preservation, interrupted sweep recovery, account movement, and restore reconciliation. ([tracking/evidence](../docs/backup-and-recovery.md))

<a id="billing-plan-transitions"></a>

### Billing and plan transitions

The Personal-to-Professional Stripe sandbox transition and exact Professional-to-Personal downgrade foundation are implemented behind an empty account cohort, but no retained end-to-end sandbox canary exists and production charging and webhooks remain dark.

- Implementation: `building`
- Managed rollout: `dark`
- Readiness: **not ready**
- Detailed docs: [billing-and-limits.md](../docs/billing-and-limits.md), [billing-transition-rollout.md](../docs/billing-transition-rollout.md)

| Gate | State | Current evidence and conclusion |
|---|---|---|
| Behavior | **CONDITIONAL** | Personal-to-Professional preview, setup, checkout and apply plus exact Professional-to-Personal fit, scheduling, cancellation, receipts, recovery, and reconciliation are implemented; Team, Enterprise, paid-to-paid transitions, automated dunning policy or collection mutations, and refund mutations remain out. [billing-and-limits.md](../docs/billing-and-limits.md), [billing_mutations_test.go](../internal/billing/lifecycle/billing_mutations_test.go) |
| Entitlement / policy | **PASS** | The control plane owns billing truth and resolves immutable cell snapshots; plan and account overrides remain separate and auditable. [billing-and-limits.md](../docs/billing-and-limits.md), [plans.json](../web/plans/plans.json) |
| Bounds / abuse | **CONDITIONAL** | Idempotency, exact provider cancellation, recovery fencing, a complete fenced count-only R2 collector with a canonical capture wrapper, and exact-reader preflight exist; receipt retention, operator terminalization, and retained production cutover proof remain incomplete. [billing_rollout_inventory_test.go](../cmd/witself-control-plane/billing_rollout_inventory_test.go), [billing-transition-rollout.md](../docs/billing-transition-rollout.md), [billing-rollout-source-fence.test.mjs](../infra/cloudflare/control-plane/test/billing-rollout-source-fence.test.mjs), [rollout_inventory_test.go](../internal/billing/lifecycle/rollout_inventory_test.go), [billing-transition-rollout-preflight.sh](../scripts/billing-transition-rollout-preflight.sh), [capture-billing-rollout-inventory.sh](../scripts/capture-billing-rollout-inventory.sh), [test-billing-transition-rollout-preflight.sh](../scripts/test-billing-transition-rollout-preflight.sh), [test-capture-billing-rollout-inventory.sh](../scripts/test-capture-billing-rollout-inventory.sh) |
| Observability | **CONDITIONAL** | Value-free billing state, receipts, usage, and reconciler metrics exist; production alerts, provider event dashboards, and support escalation are not connected. [billing-and-limits.md](../docs/billing-and-limits.md), [observability-and-operations.md](../docs/observability-and-operations.md) |
| Recovery | **CONDITIONAL** | Crash-resumable exact mutation recovery and exact-subscription failed/recovered projection are implemented, but a real-provider activation/forward-fix drill, restore reconciliation, automated dunning policy or collection mutations, and terminal operator handling of ambiguous old work remain unproven. [billing-and-limits.md](../docs/billing-and-limits.md), [billing_durability_test.go](../internal/billing/lifecycle/billing_durability_test.go) |
| Rollout / canaries | **CONDITIONAL** | The transition foundation and fenced inventory collector are deliberately dark behind an empty account cohort; real owned HTTPS return routes and a retained Personal-to-Professional-to-Personal Stripe canary are absent, and production charging and webhooks are disabled. [billing-and-limits.md](../docs/billing-and-limits.md), [billing-transition-rollout.md](../docs/billing-transition-rollout.md), [test-billing-transition-rollout-preflight.sh](../scripts/test-billing-transition-rollout-preflight.sh) |
| Docs / support | **PASS** | Authority boundaries, supported transition scope, durable mutation design, v0.0.254 incompatibility, the canonical wrapper-owned prior/drain/BEFORE/scan/AFTER ceremony, quarantine, forward-fix-only cutover, and remaining provider gates are documented. [billing-and-limits.md](../docs/billing-and-limits.md), [billing-transition-rollout.md](../docs/billing-transition-rollout.md), [runbooks.md](../docs/runbooks.md) |

Open gates:

- `billing-operations` (observability, recovery): Connect billing metrics, alerts, receipt retention, support escalation, and operator recovery. ([tracking/evidence](../docs/billing-and-limits.md))
- `billing-safety-completion` (bounds / abuse, recovery): Complete bounded completed-receipt retention and operator terminalization of deterministic provider failures without clearing ambiguous work. ([tracking/evidence](../docs/billing-and-limits.md))
- `billing-v254-exclusive-cutover` (bounds / abuse, recovery, rollout / canaries): Execute the fenced complete R2 inventory against the exact production authority; bind the exact-reader release, application, and image; prove an empty cohort, zero writers, and zero hazards; fully drain v0.0.254; forbid rollback; retain cutover and forward-fix evidence. ([tracking/evidence](../docs/billing-transition-rollout.md))
- `full-lifecycle-reconciliation` (behavior, recovery): Complete Team and Enterprise transitions, paid-to-paid compensation, automated dunning policy or collection mutations, refund mutations, and restore reconciliation. ([tracking/evidence](https://github.com/witwave-ai/witself/issues/33))
- `stripe-sandbox-acceptance` (rollout / canaries): Finish Stripe sandbox secrets, owned HTTPS return surfaces, hosted portal and webhook verification, then retain an end-to-end Personal-to-Professional-to-Personal transition and forward-fix canary; Team, Enterprise, and refund mutations remain excluded. ([tracking/evidence](../docs/billing-transition-rollout.md))

<a id="custom-email-domains"></a>

### Custom inbound email domains

Domain request, allowlist, ownership verification, durable journal, recovery, CLI/admin, plan, and route-projection contracts exist, while customer DNS and live delivery remain intentionally dark.

- Implementation: `building`
- Managed rollout: `dark`
- Readiness: **not ready**
- Plan feature keys: `agent_email_custom_domain`
- Plan limit keys: `agent_email_custom_domains_per_account`
- Detailed docs: [agent-email.md](../docs/agent-email.md), [billing-and-limits.md](../docs/billing-and-limits.md)

| Gate | State | Current evidence and conclusion |
|---|---|---|
| Behavior | **CONDITIONAL** | Request, proof, activation journal, recovery, projection, and retirement mechanics exist, but no real DNS/MX/customer-zone delivery has completed. [agent-email.md](../docs/agent-email.md), [agent-email-domain-journal.test.mjs](../infra/cloudflare/control-plane/test/agent-email-domain-journal.test.mjs) |
| Entitlement / policy | **PASS** | Feature entitlement, per-account count, administrative allowlist, authority, and independent inbound-email gate are explicit. [agent-email.md](../docs/agent-email.md), [billing-and-limits.md](../docs/billing-and-limits.md) |
| Bounds / abuse | **CONDITIONAL** | Normalized domains, reserved names, ownership proof, count limits, idempotency, and route authority exist; global authority capacity/monitoring and transfer/quarantine governance remain activation gates. [agent-email.md](../docs/agent-email.md), [threat-model.md](../docs/threat-model.md) |
| Observability | **CONDITIONAL** | Journal and route state are inspectable, but DNS verification, delivery, expiration, and capacity alerts are not connected to a tested receiver. [agent-email.md](../docs/agent-email.md), [observability-and-operations.md](../docs/observability-and-operations.md) |
| Recovery | **CONDITIONAL** | Recovery journals and route retirement are tested with fakes; authority bootstrap, restore, and customer DNS recovery need a live drill. [agent-email.md](../docs/agent-email.md), [agent-email-domain-journal.test.mjs](../infra/cloudflare/control-plane/test/agent-email-domain-journal.test.mjs) |
| Rollout / canaries | **CONDITIONAL** | All managed activation gates remain dark and no customer domain routes mail to an agent. [agent-email.md](../docs/agent-email.md) |
| Docs / support | **PASS** | Authority, proof, lifecycle, limits, recovery, security, and dark rollout boundaries are documented. [agent-email.md](../docs/agent-email.md), [billing-and-limits.md](../docs/billing-and-limits.md) |

Open gates:

- `alias-dependency-and-plan-replay` (behavior, rollout / canaries): Require an already-authorized realm alias and exercise refresh/claim plus exact plan-lifecycle replay before domain activation. ([tracking/evidence](../docs/agent-email.md))
- `authority-capacity-monitoring` (bounds / abuse, observability): Prove journal health, sealed empty-target restore, and live head-capacity monitoring below the global 10,000-authority-key ceiling. ([tracking/evidence](../docs/agent-email.md))
- `custom-domain-canary` (behavior, recovery, rollout / canaries): Complete a human-controlled domain request, DNS proof, MX route, delivery, retirement, and recovery canary. ([tracking/evidence](../docs/agent-email.md))
- `dns-provider-lane` (observability, rollout / canaries): Implement the production DNS/MX provisioning and verification lane with expiry and alerting. ([tracking/evidence](../docs/agent-email.md))
- `route-authority-drill` (recovery): Exercise authority bootstrap, backup restore, route republishing, and conflict recovery. ([tracking/evidence](../docs/agent-email.md))
- `transfer-quarantine-governance` (behavior, bounds / abuse, recovery): Define and verify ownership transfer, quarantine, conflict, suspension, and retirement governance before customer activation. ([tracking/evidence](../docs/agent-email.md))

<a id="facts"></a>

### Durable facts

Stable subjects, immutable assertions, candidates, typed values, primary resolution, guarded permanent deletion, archive/import, and per-agent fact limits are implemented; advanced conflict and cross-agent policy remain later work.

- Implementation: `implemented`
- Managed rollout: `limited`
- Readiness: **conditional**
- Plan feature keys: `facts`
- Plan limit keys: `stored_fact`
- Detailed docs: [fact-service.md](../docs/fact-service.md), [facts-model.md](../docs/facts-model.md)

| Gate | State | Current evidence and conclusion |
|---|---|---|
| Behavior | **PASS** | Subject resolution, assertions, candidates, review, primary selection, typed values, deletion preview/apply, and archive/import are implemented across store, API, CLI, and MCP. [fact-service.md](../docs/fact-service.md), [facts-model.md](../docs/facts-model.md) |
| Entitlement / policy | **PASS** | Per-agent stored_fact limits and owner-authorized sensitive reveal and permanent deletion boundaries are enforced. [billing-and-limits.md](../docs/billing-and-limits.md), [fact-service.md](../docs/fact-service.md) |
| Bounds / abuse | **PASS** | Typed-value validation, stable addresses, active-fact counting, revision fences, candidate bounds, and value-safe usage records bound the service. [fact-service.md](../docs/fact-service.md), [facts-model.md](../docs/facts-model.md) |
| Observability | **CONDITIONAL** | Value-free usage and audit records exist, but fact-specific SLOs and an external alert path are not retained. [fact-service.md](../docs/fact-service.md), [observability-and-operations.md](../docs/observability-and-operations.md) |
| Recovery | **PASS** | Immutable assertion history, candidate review, reversible changes, archive/import, and explicit permanent-delete fencing provide recovery semantics. [backup-and-recovery.md](../docs/backup-and-recovery.md), [fact-service.md](../docs/fact-service.md) |
| Rollout / canaries | **CONDITIONAL** | Core behavior is in released code, while the guarded permanent-delete path remains disabled in the active managed cohort and lacks current live acceptance evidence. [fact-service.md](../docs/fact-service.md), [runbooks.md](../docs/runbooks.md) |
| Docs / support | **CONDITIONAL** | Core facts and deletion are documented, but advanced conflict authority, predicate registries, reminder delivery, and cross-agent policy remain explicitly unresolved. [fact-service.md](../docs/fact-service.md), [facts-model.md](../docs/facts-model.md) |

Open gates:

- `advanced-fact-policy` (docs / support): Resolve authority/conflict policy, predicate registries, reminder delivery, and cross-agent fact access before declaring the broader model complete. ([tracking/evidence](../docs/facts-model.md))
- `fact-delete-canary` (rollout / canaries): Enable the guarded path for a selected cohort and retain a current preview/apply/isolation canary. ([tracking/evidence](../docs/fact-service.md))
- `fact-operations` (observability): Define fact SLOs and connect value-free metrics to the shared external alert path. ([tracking/evidence](../docs/observability-and-operations.md))

<a id="fleet-deployment-recovery"></a>

### Fleet deployment, backup, and recovery

Signed releases, immutable images, Helm/GitOps cells, schema checks, encrypted backups, archive validation, and recovery runbooks are implemented; broad mobility and continuous monitoring remain constrained.

- Implementation: `implemented`
- Managed rollout: `limited`
- Readiness: **conditional**
- Retained release/cohort evidence: `v0.0.253` — Signed release publication plus active-cell and edge postflight; broad placement and external monitoring remain open.
- Detailed docs: [backup-and-recovery.md](../docs/backup-and-recovery.md), [deployment-cells.md](../docs/deployment-cells.md), [release-and-build.md](../docs/release-and-build.md)

| Gate | State | Current evidence and conclusion |
|---|---|---|
| Behavior | **PASS** | Build, sign, attest, publish, render, migrate, health-check, archive, backup, restore validation, and GitOps convergence paths are implemented. [backup-and-recovery.md](../docs/backup-and-recovery.md), [release-and-build.md](../docs/release-and-build.md) |
| Entitlement / policy | **PASS** | Control-plane placement authority, accepting-cell policy, cohort gates, schema compatibility, and fleet-admin boundaries are explicit. [deployment-cells.md](../docs/deployment-cells.md), [governance-and-support.md](../docs/governance-and-support.md) |
| Bounds / abuse | **PASS** | Immutable digests, migration locks, bounded workers, schema floors, placement freezes, exact cohorts, encrypted artifact permissions, and a fail-closed roll-cell gate that verifies pre-migration backup evidence before any values edit constrain rollout risk. [deployment-cells.md](../docs/deployment-cells.md), [release-and-build.md](../docs/release-and-build.md) |
| Observability | **CONDITIONAL** | Health and metrics surfaces exist, but the production cells lack continuous Prometheus scraping, PVC collection, Alertmanager routing, and a tested external receiver. [observability-and-operations.md](../docs/observability-and-operations.md) |
| Recovery | **CONDITIONAL** | Encrypted pre-migration backups and disposable restore validation exist; provider PITR, committed production restore, and multi-cell movement drills remain incomplete. [backup-and-recovery.md](../docs/backup-and-recovery.md), [deployment-cells.md](../docs/deployment-cells.md) |
| Rollout / canaries | **CONDITIONAL** | v0.0.253 passed release, active-cell, and edge postflight, while the backup cell remains a non-accepting validation target and placement stays paused. [deployment-cells.md](../docs/deployment-cells.md), [tag #v0.0.253](https://github.com/witwave-ai/witself/releases/tag/v0.0.253) |
| Docs / support | **PASS** | Release, GitOps, migration, backup, restore, cell movement, and incident procedures are documented. [backup-and-recovery.md](../docs/backup-and-recovery.md), [deployment-cells.md](../docs/deployment-cells.md), [runbooks.md](../docs/runbooks.md) |

Open gates:

- `continuous-platform-alerting` (observability): Deploy and exercise the end-to-end metrics, alert routing, and external receiver chain. ([tracking/evidence](../docs/observability-and-operations.md))
- `provider-pitr-drill` (recovery): Enable and retain provider PITR plus a committed production-grade restore rehearsal. ([tracking/evidence](https://github.com/witwave-ai/witself/issues/68))
- `schema-converged-movement` (rollout / canaries): Upgrade every possible accepting destination, verify capacity monitoring, and complete a directed move before resuming placement. ([tracking/evidence](../docs/deployment-cells.md))

<a id="identity-tenancy"></a>

### Accounts, realms, agents, and tokens

Account, realm, agent, token, role, suspension, and transactional realm/agent capacity enforcement form the shared identity spine; current managed evidence is narrow and the legacy total-agent cap remains a compatibility floor.

- Implementation: `implemented`
- Managed rollout: `limited`
- Readiness: **conditional**
- Plan limit keys: `agents`, `agents_per_realm`, `realms`
- Detailed docs: [authorization-and-roles.md](../docs/authorization-and-roles.md), [billing-and-limits.md](../docs/billing-and-limits.md), [data-model.md](../docs/data-model.md)

| Gate | State | Current evidence and conclusion |
|---|---|---|
| Behavior | **PASS** | Account provisioning, realm and agent lifecycle, tokens, roles, suspension, concurrency, deletion, archive/import, and capacity boundaries are implemented and tested. [data-model.md](../docs/data-model.md), [resource_limit_integration_test.go](../internal/store/resource_limit_integration_test.go) |
| Entitlement / policy | **PASS** | Resolved realms, agents, and agents-per-realm limits are transactionally enforced per account with audited overrides and explicit unlimited semantics. [billing-and-limits.md](../docs/billing-and-limits.md), [resource_limit_integration_test.go](../internal/store/resource_limit_integration_test.go) |
| Bounds / abuse | **PASS** | Row locks, exact counters, concurrent creation tests, token hashing, role checks, lifecycle fences, and legacy rollback floors constrain identity mutation. [authorization-and-roles.md](../docs/authorization-and-roles.md), [resource_limit_integration_test.go](../internal/store/resource_limit_integration_test.go) |
| Observability | **CONDITIONAL** | Audit, usage, capacity, token, and lifecycle state are inspectable without a unified identity-capacity dashboard or tested external alert path. [observability-and-operations.md](../docs/observability-and-operations.md) |
| Recovery | **PASS** | Idempotent provisioning, immutable identifiers, token rotation, suspension, archive/import, and lifecycle receipts preserve identity across retries and movement. [backup-and-recovery.md](../docs/backup-and-recovery.md), [token-lifecycle.md](../docs/token-lifecycle.md) |
| Rollout / canaries | **CONDITIONAL** | The identity spine is live and released, but the repository does not retain a current multi-plan capacity and Founder-override acceptance record. [billing-and-limits.md](../docs/billing-and-limits.md), [runbooks.md](../docs/runbooks.md) |
| Docs / support | **PASS** | Identity, roles, token lifecycle, data model, plan limits, admin overrides, and account operations are documented. [authorization-and-roles.md](../docs/authorization-and-roles.md), [billing-and-limits.md](../docs/billing-and-limits.md), [token-lifecycle.md](../docs/token-lifecycle.md) |

Open gates:

- `identity-capacity-alerting` (observability): Add a value-free dashboard and alert path for realm, agent, token, and lifecycle capacity or drift. ([tracking/evidence](../docs/observability-and-operations.md))
- `multi-plan-capacity-canary` (rollout / canaries): Retain current boundary, concurrency, upgrade, downgrade, and Founder-unlimited evidence from managed accounts. ([tracking/evidence](../docs/billing-and-limits.md))

<a id="managed-support"></a>

### Managed support

Durable account-scoped support tickets, messages, state transitions, tenant and fleet-admin CLI/API surfaces, TUI operations, audit, and archive/import are implemented; plan enforcement, operating policy, abuse controls, alerting, and SLA claims remain limited.

- Implementation: `implemented`
- Managed rollout: `limited`
- Readiness: **conditional**
- Plan feature keys: `support`
- Detailed docs: [api-routes.md](../docs/api-routes.md), [cli-command-surface.md](../docs/cli-command-surface.md), [governance-and-support.md](../docs/governance-and-support.md), [self-host-support.md](../docs/self-host-support.md)

| Gate | State | Current evidence and conclusion |
|---|---|---|
| Behavior | **PASS** | Ticket open, list, show, reply, close, bounded state transitions, tenant CLI/API, fleet-admin CLI/TUI, audit, and archive/import are implemented and tested. [api-routes.md](../docs/api-routes.md), [support_test.go](../internal/store/support_test.go) |
| Entitlement / policy | **CONDITIONAL** | Paid plans advertise support, but ticket admission checks the separate support_policy field, whose migration default is enabled and is not synchronized from resolved plan features; Personal can therefore remain enabled unless explicitly overridden. [billing-and-limits.md](../docs/billing-and-limits.md), [0015_add_support_tickets.sql](../internal/store/migrations/0015_add_support_tickets.sql), [plans.json](../web/plans/plans.json) |
| Bounds / abuse | **CONDITIONAL** | Subjects, bodies, list pages, roles, states, and value exposure are bounded, while ticket/message retention, attachment/evidence handling, and intake-rate abuse controls remain incomplete. [security-policy.md](../docs/security-policy.md), [support.go](../internal/store/support.go) |
| Observability | **CONDITIONAL** | The admin TUI exposes ticket state and age, but no continuous response-time, escalation, backlog, or SLA metric and alert path is operationally declared. [tui_model.go](../cmd/witself-admin/tui_model.go), [governance-and-support.md](../docs/governance-and-support.md) |
| Recovery | **PASS** | Tickets and messages are durable, audited, included in account archive/import, and remain available for administrator continuity across account-policy changes. [backup-and-recovery.md](../docs/backup-and-recovery.md), [support_test.go](../internal/store/support_test.go) |
| Rollout / canaries | **CONDITIONAL** | The support surface is implemented, but there is no retained plan-entitlement canary, selected support cohort declaration, or production SLA launch evidence. [billing-and-limits.md](../docs/billing-and-limits.md), [governance-and-support.md](../docs/governance-and-support.md) |
| Docs / support | **CONDITIONAL** | Boundaries and prerequisites are documented, while the promised channels, hours, severity matrix, targets, ownership, and escalation path remain undecided. [governance-and-support.md](../docs/governance-and-support.md), [self-host-support.md](../docs/self-host-support.md) |

Open gates:

- `support-abuse-retention` (bounds / abuse): Define and enforce ticket/message retention, bounded evidence attachments, intake-rate controls, and diagnostic-data handling. ([tracking/evidence](../internal/store/support.go))
- `support-entitlement-enforcement` (entitlement / policy): Resolve paid-plan support entitlement into support_policy while preserving explicit audited account overrides and disabling Personal by default. ([tracking/evidence](../internal/store/migrations/0015_add_support_tickets.sql))
- `support-operating-policy` (docs / support): Decide channels, hours, severities, response targets, ownership, escalation, retention, and customer-visible limitations. ([tracking/evidence](../docs/governance-and-support.md))
- `support-operations-alerting` (observability): Publish value-free backlog and response-time metrics, define the selected support SLOs, and connect them to a tested external escalation path. ([tracking/evidence](../docs/observability-and-operations.md))
- `support-rollout-canary` (rollout / canaries): Run a selected-account case from intake through escalation, resolution, export, retention, and customer closure before advertising the benefit. ([tracking/evidence](../docs/governance-and-support.md))

<a id="narrative-memory"></a>

### Narrative memory and curation

Capture, versioned recall, lexical and client-vector retrieval, fenced client-authored curation, guarded deletion, archive/import, hydration, and per-agent memory limits are implemented but not production-certified across runtimes and clouds.

- Implementation: `implemented`
- Managed rollout: `limited`
- Readiness: **conditional**
- Plan feature keys: `memory`
- Plan limit keys: `stored_memory`
- Detailed docs: [memory-runtime-acceptance.md](../docs/memory-runtime-acceptance.md), [narrative-memory-and-curation.md](../docs/narrative-memory-and-curation.md)

| Gate | State | Current evidence and conclusion |
|---|---|---|
| Behavior | **PASS** | Capture, recall, revision, relations, evidence, curation plan/apply/rollback, hydration, archive/import, and guarded deletion are implemented across store, API, CLI, and MCP. [narrative-memory-and-curation.md](../docs/narrative-memory-and-curation.md), [memory_curation_hardening_integration_test.go](../internal/store/memory_curation_hardening_integration_test.go) |
| Entitlement / policy | **PASS** | Per-agent stored_memory limits, owner authority, sensitive broad-redaction, cross-agent isolation, and explicit deletion authorization are enforced. [billing-and-limits.md](../docs/billing-and-limits.md), [narrative-memory-and-curation.md](../docs/narrative-memory-and-curation.md) |
| Bounds / abuse | **PASS** | Payload, evidence, action, page, lease, revision, vector, result, and active-head bounds protect storage and curation; salience is ranking metadata, not a separate quota. [narrative-memory-and-curation.md](../docs/narrative-memory-and-curation.md), [threat-model.md](../docs/threat-model.md) |
| Observability | **CONDITIONAL** | Value-free memory, curation, worker, and quality evidence schemas exist without continuous production alerts or retained multi-environment SLO evidence. [memory-load-quality.md](../docs/memory-load-quality.md), [observability-and-operations.md](../docs/observability-and-operations.md) |
| Recovery | **PASS** | Immutable versions, rollback, lease recovery, dead-letter handling, evidence preservation, archive/import, and rebuildable search projections provide recovery paths. [backup-and-recovery.md](../docs/backup-and-recovery.md), [narrative-memory-and-curation.md](../docs/narrative-memory-and-curation.md) |
| Rollout / canaries | **CONDITIONAL** | The feature is released and used, but multi-cloud conformance, signed-in provider-runtime acceptance, and retained load/quality certification are incomplete. [memory-cloud-conformance.md](../docs/memory-cloud-conformance.md), [memory-runtime-acceptance.md](../docs/memory-runtime-acceptance.md) |
| Docs / support | **CONDITIONAL** | The current client-side inference model is documented, but historical server-embedding and server-decryption language still needs systematic consistency cleanup. [narrative-memory-and-curation.md](../docs/narrative-memory-and-curation.md), [v0-scope.md](../docs/v0-scope.md) |

Open gates:

- `docs-contract-consistency` (docs / support): Remove or clearly mark stale historical server-inference and sealed-plane claims that conflict with current architecture. ([tracking/evidence](https://github.com/witwave-ai/witself/issues/47))
- `live-runtime-acceptance` (rollout / canaries): Run and retain signed-in acceptance across the supported provider-runtime matrix. ([tracking/evidence](https://github.com/witwave-ai/witself/issues/45))
- `load-quality-certification` (rollout / canaries): Run the deterministic load, relevance, isolation, export, and hybrid-recall thresholds on production-shaped infrastructure. ([tracking/evidence](https://github.com/witwave-ai/witself/issues/46))
- `memory-production-alerting` (observability): Connect value-free memory, curation, quality, backlog, and failure metrics to continuous SLOs and a tested external alert receiver. ([tracking/evidence](../docs/observability-and-operations.md))
- `multi-cloud-conformance` (rollout / canaries): Complete the account-move and archive/restore matrix on AWS, Google Cloud, and Azure targets. ([tracking/evidence](https://github.com/witwave-ai/witself/issues/44))

<a id="operator-authentication"></a>

### Managed operator authentication

Browser PKCE, device-code fallback, secure local session custody, revocation, and managed operator authorization are specified, while the current CLI implements only bootstrap-token-file exchange and agent-token flows.

- Implementation: `specified`
- Managed rollout: `not started`
- Readiness: **not ready**
- Detailed docs: [operator-auth.md](../docs/operator-auth.md)

| Gate | State | Current evidence and conclusion |
|---|---|---|
| Behavior | **CONDITIONAL** | The hosted browser, callback, device-code, status, refresh, logout, and revocation flows are specified but not implemented in the CLI or control plane. [main.go](../cmd/witself/main.go), [operator-auth.md](../docs/operator-auth.md) |
| Entitlement / policy | **CONDITIONAL** | Operator roles and scopes are documented, but a managed human session does not yet carry and refresh that authorization contract. [authorization-and-roles.md](../docs/authorization-and-roles.md), [operator-auth.md](../docs/operator-auth.md) |
| Bounds / abuse | **CONDITIONAL** | PKCE, short-lived codes, no-password CLI handling, revocation, and secure local storage are design requirements without executable replay, phishing, callback, or device-code abuse tests. [operator-auth.md](../docs/operator-auth.md), [threat-model.md](../docs/threat-model.md) |
| Observability | **CONDITIONAL** | No value-free login, refresh, revocation, callback, device-code, or abuse metrics and alert path exist for the target managed flow. [observability-and-operations.md](../docs/observability-and-operations.md), [operator-auth.md](../docs/operator-auth.md) |
| Recovery | **CONDITIONAL** | Session revocation, credential-store fallback, lost-device recovery, and provider outage behavior are specified but not implemented or drilled. [operator-auth.md](../docs/operator-auth.md) |
| Rollout / canaries | **CONDITIONAL** | No dark managed cohort, hosted callback, device-code, multi-platform credential-store, or revocation canary exists. [operator-auth.md](../docs/operator-auth.md) |
| Docs / support | **CONDITIONAL** | The target is documented, but policy and security-group dependencies plus the current bootstrap-only command boundary need an implementation-ready contract pass. [operator-auth.md](../docs/operator-auth.md) |

Open gates:

- `operator-auth-contract-reconciliation` (docs / support): Reconcile the target with current account onboarding, implemented roles, unimplemented policies/groups, CLI commands, hosted endpoints, and credential-store support. ([tracking/evidence](../docs/operator-auth.md))
- `operator-auth-core-implementation` (behavior, bounds / abuse, entitlement / policy, recovery): Implement hosted PKCE and device-code sessions, refresh, revocation, secure local custody, lost-device recovery, authorization propagation, and hostile-flow tests. ([tracking/evidence](../docs/operator-auth.md))
- `operator-auth-operations` (observability): Add value-free login, callback, device-code, refresh, revocation, failure, and abuse metrics with SLOs and a tested receiver. ([tracking/evidence](../docs/observability-and-operations.md))
- `operator-auth-release-canary` (rollout / canaries): Ship behind a dark operator cohort and retain browser, headless, refresh, revoke, lost-device, and cross-platform credential-store acceptance. ([tracking/evidence](../docs/operator-auth.md))

<a id="plan-enforcement"></a>

### Plans, limits, and account overrides

A validated catalog defines Personal, Professional, Team, and Enterprise pricing direction, entitlements, limits, and policies; the control plane resolves account snapshots and cells enforce them with audited per-account overrides.

- Implementation: `implemented`
- Managed rollout: `limited`
- Readiness: **conditional**
- Detailed docs: [billing-and-limits.md](../docs/billing-and-limits.md), [plans.json](../web/plans/plans.json)

| Gate | State | Current evidence and conclusion |
|---|---|---|
| Behavior | **PASS** | Catalog load, pricing projection, account plan state, effective snapshots, limit resolution, overrides, and admin inspection are implemented and tested. [billing-and-limits.md](../docs/billing-and-limits.md), [plans_test.go](../internal/plans/plans_test.go) |
| Entitlement / policy | **PASS** | Plans remain global control-plane truth while cells store only resolved account behavior; per-account overrides change operation without rewriting price or plan identity. [billing-and-limits.md](../docs/billing-and-limits.md), [plans.json](../web/plans/plans.json) |
| Bounds / abuse | **PASS** | Commercial limits are bounded by independent platform ceilings, missing keys have explicit unlimited semantics, and invalid zero or oversized policies fail closed. [billing-and-limits.md](../docs/billing-and-limits.md), [plans.go](../internal/plans/plans.go) |
| Observability | **CONDITIONAL** | Usage events, current policy views, and admin surfaces exist; there is no unified entitlement-drift dashboard or alert path. [billing-and-limits.md](../docs/billing-and-limits.md), [observability-and-operations.md](../docs/observability-and-operations.md) |
| Recovery | **PASS** | Resolved snapshots, version floors, overrides, usage events, and account archive/import preserve policy state across retries and movement. [backup-and-recovery.md](../docs/backup-and-recovery.md), [billing-and-limits.md](../docs/billing-and-limits.md) |
| Rollout / canaries | **CONDITIONAL** | Personal and Professional are catalog-available; Team and Enterprise remain unavailable, and repository state alone cannot prove each live account override. [billing-and-limits.md](../docs/billing-and-limits.md), [plans.json](../web/plans/plans.json) |
| Docs / support | **PASS** | Pricing direction, limits, retention, account overrides, authority split, and unavailable-plan behavior are documented. [billing-and-limits.md](../docs/billing-and-limits.md), [plans.json](../web/plans/plans.json) |

Open gates:

- `entitlement-drift-monitoring` (observability): Add a value-free control-plane versus cell snapshot drift view and alert. ([tracking/evidence](../docs/billing-and-limits.md))
- `professional-purchase-readiness` (rollout / canaries): Reconcile Professional's catalog-purchasable flag with dark billing, limited inbound email enrollment, and support-policy enforcement before customer checkout opens. ([tracking/evidence](../web/plans/plans.json))
- `team-enterprise-activation` (rollout / canaries): Keep Team and Enterprise unavailable until their dependent features and billing transitions meet this scorecard's gates. ([tracking/evidence](../web/plans/plans.json))

<a id="realm-email-aliases"></a>

### Realm email aliases

Permanent canonical agent.realm-ID addresses are implemented independently from memorable realm aliases; alias reservation, lifecycle, admin controls, limits, and recovery exist while edge delivery remains dark.

- Implementation: `implemented`
- Managed rollout: `dark`
- Readiness: **not ready**
- Plan feature keys: `agent_email_realm_alias`
- Plan limit keys: `agent_email_realm_aliases_per_realm`
- Detailed docs: [agent-email.md](../docs/agent-email.md), [billing-and-limits.md](../docs/billing-and-limits.md)

| Gate | State | Current evidence and conclusion |
|---|---|---|
| Behavior | **CONDITIONAL** | Claim, reserve, activate, downgrade grace, retire, tombstone, project, CLI, and admin behavior exist; real alias delivery remains disabled. [agent-email.md](../docs/agent-email.md), [realm-email-alias-journal.test.mjs](../infra/cloudflare/control-plane/test/realm-email-alias-journal.test.mjs) |
| Entitlement / policy | **PASS** | Alias entitlement and per-realm count are separate from inbound email; reserved names and service-admin controls are globally authoritative. [agent-email.md](../docs/agent-email.md), [billing-and-limits.md](../docs/billing-and-limits.md) |
| Bounds / abuse | **CONDITIONAL** | Normalization, exact count, reserved words, tombstones, ownership, and immutable canonical fallback are enforced; request-rate and enumeration pressure need a dedicated edge guard. [agent-email.md](../docs/agent-email.md), [threat-model.md](../docs/threat-model.md) |
| Observability | **CONDITIONAL** | Journal and route state are inspectable, but alias conflicts, projection lag, delivery, and expiry are not wired to continuous alerts. [agent-email.md](../docs/agent-email.md), [observability-and-operations.md](../docs/observability-and-operations.md) |
| Recovery | **CONDITIONAL** | Durable journals, tombstones, canonical fallback, retirement, and retry tests exist; move and route-republication recovery need a live drill. [agent-email.md](../docs/agent-email.md), [realm-email-alias-journal.test.mjs](../infra/cloudflare/control-plane/test/realm-email-alias-journal.test.mjs) |
| Rollout / canaries | **CONDITIONAL** | The receive edge alias gate remains off and no managed customer alias delivers mail. [agent-email.md](../docs/agent-email.md) |
| Docs / support | **PASS** | Canonical fallback, reserved terms, request lifecycle, limits, downgrade, retirement, and rollout requirements are documented. [agent-email.md](../docs/agent-email.md), [billing-and-limits.md](../docs/billing-and-limits.md) |

Open gates:

- `alias-canonical-inventory` (behavior, rollout / canaries): Converge the complete canonical address inventory before any alias delivery is enabled. ([tracking/evidence](../docs/agent-email.md))
- `alias-operations-alerting` (observability): Connect alias conflicts, projection lag, delivery, route coverage, and expiry metrics to continuous alerts and a tested external receiver. ([tracking/evidence](../docs/observability-and-operations.md))
- `alias-request-rate-guard` (bounds / abuse): Add and test a request and enumeration guard at the global alias authority. ([tracking/evidence](../docs/threat-model.md))
- `alias-route-coverage` (behavior, rollout / canaries): Prove managed-domain catch-all or equivalent full route coverage before enabling alias delivery. ([tracking/evidence](../docs/agent-email.md))
- `alias-route-recovery` (recovery): Exercise account movement, projection replay, retirement, tombstone, and canonical fallback recovery. ([tracking/evidence](../docs/agent-email.md))
- `live-alias-canary` (behavior, rollout / canaries): Enable one selected alias and retain request-through-delivery and downgrade-grace evidence before widening. ([tracking/evidence](../docs/agent-email.md))

<a id="realm-messaging"></a>

### Realm-local messaging

Durable same-realm direct and fan-out messaging, mailbox claims, retention, account entitlements, rate limits, and installed-once foreground routing are implemented; cross-realm delivery and waking clients are explicitly outside the core.

- Implementation: `implemented`
- Managed rollout: `limited`
- Readiness: **conditional**
- Retained release/cohort evidence: `v0.0.172` — Same-realm messaging-core completion; current message-retention enforcement is excluded.
- Plan feature keys: `messaging`
- Plan limit keys: `message_delivered_per_realm_minute`, `message_delivered_per_recipient_minute`, `message_sent_per_agent_minute`
- Plan policy keys: `message_retention_days`, `messaging_entitlement_version`
- Detailed docs: [autonomous-realm-messaging.md](../docs/autonomous-realm-messaging.md), [inter-agent-messaging.md](../docs/inter-agent-messaging.md), [message-retention.md](../docs/message-retention.md)

| Gate | State | Current evidence and conclusion |
|---|---|---|
| Behavior | **PASS** | Send, list, fan-out, listen, claim, acknowledge, release, escalation, disabled behavior, and whole-thread retention are implemented and tested. [autonomous-realm-messaging.md](../docs/autonomous-realm-messaging.md), [inter-agent-messaging.md](../docs/inter-agent-messaging.md) |
| Entitlement / policy | **PASS** | Personal is disabled; enabled plans and account overrides take effect without reinstall, and same-realm authority fails closed. [billing-and-limits.md](../docs/billing-and-limits.md), [inter-agent-messaging.md](../docs/inter-agent-messaging.md) |
| Bounds / abuse | **PASS** | Payload, fan-out, per-agent, per-realm, per-recipient, claim, retry, escalation, and retention bounds are enforced with independent platform ceilings. [inter-agent-messaging.md](../docs/inter-agent-messaging.md), [message-retention.md](../docs/message-retention.md) |
| Observability | **CONDITIONAL** | Value-free mailbox, rate, retention, claim, and worker metrics exist without continuous scraping and tested external alert delivery. [observability-and-operations.md](../docs/observability-and-operations.md) |
| Recovery | **PASS** | Canonical PostgreSQL mailboxes, claim fences, retry releases, escalations, provenance holds, archive/import, and retention cursors preserve work. [backup-and-recovery.md](../docs/backup-and-recovery.md), [message-retention.md](../docs/message-retention.md) |
| Rollout / canaries | **CONDITIONAL** | The same-realm core has retained operational evidence, while this combined capability is limited because the managed message-retention worker remains disabled and lacks a current canary. [autonomous-realm-messaging.md](../docs/autonomous-realm-messaging.md), [message-retention.md](../docs/message-retention.md) |
| Docs / support | **PASS** | Supported behavior, disabled semantics, rate limits, retention, foreground processing, and non-goals are documented. [autonomous-realm-messaging.md](../docs/autonomous-realm-messaging.md), [inter-agent-messaging.md](../docs/inter-agent-messaging.md), [message-retention.md](../docs/message-retention.md) |

Open gates:

- `continuous-message-alerting` (observability): Connect mailbox backlog, rate, claim, failure, and retention metrics to a tested external alert receiver. ([tracking/evidence](../docs/observability-and-operations.md))
- `message-retention-rollout` (rollout / canaries): Enable enforce mode for a selected cohort and retain expiry, provenance-hold, and worker-fairness canaries. ([tracking/evidence](../docs/message-retention.md))

<a id="runtime-integrations"></a>

### Agent runtime integrations

Transactional MCP and routing installers exist for Codex, Claude Code, Grok Build, Cursor, OpenClaw, Antigravity, and GitHub Copilot, with capability-accurate hook support; broad signed-in model acceptance remains incomplete.

- Implementation: `implemented`
- Managed rollout: `limited`
- Readiness: **conditional**
- Detailed docs: [provider-integration-certification.md](../docs/provider-integration-certification.md)

| Gate | State | Current evidence and conclusion |
|---|---|---|
| Behavior | **PASS** | Install, verify, upgrade, uninstall, ownership, collision handling, rollback, MCP catalog, and supported transcript-hook paths have extensive contract tests. [provider-integration-certification.md](../docs/provider-integration-certification.md) |
| Entitlement / policy | **PASS** | The full toolset installs once and backend feature gates change behavior without client reinstallation; value-returning tools can be independently removed. [mcp-tools.md](../docs/mcp-tools.md), [provider-integration-certification.md](../docs/provider-integration-certification.md) |
| Bounds / abuse | **PASS** | Exact ownership markers, path validation, collision-resistant IDs, bounded hooks, transactional backups, and no-value modes constrain installer risk. [provider-integration-certification.md](../docs/provider-integration-certification.md), [threat-model.md](../docs/threat-model.md) |
| Observability | **CONDITIONAL** | Verification JSON and sanitized acceptance evidence schemas exist, but no recurring provider acceptance job or alerting surface is active. [memory-runtime-acceptance.md](../docs/memory-runtime-acceptance.md), [provider-integration-certification.md](../docs/provider-integration-certification.md) |
| Recovery | **PASS** | Install journals, pre-edit backups, rollback, exact ownership, idempotent reinstall, and conservative uninstall protect client configuration. [provider-integration-certification.md](../docs/provider-integration-certification.md) |
| Rollout / canaries | **CONDITIONAL** | Contract tests are strong, but only the Codex contract gate crosses MCP stdio and no provider cell is advertised model-tested with a current signed-in record. [provider-integration-certification.md](../docs/provider-integration-certification.md) |
| Docs / support | **PASS** | The per-runtime capability matrix, hooks, preview limitations, ownership, verification, and certification boundary are documented. [provider-integration-certification.md](../docs/provider-integration-certification.md) |

Open gates:

- `retained-provider-evidence` (rollout / canaries): Publish retained release JSON and the public support-matrix result from the existing credential-free provider contract gates. ([tracking/evidence](https://github.com/witwave-ai/witself/issues/45))
- `runtime-acceptance-operations` (observability): Run provider acceptance on a recurring cadence and alert on capability regressions without retaining credentials or private prompt content. ([tracking/evidence](../docs/provider-integration-certification.md))
- `signed-in-runtime-matrix` (rollout / canaries): Complete current signed-in model acceptance for every advertised runtime and operating-system capability cell. ([tracking/evidence](../docs/provider-integration-certification.md))

<a id="secrets-vault"></a>

### Secrets, vault, passwords, and TOTP

The client-custodied agent vault, ciphertext-only backend, secret lifecycle, guarded reveal, password generation, TOTP, archive/import, recovery, rotation, and per-agent limits are implemented; update, runtime injection, grants, and certification remain follow-ons.

- Implementation: `implemented`
- Managed rollout: `limited`
- Readiness: **conditional**
- Plan feature keys: `secrets`
- Plan limit keys: `stored_secret`
- Detailed docs: [client-custodied-agent-vault.md](../docs/client-custodied-agent-vault.md), [sealed-plane-acceptance.md](../docs/sealed-plane-acceptance.md), [secret-model.md](../docs/secret-model.md)

| Gate | State | Current evidence and conclusion |
|---|---|---|
| Behavior | **CONDITIONAL** | Enrollment, create, search, show, reveal, archive, restore, delete, password, TOTP, recovery, and rotation exist; secret update and runtime injection are not complete. [client-custodied-agent-vault.md](../docs/client-custodied-agent-vault.md), [secret-model.md](../docs/secret-model.md) |
| Entitlement / policy | **PASS** | Personal has zero secret capacity, paid plans have per-agent caps, and only the active agent client can decrypt value fields under explicit reveal policy. [billing-and-limits.md](../docs/billing-and-limits.md), [client-custodied-agent-vault.md](../docs/client-custodied-agent-vault.md) |
| Bounds / abuse | **PASS** | Ciphertext-only storage, local AVK binding, size limits, field bounds, reveal audit, no-value mode, rotation fences, and excluded exports constrain exposure. [client-custodied-agent-vault.md](../docs/client-custodied-agent-vault.md), [secret-size-and-attachments.md](../docs/secret-size-and-attachments.md) |
| Observability | **CONDITIONAL** | Value-free audit and lifecycle state exist, but sealed-plane SLOs, anomaly alerts, and external escalation are not connected. [observability-and-operations.md](../docs/observability-and-operations.md), [sealed-plane-acceptance.md](../docs/sealed-plane-acceptance.md) |
| Recovery | **CONDITIONAL** | Client recovery, rotation, encrypted archive/import, and fail-closed vault binding exist; cross-cloud movement and loss/recovery drills remain incomplete. [backup-and-recovery.md](../docs/backup-and-recovery.md), [client-custodied-agent-vault.md](../docs/client-custodied-agent-vault.md) |
| Rollout / canaries | **CONDITIONAL** | The implemented slice is released, but four-runtime and multi-cloud acceptance evidence is incomplete and advanced operations remain unavailable. [provider-integration-certification.md](../docs/provider-integration-certification.md), [sealed-plane-acceptance.md](../docs/sealed-plane-acceptance.md) |
| Docs / support | **CONDITIONAL** | The authoritative AVK design is documented, while older KMS/server-decryption language remains in historical documents and needs consistency cleanup. [client-custodied-agent-vault.md](../docs/client-custodied-agent-vault.md), [sealed-plane-acceptance.md](../docs/sealed-plane-acceptance.md) |

Open gates:

- `advanced-secret-operations` (behavior): Implement secret update, local runtime injection, grants/group ownership, and irreversible tombstone purge with matching policy and tests. ([tracking/evidence](../docs/secret-model.md))
- `sealed-doc-consistency` (docs / support): Remove or clearly label stale KMS and server-decryption claims that conflict with the client-custodied AVK architecture. ([tracking/evidence](../docs/sealed-plane-acceptance.md))
- `sealed-live-certification` (recovery, rollout / canaries): Complete four-runtime reveal/TOTP recovery and multi-cloud archive/movement drills with sanitized evidence. ([tracking/evidence](../docs/sealed-plane-acceptance.md))
- `sealed-operations-alerting` (observability): Define sealed-plane SLOs and connect value-free enrollment, reveal, recovery, rotation, and anomaly signals to a tested external escalation path. ([tracking/evidence](../docs/observability-and-operations.md))

<a id="self-hosting"></a>

### Self-hosted Witself

The portable server, startup migrations, Helm chart, capability contract, PostgreSQL path, and multi-cloud Pulumi cell tooling are implemented and released for self-host preview; production support and cross-cloud acceptance remain conditional.

- Implementation: `implemented`
- Managed rollout: `not applicable`
- Readiness: **conditional**
- Detailed docs: [README.md](../charts/witself-server/README.md), [self-hosting.md](../docs/self-hosting.md), [README.md](../infra/pulumi/README.md)

| Gate | State | Current evidence and conclusion |
|---|---|---|
| Behavior | **CONDITIONAL** | Portable server, startup migrations, Helm, and Pulumi cell paths exist, but the complete fresh-install, upgrade, and optional-feature matrix is not retained across supported substrates. [README.md](../charts/witself-server/README.md), [README.md](../infra/pulumi/README.md) |
| Entitlement / policy | **CONDITIONAL** | Backend-kind and capability contracts distinguish managed-only dependencies, but the current capabilities response marks implemented facts unsupported and omits documented optional-feature flags. [values.yaml](../charts/witself-server/values.yaml), [api-contract.md](../docs/api-contract.md), [self-hosting.md](../docs/self-hosting.md), [server.go](../internal/server/server.go) |
| Bounds / abuse | **CONDITIONAL** | The chart has hardened workload defaults and bounded application controls, while production capacity profiles, ingress policy, and operator-owned abuse protections are not certified end to end. [README.md](../charts/witself-server/README.md), [self-hosting.md](../docs/self-hosting.md) |
| Observability | **CONDITIONAL** | Metrics, health endpoints, structured logs, and worker status exist, but the preview does not deliver a verified continuous scrape, PVC metrics, Alertmanager routing, dashboard, and tested external receiver. [observability-and-operations.md](../docs/observability-and-operations.md), [self-hosting.md](../docs/self-hosting.md) |
| Recovery | **CONDITIONAL** | Migration, backup, archive/import, and infrastructure recovery guidance exist without a retained operator-owned PostgreSQL restore and full deployment recovery drill. [backup-and-recovery.md](../docs/backup-and-recovery.md), [self-hosting.md](../docs/self-hosting.md) |
| Rollout / canaries | **CONDITIONAL** | Managed cohort rollout does not apply, but current release acceptance across fresh install, upgrade, rollback, and the supported cloud or Kubernetes matrix is incomplete. [README.md](../charts/witself-server/README.md), [README.md](../infra/pulumi/README.md) |
| Docs / support | **CONDITIONAL** | Chart and Pulumi guides are substantial, while self-host and server-command history still contains superseded KMS/server-decryption and target-only command language; production support remains explicitly uncommitted. [self-host-support.md](../docs/self-host-support.md), [self-hosting.md](../docs/self-hosting.md) |

Open gates:

- `self-host-capability-reconciliation` (entitlement / policy): Make the capabilities response accurately describe implemented facts and every supported optional feature, then pin it against server routes and Helm values. ([tracking/evidence](../internal/server/server.go))
- `self-host-doc-consistency` (docs / support): Reconcile self-host and server-command history with startup migrations, gen-bootstrap-token plus auth login, Pulumi, current capabilities, and the released client-custodied vault. ([tracking/evidence](../docs/server-command-surface.md))
- `self-host-production-hardening` (behavior, bounds / abuse, observability): Certify hardened production values, ingress and abuse controls, database and storage capacity, continuous monitoring, alerts, and every supported optional feature. ([tracking/evidence](../docs/self-host-support.md))
- `self-host-recovery-drill` (recovery): Retain a fresh PostgreSQL backup restore, migration failure, worker restart, archive/import, and client-vault recovery drill on operator-owned infrastructure. ([tracking/evidence](../docs/backup-and-recovery.md))
- `self-host-release-acceptance` (rollout / canaries): Retain release-specific fresh-install, upgrade, rollback, and health evidence for every advertised Kubernetes and cloud substrate. ([tracking/evidence](../infra/pulumi/README.md))

<a id="transcripts"></a>

### Transcripts and retention

The transcript ledger, durable local hook outbox, normalized supported-runtime capture, usage rollups, account retention policy, whole-conversation cleanup, evidence holds, admin override, and scalable worker are implemented; managed enforcement remains staged.

- Implementation: `implemented`
- Managed rollout: `limited`
- Readiness: **conditional**
- Plan policy keys: `transcript_retention_days`
- Detailed docs: [transcript-ledger.md](../docs/transcript-ledger.md), [transcript-retention.md](../docs/transcript-retention.md)

| Gate | State | Current evidence and conclusion |
|---|---|---|
| Behavior | **CONDITIONAL** | Append, read, tail, durable hook flush, normalized events, usage rollups, account-policy retention, preview/enforce, whole-conversation deletion, and evidence holds are implemented and tested; materializing durable evidence to release resolved holds remains unimplemented. [transcript-ledger.md](../docs/transcript-ledger.md), [transcript-retention.md](../docs/transcript-retention.md) |
| Entitlement / policy | **PASS** | Catalog defaults are 30, 90, 365, and indefinite by plan; resolved cell snapshots and audited account overrides control behavior without changing price or plan. [billing-and-limits.md](../docs/billing-and-limits.md), [transcript-retention.md](../docs/transcript-retention.md) |
| Bounds / abuse | **PASS** | Payload, outbox, retry, page, claim, batch, cursor, time, worker lane, and defensive retention bounds constrain capture and cleanup. [transcript-ledger.md](../docs/transcript-ledger.md), [transcript-retention.md](../docs/transcript-retention.md) |
| Observability | **CONDITIONAL** | Capture, outbox, retention, failure, and worker metrics exist without continuous scrape, alert routing, or a tested external receiver. [observability-and-operations.md](../docs/observability-and-operations.md), [transcript-retention.md](../docs/transcript-retention.md) |
| Recovery | **PASS** | Durable local outbox, idempotent append, SKIP LOCKED cleanup, cursors, provenance holds, archive/import, and preview mode support safe retries and recovery. [backup-and-recovery.md](../docs/backup-and-recovery.md), [transcript-retention.md](../docs/transcript-retention.md) |
| Rollout / canaries | **CONDITIONAL** | The worker is deployable and tested, but transcript retention remains disabled/preview in the active managed cell and lacks an enforce-mode canary. [values.yaml](../charts/witself-server/values.yaml), [transcript-retention.md](../docs/transcript-retention.md) |
| Docs / support | **PASS** | Capture support, platform differences, ledger, policy semantics, evidence safety, worker scaling, metrics, and rollout are documented. [transcript-ledger.md](../docs/transcript-ledger.md), [transcript-retention.md](../docs/transcript-retention.md) |

Open gates:

- `continuous-retention-alerting` (observability): Connect outbox and retention backlog, failure, age, and throughput metrics to a tested external alert receiver. ([tracking/evidence](../docs/observability-and-operations.md))
- `evidence-materialization` (behavior): Implement the documented path that materializes durable evidence so resolved transcript holds can be released. ([tracking/evidence](../docs/transcript-retention.md))
- `retention-enforce-canary` (rollout / canaries): Enable enforce mode for a selected account and retain expiry, evidence-hold, crash, and multi-worker fairness evidence. ([tracking/evidence](../docs/transcript-retention.md))

<a id="usage-metering"></a>

### Usage metering and customer reporting

Immutable value-free usage events, hourly and daily rollups, time and dimension filtering, agent-scoped API authorization, and the customer CLI report are implemented; account aggregation and billing conversion remain follow-ons.

- Implementation: `implemented`
- Managed rollout: `limited`
- Readiness: **conditional**
- Detailed docs: [cli-command-surface.md](../docs/cli-command-surface.md), [transcript-ledger.md](../docs/transcript-ledger.md)

| Gate | State | Current evidence and conclusion |
|---|---|---|
| Behavior | **PASS** | Event append, canonical dimensions, hourly and daily rollups, time windows, grouping, filters, JSON output, API, and CLI reporting are implemented and tested. [usage_test.go](../internal/server/usage_test.go), [usage_integration_test.go](../internal/store/usage_integration_test.go) |
| Entitlement / policy | **PASS** | Only an active agent token may read its own rollups in the current slice; account, realm, and cross-agent reporting are not silently exposed. [transcript-ledger.md](../docs/transcript-ledger.md), [usage_test.go](../internal/server/usage_test.go) |
| Bounds / abuse | **CONDITIONAL** | Events are value-free and query windows and groups are validated, but usage dimensions are extensible and the current report query has no result-row cap or pagination. [transcript-ledger.md](../docs/transcript-ledger.md), [usage_integration_test.go](../internal/store/usage_integration_test.go) |
| Observability | **CONDITIONAL** | Usage can be queried, but append failures, rollup lag, missing intervals, reconciliation drift, and storage growth lack continuous SLOs and alerts. [observability-and-operations.md](../docs/observability-and-operations.md), [transcript-ledger.md](../docs/transcript-ledger.md) |
| Recovery | **PASS** | Immutable events, deterministic buckets, canonical database backup, and semantic validation of events and rollups on account archive/import provide a verified restore path. [backup-and-recovery.md](../docs/backup-and-recovery.md), [usage_integration_test.go](../internal/store/usage_integration_test.go) |
| Rollout / canaries | **CONDITIONAL** | Agent-scoped reporting is released, while account and realm aggregation, billing-unit conversion, current managed reconciliation, and retained load evidence are incomplete. [billing-and-limits.md](../docs/billing-and-limits.md), [transcript-ledger.md](../docs/transcript-ledger.md) |
| Docs / support | **PASS** | Current query scope, value-free dimensions, bucketing, filters, API, CLI, rollups, authorization, and deferred billing aggregation are documented. [cli-command-surface.md](../docs/cli-command-surface.md), [transcript-ledger.md](../docs/transcript-ledger.md) |

Open gates:

- `usage-operations-alerting` (observability): Connect append failures, rollup lag, missing buckets, reconciliation drift, and retained volume to continuous metrics and a tested receiver. ([tracking/evidence](../docs/observability-and-operations.md))
- `usage-query-bounds` (bounds / abuse): Define the accepted dimension vocabulary and add a hard returned-row cap or cursor pagination with boundary, hostile-import, and high-cardinality tests. ([tracking/evidence](../internal/store/usage.go))
- `usage-rollup-expansion` (rollout / canaries): Implement and retain canaries for account and realm aggregation, billing-unit conversion, archive/restore reconciliation, and production-shaped load. ([tracking/evidence](../docs/billing-and-limits.md))
