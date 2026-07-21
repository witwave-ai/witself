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

A Cloudflare verification pass later the same day settled the addressing and
inbound-edge open questions, revising two kickoff assumptions:

- **Address shape revised: the realm moves into the local part.** Cloudflare
  caps a zone at 30 domains configured for Email Routing or Email Sending
  combined (apex included) and offers no wildcard subdomain receive, so
  per-realm subdomains cannot scale. Addresses are
  `<agent-local-part>.<realm-label>@<base-domain>` on the zone apex, with an
  apex catch-all feeding an Email Worker. Cloudflare stays the inbound edge.
- **Domain plan revised: dedicated apex zone, no `witmail.witwave.ai`
  interim.** Whether catch-all works on a configured subdomain (vs only the
  zone apex) is undocumented, so v1 launches on a dedicated apex zone —
  accelerating the `witmail.ai` acquisition instead of passing through a
  subdomain of the `witwave.ai` estate.
- **Realm label settled**: the realm id body verbatim — strip `realm_`; the
  16-character lowercase-base32 body is a valid DNS label by construction.
- **Local-part sanitization settled**: a deterministic normalization pipeline
  that fails closed into an explicit operator-chosen override on collision or
  empty result. No silent renames, no auto-suffixing.
- **Edge-to-cell authentication settled**: Ed25519-signed relay webhooks
  verified by cells against a control-plane-published public key.
- **Outbound candidate update**: Cloudflare Email Sending (public beta April
  2026) replaces the kickoff assumption that a send slice would require a
  separate outbound provider; it is now the leading candidate.

An operator decision on 2026-07-21 set the launch receive domain:

- **Receive starts on `agent-mail.witwave.ai`.** V1 receives on
  `agent-mail.witwave.ai` — configured for Email Routing inside the existing
  `witwave.ai` zone — until `witmail.ai` is acquired and provisioned, then
  cuts over with a dual-domain receive window. The address shape is identical
  on both domains. Because Cloudflare documents catch-all at the zone apex
  only, verifying catch-all (or an equivalent full-coverage route) on a
  configured subdomain is a launch-gating spike (see Addressing And Domain
  Model for the fallback ladder).

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

Agents receive addresses shaped `<agent-local-part>.<realm-label>@<base-domain>`
— for example `scott.drz4xnv73ficcrko@witmail.ai` — so the realm still anchors
the address the way it anchors identity, avatars, and published signing keys,
but as a local-part segment rather than a subdomain. The subdomain shape from
kickoff was dropped after verification: Cloudflare Email Routing caps a zone
at 30 configured domains (apex plus routing/sending subdomains combined) and
has no wildcard subdomain receive, so per-realm subdomains cannot scale (see
Inbound Pipeline for the full findings). The launch receive domain is
`agent-mail.witwave.ai`, configured for Email Routing inside the existing
`witwave.ai` zone (operator decision, 2026-07-21), with `witmail.ai` — a
dedicated Cloudflare-fronted apex zone — as the durable home once acquired
and provisioned. The address shape is identical on both:
`<agent-local-part>.<realm-label>@agent-mail.witwave.ai` at launch, the same
local part at `witmail.ai` after cutover.

One engineering caveat gates the launch domain: Cloudflare documents
catch-all at the zone apex only, and whether catch-all (or an equivalent
full-coverage route) works on a configured subdomain is unverified. That
verification is a launch-gating spike. If it fails, the fallback ladder is:
run `agent-mail.witwave.ai` as its own Cloudflare zone if the account plan
permits subdomain zones; otherwise accelerate the `witmail.ai` acquisition
and launch on the apex directly. Per-address routing rules are not a
fallback beyond a small pilot fleet — Email Routing custom-address rules are
capped per zone (the spike should confirm the current cap) and the address
population grows with every agent.

**Realm label (settled).** `<realm-label>` is the realm id body verbatim:
strip the `realm_` prefix and use the remainder. Realm ids are minted as 80
crypto-random bits, base32-encoded and lowercased (`internal/id`), so the body
is always exactly 16 characters from `[a-z2-7]` — a valid DNS hostname label
and local-part atom by construction, deterministic, collision-free by id
uniqueness, and stable for the life of the realm. The label is opaque, which
is acceptable: these addresses mostly serve services, not human recall.
**Vanity realm labels (decided 2026-07-21, deferred past v1).** Realms will
eventually be able to claim a pretty alias label usable *in addition to* the
automatic id-body label — `scott.acme@…` alongside
`scott.drz4xnv73ficcrko@…`. The id-body label remains canonical, permanent,
and always live; a vanity label is an alias entry in the same routing
projection pointing at the same cell. Constraints the future design must
honor: a vanity label lives in a single shared namespace on the base domain
(first-come reservation with a reserved-word and anti-impersonation policy —
brand and service names are phishing surface); it must fit the settled
one-dot parse (same `[a-z0-9-]` grammar, no dots) and must not be a valid
16-character id-body form, so alias and canonical namespaces cannot collide;
its length reshapes the per-agent local-part budget (64-octet cap minus
label minus dot), so vanity length gets a hard cap; and the
address-permanence rule applies — once mail has been received at a vanity
address, releasing or transferring that label is constrained by the same
long deprecation contract as a domain cutover. V1 ships the id-body label
only.

**Agent local part (settled).** The agent-name-to-local-part rule must handle
arbitrary input — the API accepts any non-empty string as an agent name; only
CLI local selectors enforce a charset. Sanitization is deterministic:

1. Unicode NFKC normalization, then lowercase.
2. Map spaces, underscores, and dots to hyphens.
3. Strip every remaining character outside `[a-z0-9-]`.
4. Collapse consecutive hyphens; trim leading and trailing hyphens.
5. Enforce the length budget: the full local part must fit RFC 5321's
   64-octet limit, and `.` plus the 16-octet realm label leaves 47 octets for
   the agent segment.
6. Fail closed: an empty result, a length overflow, or a collision with any
   live or tombstoned address in the realm fails provisioning with an
   explicit error, and the operator supplies an explicit local part (same
   charset rules) recorded on the address record. No silent auto-suffixing —
   provisioning order never changes an address.

Sanitized segments can never contain a dot, so the address grammar is
unambiguous: after stripping any RFC 5233 subaddress tag (`+tag`), a valid
local part contains exactly one dot, separating agent segment from realm
label. Anything else is structurally invalid and rejected at SMTP time.

Requirements regardless of format:

- Reserved names are blocked at both the agent-segment and full-local-part
  level: the RFC 2142 role set (`postmaster`, `abuse`, `hostmaster`,
  `webmaster`, `noc`, `security`), `mailer-daemon`, `admin`, `root`,
  `noreply`/`no-reply`, and kin. RFC-required roles at the apex
  (`postmaster@witmail.ai`, `abuse@witmail.ai`) route to the operator, never
  to agents; the catch-all Worker matches these before applying the
  structural parse.
- An address, once provisioned, is stable for the life of the agent; renames
  create a new address rather than silently rebinding the old one. Released
  local parts are tombstoned, never recycled — an address a third party ever
  saw must not come to mean a different agent.
- Address counts per agent and per realm are plan-gated (see
  [billing-and-limits.md](billing-and-limits.md)).
- Self-hosted cells configure their own domain and inbound provider; the
  pipeline, mailbox semantics, and surfaces are identical (see
  [self-hosting.md](self-hosting.md)).
- Send reputation must not poison inbound routing: future sending uses
  separately onboarded sending domains, never the inbound apex MX. Because
  realms share the apex inbound domain, realm-level abuse containment is
  enforced by the backend hard caps and per-agent/per-realm kill switches
  settled at kickoff, not by DNS separation; per-realm sending subdomains
  would hit the same 30-domain zone cap and are not the isolation mechanism.
- The `agent-mail.witwave.ai` to `witmail.ai` cutover is a real migration:
  external services will hold launch-domain addresses on file, so
  `agent-mail.witwave.ai` must keep receiving (dual-domain routing) for a
  long deprecation window after `witmail.ai` activates. Every address a
  third party ever saw must keep working until its agent is done with the
  accounts behind it.

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

The kickoff verification items were resolved on 2026-07-20:

- **Per-realm subdomain routing is rejected.** A zone supports at most 30
  domains configured for Email Routing or Email Sending combined, apex
  included, and there is no wildcard subdomain receive; catch-all is
  documented at the zone apex only. Native per-realm subdomain configuration
  therefore cannot scale, which is what moved the realm label into the local
  part (see Addressing And Domain Model).
- **Settled topology: a full-coverage catch-all into an Email Worker.** On
  the `witmail.ai` apex this is the documented zone-apex catch-all; on the
  `agent-mail.witwave.ai` launch domain, catch-all on a configured subdomain
  is the launch-gating spike (see Addressing And Domain Model). The Worker
  first matches reserved/role addresses and routes them to the operator —
  explicit Email Routing rules ahead of the catch-all delivering to the
  operator support inbox; a handful of exact addresses, well inside rule
  caps — then parses the envelope recipient: strip any subaddress tag, split
  the local part on its single dot, resolve `<realm-label>` to the owning
  cell.
  Structurally invalid or unknown recipients are rejected during the SMTP
  transaction so the sender gets a bounce — never accepted and dropped.
- **Realm-to-cell routing map (settled): a KV projection.** The map
  (`realm-label` → cell ingestion endpoint) follows the locked control-plane
  directory shape: the control plane maintains a write-through Workers KV
  projection the Email Worker reads from its binding (propagation ~60 s,
  fine for realm placement); on a KV miss the Worker falls back to an
  edge-cached control-plane directory GET before rejecting. The control
  plane never handles message content — it only publishes routing facts.
- **Edge-to-cell authentication (settled): Ed25519 signed relay.** The
  Worker POSTs the raw MIME to the owning cell's ingestion endpoint with a
  detached Ed25519 signature over timestamp, provider message id, envelope
  recipient, destination-cell audience, and body digest (standard-webhooks
  style) — audience binding, so a capture replayed at a different cell never
  verifies. The private key lives as a Worker secret; cells verify against
  the control-plane-published public key, cached and pinned, with a bounded
  clock-skew replay window. Rotation is a dual-key overlap: publish the
  successor, re-sign, retire the predecessor. Compromise recovery is the
  same mechanism run fast: publish the successor and delist the compromised
  key; cells hard-fail on delisted keys and surface the attempt as a
  forged-relay event. No per-cell secret fan-out; self-hosted cells verify
  their own edge's key the same way.
- **Provider constraints recorded.** Inbound messages cap at 25 MiB, which
  bounds the Postgres raw-size cap below. Since July 2025 Cloudflare only
  forwards mail that passes SPF or DKIM; whether that gate also applies to
  Worker-delivered mail is a small implementation-time check — either way
  authentication results are recorded per message. Subaddress tags are
  preserved and stored with recipient metadata.
- **Send is no longer provider-orphaned.** Cloudflare Email Sending entered
  public beta in April 2026 (Workers Paid; 3,000 messages/month included,
  then $0.35 per 1,000; REST, Workers binding, and SMTP submission;
  suppression handling), so the send slices have an in-house leading
  candidate. That dependency still must not leak into the inbound design.

### Inbound SMTP Transaction Contract

Settled 2026-07-21 after gap review: the never-accepted-and-dropped
guarantee is only implementable inside the SMTP transaction, so the Worker
completes the whole verdict path while the sender's connection is open.

1. **Parse.** Case-fold the envelope recipient to lowercase before every
   match (RFC 5321 leaves local-part case to the receiver, and provisioning
   lowercases). The receive grammar is ASCII: `[a-z0-9-]` segments joined by
   exactly one dot, plus an optional subaddress tag. SMTPUTF8 local parts,
   quoted-string forms outside the grammar, and empty segments are
   structurally invalid — permanent reject.
2. **Route.** Resolve the realm label through KV, then the edge-cached
   directory fallback. Unknown realm: permanent reject (550).
3. **Relay-and-verdict.** The Ed25519-signed relay POST runs synchronously;
   the owning cell validates the agent segment against live mailboxes and
   returns a typed verdict the Worker maps to the SMTP reply: `accepted` →
   250; `unknown_recipient` → 550; `receive_disabled` → 550, deliberately
   indistinguishable from unknown so a kill switch never leaks mailbox
   state (and never defers forever); `over_cap` and `retry_later` → 451
   tempfail.
4. **Transient failure is always tempfail.** Cell unreachable, verdict
   timeout, directory-fallback failure, or any Worker exception maps to an
   explicit 451 — the sender's MTA is the retry mechanism, because
   Cloudflare does not queue or retry Worker relays. The handler wraps every
   path so no exception falls through to provider-default behavior.
5. **Re-homing safety.** A cell that no longer owns the realm answers with a
   not-mine verdict, which the Worker maps to 451; the sender retries after
   the routing projection has repointed. Mail is never imported by a cell
   that does not own the realm.
6. **Acceptance is a durable write.** 250 is returned only after the cell
   has committed the message row — the verdict doubles as the durability
   acknowledgment. Ingestion is idempotent on (provider message id, envelope
   recipient), which also gives multi-recipient fan-out its dedup key.
7. **Feasibility bounds go to the launch spike**: the in-transaction latency
   budget, Worker CPU/subrequest limits against the 25 MiB cap, and whether
   Cloudflare invokes the Worker once per recipient or once per message.

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
- **Parsed metadata.** From, to, subject, date, provider spam verdict,
  SPF/DKIM/DMARC authentication results, and the parsed recipient components
  (agent segment, realm label, and any subaddress tag) land as structured
  columns in Postgres alongside the immutable message row.
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
  account), quarantine-evasion, and the edge boundary — forged relay
  webhooks against cell ingestion endpoints and edge-key compromise recovery
  — before v1 promotion.

## Mailbox Semantics

The receive-side lifecycle mirrors the proven messaging shape:

- **Mail is realm data (settled 2026-07-21).** Mailboxes, messages, raw MIME,
  and processing state live in the owning realm's cell Postgres with the
  standard tenant scoping (`account_id`, `realm_id`, owner agent) — the same
  database and tenancy pattern as the realm's memories, facts, and agent
  messages. No separate mail store, ever: account archives include the
  mailbox stream, import/export round-trips it with the rest of the realm,
  and when a realm re-homes to another cell its mail moves with it (the edge
  routing map repoints; the data travels in the archive like everything
  else).
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

1. `email_checkpoint` as a separate self.show lane vs a fold into
   `message_checkpoint`.
2. OTP extraction ceremony: reveal-gated like sealed-plane material vs
   ordinary gated read; audit shape either way.
3. Attachment exposure in v1: metadata-only vs gated retrieval, pending the
   injection review (storage itself is settled: inline in Postgres).
4. Retention windows per plan tier, the quarantine window, and the Postgres
   growth watermarks that would trigger revisiting object storage.
5. Platform-notification templating, locale posture, and which events email
   operators at all.
6. Whether slice 2 reply-only send needs per-thread human approval policy
   (operator-configurable) before an agent can reply to a human.
7. Outbound provider confirmation for the send slices: Cloudflare Email
   Sending (public beta April 2026) is the leading candidate; confirm GA
   status, deliverability posture, and suppression semantics when a send
   slice is scheduled. Also settle then: the cell-to-provider send path and
   sending-credential custody, per-realm spoofing blast radius on the
   shared apex (any realm's compromise can send as any address unless
   sending is scoped), and how sending domains consume the 30-domain zone
   cap.
8. Domain cutover mechanics for `agent-mail.witwave.ai` to `witmail.ai`:
   the dual-domain receive window, address rewriting policy, and how long
   launch-domain addresses must survive after `witmail.ai` activates.
9. Launch-gating spike: verify catch-all (or an equivalent full-coverage
   route) on `agent-mail.witwave.ai` as a configured Email Routing subdomain
   of the `witwave.ai` zone, and confirm the current custom-address rule
   cap; on failure, follow the fallback ladder in Addressing And Domain
   Model. Same spike: the SMTP-transaction feasibility bounds (verdict
   latency budget, Worker CPU/subrequest limits vs the 25 MiB cap,
   per-recipient vs per-message Worker invocation) and whether the
   SPF-or-DKIM forwarding gate applies to Worker-delivered mail.
10. Vanity realm-label policy, when that deferred capability is scheduled:
    reservation and dispute rules, the reserved-word and anti-impersonation
    list, the vanity length cap, per-plan gating, and whether release or
    transfer is ever permitted given address permanence (see Addressing And
    Domain Model).
11. Edge observability and metering: rejected and tempfailed mail never
    reaches a cell, so Worker-side verdict counters (and their value-free
    export into the platform metrics plane) are the only visibility into
    edge drops; decide the mechanism before v1 promotion.

Settled on 2026-07-20 (formerly items 1–2): realm-label derivation and
local-part sanitization (see Addressing And Domain Model), and the Cloudflare
topology, edge authentication, and routing-map publication (see Inbound
Pipeline).

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
