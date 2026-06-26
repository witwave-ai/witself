# Witself Governance And Support

Status: draft. This document defines the initial public-code, self-hosting,
support, and contribution boundaries before implementation.

## Public Code Stance

Witself should be inspectable and self-hostable. The public repository should
contain the CLI, MCP adapter, backend API server, storage adapters,
authorization and policy logic, audit model, embedding-provider abstraction,
Helm chart, Terraform modules, and release definitions unless a clear security
or operational reason requires a split.

Public code is part of the trust model. It lets users, customers, and security
reviewers inspect how Witself stores, authorizes, audits, embeds, recalls, and
serves identity material — memories, facts, policies, groups, and messages.

Where the sibling product Witpass uses public code to prove the
*confidentiality* handling of secret material, Witself uses it to prove the
*integrity and authenticity* handling of identity data.

## License Decision

Decision: Witself is open source under the Apache License 2.0.

Apache-2.0 should apply to the public repository as a whole, including the CLI,
MCP adapter, backend API server, storage adapters, embedding-provider
abstraction, Helm chart, Terraform modules, release automation, and docs unless
a later file clearly states a different license.

Why this fits Witself:

- It is permissive and broadly accepted by enterprises.
- It allows users to inspect, run, fork, modify, redistribute, and self-host the
  code.
- It includes an explicit patent grant.
- It keeps adoption friction low for agent platforms, package managers, security
  reviewers, and self-hosted operators.
- It lets the managed Witself Cloud business compete on reliability, trust,
  support, integrations, billing, compliance posture, and hosted operations
  rather than license friction.

The root `LICENSE` file is the source of truth.

## Trademark Boundary

The Apache-2.0 license covers the software, not the Witself brand.

Witself names, marks, logos, and branding should be reserved for the official
project and service. Forks and third-party hosted services may use the code
under Apache-2.0, but they should not imply that they are Witself, Witself
Cloud, or an official Witwave-operated service.

A formal trademark policy can be added before public launch if forks, packages,
or third-party hosted services become likely.

## Contribution Boundary

Initial contribution posture should be maintainer-directed.

Before accepting outside contributions, add:

- Root `CONTRIBUTING.md`.
- Developer setup instructions.
- Test and lint requirements.
- Security review expectations.
- Developer certificate of origin or CLA decision.
- Code-of-conduct decision if public community contributions are expected.

Until then, public visibility should not imply that arbitrary pull requests are
accepted or that external contributors can influence security-sensitive
architecture — the authorization layer, the cross-agent policy engine, the
audit model, the messaging trust boundary, or the embedding pipeline — without
maintainer review.

## Support Boundary

Managed Witself Cloud support and self-hosted support are different products.

Managed Witself Cloud support may include:

- Account and billing help.
- Service availability issues.
- Managed backend incidents.
- Hosted payment flow issues, including crypto payment rails.
- Managed identity export, import, and recovery assistance.
- Managed support workflows.

Self-hosted support should be explicit by support level:

| Level | Support posture |
|---|---|
| Local development | Community or best-effort only. Not production support. |
| Self-host preview | Best-effort issue triage for public chart/server problems. No SLA. |
| Production self-hosted | Paid or contracted support only, after the production hardening prerequisites are real. [self-host-support.md](self-host-support.md) holds the canonical prerequisite list. |

Public code availability and self-hostability should not be presented as a
production SLA or production support entitlement.

Self-hosted operators remain responsible for:

- Cloud account security.
- Kubernetes cluster security.
- IAM and workload identity.
- Database operations, including the Postgres `pgvector` extension and stored
  embedding vectors.
- Embedding-provider configuration and credentials (`voyage`, `openai`, or
  `local-dev`).
- Object/blob storage for exports, attachments, and backups.
- Network ingress and TLS.
- Backups and disaster recovery, including vector data needed to restore
  semantic recall.
- Terraform state protection.
- Helm values and Kubernetes Secret management for agent token delivery.

Optional field-level encryption of `sensitive` facts is a capability, not a
core dependency; operators who enable it also own the associated key material.

## Feature Boundary

Managed Witself Cloud and self-hosted deployments should share the same core
memory, fact, policy, group, message, agent, token, audit, and reference
contracts.

Some managed-service features may be disabled, replaced, or unsupported in
self-hosted deployments:

- Billing commands.
- Hosted payment flows, including crypto payment rails.
- Witself support-ticket workflows.
- Managed abuse controls and rate limits.
- Managed plan limits.
- Internal Witself support/admin tools.

Clients must discover and report unsupported operations deterministically rather
than failing unpredictably. Unsupported operations return a deterministic
`unsupported_operation` response with capability context; see
[api-contract.md](api-contract.md).

Managed Witself Cloud billing attaches at the account level and rolls up usage
by realm, plan-based in v0. Self-hosted operators may replace managed billing
with local policy or their own billing system, and self-hosting must not require
Witself-managed billing.

## Public Repo Safety

The public repository must not contain:

- Real Terraform state.
- Production `.tfvars`.
- Cloud credentials.
- Kubeconfigs.
- Database passwords.
- Embedding-provider API keys or credentials.
- Optional field-level fact-encryption key material (when that capability is
  used).
- Raw Witself tokens.
- Payment provider credentials.
- Wallet credentials.
- Customer data.
- Real memory, fact, message, policy, or group fixtures derived from customer or
  production data.
- Production identity exports.
- Production support exports and diagnostic bundles.

The repository should include `.gitignore`, secret scanning, and CI checks that
make these mistakes harder. Test fixtures must be synthetic; because identity
export is plaintext by default (see
[backup-and-recovery.md](backup-and-recovery.md)), exported fixtures are
especially easy to commit by accident and must be screened.

## Package And Artifact Ownership

Public artifacts should use Witwave-owned package and repository names:

- Source repo: `github.com/witwave-ai/witself`.
- Homebrew tap: `github.com/witwave-ai/homebrew-tap`.
- CLI/MCP image: `ghcr.io/witwave-ai/images/witself`.
- Backend image: `ghcr.io/witwave-ai/images/witself-server`.
- Helm chart: `ghcr.io/witwave-ai/charts/witself`.

Release ownership and signing credentials must be tightly controlled.

## Security Reporting

Vulnerability reporting is private. Report suspected vulnerabilities to
security@witwave.ai rather than through public issues or pull requests, and do
not include real tokens, memory content, fact values, message bodies, or
customer data in the report. The disclosure process and threat framing are
tracked in [security-policy.md](security-policy.md) and
[threat-model.md](threat-model.md).

## Related Docs

- [security-policy.md](security-policy.md)
- [threat-model.md](threat-model.md)
- [self-hosting.md](self-hosting.md)
- [self-host-support.md](self-host-support.md)
- [access-policy.md](access-policy.md)
- [inter-agent-messaging.md](inter-agent-messaging.md)
- [api-contract.md](api-contract.md)
- [release-and-build.md](release-and-build.md)
- [terraform-infrastructure.md](terraform-infrastructure.md)
- [billing-and-limits.md](billing-and-limits.md)
- [backup-and-recovery.md](backup-and-recovery.md)
