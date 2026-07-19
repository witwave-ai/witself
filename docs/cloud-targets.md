# Witself Cloud Targets

Status: provider substrate and executable certification gate implemented; live
certification still in progress. The Pulumi cell program provisions AWS, GCP,
and Azure. AWS remains the first production-hardening target. The same
provider-neutral 3-by-3 memory/account-move gate is now runnable against all
three managed PostgreSQL services, but a cloud is not certified until a
specific release passes that gate on its real endpoints.

Narrative-memory decision (accepted 2026-07-14): AWS-first is certification and
hardening order only. Portable narrative memory is not complete until the same
PostgreSQL capture/recall/curation/archive conformance suite passes on AWS,
Azure, and GCP.
The backend has no model or embedding provider. Deterministic PostgreSQL lexical
recall is required everywhere. Optional immutable client-supplied vector
profiles and portable JSONB vector rows may add deterministic hybrid ranking;
zero coverage falls back to lexical recall and requires no pgvector extension.

Sealed-plane custody amendment (accepted 2026-07-18):
[ADR 0003](decisions/0003-client-custodied-agent-vault.md) and the
[client-custodied vault contract](client-custodied-agent-vault.md) supersede
KMS-rooted agent-secret, realm-KEK, and server-side-decrypt language below. The
backend holds no AVK key material, calls no KMS for agent secrets, and exposes
no decrypt or `server_side_decrypt` path. Ordinary infrastructure KMS and
storage-encryption references are unaffected.

## Decision

Witself should certify and harden AWS first without making the application or
archive format AWS-specific.

AWS is first for:

- The first production PostgreSQL narrative-memory conformance deployment.
- The first production KMS integration for the sealed plane.
- The first production-shaped Helm values example.
- The first CI or smoke environment that exercises cloud-shaped infrastructure.

The executable Pulumi substrate is already implemented for AWS, GCP, and Azure.
It provisions a provider-specific Kubernetes cluster, private managed
PostgreSQL, networking, secret delivery, DNS, and the optional GitOps bootstrap.
That implementation status must not be confused with certification: the same
released server/schema has not yet passed the complete managed-provider memory
suite or an actual cross-provider archive move on all three targets.

## Why AWS First For Certification

- It keeps the first production backend path focused.
- It gives one concrete cloud to harden before multiplying provider behavior.
- It lines up with the first production storage path: managed PostgreSQL as the
  canonical memory store with deterministic lexical recall.
- It lines up with the AWS KMS decision for the sealed plane: AWS KMS is the
  first key-management provider, with `gcp-kms` and `azure-key-vault` planned
  (see [storage.md](storage.md), [key-hierarchy.md](key-hierarchy.md)).
- It leaves the already implemented GCP and Azure substrates available for the
  same conformance suite without changing the memory architecture.

## Current Substrate And Certification Priority

The current executable provider paths live under `infra/pulumi`:

1. AWS: EKS and RDS PostgreSQL.
2. GCP: GKE and Cloud SQL for PostgreSQL.
3. Azure: AKS and Azure Database for PostgreSQL Flexible Server.

Pulumi unit and compile tests prove that these provider graphs can be built.
They do not prove that a live managed database accepts every migration or that
an account behaves identically after a provider-to-provider move. The
implemented gate in
[memory-cloud-conformance.md](memory-cloud-conformance.md) therefore requires,
for each provider:

1. provision a live cell and apply all migrations;
2. run the same capture, history, recall, lifecycle, curation, deletion, and
   tenant-isolation suite against its managed PostgreSQL service;
3. export a suspended account, import it into a cell on another provider, and
   verify canonical row graphs, idempotent retries, and curation fencing; and
4. prove that generated search documents and GIN indexes are rebuilt rather
   than transported as canonical archive data.

Terraform modules may remain an additional packaging target, but they are not
the evidence for the current three-provider substrate or memory certification.

## PostgreSQL Requirement

Every cloud target must provide a supported PostgreSQL version for canonical
memory rows, immutable history, evidence, relations, tombstones, generated
search documents, deterministic lexical indexes, immutable client vector
profiles, and optional portable vector rows. The current production contract
does not require pgvector or any other model-specific extension. A later ANN
projection may be provider-specific optimization only; it may not weaken the
portable JSONB contract or lexical baseline. See [storage.md](storage.md).

## KMS Requirement for the Sealed Plane

The sealed plane (secrets and TOTP) is protected by KMS-backed envelope
encryption: a customer master key (CMK) wraps a per-realm KEK, which wraps a
per-secret/field DEK (see [key-hierarchy.md](key-hierarchy.md)). The AWS target
provisions AWS KMS as the first key-management provider; `gcp-kms` and
`azure-key-vault` are planned and follow AWS.

- KMS is a hard dependency only when the sealed plane is enabled. An
  open-plane-only deployment (memories and facts) requires PostgreSQL but not
  KMS. Readiness gates on KMS only when the sealed plane is enabled; PostgreSQL
  stays the hard gate for the open plane.
- The AWS module surfaces the KMS provider, key id, and IAM grants as explicit
  prerequisites (`witself-server` reads `WITSELF_KMS_PROVIDER` and
  `WITSELF_KMS_KEY_ID`; see [storage.md](storage.md)).
- KMS-loss is unrecoverable for sealed values by design (crypto-shred): without
  the CMK, secret and TOTP values cannot be decrypted. This does not affect the
  open plane, whose data is plaintext at rest in Postgres.
- Sealed-plane carve-outs hold regardless of cloud target: secret and TOTP
  values are never vectorized, never returned by memory recall, never in the
  self-digest, and never written to plaintext export. Only the explicit,
  reveal-gated, audited operations return sealed values.

## Client Inference Is Not Cloud Substrate

AI inference and vector generation occur in authenticated clients, outside the
Witself cell. Cell infrastructure therefore provisions no model endpoint,
embedding service, provider egress, or provider credential for
`witself-server`.

- Cloud targets provision compute, PostgreSQL, object/blob storage, identity,
  networking, and optional sealed-plane KMS.
- Client model credentials stay with the client and never enter server, Helm, or
  infrastructure configuration.
- Every cell serves deterministic lexical/tag/kind/time recall from PostgreSQL
  without an external AI dependency.
- The optional vector capability stores and queries client-authored vectors
  under immutable portable profiles. It is implemented without backend model
  calls and remains optional rather than a cloud prerequisite.

## Multi-Cloud Cells

Cloud targets are not a single-cloud choice made once. Witself deploys as a fleet
of independent cells, where a cell is one complete, isolated Witself stack in a
single cloud account and region (see [deployment-cells.md](deployment-cells.md)).
The fleet spans AWS, GCP, and Azure, across multiple accounts per cloud.

- **A second AWS account is just another cell.** An independent second AWS account
  is not a special case — it is simply another cell. The same applies to a second
  GCP project or Azure subscription. Each cell is one cloud account/region.
- **AWS-first is a certification order.** The AWS-first ordering above is about
  which live provider path hardens first, not about how many cells exist or
  whether GCP and Azure provisioning code exists. Each cell is one Pulumi stack;
  all three provider graphs are implemented, and each must pass the same
  conformance and cell-move gates before it is described as certified.
- **Placement is by region and data-residency.** A thin global control plane picks
  the home cell for a new tenant by region / data-residency requirement, capacity
  across the fleet, and provider/account preference. The cloud target a tenant
  lands on is a placement outcome, not a global setting.
- **A fleet of independent cells, not a shared substrate.** There is no shared data
  store spanning cells and no shared-data multi-master across clouds. Each cell
  holds the full data and key material for its own tenants; a cell outage affects
  only the tenants homed on that cell. Moving a tenant between clouds or accounts is
  a deliberate, bounded migration (with a sealed-plane KMS re-wrap under the
  destination cell), not continuous cross-cloud replication — see
  [deployment-cells.md](deployment-cells.md).

This is why the provider-neutral contracts below matter: they let any cloud target
serve as a cell without AWS-only assumptions leaking into the fleet.

## Provider Portability

AWS-first should not mean AWS-only.

Shared contracts should stay provider-neutral:

- CLI behavior.
- API contracts.
- JSON contracts.
- Memory, fact, policy, group, and message model.
- Secret, TOTP, grant, and capability model (sealed plane).
- Client-authored inference boundary and vector-profile portability contract.
- KMS provider interface.
- Object/blob storage interface.
- Helm chart values shape.
- Observability and health probe semantics.
- Infrastructure stack conventions.

Provider-specific behavior should live behind storage, KMS, object/blob,
identity, and infrastructure-as-code boundaries.

## Related Docs

- [requirements.md](requirements.md)
- [deployment-cells.md](deployment-cells.md)
- [storage.md](storage.md)
- [encryption-model.md](encryption-model.md)
- [key-hierarchy.md](key-hierarchy.md)
- [terraform-infrastructure.md](terraform-infrastructure.md)
- [self-hosting.md](self-hosting.md)
- [helm-chart.md](helm-chart.md)
- [observability-and-operations.md](observability-and-operations.md)
- [implementation-plan.md](implementation-plan.md)
- [memory-cloud-conformance.md](memory-cloud-conformance.md)
