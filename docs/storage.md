# Witself Storage

Status: draft. Last reviewed 2026-07-14. Decision: production Witself starts
with PostgreSQL as the system of record for both planes, universal full-text
retrieval, optional portable JSONB storage for client-supplied vectors, an
object/blob adapter added on demand, Goose for database migrations, and a
provider-shaped KMS abstraction
(AWS KMS first) that backs the SEALED plane. Storage is two-tier: the OPEN plane
(memories + facts) is ordinary data-at-rest; the SEALED plane (secrets + TOTP)
is KMS-backed envelope encryption. KMS is **required when the sealed plane is
enabled** and is not a dependency for an open-plane-only deployment.

Open-plane amendment (accepted 2026-07-14): PostgreSQL remains the sole
authoritative memory store, but
[narrative-memory-and-curation.md](narrative-memory-and-curation.md) supersedes
the server-side embedding-provider boundary. Full-text retrieval is universal;
optional memory/query vectors are generated and supplied by clients. Object
storage and local outboxes are archive/delivery mechanisms, not live memory
sources.

## Decision

Production storage starts with PostgreSQL as the system of record. Its native
full-text facilities are required for memory recall. Migration `0032` stores
optional client-supplied vector profiles and arrays in ordinary PostgreSQL
JSONB. `pgvector` is not required; a future pgvector/ANN projection may
accelerate candidate generation without changing the canonical data contract.

PostgreSQL holds both planes. OPEN-plane state:

- Account, realm, and agent metadata.
- Token hashes and token metadata.
- Memory records (content, kind, tags, source, salience, links, sensitive flag,
  timestamps) and their versioned edit history.
- Optional version/profile-keyed memory vectors supplied by authorized clients.
- Fact records (name, value, primary flag, sensitive flag, format hint, source,
  timestamps) and their versioned edit history.
- Cross-agent access policies (the rename of Witpass's per-secret grants).
- Security groups and group membership.
- Inter-agent messages, per-recipient delivery, and read/ack state.
- Visible transcript conversations and append-only user/assistant/system/tool
  entries, including bounded structured JSON payloads.
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
- `tokens` — `tok_…` id, principal, token hash, immutable access profile,
  optional display name/expiry, and state. `full` is the compatibility default;
  expiring `curator-preview` and `curator-apply` are valid only for agent
  tokens. Raw tokens are never stored. See
  [token-lifecycle.md](token-lifecycle.md).
- `memories` — `mem_…` id, realm, owner (agent or group), content, kind, tags,
  source, salience, links, sensitive flag, timestamps (`created_at`,
  `updated_at`, `last_accessed_at`), tombstone state for soft `forget`.
- `memory_versions` — append-only edit history per memory: version number,
  actor, timestamp, changed fields.
- `memory_vector_profiles` / `memory_vectors` (migration `0032`, optional use) —
  immutable client-declared profiles and version/content-hash-bound portable
  JSONB rows (see [Optional Vector Storage](#optional-vector-storage)).
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
- `agent_messages` — `msg_…` id, account/realm, token-derived sender, resolved
  direct recipient, subject/kind, body, optional object payload, thread id,
  migration-0033 causal parent, migration-0035 backend-derived causal depth,
  optional sender-scoped idempotency key, and `created_at`.
  Explicit-list/realm/group audiences remain future additions.
  Current write surfaces normalize omitted kind to actionable `request`;
  explicit `note` is FYI-only to the runner.
- `agent_message_deliveries` — one row per message/recipient with
  delivery/read/ack state plus migration-0034 independent
  `available`/`claimed`/`completed` processing state, monotonic generation,
  claim/complete retry hashes, database-time lease, and unique result-message
  link. Migration 0036 adds the separate durable `failure_count` for exact-fence
  releases marked as deterministic message failures; generation remains only
  the stale-writer fence. Import interrupts active claims while preserving
  completed links and failure counts.
- A runner's private content-free notification ledger is derived client-local
  operational state, not a second message store. It carries message/thread/
  sender pointers only; PostgreSQL remains authoritative for content and
  delivery state. The ledger is excluded from account export even though its
  canonical messages are included. Because the runner globally acknowledges a
  delivery after recording its local pointer, another host/runtime for the same
  agent cannot see that pointer or rediscover the delivery as unread; this is an
  intentional MVP locality limitation.
- Runner cycle health in the same private state is also derived and
  content-free: timestamps, bounded status/error class, and consecutive failure
  count only.
- A runner's provider-auth capture is a separate mode-0600 client-local file
  bound to one configured native provider. It is not PostgreSQL, runner config,
  service-definition content, or account-export data.
- `transcript_conversations` — `trn_…` id, account/realm, owning recorder agent,
  optional external conversation id/title/metadata, and sequence allocator.
- `transcript_entries` — `ent_…` id, transcript, monotonically ordered role,
  visible body, optional bounded JSON payload/model/reply link, and the
  token-derived recorder. Raw hidden model reasoning is never stored.
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
- Memories have no unique name; they are addressed by `id`, recalled through
  full-text/time/metadata ranking with optional compatible vectors, and filtered
  by metadata.

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

## Optional Vector Storage

Vectors are optional derived indexes. PostgreSQL full-text, time, metadata,
salience, and recency ranking remain available when no vectors exist.

- An authorized client supplies both memory vectors and query vectors under an
  immutable profile that declares model/recipe identity, dimensions, distance
  metric, and normalization.
- A stored vector binds to the exact memory id, version, content hash, and
  profile. It never replaces memory content as the source of truth.
- The backend validates authorization, finite values, dimensions, profile,
  version, and content hash, then performs deterministic bounded similarity
  math over canonical JSONB arrays. It never calls a model, chooses a provider,
  or generates, repairs, or re-embeds a vector.
- Missing, stale, or incompatible vectors use the universal lexical path and
  report profile coverage. They are not a server dependency failure.
- Client software may regenerate vectors after a profile change and resubmit
  them. PostgreSQL index rebuilds are ordinary database maintenance and do not
  involve inference.

The optional profile and vector rows participate in schema-32 logical account
archives. Import validates profile contracts, owner/version/content-hash scope,
vector values and hashes, chronology, and exact table membership. Derived FTS
and any future ANN projection are rebuilt after import; canonical JSONB vector
rows are imported data, not a rebuild-only index.
No `WITSELF_EMBEDDINGS_*` server configuration or model credential exists.
Vector storage size may remain a metered dimension; see
[billing-and-limits.md](billing-and-limits.md) and
[narrative-memory-and-curation.md](narrative-memory-and-curation.md).

## KMS Posture

The sealed plane is backed by a key-management service. KMS is a **required
dependency when the sealed plane is enabled** and is not required for an
open-plane-only deployment — an account that only uses memories and facts never
needs a KMS provider configured. This is the two-tier posture: the open plane
relies on ordinary data-at-rest encryption (managed RDS/disk), while the sealed
plane relies on KMS envelope encryption.

The KMS boundary is provider-shaped from day one because sealed-plane key
custody is a real server responsibility. Initial provider names:

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

Managed Witself Cloud uses AWS KMS first, alongside AWS RDS for PostgreSQL and
S3; see [First Cloud Target](#first-cloud-target). Self-hosted
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

Readiness gates differ per plane. PostgreSQL and its full-text facilities gate
the open plane. Migration-0032 vector-profile operations use ordinary
PostgreSQL and require no extension; lexical recall remains healthy without any
vector rows. A future ANN projection must never become a readiness gate. KMS
readiness gates
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
are **never embedded** (no `memory_vectors` row), **never returned by semantic
recall**, **never in the self-digest**, **never ingested** from
CLAUDE.md/AGENTS.md, and **never written to a plaintext export**. Secret backup
is encrypted-only; see [Backup And Restore Implications](#backup-and-restore-implications).

## Cross-Cell KMS Re-Wrap

Witself deploys as a fleet of independent cells under a thin global control
plane: each cell is one complete, independent stack (its own PostgreSQL, KMS,
and blob store) and holds the full data
and key material for the tenants homed on it. The control plane holds only
routing metadata — the
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
envelope. Full-text indexes are rebuilt in the destination. Optional vector
rows move with their immutable profiles as canonical schema-32 archive data;
any future ANN projection is rebuilt. A profile with no vector rows still leaves
the destination fully functional through lexical recall. See
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
- Full-text indexes are part of the universal PostgreSQL memory path. Migration
  `0032` creates portable `memory_vector_profiles` and JSONB `memory_vectors`
  tables on ordinary PostgreSQL. A future ANN migration must be additive and
  must not make pgvector a gate for lexical or exact JSONB hybrid recall.
- Helm should expose an explicit migration Job path.
- Production chart guidance should prefer running migrations as a controlled
  operation before rolling `witself-server`; automatic migrations are opt-in
  and explicit.
- CI validates migration ordering and the complete vector/archive contract
  against ordinary PostgreSQL. Any future ANN projection adds separate optional
  extension tests without changing that baseline.

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
- AWS RDS for PostgreSQL and S3 are mature and well understood
  by infrastructure teams.
- Starting with one managed substrate keeps the first implementation focused.
- Provider-neutral storage and object/blob interfaces still let self-hosted
  deployments target other clouds.

Managed cloud should not expose raw database or object-store credentials to the
application. The server should use deployment identity and tightly scoped IAM
permissions. It has no backend model-provider credentials.

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

Backups must include enough state to restore the full system. Universal lexical
recall is restored from PostgreSQL content and rebuilt full-text indexes:

- Postgres backup (the required system of record).
- Migration-0032 vector profile/row data is included in PostgreSQL backup and in
  every schema-32 logical account archive. It is derived client-authored data;
  an older archive with no such tables can still upgrade into lexical recall,
  and only a client may regenerate missing vectors.
- Object/blob storage backup when used.
- Migration version.
- Immutable vector profile definitions when vector rows are retained, so the
  restored rows are interpreted consistently.
- Server configuration needed to reconnect to storage and, when the sealed
  plane is enabled, KMS. There is no backend model-provider configuration.
- KMS key identity and rotation metadata when the sealed plane is enabled, so
  the encrypted secret backup can be unwrapped.

Open-plane identity (memories + facts) is plaintext-exportable; that stays the
headline backup/restore feature. The sealed plane is **never in the plaintext
export** — secret backup is encrypted-only (envelope ciphertext plus KMS key
identity, never plaintext), and `witself export` excludes secret values and TOTP
seeds; see [backup-and-recovery.md](backup-and-recovery.md).

Backups must not include raw tokens, raw database, object-store, or KMS
credentials, client model credentials, payment provider secrets, wallet
credentials, or plaintext secret material or TOTP seeds.

Recovery characteristics specific to Witself:

- Restoring PostgreSQL restores memory content and lexical recall. Retained
  vector rows restore optional vector coverage after their derived indexes are
  rebuilt. When vector rows are absent, an authorized client may regenerate and
  resubmit them; the backend never performs that inference.
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
