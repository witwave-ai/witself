# Client-Custodied Agent Vault

Status: authoritative implementation plan. ADR 0003 is accepted. The initial
end-to-end agent-owned vertical is implemented; the explicitly listed follow-on
slices remain planned. Last reviewed 2026-07-18.

This document turns Witself's agent-secrets product into buildable slices. It is
the authoritative custody and delivery contract wherever an older sealed-plane
draft still describes KMS-rooted keys or server-side decryption.

## Outcome

A named Witself agent can keep structured credentials that follow the agent
across supported AI products, machines, and deployment cells. The backend is a
portable ciphertext store plus deterministic authorization, audit, and search.
Only an active client with both an authorized agent token and the agent's
separate vault key can turn a sensitive envelope into plaintext.

The first runtime set is Codex, Claude Code, Cursor, and Grok Build. Gemini and
GitHub Copilot remain future adapters. The backend and archive format behave the
same on AWS, Google Cloud, and Azure.

## Current Implementation Boundary

The first working vertical includes:

- migration 0055 with agent vault-key bindings, structured secrets, fields,
  wrapped DEKs, and idempotency receipts;
- client-only AVK generation, owner-only local key files, AES-256-GCM field
  envelopes, password generation, TOTP parsing, and TOTP calculation;
- agent-token HTTP routes for current-key status/registration, create,
  redacted list/search/show, one-field encrypted material access, archive, and
  restore;
- CLI and MCP access to that same client-side custody path; and
- account export/import of public key metadata and byte-identical encrypted
  vault state, never the AVK.

The following are deliberately not claims of this vertical: AVK transfer to a
second installation, AVK rotation/DEK rewrap, secret update, dedicated TOTP
enroll/delete convenience commands, runtime injection, cross-agent grants or
group ownership, permanent secret deletion, and the live four-runtime by
three-cloud certification matrix. Those are follow-on slices and must not be
inferred from older target-contract documents.

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
10. Multi-machine AVK enrollment is required, but its transport ceremony is a
    separate slice. The envelope and schema must not assume one installation.

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

### Planned rotation

This contract is reserved for the follow-on rotation slice; the initial
vertical does not expose update, rotation, or re-wrap operations. Updating a
sensitive value will create a new DEK generation and ciphertext. AVK rotation
will create a new AVK version and locally unwrap/re-wrap current DEKs without
decrypting field values. The backend will swap wrapped-DEK material under exact
row versions and flip the current key epoch only after the complete set
validates. Previous local AVK material remains available until verification.

## Local Custody

```text
~/.witself/
  tokens/accounts/<account>/realms/<realm>/agents/<agent>.token
  keys/accounts/<account>/realms/<realm>/agents/<agent>.key
```

`WITSELF_HOME` overrides the root. The key file contains a versioned opaque
record with schema, random `avk_` id, key version, algorithm, and 32-byte key.
It is portable enough for an eventual keyboard enrollment flow but ordinary
commands print only id, fingerprint, algorithm, and version.

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

Avatar migrations now occupy `0050` through `0054`. The vault begins at
`0055`; no lower-numbered migration may be introduced after deployed cells have
advanced to schema 54.

### `agent_vault_keys`

One public row per agent key epoch:

- `id`, `account_id`, `realm_id`, `owner_agent_id`;
- `key_version`, `algorithm`, `fingerprint`, `lifecycle_state`;
- `created_at`, `retired_at`, and `row_version`.

There is no key-material or wrapped-key column. Exactly one current epoch is
allowed per live agent. Registration is idempotent and rejects a different id
or fingerprint for an already-current version.

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

The unmarked routes below are implemented in the initial vertical. Routes
marked **planned** freeze the intended shape for a later slice and are not
registered by the current server.

All agent routes derive account, realm, and owner agent from the bearer token.
The first slice accepts no caller-supplied owner override.

- `POST /v1/vault/key-epochs` registers public current-key metadata.
- `GET /v1/vault/key-epochs/current` returns public key status.
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
- **Planned:** `POST /v1/vault/key-epochs/{version}:rewrap` atomically replaces
  a bounded batch of wrapped DEKs under exact guards.

Every mutation requires `Idempotency-Key`. Bodies use the `witself.v0` contract
and carry explicit algorithms and versions. The server validates size, ids,
branch shape, algorithm allowlists, scoped relationships, optimistic versions,
and quota. It cannot validate plaintext or claim decryption success.

## CLI

The implemented command surface is:

```text
witself vault key init|status
witself password generate
witself secret create|list|search|show|reveal|archive|restore
witself totp show|code SECRET FIELD
```

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

The planned `update`, TOTP enrollment/removal convenience commands, and
`witself run` reference injection surface are not part of the initial vertical.

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
TOTP-code tools from a full profile. Update, lifecycle, references, and
side-effect-oriented runtime injection are follow-on surfaces. Later filling
should prefer a side-effect-oriented path where a runtime can consume a
reference without putting plaintext in model context.

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

The archive adds key metadata, secrets, fields, DEKs, and mutation receipts in
foreign-key order. It carries ciphertext byte-for-byte and never local AVK
material. The manifest and import allowlist require every introduced table,
even when empty.

After import into an AWS, GCP, or Azure cell, an installation holding the same
AVK can decrypt unchanged envelopes. Full-text indexes are derived and rebuilt.
No source-cloud key call or re-wrap occurs.

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

### 2. Schema and store — implemented

- Migration `0055`, constraints, search indexes, and migration tests.
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
- `witself run` reference resolution and child-only environment injection.
- QR decoding remains optional after URI/seed enrollment works.

Exit: an agent can store setup material, generate a valid code locally, and run
a process with a secret without printing it.

### 7. Archive and operations — archive implemented; operations evidence planned

- Export/import streams and strict validators for every sealed table.
- Ciphertext round-trip and wrong/missing-key tests.
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

Multi-device enrollment, recovery packages, group/cross-agent sharing,
attachments, installation proof-of-possession, and browser-native filling each
receive a separate threat model and acceptance suite. Versioned key epochs and
immutable bindings keep them additive.

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
