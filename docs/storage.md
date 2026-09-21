# Witself Storage

> **Sealed-plane custody amendment (accepted 2026-07-18):**
> [ADR 0003](decisions/0003-client-custodied-agent-vault.md) and the
> [client-custodied vault plan](client-custodied-agent-vault.md) define the
> client-held agent vault key as the sealed-plane root. PostgreSQL remains authoritative for ciphertext,
> wrapped DEKs, public metadata, receipts, usage, and audit; it never stores the
> agent vault key or a sensitive plaintext value.

Status: evolving. Production Witself starts with PostgreSQL as the system of
record for both planes, universal full-text retrieval, optional portable JSONB
storage for client-supplied vectors, an object/blob adapter added on demand,
and Goose for database migrations. Storage is two-tier: the OPEN plane
(memories + facts) is ordinary data-at-rest; the SEALED plane (secrets + TOTP)
is envelope-encrypted by the active client under its agent vault key (AVK).
The backend stores ciphertext and redacted inventory without the AVK.

Open-plane decision (accepted 2026-07-14): PostgreSQL remains the sole
authoritative memory store, as specified by
[narrative-memory-and-curation.md](narrative-memory-and-curation.md).
Full-text retrieval is universal; optional memory/query vectors are generated
and supplied by clients. The backend never computes embeddings. Object storage
and local outboxes are archive/delivery mechanisms, not live memory sources.

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

SEALED-plane state:

- Secret metadata (template, label, owner, timestamps) and non-sensitive fields
  such as usernames, URLs, and labels as ordinary queryable values.
- Encrypted sensitive field blobs (envelope-encrypted; never plaintext columns).
- Encrypted TOTP field payloads, including the seed and TOTP parameters; the
  active client decrypts and parses them locally.
- Secret grants and standalone TOTP enrollments remain deferred targets.
- Public agent vault key metadata (`agent_vault_keys`) and AVK-wrapped
  per-field DEKs (`secret_deks`); see [Sealed-Plane Encryption Storage](#sealed-plane-encryption-storage).

The two planes have opposite storage postures and must coexist coherently:

- **OPEN plane.** Memory content and fact values are ordinary identity data.
  They are protected by data-at-rest encryption, but there is no requirement to
  keep them out of queryable columns and no encrypted-blob-only storage rule.
  Witself protects the *integrity and authenticity* of identity data here.
- **SEALED plane.** Secret values and TOTP seeds are confidentiality-critical.
  Clients encrypt them with per-field DEKs wrapped by the AVK. The backend
  stores only ciphertext; values are reveal-gated and **never embedded, never
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
- `agent_activity` — migration-0039 latest-only observation projections, one
  row per agent/runtime/installation. Client event ids and event times provide
  retry and ordering guards; PostgreSQL alone stamps the public
  `last_activity_at`. Same-realm peer reads aggregate the newest row per agent
  and never infer online, offline, available, or accepting-work state. These
  canonical rows are account archive data; the local runtime-hook outbox is not.
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
- `agent_messages` — `msg_…` id, account/realm, token-derived sender,
  migration-0037 audience kind (`agent`, `agents`, or `realm`) and immutable
  audience fingerprint, subject/kind, body, optional object payload, thread id,
  migration-0033 causal parent, migration-0035 backend-derived causal depth,
  optional sender-scoped idempotency key, and `created_at`. Direct responses
  carry the resolved agent; fanout responses carry the audience kind and
  delivery count. Group audiences remain a future addition.
  Current write surfaces normalize omitted kind to actionable `request`;
  explicit `note` is FYI-only to the active recipient.
- `agent_message_deliveries` — one row per message/recipient with
  delivery/read/ack state plus migration-0034 independent
  `available`/`claimed`/`completed` processing state, monotonic generation,
  claim/complete retry hashes, database-time lease, and unique result-message
  link. Migration 0036 adds the separate durable `failure_count` for exact-fence
  releases marked as deterministic message failures; generation remains only
  the stale-writer fence. Import interrupts active claims while preserving
  completed links and failure counts.
- `agent_message_requests` — migration-0038 realm-wide open jobs: token-derived
  coordinator, ordinary realm opening message, closed `client_ranked` policy,
  `max_assignees`, offer/expiry deadlines, lifecycle state, selection fence,
  and timestamps. Open retry identity is the ordinary opening message's
  sender-scoped idempotency key; the request row has no second key.
- `agent_message_request_candidates` — immutable send-time candidate snapshot
  and each candidate's one pending/offered/declined response state plus optional
  ordinary offer message link.
- `agent_message_request_selections` — append-only coordinator-authored ranking
  decisions with monotonic generation and retry/selection hashes. The selection
  hash commits the chosen IDs; linked claim rows materialize that selected-agent
  snapshot. The database validates selections but performs no ranking.
- `agent_message_request_claims` — selected-agent reservations and exact
  `reserved`/`claimed`/`released`/`completed`/`cancelled` processing fences,
  lease, generation, failure count, and ordinary result-message link. Import
  cancels active reservations/claims and advances their fence.
- PostgreSQL is the sole message and handoff store. Foreground clients discover
  unacknowledged deliveries through the bounded message checkpoint and
  metadata-only listen; no host-local notification or provider state is part of
  messaging.
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
  (`active`/`archived`/tombstoned through `deleted_at`), timestamps. The
  implemented agent-owned retained-capacity gate counts active and archived
  top-level rows; a guarded tombstone delete releases capacity, scrubs secret
  metadata, deletes its field and wrapped-DEK rows, and keeps only a minimal
  value-free tombstone plus receipt/audit evidence. Witpass's `shared` secrets
  are now group-owned, so `owner_kind ∈ {agent, group}` matches memories and
  facts.
- `secret_fields` — `fld_…` id, secret, field name, sensitivity. Non-sensitive
  field values are ordinary columns; sensitive field values are stored only as
  envelope ciphertext (DEK id, AEAD algorithm, nonce, ciphertext) — never as a
  plaintext column.
- `secret_grants` — `grt_…` id, secret, grantee (agent or group), granted
  scopes, granting actor, timestamps. The sealed-plane cross-agent/operator
  access path; see [authorization-and-roles.md](authorization-and-roles.md).
- TOTP payloads — sensitive `secret_fields` containing the encrypted seed and
  parameters together. The active client decrypts and parses the payload for
  seed-free metadata or code calculation; there is no implemented standalone
  `totp_enrollments` table. See [totp-2fa.md](totp-2fa.md).
- `agent_vault_keys` — public AVK identifiers, versions, fingerprints, and
  lifecycle state; never the AVK bytes. See
  [Sealed-Plane Encryption Storage](#sealed-plane-encryption-storage) and
  [key-hierarchy.md](key-hierarchy.md).
- `secret_deks` — per-field data-key identifiers, AVK version, wrapping nonce,
  wrapped DEK ciphertext, and wrapping revision. Clients wrap and unwrap the
  data keys; the backend never stores an unwrapped key.

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

Agent-secret custody does not depend on KMS. The active client holds the AVK
and performs encryption, decryption, password generation, and TOTP calculation.
The backend has no agent-vault KMS provider setting, unwrap authority, or
sensitive-value-returning route. The open plane relies on ordinary data-at-rest
encryption (managed RDS/disk); sealed values additionally use client-side
envelope encryption.

Each cell stores its tenants' ciphertext, wrapped DEKs, and public vault
metadata. The thin global control plane holds only routing metadata
(realm/account → home cell + endpoint + signing key), never tenant data or
key material. Moving an account copies encrypted vault state unchanged and
requires the matching AVK in the authorized client. See
[Cross-Cell Vault Portability](#cross-cell-vault-portability) and
[deployment-cells.md](deployment-cells.md).

Provider-specific credentials should come from workload identity, cloud
identity, mounted secret files, or deployment-native secret managers. They must
not be committed to config files, Terraform examples, Helm values, or the public
repository. Managed cloud must not expose raw KMS credentials to the
application; the server uses deployment identity and tightly scoped IAM
permissions.

PostgreSQL and its full-text facilities gate the open plane. Migration-0032
vector-profile operations use ordinary PostgreSQL and require no extension;
lexical recall remains healthy without any vector rows. A future ANN projection
must never become a readiness gate. Agent secrets add no KMS readiness gate:
the server stores encrypted material, while client key availability controls
local reveal. Capabilities describe this ciphertext-only backend contract.

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

The envelope is client-custodied: the AVK wraps each sensitive field's DEK,
and that DEK encrypts the field value. PostgreSQL stores:

- `agent_vault_keys` — public AVK identity, fingerprint, version, and lifecycle
  metadata. The key bytes remain with the client.
- `secret_deks` — AVK-wrapped per-field DEKs and wrapping metadata. Sensitive
  field ciphertext records its DEK identifier, AEAD algorithm, and nonce.

Only the authorized active client unwraps the DEK and decrypts the value.
A token alone cannot reveal a secret; the backend never receives the AVK or
plaintext key material. Password generation and TOTP calculation also happen
in the active client. The exact envelope and rotation contracts are tracked by
[encryption-model.md](encryption-model.md) and
[key-hierarchy.md](key-hierarchy.md); the schema is in
[data-model.md](data-model.md).

Sealed-plane carve-outs hold at the storage layer: secret values and TOTP seeds
are **never embedded** (no `memory_vectors` row), **never returned by semantic
recall**, **never in the self-digest**, **never ingested** from
CLAUDE.md/AGENTS.md, and **never written to a plaintext export**. Secret backup
is encrypted-only; see [Backup And Restore Implications](#backup-and-restore-implications).

<a id="cross-cell-kms-re-wrap"></a>

## Cross-Cell Vault Portability

Witself deploys as a fleet of independent cells under a thin global control
plane. Each cell is one complete, independent stack with its own PostgreSQL
and blob store, holding tenant data and encrypted vault state. The control
plane holds only routing metadata — the realm/account → home cell, endpoint,
and signing key mapping — and **no tenant data and no key material**. See
[deployment-cells.md](deployment-cells.md).

Moving an account from cell A to cell B copies sensitive-field ciphertext,
wrapped DEKs, and public AVK metadata unchanged. Neither source nor destination
unwraps a key, decrypts a value, or re-roots the vault in cloud infrastructure.
The authorized client continues to use the same matching AVK after the move.
Client AVK rotation is a separate lifecycle operation described by
[key-hierarchy.md](key-hierarchy.md).

The migration is audited through `tenant.migration_started`,
`tenant.migration_completed`, or `tenant.migration_failed`, per
[deployment-cells.md](deployment-cells.md) and
[audit-retention.md](audit-retention.md). After the control-plane mapping is
repointed to cell B, clients re-resolve their home cell and route to B directly.

The open plane (memories, facts, messaging) moves through the first-class
export/import path. Full-text indexes are rebuilt in the destination. Optional
vector rows move with their immutable profiles as canonical schema-32 archive
data; any future ANN projection is rebuilt. A profile with no vector rows still
leaves the destination fully functional through lexical recall. See
[backup-and-recovery.md](backup-and-recovery.md) and
[Backup And Restore Implications](#backup-and-restore-implications).

Placement and cutover decisions remain tracked in
[deployment-cells.md](deployment-cells.md). They do not change the client-custody
boundary or require cloud-key re-wrapping.

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
- Migration `0067_add_secret_delete_receipts.sql` widens the sealed mutation
  receipt checks to include `secret_delete` by adding and validating replacement
  constraints before the metadata swap. Its down migration refuses to proceed
  while any delete receipt exists; rollback across that boundary is
  backup-required because durable retry/audit evidence must not be discarded.
- Migration `0095_add_support_ticket_admission_index.sql` adds the covering
  `support_tickets_by_account_opened` index on `(account_id, opened_at DESC)`.
  The admission query reads the newest tickets in its bounded window while
  holding the account lock. This index-only migration preserves account rows;
  its reversible down migration drops only the index.

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
ordinary data-at-rest protection for identity data; the sealed plane uses
client-side envelope encryption for secret material.

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

- The active client encrypts secret values and TOTP seeds with per-field DEKs
  wrapped by its AVK. The backend stores only ciphertext; explicit client
  reveal gates value-returning operations. See [KMS Posture](#kms-posture),
  [Sealed-Plane Encryption Storage](#sealed-plane-encryption-storage), and
  [encryption-model.md](encryption-model.md).
- Losing every matching AVK copy and usable client recovery path makes sealed
  values unrecoverable. This does **not** affect memories, facts, policies,
  groups, or messages.

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
- Server configuration needed to reconnect to storage. There is no backend
  model-provider configuration.
- Public AVK metadata, wrapped DEKs, and vault lifecycle rows needed to preserve
  the encrypted vault. Matching AVK custody and recovery remain client concerns.

Open-plane identity (memories + facts) is plaintext-exportable; that stays the
headline backup/restore feature. The sealed plane is **never in the plaintext
export** — secret backup is encrypted-only (envelope ciphertext, wrapped DEKs,
and public AVK metadata, never plaintext), and `witself export` excludes secret values and TOTP
seeds; see [backup-and-recovery.md](backup-and-recovery.md).

Backups must not include raw tokens, raw database, object-store, or KMS
credentials, client model credentials, payment provider secrets, wallet
credentials, or plaintext secret material or TOTP seeds.

Recovery characteristics specific to Witself:

- Restoring PostgreSQL restores memory content and lexical recall. Retained
  vector rows restore optional vector coverage after their derived indexes are
  rebuilt. When vector rows are absent, an authorized client may regenerate and
  resubmit them; the backend never performs that inference.
- Restoring encrypted vault rows does not restore the client AVK. Without a
  matching key copy or usable client recovery path, sealed values remain
  inaccessible. Memories, facts, policies, groups, and messages are recoverable
  from the Postgres backup independently of client AVK custody. This boundary
  must be documented for managed and self-hosted deployments.

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
