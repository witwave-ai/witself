# Witself Token Lifecycle

Status: draft. Decision: v0 agent tokens are durable by default, revocable,
rotatable bearer credentials bound server-side to one realm and one named agent.

## Decision

Agent tokens are designed for ephemeral runtimes such as Kubernetes pods. A pod
or process may disappear and come back, but the named Witself agent identity
should keep working as long as its mounted token file is valid and the agent has
not been disabled or deleted.

The token is the agent's identity. Unlike Witpass, where a token guards access
to secret material whose confidentiality is the whole point, a Witself token
asserts *who an agent is* so that cross-agent access, group membership, and
inter-agent messaging can be attributed correctly. The threat the token defends
shifts from confidentiality to integrity and authenticity: a stolen or spoofable
token is an identity-forgery and memory-poisoning risk, not a secret-disclosure
risk.

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

Agent tokens carry scopes that gate the identity surface. There are no
`secret:*` or `totp:*` scopes. The v0 scope set is defined in
[requirements.md](requirements.md); tokens express these dimensions:

- Own-identity memory and fact lifecycle: `memory:create`, `memory:read`,
  `memory:update`, `memory:forget`, `fact:create`, `fact:read`, `fact:update`,
  `fact:delete`, `fact:primary`.
- Cross-agent access: `memory:read-others` (read and semantic recall over
  another agent's memories/facts) and `memory:manage-others` (curate and forget
  across agents). Both are policy-gated; the scope is necessary but not
  sufficient, because [access-policy.md](access-policy.md) must also grant the
  action against the specific target.
- Group membership and management: `group:member` (act as a group member for
  group-scoped memories/facts), `group:read`, and `group:manage`.
- Messaging: `message:send` and `message:read`.
- Policy and audit visibility: `policy:read`, `policy:manage`, `audit:read`.
- Operator/admin surfaces: `agent:manage`, `token:manage`, `realm:admin`, and
  the account/billing/support scopes, normally carried by operator tokens rather
  than agent tokens.

Scope rules:

- Scopes should be constrainable by realm, owning agent, owning group, memory
  kind or tag, or fact name where practical.
- A token cannot exceed the permissions of the principal that issued it.
- `memory:read-others` and `memory:manage-others` never bypass policy; they only
  make a token *eligible* to be granted cross-agent access by a policy.
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
memory content, fact values, message bodies or payloads, or embedding vectors.

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
- [server-command-surface.md](server-command-surface.md)
- [api-contract.md](api-contract.md)
- [access-policy.md](access-policy.md)
- [security-groups.md](security-groups.md)
- [inter-agent-messaging.md](inter-agent-messaging.md)
- [memory-model.md](memory-model.md)
- [facts-model.md](facts-model.md)
- [mcp-tools.md](mcp-tools.md)
