# Witself Backup And Recovery

Status: draft. Decision: v0 backup/export carries a **dual posture**, one per
plane. The OPEN plane (memories + facts) supports first-class
structured/plaintext identity export and round-trippable import. The SEALED
plane (secrets + TOTP) is backed up **encrypted-only** — envelope ciphertext
plus KMS key identity and rotation metadata, never plaintext, never key
material — and is **excluded from the plaintext identity export** entirely.

Visible transcript conversations and entries are account-owned open-plane data.
They travel in the logical account export/import stream in conversation then
entry order. When file artifacts land, an export is complete only when its blob
manifest/content travels with the relational metadata; restoring dangling
object references is not acceptable.

Narrative-memory amendment (accepted 2026-07-14): the implemented account
archive carries memory heads, full versions, evidence, lineage, value-free
permanent-delete tombstones, and complete hashed retry-shield sets. Transcripts
restore before evidence-bearing versions, and the derived full-text index
rebuilds. Superseded versions carry a value-free replacement count/digest
commitment. Migration `0030` curation lanes, cursors, requests, runs, frozen
inputs, actions, mutation receipts, and their memory/relation/fact-candidate
attribution participate in the same implemented account archive. Migration
`0032` vector profiles and exact version/content-hash-bound JSONB vector rows
also participate in export/import. Cross-cell moves require source freeze or
placement-epoch fencing as specified in
[narrative-memory-and-curation.md](narrative-memory-and-curation.md).
Migration `0034` direct-message processing state also participates. Completed
processing and its result-message link are preserved; import interrupts every
active message claim by advancing its generation and clearing its
claim/key/lease fields before the destination account resumes.
Migration `0035` backend-derived message causal depth also participates.
Schema-35 import validates depth against the reply graph; older archives have
depth deterministically derived during upgrade/import.
Migration `0036` adds the durable direct-message `failure_count`. Schema-36
archives preserve and validate it; earlier archives upgrade to zero because
they contain no trustworthy per-message deterministic failure history.
Migration `0037` adds explicit-list and realm audience metadata; the ordinary
delivery rows remain the authoritative immutable send-time snapshot. Migration
`0038` adds the complete open-request graph: requests, candidate snapshots,
selections, and claims, linked to the ordinary opening, offer, and result
messages. Terminal request history survives import. Active source-cell
reservations and claims are imported as interrupted history with stale fences.
Migration `0039` adds the canonical latest-only `agent_activity` projection. Every
agent/runtime/installation row, including its internal retry/order guards and
PostgreSQL-stamped `last_activity_at`, participates in account export/import so
peer activity survives a cross-cell move. The client-local runtime-hook outbox
that delivers those observations is host retry state, not server or account
archive data. Restored observations remain historical metadata only and do not
assert availability or presence.
Messaging has no client-local canonical or handoff state. PostgreSQL messages,
delivery/read/ack state, audience snapshots, and request/claim graphs are all
included in account export/import. Offline and terminal deliveries remain
unacknowledged until an active destination client handles them, so there is no
host-local notification ledger or provider-credential file to move.
Later instructions to reconnect a backend embedding provider or run server-side
re-embedding are superseded.

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
- Optional client-supplied vectors are derived data rather than memory authority,
  but schema-32 archives preserve their immutable profiles and rows. Lexical
  recall remains the correctness baseline. See
  [Client-Supplied Vectors](#client-supplied-vectors).

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
for backing up Postgres, object/blob storage when used, migration version,
server configuration, and — when the sealed plane is enabled — KMS key identity
and rotation metadata. Client-vector profiles/rows are ordinary PostgreSQL and
schema-32 archive data, not backend provider configuration.

The threat framing is **dual**: integrity and authenticity of open-plane
identity data, and confidentiality of sealed-plane secret material.
Backup/restore is correspondingly oriented toward not losing or corrupting
identity, and toward not leaking secrets. See [threat-model.md](threat-model.md).

## Backup Scope

Production backups should include:

- Postgres backup. This is the system of record and carries memories, facts,
  primary flags, memory edit history, policies, group membership, group-owned
  records, curation queue/run/receipt state, messages and per-recipient
  delivery/read/ack plus fenced processing state, audience snapshots, open
  message requests and their candidate/selection/claim graph, tokens (hashes
  and metadata only), audit records, and usage/limit state.
- Migration-0032 client-vector profiles/rows; vector rows are optional derived
  data to use, but their table streams and existing rows are part of a complete
  schema-32 archive (see [Client-Supplied Vectors](#client-supplied-vectors)).
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
- Immutable client-vector profile identity (provider/model/recipe, dimensions,
  distance metric, and normalization) for every retained vector row.
- Server configuration needed to reconnect to storage and, when the sealed
  plane is enabled, KMS. The server has no embedding-provider credentials.
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
- Memory content, fact values, message bodies/payloads, or client-supplied vectors in
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

## Client-Supplied Vectors

Vectors are optional derived data, never a source of truth. Migration `0032`
adds `memory_vector_profiles` and `memory_vectors`; both table streams and all
existing rows are included in every schema-32 account archive. Recall remains
fully functional in lexical mode when profiles or compatible rows are absent.

An authorized client supplies memory/query vectors under an immutable
provider/model/recipe/dimension/distance/normalization profile. The backend only
validates, stores canonical JSONB arrays, compares, exports/imports, and
deterministically blends them; it never contacts an embedding provider. Each row
is bound to the exact owner, profile, memory version, and content hash. Import
validates that binding, dimensions, finite values, vector/contract hashes,
chronology, and table completeness. Restore reports coverage rather than
scheduling server-side re-embedding. Any future pgvector/ANN projection is
rebuildable acceleration, not canonical archive data or a restore prerequisite.

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
- A permanently deleted memory exports only its value-free head tombstone and
  the complete `memory_deleted_references` retry-shield set. The tombstone binds
  receipt/idempotency hashes, prior version, deterministic scrub revision,
  purged row counts, and retry-shield count/digest; no purged version, evidence,
  relation, content-derived hash, locator, or raw retry key may reappear.
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
- Import rejects a live memory carrying deletion metadata, a deleted memory
  with live version/evidence/relation rows, any non-idempotency or non-SHA-256
  deleted-reference row, and any retry-shield count/digest mismatch. This keeps
  a crafted archive from turning tombstones into a payload side channel or
  weakening delayed-retry protection.
- Import verifies that a complete supersession relation set matches its
  committed replacement count/digest and rejects an incomplete live set. A
  smaller historical set is portable only after the set was reverted and cannot
  become the current head; an exact retry then fails closed.
- The logical archive includes seven curation streams in dependency order:
  `memory_curation_lanes`, `memory_curation_cursors`,
  `memory_curation_requests`, `memory_curation_runs`,
  `memory_curation_run_inputs`, `memory_curation_actions`, and
  `memory_curation_mutations`. It also preserves curation attribution on memory
  versions/relations and fact candidates. Import validates owner scope, ids,
  request/run/action graph membership, plan/action hashes, result provenance,
  cursor streams, and receipt references before writing the graph.
- A schema-32 logical archive also includes `memory_vector_profiles` before
  `memory_vectors`. Import rejects missing streams, duplicate contracts/keys,
  cross-owner references, profile/dimension/normalization mismatches,
  content-hash or vector-hash mismatches, invalid components, and a profile or
  vector timestamp that predates its required source row.
- Active curation work never resumes across an archive boundary. Export retains
  enough state for audit and compensation, but import clears the lane's active
  run, interrupts an open/planned run and claimed request, removes the live
  lease, and reserves the next fencing generation. A destination client must
  claim fresh due work; an old source-cell fence cannot apply.
- Active direct-message processing also never resumes across an archive
  boundary. Import validates the delivery's processing shape and result-message
  scope. It preserves `completed` rows and their unique result links, but
  normalizes `claimed` rows to `available`, increments the generation, and
  clears `claim_id`, claim-key hash, and lease expiry. The source client's fence
  is therefore stale on the destination.
- Direct-message `causal_depth` is portable safety state, not a client hint.
  Schema-35 imports must match every reply's depth to parent plus one; legacy
  archive rows receive deterministic depths derived from their validated reply
  graph. Migration-0036 `failure_count` is also portable safety state and is
  preserved for available, claimed-normalized, and completed deliveries.
  Processing generation remains solely a stale-writer fence; interrupting an
  active import claim advances that fence without incrementing `failure_count`.
- Fan-out messages restore one immutable header plus their exact delivery
  snapshot; current realm membership is never re-resolved during import. Open
  request candidates therefore remain the original send-time set. Completed,
  released, and cancelled request claims retain their history and result links.
  A source-cell `reserved` or `claimed` request slot imports as cancelled
  history with its generation advanced, so no old worker can renew or complete
  it in the destination.

The implemented whole-account exporter requires the account to be suspended or
closed. It streams all tables from one PostgreSQL `REPEATABLE READ` transaction
and holds a shared lock on the account row, so a concurrent resume cannot create
a torn archive.

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
- Every imported vector row has its matching immutable profile, exact live
  owner-scoped memory version/content hash, valid dimensions/components/hash,
  and valid chronology. An archive from before schema 32 may upgrade with zero
  vector coverage without blocking lexical recall.
- When the sealed plane is enabled: that the retained KMS key identity
  (`kms_provider`, `kms_key_ref`) resolves to a reachable CMK and that the
  `realm_keys` / `secret_deks` wrapping rows restore consistently, so restored
  envelopes can be unwrapped by an authorized, audited reveal later. Restore
  never decrypts the envelopes itself.

Restore should reconstruct integrity-critical structures in dependency order:

1. Agents and groups, so ownership targets exist.
2. Group membership and admins, so group-scoped ownership and policy subjects
   resolve.
3. Transcripts, memories, evidence, lineage, and facts (with edit history),
   assigning owners and re-resolving `witself://` references.
4. Curation owner lanes and cursors, then requests, normalized runs, frozen
   inputs, actions, mutation receipts, and curation attribution. Active source
   leases are interrupted and fences advanced during import.
5. Fact primary flags, enforcing at-most-one-primary per logical kind per owner.
6. Policies, so the default-deny access surface is restored before cross-agent
   access resumes.
7. Messages and per-recipient delivery/read/ack state.
8. When the sealed plane is enabled: `realm_keys` and `secret_deks` wrapping
   rows, then the secret/field/TOTP envelopes and grants (`grt_…`), re-bound to
   the retained KMS key identity. These restore as ciphertext; no decryption
   occurs during restore.
9. Audit records, when included.

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
- Lexical recall is immediately available after index rebuild. Restored vector
  coverage reflects the imported profile/rows and may be zero for an older
  archive; the backend does not schedule re-embedding.
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
  edit history, curation queue/run/receipt state, policies, group membership,
  group-owned records, messages, message audience snapshots, open request
  coordination state, and audit.
- Migration-0032 client-vector profile/row backup and restore.
- Object/blob backup and restore when used.
- Migration version tracking and upgrade ordering. See [storage.md](storage.md).
- Client-vector profile retention for every vector row; no backend
  embedding-provider configuration exists.
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

## Tenant Migration Between Cells

Witself deploys as a fleet of independent cells under a thin global control
plane; a tenant has exactly one home cell at a time. Moving a realm/account from
cell A to cell B is a deliberate, bounded operation — not continuous
replication — and it **reuses the backup/export posture on this page unchanged**
rather than adding a parallel data-movement path. See
[deployment-cells.md](deployment-cells.md) for the cell topology and the
control-plane repoint that completes a move.

Migration is dual-plane, matching the two postures above:

- **Open plane (memories + facts + curation + messaging)** moves via the first-class
  export/import described in [Identity Export And Import](#identity-export-and-import):
  `witself export` from cell A, `witself import` into cell B. Identity data
  travels in clear by design, so no KMS custody is involved. Migration-0032
  client-supplied vector profiles/rows move in the same archive under the
  [Client-Supplied Vectors](#client-supplied-vectors) validation rules.
- **Sealed plane (secrets + TOTP)** is KMS-rooted per cell/cloud, so it cannot be
  carried as a plaintext export. Migration performs an audited **KMS re-wrap**:
  the data keys are unwrapped under cell A's CMK and re-encrypted under cell B's
  CMK (decrypt-at-source / re-encrypt-at-dest), exactly the
  [Sealed-Plane Secret Backup](#sealed-plane-secret-backup) envelope rebound to a
  new key identity. Secret *values* are never exported in plaintext and the
  reveal ceremony is never exercised; the move re-lands ciphertext under the
  destination CMK. See [key-hierarchy.md](key-hierarchy.md) and
  [storage.md](storage.md).

After both planes land in cell B, the control-plane realm/account -> home-cell
mapping is repointed and clients re-resolve to cell B. Migration emits
`tenant.migration_started`, `tenant.migration_completed`, and
`tenant.migration_failed` (see [audit-retention.md](audit-retention.md)).

**Open decisions.** The migration cutover mechanism is not yet fixed: a brief
read-only freeze on cell A while the final delta moves, versus dual-write +
reconcile across both cells during the transition. The
placement/migration unit (account vs realm) is likewise open. Both are tracked
in [deployment-cells.md](deployment-cells.md) and are not resolved here.

## Restore Checklist

A managed or self-hosted restore should proceed in this order:

1. Confirm the target `witself-server` build and its expected migration version.
2. Restore the Postgres system of record from backup.
3. Apply or verify migrations with `witself-server migrate` so the restored
   database matches the build (advisory lock; Helm migration Job in Kubernetes).
   See [storage.md](storage.md).
4. Restore object/blob storage when used.
5. Rebuild the derived full-text index. Restore schema-32 vector profiles and
   rows through the validated archive streams; rebuild any future ANN
   projection separately. For an older archive, report zero vector coverage.
6. Restore server configuration and reconnect to storage. No backend embedding
   provider is involved.
7. When the sealed plane is enabled: reconnect to KMS, confirm the retained
   `kms_provider` / `kms_key_ref` resolves to a reachable CMK, and verify the
   `realm_keys` / `secret_deks` wrapping rows restored consistently — a probe
   KEK unwrap (without revealing any secret value) confirms the chain is
   reconnectable. If the CMK is unreachable or gone, the restored secret
   ciphertext is crypto-shredded; the open plane is still fully restored. See
   [key-hierarchy.md](key-hierarchy.md).
8. Verify integrity invariants: fact-name uniqueness per owner, at-most-one
   primary per logical kind per owner, group membership and admins, policy
   bindings (default-deny surface), memory edit-history continuity, curation
   request/run/action/receipt ownership and attribution, inactive imported
   leases with reserved fences, message delivery/read/ack state, completed
   processing/result links, preserved deterministic failure counts, and
   interrupted active message claims with advanced generations.
9. Confirm lexical recall works. Confirm restored hybrid recall and reported
   coverage when compatible vector rows exist; zero coverage remains a supported
   lexical fallback, not an unsupported memory service.
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
- [deployment-cells.md](deployment-cells.md)
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
