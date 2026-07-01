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
bootstrap token mounted from an existing Secret. The broader production surface
in [docs/helm-chart.md](../../docs/helm-chart.md) — embedding provider config,
KMS for the sealed plane, and the migration `Job` — is wired in as the server
gains those subsystems. Nothing here renders config the server would silently
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
`bootstrap.tokenFile` (default `/.witself/bootstrap/bootstrap-token`) and expose
the configured TTL as `WITSELF_BOOTSTRAP_TOKEN_TTL`.

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
- No secrets in `values.yaml` or rendered manifests; secret-bearing subsystems
  arrive via `existingSecret` references when they land.

## Key values

See [values.yaml](values.yaml) for the full set and [values.schema.json](values.schema.json)
for validation. Most-used: `image.tag`, `replicaCount`, `backend.kind`,
`database.existingSecret.*`, `bootstrap.existingSecret.*`, `resources`,
`metrics.serviceMonitor.enabled`, `autoscaling.*`, `ingress.*`,
`networkPolicy.*`.
