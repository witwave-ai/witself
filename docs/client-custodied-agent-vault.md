# Client-Custodied Agent Vault

Status: authoritative implementation and release plan. ADR 0003 is accepted.
The agent-owned vertical, multi-installation enrollment, offline recovery, and
crash-resumable AVK rotation are implemented through schema `0056`; only the
explicitly listed follow-on slices remain planned. Last reviewed 2026-07-19.

This document turns Witself's agent-secrets product into buildable slices. It is
the authoritative custody and delivery contract wherever an older sealed-plane
draft still describes KMS-rooted keys or server-side decryption.

## Outcome

A named Witself agent can keep structured credentials that follow the agent
across supported AI products, machines, and deployment cells. The backend is a
portable ciphertext store plus deterministic authorization, audit, and search.
Only an active client with both an authorized agent token and the agent's
separate vault key can turn a sensitive envelope into plaintext.

The first hook-capable runtime set is Codex, Claude Code, Cursor, and Grok Build.
GitHub Copilot is now a phase-one managed-instructions and stdio-MCP adapter, so
it can use the same sealed-plane tools through guided MCP fallback; Gemini
remains a future adapter. The backend and archive format behave the same on AWS,
Google Cloud, and Azure.

## Current Implementation Boundary

The implemented vertical includes:

- migrations `0055` and `0056` with agent vault-key bindings, structured
  secrets, fields, wrapped DEKs, enrollment and rotation lifecycles, and
  idempotency receipts;
- client-only AVK generation, owner-only local key files, AES-256-GCM field
  envelopes, password generation, TOTP parsing, and TOTP calculation;
- agent-token HTTP routes for current-key status/registration, create,
  redacted list/search/show, one-field encrypted material access, archive, and
  restore;
- short-lived recipient-bound installation enrollment, passphrase-encrypted
  offline recovery artifacts, and client-driven, crash-resumable AVK rotation;
- CLI access to the complete client-custody path and MCP access to the ordinary
  secret/TOTP path (key lifecycle credentials remain CLI/TTY-only); and
- account export/import of public key metadata, terminal lifecycle history,
  and byte-identical encrypted vault state, never the AVK or a recovery
  artifact.

The following are deliberately not claims of this vertical: secret update,
dedicated TOTP enroll/delete convenience commands, runtime injection,
cross-agent grants or group ownership, permanent secret deletion,
installation proof-of-possession beyond the implemented pairing ceremony, and
the live four-runtime by three-cloud certification matrix. Those are follow-on
slices and must not be inferred from older target-contract documents.

## Frozen Boundaries

1. PostgreSQL is authoritative for secret records, public fields, ciphertext,
   wrapped DEKs, key metadata, mutation receipts, usage, and value-free audit.
2. PostgreSQL never contains an AVK, plaintext sensitive value, TOTP seed,
   generated TOTP code, or generated password before encryption.
3. A `.token` authenticates an agent; it never encrypts or decrypts a vault.
4. The AVK is a separate client-held 256-bit value. Token files and token
   rotation semantics do not change.
5. The backend never calls KMS or an AI model for secret operations and exposes
   no decrypt endpoint.
6. Public metadata is searchable. Sensitive values are intentionally not.
7. An account archive carries the complete encrypted vault but never the AVK.
8. No insecure or token-only sealed-value fallback exists.
9. Automatic behavior is foreground-client behavior. Hooks and routing rules
   may teach an active client to search or use a reference; they do not wake a
   provider, run a background model, or put plaintext in hydration.
10. Multi-machine enrollment uses a short-lived, recipient-bound ciphertext
    relay. Pairing secrets and recovery passphrases are accepted only on the
    controlling TTY, never through argv, environment variables, stdin/pipes,
    JSON, MCP, messages, or transcripts.

## Threat Boundary

| Possession | Result |
| --- | --- |
| Database or account archive only | Public inventory and ciphertext; no decrypt |
| Agent token only | Authorized redacted reads; no sensitive create/reveal/use |
| AVK only | No server authorization or envelope discovery |
| Token plus matching AVK | Authorized local encrypt/decrypt/use for that agent |
| Operator token | Permitted metadata/policy management; no offline decrypt |
| Active agent directed by operator | Agent may locally decrypt/use under audit |

The first slice preserves Witself's existing bearer-token integrity model. A
stolen token cannot decrypt, but it can attempt authorized-looking mutation or
deletion until revoked. Backups, optimistic concurrency, idempotency, and audit
limit damage. Installation signatures and proof-of-possession are later defense
in depth, not part of the confidentiality claim.

## Cryptographic Contract

### Key hierarchy

- AVK: 32 random bytes from `crypto/rand`, logical version beginning at 1.
- DEK: 32 random bytes per sensitive field generation.
- Field encryption: AES-256-GCM with a fresh random 12-byte nonce prepended to
  the ciphertext by Go 1.26 `cipher.NewGCMWithRandomNonce`.
- DEK wrapping: the same primitive under the AVK with an independent nonce and
  separate AAD domain.
- Serialization: versioned base64url in the local key file; binary PostgreSQL
  columns and JSON base64 on the wire. Encoding is never a security boundary.

### Immutable bindings

Value AAD is a canonical binary sequence of a domain tag followed by unsigned
32-bit length-prefixed UTF-8 strings and unsigned 64-bit counters:

```text
witself/sealed-field/v1
account_id, realm_id, owner_agent_id, secret_id, field_id, dek_id,
value_version, dek_generation, value_encoding, aead_algorithm
```

DEK-wrap AAD uses `witself/dek-wrap/v1` and additionally binds
`wrapping_key_id`, `wrapping_key_version`, and `wrap_revision`. A TOTP payload
uses `witself/totp-payload/v1`. Components are validated before encoding; the
stored `aad_version` selects the encoder, while callers never supply stored AAD
as authority.

The client reconstructs AAD from immutable returned columns and rejects an
unexpected key id/version or any authentication failure with one generic,
value-free integrity error.

### Implemented rotation

`witself vault key rotate` starts or resumes the authenticated agent's one open
rotation. The target is exactly `source_version + 1`. The client creates and
durably stores that target epoch locally, unwraps each current DEK with the
source AVK, re-wraps it with the target AVK, and stages only encrypted wrappers
on the backend. Sensitive secret creation is blocked while the rotation is
open; public-only secret creation remains allowed.

Before commit, the client reloads every deterministic item page and validates
the complete staged plan independently. It then requires exactly one custody
disposition: durably publish, read back, inspect, and decrypt an offline
recovery artifact for the exact target epoch, or explicitly accept permanent
key-loss risk. Artifact-backed rotation binds the commit to the artifact's
SHA-256 digest; the backend receives only the value-free mode and digest. It
never receives the artifact, its path, passphrase, or AVK.

Commit atomically updates all live `secret_deks` wrappers and flips the current
public key epoch under exact row, item-count, plan-hash, and recovery-disposition
fences. An interrupted command discovers the open rotation and resumes from the
durable target epoch rather than starting a new one. A pre-existing recovery
file is accepted only when it decrypts under the supplied passphrase and exactly
matches the stable owner scope and target key metadata. The old local epoch is
retained after commit. Other installations still holding only the old epoch
fail closed and must enroll again. Cancelling retires the candidate while
permitting a fresh candidate id to retry the same logical `source + 1` version.
Any artifact already published for the cancelled candidate remains an inert,
client-owned file: it is never deleted or overwritten automatically and cannot
restore a later current binding. A retry with a fresh candidate must use a new
output path unless its selected destination is empty.

## Local Custody

```text
~/.witself/
  tokens/accounts/<account>/realms/<realm>/agents/<agent>.token
  keys/accounts/<account>/realms/<realm>/agents/<agent>.key  # legacy v1
  keys/accounts/<account>/realms/<realm>/agents/<agent>/epochs/<version>-<avk_id>.key
  keys/accounts/<account>/realms/<realm>/agents/<agent>/enrollments/<enrollment_id>/...
  keys/accounts/<account>/realms/<realm>/agents/<agent>/rotation/intent.state
  keys/accounts/<account>/realms/<realm>/agents/<agent>/rotation/.intent.lock
  journal/accounts/<account>/realms/<realm>/agents/<agent>/secret-create/<operation_hash>.json
  journal/accounts/<account>/realms/<realm>/agents/<agent>/secret-create/.lock
```

`WITSELF_HOME` overrides the root. The key file contains a versioned opaque
record with schema, random `avk_` id, key version, algorithm, and 32-byte key.
The immutable epoch path lets old/current/candidate epochs coexist without
replacement; a matching legacy v1 file remains readable and can be published
to its epoch path. Enrollment preflight state is request-scoped and owner-only.
Ordinary commands print only id, fingerprint, algorithm, version, and value-free
lifecycle state.

Every local AVK operation first requires an installed local account binding
that pins the account's immutable `AccountID`. The account name remains only a
local path alias. The client authenticates the supplied token and requires the
returned account id to equal that pinned `AccountID` before it reads or creates
the local key path. Supplying an explicit `--endpoint` and `--token-file` does
not bypass this prerequisite: the CLI still resolves the selected local account
binding (the default account or `--account`) and fails closed if it is absent or
does not match.

Creation uses `O_CREATE|O_EXCL`; a concurrent loser re-reads the winning file
and verifies it against the server binding before proceeding.
Directories are `0700`, the file is `0600`, writes are flushed, and neither
read nor creation follows a final-path symlink. A pre-existing malformed,
non-regular, or overly permissive key file fails closed and is never replaced.

Rotation writes its value-free, checksummed `intent.state` after the target
epoch is durable and before remote Start. The stable owner-only `.intent.lock`
fences all create, exact replacement/retirement, and acknowledgement deletion
across local processes. The intent contains source/target public metadata and
the Start retry fence, never AVK/DEK bytes, a recovery artifact/passphrase,
token, or secret value. A CLI terminal result is printed first and then
explicitly acknowledged; only that acknowledgement deletes the exact terminal
intent. A crash before acknowledgement therefore replays the same
committed/cancelled record and cannot silently start another rotation.

If two installations prepare different candidates concurrently, the backend's
one-open invariant chooses the canonical rotation. A losing pristine intent may
be atomically replaced only by an authenticated canonical open record in the
same owner scope whose source is the same or strictly newer. When the exact
loser id is absent, no rotation is open, and the authenticated current binding
has strictly advanced, the client atomically retires only that exact pristine
intent and then fails with the normal missing/mismatched-key result. An adopted
or otherwise non-pristine intent is never deleted by this convergence path.

Sensitive create publishes its complete sealed request to the private local
journal before the backend mutation. Every retry authenticates that exact
request with the retained AVK epoch named by its wrappers and submits it before
consulting the current epoch. Backend receipt replay is ordered before
rotation/current-key validation, so an accepted request whose response was
lost still replays after any later rotation.

Only HTTP `409` carrying the exact machine code
`secret_vault_key_mismatch` proves that the exact journal has no canonical
receipt and targets a retired epoch. After that proof, the client reconciles
the authenticated current AVK, re-wraps only the existing field DEKs, preserves
the ciphertext and secret/field/DEK identities, increments each wrap revision,
and durably compare-and-swaps the journal before sending the replacement. The
stable owner-only `.lock`, exact-byte fence, synced temporary file, atomic
rename, and directory sync make concurrent clients converge on one exact
replacement. A stale contender authenticates and retries the durable winner;
it never sends its losing random wrapper.

Open-rotation conflicts, idempotency conflicts, transport failures, timeouts,
generic errors, and the mismatch text without both the exact `409` status and
machine code never authorize journal replacement. A crash before the CAS leaves
the old request authoritative; a crash or lost response after the CAS retries
the exact replacement. Successive uncommitted epochs can repeat this narrow
transition with monotonically increasing wrap revisions. Retired AVK absence,
corrupt state, wrap-revision overflow, or failed durable publication stops
before a rebased request is sent. Older clients do not understand a rebased
create journal and therefore reject it safely rather than regenerating or
overwriting the protected value.

First sensitive write:

1. resolve the authenticated account, realm, and agent binding;
2. read both the matching local key file and the backend's public current-key
   binding;
3. if both are absent, generate the AVK locally with exclusive creation and
   register only its id, fingerprint, algorithm, and version;
4. if the local key exists but no binding exists, register that local key's
   public metadata;
5. if the binding exists but the local key is absent, fail with
   `key_unavailable` and never generate a replacement;
6. if both exist but id, version, algorithm, or fingerprint differs, fail with
   `key_mismatch` and never overwrite either side;
7. only after the states agree, generate ids, encrypt locally, and submit the
   sealed mutation.

There is intentionally no reusable `load-or-generate` API: key creation is
valid only after the authenticated backend binding has been checked.

## Relational Model

Avatar migrations occupy `0050` through `0054`. The vault begins at `0055`, and
the AVK lifecycle is schema `0056`; no lower-numbered migration may be
introduced after deployed cells have advanced.

### `agent_vault_keys`

One public row per agent key epoch:

- `id`, `account_id`, `realm_id`, `owner_agent_id`;
- `key_version`, `algorithm`, `fingerprint`, `lifecycle_state`;
- `created_at`, `retired_at`, and `row_version`.

There is no key-material or wrapped-key column. Exactly one current epoch is
allowed per live agent. Migration `0056` replaces the historical all-row key
version uniqueness constraint with `agent_vault_keys_one_live_version`, a
partial unique index over `pending` and `current` rows. Registration is
idempotent and rejects a different id or fingerprint for an already-current
version; a cancelled rotation may therefore retain its retired candidate while
a fresh id retries that same logical version.

### `secrets`

- immutable `id`, account/realm/owner-agent scope;
- byte-exact, cross-cloud-portable unique active `(owner_agent_id, name)`;
- public `name`, `description`, `template`, and tags;
- `row_version`, `archived_at`, `deleted_at`, and timestamps;
- generated search document over public metadata.

The first slice is agent-owned. Group ownership, cross-agent copy, and grants
are deferred until recipient key wrapping exists; authorization alone cannot
grant cryptographic possession.

### `secret_fields`

- immutable `id`, scope, `secret_id`, public field `name`, and `field_kind`;
- `sensitive`, `value_encoding`, `value_version`, row version, timestamps;
- non-sensitive branch: `public_value` and no envelope columns;
- sensitive branch: `ciphertext`, `aead_algorithm`, `aad_version`, `dek_id`,
  `envelope_version`, `dek_generation`, and no `public_value`;
- a database check enforces exactly one branch.

Public field names and kinds plus non-sensitive values such as username and
login URL participate in deterministic PostgreSQL full-text search. Passwords,
API keys, private keys, recovery codes, and TOTP payloads do not.

### `secret_deks`

One row per sensitive field generation:

- `id`, full owner/secret/field scope, and `dek_generation`;
- `wrapped_dek`, `wrap_algorithm`, `aad_version`, `wrap_revision`;
- `wrapping_key_id`, `wrapping_key_version`;
- `created_at`, `retired_at`, and `row_version`.

There is no plaintext DEK or server root. Composite scope keys prevent moving a
ciphertext or wrapped DEK between accounts, realms, agents, secrets, or fields.

### `agent_vault_key_enrollments` and receipts

One short-lived request records the exact account, realm, owner agent, current
AVK id/version, target installation id/name, X25519 public key, pairing
commitment, lifecycle, revision, and timestamps. The only live transfer fields
are a source installation id, ephemeral public key, recipient-bound ciphertext,
transfer algorithm, and consume commitment. States are `pending`, `approved`,
`consumed`, `cancelled`, and `expired`; terminal transitions clear the transfer
capsule. `vault_key_enrollment_receipts` stores value-free request hashes and
result revisions for create, approve, consume, and cancel retries. The exact
public labels are `X25519_RAW_32_BASE64URL_V1` for the target key and
`X25519_HKDF_SHA256_AES_256_GCM_V1` for the transfer.

### `agent_vault_key_rotations`, items, and receipts

One `open` rotation per agent binds exact source and target AVK identities,
logical versions, item/staged counts, lifecycle, row version, and timestamps.
A committed row also durably records `recovery_artifact` plus the exact
artifact SHA-256, or `risk_accepted` with no digest. Open and cancelled rows
carry neither disposition field.
`agent_vault_key_rotation_items` freezes each source `secret_deks` wrapper and
stores its optional staged target wrapper plus exact revisions and a digest;
it never stores a DEK or plaintext field value. `vault_key_rotation_receipts`
provides value-free start, stage, commit, and cancel retry shields. A committed
or cancelled rotation is terminal, and terminal archive rows contain no live
staging material.

### TOTP

TOTP uses one `field_kind=totp` sensitive field whose plaintext is a versioned
local payload containing seed, SHA1/SHA256/SHA512 algorithm, digits, period,
and optional issuer/account label. Ordinary secret reads return only its
redacted presence. `totp show` and `totp code` decrypt and parse it locally.
This avoids a second sealed lifecycle in v1 while preserving room for a later
projection table.

### Mutation receipts, audit, and usage

`secret_mutation_receipts` stores actor, operation, idempotency key, canonical
request hash, target id/version, and time. It never duplicates values or
envelopes.

Secret actions reuse Witself's account event ledger with closed, metadata-only
schemas. Audit and errors may include ids, field names, sensitivity booleans,
result codes, sizes, and key versions; never public field values, secret names
when avoidable, envelopes, nonces, wrapped DEKs, raw retry keys, queries, or
plaintext.

The implemented paths meter `stored_secret`, `secret_read`, and
`encrypted_storage_byte`. `totp_code` and `runtime_injection` are reserved for
the later dedicated server-visible operations; current local TOTP generation
is represented by its audited encrypted-field read. Metering failure cannot
make an otherwise successful read unavailable.

## Backend API

The unmarked routes below are implemented. Routes marked **planned** freeze the
intended shape for a later slice and are not registered by the current server.

All agent routes derive account, realm, and owner agent from the bearer token.
The first slice accepts no caller-supplied owner override.

- `POST /v1/vault/key-epochs` registers public current-key metadata.
- `GET /v1/vault/key-epochs/current` returns public key status.
- `POST /v1/vault/enrollments` creates a short-lived target request.
- `GET /v1/vault/enrollments` and
  `GET /v1/vault/enrollments/{enrollment}` return value-free lifecycle state.
- `POST /v1/vault/enrollments/{enrollment}:approve` stores one
  recipient-bound transfer capsule; `:receive` returns that opaque capsule only
  to the target; `:consume` proves durable receipt and clears it; `:cancel`
  terminates pending or approved work.
- `POST /v1/vault/rotations` starts one exact source-to-target rotation.
- `GET /v1/vault/rotations/open` discovers crash-resumable work;
  `GET /v1/vault/rotations/{rotation}` returns one lifecycle record; and
  `GET /v1/vault/rotations/{rotation}/items` pages its deterministic wrapper
  plan.
- `POST /v1/vault/rotations/{rotation}:stage`, `:commit`, and `:cancel`
  mutate that plan under exact revisions and idempotency receipts. Commit
  requires either `{mode: "recovery_artifact", artifact_sha256: "..."}` or
  `{mode: "risk_accepted"}`; the choice participates in request hashing and is
  retained on the terminal lifecycle row and value-free audit event.
- `POST /v1/secrets` creates public fields and client-encrypted envelopes in one
  transaction. Clients generate immutable `sec_`, `fld_`, and `dek_` ids before
  AAD construction.
- `GET /v1/secrets` performs bounded public metadata/full-text search.
- `GET /v1/secrets/{id}` returns redacted detail and permitted public values.
- **Planned:** `PATCH /v1/secrets/{id}` uses exact row versions and accepts
  complete new envelopes for changed sensitive fields.
- `POST /v1/secrets/{id}:archive` and `:restore` are idempotent lifecycle
  operations. Permanent deletion is a separately confirmed follow-on.
- `POST /v1/secrets/{id}/fields/{field_id}:access` authorizes and returns exactly
  one ciphertext plus wrapped-DEK package with `Cache-Control: no-store`. It
  never returns plaintext.

Every mutation requires `Idempotency-Key`. Bodies use the `witself.v0` contract
and carry explicit algorithms and versions. The server validates size, ids,
branch shape, algorithm allowlists, scoped relationships, optimistic versions,
and quota. It cannot validate plaintext or claim decryption success.

## CLI

The implemented command surface is:

```text
witself vault key init|status
witself vault key enroll begin|approve|complete|list|status|cancel
witself vault key recovery export|inspect|import
witself vault key rotate (--recovery-out FILE|--accept-unrecoverable-key-loss)
witself vault key rotation status|cancel
witself password generate
witself secret create|list|search|show|reveal|archive|restore
witself totp show|code SECRET FIELD
```

Exact lifecycle forms are:

```text
witself vault key enroll begin [--location NAME] [--ttl DURATION] [agent connection flags]
witself vault key enroll approve ENROLLMENT_ID [--location NAME] [agent connection flags]
witself vault key enroll complete ENROLLMENT_ID [--location NAME] [agent connection flags]
witself vault key enroll list [--state STATE] [--limit N] [agent connection flags]
witself vault key enroll status ENROLLMENT_ID [agent connection flags]
witself vault key enroll cancel ENROLLMENT_ID [agent connection flags]
witself vault key recovery export --out FILE [agent connection flags]
witself vault key recovery inspect --file FILE
witself vault key recovery import --file FILE [agent connection flags]
witself vault key rotate (--recovery-out FILE|--accept-unrecoverable-key-loss) [agent connection flags]
witself vault key rotation status|cancel [ROTATION_ID] [agent connection flags]
```

`enroll begin` displays its one-time pairing secret only on `/dev/tty`, and
`enroll approve` reads it there with hidden input. Recovery export/import and
artifact-backed rotation read the passphrase with hidden TTY input; export and
rotation require confirmation. `inspect`
is offline and returns public artifact metadata without decrypting or
connecting. No pairing secret or passphrase is accepted through argv,
environment variables, stdin/pipes, JSON, or MCP. The recovery artifact is
never overwritten, is scoped to stable account/realm/agent ids and one exact
AVK identity, and uses fixed Argon2id v1 parameters plus AES-256-GCM. The
parameters are time cost 3, memory 32,768 KiB, parallelism 1, and a fresh
16-byte salt; passphrases are 12 through 1,024 bytes and artifacts are capped at
4 KiB. The backend never receives the artifact, passphrase, path, derived key,
or AVK. `--recovery-out` is the production default: its destination must be an
external or synchronously replicated failure domain before commit. A file on
the same disk is durably written but does not protect against loss of that disk.
`--accept-unrecoverable-key-loss` is a deliberate automation/test escape hatch,
not a production backup policy. Trusted unattended clients use the typed local
recovery-sink service interface and obtain passphrases from their own credential
source; key lifecycle credentials remain absent from MCP and the backend.

`secret create` accepts one strict JSON document through `--file` or `--stdin`
and requires `--idempotency-key KEY`. Reuse that exact key only to retry the
same logical create. Before transport, the client durably journals the exact
generated ids and sealed envelopes under an owner-only scoped hash of the key;
the original journaled request wins, including after a lost response or a
concurrent retry. The raw retry key, sensitive plaintext, and AVK are not stored
in the journal. The MCP `witself.secret.create` tool likewise requires an
explicit `idempotency_key`; neither surface silently generates an undiscoverable
retry identity.
Fields are sensitive unless `sensitive` is explicitly false. A generated
password and a TOTP enrollment are sealed before the HTTP request is sent:

```json
{
  "name": "github",
  "description": "GitHub account for this agent",
  "template": "login",
  "tags": ["github"],
  "fields": [
    {"name": "username", "kind": "username", "sensitive": false, "value": "agent-name"},
    {"name": "login_url", "kind": "url", "sensitive": false, "value": "https://github.com/login"},
    {"name": "password", "kind": "password", "generate_password": true},
    {"name": "totp", "kind": "totp", "otpauth_uri": "otpauth://totp/..."}
  ]
}
```

For unattended use, pipe the document to `witself secret create --stdin` so a
plaintext input file is not left behind. `value_base64` requires `encoding` set
to `binary`. `password_policy` may set `length`, `lowercase`, `uppercase`,
`digits`, `symbols`, and `exclude_ambiguous` when `generate_password` is true.

`list`, `search`, and `show` redact every sensitive field. `reveal` emits one
value only and never includes it in a structured error. The initial create
command has no sensitive-value argv flag; stdin and generation-and-store are
preferred over retaining a plaintext document file.

Password generation uses `crypto/rand`, unbiased selection, at least one
character from every enabled class, a 32-character default, and optional
ambiguous-character exclusion. Passphrases require a vendored, versioned word
list and are not made from an ad-hoc dictionary.

Secret `update`, TOTP enrollment/removal convenience commands, and the
`witself run` reference injection surface remain deferred.

## MCP and Provider Routing

The local stdio MCP process is the custody boundary: it can load the local AVK
and call the remote ciphertext API. The backend never receives key material or
plaintext.

An installed MCP runtime binding must include the same immutable `AccountID`.
Secret tools authenticate and compare that value before any local AVK path is
accessed. Integrations installed by an older Witself version may lack
`account_id`; they fail closed for agent-secret operations and must be
reinstalled with `witself install <runtime>` before those tools can be used.

Initial tools:

- `witself.secret.search` and `witself.secret.show`: redacted reads;
- `witself.secret.create`: local encryption before transport and a redacted
  result;
- `witself.secret.reveal` and `witself.totp.code`: explicit one-field
  value-returning tools; and
- `witself.password.generate`: local generation without backend storage.

The read-only MCP profile retains only secret search and show. The implemented
`--no-value-tools` gate independently removes reveal, password-generation, and
TOTP-code tools from a full profile. AVK enrollment, recovery, and rotation are
intentionally not MCP tools: the CLI keeps pairing secrets and passphrases on
the controlling TTY and keeps lifecycle credentials out of model-visible tool
arguments and results. Secret update, references, and side-effect-oriented
runtime injection are follow-on surfaces. Later filling should prefer a
side-effect-oriented path where a runtime can consume a reference without
putting plaintext in model context.

Witself-owned portable transcript capture holds each content-bearing turn in
the owner-only local outbox until an agent response, stop/failure, session end,
or following user prompt closes it. If the turn invokes `secret.create`,
`secret.reveal`, `password.generate`, or `totp.code`, capture atomically replaces
the still-local prompt and earlier event content with value-free markers and
suppresses every later hook in that turn. This protects the portable Witself
ledger; it cannot erase or attest to provider-native conversation history or
telemetry that the provider may retain after receiving a value-returning tool
result.

Codex, Claude Code, Cursor, and Grok Build receive the same rules:

1. search redacted metadata when a task probably needs a credential;
2. prefer an existing appropriate credential over creating a duplicate;
3. reveal only the exact field needed until reference/injection is available;
4. store newly generated account credentials immediately;
5. never copy a secret into facts, narrative memory, messages, avatars, or
   transcripts;
6. respect human-only gates, service terms, and browser authorization.

## Export and Cloud Portability

The schema-56 archive adds public key metadata, terminal enrollment/rotation
history and receipts, secrets, fields, wrapped DEKs, and secret mutation
receipts in foreign-key order. Export fails while any enrollment is `pending`
or `approved` or any rotation is `open`; it never freezes or serializes an
in-flight transfer/staging capsule. Terminal enrollment transfer fields are
purged, terminal rotation item staging rows are absent, and import rejects a
terminal lifecycle that carries either form of live state.

Irreversible account close and deletion of the affected agent use the same
active-lifecycle conflict fence because either operation revokes the tokens
needed to finish or cancel the work. Realm deletion already requires no live
agents. Enrollment and rotation cancellation remain available while an account
is suspended, so cancel first and then retry export, agent deletion, or close.

The archive carries ciphertext and wrapped DEKs byte-for-byte. It never carries
a local `.key`, raw AVK, target private key, pairing secret, recovery
passphrase, or recovery artifact. The manifest and import allowlist require
every introduced stream, even when empty. The offline recovery artifact is a
separate client-owned file and must be transferred independently.

After import into an AWS, GCP, or Azure cell, an installation must separately
provide the matching AVK by moving its protected local key, importing its
offline recovery artifact, or enrolling from another active installation.
Then it can decrypt unchanged envelopes. Full-text indexes are derived and
rebuilt. No source-cloud key call or re-wrap occurs.

## Implementation Slices

This is the complete product roadmap, not a list of claims about the current
vertical. Each slice is labeled so implemented behavior cannot be confused
with a follow-on release target.

### 0. Contract and collision control — implemented

- Land ADR 0003 and this plan.
- Add supersession notices to conflicting KMS/server-decrypt drafts.
- Begin migrations at `0055` after the completed avatar series.

Exit: one authoritative decrypt owner and no migration collision.

### 1. Local key and crypto foundation — implemented

- Versioned AVK encoding, id/fingerprint, strict parse, generate/load/create.
- Owner-only local path and concurrent-safe first creation.
- AES-GCM field seal/open, DEK wrap/unwrap, canonical AAD, re-wrap.
- Password generator.
- Round-trip, tamper, every-binding-swap, wrong-key, permissions, race, and
  redaction-safe-error tests plus fuzz entry points.

Exit: sensitive bytes round-trip locally and no server component has a key.

### 2. Schema and store — implemented through `0056`

- Migrations `0055` and `0056`, constraints, search indexes, lifecycle tables,
  and migration tests.
- Agent-scoped key registration plus secret create/query/lifecycle store
  methods.
- Idempotency, optimistic concurrency, lifecycle, audit, and usage.
- Integration tests for cross-account/realm/agent isolation and
  ciphertext-only persistence.

Exit: PostgreSQL safely persists complete encrypted vault state.

### 3. API and Go client — implemented

- Authenticated ciphertext-only routes and client types.
- Envelope/size validation and `no-store` value-material responses.
- Contract tests proving no backend plaintext or operator/token-only decrypt.

Exit: local clients manage envelopes through one portable API.

### 4. CLI — implemented

- Key init/status, password generation, create/search/show/reveal, and
  archive/restore are implemented. Update is a follow-on.
- Enrollment begin/approve/complete/list/status/cancel, offline recovery
  export/inspect/import, and recovery-gated rotation/start-resume/status/cancel
  are implemented with the controlling-TTY credential boundary.
- Safe stdin/file inputs, JSON redaction, and actionable missing/wrong-key
  errors are implemented. References are a follow-on.

Exit: an agent can create a GitHub login secret, find it by username, reveal one
field locally, rotate its token without losing the vault, and use an imported
archive in another cell.

### 5. MCP and four runtime adapters — implemented and contract-tested

- Local MCP tools and provider-neutral routing instructions are implemented.
  `--no-value-tools` removes value-returning secret tools independently of
  read-only mode.
- Contract tests for Codex, Claude Code, Cursor, and Grok Build.
- Prove no AVK/value enters hydration, memory, messages, avatars, or transcript
  capture through Witself-owned code. Content-bearing transcript turns remain
  locally gated until their terminal fence, and sealed turns are persisted only
  as value-free markers.

Exit: every supported active client can search, create, and deliberately use
the same vault without backend inference.

### 6. TOTP and runtime injection — TOTP implemented; injection planned

- Strict `otpauth://` parser, Base32 handling, SHA1/SHA256/SHA512 HOTP/TOTP,
  known RFC vectors, and local code generation.
- **Planned:** `witself run` reference resolution and child-only environment
  injection.
- QR decoding remains optional after URI/seed enrollment works.

Exit: an agent can store setup material, generate a valid code locally, and run
a process with a secret without printing it.

### 7. Lifecycle, archive, and operations — lifecycle/archive implemented;
operations evidence planned

- Short-lived enrollment relay, client-only offline recovery, and crash-resumable
  client-driven rotation.
- Export/import streams and strict validators for every sealed and schema-56
  lifecycle table, including active-work export blocking.
- Ciphertext round-trip, wrong/missing-key, cancel/retry, and migration tests.
- Audit query, usage, metrics, retention, backup, and key-loss runbooks.

Exit: encrypted vault state moves losslessly among cells and remains operable
without Witself decrypt access.

### 8. Certification and release — planned

- Four-runtime acceptance using one shared synthetic agent.
- AWS/GCP/Azure PostgreSQL conformance and directed account moves.
- Load tests for public search and bounded envelope reads.
- Security review of logs/errors/audit/archive plus fuzzing of key, envelope,
  reference, `otpauth`, API, and import parsers.

Exit: release evidence covers all 12 runtime/cloud combinations.

## Deferred Slices

Secret update/replacement, grants and group/cross-agent sharing, runtime
reference injection, dedicated TOTP enrollment/removal commands, permanent
secret deletion, attachments, OS keychain/secure-enclave integration,
additional installation proof-of-possession, and browser-native filling remain
separate slices. Versioned key epochs and immutable bindings keep them
additive. Gemini adapter work, Copilot transcript-hook conformance, and the live
four-runtime by three-cloud certification matrix also remain release work, not
backend schema variants.

## Full Product Done Means

These are completion criteria for the whole roadmap. The narrower current
implementation boundary is stated above and does not claim the planned runtime
injection or live runtime/cloud certification items below.

- Backend, database, logs, audit, metrics, support data, and account archives
  contain no AVK or plaintext sealed value.
- Token without AVK cannot decrypt; AVK without token cannot fetch.
- Token issuance/revocation does not alter AVK or ciphertext.
- Sensitive fields are redacted and excluded from memory, facts, avatars,
  hydration, search documents, transcripts, and messages.
- Public username/URL/name/tag search is deterministic and indexed.
- Password generation, TOTP, and process injection happen locally.
- Export/import preserves every envelope across a cloud move.
- Codex, Claude Code, Cursor, and Grok Build pass the same scenarios.
- AWS, GCP, and Azure use the same binaries/schema without vault KMS branches.
- Missing key, wrong key, tampered envelope, stale version, revoked token, and
  unavailable backend all fail closed with value-free errors.

## Related

- [ADR 0003](decisions/0003-client-custodied-agent-vault.md)
- [Secret Model](secret-model.md)
- [Encryption Model](encryption-model.md)
- [Key Hierarchy](key-hierarchy.md)
- [TOTP / 2FA](totp-2fa.md)
- [CLI Command Surface](cli-command-surface.md)
- [MCP Tools](mcp-tools.md)
- [Backup and Recovery](backup-and-recovery.md)
