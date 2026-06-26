# Witself API Routes

Status: draft. Decision: Witself uses resource-oriented `/v1` routes with
explicit action subroutes for sensitive, integrity-impacting, or workflow
operations.

## Decision

The public HTTP API should be REST-ish and resource-oriented under `/v1`.

Use plural resources for ordinary collection and item routes:

- `/v1/accounts`
- `/v1/realms`
- `/v1/agents`
- `/v1/memories`
- `/v1/facts`
- `/v1/policies`
- `/v1/groups`
- `/v1/messages`
- `/v1/tokens`
- `/v1/audit`
- `/v1/exports`
- `/v1/imports`
- `/v1/auth`
- `/v1/bootstrap`
- `/v1/billing`
- `/v1/support`

Use explicit action subroutes for operations that are not plain CRUD,
especially when the operation is sensitive, audited, destructive,
integrity-impacting, or workflow-oriented.

Action routes should use a colon suffix:

```text
POST /v1/memories:recall
POST /v1/memories/{memory_id}:forget
POST /v1/memories/{memory_id}:restore
POST /v1/facts/{fact_id}:primary
POST /v1/policies:test
POST /v1/messages/{message_id}:ack
POST /v1/tokens/{token_id}:rotate
```

Sensitive/action routes must use `POST`, never `GET`.

Unlike Witpass, Witself has no `:reveal` action and no `/v1/totp` resource.
Witself's headline action verbs protect the integrity and authenticity of
identity data (`:recall`, `:forget`, `:restore`, `:primary`, `:test`, `:ack`),
not the confidentiality of secret material.

## Route Style Rules

- Use `/v1` as the first HTTP route contract version.
- Use plural resource names.
- Use nested routes when ownership matters.
- Use action suffixes for non-CRUD workflows.
- Keep memory content, fact values, message bodies and payloads, embedding
  vectors, raw tokens, audit reasons, payment credentials, and wallet
  credentials out of URL paths and query strings.
- Use request bodies for sensitive inputs such as recall queries, memory and
  fact content, audit reasons, policy definitions, message bodies, token
  rotation options, and payment workflow inputs.
- Generate OpenAPI from Go route/schema definitions or from one equivalent
  source of truth.

## Core Route Sketch

Initial route sketch:

```text
# Metrics listener, default :9090.
GET  /metrics

# Health listener, default :8081.
GET  /v1/health/live
GET  /v1/health/ready
GET  /v1/health/startup

# API listener, default :8080.
GET  /v1/version
GET  /v1/whoami
GET  /v1/capabilities

GET  /v1/self                # the always-loaded self-digest; ?format= renders an emit fragment
POST /v1/remember            # convenience capture; core routes to a fact or a memory

POST /v1/auth/sessions
GET  /v1/auth/sessions/{session_id}
POST /v1/auth/sessions/{session_id}:complete

POST /v1/bootstrap/operator

GET  /v1/accounts
POST /v1/accounts
GET  /v1/accounts/{account_id}
PATCH /v1/accounts/{account_id}
GET  /v1/accounts/{account_id}/members
POST /v1/accounts/{account_id}/members:invite
DELETE /v1/accounts/{account_id}/members/{principal}
POST /v1/accounts/{account_id}/members/{principal}:set-role
POST /v1/accounts/{account_id}:close

GET  /v1/realms
POST /v1/realms
GET  /v1/realms/{realm_id}
PATCH /v1/realms/{realm_id}
DELETE /v1/realms/{realm_id}

GET  /v1/agents
POST /v1/agents
GET  /v1/agents/{agent_id}
PATCH /v1/agents/{agent_id}
POST /v1/agents/{agent_id}:disable
POST /v1/agents/{agent_id}:enable
POST /v1/agents/{agent_id}:copy
DELETE /v1/agents/{agent_id}

GET  /v1/memories            # ?all_agents=true is operator/admin-only (realm-wide scan)
POST /v1/memories
GET  /v1/memories/{memory_id}
PATCH /v1/memories/{memory_id}
POST /v1/memories:recall
POST /v1/memories:consolidate
POST /v1/memories/{memory_id}:forget
POST /v1/memories/{memory_id}:restore
DELETE /v1/memories/{memory_id}

GET  /v1/agents/{agent_id}/memories
POST /v1/agents/{agent_id}/memories
GET  /v1/groups/{group_id}/memories
POST /v1/groups/{group_id}/memories

GET  /v1/facts               # ?all_agents=true is operator/admin-only (realm-wide scan)
POST /v1/facts
GET  /v1/facts/{fact_id}
PATCH /v1/facts/{fact_id}
POST /v1/facts/{fact_id}:primary
DELETE /v1/facts/{fact_id}

GET  /v1/agents/{agent_id}/facts
POST /v1/agents/{agent_id}/facts
GET  /v1/groups/{group_id}/facts
POST /v1/groups/{group_id}/facts

POST /v1/sessions:start
POST /v1/sessions:end

GET  /v1/policies
POST /v1/policies
GET  /v1/policies/{policy_id}
DELETE /v1/policies/{policy_id}
POST /v1/policies:test

GET  /v1/groups
POST /v1/groups
GET  /v1/groups/{group_id}
PATCH /v1/groups/{group_id}
DELETE /v1/groups/{group_id}
GET  /v1/groups/{group_id}/members
POST /v1/groups/{group_id}/members
DELETE /v1/groups/{group_id}/members/{principal}

GET  /v1/messages
POST /v1/messages
GET  /v1/messages/{message_id}
POST /v1/messages/{message_id}:ack

GET  /v1/tokens
POST /v1/tokens
GET  /v1/tokens/{token_id}
POST /v1/tokens/{token_id}:rotate
POST /v1/tokens/{token_id}:revoke

GET  /v1/audit
GET  /v1/audit/{event_id}

GET  /v1/exports
POST /v1/exports
GET  /v1/exports/{export_id}

GET  /v1/imports
POST /v1/imports
GET  /v1/imports/{import_id}

GET  /v1/billing
GET  /v1/billing/usage
GET  /v1/billing/limits
GET  /v1/billing/plans
POST /v1/billing/subscription
POST /v1/billing/payment-methods
GET  /v1/billing/sessions/{session_id}
POST /v1/billing/crypto:quote
POST /v1/billing/crypto:checkout

GET  /v1/support/tickets
POST /v1/support/tickets
GET  /v1/support/tickets/{ticket_id}
POST /v1/support/tickets/{ticket_id}:comment
POST /v1/support/tickets/{ticket_id}:close
```

This sketch is allowed to evolve during implementation, but the style should
remain stable.

`/metrics` is intentionally outside `/v1` because it is an operational
Prometheus scrape endpoint, not a product API resource. It should be served on
the dedicated metrics listener, default `:9090`, and must not expose memory
content, fact values, message bodies or payloads, embedding vectors, raw paths,
query strings, user input, or high-cardinality customer metadata.

Health routes should be served on the dedicated health listener, default
`:8081`, even though their paths use the `/v1/health/*` shape.

Auth session routes are for CLI-initiated browser/device-code login when
Witself owns the session flow. Self-hosted first-operator bootstrap should use
a one-time bootstrap token and `POST /v1/bootstrap/operator`; it must not rely
on a default admin password.

## Action Route Notes

The colon-action routes carry Witself's integrity-sensitive verbs. They are
`POST`-only, audited, and (where they mutate) support idempotency keys and
`dry_run`:

- `POST /v1/memories:recall` runs semantic-by-default recall over the caller's
  accessible memories. The query, filters (kind, tag, time), and ranking
  options travel in the request body, never in the path or query string. Recall
  over another agent's memories requires a policy granting `read` and is
  metered as a cross-agent access. When the embedding provider is unavailable,
  recall degrades to keyword/tag/kind/time ranking and the response surfaces the
  degraded state through `warnings`.
- `POST /v1/memories:consolidate` is the guarded garbage-collection verb
  (`witself memory consolidate`). It merges near-duplicate memories, supersedes
  stale ones, surfaces (never auto-resolves) conflicting facts, and trims the
  digest index. It defaults to `dry_run=true`, is audited as
  `memory.consolidated`, respects `source` provenance so human-/import-authored
  records are never silently overwritten, and is excluded in `--read-only` MCP
  mode.
- `POST /v1/memories/{memory_id}:forget` is the soft-delete (tombstone) path. It
  is reversible within the retention window. `DELETE /v1/memories/{memory_id}`
  is the guarded hard delete.
- `POST /v1/memories/{memory_id}:restore` reverses a forget within the retention
  window.
- `POST /v1/facts/{fact_id}:primary` is the atomic primary promotion. It demotes
  any prior primary of the same logical kind for the same owner.
- `POST /v1/policies:test` evaluates whether a given subject, permission,
  target, and scope would be allowed under current policy, returning the
  deciding policy id or a deny reason. It is the canonical dry-run for access
  decisions and does not mutate state.
- `POST /v1/messages/{message_id}:ack` records per-recipient acknowledgement.
  The message sender is always derived server-side from the token, never from
  the request body; sender forgery is structurally impossible.
- `POST /v1/tokens/{token_id}:rotate` issues a replacement token. The raw token
  value is returned once.

Cross-agent and operator-override mutations over `:forget`, `DELETE`, and the
`curate`-style `PATCH` routes require an audit reason in the request body and
support `dry_run`; see [api-contract.md](api-contract.md).

Some CLI commands map onto these routes without a dedicated path:

- `witself agent rename` is plain CRUD over `PATCH /v1/agents/{agent_id}`; its
  `--rotate-tokens` option composes the existing token `:rotate` action.
- `witself memory adjust` and `witself fact set` are plain CRUD over
  `PATCH /v1/memories/{memory_id}` and `POST`/`PATCH` on `/v1/facts`; the
  `--primary` option composes the `:primary` action.
- `witself auth login` uses `POST /v1/auth/sessions` and `witself whoami` uses
  `GET /v1/whoami`, but `witself auth status` and `witself auth logout` are
  local-only client operations over cached credentials and have no server
  route.

## Self-Management And Hydration Routes

These routes back the agent self-managed memory and hydration surface; see
[context-hydration.md](context-hydration.md). Every mutating route returns the
deterministic `echo` string and any `warnings[]` (e.g. `memory_duplicate`)
described in [api-contract.md](api-contract.md).

```text
GET  /v1/self                # ?format=claude-md|agents-md|markdown for digest emit
POST /v1/remember
POST /v1/sessions:start
POST /v1/sessions:end
POST /v1/memories:consolidate
```

- `GET /v1/self` returns the bounded self-digest (`witself self show`): primary
  facts first, then top-N salient memories, then a one-line index of
  kinds/tags/counts. It is cheap, never requires the embedding provider, and is
  hard-capped (default ~8 KiB); when capped it sets `elided=true` and points to
  `:recall` rather than silently truncating. Query parameters select what to
  include (facts, salient memories, salient limit, byte cap). Passing `?format=`
  renders the digest as a CLAUDE.md/AGENTS.md/Markdown fragment with witself
  provenance comments — this is the HTTP surface for `witself digest emit`; no
  separate emit resource exists.
- `POST /v1/remember` is the convenience capture path (`witself remember`). It
  auto-routes: a clear name→value assertion upserts a fact, anything else adds a
  verbatim memory with dedup/supersede. It never bypasses validation or limits,
  composes the same create paths as `POST /v1/facts` and `POST /v1/memories`, and
  returns the created/updated resource plus a `kind` discriminator (and
  `duplicate_of` when merged). It emits no event of its own; it routes to the
  existing `fact.created`/`fact.updated`/`memory.added` events.
- `POST /v1/sessions:start` hydrates identity, open goals, and last progress in
  one round-trip (`witself session start`), audited as `session.started`.
- `POST /v1/sessions:end` persists a progress memory (kind `session`) and updates
  open goals (`witself session end`); the summary and open goals travel in the
  request body, audited as `session.ended`.

`witself ingest` has no dedicated route: it composes the existing
`POST /v1/facts` (kv-shaped lines → upserted facts) and `POST /v1/memories`
(prose → memories) create paths, tagging records `source=import:<file>` with
dedup/upsert, and is audited as `fact.imported` / `memory.imported`.
`witself bootstrap-instructions` is a local client operation that prints the
paste-able teaching stanza and has no server route.

## Account Routes

The `/v1/accounts` resource backs the `witself account` CLI noun: the
managed-service customer account, its human operators/admins, billing ownership,
and account closure. Account operations are operator/admin-only.

```text
POST /v1/accounts
GET  /v1/accounts/{account_id}
PATCH /v1/accounts/{account_id}
GET  /v1/accounts/{account_id}/members
POST /v1/accounts/{account_id}/members:invite
DELETE /v1/accounts/{account_id}/members/{principal}
POST /v1/accounts/{account_id}/members/{principal}:set-role
POST /v1/accounts/{account_id}:close
```

- `POST /v1/accounts` creates a managed-service customer account
  (`witself account create`); `GET`/`PATCH /v1/accounts/{account_id}` back
  `witself account show` and `witself account update`.
- `GET /v1/accounts/{account_id}/members` lists human operators/admins
  (`witself account members`).
- `POST /v1/accounts/{account_id}/members:invite` invites a human
  operator/admin (`witself account invite`); the invite email travels in the
  request body, never the path.
- `DELETE /v1/accounts/{account_id}/members/{principal}` removes a member
  (`witself account remove`);
  `POST /v1/accounts/{account_id}/members/{principal}:set-role` changes a
  member's account-level role (`witself account set-role`).
- `POST /v1/accounts/{account_id}:close` closes the account
  (`witself account close`). It is audited and supports `dry_run`.

`witself account export` is an account-scoped export job served by the
`/v1/exports` resource, not a dedicated account route.

## Ownership Routes

Default agent token use should not require an agent ID in the route. The token
already binds the caller to one realm and one named agent. A bare
`GET /v1/memories` lists the caller's own memories; `POST /v1/memories` creates
a memory owned by the caller.

A bare `GET /v1/memories` and `GET /v1/facts` list only the caller's own
records. Operators/admins may pass `all_agents=true` on either listing route to
run a realm-wide scan across every agent's records. The `all_agents=true` query
parameter is operator/admin-only and is rejected for ordinary agent tokens; it
is the HTTP surface for the MCP `all_agents` inventory flag.

Operator/admin and policy-granted callers may use nested ownership routes when
they need to target a specific agent's resources:

```text
GET  /v1/agents/{agent_id}/memories
POST /v1/agents/{agent_id}/memories
GET  /v1/agents/{agent_id}/facts
POST /v1/agents/{agent_id}/facts
```

Group-owned (collective) identity data uses explicit group ownership routes:

```text
GET  /v1/groups/{group_id}/memories
POST /v1/groups/{group_id}/memories
GET  /v1/groups/{group_id}/facts
POST /v1/groups/{group_id}/facts
```

Group membership is managed through nested member routes:

```text
GET    /v1/groups/{group_id}/members
POST   /v1/groups/{group_id}/members
DELETE /v1/groups/{group_id}/members/{principal}
```

Passing an agent ID or group ID in a route is a target, not authentication.
Authorization still comes from the bearer token: reading or writing another
agent's or group's records requires a policy that permits it (or operator
override), evaluated below the route the same way `POST /v1/policies:test`
evaluates it.

## Export And Import Routes

Identity export and import are first-class Witself resources, in deliberate
contrast to Witpass, which forbids plaintext export. The routes back
`witself export` and `witself import`:

- `POST /v1/exports` starts a structured/plaintext identity export (memories
  with edit history, facts with primary and sensitive flags, and, for
  operators, policies and group membership). Exporting `sensitive` records
  requires an audit reason in the body and is reported in `warnings`.
- `GET /v1/exports/{export_id}` reports export status and the artifact location
  for large exports staged in object/blob storage.
- `POST /v1/imports` restores an exported self. It is idempotent by stable id
  where ids are preserved, supports a rename/remap mode, and supports `dry_run`
  to preview created, updated, and conflicting records without persisting.
- `GET /v1/imports/{import_id}` reports import status and the resolved record
  counts.

## Related Docs

- [api-contract.md](api-contract.md)
- [requirements.md](requirements.md)
- [context-hydration.md](context-hydration.md)
- [v0-scope.md](v0-scope.md)
- [json-contracts.md](json-contracts.md)
- [cli-command-surface.md](cli-command-surface.md)
- [mcp-tools.md](mcp-tools.md)
- [memory-model.md](memory-model.md)
- [facts-model.md](facts-model.md)
- [access-policy.md](access-policy.md)
- [security-groups.md](security-groups.md)
- [inter-agent-messaging.md](inter-agent-messaging.md)
- [observability-and-operations.md](observability-and-operations.md)
