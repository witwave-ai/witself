# Witself Deployment Cells & Multi-Cloud

Status: draft. This document captures the go-forward deployment topology for both
managed Witself Cloud and self-hosted Witself: a fleet of independent cells under a
thin global control plane. Decided 2026-06-28.

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
- Postgres + pgvector for the open plane (memories, facts, messaging) — see
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

Each cloud's cell is provisioned from the per-cloud Terraform modules already planned
in [cloud-targets.md](cloud-targets.md) and
[terraform-infrastructure.md](terraform-infrastructure.md); a cell is one instantiation
of a stack.

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
is simply another cell. The fleet reuses the per-cloud Terraform modules from
[cloud-targets.md](cloud-targets.md) and
[terraform-infrastructure.md](terraform-infrastructure.md); adding a cloud or account
means standing up another stack and registering its cell with the control plane.

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

## Tenant migration

Moving a realm/account from cell A to cell B is bounded but not free:

1. **Export** the tenant from cell A.
2. **Import** into cell B.
3. **Repoint** the control-plane mapping from cell A to cell B.
4. **Cut over** with a brief read-only freeze, or dual-write + reconcile.

Per plane:

- **Open plane** (memories, facts, messaging) moves via the existing first-class
  export/import (see [storage.md](storage.md) and
  [backup-and-recovery.md](backup-and-recovery.md)). Embeddings are recomputed in the
  destination, or moved directly if the destination cell uses the same embedding model.
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
- [terraform-infrastructure.md](terraform-infrastructure.md) — per-cloud modules a cell is built from
- [storage.md](storage.md) — open/sealed planes, export/import
- [billing-and-limits.md](billing-and-limits.md) — account-level billing
- [backup-and-recovery.md](backup-and-recovery.md) — per-cell backup and migration data movement
- [agent-collaboration.md](agent-collaboration.md) — cross-realm collaboration over the shared global directory
