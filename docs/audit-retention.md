# Witself Audit Retention

Status: draft. Decision: managed v0 keeps audit events for one year by default,
with plan-configurable retention later. Audit is part of the integrity product,
not only backend diagnostics.

Narrative-memory amendment (accepted 2026-07-14): the memory event registry
below now follows client-authored capture and curation plans. Autonomous
`memory.consolidated` and server-routed `remember` behavior are retired; see
[narrative-memory-and-curation.md](narrative-memory-and-curation.md).

Sealed-plane custody amendment (accepted 2026-07-18):
[ADR 0003](decisions/0003-client-custodied-agent-vault.md) and the
[client-custodied vault contract](client-custodied-agent-vault.md) supersede
KMS-rooted agent-secret, realm-KEK, and server-side-decrypt language below. The
backend holds no AVK key material, calls no KMS for agent secrets, and exposes
no decrypt or `server_side_decrypt` path. Ordinary infrastructure KMS and
storage-encryption references are unaffected.

Witself audit answers a different question than Witpass audit. Witpass audit
exists mostly to prove who *read* secret material (confidentiality). Witself
audit exists to prove who *changed*, *recalled*, *forgot*, or *messaged* identity
data, and under which policy (integrity and authenticity). The headline use of
the audit log is attributing cross-agent mutation, not detecting reads. See
[threat-model.md](threat-model.md).

This document is the authoritative registry of stable dotted `<resource>.<verb>`
audit-event names, grouped by family (platform, open plane, sealed plane,
collaboration & cells, and billing & support). Other docs reference these exact
spellings; the enumeration in [json-contracts.md](json-contracts.md) points back
here.

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
- Avatar SVG, description, visual specification, perceptual continuity
  fingerprint, content/locked-layer hashes, generation provenance, or prompts.
- Message bodies or structured message payloads.
- Embedding vectors or any derived vector data.
- Raw agent or operator tokens, or token-file contents.
- Sealed-plane material: secret field values, TOTP seeds, or generated TOTP
  codes. These are never embedded, recalled, in the self-digest, or
  plaintext-exported, and they are equally never written to the audit log — a
  `secret.reveal` or `totp.code` record proves the reveal happened, never the
  value revealed. See [secret-model.md](secret-model.md) and
  [totp-2fa.md](totp-2fa.md).
- Key material of any layer: CMK, per-realm KEK, or per-secret/field DEK
  (wrapped or unwrapped). A `key.rotated` record carries key ids and the KMS
  key identity only. See [key-hierarchy.md](key-hierarchy.md).

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

The implemented mutation registry is metadata-only:

- `memory.added`
- `memory.adjusted`
- `memory.superseded`
- `memory.forgotten`
- `memory.restored`
- `memory.reactivated`
- `memory.evidence.resolved`
- `memory.deleted`

Version events record only memory id, new version, and lifecycle state.
Evidence resolution records memory id/version, terminal evidence id, pending
evidence id, and resolution state. Permanent deletion records memory id,
receipt id, prior version, and version/evidence/relation/retry-shield counts.
No event contains memory content, tags, links, evidence locator/excerpt, query
text, content/scrub/shield hash, client provenance, reason text, or raw retry
key.

The deterministic deletion preview creates no state, preview resource, or audit
event. `memory.read` and `memory.recalled` remain target retrieval events; they
are not emitted by the current direct slice.

An explicit narrative capture emits `memory.added`. An atomic explicit fact
write emits the existing fact mutation event. The client chooses that route;
the server does not classify prose. A requested native-provider action is
outside Witself audit except for an optional value-free integration outcome.

### Memory Curation

- `memory.curation.requested`
- `memory.curation.started`
- `memory.curation.planned`
- `memory.curation.applied`
- `memory.curation.conflicted`
- `memory.curation.interrupted`
- `memory.curation.cancelled`
- `memory.curation.rolled_back`

Curation events may contain request/run/action ids, owner/scope ids, captured
generation, fencing generation number, plan revision, operation counts,
before/after version ids, cursor interval counts, runtime/model/recipe identity,
result code, and deciding policy id. They never contain content, transcript
text, evidence excerpts, plan payload/hash, vectors, raw query, lease token,
fence credential, or raw idempotency key. Lease renewals are metrics/usage
signals, not stable audit events.

### Session

- `session.started` (records identity hydration; no content beyond ids and counts)
- `session.ended` (records the persisted progress memory id of `kind=session` and
  open-goal count; never the summary text)

### Fact

- `fact.created` (a new fact created by name)
- `fact.updated` (an existing fact updated by name)
- `fact.deleted` (permanent content deletion; the event and receipt retain only
  ids, canonical address metadata, counts, and timestamps, never the fact value,
  evidence, source text, or raw idempotency key)
- `fact.primary_changed` — records the promotion and the matching demotion of the
  prior primary of the same logical kind, with both fact ids.
- `fact.candidate.withdrawn` — reserved target verb for curation rollback of a
  still-open, run-attributed candidate. The current rollback updates the
  candidate with curation attribution but does not emit this separate verb.

The `fact set` command and the `remember` upsert do not have their own event: a
name with no existing fact emits `fact.created`, and a name that already exists
emits `fact.updated`. (`fact.imported` covers the `ingest` path; see
[Identity Export and Import](#identity-export-and-import).)

### Avatar

- `avatar.generation.requested`
- `avatar.proposed`
- `avatar.activated`
- `avatar.evolved`
- `avatar.rejected`
- `avatar.generation.failed`
- `avatar.rolled_back`
- `avatar.reset`
- `avatar.policy.changed`
- `avatar.style.changed`
- `avatar.quota.changed`
- `avatar.payload.compacted`
- `avatar.style.rollout.completed`
- `avatar.style.rollout.superseded`

Avatar lifecycle events are transactionally coupled and value-free. They may
carry stable agent/realm ids, version and lineage numbers, status, subject form,
style identity, bounded reason code, and attempt count where applicable. They
never carry creative payload, prompts, hashes, client provenance, or raw
idempotency keys.

`avatar.quota.changed` is attributed to the operator and records prior/new count
and byte limits plus the rollback floor. `avatar.payload.compacted` is attributed
to the system and records the affected version numbers, compacted count,
`net_reclaimed_bytes`, resulting retained count/inclusive bytes, applied limits,
and rollback floor. Net reclamation includes fingerprints created or pruned and
is never negative.
Version numbers provide durable lifecycle attribution without exposing creative
payloads or their hashes. Compaction caused by either proposal creation or a
lowered quota is committed with that triggering mutation. A failed-closed quota
decision or exact idempotent replay emits no new compaction event.

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
- `message.processing.claimed`
- `message.processing.renewed`
- `message.processing.released`
- `message.processing.completed` (may carry the result message id, never
  body/payload)
- `message.request.opened`
- `message.request.offered`
- `message.request.declined`
- `message.request.selected`
- `message.request.claimed`
- `message.request.renewed`
- `message.request.released`
- `message.request.completed`
- `message.request.cancelled`
- `message.request.expired` (exactly once, with a system actor)

Message lifecycle metadata may include value-free `message_id`, agent ids,
kind, thread id, validated reply parent, backend-derived `causal_depth`, and—on
processing events—`processing_generation` and `failure_count`. Generation is a
fence; failure count is the migration-0036 durable deterministic-message-failure
counter. Neither is the client-local runner health field
`consecutive_failures`. Audit never includes subject text, body, payload, claim
id, lease, or idempotency material.

The opened-through-completed request events are agent-driven. Cancellation is
agent-driven for an explicit coordinator action or system-driven with
`reason_code=coordinator_deleted`; expiry emits `message.request.expired`
exactly once with a system actor. All request-coordination events use the same
content-free rule. Their bounded
metadata may include `request_id`, `opening_message_id`,
`coordinator_agent_id`, `agent_id`, `selection_id`, `generation`,
`failure_count`, `max_assignees`, and a completed `result_message_id`, as
applicable to the event. They never include opening or offer body/payload,
claim id, lease, or idempotency material.

### Identity Export and Import

- `identity.exported` (records scope and whether `sensitive` records were
  included; requires reason when sensitive)
- `identity.imported`
- `memory.imported` (per memory created via `ingest`; records the `source=import:<file>`
  provenance and source label, never the imported prose)
- `fact.imported` (per fact upserted via `ingest`; records `name`/namespace and the
  `source=import:<file>` provenance, never the value)
- `self.digest.emitted` (optional; records the requested format and byte budget of a
  `digest emit`, never the rendered identity content)

### Sealed Plane: Secrets

Sealed-plane events attribute mutation and, critically, *reveal* of secret
material — the confidentiality question Witpass audit existed to answer, now one
family within the integrity log. Every record names the owning agent or group
and the enclosing realm; none ever carries a field value (see
[Audit Content Rules](#audit-content-rules)). See
[secret-model.md](secret-model.md) and [encryption-model.md](encryption-model.md).

- `secret.created`
- `secret.updated` (records the changed-field set and new version, never values)
- `secret.renamed`
- `secret.copied`
- `secret.archived`
- `secret.restored`
- `secret.deleted` (guarded tombstone delete; metadata is exactly `agent_id`,
  `secret_id`, and `secret_revision`, with no secret value or deletion reason;
  secret metadata and field/DEK rows are removed, while a minimal value-free
  tombstone remains; irreversible purge of that tombstone is a separate future
  action)
- `secret.reveal` (the explicit, audited reveal ceremony for a single field;
  records the secret id, field id, and result, never the plaintext. Carries the
  `server_side_decrypt` flag — `true` when the server transiently unwrapped the
  DEK and returned plaintext over TLS for a token-only pod, `false` for the
  client-held-key decrypt path. See [key-hierarchy.md](key-hierarchy.md).)
- `secret.grant` (a grant to another agent or group; records grantee and scope)
- `secret.revoke` (requires reason)

### Sealed Plane: TOTP

TOTP enrollments are sealed-plane material colocated with a secret; the seed is
high-value and never leaves the envelope except through the audited reveal path.
See [totp-2fa.md](totp-2fa.md).

- `totp.enrolled`
- `totp.code` (generates a current code; the value-returning operation gated by
  `totp:code`. Records the secret id and result, never the code or seed; carries
  the `server_side_decrypt` flag with the same meaning as `secret.reveal`.)
- `totp.seed_revealed` (reveals the Base32 seed/otpauth URI for re-enrollment;
  the high-value reveal, gated by `totp:code`, never recording the seed itself)
- `totp.deleted`

### Sealed Plane: Key Management

- `key.rotated` (per-realm KEK rotation; re-wraps the affected DEKs without
  touching plaintext secrets, records key ids and KMS key identity only, never
  key material. See [key-hierarchy.md](key-hierarchy.md).)

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

### Collaboration (cross-realm)

Cross-realm collaboration events attribute the lifecycle of a federated
conversation/task and the federation-trust decisions that gate which peer realms
are accepted. Every record names the local realm and, where applicable, the
remote realm handle; none ever carries a message body, envelope signature, or
peer signing-key material (see [Audit Content Rules](#audit-content-rules)). See
[agent-collaboration.md](agent-collaboration.md).

- `conversation.started` (a cross-realm conversation/task opened; records the
  `thr_` conversation id, participant handles, and the local/remote realms,
  never turn content)
- `conversation.state_changed` (records the new state — for example
  `working`/`input_required`/`auth_required` — and the prior state, never
  content)
- `conversation.completed`
- `conversation.failed` (records the failure class only, never content)
- `conversation.canceled` (requires reason)
- `federation.peer_allowed` (a peer realm handle added to the deny-by-default
  federation allow-list; records the peer handle and the acting operator)
- `federation.peer_denied` (an inbound peer rejected by the allow-list, or a peer
  removed; the canonical record for refused cross-realm trust)
- `federation.consent_accepted` (per-conversation consent to accept an inbound
  peer for a specific conversation; records the peer handle and conversation id)
- `loop.suspended` (a conversation suspended by a hop/TTL/loop governor; records
  which governor tripped and the conversation id, never content)
- `budget.exhausted` (a conversation halted on its turn or cost budget; records
  the budget dimension and the conversation id)

### Deployment cells

Cell placement and tenant-migration events attribute where a realm/tenant lives
across the fleet of independent cells and any move between cells. These are
control-plane and managed-mode operations; records carry routing metadata only —
never tenant identity data, key material, or re-wrap plaintext (see
[Audit Content Rules](#audit-content-rules)). See
[deployment-cells.md](deployment-cells.md).

- `tenant.placed` (a realm/tenant assigned to a home cell; records the cell id
  and the placement basis — for example region or residency — never tenant data)
- `tenant.migration_started` (a cross-cell move begun; records source and
  destination cell ids and the migration id)
- `tenant.migration_completed`
- `tenant.migration_failed` (records the failure class and the migration id,
  never tenant data)

### Billing, Payment, and Support

Recorded as stubs in v0 until the managed service is enabled; capability-gated
operations still emit a deterministic record.

- `billing.subscription.created`, `billing.subscription.updated`,
  `billing.subscription.canceled`
- `billing.payment_method.added`, `billing.payment_method.removed`,
  `billing.payment_method.default_changed`
- `billing.invoice.created`, `billing.invoice.paid`,
  `billing.invoice.payment_failed`
- `billing.refund.created`
- `billing.crypto.quote.created`,
  `billing.crypto.checkout.started`,
  `billing.crypto.payment.confirmed`,
  `billing.crypto.payment.failed`,
  `billing.crypto.refund.created`,
  `billing.crypto.provider_event.reconciled`
- `support.ticket.created`, `support.ticket.commented`,
  `support.ticket.closed`, `support.bundle.created`

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
- [secret-model.md](secret-model.md)
- [totp-2fa.md](totp-2fa.md)
- [encryption-model.md](encryption-model.md)
- [key-hierarchy.md](key-hierarchy.md)
- [inter-agent-messaging.md](inter-agent-messaging.md)
- [agent-collaboration.md](agent-collaboration.md)
- [deployment-cells.md](deployment-cells.md)
- [billing-and-limits.md](billing-and-limits.md)
- [threat-model.md](threat-model.md)
- [json-contracts.md](json-contracts.md)
- [observability-and-operations.md](observability-and-operations.md)
- [backup-and-recovery.md](backup-and-recovery.md)
- [helm-chart.md](helm-chart.md)
- [storage.md](storage.md)
- [token-lifecycle.md](token-lifecycle.md)
- [operator-auth.md](operator-auth.md)
