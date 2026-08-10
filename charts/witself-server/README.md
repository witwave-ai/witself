# witself Helm chart

Deploys [`witself-server`](https://github.com/witwave-ai/witself) and the
separately scalable `witself-worker` background runtime onto Kubernetes. Both
workloads use the same image. One chart serves both self-hosted and
cloud/managed deployments; the difference is values, not templates.

```sh
helm install witself oci://ghcr.io/witwave-ai/charts/witself-server \
  --version <version> \
  --namespace witself --create-namespace \
  --values ./my-values.yaml
```

## Scope

This chart tracks what the two runtimes consume today. `witself-server` owns the
API `:8080`, health `:8081`, and metrics `:9090` listeners, `backend.kind`, and
the API-only bootstrap, provisioning, and agent-email configuration.
`witself-worker` runs with the explicit command
`/usr/local/bin/witself-worker serve`, exposes health on `:8081` and metrics on
`:9090`, and receives only the shared Postgres DSN plus its background-job
configuration. Agent secrets use the client-custodied AVK design in
[ADR 0003](../../docs/decisions/0003-client-custodied-agent-vault.md): this
chart needs no sealed-plane feature flag, backend KMS setting, or decrypt-key
Secret. The chart does not render a migration Job; when a database DSN is
configured, each database-backed process applies its embedded forward Goose
migrations under the shared migration lock before becoming Ready. Nothing here
renders config either process would silently ignore.

## What it renders

| Resource | When |
|---|---|
| Server Deployment, Service (API), ServiceAccount, ConfigMap | always |
| Server Metrics Service | `metrics.enabled` and `metrics.service.enabled` (default on) |
| Server ServiceMonitor / PodMonitor | `metrics.serviceMonitor.enabled` / `metrics.podMonitor.enabled` |
| Worker Deployment and ConfigMap | `worker.enabled` |
| Worker Metrics Service | `worker.enabled` and `worker.metrics.service.enabled` |
| Worker ServiceMonitor / PodMonitor | `worker.metrics.serviceMonitor.enabled` / `worker.metrics.podMonitor.enabled` |
| Ingress | `ingress.enabled` |
| HorizontalPodAutoscaler | `autoscaling.enabled` |
| PodDisruptionBudget | `podDisruptionBudget.enabled` |
| Worker PodDisruptionBudget | `worker.enabled` and `worker.podDisruptionBudget.enabled` |
| Server / worker NetworkPolicy | `networkPolicy.enabled` / `worker.networkPolicy.enabled` |
| Helm test pod | `helm test` |

Set `database.existingSecret.name` and `database.existingSecret.urlKey` to expose
the referenced key as `WITSELF_DATABASE_URL` in the server container. A
non-empty `database.existingSecret.name` is required before
`worker.enabled: true`; the same Secret key is then exposed in the worker
container.

Set `bootstrap.existingSecret.name` to mount a first-operator bootstrap token at
`bootstrap.tokenFile` (default `/.witself/tokens/bootstrap.token`) and expose
the configured TTL as `WITSELF_BOOTSTRAP_TOKEN_TTL`.

Permanent fact deletion is disabled by default. `features.factDeletion.enabled`
renders `WITSELF_FACT_DELETION_ENABLED`; a server compiled against store schema
27 or older refuses to start when it is enabled, so turn it on only with schema
28 or newer.

The receive-only agent-email pilot is disabled by default. Enabling
`agentEmail.receivePilot.enabled` requires one primary domain, audience and
realm ID, exactly 5-10 unique canonical agent IDs, one or more relay public keys
encoded in `relayPublicKeysJSON`, and a replay window. One
`acceptedLegacyDomains` entry may be configured for previously issued canonical
local parts; the primary domain cannot appear in that list. New addresses and
aliases are never minted on a legacy domain. The chart then renders these seven
base server variables:

- `WITSELF_AGENT_EMAIL_RECEIVE_PILOT_ENABLED`
- `WITSELF_AGENT_EMAIL_PILOT_DOMAIN`
- `WITSELF_AGENT_EMAIL_PILOT_AUDIENCE`
- `WITSELF_AGENT_EMAIL_PILOT_REALM_ID`
- `WITSELF_AGENT_EMAIL_PILOT_AGENT_IDS`
- `WITSELF_AGENT_EMAIL_RELAY_PUBLIC_KEYS_JSON`
- `WITSELF_AGENT_EMAIL_RELAY_REPLAY_WINDOW`

A non-empty `agentEmail.receivePilot.acceptedLegacyDomains` list renders the
comma-separated `WITSELF_AGENT_EMAIL_ACCEPTED_LEGACY_DOMAINS` variable. The
managed app-of-apps withholds that value from charts older than `0.0.232`; for
the cutover configuration it preserves the first legacy domain as the old
single-domain runtime's domain until the child chart and image advance
together.

An optional `agentEmail.receivePilot.retryCanaryAgentID` must equal one of the
enrolled agent IDs and renders `WITSELF_AGENT_EMAIL_RETRY_CANARY_AGENT_ID`.
Keep it empty until every server pod is schema-61-capable; an older pod would
ordinary-accept the synthetic first delivery instead of deliberately returning
a temporary result.

Use a two-phase rollout: first deploy schema-61-capable code with
`retryCanaryAgentID` empty and wait for every pod to converge; then set the
exact enrolled agent in a config-only rollout and wait for convergence again.
Keep canary automation manual-only until a manual run succeeds. For rollback,
turn off any recurring schedule that has been added and settle the unused arm
or let its 15-minute TTL expire before clearing this value or downgrading code.
A future 15-minute cadence would create about 96 acknowledged synthetic
messages per day; ordinary account message-retention policy now governs their
eventual whole-thread cleanup.

The Ed25519 relay private key is not a chart value, Secret reference, or server
environment variable. It remains exclusively in the isolated Cloudflare Email
Worker secret. Changing any pilot value changes the ConfigMap checksum and
restarts the server pods for fail-closed startup reconciliation.

Production receive is a separate, mutually exclusive, default-off gate under
`agentEmail.receiveProduction`. It requires the canonical primary domain, the
destination-cell audience, relay public keys, and a strictly sorted list of
1-100 unique canonical `acc_*` IDs. There is no wildcard or implicit
all-accounts mode. An optional retry canary must be one canonical `agent_*` ID;
the cell verifies that it belongs to the exact configured cohort. Enabling the
gate renders `WITSELF_AGENT_EMAIL_RECEIVE_PRODUCTION_ENABLED=true`,
`WITSELF_AGENT_EMAIL_RECEIVE_DOMAIN`, `WITSELF_AGENT_EMAIL_RECEIVE_AUDIENCE`,
and the comma-separated `WITSELF_AGENT_EMAIL_RECEIVE_ACCOUNT_IDS` in API pods
only. The app-of-apps refuses to forward this shape unless both child chart and
image are `0.0.241` or newer.

API startup performs only bounded account/canary validation. It neither scans
all agents nor provisions mailboxes, so scaling API replicas cannot multiply a
Founder-account backfill. After the production gate is healthy and before any
edge delivery activation, an operator runs exactly one idempotent
`witself-server agent-email backfill` process. New agents in the configured
cohort are thereafter created atomically with their canonical mailbox. Generate
the edge canary only with
`witself-server agent-email canary-manifest --output /absolute/new/path.json`;
the command requires zero missing mailboxes and creates the exact 5-10-entry
manifest as a new mode-0600 file. Never commit that private mapping.

Large-realm avatar style propagation belongs only to the general-purpose
worker. The `worker.avatarStyleRollout` values render
`WITSELF_AVATAR_STYLE_ROLLOUT_ENABLED`,
`WITSELF_AVATAR_STYLE_ROLLOUT_BATCH_SIZE` (1-1000),
`WITSELF_AVATAR_STYLE_ROLLOUT_INTERVAL` (100ms-1h), and
`WITSELF_AVATAR_STYLE_ROLLOUT_BATCH_TIMEOUT` (100ms-5m) in the worker ConfigMap.
Every worker replica may process jobs; PostgreSQL job locking provides the
shared fence. The server ConfigMap always renders this job's enabled gate as
`false`, including during mixed-version rolling overlap.

Ephemeral message-rate coordination rows are maintained by the same worker.
`worker.messageRateBucketCleanup` renders the enabled gate, bounded batch size,
interval, and timeout as `WITSELF_MESSAGE_RATE_BUCKET_CLEANUP_*`. It defaults
to enabled with a 10,000-row batch, one-minute interval, and ten-second
deadline. Every replica may run it: PostgreSQL `FOR UPDATE SKIP LOCKED` divides
stale rows, and the database-clock cutoff preserves a full idle minute before
deletion. API pods never schedule this cleanup.

Inbound-email rate coordination uses a separate cleanup lane.
`worker.agentEmailRateBucketCleanup` renders the enabled gate, bounded batch
size, interval, and timeout as
`WITSELF_AGENT_EMAIL_RATE_BUCKET_CLEANUP_*`. It has the same enabled-by-default
10,000-row, one-minute, ten-second worker bounds. Each scheduled sweep drains
consecutive full batches until it catches up or reaches that deadline. Every
replica may run it because the email-specific delete batch also uses PostgreSQL
`FOR UPDATE SKIP LOCKED`; API pods never schedule it.

Avatar payload compaction is disabled by default.
`avatar.payloadCompaction.enabled` renders
`WITSELF_AVATAR_PAYLOAD_COMPACTION_ENABLED`. Keep it false while rolling out a
schema-54 renderer-profile-compatible binary. After every old writer has
drained, enable it in a separate values change. The ConfigMap checksum restarts every pod, and
each restarted server reruns the bounded nullable-digest backfill before it
serves or performs irreversible cleanup. Freeze all avatar mutations and
avatar-bearing import/export during the brief old/new-writer convergence
window; compatibility is data-safe, but the freeze avoids new legacy active
rows that need later operator replacement.

Transcript retention is disabled by default.
`worker.transcriptRetention` renders the enabled gate, `preview`/`enforce`
mode, bounded batch size, interval, and per-batch timeout as
`WITSELF_TRANSCRIPT_RETENTION_*` in the worker ConfigMap. Use three separate
rollout states:

1. `enabled: false`, `mode: preview` while compatible code and schema converge;
2. `enabled: true`, `mode: preview` while value-free eligibility and hold
   counts are reviewed;
3. `enabled: true`, `mode: enforce` only after the preview is accepted.

The mode defaults to `preview`, so merely enabling the retention job cannot
delete transcripts. Changing these values changes only the worker ConfigMap
checksum and restarts worker pods; it does not restart API pods. The API
ConfigMap always renders the legacy retention enabled gate as `false`.

Message retention follows the same three-stage operational gate through
`worker.messageRetention`, but it is a separate job with separate database
lanes and `WITSELF_MESSAGE_RETENTION_*` variables. It deletes whole inactive
message threads, never individual graph rows. Its defaults are `enabled:
false`, `mode: preview`, 25 threads per batch, a five-minute interval, and a
two-minute attempt timeout. Enabling or changing this job restarts worker pods
only. Contended graph rows are skipped without waiting, and bounded oversize
graphs are quarantined and surfaced through value-free Prometheus counters so
one thread cannot monopolize a worker lane.

Inbound agent-email retention follows the same three-stage gate through
`worker.agentEmailRetention` and the `WITSELF_AGENT_EMAIL_RETENTION_*`
variables. Its defaults are `enabled: false`, `mode: preview`, 25 messages per
batch, a five-minute interval, and a two-minute attempt timeout. Each batch is
also capped at 32 MiB of raw MIME. The worker defers a still-live processing
claim, but expired claims and unread or unacknowledged messages remain
eligible. Deleting an email cascades its delivery and accepted retry-canary
proof, clears suspected-duplicate backlinks, and preserves the mailbox,
address reservation, audit events, and usage records.

The public chart default keeps `worker.enabled: false` because it has no shared
database Secret. After PostgreSQL is configured, enabling it starts the
two-replica default; operators can deliberately override `worker.replicaCount`.
No worker HPA is rendered yet, so scaling is manual. That baseline prevents one
long-running job from blocking unrelated work. Worker rolling
updates use `maxUnavailable: 0`, `maxSurge: 1`, and `minReadySeconds: 10`;
the managed cell also enables a PDB and zonal spread. Work ownership remains a
database concern, so rolling overlap and future manual scale-out do not cause
two pods to own the same row.

The old top-level `transcriptRetention` and `avatar.styleRollout` value paths,
and any top-level `messageRetention` or `agentEmailRetention` path, are
rejected by schema validation instead of being silently ignored. Move them
under `worker` when enabling a released chart that contains this workload.

Keep the separate control-plane plan-lifecycle feature gate disabled during
the initial rolling cell deployment. Wait for the Deployment rollout to
complete, confirm every ready pod is on the fenced-snapshot-capable image and
the old ReplicaSet is at zero, and verify the plan snapshot GET endpoint
through the Service before enabling control-plane snapshot writes in a
separate deployment. Old cell binaries accept an unfenced plan request and
cannot safely overlap those writes.

For an existing multi-replica database, the rollout sequence is mandatory:
first deploy schema-27-compatible writers with the flag off and wait for full
convergence; next deploy schema 28 with the flag still off and wait again; only
then enable the flag. Do not skip the schema-27 compatibility release, because
schema 28 removes the conflict target used by older writers.

## Self-hosted vs cloud

The defaults are the **self-hosted** profile: single replica, `backend.kind:
self-hosted`, NetworkPolicy on, autoscaling/PDB off, no ingress. The **cloud**
profile ([ci/cloud-values.yaml](ci/cloud-values.yaml)) layers on HA: `backend.kind:
managed`, multiple replicas + HPA, PDB, ServiceMonitor, a tightened NetworkPolicy,
ingress + TLS, and topology spread.

## Safety posture (default)

- Non-root, read-only root filesystem, all capabilities dropped, `seccompProfile:
  RuntimeDefault`.
- `automountServiceAccountToken: false` — the server needs no Kubernetes API.
- Health and metrics are on their own ports and never exposed through the API
  Service or public ingress.
- The worker has no API Service or Ingress and receives no
  bootstrap/provision/agent-email relay or receive-mode configuration. Its
  metrics Service and monitors select only `app.kubernetes.io/name: witself-worker` plus
  `app.kubernetes.io/component: worker`; they cannot select API pods.
- Rolling upgrades default to `maxUnavailable: 0`, `maxSurge: 1`, and
  `minReadySeconds: 10`, so a replacement must remain ready before Kubernetes
  retires the previous pod.
- `lifecycle.preStopSleepSeconds` optionally renders the native Kubernetes
  `preStop.sleep` handler (Kubernetes 1.30+). Set it with a sufficiently larger
  `terminationGracePeriodSeconds` when a managed load balancer needs time to
  remove and drain a terminating endpoint.
- No secrets in `values.yaml` or rendered manifests; secret-bearing subsystems
  arrive via `existingSecret` references when they land.

## Key values

See [values.yaml](values.yaml) for the full set and [values.schema.json](values.schema.json)
for validation. Most-used: `image.tag`, `replicaCount`, `backend.kind`,
`features.factDeletion.enabled`, `avatar.payloadCompaction.enabled`,
`worker.enabled`, `worker.replicaCount`, `worker.avatarStyleRollout.*`,
`worker.messageRateBucketCleanup.*`,
`worker.agentEmailRateBucketCleanup.*`,
`worker.transcriptRetention.*`, `worker.messageRetention.*`,
`worker.agentEmailRetention.*`, `worker.resources`,
`worker.podDisruptionBudget.*`, `agentEmail.receivePilot.*`,
`agentEmail.receiveProduction.*`,
`database.existingSecret.*`, `bootstrap.existingSecret.*`, `resources`,
`metrics.serviceMonitor.enabled`, `autoscaling.*`, `ingress.*`,
`networkPolicy.*`, `strategy.*`, `minReadySeconds`,
`lifecycle.preStopSleepSeconds`, and `terminationGracePeriodSeconds`.
