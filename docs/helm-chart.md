# Witself Helm Chart

Status: draft. This document describes the first self-hosting deployment
artifact for Witself.

Narrative-memory decision (accepted 2026-07-14): the chart does not configure a
backend LLM, model, embedder, or provider credential. PostgreSQL supplies the
required deterministic lexical baseline. Optional client-supplied vector
profiles and portable JSONB vector rows are implemented under
[narrative-memory-and-curation.md](narrative-memory-and-curation.md).

## Decision

Witself should go straight to Helm for the first self-hosted deployment
artifact.

The Helm chart is the production-shaped self-hosting path. Docker Compose can
remain a future developer convenience, but it should not be the initial
self-hosting artifact or the production deployment story.

Terraform should handle cloud infrastructure. Helm should handle the Kubernetes
application deployment onto that infrastructure.

Witself reuses the shared platform spine for deployment. The chart skeleton
(Deployment, Service, ServiceAccount, ConfigMap, Ingress, NetworkPolicy,
migration Job, monitors, autoscaling, disruption budget) is intentionally the
same. PostgreSQL carries canonical open-plane data (memories + facts) and its
deterministic lexical indexes. KMS provider config is present only for the
sealed plane (secrets + TOTP). There is no server-side model/provider block or
AI egress rule. An open-plane-only deployment requires no KMS configuration. See
[encryption-model.md](encryption-model.md) and
[key-hierarchy.md](key-hierarchy.md).

## Chart Identity

- Chart path: `charts/witself-server`
- Chart name: `witself-server`
- Public OCI package: `ghcr.io/witwave-ai/charts/witself-server`
- Primary image: `ghcr.io/witwave-ai/images/witself-server`
- Primary process: `witself-server`

Expected install shape:

```sh
helm install witself oci://ghcr.io/witwave-ai/charts/witself-server \
  --version 0.1.0 \
  --namespace witself \
  --create-namespace \
  --values ./witself-values.yaml
```

The chart is named after the component it deploys (`witself-server`), 1:1 with
`images/witself-server`. The bare name `witself` is reserved for a future
*umbrella* chart that aggregates components (`witself-server`, plus any later
deployables such as a cross-realm relay) so `helm install witself` can stand up
the whole product. MCP is intentionally **not** a separate chart: it is a
frontend on the one core — served client-side by `witself mcp serve` next to the
agent, or, if a hosted endpoint is ever offered, as a streamable-HTTP route on
`witself-server` (a values toggle, not a new deployable).

## Chart Goals

- Deploy `witself-server` for self-hosted Witself.
- Keep production dependencies explicit and operator-owned.
- Avoid storing raw credentials in chart defaults or recommended values files.
- Support deployment-native identity and secret references.
- Make migrations controlled and reviewable.
- Keep model execution, vector generation, and their credentials out of the
  backend chart.
- Make KMS provider configuration explicit and required only when the sealed
  plane is enabled.
- Render Kubernetes resources that security teams can inspect.
- Stay portable across Kubernetes distributions where practical.

## Production Defaults

Production chart defaults should assume:

- External PostgreSQL with the features required by the canonical relational
  and lexical-memory schema.
- External KMS-compatible key management when the sealed plane is enabled
  (`aws-kms`, `gcp-kms`, `azure-key-vault`, or `local-dev` for tests/demos).
- Optional external object/blob storage for exports, attachments, and backups.
- TLS termination through ingress, gateway, or load balancer.
- Non-root container execution where practical.
- Liveness, readiness, and startup probes.
- Dedicated API, health, and metrics container ports.
- Prometheus-compatible metrics enabled by default unless explicitly disabled.
- Structured logs with redaction.
- Resource requests and limits.
- Pod disruption budget where practical.
- Optional horizontal pod autoscaling.
- Pod and container security contexts.

The chart should not install a production database by default. A bundled
Postgres dependency may be considered later for development-only profiles, but
it must be clearly labeled as non-production and must still support the same
canonical schema and lexical recall contract.

The chart must not bundle or configure any model or embedding provider. Client
inference is outside the pod and outside the chart; see
[memory-model.md](memory-model.md).

## PostgreSQL Canonical Store

Witself's system of record is PostgreSQL. The current recall contract uses
generated search documents and deterministic lexical/tag/kind/time ranking; it
does not require pgvector or an external AI service.

- The external PostgreSQL reached through the chart must support the schema and
  indexes created by the server-owned Goose migrations.
- The migration Job fails deterministically when the required canonical schema
  or lexical-index prerequisites cannot be created.
- Readiness gates on PostgreSQL availability, not model/provider reachability.
- Optional client-vector profiles and JSONB vector rows are created by migration
  `0032`; they require no chart setting or PostgreSQL extension and cannot
  become a hidden production prerequisite.

Database provisioning belongs to Terraform or the operator; the chart only
consumes the connection reference. See
[terraform-infrastructure.md](terraform-infrastructure.md).

## Kubernetes Resources

Initial templates should include:

- `Deployment` for `witself-server`.
- `Service`.
- `ServiceAccount`.
- `ConfigMap` for non-secret server config.
- Optional `Ingress`.
- Optional `NetworkPolicy`.
- Optional `PodDisruptionBudget`.
- Optional `HorizontalPodAutoscaler`.
- Optional dedicated health `Service` for cluster-internal diagnostics.
- Optional dedicated metrics `Service`.
- Optional Prometheus Operator `ServiceMonitor`.
- Optional Prometheus Operator `PodMonitor`.
- Optional migration `Job`.
- Optional test hook or notes for health checks.

The chart should not create broad RBAC permissions unless a concrete feature
requires them. `witself-server` enforces realm, agent, policy, group, and
messaging authorization at the application layer below every frontend; it does
not delegate cross-agent access decisions to Kubernetes RBAC. Cluster RBAC for
the workload should remain minimal: the running pod needs no Kubernetes API
access for ordinary identity, policy, group, or messaging operations.

Service account annotations should support cloud-native identity systems such as
IRSA, Workload Identity, or equivalent mechanisms, primarily so the workload can
reach PostgreSQL, KMS (when the sealed plane is enabled), and object/blob storage
without static credentials.

## Network Policy

Witself's network surface is shaped by identity reads, cross-agent access,
policy evaluation, group operations, and inter-agent messaging. All of that
traffic is in-cluster application traffic on the API port; the chart's default
`NetworkPolicy` should reflect that.

Default-deny posture, opened only where required:

- Ingress to the API port (`api`) from ingress controllers, gateways, or
  in-cluster clients that carry agent or operator tokens. Inter-agent messaging
  is server-mediated: agents do not talk to each other directly, so the policy
  needs no agent-to-agent allowances.
- Ingress to the metrics port (`metrics`) only from the monitoring namespace or
  Prometheus scrapers.
- Ingress to the health port (`health`) only from the kubelet and cluster-
  internal diagnostics; never from public ingress.
- Egress to PostgreSQL for the system of record.
- Egress to the configured KMS endpoint when the sealed plane is enabled, for
  envelope encryption and reveal operations. `local-dev` and a disabled sealed
  plane require no KMS egress.
- Egress to object/blob storage when exports, attachments, or backups are
  enabled.
- Egress to the billing/payment provider when managed billing is enabled
  (optional for self-hosted; see [billing-and-limits.md](billing-and-limits.md)).

The chart must not render AI/model-provider egress for `witself-server`.
Inference clients manage their own networking outside this deployment.

## Secrets And Config

Values files must not require raw secrets.

The chart should support existing secret references such as:

```yaml
database:
  existingSecret:
    name: witself-database
    urlKey: database-url

sealedPlane:
  enabled: true

kms:
  provider: aws-kms
  keyRef: arn:aws:kms:us-east-1:123456789012:key/example
  existingSecret:
    name: witself-kms
    envFrom: true
```

The `kms` block is only consumed when `sealedPlane.enabled` is true. When the
sealed plane is disabled, secrets and TOTP are not served, no KMS provider is
configured, and the `kms` block can be omitted entirely.

Recommended values should prefer:

- Existing Kubernetes Secrets.
- Secret Store CSI driver.
- External Secrets Operator.
- Cloud workload identity.
- Mounted secret files for credentials that should not be environment
  variables.

The chart must not place database passwords, KMS credentials, object-store
credentials, raw agent tokens, bootstrap tokens, raw payment details, wallet
credentials, or provider secrets into default `values.yaml`.

There is intentionally no `embeddings` values block, model setting, or AI
provider Secret. Client inference credentials must never be mounted into the
backend pod.

KMS provider credentials are referenced exclusively through an existing Secret
(or through cloud workload identity, which is preferred). The `kms.keyRef` is a
key identifier — an ARN, resource name, or key URI — not key material; the chart
never holds the CMK and never holds a per-realm KEK or per-secret DEK. The
referenced Secret carries only the provider credentials the server needs to call
KMS. Those credentials are never a plaintext value in the chart, never a CLI
flag, and never logged. When the sealed plane is disabled or the provider is
`local-dev`, the KMS Secret reference can be omitted. This config governs only
the sealed plane; secret and TOTP values are KMS-wrapped, reveal-gated, and are
never embedded, recalled, placed in the self-digest, or plaintext-exported. See
[encryption-model.md](encryption-model.md) and
[key-hierarchy.md](key-hierarchy.md).

## Client Inference Boundary

The chart deploys a deterministic storage and retrieval service. It never
selects a model, injects AI credentials, starts an embedder, or calls a model
endpoint. Authenticated clients author memory content and curation decisions.
The current backend reports lexical retrieval and implemented optional
client-vector profile support independently.

Client-supplied vectors need no chart-side provider or pgvector configuration.
If a future pgvector/ANN projection is introduced, the chart may expose only its
optional storage/query acceleration switch. It still must not contain generation
provider, model, credential, or provider-egress settings. See
[narrative-memory-and-curation.md](narrative-memory-and-curation.md).

## KMS Provider

The sealed plane (secrets + TOTP) uses KMS-backed envelope encryption
(CMK -> per-realm KEK -> per-secret/field DEK). When the sealed plane is enabled,
the KMS provider is a required, capability-gated deployment dependency. The
provider selection is plumbed through config and an existing Secret (or
workload identity), not hard-coded. See
[key-hierarchy.md](key-hierarchy.md).

Chart behavior:

- Gate the sealed plane with `sealedPlane.enabled`. When it is false, no KMS
  values are required and the server does not serve secrets or TOTP.
- Select the provider with `kms.provider` (`aws-kms`, `gcp-kms`,
  `azure-key-vault`, or `local-dev` for tests and demos), mapped to
  `WITSELF_KMS_PROVIDER`.
- Identify the customer master key with `kms.keyRef` (an ARN, resource name, or
  key URI), mapped to `WITSELF_KMS_KEY_ID`. This is a key identifier, never key
  material.
- Supply provider credentials only through `kms.existingSecret` (injected via
  `envFrom`) or, preferably, through cloud workload identity. The chart never
  carries the CMK, KEK, or DEK.
- Treat the sealed plane as required-when-enabled: readiness gates on KMS
  reachability only when `sealedPlane.enabled` is true. The open plane stays
  available even when KMS is unreachable; PostgreSQL remains the hard gate for
  the open plane. See [storage.md](storage.md).
- Key rotation (per-realm KEK) is an explicit, audited maintenance operation and
  is never a chart upgrade side effect.

The chart must never write the CMK identifier's backing credentials, a KEK, a
DEK, or any KMS credential into a ConfigMap, log line, metric label, annotation,
or rendered manifest. KMS-wrapped secret values are never embedded, recalled,
placed in the self-digest, or plaintext-exported. KMS loss renders sealed-plane
values unrecoverable (crypto-shred) and does not affect the open plane.

## Migrations

Migrations should be explicit and operator-controlled.

Initial chart behavior:

- Include a migration `Job` template.
- Do not run destructive or backward migrations automatically.
- Support a controlled install/upgrade path where operators can run:
  `witself-server migrate status` and `witself-server migrate up`.
- Require `--yes` or equivalent explicit confirmation for destructive migration
  paths when supported by the server command.
- Treat Goose migrations as the server-owned migration mechanism, including the
  narrative-memory tables, generated search documents, and lexical indexes.
- Acquire a database advisory lock so concurrent server replicas do not race the
  migration step.

The chart may support opt-in install or upgrade hooks later, but production
guidance should prefer an explicit migration job before rolling the server. See
[server-command-surface.md](server-command-surface.md).

## Values Shape

Illustrative values:

```yaml
image:
  repository: ghcr.io/witwave-ai/images/witself-server
  tag: v0.1.0
  pullPolicy: IfNotPresent

server:
  listen: ":8080"
  publicUrl: "https://witself.example.com"
  logLevel: info

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

database:
  existingSecret:
    name: witself-database
    urlKey: database-url

sealedPlane:
  enabled: true

kms:
  provider: aws-kms
  keyRef: arn:aws:kms:us-east-1:123456789012:key/example
  existingSecret:
    name: witself-kms
    envFrom: true

audit:
  retentionDays: 365

serviceAccount:
  create: true
  annotations: {}

ingress:
  enabled: false
  className: ""
  hosts: []

networkPolicy:
  enabled: true

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

podSecurityContext:
  runAsNonRoot: true

securityContext:
  allowPrivilegeEscalation: false
  readOnlyRootFilesystem: true
  capabilities:
    drop: ["ALL"]

migrations:
  enabled: false
  command: ["witself-server", "migrate", "up"]
```

This values shape is illustrative. Exact names can change during
implementation, but the safety posture should not. The `kms` block is required
only when `sealedPlane.enabled` is true: the open plane (memories + facts) keeps
ordinary data-at-rest encryption at the storage layer, while the sealed plane
(secrets + TOTP) uses KMS-backed envelope encryption. Optional field-level
redaction of `sensitive` facts is a capability of the open plane and is distinct
from the sealed plane's reveal-gated, KMS-wrapped secrets; see
[storage.md](storage.md), [encryption-model.md](encryption-model.md), and
[key-hierarchy.md](key-hierarchy.md).

## Observability And Operational Support

The chart should make the normal Kubernetes operating path boring and
inspectable.

Required chart behavior:

- Configure `livenessProbe`, `readinessProbe`, and `startupProbe` against the
  documented health endpoints on the dedicated health port.
- Configure separate named container ports for `api`, `health`, and `metrics`.
- Avoid exposing the health port through public ingress by default.
- Expose `/metrics` only through the dedicated metrics port.
- Render a dedicated metrics service only when metrics are enabled and metrics
  service values request it.
- Support Prometheus Operator `ServiceMonitor` and `PodMonitor` resources when
  those CRDs are installed and the corresponding values are enabled.
- Keep metrics enabled by default for server deployments unless the operator
  opts out.
- Allow metrics enablement, listen address, port, path, annotations, labels,
  scrape interval, scrape timeout, relabelings, and metric relabelings to be
  configured.
- Surface `WITSELF_METRICS_ENABLED` through the metrics values so enablement is
  consistent across config, environment, CLI, and Helm.
- Provide resource requests and memory limits by default.
- Support optional HPA configuration.
- Support optional PDB configuration.
- Support configurable pod security context, container security context,
  node selectors, tolerations, affinity, and topology spread constraints.
- Support graceful shutdown settings such as termination grace period when the
  server needs them, so in-flight messaging and recall requests drain cleanly.

The scraped metric set includes memory and lexical-recall operations, fact
operations, policy decisions (allow/deny), cross-agent
accesses, group operations, message send/deliver/read, authentication, token
lifecycle, audit events, usage metering, limit decisions, storage, migrations,
and HTTP latency. The implemented client-vector surface contributes only
value-free validation, coverage/fallback, latency, and storage metrics; a future
ANN projection may add projection-specific health/latency metrics. See
[observability-and-operations.md](observability-and-operations.md).

Health responses, metrics, logs, chart annotations, and rendered manifests must
not contain memory content, fact values, message bodies or payloads,
client-supplied vectors, raw tokens, database URLs, or other
sensitive configuration values.

## Terraform Handoff

Terraform modules under `infra/terraform` should provision the cloud substrate
and output the references the Helm chart needs.

Expected handoff data:

- Namespace.
- Service account annotations for workload identity.
- Database connection secret name and key for PostgreSQL.
- KMS provider, key identifier (`kms.keyRef`), and the Secret name/key (or
  workload identity) holding its credentials, when the sealed plane is enabled.
- Object/blob storage bucket or container for exports, attachments, and backups.
- Ingress host or public URL.
- Secret Store CSI or External Secrets references when used.

Terraform should not generate Helm values files containing raw credentials. It
should create or reference deployment-native secrets, then pass only secret
names, keys, IDs, and non-sensitive configuration into Helm. Terraform is also
responsible for provisioning a supported PostgreSQL database before the
migration Job runs. See
[terraform-infrastructure.md](terraform-infrastructure.md).

## CI And Release Checks

Required checks once the chart exists:

- `helm lint charts/witself-server`.
- `helm template` with default values.
- `helm template` with representative production values.
- Kubernetes schema validation for rendered manifests.
- Secret scanning of chart defaults and examples.
- Check that rendered resources do not include raw secret values, including KMS
  credentials.
- Check that rendered resources contain no backend AI/model/provider variables,
  Secrets, sidecars, or egress rules.
- Check that liveness, readiness, startup, and metrics paths render correctly.
- Check ServiceMonitor and PodMonitor enabled and disabled render paths.
- Check the `kms` block and KMS egress render only when `sealedPlane.enabled` is
  true and the provider is not `local-dev`, and that no KMS block is required for
  an open-plane-only install.
- Package and publish the chart to
  `ghcr.io/witwave-ai/charts/witself-server`.
- Sign or provenance-attest the chart package.
- Smoke test `helm show chart` against the published OCI chart.

See [release-and-build.md](release-and-build.md).

## Non-Goals

- Do not make Docker Compose the initial self-hosting artifact.
- Do not make Helm provision cloud infrastructure that belongs in Terraform.
- Do not bundle a production database by default.
- Do not bundle, configure, or contact a backend model or embedding provider.
- Do not hide production prerequisites behind managed-service assumptions.
- Do not require Witself-managed billing or support integrations for self-hosted
  chart installs.

## Related Docs

- [self-hosting.md](self-hosting.md)
- [backend-architecture.md](backend-architecture.md)
- [server-command-surface.md](server-command-surface.md)
- [api-contract.md](api-contract.md)
- [storage.md](storage.md)
- [encryption-model.md](encryption-model.md)
- [key-hierarchy.md](key-hierarchy.md)
- [secret-model.md](secret-model.md)
- [memory-model.md](memory-model.md)
- [access-policy.md](access-policy.md)
- [security-groups.md](security-groups.md)
- [inter-agent-messaging.md](inter-agent-messaging.md)
- [observability-and-operations.md](observability-and-operations.md)
- [terraform-infrastructure.md](terraform-infrastructure.md)
- [billing-and-limits.md](billing-and-limits.md)
- [release-and-build.md](release-and-build.md)
- [requirements.md](requirements.md)
- [implementation-plan.md](implementation-plan.md)
- [threat-model.md](threat-model.md)
