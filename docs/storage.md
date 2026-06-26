# Witself Storage

Status: draft. Decision: production Witself starts with Postgres (with the
pgvector extension) as the system of record, an external embedding-provider
abstraction (`voyage` default), an object/blob adapter added on demand, and
Goose for database migrations. KMS is optional and demoted; there is no
KMS-as-pillar and no `storage-and-kms.md`.

## Decision

Production storage should start with Postgres as the system of record, with the
`pgvector` extension enabled for memory embeddings.

Postgres should initially store:

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

Unlike Witpass, memory content and fact values are ordinary identity data. They
are protected by data-at-rest encryption, but there is no requirement to keep
them out of queryable columns and no encrypted-blob-only storage rule. Witself
protects the *integrity and authenticity* of identity data, not the
*confidentiality* of secret material; see [Data-At-Rest Note](#data-at-rest-note)
and [threat-model.md](threat-model.md).

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
  mirrors how Witpass abstracted KMS: `voyage` (default), `openai`, and
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

Identity export and import (the inverse of Witpass's encrypted-only stance) use
this adapter for large artifacts; see
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

Decision: Witself does not treat encryption as a product pillar. It is identity
data, protected for integrity and authenticity, not a secret vault.

Posture:

- Use ordinary data-at-rest encryption (managed RDS/disk, or self-host-owned
  disk encryption). There is no KMS/envelope/client-side-decrypt pillar, no
  reveal ceremony, and no end-to-end secret model.
- KMS is **optional and demoted**, not a core dependency. An operator may wire a
  KMS provider for optional field-level encryption, but the product does not
  require one and does not gate identity reads on KMS availability.
- Optional field-level encryption for `sensitive` facts is a capability an
  operator can enable, not the default behavior. When enabled, it wraps only
  those specific values; everything else stays ordinary queryable data.
- `sensitive` is a PII/redaction display flag, not an encryption boundary. There
  is no value-size split between sensitive and non-sensitive values.
- Token values are hashed, not stored raw. Base64 is serialization only, not
  encryption.

The authorization and audit scaffolding that Witpass kept in its encryption
model is reused by [access-policy.md](access-policy.md); Witself has no
`encryption-model.md`.

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
- Server configuration needed to reconnect to storage and the embedding
  provider.
- Optional KMS key identity and rotation metadata only when field-level
  encryption of `sensitive` facts is enabled.

Backups must not include raw tokens, raw database or object-store credentials,
embedding-provider credentials, payment provider secrets, or wallet credentials.

Recovery characteristics specific to Witself:

- When vectors are included in the backup, restoring Postgres restores semantic
  recall directly with no re-embedding pass. When they are not, recall is
  restored by re-embedding from memory content.
- Because vectors are recomputable from content, a deliberate embedding-model
  change can also rebuild them as an explicit, audited operation.
- Losing optional KMS key material (when field-level encryption is enabled)
  affects only the wrapped `sensitive` fact values, not memories, non-sensitive
  facts, policies, groups, or messages. This narrow blast radius is the
  intentional contrast with Witpass, where losing KMS material can make secrets
  unrecoverable. This must be documented for managed and self-hosted
  deployments.

The backup, export, and recovery policy is tracked in
[backup-and-recovery.md](backup-and-recovery.md).

## Related Docs

- [requirements.md](requirements.md)
- [backend-architecture.md](backend-architecture.md)
- [memory-model.md](memory-model.md)
- [facts-model.md](facts-model.md)
- [access-policy.md](access-policy.md)
- [security-groups.md](security-groups.md)
- [inter-agent-messaging.md](inter-agent-messaging.md)
- [json-contracts.md](json-contracts.md)
- [self-hosting.md](self-hosting.md)
- [server-command-surface.md](server-command-surface.md)
- [helm-chart.md](helm-chart.md)
- [terraform-infrastructure.md](terraform-infrastructure.md)
- [cloud-targets.md](cloud-targets.md)
- [audit-retention.md](audit-retention.md)
- [billing-and-limits.md](billing-and-limits.md)
- [backup-and-recovery.md](backup-and-recovery.md)
- [threat-model.md](threat-model.md)
- [implementation-plan.md](implementation-plan.md)
