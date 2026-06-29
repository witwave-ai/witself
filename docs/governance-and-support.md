# Witself Governance And Support

Status: draft. This document defines the initial public-code, self-hosting,
support, and contribution boundaries before implementation.

## Public Code Stance

Witself should be inspectable and self-hostable. The public repository should
contain the CLI, MCP adapter, backend API server, storage adapters,
authorization and policy logic, audit model, embedding-provider abstraction,
Helm chart, Terraform modules, and release definitions unless a clear security
or operational reason requires a split.

Public code is part of the trust model across both planes. It lets users,
customers, and security reviewers inspect how Witself stores, authorizes,
audits, embeds, recalls, and serves open-plane identity material — memories,
facts, policies, groups, and messages — and how it encrypts, key-manages, and
reveal-gates sealed-plane credential material — secrets and TOTP enrollments.

Witself proves two postures at once: the *integrity and authenticity* handling
of open-plane identity data, and the *confidentiality* handling of sealed-plane
secret material. The sealed plane is inspectable precisely so reviewers can
confirm the carve-outs hold — that secret values are never embedded, never
returned by semantic recall, never in the self-digest, and never in the
plaintext export, and that values surface only through the audited reveal
ceremony. See [encryption-model.md](encryption-model.md),
[key-hierarchy.md](key-hierarchy.md), and [secret-model.md](secret-model.md).

Realm-local inter-agent messaging is a v0 part of this trust surface. The
cross-realm collaboration components are post-v0 and join the same inspectable
trust surface when they land: the blind relay — a new always-on backend
component that forwards signed envelopes between realms and never inspects
bodies — and the global directory/control plane, which carries routing metadata
only. Public code lets reviewers confirm those carve-outs hold. See
[agent-collaboration.md](agent-collaboration.md).

## License Decision

Decision: Witself is source-available under the Functional Source License
(FSL-1.1-ALv2).

The FSL applies to the public repository as a whole, including the CLI, MCP
adapter, backend API server, storage adapters, embedding-provider abstraction,
Helm chart, infrastructure modules, release automation, and docs unless a later
file clearly states a different license.

Why this fits Witself:

- It lets users inspect, run, fork, modify, redistribute, and self-host the code
  for any Permitted Purpose, including internal commercial use.
- It includes an explicit patent grant.
- The single restriction is a Competing Use: making Witself available as a
  commercial product or service that substitutes for Witself or the managed
  Witself Cloud. This protects the managed-Cloud business that funds development
  without crippling self-hosting.
- Each release carries a Grant of Future License: two years after it is published
  it converts to the Apache License 2.0, so the protection is time-limited and
  the code becomes permissively open over time.

The root `LICENSE` file is the source of truth. The FSL is source-available, not
an OSI-approved open-source license.

## Trademark Boundary

The FSL covers the software, not the Witself brand.

Witself names, marks, logos, and branding should be reserved for the official
project and service. Forks and permitted self-hosted deployments may use the code
under the FSL, but they should not imply that they are Witself, Witself Cloud, or
an official Witwave-operated service.

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
- KMS provider configuration, key access, and key rotation for the sealed plane
  (`aws-kms`, `gcp-kms`, `azure-key-vault`, or `local-dev`), when the sealed
  plane is enabled. KMS loss makes secret values unrecoverable (crypto-shred)
  and does not affect the open plane; see [key-hierarchy.md](key-hierarchy.md)
  and [backup-and-recovery.md](backup-and-recovery.md).
- Object/blob storage for exports, attachments, and backups.
- Network ingress and TLS.
- Backups and disaster recovery, including vector data needed to restore
  semantic recall.
- Terraform state protection.
- Helm values and Kubernetes Secret management for agent token delivery.
- Federation governance (post-v0), when cross-realm collaboration is enabled:
  managing the deny-by-default allow-list / trust registry, publishing and
  rotating the signed realm card, and custody of the realm/agent signing keys
  and the federation signed-card private key. These are governed by the
  operator `federation:manage` scope; see
  [agent-collaboration.md](agent-collaboration.md).

Optional field-level encryption of `sensitive` facts is an open-plane capability,
not a core dependency; operators who enable it also own the associated key
material. This is distinct from the sealed plane: secrets and TOTP seeds are
always envelope-encrypted under KMS (CMK to per-realm KEK to per-secret/field
DEK) whenever the sealed plane is enabled — that is mandatory, not optional. A
credential belongs in the sealed plane as a secret, not as a `sensitive` fact;
see [secret-model.md](secret-model.md) and [facts-model.md](facts-model.md).

## Feature Boundary

Managed Witself Cloud and self-hosted deployments should share the same core
contracts across both planes: the open-plane memory, fact, policy, group, and
message contracts, and the sealed-plane secret, TOTP, reveal, grant, and
runtime-injection contracts, along with the shared agent, token, audit, and
reference contracts. The sealed plane is a defined v0 slice that may be staged
after the open-plane core; where it is enabled it ships the same contracts in
both deployment modes. See [secret-model.md](secret-model.md) and
[totp-2fa.md](totp-2fa.md).

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

Post-v0 cross-realm collaboration is a clear managed-vs-self-host split. The
blind relay and the global directory/control plane are managed infrastructure
that route signed envelopes and carry routing metadata only; self-hosted realms
federate through them rather than reimplementing them, while each realm still
owns its own federation allow-list and realm card. See
[agent-collaboration.md](agent-collaboration.md).

## Public Repo Safety

The public repository must not contain:

- Real Terraform state.
- Production `.tfvars`.
- Cloud credentials.
- Kubeconfigs.
- Database passwords.
- Embedding-provider API keys or credentials.
- KMS credentials, KMS configuration secrets, or local-dev passphrase files
  (`WITSELF_PASSPHRASE_FILE`).
- Sealed-plane key material — CMK, per-realm KEK, or per-secret/field DEK
  material — and any unwrapped DEKs.
- Raw secret values, plaintext secret fixtures, or plaintext reveal output.
- TOTP seeds (otpauth URIs or Base32 seeds) and TOTP QR images.
- Optional field-level fact-encryption key material (when that capability is
  used).
- Cross-realm identity-root material (post-v0) — realm and agent signing keys
  and the federation signed-card private key. Only the realm JWKS public keys
  are published, in the signed realm card.
- Raw Witself tokens (the `witself_at_` raw prefix).
- Payment provider credentials.
- Wallet credentials.
- Customer data.
- Real memory, fact, message, policy, group, or secret fixtures derived from
  customer or production data.
- Production identity exports.
- Encrypted secret backups containing real envelope blobs or KMS key identity.
- Production support exports and diagnostic bundles.

The repository should include `.gitignore`, secret scanning, and CI checks that
make these mistakes harder. Test fixtures must be synthetic. Two kinds of
material are especially easy to commit by accident and must be screened:
open-plane identity export, which is plaintext by default (see
[backup-and-recovery.md](backup-and-recovery.md)); and sealed-plane material —
note that raw secret values, TOTP seeds, and DEKs never appear in any export at
all (secret backup is encrypted-only, and the plaintext export excludes the
sealed plane), so any such plaintext in the repo is necessarily a mistake. See
[encryption-model.md](encryption-model.md) and
[secret-model.md](secret-model.md).

## Package And Artifact Ownership

Public artifacts should use Witwave-owned package and repository names:

- Source repo: `github.com/witwave-ai/witself`.
- Homebrew tap: `github.com/witwave-ai/homebrew-tap`.
- CLI/MCP image: `ghcr.io/witwave-ai/images/witself`.
- Backend image: `ghcr.io/witwave-ai/images/witself-server`.
- Helm chart: `ghcr.io/witwave-ai/charts/witself-server`.

Release ownership and signing credentials must be tightly controlled.

## Security Reporting

Vulnerability reporting is private. Report suspected vulnerabilities to
security@witwave.ai rather than through public issues or pull requests, and do
not include real tokens, memory content, fact values, message bodies, secret
values, TOTP seeds, key material, or customer data in the report. The disclosure process and threat framing are
tracked in [security-policy.md](security-policy.md) and
[threat-model.md](threat-model.md).

## Related Docs

- [security-policy.md](security-policy.md)
- [threat-model.md](threat-model.md)
- [self-hosting.md](self-hosting.md)
- [self-host-support.md](self-host-support.md)
- [access-policy.md](access-policy.md)
- [authorization-and-roles.md](authorization-and-roles.md)
- [secret-model.md](secret-model.md)
- [totp-2fa.md](totp-2fa.md)
- [encryption-model.md](encryption-model.md)
- [key-hierarchy.md](key-hierarchy.md)
- [inter-agent-messaging.md](inter-agent-messaging.md)
- [agent-collaboration.md](agent-collaboration.md)
- [api-contract.md](api-contract.md)
- [release-and-build.md](release-and-build.md)
- [terraform-infrastructure.md](terraform-infrastructure.md)
- [billing-and-limits.md](billing-and-limits.md)
- [backup-and-recovery.md](backup-and-recovery.md)
