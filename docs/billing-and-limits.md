# Witself Billing And Limits

Status: draft. Decision: v0 billing is account-level, plan-based, usage-aware,
and not raw per-call billing at launch.

Narrative-memory amendment (accepted 2026-07-14): Witself may meter vector
storage/search and curation records, but it performs no billable backend model
or re-embedding inference. Client model cost stays with the client under
[narrative-memory-and-curation.md](narrative-memory-and-curation.md).

## Decision

The account is the billing target, and usage rolls up by realm. One paying
customer can run many realms, each holding many named agents, and the plan
attaches to the account.

Transcript retention is the first implemented behavioral plan policy. Personal,
Professional, Team, and Enterprise default to 30, 90, 365, and indefinite
retention respectively. Account-specific admin exceptions are independent of
billing and follow `account override > plan default > missing/indefinite`; see
[transcript-retention.md](transcript-retention.md).

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

The implemented transcript-usage slice is deliberately upstream of this
billing design: immutable `usage_events` plus hourly/daily `usage_rollups` move
with the account and power `GET /v1/usage`. No Stripe object is the usage source
of truth. Realm/account billing aggregation and conversion into plan charges
remain deferred.

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
- Agent-email addresses, received/sent events, and inline raw-MIME storage.
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
| `vector_write` | Validation and persistence load for client-supplied vectors. |
| `vector_storage_byte` | Client-vector JSONB storage and backup size. |
| `crossagent_access` | Cross-agent read/write load and security signal. |
| `security_group` | Group count, policy-evaluation surface. |
| `message_sent` | Outbound mailbox load and abuse control. |
| `message_delivered` | Fan-out delivery load (group fan-out multiplies this). |
| `email_received` | Inbound agent-email volume and abuse accounting; never a victim-billed pilot charge. |
| `email_sent` | Future outbound agent-email volume and sender-reputation enforcement. |
| `email_address` | Provisioned live agent-email address count. |
| `email_storage_byte` | Inline raw-MIME storage and backup size, separate from general open-plane storage. |
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
- `vector_write` counts accepted client-supplied vector writes. The backend does
  not generate or regenerate vectors and therefore meters no model inference.
- `vector_storage_byte` is a sub-dimension of overall storage; it is metered
  separately because vector storage scales with corpus size and vector
  dimensionality and is the dominant storage cost driver for active realms.
- `message_delivered` can exceed `message_sent` because a message addressed to a
  security group fans out to current group members (see
  [inter-agent-messaging.md](inter-agent-messaging.md)).
- Cross-realm messages (post-v0 cross-realm collaboration) are metered on the
  same existing `message_sent` and `message_delivered` dimensions; a
  realm-qualified destination does not introduce a new billing dimension (see
  [agent-collaboration.md](agent-collaboration.md)).
- Agent email uses its own dimensions because external inbound abuse, outbound
  reputation, address allocation, and MIME storage have different controls from
  the realm-local mailbox. `email_received` is accounting-only for the
  authorized Cloudflare pilot: pilot provisioning and ingestion emit no
  billable usage event, have no quota/overage enforcement, and hostile inbound
  volume can never bill the recipient. The canonical dimension and unit names
  exist in the cell usage contract so later production metering cannot invent
  incompatible keys; emission remains disabled until authoritative abuse
  classification and production pricing are both pinned. `email_sent` remains
  dormant until a send slice exists.
  `email_address` counts live provisioned addresses. `email_storage_byte`
  measures inline raw MIME independently so a mail attachment cannot silently
  consume the ordinary `storage_byte` allowance. Production pricing and abuse
  exclusions must be pinned before either receive or send becomes billable; see
  [agent-email.md](agent-email.md).
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
`stored_fact`, `security_group`, `vector_storage_byte`, `email_address`,
`email_storage_byte`, `storage_byte`,
`stored_secret`, `encrypted_storage_byte`) or a rate (`memory_recall`,
`memory_write`, `vector_write`, `crossagent_access`, `message_sent`,
`message_delivered`, `email_received`, `email_sent`, `secret_read`, `totp_code`,
`runtime_injection`, `api_request`, `audit_event`) is conveyed by the limit
object's fields (`max`/`used` for caps; `unit`, `included`, `soft_limit`,
`hard_limit` for rates), not by the key name. Using one key across
all three surfaces lets a client join capability limits, usage items, and metrics
directly. Field shapes are pinned in [json-contracts.md](json-contracts.md).

## Cross-Cell Aggregation

Managed Witself runs as a fleet of independent cells, and an account may span
more than one cell — for example when its realms are placed in different regions
or residency zones (see [deployment-cells.md](deployment-cells.md)). Billing
stays account-level and the canonical dimensions above are unchanged. When an
account spans cells, per-realm usage is summed across the cells that hold the
account's realms, and those per-realm rollups aggregate into the single
account-level total that the plan attaches to.

Each cell meters its own realms locally on the canonical dimensions; the
account-level view is the sum of those per-cell contributions. A realm has a
single home cell, so per-realm usage is never double-counted across cells. The
control plane holds only the account/realm → home-cell mapping needed to drive
this aggregation; it carries routing metadata, not tenant usage data (see
[deployment-cells.md](deployment-cells.md)). Tenant migration moves a realm
between cells without changing the account it bills to.

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
| Agent-email addresses | `block` for the hard address cap, `warn` near cap. |
| Agent email received | No plan overage action in the limited pilot. A production default is blocked on authoritative spam/abuse classification and source-scoped enforcement; aggregate recipient traffic must never become a victim-billing or mailbox-starvation lever. |
| Agent email sent | `block` at the hard per-period threshold; sending remains dormant until a send slice exists. |
| Agent-email raw-MIME storage | No quota enforcement in the limited pilot. Production may `warn` near cap and `block` at hard cap only after abuse-excluded accounting and safe inbound tempfail behavior are pinned. |
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
- [deployment-cells.md](deployment-cells.md)
- [agent-collaboration.md](agent-collaboration.md)
- [self-hosting.md](self-hosting.md)
- [implementation-plan.md](implementation-plan.md)
- [audit-retention.md](audit-retention.md)
- [observability-and-operations.md](observability-and-operations.md)
