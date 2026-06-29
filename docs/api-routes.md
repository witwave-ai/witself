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
- `/v1/secrets`
- `/v1/totp`
- `/v1/policies`
- `/v1/groups`
- `/v1/messages`
- `/v1/conversations`
- `/v1/federation`
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
POST /v1/secrets/{secret_id}:reveal
POST /v1/secrets/{secret_id}:rotate
POST /v1/secrets/{secret_id}:grant
POST /v1/totp/{secret_id}:code
POST /v1/policies:test
POST /v1/messages/{message_id}:ack
POST /v1/tokens/{token_id}:rotate
```

Sensitive/action routes must use `POST`, never `GET`.

Witself's action verbs span both planes. The open-plane verbs (`:recall`,
`:forget`, `:restore`, `:primary`, `:test`, `:ack`) protect the integrity and
authenticity of identity data. The sealed-plane verbs (`:reveal`, `:rotate`,
`:grant`, `:revoke`, `:archive`, `:restore`, plus `/v1/totp/{secret_id}:code`)
protect the confidentiality of secret material and are the only routes that
return plaintext secret values — and only through the explicit, audited reveal
ceremony described in [secret-model.md](secret-model.md). Sealed-plane material
is never embedded, never returned by semantic recall, never in the self-digest,
and never in the plaintext export; see the carve-outs in
[requirements.md](requirements.md).

## Route Style Rules

- Use `/v1` as the first HTTP route contract version.
- Use plural resource names.
- Use nested routes when ownership matters.
- Use action suffixes for non-CRUD workflows.
- Keep memory content, fact values, message bodies and payloads, embedding
  vectors, secret values, field values, TOTP seeds, TOTP codes, generated
  passwords, raw tokens, audit reasons, payment credentials, and wallet
  credentials out of URL paths and query strings.
- Use request bodies for sensitive inputs such as recall queries, memory and
  fact content, secret field names, TOTP enrollment material, password
  generation policy, audit reasons, policy definitions, message bodies, token
  rotation options, and payment workflow inputs.
- Generate OpenAPI from Go route/schema definitions or from one equivalent
  source of truth.

## Core Route Sketch

Initial route sketch:

```text
# Metrics listener, default :9090.
GET  /metrics

# Health listener, default :8081.
GET  /livez
GET  /readyz
GET  /startupz
GET  /healthz                # alias

# Signed realm card, served at the well-known path (not under /v1).
GET  /.well-known/witself-card.json

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

# Sealed plane: secrets + TOTP. Reveal-gated; never embedded, recalled,
# in the self-digest, or in the plaintext export.
GET  /v1/secrets             # metadata only; ?all_agents=true is operator/admin-only
POST /v1/secrets
GET  /v1/secrets/{secret_id} # metadata only; values require :reveal
PATCH /v1/secrets/{secret_id}
POST /v1/secrets/{secret_id}:reveal
POST /v1/secrets/{secret_id}:rotate
POST /v1/secrets/{secret_id}:copy
POST /v1/secrets/{secret_id}:archive
POST /v1/secrets/{secret_id}:restore
DELETE /v1/secrets/{secret_id}
POST /v1/secrets/{secret_id}:grant
POST /v1/secrets/{secret_id}:revoke

GET  /v1/agents/{agent_id}/secrets
POST /v1/agents/{agent_id}/secrets
GET  /v1/groups/{group_id}/secrets
POST /v1/groups/{group_id}/secrets

POST /v1/totp/{secret_id}:enroll
GET  /v1/totp/{secret_id}    # enrollment metadata only; seed requires :reveal
POST /v1/totp/{secret_id}:code
DELETE /v1/totp/{secret_id}

POST /v1/password:generate

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
POST /v1/messages:listen        # long-poll receive; drains the durable mailbox

# Cross-realm conversation/task resource (post-v0 collaboration).
GET  /v1/conversations
GET  /v1/conversations/{conversation_id}

# Realm federation allow-list (accepted peers). federation:manage scope.
GET    /v1/federation/peers
POST   /v1/federation/peers
DELETE /v1/federation/peers/{peer}

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
content, fact values, message bodies or payloads, embedding vectors, secret
values, field values, TOTP seeds or codes, raw paths, query strings, user
input, or high-cardinality customer metadata.

Health routes should be served on the dedicated health listener, default
`:8081`, at the short `/livez`, `/readyz`, `/startupz` (plus `/healthz` alias)
paths rather than under `/v1`.

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
  (`ws memory consolidate`). It merges near-duplicate memories, supersedes
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
- `POST /v1/secrets/{secret_id}:reveal` is the explicit, audited value-returning
  op (`ws secret reveal`). It runs the reveal ceremony, requires the
  `secret:reveal` scope, and is audited as `secret.reveal`. The field selector
  and audit reason travel in the request body, never the path. The response is
  either the client-decryptable envelope or, for token-only pods, the
  server-mediated plaintext behind the `server_side_decrypt` capability — see
  [key-hierarchy.md](key-hierarchy.md) for the two shapes; the chosen path is
  recorded on the audit event. This route is disabled by the MCP
  `--no-value-tools` switch. It has no `GET` equivalent: secret values are never
  reachable by a plain read.
- `POST /v1/secrets/{secret_id}:rotate` replaces secret field values (or
  re-generates them) and re-wraps the per-secret/field DEK, audited as
  `secret.updated`/`key.rotated` as applicable. It does not return plaintext;
  callers reveal separately.
- `POST /v1/secrets/{secret_id}:archive` is the reversible soft-retire path and
  `POST /v1/secrets/{secret_id}:restore` reverses it within the retention
  window; `DELETE /v1/secrets/{secret_id}` is the guarded hard delete that
  crypto-shreds the DEK. They are audited as `secret.archived`/`secret.restored`/
  `secret.deleted`.
- `POST /v1/secrets/{secret_id}:grant` and `POST /v1/secrets/{secret_id}:revoke`
  manage cross-agent and group access to a sealed secret (`secret:grant` scope),
  audited as `secret.grant`/`secret.revoke`. Sealed-plane access is governed by
  grants plus realm roles in
  [authorization-and-roles.md](authorization-and-roles.md), not by the open
  cross-agent read/curate/forget policy engine; secrets are not subject to those
  verbs. The grantee, target, and audit reason travel in the body.
- `POST /v1/totp/{secret_id}:code` returns a current TOTP code for an enrolled
  secret (`totp:code` scope), audited as `totp.code`. The seed itself is
  high-value sealed material revealed only through `:reveal`; the code is a
  short-lived value-returning op disabled by `--no-value-tools`. See
  [totp-2fa.md](totp-2fa.md).
- `POST /v1/password:generate` returns a generated password under a requested
  policy (length, character classes, passphrase mode). The policy travels in the
  body and the generated value in the response only; it is never persisted by
  this route and never placed in a URL. Storing it as a secret is a separate
  `POST /v1/secrets` call.

Cross-agent and operator-override mutations over `:forget`, `DELETE`, and the
`curate`-style `PATCH` routes require an audit reason in the request body and
support `dry_run`; see [api-contract.md](api-contract.md).

Some CLI commands map onto these routes without a dedicated path:

- `ws agent rename` is plain CRUD over `PATCH /v1/agents/{agent_id}`; its
  `--rotate-tokens` option composes the existing token `:rotate` action.
- `ws memory adjust` and `ws fact set` are plain CRUD over
  `PATCH /v1/memories/{memory_id}` and `POST`/`PATCH` on `/v1/facts`; the
  `--primary` option composes the `:primary` action.
- `ws auth login` uses `POST /v1/auth/sessions` and `ws whoami` uses
  `GET /v1/whoami`, but `ws auth status` and `ws auth logout` are
  local-only client operations over cached credentials and have no server
  route.

## Cross-Realm Collaboration Routes

These routes back the post-v0 cross-realm collaboration substrate; see
[agent-collaboration.md](agent-collaboration.md). They reuse the existing
`/v1/messages` resource and add a long-poll receive verb, a conversation/task
resource, the realm federation allow-list, and the signed realm card.

```text
POST /v1/messages:listen        # long-poll receive; drains the durable mailbox

GET  /v1/conversations
GET  /v1/conversations/{conversation_id}

GET    /v1/federation/peers
POST   /v1/federation/peers
DELETE /v1/federation/peers/{peer}

GET  /.well-known/witself-card.json
```

- `POST /v1/messages:listen` is the long-poll receive verb: it blocks up to a
  caller-supplied timeout (bounded server-side) and returns the inbound messages
  drained from the caller's durable mailbox, the source of truth for
  conversation state. It is the live face of the mailbox, not a separate
  transport — a dropped connection loses no state, and the next `:listen` (or a
  plain `GET /v1/messages`) drains whatever has arrived. The timeout and
  optional `conversation_id` filter travel in the request body. This is the HTTP
  surface for the CLI/MCP `listen`/`recv` verb (`ws message listen`,
  `witself.message.listen`); it honors `--read-only`. Local and cross-realm
  inbound arrive on the same route — a cross-realm message carries no authority
  and still resolves against a standing receive policy in this realm.
- `GET /v1/conversations` and `GET /v1/conversations/{conversation_id}` expose
  the cross-realm conversation/task resource and its A2A-style state machine
  (`submitted`, `working`, `input_required`, `auth_required`, `completed`,
  `failed`, `canceled`), participants, and the per-conversation turn/cost budget
  and remaining turns. Conversation ids reuse the existing `thr_` prefix. The
  resource is read-oriented here; conversations advance by sending and listening
  over `/v1/messages`, and state transitions emit the
  `conversation.*` audit events.
- `/v1/federation/peers` is the realm's deny-by-default accepted-peer allow-list:
  which realm handles and signing keys this realm will exchange messages with.
  `GET` lists the allow-list, `POST` adds a peer, and
  `DELETE /v1/federation/peers/{peer}` removes one (revocation takes effect for
  subsequent acceptance decisions). These routes require the
  `federation:manage` operator scope; they govern *which* peers are accepted,
  while a cross-realm `POST /v1/messages` still uses `message:send` and is
  additionally gated by per-conversation consent. Peer add/remove and consent
  decisions emit the `federation.*` audit events. See
  [access-policy.md](access-policy.md) and
  [authorization-and-roles.md](authorization-and-roles.md).
- `GET /.well-known/witself-card.json` serves the realm's signed card — its
  handle, advertised agents and skills, endpoint, accepted auth, signing
  (JWKS public key), delivery modes, and expiry — under a JWS signature over the
  canonicalized card. Signing is mandatory; an unsigned or unverifiable card is
  not honored. It is intentionally **not** under `/v1`: it is the well-known
  discovery surface a peer realm reads before federating, the cross-realm
  analog of `/metrics` living outside the product API. Publishing and rotating
  the card is a `federation:manage` operation.

Cross-realm placement is separate. The home-cell resolution that tells a CLI or
peer *which* cell a realm lives on is a control-plane surface, not a per-cell
`/v1` route; see [deployment-cells.md](deployment-cells.md). Once a caller has
resolved a realm's home cell, the routes above are served by that cell exactly
as documented here.

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

- `GET /v1/self` returns the bounded self-digest (`ws self show`): primary
  facts first, then top-N salient memories, then a one-line index of
  kinds/tags/counts. It is cheap, never requires the embedding provider, and is
  hard-capped (default ~8 KiB); when capped it sets `elided=true` and points to
  `:recall` rather than silently truncating. Query parameters select what to
  include (facts, salient memories, salient limit, byte cap). Passing `?format=`
  renders the digest as a CLAUDE.md/AGENTS.md/Markdown fragment with witself
  provenance comments — this is the HTTP surface for `ws digest emit`; no
  separate emit resource exists.
- `POST /v1/remember` is the convenience capture path (`ws remember`). It
  auto-routes: a clear name→value assertion upserts a fact, anything else adds a
  verbatim memory with dedup/supersede. It never bypasses validation or limits,
  composes the same create paths as `POST /v1/facts` and `POST /v1/memories`, and
  returns the created/updated resource plus a `kind` discriminator (and
  `duplicate_of` when merged). It emits no event of its own; it routes to the
  existing `fact.created`/`fact.updated`/`memory.added` events.
- `POST /v1/sessions:start` hydrates identity, open goals, and last progress in
  one round-trip (`ws session start`), audited as `session.started`.
- `POST /v1/sessions:end` persists a progress memory (kind `session`) and updates
  open goals (`ws session end`); the summary and open goals travel in the
  request body, audited as `session.ended`.

`ws ingest` has no dedicated route: it composes the existing
`POST /v1/facts` (kv-shaped lines → upserted facts) and `POST /v1/memories`
(prose → memories) create paths, tagging records `source=import:<file>` with
dedup/upsert, and is audited as `fact.imported` / `memory.imported`.
`ws bootstrap-instructions` is a local client operation that prints the
paste-able teaching stanza and has no server route.

## Account Routes

The `/v1/accounts` resource backs the `ws account` CLI noun: the
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
  (`ws account create`); `GET`/`PATCH /v1/accounts/{account_id}` back
  `ws account show` and `ws account update`.
- `GET /v1/accounts/{account_id}/members` lists human operators/admins
  (`ws account members`).
- `POST /v1/accounts/{account_id}/members:invite` invites a human
  operator/admin (`ws account invite`); the invite email travels in the
  request body, never the path.
- `DELETE /v1/accounts/{account_id}/members/{principal}` removes a member
  (`ws account remove`);
  `POST /v1/accounts/{account_id}/members/{principal}:set-role` changes a
  member's account-level role (`ws account set-role`).
- `POST /v1/accounts/{account_id}:close` closes the account
  (`ws account close`). It is audited and supports `dry_run`.

`ws account export` is an account-scoped export job served by the
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

Sealed-plane secrets use the same ownership model — every secret is owned by an
agent or a group, the same `owner ∈ {agent, group}` rule that governs memories
and facts (there is no separate "shared" scope):

```text
GET  /v1/agents/{agent_id}/secrets
POST /v1/agents/{agent_id}/secrets
GET  /v1/groups/{group_id}/secrets
POST /v1/groups/{group_id}/secrets
```

These nested routes are the HTTP surface for operator and group targeting. The
CLI `--owner-agent <agent>` flag targets `/v1/agents/{agent_id}/secrets` (and
the agent-scoped `:reveal`/`:grant`/etc. via the bare `/v1/secrets/{secret_id}`
once the secret is resolved), and `--group <name>` targets
`/v1/groups/{group_id}/secrets` for group-owned secrets. They mirror the secret
reference forms `witself://agent/<agent>/secret/<path>/<field>` and
`witself://group/<group>/secret/<path>/<field>`; the bare
`witself://secret/<path>/<field>` resolves against the caller's own agent. Using
`--owner-agent` is an operator/admin or policy-granted action; resolving or
revealing another agent's or a group's secret requires a grant (`secret:grant`
issued via `:grant`) or a realm role, never the open cross-agent read policy.

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

Identity export and import are first-class Witself resources: the open plane
(memories and facts) exports as plaintext, the headline durable-state feature.
The sealed plane is carved out — `POST /v1/exports` never includes secret values
or TOTP seeds; secret backup is encrypted-only (envelope plus KMS key identity)
behind an explicit, separate, audited flag, and is never part of the plaintext
identity export. The routes back `ws export` and `ws import`:

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
- [secret-model.md](secret-model.md)
- [totp-2fa.md](totp-2fa.md)
- [encryption-model.md](encryption-model.md)
- [key-hierarchy.md](key-hierarchy.md)
- [authorization-and-roles.md](authorization-and-roles.md)
- [access-policy.md](access-policy.md)
- [security-groups.md](security-groups.md)
- [inter-agent-messaging.md](inter-agent-messaging.md)
- [agent-collaboration.md](agent-collaboration.md)
- [deployment-cells.md](deployment-cells.md)
- [observability-and-operations.md](observability-and-operations.md)
