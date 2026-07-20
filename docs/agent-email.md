# Witself Agent Email

Status: draft. Kickoff spec, scoped 2026-07-20. This document is the
go-forward design for **agent email**: durable, addressable email identities
for named Witself agents on a Witself-managed domain, plus a separate
outbound-only platform-notification surface. It extends the sealed-plane
roadmap item for email-code 2FA in
[post-v0-roadmap.md](post-v0-roadmap.md#sms-and-email-code-2fa) and reuses the
durable-mailbox patterns of
[inter-agent-messaging.md](inter-agent-messaging.md) without joining its trust
domain. Nothing here changes the realm-local messaging contracts or the
cross-realm collaboration design in
[agent-collaboration.md](agent-collaboration.md).

## Settled Kickoff Decisions

Scoped by the operator at kickoff (2026-07-20):

- **Use cases, all in scope for the epic**: (1) verification links and
  email-OTP codes for accounts agents create, (2) service and transactional
  mail addressed to the agent, (3) human-to-agent correspondence, and
  (4) platform notifications from Witself to human operators.
- **V1 slice is receive-only.** No agent-authored outbound email ships in v1.
  Reply-only send and full outbound initiation are follow-on slices.
- **Addressing is a Witself-managed domain.** Bring-your-own inbox
  (IMAP/Gmail/M365 adapters) is deliberately deferred; self-hosted cells bring
  their own domain to the same pipeline.

A second requirements pass later the same day settled more:

- **Cloudflare is the inbound edge.** Cloudflare's email stack (Email Routing
  plus Email Workers) receives mail for the managed zones and relays each
  message to the owning cell's signature-verified ingestion endpoint. The
  current focus is receiving mail addressed to a specific agent.
- **Domain plan.** Interim: `witmail.witwave.ai`, a subdomain of the existing
  estate. Target: acquire and provision `witmail.ai`. Each realm gets its own
  subdomain derived from the realm's unique identifier, and the local part is
  the agent name: `scott@<realm-label>.witmail.ai`.
- **Send is confirmed but later.** Agents will eventually send — verification
  flows may force it sooner than correspondence does — and the design
  documents it now, but receive ships first and nothing in v1 depends on send.
- **Email is a billing point in both directions.** Sent and received mail are
  metered per period; sending stops hard at a per-period threshold; email is
  switchable on and off per agent and per realm; agent-originated spam
  prevention is a first-class requirement, not a slice-3 afterthought.
- **Attachments stay in Postgres.** V1 stores raw MIME, attachments included,
  directly in the database under hard size caps — no object store or
  file-management layer in this epic.

## Goal

Every named agent already has a durable, attributable self: memories, facts,
sealed credentials, an avatar, and a realm-local mailbox. Agent email gives
that self an address the *outside world* can reach. An autonomous agent that
creates its own service accounts (see the account-provisioning direction in
[client-custodied-agent-vault.md](client-custodied-agent-vault.md) and
[secret-model.md](secret-model.md)) needs somewhere to receive the signup
verification link, the password-reset mail, the receipt, and eventually the
email-OTP second factor. A human who works with an agent needs a front door
that is not a runtime-specific chat window.

Email is how agents reach **services and humans**. It is explicitly not an
inter-agent transport: same-realm agents use the durable mailbox in
[inter-agent-messaging.md](inter-agent-messaging.md), and cross-realm agents
use the collaboration substrate. Witself remains the channel for agent-to-agent
work, not an email bridge (see
[agent-collaboration.md](agent-collaboration.md#goal)).

## Architecture Stance

The standing platform invariants carry over unchanged:

- **The backend is model-free.** It terminates inbound mail through a provider
  adapter, verifies webhook signatures, parses MIME structure, records
  SPF/DKIM/DMARC results, stores, filters, meters, and returns data. Reading
  mail, deciding what it means, extracting anything semantic, and (in later
  slices) drafting replies are client-side inference in the active AI client.
- **No wake.** Inbound mail lands durably and waits. Witself and MCP never
  wake or launch an AI client; an offline agent's mail is processed on its
  next active foreground turn, exactly like the no-wake boundary in
  [autonomous-realm-messaging.md](autonomous-realm-messaging.md).
- **Email content is untrusted input.** Body, subject, headers, and
  attachments are data, never instructions, and carry no authority: receiving
  an email can never author a write, grant access, or authorize a deletion.
  This is the message-body stance from inter-agent messaging, strengthened
  because an external sender has no token-derived identity at all.
- **One core, multi-adapter.** Email is a new surface on the same spine —
  API, CLI, and MCP adapters over one core service, with shapes pinned in
  [json-contracts.md](json-contracts.md) once they settle.

## Sequencing

- **Slice 1 — receive-only core (v1).** Managed-domain address provisioning,
  inbound pipeline, durable per-agent mailbox with fenced foreground
  processing, metadata list/read/ack surfaces, verification-link and
  email-OTP consumption, spam/quarantine handling, retention, metering, and
  archive/export coverage. Human-to-agent mail arrives and is readable; the
  agent cannot reply yet.
- **Slice 2 — reply-only send.** An agent may reply within an existing inbound
  thread (no initiation), rate-limited, with outbound authentication
  (SPF/DKIM/DMARC) on the managed domain and complaint/suppression handling.
- **Slice 3 — full outbound.** Agent-initiated email with the complete
  anti-abuse program: reputation management, content policy, per-agent and
  per-realm send limits, and operator governance controls.
- **Parallel track — platform notifications.** Witself-authored operator email
  (billing, alerts, digests). Outbound-only, no agent mailbox involvement, and
  no model inference; it shares the outbound provider adapter and domain
  authentication work but none of the agent-mailbox semantics. It may ship in
  any order relative to the slices above.

Deliverability reality drives this order: receiving mail requires no sender
reputation, while sending is the largest abuse surface in the feature. V1
deliberately sidesteps it.

## Addressing And Domain Model

Agents receive addresses shaped `<agent-name>@<realm-label>.<base-domain>`, so
the realm anchors the address the same way it anchors identity, avatars, and
published signing keys. The base domain is `witmail.witwave.ai` in the interim
and `witmail.ai` once acquired and provisioned; both are Cloudflare-fronted
zones. `<realm-label>` is a DNS hostname label derived deterministically from
the realm's unique identifier — the derivation rule is an open question
because raw realm ids (`realm_...`) contain characters that are not valid in
MX-resolvable hostname labels. The local part is the agent name, subject to
the sanitization rules below.

Requirements regardless of format:

- Local parts and realm slugs are sanitized, collision-checked, and reserved
  words are blocked (`postmaster`, `abuse`, `admin`, and RFC-required roles
  route to the operator, not to agents).
- An address, once provisioned, is stable for the life of the agent; renames
  create a new address rather than silently rebinding the old one.
- Address counts per agent and per realm are plan-gated (see
  [billing-and-limits.md](billing-and-limits.md)).
- Self-hosted cells configure their own domain and inbound provider; the
  pipeline, mailbox semantics, and surfaces are identical (see
  [self-hosting.md](self-hosting.md)).
- Sending reputation must be isolatable per subdomain so a future outbound
  slice cannot poison inbound routing, and a noisy realm cannot poison the
  base domain.
- The interim-to-target domain cutover is a real migration: external services
  will hold `witmail.witwave.ai` addresses on file, so the interim domain must
  keep receiving (dual-domain routing) for a long deprecation window after
  `witmail.ai` activates. Every address a third party ever saw must keep
  working until its agent is done with the accounts behind it.

## Inbound Pipeline

Witself cells do not terminate SMTP. Cloudflare is the selected inbound edge:
Email Routing accepts mail for the managed zones, and an Email Worker relays
each message to the owning cell's signature-verified ingestion endpoint.
Cloudflare evaluates SPF/DKIM/DMARC at the edge and enforces a provider
message-size cap; both are recorded with the stored message. MX and routing
configuration follow the cell topology in
[deployment-cells.md](deployment-cells.md); the control plane stays thin and
never handles message content, consistent with the control-plane-only
provider-adapter precedent from billing.

Two Cloudflare constraints need verification during implementation: per-realm
*subdomain* routing must be confirmed against the zone plan (catch-all plus
Worker-side recipient parsing is the fallback), and Cloudflare's email stack
is receive-oriented — a future send slice needs a separate outbound provider,
and that dependency must not leak into the inbound design. The Email Worker
also needs a realm-to-cell routing map so mail lands in the owning cell; how
that map is published to and refreshed at the edge is an open question.

Pipeline contract:

- **Idempotent ingestion.** At-least-once webhook delivery deduplicated on the
  provider message id; replays are harmless.
- **Raw preservation.** The raw MIME message — attachments included — is
  stored directly in Postgres under a hard size cap aligned with the edge
  provider's message limit; parsing failures preserve the raw bytes and
  record a parse-error state rather than dropping mail. No object store or
  file-management layer is introduced in this epic: retention windows are the
  pressure valve on table growth, and an object-storage adapter is revisited
  only if measured volume demands it.
- **Parsed metadata.** From, to, subject, date, provider spam verdict, and
  SPF/DKIM/DMARC authentication results land as structured columns in
  Postgres alongside the immutable message row.
- **Quarantine.** Provider-flagged spam is retained separately with a shorter
  retention window and is excluded from checkpoint counts and default lists;
  surfaces expose it behind an explicit flag.
- **Attachments.** Attachment handling inherits the open concerns already
  flagged for messaging attachments in
  [post-v0-roadmap.md](post-v0-roadmap.md) — size limits, metering, diagnostic
  redaction, and an explicit injection and memory-poisoning review. V1 stores
  attachment bytes inline with the raw message in Postgres under the same cap
  but may gate retrieval until that review lands (open question).

## Trust Model

Inbound email inverts the messaging trust boundary and the design must never
blur the two:

- **Sender identity is unverified.** In agent messaging the sender is derived
  from an authenticated token. An email `From` header is a claim.
  SPF/DKIM/DMARC results are recorded and surfaced as advisory,
  domain-granularity evidence — never mapped to a Witself principal, never
  treated as authentication of a person or agent.
- **Separate surface, separate tables.** Email reuses the mailbox *patterns*
  of inter-agent messaging (immutable rows, delivery state, fences) but not
  its tables, tools, or contracts, so unverified external content can never
  ride the token-derived trust of the agent mailbox.
- **No authority, ever.** Verification links are followed and OTP codes are
  used by the client as part of a task the user or agent already authorized.
  An email asking the agent to do something is untrusted content to surface,
  not an instruction. Email never authorizes fact writes, memory deletion,
  secret reveals, or configuration changes.
- **Code consumption composes with the sealed plane.** An emailed OTP is
  transient, unlike a TOTP seed, but its handling follows the same posture as
  [totp-2fa.md](totp-2fa.md): deterministic, model-free extraction on the
  backend is permitted (pattern-based, no inference), the code is returned
  only through an explicit gated call scoped to one message, and codes never
  appear in logs, diagnostics, audit events, or stored plaintext state.
  Whether extraction is a distinct reveal-style ceremony or an ordinary
  read-derived tool result is an open question; the roadmap's sealed-plane
  carve-outs bound the answer.
- **Threat-model addition.** Inbound email is a new injection surface with
  attacker-controlled content arriving continuously and for free.
  [threat-model.md](threat-model.md) gains a section covering prompt
  injection via mail, address harvesting, mailbox flooding, spoofed
  verification mail (a service the agent never signed up for "confirming" an
  account), and quarantine-evasion, before v1 promotion.

## Mailbox Semantics

The receive-side lifecycle mirrors the proven messaging shape:

- Immutable message rows with per-mailbox delivery, read, and acknowledgement
  state, and fenced claim/renew/release/complete processing for foreground
  handling — the migration-0034/0036 pattern (generation fence, database-time
  lease, deterministic failure counting) applied to a new table family.
- Metadata-only, cursor-paginated list; explicit content read; separate read
  and ack, so "the client saw the metadata" and "the agent is done with this
  mail" remain distinct facts.
- A bounded, value-free checkpoint hint so active clients discover pending
  mail without polling content. Whether this is a new `email_checkpoint` lane
  in `self.show` or a fold into `message_checkpoint` is an open question; the
  working assumption is a separate lane with the same foreground policy
  (at most one lane handled per turn, user work first, no background service).
- Retention is plan-scoped: raw MIME and attachments age out by plan window;
  quarantined spam ages out faster; metadata and content-free audit events
  follow [audit-retention.md](audit-retention.md). Account archives include
  the mailbox (addresses, messages, state) with the same interrupt-on-import
  handling of active claims that messaging archives use.

## Surfaces

Sketch, to be pinned in [json-contracts.md](json-contracts.md) and
[cli-command-surface.md](cli-command-surface.md) /
[mcp-tools.md](mcp-tools.md) as shapes settle:

- CLI: `witself email address show`, `witself email list`, `witself email
  read`, `witself email claim|renew|release|complete`, `witself email ack`,
  `witself email code` (gated single-message OTP extraction).
- MCP: `witself.email.*` mirroring the CLI, with metadata-only list results
  and untrusted-content framing in every content-bearing tool description.
- API: routes on the one-core spine in [api-routes.md](api-routes.md);
  provider webhook endpoints are cell-local, signature-verified, and separate
  from the agent-facing API surface.

Platform notifications get an operator-plane surface (template + event
triggers) rather than agent tools; that design lands with its track.

## Abuse, Privacy, And Metering

Receive-only still carries real obligations:

- Per-mailbox inbound rate and size caps enforced at ingestion; overflow is
  rejected at the provider boundary, not silently dropped after storage.
- Sent and received message counts are billing points, metered per period
  through the existing value-free usage-telemetry patterns, alongside
  plan-gated address counts and stored bytes; platform notifications are cost
  of service and not user-metered.
- Email is switchable per agent and per realm: an operator or plan
  enforcement can turn receive — and later send — off independently without
  deprovisioning addresses.
- Sending, when it ships, stops hard at a per-period threshold. The cap is a
  backend-enforced gate, not client-side advice, because agent-originated
  spam is a first-class threat to the shared domain's reputation and to the
  platform; threshold accounting exists per agent and per realm from the
  first send slice.
- Content never appears in logs, metrics, or diagnostics; audit events for
  provision/ingest/read/ack/purge are content-free, matching messaging.
- Mailbox deletion purges content and attachments from live storage while
  preserving value-free usage events and rollups — the standing deletion
  posture. Export before purge remains available through account archives.
- The privacy, anti-abuse, retry, and delivery-failure obligations that
  [post-v0-roadmap.md](post-v0-roadmap.md#sms-and-email-code-2fa) named as
  the reason email-code 2FA was deferred are this document's checklist, not a
  reason to defer further.

## Non-Goals

- Not an inter-agent or cross-realm transport; agents talk to agents over the
  messaging substrate, full stop.
- No bring-your-own inbox (IMAP/Gmail/M365) in this epic; the provider
  adapter boundary should not preclude it later.
- No agent-authored outbound mail in v1; no marketing/bulk sending in any
  slice, ever.
- No server-side inference: no backend summarization, classification beyond
  deterministic spam-verdict pass-through, or auto-extraction of meaning.
- No automatic promotion of email content into facts, memories, or secrets;
  a human or the client's own authorized workflow decides what durable state
  to create, under the standing untrusted-content rules.
- Telephony/SMS remains a sibling roadmap item, not part of this epic.

## Open Questions

1. Realm-label derivation: the deterministic, hostname-safe, collision-free
   rule mapping a realm's unique identifier to its DNS label, and the
   agent-name-to-local-part sanitization rules.
2. Cloudflare verification items: subdomain routing support on the zone plan
   (vs catch-all plus Worker-side parsing), the Email-Worker-to-cell
   authentication shape, and how the realm-to-cell routing map is published
   to and refreshed at the edge.
3. `email_checkpoint` as a separate self.show lane vs a fold into
   `message_checkpoint`.
4. OTP extraction ceremony: reveal-gated like sealed-plane material vs
   ordinary gated read; audit shape either way.
5. Attachment exposure in v1: metadata-only vs gated retrieval, pending the
   injection review (storage itself is settled: inline in Postgres).
6. Retention windows per plan tier, the quarantine window, and the Postgres
   growth watermarks that would trigger revisiting object storage.
7. Platform-notification templating, locale posture, and which events email
   operators at all.
8. Whether slice 2 reply-only send needs per-thread human approval policy
   (operator-configurable) before an agent can reply to a human.
9. Outbound provider selection for the send slices (Cloudflare's stack does
   not cover arbitrary outbound); deferred until a send slice is scheduled.
10. Interim-to-target domain migration mechanics: the dual-domain receive
    window, address rewriting policy, and how long `witmail.witwave.ai`
    addresses must survive after `witmail.ai` activates.

## Relationship To Existing Docs

- [post-v0-roadmap.md](post-v0-roadmap.md): the SMS-and-email-code-2FA entry
  should point here once this draft is accepted; SMS stays deferred.
- [inter-agent-messaging.md](inter-agent-messaging.md) /
  [autonomous-realm-messaging.md](autonomous-realm-messaging.md): pattern
  source for mailbox, fences, checkpoints, and foreground policy; contracts
  untouched.
- [secret-model.md](secret-model.md) / [totp-2fa.md](totp-2fa.md) /
  [client-custodied-agent-vault.md](client-custodied-agent-vault.md): the
  account-provisioning flow email verification serves, and the sealed-plane
  carve-outs bounding code handling.
- [billing-and-limits.md](billing-and-limits.md): plan gating and metering.
- [threat-model.md](threat-model.md): gains the inbound-email injection
  surface before v1 promotion.
- [deployment-cells.md](deployment-cells.md) /
  [self-hosting.md](self-hosting.md): cell-local webhook termination and the
  self-host domain story.
