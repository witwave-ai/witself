# Witself Security Groups

Status: target, not implemented. Reconciled for #342 under
`access-policy-contract-reconciliation`.

## Current contract

The server advertises `features.groups` and `features.policies` as
`{"supported":false,"reason":"not_implemented"}` in the flat
`GET /v1/capabilities` response. The response has `principal: null`; it describes
feature availability, not the caller's permissions. These wire fields are pinned
in [Capability discovery](api-contract.md#capability-discovery) and implemented
in `internal/server/server.go:3130-3153,3213-3218,3247-3248`.

There are no registered `/v1/groups` or `/v1/policies` handlers in
`internal/server/server.go`, and the CLI dispatch has no `group` or `policy`
command (`cmd/witself/main.go:121-198`). Group-management and collective-memory
examples from the earlier draft are not runnable instructions.

Today's principals are operators and agents, derived from credentials. Agent
tokens bind an agent to one realm/account; operator tokens bind an operator to
an account. Groups are not an authenticated principal kind
(`internal/store/auth.go:43-75,222-278`). The existing `account_owner` and
`account_operator` roles do not create group administrators or memberships
(`internal/store/auth.go:199-219`, `internal/store/operator.go:17,152-161`).
See [Authentication](api-contract.md#authentication) for the current token and
wire contracts.

Current facts and narrative-memory retrieval are agent-owned and scoped by
account, realm, and owner. Group ownership cannot be selected through these
store operations (`internal/store/fact.go:460-462`,
`internal/store/memory.go:470-481,1200-1204`). A realm is therefore not a shared
fact or memory store merely because several agents belong to it.

## Target group model

The access-policy rock must settle and implement the following before groups
can be documented as available:

- Named realm-local collections and membership management.
- Group subjects and targets in a cross-agent policy evaluator.
- Group-owned collective facts/memories and their ownership, retrieval,
  mutation, conflict, and deletion contracts.
- Delegated membership administration, scope/role checks, previews, and audit.
- CLI and MCP group operations and tested HTTP request/response shapes.

No group schema, scope vocabulary (`group:read`, `group:manage`,
`group:member`), policy precedence, immediate membership-revocation behavior,
or deletion/re-homing policy is promised by the current implementation.
The earlier `grp_` record fields, `members[]`/`admins[]` semantics, `--group`
write/recall examples, and `witself.group.*` tools are removed as operational
contracts. They remain design questions under the access-policy rock.

## Target API shapes

[Cross-Agent And Group Targeting](api-contract.md#cross-agent-and-group-targeting)
retains draft membership routes as targets:

```text
GET    /v1/groups/{group_id}/members              # target; not implemented
POST   /v1/groups/{group_id}/members              # target; not implemented
DELETE /v1/groups/{group_id}/members/{principal}  # target; not implemented
```

The old `:add-member`, `:remove-member`, and `:delete` route examples are removed;
they are not shipped endpoints or current API pins. `owner_group` targeting and
collective-memory authorization in that API section also remain target-only.

<a id="cross-realm-channels"></a>

## Cross-Realm Channels (Target)

Cross-realm groups/channels and federation authorization remain deferred
access-policy work. They do not extend today's token-bound realm or agent-owned
fact/memory access. No channel membership, group fan-out, or group-owned data
contract is implemented by the group capability above.

## See also

- [access-policy.md](access-policy.md) — current principal/ownership boundaries.
- [facts-model.md](facts-model.md) — agent-owned facts and deferred advanced policy.
- [api-contract.md](api-contract.md) — implemented discovery and target group routes.
