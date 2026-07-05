# Witself Data Model (Postgres Schema)

Status: draft. Last reviewed 2026-06-26. Decision: Witself uses a single
multi-tenant Postgres schema (with the `pgvector` extension) as the system of
record, scoped on every row by `account_id` / `realm_id`, spanning **two
planes** — the **open plane** (memories + facts, stored as ordinary identity
data) and the **sealed plane** (secrets + TOTP, stored only as AEAD envelopes,
never plaintext columns). Tokens are stored only as hashes, optimistic
concurrency is via a `row_version` column, soft delete via tombstone timestamps,
and Goose expand/contract migrations are owned by `witself-server`.

This doc is the prerequisite for the first Goose migration and the
authorization/storage layers. It is the relational view of the domain objects in
[memory-model.md](memory-model.md), [facts-model.md](facts-model.md),
[access-policy.md](access-policy.md), [security-groups.md](security-groups.md),
[inter-agent-messaging.md](inter-agent-messaging.md), and the secret/TOTP
shapes; it maps column-for-column onto [json-contracts.md](json-contracts.md),
realizes the sealed-plane key implications of [key-hierarchy.md](key-hierarchy.md)
and [encryption-model.md](encryption-model.md), and shares the storage
responsibilities in [storage.md](storage.md).

## Two Planes, One Schema

The schema holds two kinds of payload in one multi-tenant store. The
distinction is load-bearing for every read, encryption, export, and
recall/digest rule:

- **Open plane** (`memories`, `facts`, plus `policies`, `security_groups`,
  `group_members`, `messages`): identity content lives in **ordinary columns**,
  protected by data-at-rest encryption (RDS/disk) for integrity and
  authenticity — *not* secret confidentiality. Open-plane content is
  semantically indexed (pgvector), recallable, cross-agent readable under
  [policy](access-policy.md), in the self-digest, and plaintext-exportable. There
  is **no reveal ceremony**; `sensitive` is a PII/redaction display flag, not an
  encryption boundary.
- **Sealed plane** (`secrets`, `secret_fields`, `totp_enrollments`,
  `secret_grants`, `realm_keys`, `secret_deks`, `attachments`): sensitive values
  and TOTP seeds live **only in envelope columns** (CMK → per-realm KEK →
  per-secret/field DEK; see [key-hierarchy.md](key-hierarchy.md)). Sealed-plane
  material is reveal-gated and is **never embedded, never returned by semantic
  recall, never in the self-digest, never plaintext-exported, and never ingested
  from CLAUDE.md/AGENTS.md** (the sealed-plane carve-out; see
  [memory-model.md](memory-model.md), [context-hydration.md](context-hydration.md),
  and [backup-and-recovery.md](backup-and-recovery.md)).
- **Shared spine** (`accounts`, `operators`, `realms`, `realm_members`, `agents`,
  `agent_tokens`, `audit_events`, `usage_counters`, `idempotency_keys`): one
  account/realm/agent model, token = identity, audit, metering, and idempotency
  across both planes.

Sealed-plane envelope columns and `realm_keys` / `secret_deks` are needed only
when the sealed plane is enabled. An **open-plane-only** deployment runs without
KMS; the sealed plane re-introduces KMS as a required dependency when enabled.
`pgvector` is a hard gate for the open plane; the embedding provider is
degradable (see [storage.md](storage.md)).

**This schema is the per-cell schema.** Under the go-forward fleet of
independent cells, each cell runs one instance of this schema and is the single
writer and source of truth for the tenants placed on it. The **realm/account ->
home-cell routing mapping lives in the thin global control plane, NOT in any
cell's Postgres schema** below. The control plane holds only routing metadata
(realm/account -> home cell + endpoint + realm signing key) and no tenant data —
no memories, facts, secrets, messages, audit, or usage rows ever live in it. The
`realm_handle` and realm signing-key columns on [`realms`](#realms) are the
per-cell copy of a realm's federation identity; the control plane's separate
directory is what routes a client (or the relay) to the realm's home cell. See
[deployment-cells.md](deployment-cells.md) and
[agent-collaboration.md](agent-collaboration.md).

## Scope And Conventions

The schema covers exactly the storage responsibilities enumerated in
[storage.md](storage.md) and [backend-architecture.md](backend-architecture.md):
account/realm/agent metadata, operator principals and memberships, token hashes
and metadata, the open-plane memory/fact/policy/group/message tables, the
sealed-plane secret/TOTP/grant/key tables, audit events, usage counters and
rate-limit state, idempotency records, and deferred-but-stubbed attachment
metadata. Managed (multi-tenant) and self-hosted (typically single-tenant)
deployments share the **same** data model; only key custody, the sealed-plane
toggle, and tenancy density differ.

V0 sequencing: the open plane (memories, facts, policies, groups, messages)
ships first as the product core; the sealed credential plane is a defined v0
slice that MAY be staged after the core (see [v0-scope.md](v0-scope.md) and
[implementation-plan.md](implementation-plan.md)). The at-rest schema below is
defined in full; sealed-plane deferrals — `attachments` (deferred-but-stubbed),
a dedicated `secret_versions` history table (an [Open Decision](#open-decisions)),
and opt-in per-field DEKs (v0 uses a per-secret DEK) — do not change the table
definitions.

Conventions that apply to every table unless stated otherwise:

- **Primary keys** are opaque text ids with a fixed type prefix (see
  [Identifier Prefixes](#identifier-prefixes)). Callers must not parse id internals.
- **Multi-tenant scoping.** Every row below the account carries `account_id`, and
  every row below the realm carries `realm_id`. The shared authorization layer
  below CLI/MCP/API MUST scope every query by the `(account_id, realm_id)`
  resolved from the bearer token; `realm_id`-scoped shared tables are the v0
  isolation model (no per-tenant schema). `realm_id` MUST NOT leak into
  high-cardinality metric labels.
- **Timestamps** are `timestamptz` in UTC. `created_at` is set on insert;
  `updated_at` is bumped on every mutation; both mirror the `created_at` /
  `updated_at` JSON fields.
- **Optimistic concurrency.** Mutable resources carry `row_version bigint NOT NULL
  DEFAULT 1`, incremented on every update (see [Optimistic Concurrency](#optimistic-concurrency-and-history)).
- **Soft delete.** Tombstoned via `deleted_at timestamptz NULL`; rows with non-null
  `deleted_at` are excluded from ordinary reads (see [Soft Delete And Tombstones](#soft-delete-and-tombstones)).
- **Open plane = ordinary columns; sealed plane = envelopes only.** Open-plane
  memory `content` and fact `value` are ordinary queryable columns (data at
  rest). Sealed-plane sensitive field values and TOTP seeds live ONLY in envelope
  columns (see [Sensitive-Field Envelope Storage](#sensitive-field-envelope-storage));
  token values live only as hashes (see [`agent_tokens`](#agent_tokens)). Base64
  is serialization only, never a security boundary.

## Identifier Prefixes

The canonical id prefixes from [json-contracts.md](json-contracts.md), plus the
sealed-plane key-material prefixes from [key-hierarchy.md](key-hierarchy.md). Ids
are opaque; the prefix is a debugging affordance, not a parseable field.
Recommended format is `<prefix><random>` (ULID/UUID body); local-file mode may
generate stable local ids.

| Prefix | Resource | Plane | Table |
| --- | --- | --- | --- |
| `acct_` | customer account | spine | `accounts` |
| `opr_` | human operator principal | spine | `operators` |
| `realm_` | realm (was Witpass vault) | spine | `realms` |
| `rmb_` | realm membership (was vault membership) | spine | `realm_members` |
| `agent_` | named agent | spine | `agents` |
| `tok_` | token (DB id; raw token text uses `witself_at_`) | spine | `agent_tokens` |
| `mem_` | memory | open | `memories` |
| `fact_` | fact | open | `facts` |
| `pol_` | cross-agent access policy | open | `policies` |
| `grp_` | security group | open | `security_groups` |
| `msg_` | message | open | `messages` |
| `thr_` | message thread / cross-realm conversation | open | (`messages.thread_id`); `conversations` |
| `fpr_` | federation peer (allow-listed realm handle + key) | spine | `federation_peers` |
| `sec_` | secret | sealed | `secrets` |
| `fld_` | secret field | sealed | `secret_fields` |
| `grt_` | secret grant | sealed | `secret_grants` |
| `totp_` | TOTP enrollment | sealed | `totp_enrollments` |
| `kek_` | per-realm key-encryption key | sealed | `realm_keys` |
| `dek_` | per-secret/per-field data-encryption key | sealed | `secret_deks` |
| `att_` | attachment metadata (deferred-but-stubbed) | sealed | `attachments` |
| `aud_` | audit event | spine | `audit_events` |
| `usg_` | usage counter row | spine | `usage_counters` |
| `idem_` | idempotency record | spine | `idempotency_keys` |

The raw agent bearer-token string uses the `witself_at_` text prefix (the
re-skin of Witpass `wp_at_`) and is shown once at create/rotate; it is never an
id and never stored. `tok_` is the token's DB id. See
[token-lifecycle.md](token-lifecycle.md).

## Enumerations

Stored as `text` with `CHECK` constraints (not native Postgres enums, to keep
expand/contract migrations cheap) using the exact contract vocabularies:

| Domain | Values |
| --- | --- |
| `principal_kind` | `agent`, `operator`, `admin`, `service` |
| `owner_kind` (open + sealed discriminator) | `agent`, `group` |
| `backend_kind` | `managed`, `self-hosted`, `local` |
| `memory_state` | `active`, `forgotten`, `deleted` |
| `policy_permission` | `read`, `contribute`, `curate`, `forget` |
| `policy_scope` | `memories`, `facts`, `both` |
| `policy_effect` | `allow` (deny is implicit / post-v0) |
| `message_recipient_kind` | `agent`, `group` |
| `delivery_state` | `pending`, `delivered` |
| `read_state` | `unread`, `read`, `acked` |
| `kms_provider` (sealed) | `aws-kms`, `gcp-kms`, `azure-key-vault`, `local-dev` |
| `aead_algorithm` (sealed) | `XCHACHA20_POLY1305`, `AES_256_GCM` |
| `totp_hash_algorithm` (sealed) | `SHA1`, `SHA256`, `SHA512` |
| `template` (sealed) | `login`, `api-key`, `ssh-key`, `certificate`, `env`, `generic` |
| `usage_dimension` | the canonical metered dimensions (see [`usage_counters`](#usage_counters)) |
| `overage_behavior` | `warn`, `throttle`, `block` |

**Ownership is unified across both planes.** Witpass distinguished
`owner_kind ∈ {agent, shared}`; Witself memories/facts use `owner ∈ {agent,
group}`. The consolidated model drops the separate "shared" scope: **all** data
(memory, fact, secret) is owned by an agent or a group, so `owner_kind ∈ {agent,
group}` everywhere. A former vault-shared secret is now a **group-owned** secret
(`owner_kind = 'group'`, `owner_group_id` set). See [Ownership Model](#ownership-model-agent-owned-vs-group-owned).

Scope strings on tokens/grants use the `<resource>:<action>` vocabulary
(open plane: `memory:create`, `memory:read`, `memory:update`, `memory:forget`,
`memory:read-others`, `memory:manage-others`, `fact:create`, `fact:read`,
`fact:update`, `fact:delete`, `fact:primary`, `policy:read`, `policy:manage`,
`group:read`, `group:manage`, `group:member`, `message:send`, `message:read`;
sealed plane: `secret:create`, `secret:show`, `secret:reveal`, `secret:update`,
`secret:delete`, `secret:grant`, `totp:enroll`, `totp:code`; spine:
`realm:admin`, `account:read`, `account:manage`, `billing:read`, `billing:manage`,
`support:read`, `support:manage`, `agent:manage`, `token:manage`, `audit:read`)
and are stored as `text[]`. Realm roles (`realm:admin`, `realm:operator`,
`realm:auditor`, `realm:member`) bundle these scopes per
[authorization-and-roles.md](authorization-and-roles.md). Audit `action` strings
use a **different**, dotted `<resource>.<verb>` namespace (see
[`audit_events`](#audit_events)); the two are intentionally distinct and MUST
NOT be conflated.

## Ownership Model (Agent-Owned vs Group-Owned)

The hierarchy is **Account > Realm > Agent**, with memories, facts, and secrets
owned by an agent or a group. A record is either agent-owned (`owner.kind:
"agent"`) or group-owned (`owner.kind: "group"`), expressed relationally on each
owned table (`memories`, `facts`, `secrets`) as:

- `owner_kind text NOT NULL CHECK (owner_kind IN ('agent','group'))` — the
  discriminator that maps to the `owner.kind` JSON field.
- `owner_agent_id text NULL REFERENCES agents(id)` — set for `owner_kind = 'agent'`.
- `owner_group_id text NULL REFERENCES security_groups(id)` — set for
  `owner_kind = 'group'`.
- A `CHECK ((owner_kind = 'agent' AND owner_agent_id IS NOT NULL AND
  owner_group_id IS NULL) OR (owner_kind = 'group' AND owner_group_id IS NOT NULL
  AND owner_agent_id IS NULL))` keeps the discriminator consistent.

Name-uniqueness constraints (per [facts-model.md](facts-model.md) and
[cli-command-surface.md](cli-command-surface.md)) are enforced as **partial
unique indexes over live (non-tombstoned) rows**. Partial and expression-based
uniqueness MUST be `CREATE UNIQUE INDEX` (Postgres does not accept a
partial/expression predicate on an inline table-level `UNIQUE` constraint), so
every such rule below is written as an index, never as an inline `UNIQUE (...)
WHERE ...`:

```sql
-- Agent name unique per realm (live rows only)
CREATE UNIQUE INDEX ux_agents_realm_name
  ON agents (realm_id, name)
  WHERE deleted_at IS NULL;

-- Fact name unique per owning agent within a realm
CREATE UNIQUE INDEX ux_facts_agent_name
  ON facts (realm_id, owner_agent_id, name)
  WHERE owner_kind = 'agent' AND deleted_at IS NULL;

-- Fact name unique per owning group within a realm
CREATE UNIQUE INDEX ux_facts_group_name
  ON facts (realm_id, owner_group_id, name)
  WHERE owner_kind = 'group' AND deleted_at IS NULL;

-- Secret name unique per owning agent within a realm
CREATE UNIQUE INDEX ux_secrets_agent_name
  ON secrets (realm_id, owner_agent_id, name)
  WHERE owner_kind = 'agent' AND deleted_at IS NULL;

-- Secret name unique per owning group within a realm
CREATE UNIQUE INDEX ux_secrets_group_name
  ON secrets (realm_id, owner_group_id, name)
  WHERE owner_kind = 'group' AND deleted_at IS NULL;
```

Memories are **not** name-unique — they are addressed by `id` and recalled
semantically (see [memory-model.md](memory-model.md)); only `agents`, `facts`,
`secrets`, and `security_groups` carry name uniqueness. Agent-created records
default to the creating agent's ownership; group ownership is explicit and
auditable. Cross-agent open-plane access is governed by
[access-policy.md](access-policy.md); cross-agent/operator secret access requires
an explicit grant or realm role plus an audit `reason` (see
[`secret_grants`](#secret_grants) and [authorization-and-roles.md](authorization-and-roles.md)).
Per-agent default isolation means an agent sees only its own `owner_kind =
'agent'` records plus group-owned records its policies/grants reach.

## Shared Spine Tables

### `accounts`

Purpose: the customer/billing root. One account owns one or more realms. The
account is the billing target; usage rolls up by realm (see
[billing-and-limits.md](billing-and-limits.md)).

| Column | Type | Notes |
| --- | --- | --- |
| `id` | `text` PK | `acct_` prefix |
| `name` | `text NOT NULL` | display name |
| `backend_kind` | `text NOT NULL` | `managed` \| `self-hosted` \| `local` |
| `plan_id` | `text NULL` | plan reference (`plan_` prefix; account-level plan) |
| `sealed_plane_enabled` | `boolean NOT NULL DEFAULT false` | whether the sealed (secret/TOTP) plane is provisioned; gates the KMS dependency |
| `row_version` | `bigint NOT NULL DEFAULT 1` | optimistic lock |
| `created_at` / `updated_at` | `timestamptz NOT NULL` | |
| `deleted_at` | `timestamptz NULL` | tombstone |

Indexes: PK on `id`. Account closure interacts with the audit/PII retention
posture (see [Account Deletion, PII, And Erasure](#account-deletion-pii-and-erasure)).

### `operators`

Purpose: human principals (the `opr_` actors in audit/operator-auth). Operators
authenticate via browser OAuth/OIDC+PKCE or device code; raw account passwords
are NEVER stored or columned here. See [operator-auth.md](operator-auth.md).

| Column | Type | Notes |
| --- | --- | --- |
| `id` | `text` PK | `opr_` prefix |
| `account_id` | `text NOT NULL` FK -> `accounts(id)` | tenant scope |
| `email` | `text NOT NULL` | non-sensitive identifier (PII; see [Account Deletion, PII, And Erasure](#account-deletion-pii-and-erasure)) |
| `display_name` | `text NULL` | |
| `idp_subject` | `text NULL` | external OIDC subject; no password material |
| `disabled_at` | `timestamptz NULL` | disabled operators cannot authenticate |
| `row_version` | `bigint NOT NULL DEFAULT 1` | |
| `created_at` / `updated_at` / `deleted_at` | `timestamptz` | |

Constraints: FK `account_id`. Email uniqueness is an expression + partial index:

```sql
CREATE UNIQUE INDEX ux_operators_account_email
  ON operators (account_id, lower(email))
  WHERE deleted_at IS NULL;
```

### `realms`

Purpose: the rename of the Witpass vault — the operator-owned container and the
billing / isolation / key-separation scope. Plans, usage limits, agent caps, and
rate limits attach to the account and roll up by realm; the per-realm KEK
(sealed plane) is keyed here. See [storage.md](storage.md).

| Column | Type | Notes |
| --- | --- | --- |
| `id` | `text` PK | `realm_` prefix |
| `account_id` | `text NOT NULL` FK -> `accounts(id)` | tenant scope |
| `name` | `text NOT NULL` | |
| `description` | `text NULL` | |
| `realm_handle` | `text NULL` | the realm's **federation identity** (the unit of trust; published in the realm card); globally unique when set. NULL for a realm not yet federated. Maps to the card `realm_handle` (see [agent-collaboration.md](agent-collaboration.md)) |
| `signing_public_key` | `text NULL` | the realm signing **public key** / JWKS (or a JWKS ref) published in the realm card and used by peers to verify cross-realm envelopes. Public material only — the **private signing key is sealed-plane / KMS-custodied and is NEVER a column here** |
| `signing_key_version` | `bigint NULL` | monotonic signing-key generation, bumped on rotation; lets peers select the verifying key during a rotation overlap |
| `card_expires_at` | `timestamptz NULL` | TTL of the currently published realm card (`ttl` / `expires_at`); cards are re-fetched on expiry |
| `row_version` | `bigint NOT NULL DEFAULT 1` | |
| `created_at` / `updated_at` / `deleted_at` | `timestamptz` | |

The signing **private** key lives where realm key material lives (KMS-custodied
sealed plane; see [key-hierarchy.md](key-hierarchy.md)), never in `realms`. The
`realm_handle` + `signing_public_key` here are the **per-cell copy** of the realm
card's identity; the authoritative *routing* directory (handle -> home cell +
endpoint + key) is the separate global control plane, not this table (see the
[control-plane note above](#two-planes-one-schema) and
[deployment-cells.md](deployment-cells.md)). Cross-realm features are post-v0; in
a realm-local-only deployment these columns are simply NULL.

Constraints: FK `account_id`; index `(account_id)`. Name uniqueness is a partial
index; `realm_handle` is globally unique among live, federated realms:

```sql
CREATE UNIQUE INDEX ux_realms_account_name
  ON realms (account_id, name)
  WHERE deleted_at IS NULL;

-- federation handle is globally unique when present (live rows only)
CREATE UNIQUE INDEX ux_realms_handle
  ON realms (realm_handle)
  WHERE realm_handle IS NOT NULL AND deleted_at IS NULL;
```

### `realm_members`

Purpose: operator <-> realm membership with a role (the operator-side permission
surface; agent identity is separate and token-bound). The re-skin of Witpass
`vault_members`.

| Column | Type | Notes |
| --- | --- | --- |
| `id` | `text` PK | `rmb_` prefix |
| `account_id` | `text NOT NULL` FK -> `accounts(id)` | |
| `realm_id` | `text NOT NULL` FK -> `realms(id)` | |
| `operator_id` | `text NOT NULL` FK -> `operators(id)` | |
| `role` | `text NOT NULL` | realm role: `realm:admin`, `realm:operator`, `realm:auditor`, `realm:member` |
| `scopes` | `text[] NOT NULL DEFAULT '{}'` | optional extra granted scopes |
| `row_version` | `bigint NOT NULL DEFAULT 1` | |
| `created_at` / `updated_at` / `deleted_at` | `timestamptz` | |

Constraints: FKs `account_id`, `realm_id`, `operator_id`. Membership uniqueness
is a partial index:

```sql
CREATE UNIQUE INDEX ux_realm_members_realm_operator
  ON realm_members (realm_id, operator_id)
  WHERE deleted_at IS NULL;
```

### `agents`

Purpose: durable named agent identities inside a realm. Ephemeral runtimes
inherit a named agent via a mounted token. Identity is determined by the token,
never by a caller-supplied `--agent` (a targeting input only).

| Column | Type | Notes |
| --- | --- | --- |
| `id` | `text` PK | `agent_` prefix |
| `account_id` | `text NOT NULL` FK -> `accounts(id)` | |
| `realm_id` | `text NOT NULL` FK -> `realms(id)` | |
| `name` | `text NOT NULL` | unique per realm (live rows) |
| `description` | `text NULL` | |
| `disabled_at` | `timestamptz NULL` | disabled agents cannot authenticate with existing tokens |
| `disabled_reason` | `text NULL` | surfaced as token `disabled_reason` |
| `row_version` | `bigint NOT NULL DEFAULT 1` | |
| `created_at` / `updated_at` / `deleted_at` | `timestamptz` | |

Constraints: `ux_agents_realm_name` (above); FKs `account_id`, `realm_id`. Index
`(realm_id)`. Disabling or tombstoning an agent invalidates its tokens (enforced
in the auth path by joining live `agents` and checking `disabled_at`).

### `agent_tokens`

Purpose: bearer token records. **Only hashes are stored, never raw token
values.** Raw `witself_at_...` text is returned exactly once at create/rotate.
Bound server-side to exactly one `(realm_id, agent_id)`. Covers both agent and
operator tokens via `principal_kind`. See [token-lifecycle.md](token-lifecycle.md).

| Column | Type | Notes |
| --- | --- | --- |
| `id` | `text` PK | `tok_` prefix (maps to `{token_id}`) |
| `account_id` | `text NOT NULL` FK -> `accounts(id)` | |
| `realm_id` | `text NOT NULL` FK -> `realms(id)` | |
| `agent_id` | `text NULL` FK -> `agents(id)` | set for agent tokens |
| `name` | `text NULL` | token name (metadata) |
| `principal_kind` | `text NOT NULL` | `agent` \| `operator` \| `admin` \| `service` |
| `token_lookup` | `text NOT NULL` | non-secret lookup id/prefix to index bearer auth without full scans |
| `token_hash` | `bytea NOT NULL` | hash/HMAC of the raw token (e.g. SHA-256/HMAC); never the raw value |
| `hash_alg` | `text NOT NULL` | hash scheme + version for rotation |
| `scopes` | `text[] NOT NULL DEFAULT '{}'` | granted scope vocabulary (open + sealed + spine) |
| `last_used_at` | `timestamptz NULL` | best-effort, updated on auth |
| `expires_at` | `timestamptz NULL` | NULL = durable (v0 default); set only via `--ttl`/`--expires-at` |
| `revoked_at` | `timestamptz NULL` | immediate revocation; revoked tokens cannot authenticate |
| `disabled_reason` | `text NULL` | blocked/disabled reason metadata |
| `created_at` / `updated_at` / `deleted_at` | `timestamptz` | tombstone per retention policy |

Constraints: `UNIQUE (token_lookup)`; FKs `account_id`, `realm_id`, `agent_id`.
Index `(agent_id) WHERE revoked_at IS NULL`. Auth lookup is by `token_lookup`
then constant-time compare of `token_hash`. Rotation: issue replacement, revoke
old immediately unless `--grace-period`. The consolidated default agent token
bundle is `{memory:create, memory:read, memory:update, memory:forget,
fact:create, fact:read, fact:update, fact:delete, fact:primary, secret:create,
secret:show, secret:reveal, secret:update, secret:delete, totp:enroll,
totp:code, message:send, message:read}` over OWN data only (see
[token-lifecycle.md](token-lifecycle.md)). Raw token values MUST NOT appear in
any column, audit row, or log.

Token mutations (create / rotate / revoke) use `revoked_at` and rotation
semantics rather than the `If-Match` / `row_version` optimistic-lock contract, so
`agent_tokens` carries no `row_version` column and is intentionally excluded from
the [Optimistic Concurrency](#optimistic-concurrency-and-history) table list.

## Open-Plane Tables

Open-plane content lives in **ordinary columns** (data at rest), is semantically
indexed, recallable, cross-agent readable under policy, in the self-digest, and
plaintext-exportable. There is no reveal ceremony; `sensitive` is a
PII/redaction display flag only.

### `memories`

Purpose: free-form self-content owned by an agent or a group, addressed by `id`
(never name-unique), recalled semantically. Maps to the Memory JSON shape in
[json-contracts.md](json-contracts.md); semantics in
[memory-model.md](memory-model.md).

| Column | Type | Notes |
| --- | --- | --- |
| `id` | `text` PK | `mem_` prefix |
| `account_id` | `text NOT NULL` FK -> `accounts(id)` | |
| `realm_id` | `text NOT NULL` FK -> `realms(id)` | |
| `owner_kind` | `text NOT NULL` | `agent` \| `group` discriminator |
| `owner_agent_id` | `text NULL` FK -> `agents(id)` | set iff `owner_kind = 'agent'` |
| `owner_group_id` | `text NULL` FK -> `security_groups(id)` | set iff `owner_kind = 'group'` |
| `content` | `text NOT NULL` | free-form payload; **ordinary column** (data at rest), <= 256 KiB; input to embedding |
| `kind` | `text NOT NULL DEFAULT 'note'` | open string label (`episodic`/`semantic`/`profile`/`note`/…); not a closed enum |
| `tags` | `text[] NOT NULL DEFAULT '{}'` | filter/rank tags (<= 64) |
| `source` | `text NOT NULL DEFAULT 'self'` | provenance: `self` \| `agent:<name>` \| `operator` \| `import:<file>` \| `msg_…` |
| `salience` | `real NOT NULL DEFAULT 0.5` | recall-ranking weight, `0.0`–`1.0` |
| `links` | `text[] NOT NULL DEFAULT '{}'` | `witself://` references to memories/facts |
| `sensitive` | `boolean NOT NULL DEFAULT false` | PII redaction display flag (NOT encryption) |
| `state` | `text NOT NULL DEFAULT 'active'` | `active` \| `forgotten` (tombstoned) \| `deleted` |
| `version` | `bigint NOT NULL DEFAULT 1` | monotonic content version (bumped on `adjust`); surfaced in history |
| `last_accessed_at` | `timestamptz NULL` | feeds recency ranking |
| `row_version` | `bigint NOT NULL DEFAULT 1` | optimistic lock (distinct from `version`) |
| `created_at` / `updated_at` / `deleted_at` | `timestamptz` | `forgotten` uses `state` + tombstone retention |

Constraints: the `owner_kind` / `owner_agent_id` / `owner_group_id` `CHECK`
(above). FKs `account_id`, `realm_id`, `owner_agent_id`, `owner_group_id`.
Indexes `(realm_id, owner_agent_id)`, `(realm_id, owner_group_id)`, and on
`(realm_id, kind)` for metadata `list`/digest selection. The embedding vector is
a separate column (see [Embedding Storage](#embedding-storage)).

```sql
-- metadata/digest filters never touch the embedding provider
CREATE INDEX ix_memories_realm_state_kind
  ON memories (realm_id, state, kind)
  WHERE deleted_at IS NULL;
```

The salient-memory digest selection and recall ranking read `salience`,
`created_at`/`last_accessed_at`, and `kind`; see [memory-model.md](memory-model.md)
and [context-hydration.md](context-hydration.md). Memory `content` is never an
ordinary readable column for unauthorized callers, but it is **not**
encrypted-as-product — that is the sealed plane's job.

### `memory_embeddings`

Purpose: the pgvector embedding column, keyed by memory id, plus the provider,
model, and dimensionality the vector was computed with. Vectors are
storage-internal: never returned in API responses, logs, audit, or export (see
[memory-model.md](memory-model.md) Recall and Embeddings).

| Column | Type | Notes |
| --- | --- | --- |
| `memory_id` | `text` PK FK -> `memories(id)` ON DELETE CASCADE | one vector per memory |
| `account_id` | `text NOT NULL` FK -> `accounts(id)` | |
| `realm_id` | `text NOT NULL` FK -> `realms(id)` | |
| `embedding` | `vector(N)` NULL | pgvector column; `N` = provider/model dimensionality; NULL when flagged-for-embedding |
| `provider` | `text NOT NULL` | `voyage` \| `openai` \| `local-dev` |
| `model` | `text NOT NULL` | provider-specific model id |
| `dimensions` | `integer NOT NULL` | vector dimensionality |
| `embedded_at` | `timestamptz NULL` | NULL while pending; set on successful embed |
| `created_at` / `updated_at` | `timestamptz` | |

Constraints: FKs all above. An approximate vector index (e.g. HNSW/IVFFlat) is
created over `embedding` per [storage.md](storage.md); index type and parameters
are tuned in [memory-model.md](memory-model.md). The migration that introduces
recall enables `pgvector` and is ordered before this column is created. Vectors
are recomputable from `content`; recall degrades deterministically to
keyword/tag/kind/time ranking when the provider is unavailable.
**Sealed-plane carve-out:** secret values and TOTP seeds are NEVER embedded and
never appear here.

### `memory_versions`

Purpose: append-only edit history per memory (and the same shape backs
`fact_versions`). Records the new version, the actor (always derived from the
token), a timestamp, the changed fields, and, for cross-agent/operator edits, the
audit `--reason` and deciding policy id. Never stores embedding vectors or raw
tokens.

| Column | Type | Notes |
| --- | --- | --- |
| `id` | `text` PK | history-row id |
| `account_id` | `text NOT NULL` FK -> `accounts(id)` | |
| `realm_id` | `text NOT NULL` FK -> `realms(id)` | |
| `memory_id` | `text NOT NULL` FK -> `memories(id)` ON DELETE CASCADE | |
| `version` | `bigint NOT NULL` | the version this entry records |
| `actor_kind` / `actor_id` | `text NOT NULL` | derived from token |
| `changed_fields` | `jsonb NOT NULL` | non-sensitive change summary (never content for unauthorized views) |
| `reason` | `text NULL` | audit reason for cross-agent/operator edits |
| `policy_id` | `text NULL` FK -> `policies(id)` | deciding policy id when cross-agent |
| `created_at` | `timestamptz NOT NULL` | |

Constraints: FKs above; index `(memory_id, version)`. History entries inherit the
memory's `sensitive` posture. Append-only (no `deleted_at`).

### `facts`

Purpose: a `name`→`value` identity attribute owned by an agent or a group,
name-unique per owner, resolved deterministically by name. Maps to the Fact JSON
shape; semantics in [facts-model.md](facts-model.md).

| Column | Type | Notes |
| --- | --- | --- |
| `id` | `text` PK | `fact_` prefix |
| `account_id` | `text NOT NULL` FK -> `accounts(id)` | |
| `realm_id` | `text NOT NULL` FK -> `realms(id)` | |
| `owner_kind` | `text NOT NULL` | `agent` \| `group` discriminator |
| `owner_agent_id` | `text NULL` FK -> `agents(id)` | set iff `owner_kind = 'agent'` |
| `owner_group_id` | `text NULL` FK -> `security_groups(id)` | set iff `owner_kind = 'group'` |
| `name` | `text NOT NULL` | attribute name; **unique per owner** (<= 255 chars, namespaced with `/`) |
| `value` | `text NOT NULL` | the fact value; **ordinary column** (data at rest), <= 64 KiB |
| `primary` | `boolean NOT NULL DEFAULT false` | identity anchor; at most one primary per logical kind per owner |
| `logical_kind` | `text NULL` | explicit `--kind` for primary uniqueness; defaults to the leaf name |
| `sensitive` | `boolean NOT NULL DEFAULT false` | PII redaction display flag (NOT encryption; NO reveal ceremony) |
| `format` | `text NULL` | advisory type hint (`string`/`email`/`url`/`date`/`number`) |
| `source` | `text NOT NULL DEFAULT 'self'` | provenance: `self` \| `agent:<name>` \| `operator` \| `import:<file>` |
| `row_version` | `bigint NOT NULL DEFAULT 1` | optimistic lock |
| `created_at` / `updated_at` / `deleted_at` | `timestamptz` | |

Constraints: `ux_facts_agent_name`, `ux_facts_group_name`, and the `owner_kind`
`CHECK` (above). FKs `account_id`, `realm_id`, `owner_agent_id`,
`owner_group_id`. At-most-one-primary-per-(owner, logical kind) is a partial
unique index that powers the atomic promotion in
[facts-model.md](facts-model.md):

```sql
CREATE UNIQUE INDEX ux_facts_agent_primary_kind
  ON facts (realm_id, owner_agent_id, coalesce(logical_kind, name))
  WHERE owner_kind = 'agent' AND primary AND deleted_at IS NULL;

CREATE UNIQUE INDEX ux_facts_group_primary_kind
  ON facts (realm_id, owner_group_id, coalesce(logical_kind, name))
  WHERE owner_kind = 'group' AND primary AND deleted_at IS NULL;
```

Facts have **no** `tags`/`salience`/`links`/embedding. A `sensitive` fact value
is redacted in `list`/`scan` by default but is returned in full on an authorized
`fact get` — an ordinary authorized read, **not a secret reveal**. A credential
belongs in the sealed plane (a [`secret`](#secrets)), not a sensitive fact.
Fact edit history reuses the `memory_versions` shape as `fact_versions`.

### `policies`

Purpose: the cross-agent IDENTITY access engine — first-class, evaluable rows
that replace Witpass's per-secret grants for the open plane. A policy binds a
subject (agent or group) to one permission verb on a target (agent or group),
scoped to memories and/or facts, optionally filtered, default-deny. Semantics in
[access-policy.md](access-policy.md). **Secrets are NOT governed by policies** —
sealed-plane cross-agent/operator access uses [`secret_grants`](#secret_grants)
plus realm roles (see [authorization-and-roles.md](authorization-and-roles.md)).

| Column | Type | Notes |
| --- | --- | --- |
| `id` | `text` PK | `pol_` prefix |
| `account_id` | `text NOT NULL` FK -> `accounts(id)` | |
| `realm_id` | `text NOT NULL` FK -> `realms(id)` | policies never span realms |
| `subject_kind` | `text NOT NULL` | `agent` \| `group` |
| `subject_agent_id` | `text NULL` FK -> `agents(id)` | set iff `subject_kind = 'agent'` |
| `subject_group_id` | `text NULL` FK -> `security_groups(id)` | set iff `subject_kind = 'group'` |
| `permission` | `text NOT NULL` | `read` \| `contribute` \| `curate` \| `forget` |
| `target_kind` | `text NOT NULL` | `agent` \| `group` |
| `target_agent_id` | `text NULL` FK -> `agents(id)` | set iff `target_kind = 'agent'` |
| `target_group_id` | `text NULL` FK -> `security_groups(id)` | set iff `target_kind = 'group'` |
| `scope` | `text NOT NULL` | `memories` \| `facts` \| `both` |
| `filter` | `jsonb NULL` | optional narrowing (kind/tag/name/namespace/sensitive); only narrows, never widens |
| `effect` | `text NOT NULL DEFAULT 'allow'` | `allow` only in v0 (deny is implicit) |
| `description` | `text NULL` | human-readable |
| `created_by` | `text NULL` | creating principal (audit) |
| `row_version` | `bigint NOT NULL DEFAULT 1` | |
| `created_at` / `updated_at` / `deleted_at` | `timestamptz` | |

Constraints: subject/target discriminator `CHECK`s; FKs all id columns. Indexes
`(realm_id, target_agent_id, permission)` and `(realm_id, target_group_id,
permission)` for default-deny evaluation. Membership is resolved at decision
time against `group_members`, never frozen at create time.

### `security_groups`

Purpose: a named set of agents in a realm that is both a policy subject and a
policy target, and that may own group-scoped collective memories/facts. A
net-new Witself concept (no Witpass equivalent). Semantics in
[security-groups.md](security-groups.md).

| Column | Type | Notes |
| --- | --- | --- |
| `id` | `text` PK | `grp_` prefix |
| `account_id` | `text NOT NULL` FK -> `accounts(id)` | |
| `realm_id` | `text NOT NULL` FK -> `realms(id)` | groups never span realms |
| `name` | `text NOT NULL` | unique per realm |
| `description` | `text NULL` | |
| `row_version` | `bigint NOT NULL DEFAULT 1` | |
| `created_at` / `updated_at` / `deleted_at` | `timestamptz` | |

Constraints: FKs `account_id`, `realm_id`. Name uniqueness per realm (live rows):

```sql
CREATE UNIQUE INDEX ux_security_groups_realm_name
  ON security_groups (realm_id, name)
  WHERE deleted_at IS NULL;
```

Bound policies are not stored on the group; they are [`policies`](#policies) rows
that reference the group as subject or target. Group ownership of identity data
is expressed via `owner_group_id` on `memories`/`facts` (and, for group-owned
secrets, on `secrets`).

### `group_members`

Purpose: group × agent membership, with an `admin` flag for delegated membership
management. Membership is evaluated at decision time, never cached into policies.

| Column | Type | Notes |
| --- | --- | --- |
| `id` | `text` PK | membership-row id |
| `account_id` | `text NOT NULL` FK -> `accounts(id)` | |
| `realm_id` | `text NOT NULL` FK -> `realms(id)` | |
| `group_id` | `text NOT NULL` FK -> `security_groups(id)` ON DELETE CASCADE | |
| `agent_id` | `text NOT NULL` FK -> `agents(id)` | member agent |
| `is_admin` | `boolean NOT NULL DEFAULT false` | `admins[]`: may manage membership with `group:manage` |
| `row_version` | `bigint NOT NULL DEFAULT 1` | |
| `created_at` / `updated_at` / `deleted_at` | `timestamptz` | |

Constraints: FKs all above. Membership uniqueness per (group, agent), live rows:

```sql
CREATE UNIQUE INDEX ux_group_members_group_agent
  ON group_members (group_id, agent_id)
  WHERE deleted_at IS NULL;
```

`add-member` is idempotent by (group, agent); `remove-member` immediately revokes
every permission the agent held *through* the group (membership is evaluated at
decision time).

### `messages`

Purpose: durable inter-agent messages with a per-recipient mailbox. `from` is
always token-derived (sender forgery is structurally impossible). `body`/`payload`
are untrusted on receipt and never authorize a write. Semantics in
[inter-agent-messaging.md](inter-agent-messaging.md).

| Column | Type | Notes |
| --- | --- | --- |
| `id` | `text` PK | `msg_` prefix; realm-unique; the at-least-once dedup key |
| `account_id` | `text NOT NULL` FK -> `accounts(id)` | |
| `realm_id` | `text NOT NULL` FK -> `realms(id)` | realm-local only in v0 |
| `from_agent_id` | `text NOT NULL` FK -> `agents(id)` | **always token-derived**, never from input |
| `to_kind` | `text NOT NULL` | `agent` \| `group` |
| `to_agent_id` | `text NULL` FK -> `agents(id)` | set iff `to_kind = 'agent'` |
| `to_group_id` | `text NULL` FK -> `security_groups(id)` | set iff `to_kind = 'group'` (fan-out, snapshot-at-send) |
| `subject` | `text NULL` | short classification (<= 256 chars) |
| `kind` | `text NOT NULL DEFAULT 'note'` | open label (`note`/`request`/`reply`/`event`/`handoff`/…) |
| `body` | `text NOT NULL` | free-form text (<= 64 KiB); ordinary column; untrusted on receipt |
| `payload` | `jsonb NULL` | optional small structured object (<= 16 KiB serialized); untrusted |
| `thread_id` | `text NULL` | `thr_` prefix; orders a conversation per recipient/thread; promoted to `conversation_id` for cross-realm (see [`conversations`](#conversations-cross-realm-post-v0)) |
| `created_at` | `timestamptz NOT NULL` | server-assigned send time |

**Cross-realm envelope columns (post-v0; additive, NULL for realm-local
messages).** When `to`/`from` carry an optional `realm`, the message rides the
signed cross-realm envelope (see [agent-collaboration.md](agent-collaboration.md));
these columns capture the on-the-wire envelope. They are absent/NULL for an
in-realm message, which is unchanged. `from` is **still token-derived** — the
`from_realm_handle` is the sender's own realm handle, never caller-supplied:

| Column | Type | Notes |
| --- | --- | --- |
| `from_realm_handle` | `text NULL` | sending realm's `realm_handle` (server-stamped from the authenticated realm, never input); NULL = local sender |
| `to_realm_handle` | `text NULL` | recipient realm's `realm_handle`; NULL = local recipient. The relay routes by this handle to the recipient's home cell |
| `conversation_id` | `text NULL` | `thr_` prefix (reuses the thread prefix); the first-class cross-realm conversation/task this envelope belongs to. FK -> [`conversations(id)`](#conversations-cross-realm-post-v0) |
| `hop_count` | `smallint NOT NULL DEFAULT 0` | relay hops so far; each hop increments it |
| `max_hops` | `smallint NOT NULL DEFAULT 8` | hop ceiling; over `max_hops` the message is dropped + audited |
| `sequence` | `bigint NULL` | per-conversation monotonic ordering of the sender's envelopes |
| `nonce` | `text NULL` | per-envelope unique nonce; dedup is on `(id, nonce)` (extends the at-least-once `msg_`-id dedup) |
| `expires_at` | `timestamptz NULL` | envelope TTL (default 1h, max 24h); an expired envelope is NOT delivered |
| `signature` | `text NULL` | the sending realm's **JWS over the canonicalized envelope**; verified against the peer's published `signing_public_key` before trust |

Constraints: subject/target discriminator `CHECK`; FKs all id columns; FK
`conversation_id` -> `conversations(id)` (nullable, constrains cross-realm rows
only). Indexes `(realm_id, thread_id, created_at)` for per-thread ordering and on
the delivery join; a `(realm_id, conversation_id, sequence)` index orders a
cross-realm conversation; cross-realm dedup is enforced on `(id, nonce)`.
`messages` is append-only at the body level (no edit/recall in v0); there is no
`deleted_at` on the message body, only on deliveries per retention. The signed
envelope is verified, and hop/TTL/budget governors applied, **before** a row is
written and a delivery is created; the relay is blind (it cannot read `body` /
`payload` or forge `signature`).

### `message_deliveries`

Purpose: per-recipient delivery + read/ack state for the mailbox/queue. A group
send produces N delivery rows at send time; each member's state is independent.

| Column | Type | Notes |
| --- | --- | --- |
| `id` | `text` PK | delivery-row id |
| `account_id` | `text NOT NULL` FK -> `accounts(id)` | |
| `realm_id` | `text NOT NULL` FK -> `realms(id)` | |
| `message_id` | `text NOT NULL` FK -> `messages(id)` ON DELETE CASCADE | |
| `recipient_agent_id` | `text NOT NULL` FK -> `agents(id)` | the mailbox owner |
| `delivery_state` | `text NOT NULL DEFAULT 'pending'` | `pending` \| `delivered` |
| `delivered_at` | `timestamptz NULL` | |
| `read_state` | `text NOT NULL DEFAULT 'unread'` | `unread` \| `read` \| `acked` |
| `read_at` / `acked_at` | `timestamptz NULL` | explicit per-recipient transitions |
| `row_version` | `bigint NOT NULL DEFAULT 1` | |
| `created_at` / `updated_at` / `deleted_at` | `timestamptz` | |

Constraints: FKs all above. One delivery per (message, recipient), live rows:

```sql
CREATE UNIQUE INDEX ux_message_deliveries_msg_recipient
  ON message_deliveries (message_id, recipient_agent_id)
  WHERE deleted_at IS NULL;

CREATE INDEX ix_message_deliveries_mailbox
  ON message_deliveries (recipient_agent_id, created_at)
  WHERE deleted_at IS NULL;
```

`message list` reads metadata only (no state change); `read` transitions
`unread → read`; `ack` transitions to `acked`. Bodies/payloads never appear in
audit, logs, or metrics.

### `conversations` (cross-realm, post-v0)

Purpose: the first-class cross-realm conversation/task — the realm-local
`thread_id` **promoted** to a durable resource carrying an A2A-style task state
machine and the per-conversation loop/budget governors. Maps to the
Conversation/task JSON shape in [json-contracts.md](json-contracts.md); semantics
in [agent-collaboration.md](agent-collaboration.md). The **durable mailbox in the
home cell remains the source of truth** for conversation state; this row is the
queryable projection of it. Reuses the `thr_` prefix so a promoted thread keeps
its id. Realm-local-only deployments need not populate it.

| Column | Type | Notes |
| --- | --- | --- |
| `id` | `text` PK | `thr_` prefix (the promoted `conversation_id`) |
| `account_id` | `text NOT NULL` FK -> `accounts(id)` | |
| `realm_id` | `text NOT NULL` FK -> `realms(id)` | the local realm's view; the peer keeps its own row |
| `state` | `text NOT NULL DEFAULT 'submitted'` | `submitted` \| `working` \| `input_required` \| `auth_required` \| `completed` \| `failed` \| `canceled` |
| `participants` | `jsonb NOT NULL DEFAULT '[]'` | the participating realm-qualified agents (`{realm_handle, agent}`); local participants resolve to `agents` |
| `peer_id` | `text NULL` FK -> `federation_peers(id)` | the allow-listed peer for a cross-realm conversation; NULL = realm-local |
| `auto_reply` | `boolean NOT NULL DEFAULT false` | directed (human-gated, default) vs autonomous; enforced on the wire |
| `turn_budget` | `integer NOT NULL DEFAULT 24` | per-conversation turn ceiling |
| `turns_used` | `integer NOT NULL DEFAULT 0` | turns consumed; exhaustion suspends the conversation (`budget.exhausted`) |
| `cost_budget` | `bigint NULL` | optional per-conversation cost ceiling (metered units) |
| `cost_used` | `bigint NOT NULL DEFAULT 0` | cost consumed so far |
| `expires_at` | `timestamptz NULL` | optional conversation-level TTL |
| `row_version` | `bigint NOT NULL DEFAULT 1` | optimistic lock |
| `created_at` / `updated_at` / `deleted_at` | `timestamptz` | |

Constraints: FKs `account_id`, `realm_id`, `peer_id`. Index `(realm_id, state,
updated_at)` for listing conversations by state. State transitions emit the
`conversation.started` / `conversation.state_changed` / `conversation.completed` /
`conversation.failed` / `conversation.canceled` audit events and budget
exhaustion emits `budget.exhausted` / `loop.suspended`; the canonical names land
in [audit-retention.md](audit-retention.md) and the
[`audit_events`](#audit_events) stable-actions list. `remaining_turns`
in the JSON shape is derived (`turn_budget - turns_used`), not a stored column.
The state machine and budgets are authoritative on the home cell; any live stream
is a latency accelerator only.

## Sealed-Plane Tables

Sealed-plane sensitive values and TOTP seeds live **only** in envelope columns
(CMK → per-realm KEK → per-secret/field DEK; see [key-hierarchy.md](key-hierarchy.md)).
This plane is reveal-gated and is **never embedded, never recalled, never in the
self-digest, never plaintext-exported, and never ingested**. These tables exist
only when the sealed plane is enabled (`accounts.sealed_plane_enabled`); enabling
the plane makes KMS a required dependency.

### `secrets`

Purpose: secret metadata and ownership. Secrets are flat sets of named fields;
only `name` and `description` are required. Maps to the secret summary/detail
JSON shape. Owner is an agent or a group (a former vault-shared secret is now
group-owned).

| Column | Type | Notes |
| --- | --- | --- |
| `id` | `text` PK | `sec_` prefix (maps to `{secret_id}`) |
| `account_id` | `text NOT NULL` FK -> `accounts(id)` | |
| `realm_id` | `text NOT NULL` FK -> `realms(id)` | |
| `owner_kind` | `text NOT NULL` | `agent` \| `group` discriminator |
| `owner_agent_id` | `text NULL` FK -> `agents(id)` | set iff `owner_kind = 'agent'` |
| `owner_group_id` | `text NULL` FK -> `security_groups(id)` | set iff `owner_kind = 'group'` (was vault-shared) |
| `name` | `text NOT NULL` | <= 255 chars; uniqueness per ownership scope |
| `description` | `text NULL` | <= 4 KiB |
| `template` | `text NOT NULL DEFAULT 'generic'` | `login`/`api-key`/`ssh-key`/`certificate`/`env`/`generic`; convention only |
| `tags` | `text[] NOT NULL DEFAULT '{}'` | non-sensitive |
| `archived_at` | `timestamptz NULL` | soft-archive state (archive/restore) |
| `row_version` | `bigint NOT NULL DEFAULT 1` | optimistic lock; surfaced for `If-Match`/`conflict` |
| `created_at` / `updated_at` / `deleted_at` | `timestamptz` | |

Constraints: `ux_secrets_agent_name`, `ux_secrets_group_name`, and the
`owner_kind` `CHECK` (above). FKs `account_id`, `realm_id`, `owner_agent_id`,
`owner_group_id`. Index `(realm_id, owner_agent_id)` and `(realm_id,
owner_group_id)`. `field_count` and `sensitive_field_count` in the JSON shape are
derived from `secret_fields`.

### `secret_fields`

Purpose: one row per named field. Non-sensitive fields are ordinary queryable
columns; sensitive field values live ONLY in the envelope columns. Plaintext is
never an ordinary column. Maps to the secret field object (`name`, `sensitive`,
and `value`/`value_encoding` in the client-facing reveal path only).

| Column | Type | Notes |
| --- | --- | --- |
| `id` | `text` PK | `fld_` prefix |
| `account_id` | `text NOT NULL` FK -> `accounts(id)` | |
| `realm_id` | `text NOT NULL` FK -> `realms(id)` | scoping + AAD binding |
| `secret_id` | `text NOT NULL` FK -> `secrets(id)` ON DELETE CASCADE | parent secret |
| `name` | `text NOT NULL` | field name |
| `sensitive` | `boolean NOT NULL` | per-field sensitivity marker |
| `plain_value` | `text NULL` | non-sensitive queryable value (usernames, URLs, issuers, labels) ONLY; <= 16 KiB; NULL when `sensitive` |
| envelope columns | see [Sensitive-Field Envelope Storage](#sensitive-field-envelope-storage) | **nullable** here; populated ONLY when `sensitive`: `ciphertext`, `nonce`, `aead_algorithm`, `dek_id`, `dek_version`, `kms_provider`, `aad_context` |
| `row_version` | `bigint NOT NULL DEFAULT 1` | |
| `created_at` / `updated_at` / `deleted_at` | `timestamptz` | |

Because `secret_fields` is a mixed table (sensitive and non-sensitive rows in one
table), the envelope columns are declared **NULLABLE** at the column level; the
"present" requirement is enforced only by the sensitive/non-sensitive `CHECK`,
not by per-column `NOT NULL` (a `NOT NULL` envelope column would reject every
non-sensitive insert). `NOT NULL` envelope columns appear only on
always-encrypted rows (`totp_enrollments` seed, `attachments`), not here.

```sql
-- one envelope, fully present, when sensitive; no envelope when not
ALTER TABLE secret_fields ADD CONSTRAINT ck_secret_fields_envelope CHECK (
  (sensitive
     AND ciphertext IS NOT NULL AND nonce IS NOT NULL AND aead_algorithm IS NOT NULL
     AND dek_id IS NOT NULL AND dek_version IS NOT NULL AND kms_provider IS NOT NULL
     AND aad_context IS NOT NULL AND plain_value IS NULL)
  OR
  (NOT sensitive
     AND ciphertext IS NULL AND nonce IS NULL AND aead_algorithm IS NULL
     AND dek_id IS NULL AND dek_version IS NULL AND kms_provider IS NULL
     AND aad_context IS NULL)
);

CREATE UNIQUE INDEX ux_secret_fields_secret_name
  ON secret_fields (secret_id, name)
  WHERE deleted_at IS NULL;
```

Constraints: the envelope `CHECK` and unique index above. FKs `account_id`,
`realm_id`, `secret_id`, and `dek_id` (the `dek_id` FK is on a nullable column,
so it constrains only sensitive rows). Sensitive-field `ciphertext` stays inline
within the 64 KiB sensitive / 256 KiB total inline limits; oversized blobs move
to [`attachments`](#attachments-deferred-but-stubbed) / object storage (see
[secret-size-and-attachments.md](secret-size-and-attachments.md)).

### `secret_grants`

Purpose: cross-agent and group-owned **secret** access grants (explicit,
auditable, never the default). A grant authorizes the authorization layer to
return encrypted material to a grantee; per-field reveal grants remain
authorization checks (not separate crypto boundaries) unless an operator opts a
field into its own DEK. This is the sealed-plane analogue of open-plane
[`policies`](#policies); secrets are NOT subject to the open cross-agent
read/curate/forget verbs.

| Column | Type | Notes |
| --- | --- | --- |
| `id` | `text` PK | `grt_` prefix |
| `account_id` | `text NOT NULL` FK -> `accounts(id)` | |
| `realm_id` | `text NOT NULL` FK -> `realms(id)` | |
| `secret_id` | `text NOT NULL` FK -> `secrets(id)` ON DELETE CASCADE | granted secret |
| `grantee_agent_id` | `text NULL` FK -> `agents(id)` | grantee agent; NULL = group exposure |
| `grantee_group_id` | `text NULL` FK -> `security_groups(id)` | grantee group; NULL = single agent |
| `capabilities` | `text[] NOT NULL` | grant capabilities mapping `--read`/`--reveal`/`--totp`/`--write` to scopes (`secret:show`, `secret:reveal`, `totp:code`, `secret:update`) |
| `field_name` | `text NULL` | set for per-field reveal grants (`--reveal FIELD`); NULL = whole secret |
| `granted_by_operator_id` | `text NULL` FK -> `operators(id)` | who granted (audit) |
| `expires_at` | `timestamptz NULL` | optional grant expiry |
| `row_version` | `bigint NOT NULL DEFAULT 1` | |
| `created_at` / `updated_at` / `deleted_at` | `timestamptz` | |

Constraints: FKs `account_id`, `realm_id`, `secret_id`, `grantee_agent_id`,
`grantee_group_id`; a `CHECK` that at most one grantee discriminator is set.
Grant uniqueness collapses null `field_name`/grantee via an expression index:

```sql
CREATE UNIQUE INDEX ux_secret_grants_unique
  ON secret_grants (
    secret_id,
    coalesce(grantee_agent_id, grantee_group_id, '*'),
    coalesce(field_name, '*')
  )
  WHERE deleted_at IS NULL;
```

Grant changes MUST emit `secret.grant` / `secret.revoke` audit events.

### `totp_enrollments`

Purpose: TOTP enrollment. The encrypted **seed** is high-value sealed material
and stored only as an envelope; the normal agent surface returns generated codes,
never the seed. Non-sensitive TOTP metadata (issuer, account label, algorithm,
digits, period) are ordinary queryable columns. Keyed by `secret_id` so
`POST /v1/totp/{secret_id}:code` resolves directly. Semantics in
[totp-2fa.md](totp-2fa.md).

The whole row is always an envelope, so the seed envelope columns are `NOT NULL`
here (unlike the mixed `secret_fields` table). The TOTP hash algorithm and the
AEAD algorithm are two distinct columns: `algorithm` is the non-sensitive TOTP
hash (`SHA1`/`SHA256`/`SHA512`, matching the `totp.show` JSON field), and
`aead_algorithm` is the envelope's AEAD primitive.

| Column | Type | Notes |
| --- | --- | --- |
| `id` | `text` PK | `totp_` prefix |
| `account_id` | `text NOT NULL` FK -> `accounts(id)` | |
| `realm_id` | `text NOT NULL` FK -> `realms(id)` | |
| `secret_id` | `text NOT NULL` FK -> `secrets(id)` ON DELETE CASCADE | one enrollment per secret |
| `issuer` | `text NULL` | non-sensitive |
| `account_label` | `text NULL` | non-sensitive |
| `algorithm` | `text NOT NULL DEFAULT 'SHA1'` | TOTP hash: `SHA1` \| `SHA256` \| `SHA512` |
| `digits` | `smallint NOT NULL DEFAULT 6` | |
| `period_seconds` | `smallint NOT NULL DEFAULT 30` | |
| `ciphertext` | `bytea NOT NULL` | the encrypted seed (never plaintext, never recalled/exported) |
| `nonce` | `bytea NOT NULL` | per-encryption nonce |
| `aead_algorithm` | `text NOT NULL` | AEAD id: `XCHACHA20_POLY1305` \| `AES_256_GCM` |
| `dek_id` | `text NOT NULL` FK -> `secret_deks(id)` | wrapping DEK row |
| `dek_version` | `bigint NOT NULL` | frozen DEK generation (see envelope notes) |
| `kms_provider` | `text NOT NULL` | root unwrap provider |
| `aad_context` | `jsonb NOT NULL` | domain tag `"totp-seed"` |
| `row_version` | `bigint NOT NULL DEFAULT 1` | |
| `created_at` / `updated_at` / `deleted_at` | `timestamptz` | |

Constraints: FKs `account_id`, `realm_id`, `secret_id`, `dek_id`. One live
enrollment per secret via a partial index:

```sql
CREATE UNIQUE INDEX ux_totp_enrollments_secret
  ON totp_enrollments (secret_id)
  WHERE deleted_at IS NULL;
```

Seed import/setup/export requires a more privileged path than ordinary code
generation; `totp:enroll` vs `totp:code` scopes gate them. The seed is sealed
material: never embedded, recalled, in the digest, or plaintext-exported.

### Sensitive-Field Envelope Storage

Sealed-plane sensitive field values and TOTP seeds are stored as the
per-ciphertext envelope from [key-hierarchy.md](key-hierarchy.md) — multiple
at-rest columns alongside `ciphertext`, field-per-row (one envelope per
`secret_fields` / `totp_enrollments` row). These are at-rest columns, NOT public
response fields; the public contract redacts by default and a value is returned
only through the audited reveal ceremony (`witself secret reveal` / `witself totp
code`; see [encryption-model.md](encryption-model.md)).

The **wrapped DEK lives only in [`secret_deks`](#secret_deks)** and is referenced
from each envelope by `dek_id`; the envelope does **not** carry an inline
`wrapped_dek` copy. KEK re-wrap on rotation updates exactly one `secret_deks`
row. The wrapping KEK is resolved through `secret_deks.kek_id` (the post-rotation
pointer), never by joining a frozen `(realm_id, key_version)` to `realm_keys`.

| Column | Type | Description |
| --- | --- | --- |
| `ciphertext` | `bytea` | AEAD ciphertext of the field value or TOTP seed. Inline within size limits; never a plaintext column. `NOT NULL` only on always-encrypted tables (`totp_enrollments`, `attachments`); nullable on the mixed `secret_fields` table (gated by its `CHECK`). |
| `nonce` | `bytea` | Per-encryption random nonce (24 B XChaCha20-Poly1305 / 12 B AES-GCM); never reused per DEK. Nullability follows `ciphertext`. |
| `aead_algorithm` | `text` | AEAD id: `XCHACHA20_POLY1305` \| `AES_256_GCM`. (Distinct from the TOTP hash `algorithm` column.) |
| `dek_id` | `text` FK -> `secret_deks(id)` | The `dek_` row holding the canonical `wrapped_dek`. Fields sharing a per-secret DEK reference one row. The wrapping KEK is resolved via `secret_deks.kek_id`, updated in place on KEK re-wrap. |
| `dek_version` | `bigint` | The **frozen** DEK generation in force when this blob was written. Recorded for AAD reconstruction and `--version` historical reads; it does NOT identify the current wrapping KEK (that is `secret_deks.kek_id`). **Distinct** from `row_version` and from `realm_keys.key_version`. |
| `kms_provider` | `text` | `aws-kms` \| `gcp-kms` \| `azure-key-vault` \| `local-dev`; surfaces as the `kms_provider` metric label. |
| `aad_context` | `jsonb` | Authenticated associated data (integrity-bound, not encrypted). Bound at encryption from **stable identifiers only**: `realm_id`, `secret_id`, field name, `owner.kind`, `dek_id`, and a domain tag (`"secret-field"` \| `"totp-seed"` \| `"attachment"`). It deliberately excludes any rotating counter (no KEK/realm `key_version`), so KEK rotation provably cannot affect AAD. At decrypt, AAD is reconstructed **strictly from these stored envelope columns**, never from current/live realm key state. |

`value_encoding` (`"plain"` \| `"base64"` \| `null`) is a **client-facing**
reveal/show field only — the decrypted value's encoding on a reveal, or `null`
with `redacted: true` and a `value_ref` on ordinary show. It is NOT an at-rest
column and base64 there is serialization, not a security boundary.

### `realm_keys`

Purpose: per-realm KEK material — the tenant-isolation and rotation unit (the
re-skin of Witpass `vault_keys`; the per-vault KEK becomes a **per-realm** KEK).
The KEK is stored only KMS-wrapped; KMS wraps the KEK (not per-field). Joins
envelopes (via `secret_deks.kek_id`) to their unwrap/rotation root. See
[key-hierarchy.md](key-hierarchy.md).

| Column | Type | Notes |
| --- | --- | --- |
| `id` | `text` PK | `kek_` prefix |
| `account_id` | `text NOT NULL` FK -> `accounts(id)` | |
| `realm_id` | `text NOT NULL` FK -> `realms(id)` | one current KEK per realm, prior versions retained |
| `wrapped_kek` | `bytea NOT NULL` | KEK wrapped by the root CMK |
| `key_version` | `bigint NOT NULL` | monotonic KEK version (per-realm; separate from envelope `dek_version`) |
| `is_current` | `boolean NOT NULL DEFAULT true` | exactly one current live row per realm |
| `kms_provider` | `text NOT NULL` | root unwrap provider |
| `kms_key_ref` | `text NOT NULL` | CMK reference (AWS KMS ARN / GCP / Azure id); never key material |
| `rotation_state` | `text NULL` | re-wrap/backfill bookkeeping |
| `row_version` | `bigint NOT NULL DEFAULT 1` | |
| `created_at` / `updated_at` / `deleted_at` | `timestamptz` | |

Constraints: FKs `account_id`, `realm_id`.

```sql
CREATE UNIQUE INDEX ux_realm_keys_realm_version
  ON realm_keys (realm_id, key_version);

-- exactly one current live KEK row per realm
CREATE UNIQUE INDEX ux_realm_keys_current
  ON realm_keys (realm_id)
  WHERE is_current AND deleted_at IS NULL;
```

KEK rotation creates a new version, re-wraps existing DEKs key-on-key (ciphertext
untouched), repoints each affected `secret_deks.kek_id` at the new KEK row, and
bumps `is_current`. Envelopes are NOT touched by KEK rotation. KMS-loss of a
realm's KEK/CMK material renders its secret values unrecoverable (crypto-shred);
it does **not** affect the open plane (see
[backup-and-recovery.md](backup-and-recovery.md)).

### `secret_deks`

Purpose: per-secret (default) or per-field (opt-in) DEK rows. DEKs are stored
only KEK-wrapped. This table is the **single source of truth** for `wrapped_dek`;
envelopes reference it by `dek_id` and never store a second copy.

| Column | Type | Notes |
| --- | --- | --- |
| `id` | `text` PK | `dek_` prefix |
| `account_id` | `text NOT NULL` FK -> `accounts(id)` | |
| `realm_id` | `text NOT NULL` FK -> `realms(id)` | |
| `secret_id` | `text NULL` FK -> `secrets(id)` ON DELETE CASCADE | owning secret (per-secret DEK) |
| `field_name` | `text NULL` | set for opt-in per-field DEKs (e.g. TOTP seeds) |
| `kek_id` | `text NOT NULL` FK -> `realm_keys(id)` | wrapping KEK; **updated in place on KEK re-wrap** so it always points at the KEK that currently wraps this DEK |
| `wrapped_dek` | `bytea NOT NULL` | canonical wrapped DEK (KEK-wrapped); the only stored copy |
| `key_version` | `bigint NOT NULL` | DEK generation; old versions retained for `--version` reads |
| `is_current` | `boolean NOT NULL DEFAULT true` | current DEK per `(secret, field_name)` |
| `created_at` / `updated_at` / `deleted_at` | `timestamptz` | |

Constraints: FKs `account_id`, `realm_id`, `secret_id`, `kek_id`.

```sql
-- exactly one current live DEK per (secret, field-or-secret-wide)
CREATE UNIQUE INDEX ux_secret_deks_current
  ON secret_deks (secret_id, coalesce(field_name, '*'))
  WHERE is_current AND deleted_at IS NULL;

-- rotation/backfill targeting by secret
CREATE INDEX ix_secret_deks_secret ON secret_deks (secret_id);
```

Prior DEK generations are retained as separate rows (`is_current = false`) so the
`(secret, field)` partial unique index pins exactly one live current DEK while
historical `--version` reads still resolve their generation. KEK rotation
re-wraps `wrapped_dek` and repoints `kek_id` **on this row only** (one write per
DEK); no envelope row is touched.

### `attachments` (deferred-but-stubbed)

Purpose: metadata for encrypted attachments / oversized sealed blobs, per
[secret-size-and-attachments.md](secret-size-and-attachments.md). Attachments are
NOT required for the first v0 slice; the table is created (or stubbed) so the
model does not preclude them, but the object/blob path is not a default
dependency for small secrets. Ciphertext lives in object/blob storage; only
metadata lives here. The whole row is always an envelope, so the envelope columns
are `NOT NULL` (except `ciphertext`, which lives in object storage and is absent
here).

| Column | Type | Notes |
| --- | --- | --- |
| `id` | `text` PK | `att_` prefix |
| `account_id` | `text NOT NULL` FK -> `accounts(id)` | |
| `realm_id` | `text NOT NULL` FK -> `realms(id)` | |
| `secret_id` | `text NULL` FK -> `secrets(id)` ON DELETE CASCADE | owning secret |
| `object_store_provider` | `text NULL` | object/blob provider (`object_store_provider` metric label) |
| `object_ref` | `text NULL` | storage key/URI of the encrypted object |
| `byte_size` | `bigint NOT NULL DEFAULT 0` | counted into `encrypted_storage_byte` metering |
| `nonce` | `bytea NOT NULL` | per-encryption nonce |
| `aead_algorithm` | `text NOT NULL` | AEAD id |
| `dek_id` | `text NOT NULL` FK -> `secret_deks(id)` | wrapping DEK row (canonical `wrapped_dek`) |
| `dek_version` | `bigint NOT NULL` | frozen DEK generation |
| `kms_provider` | `text NOT NULL` | root unwrap provider |
| `aad_context` | `jsonb NOT NULL` | domain tag `"attachment"` |
| `row_version` | `bigint NOT NULL DEFAULT 1` | |
| `created_at` / `updated_at` / `deleted_at` | `timestamptz` | |

Constraints: FKs `account_id`, `realm_id`, `secret_id`, `dek_id`. Attachment
reveal/download must be explicit, authorized, and audited. `encrypted_storage_byte`
usage sums inline ciphertext plus attachment `byte_size`.

## Spine Operational Tables

### `federation_peers` (cross-realm, post-v0)

Purpose: the realm's **deny-by-default federation allow-list** — which peer realm
handles (and their pinned signing keys) this realm accepts cross-realm messages
from. The cross-realm analog of the open-plane [policy](access-policy.md)
allow-list: an inbound cross-realm message carries **no authority** and is
accepted only if its sending handle is allow-listed here AND the message clears
the receiving realm's standing policy. Managed via the `federation:manage` scope
(realm:admin; see [authorization-and-roles.md](authorization-and-roles.md)).
Semantics in [agent-collaboration.md](agent-collaboration.md). Realm-local-only
deployments leave this table empty (no peer is trusted by default).

| Column | Type | Notes |
| --- | --- | --- |
| `id` | `text` PK | `fpr_` prefix |
| `account_id` | `text NOT NULL` FK -> `accounts(id)` | |
| `realm_id` | `text NOT NULL` FK -> `realms(id)` | the local realm whose allow-list this is |
| `peer_realm_handle` | `text NOT NULL` | the allow-listed peer realm's `realm_handle` (the unit of trust) |
| `peer_signing_key` | `text NULL` | pinned peer signing **public key** / JWKS (or ref) used to verify the peer's envelopes; NULL = resolve from the peer's published card on each verify |
| `peer_key_version` | `bigint NULL` | pinned peer signing-key generation, for rotation overlap |
| `consent_state` | `text NOT NULL DEFAULT 'allowed'` | `allowed` \| `revoked`; per-conversation consent is layered on top (see `conversations`) |
| `direction` | `text NOT NULL DEFAULT 'both'` | `inbound` \| `outbound` \| `both` — which edge(s) this peer is trusted on |
| `added_by_operator_id` | `text NULL` FK -> `operators(id)` | who allow-listed the peer (audit) |
| `row_version` | `bigint NOT NULL DEFAULT 1` | optimistic lock |
| `created_at` / `updated_at` / `deleted_at` | `timestamptz` | |

Constraints: FKs `account_id`, `realm_id`, `added_by_operator_id`. One live entry
per `(realm, peer handle)`:

```sql
CREATE UNIQUE INDEX ux_federation_peers_realm_peer
  ON federation_peers (realm_id, peer_realm_handle)
  WHERE deleted_at IS NULL;
```

Allow-list changes emit `federation.peer_allowed` / `federation.peer_denied` (and
`federation.consent_accepted` for per-conversation consent); the canonical names
land in [audit-retention.md](audit-retention.md) on the contract pass. Trust is
the **handle + pinned key**, never the routing endpoint — compromising routing
must not let an attacker into the allow-list (see
[threat-model.md](threat-model.md)).

### `audit_events`

Purpose: the platform audit trail across both planes. Append-only; emitted by the
core service or the audit layer immediately below it, never by transport
adapters. Required for **open-plane** cross-agent read/contribute/curate/forget,
policy/group/message events, and destructive identity changes, and for
**sealed-plane** reveal, TOTP code generation, grant changes, key rotation, and
server-side decrypt — plus token lifecycle, billing mutations, and
support-sensitive ops. Default retention 365 days (managed and self-hosted Helm;
operator-configurable; minimum 90d). Audit is itself a metered dimension
(`audit_event`). See [audit-retention.md](audit-retention.md).

Audit `action` uses the dotted `<resource>.<verb>` namespace, **distinct** from
the colon-delimited `<resource>:<action>` scope strings on tokens/grants; the two
MUST NOT be conflated. Stable actions span both planes:

- Open plane: `memory.added`, `memory.adjusted`, `memory.recalled`,
  `memory.forgotten`, `memory.restored`, `memory.deleted`, `memory.consolidated`,
  `fact.created`, `fact.updated`, `fact.deleted`, `fact.primary_changed`,
  `crossagent.read`,
  `crossagent.contributed`, `crossagent.curated`, `crossagent.forgotten`,
  `policy.created`, `policy.deleted`, `policy.access_denied`, `group.created`,
  `group.member_added`, `group.member_removed`, `message.sent`,
  `message.delivered`, `message.read`, `message.acked`. The `fact set` /
  `remember` upsert emits `fact.created` for a new fact or `fact.updated` for an
  existing one.
- Sealed plane: `secret.created`, `secret.updated`, `secret.renamed`,
  `secret.copied`, `secret.archived`, `secret.restored`, `secret.deleted`,
  `secret.reveal`, `secret.grant`, `secret.revoke`, `totp.enrolled`, `totp.code`,
  `totp.seed_revealed`, `totp.deleted`, `key.rotated` (KEK).
- Cross-realm collaboration (post-v0; emitted only when federation is enabled,
  see [agent-collaboration.md](agent-collaboration.md)): the conversation/task
  lifecycle `conversation.started`, `conversation.state_changed`,
  `conversation.completed`, `conversation.failed`, `conversation.canceled`; the
  federation allow-list `federation.peer_allowed`, `federation.peer_denied`,
  `federation.consent_accepted`; and the loop/budget governors
  `budget.exhausted`, `loop.suspended`. Realm-local `message.*` is unchanged and
  v0; only these cross-realm actions are deferred.

| Column | Type | Notes |
| --- | --- | --- |
| `id` | `text` PK | `aud_` prefix |
| `account_id` | `text NOT NULL` FK -> `accounts(id)` | |
| `realm_id` | `text NULL` FK -> `realms(id)` | NULL for account-level events |
| `action` | `text NOT NULL` | dotted namespace, e.g. `secret.reveal`, `memory.forgotten` |
| `actor_kind` / `actor_id` / `actor_name` | `text` | maps to `actor{kind,id,name}` (`agent`\|`operator`\|`admin`\|`service`) |
| `target_kind` / `target_id` / `target_name` / `target_field` | `text NULL` | maps to `target{kind,id,name,field}` |
| `owner_kind` / `owner_agent_id` / `owner_group_id` / `owner_name` | `text NULL` | owning agent/group context (`owner_kind ∈ {agent, group}`) |
| `policy_id` | `text NULL` FK -> `policies(id)` | deciding policy id for open-plane cross-agent actions |
| `grant_id` | `text NULL` FK -> `secret_grants(id)` | deciding grant id for sealed-plane cross-agent access |
| `result` | `text NULL` | success / error / denied / rate_limited / unsupported |
| `reason` | `text NULL` | required audit reason for operator/admin + cross-agent action + server-side decrypt |
| `request_id` | `text NULL` | redacted client context |
| `provider_event_id` | `text NULL` | provider event id for billing/payment/crypto reconciliation |
| `server_side_decrypt` | `boolean NOT NULL DEFAULT false` | distinguishes the sealed-plane server-side decrypt exception |
| `metadata` | `jsonb NOT NULL DEFAULT '{}'` | non-sensitive context only |
| `timestamp` | `timestamptz NOT NULL` | event time |

Constraints: FK `account_id`. Retention enforced by a scheduled delete on
`timestamp`. **Audit rows MUST NEVER store** secret values, TOTP seeds, generated
codes, memory content, fact values, message bodies/payloads, embedding vectors,
raw tokens, passphrases, or plaintext private keys. `metadata` carries only
non-sensitive request context. There is no `deleted_at` — audit is append-only
and removed only by retention.

```sql
CREATE INDEX ix_audit_realm_ts         ON audit_events (realm_id, timestamp);
CREATE INDEX ix_audit_account_ts       ON audit_events (account_id, timestamp);
CREATE INDEX ix_audit_realm_actor_ts   ON audit_events (realm_id, actor_id, timestamp);
CREATE INDEX ix_audit_realm_owner_ts   ON audit_events (realm_id, owner_agent_id, timestamp);
CREATE INDEX ix_audit_realm_target_ts  ON audit_events (realm_id, target_id, timestamp);
CREATE INDEX ix_audit_realm_action_ts  ON audit_events (realm_id, action, timestamp);
```

### `usage_counters`

Purpose: per-realm, per-dimension metered usage and rate-limit state. Feeds
`/v1/capabilities` limits and `/v1/billing/usage`. Caps (`max`/`used`) and rate
windows are persisted per realm per dimension. See
[billing-and-limits.md](billing-and-limits.md).

The canonical `usage_dimension` values are the open-plane/spine dimensions
(`active_agent`, `stored_memory`, `stored_fact`, `memory_recall`, `memory_write`,
`embedding_operation`, `vector_storage_byte`, `crossagent_access`,
`security_group`, `message_sent`, `message_delivered`, `storage_byte`,
`api_request`, `audit_event`) plus the **sealed-plane** dimensions added by the
consolidation (`stored_secret`, `secret_read` — reveal + reference resolution,
`totp_code`, `runtime_injection`, `encrypted_storage_byte`). The sealed-plane
dimensions are metered only when the sealed plane is enabled.

| Column | Type | Notes |
| --- | --- | --- |
| `id` | `text` PK | `usg_` prefix |
| `account_id` | `text NOT NULL` FK -> `accounts(id)` | |
| `realm_id` | `text NOT NULL` FK -> `realms(id)` | billing target (rolls up to account) |
| `dimension` | `text NOT NULL` | one of the canonical dimensions |
| `window_start` | `timestamptz NULL` | NULL for point-in-time caps; set for rate windows |
| `used` | `bigint NOT NULL DEFAULT 0` | current usage / count |
| `soft_limit` | `bigint NULL` | rate `included`/`soft_limit` |
| `hard_limit` | `bigint NULL` | cap `max` / rate `hard_limit` |
| `overage_behavior` | `text NULL` | `warn` \| `throttle` \| `block` |
| `row_version` | `bigint NOT NULL DEFAULT 1` | atomic increment / conflict guard |
| `created_at` / `updated_at` | `timestamptz` | |

Constraints: FKs `account_id`, `realm_id`. The `(realm, dimension, window)`
uniqueness uses an expression on the nullable `window_start`:

```sql
CREATE UNIQUE INDEX ux_usage_counters_dim_window
  ON usage_counters (realm_id, dimension, coalesce(window_start, 'epoch'::timestamptz));
```

Enforcement surfaces `rate_limited` (retryable, with `retry_after`) for throttle
overage and `limit_exceeded` (non-retryable) for block.

### `idempotency_keys`

Purpose: dedupe retryable mutating POSTs (`Idempotency-Key` header). Records MUST
NOT store raw secrets, raw tokens, memory content, fact values, payment details,
or provider secrets in the cached response.

| Column | Type | Notes |
| --- | --- | --- |
| `id` | `text` PK | `idem_` prefix |
| `account_id` | `text NOT NULL` FK -> `accounts(id)` | |
| `realm_id` | `text NULL` FK -> `realms(id)` | scope (realm + key) |
| `idempotency_key` | `text NOT NULL` | caller-supplied header value |
| `request_hash` | `bytea NOT NULL` | hash of method + route + body to detect key reuse with a different payload |
| `response_snapshot` | `jsonb NULL` | redacted response (never raw token/secret/content/payment material) |
| `status_code` | `integer NULL` | replayed status |
| `expires_at` | `timestamptz NOT NULL` | retention TTL |
| `created_at` | `timestamptz NOT NULL` | |

Constraints: `UNIQUE (realm_id, idempotency_key)`. FKs `account_id`, `realm_id`.
Reuse with a mismatched `request_hash` is a `conflict`. Expired rows are pruned
by retention.

## Optimistic Concurrency And History

Every mutable resource EXCEPT `agent_tokens` carries a `row_version bigint NOT
NULL DEFAULT 1` column (`accounts`, `operators`, `realms`, `realm_members`,
`agents`, `memories`, `facts`, `policies`, `security_groups`, `group_members`,
`message_deliveries`, `conversations`, `federation_peers`, `secrets`,
`secret_fields`, `secret_grants`, `totp_enrollments`, `realm_keys`,
`attachments`, `usage_counters`).
`agent_tokens` is excluded because token mutations use create/rotate/revoke
semantics (`revoked_at`), not `If-Match`/`row_version`. `memory_versions` /
`fact_versions`, `messages`, and `audit_events` are append-only.

- The contract is **If-Match / 409**: a mutation supplies the expected
  `row_version`; the update runs as `UPDATE ... SET ..., row_version = row_version
  + 1 WHERE id = $1 AND row_version = $expected`. Zero rows affected means a stale
  write -> `conflict` (exit 6, HTTP 409).
- `row_version` is kept strictly **distinct** from memory `version` (content
  generation), envelope `dek_version` (frozen DEK generation), and
  `realm_keys.key_version` (per-realm KEK generation) — one guards lost updates,
  the others govern content history and rotation/decryptability.
- **History in v0.** The open plane keeps full versioned edit history
  (`memory_versions` / `fact_versions`) plus `row_version`, `created_at` /
  `updated_at`, and the audit trail. The sealed plane keeps `row_version` +
  audit; a dedicated `secret_versions` history table is an
  [Open Decision](#open-decisions). The `--version` historical-read affordance,
  retained prior-generation `secret_deks` rows, and frozen `dek_version`
  ciphertext are forward-compatible hooks. Any historical sensitive values stay
  protected by the same reveal rules as current values.

## Soft Delete And Tombstones

Resource tables use `deleted_at timestamptz NULL` for soft delete; ordinary reads
filter `deleted_at IS NULL`, and the partial unique indexes above are scoped to
live rows so a deleted name can be reused. `memories` additionally carries a
`state` (`active`/`forgotten`/`deleted`) for the reversible `forget` lifecycle;
`secrets` additionally has `archived_at` for archive/restore (distinct from
delete). `memory_versions`/`fact_versions`, `messages`, and `audit_events` are
append-only with no `deleted_at` (audit is removed only by the 365-day retention
sweep). Token tombstones may retain metadata for audit, but raw token values are
never retained. Permanent delete of an agent invalidates and may tombstone its
tokens.

## Account Deletion, PII, And Erasure

Account/realm/operator deletion is a soft-delete tombstone (`deleted_at`), and
the audit trail is append-only PII-bearing (`actor_name`, `target_name`,
`owner_name`, `request_id`) on top of `operators.email`. v0 posture:

- **v0 default: deferred right-to-erasure with a stated retention exception.** On
  account closure, sealed-plane secret material can be crypto-shredded by
  destroying the realm KEK/CMK material (rendering ciphertext unrecoverable; see
  [key-hierarchy.md](key-hierarchy.md)). Open-plane identity content is ordinary
  data and is removed by deleting/purging its rows. **Crypto-shredding does NOT
  cover metadata/PII**: operator email and the `*_name` columns in
  `audit_events`, plus tombstoned operator/account rows, remain for up to the
  365-day audit retention window under the security/legal-hold exception. This
  GDPR/erasure tension is a **known, accepted gap** for v0
  ([Open Decision](#open-decisions)).
- **Alternative (post-v0, or if required at launch):** a hard-delete /
  anonymization path that pseudonymizes or purges PII fields (`operators.email`,
  audit `actor_name` / `target_name` / `owner_name` / `request_id`) while
  preserving audit integrity (ids, actions, timestamps), then drops tombstoned
  rows.

Crypto-shredding addresses sealed-plane secret values only; open-plane content
purge and PII erasure are separate actions the chosen path must explicitly
perform. See [backup-and-recovery.md](backup-and-recovery.md).

## Multi-Tenant Query Scoping

Realm is the single isolation/billing/key-separation scope. The rule:

- Every row below the account carries `account_id`; every row below the realm
  carries `realm_id`. The shared authorization layer resolves the bearer token to
  exactly one `(account, realm, named agent)` and MUST add `account_id = $acct
  AND realm_id = $realm` predicates to every query — including reads, writes,
  recall, audit, and usage. No query may span realms except explicit
  operator/admin cross-realm administration gated by `realm:admin` /
  `account:manage`.
- **Open vs sealed isolation.** Open-plane content is isolated by query scoping
  and [policy](access-policy.md) evaluation (default-deny). Sealed-plane content
  adds cryptographic binding: the per-realm KEK + KMS encryption-context binds
  each wrapped KEK to its own realm, so a wrapped blob from one realm cannot be
  *confused* for another's. Under the v0 single-CMK + single deployment-IAM
  model, that context does NOT scope the deployment role's *authority*: a holder
  of the one role can unwrap any realm's KEK by supplying that realm's (non-secret)
  `realm_id` context. Cross-realm isolation against a compromised server/role is
  therefore **authorization/operational, not cryptographic**, with a realm-wide
  blast radius (see [key-hierarchy.md](key-hierarchy.md) and
  [threat-model.md](threat-model.md)). Query scoping is the enforced isolation
  layer in v0; per-realm cryptographic isolation against the role is deferred
  pending per-realm KMS grants.
- Managed (multi-tenant `realm_id`-scoped shared tables) and self-hosted
  (typically single-tenant) share the identical model; `realm_id` MUST NOT appear
  in high-cardinality metric labels. The `owner_kind` Prometheus label values are
  `agent` / `group`, matching the storage/JSON discriminator (see
  [observability-and-operations.md](observability-and-operations.md)).

## Migrations

Migrations use **Goose**, owned by the separate `witself-server` binary (the
public `witself` CLI MUST NOT manage migrations). SQL migrations live in the
public repo. See [storage.md](storage.md) and
[server-command-surface.md](server-command-surface.md).

- Commands: `witself-server migrate status` (current + pending), `migrate up`
  (forward), `migrate down` (guarded; explicit confirmation when
  destructive/backward). Migration commands acquire a Postgres advisory lock.
  Helm exposes an explicit migration Job; production guidance prefers running
  migrations as a controlled step before rolling `witself-server`. CI validates
  ordering and applies against a test Postgres instance with `pgvector`
  available.
- **Expand/contract.** Schema changes follow expand → migrate/backfill →
  contract: additive changes (new nullable columns, new tables, new indexes built
  `CONCURRENTLY`) ship first and are forward-compatible; destructive changes ship
  only after all running instances no longer depend on the old shape. The
  `pgvector` extension + the memory vector column are an expand-phase migration
  ordered before any vector use. The sealed-plane envelope columns, `realm_keys`,
  and `secret_deks` are introduced as expand-phase additive migrations; KEK
  rotation backfills are metadata-only re-wrap jobs, not schema migrations.
- **Destructive-down classification.** Each migration is classified `reversible`,
  `lossy`, or `destructive`. `lossy`/`destructive` downs are guarded, require
  explicit confirmation, and SHOULD be avoided in managed production in favor of a
  forward corrective migration. Migration state is observable via
  `witself_migration_version` and `witself_migration_pending`.

## Open Decisions

Owner / engineering sign-off items.

- **Secret history retention.** Whether v0 ships a dedicated `secret_versions`
  history table (immutable prior versions, retained prior-generation
  `secret_deks` + frozen `dek_version` ciphertext, `--version` reads) or keeps the
  lighter `row_version` + audit-trail model with sealed-plane history deferred
  post-v0. (The open plane already ships `memory_versions` / `fact_versions`.)
- **GDPR / right-to-erasure vs 365-day audit retention.** Confirm the v0 posture:
  deferred erasure with audit/operator PII retained up to 365 days under the
  security/legal-hold exception (accepted gap), or ship a
  hard-delete/anonymization path on account closure.
- **Field-per-row envelope vs JSON envelope.** This doc proposes one envelope per
  `secret_fields` row (queryable, per-field rotation/grant granularity). The
  alternative is a single JSONB envelope blob per secret.
- **Token hash scheme.** Finalize the `token_hash` / `hash_alg` algorithm
  (SHA-256 vs HMAC-with-pepper) and the `token_lookup` shape.
- **DEK granularity policy.** Per-secret DEK default with opt-in per-field DEK for
  TOTP seeds / highest-value fields, and whether per-field reveal grants ever
  imply per-field DEKs or remain pure authorization checks.
- **Vector index type/params.** HNSW vs IVFFlat (and parameters) for the
  `memory_embeddings.embedding` index, tuned per [memory-model.md](memory-model.md).
- **Id format.** ULID vs UUIDv7 vs random for the body under each prefix.
- **Usage-counter granularity.** Single rolling row per `(realm, dimension)` vs
  windowed rows per `(realm, dimension, window_start)` for rate dimensions.
- **Native enum vs CHECK.** Whether to keep `text` + `CHECK` (cheaper
  expand/contract) or adopt native Postgres enums for the stable enumerations.
- **Cross-realm peer-key storage.** Whether `federation_peers` **pins** a peer's
  `peer_signing_key` (explicit rotation handling, key-continuity guarantees) or
  always resolves the key from the peer's published card at verify time (simpler,
  no local rotation tracking) — the columns above support either. Per
  [agent-collaboration.md](agent-collaboration.md).
- **Cross-realm conversation projection.** Whether `conversations` is a
  materialized row updated on each transition (queryable, listable by state) or a
  pure projection reconstructed from the durable mailbox on read (the mailbox is
  authoritative either way). Per [agent-collaboration.md](agent-collaboration.md).

## Related Docs

- [memory-model.md](memory-model.md)
- [facts-model.md](facts-model.md)
- [access-policy.md](access-policy.md)
- [security-groups.md](security-groups.md)
- [inter-agent-messaging.md](inter-agent-messaging.md)
- [agent-collaboration.md](agent-collaboration.md)
- [deployment-cells.md](deployment-cells.md)
- [context-hydration.md](context-hydration.md)
- [encryption-model.md](encryption-model.md)
- [key-hierarchy.md](key-hierarchy.md)
- [authorization-and-roles.md](authorization-and-roles.md)
- [secret-size-and-attachments.md](secret-size-and-attachments.md)
- [totp-2fa.md](totp-2fa.md)
- [storage.md](storage.md)
- [json-contracts.md](json-contracts.md)
- [api-contract.md](api-contract.md)
- [api-routes.md](api-routes.md)
- [token-lifecycle.md](token-lifecycle.md)
- [backend-architecture.md](backend-architecture.md)
- [billing-and-limits.md](billing-and-limits.md)
- [audit-retention.md](audit-retention.md)
- [backup-and-recovery.md](backup-and-recovery.md)
- [observability-and-operations.md](observability-and-operations.md)
- [operator-auth.md](operator-auth.md)
- [threat-model.md](threat-model.md)
- [requirements.md](requirements.md)
