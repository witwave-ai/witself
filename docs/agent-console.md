# Local Agent Console

Status: implemented as the loopback-only `witself dashboard` command. The
stable feature-status id remains `agent-dashboard`; “Agent Console” is the
product-facing name for this local presentation surface.

The canonical [Feature Status](feature-status.md) scorecard owns readiness.
This document defines what the Console presents and deliberately does not
present. [ADR 0004](decisions/0004-local-agent-dashboard.md) owns the local
process, browser-authentication, proxy, and redaction decision.

## Product Boundary

The Console is a cross-cutting presentation adapter over the existing public
API. Email, messaging, transcripts, memories, facts, secrets, identity, and
plans continue to own their storage, behavior, authorization, entitlements,
limits, retention, workers, provider integration, recovery, and rollout gates.
Showing one of those capabilities in the Console neither implements it nor
raises its readiness.

The browser does not mutate domain state. User-initiated sending or replying
to email, reading or processing received mail, sending or processing messages,
and creating, changing, curating, confirming, or deleting durable state remain
agent-driven CLI or MCP operations. Service-owned workers may continue to
advance durable lifecycle state; the Console passively reflects those changes
too. Its sole write is its own size-capped, validated theme preference.

## Presentation Matrix

| Panel | Presented | Deliberately absent |
|---|---|---|
| Overview | Agent identity, avatar, value-free checkpoints, salient-memory summaries, and memory/fact capacity | Domain mutations, account administration, billing, and feature-progress authority |
| Transcripts | Observational transcript inventory and entries | Append, retention-policy changes, and evidence mutation |
| Facts | Observational redacted inventory and history; one explicit exact reveal where authorized | Set, propose, confirm, reject, or delete |
| Memories | Redacted inventory, detail, version history, and evidence | Create, adjust, curate, supersede, forget, restore, or delete |
| Conversations | Passive message/thread metadata | Message bodies or payloads and listen/read/claim/acknowledge/send/reply actions |
| Email: Received | Managed address and receive state, account-wide storage capacity, sender and subject, receive/read/acknowledgement/processing state, raw-message size, attachment count, aggregate attachment-storage and retained-byte totals, retention warning, duplicate warning, and provider-supplied authentication/spam signals | Email ids, account/realm/owner/mailbox ids, decoded body, raw MIME or headers, per-attachment names, media types, or content bytes, cursors, claim fences, and read/listen/acknowledge/claim/complete/reply actions |
| Email: Sent | From, Reply-To, recipient, subject, request kind, durable outbox state, provider-neutral status/error metadata, and lifecycle timestamps | Send ids, account/realm/owner ids, reply-parent ids, submitted body, idempotency material, worker claims, provider ids or payloads, cursors, and send/reply/retry/cancel actions |
| Secrets | Names, field names and sensitivity flags, lifecycle, timestamps, counts, and public vault-binding metadata | Ciphertext, wrapped keys, field values, reveal, TOTP, lifecycle actions, and runtime injection |

Received senders, subjects, provider-supplied authentication/spam results, and
provider-neutral outbound error codes remain untrusted external data. The proxy
rebuilds both email projections through narrow allow lists and the browser
renders them only as text.

The sent lifecycle vocabulary is `queued`, `claimed`, `provider_started`,
`accepted`, `delivered`, `deferred`, `bounced`, `rejected`, `failed`,
`ambiguous`, or `canceled`. It reports the durable cell view; it does not invent
delivery success. Both Received and Sent are bounded newest-first projections
without identifier-bearing browser cursors.

## Entitlement And Compatibility States

The Console inherits exactly one agent token and never grants a feature. A
disabled account, disabled agent or realm layer, unenrolled agent, pre-feature
cell, or temporarily unavailable upstream produces an explicit disabled,
not-enrolled, or unavailable state instead of a generic broken panel. A later
plan or account-policy change takes effect through the same installed command;
no Console reinstall is required.

The Console may be accepted as a presentation surface while a domain feature
remains limited or dark, provided it accurately shows the applicable disabled
or unavailable state. Domain readiness remains on that domain's feature-status
row.

## Surface Taxonomy

- **Local Agent Console (`witself dashboard`)** — this document: one local
  per-agent browser surface on `127.0.0.1`, using that agent's token.
- **Fleet-admin TUI (`witself-admin dashboard`)** — a separate local terminal
  surface for cells, support, and fleet events. It is not an Agent Console and
  has fleet-admin authority.
- **Hosted console** — deferred. A hosted browser application would require a
  separate feature-status row and its own authentication, sessions, tenancy,
  CSRF, availability, observability, recovery, and managed-rollout evidence.
  It must not scrape or inherit trust from the local Console.

## Release Acceptance

Current release acceptance must retain macOS, Linux, and Windows evidence for:

- `serve`, browser authentication, all seven panels, live refresh, `status`,
  and conservative `stop`;
- both Received and Sent email projections, capacity and lifecycle changes,
  and strict absence of bodies, ids, provider payloads, and action targets;
- disabled, not-enrolled, pre-feature, and temporarily unavailable states;
- token isolation, redaction, untrusted-text rendering, SSE and pagination
  bounds, stale-registry recovery, and theme-preference persistence.

The feature remains conditional until that release-specific cross-platform
artifact is retained in the canonical scorecard evidence.
