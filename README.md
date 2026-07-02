# Witself

Status: pre-implementation draft. The `ws` CLI ships incrementally; v0.0.1 (the
`version` command) is installable today.

## Install

```sh
# Homebrew
brew install witwave-ai/tap/ws

# Optional infrastructure provisioner
brew install witwave-ai/tap/witself-infra

# or curl | sh (verifies the SHA-256 checksum; installs ws by default)
curl -fsSL https://raw.githubusercontent.com/witwave-ai/witself/main/install.sh | sh

# Optional infrastructure provisioner
curl -fsSL https://raw.githubusercontent.com/witwave-ai/witself/main/install.sh | sh -s witself-infra

ws version
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
  -bootstrap-token-file ~/.witself/bootstrap.token \
  -cidr 10.20.0.0/16 \
  -cloud aws \
  -control-plane https://self.witwave.ai \
  -db-version 18 \
  -domain cells.witself.witwave.ai \
  -fleet-token-file ~/.witself/fleet.token \
  -gitops-path .gitops/charts/bootstrap \
  -gitops-repo https://github.com/witwave-ai/witself \
  -gitops-revision main \
  -gitops-values-path .gitops/cells/aws-sandbox-usw2-dev/values.yaml \
  -ingress cloudflare-tunnel \
  -k8s-version 1.36 \
  -profile minimal \
  -region us-west-2 \
  -role dev
```

The bootstrap token file contains the first operator token that `witself-infra`
publishes to the cell's cloud secret store. One shared token can bootstrap every
cell (it is single-use *per cell* — each cell consumes its own copy on first
claim). If `-bootstrap-token-file` is omitted, the CLI prefers a per-cell
`~/.witself/bootstrap/<cell>/bootstrap.token` and falls back to the shared
`~/.witself/bootstrap.token`.

When `CLOUDFLARE_API_TOKEN` is present, `witself-infra` also delegates the
per-cell Route 53 zone from the Cloudflare zone for the configured domain. Keep
that token available during teardown so the delegated DNS records can be removed.

With `-control-plane`, `up` registers the cell with the Witself Cloud fleet
after provisioning (endpoint = the cell's `apiHost` output), authorized by the
fleet token. `-fleet-token-file` points at the token file; when omitted the
token is read from `WITSELF_FLEET_TOKEN`, then `~/.witself/fleet.token`.
Omit `-control-plane` entirely and no registration happens — the self-hosted
path is the same command without the flag.

The teardown command keeps only the stack identity, backend, configured domain,
and credentials. It destroys the cell resources; the shared S3/KMS Pulumi state
backend remains for the next run.

```sh
witself-infra destroy \
  -account-alias sandbox \
  -aws-profile witwave-sandbox \
  -backend s3 \
  -cloud aws \
  -control-plane https://self.witwave.ai \
  -destroy-accounts \
  -domain cells.witself.witwave.ai \
  -fleet-token-file ~/.witself/fleet.token \
  -region us-west-2 \
  -role dev
```

With `-control-plane`, `destroy` first drains the cell (placement stops) and
removes it from the fleet before tearing anything down. Removal refuses while
accounts still live on the cell; `-destroy-accounts` is the explicit
acknowledgment that those accounts die with the cell and purges their
directory entries too. Without it, migrate the accounts off first.

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
the sealed plane: one product, one `ws` CLI, one backend, one account and
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
incrementally. The `ws` CLI, `witself-server`, Helm chart, GitOps tree, release
workflows, and Pulumi-based `witself-infra` module are built in this repo.

## Docs

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
