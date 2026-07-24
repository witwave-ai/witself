# Witself Secret Model

> **Custody amendment (accepted 2026-07-18):**
> [ADR 0003](decisions/0003-client-custodied-agent-vault.md) and the
> [client-custodied vault plan](client-custodied-agent-vault.md) supersede this
> document wherever it specifies KMS-rooted encryption, backend decryption,
> token-only reveal, group ownership, or plaintext-capable operator access.
> The structured secret, per-field sensitivity, redaction, and lifecycle ideas
> remain applicable unless the new plan narrows them for v1.

Status: draft. Decision: a secret is a first-class, agent- or group-owned
**sealed-plane** payload — a named bundle of typed, per-field-sensitivity values,
stored only as KMS-backed envelopes (never plaintext columns), revealed one
field at a time through an explicit, audited reveal ceremony, and used by
reference without ever printing plaintext. Last reviewed 2026-06-26.

This doc pins the secret data model and lifecycle. It is the sealed-plane sibling
of [memory-model.md](memory-model.md) and [facts-model.md](facts-model.md) (the
open-plane identity payloads). Where a memory is free-form self-content recalled
semantically and a fact is a plainly readable name→value attribute, **a secret is
sealed credential material that is never embedded, never recalled, never in the
self-digest, never ingested, and never plaintext-exported.** Encryption is pinned
in [encryption-model.md](encryption-model.md) and
[key-hierarchy.md](key-hierarchy.md); authorization, grants, and realm roles in
[authorization-and-roles.md](authorization-and-roles.md); TOTP in
[totp-2fa.md](totp-2fa.md); storage tables in [data-model.md](data-model.md); wire
shapes in [json-contracts.md](json-contracts.md).

## The Two Planes

Witself is one product with two data planes that share one realm/agent/token
spine, one authorization layer, and one audit trail:

- **Open plane** — memories and facts. Plaintext at rest, semantically indexed,
  recallable, plainly readable, in the self-digest, plaintext-exportable,
  ingestible from `CLAUDE.md`/`AGENTS.md`.
- **Sealed plane** — secrets and TOTP enrollments. Envelope-encrypted at rest
  (CMK → per-realm KEK → per-secret/field DEK), reveal-gated, and subject to the
  carve-outs below.

A credential belongs in the sealed plane, as a secret — **not** as a sensitive
fact. Sensitive facts use lightweight display redaction (a PII/display flag, not
an encryption boundary; see [facts-model.md](facts-model.md)). Secrets use
envelope encryption plus the reveal ceremony. The two postures are distinct and
must not be conflated.

## Sealed-Plane Carve-Outs (invariant)

These hold for every secret and every TOTP seed, with no per-deployment toggle:

- **Never embedded.** Secret values and TOTP seeds are never sent to an embedding
  provider and carry no vector. Semantic recall operates on the open plane only.
- **Never recalled.** `recall` and `scan` never return secret values; sealed-plane
  material cannot surface through open-plane retrieval.
- **Never in the self-digest.** Secrets and TOTP seeds are excluded from digest
  emit and the self-digest. See [context-hydration.md](context-hydration.md).
- **Never ingested.** Secrets are not created by `CLAUDE.md`/`AGENTS.md` ingest.
  Importing identity context can never produce a secret.
- **Never plaintext-exported.** `witself export` excludes the sealed plane.
  Secret backup is encrypted-only (envelope + KMS key identity, never plaintext);
  see [backup-and-recovery.md](backup-and-recovery.md).
- **No plaintext at rest.** Sensitive values live only in envelope columns. Base64
  is serialization, never a security boundary. See
  [encryption-model.md](encryption-model.md).

Plaintext leaves the sealed plane only through the explicit, audited
value-returning operations: `secret reveal`, `totp code`, value-returning
reference resolution, and `witself run` injection.

## Secret Shape

A secret record has the following fields. The canonical JSON encoding (field
names, types, `witself.v0` schema version) lives in
[json-contracts.md](json-contracts.md); this section is the semantic source of
truth.

- `id`: stable identifier with the `sec_` prefix. Server-assigned. Immutable.
  Callers must not parse id internals.
- `realm`: the enclosing realm id (`realm_…`). Immutable. The realm is the
  isolation, billing, and key-separation scope (one per-realm KEK per realm).
- `owner`: the owning principal. An agent (`agent_…`) by default, or a security
  group (`grp_…`) for group-owned secrets. There is **no separate "shared"
  scope**: a credential meant for a team is owned by a group. Ownership is set at
  create time and changes only through an explicit, audited operation.
- `name`: the secret name or path. **Unique per owner** within the realm.
  Different owners may reuse the same name because ownership disambiguates them.
  Path-style names are allowed (for example `github/builder`); the name is one
  logical key, not a directory tree. Max 255 characters.
- `description`: a required human-readable description. Max 4 KiB.
- `template`: an optional convention label, one of `login`, `api-key`, `ssh-key`,
  `certificate`, `env`, `generic`. Default `generic`. Templates suggest
  conventional field names and validation but **do not** restrict which fields a
  secret may carry. Every template still accepts arbitrary named fields.
- `fields`: a **flat** set of named fields (not nested). Each field carries a
  `sensitive` marker and its value. Only `name` and `description` are required for
  a secret; all credential fields are optional and context-dependent. See
  [Fields and Sensitivity](#fields-and-sensitivity).
- `tags[]`: short non-sensitive string tags for filtering and inventory.
- `archived_at`: soft-archive state (distinct from delete). Null when active.
- `created_at`, `updated_at`: timestamps.
- `row_version`: optimistic-lock counter for `If-Match` / `conflict` detection.
  Distinct from the envelope `dek_version` and the per-realm `key_version`.

### Templates

| Template | Suggested fields (conventions only) |
| --- | --- |
| `login` | `username`, `password`, `url`, `totp-seed` |
| `api-key` | `api_key`, `url` |
| `ssh-key` | `private-key`, `public-key`, `passphrase` |
| `certificate` | `cert`, `private-key`, `chain` |
| `env` | one field per environment variable |
| `generic` | none; arbitrary named fields |

Templates are usability conventions, not storage columns. One secret may have
`username`/`password`/`url`; another only a sensitive `api_key`; another `cert`,
`private-key`, and `chain`.

### Example secret kinds (non-normative)

Witself is a general-purpose sealed store: the `generic` template plus arbitrary
named fields hold **any** credential kind, so this catalog is illustrative for
discoverability, **not** a closed list. The data model hard-codes no narrow set
of secret shapes; the kinds below are simply the ones agents and operators reach
for most:

- Passwords and logins.
- API keys.
- Access tokens and refresh tokens.
- Recovery codes.
- OAuth client ids and client secrets.
- SSH private keys.
- TLS private keys and certificates.
- Signing keys.
- Environment variables.
- Service-account credentials.
- Database URLs and connection strings.
- TOTP seeds (sealed; see [Relationship to TOTP](#relationship-to-totp) and
  [totp-2fa.md](totp-2fa.md)).

Any kind not listed here is stored the same way: a `generic` secret with the
arbitrary named, per-field-sensitivity fields described above.

## Fields and Sensitivity

A field is a named value with a per-field `sensitive` marker. Sensitivity is set
per field, not per secret, so one secret freely mixes both:

- **Non-sensitive fields** are ordinary name→value pairs — `username`, `url`,
  `issuer`, `account-label`. They are stored as plain queryable values and are
  displayed by `secret show`. They are not encrypted material.
- **Sensitive fields** are name→value pairs marked `sensitive` — `password`,
  `api_key`, `private-key`, `totp-seed`, `recovery-code`. They are **redacted by
  default** everywhere and stored only as per-field envelopes. They are returned
  in plaintext only through `secret reveal` (or, for TOTP seeds, never directly —
  only generated codes via `totp code`).

V0 size and count limits (full limits and attachments in
[secret-size-and-attachments.md](secret-size-and-attachments.md)):

| Limit | Value |
| --- | --- |
| Max sensitive field value | 64 KiB before encoding |
| Max non-sensitive field value | 16 KiB |
| Max total inline secret payload | 256 KiB before storage overhead |
| Max field count per secret | 100 |
| Max name length | 255 characters |
| Max description length | 4 KiB |

Oversized blobs move to encrypted attachments / object storage; small secrets do
not depend on the object-store path.

Illustrative field shape (normative shape in
[json-contracts.md](json-contracts.md)):

```json
{
  "name": "github/builder",
  "description": "GitHub login for agent-amy",
  "owner": { "kind": "agent", "id": "agent_…" },
  "realm": "realm_…",
  "template": "login",
  "tags": ["ci", "github"],
  "fields": {
    "username": { "sensitive": false, "value": "agent-amy" },
    "url": { "sensitive": false, "value": "https://github.com/login" },
    "password": { "sensitive": true, "redacted": true, "value_ref": "witself://secret/github/builder/password" }
  }
}
```

`secret show` redacts every sensitive field as above: `value` is `null`,
`redacted: true`, and a resolvable `value_ref` is returned instead of plaintext.

## Ownership: Agent or Group

Ownership is unified across all three data types — memory, fact, and secret — as
`owner ∈ {agent, group}`. There is no Witpass-style separate "shared" scope.

- **Agent-owned** (default). An agent-created secret defaults to the creating
  agent. By default a named agent can list, show, reveal, update, and delete only
  the secrets it owns plus those explicitly granted to it. One agent cannot touch
  another agent's secrets without an explicit grant or a realm role that permits
  it.
- **Group-owned.** A secret meant for a team is owned by a security group
  (`grp_…`); members resolve it under group authorization. This replaces the old
  "vault-shared" concept. See [security-groups.md](security-groups.md).

Name uniqueness is scoped to the owner: unique per owning agent for agent-owned
secrets, unique per owning group for group-owned secrets. Operators and elevated
realm roles may inspect across the realm but must target a specific owner for
mutations (see [Cross-Agent and Operator Access](#cross-agent-and-operator-access)).

Cross-agent open-plane verbs (read/curate/forget under
[access-policy.md](access-policy.md)) do **not** apply to secrets. Sealed-plane
cross-agent and operator access is governed exclusively by grants plus realm
roles in [authorization-and-roles.md](authorization-and-roles.md).

## Lifecycle

Secrets support a complete, audited lifecycle. Each operation maps to a scope (see
[authorization-and-roles.md](authorization-and-roles.md)) and to a CLI verb /
`/v1/secrets` route. Mutations are guarded by `If-Match` / `row_version`; stale
writes return `conflict`.

- **create** (`secret:create`). Create a secret with `name`, required
  `description`, optional `template`, `tags`, and fields. Sensitive field values
  are enveloped on write. Name must be unique within the owner. Group ownership is
  selected with `--group <name>`.
- **show** (`secret:show`). Return redacted detail: metadata, non-sensitive field
  values, and `value_ref` placeholders for sensitive fields. Never returns
  plaintext sensitive values. This is the open, non-ceremony read of sealed-plane
  *metadata*.
- **list / scan** (`secret:show`). Inventory by metadata — owner, template, tag,
  name prefix, field name, archived state. `scan` is the realm-wide redacted
  inventory for operators (`--all-agents`); neither ever reveals sensitive values.
- **reveal** (`secret:reveal`). The explicit, audited value-returning operation
  for **one** sensitive field. See [Reveal Ceremony](#reveal-ceremony).
- **update** (`secret:update`). Add, change, or remove fields, flip a field's
  `sensitive` marker, edit description/tags. Changing a sensitive value re-envelopes
  it. Historical sensitive values stay protected by the same reveal rules as
  current values.
- **rename** (`secret:update`). Rename within the owning principal. Subject to the
  per-owner name-uniqueness rule.
- **copy** (`secret:create` on the destination, plus `secret:reveal` when copying
  sensitive values). Copy within an agent, to another agent, or to a group. Copying
  sensitive values duplicates protected material and is therefore a separate
  explicit option that requires confirmation and an audit `reason`.
- **archive** (`secret:update`). Soft-archive (`archived_at`); excluded from
  ordinary reads, retained and restorable. Distinct from delete.
- **restore** (`secret:update`). Clear `archived_at` and return the secret to
  active state.
- **delete** (`secret:delete`). The implemented operation is a guarded
  tombstone delete under an exact row-version fence and durable retry key. It
  excludes the secret from ordinary reads and releases retained capacity while
  scrubbing secret metadata and deleting every field and wrapped-DEK row. A
  minimal value-free tombstone, receipt, and audit event remain for retry and
  recovery bookkeeping. Irreversible purge of that tombstone is a separate
  future operation.
- **grant / revoke** (`secret:grant`). Create or remove an explicit cross-agent or
  group access grant. See [Cross-Agent and Operator Access](#cross-agent-and-operator-access).

Every lifecycle operation emits an audit event
(`secret.created`/`updated`/`renamed`/`copied`/`archived`/`restored`/`deleted`,
`secret.reveal`, `secret.grant`/`revoke`). Audit rows never contain sensitive
values; see [audit-retention.md](audit-retention.md). The current tombstone
delete does not accept a reason or dry-run flag; its optimistic revision and
idempotency receipt are the mutation guards. Future irreversible operations and
copy-with-sensitive may add separately specified confirmation and dry-run
ceremonies.

## Reveal Ceremony

`secret reveal` is the bright line between sealed material and plaintext. Unlike
the open plane — where memories are recalled and facts are plainly read with no
ceremony (that statement is scoped to the open plane; see
[memory-model.md](memory-model.md) and [facts-model.md](facts-model.md)) —
sensitive secret values cross into plaintext only here.

Reveal rules:

- Reveal returns exactly **one** named sensitive field, not a whole secret.
- Reveal requires `secret:reveal` (own secrets) or a matching grant / realm role
  (others). Operator and cross-agent reveals also require an audit `reason`.
- Reveal is always audited (`secret.reveal`), recording actor, target secret,
  target field, owner context, result, and — for the server-mediated path — the
  `server_side_decrypt` flag. The plaintext value is never written to the audit
  row, logs, metrics, or errors.
- Decryption follows the hybrid model in [key-hierarchy.md](key-hierarchy.md):
  clients that can hold key material decrypt client-side by default
  (`client_side_decrypt`); managed token-only pods that hold only a bearer token
  use the capability-gated, narrowly audited `server_side_decrypt` path.
- Each reveal (and each reference resolution) is metered as `secret_read`.

The MCP surface gates reveal explicitly. `--no-value-tools` disables
`secret.reveal`, `totp.code`, and value-returning `reference.resolve` while
leaving metadata reads available; `--read-only` disables all mutations. See
[mcp-tools.md](mcp-tools.md).

## Secret References

A reference is a stable pointer to a sensitive (or non-sensitive) field that lets
scripts, config files, MCP tools, and `witself run` name a value **without
embedding plaintext**. Secret references use the `witself://secret/...` family.

Reference forms:

- Current authenticated agent (or a granted view):
  `witself://secret/<secret-path>/<field>`
- Specific owning agent (operator/admin, or granted):
  `witself://agent/<agent-name>/secret/<secret-path>/<field>`
- Group-owned secret:
  `witself://group/<group-name>/secret/<secret-path>/<field>`

Examples:

```
witself://secret/github/builder/password
witself://agent/browser-agent/secret/github/builder/password
witself://group/platform/secret/github/org-readonly-token/token
```

In reference syntax the **final** path component is the field name; all prior
components form the secret path. Field names that conflict with path syntax are
URL-encoded.

Reference rules:

- A reference is resolvable without exposing its value (it is safe to store in
  config and logs). It resolves to plaintext only through an explicit,
  value-returning operation: `secret reveal`, `totp code`, `witself run`, or an
  authorized MCP `reference.resolve` (disabled under `--no-value-tools`).
- A reference can never cross an owner boundary unless the authenticated principal
  holds permission (grant or realm role) for that owner. The `agent/` and `group/`
  forms exist precisely because secret names are unique per owner.
- Value-returning reference resolution is audited (`secret.reveal`) and metered
  (`secret_read`).

## Password Generation

`witself password generate` produces credentials without storing them, with
consumer-grade controls:

- Length, character classes, special-character inclusion, ambiguous-character
  avoidance.
- Human-readable passphrase-style generation.
- `--json` machine-readable output for unattended workflows.

The generator is available on the CLI and, where policy allows, via MCP
(`witself.password.generate`). Generated values follow the same redaction rules:
they appear only in the generating command's output, never in logs, audit rows,
or errors. A common flow is generate → create/update a sensitive field in one
authorized step so the value is enveloped immediately.

## Runtime Injection (`witself run`)

`witself run` resolves `witself://` references and injects plaintext into a child
process's environment or argv **without printing it to stdout**, so an agent can
use a credential without ever surfacing it in context, memory, or logs:

```
witself run --env GITHUB_TOKEN=witself://secret/github/builder/password -- ./deploy.sh
```

Injection rules:

- Each injected reference is authorized exactly like a reveal (own secret, grant,
  or realm role) and audited (`secret.reveal`) and metered (`secret_read`) plus
  `runtime_injection`.
- Plaintext exists only in the spawned child's environment; it is not written to
  Witself logs, audit metadata, or the parent's stdout.
- References that cannot be authorized fail the run deterministically before the
  child starts.

## Cross-Agent and Operator Access

Default isolation: an agent sees and acts on only its own secrets. Anything wider
is an explicit grant or a realm role — never a default.

- **Grants** (`grt_`, `secret:grant`). An explicit, audited, optionally
  field-scoped and optionally expiring authorization that lets a named agent (or a
  group) show, reveal, generate codes for, or update a specific secret it does not
  own. Grants are authorization checks, not separate crypto boundaries (a field
  gets its own DEK only when an operator opts in). Grant and revoke emit audit
  events.
- **Realm roles** (`realm:admin`, `realm:operator`, `realm:auditor`,
  `realm:member`). Operators inspect realm-wide redacted inventory (`scan
  --all-agents`) and, when their role and an audit `reason` allow, reveal or
  mutate a specific owner's secret. Broad destructive operations require explicit
  targeting (`--owner-agent <name>` or a specific group) plus confirmation. Role
  and scope resolution is pinned in
  [authorization-and-roles.md](authorization-and-roles.md).

The default agent token bundle includes `secret:create`, `secret:show`,
`secret:reveal`, `secret:update`, `secret:delete`, `totp:enroll`, and `totp:code`
over the agent's **own** data only — and excludes `secret:grant` and all
cross-owner scopes.

## Relationship to TOTP

A TOTP enrollment is sealed-plane material colocated with a secret: the seed is a
high-value sensitive field (`totp-seed`) and is **never** returned by the ordinary
agent surface. `totp code` returns the current generated code (audited
`totp.code`, metered `totp_code`); the seed itself is revealed only through the
more privileged `totp:enroll` path. Full enrollment/code/show/delete behavior,
`otpauth://`/Base32/QR import, and seed handling are pinned in
[totp-2fa.md](totp-2fa.md).

## Metering and Audit Summary

Sealed-plane operations meter these dimensions (see
[billing-and-limits.md](billing-and-limits.md)): `stored_secret`,
`encrypted_storage_byte`, `secret_read` (reveal + reference resolution),
`totp_code`, `runtime_injection`. Audit events:
`secret.created`/`updated`/`renamed`/`copied`/`archived`/`restored`/`deleted`,
`secret.reveal`, `secret.grant`/`revoke`, and the TOTP events in
[totp-2fa.md](totp-2fa.md), with `server_side_decrypt` flagged on the
server-mediated reveal/code path.

## Related Docs

- [memory-model.md](memory-model.md)
- [facts-model.md](facts-model.md)
- [encryption-model.md](encryption-model.md)
- [key-hierarchy.md](key-hierarchy.md)
- [authorization-and-roles.md](authorization-and-roles.md)
- [totp-2fa.md](totp-2fa.md)
- [secret-size-and-attachments.md](secret-size-and-attachments.md)
- [data-model.md](data-model.md)
- [json-contracts.md](json-contracts.md)
- [access-policy.md](access-policy.md)
- [security-groups.md](security-groups.md)
- [context-hydration.md](context-hydration.md)
- [mcp-tools.md](mcp-tools.md)
- [cli-command-surface.md](cli-command-surface.md)
- [billing-and-limits.md](billing-and-limits.md)
- [audit-retention.md](audit-retention.md)
- [backup-and-recovery.md](backup-and-recovery.md)
- [requirements.md](requirements.md)
