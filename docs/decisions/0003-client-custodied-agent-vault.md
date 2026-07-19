# ADR 0003: Agent Secrets Use Client-Custodied Vault Keys

Status: accepted; implementation in progress (2026-07-18).

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
~/.witself/keys/accounts/<account>/realms/<realm>/agents/<agent>.key
```

`WITSELF_HOME` overrides the root. The key file is versioned, created once with
exclusive creation, owner-only directories, mode `0600`, an atomic durable
write, regular-file checks, and no silent overwrite. Ordinary output exposes
only the key id/fingerprint. The initial file may be protected by host
permissions; OS keychain and passphrase-protected capsules are additive
hardening, not a backend recovery path.

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
and value-free history so import into another cell is lossless. The AVK travels
separately with the client. An archive without the matching AVK remains a
complete encrypted archive but cannot yield plaintext. Loss of every AVK copy
is deliberately unrecoverable.

Multiple-machine enrollment is required but its exact transfer ceremony is a
follow-on. Explicitly paired installations will end with the same AVK or a
successor version. The flow must support two laptops without a QR code, require
human confirmation by default, and allow an AI to orchestrate opaque local
steps without exposing raw key bytes to the model, MCP, messages, or
transcripts. Versioned AVK ids and wrappers make that additive.

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
- `secret list`, `secret search`, and `secret show` are redacted. `secret
  reveal`, `totp code`, reference resolution, and `witself run` decrypt locally.
- `witself run` and reference-based injection are preferred to printing a
  value. Deliberately value-returning CLI/MCP operations may place plaintext
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

- installation-to-installation AVK enrollment and lost-device handling;
- optional OS keychain, secure enclave, or passphrase capsule protection;
- user-controlled encrypted recovery packages;
- installation proof-of-possession and signed mutation intents;
- agent-to-agent or group sharing using recipient-specific key wrappers;
- encrypted attachments and large-object streaming;
- browser-specific filling that keeps values out of model context;
- polished AVK rotation orchestration beyond versioned DEK re-wrap primitives.

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
