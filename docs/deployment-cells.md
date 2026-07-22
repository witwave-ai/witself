# Witself Deployment Cells & Multi-Cloud

Status: draft. This document captures the go-forward deployment topology for both
managed Witself Cloud and self-hosted Witself: a fleet of independent cells under a
thin global control plane. Decided 2026-06-28.

Narrative-memory amendment (accepted 2026-07-14): cells have no backend memory
inference provider. Account movement uses source freeze or placement-epoch
fencing, clears imported leases, and rebuilds derived indexes under
[narrative-memory-and-curation.md](narrative-memory-and-curation.md).

> **Sealed-plane custody amendment (accepted 2026-07-18):**
> [ADR 0003](decisions/0003-client-custodied-agent-vault.md) and the
> [client-custodied vault plan](client-custodied-agent-vault.md) supersede the
> cell-rooted KMS design below. Cells store ciphertext, wrapped per-field DEKs,
> and public AVK metadata in PostgreSQL; they hold no agent AVK and expose no
> server-decrypt path. Moving an account copies the encrypted vault unchanged
> and requires no source/destination KMS re-wrap. An authorized client must
> separately possess the matching AVK. References below to a sealed-plane KMS,
> cloud-rooted secret key material, or cross-cloud KMS re-wrap are retained only
> as superseded design history.

## Decision

Witself deploys as a fleet of independent cells. A cell is one complete, isolated
Witself stack. A single thin, globally-replicated control plane holds only routing
metadata and decides which cell a tenant lands on and where to route. The control
plane holds no tenant data.

This topology is the same for managed Witself Cloud and for self-host: the same
[backend-architecture.md](backend-architecture.md) backend code runs in each cell;
the difference is how many cells exist and who operates them.

## Cell

A cell is one complete, independent Witself stack in a single cloud account/region:

- `witself-server` (see [backend-architecture.md](backend-architecture.md))
- PostgreSQL for the open plane (memories, facts, messaging, and optional
  migration-0032 JSONB vectors) — see
  [storage.md](storage.md)
- KMS for the sealed plane (secrets), rooted in that cell's cloud — see
  [storage.md](storage.md) and [key-hierarchy.md](key-hierarchy.md)
- Blob/object storage for attachments

Examples of distinct cells:

- AWS account #1, `us-east-1`
- AWS account #2, `us-east-1` — an independent second AWS account is simply another cell
- A GCP project, `europe-west1`
- An Azure subscription, `westus2`
- A self-host operator's single in-cluster stack

Cells are isolated. A cell holds the full data and key material for its own tenants
and depends on nothing in another cell to serve them. A cell outage affects only the
tenants homed on that cell — blast-radius containment. There is no shared data store
spanning cells.

Each cloud's cell is provisioned by the executable Pulumi program under
[`infra/pulumi`](../infra/pulumi), as tracked in
[cloud-targets.md](cloud-targets.md); a cell is one instantiation of a stack.

## Control plane

The control plane is the one new always-on global component. It is thin, HA, and
globally replicated. It holds only routing metadata:

```json
{
  "realm": "acme-prod",
  "account": "acct_8f3a",
  "home_cell": "aws-use1-01",
  "endpoint": "https://aws-use1-01.cells.witself.cloud",
  "signing_key": "<realm signing public key / JWKS ref>"
}
```

It does two things:

- **Placement** — picks the home cell for a new tenant.
- **Resolution** — answers "where is this realm/account, and how do I reach it?"

It holds no memories, facts, secrets, or messages. Keeping it thin keeps its blast
radius tiny: if the control plane degrades, existing clients that have already
resolved their home cell keep working against that cell directly; only fresh
placement and first-time resolution are affected.

The control plane extends the existing `--endpoint` / token model. Today a client
points at an endpoint; the go-forward client resolves its home cell from the control
plane (and may cache it), then talks directly to that cell. Tokens remain
cell-scoped and are validated by the home cell, not the control plane.

```text
witself --endpoint https://api.witself.cloud login   # control plane resolves home cell
# subsequent calls go directly to the resolved home-cell endpoint
```

## Placement and landing

At account/realm creation, the control plane picks a cell by:

- region / data-residency requirement
- capacity and load across the fleet
- provider preference (AWS / GCP / Azure, or a specific account)
- rollout wave (see versioning below)

It records the realm/account -> cell mapping. Clients then resolve their home cell on
login or first call and route directly. Placement emits `tenant.placed` — registered
in the audit-event registry alongside `tenant.migration_started` /
`tenant.migration_completed` / `tenant.migration_failed` (see
[audit-retention.md](audit-retention.md)).

## Multi-cloud

The fleet spans AWS, GCP, and Azure, across multiple accounts per cloud. Each cell is
one cloud account/region. An independent second AWS account is not a special case — it
is simply another cell. The fleet reuses the AWS, GCP, and Azure paths in the
Pulumi cell program described by [cloud-targets.md](cloud-targets.md); adding a
cloud account or project means standing up another stack and registering its
cell with the control plane.

## Cells at different versions

Cells may run different software versions at the same time. This is a strength of the
cell model, not a problem:

- **Canary / wave rollout** — a new release lands on one cell (or a wave of cells)
  first; placement can steer new tenants toward or away from a wave.
- **Capability discovery** — clients discover a cell's capabilities/version and adapt,
  rather than assuming a single global version. The same discovery mechanism the
  collaboration substrate uses for realm/agent cards (see
  [agent-collaboration.md](agent-collaboration.md)) covers cell capability advertisement.

Because cells are isolated, a bad release is contained to the cells it reached.

## Receive-only agent-email pilot

The Cloudflare receive pilot is a managed, capability-limited cell feature. It
is disabled unless every one of these server settings is present and valid:

- `WITSELF_AGENT_EMAIL_RECEIVE_PILOT_ENABLED=true`
- `WITSELF_AGENT_EMAIL_PILOT_DOMAIN` — the one canonical lowercase pilot domain
- `WITSELF_AGENT_EMAIL_PILOT_AUDIENCE` — the exact destination-cell audience
- `WITSELF_AGENT_EMAIL_PILOT_REALM_ID` — the one enrolled realm
- `WITSELF_AGENT_EMAIL_PILOT_AGENT_IDS` — a comma-separated set of 5–10 enrolled
  agent IDs
- `WITSELF_AGENT_EMAIL_RELAY_PUBLIC_KEYS_JSON` — a JSON object mapping relay key
  IDs to standard-base64 raw Ed25519 public keys
- `WITSELF_AGENT_EMAIL_RELAY_REPLAY_WINDOW` — optional; defaults to `5m` and may
  not exceed `15m`

The private Ed25519 relay key is a secret of the isolated Cloudflare Email
Worker and must never be placed in cell configuration. On startup, an enabled
cell reconciles the one realm and exact agent allowlist into durable mailboxes
and addresses. Startup fails before serving when the realm or an agent is
missing or inactive, an agent belongs to another realm, an address collides, or
an existing mailbox/address has inconsistent ownership.

The edge implementation lives in `infra/cloudflare/agent-email/`. It uses its
own `witself-agent-email-pilot` Worker and an isolated
`witself-agent-email-pilot-directory` KV namespace. It must never bind the
control-plane `DIRECTORY` namespace. Route management is limited to literal
rules for the 5–10 enrolled addresses; it reads and fingerprints the existing
catch-all but has no operation that can update it.

An operator activates the edge only after the cell release and configuration
are healthy, the disabled exact-route set has been reviewed, and KV propagation
has settled. A synthetic exact-address canary must then prove Worker-to-cell
commit before any expected pilot mail is sent. Rollback disables only the
pilot's directory gate and literal routes. See [Agent Email](agent-email.md) and
the [edge README](../infra/cloudflare/agent-email/README.md) for the staged
procedure. A configured cell, deployed Worker, or enabled routing rule alone is
not proof of end-to-end operation.

## Current GitOps Release Rollout

The directories under `.gitops/cells/` are configured desired-state targets;
their presence does not prove that the cell is provisioned, reachable, or
currently reconciled. Confirm the intended rollout set from live Argo and cloud
state before changing a values file.

Release publication and cell deployment are separate operations. First verify
that the tag-triggered release completed and that its version-matched chart
exists. `VERSION` omits the Git tag's `v` prefix:

```sh
VERSION="${RELEASE_VERSION:?set RELEASE_VERSION}"
gh release view "v${VERSION}"
helm show chart oci://ghcr.io/witwave-ai/charts/witself-server \
  --version "$VERSION"
```

Before any release that can advance the database schema, create and verify a
pre-migration backup for the canary cell and record its identifier in the
private rollout record. Managed GCP rollouts must complete the on-demand Cloud
SQL procedure in
[Backup And Recovery](backup-and-recovery.md#gcp-cloud-sql-pre-migration-backup)
before `roll-cell.sh`; a recent scheduled backup is not a substitute.

Then roll one provisioned canary by its exact cell-directory name:

```sh
CELL="${CANARY_CELL:?set CANARY_CELL}"
scripts/roll-cell.sh "$CELL" "$VERSION"
git diff -- ".gitops/cells/${CELL}/values.yaml"
```

The helper changes only `apps.witselfServer.chartVersion` and
`apps.witselfServer.imageTag`, keeping the chart and image on the same released
version. Review and commit the desired canary or wave to `main`; do not edit
unrelated platform chart versions as part of an application rollout. A
bootstrapped cell's Argo applications use automated pruning and self-healing,
so they reconcile the committed values without a separate deployment command.

For each provisioned cell in the wave, verify all of the following before
advancing:

1. The bootstrap, apps, and `witself-server` Argo applications are Healthy and
   Synced.
2. Replacement pods become Ready without sacrificing the required available
   replicas.
3. `GET https://<cell-api-host>/v1/version` reports `${VERSION}` and the tagged
   commit.
4. Server startup logs confirm migration completion. The current server runs
   embedded Goose migrations before serving when a database DSN is configured;
   a migration error exits the process rather than serving the new build.
5. The release-specific API, CLI/MCP, and multi-provider client smoke tests pass.

For the agent-email schema-60/61 rollout, treat old/new writer convergence as
a hard feature barrier. Freeze agent-email receive-control mutations and all
account export/import or cell-move work before Phase A. Deploy the new schema
and application with `retryCanaryAgentID` empty, then verify that every old pod
has drained. A realm disable is not authoritative while a pre-schema-60 pod is
still serving: that binary reads only each mailbox's agent layer. A pre-60
export can also omit the realm-control row and cause a newer importer to
synthesize `enabled`.

Only after full Phase-A convergence may operators change agent/realm receive
controls or resume archive movement. Enable the provider-retry canary in a
separate config-only Phase B, wait for every pod to converge again, and only
then arm/send a manual proof. For rollback, turn off any recurring canary
schedule that has been added and settle any armed proof first. Before removing
the canary setting or deploying pre-60/61 code, disable the process-level
receive pilot and the exact edge routes; never rely on a realm-disabled row to
protect traffic from an older binary, and never run a pre-60 export after that
row has become authoritative.

For avatar creative-payload compaction, this release pin is Phase A: leave
`apps.witselfServer.avatarPayloadCompactionEnabled: false`, freeze avatar
mutation/import/export during writer convergence, and wait until every old
writer has drained. After Phase A is healthy, use a separate config-only commit
to set the gate to `true`; verify that the nested ConfigMap checksum restarts
every pod. Do not mix the Phase-B gate flip with another chart/image change.

Repeat the same narrow GitOps change and verification for later waves. A values
pin, a Git commit, or an Argo sync alone is not proof that a feature is
operational end to end. When a release changes installed hooks or managed
instructions, upgrade the client binary and rerun `witself install` for each
supported runtime before declaring the client behavior complete. See
[Release And Build Notes](release-and-build.md) and
[Autonomous Realm Messaging](autonomous-realm-messaging.md).

## Tenant migration

Moving a realm/account from cell A to cell B is bounded but not free:

1. **Export** the tenant from cell A.
2. **Import** into cell B.
3. **Repoint** the control-plane mapping from cell A to cell B.
4. **Cut over** with a brief read-only freeze, or dual-write + reconcile.

Per plane:

- **Open plane** (memories, facts, messaging) moves via the existing first-class
  export/import (see [storage.md](storage.md) and
  [backup-and-recovery.md](backup-and-recovery.md)). Immutable vector profiles and
  client-supplied JSONB vector rows move in that archive. Only derived full-text
  indexes and any future optional ANN projection are rebuilt in the destination.
- **Sealed plane** (secrets) is KMS-rooted per cell/cloud, so migration **re-wraps**
  keys under the destination KMS: an audited decrypt-at-source / re-encrypt-at-dest
  pass. The plaintext data keys are unwrapped under cell A's KMS and re-wrapped under
  cell B's KMS; the operation is audited end to end (see
  [key-hierarchy.md](key-hierarchy.md)).

Migration emits `tenant.migration_started`, `tenant.migration_completed`, and
`tenant.migration_failed`. After repoint, clients re-resolve and route to cell B.

## Fleet model

The fleet is many independent live cells, each authoritative for its own tenants.
There is no shared data store across cells and no shared-data multi-master across
clouds in v1 — that is a much harder problem and is deferred. A tenant has exactly one
home cell at a time; that cell is the single writer and source of truth for its data.

Migration (above) is how a tenant changes home cell; it is a deliberate, bounded
operation, not continuous replication. Per-cell backup/recovery follows
[backup-and-recovery.md](backup-and-recovery.md).

## Shared global directory

The collaboration relay needs to resolve a realm handle to where it lives plus its
signing key. That is the same registry the control plane already maintains for
placement and resolution: realm/account -> home cell + endpoint + signing key. Cells
and cross-realm collaboration share one global directory.

So a cross-realm message addressed to `witself://<realm-handle>/agent/<name>` resolves
through the same control-plane directory that routes a client to its home cell. The
relay routes by realm handle to the realm's home cell and verifies the published
signing key; see [agent-collaboration.md](agent-collaboration.md) for the blind-relay
model, signed realm/agent cards, and federation trust.

## Billing aggregation across cells

Billing is account-level (see [billing-and-limits.md](billing-and-limits.md)). An
account's realms may be placed on different cells. Usage is metered per realm in each
realm's home cell, then aggregated to the account level across cells for billing and
limit enforcement. The control-plane account -> realm -> cell mapping is what makes
cross-cell aggregation possible.

## Open decisions

These are open; this document records them without resolving them.

- **Placement unit (account vs realm).** Whether the cell-placement and migration unit
  is the account or the realm. Recommendation under discussion: the realm is the
  placement/migration unit, with an account-level default cell, and realms individually
  re-homeable.
- **Self-host single-cell vs multi-cell.** Whether a self-host deployment is always a
  single cell (single-tenant norm) or may itself be a multi-cell fleet with its own
  control plane.
- **Migration cutover approach.** Brief read-only freeze vs dual-write + reconcile as
  the default cutover mechanism.

## Cross-links

- [backend-architecture.md](backend-architecture.md) — backend code that runs in each cell
- [cloud-targets.md](cloud-targets.md) — provider order and per-cloud targets
- [`infra/pulumi`](../infra/pulumi) — executable per-cloud cell provisioner
- [storage.md](storage.md) — open/sealed planes, export/import
- [billing-and-limits.md](billing-and-limits.md) — account-level billing
- [backup-and-recovery.md](backup-and-recovery.md) — per-cell backup and migration data movement
- [agent-collaboration.md](agent-collaboration.md) — cross-realm collaboration over the shared global directory
