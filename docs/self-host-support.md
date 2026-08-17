# Witself Self-Hosted Support

Status: implemented self-host preview. Production self-host support is not yet
an advertised entitlement; it may become paid or contracted after the
hardening and acceptance gates in the canonical
[Feature Status](feature-status.md) scorecard are closed.

Narrative-memory amendment (accepted 2026-07-14): self-host support includes
the deterministic PostgreSQL memory path, not a backend embedding provider.
Client curation and optional client-supplied vectors follow
[narrative-memory-and-curation.md](narrative-memory-and-curation.md).

> **Sealed-plane custody amendment (accepted 2026-07-18):**
> [ADR 0003](decisions/0003-client-custodied-agent-vault.md) and the
> [client-custodied vault plan](client-custodied-agent-vault.md) supersede the
> earlier KMS prerequisites. Self-hosted and managed servers use the same
> ciphertext-only backend: the AVK remains in the authorized client, and the
> backend has no decrypt key or server-decrypt route. No
> `WITSELF_SEALED_PLANE_ENABLED`, `WITSELF_KMS_PROVIDER`, or
> `WITSELF_KMS_KEY_ID` configuration is implemented or required for secrets.
> Any remaining KMS/realm-KEK or cross-cloud re-wrap language is superseded
> support-design history. Operators still own
> ordinary infrastructure encryption, database backups, and separate custody
> of client AVKs.

## Decision

Managed Witself Cloud is the default supported product.

Self-hosting is first-class in the sense that the public repo includes the
backend server, Helm chart, Pulumi cell tooling, configuration docs, migration
paths, PostgreSQL retrieval and optional client-vector guidance, and operational
guidance needed to run Witself outside Witself Cloud.

Self-hosting is not automatically a production support entitlement. Production
self-host support should be paid or contracted after the required production
hardening docs and operational paths exist.

Witself self-hosters protect the integrity and authenticity of **open-plane**
identity data and preserve **sealed-plane** ciphertext, public metadata, and
audit state. The authorized client holds the AVK and performs secret/TOTP value
encryption and decryption; the server has no decrypt key or plaintext reveal
path. Sealed values are never embedded, semantically recalled, included in the
self-digest, or plaintext-exported. See
[client-custodied-agent-vault.md](client-custodied-agent-vault.md).

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
  Pulumi cell tooling run against externally managed PostgreSQL, on a best-effort
  basis. Optional client vectors use migration-0032 JSONB and need no extension.
- Production self-hosted is the only tier that carries a support commitment, and
  only under a paid or contracted agreement.

The tiers span both planes. A deployment may run the open plane only or enable
the ciphertext-only sealed plane. The latter adds vault enrollment, ciphertext
backup, and client-AVK recovery responsibilities, not a server KMS dependency.

## Production Support Prerequisites

Witself should not claim production self-host support until these are real:

Open-plane (memory, fact, identity) prerequisites:

- Backup and restore documentation for PostgreSQL memory content and full-text
  index rebuilds, plus optional vector profile/row handling when enabled.
- Forward-only startup migration and recovery guidance for the embedded shared
  migration lock; the current chart has no separate migration Job or down path.
- Optional client-vector profile, validation, JSONB storage, coverage, and
  future ANN-projection guidance. Client software, not the server, owns vector
  generation after a profile change.
- Upgrade guide.
- Production Helm values examples.
- Pulumi state and configuration-management guidance.
- Observability guidance.
- Disaster recovery guidance.
- Security patch and release process.

Sealed-plane (secret, TOTP) prerequisites — required only when the sealed plane
is enabled:

- Documented client-vault enrollment, recovery, and rotation with fail-closed
  behavior when the local AVK is missing or mismatched.
- Ciphertext-only secret backup and restore that preserves vault-key bindings,
  immutable lifecycle state, and audit without exporting AVKs or plaintext.
- A tested client-AVK loss and recovery posture. Losing every valid AVK/recovery
  copy makes value fields unrecoverable while leaving open-plane data and public
  secret metadata intact.

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

- Per-cell operational ownership: each cell is one complete, isolated Witself
  stack (`witself-server`, PostgreSQL including sealed ciphertext, and blob
  storage) with its own backup and recovery. Client AVKs never enter the cell.
- Tenant migration between cells, when used, leans on first-class export/import
  for open-plane rows and sealed ciphertext; the client-held AVK does not move
  through either server. See
  [backup-and-recovery.md](backup-and-recovery.md).

Data-at-rest protection relies on ordinary database and disk encryption. Sealed
value confidentiality additionally relies on the client-held AVK; the backend
stores ciphertext and has no decrypt key. Sealed-plane material is never
embedded, recalled, placed in the self digest, or included in the plaintext
export. See [storage.md](storage.md) and
[client-custodied-agent-vault.md](client-custodied-agent-vault.md).

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
- **Client-vault recovery (sealed plane)**: preserving ciphertext backups and
  supporting client AVK enrollment, recovery, and rotation without receiving
  the AVK or plaintext. See
  [Sealed-Plane Configuration Guidance](#sealed-plane-configuration-guidance).
- Network ingress and TLS.
- Backups and disaster recovery execution.
- Pulumi state protection.
- Helm values and Kubernetes Secret management for agent token files, database
  URLs, object-store credentials, and infrastructure integrations. The chart
  has no sealed-plane decrypt key or KMS setting.
- Policy, security-group, and messaging configuration appropriate to their
  deployment (see [Identity Configuration Guidance](#identity-configuration-guidance)).
- **Federation (cross-realm, post-v0)**: the realm FQDN and TLS certificate, the
  realm signing key and its rotation/revocation, the published signed realm card,
  and the deny-by-default federation allow-list of accepted peer realm handles and
  keys. Required only when the realm participates in cross-realm collaboration. See
  [agent-collaboration.md](agent-collaboration.md).
- **Per-cell operations (multi-cell self-host)**: when a deployment runs more
  than one cell, the operator owns each isolated PostgreSQL/blob/backup stack
  plus audited export/import. Sealed ciphertext moves without server-side
  re-encryption; client AVKs remain outside every cell. See
  [deployment-cells.md](deployment-cells.md).
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

When the sealed plane (secrets, TOTP) is enabled, self-hosted deployments use
the same client-custodied vault contract as managed Witself Cloud.

### Client vault and recovery

- The authorized client creates and holds the AVK. The backend stores only a
  public vault-key binding, encrypted envelopes, ciphertext value fields,
  lifecycle metadata, and value-free audit records.
- Encryption, decryption, password generation, and TOTP calculation happen in
  the active client. The server has no decrypt key, KMS provider setting, or
  server-decrypt route.
- A missing or mismatched AVK fails closed. The backend never creates a
  replacement key for an existing binding.
- Back up ciphertext and binding metadata without AVKs or plaintext. Users must
  separately preserve valid AVK recovery material; losing every valid copy makes
  value fields unrecoverable while leaving open-plane data and public secret
  metadata intact. See
  [client-custodied-agent-vault.md](client-custodied-agent-vault.md) and
  [backup-and-recovery.md](backup-and-recovery.md).

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
- [Pulumi infrastructure guide](../infra/pulumi/README.md)
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
