# Witself API Routes

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
  inference boundary, and server limits for the presented credential. Clients
  must use it instead of treating deployment-wide `/v1/capabilities` as an
  authorization decision.
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
  A foreground client must not mark provider-wide, configuration, cancellation,
  or lease-maintenance failures deterministic and treats the fifth
  deterministic attempt as escalation under the default handling guidance.
- `POST /v1/messages/{message_id}:complete` validates the exact unexpired fence
  and required `Idempotency-Key`, then in one transaction creates a
  server-routed result reply at parent `causal_depth + 1`, links it to the
  delivery, and marks processing completed. It returns HTTP 201 with
  `processing` and `message`; it does not ack. Active conflicts and stale fences
  return HTTP 409.
  The message sender is always derived server-side from the token, never from
  the request body; sender forgery is structurally impossible.
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
- `POST /v1/secrets/{secret_id}:reveal` is the explicit, audited value-returning
  op (`witself secret reveal`). It runs the reveal ceremony, requires the
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

These routes back the agent self-managed memory, hydration, and observational
activity surfaces; see [context-hydration.md](context-hydration.md). User-
authored memory mutations return the deterministic `echo` string and any
`warnings[]` (e.g. `memory_duplicate`) described in
[api-contract.md](api-contract.md); the internal activity touch returns the
projection contract documented below.

```text
GET  /v1/self                # implemented JSON digest; no formatted emit response yet
GET  /v1/self/peers
POST /v1/self/activity       # authenticated runtime-hook activity projection
POST /v1/remember            # target; not implemented
POST /v1/sessions:start      # target; not implemented
POST /v1/sessions:end        # target; not implemented
POST /v1/memories:consolidate # superseded target; not implemented
```

- `GET /v1/self` returns the bounded self-digest (`witself self show`): primary
  facts first, then top-N salient memories, authenticated value-free memory and
  message checkpoints, then a one-line index of kinds/tags/counts. It is cheap,
  never requires a vector profile or query vector, and is
  hard-capped (default ~8 KiB); when capped it sets `elided=true` and points to
  `:recall` rather than silently truncating. Implemented query parameters select
  what to include (`include_facts`, `include_salient`, `salient_limit`,
  `max_bytes`, `include_counts`, `include_checkpoint`,
  `include_message_checkpoint`, and `include_sensitive`). Each checkpoint is
  additive and independently fails open with `unavailable:true`; neither is
  source content or authority. The target
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
identity export. The routes back `witself export` and `witself import`:

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
