# Witself Security Groups

Status: pre-implementation draft. Last reviewed 2026-06-26.

Decision: a security group is a named set of agents within a realm that acts as
both a policy **subject** and a policy **target**, and that may own group-scoped
shared memories and facts (collective memory). Groups have no implicit
permissions; every cross-agent effect of a group flows through an explicit
[Policy](access-policy.md) under the realm's default-deny stance.

Security groups are a net-new Witself concept with no Witpass equivalent. They
build on the same authorization scaffolding reused by
[access-policy.md](access-policy.md), and they bind to the same memory and fact
shapes defined in [memory-model.md](memory-model.md) and
[facts-model.md](facts-model.md). JSON shapes are pinned in
[json-contracts.md](json-contracts.md).

## What A Group Is

- A group is a durable, named collection of agents scoped to one realm.
- A group is a **policy subject**: binding a permission with a group subject
  grants every current member that permission on the target.
- A group is a **policy target**: a permission may be granted *to* a group, and
  it applies to the group's collective memory and facts.
- A group may **own** memories and facts directly (collective memory), in
  addition to being a subject/target over agent-owned data.
- A group is not a token holder. Agents authenticate; groups do not. Membership
  amplifies an agent's reach through policy; it never replaces the agent's own
  token identity.
- A group is not a role and not a scope. Token scopes
  (`group:read`, `group:manage`, `group:member`) gate the *surface*; group
  membership and bound policies decide the *effect*.

Two distinct purposes, kept separate on purpose:

- **Grouping for policy** — name a set of agents once, then write policies
  against the name instead of enumerating agents. Membership changes propagate to
  every policy that references the group.
- **Collective memory** — give a set of agents a shared, group-owned identity
  store (shared memories/facts) that is no single agent's private data.

## Group Shape

A group record:

- `id` — stable, `grp_` prefix.
- `realm` — the enclosing realm. Groups never span realms in v0. The
  realm-spanning sibling is a post-v0 **cross-realm channel** (see
  [Cross-Realm Channels](#cross-realm-channels)), not a wider group.
- `name` — unique within the realm. Stable, human-meaningful, used in
  `witself://group/<group>/...` references.
- `description` — optional, human-readable.
- `members[]` — the agents in the group (agent ids/names). Identity of a member
  is always the durable named agent, never a token.
- `admins[]` — agents allowed to manage this group's membership when they also
  hold `group:manage`. Operators always qualify; `admins[]` is how an agent is
  delegated membership management without realm-wide operator rights.
- `created_at`, `updated_at`.
- Bound policies are not stored on the group record. They are
  [Policy](access-policy.md) objects that *reference* the group as subject or
  target, and are discovered by querying policies, not by reading the group.

Rules:

- `name` is unique per realm and is validated at create/rename. Reusing a freed
  name is allowed.
- `members[]` and `admins[]` reference agents in the same realm. An agent may
  belong to many groups. Groups do not nest in v0 (no group-of-groups);
  nesting is a [post-v0](post-v0-roadmap.md) topic.
- Deleting a group is guarded (see [Group Deletion](#group-deletion)) because it
  affects every bound policy and any group-owned identity data.

See [json-contracts.md](json-contracts.md) for the exact field names and the
`witself.v0` Group shape.

## Membership Management

Who may change membership:

- **Operators / realm admins** (operator override) may manage any group's
  membership in the realm. Audited like any agent action.
- **Agents in `admins[]`** holding the `group:manage` scope may manage that
  group's membership.
- An agent holding only `group:member` may join/leave groups that explicitly
  allow self-service membership; it may not add or remove *other* agents.
- An agent holding only `group:read` may list/show groups it can see but may not
  mutate membership.

Membership operations:

- `add-member` — add an agent to `members[]`. Idempotent by (group, agent).
- `remove-member` — remove an agent from `members[]`. Guarded: a removed member
  immediately loses every permission it held *through* this group. Removal
  supports `--dry-run` to preview affected policies and supports `--reason`.
- Promoting/demoting an admin is an operator or `group:manage` action and is
  audited.

Effect of membership changes:

- Membership is evaluated **at decision time**, not cached into policies. Adding
  an agent to a group subject grants it the group's policies on the next request;
  removing it revokes them immediately.
- Removing the last member of a group does not delete the group or its
  group-owned data. An empty group is valid; its policies simply match no one as
  subject until members are added.
- Member changes emit `group.member_added` / `group.member_removed` audit events
  (see [audit-retention.md](audit-retention.md)).

## Groups As Policy Subject And Target

Groups are the reason policies stay small. The binding shapes live in
[access-policy.md](access-policy.md); the group-relevant cases:

- **Group as subject** — `subject: grp_…` grants the permission to every current
  member. Example: group `analysts` is granted `read` on agent `archivist`; every
  member of `analysts` may read and semantically recall over `archivist`'s
  memories and facts, subject to any policy `filter`.
- **Group as target** — `target: grp_…` grants the permission *toward* the
  group's collective memory. Example: agent `coordinator` is granted
  `contribute` to group `shared-context`; `coordinator` may add memories/facts to
  that group's collective store.
- **Group-to-group** — both `subject` and `target` may be groups. Example: group
  `editors` granted `curate` on group `shared-context` lets every editor adjust
  the group's collective memory.

Evaluation notes:

- A request is allowed if **any** matching `allow` policy applies, whether the
  caller matches the subject directly (as an agent) or transitively (as a member
  of a subject group). Absence of a match is deny.
- The same escalating verbs apply (`read` < `contribute` < `curate` < `forget`);
  see [access-policy.md](access-policy.md). A group binding does not grant a
  stronger verb than the policy states.
- `witself policy test` resolves group membership, so it answers "would agent X,
  via its groups, be allowed to do Y on target Z" with the deciding policy id.

## Group-Scoped Collective Memory

v0 supports memories and facts **owned by a group** rather than by a single
agent. This is the collective-memory feature.

- Group-owned records use the same `mem_` / `fact_` shapes from
  [memory-model.md](memory-model.md) and [facts-model.md](facts-model.md); the
  owner is a `grp_` id instead of an `agent_` id.
- Ownership is chosen at write time. `memory add --group <group>` and
  `fact set --group <group> NAME VALUE` create group-owned records when the
  caller is authorized (operator, group admin, or an agent with a `contribute`
  policy targeting the group). Default ownership is still the creating agent;
  group ownership is an explicit opt-in.
- Access to group-owned data is governed by the group's bound policies, not by
  the accessing agent's private ownership. A member is not automatically allowed
  to read group-owned memory; a policy granting the group (or its members) the
  relevant verb on the group target is what permits it. Operators may always
  access within the realm (audited).
- Group-owned facts keep `name` uniqueness **per owner** — a group's facts are
  name-unique within that group, independent of any member agent's facts. Primary
  promotion (`--primary`) applies within the group's namespace.
- Group-owned memories participate in semantic recall the same way: a member with
  a `read` policy on the group may `recall` over the group's collective memory.
  Cross-owner recall is metered as a cross-agent access (see
  [billing-and-limits.md](billing-and-limits.md)).
- References to group-owned data use `witself://group/<group>/memory/<id>` and
  `witself://group/<group>/fact/<name>`; resolution enforces the same
  authorization as a direct read.

Destructive actions on collective memory follow the cross-agent guardrails from
[access-policy.md](access-policy.md):

- `curate` and `forget` on group-owned records require an audit `--reason`,
  support `--dry-run`, and require confirmation unless `--yes`.
- Forget is soft/tombstoned and reversible within the retention window; hard
  delete is a further-guarded step.
- Every group-owned mutation is attributed: "memory `mem_…` of group `grp_…` was
  curated by agent `agent_…` under policy `pol_…`".

## CLI: `group`

The `group` noun lives in the CLI surface tracked by
[cli-command-surface.md](cli-command-surface.md). All commands support `--json`;
mutations honor `--dry-run`, `--yes`, `--reason`, and `--no-input` where noted.

- `witself group create NAME [--description …]` — create a group. Idempotent by
  name within the realm (selects the existing group instead of erroring).
- `witself group list [--member <agent>] [--json]` — list groups visible to the
  caller; optionally filter to groups containing an agent.
- `witself group show NAME|grp_… [--json]` — show one group: members, admins,
  description, timestamps, and the policies that reference it as subject/target.
- `witself group add-member NAME <agent> [--admin]` — add an agent (optionally as
  an admin). Idempotent.
- `witself group remove-member NAME <agent> [--dry-run] [--reason …]` — remove an
  agent. `--dry-run` reports which policies/permissions the agent loses.
- `witself group delete NAME [--dry-run] [--reason …] [--yes]` — delete a group.
  Guarded; see [Group Deletion](#group-deletion).

Collective-memory writes are not new commands — they are the `--group` flag on
existing `memory` / `fact` commands:

- `witself memory add --group NAME "…"` — create a group-owned memory.
- `witself fact set --group NAME NAME VALUE [--primary]` — set a group-owned
  fact.
- `witself memory recall --group NAME "<query>"` — recall over a group's
  collective memory (policy-gated).
- `witself memory list --owner-group NAME` / `witself fact list --owner-group
  NAME` — enumerate group-owned records.

Operator inventory across the realm uses `--all-agents` / `--owner-group` per the
operator workflows in [requirements.md](requirements.md); broad destructive
actions require explicit `--owner-group` targeting.

## MCP Tools

The MCP catalog is tracked in [mcp-tools.md](mcp-tools.md). Group exposure is
intentionally read-leaning in v0:

- `witself.group.list` — list visible groups (optional `member` filter).
- `witself.group.show` — show one group, its membership, and referencing
  policies.

Group **mutation** (create/delete, member add/remove, admin changes) is an
operator-leaning, higher-risk surface and is **operator-only / may be excluded
from MCP v0**, consistent with the MCP boundary in
[requirements.md](requirements.md). Collective-memory reads/writes flow through
the existing `witself.memory.*` / `witself.fact.*` tools with a group owner, not
through group-specific tools. All MCP group access respects the same
authorization as the CLI and honors `--read-only` mode.

## API Routes Overview

Group routes follow the `/v1` plural-resource + colon-action style pinned in
[api-routes.md](api-routes.md) and [api-contract.md](api-contract.md). All
responses use the shared envelope `{schema_version, ok, data, warnings}`.

- `POST /v1/groups` — create a group. Supports `Idempotency-Key` and `dry_run`.
- `GET /v1/groups` — list groups (cursor pagination; optional `member` filter).
- `GET /v1/groups/{group_id}` — show one group.
- `PATCH /v1/groups/{group_id}` — update name/description/admins.
- `POST /v1/groups/{group_id}:add-member` — add a member (action route, `POST`).
- `POST /v1/groups/{group_id}:remove-member` — remove a member (`POST`,
  `dry_run` supported).
- `POST /v1/groups/{group_id}:delete` — guarded delete (`POST`, `dry_run`
  supported, requires confirmation/reason for non-empty groups).

Group ownership of identity data reuses the existing resource routes:
group-owned memories/facts are created and read through `/v1/memories` and
`/v1/facts` with a group owner; recall over a group uses
`/v1/memories:recall` scoped to the group target. There is no separate
collective-memory route family.

Authorization is enforced below every frontend, so the CLI, MCP, and these routes
produce identical decisions for the same caller and group.

## Group Deletion

Deletion is guarded because a group touches policies and may own identity data:

- A group with bound policies or group-owned records cannot be silently deleted.
  `delete` reports, via `--dry-run`, the referencing policies and the count of
  group-owned memories/facts that would be affected.
- Group-owned memories/facts are **not** hard-deleted as a side effect. They are
  soft-deleted (tombstoned, reversible within the retention window) or must be
  re-homed/forgotten explicitly first; hard removal is a separate guarded step.
- Deleting a group invalidates policies that reference it as subject or target;
  affected policies are reported and, per [access-policy.md](access-policy.md),
  resolve to deny once the group is gone (default deny).
- Deletion requires confirmation unless `--yes`, requires an audit `--reason`,
  and emits a `group.created`-paired deletion audit event (see
  [audit-retention.md](audit-retention.md)).

## Scopes

Group behavior is gated by the token scopes pinned in
[requirements.md](requirements.md):

- `group:read` — list/show groups and their membership.
- `group:manage` — create, rename, delete groups; add/remove members and admins
  (for groups the caller administers, plus operator override).
- `group:member` — self-service join/leave where a group allows it.

Accessing or writing collective memory additionally requires the relevant
`memory:*` / `fact:*` scopes and a [Policy](access-policy.md) granting the group
(or its members) the needed verb. Holding `group:read` never implies access to a
group's collective memory.

## Audit

Group activity is audited per [audit-retention.md](audit-retention.md). Stable
event names include `group.created`, `group.member_added`, and
`group.member_removed`. Group-owned identity mutations reuse the cross-agent
attribution events (`crossagent.curated`, `crossagent.forgotten`, etc.) with the
owning group recorded as the target. Audit records carry non-sensitive context
only — group id, group name, member agent id, policy id, and decision outcome —
and never include memory content, fact values, or message bodies.

## Threats

Groups widen the blast radius of a single policy, so they are a deliberate target
in [threat-model.md](threat-model.md):

- **Membership escalation** — adding an agent to a high-privilege group silently
  grants every bound policy. Mitigated by `group:manage`/operator gating,
  `--reason`, audit, and `policy test` previewing effective reach.
- **Collective-memory poisoning** — a `contribute`/`curate` policy on a group
  lets a member write into shared identity data trusted by other members. Treated
  as the group analog of memory-poisoning; mitigated by escalating verbs, source
  attribution, soft-delete-by-default, and full audit.
- **Stale membership** — a removed member must lose group reach immediately;
  membership is evaluated at decision time, never cached into policies.
- **Deletion blast radius** — deleting a group can break many policies at once;
  mitigated by guarded deletion, `--dry-run`, and default-deny fallthrough.

## Cross-Realm Channels (Post-v0)

In-realm groups stop at the realm boundary. The cross-realm sibling of a group
is a **channel**: realm-spanning membership formed by **mutual consent** between
the participating realms, where members live in more than one realm rather than
all within one. Channels are a [post-v0](post-v0-roadmap.md) topic specified in
[agent-collaboration.md](agent-collaboration.md), which reuses the group
fan-out mechanics described here (a message addressed to the collective delivers
to every member) extended across the federation trust boundary.

Nothing in this document changes for v0: in-realm group membership, policy
binding, collective memory, and the `witself://group/<group>/...` reference shape
all remain realm-scoped and behave exactly as above. Channels are additive and do
not alter how groups resolve, fan out, or bind policy within a realm.

## See Also

- [access-policy.md](access-policy.md) — policy objects, verbs, default-deny,
  cross-agent guardrails.
- [memory-model.md](memory-model.md) — memory shape, lifecycle, semantic recall.
- [facts-model.md](facts-model.md) — fact shape, name uniqueness, primary
  promotion.
- [inter-agent-messaging.md](inter-agent-messaging.md) — messages addressed to a
  group fan out to members.
- [agent-collaboration.md](agent-collaboration.md) — cross-realm channels, the
  post-v0 realm-spanning sibling of in-realm groups (reuses group fan-out).
- [json-contracts.md](json-contracts.md) — the `witself.v0` Group, Memory, and
  Fact JSON shapes and `witself://` references.
- [cli-command-surface.md](cli-command-surface.md),
  [mcp-tools.md](mcp-tools.md), [api-routes.md](api-routes.md) — the three
  frontends over the same core.
- [requirements.md](requirements.md) — master spec.
