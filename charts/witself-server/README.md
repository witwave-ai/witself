# witself Helm chart

Deploys [`witself-server`](https://github.com/witwave-ai/witself) — the Witself
backend — onto Kubernetes. One chart serves both self-hosted and cloud/managed
deployments; the difference is values, not templates.

```sh
helm install witself oci://ghcr.io/witwave-ai/charts/witself-server \
  --version <version> \
  --namespace witself --create-namespace \
  --values ./my-values.yaml
```

## Scope

This chart tracks what `witself-server` actually consumes today: the three
listeners (API `:8080`, health `:8081`, metrics `:9090`), `backend.kind`, and an
optional Postgres DSN from an existing Secret, and an optional first-operator
bootstrap token mounted from an existing Secret. Agent secrets use the
client-custodied AVK design in
[ADR 0003](../../docs/decisions/0003-client-custodied-agent-vault.md): this
chart needs no sealed-plane feature flag, backend KMS setting, or decrypt-key
Secret. The chart does not render a migration Job; when a database DSN is
configured, `witself-server serve` applies its embedded forward Goose migrations
before becoming Ready. Nothing here renders config the server would silently
ignore.

## What it renders

| Resource | When |
|---|---|
| Deployment, Service (API), ServiceAccount, ConfigMap | always |
| Metrics Service | `metrics.enabled` and `metrics.service.enabled` (default on) |
| ServiceMonitor / PodMonitor | `metrics.serviceMonitor.enabled` / `metrics.podMonitor.enabled` |
| Ingress | `ingress.enabled` |
| HorizontalPodAutoscaler | `autoscaling.enabled` |
| PodDisruptionBudget | `podDisruptionBudget.enabled` |
| NetworkPolicy | `networkPolicy.enabled` (default on) |
| Helm test pod | `helm test` |

Set `database.existingSecret.name` and `database.existingSecret.urlKey` to expose
the referenced key as `WITSELF_DATABASE_URL` in the server container.

Set `bootstrap.existingSecret.name` to mount a first-operator bootstrap token at
`bootstrap.tokenFile` (default `/.witself/tokens/bootstrap.token`) and expose
the configured TTL as `WITSELF_BOOTSTRAP_TOKEN_TTL`.

Permanent fact deletion is disabled by default. `features.factDeletion.enabled`
renders `WITSELF_FACT_DELETION_ENABLED`; a server compiled against store schema
27 or older refuses to start when it is enabled, so turn it on only with schema
28 or newer.

Large-realm avatar style propagation is enabled by default. The
`avatar.styleRollout` values render
`WITSELF_AVATAR_STYLE_ROLLOUT_ENABLED`,
`WITSELF_AVATAR_STYLE_ROLLOUT_BATCH_SIZE` (1-1000),
`WITSELF_AVATAR_STYLE_ROLLOUT_INTERVAL` (100ms-1h), and
`WITSELF_AVATAR_STYLE_ROLLOUT_BATCH_TIMEOUT` (server bound 100ms-5m). Every replica
may run the worker; PostgreSQL job locking provides the shared fence.

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
`avatar.styleRollout.*`, `database.existingSecret.*`,
`bootstrap.existingSecret.*`, `resources`,
`metrics.serviceMonitor.enabled`, `autoscaling.*`, `ingress.*`,
`networkPolicy.*`, `strategy.*`, `minReadySeconds`,
`lifecycle.preStopSleepSeconds`, and `terminationGracePeriodSeconds`.
