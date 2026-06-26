# Witself Cross-Agent Access Policy

Status: draft. Decision: Witself authorizes cross-agent identity access through
declarative, evaluable **Policy** objects with a default-deny stance. This doc
replaces the Witpass encryption-model doc and reuses its authorization, audit,
and decision scaffolding, swapped from confidentiality of secrets to integrity
and authenticity of identity data.

## Decision

By default a named agent can only read, recall, contribute to, curate, or
forget the memories and facts it owns. One agent has **no** access to another
agent's identity data unless an operator grants it or a Policy permits it.
Authorization is enforced below every frontend, so the CLI, MCP server, and
`witself-server` HTTP API reach the same decision for the same request (see
[backend-architecture.md](backend-architecture.md)).

Authorization moves from ad-hoc grants to evaluable Policy objects:

- A Policy binds a subject (agent or group) to one permission verb on a target
  (agent or group), scoped to memories and/or facts, optionally filtered.
- Evaluation is **default deny**: absence of a matching `allow` Policy is a deny.
- The deciding Policy is recorded in audit on every cross-agent action.
- Operators can always manage and access identity data within their realm
  (operator override), audited like any agent action.

The threat framing flips from Witpass: the risk is not leaking a secret value,
it is **memory-poisoning, unauthorized curation or forgetting, and cross-agent
write abuse**. The full threat model is tracked in
[threat-model.md](threat-model.md).

## Policy Object

A Policy is a first-class object in the realm. Its shape is pinned in
[json-contracts.md](json-contracts.md); the fields below are load-bearing:

- `id` — stable identifier with the `pol_` prefix.
- `realm` — the enclosing realm. Policies never span realms in v0.
- `subject` — the principal the Policy grants to: an agent
  (`witself://agent/<agent>`) or a security group
  (`witself://group/<group>`). A group subject grants every current member.
- `permission` — exactly one verb: `read`, `contribute`, `curate`, or `forget`
  (see [Permission Verbs](#permission-verbs)).
- `target` — the owner whose identity data is reachable: an agent or a security
  group. A group target covers that group's group-owned memories and facts.
- `scope` — `memories`, `facts`, or both. A Policy with no scope is invalid.
- `filter` — optional narrowing: memory `kind`/`tag`, fact `name`/namespace, or
  the `sensitive` flag. A filter only ever *narrows* a grant; it never widens
  one.
- `effect` — `allow`. This is the only effect in v0; deny is implicit. Policy
  `deny` effects are post-v0 (see [post-v0-roadmap.md](post-v0-roadmap.md)).
- Metadata — `created_at`, `updated_at`, the creating principal, and an optional
  human-readable `description`.

A Policy describes one grant. Broader access is expressed as several Policies,
not as a wildcard verb. Subject and target may both be groups; the cartesian
expansion is evaluated at decision time against current membership, never frozen
at create time.

## Permission Verbs

Four verbs, escalating in danger. Each is independently grantable; holding one
does not imply another.

1. `read` — read the target's memories and facts, including semantic recall over
   them and reading `sensitive` facts. Read is the only non-mutating verb, and
   requires the `memory:read-others` scope.
2. `contribute` — add new memories or facts to the target's store. The created
   record's `source` records the contributing agent; nothing existing is
   changed. Requires the `memory:manage-others` scope.
3. `curate` — adjust, merge, re-tag, or re-link existing memories or facts owned
   by the target, including editing salience, links, and `primary` flags.
   Curate appends to versioned edit history; prior versions are retained.
   Requires the `memory:manage-others` scope; an own-data scope such as
   `memory:update` or `fact:update` never authorizes cross-agent curate.
4. `forget` — soft-delete (tombstone) the target's memories or facts within the
   reversible retention window. Requires the `memory:manage-others` scope. Hard
   delete across agents is a further-guarded step and is never the default (see
   [Guardrails](#guardrails-for-curate-and-forget)).

Self-access is not expressed as a Policy. An agent's access to its own identity
data is governed by its own token scopes (`memory:*`, `fact:*`), not by
cross-agent Policy.

## Default-Deny Evaluation

Evaluation of a request (subject, permission, target, scope, optional record
attributes) proceeds:

1. Resolve the caller's identity from the authenticated token. The actor is
   never taken from an input field (see [Capability Gating](#capability-gating)).
2. If the target owner is the caller, allow subject to token scope; this is not
   a cross-agent decision.
3. Collect every `allow` Policy in the realm whose `subject` matches the caller
   (directly or through group membership), whose `permission` matches the
   requested verb, whose `target` matches the record owner (directly or through
   group membership), whose `scope` covers the record type, and whose `filter`
   (if any) matches the record's attributes.
4. If at least one Policy matches, **allow**, and record the deciding Policy id.
5. If no Policy matches, **deny** with a stable reason
   (`policy.access_denied`), naming the missing
   subject/permission/target/scope.

A deny is the safe default at every layer: an empty policy set, an unknown
target, an unmatched filter, or a degraded backend all resolve to deny, never to
allow. Read denials and write denials use the same evaluation path so behavior
cannot drift between verbs.

## Precedence And Conflict Resolution

v0 has a single effect (`allow`), so resolution is **most-permissive-wins by
union**: if any matching Policy allows the request, it is allowed. There are no
competing deny rules to reconcile in v0.

- Direct-subject and group-subject grants are equivalent; neither outranks the
  other. Membership in any granting group is sufficient.
- Overlapping grants are idempotent. Two Policies that both allow `read` on the
  same target add no extra privilege; either alone suffices.
- Narrower filters do not suppress broader grants. A filtered `read` on
  `kind=profile` does not restrict a separate unfiltered `read` on the same
  target; the union of both applies.
- When deny effects arrive post-v0, the decided precedence is **explicit deny
  overrides allow**; v0 deliberately ships without that ambiguity. This is
  flagged so [post-v0-roadmap.md](post-v0-roadmap.md) does not re-open it.

`policy test` (see [Policy Test](#policy-test-the-canonical-dry-run)) returns the
single deciding Policy id for an allow, or the deny reason, so the resolution
outcome is always inspectable rather than implicit.

## Guardrails For Curate And Forget

The mutating cross-agent verbs reuse the Witpass guardrail pattern, retargeted
from reveal/decrypt to integrity-impacting writes. `contribute` is attributed
but light-weight; `curate` and `forget` are guarded.

For every cross-agent `curate` and `forget`:

- Require an audit `--reason`. The reason is recorded in audit and is never
  optional for cross-agent mutation or operator override.
- Support `--dry-run`. A dry run validates identity, authorization, filters,
  conflicts, and quotas, and reports the records that *would* change, but
  persists nothing, writes no tombstone, and emits no notification.
- Require confirmation unless `--yes` is supplied by an authorized caller.
  Non-interactive callers must pass `--yes`; otherwise the command fails with a
  deterministic confirmation-required error.
- Forget is **soft by default**: it writes a tombstone, reversible via `restore`
  within the retention window. Soft-deleted records remain attributable and
  recoverable.
- Hard cross-agent delete is a separate, further-guarded step. It requires the
  `memory:manage-others` scope, an audit `--reason`, explicit confirmation, and
  is irreversible. It is never reached by `forget` alone.

For `contribute`:

- The created memory/fact records the contributing agent in `source`, so a
  poisoned write is traceable to its author.
- A message arriving in another agent's mailbox **cannot** itself authorize a
  write; the contributing agent still needs a `contribute` Policy (see
  [inter-agent-messaging.md](inter-agent-messaging.md)).

## Attribution And Audit

Every cross-agent decision and mutation is fully attributed. Audit records are
structured, PII-redacted, and retained per [audit-retention.md](audit-retention.md).

Cross-agent audit events (stable dotted names, from
[requirements.md](requirements.md)):

- `crossagent.read`
- `crossagent.contributed`
- `crossagent.curated`
- `crossagent.forgotten`
- `policy.created`
- `policy.deleted`
- `policy.access_denied`

Each cross-agent mutation records the acting agent, the target owner, the record
id, the verb, the deciding Policy id, and the reason. A representative line:
"memory `mem_…` of agent A was pruned by agent B under policy `pol_…`". A denied
attempt records `policy.access_denied` with the missing grant, so probing for
unauthorized access is itself visible.

Audit records must not contain memory content, fact values, message bodies or
payloads, embedding vectors, or raw tokens. They may include non-sensitive
context: record ids, owner agent/group, memory kind/tags, fact names, Policy
ids, scopes, decision outcome, and reason.

## Operator Override

Operators and realm admins can always manage and access identity data within
their realm. This is the override that replaces Witpass's server-side-decrypt
exception, retargeted to integrity:

- Operator override is gated on the operator/admin role and `realm:admin` (or the
  `memory:manage-others` scope), not on a cross-agent Policy.
- Operator override is audited identically to agent action, with the operator as
  actor and the same `--reason` requirement on destructive and cross-agent
  mutation.
- Broad destructive actions require explicit targeting. An operator may
  list/scan `--all-agents`, but `curate`/`forget`/`delete` require a specific
  `--owner-agent` or `--owner-group`, or an explicitly group-shared target.
- Operator override never silently bypasses soft-delete: cross-agent forget by an
  operator is still tombstoned and reversible by default.

## Capability Gating

Policy-gated cross-agent actions are also scope- and capability-gated, so a
grant is necessary but not sufficient.

- The caller must hold the matching cross-agent umbrella scope. There are exactly
  two umbrella scopes, each spanning **both** memories and facts (there is no
  `fact:read-others` or `fact:manage-others`):
  - `memory:read-others` authorizes the `read` verb.
  - `memory:manage-others` authorizes the `contribute`, `curate`, and `forget`
    verbs, and hard delete.
  Own-data scopes (`memory:create`/`fact:create`, `memory:update`/`fact:update`,
  `fact:primary`, `memory:forget`/`fact:delete`) gate only the caller's own data
  and never authorize a cross-agent action on their own; a cross-agent fact
  delete, for example, is a `forget`/`curate` caller holding `memory:manage-others`
  alongside `fact:delete`. Scope and Policy are evaluated together; failing either
  denies.
- The actor is derived server-side from the token. Passing an `--owner-agent` or
  `from` field never lets a caller act as another agent unless the token or
  operator credential has explicit permission. Identity comes from the token,
  never from input.
- Policy management itself requires `policy:read` (test/list/show) and
  `policy:manage` (create/delete). Agents do not get policy management by
  default.
- Where a backend cannot honor a path, it returns a deterministic
  `unsupported_operation` with capability context, never a vague error (see
  [backend-architecture.md](backend-architecture.md)).

## Policy Test (The Canonical Dry-Run)

`policy test` evaluates whether a given subject/permission/target/scope would be
allowed under current policy without performing any action. It is the access
analogue of a dry run.

- Inputs: subject, permission, target, scope, and optional record attributes
  (kind/tag/name/sensitive) to exercise filters.
- Output: `allow` with the deciding Policy id, or `deny` with the stable reason
  and the missing grant.
- Surfaces: CLI `policy test`, MCP `witself.policy.test`, and API
  `POST /v1/policies:test`. All three share the request/response contract from
  [json-contracts.md](json-contracts.md).
- `policy test` does not mutate state and (by configuration) may record a
  decision audit event for security review without counting as a real access.

## Caller Responsibilities

A caller (agent runtime, CLI user, or MCP client) performing cross-agent access
must:

- Treat data read from another agent as that agent's identity, not the caller's,
  and treat message-driven write requests as untrusted input (see
  [inter-agent-messaging.md](inter-agent-messaging.md)).
- Run `policy test` or `--dry-run` before a broad `curate`/`forget` to confirm
  scope and blast radius.
- Supply a meaningful audit `--reason` for cross-agent mutation; do not pass a
  placeholder.
- Prefer soft `forget` over hard delete; reach for hard delete only with explicit
  intent and the required scope.
- Never log another agent's memory content or fact values.

## Backend Responsibilities

The backend (core service behind every frontend) must:

- Authenticate the caller and derive the actor from the token, never from input.
- Evaluate Policy default-deny for every cross-agent request, returning the
  deciding Policy id or a stable deny reason.
- Enforce token scope and capability gating alongside Policy.
- Apply per-agent ownership isolation; an agent reaches another agent's data only
  through a matching grant or operator override.
- Enforce `--reason`, `--dry-run`, and confirmation guardrails on cross-agent
  `curate`/`forget` and on hard delete.
- Write a fully attributed audit record (including denials) for every
  cross-agent decision and mutation.
- Tombstone cross-agent forgets by default and support `restore` within the
  retention window.
- Keep memory content, fact values, message bodies/payloads, embedding vectors,
  and raw tokens out of logs, audit records, analytics, support data, and errors.

## Data-At-Rest Note

Witself does not treat encryption as a product pillar. Identity data is protected
for **integrity and authenticity**, not confidentiality of a secret value, so
there is no KMS/envelope/client-side-decrypt model, no reveal ceremony, and no
end-to-end secret boundary inherited from Witpass.

- Use ordinary data-at-rest encryption (managed RDS/disk, or self-host-owned disk
  encryption). Memory content and fact values are ordinary identity columns.
- Optional field-level encryption of `sensitive` facts is a capability an
  operator may enable, not the default. `sensitive` is a PII/redaction display
  flag, not an encryption boundary.
- KMS is optional and demoted, not a core dependency. The full data-at-rest and
  storage decision lives in [storage.md](storage.md).

## Related Docs

- [requirements.md](requirements.md)
- [memory-model.md](memory-model.md)
- [facts-model.md](facts-model.md)
- [security-groups.md](security-groups.md)
- [inter-agent-messaging.md](inter-agent-messaging.md)
- [threat-model.md](threat-model.md)
- [audit-retention.md](audit-retention.md)
- [json-contracts.md](json-contracts.md)
- [api-contract.md](api-contract.md)
- [backend-architecture.md](backend-architecture.md)
- [storage.md](storage.md)
- [post-v0-roadmap.md](post-v0-roadmap.md)
