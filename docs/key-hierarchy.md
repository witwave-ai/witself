# Key hierarchy and decrypt trust boundary

Status: accepted custody model. [ADR 0003](decisions/0003-client-custodied-agent-vault.md)
replaced the earlier hierarchy on 2026-07-18. The sealed plane uses a
client-custodied per-agent vault key (AVK) that wraps per-sensitive-field DEKs.
The backend stores ciphertext, wrapped DEKs, redacted inventory, and public key
metadata; encryption and decryption happen only in the active client.

This document summarizes the hierarchy, envelope, rotation, and trust boundary
in [client-custodied-agent-vault.md](client-custodied-agent-vault.md).
[Sealed-plane acceptance](sealed-plane-acceptance.md) distinguishes the
implemented lifecycle gate from the larger live certification target.

This doc governs the **sealed plane only** (secrets and TOTP seeds). The open plane
(memories + facts) is ordinary data-at-rest and is never wrapped in this hierarchy. Sealed
material in this hierarchy is **never embedded, never returned by semantic recall, never in
the self-digest, never plaintext-exported, and never ingested** from CLAUDE.md/AGENTS.md.
Sensitive use follows the explicit client ceremonies described under
[Reconciliation](#reconciliation-with-the-reveal-totp-contract).

<a id="why-pure-client-side-decrypt-is-not-achievable-everywhere"></a>

## Client key custody is required

A bearer token (`witself_at_...`) authenticates an agent; it is not a decryption
key. Token delivery via `WITSELF_TOKEN_FILE` does not provision an AVK.
A token-only ephemeral pod may read redacted inventory but cannot create,
reveal, or use sensitive values until it holds the matching agent vault key.
A missing or mismatched local key fails closed. If the backend already has a
key binding, the client must not generate a replacement as a fallback.

The backend never receives the AVK, never calls KMS for agent-secret operations,
and has no plaintext-returning sensitive-value operation. Managed and self-hosted
backends use the same client-custody contract.

### Custody and authorization

| Possession | Result |
| --- | --- |
| Database or account archive only | Public inventory, ciphertext, and wrapped DEKs; no plaintext sensitive values |
| Agent token only | Authorized redacted reads; no sensitive create/reveal/use |
| AVK only | No backend authorization or envelope discovery |
| Token plus matching AVK | Authorized local encrypt/decrypt/use for that agent |
| Operator token | Permitted metadata/policy management; no token-only plaintext access |

### Accepted decision

Use one hierarchy and one custody model: `client-held AVK → field DEK → value`.
The client independently needs both backend authorization and the correct key.
Authorization determines which encrypted material may be read; the AVK enables
local decryption. Neither possession substitutes for the other.

Password generation, TOTP parsing/calculation, and encryption/decryption happen
in the active client. The backend cannot attest that authorized encrypted
material was successfully decrypted or used.

## V0 crypto subset

The agent-owned vertical and key lifecycle are implemented through schemas
`0055`, `0056`, and `0067`. This is not a claim that the larger live runtime/cloud
certification has passed. The implemented subset includes:

- One AES-256-GCM envelope and one fresh DEK per sensitive field generation.
- Client-held AVKs and public backend bindings that are independent of bearer
  tokens and token rotation.
- Agent-token access to redacted inventory and one-field encrypted material.
- Client-local secret reveal and TOTP show/code.
- Recipient-bound installation enrollment, passphrase-encrypted offline
  recovery artifacts, and client-driven, crash-resumable AVK rotation.
- Account archives containing encrypted vault state and public key metadata,
  never the AVK or a recovery artifact.

Secret update/replacement, dedicated TOTP enroll/delete convenience commands,
runtime injection, grants/group ownership, irreversible secret purge or
crypto-shred, encrypted attachments, and additional installation
proof-of-possession remain follow-on work. See the authoritative
[vault plan](client-custodied-agent-vault.md) for the exact boundary.

## Key hierarchy

```text
Client-held Agent Vault Key (AVK; one per agent)
  └── wraps one DEK per sensitive field generation
        └── encrypts the sensitive value or TOTP payload
```

1. **AVK.** A random 256-bit key generated and retained by the active client.
   The local owner-only key file holds the opaque key record. The backend
   stores only public identity, fingerprint, algorithm, and version metadata.
2. **DEK.** A fresh random 256-bit key for each sensitive field generation.
   The client wraps it under the matching AVK; only the encrypted wrapper is
   stored remotely in `secret_deks`.
3. **Field ciphertext.** The client encrypts the sensitive value with
   AES-256-GCM and a fresh random 12-byte nonce. DEK wrapping uses the same
   primitive with an independent nonce and a separate AAD domain.

Key versions and logical epochs make rotation deliberate and recoverable.
They do not change the token's authentication semantics. Base64 is transport
serialization, not a security boundary.

## Per-ciphertext envelope

Each sensitive field value or TOTP payload is stored as an envelope. Ordinary
list/search/show returns redacted inventory; authorized material access returns
the encrypted package. Open-plane memory/fact rows carry no such envelope and
have no secret-style reveal ceremony.

The wrapped DEK is stored separately from the field ciphertext. Rotation must
update the wrapper and its current wrapping-key metadata together without
changing the field's immutable value binding. The backend validates structure
and scope, while the active client authenticates the encrypted material.

The following are conceptual components; the backend material response uses
`algorithm`, `encoding`, and a nested `dek` object.

| Component | Description |
| --- | --- |
| `ciphertext` | AES-256-GCM encrypted field value or TOTP payload, with the random nonce prepended; binary storage and JSON base64 transport. |
| `aead_algorithm` | `AES_256_GCM_RANDOM_NONCE_V1`, the exact wire identifier for AES-256-GCM with a random nonce; distinct from the TOTP HMAC algorithm inside the encrypted payload. |
| DEK identity and generation | Field DEK identity and generation used to reconstruct authenticated bindings. |
| Value version and encoding | Immutable value generation and encoding, bound into value AAD. |
| Wrapped DEK | The only remotely stored form of the DEK; the active client unwraps it using the AVK. |
| Wrapping key identity, version, and wrap revision | Public current-wrapper metadata, independently bound into DEK-wrap AAD. |
| `aad_version` | Selects the canonical AAD encoder; callers do not supply stored AAD as authority. |

**AAD invariant.** The client reconstructs value AAD from immutable returned
account, realm, owner-agent, secret, field, DEK, and value-generation bindings.
Value AAD uses `witself/sealed-field/v1`, or `witself/totp-payload/v1` for TOTP.
DEK-wrap AAD uses `witself/dek-wrap/v1` and also binds the wrapping key identity,
version, and wrap revision. AVK rotation changes the wrapper binding without
changing the value AAD or field ciphertext.

An unexpected key identity/version or authentication failure produces a generic,
value-free integrity error. A client must never accept a caller-supplied AAD
blob as authority or fall back to a different key on mismatch.

Encoding metadata describes the value opened locally by the client. Ordinary
redacted show does not return that value. The distinction
between binary serialization and encryption remains the same in storage,
transport, and client output; see [json-contracts.md](json-contracts.md).

## Rotation, re-wrap, and backfill

**Field generations.** The current create path generates a fresh DEK for each
sensitive field. Secret update/replacement and historical-value commands are
follow-on work; they must not be inferred from the envelope's generation
fields.

**AVK rotation.** `witself vault key rotate` creates or resumes the agent's one
open rotation targeting exactly `source_version + 1`. The client durably
stores the target epoch, unwraps each current DEK with the source AVK, re-wraps
it under the target AVK, and stages only encrypted wrappers on the backend.
Field ciphertext is unchanged. Sensitive create is blocked while rotation is
open; public-only create remains allowed.

Before commit, the client independently reloads and validates all staged
items. It must either durably publish, read back, inspect, and decrypt-verify an
exact target recovery artifact, or explicitly accept permanent key-loss risk.
Only the value-free disposition and artifact digest reach the backend.

Commit atomically updates all current wrappers and the public key epoch under
revision, item-count, plan-hash, and recovery-disposition fences. Interrupted
work resumes from the locally retained target key. The old local epoch remains
available; another installation with only the old epoch must enroll again.
Cancellation retires the candidate; a fresh candidate may reuse the logical
target version, but an old recovery artifact cannot restore a later binding.
See [client-custodied-agent-vault.md](client-custodied-agent-vault.md) for the
complete retry and recovery contract.

AVK logical version, DEK generation, wrap revision, and optimistic row revision
are different counters. None is a substitute for another when validating or
committing encrypted state. The backend stages and commits ciphertext; it does
not perform a background plaintext-key re-wrap.

<a id="kms-loss-posture"></a>

## Key-loss posture

Loss of every usable matching AVK and its recovery material can make the
affected sealed values and TOTP payloads unrecoverable. Witself cannot recover
them from a bearer token, operator access, or a database archive alone.
Open-plane memories and facts do not depend on the agent vault key.

- There is deliberately **no managed break-glass decrypt**. Managed recovery
  restores service availability, operator account access, and encrypted data;
  the client separately supplies or recovers its AVK.
- Multi-installation enrollment transfers the key through a recipient-bound
  encrypted capsule; the backend is a ciphertext relay.
- Offline recovery artifacts are client-created and passphrase-encrypted.
  Production certification requires an independent recovery destination, not
  a same-disk copy alone.

**Key loss is not metadata erasure.** It does not remove secret names, public
fields, access patterns, audit records, or other retained metadata/PII. It also
does not erase open-plane data. Account deletion, retention, and erasure are
separate operations described in [data-model.md](data-model.md) and
[backup-and-recovery.md](backup-and-recovery.md). Irreversible secret purge or
crypto-shred is not part of the implemented vault lifecycle.

Account archives carry ciphertext, wrapped DEKs, public AVK bindings, and
terminal lifecycle history. They carry no AVK, local key file, live transfer
capsule, passphrase, or recovery artifact. The AVK must remain separately
recoverable; moving an archive does not require source-cloud key access.

## Per-realm tenant isolation

Authorization, query scoping, and client cryptographic binding must agree:

1. **Cryptographic binding.** Each agent has its own AVK. Value and wrapper AAD
   bind account, realm, owner agent, secret, and field identity. A client
   rejects mismatched returned material rather than opening it in another
   scope. Backend access alone does not supply the missing AVK.
2. **Authorization.** The shared authorization layer resolves the bearer token
   to its owning agent and checks encrypted-material access. The implemented
   vault is agent-owned; cross-agent grants and group ownership require
   separate follow-on custody design.
3. **Query scoping.** The backend scopes secret, field, key, audit, and usage
   operations to the authenticated account, realm, and agent. Identifiers do
   not become high-cardinality metric labels.

Managed and self-hosted backends share the same encrypted package and custody
model. A compromised token or backend can still threaten integrity and
availability of authorized ciphertext and expose public inventory; it does not
by itself provide a key for offline decryption.

<a id="reconciliation-with-the-reveal-totp-contract"></a>

## Reconciliation with the reveal / TOTP contract

Sensitive API responses carry encrypted material. CLI and local MCP reveal
results are value-returning only after decryption in the active client. Memory
and fact reads retain their separate open-plane authorization and redaction
rules.

| Aspect | Client-custodied path |
| --- | --- |
| Who unwraps the DEK and runs AEAD | Active client holding the matching AVK |
| Backend material response | Ciphertext, wrapped DEK, and public envelope/key metadata |
| CLI/MCP reveal result | One locally decrypted field |
| TOTP show | Client decrypts/parses the payload and returns seed-free metadata |
| TOTP code | Client decrypts/parses the payload and calculates the current code |
| Backend audit | Authorized encrypted material access; no assertion of local decryption/use success |

The client path is:

1. Authenticate as the owning agent and obtain authorized encrypted field
   material from the backend.
2. Verify the returned scope, immutable field bindings, and AVK identity/version
   against the local installation.
3. Reconstruct DEK-wrap AAD and unwrap the DEK with the matching local AVK.
4. Reconstruct value AAD and authenticate/decrypt the field locally.
5. Return the explicitly requested value, or parse the TOTP payload and return
   seed-free metadata or a locally calculated code.

A token-only installation cannot complete these steps. There is no backend
plaintext fallback. MCP `--no-value-tools` disables value-returning tools;
`--read-only` disables mutations. Runtime injection remains follow-on work.

The current ADR 0003 implementation emits
`witself_secret_material_deliveries_total` for server-observed ciphertext
delivery calls; it cannot observe client decryption. See
[Sealed-plane SLOs](observability-and-operations.md#sealed-plane-slos).

## Revises

ADR 0003 replaces the former custody decision. These documents now use the
same client-held key boundary:

- [encryption-model.md](encryption-model.md): the backend never receives sealed
  plaintext or the AVK.
- [api-contract.md](api-contract.md) and [json-contracts.md](json-contracts.md):
  ciphertext-only backend material responses and client-local reveal results.
- [storage.md](storage.md): encrypted fields, wrapped DEKs, and public AVK
  bindings; no backend key-provider dependency for agent secrets.
- [threat-model.md](threat-model.md): client confidentiality, bearer-token
  integrity risks, and separate infrastructure protection.
- [requirements.md](requirements.md): client custody and the sealed-plane
  recall/digest/export/ingest exclusions apply across deployments.

## Residual risks

- A compromised active client can expose its AVK, unwrapped DEKs, or plaintext
  in process memory, crash dumps, or deliberate output. Minimize plaintext
  lifetime and keep it out of logs, errors, metrics, and audit.
- A value-returning provider tool can deliberately place a secret into model
  context. Client custody does not undo a selected plaintext disclosure.
- A stolen bearer token cannot decrypt but can attempt authorized-looking
  mutation or deletion until revoked. Concurrency, idempotency, backup, and
  audit remain necessary integrity and availability controls.
- Token-only pods cannot use sensitive values; operators must provision or
  recover the matching key through the supported client lifecycle.
- AVK rotation needs an active client, durable local state, and an explicit
  recovery disposition. Other installations must enroll after the key changes.
- Public metadata, public fields, access patterns, and audit remain visible to
  the backend. TOTP parameters remain inside the encrypted payload; local
  `totp show` deliberately returns seed-free metadata.
- Key loss does not erase metadata/PII, old exports, or backups. Recovery and
  account erasure remain distinct workflows.

## Open decisions

The accepted AVK hierarchy, per-field DEKs, AES-256-GCM envelope, and client
custody are settled. Neither a token-only plaintext exception nor a cloud-KMS
vault root is an open mode choice.

Follow-on decisions concern the explicitly deferred product surfaces in the
[vault plan](client-custodied-agent-vault.md): secret update/replacement,
runtime injection, grants/group ownership, dedicated TOTP convenience commands,
irreversible secret purge or crypto-shred, additional installation
proof-of-possession, and encrypted attachments. The broader live certification
must pass its named cases before it is claimed complete.

## Related docs


- [encryption-model.md](encryption-model.md)
- [data-model.md](data-model.md)
- [storage.md](storage.md)
- [secret-model.md](secret-model.md)
- [totp-2fa.md](totp-2fa.md)
- [authorization-and-roles.md](authorization-and-roles.md)
- [threat-model.md](threat-model.md)
- [requirements.md](requirements.md)
- [api-contract.md](api-contract.md)
- [json-contracts.md](json-contracts.md)
- [token-lifecycle.md](token-lifecycle.md)
- [backup-and-recovery.md](backup-and-recovery.md)
- [self-hosting.md](self-hosting.md)
- [secret-size-and-attachments.md](secret-size-and-attachments.md)
- [observability-and-operations.md](observability-and-operations.md)
- [v0-scope.md](v0-scope.md)
