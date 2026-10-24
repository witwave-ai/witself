# Witself Deferred-Feature Roadmap

This is an index of open product candidates and unresolved hardening work, not
a delivery schedule or an implementation checklist. Recording an idea here does
not authorize implementation or relax an existing release gate. Use
[Feature Status](feature-status.md) for implementation, rollout, evidence, and
remaining readiness gates. The [Local Cell plan](research/local-cell/README.md)
tracks that active initiative separately.

Reviewed against repository main at
[`2daf2f9f`](https://github.com/witwave-ai/witself/commit/2daf2f9f2cc6aa6ac1d00e4af1126b99fb5acb9a).
Completed foundations and detailed contracts belong in their canonical docs;
this index keeps the remaining work. A source review is not live certification.

All candidates preserve the current boundaries unless a separate decision
changes them: the backend performs no model inference or embedding generation;
agent-secret keys and decryption stay in the client; messages and email confer
no authority and never wake an offline AI client. See
[narrative memory](narrative-memory-and-curation.md),
[client-held vault custody](decisions/0003-client-custodied-agent-vault.md), and
[foreground messaging](autonomous-realm-messaging.md).

## Recall And Memory Intelligence

- **Richer ranking:** client-side reranking, learned weights, and feedback-driven
  ranking beyond the deterministic lexical/hybrid scorer. Model credentials,
  egress, privacy, and cost remain the client's responsibility; lexical recall
  must remain available. See [memory-model.md](memory-model.md).
- **Multi-vector and chunked recall:** multiple vectors/chunks per memory version,
  chunk attribution, metering, and portable archives. The current vector model
  stores one exact vector per immutable profile and memory version. Optional
  pgvector/ANN projections are a separate optimization, not a replacement for
  canonical portable data or a backend embedding service. See
  [storage.md](storage.md).
- **Additional curation policies:** client-authored policies beyond the existing
  bounded plan/apply lifecycle. Preserve previews, fences, provenance,
  reversible actions, and opt-in behavior. Backend-owned summarization and
  inference are outside the accepted architecture, not pending implementation.
  See [narrative-memory-and-curation.md](narrative-memory-and-curation.md).
- **Vector-profile migration tooling:** progress, idempotency, coverage, client
  cost accounting, and rollback for a partially populated replacement profile.
  Registering a new immutable profile and supplying new vectors already works;
  the missing piece is coordinated migration tooling. See
  [backup-and-recovery.md](backup-and-recovery.md#client-supplied-vectors).

## Human-Verified Destructive Grants

Stronger human approval for permanent fact deletion remains open. The MCP
adapter requires a same-turn direct-user assertion, but that Boolean is
client-attested, not server-verifiable proof of a human decision. Current API
checks enforce the authenticated agent/access profile, ownership, preview
concurrency fields, and idempotency; they do not provide a per-operation
human grant. See [fact deletion](../internal/server/fact.go) and
[its MCP boundary](../cmd/witself/mcp.go).

A future grant must bind the authenticated decision to the account, agent,
fact, resolved assertion, candidate revision, and short expiration; be
single-use or replay-stable for one retry key; and leave value-free evidence.
Do not describe ordinary agent credentials as technically unable to delete,
or assume a fine-grained `fact:delete` scope exists today.

## Cross-Realm Agent Collaboration

Same-realm messaging, long-poll `listen`, claims, and collaboration requests
already exist. The open extension is federation across realms/accounts:
realm-qualified addressing, verified discovery/signing keys, relay delivery,
cross-realm conversations, and consent/revocation. The detailed proposal and
its unresolved choices live in [agent-collaboration.md](agent-collaboration.md);
its future contract is not proof that federation is implemented.

The proposal calls for deny-by-default trust, first-contact consent, loop/cost
limits, and durable offline delivery. Automatic-reply defaults remain an
explicit decision; autonomous replies must stay within finite budgets.
Agents remain outbound clients; a relay
does not activate a model. **Signatures do not hide message
contents:** claiming a blind relay also requires an end-to-end encryption and
key-management contract, which remains unresolved. Signing identity,
relay-first versus peer-to-peer federation, and A2A interoperability also need
explicit decisions before implementation.

## Messaging And Transcript Artifacts

### Cross-Client Messaging Under One Agent Identity

[Cross-client messaging research](research/cross-client-messaging/README.md)
covers addressing a particular AI client or location while retaining one agent
identity and shared memory. This is internal messaging, not email, and is
separate from cross-realm federation. Per-client delivery, acknowledgement,
return routing, authenticated endpoint identity, session lifecycle, upgrades,
and Local Cell synchronization remain research; implementation is not authorized.

### Message Attachments And Large Payloads

Internal messages currently accept inline body/payload content. Blob-backed
attachments need storage adapters, size/retention limits, byte metering,
portable backup, safe diagnostics, and untrusted-content review. Existing email
attachment handling does not establish an internal-message attachment contract.
See [inter-agent-messaging.md](inter-agent-messaging.md).

### Transcript File Artifacts

Binary transcript artifacts still need cell-local object storage, portable
metadata/revisions, and archives carrying both metadata and bytes. Bounded
structured payloads are supported; non-empty transcript file attachments are
refused. See [the artifact contract](transcript-ledger.md#structured-objects-and-file-artifacts).

## Agent Activity Hardening

Before production exposure, choose a concurrency-safe per-agent bound with
deterministic oldest-projection eviction, or an explicit registered-installation
lifecycle with retention, for distinct runtime/location observations.
The current projection updates the newest observation per tuple
but does not cap the number of tuple keys an agent may introduce. Preserve
existing-key updates at the limit and archive/import behavior. This hardening
must not turn observations into availability or heartbeat claims. See
[the installed activity and peer routes](api-routes.md#self-management-and-hydration-routes).

## Avatar Webhooks

A future outbound dispatcher needs a product use case and reviewed event,
authentication, ordering, retry, replay, and dead-letter contracts. Reuse the
value-free audit boundary; do not presume hashes, provenance, prompts, SVG, or
visual specifications are safe webhook metadata. Full artwork remains an
explicit authenticated read. See [agent-avatars.md](agent-avatars.md).

## Login And Additional 2FA

These extend the existing client-held secrets and TOTP capabilities. New secret
material must remain client-encrypted, reveal-gated, and excluded from recall,
self digests, and plaintext export.

### SMS And Email-Code 2FA

SMS and stronger sealed-plane email-code 2FA remain open. Agent email and its
conservative code-candidate helper already support expected, user-authorized,
low-risk workflows; untrusted stored mail is not a sealed credential or proof
of identity. Further support needs sender/provider authentication, abuse and
privacy controls, retries, failure handling, and a support boundary. See
[agent-email.md](agent-email.md) and [totp-2fa.md](totp-2fa.md).

### Other Modalities And Browser Handoff

- **Push approvals:** an authenticated device approval channel, expiry, audit,
  and explicit unattended-agent policy that composes with reveal controls.
- **Passkeys:** origin/browser binding, recovery, and a device/agent possession
  model. Any stored private credential material stays client-held.
- **Hardware keys:** physical-device access, operator approval, handoff, and
  container/Kubernetes limitations.
- **Browser-session handoff:** safe cookie/session custody and authorized
  browser use. Secret references and subprocess injection through `witself run`
  are also still targets, not a shipped foundation for this feature. See
  [the command surface](cli-command-surface.md) and [secret-model.md](secret-model.md).

## Administrative Surfaces

- **Administrative web dashboard:** optional convenience over the same public
  authorization, audit, and redaction contracts; managed administration must
  not require a dashboard. The local, content-read-only
  [Agent Console](agent-console.md) is implemented; it does not close this
  separate administrative UI proposal. The implemented operator tool is
  [witself-admin](witself-admin.md).
- **Exceptional account recovery:** fleet-authorized closure when all owner
  credentials are lost, and an operator-assisted recovery ceremony when email
  recovery is unavailable. Define evidence, abuse controls, and authority
  before adding such escape hatches.
- **Standalone tombstone anonymization:** clarify whether a separate immediate,
  metadata-only operation is needed. Existing account purge already scrubs
  identifying fields while retaining the account row, but it is a grace-period,
  eligibility-fenced full-content purge, not this proposed operation. See
  [account purge](../internal/store/account_purge.go) and
  [data-retention-policy.md](data-retention-policy.md).

## MCP Network Transport

Network MCP remains a distinct, higher-risk remote tool surface. The current
transport is stdio; backend messaging long-poll does not implement network MCP
and does not require an agent-run HTTP server. Any new transport needs explicit
enablement, authentication, authorization parity, audit attribution, and rate
limits. See [mcp-tools.md](mcp-tools.md).

## Cells And Provider Readiness

The cell/control-plane foundation and AWS/GCP/Azure/Civo Pulumi provider paths
are implemented. Do not treat their existence as live provider certification
or all fleet operations being ready. Remaining provider conformance,
backup/PITR, recovery, destination monitoring, and movement drills belong in
[Feature Status](feature-status.md#fleet-deployment-recovery),
[cloud-targets.md](cloud-targets.md), and
[memory-cloud-conformance.md](memory-cloud-conformance.md).
Optional [Terraform packaging](terraform-infrastructure.md) remains separate
future work; it is not the current provider implementation.

Current placement and movement operate on **accounts**, with source suspension
and fenced cutover. Per-realm placement and dual-write migration are not
implemented alternatives. Further realm placement, dual-write cutover, and
self-hosted multi-cell support need separate scope and decisions. General
shared-data multi-master remains outside the fleet v1 scope; the existing fleet
is independent cells, not one multi-master database. Local
Cell's selected continuous-sync requirements are tracked in its own
[implementation plan](research/local-cell/implementation-plan.md), not silently
excluded by this fleet boundary. See [deployment-cells.md](deployment-cells.md)
and [account movement readiness](feature-status.md#account-lifecycle).

Account/realm usage aggregation and billing-unit conversion also remain open.
Any future independently placed realms need an explicit cross-cell billing
contract; current account-level billing does not supply it. See
[usage metering](feature-status.md#usage-metering).

**Private GitOps repositories:** add reviewed Argo CD read-credential inputs,
rotation/revocation, and bootstrap support for private forks. The public-repo
bootstrap does not supply them. A GitHub App is the proposed preferred route,
with deploy keys as a fallback; credentials must remain out of logs and Git.
See [the current bootstrap](../infra/pulumi/internal/cell/aws_argocd.go).

## Control-Plane Counter Hardening

Invite-use and signup counters already use transactional Durable Object
authority. The remaining concurrency work concerns verification resend,
recovery, and email-change quotas/cooldowns and code-attempt counters that still
use KV updates. Scope
any migration to those actual paths, with consistent expiry, retries, and
concurrency tests. See [the control-plane handlers](../infra/cloudflare/control-plane/src/index.js)
and [signup counter authority](../infra/cloudflare/control-plane/src/signup-counters.mjs).

## Policy And Vault Extensions

- **Cross-agent policy and groups:** the base policy/group engine is specified,
  not implemented. Explicit deny, precedence, conditional expressions, and
  time-bounded grants are extensions of that unfinished foundation, not of an
  already-shipped allow-only engine. See [access-policy.md](access-policy.md),
  [security-groups.md](security-groups.md), and
  [its scorecard](feature-status.md#access-policy-security-groups).
- **Field-level fact encryption:** a future design, not an implemented optional
  switch awaiting a default change; whether to make it a managed default is
  part of that design. Facts currently use ordinary data-at-rest
  protection; `sensitive` is a redaction/display flag. Any proposal must settle
  key custody, query behavior, backup/recovery, and how to preserve the open-plane
  model without a secret-style reveal ceremony. Credentials remain secrets,
  not sensitive facts. See [storage.md](storage.md#data-at-rest-note).
- **Vault operations and sharing:** client-held AVK custody and local decryption
  are implemented under [ADR 0003](decisions/0003-client-custodied-agent-vault.md).
  Updates, runtime injection, grants, and cross-agent/group sharing remain
  follow-ons. Sharing must establish cryptographic possession as well as
  authorization; backend roles alone cannot reveal a field. Keep certification
  gates in [sealed-plane-acceptance.md](sealed-plane-acceptance.md).
- **Plaintext secret export:** research-only break-glass behavior, if ever
  approved. It needs deliberate operator authorization, strong confirmation,
  least privilege, value-free audit, and separate tooling. Normal whole-account
  `witself export` already includes encrypted vault envelopes and public lifecycle
  state, but never plaintext secrets, seeds, AVKs, or recovery material. Do not
  add plaintext reveal to ordinary export, digest, ingest, or self hydration.
  See [backup-and-recovery.md](backup-and-recovery.md#sealed-plane-secret-backup).

## Commercial Candidates

- **Additional activation gates:** optional per-plan billing/identity-verification
  ceremonies require a separate product and privacy decision. Managed billing
  already exists; the current email-verification activation path is not a
  mandatory email-plus-billing-plus-KYC chain. Do not infer a live KYC requirement
  or change account policy from this candidate. The future free-local/paid-hosted
  direction is owned by the [Local Cell plan](research/local-cell/implementation-plan.md).
  See [billing-and-limits.md](billing-and-limits.md).
- **Witself utility token:** research only, explicitly excluded from v0 and v1.
  No account, billing, identity, memory, messaging, CLI, or MCP workflow may
  require one. This exclusion does not rule out ordinary provider-mediated
  payment methods. See [requirements.md](requirements.md).

## Identity-Tied Candidates

These are design candidates, not effort estimates or claims of
implemented enforcement. Existing facts, subject aliases, token identity,
client vault keys, and avatars do not by themselves implement these contracts.

| Candidate | Remaining design and dependencies |
| --- | --- |
| Typed identity profile | Validate stable ID, display name, owner/sponsor, issuer/publisher, type/blueprint/model, lifecycle, classification, and provenance over the current subject/predicate/assertion fact model. See [facts-model.md](facts-model.md). |
| Stable self-ID and signing key | Proof of control distinct from a backend ID, bearer token, or vault encryption key; signing belongs in trusted client tooling. Decide lifecycle, enrollment, revocation, and optional DID/SPIFFE projections. |
| Graduated self-disclosure | Requester-specific public/private self-views; depends on the unfinished policy/group engine and explicit disclosure boundaries. |
| Values and constitution | Versioned principles, priority/conflict rules, and hard constraints with a defined trusted enforcement point. Stored prose alone cannot guarantee non-overridable behavior in an external model. |
| Persona and self-concept | A validated persona object with context-scoped voice, traits, and expertise; ordinary facts/memories can hold text but do not provide that contract. |
| Principal binding and delegation | Owner/sponsor/delegator relationships, sponsor escalation, an ordered delegation chain, authority scope, validity, revocation, and audit linkage; bearer credentials do not substitute for this graph. |
| Agent passport and challenge-response | Signed identity/permission claims peers can verify; coordinate with federation discovery cards and the self-ID/signing decision. |

Other research candidates remain: capability/skill manifests, counterparty trust
and vouch/attestation records, enforceable consent grants, verifiable forgetting
or crypto-shredding, bitemporal queries, a typed entity graph, and C2PA
provenance. Existing capability flags, deletion receipts, validity/history
fields, and subjects/aliases are foundations, not evidence these broader
contracts are complete. Coordinate signed-card work with
[cross-realm collaboration](#cross-realm-agent-collaboration) rather than create
a duplicate project. See [data-model.md](data-model.md),
[memory-model.md](memory-model.md), and [authorization-and-roles.md](authorization-and-roles.md).

## Release Hardening

- **macOS signing/notarization:** add and verify Developer ID signing and
  notarization for direct-download binaries. The current release pipeline does
  not provide that acceptance; acquiring credentials alone is not completion.
- **Homebrew cask migration:** depends on signing/notarization and a verified
  clean installation/upgrade path. Current formula packaging remains the baseline.
- **Native signature verification in `witself-admin` self-upgrade:** remove reliance on an
  externally installed `cosign`. Today its absence permits checksum-only
  verification; embedded Sigstore verification remains proposed work. Preserve
  workflow/tag identity checks and hard failure on invalid signatures.

See [release-and-build.md](release-and-build.md) and
[the current updater](../cmd/witself-admin/tui_upgrade.go).

## Promotion Criteria

Before promoting a candidate into implementation, record:

- The problem, accepted scope, owner, dependencies, and explicit authorization.
- A threat model, principals, permissions, consent, and deterministic conflict
  rules; never assume a planned policy engine already enforces them.
- CLI/API/MCP/JSON contracts, capability discovery, and predictable unsupported
  behavior, with managed/self-hosted support boundaries.
- Value-free audit and diagnostics: no memory/fact/message content, vectors,
  secrets, TOTP material, raw tokens, or high-risk payment data.
- Migration, portable archive, recovery, retention, and rollback effects.
- Acceptance tests, required validation, release notes, and a controllable
  rollout. Distinguish implementation, validation, merge, deployment, and
  operational acceptance.

Keep detailed research in a linked document; this index should retain the open
question and the next decision, not repeat a full design or completed history.
