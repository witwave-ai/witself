# Witself Authorization And Roles

Status: draft. Decision: v0 uses **lean fixed-role bundles** on the human plane —
four account-level roles stored on `operators.account_role` and four realm-level
roles stored verbatim in `realm_members.role`, each an immutable named bundle that
expands to a frozen scope set — composed with a **token-bound agent plane** whose
effective access to any datum is the deterministic intersection
`token_scopes ∩ ownership ∩ grants`. Roles are necessary-but-never-sufficient:
holding a scope never reaches another principal's data without ownership, a
grant (sealed plane), or a Policy ([access-policy.md](access-policy.md), open plane).
The `realm_members.scopes[]` composability hook exists in the schema but is
**reserved/frozen-empty** for v0; custom per-member scopes, `denied_scopes[]`,
fine-grained scope editing, and SCIM are deferred to v1.

This is the consolidated authorization model for **both planes** of Witself:

- **Open plane** (memories + facts) — integrity/authenticity of identity data,
  with cross-agent access governed by the declarative Policy engine in
  [access-policy.md](access-policy.md), which **composes with** the roles and
  scopes defined here.
- **Sealed plane** (secrets + TOTP) — confidentiality of credential material,
  with cross-agent/operator access governed by per-secret **grants** plus realm
  roles. Sealed-plane data is never embedded, never returned by semantic recall,
  never in the self-digest, and never plaintext-exported (see
  [secret-model.md](secret-model.md), [encryption-model.md](encryption-model.md)).

This doc owns the authorization resolution algorithm that sits in the shared core
below CLI/MCP/HTTP (see [data-model.md](data-model.md) Multi-Tenant Query Scoping,
[requirements.md](requirements.md) Authorization and Scopes,
[token-lifecycle.md](token-lifecycle.md) Scopes). It enumerates the roles that the
rest of the doc-set references (`--role ROLE`, `realm:admin`/`realm:member`), the
role→scope expansion, the account-vs-realm composition, and the bootstrap operator
grant.

## Principals And Principal Kinds

Every authenticated request resolves to exactly one principal, whose `principal_kind`
is one of the four canonical values from [data-model.md](data-model.md) Enumerations —
`agent`, `operator`, `admin`, `service` — and which becomes `actor.kind` on the audit
event. Authorization branches on this kind first.

| Principal | `principal_kind` | Identity source | Authority source | Notes |
| --- | --- | --- | --- | --- |
| Account owner / delegated account admin (human) | `admin` | `operators` row (browser OAuth/OIDC+PKCE or device code) | `operators.account_role` ∈ {`account_owner`, `account_admin`} + implicit `realm:admin` on every realm | Member/role management and cross-realm administration. |
| Realm/billing/account operator (human) | `operator` | `operators` row | `operators.account_role` (non-admin) UNION `realm_members.role` for each realm | No member/role management; no cross-realm reach unless granted `realm:admin` per realm. |
| Named agent (machine) | `agent` | `agent_tokens` row bound server-side to one `(account_id, realm_id, agent_id)` | `agent_tokens.scopes[]` over its OWN data plus explicit grants (sealed) and Policy allows (open) | NOT an operator role; per-agent default isolation; never holds a `realm_members` role for human-style authority. |
| Service token (machine) | `service` | `agent_tokens` row, operator-issued, tenant-bound | `agent_tokens.scopes[]`, a SUBSET of the issuing operator's effective scopes | Unattended operator automation (CI). No human OAuth identity, no `realm_members` row, no escalation via token minting. |

The token determines the authenticated identity; a caller-supplied `--agent`,
`--owner-agent`, `--owner-group`, or MCP `owner_agent` is **targeting input for
authorized operator/admin actions only** and is never proof of identity
([token-lifecycle.md](token-lifecycle.md) Scopes). An agent token that passes
`--agent`/`--owner-agent` has those values ignored as identity and rejected as
targeting (an agent token has no cross-agent authority to target).

The operator-vs-admin distinction is concrete: **admin** is the `principal_kind` for
`account_owner`/`account_admin` and adds `account:manage`, member/role management, and
cross-realm `realm:admin`. **operator** is everyone else — realm-scoped or
billing-scoped, no member/role management, no cross-realm reach unless granted
`realm:admin` on a specific realm.

## Canonical Scope Vocabulary

Scope strings use the colon-delimited `<resource>:<action>` vocabulary reused verbatim
from [data-model.md](data-model.md) Enumerations and stored as `text[]`. Audit `action`
strings use a **distinct** dotted `<resource>.<verb>` namespace and MUST NOT be
conflated with scopes. Realm roles deliberately reuse the scope namespace
(`realm:admin`/`realm:member` are simultaneously scope tokens AND `realm_members.role`
values, because the schema already stores them that way); account roles use a separate
role namespace (below) so no one mistakes a role for a single scope.

### Open-plane scopes (memories + facts + identity)

| Scope | Authorizes |
| --- | --- |
| `memory:create` | Create an own memory. |
| `memory:read` | Read/recall own memories (semantic recall over own store). |
| `memory:update` | Update own memory fields/salience/links. |
| `memory:forget` | Soft-delete (tombstone) own memories within the retention window. |
| `memory:read-others` | Cross-agent **read** umbrella (memories AND facts) — necessary but not sufficient; the `read` verb still requires a matching Policy ([access-policy.md](access-policy.md)). |
| `memory:manage-others` | Cross-agent **contribute/curate/forget** umbrella (memories AND facts) + hard delete — Policy-gated; an own-data scope never authorizes a cross-agent mutation. |
| `fact:create` | Create an own fact. |
| `fact:read` | Read own facts (including `sensitive` facts; redaction is a display flag, not a reveal ceremony). |
| `fact:update` | Update an own fact value/namespace. |
| `fact:delete` | Soft-delete an own fact. |
| `fact:primary` | Set/clear the `primary` flag on an own fact. |
| `group:member` | Act as a group member for group-owned memories/facts. |
| `group:read` | Read group membership/metadata. |
| `group:manage` | Create/rename/delete groups and manage membership. Operator/admin or delegated. |
| `policy:read` | Test/list/show cross-agent Policy objects. |
| `policy:manage` | Create/delete cross-agent Policy objects. Not in the default agent bundle. |
| `message:send` | Send an inter-agent message. A message never itself authorizes a write ([inter-agent-messaging.md](inter-agent-messaging.md)). |
| `message:read` | Read own mailbox. |

There is no `fact:read-others` or `fact:manage-others`: the two cross-agent
umbrellas (`memory:read-others`, `memory:manage-others`) each span **both** memories
and facts, exactly as in [access-policy.md](access-policy.md) Capability Gating.

### Sealed-plane scopes (secrets + TOTP)

| Scope | Authorizes |
| --- | --- |
| `secret:create` | Create a secret. Agent: own (`owner_kind='agent'`, `owner_agent_id=self`) secrets only; operator: gated by realm role + targeting. |
| `secret:show` | Show redacted secret metadata/inventory. Grant capability `--read` maps here. Realm-wide `--all-agents` inventory additionally requires `realm:admin`. NEVER returns plaintext field values. |
| `secret:reveal` | Reveal a sealed field plaintext value via the audited reveal ceremony. Grant capability `--reveal` maps here; per-field grants narrow by `secret_grants.field_name`. The only open/sealed verb that returns secret plaintext. |
| `secret:update` | Update secret fields/description. Grant capability `--write` maps here. |
| `secret:delete` | Delete (or archive) a secret. Not grantable to an agent token; operator-gated. |
| `secret:grant` | Grant/revoke cross-agent or group-owned access to a secret (writes/tombstones `secret_grants`). The sealed plane has no Policy engine; cross-agent secret reach is grants + realm roles only. |
| `totp:enroll` | TOTP enrollment / seed import-setup-backup-export (the privileged seed path). The seed is high-value sealed material; never returns codes-only; distinct from `totp:code`. |
| `totp:code` | Generate a current TOTP one-time code (seed never returned). Grant capability `--totp` maps here. |

Sealed-plane scopes never reach the open-plane cross-agent verbs and vice versa: a
secret is **never** subject to the open `read`/`contribute`/`curate`/`forget` Policy
verbs ([access-policy.md](access-policy.md)). Conversely `memory:read-others` /
`memory:manage-others` never reach a secret value.

### Operator / account scopes (both planes)

| Scope | Authorizes |
| --- | --- |
| `agent:manage` | Agent lifecycle: create, rename, copy, disable, enable, archive, delete. Operator/admin only; not in default agent bundle. |
| `token:manage` | Token lifecycle: create, rotate, revoke (agent and operator/service tokens). Operator/admin only. |
| `audit:read` | Read the audit trail (`audit_events`) within scope (realm for realm roles; account-wide for account roles that carry it). |
| `account:read` | Read customer account details/members. |
| `account:manage` | Manage account details, members, and roles; gates cross-realm administration alongside `realm:admin`. |
| `billing:read` | Read billing/plan/usage/invoices. No audit reason required. |
| `billing:manage` | Mutate billing/subscription/payment-method/refund. Requires audit reason + confirmation. |
| `support:read` | Read support tickets. |
| `support:manage` | Open/comment/close support tickets and support-sensitive actions (redact secret material by default). |
| `federation:manage` | Manage the realm's cross-realm **federation allow-list** / trust registry (which peer realm handles + signing keys this realm accepts) and **publish/rotate the signed realm card** served at `/.well-known/witself-card.json`. Operator scope; not in the default agent bundle. Cross-realm SEND is unaffected — it still uses `message:send` gated by the federation allow-list + per-conversation consent (see [agent-collaboration.md](agent-collaboration.md)). |
| `realm:admin` | Realm-wide administration: realm-wide inventory (`--all-agents`), cross-agent ops, agent/token lifecycle within the realm, realm rename/archive/delete, and (combined with `account:manage`) cross-realm administration. Operator override for the open plane ([access-policy.md](access-policy.md) Operator Override) and the elevated realm role across BOTH planes. Also the literal `realm_members.role` value for the elevated realm role. |
| `realm:member` | Non-admin realm membership marker. Used only as a `realm_members.role` value (and read-only namespace anchor); confers minimal presence, not a power scope on its own. |

`realm:operator` and `realm:auditor` are **not** new scopes — they are role-namespace
values composed of existing scopes (below). The scope CHECK list is unchanged from
[data-model.md](data-model.md).

## Account-Level Roles

Account roles are immutable named bundles stored single-valued on the
`operators.account_role` text column (one role per operator per account; see
[Revises / Reconciliation](#revises--reconciliation)). They are the floor an account
invite lands in; real realm power for non-admins comes from per-realm membership.

| `account_role` | `principal_kind` | Scope bundle |
| --- | --- | --- |
| `account_owner` | `admin` | `account:read`, `account:manage`, `billing:read`, `billing:manage`, `support:read`, `support:manage`, `audit:read`, `realm:admin` |
| `account_admin` | `admin` | `account:read`, `account:manage`, `billing:read`, `support:read`, `support:manage`, `audit:read`, `realm:admin` |
| `account_billing` | `operator` | `account:read`, `billing:read`, `billing:manage`, `support:read` |
| `account_member` | `operator` | `account:read` |

- **`account_owner`** — root of the account. Carries `billing:manage`
  (subscription/payment/refund/account-close) and implicit `realm:admin` on EVERY
  realm in the account with no `realm_members` row needed. The first bootstrapped
  operator is provisioned exactly as `account_owner`. At least one `account_owner` MUST
  always exist; the last one cannot be removed or demoted.
- **`account_admin`** — delegated administrator. Identical to `account_owner` EXCEPT no
  `billing:manage` (cannot mutate subscription/payment/refund or close the account) and
  cannot remove/demote the last owner. Manages members, realm creation, and cross-realm
  administration.
- **`account_billing`** — finance/billing operator. Mutates billing (with an audit
  reason) but has NO realm, agent, memory/fact, secret, token, or member-management
  authority.
- **`account_member`** — least-privilege default for any invited human. Carries NO
  realm authority by itself; must additionally hold a `realm_members` row to act in a
  realm.

## Realm-Level Roles

Realm roles are immutable named bundles stored verbatim in `realm_members.role`. They
contribute open-plane (memory/fact/policy/group/message), sealed-plane
(secret/totp/grant), and realm/agent/token scopes; billing/account/support scopes come
solely from the account layer. The elevated realm roles carry **both** planes so an
operator can administer identity data and credential material within one realm.

| `realm_members.role` | Scope bundle |
| --- | --- |
| `realm:admin` | Open: `memory:read-others`, `memory:manage-others`, `group:read`, `group:manage`, `policy:read`, `policy:manage`, `message:read`. Sealed: `secret:create`, `secret:show`, `secret:reveal`, `secret:update`, `secret:delete`, `secret:grant`, `totp:enroll`, `totp:code`. Operator: `agent:manage`, `token:manage`, `audit:read`, `federation:manage`, `realm:admin` |
| `realm:operator` | Open: `memory:read-others`, `memory:manage-others`, `group:read`, `policy:read`, `message:read`. Sealed: `secret:create`, `secret:show`, `secret:reveal`, `secret:update`, `secret:grant`, `totp:enroll`, `totp:code`. Operator: `audit:read`, `federation:manage` |
| `realm:auditor` | Open: `memory:read-others` (read/inspect only), `group:read`, `policy:read`. Sealed: `secret:show`. Operator: `audit:read` |
| `realm:member` | Open: `memory:read`, `fact:read`, `group:member`, `message:read`. Sealed: `secret:show`, `secret:create` |

- **`realm:admin`** — full operator authority within ONE realm: realm-wide inventory
  (`--all-agents`), cross-agent open-plane curate/forget (Policy-gated, operator
  override available), cross-agent sealed show/reveal/update/delete/grant with explicit
  `--owner-agent`/`--owner-group` targeting, group-owned ops, agent + token lifecycle,
  realm rename/archive/delete, TOTP enroll/code across agents.
- **`realm:operator`** — day-to-day curation and reveal across agents (with
  `--owner-agent`/`--owner-group` + `--reason`) plus `federation:manage` (manage the
  federation allow-list and publish/rotate the realm card) but CANNOT run `agent:manage`,
  `token:manage`, `secret:delete`, `policy:manage`, or realm rename/delete. Sits between
  `realm:auditor` and `realm:admin`. (Whether `realm:operator` carries `federation:manage`
  or that scope is reserved to `realm:admin` is an Open decision below.)
- **`realm:auditor`** — read-only compliance/inspection: lists/scans realm-wide
  redacted inventory (both planes), reads the audit trail and Policy set, but cannot
  reveal secret field values, mutate, grant, curate/forget, or run TOTP.
- **`realm:member`** — minimal non-admin membership, the canonical data-model non-admin
  value and the default a realm invite lands in. Confers nothing cross-agent; scoped to
  group-owned read plus the operator's own working set.

`memory:read-others`/`memory:manage-others` in a realm-role bundle make the operator
*eligible* for cross-agent open-plane access; the specific cross-agent action still goes
through Policy default-deny (or operator override on `realm:admin`) per
[access-policy.md](access-policy.md). The sealed scopes in the same bundle reach a
non-self secret only via explicit targeting + grant/role, never via Policy.

## Role→Scope Expansion And Custom Scopes

Roles are **immutable named bundles in v0** — a static lookup table compiled into the
shared authorization layer that maps a role string to a frozen scope set (exactly the
bundles above). There is NO per-member scope editing, NO custom scopes, and NO
`denied_scopes[]` in v0.

- The `realm_members.scopes[]` column exists in the schema but is **RESERVED/frozen-
  empty**: the v0 CLI never writes it and the authz layer MUST treat it as the empty
  set. A runtime guard (and ideally a `CHECK` or assertion) MUST treat any non-empty
  `realm_members.scopes[]` as a config error in v0. It is the documented, additive-only
  forward-compat hook for v1 composability (e.g. give a `realm:operator` a one-off
  `secret:reveal` without inventing a role).
- Resource-dimension constraints (realm, owning-agent/group, memory kind/tag, fact
  name, secret-name, field-name, template) are NOT expressed as custom scope strings in
  v0; they are carried **structurally**: realm by the token's `(account_id, realm_id)`
  binding; open-plane cross-agent reach by Policy objects ([access-policy.md](access-policy.md));
  sealed-plane owning-agent/field/secret-name by `secret_grants` rows
  (`grantee_agent_id`, `field_name`) and by explicit `--owner-agent`/`--owner-group` /
  `witself://agent/...` / `witself://group/...` targeting. Template/operation narrowing
  is deferred.

## Agent Authorization (token_scopes ∩ ownership ∩ grants)

Agents and service tokens are machine principals, NOT operator roles; they never appear
in `realm_members` as role-holders for human-style authority. A v0 agent token is a
bearer credential bound server-side to exactly one `(account_id, realm_id, agent_id)`
via the `agent_tokens` row; identity is the binding, never a request input.

An agent's effective access to any datum (memory, fact, or secret) is the deterministic
**intersection** of three dimensions, evaluated in fixed order (NOT a union):

1. **Tenant.** Resolve the token to `(account_id, realm_id, agent_id)`. Reject disabled
   agents, revoked tokens, and expired tokens here, before any scope is consulted. Every
   query is hard-scoped by `account_id` AND `realm_id`; no cross-realm span.
2. **Token scopes.** `agent_tokens.scopes[]`. The **default agent bundle** is the
   consolidated set spanning both planes over OWN data only:

   ```
   {
     memory:create, memory:read, memory:update, memory:forget,
     fact:create, fact:read, fact:update, fact:delete, fact:primary,
     secret:create, secret:show, secret:reveal, secret:update, secret:delete,
     totp:enroll, totp:code,
     message:send, message:read
   }
   ```

   explicitly EXCLUDING `secret:grant`, the cross-agent umbrellas
   `memory:read-others`/`memory:manage-others`, `group:manage`, `policy:manage`,
   `agent:manage`, `token:manage`, `audit:read`, `federation:manage`, `realm:admin`,
   `account:*`, `billing:*`, `support:*`. A missing required scope is an immediate deny.
   Cross-realm SEND remains in the agent bundle as `message:send` (gated by the
   federation allow-list + per-conversation consent), but managing that allow-list and
   the realm card is operator-only via `federation:manage`.
3. **Ownership-or-grant-or-policy.** The target datum is self-owned
   (`owner_kind='agent'` AND `owner_agent_id = this agent`) OR access is conferred by
   the plane-specific cross-agent mechanism:
   - **Sealed plane (secret/TOTP):** a live `secret_grant` whose
     `grantee_agent_id = this agent` (or a group grant where this agent is a current
     member of the grantee group = group-owned reach) whose capabilities cover the
     required action (`--read`→`secret:show`, `--reveal`→`secret:reveal`,
     `--totp`→`totp:code`, `--write`→`secret:update`) and whose `field_name`, if set,
     matches the targeted field.
   - **Open plane (memory/fact):** a matching `allow` Policy in
     [access-policy.md](access-policy.md) for the requested verb against the record
     owner, AND the matching cross-agent umbrella scope
     (`memory:read-others` for `read`, `memory:manage-others` for
     contribute/curate/forget). Neither default-bundle scope reaches another agent's
     identity data; only the umbrella scopes do, and only via Policy.

Effective permission on a NON-self-owned datum = `token_scope ∩ (grant_capability or
Policy_allow)`: **both** MUST allow. A grant/Policy never elevates beyond the token
scope, and a token scope never reaches another agent's data without a grant (sealed) or
a Policy (open). Agents cannot self-grant, cannot author Policy by default
(`policy:manage` excluded), cannot run agent/token/realm/account/billing/support
management, and cannot see realm-wide inventory (`--all-agents` is operator-only).
`totp:code` returns codes only; `totp:enroll` (seed import/export) is operator-gated for
cross-agent use. Secrets are **never** embedded, recalled, in the self-digest, or
plaintext-exported regardless of any scope or grant.

The **service** principal is the same machinery: an operator-issued, tenant-bound,
narrowly-scoped token for unattended operator automation, with scopes a SUBSET of the
issuing operator's effective scopes (no escalation via token minting); no human OAuth
identity and no `realm_members` row, so its authority is purely `token_scopes` plus
tenant scoping. Disabled agents and revoked/rotated-out tokens fail at step 1.

## Account + Realm Composition

Effective operator permission for a request against realm `R` is:

```
effective_scopes(operator, R) =
    account_role_bundle(operator)
  UNION realm_role_bundle(operator, R)
  hard-clamped by tenant (account_id, realm_id) scoping
```

The two layers are **ADDITIVE (union)**, never subtractive — both are least-privilege-
by-default so there is no deny bundle. Account roles apply account-wide; the per-realm
role applies only within the realm named by its `realm_members` row.

- Account roles that carry `realm:admin` (`account_owner`, `account_admin`) confer admin
  on EVERY realm in the account WITHOUT a per-realm row — this is how cross-realm
  administration (gated by `realm:admin` + `account:manage`) is expressed. A plain
  `account_member` MUST additionally hold a `realm_members` row to act in a realm.
- Billing/account/support/audit scopes come ONLY from the account layer;
  open-plane (memory/fact/policy/group/message) and sealed-plane (secret/totp) scopes
  come from the realm layer (or from an account role carrying `realm:admin`). Billing is
  **realm-targeted but account-held**: `billing:read`/`billing:manage` are account-role
  scopes that act on whichever realm's plan is targeted.

**Operator-acting-on-an-agent's-data rule.** An operator never owns agent data;
operator access to an agent-owned datum is authorized purely by the operator's role
scopes, and additionally:

- **Open plane:** cross-agent read/curate/forget is gated by Policy default-deny OR the
  `realm:admin` operator override ([access-policy.md](access-policy.md) Operator
  Override), plus the `memory:read-others`/`memory:manage-others` umbrella scope.
- **Sealed plane:** cross-agent secret/TOTP access is authorized by the operator's realm
  scopes (`secret:reveal` etc. from `realm:admin`/`realm:operator`) and is **not**
  subject to Policy — there is no open `read`/`curate`/`forget` verb on a secret.
- both planes are gated on **explicit targeting** — `--owner-agent` (or
  `--owner-group` / `witself://agent/...` / `witself://group/...`). List/scan MAY use
  `--all-agents`, but update/delete/reveal/grant/curate/forget/TOTP-generate MUST target
  a specific owning agent or group.
- both require an audit **`--reason`** for reveal / destructive / cross-agent / TOTP-
  generate / curate / forget operations.
- both are audited identically to agent access (`actor_kind=operator|admin`).

## Authorization Resolution Algorithm

One shared authorization layer sits below CLI/MCP/HTTP. AI-assisted admin uses the same
auth, scopes, confirmations, audit reasons, and redaction as humans — there is no
privileged AI-only path. The same 8-step algorithm governs both planes; the plane
differs only at Step 5 (Policy for open, grant for sealed).

- **Step 0 — Token → principal (tenant resolution, precedes everything).** Resolve the
  bearer token via `token_lookup` + constant-time `token_hash` compare to its
  `agent_tokens` row; reject if `revoked_at` set, `expires_at` passed, or the bound
  agent is disabled/tombstoned. Derive `principal_kind` and the bound
  `(account_id, realm_id, [agent_id])`. Every subsequent query is unconditionally
  predicated on `account_id=$acct` AND `realm_id=$realm`; no query spans realms unless
  effective scopes include `realm:admin` (cross-realm within account) or
  `account:manage`.
- **Step 1 — Principal → roles.** If `principal_kind` is `agent` or `service`, skip role
  expansion (authority derives from `token_scopes` directly). If `operator`/`admin`,
  look up `operators.account_role` → account bundle and the `realm_members` row for
  `(realm_id, operator_id)` → realm bundle.
- **Step 2 — Roles → scopes (effective set).** For operators,
  `effective_scopes = account_role_bundle UNION realm_role_bundle`
  (`account_owner`/`account_admin` treated as holding `realm:admin` on every realm even
  without a row); `realm_members.scopes[]` is frozen-empty and contributes nothing. For
  agents/service, `effective_scopes = token.scopes[]`.
- **Step 3 — Capability gate.** Map the requested operation to its required scope (e.g.
  reveal→`secret:reveal`, cross-agent recall→`memory:read-others`, cross-agent
  forget→`memory:manage-others`, `--all-agents` inventory→`secret:show`/`memory:read` +
  `realm:admin`, agent create→`agent:manage`, token rotate→`token:manage`, billing
  subscribe→`billing:manage`).
- **Step 4 — Scope check.** The required scope MUST be in `effective_scopes`, else deny.
- **Step 5 — Object check (ownership / grant / Policy / targeting).** The specific target
  MUST be permitted for the request's plane:
  - **Sealed plane** (secret/TOTP) — agents: self-owned OR a covering `secret_grant`
    (capability + `field_name` match); operators: `realm:admin`/`realm:operator`-class
    scope PLUS explicit `--owner-agent`/`--owner-group`/`witself://agent` targeting, with
    `--all-agents` allowed only for list/scan. No Policy applies to secrets.
  - **Open plane** (memory/fact) — agents: self-owned OR a matching `allow` Policy for
    the verb against the record owner ([access-policy.md](access-policy.md)); operators:
    Policy match OR `realm:admin` operator override, plus explicit targeting for
    cross-agent curate/forget/delete.
- **Step 6 — Preconditions (alongside, not inside, the allow decision).** A required
  audit `--reason` (operator/admin reveal/destructive/copy-with-sensitive/cross-agent/
  curate/forget/server-side-decrypt), confirmation/`--yes`, and advertised backend
  capability (e.g. `server_side_decrypt` for sealed reveal). A missing reason or unmet
  confirmation fails as a deterministic **VALIDATION** error, distinct from a scope
  **DENY**; either outcome is audited.
- **Step 7 — Audit.** Emit the `audit_events` row (dotted `action` namespace,
  `actor_kind`, target, owner context, deciding Policy id for open-plane cross-agent,
  reason, `server_side_decrypt` flag for sealed reveal, result
  `success|denied|error`). Audit rows never store memory content, fact values, secret
  values, TOTP seeds/codes, message bodies, embedding vectors, raw tokens, passphrases,
  or private keys.

## Command / Route / MCP-Tool → Required Scope

Representative operations and the scope each requires (plus object/precondition gates).
Required scope is necessary but not sufficient — Step 5/6 gates still apply.

### Open plane (memories + facts)

| Operation (CLI / route / MCP tool) | Required scope | Additional gate |
| --- | --- | --- |
| `memory add` / `POST /v1/memories` / `witself.memory.create` | `memory:create` | Agent: self-owned only. |
| `memory recall` / `witself.memory.recall` | `memory:read` (own) or `memory:read-others` (cross) | Cross-agent recall needs a matching `read` Policy. |
| `memory curate` / `memory forget` | `memory:update`/`memory:forget` (own) or `memory:manage-others` (cross) | Cross-agent: Policy `curate`/`forget` + `--reason`; soft by default. |
| `fact set` / `fact get` / `witself.fact.*` | `fact:create`/`fact:read`/`fact:update` | `sensitive` redaction is a display flag, not a reveal. |
| `policy create|delete` / `:policies` / `witself.policy.create` | `policy:manage` | Operator/admin or delegated; not in default agent bundle. |
| `policy test|list|show` / `witself.policy.test` | `policy:read` | Non-mutating dry-run. |
| `group create|set-role|members` | `group:manage` | Operator/admin or delegated. |
| `message send` / `message inbox` | `message:send` / `message:read` | A message never authorizes a write. |

### Sealed plane (secrets + TOTP)

| Operation (CLI / route / MCP tool) | Required scope | Additional gate |
| --- | --- | --- |
| `secret create` / `POST /v1/secrets` / `witself.secret.create` | `secret:create` | Agent: self-owned only. |
| `secret show` / `GET /v1/secrets/...` / `witself.secret.show` | `secret:show` | Self-owned or grant; operator cross-agent needs `--owner-agent`/`--owner-group`. Redacted; never plaintext. |
| `secret scan --all-agents` (realm-wide inventory) | `secret:show` + `realm:admin` | Operator/admin only; agents denied. |
| `secret reveal` / reveal route / `witself.secret.reveal` | `secret:reveal` | Grant `--reveal` or realm role; `--reason`; reveal ceremony; capability-gated decrypt path. Never embedded/recalled/in-digest/exported. |
| `secret update` / `witself.secret.update` | `secret:update` | Self-owned or `--write` grant; operator needs `--owner-agent`/`--owner-group` + `--reason`. |
| `secret delete [--permanent]` | `secret:delete` | Operator-gated; explicit target; `--reason` + `--yes`. |
| `secret grant` / `secret revoke` / `:grant` / `:revoke` | `secret:grant` | Operator/admin for cross-agent/group-owned; `--reason`; audited. Sealed-plane only — no Policy. |
| `totp code` / `witself.totp.code` | `totp:code` | Self-owned or `--totp` grant; cross-agent needs `--owner-agent`/`--owner-group` + `--reason`. |
| `totp enroll` / `totp show --reveal-seed` / `witself.totp.enroll` | `totp:enroll` | Privileged seed path; seed is high-value sealed material; operator-gated for cross-agent. |

### Operator / account (both planes)

| Operation (CLI / route / MCP tool) | Required scope | Additional gate |
| --- | --- | --- |
| `agent create|rename|copy|disable|enable|archive|delete` | `agent:manage` | Operator/admin; destructive forms `--yes` + `--reason`. |
| `token create|rotate|revoke` / `token create --operator` | `token:manage` | Operator/admin; raw token (`witself_at_…`) returned once; subset-of-issuer for minted tokens. |
| `audit ...` / `GET /v1/audit` / `witself.audit.*` | `audit:read` | Realm scope for realm roles; account-wide for account roles. |
| `account members|invite|remove|set-role|close` | `account:manage` (close: + `billing:manage`) | Admin only; `--reason`; `--yes`; last-owner protection. |
| `account show` / `account members --list` | `account:read` | |
| `realm members add|remove|set-role` | `realm:admin` + `account:manage` | Member/role management is admin-class. |
| `realm rename|delete [--permanent]` | `realm:admin` | `--yes` + `--reason`. |
| `billing show|usage|limits|plans` | `billing:read` | No reason required. |
| `billing subscribe|payment-method ...|refund` / `account close` | `billing:manage` | `--reason` + confirmation. |
| `support show|list` | `support:read` | |
| `support open|comment|close` | `support:manage` | Redact secret material by default. |
| `federation peers add|remove|list` / `/v1/federation/peers` | `federation:manage` | Manage the accepted-peer allow-list / trust registry; deny-by-default. Cross-realm SEND uses `message:send`, not this scope. |
| `federation card publish|rotate` / `GET /.well-known/witself-card.json` | `federation:manage` | Publish/rotate the realm's signed card; signing mandatory. Card read is public/unauthenticated. |

## Bootstrap Operator And Least-Privilege Defaults

The first/bootstrap operator is provisioned as **`account_owner`** — an explicit named
bundle (`account:read`, `account:manage`, `billing:read`, `billing:manage`,
`support:read`, `support:manage`, `audit:read`, implicit `realm:admin` on every realm),
`principal_kind=admin` — never an implicit superuser special case. Three provisioning
paths, same resulting role:

1. **Managed.** CLI-initiated browser/device-code login creates the first operator
   context as `account_owner`.
2. **Self-hosted.** `witself-server bootstrap token` (or an equivalent deployment
   command, e.g. a Kubernetes admin Job) mints a **one-time, single-use, quickly-
   expiring, audited** bootstrap token with NO default admin password;
   `ws setup --endpoint URL --bootstrap-token-file PATH` redeems it to create the
   first operator (`account_owner`), the account, realm(s), agents, and durable token
   files.
3. **Local dev.** `ws realm init` creates the local realm and the first
   local operator/admin context (`account_owner`-equivalent) when the realm is empty,
   able to create agents and write token files. When the sealed plane is enabled, local
   dev uses the `local-dev` KMS provider for envelope keys (see
   [key-hierarchy.md](key-hierarchy.md)); an open-plane-only deployment needs no KMS.

After the first operator exists, ordinary account/realm/agent/token management flows
through the public `ws` CLI under normal authz.

**Least-privilege defaults.** Account invites land in `account_member` (account-read
only); realm invites land in `realm:member`; agent tokens issue with the default agent
bundle over own data only (both planes); service tokens are a subset of the issuing
operator's effective scopes. **Invariant:** at least one `account_owner` MUST always
exist; the last one cannot be removed or demoted.

## Guarded-Operation Rules

- **Audit reason.** Operator/admin secret reveal, cross-agent open-plane curate/forget,
  destructive (delete/archive/permanent), copy-with-sensitive, cross-agent, grant/revoke,
  TOTP-generate, billing-mutation, and cross-agent server-side-decrypt ops MUST carry
  `--reason`; read-only billing/show/list MUST NOT require one. A missing required
  `--reason` is a deterministic VALIDATION failure (the op is refused alongside the authz
  decision), NOT a silent allow and NOT a scope DENY.
- **Explicit targeting.** List/scan MAY use `--all-agents`;
  update/delete/reveal/grant/revoke/curate/forget/TOTP-generate MUST use a specific
  `--owner-agent`/`--owner-group` (or a `witself://agent/...` / `witself://group/...`
  reference) — never a broad `--all-agents` mutation.
- **Confirmation.** Destructive/security-impacting ops (realm delete, agent delete,
  `secret delete --permanent`, grant/revoke, TOTP delete, cross-agent hard delete, token
  revoke, billing mutations, import `--replace`) MUST require confirmation unless `--yes`
  by an authorized caller; `--dry-run` is supported for preview.
- **Server-side decrypt (sealed plane, distinguishing).** There is NO dedicated scope —
  it rides on `secret:reveal` / `totp:code`. It is gated additionally by the
  backend/realm advertising the `server_side_decrypt` capability (remote managed
  token-only pods; capability discovery is authoritative, else `unsupported_operation`),
  is the default for remote backends in v0 (client-side decrypt is post-v0), and is
  recorded by setting `audit_events.server_side_decrypt=true` — which is exactly what
  distinguishes it in the audit trail from a client-side reveal. Cross-agent server-side
  decrypt additionally requires an audit reason. This applies only to the sealed plane;
  the open plane has no decrypt path (see [encryption-model.md](encryption-model.md),
  [key-hierarchy.md](key-hierarchy.md)). Audit rows NEVER store secret values, TOTP
  seeds/codes, memory content, fact values, raw tokens, passphrases, or private keys.

## V0 Subset

- Four account roles (`account_owner`, `account_admin`, `account_billing`,
  `account_member`) as immutable bundles stored on `operators.account_role`.
- Four realm roles (`realm:admin`, `realm:operator`, `realm:auditor`, `realm:member`) as
  immutable bundles stored verbatim in `realm_members.role`, each spanning the open and
  sealed planes for the elevated roles.
- The consolidated canonical scope vocabulary (open-plane memory/fact/policy/group/message
  + cross-agent umbrellas, sealed-plane secret/totp, operator/account) exactly as spelled
  in [data-model.md](data-model.md).
- Agent track: `token_scopes ∩ ownership ∩ grants` (sealed) / Policy (open), with the
  consolidated default agent bundle (own memory/fact lifecycle + own secret
  create/show/reveal/update/delete + totp enroll/code + messaging).
- `service` `principal_kind` as a subset-of-issuer, tenant-bound automation token.
- The 8-step resolution algorithm (token→principal→roles→scopes→capability gate→scope
  check→object check→preconditions→audit) shared below CLI/MCP/HTTP, plane-aware at the
  object check.
- `account set-role` / `realm members set-role` / `account invite` / `realm members add`
  as the CLI role-management surface (pick a role, get its bundle; no per-member scope
  flags).
- Bootstrap operator = `account_owner` across managed/self-hosted/local.
- Guarded-op rules: audit reason, explicit targeting, confirmation, server-side-decrypt
  capability gating + audit flag (sealed plane).
- Sealed-plane invariants: secrets/TOTP seeds are never embedded, recalled, in the
  self-digest, or plaintext-exported, regardless of role/scope/grant.
- `realm_members.scopes[]` present in schema but frozen-empty (runtime guard treats
  non-empty as a config error).

**Deferred to v1+:** custom per-member scopes (writing `realm_members.scopes[]`,
additive, no migration); `denied_scopes[]` / subtractive composition (no schema column,
rejected for audit simplicity); fine-grained scope editing and arbitrary custom roles; a
scope-constraint mini-language (v0 uses structural grants + targeting + Policy); SCIM /
external-IdP role provisioning; per-field DEK-backed reveal grants as crypto (v0 keeps
per-field grants as authorization checks); client-side/BYOK decrypt over the wire;
Policy `deny` effects; private internal Witself staff admin CLI roles + step-up
approval; per-realm cryptographic isolation against the deployment role (v0 isolation is
authorization + `realm_id` query scoping + per-realm KEK for the sealed plane).

## Revises / Reconciliation

This doc reconciles role/scope naming across the existing docs. No scopes are renamed or
removed — the consolidated vocabulary is reused verbatim.

- **[data-model.md](data-model.md) — `realm_members`.** Confirm the canonical
  `realm_members.role` value set is `{realm:admin, realm:operator, realm:auditor,
  realm:member}`. `realm_members.scopes[]` is RESERVED/frozen-empty for v0 with a runtime
  guard (non-empty = config error).
- **[data-model.md](data-model.md) — `operators`.** The `operators.account_role` text
  column with `CHECK (account_role IN ('account_owner','account_admin',
  'account_billing','account_member'))` stores account-level roles. The single-column add
  keeps account-role assignment canonical (no separate `account_members` table in v0).
- **[data-model.md](data-model.md) — ownership unification.** All three data types
  (memory, fact, secret) carry `owner_kind ∈ {agent, group}`. The former Witpass
  vault-shared secret is a **group-owned** secret; `--shared` is replaced by
  `--group <name>` and `witself://group/<group>/secret/...`.
- **[data-model.md](data-model.md) Enumerations / scope list.** No scopes change.
  `realm:operator`/`realm:auditor` are role-namespace values composed of existing scopes,
  NOT new scopes. `realm:admin` and `realm:member` are simultaneously scope tokens AND
  `realm_members.role` values; `realm:operator`/`realm:auditor` are role-namespace values
  only.
- **[access-policy.md](access-policy.md).** The cross-agent IDENTITY policy engine (open
  plane) **composes with** the roles/scopes here: a cross-agent open-plane action needs
  the umbrella scope from a role/token AND a matching `allow` Policy. Secrets are NOT
  subject to the open `read`/`contribute`/`curate`/`forget` verbs; the sealed plane uses
  grants + realm roles instead.
- **[cli-command-surface.md](cli-command-surface.md).** Concrete `--role` vocabularies:
  `account set-role`/`account invite`/`account members --role` accept `{account_owner,
  account_admin, account_billing, account_member}`; `realm members add`/`set-role`/`members
  --role` accept `{realm:admin, realm:operator, realm:auditor, realm:member}`. `--role`
  values are immutable bundles in v0.
- **[requirements.md](requirements.md) (Authorization and Scopes) +
  [operator-auth.md](operator-auth.md).** The first/bootstrap operator is provisioned as
  `account_owner` (named bundle) across managed/self-hosted/local. The consolidated
  default agent token bundle and the agent intersection rule (scope necessary but not
  sufficient) are the canonical model.
- **[token-lifecycle.md](token-lifecycle.md) / [json-contracts.md](json-contracts.md).**
  Operator/service token minting is subset-constrained (`token.scopes[]` MUST be a subset
  of the issuing operator's effective scopes). Raw token prefix is `witself_at_`.

## Open Decisions

- Confirm the `operators.account_role` column vs an alternative `account_members` table
  (single-column add assumed minimal; a table is needed only if one operator holds
  different account roles across multiple accounts, which v0 does not support).
- Bundle boundary calls: should `realm:operator` include `secret:delete` or
  `policy:manage`? Should `realm:operator` carry `federation:manage` (current: yes), or is
  managing the federation allow-list / realm card reserved to `realm:admin`? Should
  `account_admin` get `billing:read` only (current: yes read, no manage)? All-or-nothing
  until v1 composability.
- Whether `realm:operator` and `realm:auditor` ship in v0 or are deferred, leaving only
  `realm:admin`/`realm:member` at launch. Recommendation: ship all four — pure
  lookup-table additions with no schema cost.
- Confirm the runtime guard semantics for a non-empty `realm_members.scopes[]` in v0
  (hard error vs ignore-with-warning).
- Confirm last-`account_owner` protection and `account remove --transfer-to` interaction
  for ownership transfer.
- Confirm whether elevated realm roles should carry the `memory:read-others`/
  `memory:manage-others` umbrellas by default, or require an explicit per-realm grant, so
  realm-admin open-plane reach is opt-in vs implicit.

## Related Docs

- [requirements.md](requirements.md)
- [data-model.md](data-model.md)
- [access-policy.md](access-policy.md)
- [token-lifecycle.md](token-lifecycle.md)
- [operator-auth.md](operator-auth.md)
- [cli-command-surface.md](cli-command-surface.md)
- [mcp-tools.md](mcp-tools.md)
- [api-contract.md](api-contract.md)
- [json-contracts.md](json-contracts.md)
- [memory-model.md](memory-model.md)
- [facts-model.md](facts-model.md)
- [secret-model.md](secret-model.md)
- [encryption-model.md](encryption-model.md)
- [key-hierarchy.md](key-hierarchy.md)
- [inter-agent-messaging.md](inter-agent-messaging.md)
- [threat-model.md](threat-model.md)
