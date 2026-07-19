# Encryption model (sealed plane)

> **Superseded custody model (2026-07-18):**
> [ADR 0003](decisions/0003-client-custodied-agent-vault.md) and the
> [client-custodied vault plan](client-custodied-agent-vault.md) replace the
> KMS-rooted and server-mediated decrypt design below. Cloud KMS may protect
> infrastructure at rest, but it is not the agent-vault trust root and Witself
> has no backend decrypt path.

Status: draft. Decision: this document specifies confidentiality for the **sealed
plane** only — secrets and TOTP enrollments. The **open plane** (memories and
facts) is ordinary application data-at-rest: stored in PostgreSQL and on disk
under the deployment's standard volume/RDS encryption, indexed for recall,
recallable, and plaintext-exportable. The open plane has no reveal ceremony, no
envelope encryption, and no KMS dependency. None of the guarantees below apply to
it. The two-tier split is the master decision; see
[requirements.md](requirements.md).

Sealed-plane invariant (carried everywhere secrets appear): secret values and
TOTP seeds are **never embedded, never returned by semantic recall, never in the
self-digest, never plaintext-exported, and never ingested** from
CLAUDE.md/AGENTS.md. Encryption is one half of that invariant; the recall/digest
carve-out is the other (see [memory-model.md](memory-model.md) and
[context-hydration.md](context-hydration.md)).

Witself uses a hybrid encryption model for the sealed plane. Client-side decrypt
is the default for clients that can hold or derive key material (the `ws`
CLI, local `witself mcp serve`, `witself run`, local dev, and self-hosted/BYOK).
Managed token-only ephemeral pods hold only a bearer token and cannot unwrap a key
locally, so their reveal/TOTP path runs over the capability-gated
`server_side_decrypt` exception — which, for that deployment shape, is the everyday
path, not a rare one. The concrete key hierarchy, envelope, rotation, and
trust-boundary analysis is in [key-hierarchy.md](key-hierarchy.md).

## Decision

Managed Witself Cloud and production self-hosted deployments store encrypted
secret blobs remotely. The default secret-use path decrypts in the trusted client
runtime:

- `witself` CLI.
- `witself mcp serve` when running locally beside an agent.
- Local agent runtime performing `witself run`.
- Local development adapter after the local realm is unlocked.

The backend authorizes access, returns encrypted material and required metadata,
and records audit events. For client-side-decrypt clients the backend does not
decrypt secret values just because a caller requested a reveal.

The structural exception is the managed token-only ephemeral pod: it holds only a
bearer token (`witself_at_...`) and no key material, so it cannot reach KMS to
unwrap a DEK. Its reveal/TOTP path necessarily runs server-mediated under the
`server_side_decrypt` capability — the server unwraps and returns plaintext over
TLS. This is the dominant managed shape, so `server_side_decrypt` runs at default
frequency there rather than as a rare event; the trade-off is analyzed in
[key-hierarchy.md](key-hierarchy.md).

Server-side decrypt is otherwise allowed only for explicit workflows that truly
require it. All server-side decrypt paths must be narrow, policy-gated,
capability-labeled, and audited.

KMS is a required dependency only when the sealed plane is enabled. An
open-plane-only deployment (memories + facts) does not need KMS. Readiness gates on
KMS only when the sealed plane is enabled; ordinary PostgreSQL is the open-plane
gate and migration-0032 client vectors need no extension. See
[storage.md](storage.md).

## Default reveal and TOTP flow

Default sensitive field reveal:

1. Caller authenticates with an agent or operator token.
2. Backend authorizes the reveal (`secret:reveal`; cross-agent/operator access via
   grants and realm roles — see [authorization-and-roles.md](authorization-and-roles.md)).
3. Backend records an audit event (`secret.reveal`) for the reveal request.
4. Backend returns encrypted field material plus metadata needed by the client.
5. CLI or local MCP runtime decrypts the field value.
6. Only the explicit `witself secret reveal` command prints or returns the
   plaintext value.

Default TOTP code generation:

1. Caller authenticates with an agent or operator token.
2. Backend authorizes TOTP code generation for the target secret (`totp:code`).
3. Backend records an audit event (`totp.code`) for the TOTP request.
4. Backend returns encrypted TOTP seed material plus TOTP metadata.
5. CLI or local MCP runtime decrypts the seed and generates the current code.
6. The generated code is returned only by the explicit `witself totp code` flow.

For client-side-decrypt clients (CLI, local `mcp serve`, `witself run`, BYOK) this
keeps the backend from seeing plaintext passwords, API keys, TOTP seeds, or
generated TOTP codes. For managed token-only pods the same flows run server-mediated
(`server_side_decrypt`): the server transiently sees the DEK and plaintext in
memory, never persists or logs it, redacts it everywhere, and audits every event
with the `server_side_decrypt` flag set.

Reveal and TOTP code are the only value-returning sealed-plane operations. MCP
exposes them as policy-gated tools that `--no-value-tools` disables; `--read-only`
disables mutations. The open plane has no equivalent — memories and facts are
plainly readable and have no reveal (see [memory-model.md](memory-model.md)).

## Server-side decrypt exception

Server-side decrypt may be introduced for workflows that cannot reasonably work
with pure client-side decrypt.

Candidate exception cases:

- Managed token-only ephemeral-pod reveal/TOTP, where the pod has only a bearer
  token and cannot unwrap a DEK locally. This is the everyday managed path, not a
  rare one (see [key-hierarchy.md](key-hierarchy.md)).
- Future managed browser automation where the service performs the login.
- Future managed automation that must use a secret without a local agent runtime.
- Carefully designed recovery or migration workflows.
- Self-hosted deployments where the operator explicitly enables server-side
  decrypt for internal automation.

Requirements for every server-side decrypt path:

- Disabled unless the backend advertises the `server_side_decrypt` capability.
- Requires a specific permission or policy grant.
- Requires a specific operation, not a generic decrypt endpoint.
- Requires an audit event that identifies the actor, target, purpose, and outcome
  without storing plaintext.
- Should require an audit `reason` for operator/admin and cross-agent use.
- Must redact plaintext from logs, errors, analytics, support data, and audit
  metadata.
- Must be distinguishable in API, CLI, MCP, and audit records from the default
  client-side decrypt path (the `server_side_decrypt` flag on `secret.reveal` and
  `totp.code` audit events).

## Envelope shape

The exact cryptographic package choices are finalized with the storage and KMS
design in [key-hierarchy.md](key-hierarchy.md), but the model is envelope-oriented:

- Each sensitive field or secret blob has ciphertext, nonce, algorithm
  (`XCHACHA20_POLY1305` or `AES_256_GCM`), key-version metadata, and authenticated
  context.
- Data-encryption keys (`dek_...`) are wrapped by per-realm key material
  (`kek_...`) — one KEK per realm, not per deployment.
- KMS or local key-management boundaries protect the wrapping keys
  (`CMK → per-realm KEK → per-secret/per-field DEK`).
- Key versions are recorded so rotation can be performed deliberately
  (metadata-only re-wrap; see [key-hierarchy.md](key-hierarchy.md)).
- Base64 is only serialization for binary-safe storage and transport, not a
  security boundary.

Sketch of a per-ciphertext envelope (illustrative; canonical shape in
[json-contracts.md](json-contracts.md) and [key-hierarchy.md](key-hierarchy.md)):

```json
{
  "schema_version": "witself.v0",
  "realm_id": "realm_...",
  "dek_id": "dek_...",
  "kek_version": 3,
  "aead_algorithm": "XCHACHA20_POLY1305",
  "nonce": "<base64>",
  "ciphertext": "<base64>",
  "aad": {
    "secret_id": "sec_...",
    "field_id": "fld_...",
    "owner_kind": "agent"
  }
}
```

Plaintext secret values and TOTP seeds must not be ordinary database columns. They
live only as ciphertext in the `secrets`, `secret_fields`, and `totp_enrollments`
tables; DEKs are stored wrapped in `secret_deks`; per-realm KEKs are tracked in
`realm_keys`. See [data-model.md](data-model.md) and [storage.md](storage.md).

## Client responsibilities

Clients that decrypt secret material must:

- Decrypt only for explicit reveal, TOTP, reference resolution
  (`witself://secret/<path>/<field>`), or runtime injection flows.
- Avoid logging plaintext.
- Avoid placing plaintext in errors.
- Prefer `witself run` or reference resolution over printing secrets when a
  subprocess can consume them directly.
- Mask injected values from stdout/stderr where practical.
- Keep decrypted values in memory only as long as needed.

## Backend responsibilities

The backend must:

- Authenticate the caller.
- Authorize access before returning encrypted material.
- Apply per-agent isolation and explicit grants; group-owned secrets resolve
  through the owning group (`owner_kind ∈ {agent, group}`).
- Record audit events for reveal, TOTP code, server-side decrypt, grant/revoke,
  key rotation, and other sensitive use.
- Enforce capability flags for client-side and server-side decrypt modes.
- Keep raw secret values, TOTP seeds, generated TOTP codes, raw tokens
  (`witself_at_...`), passphrases, private keys, raw payment details, and wallet
  credentials out of logs, audit records, analytics, support data, and errors.
- Never embed, recall, digest, or plaintext-export sealed-plane material — the
  carve-out is enforced at the data layer, not just by convention.

## Recovery posture (secret values only)

This section concerns sealed-plane secret values only. Open-plane memories and
facts follow ordinary backup/restore and plaintext export (see
[backup-and-recovery.md](backup-and-recovery.md)); they are not subject to the
crypto-shred outcome below.

Default posture: Witself staff should not have ordinary support access to customer
plaintext secrets.

V0 managed recovery is limited to restoring service availability, restoring
operator account access, and restoring encrypted customer data when the required
database, object/blob, and KMS material still exists. It is not a plaintext secret
recovery feature.

V0 does not include a managed support break-glass decrypt path. If key material
required to unwrap encrypted data is lost (KMS-loss / crypto-shred), some or all
encrypted secret values may be unrecoverable. KMS-loss affects secret values only;
it does not affect the open plane.

Any future recovery feature that can expose plaintext or rewrap customer secret
material must be designed as an explicit product/security feature with:

- Clear operator consent.
- Strong authorization.
- Step-up approval when appropriate.
- Tamper-evident audit.
- Clear managed versus self-hosted behavior.

## Key hierarchy follow-up

The concrete design for the key hierarchy, key wrapping, and rotation mechanics is
settled in [key-hierarchy.md](key-hierarchy.md): a `CMK → per-realm KEK →
per-secret/per-field DEK` hierarchy, the per-ciphertext envelope, and
metadata-only re-wrap rotation. The storage/KMS context is in
[storage.md](storage.md) and the relational schema in
[data-model.md](data-model.md).

The trust-boundary reframing — client-side decrypt as the structurally enforced
default where a client can hold key material, and the managed token-only
ephemeral-pod reveal/TOTP path reclassified as the capability-gated
`server_side_decrypt` exception — was **approved on 2026-06-26** and is reflected in
this doc's Decision above. Remaining sub-decisions (BYOK in v0, per-realm
cryptographic isolation, DEK granularity, KEK cache, GDPR posture) are tracked in
the key-hierarchy doc's open decisions.

Backup, export, and KMS-loss recovery posture is tracked in
[backup-and-recovery.md](backup-and-recovery.md). Note: secrets are never in the
plaintext export; secret backup is encrypted-only (envelope plus KMS key identity).

## Related docs

- [requirements.md](requirements.md)
- [key-hierarchy.md](key-hierarchy.md)
- [data-model.md](data-model.md)
- [authorization-and-roles.md](authorization-and-roles.md)
- [backend-architecture.md](backend-architecture.md)
- [threat-model.md](threat-model.md)
- [api-contract.md](api-contract.md)
- [json-contracts.md](json-contracts.md)
- [storage.md](storage.md)
- [backup-and-recovery.md](backup-and-recovery.md)
- [memory-model.md](memory-model.md)
- [context-hydration.md](context-hydration.md)
- [self-hosting.md](self-hosting.md)
