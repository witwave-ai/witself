# Witself Token Lifecycle

Status: draft. Decision: v0 agent tokens are durable by default, revocable,
rotatable bearer credentials bound server-side to one realm and one named agent.

## Decision

Agent tokens are designed for ephemeral runtimes such as Kubernetes pods. A pod
or process may disappear and come back, but the named Witself agent identity
should keep working as long as its mounted token file is valid and the agent has
not been disabled or deleted.

The token is the agent's identity, and in the consolidated product that identity
spans **both planes**: the open plane (memories + facts) and the sealed plane
(secrets + TOTP). A Witself token asserts *who an agent is* so that cross-agent
access, group membership, inter-agent messaging, and the agent's own sealed
credential material can be attributed and gated correctly. The threats it defends
are therefore dual: in the open plane a stolen or spoofable token is an
identity-forgery and memory-poisoning risk (integrity/authenticity); in the
sealed plane it is also a credential-disclosure risk (confidentiality), because
the same token authorizes the audited `secret reveal` and `totp code` ceremonies
over the agent's own sealed data. The scope set the token carries is what keeps
those planes distinct (see [Scopes](#scopes) and
[authorization-and-roles.md](authorization-and-roles.md)).

Default v0 posture:

- Agent tokens are bearer tokens.
- Agent tokens are bound server-side to one realm and one named agent.
- Agent tokens do not expire by default unless the operator sets `--ttl` or
  `--expires-at`.
- Raw token values are returned only once, during create or rotate.
- Server-side storage keeps token hashes and token metadata, never raw token
  values.
- Token files use plain token text in v0.
- Revocation is immediate.
- Disabled agents cannot authenticate with existing tokens.
- Deleting or permanently removing an agent invalidates its tokens.

## Token Identity

The token determines the authenticated identity. A caller cannot become an agent
by passing an agent name. The realm and named agent are resolved server-side
from the token, never from input fields.

This is load-bearing for the features that distinguish Witself from a
single-principal store:

- Cross-agent reads, contributions, curation, and forgets are attributed to the
  token-bound acting agent and evaluated against [access-policy.md](access-policy.md).
- A message `from` is always derived from the token; sender forgery is
  structurally impossible through the API (see
  [inter-agent-messaging.md](inter-agent-messaging.md)).
- Group membership is resolved from the token-bound agent identity, not from a
  claimed name (see [security-groups.md](security-groups.md)).

`--agent`, `--owner-agent`, `--owner-group`, and MCP `owner_agent`/`owner_group`
fields are targeting inputs for authorized operator/admin and policy-gated
cross-agent actions. They are not authentication.

## Token File Format

v0 token files should contain only the raw token text:

```text
witself_at_...
```

Raw agent tokens carry the `witself_at_` prefix (consolidated from the former
Witpass `wp_at_`). The prefix is a stable, greppable marker for the secret-leak
scanners and repo-safety checks — a `witself_at_` string in a commit, log, or
export is treated as a leaked credential. The raw value is returned only once at
create or rotate; server-side storage keeps only the `token_hash` and metadata.

No JSON wrapper is required for v0 agent runtime use. Metadata is available
through `witself token list`, `witself token show`, or equivalent API results.

Why plain text:

- Easy to mount as a Kubernetes Secret file.
- Easy to map through `WITSELF_TOKEN_FILE`.
- Easy for agents and humans to understand.
- Avoids requiring parsers in shell scripts and init containers.

Token files should be delivered by deployment-native secret mechanisms. Raw
tokens should not be committed to project files, Terraform state, Helm values,
logs, audit records, support tickets, identity exports, or prompts. Identity
export (see [backup-and-recovery.md](backup-and-recovery.md)) is plaintext by
design for memories and facts, but it must never carry raw token values.

## Token File Writes

When Witself writes a token file directly:

- The file should be owner-readable and owner-writable only where the operating
  system supports file modes, such as POSIX mode `0600`.
- Parent directories created by Witself should be owner-only where supported,
  such as POSIX mode `0700`.
- Writes should be atomic, using a temporary file in the target directory and a
  final rename or equivalent platform-safe operation.
- Normal create paths should refuse to overwrite an existing token file.
- `--reuse-existing-token` should verify the existing token file and leave the
  file unchanged.
- `--rotate-existing-tokens` is the explicit path that may replace an existing
  token file after a replacement token has been issued successfully.
- Existing symlinks, directories, non-regular files, or token files with unsafe
  permissions should fail closed unless a later explicit repair mode is
  designed.

Recommended token locations mirror the bootstrap guidance in
[requirements.md](requirements.md):

- Production container or service: an explicit secret mount such as
  `/run/secrets/witself-agent-token`.
- Local development fallback:
  `${XDG_CONFIG_HOME:-~/.config}/witself/tokens/<profile-or-agent>.token`.

## Authentication Source Precedence

The CLI and MCP adapter resolve the active token from the first source that is
present, in this order:

1. Explicit CLI flag, such as `--token-file`.
2. `WITSELF_TOKEN_FILE`.
3. `WITSELF_TOKEN`.
4. Stored local profile auth, when available for human/operator use.

`WITSELF_TOKEN` is convenient for tests and short-lived local use, but should be
documented as the least-safe unattended option. Production deployments should
prefer a token file delivered through a secret mount and referenced with
`--token-file` or `WITSELF_TOKEN_FILE`.

## Home-Cell Resolution

A token remains the agent's identity and the lifecycle above is unchanged. What
the go-forward deployment topology adds is *where* a token-bound caller lands: in
the multi-cloud cell model (see [deployment-cells.md](deployment-cells.md)) a
realm lives in exactly one home cell at a time, and the CLI and MCP adapter must
reach that cell.

The control plane extends the existing `--endpoint` / token model rather than
replacing it. Today a client points at an endpoint; the go-forward client points
at the control plane, resolves its home cell (and may cache the result), then
talks directly to that cell:

```text
witself --endpoint https://api.witself.cloud login   # control plane resolves home cell
# subsequent calls go directly to the resolved home-cell endpoint
```

This does not change the token contract:

- The token still asserts *who an agent is*; the realm and named agent are
  resolved server-side from the token, never from input (see
  [Token Identity](#token-identity)).
- Tokens remain **cell-scoped and validated by the home cell, not the control
  plane**. The thin control plane holds only routing metadata
  (`realm/account -> home cell + endpoint + signing key`) and never sees token
  hashes, memories, facts, secrets, or messages.
- Authentication source precedence ([above](#authentication-source-precedence))
  is unchanged. `WITSELF_ENDPOINT` / `--endpoint` keep their current targeting
  meaning; the only difference is that the resolved endpoint may be a home-cell
  endpoint returned by the control plane rather than a fixed cell URL.
- Tenant migration between cells (deployment-cells.md) re-homes the realm's data
  but does **not** rotate or re-issue tokens: after the control-plane mapping is
  repointed, clients re-resolve and route to the new home cell with the same
  token. Migration does not alter the agent identity binding, mirroring
  [Rotation](#rotation)'s identity-stability guarantee.

Cross-realm addressing rides the same directory: a message addressed to
`witself://<realm-handle>/agent/<name>` resolves through the same control-plane
registry that routes a client to its home cell (see
[deployment-cells.md](deployment-cells.md) and
[agent-collaboration.md](agent-collaboration.md)). A cross-realm send still uses
`message:send` and is gated by the receiving realm's federation allow-list; it
carries no authority of its own.

Open decisions: how aggressively clients cache the resolved home-cell endpoint
and how re-resolution is triggered after a migration are operational concerns
left to [deployment-cells.md](deployment-cells.md).

## Ephemeral Pod Mounted-Token Pattern

The canonical agent runtime pattern is a durable token mounted into an ephemeral
pod:

- The named agent identity is durable; the pod or process instance is not.
- A restarted pod mounts or maps the same token file, points
  `WITSELF_TOKEN_FILE` at it, and immediately continues as the same named agent.
- No interactive login is required for agents.
- Token rotation and revocation are explicit lifecycle operations on the named
  agent identity, not automatic side effects of runtime churn.

This is why expiration is off by default: routine pod churn must not strand a
valid agent identity.

## Expiration

Agent tokens do not expire by default in v0.

Operators may set explicit expiration:

- `--ttl DURATION`
- `--expires-at TIMESTAMP`

Reasoning:

- Ephemeral pods must be able to restart and use the same mounted token.
- Token rotation and revocation are explicit lifecycle controls.
- Automatic expiration is useful later, but should not surprise v0 agent runtime
  deployments.

## Scopes

Agent tokens carry scopes that gate access across both planes. The canonical
scope vocabulary, the role→scope expansion, and the deterministic resolution
algorithm (`token_scopes ∩ ownership ∩ grants` for the sealed plane / Policy for
the open plane) live in [authorization-and-roles.md](authorization-and-roles.md);
the v0 scope dimensions are listed in [requirements.md](requirements.md). This
section states what a token *carries* and how it is constrained.

Tokens express these dimensions:

- Own-identity memory and fact lifecycle (open plane): `memory:create`,
  `memory:read`, `memory:update`, `memory:forget`, `fact:create`, `fact:read`,
  `fact:update`, `fact:delete`, `fact:primary`.
- Own sealed credential lifecycle (sealed plane): `secret:create`,
  `secret:show`, `secret:reveal`, `secret:update`, `secret:delete`,
  `totp:enroll`, `totp:code`. These gate the agent's own secrets and TOTP
  enrollments; `secret:reveal` and `totp:code` are the only scopes that return
  sealed plaintext, always through the audited reveal/code ceremony. `secret:show`
  is metadata-only and never returns field values.
- Cross-agent open-plane access: `memory:read-others` (read and semantic recall
  over another agent's memories/facts) and `memory:manage-others` (curate and
  forget across agents). Both are Policy-gated; the scope is necessary but not
  sufficient, because [access-policy.md](access-policy.md) must also grant the
  action against the specific target.
- Cross-agent / group-owned sealed access: `secret:grant` writes and revokes the
  per-secret grants that confer cross-agent or group-owned reach. There is no
  Policy engine on the sealed plane; cross-agent secret access is grants plus
  realm roles only ([authorization-and-roles.md](authorization-and-roles.md)).
- Group membership and management: `group:member` (act as a group member for
  group-owned memories/facts/secrets), `group:read`, and `group:manage`.
- Messaging: `message:send` and `message:read`.
- Policy and audit visibility: `policy:read`, `policy:manage`, `audit:read`.
- Operator/admin surfaces: `agent:manage`, `token:manage`, `realm:admin`,
  `federation:manage`, and the account/billing/support scopes, normally carried
  by operator tokens rather than agent tokens. `federation:manage` is the
  operator scope that governs the realm's federation allow-list / trust registry
  and publishing or rotating the realm card; it does not authorize cross-realm
  send, which still uses `message:send` (see
  [authorization-and-roles.md](authorization-and-roles.md) and
  [agent-collaboration.md](agent-collaboration.md)).

### Default Agent Token Bundle

Agent tokens issue with the **consolidated default agent bundle** — own-data
lifecycle across both planes, and nothing cross-agent or operator-class:

```
{
  memory:create, memory:read, memory:update, memory:forget,
  fact:create, fact:read, fact:update, fact:delete, fact:primary,
  secret:create, secret:show, secret:reveal, secret:update, secret:delete,
  totp:enroll, totp:code,
  message:send, message:read
}
```

The bundle is over **OWN data only** (`owner_kind='agent'` and
`owner_agent_id = this agent`, plus group-owned data the agent is a current
member of for the open-plane verbs). It deliberately **excludes** `secret:grant`,
the cross-agent umbrellas `memory:read-others`/`memory:manage-others`,
`group:manage`, `policy:manage`, `agent:manage`, `token:manage`, `audit:read`,
`realm:admin`, `federation:manage`, and the `account:*`/`billing:*`/`support:*`
scopes. An agent
therefore cannot self-grant, cannot reach another agent's data, cannot author
Policy, cannot manage agents/tokens/realms, and cannot see realm-wide inventory
(`--all-agents` is operator-only).

The sealed-plane scopes in the default bundle apply to the agent's own secrets
only. Even with `secret:reveal`, sealed values remain carved out of every
ambient path: secrets and TOTP seeds are **never embedded, never returned by
semantic recall, never in the self-digest, and never plaintext-exported**,
regardless of any scope or grant (see
[secret-model.md](secret-model.md), [encryption-model.md](encryption-model.md),
[backup-and-recovery.md](backup-and-recovery.md)). `secret:reveal` only
authorizes the explicit, audited per-value ceremony.

### Operator And Service Tokens (Subset Of Issuer)

Operator tokens and unattended **service** tokens are minted through
`token:manage` and are **subset-constrained**: a minted token's `scopes[]` MUST
be a subset of the issuing operator's effective scopes
(`account_role_bundle ∪ realm_role_bundle`, hard-clamped by the issuer's tenant).
Token minting can never escalate — it can only carve a narrower credential out of
the issuer's authority. A `service` token additionally has no human OAuth
identity and no `realm_members` row, so its authority is purely its
`token_scopes` plus tenant scoping
([authorization-and-roles.md](authorization-and-roles.md) Principals And
Principal Kinds).

Scope rules:

- A token cannot exceed the permissions of the principal that issued it; minted
  operator/service token scopes are a subset of the issuer's effective scopes.
- Scopes should be constrainable by realm, owning agent, owning group, memory
  kind or tag, fact name, or — for the sealed plane — secret name and field name
  where practical. In v0 these resource constraints are carried structurally
  (tenant binding, `secret_grants` rows, explicit targeting), not as custom scope
  strings ([authorization-and-roles.md](authorization-and-roles.md) Role→Scope
  Expansion And Custom Scopes).
- `memory:read-others` and `memory:manage-others` never bypass Policy, and
  `secret:reveal`/`secret:grant` never bypass ownership/grants; they only make a
  token *eligible* for the gated action, which still requires the object check.
- A token's scopes are recorded in token metadata and are reported by
  `witself whoami` and `witself token show`.

## Rotation

Token rotation creates a replacement token and returns it once.

Rotation should support:

- Writing the replacement token directly to a file with `--out`.
- Immediate old-token revocation.
- Optional grace period where both old and new token are valid.
- Audit reason.
- Dry run.

`witself setup` should not silently rotate or reuse existing token material.
When setup detects an existing token file or active token for a requested agent,
the caller must choose `--reuse-existing-token` or `--rotate-existing-tokens`.
This keeps setup idempotent for resources (account, realm, agent) without making
raw-token lifecycle changes accidental. Account, realm, and agent creation are
idempotent by name; token material is not.

Default rotation posture should be conservative:

- If no grace period is supplied, revoke the old token immediately after the new
  token is issued and written successfully.
- If `--grace-period` is supplied, keep the old token valid until the grace
  period ends unless it is manually revoked sooner.

Rotation preserves the agent identity binding: the replacement token is bound to
the same realm and named agent, so cross-agent policies, group membership,
mailbox addressing, and prior message attribution remain stable across a
rotation. Rotation does not re-home memories or facts and does not alter
ownership.

## Revocation

Revocation is immediate.

Revoked tokens:

- Cannot authenticate.
- Should remain visible as revoked token metadata where policy allows.
- Should be recorded in audit.
- Should not reveal raw token values.

Revocation should support `--dry-run`, `--reason`, and `--yes`.

Revoking a token does not delete the agent's memories, facts, group membership,
or messages; the identity persists and can be re-credentialed with a new token.
Revocation only invalidates the credential, not the self it authenticated.

## Agent Disable And Delete

Disabled agents cannot authenticate with existing tokens.

Agent delete behavior:

- Archive/delete action should invalidate active tokens for that agent.
- Permanent delete should invalidate tokens and remove or tombstone token
  metadata according to retention policy.
- Token metadata may remain in audit history, but raw token values must not.

Identity-data interaction:

- Disabling an agent suspends authentication but leaves its memories, facts,
  policies, group membership, and mailbox intact for later re-enablement or
  operator inspection.
- Deleting an agent follows the memory/fact lifecycle: agent-owned identity data
  is soft-deleted/tombstoned by default and reversible within the retention
  window before hard delete (see [memory-model.md](memory-model.md) and
  [facts-model.md](facts-model.md)). Token invalidation is immediate regardless
  of the identity-data retention path.
- Cross-agent policies naming the deleted agent as subject or target, and group
  memberships, are evaluated against the agent's deleted/tombstoned state and
  reported in `policy test`.

## Token Metadata

Token metadata should include:

- Token ID (`tok_` prefix).
- Token name.
- Principal kind (agent or operator/admin).
- Realm ID/name.
- Agent ID/name when agent-bound.
- Scopes.
- Created time.
- Last-used time when available.
- Expires-at time when set.
- Revoked time when revoked.
- Disabled/blocked reason when inherited from agent state.

Token metadata must not include raw token values. It also must not include
memory content, fact values, message bodies or payloads, embedding vectors, or
any sealed material (secret field values, TOTP seeds or codes).

## Auditing

Token lifecycle events are security-relevant and are audited with stable dotted
event names (see [audit-retention.md](audit-retention.md)):

- Token create, rotate, and revoke.
- Failed token use (authentication failure).
- Agent disable/enable and delete that invalidate tokens.

Audit records carry the token ID, realm, agent, scopes, and outcome, but never
the raw token value. Operator override actions that issue, rotate, or revoke
agent tokens are attributed to the operator and require an audit `--reason`.

## Future Hardening

Future versions may add:

- Proof-of-possession tokens.
- Short-lived session tokens derived from a durable bootstrap token.
- Token binding to workload identity.
- Token audience restrictions.
- Automatic rotation policies.
- Step-up approval for operator tokens and for `memory:manage-others`.

These should be additive and should not break the v0 durable token-file contract
without a migration path.

## Related Docs

- [requirements.md](requirements.md)
- [authorization-and-roles.md](authorization-and-roles.md)
- [server-command-surface.md](server-command-surface.md)
- [api-contract.md](api-contract.md)
- [access-policy.md](access-policy.md)
- [secret-model.md](secret-model.md)
- [totp-2fa.md](totp-2fa.md)
- [security-groups.md](security-groups.md)
- [inter-agent-messaging.md](inter-agent-messaging.md)
- [memory-model.md](memory-model.md)
- [facts-model.md](facts-model.md)
- [mcp-tools.md](mcp-tools.md)
