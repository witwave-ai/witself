# Witself Observability And Operations

Status: draft. This document defines the observability and Kubernetes
operations requirements for `witself-server`.

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
is instrumented for memory operations, semantic recall, embedding calls, fact
operations, policy decisions, cross-agent access, groups, and inter-agent
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
or payloads, embedding vectors, secret values, TOTP seeds, generated TOTP codes,
raw tokens, database URLs, embedding-provider credentials, object-store
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

Liveness should be conservative. A transient database, object-store,
embedding-provider, or KMS failure should normally make readiness fail, not
force Kubernetes to restart a healthy process.

Readiness should fail when:

- Required storage is unavailable.
- Required migrations are missing or incompatible.
- The pgvector extension required for semantic recall is unavailable when vector
  storage is required.
- Required KMS operations cannot complete when the sealed plane is enabled.
  KMS is a hard readiness gate only for sealed-plane deployments; an
  open-plane-only deployment does not depend on KMS (see
  [storage.md](storage.md) and [key-hierarchy.md](key-hierarchy.md)).
- The server is intentionally in a mode that should not accept ordinary
  traffic.
- The server is draining or shutting down.

The embedding provider is treated as a degradable dependency, not a hard
readiness gate. When the configured provider (`voyage`, `openai`, or
`local-dev`) is unavailable, recall degrades to keyword/tag/kind/time ranking
and the capabilities contract reports the degraded state (see
[memory-model.md](memory-model.md)). Embedding-provider unavailability should be
surfaced through metrics and the capability contract, and should not by itself
fail readiness unless the deployment explicitly requires semantic recall.

Startup should cover slow boot paths such as config validation, first database
connection, migration status checks, pgvector availability,
embedding-provider client initialization, and KMS provider client
initialization when the sealed plane is enabled.

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
| `witself_secret_reveals_total` | Sealed-plane value-returning reveals (`secret reveal` and reference resolution that returns a value) by principal kind, owner kind, `server_side_decrypt` (`true`, `false`), and result. These are the audited reveal-ceremony events; the metric counts events only and never carries the revealed value. |
| `witself_totp_operations_total` | TOTP operations by operation (`enroll`, `code`, `show`, `delete`), owner kind, `server_side_decrypt` (`true`, `false`), and result. The `code` operation is value-returning and audited; the metric never carries the generated code or the seed. |
| `witself_kms_operations_total` | KMS envelope operations by provider, operation (`generate_data_key`, `encrypt`, `decrypt`, `rotate`), and result. Present only when the sealed plane is enabled. |
| `witself_kms_operation_duration_seconds` | KMS operation latency histogram by provider and operation. Present only when the sealed plane is enabled. |
| `witself_memory_operations_total` | Memory operations by operation (`add`, `adjust`, `read`, `list`, `forget`, `restore`, `delete`), owner kind, and result. |
| `witself_memory_recalls_total` | Recall requests by mode (`semantic`, `degraded`), owner kind, and result. |
| `witself_memory_recall_duration_seconds` | Recall latency histogram by mode and owner kind. |
| `witself_memory_recall_hits` | Histogram of result counts returned per recall, by mode. |
| `witself_embedding_operations_total` | Embedding operations by provider, operation (`embed`, `reembed`), and result. |
| `witself_embedding_operation_duration_seconds` | Embedding provider call latency histogram by provider. |
| `witself_embedding_failures_total` | Embedding provider failures by provider and reason class. |
| `witself_embedding_degrade_events_total` | Recall degrade-to-lexical events by provider and reason class. |
| `witself_fact_operations_total` | Fact operations by operation (`set`, `get`, `list`, `delete`, `primary_change`), owner kind, and result. |
| `witself_remember_total` | Deferred metric for a future explicit Witself `remember` action, by `routed_kind` (`fact`, `memory`), owner kind, and result. It never carries captured text. |
| `witself_self_digest_renders_total` | Self-digest (`self show` / `GET /v1/self`) renders by source surface, `elided` (`true`, `false`), and result. |
| `witself_self_digest_render_duration_seconds` | Self-digest render latency histogram by source surface. The digest path never calls the embedding provider. |
| `witself_self_digest_elided_entries` | Histogram of entries elided from a digest render when the byte/line cap is hit, by source surface. |
| `witself_memory_consolidations_total` | Memory consolidation runs by `mode` (`dry_run`, `apply`), owner kind, and result. |
| `witself_memory_consolidation_actions_total` | Consolidation outcomes by `action` (`merged`, `superseded`, `conflict`) and `mode` (`dry_run`, `apply`). Conflicts are surfaced, never auto-resolved. |
| `witself_session_operations_total` | Session lifecycle operations by `phase` (`start`, `end`), owner kind, and result. |
| `witself_ingest_operations_total` | Ingest runs (CLAUDE.md/AGENTS.md/GEMINI.md import) by `mode` (`dry_run`, `apply`) and result. Source labels are never raw file paths. |
| `witself_ingest_records_total` | Records produced by ingest by `outcome` (`fact_added`, `memory_added`, `duplicate_skipped`). |
| `witself_policy_decisions_total` | Policy evaluations by permission verb, decision (`allow`, `deny`), scope, and owner kind. |
| `witself_crossagent_accesses_total` | Cross-agent identity accesses by permission verb, scope, owner kind, and result. |
| `witself_group_operations_total` | Group operations by operation (`create`, `delete`, `member_add`, `member_remove`) and result. |
| `witself_messages_total` | Messaging events by stage (`sent`, `delivered`, `read`), recipient kind, and result. |
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
| `witself_vector_storage_bytes` | Approximate pgvector storage size when known. |
| `witself_object_store_operations_total` | Object/blob storage operations when configured. |
| `witself_migration_version` | Applied migration version. |
| `witself_migration_pending` | Pending migration count when known. |

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
  method IDs, wallet addresses, database URLs, embedding-provider credentials,
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
- `mode`, such as `semantic` or `degraded`, for recall, and `dry_run` or
  `apply` for consolidation and ingest.
- `routed_kind`, such as `fact` or `memory`, reserved for a future explicit
  Witself `remember` action; it never carries the captured text.
- `phase`, such as `start` or `end`, for session lifecycle operations.
- `action`, such as `merged`, `superseded`, or `conflict`, for consolidation
  outcomes.
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
- `embedding_provider`, such as `voyage`, `openai`, or `local_dev`.
- `backend_kind`, such as `managed`, `self_hosted`, or `local`.
- `store_backend`, `object_store_provider`.
- `kms_provider`, the KMS provider family for sealed-plane operations, such as
  `aws_kms`, `gcp_kms`, `azure_key_vault`, or `local_dev`. It must never carry a
  key id, key ARN, endpoint URL, or key material.
- `server_side_decrypt`, `true` or `false`, recording which decrypt path served
  a reveal or TOTP code: `true` when the server mediates decryption for a
  token-only pod, `false` for client-held decryption. It never carries a key,
  a value, or any plaintext.
- `reason_class`, a small normalized set such as `unavailable`, `timeout`,
  `rejected`, or `rate_limited`, for embedding and degrade events.
- `limit_dimension`, using the canonical metered-dimension names from
  [billing-and-limits.md](billing-and-limits.md): `active_agent`,
  `stored_memory`, `stored_fact`, `memory_recall`, `memory_write`,
  `embedding_operation`, `vector_storage_byte`, `crossagent_access`,
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

The `embedding_provider` label identifies the provider family only. It must
never carry the model name as free text, an API key, an endpoint URL, or any
provider credential. Vector dimensionality, when exposed, belongs in the
capabilities contract, not in metric labels.

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
- Embedding provider and recall mode for recall operations.
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
enabled. The chart should not require those CRDs for a basic install.

## Alerts And Dashboards

The repo should eventually include optional example Prometheus alert rules and
dashboard definitions for self-hosted operators.

Initial alert candidates:

- High 5xx rate.
- Readiness failures.
- Audit write failures.
- Embedding provider failure rate above a baseline.
- Sustained recall degrade-to-lexical events (semantic recall unavailable).
- Storage operation failures.
- KMS operation failures (sealed plane).
- Secret reveal spikes (possible credential-exfiltration signal).
- TOTP code generation spikes.
- Server-side-decrypt reveal rate above a baseline (token-only pods serving
  values; expands the decrypt trust boundary).
- Cross-agent access denials above a baseline (possible policy or abuse signal).
- Cross-agent curate/forget spikes (possible memory-poisoning or write abuse).
- Message send or delivery failure rate above a baseline.
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
- Tests proving secret values, secret/field names, TOTP seeds, generated TOTP
  codes, KMS key material, data keys, and private keys never appear in metrics,
  logs, or health responses.
- Tests proving the `kms_provider` label carries only the provider family and
  never a key id, ARN, endpoint, or key material.
- Tests proving the `embedding_provider` label carries only the provider family
  and never a model name, endpoint, or credential.
- Health endpoint tests for live, ready, and startup behavior.
- Readiness test proving embedding-provider unavailability degrades recall
  rather than failing readiness, and is reflected in metrics.
- Server smoke test that `/metrics` returns Prometheus text format.
- Server smoke test that API, health, and metrics listeners bind separately.
- Server smoke test that metrics can be disabled and the metrics listener is
  not started.
- Helm template tests for probes and metrics values.
- Helm template tests for ServiceMonitor and PodMonitor enabled and disabled
  paths.
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
