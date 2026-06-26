# Witself Backup And Recovery

Status: draft. Decision: v0 backup/export carries a **dual posture**, one per
plane. The OPEN plane (memories + facts) supports first-class
structured/plaintext identity export and round-trippable import. The SEALED
plane (secrets + TOTP) is backed up **encrypted-only** — envelope ciphertext
plus KMS key identity and rotation metadata, never plaintext, never key
material — and is **excluded from the plaintext identity export** entirely.

## Decision

Witself treats backup, export, and recovery as two distinct postures matched to
the two planes: ordinary, well-supported plaintext operations on identity data
(the open plane), and security-sensitive encrypted-only operations on secret
material (the sealed plane). See [data-model.md](data-model.md) for the plane
split and [encryption-model.md](encryption-model.md) for the sealed-plane
confidentiality model.

V0 posture (open plane — memories + facts):

- Backups preserve all identity data and the metadata needed to bring a realm
  fully back, including memories, facts, primary flags, memory edit history,
  policies, group membership, group-owned records, messages, and audit.
- Identity export is a supported plaintext feature, not a forbidden one.
  `witself export` produces structured, human-readable, round-trippable identity
  data; `witself import` restores it. See
  [Identity Export And Import](#identity-export-and-import).
- Open-plane recovery does **not** require any KMS or key-material custody.
  There is no encrypted-only export, no no-plaintext rule, and no break-glass
  decrypt path for identity data, because identity has no secret-confidentiality
  pillar to protect.
- Embedding vectors are recomputable from memory `content`, so backing them up
  is an optimization, not a correctness requirement. See
  [Embedding Vectors](#embedding-vectors).

V0 posture (sealed plane — secrets + TOTP):

- Secret backup is **encrypted-only**: the at-rest AEAD envelope (`ciphertext`,
  `nonce`, `aead_algorithm`, `dek_id`, `dek_version`, `kms_provider`,
  `aad_context`) plus the `realm_keys`/`secret_deks` wrapping rows and KMS key
  identity + rotation metadata. Never plaintext secret values, never TOTP seeds
  or codes, never key material. See [Sealed-Plane Secret Backup](#sealed-plane-secret-backup).
- Secrets and TOTP seeds are **excluded from the plaintext identity export**.
  `witself export` covers the open plane only and never emits a plaintext secret
  value or seed. The only secret backup is the encrypted envelope plus KMS key
  identity. See [Identity Export And Import](#identity-export-and-import) and
  [key-hierarchy.md](key-hierarchy.md).
- Sealed-plane recovery depends on KMS key material being retained.
  **CMK loss = the sealed plane is crypto-shredded** (every per-realm KEK, hence
  every DEK, hence all secret values and TOTP seeds become permanently
  unrecoverable); the open plane is unaffected. There is no managed break-glass
  decrypt. See [KMS-Loss And Crypto-Shred Posture](#kms-loss-and-crypto-shred-posture).
- Secrets/TOTP seeds are sealed material: never embedded, never returned by
  semantic recall, never in the self-digest, never plaintext-exported, never
  ingested from CLAUDE.md/AGENTS.md. The only value-returning paths are the
  audited `witself secret reveal` / `witself totp code` reveal ceremonies, which
  backup/restore never exercises.

Managed cloud recovery restores both planes: customer identity data, encrypted
secret material, and service availability. Self-hosted operators are responsible
for backing up Postgres (including pgvector data when retained), object/blob
storage when used, migration version, server configuration,
embedding-provider/model identity, and — when the sealed plane is enabled — KMS
key identity and rotation metadata.

The threat framing is **dual**: integrity and authenticity of open-plane
identity data, and confidentiality of sealed-plane secret material.
Backup/restore is correspondingly oriented toward not losing or corrupting
identity, and toward not leaking secrets. See [threat-model.md](threat-model.md).

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
- Encrypted sealed-plane material when the sealed plane is enabled: the at-rest
  secret/TOTP envelopes (`ciphertext` and envelope columns) and the
  `realm_keys` / `secret_deks` wrapping rows. These ride in the Postgres backup
  as ciphertext; they are useless without KMS unwrap authority. See
  [Sealed-Plane Secret Backup](#sealed-plane-secret-backup) and
  [storage.md](storage.md).
- KMS key identity and rotation metadata when the sealed plane is enabled:
  `kms_provider`, the CMK reference (`kms_key_ref`, e.g. AWS KMS key ARN), the
  per-realm `key_version` and KEK rotation generation — identity and metadata
  only, **never key material or raw KMS credentials**. See
  [key-hierarchy.md](key-hierarchy.md).
- Migration version, so a restored database is matched to a compatible
  `witself-server` build. See [storage.md](storage.md).
- Embedding-provider and model identity (provider name, model, and vector
  dimensionality), so recall behavior and re-embedding decisions are
  reproducible after restore.
- Server configuration needed to reconnect to storage, the embedding provider,
  and (when the sealed plane is enabled) KMS.
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
- Raw KMS credentials, unwrapped KEKs/DEKs, or any sealed-plane key material —
  the backup carries KMS key *identity* and rotation metadata only, never key
  material. See [key-hierarchy.md](key-hierarchy.md).
- Plaintext secret values, TOTP seeds, or generated TOTP codes. Sealed-plane
  material is present in the backup only as the at-rest AEAD envelope
  (ciphertext), never decrypted. See [Sealed-Plane Secret Backup](#sealed-plane-secret-backup).
- Payment provider secrets or wallet credentials.
- Memory content, fact values, message bodies/payloads, or embedding vectors in
  logs, metrics, or diagnostic output. Identity content lives in the backup
  payload itself, not in operational telemetry. See
  [observability-and-operations.md](observability-and-operations.md).

Open-plane identity content (memory `content`, fact `value`,
message `body`/`payload`) **is** present in the backup payload by design —
Witself stores and restores it in clear. Sealed-plane secret values and TOTP
seeds are the deliberate inverse: present only as encrypted envelope ciphertext
and never recoverable from the backup without KMS unwrap authority.

## Sealed-Plane Secret Backup

Secret and TOTP backup is encrypted-only, by construction. There is no plaintext
secret backup path and no plaintext secret export path; the reveal ceremony
(`witself secret reveal` / `witself totp code`) is the only value-returning
surface and backup/restore never touches it.

What a secret backup contains:

- The at-rest AEAD envelope per sensitive field and per TOTP seed —
  `ciphertext`, `nonce`, `aead_algorithm`, `dek_id`, `dek_version`,
  `kms_provider`, `aad_context` — exactly as stored. The envelope is identical
  whatever the reveal mode (`client_side_decrypt` / `server_side_decrypt`), so
  the backup shape does not vary by custody mode. See
  [encryption-model.md](encryption-model.md).
- The wrapping rows: `realm_keys` (per-realm `wrapped_kek`, `key_version`,
  `kms_provider`, `kms_key_ref`, rotation metadata) and `secret_deks` (canonical
  `wrapped_dek`, current `kek_id`, DEK generation). These are stored only
  KMS-wrapped / KEK-wrapped; the backup never holds an unwrapped KEK or DEK. See
  [key-hierarchy.md](key-hierarchy.md) and [storage.md](storage.md).
- KMS key identity and rotation metadata: `kms_provider`, `kms_key_ref` (the CMK
  reference, e.g. AWS KMS key ARN), the per-realm `key_version`, and the KEK/CMK
  rotation generation — so a restore reconnects to the correct CMK and resolves
  the current wrapping KEK through `secret_deks.kek_id`. Identity and metadata
  only; **never** the CMK, KEK, DEK, or raw KMS credentials.
- Sealed-plane non-sensitive metadata that lives outside the envelope (secret
  names, templates, usernames/URLs/issuers, grants `grt_…`, `owner ∈ {agent,
  group}`), so secrets restore with their structure intact. This metadata is not
  crypto-shredded by KMS loss (see below).

What it must never contain: plaintext secret values, TOTP seeds, generated TOTP
codes, unwrapped KEKs or DEKs, the CMK, or raw KMS credentials. The sealed plane
is **never embedded, never recalled, never in the self-digest, never
plaintext-exported, never ingested**; the encrypted backup is the one and only
way secret material leaves the live store, and it leaves as ciphertext.

Restoring a secret backup re-lands the envelopes and wrapping rows as ciphertext
and re-binds them to the retained KMS key identity. The values themselves stay
sealed until an authorized, audited reveal — restore never decrypts. A restore
into a realm whose CMK is gone re-lands ciphertext that can never be unwrapped
(see [KMS-Loss And Crypto-Shred Posture](#kms-loss-and-crypto-shred-posture)).

## KMS-Loss And Crypto-Shred Posture

The sealed plane roots every value in `CMK → per-realm KEK → per-secret/field
DEK`. Loss of the CMK (deletion, scheduled-deletion, a disabled key, or a
withdrawn key policy/IAM) makes every per-realm KEK unwrappable, hence every DEK
unwrappable, hence **all secret values and TOTP seeds permanently
unrecoverable** — in both custody modes, since both depend on unwrapping the
same envelope. This is intentional crypto-shredding and is the same posture
[key-hierarchy.md](key-hierarchy.md), [encryption-model.md](encryption-model.md),
and [storage.md](storage.md) mandate.

- **The open plane is unaffected.** Memories, facts, primary flags, edit
  history, policies, group membership, messages, and audit are ordinary
  data-at-rest with no envelope; they survive CMK loss untouched and restore in
  clear. CMK loss crypto-shreds the sealed plane only.
- **Crypto-shred covers secret values only, not metadata/PII.** CMK loss renders
  secret values and TOTP seeds unrecoverable but does not erase sealed-plane
  metadata (secret names, usernames/URLs/issuers) or the audit/operator PII held
  under retention. Erasing that metadata is a separate, metadata-level action.
  See [data-model.md](data-model.md).
- **No managed break-glass decrypt.** Managed recovery restores service
  availability, operator account access, encrypted secret material, and the open
  plane — never plaintext secrets. There is no support path that decrypts secret
  values.
- **Loss surface by custody mode.** Server-mediated managed mode concentrates
  the loss point in one CMK plus the deployment IAM identity. Client-held / BYOK
  mode adds an independent loss point — the operator-held key or passphrase; if
  the operator loses it, those realms are unrecoverable by construction.

Required mitigations to keep crypto-shred a *deliberate* outcome rather than an
accident: enable KMS key rotation with retained prior versions; use deletion
windows with pending-deletion alarms; consider multi-region CMK replication for
managed; and for BYOK require explicit operator consent at realm creation with
prominent warnings and push toward multi-region CMK / key-policy backups /
operator-controlled passphrase escrow. Backups carry KMS key identity + rotation
metadata so a restore reconnects to a *retained* CMK — they cannot resurrect a
destroyed one.

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
headline Witself feature for the **open plane**. Export/import is the
user-facing, portable counterpart to operational backups: backups protect a
realm operationally; export/import moves identity between agents, realms, and
environments.

**Sealed-plane carve-out.** `witself export` covers memories and facts only.
Secrets and TOTP seeds are **never** in the plaintext identity export — there is
no plaintext secret value or seed in any export artifact. The only secret backup
is the encrypted envelope plus KMS key identity described in
[Sealed-Plane Secret Backup](#sealed-plane-secret-backup); an operator who wants
encrypted secret blobs in a portable artifact uses that explicit, audited,
separate path, not the identity export. Sealed material is never embedded, never
recalled, never in the self-digest, and never ingested either. See
[key-hierarchy.md](key-hierarchy.md).

**Future sealed plaintext export (post-v0).** A plaintext secret/TOTP export is
not part of v0 and is tracked as a post-v0 candidate in
[post-v0-roadmap.md](post-v0-roadmap.md). This note records the constraints so
they are not lost if the feature is later built: any future plaintext sealed
export must be designed as a separate high-risk feature, distinct from the open
plane's first-class identity export, with explicit operator action and strong
confirmation, an audit `--reason`, least-privilege authorization, an optional
break-glass framing, redaction controls for support and diagnostics, and its own
design doc separate from this normal backup/export guidance. It would route
through the audited reveal ceremony, not `witself export`. See
[secret-model.md](secret-model.md) and [threat-model.md](threat-model.md).

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

- `sensitive` facts and memories are exported in clear by default — the open
  plane embraces plaintext export. There is no reveal ceremony and no value-size
  split for identity data. `sensitive` is the open plane's lightweight redaction
  marker, distinct from the sealed plane: an actual credential belongs in a
  secret (sealed, never exported), not a sensitive fact. See
  [facts-model.md](facts-model.md) and [secret-model.md](secret-model.md).
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
- When the sealed plane is enabled: that the retained KMS key identity
  (`kms_provider`, `kms_key_ref`) resolves to a reachable CMK and that the
  `realm_keys` / `secret_deks` wrapping rows restore consistently, so restored
  envelopes can be unwrapped by an authorized, audited reveal later. Restore
  never decrypts the envelopes itself.

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
7. When the sealed plane is enabled: `realm_keys` and `secret_deks` wrapping
   rows, then the secret/field/TOTP envelopes and grants (`grt_…`), re-bound to
   the retained KMS key identity. These restore as ciphertext; no decryption
   occurs during restore.
8. Audit records, when included.

Open-plane restore does **not** require KMS or any key-material custody — there
is no plaintext-exposure exception to manage, because identity data is plaintext
by design. Sealed-plane restore re-lands ciphertext and wrapping rows and
requires the retained KMS key identity to be reconnectable for later reveal; if
the CMK is gone, the restored secret ciphertext is crypto-shredded and cannot be
unwrapped (see [KMS-Loss And Crypto-Shred Posture](#kms-loss-and-crypto-shred-posture)).
Restore never exposes plaintext secret values. Restore and recovery actions are
audited.

## Managed Cloud Recovery

Managed Witself Cloud recovery focuses on restoring customer identity data,
encrypted secret material, and service availability.

- Recovery restores memories, facts (with primary flags), memory edit history,
  policies, group membership, group-owned records, messages, and audit for the
  affected realms.
- When the sealed plane is enabled, recovery also re-lands the encrypted secret
  envelopes and the `realm_keys` / `secret_deks` wrapping rows, re-bound to the
  retained CMK. It never restores plaintext secrets and provides no break-glass
  decrypt; secret values stay sealed until an authorized, audited reveal. See
  [KMS-Loss And Crypto-Shred Posture](#kms-loss-and-crypto-shred-posture).
- **Open-plane** recovery does not depend on KMS key material; there is no
  scenario where identity becomes unrecoverable because a key was lost. Loss of
  the Postgres system of record and all its backups is the limiting risk for
  identity, which is why backups are the operational safeguard.
- **Sealed-plane** recovery does depend on the CMK being retained: CMK loss
  crypto-shreds secret values and TOTP seeds for the affected realms while
  leaving the open plane intact.
- Embedding vectors are recomputed when not restored from backup; recall returns
  in degraded mode and is fully restored after re-embedding completes.
- Operator account recovery may restore access to the account or realm
  administration surface, but it must not bypass authorization. Operator access
  to identity data after recovery is audited exactly like any operator override.
  See [operator-auth.md](operator-auth.md).
- Because identity data is plaintext, recovery and any support access to
  identity content follow the standard authorization, redaction, and audit
  rules; support diagnostic bundles redact identity content by default and never
  contain plaintext secret values, TOTP seeds/codes, or key material. See
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
- When the sealed plane is enabled: KMS key retention, rotation, and access
  policy — the CMK under the configured `WITSELF_KMS_PROVIDER`
  (`aws-kms` / `gcp-kms` / `azure-key-vault`), with rotation enabled and prior
  versions retained — plus backing up KMS key identity and rotation metadata
  (never key material). See [key-hierarchy.md](key-hierarchy.md) and
  [self-host-support.md](self-host-support.md).
- Terraform state protection. See
  [terraform-infrastructure.md](terraform-infrastructure.md).
- Helm values and Kubernetes Secret protection. See
  [helm-chart.md](helm-chart.md).
- Disaster recovery execution and rehearsal.

For the **open plane** there is no KMS key-retention obligation and no scenario
where lost key material makes identity unrecoverable. If a self-hosted operator
loses the Postgres system of record and all backups, identity data is lost; this
is the single operational failure mode for identity that self-hosters must guard
against. For the **sealed plane**, there is an additional, deliberate failure
mode: if the operator loses required CMK key material, Witself cannot recover the
affected secret values or TOTP seeds — crypto-shred by construction, while the
open plane remains recoverable from Postgres. Production self-host support is
paid or contracted once backup/restore, migrations, upgrade, observability, and
disaster recovery are real. See [self-host-support.md](self-host-support.md).

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
7. When the sealed plane is enabled: reconnect to KMS, confirm the retained
   `kms_provider` / `kms_key_ref` resolves to a reachable CMK, and verify the
   `realm_keys` / `secret_deks` wrapping rows restored consistently — a probe
   KEK unwrap (without revealing any secret value) confirms the chain is
   reconnectable. If the CMK is unreachable or gone, the restored secret
   ciphertext is crypto-shredded; the open plane is still fully restored. See
   [key-hierarchy.md](key-hierarchy.md).
8. Verify integrity invariants: fact-name uniqueness per owner, at-most-one
   primary per logical kind per owner, group membership and admins, policy
   bindings (default-deny surface), memory edit-history continuity, and message
   delivery/read/ack state.
9. Run re-embedding if needed; confirm recall leaves degraded mode and the
   capabilities contract reports semantic recall as healthy.
10. Verify health and readiness probes and metrics. See
    [observability-and-operations.md](observability-and-operations.md).
11. Confirm a sample of cross-agent access decisions with `policy test` so the
    restored default-deny surface behaves as expected. See
    [access-policy.md](access-policy.md).
12. Confirm restore/recovery audit events are present, including any
    `key.rotated` and sealed-plane events when the sealed plane is enabled. See
    [audit-retention.md](audit-retention.md).

## Related Docs

- [requirements.md](requirements.md)
- [data-model.md](data-model.md)
- [storage.md](storage.md)
- [memory-model.md](memory-model.md)
- [facts-model.md](facts-model.md)
- [secret-model.md](secret-model.md)
- [totp-2fa.md](totp-2fa.md)
- [encryption-model.md](encryption-model.md)
- [key-hierarchy.md](key-hierarchy.md)
- [secret-size-and-attachments.md](secret-size-and-attachments.md)
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
