# Witself Server Command Surface

Status: draft. This document describes the separate `witself-server` binary for
managed and self-hosted backend API deployments.

## Decision

`witself-server` is a separate binary from `ws`.

- `ws` is for humans, agents, and MCP usage.
- `witself-server` is for running and operating the backend API service.

There should not be a public `ws server` subcommand for production service
operation. Keeping the server process separate makes service packaging,
container images, process permissions, and self-hosting documentation clearer.

The platform spine is shared; this one process serves both planes — the OPEN
plane (memories, facts, policy, groups, messaging) and the SEALED plane (secrets,
TOTP). There is no separate secrets process or sidecar; the sealed plane is a
domain module behind the same API listener, gated by the sealed-plane capability
switch. See [secret-model.md](secret-model.md), [encryption-model.md](encryption-model.md),
and [key-hierarchy.md](key-hierarchy.md).

## Design Goals

- Run the backend API server for managed Witself Cloud and self-hosted
  deployments.
- Keep server operation separate from agent/operator identity workflows.
- Validate configuration before booting, including the Postgres/pgvector and
  embedding-provider prerequisites for the open plane, and the KMS prerequisite
  when the sealed plane is enabled.
- Run migrations explicitly and safely.
- Provide liveness, readiness, and startup checks for containers and service
  managers.
- Provide Prometheus-compatible metrics and Kubernetes-compatible health probes.
- Never print memory content, fact values, message bodies or payloads, embedding
  vectors, raw tokens, database passwords, provider credentials, embedding-provider
  API keys, raw payment details, or raw wallet credentials. Never print sealed-plane
  material: secret values, TOTP seeds, plaintext private keys, per-realm KEKs,
  per-secret/field DEKs, or KMS credentials. There is no secret-reveal or TOTP-code
  surface on `witself-server`; reveal is reserved for the audited `ws` value
  ceremony (see [secret-model.md](secret-model.md) and [totp-2fa.md](totp-2fa.md)).

## Command Tree

```text
witself-server
  version
  serve
  migrate up|down|status
  config check|print
  bootstrap token
  healthcheck
```

## Global Flags

| Flag | Description |
|---|---|
| `--config PATH` | Server config file. |
| `--env-prefix TEXT` | Environment variable prefix. Default: `WITSELF_`. |
| `--json` | Emit machine-readable JSON. |
| `--log-level LEVEL` | Override log level. |
| `--no-color` | Disable colored human output. |

## Environment Variables

Expected server environment variables may include:

| Variable | Description |
|---|---|
| `WITSELF_SERVER_LISTEN` | Public API listen address. Default should be `:8080`. |
| `WITSELF_HEALTH_LISTEN` | Dedicated health probe listen address. Default should be `:8081`. |
| `WITSELF_PUBLIC_URL` | Public URL for generated callbacks and metadata. |
| `WITSELF_DATABASE_URL` | Postgres connection string, supplied by the runtime or secret manager. The target database must have the pgvector extension available. |
| `WITSELF_OBJECT_STORE_PROVIDER` | Object/blob store provider when configured (exports, attachments, backups). |
| `WITSELF_OBJECT_STORE_BUCKET` | Object/blob store bucket/container. |
| `WITSELF_EMBEDDINGS_PROVIDER` | Embedding provider name: `voyage` (default), `openai`, or `local-dev`. |
| `WITSELF_EMBEDDINGS_MODEL` | Embedding model identifier for the active provider. |
| `WITSELF_EMBEDDINGS_API_KEY_FILE` | Path to a file holding the embedding-provider API key. Preferred over passing the key inline. |
| `WITSELF_EMBEDDINGS_API_KEY` | Embedding-provider API key supplied directly. Least-safe option; prefer the `_FILE` form or a secret mount. |
| `WITSELF_SEALED_PLANE_ENABLED` | Enable the sealed plane (secrets, TOTP). When true, the KMS variables below are required. An open-plane-only deployment may leave the sealed plane disabled. |
| `WITSELF_KMS_PROVIDER` | KMS provider for sealed-plane envelope encryption: `aws-kms` (default), `gcp-kms`, `azure-key-vault`, or `local-dev`. Required when the sealed plane is enabled; ignored otherwise. |
| `WITSELF_KMS_KEY_ID` | KMS customer master key (CMK) identifier that roots the per-realm KEK / per-secret-and-field DEK hierarchy. Required when the sealed plane is enabled. |
| `WITSELF_AUDIT_RETENTION` | Audit retention duration. Default should be `8760h` (365 days). |
| `WITSELF_METRICS_ENABLED` | Enable Prometheus-compatible metrics. Default should be true for server deployments and false only when explicitly disabled. |
| `WITSELF_METRICS_LISTEN` | Dedicated metrics listen address. Default should be `:9090`. |
| `WITSELF_METRICS_PATH` | Metrics path. Default: `/metrics`. |
| `WITSELF_LOG_LEVEL` | Log level. |

Embedding-provider credentials should be supplied through a file or secret mount,
not inline, wherever the deployment substrate supports it. Config validation and
logs must redact embedding-provider keys, database passwords, KMS credentials,
and all other sensitive values.

Witself serves two planes with two distinct production dependency sets. The OPEN
plane (memories, facts) is backed by ordinary data-at-rest; a pgvector-capable
Postgres is the hard dependency that gates semantic recall, and the embedding
provider is degradable. The SEALED plane (secrets, TOTP) is backed by KMS
envelope encryption: a CMK roots a per-realm KEK, which wraps per-secret and
per-field DEKs (XChaCha20-Poly1305 / AES-256-GCM). KMS is therefore a required
dependency when, and only when, the sealed plane is enabled — it is not required
for an open-plane-only deployment. KMS loss makes secret values unrecoverable
(crypto-shred) but does not affect the open plane. Sealed-plane material is never
embedded, recalled, placed in the self-digest, or included in plaintext export.
See [storage.md](storage.md), [encryption-model.md](encryption-model.md),
[key-hierarchy.md](key-hierarchy.md), and [memory-model.md](memory-model.md).

## `witself-server version`

Print server version and build metadata.

```sh
witself-server version
witself-server version --json
```

Flags:

| Flag | Description |
|---|---|
| `--short` | Print only the version string in human mode. |

## `witself-server serve`

Start the backend API server.

```sh
witself-server serve --config /etc/witself/server.toml
witself-server serve --listen :8080 --health-listen :8081 --metrics-listen :9090
witself-server serve --dev --data-dir ./dev-witself
```

Flags:

| Flag | Description |
|---|---|
| `--listen ADDRESS` | Public API listen address. Overrides config. |
| `--health-listen ADDRESS` | Dedicated health probe listen address. Overrides config. |
| `--public-url URL` | Public URL. Overrides config. |
| `--dev` | Run with the local development adapter and the `local-dev` embedding provider. Not production. |
| `--data-dir PATH` | Development data directory for `--dev`. |
| `--migrate` | Run pending migrations before serving when safe. |
| `--read-only` | Start in read-only maintenance mode when supported. |
| `--metrics-enabled BOOL` | Enable or disable Prometheus metrics. Overrides config. |
| `--metrics-listen ADDRESS` | Dedicated metrics listen address. Overrides config. |
| `--metrics-path PATH` | Metrics path. Default: `/metrics`. |
| `--disable-metrics` | Convenience alias for `--metrics-enabled=false`. |

`--dev` should use the local adapter, select the `local-dev` embedding provider
so semantic recall works offline, and must clearly label itself as
development-only in logs and health output.

Every server mode should expose `/v1/capabilities` so the public CLI can
determine whether it is talking to a managed, self-hosted, or local backend,
which features are supported, and the active embedding provider, model, and
vector dimensionality. When the embedding provider is unavailable or disabled,
the capability contract should report that semantic recall is degraded to
keyword/tag/kind/time ranking rather than failing silently; see
[memory-model.md](memory-model.md).

The API listener, health listener, and metrics listener should be separate by
default. Health and metrics endpoints should not be exposed through public
ingress unless an operator deliberately configures that.

The policy store, security groups, and inter-agent messaging are served by the
same process and share the same Postgres system of record. There are no separate
listeners or sidecars for policy evaluation, group membership, or the message
mailbox; they are domain modules behind the one API listener. See
[access-policy.md](access-policy.md), [security-groups.md](security-groups.md),
and [inter-agent-messaging.md](inter-agent-messaging.md). Durable message
delivery and per-recipient ordering depend on the same Postgres adapter; no
external broker is required for v0.

## `witself-server migrate`

Manage backend database migrations. Witself should use Goose migrations behind
these commands.

```sh
witself-server migrate status --config /etc/witself/server.toml
witself-server migrate up --config /etc/witself/server.toml
witself-server migrate down --steps 1 --config /etc/witself/server.toml
```

Flags:

| Flag | Description |
|---|---|
| `--target VERSION` | Migrate to a specific version when supported. |
| `--steps N` | Number of down migrations to apply. |
| `--dry-run` | Show planned migration changes without applying them. |
| `--yes` | Confirm destructive or backward migration actions. |

Migration commands should acquire a migration lock where the storage adapter
supports it. The Postgres adapter should use advisory locking where practical so
that concurrent rollouts and Helm migration Jobs do not race.

Migrations must create and verify the pgvector extension and the vector columns
and indexes that back open-plane semantic recall. `migrate status` should surface
whether the required extension and vector indexes are present. Changing the
embedding model or vector dimensionality is a separate, audited re-embedding
maintenance operation, not an automatic migration side effect; see
[storage.md](storage.md) and [backup-and-recovery.md](backup-and-recovery.md).

Migrations must also cover the sealed-plane tables when the sealed plane is in
scope: `secrets`, `secret_fields`, `secret_grants`, `totp_enrollments`,
`realm_keys` (the per-realm KEK records, wrapped under the CMK), `secret_deks`
(the per-secret/field wrapped DEKs), and `attachments`. Migrations only ever store
wrapped key material and ciphertext; no plaintext secret value, TOTP seed, KEK,
or DEK is written to the schema. Changing the KMS provider or rotating the CMK is
a separate, audited key-rotation operation, not an automatic migration side
effect; see [storage.md](storage.md), [key-hierarchy.md](key-hierarchy.md), and
[backup-and-recovery.md](backup-and-recovery.md).

## `witself-server config`

Validate and inspect effective server configuration.

### `witself-server config check`

Validate server config without starting the server.

```sh
witself-server config check --config /etc/witself/server.toml
witself-server config check --config /etc/witself/server.toml --check-connections
```

Flags:

| Flag | Description |
|---|---|
| `--check-connections` | Check Postgres (including pgvector availability), object store, embedding-provider, and — when the sealed plane is enabled — KMS connectivity. |
| `--strict` | Treat warnings as errors. |

`--check-connections` should confirm that the configured Postgres has the
pgvector extension available and that the configured embedding provider can be
reached and returns the expected vector dimensionality. A missing or
unreachable embedding provider should be reported as a degraded-recall warning,
not a hard failure, because plain `read`/`list` by id and metadata do not depend
on the embedding provider. When the sealed plane is enabled, `--check-connections`
must also confirm that the configured KMS provider and CMK are reachable and
usable for wrap/unwrap; an unreachable KMS is a hard failure for the sealed plane
because secrets cannot be sealed or revealed without it. When the sealed plane is
disabled, KMS connectivity is not checked.

### `witself-server config print`

Print effective server config with sensitive fields redacted.

```sh
witself-server config print --config /etc/witself/server.toml --json
```

Flags:

| Flag | Description |
|---|---|
| `--show-source` | Show whether each value came from file, env, or default. |

`config print` must never print embedding-provider API keys, database passwords,
KMS credentials, raw tokens, provider secrets, secret values, TOTP seeds, KEKs,
DEKs, or any memory content or fact values. The active embedding provider name,
model, and vector dimensionality are safe to print and should be shown. The KMS
provider name, the configured CMK key identifier, and whether the sealed plane is
enabled are also safe to print and should be shown when configured.

## `witself-server bootstrap`

Create one-time bootstrap material for first-operator setup in self-hosted
deployments.

### `witself-server bootstrap token`

Create a short-lived, single-use token that lets `ws setup --endpoint`
create the first operator context.

```sh
witself-server bootstrap token --config /etc/witself/server.toml --out /run/secrets/witself-bootstrap-token
witself-server bootstrap token --config /etc/witself/server.toml --ttl 15m --json
```

Flags:

| Flag | Description |
|---|---|
| `--ttl DURATION` | Token lifetime. Default should be short, such as `15m`. |
| `--out PATH` | Write the bootstrap token to an owner-only file. |
| `--force` | Create a new token even when an unused bootstrap token exists. |

Bootstrap tokens should be single-use, expire quickly, be auditable, and never
act as ordinary operator tokens. This command is not a general account or
support administration surface. There is no default admin password; the
self-hosted first operator is created exclusively through this bootstrap path.
See [operator-auth.md](operator-auth.md) and
[token-lifecycle.md](token-lifecycle.md).

## `witself-server healthcheck`

Run a local liveness, readiness, or startup check for containers and service
managers.

```sh
witself-server healthcheck --config /etc/witself/server.toml
witself-server healthcheck --url http://127.0.0.1:8081/readyz
```

Flags:

| Flag | Description |
|---|---|
| `--url URL` | Check a running server over HTTP. |
| `--ready` | Require readiness checks, including Postgres connectivity. |
| `--live` | Require only liveness checks. |
| `--startup` | Require startup checks suitable for Kubernetes startup probes. |
| `--timeout DURATION` | Maximum check duration. |

Readiness should depend on Postgres connectivity (the system of record for
memories, facts, policies, groups, messages, secrets, TOTP, and audit).
Embedding-provider reachability should be reported as a recall-degradation signal
in readiness detail rather than failing the readiness probe, so that a transient
provider outage does not take the service offline for plain reads and metadata
listing. When the sealed plane is enabled, readiness should also gate on KMS
reachability, because the sealed plane cannot seal or reveal secrets without it;
when the sealed plane is disabled, KMS is not part of the readiness gate.

Healthcheck output must not include memory content, fact values, message bodies,
embedding vectors, raw tokens, secret values, TOTP seeds, key material, or full
sensitive config.

## Non-Goals

- Do not use `witself-server` for human/operator account management.
- Do not make `witself-server` a private internal admin CLI.
- Do not require `witself-server` for local CLI-only development.
- Do not expose policy mutation, group management, message sending, identity
  export/import, secret reveal, TOTP code generation, secret grants, or runtime
  credential injection as server admin commands; those are agent/operator surfaces
  on `ws`.
- Do not turn `witself-server` into a sealed-plane value surface. The server
  process wraps and unwraps key material via KMS to operate the sealed plane, but
  the audited secret-reveal ceremony, TOTP code generation, and `ws run`
  runtime injection live exclusively on `ws`; sealed-plane values are never
  emitted by any `witself-server` command. See [secret-model.md](secret-model.md)
  and [totp-2fa.md](totp-2fa.md).

## Related Docs

- [backend-architecture.md](backend-architecture.md)
- [self-hosting.md](self-hosting.md)
- [api-contract.md](api-contract.md)
- [observability-and-operations.md](observability-and-operations.md)
- [storage.md](storage.md)
- [memory-model.md](memory-model.md)
- [secret-model.md](secret-model.md)
- [totp-2fa.md](totp-2fa.md)
- [encryption-model.md](encryption-model.md)
- [key-hierarchy.md](key-hierarchy.md)
- [data-model.md](data-model.md)
- [access-policy.md](access-policy.md)
- [security-groups.md](security-groups.md)
- [inter-agent-messaging.md](inter-agent-messaging.md)
- [release-and-build.md](release-and-build.md)
- [requirements.md](requirements.md)
- [operator-auth.md](operator-auth.md)
- [token-lifecycle.md](token-lifecycle.md)
- [post-v0-roadmap.md](post-v0-roadmap.md)
