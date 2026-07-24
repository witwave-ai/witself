# ADR 0003: Agent Secrets Use Client-Custodied Vault Keys

Status: accepted; end-to-end vertical plus multi-installation enrollment,
offline recovery, and crash-resumable rotation implemented through schema
`0056`, with the explicitly listed follow-ons still planned (2026-07-19).

## Context

Witself needs a sealed plane for named autonomous agents: structured login
credentials, API keys, generated passwords, TOTP enrollments, recovery codes,
and runtime injection. The same stable agent can run under Codex, Claude Code,
Cursor, or Grok Build, on multiple machines, while its account moves among AWS,
Google Cloud, and Azure cells.

The earlier sealed-plane draft uses a cloud KMS root and permits
server-mediated decryption for token-only clients. That conflicts with the
required trust boundary:

- a database copy, cell operator, or Witself service must not be sufficient to
  decrypt an agent's values;
- a bearer token authenticates an installation but is not a decryption key;
- independently issued tokens for one agent must share the same logical vault;
- token rotation or revocation must not rotate or destroy the vault;
- moving an account cannot depend on the source cloud's KMS still existing;
- the client that is already running the AI performs encryption, decryption,
  password generation, TOTP calculation, and eventual key transfer.

The backend remains deterministic and model-free. Client inference decides when
to search for or use a credential. Witself and MCP do not wake an offline AI
client.

## Decision

Each named agent owns one logical vault protected by a separate, stable
256-bit **Agent Vault Key (AVK)**. The AVK is generated and held by an active
client installation. It is never uploaded to the Witself API, written to
PostgreSQL, included in an account archive, sent through MCP, or placed in a
prompt, transcript, message, hook payload, log, metric, audit event, or support
bundle.

The agent's `.token` and `.key` have different jobs:

- the token proves which agent is calling the backend and can be independently
  issued, expired, rotated, or revoked;
- the AVK decrypts that agent's sealed material and is independent of token
  lifecycle.

The v1 hierarchy is:

```text
client-held Agent Vault Key
    -> wraps one random 256-bit DEK per sensitive field generation
        -> encrypts that field value or TOTP payload
```

AES-256-GCM is the v1 AEAD for both value encryption and DEK wrapping. The Go
1.26 client uses `cipher.NewGCMWithRandomNonce`, which prepends a fresh random
96-bit nonce to every sealed payload. The implementation must keep one AVK and
one DEK below the primitive's per-key message limit. Algorithm, envelope, AAD,
and key versions are explicit; there is no negotiation in v1.

Authenticated associated data uses a canonical length-prefixed binary encoding
and binds every envelope to immutable account, realm, owner-agent, secret,
field, DEK, domain, encoding, and version identifiers. Mutable names,
descriptions, usernames, URLs, tags, timestamps, token ids, cell ids, and cloud
providers are excluded. A blob copied into another logical slot therefore fails
authentication, while rename and cell migration do not require re-encryption.

The backend stores only:

- public secret metadata and non-sensitive searchable field values;
- ciphertext and wrapped DEKs, including their algorithm and version metadata;
- public AVK id, fingerprint, version, and lifecycle metadata, never key bytes;
- value-free idempotency receipts, usage, and audit events.

The backend authorizes an envelope read but has no decrypt operation and no
`server_side_decrypt` capability. An audit event records authorized encrypted
material delivery; it does not claim that the client successfully produced or
displayed plaintext. The client can emit a separate value-free outcome when
useful.

The portable v1 local layout mirrors token scoping without changing token
format:

```text
~/.witself/tokens/accounts/<account>/realms/<realm>/agents/<agent>.token
~/.witself/keys/accounts/<account>/realms/<realm>/agents/<agent>.key  # legacy v1
~/.witself/keys/accounts/<account>/realms/<realm>/agents/<agent>/epochs/<version>-<avk_id>.key
```

`WITSELF_HOME` overrides the root. The key file is versioned, created once with
exclusive creation, owner-only directories, mode `0600`, an atomic durable
write, regular-file checks, and no silent overwrite. Ordinary output exposes
only the key id/fingerprint. Immutable epoch files allow a rotation source and
target to coexist; a matching legacy v1 file remains supported. Local files may
be protected by host permissions; OS keychain/secure-enclave integration is
additive hardening, while the implemented offline recovery artifact remains a
separate client-owned recovery path.

Access to that path requires a trusted local account binding containing the
account's immutable `AccountID`; the account name is only a local path alias.
The client verifies that the authenticated self identity has the pinned
`AccountID` before reading or creating any AVK file. An explicit endpoint and
token file do not replace this binding or authorize inferring account identity
from the token alone. Installed MCP bindings without `account_id` are legacy
and must be reinstalled before agent-secret tools can access a vault key.

Key bootstrap is a fail-closed state machine, not a local `load-or-create`
helper. The client first reads the authenticated agent's public current-key
binding. It generates a new AVK only when both that binding and the local key
are absent. A local key with no binding may register its own public metadata.
If a binding exists but the local key is absent, the client reports
`key_unavailable`; if both exist but id, version, or fingerprint differ, it
reports `key_mismatch`. Neither condition generates, replaces, or uploads a
key. This prevents a fresh machine or deleted local file from silently
orphaning existing ciphertext.

No unencrypted or token-only fallback exists. A client without the AVK can use
authorized redacted inventory but cannot create, reveal, update, generate TOTP
from, or inject sensitive values.

Account export includes all public metadata, ciphertext, wrapped DEKs, key ids,
and terminal value-free lifecycle history so import into another cell is
lossless. Export is rejected while an enrollment is pending/approved or a
rotation is open. The archive never contains an AVK, local key file, enrollment
private key, pairing secret, recovery passphrase, or recovery artifact. One of
those client-owned key paths must travel separately. An archive without the
matching AVK remains a complete encrypted archive but cannot yield plaintext.
Loss of every AVK copy and every usable recovery artifact is deliberately
unrecoverable.

Multi-installation enrollment is an implemented, short-lived relay. The target
creates an X25519 key pair and pairing commitment locally; the backend stores
only the target public key and commitment. An already-enrolled installation
authenticates the pairing secret, seals the exact AVK to that recipient, and
uploads only an ephemeral public key and recipient-bound ciphertext. The target
durably installs the AVK, proves consumption, and the backend purges the live
transfer capsule. The pairing secret is displayed/read only on the controlling
TTY and is never accepted through argv, environment variables, stdin/pipes,
JSON, MCP, messages, or transcripts.

Offline recovery is also implemented entirely client-side. Export writes a
new, never-overwritten artifact containing the exact AVK encrypted with fixed
Argon2id-v1 parameters and AES-256-GCM, authenticated to stable account, realm,
agent, and key identity. Import succeeds only when that identity matches the
backend's current public binding. Recovery passphrases are read with hidden
controlling-TTY input and never sent to Witself. Public artifact inspection
requires neither a passphrase nor a backend connection.

AVK rotation is client-driven and crash-resumable. It creates exactly
`source_version + 1`, persists that candidate locally, stages only newly wrapped
DEKs, verifies the complete deterministic plan, and atomically flips every
wrapper and the current public epoch under exact fences. Immediately before
commit, the client must either durably publish and read-back/decrypt-verify a
passphrase recovery artifact for the exact target, or explicitly accept
permanent key-loss risk. The committed lifecycle row records only the
`recovery_artifact` mode and artifact SHA-256, or `risk_accepted`; the backend
never receives the artifact, path, passphrase, or AVK. Sensitive secret creation
is blocked while a rotation is open. The old local epoch is retained; other
installations must enroll again after commit. Cancelling retires the candidate
while allowing a fresh id to retry the same logical target version.

Production recovery output must be external or synchronously replicated before
the sink acknowledges durability. A recovery file on the same physical disk is
useful for process-level recovery but is not an independent device-loss copy.
The typed sink remains entirely client-side so trusted automation can supply its
own storage and credential source without adding provider-specific backend
custody or exposing key lifecycle credentials through MCP.

Account export, irreversible account close, and deletion of an agent are fenced
while that agent has pending/approved enrollment or an open rotation. Those
operations would otherwise revoke tokens or move state needed to settle the
lifecycle. Cancellation remains available on a suspended account; cancel first
and then retry the export or destructive operation.

Bearer tokens continue to govern integrity and authorization in the first
slice, as they do for the rest of Witself. A stolen token can attempt destructive
sealed mutations but cannot decrypt ciphertext. Installation proof-of-possession
and signed mutation intents are a compatible later hardening layer; they do not
change the encryption or archive format. This boundary is stated explicitly
rather than implying that client custody replaces token security.

## Product Consequences

- Secret name, description, template, tags, field names, and fields explicitly
  marked non-sensitive can be indexed and searched in PostgreSQL. Sensitive
  values cannot be searched by the backend.
- `secret list`, `secret search`, and `secret show` are redacted. Implemented
  `secret reveal` and `totp code` decrypt locally; reference resolution and
  `witself run` remain planned.
- Once implemented, `witself run` and reference-based injection are preferred
  to printing a value. Deliberately value-returning CLI/MCP operations may place plaintext
  inside the invoking provider's client boundary and must be explicit,
  auditable, and disableable with `--no-value-tools`.
- TOTP seeds use the same sealed envelope. Parsing `otpauth://` input and
  generating the current code happen locally; the backend sees neither seed nor
  code.
- An operator may direct an active agent to create, use, or rotate a secret. An
  operator token and backend access alone cannot decrypt it.
- Cloud KMS may still protect database volumes, backups, deployment secrets, or
  Pulumi state. It is defense in depth, not the agent-vault trust root.
- The actively running client necessarily sees plaintext in process memory.
  Client custody does not protect against compromise of that machine, model
  provider, child process, or destination service.

## Deferred Without Blocking the Core

- secret update/replacement and dedicated TOTP enrollment/removal commands;
- runtime reference resolution and side-effect-oriented injection;
- optional OS keychain or secure enclave protection;
- installation proof-of-possession and signed mutation intents;
- agent-to-agent or group sharing using recipient-specific key wrappers;
- encrypted attachments and large-object streaming;
- browser-specific filling that keeps values out of model context;
- irreversible purge of the minimal value-free secret tombstone and its
  recovery/retention policy. The ordinary guarded delete now scrubs secret
  metadata, fields, and wrapped DEKs while retaining that tombstone plus
  receipt/audit evidence.

None of these may introduce a backend-held plaintext or master-key path.

## Superseded Decisions

This ADR supersedes only the sealed-custody portions of ADR 0001 and drafts that
specify `CMK -> realm KEK -> DEK`, KMS as the agent-vault root, BYOK as an
alternate mode, or a capability-gated `server_side_decrypt` exception. It keeps
the two-plane separation, structured-secret model, field sensitivity,
redaction, audit, usage, TOTP, and account-portability requirements.

## Alternatives Considered

- **Cell KMS as the sole root.** Rejected because the service can decrypt and a
  move depends on source-cloud key access.
- **Token-derived encryption.** Rejected because token rotation would destroy
  access or require backend escrow, and any token copy would become a vault
  copy.
- **Backend-held account master key.** Rejected because database/service
  compromise becomes plaintext compromise.
- **Optional unencrypted secrets.** Rejected because accidental downgrade is
  too easy and agents with no sensitive values need no vault.
- **Per-machine independent vault keys.** Rejected because the same named agent
  would fragment across machines. Installation-specific wrappers can be added,
  but paired installations must reach the same logical AVK.
- **Build multi-device enrollment before basic storage.** Rejected because the
  transfer ceremony can be added over versioned AVK metadata without weakening
  the single-installation core.

## Related

- [Client-Custodied Agent Vault](../client-custodied-agent-vault.md)
- [Secret Model](../secret-model.md)
- [TOTP / 2FA](../totp-2fa.md)
- [Token Lifecycle](../token-lifecycle.md)
- [Backup and Recovery](../backup-and-recovery.md)
- [ADR 0001](0001-consolidate-witpass-into-witself.md)
