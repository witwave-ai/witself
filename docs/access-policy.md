# Witself Cross-Agent Access Policy

Status: current contract reconciliation for #342; declarative cross-agent
Policy objects remain target work. Gate: `access-policy-contract-reconciliation`.
This is a checkout contract, not a deployment claim.

## Implemented principals and credentials

Authenticated domain principals are `operator` or `agent`. Authentication derives
their identity from the stored token and its account/agent/realm relationships;
request fields do not replace that identity. Agent credentials carry one agent,
realm, and account. Operator credentials carry an operator and account, with no
agent or realm binding (`internal/store/auth.go:222-278`).

The current credential boundaries are:

| Credential | Implemented boundary |
|---|---|
| Bootstrap | Single-use exchange into an operator credential; it is not an ordinary domain principal. |
| Operator | Account management through operator-authenticated routes; ordinary management requires an active account. |
| Agent, `full` profile | Domain routes apply their own agent and ownership checks within the token-bound account and realm. |
| Agent, `curator-preview` or `curator-apply` profile | Restricted, expiring credentials admitted only by the explicit curation permissions; ordinary domain routes reject them. |
| Provision/service credential | Separately configured cell control-plane authority. It is not an `admin` value accepted by ordinary operator/agent authentication. |

Evidence: `internal/store/auth.go:43-75,102-173,222-278`,
`internal/server/server.go:3312-3400,4107-4125`. Curator credentials require a
display name and a positive TTL no greater than 24 hours
(`internal/server/server.go:6453-6500`).

Account roles are distinct from token kinds: newly created non-root operators
receive `account_operator`; the root operator resolves to `account_owner`.
These are account roles, not implemented per-realm policy memberships
(`internal/store/operator.go:17,152-161`, `internal/store/auth.go:199-219`).
The current wrappers do not implement the draft `realm:admin`, `policy:*`,
`group:*`, or `memory:*-others` scope evaluator. Authorization is determined by
the route's principal/profile checks and store ownership rules.

## Ownership and operator access

Facts and narrative memories use the authenticated agent as owner. Fact reads
and writes require an agent token, and fact lookup includes account, realm, and
owner-agent predicates. Memory get/list likewise require an agent principal and
filter to its account, realm, and agent-owned records
(`internal/server/fact.go:219-224,291-296`,
`internal/store/fact.go:460-462`, `internal/store/memory.go:459-481,1200-1204`).

On the direct fact and memory routes, an operator credential does not confer
access to any agent's facts or narrative memories; those handlers reject
operator principals. This is a route-level restriction, not a data-access
boundary: the whole-account export route (`GET /v1/export`) accepts an
`account_operator` bearer token through `requireOperatorAnyStatus`
(`internal/server/export_self.go:45`), and the archive it streams contains every
agent's fact assertion values and memory version contents, including records
marked sensitive (`internal/store/export.go:956-979,1186-1213`). Treat an
operator token as able to read the account's complete content through export.
The account-wide transcript audit-read is likewise a separate operator
permission: operators may list/read account transcripts; agents see their own
(`internal/store/transcript.go:410-425,448-467`).

The secret vault also requires a full agent principal; this document does not
grant operator or group access to it (`internal/server/secret.go:467-476`).
The client-custodied ciphertext boundary is recorded in the
[API contract amendment](api-contract.md): the backend never decrypts sealed
field packages.

## Implemented wire contracts

[Authentication](api-contract.md#authentication) pins the bearer header,
`POST /v1/auth/bootstrap` exchange, operator-only `GET /v1/whoami` (also
`GET /v1/auth/whoami`), and curator-token request/response fields. These auth
responses are flat `witself.v0` objects; do not wrap them in `ok`/`data`.

[Capability discovery](api-contract.md#capability-discovery) is public and
returns `principal: null`. The current `features.policies` and `features.groups`
entries each contain `{"supported":false,"reason":"not_implemented"}`
(`internal/server/server.go:3130-3153,3213-3218,3247-3248`). Capability discovery
does not authenticate a caller or grant data access.

## Target access-policy work

The access-policy rock still needs implementation and contract review for:

- Realm-local cross-agent Policy objects, filtered permissions, and a policy
  decision/test API.
- Group subjects, targets, membership, and group-owned facts or memories.
- Cross-agent fact conflict authority and predicate/reminder policy, tracked by
  `advanced-fact-policy` in [facts-model.md](facts-model.md).
- Explicit cross-agent operator permissions, scope vocabulary, decision audit,
  and mutation/preview guardrails.
- Federation trust, cross-realm identity access, and channel authorization.

These are targets, not effective grants or executable workflows. The old
`read`/`contribute`/`curate`/`forget` policy verbs, union/deny precedence,
operator override, policy audit-event names, and group-deletion semantics are
not pinned as implemented decisions here. Their final behavior must be
reconciled with the owner-only fact/memory contracts before implementation.

The draft route shapes in
[Cross-Agent And Group Targeting](api-contract.md#cross-agent-and-group-targeting)
remain explicitly target-only. There is no shipped `witself policy` or
`witself group` command in `cmd/witself/main.go:121-198`; no policy/group HTTP
handler is registered in `internal/server/server.go`. The false capability
entries above are the current wire contract, not a policy decision response.

## Related docs

- [security-groups.md](security-groups.md) — current absence and deferred group work.
- [operator-auth.md](operator-auth.md) — implemented onboarding and roles.
- [facts-model.md](facts-model.md) — current fact model and deferred policy.
- [api-contract.md](api-contract.md) — implemented wire pins and marked targets.
