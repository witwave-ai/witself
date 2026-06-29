# Witself Storage

Status: draft. Decision: production Witself starts with Postgres (with the
pgvector extension) as the system of record for both planes, an external
embedding-provider abstraction (`voyage` default), an object/blob adapter added
on demand, Goose for database migrations, and a provider-shaped KMS abstraction
(AWS KMS first) that backs the SEALED plane. Storage is two-tier: the OPEN plane
(memories + facts) is ordinary data-at-rest; the SEALED plane (secrets + TOTP)
is KMS-backed envelope encryption. KMS is **required when the sealed plane is
enabled** and is not a dependency for an open-plane-only deployment.

## Decision

Production storage should start with Postgres as the system of record, with the
`pgvector` extension enabled for memory embeddings.

Postgres holds both planes. OPEN-plane state:

- Account, realm, and agent metadata.
- Token hashes and token metadata.
- Memory records (content, kind, tags, source, salience, links, sensitive flag,
  timestamps) and their versioned edit history.
- Memory embedding vectors via `pgvector`.
- Fact records (name, value, primary flag, sensitive flag, format hint, source,
  timestamps) and their versioned edit history.
- Cross-agent access policies (the rename of Witpass's per-secret grants).
- Security groups and group membership.
- Inter-agent messages, per-recipient delivery, and read/ack state.
- Audit events.
- Usage counters and rate-limit state.
- Idempotency records.
- Capability and backend configuration metadata when needed.

SEALED-plane state (present only when the sealed plane is enabled):

- Secret metadata (template, label, owner, timestamps) and non-sensitive fields
  such as usernames, URLs, issuers, and labels as ordinary queryable values.
- Encrypted sensitive field blobs (envelope-encrypted; never plaintext columns).
- Encrypted TOTP seed material (envelope-encrypted high-value sealed material).
- Secret grants and TOTP enrollments.
- Per-realm KEK wrapping state (`realm_keys`) and per-secret/field DEK state
  (`secret_deks`); see [Sealed-Plane Encryption Storage](#sealed-plane-encryption-storage).

The two planes have opposite storage postures and must coexist coherently:

- **OPEN plane.** Memory content and fact values are ordinary identity data.
  They are protected by data-at-rest encryption, but there is no requirement to
  keep them out of queryable columns and no encrypted-blob-only storage rule.
  Witself protects the *integrity and authenticity* of identity data here.
- **SEALED plane.** Secret values and TOTP seeds are confidentiality-critical.
  They are stored only as KMS-backed envelope ciphertext (CMK → per-realm KEK →
  per-secret/field DEK), are reveal-gated, and are **never embedded, never
  returned by semantic recall, never in the self-digest, and never in a
  plaintext export**. See [Data-At-Rest Note](#data-at-rest-note),
  [encryption-model.md](encryption-model.md), and
  [threat-model.md](threat-model.md).

Object/blob storage should be added when the data shape actually requires it,
not as a default dependency for every memory or fact. Good object/blob
candidates:

- Large structured/plaintext identity exports.
- Diagnostic bundles.
- Support attachments.
- Backup artifacts.
- Future import/export jobs that should not sit in ordinary relational rows.

Default v0 memory content and fact values stay inline in Postgres within the
limits defined in [memory-model.md](memory-model.md) and
[facts-model.md](facts-model.md). Oversized exports, attachments, and backup
artifacts move to object/blob storage when that path is implemented.

## Core Tables

The relational shape follows the identity hierarchy in
[requirements.md](requirements.md): account → realm → agent, with memories,
facts, policies, groups, and messages scoped to a realm. The shapes below are
the storage view of the domain objects defined in
[json-contracts.md](json-contracts.md); they are illustrative, not the migration
source of truth.

- `realms` — `realm_…` id, owning account, name, timestamps. The rename of the
  Witpass vault.
- `agents` — `agent_…` id, realm, name, state (`active`/`disabled`/`archived`),
  timestamps. The durable named principal; identity is derived from the token,
  never from input.
- `tokens` — `tok_…` id, agent, token hash, scopes, optional expiry, state.
  Raw tokens are never stored. See [token-lifecycle.md](token-lifecycle.md).
- `memories` — `mem_…` id, realm, owner (agent or group), content, kind, tags,
  source, salience, links, sensitive flag, timestamps (`created_at`,
  `updated_at`, `last_accessed_at`), tombstone state for soft `forget`.
- `memory_versions` — append-only edit history per memory: version number,
  actor, timestamp, changed fields.
- `memory_embeddings` — `pgvector` column keyed by memory id, plus the provider,
  model, and dimensionality the vector was computed with (see
  [Embedding Storage](#embedding-storage)).
- `facts` — `fact_…` id, realm, owner (agent or group), name (unique per owner),
  value, primary flag, sensitive flag, format hint, source, timestamps.
- `fact_versions` — append-only edit history per fact, same shape as
  `memory_versions`.
- `policies` — `pol_…` id, realm, subject (agent or group), permission verb
  (`read`/`contribute`/`curate`/`forget`), target (agent or group), scope
  (memories/facts/both), optional filter, effect (`allow`), metadata.
- `groups` — `grp_…` id, realm, name (unique per realm), owner/admins,
  timestamps.
- `group_members` — group id × agent id membership rows.
- `messages` — `msg_…` id, realm, `from` agent (always token-derived), `to`
  (agent or group), subject/kind, body, optional payload, optional
  thread/conversation id, `created_at`.
- `message_deliveries` — per-recipient delivery rows with ordering, read, and
  ack state for the mailbox/queue model.
- `audit_events` — `aud_…` id, realm, actor, event name, target ids, decision
  outcome, reason, timestamp. Never identity content. See
  [audit-retention.md](audit-retention.md).
- `usage_counters`, `idempotency_records`, `capabilities`/config — operational
  state shared with the platform spine.

Sealed-plane tables (present only when the sealed plane is enabled; the storage
view of the domain objects in [data-model.md](data-model.md) and
[secret-model.md](secret-model.md)):

- `secrets` — `sec_…` id, realm, owner (agent or group), template
  (`login`/`api-key`/`ssh-key`/`certificate`/`env`/`generic`), label, state
  (`active`/`archived`), timestamps. Witpass's `shared` secrets are now
  group-owned, so `owner_kind ∈ {agent, group}` matches memories and facts.
- `secret_fields` — `fld_…` id, secret, field name, sensitivity. Non-sensitive
  field values are ordinary columns; sensitive field values are stored only as
  envelope ciphertext (DEK id, AEAD algorithm, nonce, ciphertext) — never as a
  plaintext column.
- `secret_grants` — `grt_…` id, secret, grantee (agent or group), granted
  scopes, granting actor, timestamps. The sealed-plane cross-agent/operator
  access path; see [authorization-and-roles.md](authorization-and-roles.md).
- `totp_enrollments` — `totp_…` id, owning secret/account, issuer, label, and
  the envelope-encrypted seed material (DEK id, AEAD algorithm, nonce,
  ciphertext). Seeds are high-value sealed material; see [totp-2fa.md](totp-2fa.md).
- `realm_keys` — `kek_…` id, realm, KMS provider, CMK key id, wrapped per-realm
  KEK ciphertext, rotation generation, timestamps. One active KEK generation per
  realm; see [Sealed-Plane Encryption Storage](#sealed-plane-encryption-storage)
  and [key-hierarchy.md](key-hierarchy.md).
- `secret_deks` — `dek_…` id, realm, owning secret/field, wrapping `kek_…`, AEAD
  algorithm, wrapped DEK ciphertext, generation. Per-secret/field data keys are
  wrapped by the realm KEK, never stored unwrapped.
- `attachments` — `att_…` id, owning secret, envelope-encrypted blob reference
  (object/blob when oversized); see
  [secret-size-and-attachments.md](secret-size-and-attachments.md).

Ownership and uniqueness rules:

- Default ownership is the creating agent. Group-scoped memories and facts use
  the same `mem_`/`fact_` shapes with a group owner; see
  [security-groups.md](security-groups.md).
- Fact `name` is unique per owner. Different owners may reuse the same name
  because ownership disambiguates them.
- At most one primary fact per logical kind per owner is enforced at the storage
  layer as part of the atomic promotion in [facts-model.md](facts-model.md).
- Memories have no unique name; they are addressed by `id`, recalled
  semantically, and filtered by metadata.

## Policy Storage Replaces Per-Secret Grants

Witpass stored authorization as per-secret access grants. Witself replaces that
with first-class, evaluable **policy** rows.

- A policy binds a subject (agent or group) × permission × target (agent or
  group), scoped to memories and/or facts, optionally filtered by kind/tag/name
  or sensitivity, with an `allow` effect under a default-deny stance.
- Policies are realm-scoped rows; there is no per-record grant table. Access
  decisions are computed by evaluating policy rows against the requested
  subject/permission/target/scope.
- `policy test` is a read-only evaluation over these rows and persists nothing.
- Group membership participates in evaluation: a group subject grants its
  permission to every current member; a group target receives permissions on
  behalf of its members.

The evaluation engine, verb semantics, guardrails, and operator override are
specified in [access-policy.md](access-policy.md). Cross-agent mutations are
attributed in audit (for example, "memory `mem_…` of agent A was pruned by agent
B under policy `pol_…`") per [audit-retention.md](audit-retention.md).

## Embedding Storage

Memory recall is semantic by default, so embedding vectors are first-class
stored state, not a cache that can be silently lost.

- Vectors are stored in Postgres via the `pgvector` extension, keyed by memory
  id, alongside the provider, model, and dimensionality used to produce them.
- The embedding migration enables the `pgvector` extension and creates the
  vector column and its index. Vector index type and parameters (for example an
  approximate index) are tuned per [memory-model.md](memory-model.md).
- The embedding **provider is external** and behind a provider abstraction that
  mirrors the KMS abstraction in [KMS Posture](#kms-posture): `voyage`
  (default), `openai`, and
  `local-dev`. Provider and model are selected with
  `WITSELF_EMBEDDINGS_PROVIDER` and `WITSELF_EMBEDDINGS_MODEL` and reported by
  the capabilities contract.
- Witself does not host the embedding model. The provider is a configurable
  production dependency, not part of the storage engine.

Recomputability:

- Vectors are **recomputable** from memory content. If a vector is lost or the
  model changes, recall can be restored by re-embedding from the source content.
- Re-embedding on provider/model change is an explicit, audited maintenance
  operation, not an automatic side effect of reads or writes.
- If the embedding provider is unavailable or disabled, recall degrades
  deterministically to keyword/tag/kind/time ranking and the capability contract
  reports the degraded state. Plain `read`/`get` by id and metadata `list` never
  depend on the provider.

Because vectors are stored, a backup that includes them can restore semantic
recall without re-embedding (vectors are optional to back up; see
[Backup And Restore Implications](#backup-and-restore-implications)); because
vectors are recomputable, a model migration — or a restore without vectors — can
rebuild them from content. Vector storage size is a metered dimension; see
[billing-and-limits.md](billing-and-limits.md).

## KMS Posture

The sealed plane is backed by a key-management service. KMS is a **required
dependency when the sealed plane is enabled** and is not required for an
open-plane-only deployment — an account that only uses memories and facts never
needs a KMS provider configured. This is the two-tier posture: the open plane
relies on ordinary data-at-rest encryption (managed RDS/disk), while the sealed
plane relies on KMS envelope encryption.

The KMS boundary is provider-shaped from day one, mirroring how the embedding
provider is abstracted. Initial provider names:

- `aws-kms`
- `gcp-kms`
- `azure-key-vault`
- `local-dev`

`local-dev` exists for tests, demos, and `witself-server serve --dev`. It is not
a production KMS provider.

KMS is **per cell**. A cell is one complete, independent Witself stack in a
single cloud account/region, and its sealed plane is rooted in that cell's own
KMS (its CMK → per-realm KEK → per-secret/field DEK hierarchy). No CMK, KEK, or
DEK is shared across cells, and the thin global control plane holds only routing
metadata (realm/account → home cell + endpoint + signing key) — never tenant
data and never key material. Moving a tenant between cells therefore re-roots its
sealed plane under the destination cell's KMS; see
[Cross-Cell KMS Re-Wrap](#cross-cell-kms-re-wrap) and
[deployment-cells.md](deployment-cells.md).

Managed Witself Cloud uses AWS KMS first, alongside AWS RDS for Postgres (with
`pgvector`) and S3; see [First Cloud Target](#first-cloud-target). Self-hosted
deployments that enable the sealed plane select a provider through server
configuration:

```text
WITSELF_KMS_PROVIDER=aws-kms
WITSELF_KMS_KEY_ID=arn:aws:kms:...
```

Provider-specific credentials should come from workload identity, cloud
identity, mounted secret files, or deployment-native secret managers. They must
not be committed to config files, Terraform examples, Helm values, or the public
repository. Managed cloud must not expose raw KMS credentials to the
application; the server uses deployment identity and tightly scoped IAM
permissions.

Readiness gates differ per plane. `pgvector` is a hard gate for the open plane
(semantic recall) and the embedding provider is degradable. KMS readiness gates
**only the sealed plane**: when the sealed plane is enabled, the server must be
able to reach the configured KMS provider and unwrap the active per-realm KEK
before serving secret reveal, TOTP code, or value-returning reference
resolution. Open-plane reads never gate on KMS availability. The capability
contract reports `client_side_decrypt` and `server_side_decrypt` availability;
see [key-hierarchy.md](key-hierarchy.md).

## Sealed-Plane Encryption Storage

Postgres may store encrypted secret blobs for the sealed plane, but it must
never store plaintext secret values or TOTP seeds as ordinary columns.

Rules:

- Sensitive field values are envelope-encrypted before storage.
- TOTP seeds are envelope-encrypted before storage.
- Token values are hashed, not stored raw. Base64 is serialization only, not
  encryption.
- Non-sensitive fields such as usernames, URLs, issuers, and labels may be
  stored as ordinary queryable values.
- Audit records must never store secret values, TOTP seeds, generated TOTP
  codes, raw tokens, passphrases, plaintext private keys, payment credentials,
  or wallet credentials; see [audit-retention.md](audit-retention.md).

The envelope is a CMK → per-realm KEK → per-secret/field DEK hierarchy. The
relational shape that holds it is two tables:

- `realm_keys` — the per-realm KEK (`kek_…`), wrapped by the KMS CMK
  identified by `WITSELF_KMS_KEY_ID`. The KEK ciphertext, the KMS provider and
  CMK id, and the rotation generation live here; the unwrapped KEK exists only
  in memory after a KMS unwrap.
- `secret_deks` — the per-secret/per-field DEK (`dek_…`), wrapped by the active
  realm KEK. Sensitive field and TOTP-seed ciphertext records the wrapping
  `dek_…`, the AEAD algorithm (`XCHACHA20_POLY1305` or `AES_256_GCM`), and the
  nonce.

Decrypt is hybrid behind one capability switch: `client_side_decrypt` (the
client holds key material and the server returns wrapped material) is the
default where the client can hold keys; `server_side_decrypt` lets token-only
pods perform the unwrap server-side, expanding the trusted computing base and
recorded on the reveal/code audit event. The exact envelope format and rotation
design are tracked by [encryption-model.md](encryption-model.md) and
[key-hierarchy.md](key-hierarchy.md); the schema is in
[data-model.md](data-model.md).

Sealed-plane carve-outs hold at the storage layer: secret values and TOTP seeds
are **never embedded** (no `pgvector` row), **never returned by semantic
recall**, **never in the self-digest**, **never ingested** from
CLAUDE.md/AGENTS.md, and **never written to a plaintext export**. Secret backup
is encrypted-only; see [Backup And Restore Implications](#backup-and-restore-implications).

## Cross-Cell KMS Re-Wrap

Witself deploys as a fleet of independent cells under a thin global control
plane: each cell is one complete, independent stack (its own Postgres + pgvector,
its own KMS, its own blob store) and holds the full data and key material for the
tenants homed on it. The control plane holds only routing metadata — the
realm/account → home cell + endpoint + signing key mapping — and **no tenant
data and no key material**. See [deployment-cells.md](deployment-cells.md).

Because KMS is per cell (see [KMS Posture](#kms-posture)), the sealed plane's
CMK → per-realm KEK → per-secret/field DEK hierarchy is rooted in the *home
cell's* KMS. A tenant's wrapped KEK and wrapped DEKs only resolve under that
cell's CMK; nothing in another cell can unwrap them. This is the same
blast-radius containment the open plane gets from per-cell Postgres.

Moving a realm/account from cell A to cell B therefore cannot just copy the
encrypted blobs — cell B's KMS cannot unwrap material rooted in cell A's CMK. The
sealed plane migrates by an audited **re-wrap**:

- **Decrypt-at-source.** Under cell A's KMS, unwrap the per-realm KEK and unwrap
  each affected DEK (key-on-key; the at-rest `ciphertext` is never decrypted and
  no secret value or TOTP seed is exposed).
- **Re-encrypt-at-dest.** Under cell B's KMS, mint the destination-rooted
  per-realm KEK and re-wrap the DEKs beneath it, writing the destination
  `realm_keys` / `secret_deks` rows. The DEK ciphertext stays put; only the
  wrapping changes, exactly as in the in-cell KEK re-wrap of
  [key-hierarchy.md](key-hierarchy.md) (Rotation, Re-Wrap, And Backfill), here
  crossing a cloud/KMS boundary rather than rotating in place.

The plaintext DEKs transit only in memory on the migration path and are zeroized
after; CMK-rooted plaintext secret values and TOTP seeds are never materialized.
The operation is audited end to end — it emits `tenant.migration_started`,
`tenant.migration_completed`, or `tenant.migration_failed` alongside the
sealed-plane re-wrap, per [deployment-cells.md](deployment-cells.md) and the
audit-event registry in [audit-retention.md](audit-retention.md). After the
control-plane mapping is repointed to cell B, clients re-resolve their home cell
and route to B directly.

The open plane (memories, facts, messaging) moves over the same migration by the
first-class export/import path rather than a re-wrap, since it carries no KMS
envelope; embeddings are recomputed in the destination or moved directly when the
destination cell uses the same embedding model. See
[backup-and-recovery.md](backup-and-recovery.md) and
[Backup And Restore Implications](#backup-and-restore-implications).

Open decisions (tracked in [deployment-cells.md](deployment-cells.md), not
resolved here): whether the placement/migration unit is the account or the realm,
and whether cutover uses a brief read-only freeze or dual-write + reconcile —
either way the sealed-plane re-wrap is the same audited decrypt-at-source /
re-encrypt-at-dest pass.

## Migration Tool

Witself should use Goose for database migrations.

Migration requirements:

- SQL migrations should live in the public repo.
- `witself-server migrate status` shows current and pending migrations.
- `witself-server migrate up` applies forward migrations.
- `witself-server migrate down` is guarded and requires explicit confirmation
  when destructive or backward changes are possible.
- Migration commands should acquire a Postgres advisory lock so concurrent
  `witself-server` instances do not race migrations.
- The migration that introduces semantic recall enables the `pgvector`
  extension and is ordered before any migration that creates a vector column.
- Helm should expose an explicit migration Job path.
- Production chart guidance should prefer running migrations as a controlled
  operation before rolling `witself-server`; automatic migrations are opt-in
  and explicit.
- CI should validate migration ordering and apply migrations against a test
  Postgres instance with `pgvector` available when practical.

The public customer/operator CLI should not manage database migrations.
Migration commands belong to the separate `witself-server` binary; see
[server-command-surface.md](server-command-surface.md).

## Object/Blob Adapter

Object/blob storage is an on-demand adapter, not a per-record dependency.

- It backs large exports, diagnostic bundles, support attachments, and backup
  artifacts; identity records themselves stay in Postgres.
- The adapter is provider-shaped (AWS S3 first; GCP and Azure equivalents
  planned) and selected through server configuration.
- Provider credentials should come from workload identity, cloud identity,
  mounted secret files, or deployment-native secret managers. They must not be
  committed to config files, Terraform examples, Helm values, or the public
  repo.
- Self-hosted deployments may run without object/blob storage until they need
  large exports or attachments; the capability contract reports availability.

Open-plane identity export and import use this adapter for large plaintext
artifacts. Oversized sealed-plane attachments use the same adapter but stay
envelope-encrypted (the encrypted-blob path), never plaintext; see
[secret-size-and-attachments.md](secret-size-and-attachments.md) and
[backup-and-recovery.md](backup-and-recovery.md).

## First Cloud Target

Witself implements AWS first.

Reasoning:

- AWS is the recommended first cloud target for managed deployment.
- AWS RDS for Postgres (with `pgvector`) and S3 are mature and well understood
  by infrastructure teams.
- Starting with one managed substrate keeps the first implementation focused.
- Provider-neutral storage and object/blob interfaces still let self-hosted
  deployments target other clouds.

Managed cloud should not expose raw database, object-store, or embedding-provider
credentials to the application. The server should use deployment identity and
tightly scoped IAM permissions.

AWS is also the first cloud infrastructure target for managed Witself Cloud and
self-hosted Terraform. GCP and Azure remain planned follow-up targets; see
[cloud-targets.md](cloud-targets.md).

## Data-At-Rest Note

Decision: encryption is two-tier, matched to the plane. The open plane uses
ordinary data-at-rest protection for identity data; the sealed plane uses a KMS
envelope for secret material.

Open-plane posture:

- Use ordinary data-at-rest encryption (managed RDS/disk, or self-host-owned
  disk encryption) for memories and facts. There is no reveal ceremony for
  identity data and no encrypted-blob-only rule; the open plane protects
  integrity and authenticity, not confidentiality.
- For memories and facts, `sensitive` is a PII/redaction display flag, not an
  encryption boundary. There is no value-size split between sensitive and
  non-sensitive identity values. A credential does not belong in a sensitive
  fact; it belongs in the sealed plane as a secret; see
  [facts-model.md](facts-model.md) and [secret-model.md](secret-model.md).
- Token values are hashed, not stored raw. Base64 is serialization only, not
  encryption.

Sealed-plane posture:

- When the sealed plane is enabled, secret values and TOTP seeds are
  KMS-envelope-encrypted (CMK → per-realm KEK → per-secret/field DEK) and stored
  only as ciphertext, with a reveal ceremony gating value-returning operations.
  KMS is required for this plane; see [KMS Posture](#kms-posture),
  [Sealed-Plane Encryption Storage](#sealed-plane-encryption-storage), and
  [encryption-model.md](encryption-model.md).
- Losing KMS key material makes the affected realm's secret values and TOTP
  seeds unrecoverable (crypto-shred). This blast radius is confined to the
  sealed plane: it does **not** affect memories, facts, policies, groups, or
  messages.

The cross-agent authorization and audit scaffolding for the open plane lives in
[access-policy.md](access-policy.md); the sealed-plane confidentiality model
lives in [encryption-model.md](encryption-model.md) and
[key-hierarchy.md](key-hierarchy.md).

## Backup And Restore Implications

Backups must include enough state to restore the full system; semantic recall
can then be restored either from backed-up vectors or by re-embedding:

- Postgres backup (the required system of record).
- `pgvector` vector data is **optional** to back up — it is recomputable from
  memory content, so including it is an optimization that avoids a re-embedding
  pass, not a must-include.
- Object/blob storage backup when used.
- Migration version.
- Embedding-provider and model identity (so restored vectors are interpreted
  consistently).
- Server configuration needed to reconnect to storage, the embedding provider,
  and (when the sealed plane is enabled) KMS.
- KMS key identity and rotation metadata when the sealed plane is enabled, so
  the encrypted secret backup can be unwrapped.

Open-plane identity (memories + facts) is plaintext-exportable; that stays the
headline backup/restore feature. The sealed plane is **never in the plaintext
export** — secret backup is encrypted-only (envelope ciphertext plus KMS key
identity, never plaintext), and `ws export` excludes secret values and TOTP
seeds; see [backup-and-recovery.md](backup-and-recovery.md).

Backups must not include raw tokens, raw database, object-store, or KMS
credentials, embedding-provider credentials, payment provider secrets, wallet
credentials, or plaintext secret material or TOTP seeds.

Recovery characteristics specific to Witself:

- When vectors are included in the backup, restoring Postgres restores semantic
  recall directly with no re-embedding pass. When they are not, recall is
  restored by re-embedding from memory content.
- Because vectors are recomputable from content, a deliberate embedding-model
  change can also rebuild them as an explicit, audited operation.
- Losing KMS key material (when the sealed plane is enabled) makes the affected
  realm's secret values and TOTP seeds unrecoverable — crypto-shred — but does
  not affect memories, facts, policies, groups, or messages. The open plane is
  recoverable from the Postgres backup independent of KMS. This split blast
  radius must be documented for managed and self-hosted deployments.

The backup, export, and recovery policy is tracked in
[backup-and-recovery.md](backup-and-recovery.md).

## Related Docs

- [requirements.md](requirements.md)
- [backend-architecture.md](backend-architecture.md)
- [data-model.md](data-model.md)
- [memory-model.md](memory-model.md)
- [facts-model.md](facts-model.md)
- [secret-model.md](secret-model.md)
- [totp-2fa.md](totp-2fa.md)
- [encryption-model.md](encryption-model.md)
- [key-hierarchy.md](key-hierarchy.md)
- [authorization-and-roles.md](authorization-and-roles.md)
- [secret-size-and-attachments.md](secret-size-and-attachments.md)
- [access-policy.md](access-policy.md)
- [security-groups.md](security-groups.md)
- [inter-agent-messaging.md](inter-agent-messaging.md)
- [json-contracts.md](json-contracts.md)
- [self-hosting.md](self-hosting.md)
- [server-command-surface.md](server-command-surface.md)
- [helm-chart.md](helm-chart.md)
- [terraform-infrastructure.md](terraform-infrastructure.md)
- [cloud-targets.md](cloud-targets.md)
- [deployment-cells.md](deployment-cells.md)
- [audit-retention.md](audit-retention.md)
- [billing-and-limits.md](billing-and-limits.md)
- [backup-and-recovery.md](backup-and-recovery.md)
- [threat-model.md](threat-model.md)
- [implementation-plan.md](implementation-plan.md)
