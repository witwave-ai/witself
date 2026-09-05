# Witself Observability And Operations

Status: draft. This document defines the observability and Kubernetes
operations requirements for `witself-server` and `witself-worker`.

The canonical [Feature Status](feature-status.md) scorecard applies this
operational bar to every feature. Instrumentation alone does not pass its
observability gate without continuous collection, alert routing, and a tested
external receiver.

Implementation status (2026-07-16): the server now exports `witself_up`,
bounded route-template HTTP request/in-flight/latency metrics, narrative-memory
domain-operation and recall counters/histograms, vector coverage/fallback
counters, and curation domain-call counters. The curation counter records
completed API-backed calls, including successful idempotent replays; it does
not claim that every call caused a state transition or lease event. Durable run
transitions, lease claim/renew/release/fence/expiry events, queue-age gauges,
and the other families in the larger table remain pending. Labels are restricted to server-controlled enums,
principal kinds, and route templates. Tenant identifiers, concrete URL paths,
memory or message content, vector/profile values, database metadata, and error
text are never collected. Families not named in this implementation note are
still pending rather than silently implied by the document.

Worker implementation status (2026-07-23): `witself-worker` has private health
and Prometheus listeners, process/job-loop gauges and counters, and value-free
transcript-retention batch and item counters. It runs in a distinct Deployment
with its own metrics Service and optional ServiceMonitor or PodMonitor. Worker
labels are limited to process-defined job names, retention mode, and bounded
result/count classes; tenant identifiers and stored content are never labels.

Control-plane realm-alias guard status (2026-08-02): the authoritative Durable
Object maintains exact value-free open-request counters per realm and account,
plus per-realm allocated-customer counts. Authenticated request-list responses
surface only readiness, configured maxima, used, remaining, and at-limit state
for the caller's scoped realm/account. A bounded legacy rebuild failure emits
one fixed Cloudflare log message with no account, realm, alias, request, or claim
identifier and no error text; count-changing requests remain fail-closed while
rebuilding. Technical-ceiling refusal emits exactly the low-cardinality
Cloudflare JSON event `realm_email_alias_pending_limit_refused` with only
`scope=realm|account` and numeric `limit`; it includes no tenant identifier and
does not append per-refusal durable audit history. Administrative counter
recovery does append one value-free request audit event, then uses bounded
clear, canonical scan, and verification phases before reopening writes. These
counters are admission state, not billable usage and not tenant labels in the
Prometheus plane.

Canonical realm-email and authority-recovery status (2026-08-02): canonical
inventory, control-plane delivery, and edge delivery are independent
exact-`true` gates and are all default-off. Inventory responses report only
bounded progress (`complete`, cycle, accounts/routes scanned, and account-page
completion); failures use the fixed log messages
`realm-email-canonical: inventory configuration is incomplete` or
`realm-email-canonical: inventory tick failed`, with no tenant or route
identifier. Realm-close status exposes only account/realm ids and one closed
phase label; durable alarms retry the fenced operation.

The realm-email-alias authority journal status is deliberately value-free:
enabled/required, exact sequence/hash/epoch fences, pending/fork flags, bounded
scan counts, recovery phase, authority/derived key counts, and state digest.
Neither journal entries nor operator errors are copied into Prometheus labels.
Bootstrap/checkpoint leave an observable write freeze until completion; a hash
fork leaves a permanent local fence. Empty-target recovery exposes bounded
`replay`, `replayed`, `rebuild`, `sealed`, or `failed` state. A sealed target is
not evidence of cutover: no runtime or administrative route automatically
changes the active Durable Object name.

Narrative-memory contract (accepted 2026-07-14): memory observability covers
deterministic search, client-supplied vectors, curation queues/leases/conflicts,
and archive rebuilds. `witself-server` never calls a model, so there are no
backend embedding-provider metrics, credentials, egress checks, or health
probes. See
[narrative-memory-and-curation.md](narrative-memory-and-curation.md).

Sealed-plane custody amendment (accepted 2026-07-18):
[ADR 0003](decisions/0003-client-custodied-agent-vault.md) and the
[client-custodied vault contract](client-custodied-agent-vault.md) supersede
KMS-rooted agent-secret, realm-KEK, and server-side-decrypt language below. The
backend holds no AVK key material, calls no KMS for agent secrets, and exposes
no decrypt or `server_side_decrypt` path. Ordinary infrastructure KMS and
storage-encryption references are unaffected.

## Decision

`witself-server` must be instrumented from the beginning. Prometheus-compatible
metrics, Kubernetes health probes, structured logs, and Helm chart support are
part of the server contract, not post-launch extras.

The initial operational surface should include:

- A public API listener, default `:8080`.
- A separate health listener, default `:8081`.
- A separate metrics listener, default `:9090`.
- `GET /livez` on the health listener for Kubernetes liveness probes.
- `GET /readyz` on the health listener for Kubernetes readiness
  probes.
- `GET /startupz` on the health listener for Kubernetes startup
  probes.
- `GET /metrics` on the metrics listener for Prometheus scrape output.
- Structured JSON logs with request IDs and strict redaction.
- Helm values for probes, metrics, ServiceMonitor, PodMonitor, resources,
  autoscaling, disruption budgets, security context, and network policy.

Witself's telemetry spans both planes. The **open plane** (memories and facts)
is instrumented for memory operations, deterministic recall, optional vector
validation/search, curation state, fact operations, policy decisions,
cross-agent access, groups, and inter-agent
messaging; its threat focus is the **integrity and authenticity** of identity
data. The **sealed plane** (secrets and TOTP) is instrumented for secret
operations, reveals, TOTP codes, and KMS calls; its threat focus is the
**confidentiality** of credential material. The privacy rules tighten across
both: metrics and logs must never carry memory content, fact values, message
bodies, or embedding vectors, and must never carry secret values, TOTP seeds,
generated TOTP codes, KMS key material, or private keys. The sealed plane keeps
its carve-outs everywhere, including telemetry — secret material is never
embedded, recalled, placed in the self-digest, or plaintext-exported, and is
revealed only through the audited reveal ceremony (see
[secret-model.md](secret-model.md) and [encryption-model.md](encryption-model.md)).

## Listener Model

`witself-server` should use separate listener surfaces for API traffic, health
probes, and metrics.

Default listeners:

| Listener | Default | Purpose | Public exposure |
|---|---:|---|---|
| API | `:8080` | Product API traffic. | May be exposed through ingress or load balancer. |
| Health | `:8081` | Kubernetes and service-manager probes. | Cluster-local only by default. |
| Metrics | `:9090` | Prometheus scraping. | Restricted to monitoring systems by default. |

Health endpoints should not share the public API listener by default. Metrics
should not share either the public API listener or the health listener. This
keeps unauthenticated probes and operational scrape traffic out of the public
serving path and gives Kubernetes, network policy, and service monitors clean
targets.

Metrics must be configurable through server config, environment variables, CLI
flags, and Helm values. The canonical environment variable should be
`WITSELF_METRICS_ENABLED=true|false`. When metrics are disabled, the metrics
listener should not be started and the Helm chart should not render metrics
ServiceMonitor or PodMonitor resources unless explicitly requested in a
development profile.

## Health Probes

Health endpoints must never expose memory content, fact values, message bodies
or payloads, vector values, secret values, TOTP seeds, generated TOTP codes,
raw tokens, database URLs, client model credentials, object-store
credentials, KMS credentials or key material, private keys, passphrases, raw
payment details, wallet credentials, or provider secrets.

Health probes should be served from the dedicated health listener, default
`:8081`. The Helm chart should wire Kubernetes probes directly to this port.

Probe semantics:

| Endpoint | Purpose | Dependency checks |
|---|---|---|
| `/livez` | Process is alive and should not be restarted. | Minimal process-local checks only. |
| `/readyz` | Server can safely receive traffic. | Storage, migrations, KMS reachability when the sealed plane is enabled, and read-only maintenance state. |
| `/startupz` | Server completed boot and initial dependency validation. | Startup config, migrations state, and required dependency availability. |
| `/healthz` | Alias for the liveness probe. | Minimal process-local checks only. |

Liveness should be conservative. A transient database, object-store, or KMS
failure should normally make readiness fail, not
force Kubernetes to restart a healthy process.

Readiness should fail when:

- Required storage is unavailable.
- Required migrations are missing or incompatible.
- PostgreSQL full-text facilities required for universal recall are unavailable.
- Migration-0032 vector tables are missing or invalid. This gates vector
  operations, not lexical memory traffic; no extension is required.
- Required KMS operations cannot complete when the sealed plane is enabled.
  KMS is a hard readiness gate only for sealed-plane deployments; an
  open-plane-only deployment does not depend on KMS (see
  [storage.md](storage.md) and [key-hierarchy.md](key-hierarchy.md)).
- The server is intentionally in a mode that should not accept ordinary
  traffic.
- The server is draining or shutting down.

There is no embedding provider in server readiness. Missing, stale, or
incompatible client vectors use the PostgreSQL full-text path and are reported
as vector coverage, not dependency health. A deployment with optional vector
support disabled remains ready for all universal memory operations.

Startup should cover slow boot paths such as config validation, first database
connection, migration status checks, full-text index availability,
migration-0032 vector-table validation, and KMS provider client initialization when the
sealed plane is enabled.

## Prometheus Metrics

The server should expose Prometheus text-format metrics on `/metrics` from the
dedicated metrics listener, default `:9090`. Managed and self-hosted
deployments should restrict access through network policy, service annotations,
or Prometheus Operator resources.

Metrics should be enabled by default for all server deployments (including
self-hosted Kubernetes), and off only for local/`--dev` mode or when the
operator explicitly disables them. This is the same rule stated in
[server-command-surface.md](server-command-surface.md) and
[helm-chart.md](helm-chart.md).

Initial metric families should include:

| Metric family | Purpose |
|---|---|
| `go_*` and `process_*` | Standard Go runtime and process metrics. |
| `witself_http_requests_total` | HTTP request counts by route template, method, status class, and result. |
| `witself_http_request_duration_seconds` | HTTP request latency histogram by route template and method. |
| `witself_http_in_flight_requests` | In-flight HTTP requests. |
| `witself_auth_attempts_total` | Authentication attempts by principal kind, result, and reason class. |
| `witself_token_operations_total` | Token create, rotate, revoke, and verification operations. |
| `witself_secret_operations_total` | Sealed-plane secret operations by operation (`create`, `show`, `update`, `rename`, `copy`, `archive`, `restore`, `delete`, `grant`, `revoke`), owner kind, and result. The `show` operation returns metadata only and never a value; reveals are counted separately. |
| `witself_secret_limit_rejections_total` | Implemented non-retryable stored-secret create refusals. Its bounded labels are exactly `limit_dimension="stored_secret"` and `operation="create"`; it never carries an account, realm, agent, secret id, name, value, or error text. |
| `witself_fact_limit_rejections_total` | Implemented non-retryable current-fact capacity refusals. Its bounded labels are exactly `limit_dimension="stored_fact"` and operation from the closed set `create` or `confirm`; it never carries account, realm, agent, subject, predicate, fact/candidate id, usage, maximum, value, or error text. Phase B activates finite defaults only after migration 0078 reconciles every target cell. |
| `witself_memory_limit_rejections_total` | Implemented non-retryable active-memory capacity refusals. Its bounded labels are exactly `limit_dimension="stored_memory"` and operation from the closed set `create`, `supersede`, `restore`, `reactivate`, or `curation_apply`; it never carries account, realm, agent, memory, plan, usage, maximum, content, or error-text labels. |
| `witself_plan_limit_rejections_total` | Implemented non-retryable realm and agent create refusals. Its bounded labels are `limit_dimension="realms"`, legacy `"agents"`, or `"agents_per_realm"`, plus `operation="create"`; it never carries an account, realm, agent, resource name, or error text. |
| `witself_secret_reveals_total` | Sealed-plane value-returning reveals (`secret reveal` and reference resolution that returns a value) by principal kind, owner kind, `server_side_decrypt` (`true`, `false`), and result. These are the audited reveal-ceremony events; the metric counts events only and never carries the revealed value. |
| `witself_totp_operations_total` | TOTP operations by operation (`enroll`, `code`, `show`, `delete`), owner kind, `server_side_decrypt` (`true`, `false`), and result. The `code` operation is value-returning and audited; the metric never carries the generated code or the seed. |
| `witself_kms_operations_total` | KMS envelope operations by provider, operation (`generate_data_key`, `encrypt`, `decrypt`, `rotate`), and result. Present only when the sealed plane is enabled. |
| `witself_kms_operation_duration_seconds` | KMS operation latency histogram by provider and operation. Present only when the sealed plane is enabled. |
| `witself_memory_operations_total` | Memory domain operations by operation (`add`, `read`, `list`, `history`, `adjust`, `supersede`, `forget`, `restore`, `reactivate`, `evidence_resolve`, `delete`), authenticated principal kind, and result. Authentication or request-decoding failures remain visible in the HTTP family rather than being misreported as completed domain calls. |
| `witself_memory_recalls_total` | Recall requests by mode (`lexical`, `hybrid`), authenticated principal kind, and result. |
| `witself_memory_recall_duration_seconds` | Recall latency histogram by mode and authenticated principal kind. |
| `witself_memory_recall_hits` | Histogram of result counts returned per recall, by mode. |
| `witself_memory_vector_validations_total` | Optional client-vector submissions by operation (`memory`, `query`), validation result, and bounded reason class. |
| `witself_memory_vector_searches_total` | Optional bounded hybrid searches by coverage class (`full`, `partial`, `none`, or `unknown` when the domain call fails before coverage is known) and result. No profile id, model name, or vector value is a label. |
| `witself_memory_vector_search_duration_seconds` | Deterministic JSONB-vector comparison latency by coverage class. |
| `witself_memory_vector_fallbacks_total` | Hybrid requests that used lexical-only ranking because vectors were missing, stale, or incompatible. This is coverage, not provider health. |
| `witself_fact_operations_total` | Fact operations by operation (`set`, `get`, `list`, `delete`, `primary_change`), owner kind, and result. |
| `witself_remember_total` | Deferred metric for a future explicit Witself `remember` action, by `routed_kind` (`fact`, `memory`), owner kind, and result. It never carries captured text. |
| `witself_self_digest_renders_total` | Self-digest (`self show` / `GET /v1/self`) renders by source surface, `elided` (`true`, `false`), and result. |
| `witself_self_digest_render_duration_seconds` | Self-digest render latency histogram by source surface. The digest path performs no model call. |
| `witself_self_digest_elided_entries` | Histogram of entries elided from a digest render when the byte/line cap is hit, by source surface. |
| `witself_memory_curation_operations_total` | Completed curation domain calls by operation (`start`, `renew`, `plan`, `apply`, `cancel`, `abandon`, `rollback`) and result. Successful idempotent replays count as calls, not as proven state transitions. |
| `witself_memory_curation_requests` | Due curation requests by bounded state/priority class. No transcript or memory content is exposed. |
| `witself_memory_curation_runs_total` | Client-run curation transitions by state (`started`, `planned`, `applied`, `conflict`, `abandoned`, `interrupted`, `rolled_back`) and result. |
| `witself_memory_curation_actions_total` | Caller-authored plan actions by primitive (`create`, `replace`, `supersede`, `relate`, `propose_fact`), mode (`preview`, `apply`, `rollback`), and result. |
| `witself_memory_curation_lease_events_total` | Durable lease claim, renew, fence, expire, and release events by result. Pending store/audit-point instrumentation; API-call success is not treated as proof that a new lease event occurred. |
| `witself_session_operations_total` | Session lifecycle operations by `phase` (`start`, `end`), owner kind, and result. |
| `witself_ingest_operations_total` | Ingest runs (CLAUDE.md/AGENTS.md/GEMINI.md import) by `mode` (`dry_run`, `apply`) and result. Source labels are never raw file paths. |
| `witself_ingest_records_total` | Records produced by ingest by `outcome` (`fact_added`, `memory_added`, `duplicate_skipped`). |
| `witself_policy_decisions_total` | Policy evaluations by permission verb, decision (`allow`, `deny`), scope, and owner kind. |
| `witself_crossagent_accesses_total` | Cross-agent identity accesses by permission verb, scope, owner kind, and result. |
| `witself_group_operations_total` | Group operations by operation (`create`, `delete`, `member_add`, `member_remove`) and result. |
| `witself_messages_total` | Messaging events by stage (`sent`, `delivered`, `read`), recipient kind, and result. |
| `witself_message_rate_limit_rejections_total` | Implemented shared message-write budget refusals, including both retryable exhaustion and non-retryable zero/oversized-debit cases. Labels are the closed sets `limit_dimension` (`message_sent`, `message_delivered`, or `unknown`), `scope` (`agent`, `realm`, `recipient`, or `unknown`), and `operation` (`send`, `reply`, `complete`, `request_open`, `request_offer`, `request_complete`, or `unknown`). It never carries account, realm, agent, recipient, request, or message ids; plan/source names; limit/usage/retry values; content; or error text. |
| `witself_agent_email_ingests_total` | Signed inbound agent-email deliveries by the single bounded `outcome` label: `retained`, `omitted_capacity`, `over_size`, `storage_full`, `feature_disabled`, `receive_disabled`, `unknown_recipient`, `retry_canary_temporary`, `retry_canary_rejected`, or `error`. `retained` means the accepted message kept its raw MIME, including ordinary text-only mail, while `omitted_capacity` means bounded text and metadata were retained without the attachment-bearing raw payload. `storage_full` is the independent schema-91 cell-ledger refusal; it is not an account-plan limit. The metric never carries account, realm, agent, address, sender, message id, plan, byte count, limit value, content, or error text. |
| `witself_agent_email_cell_storage_metrics_up`, `witself_agent_email_cell_storage_retained_bytes`, `witself_agent_email_cell_storage_admission_bytes`, `witself_agent_email_cell_storage_hard_bytes`, `witself_agent_email_cell_storage_root_rows`, `witself_agent_email_cell_storage_admission_root_rows`, `witself_agent_email_cell_storage_counted_rows`, `witself_agent_email_cell_storage_hard_counted_rows` | Implemented unlabeled, value-free schema-91 cell-ledger gauges. The server performs one read-only singleton query per scrape under a fixed two-second deadline. A read/query/invariant failure emits only `metrics_up 0` and omits the seven values; it never exports database error text or account/message identity. These are logical charges and thresholds, not PostgreSQL relation size, PVC usage, or billable account allowance. |
| `witself_agent_email_rate_limit_rejections_total` | Signed inbound safety refusals with only closed `limit_dimension`, `scope`, and `source` labels. Account scope is explicit and bounded; no tenant, sender, address, limit value, or arbitrary key becomes a label. |
| `witself_worker_agent_email_outbound_batches_total`, `witself_worker_agent_email_outbound_items_total`, `witself_worker_agent_email_outbound_last_success_timestamp_seconds` | Durable outbound worker health and value-free closed outcomes. No sender, recipient, message, provider id, or error text is a label. |
| `witself_worker_agent_email_retention_batches_total`, `witself_worker_agent_email_retention_items_total`, `witself_worker_agent_email_retention_last_success_timestamp_seconds` | Preview/enforce batch results, bounded inbound/outbound/provider-event/suppression item kinds, and last successful attempt. An errored multi-pass attempt records one error batch with any already-committed counts and does not refresh last-success. |
| `witself_worker_agent_email_rate_bucket_cleanup_batches_total`, `witself_worker_agent_email_rate_bucket_cleanup_deleted_rows_total`, `witself_worker_agent_email_rate_bucket_cleanup_last_success_timestamp_seconds` | Cooperative cleanup across the three physical inbound/outbound limiter tables with bounded result labels and no tenant-derived labels. |
| `witself_message_delivery_duration_seconds` | Send-to-delivery latency histogram by recipient kind. |
| `witself_conversations_total` | Cross-realm conversation/task lifecycle transitions by `conversation_state` (`submitted`, `working`, `input_required`, `auth_required`, `completed`, `failed`, `canceled`) and result. Counts state transitions only; it never carries the `conversation_id`, participant handles, or message content (see [agent-collaboration.md](agent-collaboration.md)). |
| `witself_relay_envelopes_total` | Blind-relay envelope throughput by `direction` (`inbound`, `outbound`), `relay_action` (`routed`, `dropped`, `quarantined`), and `result`. The relay sees only routing metadata, so this metric carries no envelope body, signature, or peer realm handle. |
| `witself_relay_envelope_duration_seconds` | Relay route/forward latency histogram by `direction`. |
| `witself_loop_suspended_total` | Loop-safety suspensions by `suspend_reason` (`turn_budget`, `hop_limit`, `ttl_expired`, `repeat_hash`, `flood`). Mirrors the `loop.suspended` audit event; carries no conversation id or message content. |
| `witself_budget_exhausted_total` | Per-conversation budget exhaustions by `budget_kind` (`turn`, `cost`) and `enforcement` (`warn`, `fail`). Mirrors the `budget.exhausted` audit event; carries no spend amount that identifies a tenant and no conversation id. |
| `witself_federation_decisions_total` | Deny-by-default federation decisions by `federation_stage` (`peer_check`, `consent`), `decision` (`allow`, `deny`), and result. Mirrors the `federation.peer_allowed` / `federation.peer_denied` / `federation.consent_accepted` audit events; it never carries a peer realm handle, key, or card. |
| `witself_cell_placements_total` | Tenant placement decisions by `placement_reason` (`residency`, `capacity`, `wave`, `manual`) and result. Mirrors the `tenant.placed` audit event; it never carries a realm/account id or a cell id (see [deployment-cells.md](deployment-cells.md)). |
| `witself_cell_migrations_total` | Tenant migrations between cells by `migration_phase` (`started`, `completed`, `failed`) and `plane` (`open`, `sealed`). Mirrors the `tenant.migration_started` / `tenant.migration_completed` / `tenant.migration_failed` audit events; it never carries a realm/account id or a source/destination cell id. |
| `witself_cell_migration_duration_seconds` | Tenant migration latency histogram by `plane` (`open`, `sealed`). |
| `witself_audit_events_total` | Audit events emitted by type, result, and backend. |
| `witself_audit_write_failures_total` | Audit sink write failures. |
| `witself_audit_queue_depth` | Buffered audit events waiting to be written when a queue exists. |
| `witself_usage_events_total` | Usage metering events by dimension and result. |
| `witself_limit_decisions_total` | Rate limit, quota, and plan-limit decisions by dimension and action. |
| `witself_storage_operations_total` | Storage operations by backend, operation, and result. |
| `witself_storage_operation_duration_seconds` | Storage operation latency histogram. |
| `witself_vector_storage_bytes` | Approximate client-vector JSONB storage size when known. |
| `witself_object_store_operations_total` | Object/blob storage operations when configured. |
| `witself_migration_version` | Applied migration version. |
| `witself_migration_pending` | Pending migration count when known. |

The production Cloudflare inbound-email Worker also writes one best-effort
Analytics Engine point per final SMTP-facing disposition to
`witself_agent_email_edge`. This is an edge dataset, not a Prometheus family.
Its fixed schema marker is `witself.agent-email.edge.v1`; `outcome` is one of
`accepted`, `discarded_feature_disabled`, `rejected_cell_capacity`,
`rejected_invalid_recipient`,
`rejected_unknown_recipient`, `rejected_inactive_route`, `rejected_over_size`,
`rejected_cell_permanent`, `rejected_retry_canary`,
`tempfail_configuration`, `tempfail_disabled`, `tempfail_directory`,
`tempfail_alias_gate`, `tempfail_canonical_gate`, `tempfail_suspended_route`,
`tempfail_route_lookup`, `tempfail_content`,
`tempfail_signing`, `tempfail_transport`, `tempfail_rate_limited`,
`tempfail_cell_response`, or `tempfail_internal`; and `phase` is one of
`configuration`, `recipient`, `directory`, `route`, `content`, `signing`,
`fetch`, `response`, or `internal`. Numeric fields are limited to count,
duration milliseconds, raw byte count, and response status. No address,
account, realm, agent, sender, subject, message identifier, route label,
digest, signature, token, content, or error text is recorded. Analytics
failure never changes the SMTP disposition. When the renderer-issued release
version, full commit, and commit date are all valid, they are appended as
deployment-attribution blobs. They are never sampling indexes or summary group
dimensions, and a missing or malformed triple is omitted in full.

`rejected_cell_capacity` is the receive Worker's sanitized permanent SMTP
response to an exact HTTP 507 `storage_full` cell verdict. Alert on it as a cell
capacity incident; do not reinterpret it as a sender fault, plan downgrade, or
retryable provider error. The unlabeled
`witself_agent_email_cell_storage_*` gauges expose schema 91's one-row ledger
without account or message identity. Alert if the collector is absent/down,
charged bytes or roots reach 80% of admission, or charged bytes/counts reach 80%
of hard reserve. Confirm an alert through the authenticated read-only ledger
query in the runbook before changing gates or capacity.

The v0.0.253/schema-91 rollout verified a point-in-time scrape with
`witself_agent_email_cell_storage_metrics_up 1` and all seven usage/threshold
gauges. The serving cell has since moved to continuous monitoring: at
v0.0.258/schema 93 it runs kube-prometheus-stack scraping with PVC metrics
collection, Alertmanager routing, and a canary-tested PagerDuty `witself-prod`
receiver plus dead-man heartbeat, with the logical-ledger and PVC capacity
alerts among its 14 alerting rules (alert canary and dead-man lapse/restore
proofs passed 2026-08-26). Database triggers enforce the
logical boundary independently.
Any additional accepting cell must meet the same continuous logical-ledger and
physical-PVC alerting bar before joining the email cohort.

The ledger is logical: deletion releases its charge transactionally but does
not make PostgreSQL relation files or a persistent volume shrink. Monitor PVC
available bytes and database/relation growth independently, alert before 80%
physical utilization, and keep backup/restore evidence current. A healthy
logical ledger is not proof of physical disk headroom, and physical file size
must never replace the transactionally enforced admission decision.

The same dataset carries a second fixed schema marker,
`witself.agent-email.route-lookup.v1`, for dependency shielding and route-cache
convergence. Its `result` is one of `kv_fresh`, `legacy`, `cp_found`,
`cp_not_found`, `miss_suppressed`, `cold_limited`, `known_limited`, `kv_error`,
or `cp_error`; `evidence` is `none`, `known`, or `uncertain`; and `route_kind`
is `canonical`, `alias`, `custom_domain`, `pilot`, or `unknown`. Numeric fields
are limited to count, duration milliseconds, and response status. The same
optional release-attribution triple is appended only as metadata, after the
fixed route fields. The event never contains an address, domain, realm label,
account, realm, agent, route digest, limiter key, or error text. Each recipient
lookup emits exactly one terminal route event. If a failed or corrupt KV read
continues to the control plane,
`evidence=uncertain` carries that context on the terminal control-plane result;
there is no second early `kv_error` event. Strict fixed in-isolate windows admit
at most 10 cold and 100 known-or-uncertain leader lookups per 10 seconds before
the fixed-key, per-location Cloudflare shield. Singleflight followers consume
no additional admission. Cloudflare's shared rate counters are permissive and
eventually consistent, so neither layer is exact account-level accounting or a
customer quota.

The outbound adapter's lifecycle Queue consumer emits exactly one privacy-safe
`witself.agent-email-provider-event-consume-log.v1` record per attempt. Its only
fields are the fixed schema/component, a closed outcome, and disposition (`ack`
or `retry`); it never
logs provider event ids, accounts, cells, recipients, provider message ids, or
error text. Exhausted retries move the original Queue item to the configured
DLQ for operator inspection. See [cell-worker.md](cell-worker.md) for worker
metrics and the
[sending-adapter README](../infra/cloudflare/agent-email-send/README.md) for the
Queue/DLQ boundary.

The deployed hardened outbound adapter enables Cloudflare Workers Observability
and a Rate Limiter binding. Its request order is source-IP lane,
Ed25519 header verification, bounded 2-MiB body/digest and account authorization,
then aggregate and signer lanes. Namespace `2301` is configured for 1,000
requests per 60 seconds with version preview URLs disabled
(`preview_urls=false`). Treat sustained 429
`frontdoor_rate_limited` or 503 `frontdoor_unavailable` responses as edge
capacity/configuration signals. Cloudflare counters are point-of-presence-local
and eventually consistent, so their absence is not proof of a global exact
budget. Verify account-wide namespace uniqueness before deployment and keep
request/signature/source values out of logs. The v0.0.253 deployment completed
the account-wide inventory and proved namespace `2301` was unused by every
other Worker before binding it; repeat that audit before future changes.

From `infra/cloudflare/agent-email`, `npm run metrics -- summary [minutes]`
queries both schemas for the same bounded window. The additive v2 CLI envelope
keeps the final-verdict response in `result` and returns the value-free route
breakdown in `route_lookup_result`, grouped only by `result`, `evidence`, and
`route_kind`. This makes `cp_error` on `custom_domain` routes directly visible
without exposing a customer domain or tenant identifier.
Metric names can evolve during implementation, but the coverage categories
should remain. The sealed-plane families (`witself_secret_operations_total`,
`witself_secret_reveals_total`, `witself_totp_operations_total`,
`witself_kms_operations_total`, and `witself_kms_operation_duration_seconds`)
count events and never carry payload: no secret value, field value, TOTP seed,
generated code, or key material ever appears in a metric or its labels. They
are present only when the sealed plane is enabled. The `server_side_decrypt`
label on the reveal and TOTP families records which decrypt path served the
value — `true` for token-only pods where the server mediates decryption,
`false` for client-held decryption — per the hybrid model in
[key-hierarchy.md](key-hierarchy.md).

The cross-realm collaboration families
(`witself_conversations_total`, `witself_relay_envelopes_total`,
`witself_relay_envelope_duration_seconds`, `witself_loop_suspended_total`,
`witself_budget_exhausted_total`, `witself_federation_decisions_total`) and the
multi-cell families (`witself_cell_placements_total`,
`witself_cell_migrations_total`, `witself_cell_migration_duration_seconds`)
follow the same discipline: they count events and carry no envelope body,
participant handle, conversation id, peer realm handle, signing key, card,
realm/account id, or cell id. They mirror the conversation, federation, loop,
budget, and cell audit events registered in
[audit-retention.md](audit-retention.md), and their counts and latencies are
the operational face of the collaboration substrate in
[agent-collaboration.md](agent-collaboration.md) and the cell fleet in
[deployment-cells.md](deployment-cells.md). The relay and cell families exist
only where those surfaces are deployed: relay/federation/conversation metrics on
realms that participate in cross-realm collaboration, and placement/migration
metrics on the thin global control plane that owns those decisions (a separate
surface from any per-cell `/v1` route).

## Label And Privacy Rules

Metrics are operational metadata, not an escape hatch around the security
model. They must never expose identity material, secret material, or
high-cardinality customer metadata. Witself protects the integrity and
authenticity of open-plane identity data and the confidentiality of
sealed-plane credential data, so the content of memories, facts, and messages
and the values of secrets and TOTP material are equally off-limits in
telemetry.

Forbidden metric labels and values include:

- Memory content, memory titles, fact names that carry user data, fact values,
  tags, descriptions, sources, links, or arbitrary user input.
- Message subjects, message bodies, or structured message payloads.
- Cross-realm envelope bodies, conversation ids, participant agent handles, peer
  realm handles, realm signing keys, realm cards, or per-conversation spend
  amounts that identify a tenant.
- Realm/account ids, cell ids, cell endpoints, or home-cell routing data on the
  placement and migration families.
- Embedding vectors or any vector component.
- Secret names, secret field names, field values, secret tags, descriptions,
  URLs, or account labels.
- TOTP seeds, generated TOTP codes, or recovery codes.
- KMS key material, data keys, ciphertext blobs, private keys, or passphrases.
- Token IDs, raw tokens, or token prefixes.
- Email addresses, customer names, support ticket text, invoice IDs, payment
  method IDs, wallet addresses, database URLs, client model credentials,
  object-store credentials, or provider secrets.
- Raw HTTP paths, query strings, request bodies, user agents, IP addresses, or
  error messages.
- Remembered, ingested, or digest-rendered content; raw ingest file paths or
  source labels; per-record memory or fact ids in consolidation, ingest, or
  remember metrics. Routing and outcome are recorded only as the normalized
  `routed_kind`, `action`, and `outcome` labels.

Allowed labels should be low cardinality and pre-normalized, such as:

- `route` as a route template, such as `/v1/memories/{memory_id}:recall`.
- `method`.
- `status_class`, such as `2xx`, `4xx`, or `5xx`.
- `operation`, such as `add`, `adjust`, `recall`, `set`, `forget`, `delete`,
  and the sealed-plane operations `create`, `show`, `reveal`, `rename`, `copy`,
  `archive`, `restore`, `grant`, `revoke`, `enroll`, and `code`.
- `result`, such as `success`, `error`, `denied`, `rate_limited`, or
  `unsupported`.
- `decision`, such as `allow` or `deny`, for policy evaluations.
- `permission`, the policy verb: `read`, `contribute`, `curate`, or `forget`.
- `scope`, such as `memory`, `fact`, or `both`.
- `mode`, such as `lexical` or `hybrid`, for recall, and `preview`, `apply`, or
  `rollback` for curation.
- `routed_kind`, such as `fact` or `memory`, reserved for a future explicit
  Witself `remember` action; it never carries the captured text.
- `phase`, such as `start` or `end`, for session lifecycle operations.
- `action`, such as `create`, `replace`, `supersede`, `relate`, or
  `propose_fact`, for client-authored curation plans.
- `outcome`, such as `fact_added`, `memory_added`, or `duplicate_skipped`, for
  ingest records.
- `elided`, `true` or `false`, for self-digest renders.
- `stage`, such as `sent`, `delivered`, or `read`, for messaging.
- `conversation_state`, the A2A-style state of a cross-realm conversation/task,
  exactly one of `submitted`, `working`, `input_required`, `auth_required`,
  `completed`, `failed`, or `canceled`. It never carries the `conversation_id`.
- `direction`, `inbound` or `outbound`, for relay throughput.
- `relay_action`, `routed`, `dropped`, or `quarantined`, for relay envelopes. It
  never carries a peer realm handle or envelope content.
- `suspend_reason`, a small normalized set — `turn_budget`, `hop_limit`,
  `ttl_expired`, `repeat_hash`, or `flood` — for loop suspensions.
- `budget_kind`, `turn` or `cost`, and `enforcement`, `warn` or `fail`, for
  budget-exhaustion events. Neither carries a spend amount or conversation id.
- `federation_stage`, `peer_check` or `consent`, for federation decisions. It
  pairs with `decision` (`allow`/`deny`) and never carries a peer realm handle,
  key, or card.
- `placement_reason`, a small normalized set — `residency`, `capacity`, `wave`,
  or `manual` — for tenant placement. It never carries a realm/account id or a
  cell id.
- `migration_phase`, `started`, `completed`, or `failed`, and `plane`, `open` or
  `sealed`, for tenant migration. Neither carries a realm/account id or a
  source/destination cell id.
- `principal_kind`, such as `agent`, `operator`, `admin`, or `service`.
- `owner_kind`. On open-plane access metrics it records the access perspective
  as `self`, `other_agent`, or `group`. On sealed-plane metrics (and anywhere
  it records data ownership rather than access perspective) it records the
  owning principal kind as exactly `agent` or `group` — the unified ownership
  model for memories, facts, and secrets. In every case it must never carry an
  agent name, group name, or realm id.
- `recipient_kind`, such as `agent` or `group`.
- `vector_coverage`, one of `full`, `partial`, or `none`; it never carries a
  profile id, model name, dimensions, or vector value.
- `backend_kind`, such as `managed`, `self_hosted`, or `local`.
- `store_backend`, `object_store_provider`.
- `kms_provider`, the KMS provider family for sealed-plane operations, such as
  `aws_kms`, `gcp_kms`, `azure_key_vault`, or `local_dev`. It must never carry a
  key id, key ARN, endpoint URL, or key material.
- `server_side_decrypt`, `true` or `false`, recording which decrypt path served
  a reveal or TOTP code: `true` when the server mediates decryption for a
  token-only pod, `false` for client-held decryption. It never carries a key,
  a value, or any plaintext.
- `reason_class`, a small normalized set such as `missing`, `stale`,
  `incompatible`, `non_finite`, `wrong_dimension`, or `unauthorized`, for
  optional-vector validation and fallback events.
- `limit_dimension`, using the canonical metered-dimension names from
  [billing-and-limits.md](billing-and-limits.md): `active_agent`,
  `stored_memory`, `stored_fact`, `memory_recall`, `memory_write`,
  `vector_write`, `vector_storage_byte`, `crossagent_access`,
  `security_group`, `message_sent`, `message_delivered`, `audit_event`,
  `api_request`, and the sealed-plane dimensions `stored_secret`,
  `secret_read`, `totp_code`, `runtime_injection`, or
  `encrypted_storage_byte`.

The `owner_kind` label is the load-bearing dimension for distinguishing
self-access from cross-agent and group access on the open plane, and for
distinguishing agent-owned from group-owned data across both planes. It must be
normalized to exactly `self`, `other_agent`, or `group` when recording access
perspective, or to exactly `agent` or `group` when recording data ownership; it
must never carry an agent name, group name, or realm id.

There is no backend model-provider label. Model and recipe identity live in
immutable vector profiles and are too high-cardinality for metrics. Vector
dimensions, profile ids, query text, and vector values must not become labels.

Route metrics must use route templates rather than raw request paths.

## Structured Logs

Server logs should be structured JSON in production-shaped deployments.

Expected log fields:

- Timestamp.
- Level.
- Message.
- Request ID or trace ID when available.
- Route template.
- HTTP method.
- Status code.
- Duration.
- Principal kind when authenticated.
- Owner kind (`self`, `other_agent`, or `group`) for identity operations.
- Permission verb and decision for policy-gated operations.
- Recall mode and bounded vector coverage class for recall operations.
- KMS provider, KMS operation, and the `server_side_decrypt` flag for
  sealed-plane reveal, TOTP code, and key operations.
- Backend kind.
- Stable error code when an operation fails.

Logs may carry non-sensitive correlation context such as record ids
(`mem_…`, `fact_…`, `grp_…`, `pol_…`, `msg_…`, and the sealed-plane ids
`sec_…`, `fld_…`, `grt_…`, `totp_…`, `kek_…`, `dek_…`, `att_…`), memory kind,
recipient kind, policy id, and decision outcome. They must follow the same
redaction rules as API errors, CLI output, audit events, and support data.

Forbidden log fields mirror the audit and metric rules: no memory content, no
fact values, no message bodies or payloads, no embedding vectors, no secret
values, no secret or field names that carry user data, no TOTP seeds or
generated codes, no KMS key material or data keys, no private keys or
passphrases, no raw tokens, no PII (email addresses, customer names, wallet
addresses, raw payment details), no database URLs, no provider credentials, and
no raw request paths, query strings, request bodies, or arbitrary user input.
Sensitive request bodies and `sensitive`-marked records must be redacted before
logging. Sealed-plane values are never logged at all, even at debug level.

## Helm Chart Integration

The `charts/witself-server` chart should expose observability and operational controls
through values.

Illustrative values. [helm-chart.md](helm-chart.md) holds the authoritative
chart values schema; this block must stay consistent with it:

```yaml
server:
  listen: ":8080"

health:
  listen: ":8081"
  port: 8081
  service:
    enabled: false
    annotations: {}

metrics:
  enabled: true
  listen: ":9090"
  path: /metrics
  port: 9090
  service:
    enabled: true
    annotations: {}
  serviceMonitor:
    enabled: false
    interval: 30s
    scrapeTimeout: 10s
    labels: {}
    relabelings: []
    metricRelabelings: []
  podMonitor:
    enabled: false
    interval: 30s
    scrapeTimeout: 10s
    labels: {}

probes:
  liveness:
    enabled: true
    path: /livez
    port: health
    initialDelaySeconds: 10
    periodSeconds: 10
    timeoutSeconds: 2
    failureThreshold: 3
  readiness:
    enabled: true
    path: /readyz
    port: health
    initialDelaySeconds: 5
    periodSeconds: 10
    timeoutSeconds: 2
    failureThreshold: 3
  startup:
    enabled: true
    path: /startupz
    port: health
    periodSeconds: 5
    timeoutSeconds: 2
    failureThreshold: 30

resources:
  requests:
    cpu: 100m
    memory: 128Mi
  limits:
    memory: 512Mi

autoscaling:
  enabled: false
  minReplicas: 2
  maxReplicas: 10
  targetCPUUtilizationPercentage: 70

podDisruptionBudget:
  enabled: true
  minAvailable: 1
```

The chart should support Prometheus Operator `ServiceMonitor` and `PodMonitor`
resources when those CRDs are installed and the corresponding values are
enabled. The worker has a separate metrics Service and separate optional
monitors so API and worker scrape selectors never overlap. The chart should not
require those CRDs for a basic install.

## Alerts And Dashboards

The repository now contains a default-off Founder/open-plane monitoring
capability in the GitOps platform chart. It pins a trimmed
`kube-prometheus-stack` package, disables Grafana and upstream default rules,
and enables only Prometheus Operator, one bounded Prometheus, one bounded
Alertmanager, kubelet/PVC collection, and kube-state-metrics. The application
chart can label the existing server and worker ServiceMonitors for exact
selection and restrict both metrics ports to the `monitoring` namespace.

The receiver contract accepts only the name and key of an immutable Kubernetes
Secret. Alertmanager mounts that Secret and reads the value from the mounted
file, so it never enters Git, Helm values, Argo Application state, or the
retained canary artifact. Two receivers are configured independently:

- The **incident receiver** is selected by `platform.monitoring.receiver.kind`.
  `pagerduty` configures Alertmanager's native PagerDuty Events API v2 client
  and reads only the integration (routing) key through `routing_key_file`, so
  firing maps to trigger, resolved maps to resolve, and the alert `severity`
  label is carried through. The default `webhook` keeps the provider-neutral
  HTTPS receiver, reading a full URL through `url_file`.
- The **dead-man receiver** (`platform.monitoring.receiverDeadman`) is a
  deliberately provider-agnostic heartbeat webhook. The always-firing
  `WitselfWatchdog` rule routes there and nowhere else, and carries no
  `witself_alert` label so it never opens an incident. An outside monitor pages
  when the heartbeat stops, which is the only signal that survives losing the
  alerting plane itself. Omitting its Secret omits the route, receiver, and
  mount.

The Witself product rules aggregate away pod, instance, route, account, realm,
and agent identity. They expose only fixed service/severity labels and the
worker's existing closed-set job label. PostgreSQL rules additionally retain
the namespace, scrape job, and instance needed to identify the failing exporter
target, while removing database, user, application, and wait-event labels.

This capability is now live on the serving cell. Shared chart defaults remain
disabled, and the staged GitOps rollout — stack, then targets, then alerting —
proved sustained scrapes, bounded storage and resource headroom, rule health,
plus both firing and resolved delivery at the tested external PagerDuty
receiver, accepted 2026-08-26.
The target-cell ServiceMonitors must be enabled only after the monitoring child
Application and CRDs are Healthy; Argo sync waves in separate parent
Applications do not establish that ordering.

<a id="postgresql-alerts"></a>

Deployment-hardening batch B adds the following database rules in
[`postgresql.rules.yaml`](../.gitops/charts/platform/files/postgresql.rules.yaml)
for `civo-sandbox-usw2-dev`. They require monitoring, alerting, the default-off
`platform.monitoring.postgresql.enabled` switch, and an enabled Civo PostgreSQL
exporter. They select the `witself-postgresql-metrics` service in the `witself`
namespace. Their fixed `service: witself-postgresql`, severity, and
`witself_alert: "true"` labels use the existing PagerDuty incident route; the
PrometheusRule carries `release: witself-monitoring` for discovery.

| Alert | Condition | Severity |
| --- | --- | --- |
| `WitselfPostgreSQLConnectionsHigh` | Sum of [`pg_stat_activity_count`](https://github.com/prometheus-community/postgres_exporter/blob/v0.20.1/collector/pg_stat_activity.go#L37-L57) across connection states, divided by [`pg_settings_max_connections`](https://github.com/prometheus-community/postgres_exporter/blob/v0.20.1/collector/pg_setting.go#L90-L118), exceeds 0.8 for 10 minutes. | warning |
| `WitselfPostgreSQLTransactionAgeHigh` | Oldest transaction age, [`pg_stat_activity_max_tx_duration`](https://github.com/prometheus-community/postgres_exporter/blob/v0.20.1/collector/pg_stat_activity.go#L37-L57), exceeds 300 seconds for 5 minutes. | warning |
| `WitselfPostgreSQLDeadlocks` | [`pg_stat_database_deadlocks`](https://github.com/prometheus-community/postgres_exporter/blob/v0.20.1/collector/pg_stat_database.go#L172-L181) increases over 10 minutes. | warning |
| `WitselfPostgreSQLExporterUnavailable` | [`pg_up`](https://github.com/prometheus-community/postgres_exporter/blob/v0.20.1/exporter/postgres_exporter.go#L474-L479) is absent or zero for 10 minutes. | critical |
| `WitselfPostgreSQLDown` | [`pg_up`](https://github.com/prometheus-community/postgres_exporter/blob/v0.20.1/exporter/postgres_exporter.go#L474-L479) is zero for 5 minutes. | critical |

Connection counts and limits are matched per scrape target before comparison.
The two critical rules deliberately
overlap for a database that remains down for 10 minutes; only exporter
unavailability covers a missing series. The metric links above identify the
upstream postgres_exporter 0.20.1 definitions declared by Bitnami PostgreSQL chart
18.8.0. Its exporter image tag is mutable; verify the resolved version and all
five metric families in a live scrape during activation. Batch B prepares these
rules; serving-cell scrape and alert delivery acceptance follows deployment.

Initial alert candidates:

- High 5xx rate.
- Readiness failures.
- Audit write failures.
- PostgreSQL full-text search failures or latency above a baseline.
- Optional vector validation failures or sustained low coverage above a
  baseline. These indicate client/profile drift, not backend provider health.
- Curation request backlog, lease expiry, fencing conflict, or failed plan
  application above a baseline.
- Storage operation failures.
- KMS operation failures (sealed plane).
- Secret reveal spikes (possible credential-exfiltration signal).
- TOTP code generation spikes.
- Server-side-decrypt reveal rate above a baseline (token-only pods serving
  values; expands the decrypt trust boundary).
- Cross-agent access denials above a baseline (possible policy or abuse signal).
- Cross-agent curate/forget spikes (possible memory-poisoning or write abuse).
- Sustained active-memory capacity refusals by bounded operation (capacity or
  consolidation-health signal; investigate through authenticated status rather
  than adding tenant labels).
- Message send or delivery failure rate above a baseline.
- Sustained inbound-email edge `tempfail_route_lookup` or
  `tempfail_suspended_route` outcomes (control-plane/directory convergence or
  managed-route lifecycle problems). Keep alerts aggregate and do not add a
  domain, realm label, account, or recipient dimension.
- Sustained inbound-email route-lookup `cold_limited`, `known_limited`,
  `miss_suppressed`, `kv_error`, or `cp_error` results. These distinguish
  probing pressure from directory/control-plane convergence without exposing
  the probed domain or label; alerts remain aggregate because the protective
  limiter is approximate and per location.
- Any inbound-email `rejected_cell_capacity` edge outcome or cell ingest
  `storage_full` outcome; absent/down
  `witself_agent_email_cell_storage_metrics_up`; logical email-ledger bytes or
  roots at 80% of admission; or logical bytes/counts at 80% of the hard
  boundary. The 3-GiB/25,000-root admission boundary is the operating ceiling;
  the 4-GiB/100,000-row hard boundary is emergency lifecycle reserve, not a
  normal target. Alert separately when the PostgreSQL PVC or database reaches
  80% physical utilization because logical deletion does not shrink files.
- Sustained outbound-dispatch `frontdoor_rate_limited` or
  `frontdoor_unavailable` responses from the live hardened adapter.
  Distinguish hostile-source pressure from a missing/unavailable Rate Limiter
  binding without adding source IP, signer, account, or request labels.
- Relay envelope drop or quarantine rate above a baseline (cross-realm routing or
  flood signal).
- Loop-suspension or budget-exhaustion spikes (possible cross-realm loop, flood,
  or runaway auto-reply).
- Federation deny rate above a baseline (possible misconfigured trust or probing).
- Tenant migration failures (`migration_phase="failed"`).
- Sustained rate limiting or limit blocking.
- Pending migrations.
- Token authentication failures above a baseline.

Alerting rules should avoid customer-specific labels and must not include memory
content, fact values, message bodies, embedding vectors, fact names, secret
names, field names, TOTP material, KMS key material, raw paths, user input, or
payment details.

These email alerts are now live operating controls on the serving cell. The
default-off, resource-bounded Prometheus/Alertmanager capability was enabled
there by the staged 3-phase GitOps rollout — null-routed stack first,
application targets second, rules plus external receiver third — so expected
pre-activation absence never created false production evidence. Continuous
scraping, PVC metrics collection, Alertmanager routing, and the canary-tested
PagerDuty `witself-prod` receiver with a dead-man heartbeat were accepted
2026-08-26, with both Civo cells running v0.0.258 at schema 93.

The first rollout is intentionally limited to the cell-local open-plane and
Founder email storage path. Cloudflare email edge/Queue outcomes live in
Analytics Engine rather than Prometheus and require a separate bounded bridge
or alerting path. One monitored cell also does not close fleet-wide placement,
restore, or capacity gates for any unmonitored accepting cell.

## CI And Release Checks

Required checks once the server and chart exist:

- Unit tests for metric registration and label normalization.
- Tests proving raw paths and user input do not become metric labels.
- Tests proving `owner_kind` only ever takes `self`, `other_agent`, or `group`
  for access-perspective metrics, and only `agent` or `group` for
  data-ownership metrics, and never an agent name, group name, or realm id.
- Tests proving `server_side_decrypt` only ever takes `true` or `false`.
- Tests proving memory content, fact values, message bodies, and embedding
  vectors never appear in metrics, logs, or health responses.
- Tests proving active-memory refusal labels are restricted to
  `limit_dimension="stored_memory"` and the five closed operation values, with
  no tenant ids, usage values, maximums, memory ids, or plan content.
- Tests proving current-fact refusal labels are restricted to
  `limit_dimension="stored_fact"` and operation `create` or `confirm`, with no
  tenant ids, subjects, predicates, fact/candidate ids, usage values, maximums,
  values, or error text.
- Tests proving secret values, secret/field names, TOTP seeds, generated TOTP
  codes, KMS key material, data keys, and private keys never appear in metrics,
  logs, or health responses.
- Tests proving the `kms_provider` label carries only the provider family and
  never a key id, ARN, endpoint, or key material.
- Tests proving no backend model-provider label/config/health path is
  registered, and vector metrics never carry a profile id, model name,
  dimensions, query text, endpoint, credential, or vector value.
- Health endpoint tests for live, ready, and startup behavior.
- Readiness tests proving lexical recall requires PostgreSQL FTS but does not
  require pgvector or model connectivity; migration-0032 table failures gate
  only vector operations and are reflected in capabilities/metrics.
- Server smoke test that `/metrics` returns Prometheus text format.
- Server smoke test that API, health, and metrics listeners bind separately.
- Server smoke test that metrics can be disabled and the metrics listener is
  not started.
- Helm template tests for probes and metrics values.
- Helm template tests for ServiceMonitor and PodMonitor enabled and disabled
  paths.
- Default-off platform render tests, exact child-chart package checksum
  verification, Prometheus rule syntax/unit tests, receiver Secret validation,
  private-service checks, and monitoring-only metrics ingress checks.
- A synthetic `PrometheusRule` canary that observes local firing and resolution
  without causing an outage. Retain separate external receiver receipts for
  both states; local Alertmanager evidence alone is insufficient.
- Kubernetes schema validation for rendered probe, service, and monitor
  resources.
- Release smoke tests for `witself-server healthcheck --live`,
  `witself-server healthcheck --ready`, and the published backend image.

## Related Docs

- [requirements.md](requirements.md)
- [backend-architecture.md](backend-architecture.md)
- [server-command-surface.md](server-command-surface.md)
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
- [api-contract.md](api-contract.md)
- [api-routes.md](api-routes.md)
- [storage.md](storage.md)
- [audit-retention.md](audit-retention.md)
- [billing-and-limits.md](billing-and-limits.md)
- [helm-chart.md](helm-chart.md)
- [self-hosting.md](self-hosting.md)
- [release-and-build.md](release-and-build.md)
- [implementation-plan.md](implementation-plan.md)
- [threat-model.md](threat-model.md)
