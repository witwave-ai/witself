# Encryption model (sealed plane)

> **Client custody model (accepted 2026-07-18):**
> [ADR 0003](decisions/0003-client-custodied-agent-vault.md) and the
> [client-custodied vault plan](client-custodied-agent-vault.md) define the
> client-held agent vault key and encrypted backend storage. Cloud KMS may protect
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

Witself uses client-custodied envelope encryption for the sealed plane. The
active client encrypts and decrypts with the agent vault key (AVK); its bearer
token independently authorizes backend access. A token-only installation can
read redacted inventory but cannot reveal sensitive values or generate TOTP
codes. The concrete key hierarchy, envelope, rotation, and trust-boundary
analysis is in [key-hierarchy.md](key-hierarchy.md).

## Decision

Managed Witself Cloud and production self-hosted deployments store encrypted
secret blobs remotely. The secret-use path decrypts in the trusted client
runtime:

- `witself` CLI.
- `witself mcp serve` when running locally beside an agent.
- Local agent runtime performing the planned `witself run` injection flow.

Each sensitive-value path requires the matching AVK in that client.

The backend authorizes access, returns encrypted material and required metadata,
and records audit events. The backend never receives the AVK and cannot decrypt
secret values, including when a caller requests a reveal.

Managed token-only ephemeral pods need an enrolled matching AVK before they
can use sensitive values. There is no backend plaintext fallback. Encryption,
decryption, password generation, and TOTP calculation happen in the active
client for managed and self-hosted backends alike.

The sealed plane has no cloud-KMS dependency. Backend readiness concerns
storage; client key availability is checked separately and fails closed. See
[storage.md](storage.md) and the [vault plan](client-custodied-agent-vault.md).

## Default reveal and TOTP flow

Default sensitive field reveal:

1. Caller authenticates with the owning agent token and holds the matching AVK.
2. Backend authorizes encrypted field access for that agent; future grants and
   group ownership require a separately specified client-custody design.
3. Backend records value-free audit for authorized encrypted material access.
4. Backend returns encrypted field material plus metadata needed by the client.
5. CLI or local MCP runtime decrypts the field value.
6. Only the explicit `witself secret reveal` command prints or returns the
   plaintext value.

Default TOTP code generation:

1. Caller authenticates with the owning agent token and holds the matching AVK.
2. Backend authorizes encrypted field access for the target agent and field.
3. Backend records value-free audit for the encrypted material request.
4. Backend returns the encrypted TOTP payload and envelope metadata.
5. CLI or local MCP runtime decrypts the seed and generates the current code.
6. The generated code is returned only by the explicit `witself totp code` flow.

These flows keep plaintext passwords, API keys, TOTP seeds, generated TOTP
codes, and unwrapped DEKs in the active client. A missing or mismatched AVK
fails closed; a bearer token alone never substitutes for it.

Reveal and TOTP code are the only value-returning sealed-plane operations. MCP
exposes them as policy-gated tools that `--no-value-tools` disables; `--read-only`
disables mutations. The open plane has no equivalent — memories and facts are
plainly readable and have no reveal (see [memory-model.md](memory-model.md)).

## Client custody boundary

A workflow that needs a secret must run in an authorized active client holding
the matching AVK. Automation, recovery, and migration do not create a backend
plaintext exception. Token-only installations retain access to redacted
inventory and must enroll or recover the key before sensitive use.

Sensitive use remains narrow and explicit:

- Authorize encrypted material access before returning an envelope.
- Record the actor, target, purpose, and outcome without plaintext.
- Keep plaintext out of logs, errors, analytics, support data, and audit.
- Gate value-returning client tools with `--no-value-tools`.
- Treat runtime injection and cross-agent sharing as follow-on work with their
  own acceptance requirements.

## Envelope shape

The cryptographic contract is defined in
[client-custodied-agent-vault.md](client-custodied-agent-vault.md) and summarized
in [key-hierarchy.md](key-hierarchy.md):

- Each sensitive field generation has a fresh 256-bit DEK.
- The active client encrypts the field with AES-256-GCM and wraps the DEK under
  the agent's separate 256-bit AVK, using independent random nonces.
- Value AAD binds immutable field identity and generation; DEK-wrap AAD also
  binds the wrapping AVK identity/version and wrap revision.
- The backend stores ciphertext, wrapped DEKs, and public key metadata. It
  never stores the AVK or a plaintext sensitive value.
- AVK rotation re-wraps DEKs in the active client without changing field
  ciphertext. Key versions remain distinct from optimistic row revisions.
- Base64 is only serialization for binary-safe storage and transport, not a
  security boundary.

A TOTP enrollment is one sensitive encrypted payload, including its seed and
TOTP parameters. `totp show` decrypts and parses that payload locally to return
seed-free metadata; the backend has no plaintext TOTP metadata table. See
[totp-2fa.md](totp-2fa.md) and [data-model.md](data-model.md).

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
- Apply per-agent isolation. Cross-agent grants and group ownership remain
  follow-on work and do not authorize a token-only plaintext path.
- Record value-free audit for encrypted material access, key rotation, and
  other sensitive operations. Audit does not prove that local use succeeded.
- Return ciphertext for sensitive fields and redacted ordinary inventory.
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
database and object/blob backups still exist. Infrastructure-encrypted storage
may separately require its infrastructure keys to restore those backups.
Sensitive-value recovery requires the matching client-held AVK or a usable
client recovery artifact; support cannot recover plaintext from ciphertext alone.

Witself does not include a managed support break-glass decrypt path. If every
usable copy of the matching AVK and its recovery material is lost, the affected
encrypted secret values may be unrecoverable. AVK loss does not affect
open-plane memories and facts.

Any future recovery feature that can expose plaintext or rewrap customer secret
material must be designed as an explicit product/security feature with:

- Clear operator consent.
- Strong authorization.
- Step-up approval when appropriate.
- Tamper-evident audit.
- Clear managed versus self-hosted behavior.

## Key hierarchy follow-up

The accepted hierarchy is `client-held AVK → per-sensitive-field DEK → value`.
[key-hierarchy.md](key-hierarchy.md) summarizes envelope binding and client-led
rotation; [client-custodied-agent-vault.md](client-custodied-agent-vault.md)
records the implemented lifecycle and the remaining follow-on slices. Storage
and relational schema context is in [storage.md](storage.md) and
[data-model.md](data-model.md).

Backup, export, and key-loss recovery posture is tracked in
[backup-and-recovery.md](backup-and-recovery.md). Secrets are never in the
plaintext identity export. An account archive carries ciphertext, wrapped
DEKs, and public key metadata; the client retains the AVK or protected recovery
artifact separately.

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
