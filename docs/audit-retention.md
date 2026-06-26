# Witself Audit Retention

Status: draft. Decision: managed v0 keeps audit events for one year by default,
with plan-configurable retention later. Audit is part of the integrity product,
not only backend diagnostics.

Witself audit answers a different question than Witpass audit. Witpass audit
exists mostly to prove who *read* secret material (confidentiality). Witself
audit exists to prove who *changed*, *recalled*, *forgot*, or *messaged* identity
data, and under which policy (integrity and authenticity). The headline use of
the audit log is attributing cross-agent mutation, not detecting reads. See
[threat-model.md](threat-model.md).

## Decision

Audit records are first-class, security-relevant, and retained on a defined
schedule across all three deployment modes.

Managed v0 defaults:

- Default audit retention: 365 days.
- Minimum managed retention target: 90 days, unless legal or abuse controls
  require longer.
- Retention is configurable per plan; higher plans may offer longer retention.
- Audit exports are available as redacted, machine-readable data where policy
  allows (see [Audit Export](#audit-export)).
- Audit retention and stored audit volume are metered billing dimensions (see
  [Metered Dimension](#metered-dimension)).

Self-hosted defaults:

- Default Helm values set audit retention to 365 days (see
  [Helm Values](#helm-values)).
- Operators may configure retention according to their own policy and storage
  budget, subject to a configurable minimum.
- Self-hosted operators own backup, archival, and deletion behavior for audit
  data. Witself does not silently delete below the configured floor.

Local development:

- Local audit retention is best-effort and tied to the local store file.
- Local mode is not the compliance retention model and is not metered.

Retention applies uniformly to all event families below. Witself does not
shorten retention for high-volume families (for example `memory.recalled` or
`message.delivered`); volume is handled through metering and plan limits, not
through silent truncation. See [Retention Modes](#retention-modes).

## Retention Modes

Retention behavior at the end of the window is configurable, mirroring the
Witpass model:

- `delete` (default) — audit rows older than the retention window are hard
  deleted on a scheduled sweep.
- `archive` — older rows are exported to the object/blob adapter (see
  [storage.md](storage.md)) before deletion, preserving a redacted,
  machine-readable archive. Archived data is no longer counted toward live
  stored-audit metering.
- `hold` — retention sweeps are suspended (for legal hold or active
  investigation). A hold is itself an audited operator action and is surfaced in
  `witself audit status`.

Modes are set per realm by an operator and may be overridden per plan in managed
mode. The sweep is idempotent, advisory-locked (same pattern as migrations; see
[backend-architecture.md](backend-architecture.md)), and emits its own
`audit.retention.swept` record summarizing counts only — never content.

## Audit Content Rules

Audit records describe *that* something happened and *who* did it under *what*
authority. They never carry the identity payload itself.

Audit records must never include:

- PII of any subject (agent, operator, or third party).
- Identity content: memory `content` or any portion of it.
- Fact values, including `sensitive` fact values.
- Message bodies or structured message payloads.
- Embedding vectors or any derived vector data.
- Raw agent or operator tokens, or token-file contents.

Audit records may include non-sensitive context such as:

- Actor agent (token-derived) and operator principal where applicable.
- Target agent or target group, and the enclosing realm.
- Record ids (`mem_…`, `fact_…`, `grp_…`, `pol_…`, `msg_…`, `agent_…`,
  `tok_…`) and the audit record id (`aud_…`).
- Operation name, result, and decision outcome (allow/deny).
- Deciding policy id for cross-agent and policy-gated operations.
- Memory `kind`/`tags`, fact `name`/namespace, `primary` flag transitions,
  `sensitive` markers (as booleans, never values).
- Reason string (required on cross-agent, destructive, and operator-override
  actions), request id, provider event id, plan/limit status, and redacted
  client context, and timestamp.

The same redaction rule binds error messages, structured logs, metrics labels,
and JSON responses; see [observability-and-operations.md](observability-and-operations.md)
and [json-contracts.md](json-contracts.md). A `sensitive` memory or fact is
referenced in audit by id and metadata only.

## Attribution Rules

Because Witself's threat model is integrity and authenticity, attribution is the
load-bearing part of every record:

- The actor is always derived from the authenticated token, never from an input
  field. A caller passing an agent name cannot forge the recorded actor.
- Cross-agent records name both the target agent and the actor agent, plus the
  deciding policy id — for example, "memory `mem_…` of agent A was pruned by
  agent B under policy `pol_…`". See [access-policy.md](access-policy.md).
- Message records derive `from` from the token; sender spoofing is structurally
  impossible and audit reflects the true sender. See
  [inter-agent-messaging.md](inter-agent-messaging.md).
- Operator-override actions are attributed to the operator principal and flagged
  as override, and are subject to the same `--reason` requirement as agent
  cross-agent actions.

## Required V0 Audit Events

V0 records audit events for the families below. Event names are stable, dotted,
and lower-case. They are stable enough for support, reconciliation, and customer
exports.

### Memory

- `memory.added`
- `memory.adjusted` (records changed-field set and new version number, never the
  content)
- `memory.read` (deterministic read by id; updates `last_accessed_at`)
- `memory.recalled` (semantic recall; records query metadata, result count, and
  whether recall was degraded, never the query text or content)
- `memory.forgotten` (soft delete / tombstone; reversible window)
- `memory.restored`
- `memory.deleted` (hard delete; guarded, requires reason)

### Fact

- `fact.set` (create or update by name)
- `fact.updated` (when distinguishing update from create is useful)
- `fact.deleted`
- `fact.primary_changed` — records the promotion and the matching demotion of the
  prior primary of the same logical kind, with both fact ids.

### Cross-Agent Access

Every cross-agent event is fully attributed: target agent, actor agent, and the
deciding policy id.

- `crossagent.read` (cross-agent read or semantic recall over a target)
- `crossagent.contributed`
- `crossagent.curated` (requires reason)
- `crossagent.forgotten` (requires reason; soft by default)

### Policy

- `policy.created`
- `policy.updated`
- `policy.deleted`
- `policy.access_denied` (default-deny outcome; the canonical record for refused
  cross-agent attempts)
- `policy.access_allowed` and `policy.tested` (recorded where decision logging is
  enabled; `policy test` is the dry-run access check)

### Security Group

- `group.created`
- `group.deleted`
- `group.member_added`
- `group.member_removed`
- `group.record_changed` (changes to group-owned memories/facts, attributed like
  cross-agent actions)

### Message

- `message.sent`
- `message.delivered` (per recipient on group fan-out)
- `message.read`
- `message.acked`

### Identity Export and Import

- `identity.exported` (records scope and whether `sensitive` records were
  included; requires reason when sensitive)
- `identity.imported`

### Authentication, Agent, and Token Lifecycle

- `auth.succeeded`
- `auth.failed`
- `agent.created`, `agent.renamed`, `agent.copied`, `agent.disabled`,
  `agent.enabled`, `agent.archived`, `agent.deleted`
- `token.created`, `token.rotated`, `token.revoked`, `token.use_failed`,
  `token.file_choice` (reuse vs rotate decision during setup)

See [token-lifecycle.md](token-lifecycle.md) and
[operator-auth.md](operator-auth.md).

### Operator and Account

- `operator.override` (flag carried on the overridden action; standalone record
  where the override is itself the event)
- `account.profile_changed`, `account.member_changed`, `account.role_changed`
- `limit.decision` (warn/throttle/block outcomes; see
  [billing-and-limits.md](billing-and-limits.md))

### Billing, Payment, and Support

Recorded as stubs in v0 until the managed service is enabled; capability-gated
operations still emit a deterministic record.

- `billing.subscription.created/updated/canceled`
- `billing.payment_method.added/removed/default_changed`
- `billing.invoice.created/paid/payment_failed`
- `billing.refund.created`
- `billing.crypto.quote.created`,
  `billing.crypto.checkout.started`,
  `billing.crypto.payment.confirmed`,
  `billing.crypto.payment.failed`,
  `billing.crypto.refund.created`,
  `billing.crypto.provider_event.reconciled`
- `support.ticket.created/commented/closed`, `support.bundle.created`

Billing and crypto records may include non-sensitive context such as account id,
realm id, invoice id, subscription id, payment provider, payment-method type,
crypto asset, network, amount, currency, status, and provider event id. They must
not contain raw payment details, card numbers, provider tokens, wallet seed
phrases, wallet private keys, or full wallet identifiers.

## Audit Export

`witself audit export` produces redacted, machine-readable audit data for the
realm where policy allows.

- Default format is JSON using the `witself.v0` schema version; line-delimited
  JSON is supported for streaming large ranges.
- Exports honor the same content rules as live records: no PII, identity content,
  fact values, message bodies/payloads, embedding vectors, or raw tokens.
- Exports are filterable by event family, actor agent, target agent/group,
  policy id, decision outcome, and time range.
- `audit:read` gates export; cross-realm export is operator-only and audited via
  `identity.exported` scope rules where it overlaps identity export.
- Archived audit (see [Retention Modes](#retention-modes)) is exportable from the
  object/blob adapter through the same command with an `--include-archived`
  selector.

Audit export is distinct from identity export; the latter carries memories and
facts in plaintext by design (see
[backup-and-recovery.md](backup-and-recovery.md)). Audit export never carries
identity payloads.

## Helm Values

Self-hosted retention is configured through chart values (see
[helm-chart.md](helm-chart.md)). Defaults match managed v0.

```yaml
audit:
  retention:
    days: 365            # default window; configurable, floored per policy
    mode: delete         # delete | archive | hold
    minDays: 90          # refuse to configure below this floor
    sweep:
      enabled: true
      schedule: "0 3 * * *"   # daily sweep; advisory-locked
    archive:
      enabled: false          # required true when mode=archive
      bucket: ""              # object/blob target for archived audit
  export:
    enabled: true
```

Notes:

- `mode: archive` requires `archive.enabled: true` and a configured `bucket`;
  the chart fails template rendering otherwise.
- Setting `days` below `minDays` is rejected at render time, not silently
  clamped.
- The sweep Job reuses the migration Job's advisory-lock pattern so concurrent
  replicas do not double-sweep.

## Metered Dimension

Audit is metered on two related dimensions, replacing the Witpass secret/TOTP
audit metering:

- Audit retention window (the configured `days`, contributing to plan
  eligibility and storage budgeting).
- Stored audit volume (live, non-archived audit rows per realm).

Metering rules:

- Volume is measured per realm and rolls up to the account, consistent with all
  other Witself dimensions (see [billing-and-limits.md](billing-and-limits.md)).
- Archived rows (mode `archive`) leave the live volume meter once archived.
- High-volume families (`memory.recalled`, `message.delivered`,
  `crossagent.read`) are metered like any other event; they are not exempt and
  not truncated.
- Overage on stored audit volume follows the plan's configured behavior:
  `warn`, `throttle`, or `block`, the same overage model used across dimensions.

The full set of metered dimensions (active agents, stored memories, stored
facts, recalls/reads, memory writes, embedding operations, vector storage,
cross-agent accesses, security groups, messages sent/delivered, audit retention,
and general API volume) is defined in
[billing-and-limits.md](billing-and-limits.md).

## Related Docs

- [requirements.md](requirements.md)
- [access-policy.md](access-policy.md)
- [inter-agent-messaging.md](inter-agent-messaging.md)
- [billing-and-limits.md](billing-and-limits.md)
- [threat-model.md](threat-model.md)
- [json-contracts.md](json-contracts.md)
- [observability-and-operations.md](observability-and-operations.md)
- [backup-and-recovery.md](backup-and-recovery.md)
- [helm-chart.md](helm-chart.md)
- [storage.md](storage.md)
- [token-lifecycle.md](token-lifecycle.md)
- [operator-auth.md](operator-auth.md)
