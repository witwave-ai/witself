# Witself Self-Hosted Support

Status: draft. Decision: self-hosting is available from the public repo, but
production self-host support is paid or contracted once the hardening path is
real.

Narrative-memory amendment (accepted 2026-07-14): self-host support includes
the deterministic PostgreSQL memory path, not a backend embedding provider.
Client curation and optional client-supplied vectors follow
[narrative-memory-and-curation.md](narrative-memory-and-curation.md).

> **Sealed-plane custody amendment (accepted 2026-07-18):**
> [ADR 0003](decisions/0003-client-custodied-agent-vault.md) and the
> [client-custodied vault plan](client-custodied-agent-vault.md) supersede the
> KMS prerequisites below. Self-hosted and managed servers use the same
> ciphertext-only backend: the AVK remains in the authorized client, and the
> backend has no decrypt key or server-decrypt route. No
> `WITSELF_SEALED_PLANE_ENABLED`, `WITSELF_KMS_PROVIDER`, or
> `WITSELF_KMS_KEY_ID` configuration is implemented or required for secrets.
> KMS/realm-KEK readiness, rotation, and cross-cloud re-wrap sections below are
> retained only as superseded support-design history. Operators still own
> ordinary infrastructure encryption, database backups, and separate custody
> of client AVKs.

## Decision

Managed Witself Cloud is the default supported product.

Self-hosting is first-class in the sense that the public repo should include the
backend server, Helm chart, Terraform modules, configuration docs, migration
paths, PostgreSQL retrieval and optional client-vector guidance, and operational
guidance needed to run Witself outside Witself Cloud.

Self-hosting is not automatically a production support entitlement. Production
self-host support should be paid or contracted after the required production
hardening docs and operational paths exist.

Witself self-hosters protect two payloads with two postures. They protect the
integrity and authenticity of **open-plane** identity data (memories, facts,
policies, groups, and messages), and, when the **sealed plane** is enabled, the
confidentiality of secret material and TOTP seeds. The sealed plane is
KMS-backed envelope-encrypted (CMK → per-realm KEK → per-secret/field DEK) and
carries production prerequisites — a KMS provider and key-rotation guidance —
that the open plane does not. Sealed-plane values are never embedded, never
returned by semantic recall, never in the self-digest, and never in the
plaintext export; reveal is the only audited value-returning path. See
[encryption-model.md](encryption-model.md) and
[key-hierarchy.md](key-hierarchy.md).

## Support Levels

| Level | Support posture |
|---|---|
| Local development | Community or best-effort only. No production support. |
| Self-host preview | Best-effort issue triage for public chart/server problems. No SLA. |
| Production self-hosted | Paid or contracted support only, after production hardening requirements are met. |

Notes:

- Local development covers `witself realm init`, `witself setup --local`, and a
  future `witself-server serve --dev` using local PostgreSQL. It runs the same
  full-text retrieval path and no model provider. It is scaffolding, not a
  production mode.
- Self-host preview covers the public `witself-server` image, Helm chart, and
  Terraform modules run against externally managed PostgreSQL, on a best-effort
  basis. Optional client vectors use migration-0032 JSONB and need no extension.
- Production self-hosted is the only tier that carries a support commitment, and
  only under a paid or contracted agreement.

The tiers span both planes. A deployment may run the open plane only (no KMS),
or enable the sealed plane, which adds a KMS provider as a hard dependency and
the key-rotation operational path below.

## Production Support Prerequisites

Witself should not claim production self-host support until these are real:

Open-plane (memory, fact, identity) prerequisites:

- Backup and restore documentation for PostgreSQL memory content and full-text
  index rebuilds, plus optional vector profile/row handling when enabled.
- Database migration and rollback guidance (`witself-server migrate`, advisory
  lock, Helm migration Job).
- Optional client-vector profile, validation, JSONB storage, coverage, and
  future ANN-projection guidance. Client software, not the server, owns vector
  generation after a profile change.
- Upgrade guide.
- Production Helm values examples.
- Terraform state and configuration-management guidance.
- Observability guidance.
- Disaster recovery guidance.
- Security patch and release process.

Sealed-plane (secret, TOTP) prerequisites — required only when the sealed plane
is enabled:

- A configured **KMS provider** (`WITSELF_KMS_PROVIDER` ∈ `aws-kms`, `gcp-kms`,
  `azure-key-vault`; `local-dev` is not a production mode) and a reachable root
  CMK (`WITSELF_KMS_KEY_ID`). The server must unwrap the active per-realm KEK at
  startup; readiness gates on KMS reachability whenever the sealed plane is on.
- **Key-rotation guidance** covering per-realm KEK rotation (re-wrapping the
  `realm_keys` KEK under a new CMK version) and DEK rotation, emitting the
  `key.rotated` audit event, with no plaintext exposure during rotation. See
  [key-hierarchy.md](key-hierarchy.md).
- Encrypted-only secret backup that carries the envelope plus KMS key identity
  and rotation metadata (never key material, never plaintext), and a documented
  crypto-shred / KMS-loss posture: losing CMK access renders that realm's secret
  values and TOTP seeds unrecoverable, while leaving the open plane intact. See
  [backup-and-recovery.md](backup-and-recovery.md).

Federation prerequisites — required only when a self-hosted realm participates
in cross-realm collaboration (a post-v0 epic; an isolated single-realm deployment
skips this):

- A stable public **FQDN and TLS certificate** for the realm endpoint. A
  self-hosted realm is reached by handle exactly like a managed one; peers and the
  blind relay route to that endpoint after resolving the handle.
- A **realm signing key** (the realm's published JWKS). Cross-realm envelopes are
  signed with this key and verified by peers; signing is mandatory, and an unsigned
  realm card is rejected.
- A **signed, well-known realm/agent card** served at
  `GET /.well-known/witself-card.json`, carrying the realm handle, advertised
  skills, endpoint, accepted auth, the signing public key, delivery modes, and a
  TTL. The card is a JWS over canonicalized JSON and is re-fetched on expiry.
- Registration of the FQDN + signing key in the shared global directory so the
  realm is resolvable to peers and the relay, and a documented **key-rotation and
  revocation** posture so a compromised realm key stops being honored promptly
  rather than at the next card TTL. See [agent-collaboration.md](agent-collaboration.md)
  and [deployment-cells.md](deployment-cells.md).

Deployment-cell prerequisites — required only when a self-hosted deployment runs
as more than one cell (whether a self-host is always a single cell or may itself be
a fleet is an Open decision in [deployment-cells.md](deployment-cells.md)):

- Per-cell operational ownership: each cell is one complete, isolated Witself stack
  (`witself-server`, PostgreSQL, sealed-plane KMS rooted
  in that cell, and blob storage) with its own backup, recovery, and KMS. A cell holds the full data
  and key material for its own tenants and depends on nothing in another cell.
- Tenant migration between cells, when used, leans on the first-class export/import
  for the open plane and a KMS **re-wrap** (decrypt-at-source / re-encrypt-at-dest)
  for the sealed plane, both audited end to end. See
  [backup-and-recovery.md](backup-and-recovery.md).

Data-at-rest protection for the open plane relies on ordinary RDS/disk
encryption; KMS is not a dependency for an open-plane-only deployment. The
sealed plane, when enabled, makes KMS a hard dependency rather than an optional
capability. Sealed-plane material is never embedded, recalled, placed in the
self-digest, or included in the plaintext export. See
[storage.md](storage.md) and [encryption-model.md](encryption-model.md).

## What Self-Hosted Operators Own

Self-hosted operators remain responsible for:

- Cloud account security.
- Kubernetes cluster security.
- IAM and workload identity.
- Database operations.
- **Memory store backup**: backing up PostgreSQL memory content, versions,
  evidence, lineage, curation state, and schema-32 vector profiles/rows;
  rebuilding full-text and any future ANN indexes.
- Capacity and policy for optional client-supplied vectors. Any model selection,
  model credentials, inference availability, and inference cost stay entirely
  in client software and are not `witself-server` configuration.
- Object/blob storage for exports, attachments, diagnostic bundles, and backup
  artifacts.
- **KMS configuration (sealed plane)**: provisioning the KMS provider and root
  CMK, granting the server workload identity wrap/unwrap on it, and owning per-realm
  KEK and DEK rotation including the `key.rotated` audit trail. Required only when
  the sealed plane is enabled; not needed for open-plane-only deployments. See
  [Sealed-Plane Configuration Guidance](#sealed-plane-configuration-guidance).
- Network ingress and TLS.
- Backups and disaster recovery execution.
- Terraform state protection.
- Helm values and Kubernetes Secret management (agent token files, database
  URLs, and — when the sealed plane is enabled —
  KMS provider configuration and the `WITSELF_KMS_KEY_ID` reference).
- Policy, security-group, and messaging configuration appropriate to their
  deployment (see [Identity Configuration Guidance](#identity-configuration-guidance)).
- **Federation (cross-realm, post-v0)**: the realm FQDN and TLS certificate, the
  realm signing key and its rotation/revocation, the published signed realm card,
  and the deny-by-default federation allow-list of accepted peer realm handles and
  keys. Required only when the realm participates in cross-realm collaboration. See
  [agent-collaboration.md](agent-collaboration.md).
- **Per-cell operations (multi-cell self-host)**: when a deployment runs more than
  one cell, the operator owns each cell as an isolated stack with its own Postgres,
  KMS, blob storage, and backup, plus the audited export/import + KMS re-wrap that a
  tenant migration between cells uses. See [deployment-cells.md](deployment-cells.md).
- Payment, billing, and support integrations they choose to wire themselves.

## Identity Configuration Guidance

Self-hosted operators configure the identity payload that the managed service
would otherwise tune for them. The following are operator responsibilities in
production self-hosting.

### Memory store and retrieval

- Provision PostgreSQL as the sole system of record for memories, facts,
  policies, groups, messages, transcript evidence, and curation state.
- Enable and operate PostgreSQL full-text indexes for the universal recall path.
  Capture, recall, export/import, and recovery must work with no pgvector
  extension and no model service.
- If optional vectors are used, size the migration-0032 JSONB tables. An
  authorized client supplies version/content-hash-bound
  memory vectors and per-request query vectors under immutable profiles.
- Monitor vector validation failures and profile coverage, not model-provider
  health. Missing, stale, or incompatible vectors fall back to full-text recall
  and do not make the server unready.
- Rebuild FTS and any future ANN indexes after import. If vectors need regeneration,
  a client does it and submits new rows; `witself-server` never performs
  inference or holds model credentials. See
  [narrative-memory-and-curation.md](narrative-memory-and-curation.md) and
  [backup-and-recovery.md](backup-and-recovery.md).

### Cross-agent policy

- The access-policy engine is default-deny; with no matching `allow` policy,
  cross-agent access is denied. Self-hosted operators own the policy objects that
  permit `read`, `contribute`, `curate`, and `forget` across agents and groups.
- Operator override applies within a realm and is audited like agent actions.
  Operators should confirm that cross-agent `curate`/`forget` require an audit
  `--reason` and confirmation in their deployment, and that destructive actions
  default to soft-delete/tombstone within the retention window.
- Use `policy test` to validate access decisions before relying on them. See
  [access-policy.md](access-policy.md).

### Security groups

- Security groups are realm-scoped and act as both policy subjects and policy
  targets, and may own group-scoped shared memories and facts. Operators own
  group membership and the agents granted `group:manage`.
- Group-owned destructive actions follow the same guardrails as cross-agent
  actions. See [security-groups.md](security-groups.md).

### Inter-agent messaging

- Messaging is fully in scope and durable; the mailbox/queue survives process and
  pod churn on the Postgres system of record. Operators size and back up the
  messaging tables along with the rest of the store.
- Sender identity is always derived from the authenticated token, never from
  input; sender forgery is structurally impossible. Operators own rate limits for
  send and delivery, and the `message:send`/`message:read` scope assignments.
- Treat message bodies and payloads as untrusted input to receiving agents,
  especially when a message would drive a memory or fact write. A message cannot
  itself authorize a cross-agent write; writes still require policy. See
  [inter-agent-messaging.md](inter-agent-messaging.md) and
  [threat-model.md](threat-model.md).

## Sealed-Plane Configuration Guidance

When the sealed plane (secrets, TOTP) is enabled, self-hosted operators configure
the confidentiality controls that the managed service would otherwise run for
them. These are operator responsibilities in production self-hosting and apply
only to the sealed plane; an open-plane-only deployment skips this section.

### KMS provider

- Provision a KMS provider and root CMK and configure it through
  `WITSELF_KMS_PROVIDER` (`aws-kms`, `gcp-kms`, or `azure-key-vault`) and
  `WITSELF_KMS_KEY_ID`. Do not run `local-dev` in production. Provide credentials
  and the key reference through Kubernetes Secrets and workload identity, not Helm
  values or environment files committed to source.
- Grant the `witself-server` workload identity wrap/unwrap permission on the CMK
  only. The server unwraps the active per-realm KEK (`realm_keys`) at startup and
  on reveal; readiness gates on KMS reachability whenever the sealed plane is on.
- Sealed-plane encryption is a CMK → per-realm KEK → per-secret/field DEK
  envelope (`XCHACHA20_POLY1305` / `AES_256_GCM`) with client-held and
  server-mediated decrypt behind the `client_side_decrypt` / `server_side_decrypt`
  capability switch. See [encryption-model.md](encryption-model.md) and
  [key-hierarchy.md](key-hierarchy.md).

### Key rotation

- Own per-realm KEK rotation (re-wrapping the `realm_keys` KEK under a new CMK
  version, updating `secret_deks.kek_id` in place) and DEK rotation as audited
  maintenance operations. Each rotation emits the `key.rotated` audit event and
  exposes no plaintext during re-wrap.
- Old ciphertext keeps its frozen `dek_version`; the current wrapping KEK is
  always resolved through `secret_deks.kek_id`, so historical reads continue to
  unwrap after rotation. See [key-hierarchy.md](key-hierarchy.md).

### Sealed-plane backup and crypto-shred

- Back up secrets as encrypted-only envelope ciphertext plus KMS key identity and
  rotation metadata — never key material, never plaintext. Secrets are excluded
  from the plaintext identity export; any encrypted secret export is a separate,
  explicit, audited flag.
- Understand the crypto-shred posture: losing CMK access renders that realm's
  secret values and TOTP seeds unrecoverable, while leaving the open plane
  (memories, facts) fully intact. Validate KMS access and KEK unwrap as part of
  disaster-recovery drills. See [backup-and-recovery.md](backup-and-recovery.md).

### Reveal and value-returning surfaces

- `witself secret reveal` and `witself totp code` are the only audited,
  value-returning sealed-plane operations and run the reveal ceremony; the open
  plane has no reveal because memories and facts are plainly readable. Operators
  own the `secret:reveal` / `totp:code` scope assignments and realm-role grants.
- For MCP exposure, `--no-value-tools` disables `witself.secret.reveal`,
  `witself.totp.code`, and value-returning `witself.reference.resolve`, while
  `--read-only` disables mutations. See
  [authorization-and-roles.md](authorization-and-roles.md) and
  [secret-model.md](secret-model.md).

## Managed Feature Differences

Self-hosted deployments may not include managed-service features unless the
operator configures equivalents:

- Witself-managed billing.
- Hosted payment flows.
- Crypto payment provider flows.
- Witself support ticket workflows.
- Managed abuse controls.
- Managed plan enforcement.
- Managed client-curation scheduling or optional vector-generation assistance;
  neither changes the backend no-inference boundary.
- Internal Witself staff admin workflows.

Account on self-host: a self-hosted deployment still has an account as the
top-level owner of its realms, but it is a single implicit deployment root with
managed billing, hosted payment, and plan enforcement capability-gated off. The
account is the unit usage aggregates to (and, when a self-host runs as a fleet of
independent cells, the unit cross-cell usage would aggregate to), but no managed
billing or charging runs against it; operators wire any billing they want
themselves. See [deployment-cells.md](deployment-cells.md).

The CLI should surface unavailable self-hosted features through
`witself capabilities` and deterministic `unsupported_operation` errors. The
capability contract reports full-text recall availability and optional
vector-profile support/coverage. It does not report an active backend model
provider because `witself-server` has none.

## Related Docs

- [self-hosting.md](self-hosting.md)
- [agent-collaboration.md](agent-collaboration.md)
- [deployment-cells.md](deployment-cells.md)
- [governance-and-support.md](governance-and-support.md)
- [requirements.md](requirements.md)
- [helm-chart.md](helm-chart.md)
- [terraform-infrastructure.md](terraform-infrastructure.md)
- [implementation-plan.md](implementation-plan.md)
- [backup-and-recovery.md](backup-and-recovery.md)
- [memory-model.md](memory-model.md)
- [access-policy.md](access-policy.md)
- [security-groups.md](security-groups.md)
- [inter-agent-messaging.md](inter-agent-messaging.md)
- [storage.md](storage.md)
- [encryption-model.md](encryption-model.md)
- [key-hierarchy.md](key-hierarchy.md)
- [secret-model.md](secret-model.md)
- [totp-2fa.md](totp-2fa.md)
- [authorization-and-roles.md](authorization-and-roles.md)
