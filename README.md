# Witself

Status: pre-implementation draft. The `witself` CLI ships incrementally (with
`ws` kept as a short alias).

## Install

```sh
# Homebrew (installs the `witself` binary plus a `ws` alias)
brew install witwave-ai/tap/witself

# Optional infrastructure provisioner
brew install witwave-ai/tap/witself-infra

# Fleet-admin CLI (talks to the control plane; not needed for tenants)
brew install witwave-ai/tap/witself-admin
# (the fullscreen operator dashboard ships inside it: `witself-admin dashboard`)

# or curl | sh (verifies the SHA-256 checksum; installs witself by default)
curl -fsSL https://raw.githubusercontent.com/witwave-ai/witself/main/install.sh | sh

# Optional infrastructure provisioner
curl -fsSL https://raw.githubusercontent.com/witwave-ai/witself/main/install.sh | sh -s witself-infra

# Fleet-admin CLI
curl -fsSL https://raw.githubusercontent.com/witwave-ai/witself/main/install.sh | sh -s witself-admin

witself version
```

## Infrastructure Example

`witself-infra` provisions one complete isolated cell per cloud account/region.
For the current AWS sandbox cell in `us-west-2`:

```sh
witself-infra up \
  -account-alias sandbox \
  -argocd \
  -aws-profile witwave-sandbox \
  -backend s3 \
  -bootstrap-token-file ~/.witself/tokens/bootstrap.token \
  -cidr 10.20.0.0/16 \
  -cloud aws \
  -control-plane https://self.witwave.ai \
  -db-version 18 \
  -domain cells.witself.witwave.ai \
  -fleet-token-file ~/.witself/tokens/fleet.token \
  -gitops-path .gitops/charts/bootstrap \
  -gitops-repo https://github.com/witwave-ai/witself \
  -gitops-revision main \
  -gitops-values-path .gitops/cells/aws-sandbox-usw2-dev/values.yaml \
  -ingress alb \
  -k8s-version 1.36 \
  -profile minimal \
  -region us-west-2 \
  -role dev
```

For the current GCP sandbox cell in `us-east1`:

```sh
# Pulumi GCS/GCP KMS and GKE verification use Application Default Credentials.
gcloud auth application-default login --project witself-sandbox

witself-infra up \
  -account-alias sandbox \
  -argocd \
  -backend gcs \
  -bootstrap \
  -bootstrap-token-file ~/.witself/tokens/bootstrap.token \
  -cidr 10.20.0.0/16 \
  -cloud gcp \
  -control-plane https://self.witwave.ai \
  -db-version 18 \
  -domain cells.witself.witwave.ai \
  -fleet-token-file ~/.witself/tokens/fleet.token \
  -gcp-project witself-sandbox \
  -gitops-path .gitops/charts/bootstrap \
  -gitops-repo https://github.com/witwave-ai/witself \
  -gitops-revision main \
  -gitops-values-path .gitops/cells/gcp-sandbox-use1-dev/values.yaml \
  -profile minimal \
  -region us-east1 \
  -restore-archives \
  -restore-any-region \
  -role dev
```

For the Azure sandbox subscription in `eastus2`, Azure support currently covers
the shared Pulumi state backend plus the first workload substrate slices:
resource group, VNet, workload subnet, PostgreSQL-delegated DB subnet,
controlled outbound egress through Azure NAT Gateway, private Azure Database
for PostgreSQL Flexible Server, per-cell Azure Key Vault app secrets, AKS with
OIDC/workload identity enabled, native AKS cluster autoscaler on the system
node pool, Azure DNS delegation, and GitOps identities for platform add-ons such
as External Secrets Operator and ExternalDNS. Pulumi also enables the
AKS-managed Application Gateway for Containers ALB Controller add-on so the
GitOps app tier can render the Azure Gateway API path for the Witself API,
including cert-manager-managed Let's Encrypt TLS with Azure DNS-01 validation
for HTTPS. HTTP-to-HTTPS redirect policy remains a follow-up ingress polish
slice.

```sh
az login --tenant a18639f4-1eb4-4810-ab3b-5717aa935e27
az account set --subscription witwave-sandbox

witself-infra bootstrap \
  -azure-subscription witwave-sandbox \
  -backend azblob \
  -cloud azure \
  -region eastus2

witself-infra up \
  -account-alias sandbox \
  -argocd \
  -azure-subscription witwave-sandbox \
  -backend azblob \
  -cloud azure \
  -db-version 18 \
  -domain cells.witself.witwave.ai \
  -gitops-path .gitops/charts/bootstrap \
  -gitops-repo https://github.com/witwave-ai/witself \
  -gitops-revision main \
  -gitops-values-path .gitops/cells/azure-sandbox-use2-dev/values.yaml \
  -ingress none \
  -k8s-version 1.36 \
  -profile minimal \
  -region eastus2 \
  -role dev
```

The bootstrap token file contains the first operator token that `witself-infra`
publishes to the cell's cloud secret store. One shared token can bootstrap every
cell (it is single-use *per cell* — each cell consumes its own copy on first
claim). If `-bootstrap-token-file` is omitted, the CLI prefers a per-cell
`~/.witself/tokens/<cell>/bootstrap.token` and falls back to the shared
`~/.witself/tokens/bootstrap.token`.

When `CLOUDFLARE_API_TOKEN` is present, `witself-infra` also delegates the
per-cell DNS zone from the Cloudflare zone for the configured domain. Keep
that token available during teardown so the delegated DNS records can be removed.

The GCP substrate includes a regional Cloud Router and Public Cloud NAT with a
reserved outbound IP, so sandbox and production cells get predictable controlled
egress in the same one-shot `up` path.

The Azure substrate includes a NAT Gateway and static public IP on the workload
subnet, with default outbound access disabled, so AKS workloads have a
predictable controlled egress path. AKS uses Azure CNI overlay, a one-node
minimal system pool that can autoscale to 20 nodes (`prod` starts at two nodes
and also caps at 20), and OIDC/workload identity so GitOps platform components
can read cloud secrets without long-lived
credentials. Its
PostgreSQL Flexible Server is private only, uses the delegated DB subnet, and
resolves through the cell's private DNS zone link. Azure stores the cell DB JSON,
first-operator bootstrap token, and account-provision token as JSON secrets in
the cell Key Vault. Pulumi also creates the ESO managed identity, federates it
to the `external-secrets/external-secrets` Kubernetes service account, and grants
it read access to the cell Key Vault. Pulumi creates the Azure DNS zone,
delegates it from Cloudflare when `CLOUDFLARE_API_TOKEN` is present, and creates
the ExternalDNS managed identity plus federated credential for the
`external-dns/external-dns` service account. Pulumi also reserves a delegated
subnet for Azure Application Gateway for Containers, enables the AKS-managed ALB
Controller add-on, grants that add-on identity permission to join the delegated
subnet, and passes the subnet ID into GitOps so Argo renders the Gateway API
manifests plus cert-manager's Azure DNS-01 issuer/certificate resources for
HTTPS.
Add `-argocd` to the Azure `up` command to install the same Argo CD app-of-apps
layer used by AWS and GCP.

With `-control-plane`, `up` registers the cell with the Witself Cloud fleet
after provisioning (endpoint = the cell's `apiHost` output), authorized by the
fleet token. `-fleet-token-file` points at the token file; when omitted the
token is read from `WITSELF_FLEET_TOKEN`, then `~/.witself/tokens/fleet.token`.
Omit `-control-plane` entirely and no registration happens — the self-hosted
path is the same command without the flag.

With `-argocd` on GCP and Azure, `up` also waits for the Argo CD Applications to
report `Synced/Healthy` through the cluster API after Pulumi finishes. This
catches late DNS, ingress, certificate, and app-of-apps convergence before the
command exits.

The examples use `-profile minimal`: cheap, disposable sandbox shapes with small
managed Postgres instances and clean teardown behavior. GCP `prod` raises Cloud
SQL to regional availability, PITR backups, retained/final backups, and larger
disk headroom; similar Azure prod hardening is a later slice. Use prod for
persistent cells, not save-money teardown tests.

The teardown command keeps only the stack identity, backend, configured domain,
and credentials. It destroys the cell resources; the shared Pulumi state backend
remains for the next run.

```sh
witself-infra destroy \
  -account-alias sandbox \
  -aws-profile witwave-sandbox \
  -backend s3 \
  -cloud aws \
  -control-plane https://self.witwave.ai \
  -domain cells.witself.witwave.ai \
  -fleet-token-file ~/.witself/tokens/fleet.token \
  -region us-west-2 \
  -role dev
```

For the GCP sandbox, use the same teardown shape with the GCS backend and GCP
project:

```sh
witself-infra destroy \
  -account-alias sandbox \
  -argocd \
  -backend gcs \
  -cloud gcp \
  -control-plane https://self.witwave.ai \
  -domain cells.witself.witwave.ai \
  -fleet-token-file ~/.witself/tokens/fleet.token \
  -gcp-project witself-sandbox \
  -region us-east1 \
  -role dev
```

For the Azure sandbox, use the Azure Blob backend and subscription selector:

```sh
witself-infra destroy \
  -account-alias sandbox \
  -azure-subscription witwave-sandbox \
  -backend azblob \
  -cloud azure \
  -ingress none \
  -profile minimal \
  -region eastus2 \
  -role dev
```

With `-control-plane`, `destroy` first drains the cell (placement stops),
**evacuates every account into a per-account archive in Cloudflare R2**
(suspend → export → verify → retire routing, looping until none remain), and
only then removes the cell from the fleet and tears the infrastructure down.
The accounts wait in R2 — "archived, awaiting placement" — until they are
restored onto another cell. Add `-destroy-accounts` to skip preservation and
purge the directory entries instead: the sandbox/dev override, an explicit
acknowledgment that the accounts die with the cell.

Pass `-restore-archives` to `up` to close the loop. After the new cell
registers, the fleet placement runner assigns each archived account to its
best eligible accepting cell (import → resume → route → cleanup). Ranked
cloud, canonical-region, and release-channel preferences decide first; hard
`only-*` pins filter eligibility; current account count and then cell name
break ties.

For an explicit evacuation test where provider region names do not line up
(for example, restoring AWS `us-west-2` archives onto a temporary GCP
`us-east1` cell), add `-restore-any-region` with `-restore-archives`. That
operator override bypasses the provider-region guard on legacy archives.
Policy-aware archives still honor their explicit hard pins.

Account owners manage the policy stored with their account:

```sh
witself account placement show
witself account placement set \
  --prefer-clouds gcp,aws,azure \
  --prefer-regions usw2,use1 \
  --prefer-channels stable,edge,experimental \
  --rebalance-on cloud,channel
```

Fleet operators can inspect placement, preview or perform live movement, and
enable the five-minute scheduled restore/rebalance pass:

```sh
witself-infra placement-status -control-plane https://self.witwave.ai
witself-infra rebalance -control-plane https://self.witwave.ai -dry-run
witself-infra placement-runner -control-plane https://self.witwave.ai -enable -run
```

If an archived account has impossible hard pins, an operator can clear only
the blocked axes while preserving its ranked preferences. The rescued policy
is applied to the imported account before it resumes on its destination cell:

```sh
witself-admin placement rescue --account-id acc_... --axes region,channel
```

See [infra/pulumi/README.md](infra/pulumi/README.md) for the CLI internals and
[.gitops/README.md](.gitops/README.md) for how Argo CD reconciles the GitOps
tree after `-argocd` is enabled.

## Overview

Witself is the agent durable-state platform **and** the trust fabric agents
collaborate over. Every agent gets a durable, attributable self — memories,
facts, and sealed credentials — plus a verified, loop-safe channel to work with
other agents across machines, realms, and accounts. The identity and memory
store is what makes that channel trustworthy: collaboration rides on the same
attributable self, so a counterpart can be verified before it is trusted.

It is being designed to let AI agents and human operators manage their own state
across two planes: an **open plane** of memories and facts, and a **sealed
plane** of secrets and TOTP authenticators. Agents manage their own state,
access other agents' state under declarative policy and grants, organize into
security groups, and exchange durable messages — within a realm and, as the
flagship post-v0 epic, across realms and accounts — all through a safe,
auditable CLI, MCP, and API surface. Witself is deployed as a fleet of
independent multi-cloud cells, each authoritative for its own tenants.

The two planes share one platform spine — one core domain service behind thin
CLI/MCP/API adapters, the same account/realm/agent tenancy and token model, the
same deployment shape, and the same authorization, audit, observability, release,
and billing apparatus — but take deliberately opposite postures on the payload:

- The **open plane** (memories, facts) protects the *integrity and authenticity*
  of identity data: plaintext at rest, semantically indexed and recallable,
  cross-agent readable under policy, in the self-digest, and plaintext-exportable.
- The **sealed plane** (secrets, TOTP) protects the *confidentiality* of
  credential material: KMS-backed envelope encryption (CMK → per-realm KEK →
  per-secret/field DEK) and reveal-gated access. Sealed-plane values are never
  embedded, never returned by semantic recall, never in the self-digest, and
  never plaintext-exported. They surface only through explicit, audited reveal.

Witself consolidates the former Witpass credential vault and authenticator into
the sealed plane: one product, one `witself` CLI, one backend, one account and
agent model.

The product goal is a CLI-first agent durable-state service spanning both planes:

- Named agents inside an operator-managed realm.
- Memories: free-form self-content with versioned edit history and a soft
  forget/restore lifecycle.
- Semantic recall by default: embedding-backed similarity search blended with
  keyword, tag, kind, and time filters.
- Facts: deterministic name→value identity cards, with a `primary` flag for
  identity anchors.
- A sealed credential plane: secrets and TOTP authenticators under KMS-backed
  envelope encryption (CMK → per-realm KEK → per-secret/field DEK), with explicit,
  audited, reveal-gated access and cross-agent grants. Sealed-plane values are
  never embedded, never returned by semantic recall, never in the self-digest, and
  never plaintext-exported.
- Cross-agent access governed by evaluable, default-deny policy.
- Security groups that act as both policy subjects and policy targets and can own
  group-scoped shared memories and facts.
- Durable inter-agent messaging with delivery, ordering, and acknowledgement.
- Cross-realm agent collaboration: a verified, loop-safe channel for agents to
  work together across machines, realms, and accounts — signed realm discovery,
  a blind relay, and enforced loop and spend caps — built on the realm-local
  mailbox as the first post-v0 epic.
- First-class structured/plaintext identity export and round-trippable import.
- Identity references such as `witself://fact/email` and
  `witself://agent/archivist/memory/mem_...`.
- Agent self-managed memory and hydration: an always-loaded self-digest, recall
  before acting, and a `remember` quick-add — plus being a good CLAUDE.md/AGENTS.md
  citizen via two-way `digest emit` / `ingest`.
- MCP compatibility for agent runtimes.
- Managed Witself Cloud by default.
- A multi-cloud cell platform: each cell is one complete, independent Witself
  stack in its own cloud account or region, with a thin global control plane for
  tenant placement and routing — a fleet of cells, each authoritative for its
  own tenants, so a cell outage stays contained.
- Public backend code and first-class self-hosting for operators who want to run
  Witself in their own cloud.

Managed Witself Cloud is the default supported product. Self-hosting is available
from the public repo, while production self-host support is a paid or contracted
support path once hardening docs and operational guidance are real.

## Repository Status

This repository is pre-v0 and still docs-led, but implementation is now landing
incrementally. The `witself` CLI, `witself-server`, Helm chart, GitOps tree, release
workflows, and Pulumi-based `witself-infra` module are built in this repo.

## Docs

- [Runbooks](docs/runbooks.md) — hand-testing recipes for everything that is built and running
- [Requirements](docs/requirements.md)
- [Data Model](docs/data-model.md)
- [Context Hydration](docs/context-hydration.md)
- [V0 Scope](docs/v0-scope.md)
- [Memory Model](docs/memory-model.md)
- [Facts Model](docs/facts-model.md)
- [Secret Model](docs/secret-model.md)
- [TOTP 2FA](docs/totp-2fa.md)
- [Secret Size And Attachments](docs/secret-size-and-attachments.md)
- [Encryption Model](docs/encryption-model.md)
- [Key Hierarchy](docs/key-hierarchy.md)
- [Authorization And Roles](docs/authorization-and-roles.md)
- [Access Policy](docs/access-policy.md)
- [Security Groups](docs/security-groups.md)
- [Inter-Agent Messaging](docs/inter-agent-messaging.md)
- [Agent Collaboration](docs/agent-collaboration.md)
- [Operator Authentication](docs/operator-auth.md)
- [Token Lifecycle](docs/token-lifecycle.md)
- [Workflow Scripts](docs/workflow-scripts.md)
- [CLI Command Surface](docs/cli-command-surface.md)
- [Server Command Surface](docs/server-command-surface.md)
- [API Contract](docs/api-contract.md)
- [API Routes](docs/api-routes.md)
- [MCP Tools](docs/mcp-tools.md)
- [JSON Contracts](docs/json-contracts.md)
- [Backend Architecture](docs/backend-architecture.md)
- [Deployment Cells](docs/deployment-cells.md)
- [Storage](docs/storage.md)
- [Observability And Operations](docs/observability-and-operations.md)
- [Self-Hosting](docs/self-hosting.md)
- [Self-Hosted Support](docs/self-host-support.md)
- [Helm Chart](docs/helm-chart.md)
- [Terraform Infrastructure](docs/terraform-infrastructure.md)
- [Cloud Targets](docs/cloud-targets.md)
- [Backup And Recovery](docs/backup-and-recovery.md)
- [Billing And Limits](docs/billing-and-limits.md)
- [Release And Build](docs/release-and-build.md)
- [Implementation Plan](docs/implementation-plan.md)
- [Scaffold Readiness](docs/scaffold-readiness.md)
- [Audit Retention](docs/audit-retention.md)
- [Post-v0 Roadmap](docs/post-v0-roadmap.md)
- [Threat Model](docs/threat-model.md)
- [Security Policy](docs/security-policy.md)
- [Governance And Support](docs/governance-and-support.md)
- [Competitive Analysis](docs/competitive-analysis.md)

## Planned Public Artifacts

- Source repo: `github.com/witwave-ai/witself`
- Homebrew tap: `github.com/witwave-ai/homebrew-tap`
- CLI/MCP image: `ghcr.io/witwave-ai/images/witself`
- Backend image: `ghcr.io/witwave-ai/images/witself-server`
- Helm chart: `ghcr.io/witwave-ai/charts/witself-server`

## Security

Do not file public issues for suspected vulnerabilities. See
[SECURITY.md](SECURITY.md) and [docs/security-policy.md](docs/security-policy.md).

## License

Witself is **source-available** under the [Functional Source License,
FSL-1.1-ALv2](LICENSE). You may use, modify, and self-host it for any purpose
**except** offering it as a commercial product or service that competes with
Witself. Two years after each release is published, that release converts to the
Apache License 2.0.

The license covers the software. It does not grant rights to use the Witself
name, marks, logos, or branding except as allowed by the license for describing
the origin of the work.
