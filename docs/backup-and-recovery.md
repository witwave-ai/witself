# Witself Backup And Recovery

Status: draft. Decision: v0 supports first-class structured/plaintext identity
export and round-trippable import, alongside operational backups. This is the
deliberate inverse of the Witpass encrypted-only export stance.

## Decision

Witself should treat backup, export, and recovery as ordinary, well-supported
operations on identity data, not as a confidentiality choke point.

V0 posture:

- Backups preserve all identity data and the metadata needed to bring a realm
  fully back, including memories, facts, primary flags, memory edit history,
  policies, group membership, group-owned records, messages, and audit.
- Identity export is a supported plaintext feature, not a forbidden one.
  `witself export` produces structured, human-readable, round-trippable identity
  data; `witself import` restores it. See
  [Identity Export And Import](#identity-export-and-import).
- Recovery does **not** require any KMS or key-material custody. There is no
  encrypted-only export, no no-plaintext rule, and no break-glass decrypt path,
  because there is no secret-confidentiality pillar to protect (contrast:
  Witpass forbade plaintext vault export and could lose secrets if KMS key
  material was lost).
- Embedding vectors are recomputable from memory `content`, so backing them up
  is an optimization, not a correctness requirement. See
  [Embedding Vectors](#embedding-vectors).
- Managed cloud recovery restores customer identity data and service
  availability.
- Self-hosted operators are responsible for backing up Postgres (including
  pgvector data when retained), object/blob storage when used, migration
  version, server configuration, and embedding-provider/model identity.

The threat framing is integrity and authenticity of identity data, not
confidentiality of secret material. Backup/restore is correspondingly oriented
toward losing or corrupting identity, not toward leaking it. See
[threat-model.md](threat-model.md).

## Backup Scope

Production backups should include:

- Postgres backup. This is the system of record and carries memories, facts,
  primary flags, memory edit history, policies, group membership, group-owned
  records, messages and per-recipient delivery/read/ack state, tokens (hashes
  and metadata only), audit records, and usage/limit state.
- pgvector embedding data when the operator chooses to back it up (optional;
  recomputable — see [Embedding Vectors](#embedding-vectors)).
- Object/blob storage backup when object storage is used (large exports,
  diagnostic bundles, support attachments, backup artifacts).
- Migration version, so a restored database is matched to a compatible
  `witself-server` build. See [storage.md](storage.md).
- Embedding-provider and model identity (provider name, model, and vector
  dimensionality), so recall behavior and re-embedding decisions are
  reproducible after restore.
- Server configuration needed to reconnect to storage and the embedding
  provider.
- Helm release values without raw secrets. See [helm-chart.md](helm-chart.md).
- Terraform state stored outside the public repo and protected as sensitive
  infrastructure state. See
  [terraform-infrastructure.md](terraform-infrastructure.md).

Backups should preserve the integrity-critical identity structures explicitly,
not just the raw rows:

- **Policies** — every `pol_…` binding (subject, permission, target, scope,
  filter, effect, metadata) so the default-deny access surface is restored
  exactly. See [access-policy.md](access-policy.md).
- **Group membership** — every `grp_…` group, its `members[]`, `admins[]`/owner,
  and the policies bound to it as subject and target, plus any group-owned
  `mem_…`/`fact_…` records. See [security-groups.md](security-groups.md).
- **Fact primary flags** — the `primary` flag and logical-kind uniqueness on
  every `fact_…`, so identity anchors and `whoami`/profile ordering survive a
  restore. At most one primary per logical kind per owner must hold after
  restore. See [facts-model.md](facts-model.md).
- **Memory edit history** — the full versioned history of every `mem_…`
  (version number, actor, timestamp, changed fields), so safe operator recovery,
  conflict detection, and audit remain possible after restore. See
  [memory-model.md](memory-model.md).

Production backups must not include:

- Raw agent or operator tokens (only token hashes and token metadata).
- Database URLs, embedding-provider credentials, or other provider secrets.
- Payment provider secrets or wallet credentials.
- Memory content, fact values, message bodies/payloads, or embedding vectors in
  logs, metrics, or diagnostic output. Identity content lives in the backup
  payload itself, not in operational telemetry. See
  [observability-and-operations.md](observability-and-operations.md).

Identity content (memory `content`, fact `value`, message `body`/`payload`)
**is** present in the backup payload by design — Witself stores and restores it
in clear. That is the inverse of Witpass, where the equivalent payload was
encrypted and never recoverable in plaintext.

## Embedding Vectors

Embedding vectors are derived data, not source of truth.

- Each memory's vector is computed from its `content` (and optionally tags/kind)
  at write time by the configured embedding provider. See
  [memory-model.md](memory-model.md).
- Backing up pgvector data is **optional**. A restore that omits vectors is
  still complete: every memory, fact, policy, group, message, and edit-history
  entry is intact, and semantic recall is recomputable by re-embedding restored
  memory content.
- Backing up vectors is an optimization: it avoids a re-embedding pass and the
  associated embedding-operation cost and load on restore, and it lets recall
  come back immediately without contacting the embedding provider.
- Re-embedding after restore is an explicit, audited maintenance operation, the
  same operation used when intentionally changing the embedding model. It is not
  an automatic side effect of restore.
- If the embedding provider or model is changed at restore time, vectors should
  be recomputed rather than reused; the backed-up vector dimensionality and the
  new provider's dimensionality must match for reuse to be valid.
- Until re-embedding completes, recall degrades deterministically to
  keyword/tag/kind/time ranking and the capabilities contract reports the
  degraded state. Recall never silently returns unranked or empty results.

## Identity Export And Import

First-class structured/plaintext identity export and round-trippable import is a
headline Witself feature and the inverse of Witpass's encrypted-only export
stance. Export/import is the user-facing, portable counterpart to operational
backups: backups protect a realm operationally; export/import moves identity
between agents, realms, and environments.

Export rules:

- `witself export` emits a structured, human-readable, plaintext export of an
  agent's self: all memories (content, kind, tags, source, salience, links,
  timestamps, and **edit history**), all facts (values, `primary` flags,
  `sensitive` flags, format hints, source, and edit history), and the agent's
  identity anchors.
- For operators, export can include realm-level context: **policies**,
  **security-group membership**, and **group-owned** memories and facts.
- Export defaults to JSON using the `witself.v0` schema version. A
  directory/file layout suitable for diffing and version control is also
  supported.
- Identity references (`witself://…`) are preserved on export and re-resolved on
  import. Dangling references are reported, not silently dropped.
- Export requires an explicit output path selection.
- Export produces an `identity.exported` audit event when audit is available.
- Export must not include raw tokens.

Sensitivity handling (display-level, not encryption):

- `sensitive` facts and memories are exported in clear by default — Witself
  embraces plaintext export. There is no reveal ceremony and no value-size
  split.
- Exporting `sensitive` records requires an audit `--reason` and emits a
  warning.
- Export is least-privilege authorized. Operators may scope exports to
  non-sensitive records (for example for sharing or diffing) where policy
  allows.

Import rules:

- `witself import` restores an exported self into the same or a different
  agent/realm, preserving primary flags, sensitive markers, links, and (where
  chosen) edit history.
- Import is idempotent by stable id where ids are preserved, and supports a
  rename/remap mode when importing into a different agent or realm.
- Import is audited (`identity.imported`) and supports `--dry-run` to preview
  created/updated/conflicting records without persisting.
- Importing facts re-applies primary promotion atomically, demoting any prior
  primary of the same logical kind so the at-most-one-primary invariant holds.
- Imported references are re-resolved and re-checked for authorization; a
  cross-agent or cross-group reference resolves only when policy permits.

Local development exports use the same `witself export`/`witself import` paths
for fixtures, demos, backup, and migration, so the local backend exercises the
real export contract rather than a parallel format. See
[cli-command-surface.md](cli-command-surface.md).

## Restore Scope

Restore should validate:

- Export/backup schema version (`witself.v0`).
- Integrity metadata of the backup or export.
- Migration compatibility between the backed-up database and the target
  `witself-server` build.
- Target realm/agent/account conflicts, including fact-name uniqueness and
  primary logical-kind uniqueness per owner.
- Whether the restore is a merge, a replace, or a new-realm/new-agent import.
- Whether embedding vectors are present and whether their dimensionality matches
  the active embedding provider/model; if not, schedule re-embedding.

Restore should reconstruct integrity-critical structures in dependency order:

1. Agents and groups, so ownership targets exist.
2. Group membership and admins, so group-scoped ownership and policy subjects
   resolve.
3. Memories and facts (with edit history), assigning owners and re-resolving
   `witself://` references.
4. Fact primary flags, enforcing at-most-one-primary per logical kind per owner.
5. Policies, so the default-deny access surface is restored before cross-agent
   access resumes.
6. Messages and per-recipient delivery/read/ack state.
7. Audit records, when included.

Restore does **not** require KMS or any key-material custody. There is no
plaintext-exposure exception to manage, because identity data is plaintext by
design. Restore and recovery actions are audited.

## Managed Cloud Recovery

Managed Witself Cloud recovery focuses on restoring customer identity data and
service availability.

- Recovery restores memories, facts (with primary flags), memory edit history,
  policies, group membership, group-owned records, messages, and audit for the
  affected realms.
- Recovery does not depend on KMS key material; there is no scenario where
  identity becomes unrecoverable because a key was lost. Loss of the Postgres
  system of record and all its backups is the limiting risk, which is why
  backups are the operational safeguard.
- Embedding vectors are recomputed when not restored from backup; recall returns
  in degraded mode and is fully restored after re-embedding completes.
- Operator account recovery may restore access to the account or realm
  administration surface, but it must not bypass authorization. Operator access
  to identity data after recovery is audited exactly like any operator override.
  See [operator-auth.md](operator-auth.md).
- Because identity data is plaintext, recovery and any support access to
  identity content follow the standard authorization, redaction, and audit
  rules; support
  diagnostic bundles redact identity content by default. See
  [audit-retention.md](audit-retention.md).

## Self-Hosted Recovery

Self-hosted operators own:

- Database backup and restore, including memories, facts, primary flags, memory
  edit history, policies, group membership, group-owned records, messages, and
  audit.
- Optional pgvector backup and restore, or re-embedding on restore.
- Object/blob backup and restore when used.
- Migration version tracking and upgrade ordering. See [storage.md](storage.md).
- Embedding-provider and model configuration retention, so recall is
  reproducible after restore.
- Terraform state protection. See
  [terraform-infrastructure.md](terraform-infrastructure.md).
- Helm values and Kubernetes Secret protection. See
  [helm-chart.md](helm-chart.md).
- Disaster recovery execution and rehearsal.

There is no KMS key-retention obligation and no scenario where lost key material
makes identity unrecoverable. If a self-hosted operator loses the Postgres
system of record and all backups, identity data is lost; this is the single
operational failure mode self-hosters must guard against. Production self-host
support is paid or contracted once backup/restore, migrations, upgrade,
observability, and disaster recovery are real. See
[self-host-support.md](self-host-support.md).

## Restore Checklist

A managed or self-hosted restore should proceed in this order:

1. Confirm the target `witself-server` build and its expected migration version.
2. Restore the Postgres system of record from backup.
3. Apply or verify migrations with `witself-server migrate` so the restored
   database matches the build (advisory lock; Helm migration Job in Kubernetes).
   See [storage.md](storage.md).
4. Restore object/blob storage when used.
5. Restore or omit pgvector data. If omitted or dimensionality-mismatched,
   schedule re-embedding.
6. Restore server configuration and reconnect to storage and the embedding
   provider; verify the embedding provider/model identity matches the backup or
   record an intentional change.
7. Verify integrity invariants: fact-name uniqueness per owner, at-most-one
   primary per logical kind per owner, group membership and admins, policy
   bindings (default-deny surface), memory edit-history continuity, and message
   delivery/read/ack state.
8. Run re-embedding if needed; confirm recall leaves degraded mode and the
   capabilities contract reports semantic recall as healthy.
9. Verify health and readiness probes and metrics. See
   [observability-and-operations.md](observability-and-operations.md).
10. Confirm a sample of cross-agent access decisions with `policy test` so the
    restored default-deny surface behaves as expected. See
    [access-policy.md](access-policy.md).
11. Confirm restore/recovery audit events are present. See
    [audit-retention.md](audit-retention.md).

## Related Docs

- [requirements.md](requirements.md)
- [storage.md](storage.md)
- [memory-model.md](memory-model.md)
- [facts-model.md](facts-model.md)
- [access-policy.md](access-policy.md)
- [security-groups.md](security-groups.md)
- [inter-agent-messaging.md](inter-agent-messaging.md)
- [self-hosting.md](self-hosting.md)
- [self-host-support.md](self-host-support.md)
- [helm-chart.md](helm-chart.md)
- [terraform-infrastructure.md](terraform-infrastructure.md)
- [observability-and-operations.md](observability-and-operations.md)
- [audit-retention.md](audit-retention.md)
- [operator-auth.md](operator-auth.md)
- [cli-command-surface.md](cli-command-surface.md)
- [threat-model.md](threat-model.md)
- [implementation-plan.md](implementation-plan.md)
- [post-v0-roadmap.md](post-v0-roadmap.md)
