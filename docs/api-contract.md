# Witself API Contract

> **Sealed-plane API amendment (accepted 2026-07-18):**
> [the client-custodied vault plan](client-custodied-agent-vault.md) controls
> secret routes and wire shapes. The backend authorizes and returns one sealed
> field package at a time but never decrypts it; older server-side reveal and
> KMS capability shapes below are superseded.

Status: draft. This document defines the initial public HTTP API contract for
managed Witself Cloud, self-hosted `witself-server`, and local development
server mode.

Narrative-memory amendment (accepted 2026-07-14): direct memory capture,
history, lexical recall, atomic supersede, lifecycle, evidence resolution,
permanent deletion, and the fenced client-curation protocol are implemented.
The curation API queues deterministic work, freezes authorized inputs, accepts
and hashes an exact client-authored plan, applies it transactionally, and can
perform a guarded compensating rollback. It never runs a model. See
[narrative-memory-and-curation.md](narrative-memory-and-curation.md). Conflicting
`remember`, autonomous consolidation, server-embedding, or backend-synthesis
shapes below are not implementation authority.

Messaging amendment (implemented in the current checkout, not a deployment or
release claim): delivery now includes migration-0034
claim/renew/release/atomic-complete processing, migration-0035 server-derived
causal depth, migration-0036 deterministic per-message failure counting, and the
`message_processing` capability. Migration 0037 adds explicit-list/realm
audience snapshots. Migration 0038 and the `message_requests` capability add
open multi-assignee requests with client-ranked selection and exact claim
fences.

## Decision

The backend API is a public product contract. The `witself` CLI, MCP adapter,
managed Witself Cloud, self-hosted deployments, and local development server
mode should use the same API semantics where practical.

The first API version should use a stable `/v1` base path:

```text
https://api.witself.com/v1
https://witself.internal.example.com/v1
http://127.0.0.1:8080/v1
```

Breaking HTTP contract changes require a new version path. Non-breaking fields
may be added to JSON objects when clients can safely ignore unknown fields.

Witself's API spans two planes. For the open plane (memories and facts) it
guards the integrity and authenticity of identity data: there is no reveal
ceremony and no encrypted-only export, but every cross-agent and destructive
identity mutation is attributed, audited, and reversible by default. For the
sealed plane (secrets and TOTP), clients encrypt under a client-custodied agent
vault key and the backend stores and authorizes ciphertext-only field packages.
Inventory is redacted, exact material delivery is audited, and encrypted vault
state is included in account archives without the agent vault key. Secret
material is never embedded, recalled, included in the self-digest, or written
to a plaintext identity export. The amendment above controls wherever the
older target sections below still describe a KMS-rooted or server-decrypt path.

## Transport Requirements

- Production managed and self-hosted endpoints must require TLS.
- Local development may use `http://127.0.0.1` or `http://localhost`.
- Requests and responses should use `application/json` unless an endpoint is
  explicitly for binary export or download.
- Raw tokens, `sensitive` fact values, `sensitive` memory content, message
  bodies and payloads, embedding vectors, raw payment details, wallet
  credentials, provider secrets, and all sealed-plane material — secret field
  values, TOTP seeds, generated TOTP codes, generated passwords, and DEK/KEK key
  material — must not appear in URLs, query strings, access logs, server logs,
  audit records, or ordinary error responses.
- Request bodies that carry memory content, fact values, or message bodies must
  be redacted before logging.

## Authentication

CLI remote mode should pass the token loaded from `--token-file`,
`WITSELF_TOKEN_FILE`, `WITSELF_TOKEN`, or stored profile auth as a bearer token:

```http
Authorization: Bearer <token>
```

The token resolves server-side to a principal:

- Agent token bound to one realm and one named agent.
- Operator token bound to an account and one or more realm roles.
- Admin/service token for tightly controlled internal or deployment use.

The authenticated principal, not a caller-supplied agent name, determines the
acting agent identity. This is load-bearing for cross-agent access and
messaging: the actor on every operation and the `from` on every message are
derived server-side from the token. Caller-supplied agent or group targets are
authorization inputs, resolved against policy or operator role, never a way to
assume another agent's identity.

V0 agent tokens are durable by default. They do not expire unless an operator
sets `ttl` or `expires_at`. Disabled agents and revoked tokens must fail
authentication immediately.

Human operator login for managed endpoints should be performed through
CLI-initiated browser or device-code flows. The API should expose auth-session
or bootstrap endpoints only as needed to support those flows; it should not
accept raw account passwords from CLI JSON payloads. Self-hosted first-operator
setup should use a one-time bootstrap token or equivalent deployment-owned
mechanism. The operator auth model is tracked in
[operator-auth.md](operator-auth.md).

## Response Envelope

API responses should use the same envelope as CLI `--json` output:

```json
{
  "schema_version": "witself.v0",
  "ok": true,
  "data": {},
  "warnings": []
}
```

Error responses should use the same structured error object and error codes
defined in [json-contracts.md](json-contracts.md).

The meta/discovery endpoints are the exception: `GET /v1/version`,
`GET /v1/capabilities`, and the health probes return a **bare/flat** JSON object
with a top-level `schema_version` and no `ok`/`data`/`warnings` wrapper. The
domain/data endpoints (memories, facts, policies, groups, messaging, secrets,
totp, and the rest) keep the standard envelope above.

HTTP status codes should align with the structured error:

| HTTP | Error code | Meaning |
|---:|---|---|
| `400` | `usage_error` | Invalid request shape, flags, or query parameters. |
| `401` | `auth_failed` | Missing, invalid, expired, or revoked token. |
| `403` | `access_denied` | Authenticated principal lacks permission, or no policy allows the cross-agent access. |
| `403` | `stored_secret_limit_reached` | Implemented non-retryable refusal when a new top-level secret would exceed the authenticated owner agent's retained cap. |
| `404` | `not_found` | Resource not found or not visible to the caller. |
| `409` | `conflict` | Already exists, stale version, or state conflict. |
| `422` | `usage_error` | Valid JSON with semantically invalid input. |
| `429` | `rate_limited` | Transient service-protection or throttle limit; `retryable: true`. |
| `429` | `limit_exceeded` | Plan, quota, or hard usage cap; `retryable: false`. |
| `500` | `internal_error` | Unexpected server failure. |
| `503` | `backend_unavailable` | Backend dependency unavailable. |
| `501` | `unsupported_operation` | Current backend does not support the operation. |

Because two codes share HTTP `429` (and CLI exit `7`), clients must distinguish
conditions by `error.code` and the `retryable` flag, not by status or exit code
alone:

- `rate_limited` is a transient service-protection throttle. It is
  `retryable: true`; clients should back off and retry, honoring
  `details.retry_after` (seconds) and the `Retry-After` header when present.
- `limit_exceeded` is a plan/quota hard cap. It is `retryable: false`; retrying
  will not succeed until an operator raises the plan or the usage window resets.
- `stored_secret_limit_reached` is the implemented inventory-cap specialization.
  It is HTTP 403 with `retryable: false` and a value-free top-level `limit`
  object containing `used`, `max`, `remaining`, `unlimited`, and `over_limit`.
  Retrying a new create cannot succeed until a tombstone delete releases
  capacity or the resolved maximum changes; exact replay of an already-complete
  idempotent create is resolved before the gate.

  The current sealed-plane handler returns this flat, machine-stable exception
  to the generic error wrapper:

  ```json
  {
    "schema_version": "witself.v0",
    "code": "stored_secret_limit_reached",
    "error": "stored secret limit reached",
    "retryable": false,
    "limit": {
      "used": 100,
      "max": 100,
      "remaining": 0,
      "unlimited": false,
      "over_limit": false
    }
  }
  ```

This matches the overage behaviors in [billing-and-limits.md](billing-and-limits.md):
`throttle` surfaces as `rate_limited`, `block` surfaces as `limit_exceeded`.

The `access_denied` code covers both ordinary scope failures and cross-agent
policy denials. A policy denial should include non-sensitive decision context in
`details`, such as the requested permission, scope, and target owner, so callers
and `policy test` agree on why access was refused. Denial context must not
include memory content, fact values, or message bodies. The policy engine is
tracked in [access-policy.md](access-policy.md).

## Capability Discovery

Every backend should expose its supported features:

```text
GET /v1/capabilities
```

The HTTP `GET /v1/capabilities` response is a **bare/flat** object: a top-level
`schema_version` alongside `backend`, `principal`, `features`, and `limits`, not
the `ok`/`data` envelope.

Clients should call this during setup, `witself capabilities`, and before
service-administration operations whose availability differs by backend.
`witself setup` should use the default managed Witself Cloud endpoint unless the
caller supplies `--endpoint`, `WITSELF_ENDPOINT`, or a stored profile endpoint.
Local setup bypasses the remote API and is selected explicitly with `--local`.

Capability results should include:

- Backend kind: `managed`, `self-hosted`, or `local`. `backend.kind` is a
  configured value, not inferred: it comes from `WITSELF_BACKEND_KIND` and
  defaults to `self-hosted`; `managed` is set only by Witself's managed
  deployment, and `local` is reported by the CLI's local adapter and is never the
  server. `kind` is advisory — clients should branch on specific feature flags,
  and each feature is independently gated so a mislabeled kind unlocks nothing.
- The deployment `account` block (`{"id": "acc_…"}`) on single-account backends
  (local, self-hosted): the seeded default/root account, surfaced unauthenticated.
  On `managed` the account context comes from the authenticated `principal`
  instead, so the top-level block is omitted; it is omitted entirely when no
  database is configured.
- Server version and API version.
- Supported feature flags.
- Unsupported feature reasons.
- Effective limits when available.
- Direct memory, lexical recall, atomic-supersede, permanent-delete,
  automatic-curation, and client-vector capability states. Optional vector
  profiles/hybrid recall are reported independently from the lexical baseline.
- Authenticated principal and realm context when authenticated.

Examples of feature flags:

- `memories`
- `memory_recall`
- `memory_supersede`
- `memory_permanent_delete`
- `memory_vector_profiles`
- `client_vector_recall`
- `automatic_capture`
- `opportunistic_curation`
- `scheduled_curation`
- `facts`
- `self_digest`
- `consolidate`
- `semantic_recall`
- `policies`
- `groups`
- `messaging`
- `message_listen`
- `message_reply`
- `message_processing`
- `message_requests`
- `export`
- `audit`
- `mcp`
- `operator_auth_browser`
- `operator_auth_device_code`
- `self_hosted_bootstrap`
- `billing`
- `payments`
- `crypto_payments`
- `support`
- `attachments`
- `terraform_outputs`
- `field_level_encryption`
- `secrets`
- `totp`
- `password_generate`
- `runtime_injection`
- `client_side_decrypt`
- `server_side_decrypt`
- `cross_realm_collaboration`
- `federation`
- `agent_card`
- `multi_cell`

In v0.0.x the `features` are reported as `{"supported": false, "reason":
"not_implemented"}` until each subsystem ships.

The `secrets`, `totp`, `password_generate`, and `runtime_injection` flags report
whether the sealed plane — the KMS-backed credential side of Witself, distinct
from the open plane of memories and facts — is enabled on this backend. The open
plane (memories, facts, recall, digest) does not require KMS and stays available
even when the sealed plane is disabled; the sealed plane requires a configured
KMS provider, so these flags are off on an open-plane-only deployment. The
sealed plane carries hard carve-outs: secret and TOTP material is never embedded,
never returned by semantic recall, never in the self-digest, never in the
plaintext identity export, and never ingested from `CLAUDE.md`/`AGENTS.md`. The
sealed-plane model is tracked in [secret-model.md](secret-model.md) and
[totp-2fa.md](totp-2fa.md).

`client_side_decrypt` and `server_side_decrypt` must be advertised honestly per
backend and realm and select the reveal/TOTP response shape (see
[key-hierarchy.md](key-hierarchy.md) and [json-contracts.md](json-contracts.md)).
A managed token-only backend MUST advertise `server_side_decrypt: true` and
returns a decrypted `field.value` plus `value_encoding`; a client-side-decrypt
backend returns ciphertext plus envelope metadata and the key-unwrap material,
with no plaintext. Clients use capability discovery to know which path applies
before a reveal. When the sealed plane is enabled, the `secrets`/`totp` flags are
nested objects that also report the active `kms_provider` (`aws-kms`, `gcp-kms`,
`azure-key-vault`, or `local-dev`) and the per-realm key state, so callers know
which decrypt path and KMS dependency are in effect. The CMK→per-realm KEK→
per-secret/field DEK hierarchy is tracked in [key-hierarchy.md](key-hierarchy.md)
and [encryption-model.md](encryption-model.md).

The implemented surface advertises `memories`, `memory_recall`,
`memory_supersede`, `memory_permanent_delete`, and
`opportunistic_curation` independently. `opportunistic_curation` means an
authorized client can create or claim queued work and drive the fenced
request/run/plan/apply/rollback protocol. It does not mean the server can launch
an AI runtime. `automatic_capture` and `scheduled_curation` remain explicitly
unsupported with reason `not_implemented`; clients must not infer either from
basic memory or curation support. Those flags describe the backend, not the
foreground integration: PostgreSQL stores due work, Codex and Claude can inject
an already-durable pending checkpoint into model-visible hook context, and
Cursor, Grok Build, OpenClaw, Antigravity, and Copilot use guided `self.show`.
Runtime hooks never launch inference or a curator. The optional client-owned
`memory curate auto` process is retained only
as explicit legacy/manual or user-scheduled compatibility tooling.
`memory_recall` includes the
deterministic lexical/structured baseline. `memory_vector_profiles`,
`client_vector_recall`, and `semantic_recall` report the optional implemented
migration-0032 vector/hybrid surface independently; there is no hidden backend
embedding provider or fallback call. The `self_digest` flag
(`GET /v1/self`) is independent and uses deterministic salience/recency
hydration plus authenticated, value-free `memory_checkpoint` and
`message_checkpoint` objects. The memory field is a point-in-time
request/run/fence pointer; the message field is content-free mailbox and
open-request attention state. Neither is source content or inference authority.
Both projections are additive and fail open with their own `unavailable:true`
marker instead of hiding identity, facts, or salient memories.

The `cross_realm_collaboration`, `federation`, and `agent_card` flags report
whether this backend speaks the cross-realm collaboration substrate: whether it
accepts and runs cross-realm conversations, whether it maintains a federation
allow-list of accepted peer realms, and whether it publishes a signed realm
card. They travel together in practice but are advertised separately so a backend
can publish a card without yet accepting inbound conversations. The cross-realm
collaboration model is tracked in [agent-collaboration.md](agent-collaboration.md).
The `multi_cell` flag reports whether tenant placement and migration across a
fleet of independent cells is in effect; it is a managed-plane property and is
off on single-cell self-hosted and local backends. Cell placement and migration
are tracked in [deployment-cells.md](deployment-cells.md).

Self-hosted and local deployments should return deterministic
`unsupported_operation` errors for unsupported commands. They should also include
capability metadata so humans and agents know what to do next.

V0 may expose command, route, and JSON shapes before every managed-service
feature is live. Billing, payment, crypto payment, support, and broader
managed-account operations should be governed by capability discovery and must
fail deterministically when unsupported.

## Idempotency

Mutating `POST` endpoints should accept an `Idempotency-Key` header when a
client might retry after a timeout or network failure.

```http
Idempotency-Key: 7a3fe2b5-f2d8-4a19-9d95-6f6f7b8b8e8a
```

Idempotency is required for operations that create durable or external effects:

- Account creation.
- Realm creation.
- Agent creation.
- Token creation or rotation.
- Memory capture, adjust, atomic supersede, forget, restore, reactivate,
  evidence resolution, and permanent-delete apply. Permanent-delete preview is
  read-only and has no key.
- Memory-curation request, start, renew, plan, apply, cancel, abandon, and
  rollback. Read-only request/run/input/status reads have no key.
- Fact set, primary promotion, and delete.
- Fact candidate proposal, confirmation, and rejection.
- Policy create and delete.
- Group create, member add/remove, and delete.
- Message send and acknowledgement.
- Secret create, update, rename, copy, archive, restore, delete, grant, and
  revoke.
- TOTP enrollment and deletion.
- Identity export and import jobs.
- Hosted payment or crypto checkout sessions.
- Support ticket creation and comments.

Idempotency records must not store memory content, fact values, message
bodies/payloads, embedding vectors, raw tokens, payment details, provider
secrets, secret field values, TOTP seeds, generated codes, or key material.

Atomic memory supersede has one operation `Idempotency-Key` header and one
distinct body `idempotency_key` for each replacement capsule. The operation key
replays the complete source/replacement result only for the exact normalized
request; a changed expected version, replacement set, reason, or provenance is
a conflict. Retry receipts keep only value-free source/replacement version
references and supersession-set metadata, including `replacement_count` and a
lowercase SHA-256 `replacement_digest` over the sorted exact replacement set.
Replay fails closed if live relation membership no longer matches that digest.

Narrative `content` is paired with `content_encoding`, which is `plain` by
default or `base64` for canonical binary-safe content. Capture and every
supersede replacement accept `content_encoding`; adjustment uses
`set_content_encoding`. Current-memory and immutable-version responses always
return the effective encoding. The server admits worst-case JSON escaping for
every store-legal payload: capture bodies are capped at 8 MiB, adjust bodies at
16 MiB, and 32-replacement supersede bodies at 257 MiB. A body above its route
ceiling returns `413 Request Entity Too Large`; malformed in-bound JSON remains
`400 Bad Request`.

Fact mutation retry keys are scoped to the authenticated agent and mutation
surface (fact set, candidate proposal, or candidate decision). The service
stores the key and a one-way normalized request fingerprint beside the created
assertion or candidate; it does not copy the fact value into a separate retry
record. Reusing a key for a different request or candidate decision returns
`409 Conflict`. Retrying the same fact set, proposal, confirmation, or rejection
returns the existing resource without appending another assertion or candidate.

`witself setup` should combine API idempotency with name-based ensure semantics:
account, realm, and agent creation can safely select existing visible resources
when names match. Token create and rotate operations remain sensitive. Setup
must not silently reuse or rotate an existing token; callers must choose
`--reuse-existing-token` or `--rotate-existing-tokens`. If no token choice is
provided when one is required, the API-facing operation should fail with
`conflict` and a stable detail such as `reason: "token_choice_required"`.

Message send is idempotent by `Idempotency-Key`: a retried send with the same
key returns the original `msg_…` rather than fanning out a duplicate to the
recipient mailbox. Messaging delivery semantics are tracked in
[inter-agent-messaging.md](inter-agent-messaging.md).
Omitted direct-send `kind` normalizes to actionable `request` in the backend;
clients must send explicit `note` for FYI-only runner notification/ack without
provider inference.

## Dry Runs

Mutating endpoints that support CLI `--dry-run` should accept either a request
body field or query parameter named `dry_run`.

Preferred JSON request shape:

```json
{
  "dry_run": true,
  "reason": "operator recovery",
  "request": {}
}
```

Dry runs should validate:

- Request shape.
- Authentication and authorization, including cross-agent policy evaluation.
- Resource existence.
- Conflicts and stale-version checks.
- Quotas and rate limits.
- External-provider prerequisites for operations that actually use one. Direct
  memory writes and lexical recall have no model/provider prerequisite.
- Planned side effects, including which prior primary fact a promotion would
  demote and which records a forget would tombstone.

Dry runs must not persist state, generate tokens, compute or store embedding
vectors, create hosted provider sessions, charge payment methods, send or
deliver messages, or send customer/support notifications.

`dry_run` is expected on the integrity-impacting identity mutations called out in
[requirements.md](requirements.md): memory forget/restore/delete, cross-agent
curate/forget, fact delete, primary promotion, policy delete, group member
removal, and group deletion. The canonical dry run for a pure access decision is
`POST /v1/policies:test`, which evaluates whether a subject/permission/target
would be allowed without touching any record.

The implemented address-based fact-delete dry run is `DELETE
/v1/facts?dry_run=true&subject={subject}&predicate={predicate}`. It resolves the
fact without fetching its value or recording retrieval usage; an id-based
preview is also available at `DELETE /v1/facts/{fact_id}?dry_run=true`. The
metadata-only response supplies the resolved assertion id and value-free
candidate-set revision used as apply-time concurrency tokens. Apply requires
both tokens plus `Idempotency-Key`; a changed assertion or candidate set
returns 409. Successful
apply permanently removes values, assertion/evidence history, and
address-matching candidates while retaining a non-restorable value-free fact
tombstone and immutable usage events. Responses use `Cache-Control: private,
no-store` and never include values, evidence, candidate reasons, raw retry keys,
or value-derived request fingerprints.

Permanent memory deletion is a narrower dry-run exception. Its implemented
preview is `DELETE /v1/memories/{memory_id}?dry_run=true` and accepts the memory
id only; it rejects idempotency, version/scrub guards, and a direct-user header.
It returns deterministic value-free impact and concurrency guards without
creating a preview resource. Apply uses the same route without `dry_run`, binds
`expected_version` and `scrub_set_revision`, and requires `Idempotency-Key` plus
`X-Witself-Direct-User-Authorized: true`. Preview and apply responses use
`Cache-Control: private, no-store` and contain no memory/evidence values, raw
retry keys, or content-derived hashes.

## Pagination And Filtering

List endpoints should use cursor pagination:

```text
GET /v1/memories?limit=100&cursor=...
```

Rules:

- `limit` should have a safe default and a documented maximum.
- `cursor` is opaque.
- Responses use `items` and `next_cursor`.
- Filter names should match CLI concepts where practical, such as `owner_agent`,
  `owner_group`, `kind`, `tag`, `source`, `name`, `prefix`, `primary`,
  `sensitive`, `state`, `template`, `since`, and `until`.
- `sensitive` memories and facts must be redacted in list, scan, and broad
  recall responses by default. Memory redaction clears content and its hash,
  tags, links, capture/lifecycle reasons, occurrence bounds, client provenance,
  and evidence. An authorized single-record read returns the value (there is no
  reveal ceremony for the open plane).
- Embedding vectors must never be returned by list endpoints.
- Sealed-plane secret field values, TOTP seeds, and generated codes are never
  returned by any list, scan, or single-record `GET`. Secret and TOTP list/show
  responses carry only metadata (name, template, owner, fields present, rotation
  and grant state); the value is returned only by the explicit, audited
  `:reveal`/`:code` colon actions below.

Recall is a query operation, not a list traversal, so it uses a `POST`
colon-action route rather than `GET` filters (see
[Action And Colon Routes](#action-and-colon-routes)). Recall responses are
ranked and may include per-item score context, but they follow the same
cursoring and redaction rules as list endpoints.

## Versioning And Schema Generation

The first implementation should generate OpenAPI from the same route and schema
definitions used by the Go server, or generate both from one source of truth.

Requirements:

- Publish an OpenAPI document with each release once the API exists.
- Validate API examples in CI.
- Keep CLI, MCP, and API JSON structs aligned.
- Treat `schema_version` (`witself.v0`) as the machine-readable response
  contract version.
- Treat `/v1` as the HTTP route contract version.

The shared resource shapes are tracked in [json-contracts.md](json-contracts.md).

## Route Catalog

Route style decision: use resource-oriented `/v1` routes with plural resources
and explicit colon action subroutes for mutating or workflow operations. The
canonical route style is tracked in [api-routes.md](api-routes.md).

Initial route groups:

| Route group | Purpose |
|---|---|
| `/v1/version` | Server version and build metadata. Bare/flat object, no auth. |
| `/livez` | Liveness probe. No auth, no sensitive config. |
| `/readyz` | Readiness probe. No sensitive config. |
| `/startupz` | Startup probe. No auth, no sensitive config. |
| `/healthz` | Alias probe. No auth, no sensitive config. |
| `/v1/whoami` | Current authenticated principal, realm, and identity-anchor summary. |
| `/v1/capabilities` | Backend feature discovery and limits, including independent direct-memory, lexical-recall, atomic-supersede, permanent-delete, and curation-automation states. |
| `/v1/self` | Implemented JSON self-digest: primary facts, salient memories, value-free memory and message checkpoints, and a kinds/tags/counts index. `include_facts`, `include_salient`, `include_counts`, `include_checkpoint`, `include_message_checkpoint`, and `include_sensitive` select bounded sections; sensitive open-plane values remain redacted unless the authenticated caller explicitly opts in. Checkpoint projection failure is additive and fails open without hiding identity or recall. The target `?format=` emit renderer is not implemented. |
| `/v1/self/peers` | Agent-token-scoped list of every other live agent in the same realm with optional last-observed activity; no caller-controlled realm or agent selector and no availability inference. |
| `/v1/self/activity` | Agent-token-scoped runtime-hook ingestion for the latest-only activity projection; identity and public observation time are server-derived. |
| `/v1/self/dashboard-preferences` | Agent-token-only, own-row-only read (`GET`) and last-write-wins upsert (`PUT`) of the local dashboard's UI preferences row: a strictly validated 4 KiB `{"schema":"witself.dashboard-prefs.v1","theme":...}` document with unknown keys refused. Reads record no usage; writes emit no audit event. |
| `/v1/remember` | Deferred explicit Witself capture action; it is not the natural-language cross-provider router. |
| `/v1/sessions` | Target, not implemented: multi-session bootstrap would hydrate identity and open goals (`:start`) and persist a progress memory (`:end`). |
| `/v1/auth` | CLI-initiated browser/device-code auth sessions when Witself owns the flow. |
| `/v1/bootstrap` | One-time self-hosted first-operator bootstrap. |
| `/v1/account` | Customer account and human operator/admin management. |
| `/v1/realms` | Realm lifecycle and membership. |
| `/v1/agents` | Named agent lifecycle and policy summary. |
| `/v1/tokens` | Token create, list, revoke, and rotate. |
| `/v1/memories` | Implemented agent-owned capture, read, list, history, lexical recall, adjust, atomic supersede, forget, restore, reactivate, evidence resolution, and permanent delete. |
| `/v1/memory-curation-requests` | Implemented agent-self curation work queue: create/coalesce, list, inspect, and claim due work. List accepts `exclude_sensitive=true`: full tokens omit explicitly sensitive scopes but retain separately authorized transcript scopes; restricted profiles always omit both. |
| `/v1/memory-curation-runs` | Implemented fenced runs: inspect frozen inputs, renew, plan, apply, cancel, abandon, and guarded rollback. |
| `/v1/memory-curation-status` | Implemented value-free owner-lane/request/run status, optionally for one run. |
| `/v1/memory-curation-preflight` | Authenticated effective credential/profile permissions, protocol schema, inference boundary, and limits for a curator. |
| `/v1/facts` | Fact set, get, list, scan, primary promotion, and delete. |
| `/v1/policies` | Cross-agent policy create, list, show, delete, and test. |
| `/v1/groups` | Security group lifecycle and membership. |
| `/v1/transcripts` | Append-only visible user/assistant/system/tool interaction capture; agent write/own-read and account-operator audit-read. |
| `/v1/messages` | Implemented same-realm direct, bounded explicit-list, and realm send; recipient-only reply; server-derived causal depth; metadata-only list/listen; read/acknowledgement; and fenced claim/renew/release/atomic-complete processing. Group and cross-realm delivery remain target extensions. |
| `/v1/message-requests` | Implemented message-backed realm open jobs: create/list/detail plus candidate offer/decline, coordinator client-ranked select/cancel, and selected-agent claim/renew/release/atomic-complete actions. |
| `/v1/conversations` | Cross-realm conversation/task resource: list and show participants, state, and turn/cost budgets. |
| `/v1/federation/peers` | Federation allow-list: list, add, and remove the accepted peer realms for cross-realm collaboration. |
| `/v1/secrets` | Sealed-plane secret create, show, list, scan, reveal, update, rename, copy, archive, restore, delete, grant, and revoke. |
| `/v1/totp` | Sealed-plane TOTP enrollment, metadata, code generation, and deletion. |
| `/v1/password` | Stateless password generation (`:generate`). |
| `/v1/audit` | Audit event list and show. |
| `/v1/billing` | Plans, usage, limits, subscription, payment methods, hosted provider sessions, invoices, and crypto payment flows. |
| `/v1/support` | Support tickets and comments. |
| `/v1/exports` | Identity, realm, and account export jobs. |
| `/v1/imports` | Identity, realm, and account import jobs. |

Exact route names can evolve during implementation, but they should preserve the
style in [api-routes.md](api-routes.md) and remain recognizable in the OpenAPI
document.

The signed realm card is served at `GET /.well-known/witself-card.json` and is
deliberately **not** under `/v1`: like a well-known discovery document it is
fetched by peer realms during federation handshake, carries no caller identity,
and returns the realm card shape (handle, agents and skills, endpoint, accepted
auth, signing JWKS, delivery modes, and a mandatory JWS signature) defined in
[json-contracts.md](json-contracts.md). The cross-realm collaboration substrate
is tracked in [agent-collaboration.md](agent-collaboration.md).

Tenant placement, home-cell resolution, and tenant migration across a fleet of
independent cells are a **separate control-plane surface**, not per-cell `/v1`
product routes. A cell serves the `/v1` contract above for the tenants placed on
it; which cell a realm or account lives on is resolved by the thin control plane.
That boundary is tracked in [deployment-cells.md](deployment-cells.md).

Operational endpoints such as `GET /metrics` are server operations endpoints,
not versioned product API routes. `/metrics` should be served from the dedicated
metrics listener, default `:9090`, expose Prometheus text-format output, and
follow the privacy and low-cardinality label rules in
[observability-and-operations.md](observability-and-operations.md).

Health endpoints should be served from the dedicated health listener, default
`:8081`, at the short `/livez`, `/readyz`, `/startupz` (plus `/healthz` alias)
paths rather than under `/v1`. The public API listener, default `:8080`, should
not be the default target for Kubernetes probes.

Billing usage and limit endpoints should expose plan-tier usage rather than raw
per-call billing in v0. Backends should return deterministic `rate_limited` or
`limit_exceeded` errors when a plan or service-protection limit throttles or
blocks an operation. The Witself metered dimensions (active agents, stored
memories and facts, recalls/reads, writes, embedding operations, vector storage,
cross-agent accesses, groups, and messages, plus the sealed-plane dimensions
stored secrets, secret reads, TOTP codes, runtime injections, and encrypted
storage bytes) are defined in [billing-and-limits.md](billing-and-limits.md).

Export endpoints produce structured, round-trippable identity data. Unlike
Witpass, plaintext identity export is a first-class v0 feature, not a forbidden
one: an export may include memory content, fact values, `primary` flags,
`sensitive` markers, links, and edit history. Exports of `sensitive` records
require an audit `reason` and emit a warning. Account archives also carry
messages, migration-0035 causal links/depths, recipient read/ack state, and
migration-0034 processing state plus migration-0036 `failure_count`. Import
derives or validates depth against the reply graph, preserves completed result
links and the deterministic failure count, converts every active `claimed`
delivery to `available`, advances its generation, and clears its claim/key/lease
fields so a source-cell fence cannot survive the move. Archives older than
schema 36 receive `failure_count=0` during upgrade.
Migration-0037 audience metadata and the exact per-recipient snapshot are also
portable. Schema-38 archives additionally require the request, candidate,
selection, and claim streams. Import preserves terminal request history and
result links but cancels active source-cell request reservations/claims and
advances their fence before the destination account resumes. Schema-39 archives
also require the latest-only `agent_activity` projection so last-observed peer
metadata survives a cross-cell move. Its local runtime-hook outbox is host retry
state and is not part of the account archive; restored activity remains
observational and never establishes availability.
Export/import is tracked in
[backup-and-recovery.md](backup-and-recovery.md).

## Action And Colon Routes

Do not put memory content, fact values, or message bodies in path or query
parameters. Mutating and workflow operations use explicit colon-action subroutes
and always use `POST`, never `GET`.

Allowed:

```http
POST /v1/memories:recall
```

With request body:

```json
{
  "query": "what did we decide about the migration",
  "kind": "episodic",
  "tags": ["migration"],
  "limit": 20
}
```

Avoid:

```text
/v1/memories/recall?query=what+did+we+decide
```

The open plane has no reveal route. Fact and memory reads return ordinary
identity data under normal authorization; reading a `sensitive` record is an
ordinary authorized read, not a reveal ceremony, and `sensitive` facts use
lightweight redaction rather than the sealed-plane reveal ceremony.

The sealed plane is the exception: `POST /v1/secrets/{secret_id}:reveal` and
`POST /v1/totp/{secret_id}:code` are the explicit, audited, reveal-gated
value-returning operations, and `POST /v1/password:generate` returns a freshly
generated password once. These are the only sealed-plane routes that emit
plaintext; secret and TOTP material is never embedded, recalled, in the
self-digest, or in the plaintext identity export. Token create and token rotate
remain the open-plane routes that return a raw token exactly once.

Initial action and curation-workflow routes (curation uses durable slash
subresources rather than colon verbs):

```http
POST /v1/remember
POST /v1/memories:recall
POST /v1/memories/{memory_id}/supersede
POST /v1/memories:consolidate # superseded target; not implemented
POST /v1/memories/{memory_id}:forget
POST /v1/memories/{memory_id}:restore
POST /v1/memories/{memory_id}:reactivate
POST /v1/memory-evidence/{evidence_id}/resolution
GET  /v1/memory-curation-preflight
POST /v1/memory-curation-requests
GET  /v1/memory-curation-requests # ?exclude_sensitive=true
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
POST /v1/facts/{fact_id}:primary
POST /v1/sessions:start # target; not implemented
POST /v1/sessions:end   # target; not implemented
POST /v1/policies:test
POST /v1/messages:listen
POST /v1/messages/{message_id}:reply
POST /v1/messages/{message_id}:read
POST /v1/messages/{message_id}:ack
POST /v1/messages/{message_id}:claim
POST /v1/messages/{message_id}:renew
POST /v1/messages/{message_id}:release
POST /v1/messages/{message_id}:complete
POST /v1/message-requests
GET  /v1/message-requests
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
POST /v1/secrets/{secret_id}:reveal
POST /v1/secrets/{secret_id}:rotate
POST /v1/secrets/{secret_id}:archive
POST /v1/secrets/{secret_id}:restore
POST /v1/secrets/{secret_id}:grant
POST /v1/secrets/{secret_id}:revoke
POST /v1/totp/{secret_id}:code
POST /v1/password:generate
```

Notes on specific actions and workflows:

- `POST /v1/remember` is deferred. If implemented, calling it explicitly selects
  Witself, so it may route a clear name-to-value assertion to a fact and other
  text to Witself memory with dedup/supersede. It is a `POST` because captured
  text belongs in the body, and it composes existing fact and memory resources.
  It is not the router for an agent's natural-language remember request; that
  provider-aware behavior is defined in
  [Agent Memory Routing](agent-memory-routing.md).
- `:recall` is an implemented deterministic query over the token-bound agent's
  active current memory heads. It is a `POST` because literal query text and
  structured kind/tag/link/origin/capture/time/sensitivity filters travel in
  the body. PostgreSQL lexical, salience, and recency signals are returned
  separately. Supplying an immutable `vector_profile_id` and compatible
  caller-authored `query_vector` adds deterministic bounded hybrid ranking and
  explicit coverage/degradation metadata; there is no backend inference or
  embedding-provider fallback. Cross-agent/group recall remains target work.
- `/supersede` atomically replaces one expected active version with a nonempty
  caller-authored set. The operation key is the `Idempotency-Key` header; each
  replacement carries a distinct body `idempotency_key` and at least one exact,
  pending, or explicitly unavailable evidence item. HTTP 201 returns the full
  authorized source/replacements and a value-free supersession receipt with
  exact references plus replacement count and membership digest. The backend
  validates the proposed set but makes no semantic choice. The current HTTP/
  client/CLI/MCP surface is agent-self.
- `:consolidate` is superseded and not implemented. Semantic merge/split
  decisions require an exact caller-authored curation plan; direct one-to-many
  supersede uses the caller-authored route above. The backend may not choose any
  of those semantic changes autonomously.
- Target `:start` and `:end` routes would form the multi-session bootstrap pair;
  neither is implemented. `:start` would hydrate identity, open goals, and last
  progress in one round-trip without mutating state. `:end` would persist a
  progress memory (kind `session`) and update open goals, so it remains designed
  as `POST` with a body and would be audited as `session.started` /
  `session.ended`.
- `:forget` is the default destructive path: a reversible versioned lifecycle
  state. `:restore` reverses it, and `:reactivate` explicitly restores a
  reverted or otherwise invalidly-restorable head. Permanent deletion is the
  implemented, guarded `DELETE /v1/memories/{memory_id}` preview/apply contract
  described under [Dry Runs](#dry-runs); the current surface is agent-self only.
- `/v1/memory-evidence/{evidence_id}/resolution` appends one terminal exact or
  explicitly unresolvable result to a pending evidence locator. It requires an
  idempotency key and never edits the pending row.
- The 15 `/v1/memory-curation-*` endpoints, including preflight, form one
  implemented agent-self protocol. A request carries deterministic source
  scope, coalescing, priority,
  due time, and retry metadata. `start` claims due work, freezes bounded memory,
  evidence, transcript, and cursor inputs, and returns a lease plus fencing
  generation. `inputs` pages those immutable snapshots; all value-bearing input
  is untrusted data, never instructions. `renew` is a heartbeat. `plan` accepts
  only strict `witself.memory-plan.v1` JSON with the five reversible primitives
  `create`, `replace`, `supersede`, `relate`, and `propose_fact`; the last creates
  a review candidate and never sets a canonical fact. The server normalizes and
  hashes the plan but performs no synthesis. `apply` binds the fence, accepted
  revision, and SHA-256 plan hash and either commits the complete plan plus
  contiguous cursors or makes no semantic change. A zero-action plan is valid:
  applying it advances the exact reviewed cursor intervals without creating a
  memory or fact. `cancel` terminates work;
  `abandon` requeues retryable work. `rollback` requires the original apply
  receipt and the complete exact set of apply-produced current heads, refuses
  live downstream dependencies, performs append-only compensation, never
  rewinds cursors, and creates read-only replay work. Status reads are
  value-free. Every curation response uses `Cache-Control: private, no-store`.
  There is no server-side scheduler or inference launcher; an active foreground
  client normally claims and processes at most one pending request per turn.
- Curator credentials are immutable, expiring token profiles rather than an MCP
  display filter. `curator-preview` admits only list/get/start/inputs/renew/
  plan/abandon/status; `curator-apply` adds only apply. Both are rejected by
  ordinary memory, fact, transcript, message, self, and permanent-delete
  handlers, and both are barred from sensitive curation scopes. An operator
  mints one with `POST /v1/agents/{agent_id}/curator-tokens`, a mandatory audit
  display name, and a TTL of at most 24 hours. Existing tokens migrate as
  `full`. The authenticated preflight route reports the effective permission
  matrix for the presented token; `/v1/capabilities` remains deployment-wide
  feature discovery and is not an authorization oracle.
- `:primary` is an atomic promotion that demotes any prior primary of the same
  logical fact kind for the same owner. At most one primary per logical kind per
  owner. See [facts-model.md](facts-model.md).
- `:test` evaluates a subject/permission/target/scope against current policy and
  returns the deciding policy id or a deny reason. It never mutates state and is
  the canonical access-decision dry run.
- `POST /v1/messages:listen` is the implemented stateless long-poll receive
  verb. It returns the oldest unacknowledged inbound message **metadata**, or an
  empty `messages` array with `timed_out=true` when the window elapses. The
  request accepts `wait_seconds` from 0 through 20 (default 20) and optional
  `from_agent`, `thread_id`, `kind`, and `limit` filters; it has no cursor. The
  response never exposes bodies, marks read, or acknowledges and uses
  `Cache-Control: private, no-store`. Each server process bounds concurrent
  listen admission and returns HTTP 429 with `Retry-After` when saturated;
  retrying loses no durable mailbox state. Agents run no inbound HTTP server; an
  installed policy instructs an already-active foreground client to use a
  zero-wait call at task startup and then use the explicit read/ack lifecycle;
  it cannot force the model to comply. It is a `POST` because the bounded
  wait/filter request is authenticated input rather than a public cacheable URL.
  Delivery semantics are tracked in
  [inter-agent-messaging.md](inter-agent-messaging.md); the no-wake foreground
  inference boundary is tracked in
  [autonomous-realm-messaging.md](autonomous-realm-messaging.md).
- `POST /v1/messages/{message_id}:reply` accepts only reply content and a
  header idempotency key. The caller must be the parent recipient; the server
  derives the recipient from the parent sender and derives the thread and
  `reply_to_message_id`; it derives `causal_depth` as parent plus one. Supplying
  routing, identity, or depth fields is rejected.
- `:read` returns content and records only the recipient read transition.
  `:ack` separately records per-recipient acknowledgement; the acting agent is
  derived from the token for both operations. The ack response is metadata-only
  and never returns the message body or payload.
- `:claim` acquires or exactly replays a bounded 30–900 second processing lease
  on the token-bound recipient's unacknowledged delivery. The required
  `Idempotency-Key` is hashed at rest and never returned. A different live
  claimant receives HTTP 409; an available or expired acquisition increments
  the monotonic generation fence. Generation is solely a stale-writer fence;
  claim, expiry, and takeover do not increment `failure_count`.
- `:renew` and `:release` require the exact `claim_id` and positive `generation`.
  Renewal replaces the database-time expiry; release makes processing
  available and invalidates the old fence without acknowledging. Release also
  accepts optional `deterministic_failure` (default false). Only an exact-fence
  release with that field true atomically increments the migration-0036
  `failure_count`; provider-wide, configuration, cancellation, timeout, and
  lease-maintenance releases leave it unchanged. Installed foreground policy
  directs clients to release and count the first four deterministic failures and
  complete a durable escalation when `failure_count` is already 4 or greater;
  the backend does not enforce that threshold or model compliance.
- `:complete` requires the exact unexpired fence and an `Idempotency-Key`. In
  one transaction it derives recipient, thread, causal parent, causal depth,
  sender, account, and realm from the claimed message and token; inserts one
  result reply; links that reply; and marks processing completed. It returns
  HTTP 201 with
  `processing` plus `message` and deliberately does not ack. Exact completion
  retry returns the same result; a stale fence returns HTTP 409.
- `/v1/message-requests` is agent-only, realm-local coordination. Create
  requires `Idempotency-Key`, persists one realm `open_request` message and its
  immutable candidate snapshot atomically, and accepts only
  `selection_policy=client_ranked`. List/get authorize the coordinator or a
  snapshot candidate and never widen access merely because an agent is now in
  the realm.
- Candidate `:offer`/`:decline`, coordinator `:select`/`:cancel`, and selected
  agent `:claim`/`:renew`/`:release`/`:complete` derive every actor and routing
  field from the bearer token and stored request. Offers are ordinary untrusted
  direct messages and reserve no capacity. The client ranks; PostgreSQL only
  validates offered agent IDs, permits selection after the offer deadline or
  once no candidate remains pending, locks the request, enforces the 1-8
  capacity ceiling, and issues bounded reservations and exact `mrc_` claim
  fences. Selecting fewer than `max_assignees` is valid. Completion atomically
  creates and links a `kind=result` reply; after that result the request closes
  when the selected batch has no other live reservation or claim. A coordinator
  client outage leaves durable `awaiting_selection` state; deleting the
  coordinator system-cancels its open requests. There is no first-eligible
  fallback or backend inference.
- `:rotate` (on `/v1/tokens`) issues a replacement token and returns the raw
  value once; the prior token is revoked immediately or after an explicit grace
  period.
- `:reveal` (on `/v1/secrets`) is the sealed plane's explicit, audited,
  reveal-gated value path. It is a `POST` because the requested `field` and audit
  `reason` travel in the body and because the returned plaintext or unwrap
  material must never appear in a URL. It requires `secret:reveal`, is metered as
  `secret_read`, and is audited as `secret.reveal`. Its response shape is
  selected by capability discovery:
  - When the backend advertises `server_side_decrypt: true` (managed token-only
    pods), the server unwraps the DEK against the per-realm KEK in KMS and
    returns a decrypted `field.value` plus `value_encoding`; the audit record
    carries the `server_side_decrypt` flag.
  - When the backend advertises `client_side_decrypt: true`, the response
    returns ciphertext, AEAD/envelope metadata, and the wrapped key material with
    no plaintext; the client unwraps locally.

  The two shapes are defined in [json-contracts.md](json-contracts.md) and the
  key hierarchy in [key-hierarchy.md](key-hierarchy.md). Revealed values are
  never embedded, recalled, placed in the self-digest, or written to the
  plaintext export.
- `:code` (on `/v1/totp`) returns a current TOTP code (and the seconds
  remaining) for an enrolled secret. It requires `totp:code`, is metered as
  `totp_code`, and is audited as `totp.code`. Like `:reveal`, the generated code
  and the underlying seed are sealed material: never logged, embedded, recalled,
  in the digest, or in the plaintext export. The seed itself is high-value sealed
  material revealed only through its own audited path. See
  [totp-2fa.md](totp-2fa.md).
- `:rotate` (on `/v1/secrets`) writes a new secret version (new per-secret/field
  DEK), keeping prior versions per retention; it requires `secret:update` and is
  audited as `secret.updated`. `:archive` and `:restore` are the soft-delete pair
  for secrets, mirroring memory `:forget`/`:restore`; archive is reversible.
  The implemented `POST /v1/secrets/{secret_id}:delete` is an exact-row-version,
  retry-keyed tombstone delete. It scrubs secret metadata, deletes every field
  and wrapped-DEK row, releases retained capacity, and keeps only a minimal
  value-free tombstone plus receipt/audit evidence. Irreversible
  `DELETE /v1/secrets/{secret_id}` purge of that tombstone remains target-only.
  These lifecycle actions are audited as
  `secret.archived`/`secret.restored`/`secret.deleted`.
- `:grant` and `:revoke` (on `/v1/secrets`) manage cross-agent and operator
  access to a sealed-plane secret. Unlike the open plane — where cross-agent
  read/curate/forget is governed by the [access-policy.md](access-policy.md)
  identity policy engine — secret cross-agent and operator access is governed by
  explicit grants plus realm roles, tracked in
  [authorization-and-roles.md](authorization-and-roles.md). `:grant` names the
  target `owner_agent` or `owner_group` and the granted scope; both require
  `secret:grant`, support `dry_run`, carry an audit `reason`, and are audited as
  `secret.grant`/`secret.revoke`. Secrets are not subject to the open cross-agent
  read/curate/forget verbs.
- `:generate` (on `/v1/password`) is a stateless generator: it returns a freshly
  generated password once and creates no resource. It is a `POST` because the
  generated value must never appear in a URL; the returned value is sealed
  material (never logged, embedded, recalled, in the digest, or in the plaintext
  export). See [secret-model.md](secret-model.md).

Cross-agent and operator mutations through these routes (`contribute`, `curate`,
and `forget` against another agent's or group's records) require an audit
`reason` in the request body, support `dry_run`, and are fully attributed in
audit, for example "memory `mem_…` of agent A was pruned by agent B under policy
`pol_…`". The verb model and guardrails are tracked in
[access-policy.md](access-policy.md).

## Cross-Agent And Group Targeting

Cross-agent and group-scoped operations carry their target in the request body
or path, never in a way that lets a caller assume another identity:

- The acting agent is always the token-bound agent. The body never names the
  actor.
- A cross-agent read, contribute, curate, or forget names the target owner
  (`owner_agent` or `owner_group`) and is evaluated against policy with a
  default-deny stance. Absence of a matching `allow` policy yields
  `access_denied`.
- Group-scoped writes target `owner_group`; the same guardrails as cross-agent
  writes apply (audit `reason`, `dry_run`, soft-delete by default).
- Group membership is managed through nested member routes —
  `GET /v1/groups/{group_id}/members`, `POST /v1/groups/{group_id}/members`, and
  `DELETE /v1/groups/{group_id}/members/{principal}` — not colon actions; the
  canonical route shapes are tracked in [api-routes.md](api-routes.md).
- Operators may target any owner within their realm (operator override),
  audited the same way as agent actions and subject to the same `reason`
  requirements on destructive and cross-agent operations.

Identity references (`witself://…`) used in memory `links[]` or request bodies
are validated at write time and re-checked at resolve time; cross-agent and
cross-group references resolve only when policy permits. Reference parsing and
resolution are tracked in [json-contracts.md](json-contracts.md).

## Managed, Self-Hosted, And Local Behavior

Managed Witself Cloud should support the full commercial command surface as the
product matures.

Self-hosted deployments should support the core memory, fact, recall, policy,
group, messaging, audit, reference, and export/import contracts. Billing, hosted
payment flows, Witself support workflows, and internal admin workflows may be
disabled unless configured by the operator. Direct memory and lexical recall
require PostgreSQL but no model provider. Implemented vectors are
client-supplied, optional, and stored as portable PostgreSQL JSONB; pgvector is
not required. The sealed plane (secrets, TOTP, password generation, runtime
injection) is optional and requires a configured KMS provider; when no KMS
provider is configured the `secrets`/`totp` capabilities are off and the sealed-
plane routes return deterministic `unsupported_operation`. Enabling the sealed
plane gates readiness on the KMS provider; it does not affect the open-plane
memory path.

Local development mode should support enough API behavior to exercise the CLI,
MCP adapter, JSON contracts, and integration tests. It exercises the same model-
free lexical recall contract and reports `backend.kind: "local"` in
capabilities.

## Related Docs

- [requirements.md](requirements.md)
- [v0-scope.md](v0-scope.md)
- [cli-command-surface.md](cli-command-surface.md)
- [api-routes.md](api-routes.md)
- [json-contracts.md](json-contracts.md)
- [memory-model.md](memory-model.md)
- [facts-model.md](facts-model.md)
- [secret-model.md](secret-model.md)
- [totp-2fa.md](totp-2fa.md)
- [secret-size-and-attachments.md](secret-size-and-attachments.md)
- [encryption-model.md](encryption-model.md)
- [key-hierarchy.md](key-hierarchy.md)
- [authorization-and-roles.md](authorization-and-roles.md)
- [data-model.md](data-model.md)
- [access-policy.md](access-policy.md)
- [security-groups.md](security-groups.md)
- [inter-agent-messaging.md](inter-agent-messaging.md)
- [agent-collaboration.md](agent-collaboration.md)
- [deployment-cells.md](deployment-cells.md)
- [backend-architecture.md](backend-architecture.md)
- [self-hosting.md](self-hosting.md)
- [server-command-surface.md](server-command-surface.md)
- [operator-auth.md](operator-auth.md)
- [observability-and-operations.md](observability-and-operations.md)
- [threat-model.md](threat-model.md)
- [billing-and-limits.md](billing-and-limits.md)
- [backup-and-recovery.md](backup-and-recovery.md)
