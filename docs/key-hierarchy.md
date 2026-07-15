# Key hierarchy and decrypt trust boundary

Status: draft. Decision: the Witself sealed plane adopts a single KMS-rooted three-layer
envelope (CMK -> per-realm KEK -> per-secret/per-field DEK) with two custody modes behind
one capability switch. Client-held decrypt stays the default where a client can hold key
material (`client_side_decrypt`); server-mediated decrypt is the capability-gated path for
token-only ephemeral pods (`server_side_decrypt`). This resolves the "Key Hierarchy
Follow-Up" in [encryption-model.md](encryption-model.md) and the open crypto items in
[threat-model.md](threat-model.md). The headline-guarantee revision in [Revises](#revises)
was **approved on 2026-06-26** and propagated into those docs; the remaining sub-decisions
(BYOK-in-v0, per-realm cryptographic isolation, DEK granularity, KEK cache, GDPR posture)
stay in [Open Decisions](#open-decisions).

This doc governs the **sealed plane only** (secrets and TOTP seeds). The open plane
(memories + facts) is ordinary data-at-rest and is never wrapped in this hierarchy. Sealed
material in this hierarchy is **never embedded, never returned by semantic recall, never in
the self-digest, never plaintext-exported, and never ingested** from CLAUDE.md/AGENTS.md;
the only value-returning paths are the audited `witself secret reveal` / `witself totp code`
ceremonies described under [Reconciliation](#reconciliation-with-the-reveal-totp-contract).

## Why pure client-side decrypt is not achievable everywhere

[encryption-model.md](encryption-model.md) makes client-side decrypt the default and states
the managed backend should not see plaintext. That guarantee holds for clients that hold or
derive key material. It does not hold for the dominant managed deployment shape.

[token-lifecycle.md](token-lifecycle.md) and [threat-model.md](threat-model.md) fix the
ephemeral-pod model: a pod's only credential is a mounted durable bearer token
(`witself_at_...`, delivered via `WITSELF_TOKEN_FILE`), with no other key material.
[storage.md](storage.md) puts CMK access behind the `witself-server` deployment IAM identity
(IRSA), which the pod cannot assume. A principal holding only a bearer token therefore cannot
reach KMS to unwrap a DEK and cannot derive a KEK. For token-only pods, something server-side
must call KMS and run the AEAD, or a second key-bearing credential must be injected into every
pod — which reintroduces a custodian and breaks the documented "the token is the only
credential" promise.

The honest conclusion: pure client-side decrypt (Option B) is structurally incompatible with
the token-only-pod UX, and a blanket server-mediated default (Option A) would falsely advertise
`client_side_decrypt` for clients (the CLI) that genuinely can and should decrypt locally.

### Options

| Option | Mechanics | Honest? | Fits token-only pod? | Cost |
| --- | --- | --- | --- | --- |
| A — server-mediated | Server holds KMS-unwrap authority, runs AEAD, returns plaintext | Buildable, but as a global default it weakens the headline guarantee for clients that don't need it | Yes | Server transiently sees DEK + plaintext for every reveal |
| B — client-held-key | Client unwraps DEK and runs AEAD; server returns ciphertext only | True zero-knowledge for values/seeds | No — a token-only pod has no key material to unwrap with | Needs a second credential per pod, breaking the token-only promise |
| C — hybrid, capability-gated (RECOMMENDED) | One envelope; custody mode selected per backend/realm by `client_side_decrypt` / `server_side_decrypt` | Yes — advertises each path as what it actually is | Yes, via the server-mediated path advertised as `server_side_decrypt` | Token-only managed path is the server-side exception run at default frequency |

### Recommended decision

Adopt **Option C**. One envelope, one rotation story, one KMS-loss posture; two custody modes
behind one capability switch:

- Managed token-only-pod path = server-mediated unwrap (Option A mechanics), advertised
  honestly as `server_side_decrypt`. Returns `field.value` + `value_encoding: "plain"`,
  matching the existing reveal contract.
- CLI / local `witself mcp serve` / `witself run` / self-hosted-BYOK path = true
  client-held-key decrypt (Option B mechanics), advertised as `client_side_decrypt`. Returns
  ciphertext + envelope metadata + the material the client needs to obtain the KEK and unwrap
  the DEK (see [Reconciliation](#reconciliation-with-the-reveal-totp-contract)), no plaintext.

Option C is the only choice that is simultaneously honest and buildable. It keeps client-side
decrypt as the real, structurally enforced default exactly where it is achievable, and
reclassifies the token-only-pod managed reveal/TOTP path as precisely the `server_side_decrypt`
exception the docs already define. It fits the ephemeral-pod UX with zero new key plumbing and
reuses `GET /v1/capabilities` discovery verbatim.

The honest cost the owner must accept: for managed token-only realms, the everyday reveal/TOTP
path is the server-side exception run at default frequency, so the server transiently sees the
DEK and plaintext. This is stated plainly here rather than papered over (see
[Residual Risks](#residual-risks)).

## V0 crypto subset

The full design above is the target. **V0 builds only the minimal slice below**; the rest is
deliberately deferred behind the same seams (capability flags, forward-compatible envelope
columns) so it can be added later without a rewrite or data migration of ciphertext.

The sealed plane is a defined v0 slice that MAY be staged after the open-plane core
(memories + facts) ships; see [v0-scope.md](v0-scope.md). KMS is a hard readiness gate only
when the sealed plane is enabled — an open-plane-only deployment does not require it.

A clarifying consequence of the trust-boundary analysis: against a *remote* backend (managed
or self-hosted) a client has no independent KMS access, so **all over-the-wire decrypt in v0
is server-mediated**. True client-held decrypt over the wire is the BYOK feature, which is
deferred. The only client-side decrypt in v0 is **local-dev mode**, where the CLI unlocks a
local passphrase-derived key with no KMS and no server.

**Build in v0:**

- One AEAD envelope per sensitive field and per TOTP seed (`ciphertext`, `nonce`,
  `aead_algorithm`, `dek_id`, `dek_version`, `kms_provider`, `aad_context`), exactly as
  schema'd in [data-model.md](data-model.md). The at-rest envelope is identical whatever the
  reveal mode, so it is not deferrable.
- The `CMK → per-realm KEK → per-secret DEK` hierarchy with `realm_keys` and `secret_deks`
  (per-secret DEK only; per-field DEK is opt-in/deferred). Keeping the KEK layer in v0 is the
  recommended call — retrofitting it later means re-wrapping every DEK.
- Server-mediated reveal/TOTP for managed and self-hosted backends: advertise
  `server_side_decrypt: true` and `client_side_decrypt: false`; the reveal contract returns
  the server-mediated shape (`field.value` + `value_encoding`).
- Local-dev client-side decrypt (local passphrase-derived key wrapping the same KEK/DEK
  structure).
- AWS KMS provider path with the per-realm KEK wrap/unwrap and a smoke/integration test.
- `realm_id` query scoping as the enforced tenant-isolation layer.
- KMS-loss posture and the redaction/audit guarantees for the server-mediated path.

**Defer past v0** (each already an [Open Decision](#open-decisions) or a residual item):

- BYOK / client-held decrypt over the wire — the entire client-held reveal shape (`envelope`
  + `key_material` + `wrapped_kek` delivery) and the client-held step list below. Ship
  `client_side_decrypt: false` on remote backends; the capability flag is the seam to turn it
  on later.
- Per-realm *cryptographic* isolation against the deployment role (least-privilege per-realm
  KMS grants / per-realm CMKs). V0 relies on authorization + query scoping, with the
  tenant-wide blast radius accepted and documented.
- Per-field DEKs, secret version history, and KEK-cache tuning beyond a simple default.

Everything in the Reconciliation, rotation, and residual-risk sections below still describes
the full design; the `client_side_decrypt` column and the client-held step list are the
post-v0 portion.

## Key hierarchy

Three layers, identical across managed and self-hosted, differing only in **where** the root
CMK lives and **who** may call unwrap.

```text
CMK (KMS Customer Master Key)
  └─ wraps ─> per-realm KEK (kek_...)          one per realm, in realm_keys
       └─ wraps ─> per-secret/per-field DEK (dek_...)   wrapped_dek in secret_deks
            └─ AEAD-encrypts ─> field value / TOTP seed (ciphertext)
```

1. **Root CMK.** A KMS Customer Master Key under the `WITSELF_KMS_PROVIDER` abstraction:
   `aws-kms` (ARN via `WITSELF_KMS_KEY_ID`) for managed v0; `gcp-kms` / `azure-key-vault` for
   self-hosted; `local-dev` for tests and `witself-server serve --dev` only. In managed mode
   the CMK lives in Witself's AWS account and is reachable only via the `witself-server`
   deployment IAM identity (IRSA), never by clients; the app never holds raw KMS credentials.
   In self-hosted/BYOK mode the CMK lives in the operator's own KMS/account and the
   operator/client identity calls unwrap.

2. **Per-realm KEK** (`kek_` prefix). A 256-bit symmetric KEK generated per realm, stored
   only KMS-wrapped (`wrapped_kek`) in a `realm_keys` row alongside the realm (`realm_`). The
   KEK is the rotation unit. It is unwrapped via one KMS Decrypt call (server-side in managed
   mode; client/operator-side in BYOK mode) with a KMS request encryption-context (AAD)
   binding the unwrap to `realm_id` + key purpose + the KEK identity/version (`kek_id` /
   `key_version`), so a wrapped KEK from one realm — or an old KEK generation of the same
   realm — cannot be unwrapped in place of the current one.

3. **Per-secret (default) DEK**, with optional per-field DEK for high-value fields and TOTP
   seeds (`dek_` prefix). Each DEK is a 256-bit key used for XChaCha20-Poly1305 (or
   AES-256-GCM) AEAD over the field/seed plaintext. DEKs are stored only KEK-wrapped, in a
   `secret_deks` row (`wrapped_dek`). The wrapped DEK is stored in exactly one place —
   `secret_deks` — and envelopes reference it by `dek_id`; there is no second inline copy to
   diverge on rotation.

KMS wraps the KEK only; the KEK wraps the DEKs; KMS is never on the per-field hot path except
transitively through the (cacheable, zeroized) unwrapped KEK. Per-field reveal grants
(`--reveal FIELD`) remain authorization-layer checks, not separate crypto boundaries, unless
an operator opts a field into its own DEK.

Local-dev degrades cleanly: the CMK role is played by an Argon2id-derived key (from
`WITSELF_PASSPHRASE_FILE`) wrapping the same KEK/DEK structure, so the hierarchy is one model
end to end. Local dev is not the production security model.

## Per-ciphertext envelope

Each encrypted sensitive field value or TOTP seed is stored as an envelope. These are at-rest
columns next to `ciphertext`, not public response fields (the public contract still redacts by
default per [json-contracts.md](json-contracts.md)). Open-plane memory/fact rows carry no such
envelope — they are stored as ordinary plaintext data-at-rest and have no reveal ceremony.

The canonical wrapped DEK and its current wrapping-KEK pointer live on the `secret_deks` row
(referenced by `dek_id`), NOT inline on the envelope. The envelope records only what is frozen
at encryption time plus the `dek_id` join. This is the critical correctness rule for rotation:

- The **wrapping KEK is resolved through `secret_deks.kek_id`** — the post-rotation pointer,
  updated in place when a DEK is re-wrapped under a new KEK. It is NEVER resolved by joining a
  frozen envelope value (e.g. an old `key_version`) to `realm_keys`, because that value does
  not move when the KEK rotates and would resolve a stale or wrong KEK.
- The envelope's `dek_version` is a **frozen** identifier recorded for AAD reconstruction and
  `--version` historical reads; it identifies the DEK generation, not the current KEK.

| Field | Description |
| --- | --- |
| `ciphertext` | AEAD ciphertext of the field value or TOTP seed (binary; base64 for transport only, never a plaintext column). Stored inline in Postgres within the 64 KiB sensitive-field / 256 KiB total inline limits; oversized blobs move to encrypted object/blob storage. |
| `nonce` | Per-encryption random nonce/IV (24 bytes XChaCha20-Poly1305, 12 bytes AES-GCM). Unique per `(DEK, encryption)`; never reused. |
| `aead_algorithm` | AEAD algorithm id, e.g. `"XCHACHA20_POLY1305"` or `"AES_256_GCM"`. Lets clients select the primitive and enables migration. (Named `aead_algorithm` to stay distinct from the non-sensitive TOTP hash `algorithm` column.) |
| `dek_id` | Stable id (`dek_` prefix) of the `secret_deks` row holding the canonical `wrapped_dek` and the current `kek_id`. Fields sharing a per-secret DEK reference one row; rotation targets the DEK by this id. The single unwrap input — reveal reads the wrapped DEK from `secret_deks` via this join. |
| `dek_version` | **Frozen** DEK generation in force when this blob was written. Recorded for AAD reconstruction and `--version` historical reads. Old ciphertext keeps its old `dek_version` after rotation. Does NOT identify the current wrapping KEK (that is `secret_deks.kek_id`), and is distinct from the row's optimistic-lock/version column used for `conflict` (HTTP 409) and from the per-realm `realm_keys.key_version`. |
| `kms_provider` | Provider for the root unwrap (`aws-kms` \| `gcp-kms` \| `azure-key-vault` \| `local-dev`), recorded so backups/restore and multi-provider self-host resolve the right CMK. Surfaces as the `kms_provider` metric label. |
| `aad_context` | Authenticated associated data (integrity-bound, not encrypted), bound from **stable identifiers only**: `realm_id`, `secret_id`, field name, `owner.kind` (`agent` \| `group`), `dek_id`, and a domain tag (`"secret-field"` vs `"totp-seed"`). It deliberately excludes any rotating counter (no KEK/realm `key_version`), so KEK rotation provably cannot change the AAD. Binds ciphertext to its logical slot so a blob cannot be replayed into another field/secret/realm. |

The wrapped DEK and KEK pointer are carried on `secret_deks`, not per envelope:

| `secret_deks` field | Description |
| --- | --- |
| `wrapped_dek` | The per-secret/per-field DEK wrapped by the per-realm KEK — the **only** stored copy. In client-held mode the client unwraps it after obtaining the KEK; in server-mediated mode the server unwraps it. |
| `kek_id` | Id (`kek_` prefix) of the `realm_keys` KEK that **currently** wraps this DEK. Updated in place on KEK re-wrap; this is the authoritative join to `realm_keys` for unwrap. |
| `kms_key_ref` (on `realm_keys`) | Reference to the root CMK that wraps the KEK (AWS KMS key ARN / GCP / Azure resource id). Stored at the `realm_keys`/KEK level, not per field; carried in backups as KMS key identity + rotation metadata, never key material. |

**AAD invariant.** AAD is computed once at encryption and reconstructed at decrypt **strictly
from the stored envelope columns** (the frozen `dek_id`, field name, `owner.kind`, and domain
tag on that row), never from any current/live realm key state. Because the AAD contains no
rotating counter, KEK rotation cannot alter it and cannot silently break decrypt.

`value_encoding` (`"plain"` \| `"base64"` \| `null`) is a public
[json-contracts.md](json-contracts.md) field on the **client-facing** reveal/show response
only — the decrypted value's encoding on a reveal result, or `null` with `redacted: true` and
`value_ref` on ordinary show. It is NOT an at-rest envelope column; base64 there is
serialization, not a security boundary.

Schema implications for [storage.md](storage.md) and [data-model.md](data-model.md): a
`realm_keys` table (per-realm `wrapped_kek`, `key_version`, `kms_provider`, `kms_key_ref`,
rotation metadata), a `secret_deks` table (canonical `wrapped_dek`, `kek_id`, `dek_` id, DEK
generation), and per-field envelope columns (`ciphertext`, `nonce`, `aead_algorithm`,
`dek_id`, `dek_version`, `kms_provider`, `aad_context`) alongside `ciphertext`. Goose
migrations introduce these tables and columns.

## Rotation, re-wrap, and backfill

Two independent rotation tracks over a versioned key model, both deliberate and recorded.

**DEK rotation (cheap, common, lazy/on-write).** On any write/update of a secret/field, mint a
fresh DEK, AEAD-encrypt with it, wrap under the current KEK, and store a new current
`secret_deks` row with an incremented DEK generation; the new envelope records the matching
frozen `dek_version`. Prior secret versions keep their old `ciphertext` and their
prior-generation `secret_deks` row under their own `dek_version`, so historical reads
(`--version`) still decrypt. No backfill needed.

**KEK rotation (per-realm, the real rotation unit).** Create a new KEK version, wrap it under
the current CMK, mark it current in `realm_keys`, and bump the realm `key_version`. Existing
DEKs were wrapped under the OLD KEK; they are **re-wrapped** (DEK plaintext unwrapped under the
old KEK, re-wrapped under the new KEK) WITHOUT touching ciphertext. The re-wrap updates each
affected `secret_deks` row's `wrapped_dek` **and** repoints its `kek_id` at the new KEK row — a
single write per DEK. **Envelope rows are not touched** (they resolve the current KEK through
`secret_deks.kek_id`), so there is no two-copy divergence and no stale envelope pointer. This
is a metadata-only backfill that never re-encrypts secret values and never exposes plaintext
secrets (only DEKs transit, key-on-key). KEK rotation emits the `key.rotated` audit event.

- Managed / server-mediated mode: re-wrap is a `witself-server` background job; it already
  holds KEK-unwrap authority. Each DEK re-wrap is one transaction on its `secret_deks` row.
- Client-held / BYOK mode: the server cannot unwrap, so KEK rotation/re-wrap is an
  operator-driven online-client operation. This is a BYOK cost that must be called out.

**CMK rotation.** Use native KMS key rotation with retained prior key versions. Only the
KEK-wrapping layer is affected; old KEK versions remain unwrappable until re-wrapped under the
new CMK material, tracked by `kms_key_ref` + rotation metadata in backups.

Throughout, the frozen `dek_version` (envelope/DEK generation) and the per-realm
`realm_keys.key_version` (KEK generation) are kept strictly distinct from the secret row's
optimistic-lock/version column used for conflict detection (`conflict` / HTTP 409 / stale
version). Rotation status is observable via `witself_kms_operations_total` and the migration
metrics (`witself_migration_version`, `witself_migration_pending`).

## KMS-loss posture

Uniform, centralized, and intentionally unforgiving — the posture
[encryption-model.md](encryption-model.md), [storage.md](storage.md), and
[backup-and-recovery.md](backup-and-recovery.md) already mandate, made concrete.

Because every secret roots in CMK -> KEK -> DEK, loss of the CMK (deletion,
scheduled-deletion, disabled key, or withdrawn key policy/IAM) makes every per-realm KEK
unwrappable, hence every DEK unwrappable, hence all affected ciphertext and TOTP seeds
permanently unrecoverable. This holds in both custody modes, since both depend on unwrapping
the same envelope. **This is a sealed-plane-only loss**: it does NOT affect the open plane —
memories and facts are ordinary data-at-rest and survive CMK loss untouched.

- There is deliberately **no managed break-glass decrypt**. Managed recovery restores service
  availability, operator account access, and encrypted data only — never plaintext.
- Server-mediated managed mode: loss surface is one CMK + one deployment IAM identity — simple
  to reason about, but a single point of total sealed-plane data loss.
- Client-held / BYOK mode: an additional independent loss point — the operator-held key or
  passphrase. If the operator loses it, Witself cannot recover those realms, by construction.

**Crypto-shredding covers secret values only, not metadata/PII.** CMK loss (or deliberate
destruction on account closure) renders secret values and TOTP seeds unrecoverable, but it
does NOT erase the metadata and PII retained elsewhere — secret names, usernames/URLs/issuers,
and the audit/operator PII held under the 365-day retention window. It also does not touch the
open plane. Account-deletion vs erasure of that PII is handled in
[data-model.md](data-model.md) (Account Deletion, PII, And Erasure) and is a separate,
metadata-level action.

Backups carry KMS key identity + rotation metadata + `kms_provider` / `kms_key_ref` +
migration version + server config to reconnect — never key material, raw KMS credentials, or
plaintext. Secrets are **never in the plaintext export**; the only secret backup is the
encrypted envelope plus KMS key identity, per [backup-and-recovery.md](backup-and-recovery.md).

Required mitigations to document and ship: enable KMS key rotation with retained versions; use
deletion windows with pending-deletion CloudWatch alarms; consider multi-region CMK replication
for managed; require explicit operator consent at BYOK realm creation with prominent CLI/UI
warnings; for BYOK push operators toward multi-region CMK / key-policy backups / passphrase
escrow the operator controls. This strengthens, and does not contradict, the already-accepted
"KMS loss = possible permanent loss" decision.

## Per-realm tenant isolation

The realm (`realm_`) is the single isolation, billing, and key-separation scope, enforced at
three layers that must agree. The crypto-layer claim is stated precisely to avoid overclaiming
what a single shared CMK can guarantee.

1. **Cryptographic (blob confusion only).** One KEK per realm (`kek_` in `realm_keys`). A DEK
   from realm A is wrapped under A's KEK and cannot be unwrapped by B's KEK. The KMS unwrap of
   each KEK carries an encryption-context/AAD pinned to `realm_id` + purpose + KEK
   identity/version, so a wrapped KEK blob from one realm (or an old generation) cannot be
   *confused* for another's — it will not unwrap under a different context. **This does NOT,
   under the v0 single-CMK + single deployment-IAM-identity model, prevent that one role from
   unwrapping any realm's KEK.** The encryption-context is non-secret (it is just `realm_id` +
   purpose + `kek_id`, recorded in `realm_keys`), so a holder of the deployment role can supply
   any realm's own context and unwrap that realm's KEK. Cross-realm isolation against a
   compromised server/role is therefore **authorization/operational, not cryptographic**, with
   a tenant-wide blast radius (see [Residual Risks](#residual-risks)). Making the cryptographic
   claim true would require promoting least-privilege per-realm KMS grants /
   encryption-context-constrained grants / per-realm CMKs (currently an
   [Open Decision](#open-decisions)) from optional to a stated mechanism; until then, per-realm
   cryptographic isolation against the role is **deferred**.

2. **Authorization.** The shared authorization layer below CLI/MCP/API resolves the bearer
   token (`tok_` / `witself_at_`) to exactly one `(realm, named agent)`. Per-agent default
   isolation means an agent sees only its own (`owner.kind: "agent"`) secrets plus explicit
   grants; group-owned (`owner.kind: "group"`) access is an explicit auditable grant, never
   default; cross-agent and operator access require a grant/role plus an audit `reason`.
   Secret-name uniqueness is enforced per `(realm_id, owner_agent_id, name)` for agent-owned
   secrets and per group scope for group-owned secrets, via a sentinel/nullable
   `owner_agent_id` + `owner_group_id`. The unified owner model is `owner ∈ {agent, group}`
   across all data types; see [authorization-and-roles.md](authorization-and-roles.md).

3. **Query scoping.** Every secret/field/TOTP/grant/audit/usage row carries `realm_id` and
   every query is `realm_id`-scoped in one shared data model (no per-tenant schema in v0).
   `realm_id` never leaks into high-cardinality metric labels. This is the enforced isolation
   layer in v0.

Server-mediated managed decrypt does NOT weaken tenant isolation against ordinary co-tenants:
the server only ever unwraps the KEK for the realm the authenticated token resolves to, gated
by the same authorization layer. What it changes is the operator/server-compromise boundary,
addressed in [Residual Risks](#residual-risks). Managed (multi-tenant, `realm_id`-scoped shared
tables) and self-hosted (typically single-tenant) share the identical model — settling the
[threat-model.md](threat-model.md) open question by making the key hierarchy structurally
identical and the trust posture the configurable part.

## Reconciliation with the reveal / TOTP contract

The two custody modes map onto the existing contracts without new endpoints. Mode is
discoverable per backend/realm via `GET /v1/capabilities` (`client_side_decrypt` /
`server_side_decrypt`); the reveal route stays `POST /v1/secrets/{secret_id}:reveal` and TOTP
stays `POST /v1/totp/{secret_id}:code`. These are the sealed plane's only value-returning ops;
the open plane (memories/facts) has no reveal — identity data is plainly readable per its own
scopes, with sensitive facts using lightweight redaction, not this ceremony.

V0 scope note: over the wire, v0 ships only the `server_side_decrypt` column below (remote
backends advertise `client_side_decrypt: false`); the `client_side_decrypt` column and the
client-held step list are the post-v0 BYOK path (see [V0 Crypto Subset](#v0-crypto-subset)).
Local-dev mode decrypts locally but does so without a server or KMS, so it does not use the
over-the-wire client-held contract.

| Aspect | `client_side_decrypt` (default) | `server_side_decrypt` (exception) |
| --- | --- | --- |
| Who unwraps DEK / runs AEAD | client (CLI, `mcp serve`, `witself run`, BYOK operator) | `witself-server` via deployment IAM identity |
| Reveal response | `ciphertext` + envelope metadata (`nonce`, `aead_algorithm`, `dek_version`, `dek_id`, `aad_context`) + the wrapped DEK and KEK-delivery material below; no plaintext | `field.value` + `value_encoding: "plain"` (current shape) |
| KEK delivery (client-held) | `wrapped_dek` + `wrapped_kek` + `kms_key_ref` + the KMS encryption-context inputs (`realm_id` + purpose + `kek_id`/`key_version`), either inline on the reveal response or fetched once via a capabilities/key-material endpoint keyed by `kek_id` and cached | n/a (server holds KEK-unwrap authority) |
| TOTP `code` response | client fetches/holds the same KEK material, decrypts the seed, generates code | server decrypts seed, returns generated `code` |
| Server sees plaintext? | no | yes, transiently |
| Always present | `audit_event_id`, honors `expires_at` | `audit_event_id`, honors `expires_at` |

**Client-held decrypt step list (the load-bearing path).** Because the client holds only a
bearer token, the contract must deliver the material to reach the KEK, or the wrapped DEK is
unusable. The default client-side path is:

1. `POST /v1/secrets/{secret_id}:reveal` (or `:code`) returns `ciphertext`, `nonce`,
   `aead_algorithm`, `dek_id`, `dek_version`, `aad_context`, and `wrapped_dek`.
2. The client obtains the KEK material for the resolved realm: `wrapped_kek`, `kms_key_ref`,
   and the exact KMS encryption-context fields (`realm_id` + purpose + `kek_id`/`key_version`)
   — returned inline on the reveal response or fetched once via a key-material endpoint keyed
   by `kek_id` and cached.
3. KMS Decrypt(`wrapped_kek`, encryption-context) → unwrapped KEK (BYOK: the client/operator
   identity holds this KMS authority).
4. unwrap(`wrapped_dek`) under the KEK → DEK.
5. Reconstruct AAD from the stored envelope fields (per the
   [AAD invariant](#per-ciphertext-envelope)) and AEAD-open(`ciphertext`, `nonce`,
   `aad_context`) → plaintext, decoded per `value_encoding`.

The TOTP client-side path is identical through step 4, then decrypts the seed and generates the
code locally.

The `server_side_decrypt` path remains the narrow exception
[encryption-model.md](encryption-model.md) defines: disabled unless advertised; per-operation
(`:reveal` / `:code`, never a generic decrypt endpoint); requires a specific permission/policy
grant (`secret:reveal`, `totp:code`); emits an audit event (`secret.reveal` / `totp.code` with
the `server_side_decrypt` flag) identifying actor/target/purpose/outcome without plaintext;
requires an audit `reason` for operator/admin and cross-agent use; redacts plaintext from
logs/errors/analytics/support/audit; and is distinguishable in API/CLI/MCP/audit. The MCP
`--no-value-tools` switch disables `secret.reveal` / `totp.code` / value-returning
`reference.resolve` entirely; `--read-only` disables mutations. Both paths still emit
`witself_secret_reveals_total`.

The intellectually honest reclassification: for managed token-only realms the
`server_side_decrypt` exception is the everyday path, run at default frequency, not a rare
break-glass. The guarantee for that path narrows to: plaintext is transient, never persisted,
never logged, redacted from audit/errors/analytics, always audited.

## Revises

These items change stated defaults in existing docs. **Approved on 2026-06-26** and propagated
into the docs named below.

- **[encryption-model.md](encryption-model.md) — headline guarantee.** The literal claim that
  the default managed backend never sees plaintext passwords, API keys, TOTP seeds, or codes is
  no longer true for the token-only ephemeral-pod path, which is the dominant managed shape.
  Reframe: client-side decrypt remains the structurally enforced default for clients that
  hold/derive key material (CLI, local `mcp serve`, `witself run`, local-dev-after-unlock,
  self-hosted/BYOK). For managed token-only pods, reveal/TOTP run over `server_side_decrypt`;
  the server transiently sees the DEK and plaintext in memory, and the guarantee narrows to
  "plaintext is transient, never persisted, never logged, redacted, always audited." Finalize
  the "Key Hierarchy Follow-Up" section with the CMK -> KEK -> DEK hierarchy, envelope fields,
  and rotation mechanics above.

- **[api-contract.md](api-contract.md) / [json-contracts.md](json-contracts.md) — honest
  capability advertisement.** Managed token-only backends MUST advertise
  `server_side_decrypt: true` (and MAY advertise `client_side_decrypt: false` for that path)
  rather than implying client-side decrypt everywhere. The reveal contract supports both shapes
  selected by capability: server-mediated returns `field.value` + `value_encoding: "plain"`;
  client-held returns ciphertext + envelope metadata + the KEK-delivery material
  (`wrapped_dek`, `wrapped_kek`, `kms_key_ref`, encryption-context) and no plaintext value.
  This extends the current Secret Reveal Result shape, which has no such fields today — owner
  approval of that json-contracts.md extension is required before either doc is marked
  non-draft, since [data-model.md](data-model.md)'s envelope columns assume it. Both shapes
  honor `audit_event_id` and `expires_at`.

- **[storage.md](storage.md) — schema.** Add the `realm_keys` and `secret_deks` tables and the
  per-secret/per-field envelope columns; document the KMS encryption-context (AAD) binding
  unwrap to `realm_id` + purpose + KEK identity/version; confirm KMS wraps the KEK (not per
  field), that the wrapped DEK has a single source of truth in `secret_deks`, and that
  server-side unwrap authority exists only via the deployment IAM identity in managed mode.
  Keep ordinary PostgreSQL as the open-plane hard gate; migration-0032 JSONB
  vectors need no extension, and KMS gates only the sealed plane.

- **[threat-model.md](threat-model.md) — open items.** Envelope format, key hierarchy,
  rotation, tenant/realm separation, and managed-vs-self-hosted parity are settled as one
  identical CMK -> KEK -> DEK hierarchy with configurable root custody. Add the explicit
  statement that managed token-only-pod reveal expands the plaintext TCB to include
  `witself-server` + its KMS IAM identity, that per-realm cryptographic isolation against that
  role is deferred (single CMK + single role gives a tenant-wide blast radius), and enumerate
  the operator/server-compromise mitigations. The dual posture (sealed-plane confidentiality +
  open-plane integrity/authenticity) is the threat-model frame.

- **[requirements.md](requirements.md) — framing.** Soften the blanket "prefer client-side
  decrypt" / "managed backend never sees plaintext" framing to "client-side decrypt where the
  client can hold key material; capability-gated server-side decrypt for token-only managed
  pods," keeping the same-domain-logic / same-data-model mandate intact since only key custody
  differs by mode. Record the sealed-plane invariants (never embedded/recalled/in-digest/
  plaintext-exported/ingested) in the master decision log.

## Residual risks

- Managed token-only-pod reveal/TOTP runs the server-side path at default frequency, so the
  plaintext TCB for those realms expands from the client runtime to the entire `witself-server`
  tier plus its KMS-capable deployment IAM identity. A compromised server process, memory
  scrape, heap/coredump, malicious build, or insider with the IAM role can observe plaintext
  for any reveal flowing through it, and — because v0 uses a single CMK + single deployment role
  whose KMS encryption-context is non-secret — can unwrap any reachable realm's KEK by supplying
  that realm's own context: a tenant-wide blast radius. This is the operational reality the
  Per-Realm Tenant Isolation section's cryptographic claim is deliberately scoped against.
- For server-mediated realms the "no managed break-glass" and "staff have no ordinary plaintext
  access" guarantees become policy- and audit-enforced rather than architecture-enforced. The
  cryptographic barrier no longer exists; only discipline does (tight IAM, no prod
  debug/coredump, DEK zeroization, tamper-evident audit, Decrypt alerting).
- Server-side TOTP for token-only pods means the server sees the seed AND the generated code —
  the highest-value assets. Real exposure, mitigated only by transience and redaction.
- Caching the unwrapped KEK in server memory to avoid per-reveal KMS latency widens the
  plaintext-KEK exposure window — a direct tension between performance and exposure.
- BYOK/client-held mode does not give token-only pods a no-server-plaintext path (a pod with
  only a bearer token cannot unwrap); BYOK is realistically only for CLI/operator/self-hosted
  clients. Operators may wrongly believe their pods are zero-knowledge when they are on the
  server-mediated default.
- Two reveal code paths double the security-critical surface, and the mode branch is invisible
  in a casual reveal response. `GET /v1/capabilities` is the mitigation but is coarse
  (per-backend/per-realm, not per-secret).
- BYOK KEK rotation/re-wrap must be operator-driven and online-client, harder to guarantee
  fleet-wide than a backend job.
- Metadata (secret names, usernames, URLs, issuers, labels, access patterns, audit trail)
  remains visible to Witself in all modes; even BYOK is zero-knowledge for values/seeds only,
  not metadata. Customers may overestimate "end-to-end." Account deletion does not crypto-shred
  this metadata/PII (see KMS-Loss Posture and [data-model.md](data-model.md)).

## Open decisions

Resolved on 2026-06-26 (propagated into the docs under [Revises](#revises)):

- ✅ The headline-guarantee revision is accepted: [encryption-model.md](encryption-model.md)'s
  "default managed backend never sees plaintext" is now false for token-only-pod reveal and is
  replaced by the narrower transient/redacted/audited promise, to be communicated honestly in
  docs and any zero-knowledge marketing.
- ✅ Managed token-only backends advertise `server_side_decrypt: true` (with the matching
  `/v1/capabilities` semantics), accepting that this path runs at default frequency for the
  dominant deployment shape.
- ✅ The [json-contracts.md](json-contracts.md) Secret Reveal Result extension that adds the
  client-held shape (`ciphertext` + envelope metadata + `wrapped_dek` + `wrapped_kek` /
  `kms_key_ref` / encryption-context) is approved and documented.

Still open — owner sign-off items:

- Decide whether BYOK/client-held mode ships in v0 as an opt-in high-assurance realm tier or is
  deferred post-v0 (it adds operator-driven rotation burden, a second KMS-loss point, and
  forecloses future managed server-side automation for those realms).
- Decide DEK granularity policy: per-secret DEK as default, with opt-in per-field DEK for TOTP
  seeds / highest-value fields — and whether per-field reveal grants ever imply per-field DEKs
  or remain pure authorization checks.
- Set the server-mediated KEK-cache policy: cache unwrapped KEKs in server memory (lower KMS
  latency/cost, wider exposure window) vs per-reveal KMS Decrypt (higher cost, narrower
  window), including TTL and zeroization rules.
- Confirm capability granularity: is the `client_side_decrypt` / `server_side_decrypt` choice
  per-backend, per-realm, or finer — and whether a single deployment may mix token-only
  (server-mediated) and CLI (client-side) clients against the same realm (recommended: yes,
  since the mode follows the client's capability, not the realm).
- Approve the managed server-compromise hardening bar as release-gating, and decide whether
  per-realm cryptographic isolation is promoted from deferred to v0: enforce least-privilege
  per-realm KMS grants / encryption-context-constrained grants (or per-realm CMKs) to make the
  cryptographic isolation claim true against the deployment role, plus CloudTrail Decrypt
  alerting, no prod debug/coredump, DEK/KEK zeroization, tamper-evident audit — i.e. accept
  these as required, not optional, given the expanded TCB and tenant-wide blast radius.
- Confirm the GDPR / account-deletion posture cross-referenced from
  [data-model.md](data-model.md): crypto-shredding handles secret values only; decide whether
  audit/operator PII is retained up to 365 days under the security/legal-hold exception
  (accepted gap) or purged/anonymized on account closure.
- Sign off on the strengthened BYOK KMS-loss consent UX (explicit consent at realm creation,
  escrow/multi-region guidance) given there is no Witself-side recovery for client-held keys.

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
