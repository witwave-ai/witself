# Witself

Status: pre-implementation draft.

Witself is an agent self/identity store. It is being designed to let AI agents
and human operators manage their own memories and facts, access other agents'
memories and facts under declarative policy, organize agents into security
groups, and exchange durable messages with other agents — all through a safe,
auditable CLI, MCP, and API surface.

Witself is the sibling of Witpass. Witpass is an agent credential vault and
authenticator (secrets). Witself reuses the entire Witpass platform spine — one
core domain service behind thin CLI/MCP/API adapters, the same tenancy and token
model, the same deployment shape, and the same observability, release, and
billing apparatus — but swaps the secret payload for self-identity. Where Witpass
protects the *confidentiality* of secret material, Witself protects the
*integrity and authenticity* of identity data.

The product goal is a CLI-first self/identity service for agents:

- Named agents inside an operator-managed realm.
- Memories: free-form self-content with versioned edit history and a soft
  forget/restore lifecycle.
- Semantic recall by default: embedding-backed similarity search blended with
  keyword, tag, kind, and time filters.
- Facts: deterministic name→value identity cards, with a `primary` flag for
  identity anchors.
- Cross-agent access governed by evaluable, default-deny policy.
- Security groups that act as both policy subjects and policy targets and can own
  group-scoped shared memories and facts.
- Durable inter-agent messaging with delivery, ordering, and acknowledgement.
- First-class structured/plaintext identity export and round-trippable import.
- Identity references such as `witself://fact/email` and
  `witself://agent/archivist/memory/mem_...`.
- MCP compatibility for agent runtimes.
- Managed Witself Cloud by default.
- Public backend code and first-class self-hosting for operators who want to run
  Witself in their own cloud.

Managed Witself Cloud is the default supported product. Self-hosting is available
from the public repo, while production self-host support is a paid or contracted
support path once hardening docs and operational guidance are real.

## Repository Status

This repository is currently docs-first. Code, release workflows, images, Helm
charts, and Terraform modules will be added after the product and security
contracts are clear.

## Docs

- [Requirements](docs/requirements.md)
- [V0 Scope](docs/v0-scope.md)
- [Memory Model](docs/memory-model.md)
- [Facts Model](docs/facts-model.md)
- [Access Policy](docs/access-policy.md)
- [Security Groups](docs/security-groups.md)
- [Inter-Agent Messaging](docs/inter-agent-messaging.md)
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
- Helm chart: `ghcr.io/witwave-ai/charts/witself`

## Security

Do not file public issues for suspected vulnerabilities. See
[SECURITY.md](SECURITY.md) and [docs/security-policy.md](docs/security-policy.md).

## License

Witself is open source under the [Apache License 2.0](LICENSE).

The Apache-2.0 license covers the software. It does not grant rights to use the
Witself name, marks, logos, or branding except as allowed by the license for
describing the origin of the work.
