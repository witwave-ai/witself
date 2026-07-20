# Witself TOTP and 2FA

> **Custody amendment (accepted 2026-07-18):**
> [the client-custodied vault plan](client-custodied-agent-vault.md) stores a
> TOTP enrollment as a sensitive client-encrypted field. URI parsing and code
> generation happen locally; KMS-backed or server-generated code paths below
> are superseded.

> **Current implementation boundary:** enroll by including one sensitive
> `kind: "totp"` field with `otpauth_uri` in `witself secret create --file` or
> `--stdin`. Use `witself totp show SECRET FIELD` for seed-free metadata and
> `witself totp code SECRET FIELD` for a current code; each selector may be an
> exact ID or an unambiguous human-readable name. Both retrieve
> one encrypted field package and decrypt it locally; even `show` does not rely
> on server-readable TOTP metadata. Dedicated `totp enroll`/`delete`, raw seed,
> seed-file, and QR-image convenience inputs remain target behavior, not the
> current command surface.

Status: draft. Last reviewed 2026-06-26.

Witself acts as the authenticator application for agents. When a service offers a
QR code or setup secret that a human would normally enroll in an authenticator
app, the agent or operator enrolls that material into Witself instead. Witself
stores only the client-encrypted setup payload. The active client generates the
current one-time code on request and returns it to the authorized agent at login
time.

TOTP lives in the **sealed plane** alongside [secrets](secret-model.md). The
underlying seed is high-value sealed material: it is stored only in a
client-authored envelope and is **never embedded, never returned by semantic recall, never in the
self-digest, never ingested from `CLAUDE.md`/`AGENTS.md`, and never
plaintext-exported**. The normal agent-facing surface returns generated codes,
not the seed. This document pins the authenticator role, the enrollment inputs,
code generation, metadata `show`, removal, the sealed-seed storage model, and the
`totp:enroll` vs `totp:code` scope split.

Confidentiality of the seed envelope is governed by
[encryption-model.md](encryption-model.md) and the
[key hierarchy](key-hierarchy.md). Who may enroll, generate, reveal, or delete is
governed by [authorization-and-roles.md](authorization-and-roles.md).

## Goals

- Make the common authenticator-app TOTP role work well inside Witself in v0.
- Let an authorized agent request a current one-time code through the CLI, MCP,
  or API when a login flow requires it.
- Keep the seed sealed: import/setup/export takes a more privileged path than
  ordinary code generation, and the agent login surface never sees the seed.
- Carry non-sensitive TOTP metadata (issuer, account label, algorithm, digits,
  period) as ordinary queryable columns so `totp show` answers without unwrapping.

## Scope (v0)

In scope: authenticator-app style TOTP — enrollment, code generation, metadata
`show`, and removal. Out of scope (post-v0): 2FA mechanisms that require external
push approval, an SMS inbox, an email inbox, a passkey, or a hardware security
key. See [post-v0-roadmap.md](post-v0-roadmap.md).

## Authenticator role

The expected login flow:

- Agent retrieves or fills the username (a secret field — see
  [secret-model.md](secret-model.md)).
- Agent retrieves or fills the password (a secret field).
- Agent requests a current 2FA code from Witself (`witself totp code`).
- Witself returns the current generated code through the authorized CLI, MCP, or
  API path, never the seed.

A TOTP enrollment hangs off a secret. It is keyed by `secret_id`, so the code
path resolves directly without enumerating fields. One live enrollment per
secret.

## Enroll

`witself totp enroll NAME` enrolls TOTP setup material into an existing or new
secret. Enrollment is the **privileged seed path** and requires the `totp:enroll`
scope.

```sh
witself totp enroll github/builder --otpauth 'otpauth://totp/...'
witself totp enroll github/builder --secret JBSWY3DPEHPK3PXP --issuer GitHub --account builder
witself totp enroll github/builder --secret-file ./github-seed.txt
witself totp enroll github/builder --qr ./github-2fa.png
```

Enrollment accepts the seed from any of four inputs:

| Input | Flag | Notes |
|---|---|---|
| `otpauth://` URL | `--otpauth URL` | Carries seed plus issuer/account/algorithm/digits/period when present. |
| Base32 seed | `--secret VALUE` | Manual Base32 setup secret. Least safe — the seed sits on the command line. |
| Seed file | `--secret-file PATH` | Read the Base32 seed from a file rather than argv. Preferred over `--secret`. |
| QR-code image | `--qr PATH` | Parse setup material from a QR image when image parsing is available. |

Other flags:

| Flag | Description |
|---|---|
| `--group NAME` | Enroll on a group-owned secret. |
| `--owner-agent NAME_OR_ID` | Operator/admin only: enroll on a secret owned by a specific agent. |
| `--issuer TEXT` | Issuer name (non-sensitive metadata). |
| `--account TEXT` | Account label (non-sensitive metadata). |
| `--digits N` | Number of TOTP digits. Default: `6`. |
| `--period SECONDS` | TOTP period. Default: `30`. |
| `--algorithm SHA1\|SHA256\|SHA512` | TOTP HMAC algorithm. Default: `SHA1`. |
| `--create-secret` | Create the secret if it does not exist. |
| `--description TEXT` | Required when `--create-secret` creates a new secret. |
| `--reason TEXT` | Audit reason. |

Ownership is unified across the platform: a TOTP enrollment is owned by an
**agent** or a **group** (`owner.kind ∈ {agent, group}`), inheriting the owner of
its secret. There is no separate "shared" scope; a shared 2FA login is a
group-owned secret.

On enrollment, the seed is encrypted into an envelope and discarded in plaintext.
The non-sensitive metadata is stored as ordinary columns. Enrollment emits
`totp.enrolled`.

## Code generation

`witself totp code NAME` generates the current one-time code. This is an
explicit, audited, value-returning op gated by the `totp:code` scope.

```sh
witself totp code github/builder
witself totp code github/builder --remaining --json
```

Flags:

| Flag | Description |
|---|---|
| `--group NAME` | Generate a code for a group-owned secret. |
| `--owner-agent NAME_OR_ID` | Operator/admin only: generate a code for a secret owned by a specific agent. |
| `--at TIMESTAMP` | Generate the code for a specific time. Testing and recovery only. |
| `--remaining` | Include seconds remaining in the current period. |
| `--reason TEXT` | Audit reason for code generation. |

Code generation unwraps the seed envelope per the
[key hierarchy](key-hierarchy.md) (client-side decrypt where the client holds key
material, server-mediated decrypt for token-only pods, behind the capability
switch), computes the code, and returns it. The returned result carries the code
and timing metadata, **never the seed**:

```json
{
  "secret": {
    "id": "sec_123",
    "name": "github/builder"
  },
  "code": "123456",
  "digits": 6,
  "period_seconds": 30,
  "remaining_seconds": 18,
  "expires_at": "2026-06-26T18:00:30Z",
  "audit_event_id": "aud_124"
}
```

The full shape is normative in [json-contracts.md](json-contracts.md). Each
generation emits `totp.code` and meters the `totp_code` dimension (see
[billing-and-limits.md](billing-and-limits.md)).

## Show metadata

`witself totp show NAME` returns the non-sensitive TOTP metadata — issuer,
account label, algorithm, digits, period — **with the seed redacted**. This is
the safe inspection path and does not unwrap the envelope.

Flags:

| Flag | Description |
|---|---|
| `--group NAME` | Show metadata for a group-owned secret. |
| `--owner-agent NAME_OR_ID` | Operator/admin only: show metadata for a secret owned by a specific agent. |
| `--reveal-seed` | Reveal the underlying TOTP seed. Admin/operator privileged path only. |
| `--reason TEXT` | Audit reason when revealing the seed. |

`--reveal-seed` is the only path that returns the seed, and it is the
seed-export/backup ceremony, **not** ordinary login use. It requires the
privileged `totp:enroll` scope (the same seed-handling tier as enrollment), not
`totp:code`. Revealing the seed emits `totp.seed_revealed` and is audited the
same way as a secret reveal. The seed is never returned by `totp code`, never
embedded, never recalled, never in the self-digest, and never
plaintext-exported — `--reveal-seed` is an explicit, audited break-glass step
distinct from those carve-outs.

## Delete

`witself totp delete NAME` removes the TOTP setup material from a secret. The
secret and its other fields are untouched.

Flags:

| Flag | Description |
|---|---|
| `--group NAME` | Remove TOTP from a group-owned secret. |
| `--owner-agent NAME_OR_ID` | Operator/admin only: remove TOTP from a secret owned by a specific agent. |
| `--dry-run` | Show removal impact without deleting the setup material. |
| `--yes` | Skip confirmation. |
| `--reason TEXT` | Audit reason. |

Deletion soft-deletes the enrollment row (frees the one-enrollment-per-secret
slot) and emits `totp.deleted`. It does not crypto-shred the secret; only loss of
the wrapping key material does (see [encryption-model.md](encryption-model.md)).

## Sealed-seed storage model

The seed is stored only as a per-ciphertext envelope, never as a plaintext
column. The enrollment row lives in `totp_enrollments` (see
[data-model.md](data-model.md)), keyed by `secret_id` with one live row per
secret:

```sql
CREATE UNIQUE INDEX ux_totp_enrollments_secret
  ON totp_enrollments (secret_id)
  WHERE deleted_at IS NULL;
```

The row separates two distinct algorithm columns:

- `algorithm` — the **non-sensitive TOTP HMAC hash** (`SHA1` | `SHA256` |
  `SHA512`), surfaced by `totp show`.
- `aead_algorithm` — the **envelope's AEAD primitive** (`XCHACHA20_POLY1305` |
  `AES_256_GCM`).

Because the whole row is always an envelope, the seed envelope columns
(`ciphertext`, `nonce`, `aead_algorithm`, `dek_id`, `kms_provider`,
`aad_context`) are `NOT NULL`. The seed's wrapping follows the
[key hierarchy](key-hierarchy.md): CMK → per-realm KEK → per-secret/field DEK.
TOTP seeds are a default site for an **opt-in per-field DEK** so the seed gets its
own DEK distinct from the secret's other fields.

The envelope binds authenticated associated data (AAD) from stable identifiers
only, with the domain tag `"totp-seed"`. AAD deliberately excludes any rotating
KEK version, so KEK rotation provably cannot affect AAD. The wrapped DEK lives in
`secret_deks` and is referenced by `dek_id`; KEK rotation re-wraps exactly that
one row.

| Stored | Plane | Treatment |
|---|---|---|
| Seed | sealed | Envelope only. Never plaintext at rest, never embedded/recalled/in-digest/plaintext-exported. Returned only via `totp show --reveal-seed`. |
| Generated code | — | Computed on demand, returned by `totp code`, never persisted. Never in audit. |
| Issuer, account, algorithm, digits, period | metadata | Ordinary queryable columns; returned by `totp show`. |

Audit rows MUST NEVER store the seed or any generated code. Audit events for
TOTP: `totp.enrolled`, `totp.code`, `totp.seed_revealed`, `totp.deleted` (see
[audit-retention.md](audit-retention.md)).

## Scopes: `totp:enroll` vs `totp:code`

Two scopes split the privileged seed path from ordinary login use:

| Scope | Grants | Operations |
|---|---|---|
| `totp:enroll` | The **privileged seed path**. Holds, imports, and reveals seed material. | `totp enroll`, `totp delete`, `totp show --reveal-seed`. |
| `totp:code` | Ordinary login use. Generates codes; never sees the seed. | `totp code`, and `totp show` for metadata. |

The default agent token bundle includes both `totp:enroll` and `totp:code` over
the agent's **own** data only. Operator or cross-agent use of `--owner-agent`
requires the corresponding realm role or grant; secrets are not subject to the
open cross-agent read/curate verbs — TOTP access composes through grants and
realm roles in [authorization-and-roles.md](authorization-and-roles.md), not the
open-plane policy engine.

Code generation is also surfaced through MCP (`witself.totp.code`) and the API
(`POST /v1/totp/{secret_id}:code`). Both are value-returning and are disabled by
the MCP `--no-value-tools` switch alongside `secret.reveal`; `--read-only`
disables mutations such as `totp.enroll` and `totp.delete`. See
[mcp-tools.md](mcp-tools.md).

## Related

- [secret-model.md](secret-model.md) — the sealed-plane secret model TOTP hangs off.
- [encryption-model.md](encryption-model.md) — sealed-plane confidentiality model.
- [key-hierarchy.md](key-hierarchy.md) — CMK → per-realm KEK → DEK envelope.
- [authorization-and-roles.md](authorization-and-roles.md) — scopes, realm roles, grants.
