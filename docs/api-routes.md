# Witself API Routes

> **Sealed-plane implementation amendment (accepted 2026-07-23):** schema 67
> extends the current agent-owned ciphertext API through multi-installation AVK
> enrollment, crash-resumable AVK rotation, retained-secret status/enforcement,
> and guarded tombstone deletion. The exact implemented routes are listed in
> [Implemented sealed-plane routes](#implemented-sealed-plane-routes). They
> return public metadata, ciphertext, and wrapped DEKs only; the server never
> receives an AVK, pairing secret, enrollment private key, recovery artifact or
> passphrase, plaintext secret value, TOTP seed/code, or AI/model inference.
> Secret update, irreversible purge, grants, group ownership, runtime
> injection, and server-side reveal/TOTP routes described in target sections
> below remain unregistered and are superseded wherever they conflict with
> [ADR 0003](decisions/0003-client-custodied-agent-vault.md).

Status: draft. Decision: Witself uses resource-oriented `/v1` routes with
explicit action subroutes for sensitive, integrity-impacting, or workflow
operations.

Narrative-memory amendment (accepted 2026-07-14): direct capture, history,
lexical and optional client-vector hybrid recall, atomic supersede, lifecycle,
evidence resolution, permanent deletion, migration-0032 vector profiles/rows,
and the 15-endpoint client-curation protocol (including authenticated effective
preflight) are implemented below. The
curation routes manage a deterministic queue, fenced snapshots, exact
client-authored plans, transactional apply, and guarded compensation; they do
not run inference. Older server-classification/consolidation routes are
superseded. See
[narrative-memory-and-curation.md](narrative-memory-and-curation.md).

Messaging amendment (implemented in the current checkout): direct, bounded
explicit-list, and whole-realm sends share one immutable message plus
per-recipient delivery snapshots. Direct message actions include `:claim`,
`:renew`, `:release`, and atomic `:complete`. `/v1/message-requests` adds
message-backed open jobs, client-ranked selection, and separate exact claim
fences. This is not a deployment or release statement.

Realm-email-alias amendment (implemented in the current checkout): customer
requests and the platform-admin global namespace live in the control plane;
cells hold fenced routing projections and immutable delivery provenance. The
edge KV directory is a rebuildable projection, not claim authority. Activation
is separately default-off until a full-coverage SMTP route reaches the Email
Worker.

Custom inbound-domain amendment (dark foundation, implemented in the current
checkout): account operators may eventually request an organization-owned
domain through a separate control-plane authority. The current implementation
issues a stable TXT ownership challenge and contains separately gated manual
and scheduled DNS observation, ownership/lifecycle state, an append-only
authority journal, sealed empty-target recovery, and the schema-88 derived
cell/KV routing foundation. Request, verification, control-plane routing, and
Email Worker delivery each have independent exact-`true` gates; every one is
absent from release configuration. DNS is read only after the verification
gate passes. This slice performs no DNS/provider mutation, MX or Email Routing
change, or live mail delivery.

Billing-read amendment (dark foundation, implemented in the current checkout):
the control plane exposes account-scoped provider-neutral billing status,
bounded invoice/payment history, and hosted setup/portal actions. Reads never
create provider objects; setup is the explicit first-contact path. Its guarded
mutation route durably replays both provider-customer establishment and the
hosted setup action under one operation identity.
Strict pending-change cancellation distinguishes a successful cancel from a
race it did not win: the latter returns `kind: "resolved"` and omits
`cancelled`, plan, URL, and effective time. Exact pending retries remain valid
after an authorized actor, role, or account-email change; the initiating audit
attribution remains immutable on the receipt.
An expired account-lane reservation with no receipt can be safely replaced;
every late receipt creator must revalidate the original generation and claim
before it can reach a provider.
Outbound billing receipts now join a fixed-shard global recovery queue before
provider work. The hosted plan-lifecycle clock resumes bounded windows under
the same account and receipt fences, stops provider retries before the assumed
idempotency horizon, and exposes only value-free nested status. Private
`GET /v1/plan-lifecycle/metrics` uses the internal bridge bearer and closed
Prometheus labels; `/healthz` remains liveness-only. Receipt schema 2 pins the
approved effect and monetary terms so catalog drift cannot reinterpret queued
work. Billing remains dark pending the separate activation gates below.
No provider/customer identifiers or raw payment data cross the API. This is a
code-contract statement, not a release, deployment, or live-charging statement.
Every billing response uses `Cache-Control: no-store`; optional document links
that fail hosted-HTTPS validation are omitted, while an unsafe required action
fails closed. Clients first authenticate the operator against the selected
account cell with `GET /v1/whoami`, verify that principal's account id, and use
its authenticated `account_role` to enforce distinct `billing:read` and
`billing:manage` permissions. Older cells without the role fail closed. They then
read the same cell's public, value-free `GET /v1/capabilities` billing block
without a bearer and use only its validated endpoint. This supports managed
cells with many accounts without treating a deployment account as tenant
identity. Credential-bearing API requests never follow redirects; a changed
origin must be rediscovered and revalidated explicitly. Billing-disabled cells
never fall back to a public control plane.

## Implemented sealed-plane routes

```text
POST /v1/vault/key-epochs
GET  /v1/vault/key-epochs/current

POST /v1/vault/enrollments
GET  /v1/vault/enrollments
GET  /v1/vault/enrollments/{enrollment}
POST /v1/vault/enrollments/{enrollment}:approve
POST /v1/vault/enrollments/{enrollment}:receive
POST /v1/vault/enrollments/{enrollment}:consume
POST /v1/vault/enrollments/{enrollment}:cancel

POST /v1/vault/rotations
GET  /v1/vault/rotations/open
GET  /v1/vault/rotations/{rotation}
GET  /v1/vault/rotations/{rotation}/items
POST /v1/vault/rotations/{rotation}:stage
POST /v1/vault/rotations/{rotation}:commit
POST /v1/vault/rotations/{rotation}:cancel

GET  /v1/secrets
GET  /v1/secrets:status
POST /v1/secrets
GET  /v1/secrets/{secret_id}
POST /v1/secrets/{secret_id}:archive
POST /v1/secrets/{secret_id}:restore
POST /v1/secrets/{secret_id}:delete
POST /v1/secrets/{secret_id}/fields/{field_id}:access
```

Every route derives account, realm, and owner agent from the full bearer token;
there is no caller-supplied agent scope. Mutations require `Idempotency-Key`
and exact optimistic guards. `:access` returns one ciphertext/wrapped-DEK
package with `Cache-Control: no-store`, never plaintext. Enrollment transfer
data is recipient-bound, short-lived, and purged at a terminal transition.
Rotation stages replacement wrappers away from live `secret_deks`, exposes one
open run for crash recovery, and commits the fully verified plan atomically.
Sensitive secret create is rejected while a rotation is open. When an account
is suspended, value-free enrollment list/exact reads and rotation open/exact
reads remain available, as do the safety-only enrollment and rotation cancel
operations. Enrollment receive and rotation item pages can expose opaque sealed
material and remain active-only; all other lifecycle mutations also require an
active account. Account export,
irreversible account close, and deletion of the affected agent return conflict
while pending/approved enrollment or open rotation work exists; cancel first.

Offline recovery has no HTTP route. Export, inspect, and import operate on a
client-owned artifact; passphrases and AVKs never cross the API. Enrollment,
recovery, and rotation likewise have no MCP tools because their credentials and
ceremonies are intentionally confined to the CLI and controlling TTY.

## Decision

The public HTTP API should be REST-ish and resource-oriented under `/v1`.

Use plural resources for ordinary collection and item routes:

- `/v1/accounts`
- `/v1/realms`
- `/v1/agents`
- `/v1/memories`
- `/v1/memory-evidence`
- `/v1/memory-vector-profiles`
- `/v1/memory-vectors`
- `/v1/memory-curation-requests`
- `/v1/memory-curation-runs`
- `/v1/memory-curation-status`
- `/v1/facts`
- `/v1/secrets`
- `/v1/totp`
- `/v1/policies`
- `/v1/groups`
- `/v1/messages`
- `/v1/email` (account-policy-gated production inbound agent email)
- `/v1/message-requests`
- `/v1/transcripts`
- `/v1/usage`
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
POST /v1/memories/{memory_id}/supersede
POST /v1/memories/{memory_id}:forget
POST /v1/memories/{memory_id}:restore
POST /v1/memory-evidence/{evidence_id}/resolution
POST /v1/facts/{fact_id}:primary
POST /v1/secrets/{secret_id}:reveal
POST /v1/secrets/{secret_id}:rotate
POST /v1/secrets/{secret_id}:grant
POST /v1/totp/{secret_id}:code
POST /v1/policies:test
POST /v1/messages:listen
POST /v1/messages/{message_id}:reply
POST /v1/messages/{message_id}:read
POST /v1/messages/{message_id}:ack
POST /v1/messages/{message_id}:claim
POST /v1/messages/{message_id}:renew
POST /v1/messages/{message_id}:release
POST /v1/messages/{message_id}:complete
GET  /v1/email/address
GET  /v1/email:status
GET  /v1/email
POST /v1/email:listen
GET  /v1/email/checkpoint
POST /v1/email/{message_id}:read
POST /v1/email/{message_id}:code-consumed
POST /v1/email/{message_id}:ack
POST /v1/email/{message_id}:claim
POST /v1/email/{message_id}:renew
POST /v1/email/{message_id}:release
POST /v1/email/{message_id}:complete
GET  /v1/agents/{agent_id}/email-receive
PATCH /v1/agents/{agent_id}/email-receive
GET  /v1/realms/{realm_id}/email-receive
PATCH /v1/realms/{realm_id}/email-receive
POST /v1/email/retry-canary:arm
POST /v1/email/retry-canary:status

# Control-plane customer request surface; account-operator bearer token.
GET  /v1/accounts/{account_id}/billing
GET  /v1/accounts/{account_id}/billing/invoices
GET  /v1/accounts/{account_id}/billing/payments
POST /v1/accounts/{account_id}/billing:preview
POST /v1/accounts/{account_id}/billing:setup
POST /v1/accounts/{account_id}/billing:portal
GET  /v1/accounts/{account_id}/plan
POST /v1/accounts/{account_id}/plan:upgrade
POST /v1/accounts/{account_id}/plan:downgrade
POST /v1/accounts/{account_id}/plan:cancel
GET  /v1/accounts/{account_id}/realms/{realm_id}/email-alias-requests
POST /v1/accounts/{account_id}/realms/{realm_id}/email-alias-requests
GET  /v1/accounts/{account_id}/email-domain-requests
POST /v1/accounts/{account_id}/email-domain-requests
POST /v1/accounts/{account_id}/realms/{realm_id}:close

# Control-plane platform-administrator namespace surface.
GET  /v1/admin/realm-email-alias-requests
POST /v1/admin/realm-email-alias-requests/{request_id}:approve
POST /v1/admin/realm-email-alias-requests/{request_id}:reject
GET  /v1/admin/realm-email-aliases
POST /v1/admin/realm-email-aliases/{alias}:suspend
POST /v1/admin/realm-email-aliases/{alias}:reactivate
POST /v1/admin/realm-email-aliases/{alias}:retire
POST /v1/admin/realm-email-aliases/{alias}:abort-provisioning
POST /v1/admin/realm-email-aliases:assign-internal
GET  /v1/admin/realm-email-reserved-names
POST /v1/admin/realm-email-reserved-names
GET  /v1/admin/realm-email-reserved-names/{name}
PATCH /v1/admin/realm-email-reserved-names/{name}
DELETE /v1/admin/realm-email-reserved-names/{name}
GET  /v1/admin/realm-email-alias-audit
POST /v1/admin/realm-email-alias-counters:rebuild
POST /v1/admin/accounts/{account_id}/realms/{realm_id}:close
GET  /v1/admin/realm-email-alias-journal
POST /v1/admin/realm-email-alias-journal:bootstrap
POST /v1/admin/realm-email-alias-journal:checkpoint
POST /v1/admin/realm-email-alias-recoveries
GET  /v1/admin/realm-email-alias-recoveries/{recovery_id}
POST /v1/admin/realm-email-alias-recoveries/{recovery_id}:advance
POST /v1/admin/realm-email-alias-recoveries/{recovery_id}:verify
GET  /v1/admin/agent-email-domain-requests
GET  /v1/admin/agent-email-domain-requests/{request_id}
POST /v1/admin/agent-email-domain-requests/{request_id}:verify
POST /v1/admin/agent-email-domain-requests/{request_id}:reject
POST /v1/admin/agent-email-domain-requests/{request_id}:retire
GET  /v1/admin/agent-email-domain-audit
GET  /v1/admin/agent-email-domain-journal
POST /v1/admin/agent-email-domain-journal:bootstrap
POST /v1/admin/agent-email-domain-journal:checkpoint
POST /v1/admin/agent-email-domain-recoveries
GET  /v1/admin/agent-email-domain-recoveries/{recovery_id}
POST /v1/admin/agent-email-domain-recoveries/{recovery_id}:advance
POST /v1/admin/agent-email-domain-recoveries/{recovery_id}:verify

# Edge-to-control-plane authenticated fallback; not a customer route.
GET  /v1/email/realm-routes/{domain}/{realm_label}

# Control-plane-to-cell preflight; cell provision token, not a customer route.
GET  /v1/accounts/{account_id}:email-realm-alias-target?realm_id={realm_id}
GET  /v1/accounts/{account_id}:email-realm-route?realm_id={realm_id}
GET  /v1/accounts/{account_id}:email-realm-routes?limit={1..100}&cursor={opaque}
POST /v1/accounts/{account_id}:prepare-email-realm-route-retirement
POST /v1/accounts/{account_id}:commit-email-realm-route-retirement
GET  /v1/accounts/{account_id}:email-custom-domain-route?domain_request_id={request_id}&realm_alias_claim_id={claim_id}
POST /v1/accounts/{account_id}:email-custom-domain-route

GET  /v1/message-requests
POST /v1/message-requests
GET  /v1/message-requests/{request_id}
POST /v1/message-requests/{request_id}:offer
POST /v1/message-requests/{request_id}:decline
POST /v1/message-requests/{request_id}:select
POST /v1/message-requests/{request_id}:cancel
POST /v1/message-requests/{request_id}:claim
POST /v1/message-requests/{request_id}:renew
POST /v1/message-requests/{request_id}:release
POST /v1/message-requests/{request_id}:complete
POST /v1/tokens/{token_id}:rotate
```

All five realm-email-alias list surfaces use bounded opaque-cursor pagination.
The customer request route accepts `cursor`. Administrator request and
assignment routes accept `status`, `account_id`, `realm_id`, and `cursor`;
reserved-name list accepts `category`, `enabled=true|false`, and `cursor`; the
audit route accepts `action`, `limit=1..500`, and `cursor`. Each response
returns `truncated` plus `next_cursor` (a string when more storage rows remain,
otherwise `null`). Callers must reuse the same filters and follow every
non-null cursor, including after an empty filtered result. Filtering is bounded
to the current storage page; the server does not perform an unbounded hidden
scan to fill a page with matches.

Custom-domain customer/admin request lists and the custom-domain audit list
also use bounded opaque cursors. Their filters are `state`, `account_id`, and
`domain` for administrator requests and `action`, `account_id`, `domain`, and
`limit` for audit. The request registry is deliberately not a DNS/provider
mutation or mail-delivery control surface. The v0.0.238 request registry has no
routing side effect. The schema-88 dark route foundation is a
separate derived path behind the absent
`CP_AGENT_EMAIL_CUSTOM_DOMAIN_ROUTING_ENABLED` gate and exact
`CP_AGENT_EMAIL_CUSTOM_DOMAIN_ROUTING_ACCOUNT_ALLOWLIST`; it does not change the
request or verification contract.

Custom-domain journal and recovery administration requires both the ordinary
platform-admin bearer token and the distinct
`X-Witself-Agent-Email-Domain-Recovery` credential. Bootstrap and checkpoint
freeze authority writes and advance one bounded page per byte-equivalent
retry. Recovery creation requires an `aedrec_` id, an `aedj_` stream, and the
exact journal-head sequence/hash; replay to that head must cross a complete
checkpoint. It can target only the derived empty Durable Object
`recovery:<recovery_id>`. Advance and verify require the exact current
64-lowercase-hex action fence and an idempotency key. A verified target is
permanently sealed. These routes have no active-object selector, DNS mutation,
route publication, delivery action, or gate activation.

Sensitive/action routes must use `POST`, never `GET`.

The implemented supersede action is the existing slash subresource
`POST /v1/memories/{memory_id}/supersede`; clients must not translate it to a
colon route.

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
GET  /v1/auth/whoami         # compatibility alias for authenticated whoami
GET  /v1/capabilities

GET  /v1/self                # implemented JSON digest; target ?format= renderer is not implemented
POST /v1/remember            # deferred explicit Witself-only capture action

POST /v1/auth/sessions
GET  /v1/auth/sessions/{session_id}
POST /v1/auth/sessions/{session_id}:complete

POST /v1/bootstrap/operator

GET  /v1/operators
POST /v1/operators
DELETE /v1/operators/{operator_id}
POST /v1/operators/self/tokens
POST /v1/agents/{agent_id}/tokens
POST /v1/agents/{agent_id}/curator-tokens

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
GET  /v1/memories:status     # token-bound value-free active-memory capacity
POST /v1/memories
GET  /v1/memories/{memory_id}
GET  /v1/memories/{memory_id}/history
PATCH /v1/memories/{memory_id}
POST /v1/memories:recall
POST /v1/memories/{memory_id}/supersede
POST /v1/memories:consolidate              # superseded target; not implemented
POST /v1/memories/{memory_id}:forget
POST /v1/memories/{memory_id}:restore
POST /v1/memories/{memory_id}:reactivate
POST /v1/memory-evidence/{evidence_id}/resolution
DELETE /v1/memories/{memory_id}
POST /v1/memory-vector-profiles
GET  /v1/memory-vector-profiles
POST /v1/memory-vectors

# Client-side narrative-memory curation. These slash action subresources are
# the implemented workflow contract; every mutating route requires an
# Idempotency-Key header.
GET  /v1/memory-curation-preflight
POST /v1/memory-curation-requests
GET  /v1/memory-curation-requests # ?exclude_sensitive=true omits explicitly sensitive scopes
GET  /v1/memory-curation-requests/{request_id}
POST /v1/memory-curation-requests/{request_id}/start
GET  /v1/memory-curation-runs/{run_id}
GET  /v1/memory-curation-runs/{run_id}/inputs
POST /v1/memory-curation-runs/{run_id}/renew
POST /v1/memory-curation-runs/{run_id}/plan
GET  /v1/memory-curation-runs/{run_id}/plan # ?fencing_generation=N; verified accepted-plan review
POST /v1/memory-curation-runs/{run_id}/apply
POST /v1/memory-curation-runs/{run_id}/cancel
POST /v1/memory-curation-runs/{run_id}/abandon
POST /v1/memory-curation-runs/{run_id}/rollback
GET  /v1/memory-curation-status

The curation-request list also accepts `state`, `limit`, `cursor`, and boolean
`exclude_sensitive`. For a full agent credential, `exclude_sensitive=true`
omits scopes whose `include_sensitive` field is explicitly true; it does not
omit a transcript scope merely because transcripts are conservatively treated
as sensitive for restricted curator profiles. `curator-preview` and
`curator-apply` credentials always omit both explicit-sensitive and
transcript-bearing scopes, regardless of the query flag.

GET  /v1/agents/{agent_id}/memories
POST /v1/agents/{agent_id}/memories
GET  /v1/groups/{group_id}/memories
POST /v1/groups/{group_id}/memories

GET  /v1/facts               # ?all_agents=true is operator/admin-only (realm-wide scan)
GET  /v1/facts:status        # token-bound owner agent, value-free current-fact capacity
POST /v1/facts
GET  /v1/facts/{fact_id}
PATCH /v1/facts/{fact_id}
POST /v1/facts/{fact_id}:primary
DELETE /v1/facts/{fact_id}

GET  /v1/agents/{agent_id}/facts
POST /v1/agents/{agent_id}/facts
GET  /v1/groups/{group_id}/facts
POST /v1/groups/{group_id}/facts

# Implemented sealed plane: agent-owned, ciphertext-only API.
# See the authoritative route list above for key enrollment and rotation.
GET  /v1/secrets
GET  /v1/secrets:status
POST /v1/secrets
GET  /v1/secrets/{secret_id}
POST /v1/secrets/{secret_id}:archive
POST /v1/secrets/{secret_id}:restore
POST /v1/secrets/{secret_id}:delete
POST /v1/secrets/{secret_id}/fields/{field_id}:access

# Target-only sealed routes; not registered in schema 67.
PATCH  /v1/secrets/{secret_id}
DELETE /v1/secrets/{secret_id}  # irreversible purge, not tombstone delete
POST   /v1/secrets/{secret_id}:copy
POST   /v1/secrets/{secret_id}:grant
POST   /v1/secrets/{secret_id}:revoke
GET    /v1/agents/{agent_id}/secrets
POST   /v1/agents/{agent_id}/secrets
GET    /v1/groups/{group_id}/secrets
POST   /v1/groups/{group_id}/secrets
POST   /v1/totp/{secret_id}:enroll
GET    /v1/totp/{secret_id}
DELETE /v1/totp/{secret_id}

POST /v1/sessions:start      # target; not implemented
POST /v1/sessions:end        # target; not implemented

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
POST /v1/messages:listen        # metadata-only, oldest-unacked long poll
POST /v1/messages/{message_id}:reply
POST /v1/messages/{message_id}:read
POST /v1/messages/{message_id}:ack
POST /v1/messages/{message_id}:claim
POST /v1/messages/{message_id}:renew
POST /v1/messages/{message_id}:release
POST /v1/messages/{message_id}:complete

# Account-policy-gated, owner-agent-only inbound agent email.
GET  /v1/email/address
GET  /v1/email:status
GET  /v1/email
POST /v1/email:listen
GET  /v1/email/checkpoint
POST /v1/email/{message_id}:read
POST /v1/email/{message_id}:code-consumed
POST /v1/email/{message_id}:ack
POST /v1/email/{message_id}:claim
POST /v1/email/{message_id}:renew
POST /v1/email/{message_id}:release
POST /v1/email/{message_id}:complete

# Independently account-policy-gated outbound agent email.
POST /v1/email:send
POST /v1/email/{inbound_message_id}:reply
GET  /v1/email/sent
GET  /v1/email/sent/{send_id}

# Operator-only, value-free receive and send lifecycle controls.
GET   /v1/agents/{agent_id}/email-receive
PATCH /v1/agents/{agent_id}/email-receive
GET   /v1/realms/{realm_id}/email-receive
PATCH /v1/realms/{realm_id}/email-receive
GET   /v1/agents/{agent_id}/email-send
PATCH /v1/agents/{agent_id}/email-send
GET   /v1/realms/{realm_id}/email-send
PATCH /v1/realms/{realm_id}/email-send

# Exact configured canary agent only; opaque challenge is POST-body-only.
POST /v1/email/retry-canary:arm
POST /v1/email/retry-canary:status

# Cell-local signed relay endpoint; no agent/operator bearer token.
POST /v1/internal/agent-email:ingest

# Bearer-protected, content-free provider lifecycle callback.
POST /v1/internal/agent-email-send:provider-event

# Control-plane-to-cell system projection; provision-token authenticated.
POST /v1/accounts/{account_id}:email-realm-alias
GET  /v1/accounts/{account_id}:email-realm-alias?claim_id={claim_id}
GET  /v1/accounts/{account_id}:email-realm-aliases

GET  /v1/message-requests
POST /v1/message-requests
GET  /v1/message-requests/{request_id}
POST /v1/message-requests/{request_id}:offer
POST /v1/message-requests/{request_id}:decline
POST /v1/message-requests/{request_id}:select
POST /v1/message-requests/{request_id}:cancel
POST /v1/message-requests/{request_id}:claim
POST /v1/message-requests/{request_id}:renew
POST /v1/message-requests/{request_id}:release
POST /v1/message-requests/{request_id}:complete

# Append-only visible conversation ledger. Agent tokens write their own;
# account operators may read every transcript in their account.
GET  /v1/transcripts
POST /v1/transcripts
GET  /v1/transcripts/{transcript_id}
POST /v1/transcripts/{transcript_id}/entries
POST /v1/transcripts/{transcript_id}/entries:batch

# Token-derived product usage. V0 is deliberately agent-self only.
GET  /v1/usage

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

# Target account-token billing surface; the implemented control-plane routes
# are account-scoped and listed above.
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

`GET /v1/transcripts/{transcript_id}` accepts either forward paging with
`after_sequence` and `limit` or a bounded newest-page read with `tail=true` and
`limit`. Results remain ordered oldest-first and return `next_after_sequence`
when another forward page exists. `limit` defaults to 100 and is capped at 500.

The batch append accepts 1-100 ordered entries. Transcript creation and entry
append are retry-safe when the caller supplies external ids. Reusing an entry
external id with different content is a conflict; `reply_to_external_id` may
refer to an earlier entry in the same transcript, including an earlier entry in
the same batch.

`GET /v1/usage` accepts only an active agent token; account operators cannot
expand it into another agent's view. `since` and `until` are RFC3339, repeated
`dimension` parameters filter dimensions, and `group_by` is `hour` or `day`
(default `day`). The default window is 30 days. Hourly windows are capped at 90
days and daily windows at five years. Results contain time-bucket points and
whole-window totals, each with `quantity`, `unit`, and source `event_count`.

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

The action routes carry Witself's integrity-sensitive verbs. They are
`POST`-only. Implemented memory mutations use idempotency keys and metadata-only
audit events; read-only recall does neither:

- `GET /v1/facts:status` is a read-only status exception to the action-route
  convention. It returns the authenticated owner agent's value-free
  `fact_capacity` projection. `used` counts resolved, non-deleted current facts
  across all subjects; assertions, candidates, aliases, history, evidence, and
  tombstones do not consume another slot. For a finite maximum, `near_limit`
  starts at 90 percent. The route returns no ids, subjects, predicates, values,
  or history.
- `GET /v1/memories:status` returns the authenticated owner agent's value-free
  `memory_capacity` projection: `used`, nullable `max` and `remaining`,
  `unlimited`, `near_limit`, `at_limit`, and `over_limit`. For a finite maximum,
  `near_limit` begins at 90 percent and remains true at or beyond the cap.
  `used == max` sets `at_limit`, is not over-limit, and refuses another
  net-growing write.
  This read returns no memory ids, content, evidence, or plan data.
- `POST /v1/memories:recall` is implemented for the token-bound agent's active
  current heads. The body accepts literal query text plus kind, tags, links,
  origin, capture reason, occurrence/capture ranges, sensitivity, limit, and an
  opaque filter-bound cursor. PostgreSQL full-text, salience, and recency produce
  explicit score components and stable ordering; the backend makes no model or
  embedding call. Supplying both `vector_profile_id` and a compatible
  `query_vector` enables deterministic hybrid scoring over the bounded candidate
  universe. Responses expose similarity, per-hit vector use, profile, coverage,
  candidate counts/limit, truncation, retrieval mode, and degradation reason.
  With no profile, or zero compatible rows, lexical recall remains the baseline.
  Cross-agent/group recall remains future work.
- `POST /v1/memory-vector-profiles` creates or exactly replays one immutable
  agent-owned profile declaring provider/model/recipe identity, dimensions,
  distance metric, and normalization. `GET /v1/memory-vector-profiles` returns
  the caller's bounded profile set. These identifiers describe a client recipe;
  they are not backend provider configuration or credentials.
- `POST /v1/memory-vectors` stores or exactly replays one finite vector bound to
  a profile, exact memory id/version, and content hash. The response is a
  value-free receipt and never returns vector components. Migration `0032`
  stores vectors as portable JSONB; no pgvector extension is required.
- `POST /v1/memories/{memory_id}/supersede` atomically supersedes one exact
  active version with a nonempty caller-authored replacement set. It requires
  an operation `Idempotency-Key` header, positive `expected_version`, and one
  body `idempotency_key` plus exact, pending, or explicitly unavailable evidence
  for every replacement. HTTP 201 returns the full authorized source and
  replacements plus a value-free receipt containing the supersession set,
  exact version references, replacement count and SHA-256 membership digest,
  actor, request hash, and retry key. The current HTTP, Go client, CLI, and MCP
  surfaces are agent-self only.
- Capture, restore, reactivate, supersede, and curation apply share the
  per-agent `stored_memory` gate. The server evaluates their net active-memory
  effect under the owner-agent concurrency fence after resolving exact
  idempotent replay. A positive delta that would exceed a finite maximum returns
  HTTP 403 with `code: "stored_memory_limit_reached"`, `retryable: false`, and a
  value-free `limit` object. Reads and zero- or negative-delta correction and
  consolidation remain available at or above the maximum.
- Current-memory and history outputs preserve the immutable source-version
  receipt fields (`supersession_set_id`, `supersession_set_revision`,
  `supersession_replacement_count`, `supersession_replacement_digest`) and
  separately project the currently unreverted relation set as
  `active_supersession_set_id` and `active_supersession_set_revision`.
  Reactivation clears only the active projection; it does not rewrite the
  historical receipt. These fields are value-free and survive broad-response
  redaction.
- `content_encoding` is `plain` by default and may be `base64` for canonical
  binary-safe content. Capture and supersede replacements use that field;
  adjust uses `set_content_encoding`; current and historical outputs include
  the effective value. JSON body ceilings account for worst-case escaping of
  store-legal inputs: 8 MiB for capture, 16 MiB for adjust, and 257 MiB for a
  32-replacement supersede. Exceeding a ceiling returns HTTP 413.
- `POST /v1/memories:consolidate` is not implemented and must not make semantic
  decisions in the backend. Deeper merge/split work uses an exact,
  caller-authored plan submitted through the implemented curation run; direct
  one-to-many supersede already uses the exact caller-authored route above. See
  [narrative-memory-and-curation.md](narrative-memory-and-curation.md).
- The 15 curation endpoints, including preflight, are deliberately
  resource/action slash routes because
  they operate on durable queue requests and fenced run resources. Request
  creation coalesces equivalent open work. Request listing uses stable,
  filter-bound cursor pagination; an empty state filter lists claimable due
  work, while an explicit state lists that lifecycle state. `start` claims one
  due request and freezes bounded, authorized memory/evidence/transcript/cursor
  inputs under one lease and fencing generation. `GET .../inputs` requires that
  fence and pages the immutable snapshot. `renew`, `plan`, `apply`, `cancel`,
  and `abandon` also require the fence; `plan` accepts strict
  `witself.memory-plan.v1`, and `apply` additionally binds its accepted revision
  and lowercase SHA-256 hash. `rollback` instead binds the apply receipt and the
  complete expected produced-head set. It refuses downstream consumers, never
  cascades, never rewinds source cursors, and queues a read-only replay. All
  responses are `private, no-store`; the input content is untrusted data. The
  backend provides concurrency, validation, persistence, and compensation only
  and never launches or calls an AI model.
- `POST /v1/agents/{agent_id}/curator-tokens` is operator-authorized and returns
  one short-lived agent credential once. Its required immutable
  `access_profile` is `curator-preview` or `curator-apply`, its audit display
  name is mandatory, and its TTL must be greater than zero and no more than
  24 hours. Existing and ordinary tokens have profile `full`. Curator profiles
  fail closed on every ordinary domain route. Preview may list/get/start/page/
  renew/plan/get-plan/abandon/status curation; apply adds only apply. Neither profile may
  create/cancel/rollback work, include sensitive inputs, write facts/messages/
  direct memories, or permanently delete anything. The credential response is
  `private, no-store`, and normal token revocation applies.
- `GET /v1/memory-curation-preflight` is authenticated and reports the effective
  principal, token id/profile/expiry, exact allowed operations, plan schema,
  inference boundary, server limits, and the same value-free
  `memory_capacity` projection for the presented credential. Clients must use
  it instead of treating deployment-wide `/v1/capabilities` as an authorization
  decision. Plan acceptance reports count-only `active_memory_delta` and
  `projected_active_memories`; apply recomputes the projection from locked live
  heads and refuses a
  net-growing over-cap plan atomically.
- `POST /v1/memories/{memory_id}:forget` appends a reversible `forgotten`
  version. `DELETE /v1/memories/{memory_id}` is the guarded physical purge.
- `POST /v1/memories/{memory_id}:restore` appends an active version from a valid
  forgotten state.
- `POST /v1/memory-evidence/{evidence_id}/resolution` appends one terminal
  resolution to a pending evidence row. The body selects exactly one exact
  transcript range, source memory/version, realm message, import-artifact
  locator, or explicit unresolvable reason; `Idempotency-Key` is required. The
  pending row is immutable.
- `DELETE /v1/memories/{memory_id}?dry_run=true` is the implemented value-free
  permanent-deletion preview. It accepts the memory id only: mutation guards,
  authorization assertion, and idempotency key are rejected, and no preview
  resource/id/expiry is created. Apply omits `dry_run`, supplies
  `expected_version` and `scrub_set_revision` query parameters, and requires
  both `Idempotency-Key` and `X-Witself-Direct-User-Authorized: true`. The latter
  is valid only for this turn's direct current-user request for that exact
  memory. `reason_code` is server-owned (`direct_user_request`) and a supplied
  query value is rejected. Apply conflicts on stale guards or live incoming
  dependencies and returns an exact replay for the same apply key and guards.
- `POST /v1/facts/{fact_id}:primary` is the atomic primary promotion. It demotes
  any prior primary of the same logical kind for the same owner.
- `DELETE /v1/facts?dry_run=true&subject={subject}&predicate={predicate}`
  resolves an exact canonical address and returns a value-free permanent-
  deletion preview without recording retrieval usage. A fact-id preview is
  also available at `DELETE /v1/facts/{fact_id}?dry_run=true`. Apply uses the
  fact-id route without `dry_run`, requires
  `Idempotency-Key`, `expected_resolved_assertion_id`, and the preview's
  `expected_candidate_revision`, and returns HTTP 409 when either the resolved
  assertion or address-matching candidate set changed after preview. It permanently removes
  assertions and address-matching candidates, retains a value-free fact
  tombstone plus immutable usage events, and returns HTTP 410 for a deleted
  target that is not an idempotent replay. Ordinary fact reads exclude the
  tombstone. A new fact at that address requires explicit recreation.
- `POST /v1/policies:test` evaluates whether a given subject, permission,
  target, and scope would be allowed under current policy, returning the
  deciding policy id or a deny reason. It is the canonical dry-run for access
  decisions and does not mutate state.
- `POST /v1/messages:listen` performs a stateless metadata-only long poll for
  the caller's oldest unacknowledged inbound messages. The body accepts
  `wait_seconds` (0–20, default 20), `from_agent`, `thread_id`, `kind`, and
  `limit`; the response contains `messages` and `timed_out`. It has no cursor
  and changes no read/ack state. Each server process bounds concurrent listen
  admission; saturation returns HTTP 429 with `Retry-After` so callers can retry
  without losing durable mailbox state. On list and listen, a lowercase
  `agent_` `from_agent` selector matches only that exact sender ID and never a
  same-text name; ordinary selectors use exact ID-or-name matching with ID
  precedence.
- `POST /v1/messages` normalizes an omitted `kind` to actionable `request`.
  Clients use explicit `kind=note` for FYI-only delivery with no implied reply
  or provider-inference requirement; an active client acknowledges it only after
  handling it. `to.kind` is `agent`, `agents`, or `realm`. Direct input uses one
  `id`; explicit-list input uses 1-64 `ids`;
  realm input supplies neither. Every selector resolves exactly and
  case-sensitively inside the token-derived realm, the whole operation is
  all-or-none, and a realm snapshot excludes the sender. A selector beginning
  with lowercase `agent_` is an ID-only reference and never falls back to an
  agent name; other selectors use exact ID-or-name resolution with ID
  precedence.
- `POST /v1/messages/{message_id}:reply` is recipient-only. It verifies that the
  caller received the parent, then derives the recipient from the parent sender
  and derives the thread, `reply_to_message_id`, and `causal_depth` (parent plus
  one). Caller-supplied routing, identity, or depth fields are rejected.
- `POST /v1/messages/{message_id}:read` returns content and records only the
  recipient read transition; `POST /v1/messages/{message_id}:ack` separately
  records per-recipient acknowledgement and returns metadata only, never the
  message body or payload.
- `POST /v1/messages/{message_id}:claim` acquires or idempotently replays a
  30–900 second direct-delivery processing lease for the token-bound recipient.
  Taking an available or expired delivery advances a monotonic generation;
  claiming does not read or acknowledge. Generation is solely the stale-writer
  fence; it is not a failure-attempt counter.
- `POST /v1/messages/{message_id}:renew` and `:release` require the exact live
  `claim_id` and generation. Renewal replaces the database-time expiry; release
  makes processing available and invalidates the old fence without acking.
  Release accepts optional `deterministic_failure` (default false); only true on
  an exact-fence release atomically increments migration-0036 `failure_count`.
  Installed foreground policy directs a client not to mark provider-wide,
  configuration, cancellation, timeout, or lease-maintenance failures
  deterministic and to complete the fifth deterministic attempt as a durable
  escalation. The backend stores the count but cannot force model compliance.
- `POST /v1/messages/{message_id}:complete` validates the exact unexpired fence
  and required `Idempotency-Key`, then in one transaction creates a
  server-routed result reply at parent `causal_depth + 1`, links it to the
  delivery, and marks processing completed. It returns HTTP 201 with
  `processing` and `message`; it does not ack. Active conflicts and stale fences
  return HTTP 409.
  The message sender is always derived server-side from the token, never from
  the request body; sender forgery is structurally impossible.
- Inbound and outbound `/v1/email` routes are independently registered and
  gated. Inbound owner routes require a valid process-lifetime receive mode;
  production receive uses an exact account cohort, while the retired
  compatibility mode remains limited to one realm plus 5–10 agents. Every
  inbound owner route rechecks that scope and requires a full agent token;
  operator, non-full credential-profile, and unenrolled-agent access is denied.
  `GET /v1/email/address` returns the caller's one provisioned address. Startup
  reconciliation provisions exactly the configured agents and fails closed on
  a missing agent, wrong realm, name collision, or ownership mismatch.
- Outbound owner routes are separate from process-local receive configuration.
  `POST /v1/email:send`, `POST /v1/email/{inbound_message_id}:reply`, and the
  metadata-only sent list/show routes require effective `agent_email_send` plus
  the current agent and realm send controls. A disabled direction returns the
  stable, non-retryable `feature_not_enabled` refusal before an outbox row or
  rate debit is created. Plan and account-policy changes take effect from the
  resolved snapshot without reinstalling a client or restarting the server.
  Send and reply accept one recipient and plain UTF-8 text only; sender,
  Reply-To, and reply provenance are server-derived. Admission always retains
  platform-only account-minute, account-day, and normalized-recipient-day
  breakers in addition to the agent and realm minute limits.
- `GET /v1/email` is metadata-only and cursor-paginated (`unread`, `unacked`,
  `limit` 1–100, `cursor`). `POST /v1/email:listen` is a stateless metadata-only
  long poll (`wait_seconds` 0–20, default 20; `limit` 1–100) over oldest
  unacknowledged mail. Neither operation marks read/acknowledged, exposes body
  text, raw MIME, attachment names/media types/content, or returns an active
  claim capability. The metadata projection includes attachment count,
  attachment-storage byte counts, and payload-retention state. Listen admission
  is bounded per process and per agent.
- `GET /v1/email:status` is value-free and reports the effective per-message
  raw-MIME maximum plus account-wide attachment-storage capacity (`used`,
  nullable `max`/`remaining`, `unlimited`, `near_limit`, `at_limit`, and
  `over_limit`). It never exposes message, sender, or attachment content.
- `POST /v1/email/{message_id}:read` marks read and returns bounded decoded text
  with the sender explicitly unverified. Raw MIME, HTML markup, attachment
  names/media types/bytes, trusted auth results, and provider identifiers are
  unavailable. `:code-consumed` records only a one-time timestamp after a
  client successfully uses a low-risk expected code; it never stores or returns
  the code. `:ack` remains a separate metadata-only durable handling marker.
- `:claim`, `:renew`, `:release`, and `:complete` mirror the ordinary mailbox's
  30–900 second exact-fence lifecycle. Claim and complete require an
  `Idempotency-Key`; renew/release/complete require the live claim id and
  generation. Email completion creates no reply/result artifact and does not
  acknowledge the message.
- `GET|PATCH /v1/agents/{agent_id}/email-receive` and
  `GET|PATCH /v1/realms/{realm_id}/email-receive` are operator-only,
  path-bound, value-free lifecycle controls; they never expose an address or
  message metadata. Effective receive is enabled only when both independent
  layers are enabled. A suspended account may inspect or disable either layer,
  but cannot re-enable one until resume. Pending and closed accounts are
  denied.
- The retry-canary arm/status routes exist only when one exact enrolled canary
  agent is configured and accept only that agent's full token. They return
  cumulative value-free state and never echo the opaque challenge.
- `GET /v1/email/checkpoint` and the enrolled caller's `email_checkpoint` in
  `GET /v1/self` are value-free pending-mail hints. They contain no address,
  message id, sender, subject, body, attachment, or processing fence.
- `POST /v1/internal/agent-email:ingest` accepts only the byte-identical raw body
  with the receive service's Ed25519 relay headers. It verifies key id, signature, body
  digest/size, audience, and a bounded timestamp window before calling the
  scoped store. The endpoint is capped at the 25 MiB transport ceiling; the
  owning cell enforces any lower resolved account limit and returns the exact
  content-free `over_size` verdict. Successful `accepted` is emitted only after
  the owning cell commit. It is not a public bearer-token route. Schema 88 adds
  no route-provenance relay header: the route-projection signature is verified
  at the edge and is not forwarded. The cell derives canonical, managed-alias,
  or custom-domain receipt provenance from the existing signed envelope
  recipient and its local route rows.
- `POST /v1/internal/agent-email-send:provider-event` is the independent,
  content-free lifecycle callback used by the sending adapter's Queue consumer.
  It requires the dedicated provider-event bearer, derives the target account
  and cell from the adapter's bounded route map, validates the exact event and
  dispatch provenance, and folds one closed `delivered`, `deferred`, `bounced`,
  `failed`, `rejected`, or `complained` outcome idempotently into the durable
  outbox. It accepts no sender, recipient,
  subject, body, or arbitrary target selector. Cloudflare retries every non-204
  response and moves exhausted work to the configured DLQ; the cell never asks
  the adapter to resend a logical message.
- `GET|PATCH /v1/agents/{agent_id}/email-send` and
  `GET|PATCH /v1/realms/{realm_id}/email-send` are operator-only, path-bound,
  value-free kill switches. Effective send requires an active account, effective
  account send entitlement, a live agent, and both layers enabled. A suspended
  account may inspect or disable a layer but cannot enable one.
- Canonical Realm-ID routes have an independent bounded inventory. The
  control-plane schedule does nothing unless
  `CP_REALM_EMAIL_CANONICAL_INVENTORY_ENABLED` is exactly `true`. Its controller
  writes applied destinations only when
  `CP_REALM_EMAIL_CANONICAL_DELIVERY_ENABLED` is also exactly `true`; otherwise
  it can still converge retry-suspended and retired authority. The Email Worker
  independently requires `REALM_EMAIL_CANONICAL_DELIVERY_ENABLED=true` before
  relaying a canonical route. All three gates are default-off.
- `POST /v1/accounts/{account_id}/realms/{realm_id}:close` authenticates the
  account operator against the current cell. Its platform-admin counterpart is
  `POST /v1/admin/accounts/{account_id}/realms/{realm_id}:close`. Both accept an
  `idempotency_key` and drive the same durable operation. The controller rejects
  any live or pending realm alias or non-retired custom-domain route, prepares
  the cell's exact route generation, publishes a retired canonical route, and
  commits the cell tombstone. It
  returns 202 with the current phase while converging and 200 when complete;
  retry the same request and idempotency key. Direct deletion on a managed cell
  fails closed so it cannot bypass this ordering.
- The provision-token cell routes expose only portable, value-free lifecycle
  state. `GET ...:email-realm-route` reads one realm; bounded
  `GET ...:email-realm-routes` includes live, closing, and retired rows.
  Prepare requires the live generation and changes it once to `closing`;
  commit requires that prepared generation and atomically records `retired`
  with the realm soft-delete. Exact replays are idempotent. A stale generation,
  different operation id, nonempty realm, or applied alias returns conflict.
- Realm alias claims are globally authoritative in one control-plane Durable
  Object, while account, realm, and skeleton mutation lanes are independently
  serialized so unrelated work does not share a global head-of-line lock.
  Customer creation rechecks the account operator against its live cell
  and requires both `agent_email_realm_alias` and a positive or explicitly
  unlimited `agent_email_realm_aliases_per_realm` limit. Platform-admin
  approval rechecks the current plan, applies and reads back the exact cell projection, then
  publishes isolated edge-directory rows before exposing the assignment as
  active. Every mutation is idempotent and audited.
- Realm alias request lists also report `pending_counter_state`, the configured
  `technical_pending_limits`, and realm/account `pending_capacity` when the
  request is scoped to one realm. The technical maxima are eight open requests
  per realm and 64 per account, independent of plan capacity. Open means both
  `pending_review` and `provisioning`; a failed projection therefore keeps its
  slot. Counter rebuild is paginated and customer creation returns 503 until it
  is ready or whenever durable counter state is corrupt. A technical-ceiling
  refusal is a stable 409 with
  `code=technical_pending_limit_reached`, `scope=realm|account`, and the
  effective numeric `limit`.
- An authenticated platform administrator can recover operator-detected
  derived-counter corruption with
  `POST /v1/admin/realm-email-alias-counters:rebuild`. The bounded JSON body
  requires `reason` and `idempotency_key`. The request durably audits one
  recovery intent, fences all count-changing writes, clears only the derived
  usage keyspaces in bounded pages, rebuilds them from canonical claims, and
  performs a second bounded verification pass before reporting `ready`.
  The accepted response is returned only after the recovery alarm is armed. If
  that initial alarm write fails, the durable fence and idempotency record stay
  intact but the request returns retryable 503; replaying the same key attempts
  to re-arm and then returns the original 202 without another audit event. A
  different rebuild while one is active returns 409. A re-arm failure during
  an alarm turn propagates so the platform retries that turn. This maintenance
  route remains available while realm-alias activation is off.
- Portable alias-authority maintenance uses a second administrative boundary.
  Every journal and recovery route requires both the normal platform-admin
  bearer token and `X-Witself-Realm-Alias-Recovery` matching the distinct
  `CP_REALM_EMAIL_ALIAS_RECOVERY_TOKEN`. Bootstrap/checkpoint requests require
  a bounded reason and idempotency key, freeze authority writes, and advance one
  bounded scan step per replayed call. Recovery creation additionally requires
  a caller-minted `rear_` id, source `reaj_` stream, and exact expected
  sequence/hash. It can target only the empty named object
  `recovery:<recovery_id>`. `:advance` replays one journal entry per call;
  repeated `:verify` calls rebuild derived state in bounded pages and finally
  seal the verified target. Recovery status exposes an opaque 64-lowercase-hex
  `action_fence`; every action body requires that value as
  `expected_action_fence` with an idempotency key. Persisted actions rotate the
  fence. Calls must be serial, and only a byte-equivalent retry of the immediate
  last action replays after a lost acknowledgement. Stale fences return 409
  without mutation; an idempotency-key label is fence-scoped rather than
  globally reserved forever. Legacy v1 targets remain status-readable with a
  null fence but refuse actions. These routes never select the active object or
  perform cutover.
- The managed-alias cell projection endpoint accepts only the account cell's
  provision token and an exact `era_` claim fence, domain, label, state, and
  monotonically increasing controller revision. Equal replay is idempotent;
  stale or conflicting projections fail closed.
- The schema-88 provision-token-only
  `POST /v1/accounts/{account_id}:email-custom-domain-route` applies one exact
  join of verified domain request/allocation, realm-alias claim, realm, and
  account. Its `GET` form requires both `domain_request_id` and
  `realm_alias_claim_id` and provides the controller's exact readback fence.
  The cell stores domain-allocation, domain-state, alias, and controller
  revisions; equal-revision exact replay is idempotent, while lower revisions,
  a same-revision mismatch, a wrong binding, or retired-row resurrection fail
  closed. An applied row additionally requires the local alias at the advertised
  revision and a live realm-email route. Received messages preserve
  `canonical`, `realm_alias`, or `custom_domain` provenance; the custom variant
  stores both source ids.
- The cell's provision-token
  `GET /v1/accounts/{account_id}:email-realm-alias-target?realm_id={realm_id}`
  preflight proves only that the exact account owns that live (not soft-deleted)
  realm. Its response is content-minimal and exposes no realm name or account
  metadata.
- `GET /v1/email/realm-routes/{domain}/{realm_label}` requires the dedicated
  edge token. The route-kind `canonical | realm_alias | custom_domain` union is
  a strict unsigned schema-v1 scalar inner route, but neither this response nor
  the dynamic-route KV value exposes that inner value directly. The control
  plane signs the exact projection and returns or publishes a flat schema-v2
  object containing the same union fields plus `route_signing_key_id` and
  `route_signature`. A
  missing or unusable signer, invalid key configuration, import failure, or
  signature failure returns retryable HTTP 503 with code
  `agent_email_route_signing_unavailable`; it never emits or publishes an
  unsigned fallback. Managed-domain cache misses keep the existing bounded
  durable refresh behavior; their response path does no cell or KV repair I/O.
  A non-managed domain returns 404 without touching custom authority unless
  `CP_AGENT_EMAIL_CUSTOM_DOMAIN_ROUTING_ENABLED=true`. The custom registry then
  independently requires the owning account in
  `CP_AGENT_EMAIL_CUSTOM_DOMAIN_ROUTING_ACCOUNT_ALLOWLIST`, stages a durable
  derived intent, applies and reads back the exact cell projection, re-proves
  both authorities, and publishes the same KV key only after all fences match.
  It never writes the general account/cell directory namespace.
- The Email Worker accepts a schema-v2 route only after verifying its Ed25519
  signature with the configured key id, reconstructing and strictly validating
  the schema-v1 union, and matching the requested domain, label, and expected
  route-kind variant. It performs those checks before trusting
  `cell_audience` or `ingest_url` and before reading `message.raw`. Unsigned v1,
  malformed v2, an unknown signing key, or a mutation of any signed scalar is
  not route authority: bad KV evidence triggers the authenticated bounded
  control-plane fallback, while a bad control-plane response or failed fallback
  is a temporary SMTP failure.
- `CP_REALM_EMAIL_ALIAS_ACTIVATION_ENABLED` is an exact-`true`, default-off
  operational gate. While disabled, create, approve, internal assignment, and
  reactivation return conflict; listing, audit, reservation management,
  suspension, retirement, and terminal customer/internal-provisioning abort remain
  usable. The gate must stay off until the
  managed domain's catch-all or equivalent full-coverage route is verified.
- `REALM_EMAIL_ALIAS_DELIVERY_ENABLED` is a second exact-`true`, default-off
  Email Worker gate checked on every `realm_alias` delivery. Any other value
  tempfails the alias before content read or cell relay. Canonical Realm ID and
  legacy literal-pilot routes are unaffected.
- `AGENT_EMAIL_CUSTOM_DOMAIN_DELIVERY_ENABLED` is an independent exact-`true`,
  default-off Email Worker gate for any domain outside the configured managed
  primary/legacy set. Any other value tempfails before KV, control-plane
  fallback, either lookup limiter, or raw-MIME access. It does not affect
  managed canonical, alias, or legacy routes. The edge projection uses
  `route_kind=custom_domain` and the existing
  `email:realm-route:v1:<domain>:<realm_label>` key, with exact domain
  request/allocation and alias claim/revision fences. This gate and both
  control-plane routing controls are absent; this slice authorizes only fake,
  offline acceptance and performs no provider, DNS, MX, Email Routing, or live
  delivery change.
- `POST /v1/message-requests` requires an agent token and `Idempotency-Key` and
  creates one realm `kind=open_request` message plus an immutable candidate
  snapshot in the same transaction. `selection_policy` is omitted or
  `client_ranked`; `max_assignees` is 1-8 (default 1),
  `offer_window_seconds` is 1-900 (default 30), and `expires_in_seconds` must be
  greater than the offer window and at most 604800 (default 3600). Sender,
  realm, coordinator, thread, parent, and causal depth are derived.
- `GET /v1/message-requests` returns metadata visible to the coordinator or one
  immutable candidate, with optional `state`, `phase`, `role`, `limit`, and
  `cursor`. `GET /v1/message-requests/{request_id}` returns authorized detail:
  the coordinator sees the candidate/offer/selection/claim graph, while a
  candidate sees only its own response, offer, selected ID, selections, and
  claims; co-selected agent IDs are not exposed. Request and offer content is
  untrusted input.
- `:offer` and `:decline` are candidate-only during the bounded offer window.
  An offer atomically creates one ordinary direct `kind=offer` reply; it reserves
  no capacity. Each candidate has one idempotent response. `:select` is
  coordinator-only, accepts 1-8 offered agent IDs plus a 30-900 second
  reservation, and atomically enforces `max_assignees`. Selection is allowed
  only after the offer deadline or once no candidate remains pending; the
  backend validates but never ranks or chooses candidates. An offline
  coordinator leaves the durable request in `awaiting_selection` rather than
  falling back to first claimant. Deleting that coordinator system-cancels its
  open requests and live claims.
- `:claim` converts the selected agent's current live reservation into a claim
  and returns an opaque `mrc_` claim ID plus generation. `:renew`, `:release`,
  and `:complete` require that exact live fence. Completion atomically creates a
  server-routed direct `kind=result` reply, links it, and closes the work slot.
  `max_assignees` is a ceiling: selecting fewer is valid, and the request closes
  after a completion once that selected batch has no other live reservation or
  claim, even when the ceiling was larger.
  Cancellation is coordinator-only and invalidates every live reservation and
  claim. Deleting a candidate declines a pending response and cancels that
  agent's live claims while preserving historical offers. Stored deadlines make
  expiry and phase recoverable without a backend inference or scheduling worker.
- `POST /v1/tokens/{token_id}:rotate` issues a replacement token. The raw token
  value is returned once.
- `POST /v1/tokens/{token_id}:revoke` immediately invalidates a live operator
  or agent token by token ID. It never requires or returns the raw token value.
- `POST /v1/secrets/{secret_id}/fields/{field_id}:access` is the implemented
  explicit material-delivery route. It returns exactly one client-decryptable
  ciphertext and wrapped-DEK package with `Cache-Control: no-store`; local CLI
  or MCP code performs the reveal. There is no server-side decrypt or plaintext
  response shape.
- `GET /v1/secrets:status` returns the authenticated owner agent's value-free
  retained capacity: `used`, nullable `max`, nullable `remaining`, `unlimited`,
  and `over_limit`. Active and archived bundles count; tombstones do not.
  Missing `stored_secret` means unlimited and zero is a real cap.
- `POST /v1/secrets` returns HTTP 403 with
  `code: "stored_secret_limit_reached"`, `retryable: false`, and a `limit`
  object when an ordinary create would exceed the owner-agent cap. Exact
  idempotent replay is resolved before the gate. Same-owner create/delete
  capacity changes serialize across replicas.
- `POST /v1/secrets/{secret_id}:archive` is the reversible soft-retire path and
  `POST /v1/secrets/{secret_id}:restore` reverses it within the retention
  window.
- `POST /v1/secrets/{secret_id}:delete` accepts
  `{"expected_row_version": N}` plus `Idempotency-Key` and performs a guarded
  tombstone delete of an active or archived secret. It increments the row
  version, returns a redacted tombstone and value-free receipt, and releases
  retained capacity. The same transaction scrubs identifying/public secret
  metadata and deletes all field and wrapped-DEK rows. Ordinary get/list/access
  routes exclude the remaining minimal value-free tombstone. Permanent
  `DELETE` purge of that tombstone, `PATCH` update, copy, grants/group
  ownership, and server-side TOTP routes are target-only and are not registered
  in schema 67.
- `/v1/vault/enrollments` implements the five-state, short-lived transfer
  lifecycle. `:receive` is an opaque target read; `:consume` proves durable
  local receipt before terminal capsule purge. Collection/exact lifecycle reads
  remain available while suspended, but `:receive` and non-cancel mutations are
  active-only; `:cancel` is the suspended-account safety path.
- `/v1/vault/rotations` implements an `open|committed|cancelled` wrapper
  lifecycle. `GET .../open` is the crash-resume discovery route, item pages are
  deterministic, and `:commit` atomically flips a fully staged plan. Commit
  requires a value-free `recovery_disposition`: either
  `{"mode":"recovery_artifact","artifact_sha256":"<64 lowercase hex>"}` or
  `{"mode":"risk_accepted"}`. The disposition participates in the request
  hash and retry fence and remains visible on the committed rotation and its
  audit event; artifacts, paths, passphrases, and key material never cross the
  API boundary. Open/exact lifecycle reads remain available while suspended;
  item pages and every mutation except `:cancel` remain active-only.
- Password generation and TOTP calculation are implemented locally. There is
  no `/v1/password:generate` or `/v1/totp/{secret_id}:code` server route.

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

## Cross-Realm Collaboration Routes

These routes back the post-v0 cross-realm collaboration substrate; see
[agent-collaboration.md](agent-collaboration.md). They reuse the existing
`/v1/messages` resource and its implemented realm-local long-poll receive verb, then
add a conversation/task resource, the realm federation allow-list, and the
signed realm card. `:listen` belongs to the same-realm mailbox first; cross-realm
delivery reuses it later.

```text
POST /v1/messages:listen        # implemented realm-local metadata-only receive

GET  /v1/conversations
GET  /v1/conversations/{conversation_id}

GET    /v1/federation/peers
POST   /v1/federation/peers
DELETE /v1/federation/peers/{peer}

GET  /.well-known/witself-card.json
```

- `POST /v1/messages:listen` is the implemented realm-local long-poll receive
  verb: it blocks for 20 seconds by default, bounded to 0–20, and returns the
  oldest unacknowledged inbound **metadata** from the caller's durable mailbox.
  It is a stateless waitable query, not a drain: a dropped connection loses no
  state, and neither listen nor list marks read or ack. Sender, thread, kind,
  and limit filters travel in the request body. CLI and MCP expose the same
  operation, including in MCP read-only mode. Per-process bounded admission
  rejects excess concurrent listens with HTTP 429 and `Retry-After`; retrying
  cannot lose a durable delivery. Local and later cross-realm
  inbound use the same mailbox, but cross-realm content still carries no
  authority and resolves against standing receive policy.
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

These routes back the agent self-managed memory, hydration, observational
activity, and dashboard-preference surfaces; see
[context-hydration.md](context-hydration.md). User-
authored memory mutations return the deterministic `echo` string and any
`warnings[]` (e.g. `memory_duplicate`) described in
[api-contract.md](api-contract.md); the internal activity touch returns the
projection contract documented below.

```text
GET  /v1/self                # implemented JSON digest; no formatted emit response yet
GET  /v1/self/peers
POST /v1/self/activity       # authenticated runtime-hook activity projection
GET  /v1/self/dashboard-preferences
PUT  /v1/self/dashboard-preferences
POST /v1/remember            # target; not implemented
POST /v1/sessions:start      # target; not implemented
POST /v1/sessions:end        # target; not implemented
POST /v1/memories:consolidate # superseded target; not implemented
```

- `GET /v1/self` returns the bounded self-digest (`witself self show`): primary
  facts first, then top-N salient memories, authenticated value-free
  `fact_capacity` and `memory_capacity`, memory, message, email, and avatar
  checkpoints, then a one-line index of
  kinds/tags/counts. It is cheap,
  never requires a vector profile or query vector, and is
  hard-capped (default ~8 KiB); when capped it sets `elided=true` and points to
  `:recall` rather than silently truncating. Implemented query parameters select
  what to include (`include_facts`, `include_salient`, `salient_limit`,
  `max_bytes`, `include_counts`, `include_checkpoint`,
  `include_message_checkpoint`, `include_email_checkpoint`,
  `include_avatar_checkpoint`, and `include_sensitive`). Each checkpoint is
  additive and independently fails open with `unavailable:true`; none is
  source content or authority. Both capacity projections are value-free and
  additive. `fact_capacity` reports current-fact pressure without authorizing
  deletion or unrelated rewriting; `memory_capacity` lets an active client
  prefer reversible non-growing consolidation when `near_limit` is true
  without granting the backend semantic authority or waking a model. The target
  `?format=claude-md|agents-md|markdown` renderer would be the HTTP surface for
  `witself digest emit`, but neither that rendering behavior nor the command is
  implemented in the current checkout. Passing `?format=` does not currently
  produce an emit fragment.
- `GET /v1/self/peers` lists every other non-deleted agent in the authenticated
  agent's realm, with each peer's optional last-observed activity fields. Realm
  scope and self exclusion come only from the agent token; there are no realm,
  agent, availability, or status query parameters. A missing activity timestamp
  means no activity has been recorded, not that the peer is offline.
- `POST /v1/self/activity` is the agent-token-only hook ingestion route behind
  those timestamps. It accepts only bounded runtime, installation, canonical
  event, event-id, and client event-time metadata; it never accepts transcript
  content, CWDs, models, session identifiers, availability, or a public
  activity timestamp. The client time and event id order and deduplicate the
  per-agent/runtime/installation projection, while PostgreSQL stamps
  `last_activity_at` when a strictly newer event is accepted. Replaying the
  same or an older event returns the current projection without advancing that
  server-observed time. Transcript upload proceeds independently when an
  activity touch fails. Every transient or domain activity error leaves the
  local event queued so the touch can retry; only an older server's bare
  route-missing `404` is treated as permanently unsupported, allowing the event
  to be removed after its transcript upload succeeds.
- `GET`/`PUT /v1/self/dashboard-preferences` read and upsert the authenticated
  agent's own dashboard UI preferences row — the local dashboard's sole write
  surface (see
  [ADR 0004](decisions/0004-local-agent-dashboard.md)). Both routes are
  agent-token-only and own-row-only, accept no query parameters, and carry the
  strict v1 document contract:
  `{"schema":"witself.dashboard-prefs.v1","theme":<string<=64>}`, unknown keys
  refused, 4 KiB cap. The upsert is last-write-wins with no revision machinery;
  `GET` returns a `null` preferences default when no row was ever stored.
  Reads record no usage and writes emit no audit event (the value-free
  `agent_activity` precedent: a theme flip is not owner-facing).
- `POST /v1/remember` is deferred. If implemented, invoking it is an explicit
  choice of Witself: a clear name→value assertion may upsert a fact and other
  text may add Witself memory with dedup/supersede. It never bypasses validation
  or limits and composes the existing fact and memory create paths. It is not
  the natural-language provider router described in
  [Agent Memory Routing](agent-memory-routing.md).
- Target `POST /v1/sessions:start` would hydrate identity, open goals, and last
  progress in one round-trip (`witself session start`) and emit
  `session.started`; the route and command are not implemented.
- Target `POST /v1/sessions:end` would persist a progress memory (kind
  `session`), update open goals from the request body, and emit `session.ended`;
  the route and `witself session end` command are not implemented.

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
  (`witself account close`). It is audited and supports `dry_run`. It returns
  conflict while any pending/approved enrollment or open rotation exists;
  cancellation remains available while suspended so the work can be settled
  before close.

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

The implemented sealed-plane vertical is agent-owned and derives that owner
from the bearer token. The following nested group/operator secret routes are
target-only for the deferred grants/group-sharing slice:

```text
GET  /v1/agents/{agent_id}/secrets
POST /v1/agents/{agent_id}/secrets
GET  /v1/groups/{group_id}/secrets
POST /v1/groups/{group_id}/secrets
```

In the target contract, these nested routes are the HTTP surface for operator
and group targeting. The CLI `--owner-agent <agent>` flag targets
`/v1/agents/{agent_id}/secrets` (and
the agent-scoped `:reveal`/`:grant`/etc. via the bare `/v1/secrets/{secret_id}`
once the secret is resolved), and `--group <name>` targets
`/v1/groups/{group_id}/secrets` for group-owned secrets. They mirror the secret
reference forms `witself://agent/<agent>/secret/<path>/<field>` and
`witself://group/<group>/secret/<path>/<field>`; the bare
`witself://secret/<path>/<field>` resolves against the caller's own agent. Using
`--owner-agent` is an operator/admin or policy-granted action; resolving or
revealing another agent's or a group's secret requires a grant (`secret:grant`
issued via `:grant`) or a realm role, never the open cross-agent read policy.
They are not registered in schema 67. `DELETE /v1/agents/{agent_id}` also
returns conflict while that agent has pending/approved enrollment or an open
rotation, because deletion would revoke the tokens needed to settle the work.

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
The sealed plane participates only as its client-encrypted state: schema-67
archives include public AVK bindings, terminal enrollment/rotation history,
ciphertext, wrapped DEKs, and value-free receipts, never plaintext values, TOTP
seeds, AVKs, local key files, pairing/passphrase material, or recovery
artifacts. Export rejects active enrollment or rotation work. The client must
move or recover the matching AVK separately after import. The routes back
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

## Agent Avatar Routes

Avatar generation stays in the active AI client. These routes own the
authenticated profile, deterministic fallback, immutable versions, style-pack
selection, validation, activation policy, and retry state:

Agent proposals and repeated generation-failure reports are rejected until a
server-stamped `retry_after` is due. Operator proposals remain an explicit
recovery path. Idempotent mutation replays return the original value-free
receipt plus the resource's current projection; they do not restore an older
mutable view.

```text
GET  /v1/self/avatar
GET  /v1/self/avatar/history
GET  /v1/self/avatar/versions/{version}
GET  /v1/self/avatar/style
POST /v1/self/avatar/proposals
POST /v1/self/avatar:activate
POST /v1/self/avatar:rollback
POST /v1/self/avatar:reset
POST /v1/self/avatar:generation-failed

GET   /v1/agents/{agent}/avatar
GET   /v1/agents/{agent}/avatar/history
GET   /v1/agents/{agent}/avatar/versions/{version}
POST  /v1/agents/{agent}/avatar/proposals
POST  /v1/agents/{agent}/avatar:activate
POST  /v1/agents/{agent}/avatar:reject
POST  /v1/agents/{agent}/avatar:rollback
POST  /v1/agents/{agent}/avatar:reset
PATCH /v1/agents/{agent}/avatar-policy
PATCH /v1/agents/{agent}/avatar-quota
GET   /v1/realms/{realm}/avatar-style
POST  /v1/realms/{realm}/avatar-style/versions
```

Both history routes return newest-first, payload-free version metadata. They
accept `limit` (default 20, maximum 100) and exclusive `before_version` (zero or
omitted for newest), and return `next_before_version` only when another page
exists. Summaries never include SVG, visual specifications, descriptions, or
generation provenance. History includes `svg_sha256`,
`locked_layers_sha256`, the value-free immutable `renderer_profile`, style,
subject, parent, `lineage_generation`, proposer,
timestamps, `payload_state`, original `payload_bytes`, optional
`payload_compacted_at`, optional `payload_compaction_reason`, and the lifecycle
projection `is_active`, `is_proposed`, `was_activated`, `rollback_eligible`, and
`rejected`. `last_activated_at` and `rejected_at` are included when the
corresponding lifecycle record exists.

Use the positive-version detail routes for one exact version. A `full` version
includes its canonical SVG, description, visual specification, and generation
provenance. A `compacted` version still returns HTTP `200` with immutable
metadata, hashes, provenance, payload accounting, and compaction metadata, but
omits `svg`, `description`, and `visual_spec`; it is never rollback-eligible.
These fields let a client choose valid actions without inferring lifecycle or
payload state from version order.

`renderer_profile` is always explicit in current server responses. New
versions are `perceptual-v1`; `legacy` identifies readable, exportable history
that predates the deterministic renderer contract or was written by an older
server during rollout. Legacy is never promoted by inspecting SVG bytes, cannot
seed perceptual continuity or parent same-style self evolution, and is
rebaselined only by an operator replacement, a post-reset parentless proposal,
or a proposal under a newly selected style. A mixed-version client treats a
missing field from an older response as legacy in memory only.

Avatar profile responses include the operator-configured
`retained_payload_count_limit` and `retained_payload_byte_limit`, the current
full-row count in `retained_payload_count`, inclusive full-payload plus retained
continuity-fingerprint bytes in `retained_payload_bytes`, and
`rollback_payload_floor=2`. Defaults are `20` full payloads and `2097152` bytes.
Supported bounds are `4`–`1000` payloads and `524288`–`67108864` bytes.

The operator-only quota route accepts:

```json
{
  "retained_payload_count_limit": 20,
  "retained_payload_byte_limit": 2097152,
  "expected_profile_revision": 7
}
```

It also requires `Idempotency-Key`. Lowering a limit compacts the oldest
eligible inactive payloads in the same transaction before committing the new
limits; an exact replay does not compact again. Proposal creation performs the
same compaction check before inserting the new version. Active and proposed
versions are always protected, as are the two most recently activated distinct
inactive versions in the current lineage. Compaction considers retired
lineages first, then rejected versions, other never-activated versions, and
finally activated versions older than that rollback floor; each class is oldest
version first.

While `WITSELF_AVATAR_PAYLOAD_COMPACTION_ENABLED=false`, a proposal or quota
change that already fits succeeds normally. One that would require cleanup
returns HTTP `409` with `avatar_payload_compaction_not_active` and makes no
mutation; this phase-A conflict is retryable after compaction is activated.

If protected payloads plus an incoming proposal cannot fit after all eligible
payloads are compacted, or a lowered quota cannot be satisfied, the mutation
returns HTTP `409` with `avatar_payload_quota_exceeded`. The transaction leaves
the prior limits and payloads unchanged and creates no proposal, receipt, or
partial audit state.

Reset accepts the exact profile revision, an optional bounded `reason_code`,
and an `Idempotency-Key`. It preserves all version identity/lifecycle records
while advancing the profile to a fresh lineage and deterministic placeholder.
Self reset is
authorized only by `agent_self_managed`; other policies require the operator
route. Reset is not permanent deletion and cannot be used on an empty lineage.

Self routes derive account, realm, and agent only from the bearer token.
Operator paths remain account-scoped. Mutations require an `Idempotency-Key`
header and exact current revision; every response is `private, no-store`. See
[agent-avatars.md](agent-avatars.md) for lifecycle, payload retention, archive,
and SVG boundaries.

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
- [agent-avatars.md](agent-avatars.md)
- [deployment-cells.md](deployment-cells.md)
- [observability-and-operations.md](observability-and-operations.md)
