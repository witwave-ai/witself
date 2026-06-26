# Witself Billing And Limits

Status: draft. Decision: v0 billing is account-level, plan-based, usage-aware,
and not raw per-call billing at launch.

## Decision

The account is the billing target, and usage rolls up by realm. One paying
customer can run many realms, each holding many named agents, and the plan
attaches to the account.

V0 should meter meaningful usage internally, but charge primarily by plan tier.
This gives Witself enough data to understand real service load without making
the first pricing model feel like nickel-and-dime metering.

The first v0 release does not need live payment collection or full subscription
management. Billing, payment, crypto payment, and invoice commands may be
contract-shaped and capability-gated while the core product matures across both
planes — the open plane (realm, agent, memory, fact, policy, group, message, and
audit) and the sealed plane (secret and TOTP). The metered payload spans identity
usage and credential usage; the sealed-plane dimensions count events only and
never carry secret or seed values.

## Billing Model

V0 billing posture:

- Plan-based first.
- Usage-aware from the beginning.
- Account-level billing target; usage measured per realm.
- No required per-realm or per-agent invoice line items.
- No raw per-call billing at launch.
- Overage behavior is configurable by plan and dimension.

Plans should define soft and hard limits for:

- Active named agents.
- Stored memories.
- Stored facts.
- Memory recalls and reads.
- Memory writes (add/adjust).
- Embedding operations.
- Vector storage size.
- General data-at-rest storage size.
- Cross-agent accesses.
- Security groups.
- Messages sent and delivered.
- Stored secrets (sealed plane).
- Secret reads, including reveal events and reference resolution (sealed plane).
- TOTP code generation (sealed plane).
- Runtime injection through `witself run` (sealed plane).
- Total encrypted storage size for envelope-encrypted secret material (sealed
  plane).
- General managed-service API request volume.
- Audit retention and stored audit volume.

The five sealed-plane dimensions meter the credential plane only. They never
count toward, and are never derived from, the open-plane recall, embedding, or
digest paths: secrets and TOTP seeds are never embedded, recalled, placed in the
self-digest, or plaintext-exported, and their values surface only through the
reveal-gated paths (see [secret-model.md](secret-model.md) and
[encryption-model.md](encryption-model.md)).

Managed v0 should default to 365 days of audit retention. Longer retention can
be plan-based later, and self-hosted operators can configure retention according
to their own policy (see [audit-retention.md](audit-retention.md)).

## Metered Dimensions

Witself should meter these dimensions internally in v0:

| Dimension | Why it matters |
|---|---|
| `active_agent` | Principal count, plan shape, support burden. |
| `stored_memory` | Storage footprint and recall corpus size. |
| `stored_fact` | Identity-card inventory size. |
| `memory_recall` | Semantic search load and security-relevant access. |
| `memory_write` | Add/adjust load and integrity-relevant mutation. |
| `embedding_operation` | Embedding-provider cost and write-path load. |
| `vector_storage_byte` | pgvector storage cost and backup size. |
| `crossagent_access` | Cross-agent read/write load and security signal. |
| `security_group` | Group count, policy-evaluation surface. |
| `message_sent` | Outbound mailbox load and abuse control. |
| `message_delivered` | Fan-out delivery load (group fan-out multiplies this). |
| `storage_byte` | General open-plane data-at-rest footprint and backup size. |
| `stored_secret` | Sealed-plane inventory size and storage footprint. |
| `secret_read` | Sealed-plane sensitive access risk and service load (reveal + reference resolution). |
| `totp_code` | Sealed-plane sensitive login assistance and service load. |
| `runtime_injection` | Sealed-plane secret use without printing, service load. |
| `encrypted_storage_byte` | Sealed-plane envelope-encrypted storage cost and backup size. |
| `api_request` | General API burden and abuse control. |
| `audit_event` | Audit retention size and compliance cost. |

Recalls, embedding operations, cross-agent accesses, and messages must be metered
even if v0 pricing stays tiered. They create real backend load and
security-relevant usage signals — the integrity-and-authenticity signals of the
open plane.

On the sealed plane, secret reads, TOTP code generation, and runtime injection
must likewise be metered even when v0 pricing stays tiered. They create real
backend load and are the confidentiality-relevant usage signals of the
credential plane. Metering counts the event; it never records secret or seed
values, and it does not cause secrets to be embedded, recalled, or placed in the
self-digest (see [secret-model.md](secret-model.md) and
[audit-retention.md](audit-retention.md)).

Notes on a few dimensions:

- `memory_recall` covers semantic recall and plain read/get. Recall that runs
  over another agent's memories also increments `crossagent_access`.
- `embedding_operation` is incremented on write-time embedding and on explicit,
  audited re-embedding maintenance, not on plain reads (see
  [memory-model.md](memory-model.md)).
- `vector_storage_byte` is a sub-dimension of overall storage; it is metered
  separately because pgvector storage scales with corpus size and vector
  dimensionality and is the dominant storage cost driver for active realms.
- `message_delivered` can exceed `message_sent` because a message addressed to a
  security group fans out to current group members (see
  [inter-agent-messaging.md](inter-agent-messaging.md)).
- `storage_byte` measures ordinary open-plane data-at-rest footprint (memories,
  facts, and the rest of the open plane on RDS/disk), not envelope-encrypted
  secret material (see [storage.md](storage.md)).
- `encrypted_storage_byte` is the sealed-plane companion to `storage_byte`. It
  measures the envelope-encrypted secret bytes (ciphertext, wrapped DEKs, and
  attachments) governed by the CMK→per-realm KEK→per-secret/field DEK hierarchy.
  It is metered separately because the sealed plane is a distinct storage and
  KMS cost driver (see [encryption-model.md](encryption-model.md),
  [key-hierarchy.md](key-hierarchy.md), and
  [secret-size-and-attachments.md](secret-size-and-attachments.md)).
- `secret_read` increments on the reveal-gated value-returning paths only —
  `witself secret reveal` and value-returning reference resolution — never on
  plain metadata listing. `totp_code` increments on `witself totp code`, and
  `runtime_injection` increments when a secret is injected into a child process
  by `witself run` without being printed. None of these dimensions cause secret
  values to be embedded, recalled, placed in the self-digest, or
  plaintext-exported (see [secret-model.md](secret-model.md) and
  [totp-2fa.md](totp-2fa.md)).

## Canonical Dimension Names

The metered-dimension names above are the single canonical identifier for each
usage dimension. They are reused verbatim as:

- the keys of the `/v1/capabilities` `limits` object,
- the dimension/item keys in `/v1/billing/usage` output, and
- the `limit_dimension` Prometheus metric label (see
  [observability-and-operations.md](observability-and-operations.md)).

Whether a dimension is a point-in-time cap (`active_agent`, `stored_memory`,
`stored_fact`, `security_group`, `vector_storage_byte`, `storage_byte`,
`stored_secret`, `encrypted_storage_byte`) or a rate (`memory_recall`,
`memory_write`, `embedding_operation`, `crossagent_access`, `message_sent`,
`message_delivered`, `secret_read`, `totp_code`, `runtime_injection`,
`api_request`, `audit_event`) is conveyed by the limit object's fields
(`max`/`used` for caps; `unit`, `included`, `soft_limit`, `hard_limit` for
rates), not by the key name. Using one key across
all three surfaces lets a client join capability limits, usage items, and metrics
directly. Field shapes are pinned in [json-contracts.md](json-contracts.md).

## Overage Behavior

Overage behavior should be configurable per plan and dimension:

- `warn`: allow the action and emit a warning (no error).
- `throttle`: apply service-protection rate limiting. The action may be delayed
  and still succeed; when it must be rejected, return `rate_limited`
  (HTTP 429, `retryable: true`) with a `retry_after` hint.
- `block`: deny the action with `limit_exceeded` (HTTP 429, `retryable: false`).
  Retrying does not succeed until the plan is raised or the window resets.

Recommended defaults:

| Dimension | Default overage behavior |
|---|---|
| Active agents | `block` for hard plan cap, `warn` near cap. |
| Stored memories | `block` for hard cap, `warn` near cap. |
| Stored facts | `block` for hard cap, `warn` near cap. |
| Memory recalls/reads | `throttle` or `warn`; block only for abuse or hard caps. |
| Memory writes | `throttle` or `warn`; block only for abuse or hard caps. |
| Embedding operations | `throttle` or `warn`; block only for abuse or hard caps. |
| Vector storage size | `warn` near cap, `block` at hard cap. |
| General data-at-rest storage size | `warn` near cap, `block` at hard cap. |
| Cross-agent accesses | `throttle` or `warn`; block only for abuse or hard caps. |
| Security groups | `block` for hard cap, `warn` near cap. |
| Messages sent/delivered | `throttle` or `warn`; block only for abuse or hard caps. |
| Stored secrets | `block` for hard cap, `warn` near cap. |
| Secret reads | `throttle` or `warn`; block only for abuse or hard caps. |
| TOTP code generation | `throttle` or `warn`; block only for abuse or hard caps. |
| Runtime injection | `throttle` or `warn`; block only for abuse or hard caps. |
| Encrypted storage size | `warn` near cap, `block` at hard cap. |
| API requests | `throttle`. |
| Audit retention | `warn` and require plan/config change before retention loss. |

Limit responses should be deterministic and machine-readable so agents can
recover or ask for operator help. The error envelope, error codes, HTTP status,
and exit-code mapping are defined in [api-contract.md](api-contract.md); the
limit-error JSON shape is pinned in [json-contracts.md](json-contracts.md).

A `limit_exceeded` or `rate_limited` response should carry, at minimum: the
canonical `limit_dimension`, the `overage_behavior` in force, `used`,
`included`/`max`, the `soft_limit`/`hard_limit` that tripped, the `reset_at`
window when applicable, a `retry_after` hint for `rate_limited`, and a
recommended next command for the CLI/agent to surface.

## Crypto Payment Rails

Witself retains the full Witpass payment apparatus, including crypto rails, with
no Witself-managed wallet custody. Crypto payment support sits alongside
traditional payment methods rather than replacing them.

Posture:

- Provider-mediated checkout, invoice, or subscription payment only. There is no
  Witself-held wallet, no treasury management, and no on-chain custody in v0.
- Candidate rails: stablecoins such as USDC or USDT where a payment provider
  supports them, and native ETH as a source asset only when a provider can safely
  quote, confirm, and settle the payment.
- Witself prefers fiat or provider-managed stablecoin settlement over direct
  treasury management until there is a deliberate finance, tax, and compliance
  design.
- Witself must not collect wallet seed phrases, private keys, or raw wallet
  credentials in CLI flags, environment variables, config files, logs, support
  tickets, or billing metadata.
- A crypto quote has a finite validity window; settlement must reconcile against
  the provider event, and under/over/late payment is handled by the provider's
  reconciliation flow, surfaced through billing status.
- There is no Witself utility token for v0 or v1. A utility token is not required
  for account setup, billing, agent access, memory recall, fact reads, messaging,
  CLI use, or MCP use.

CLI-owned hosted flows:

- When a payment-provider or regulatory requirement demands hosted checkout,
  secure payment setup, bank authorization, SCA-style browser approval, or a
  crypto checkout page, the CLI owns the workflow rather than collecting payment
  data. It creates the session, shows or opens the URL on request, returns a
  resumable session ID, polls or watches status, and emits machine-readable
  completion state.
- Crypto checkout sessions are tracked through the same session surface as other
  hosted payment flows (`witself billing sessions show`).

## CLI Requirements

The CLI should expose:

- `witself billing show`
- `witself billing usage`
- `witself billing limits`
- `witself billing plans`
- `witself billing subscribe --promo-code`
- `witself billing payment-methods` (list/add/remove/default; hosted-flow
  initiation, never raw card or wallet collection)
- `witself billing crypto` (quote/checkout/status; provider-mediated, no wallet
  custody)
- `witself billing invoices` (list/show/download)
- `witself billing sessions show`
- `witself capabilities`

The full noun/verb surface and flag conventions are defined in
[cli-command-surface.md](cli-command-surface.md). Billing-impacting payment
changes require or prompt for an audit `--reason`; read-only billing commands do
not. Risky billing/payment mutations support `--dry-run`, which validates inputs,
authorization, conflicts, quotas, and provider prerequisites without persisting
state, creating hosted provider sessions, or charging payment methods.

Usage and limits output should include:

- Current plan.
- Account and per-realm rollup.
- Metered dimensions (canonical names).
- Used quantity.
- Included quantity.
- Soft/hard limit status.
- Overage behavior.
- Reset window when applicable.
- Recommended next command.

## Capability Requirements

Backends should report billing capabilities:

- Managed Witself Cloud: billing is expected to be supported as the product
  matures.
- Self-hosted: billing may be disabled, local-only, or wired to the operator's
  own billing system. Self-hosting must not require Witself-managed billing (see
  [self-hosting.md](self-hosting.md)).
- Local development: billing is normally unsupported or mocked.

Unsupported billing operations should return `unsupported_operation` with
capability context. Crypto payment is independently capability-gated: a backend
may support fiat billing while reporting crypto rails as unsupported.

V0 clients should treat billing capability discovery as authoritative. A command
shape can exist before the backend supports live billing or crypto settlement.

## Pricing Follow-Up

The exact plan names, prices, included quantities, and overage policy are still
business decisions. V0 should preserve pricing flexibility while collecting
enough usage data to make those decisions responsibly. The embedding-operation
and vector-storage dimensions in particular carry real provider cost and should
be observed before fixed inclusions are set. On the sealed plane, `secret_read`,
`totp_code`, and `encrypted_storage_byte` carry real KMS and envelope-storage
cost and should be observed on the same basis (see
[key-hierarchy.md](key-hierarchy.md)).

## Related Docs

- [requirements.md](requirements.md)
- [v0-scope.md](v0-scope.md)
- [cli-command-surface.md](cli-command-surface.md)
- [json-contracts.md](json-contracts.md)
- [api-contract.md](api-contract.md)
- [memory-model.md](memory-model.md)
- [inter-agent-messaging.md](inter-agent-messaging.md)
- [secret-model.md](secret-model.md)
- [totp-2fa.md](totp-2fa.md)
- [encryption-model.md](encryption-model.md)
- [key-hierarchy.md](key-hierarchy.md)
- [secret-size-and-attachments.md](secret-size-and-attachments.md)
- [storage.md](storage.md)
- [self-hosting.md](self-hosting.md)
- [implementation-plan.md](implementation-plan.md)
- [audit-retention.md](audit-retention.md)
- [observability-and-operations.md](observability-and-operations.md)
