# Witself Agent Email

Status: capability-limited receive pilot live in the GCP sandbox as of
2026-07-22 (`v0.0.197`). One internal realm and seven exact-address routes are
enabled; durable receipt, provider-managed retry, owner processing,
disable/re-enable rollback, and the default-off exact-agent synthetic retry
proof have been exercised without changing the existing Cloudflare catch-all.
Schema 61 and the receive controls are live. The scheduled retry canary remains
disabled by default. This pilot does not add a sender-trust claim or automatic
code use.

Kickoff spec, scoped 2026-07-20. A capability-limited Cloudflare receive pilot
was authorized on 2026-07-21; the stronger production contract remains the
promotion target. This document is the go-forward design for **agent email**:
durable, addressable email identities
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

A whole-spec adversarial gap review on 2026-07-21 surfaced 7 blocking gaps;
all were settled in place (see the sections named):

- **Sender-auth results are signed relay metadata, not message headers** —
  the trust anchor the OTP flow depends on cannot be sender-forgeable
  (Inbound Pipeline, SMTP contract).
- **Mail is owner-agent-only** with `email:*` read/processing scopes and no
  operator content access in v1 (Surfaces).
- **OTP extraction is client-side** with sender-binding and single-use
  marking, resolving the model-free-backend / Non-Goals tension (Trust
  Model).
- **Hostile inbound volume never bills the victim**; spam/quarantine/abuse
  traffic is unmetered, and a disabled mailbox tempfails rather than
  permanently suppressing its address (Abuse, Privacy, And Metering; SMTP
  contract).
- **One name-derived address per agent** in v1, provisioned automatically
  (Mailbox Semantics).
- **Address/tombstone rows outlive the agent** so a re-created name cannot
  inherit a prior principal's mail (Mailbox Semantics).
- **Email gets its own billing dimensions**, including a dedicated
  `email_storage_byte` (Abuse, Privacy, And Metering).

An operator decision on 2026-07-21 set the launch receive domain:

- **Receive starts on `agent-mail.witwave.ai`.** V1 receives on
  `agent-mail.witwave.ai` — configured for Email Routing inside the existing
  `witwave.ai` zone — until `witmail.ai` is acquired and provisioned, then
  cuts over with a dual-domain receive window. The address shape is identical
  on both domains. Because Cloudflare documents catch-all at the zone apex
  only, verifying catch-all (or an equivalent full-coverage route) on a
  configured subdomain is a launch-gating spike (see Addressing And Domain
  Model for the fallback ladder).

The launch spike passed the basic receive path but failed the strict production
contract. A follow-up operator decision on 2026-07-21 authorized development of
a deliberately limited pilot rather than treating those production gaps as a
total provider no-go:

- **The strict contract is preserved.** Explicit temporary SMTP control,
  trusted structured sender-auth/spam metadata, a provider message id, and the
  full size/latency envelope remain requirements for production promotion.
- **The pilot is narrow and capability-honest.** It uses exact-address routes
  for one internal realm and 5–10 enrolled agents, marks every message
  unverified, excludes pilot receive from billing, and permits only expected,
  low-risk verification-code workflows. See Capability Tiers And Authorized
  Pilot for the full boundary.

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
  available SPF/DKIM/DMARC results, stores, filters, meters where authorized,
  and returns data. The limited pilot records unavailable authentication and
  spam results as `unknown` and excludes receive from billing. Reading
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

- **Pilot — Cloudflare-limited receive-only.** Build the slice-1 storage,
  ingestion, owner-only mailbox, and bounded code-consumption spine behind a
  default-off feature flag for one internal realm. The pilot limitations below
  are part of its contract, not TODOs hidden behind production-looking fields.
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

## Capability Tiers And Authorized Pilot

The 2026-07-21 Cloudflare spike was a failure of the **strict production
contract**, not a failure of basic email receipt. Cloudflare delivered real mail
to a Worker, matched the configured subdomain, invoked once per envelope
recipient, exposed the raw MIME stream, supported permanent rejection, and
retried deliberate Worker exceptions. Development may therefore proceed in two
explicit tiers. The pilot tier does not weaken or silently redefine the
production tier.

**Cloudflare limited receive-only pilot (authorized 2026-07-21):**

- One internal realm, with 5–10 explicitly enrolled agents. Each mailbox gets
  one exact Cloudflare Email Routing address rule pointing to the Worker. The
  existing zone-global catch-all, its action, and its destination remain
  unchanged; unknown addresses are outside the pilot and the pilot makes no
  claim that they receive the production `550 unknown_recipient` behavior.
- A default-off `agent_email_receive_pilot` feature flag plus a realm/agent
  allowlist gates provisioning, ingestion, and agent-facing surfaces. Merely
  possessing an address-like local part does not enroll an agent.
- The pilot cap is **5 MiB raw MIME**, below Cloudflare's 25 MiB provider cap.
  The Worker rejects an over-pilot-cap message before relay. Raw MIME may still
  contain attachments and is stored as one message, but neither raw MIME nor
  attachment content is retrievable through API, CLI, or MCP during the pilot;
  content reads expose bounded decoded text and an attachment count only.
- Success is returned only after the owning cell durably commits the message.
  On a cell timeout, transport failure, transient verdict, or unexpected
  exception, the Worker throws one deliberate **sanitized** exception and lets
  Cloudflare manage retry. The spike observed provider temporary-error retries,
  but the pilot neither promises a literal `451` nor depends on a documented
  retry count or schedule. No raw provider error or message content is placed
  in the exception.
- The signed edge envelope covers only fields the Worker can actually observe:
  timestamp, normalized envelope sender and recipient, destination-cell
  audience, raw size, and body digest. Provider message id, structured
  SPF/DKIM/DMARC results, and spam verdict are unavailable. Header-carried
  `Authentication-Results`, `Received-SPF`, provider trace ids, and spam headers
  remain untrusted message content and never fill those fields.
- Every pilot message is stored and surfaced as **sender unverified**, with
  authentication and spam states `unknown` and no authoritative provider id.
  Pilot receive is excluded from billable usage and quota enforcement. Value-
  free operational counters may still measure volume, bytes, errors, and
  latency, but they cannot become customer charges.
- Retry correlation uses a non-authoritative grouping key over the raw MIME
  SHA-256 digest, normalized envelope recipient, and normalized envelope
  sender. Matching keys mark messages as suspected duplicates for the owner;
  they never cause an automatic drop, overwrite, or content deletion. This is
  grouping, not the production idempotency guarantee.
- Verification-code use is allowed only when an active, user-authorized
  workflow is already waiting for mail from an expected service and the
  consequence is low risk. The client may read and extract a candidate code,
  but must present the sender as unverified and must not infer authenticity
  from message headers. Financial, identity-proofing, password/account
  recovery, domain or credential transfer, and other consequential automation
  are prohibited in the pilot; automated link following is disabled.
- A dedicated synthetic exact-route canary continuously proves both durable
  accept and provider-managed retry behavior. Promotion or continued operation
  requires recent canary success. The rollback is intentionally small: turn
  off `agent_email_receive_pilot`, stop provisioning and surfaces, disable or
  remove only the pilot exact-address rules, and leave the pre-existing global
  catch-all and its destination unchanged. Stored pilot mail follows normal
  retention/export policy rather than being destroyed by rollback.

### Controlled provider-retry proof

The stronger canary is separately default-off through
`WITSELF_AGENT_EMAIL_RETRY_CANARY_AGENT_ID`, which must equal one enrolled pilot
agent. The two control routes are exposed only to that exact agent's full token:
`POST /v1/email/retry-canary:arm` and
`POST /v1/email/retry-canary:status`. The runner also uses that token's ordinary
owner-only list, read, claim, processing, and acknowledgement routes to prove
the accepted message lifecycle. Both control routes accept one canonical
lowercase UUIDv4 in a JSON body, never a URL. Responses are value-free
cumulative checkpoints. No challenge, digest, address, message id, or content
enters logs, audit metadata, status output, or runner output.

The proof/arm row stores the challenge only as SHA-256. The challenge appears
only in the synthetic `X-Witself-Canary-Retry` header; a separate random
correlation nonce identifies the message through its subject, so neither the
subject nor body copies the challenge. After the retry is accepted, the opaque
UUID header remains ordinary synthetic `raw_mime` and is covered by normal
mailbox/archive policy. The first matching signed delivery atomically records a
value-free fingerprint over the normalized envelope, stable parsed fields,
decoded text projection, attachment count, and exact MIME body, then returns a
deliberate temporary verdict without inserting a message. A provider retry may
change volatile transport/authentication headers such as `Received`,
`DKIM-Signature`, or `Authentication-Results`; if the fingerprint is otherwise
unchanged, it inserts exactly one message and marks the proof accepted in the
same transaction. Later matching replays return that message without
duplication. Parse-invalid canary deliveries fail closed to the legacy exact
raw-body/envelope fingerprint rather than using the stable parsed projection.
While an arm is live, missing, malformed, mismatched, or changed-body attempts
tempfail. Once no unused arm is live, a malformed, unknown, expired, or
wrong-body `X-Witself-Canary-Retry` marker gets the fixed terminal cell verdict
`retry_canary_rejected`; the Worker maps only that exact verdict to its generic
permanent rejection and records the value-free `rejected_retry_canary` edge
outcome. This prevents both ordinary acceptance after tombstone cleanup and an
attacker-triggerable provider retry loop. Because the canary owner is a
dedicated synthetic mailbox, parse-invalid RFC 5322 is also terminally rejected
when no arm is live: the parser cannot safely prove that a physical retry
marker was absent.

An unused arm expires after 15 minutes. Once the first delivery tempfails, its
separate retry grace is 24 hours, so crossing the arm TTL cannot lose the
idempotency proof. A retained tempfailed proof remains independently retryable
but does not block the next run from arming a new challenge; only one unused
`armed` challenge may exist at a time. An unaccepted proof then becomes an
expiring tombstone; bounded cleanup runs opportunistically after seven
additional days. A retry after grace is terminal even after that tombstone is
cleaned. Accepted proofs remain attached to their accepted message and move
with it in account archives. Unused arms and tempfailed proofs never move
between cells.

The runner uses a distinct opaque correlation nonce in the subject and keeps
the proof challenge out of both subject and body. After the accepted checkpoint
is proven, it passively traverses bounded newest-first owner-mailbox pages with
an opaque cursor; it does not use the oldest-first listen surface, so more than
100 older unacknowledged messages cannot hide the new canary.

Mixed versions are unsafe for arming: a replica without the exact canary config
would ordinary-accept the first delivery. Keep the schedule off, deploy
schema-61-capable code with the canary agent unset, wait for every pod to
converge, then add the exact agent in a config-only rollout and wait again.
Only then arm/send manually; enable the schedule only after that run proves the
fixed edge sequence `tempfail_cell_response` / `response` / `503`, then
`accepted`. Rollback reverses this carefully: disable the schedule, settle the
unused arm or let its 15-minute TTL expire, and only then unset the agent or
downgrade.

The 15-minute workflow schedule is intentionally gated off. A successful run
acknowledges but does not delete its synthetic message, so enabling that cadence
retains about 96 messages per day until mailbox retention/delete is settled.

**Production receive-only contract:** the Inbound SMTP Transaction Contract
below remains the target. Promotion beyond the internal pilot, catch-all Worker
cutover, messages above 5 MiB, billable receive, sender-auth-dependent behavior,
or consequential OTP/link automation stays blocked until the provider path (or
a replacement inbound edge) supplies explicit temporary SMTP semantics,
trusted structured authentication/spam metadata, a stable provider identity,
and the size/latency feasibility evidence required by that contract.

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

One engineering caveat gates production use of the launch domain: Cloudflare
documents catch-all at the zone apex only. The launch spike established that
the zone-global catch-all covers the configured subdomain, but routing that
catch-all to the Worker would also move existing apex traffic. The limited
pilot therefore uses exact-address rules and leaves the catch-all unchanged.
Before production cutover, the fallback ladder remains:
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
message-size cap. The production contract requires edge results to be signed
and recorded with the stored message; the limited pilot cannot obtain those
structured results and records them as `unknown`. MX and routing
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
- **Production topology: a full-coverage catch-all into an Email Worker.** On
  the `witmail.ai` apex this is the documented zone-apex catch-all; on the
  `agent-mail.witwave.ai` launch domain, the spike confirmed that the
  zone-global catch-all covers the configured subdomain but cannot be moved
  without also moving existing apex traffic (see Addressing And Domain Model).
  The Worker
  first matches reserved/role addresses and routes them to the operator —
  explicit Email Routing rules ahead of the catch-all delivering to the
  operator support inbox; a handful of exact addresses, well inside rule
  caps — then parses the envelope recipient: strip any subaddress tag, split
  the local part on its single dot, resolve `<realm-label>` to the owning
  cell.
  Structurally invalid or unknown recipients are rejected during the SMTP
  transaction so the sender gets a bounce — never accepted and dropped.
  The limited pilot is the explicit exception: exact-address rules feed the
  Worker for enrolled agents while the pre-existing global catch-all remains
  unchanged.
- **Realm-to-cell routing map (settled): a KV projection.** The map
  (`realm-label` → cell ingestion endpoint) follows the locked control-plane
  directory shape: the control plane maintains a write-through Workers KV
  projection the Email Worker reads from its binding (propagation ~60 s,
  fine for realm placement); on a KV miss the Worker falls back to an
  edge-cached control-plane directory GET before rejecting. The control
  plane never handles message content — it only publishes routing facts.
  The limited pilot intentionally does **not** bind its content-handling Worker
  to the existing control-plane `DIRECTORY` namespace. That namespace contains
  provisioning, administrative, and token indexes outside the email Worker's
  authority. The isolated `witself-agent-email-pilot` Worker receives only an
  email-specific `EMAIL_DIRECTORY` KV projection containing its default-off
  pilot config and the 5–10 literal recipient routes. It has no HTTP route,
  control-plane container binding, or catch-all mutation capability. A later
  production projection may preserve the directory shape, but it must keep the
  same least-privilege content-plane separation.
- **Edge-to-cell authentication (settled): Ed25519 signed relay.** The
  Worker POSTs the raw MIME to the owning cell's ingestion endpoint with a
  detached Ed25519 signature over timestamp, provider message id, envelope
  recipient, destination-cell audience, the edge-evaluated SPF/DKIM/DMARC
  results, the provider spam verdict, and the body digest (standard-webhooks
  style) — audience binding, so a capture replayed at a different cell never
  verifies, and authentication/spam results are covered by the signature so
  they are the cell's sole trust anchor for sender authenticity (the cell
  never trusts message-header trace fields; see the SMTP contract). The private key lives as a Worker secret; cells verify against
  the control-plane-published public key, cached and pinned, with a bounded
  clock-skew replay window. Rotation is a dual-key overlap: publish the
  successor, re-sign, retire the predecessor. Compromise recovery is the
  same mechanism run fast: publish the successor and delist the compromised
  key; cells hard-fail on delisted keys and surface the attempt as a
  forged-relay event. No per-cell secret fan-out; self-hosted cells verify
  their own edge's key the same way.
  During the limited pilot the same signature and audience binding protect a
  reduced envelope containing only Worker-observable fields; unavailable
  provider identity, authentication, and spam fields are represented as
  absent/unknown, never synthesized from MIME headers.
- **Provider constraints recorded.** Inbound messages cap at 25 MiB, which
  bounds the Postgres raw-size cap below. Since July 2025 Cloudflare only
  forwards mail that passes SPF or DKIM, and the spike confirmed that the
  authentication stage precedes Worker delivery. The Worker event does not
  expose those structured results, so the production relay cannot yet record
  them authoritatively; pilot rows use `unknown`. Subaddress tags are preserved
  and stored with recipient metadata. The pilot imposes its own 5 MiB cap.
- **Send is no longer provider-orphaned.** Cloudflare Email Sending entered
  public beta in April 2026 (Workers Paid; 3,000 messages/month included,
  then $0.35 per 1,000; REST, Workers binding, and SMTP submission;
  suppression handling), so the send slices have an in-house leading
  candidate. That dependency still must not leak into the inbound design.

### Inbound SMTP Transaction Contract (Production Target)

Settled 2026-07-21 after gap review: the never-accepted-and-dropped
guarantee is only implementable inside the SMTP transaction, so the Worker
completes the whole verdict path while the sender's connection is open.
This contract remains mandatory for production promotion. The authorized
limited pilot uses the documented exception-and-retry downgrade above and does
not claim compliance with steps 3–8 where Cloudflare lacks the required
control or metadata.

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
   returns a typed verdict the Worker maps to the SMTP reply:
   - `accepted` → 250 (only after the durable write — see step 6);
   - `unknown_recipient` → 550 permanent;
   - `receive_disabled` (kill switch or plan enforcement) → **451 tempfail
     within a grace window** (operator decision 2026-07-21). A disabled
     mailbox defers rather than hard-bouncing, so external services do not
     convert a bounce into permanent address suppression; if the mailbox is
     still disabled when the grace window lapses, the sender's own MTA
     produces the eventual bounce. The 451 is deliberately the same shape as
     a transient failure, so a kill switch never leaks a distinct
     mailbox-state signal to senders;
   - `over_cap_transient` (per-period inbound cap; will free up) → 451;
   - `mailbox_full` or other permanent-refusal conditions → 550. `over_cap`
     must be split at the cell into transient vs permanent so the Worker
     never maps a recoverable cap to a permanent bounce.
4. **Transient failure is always tempfail.** Cell unreachable, verdict
   timeout, directory-fallback failure, or any Worker exception maps to an
   explicit 451 — the sender's MTA is the retry mechanism, because
   Cloudflare does not queue or retry Worker relays. The handler wraps every
   path so no exception falls through to provider-default behavior.
5. **Re-homing safety.** A cell that no longer owns the realm answers with a
   not-mine verdict, which the Worker maps to 451; the sender retries after
   the routing projection has repointed. Mail is never imported by a cell
   that does not own the realm.
6. **Acceptance is a durable write, idempotent across retries.** 250 is
   returned only after the cell has committed the message row — the verdict
   doubles as the durability acknowledgment. Ingestion is idempotent on
   (provider message id, envelope recipient): if the durable write commits
   but the 250 is lost and the sender's MTA retries, the cell recognizes the
   committed key and re-returns 250 without creating a duplicate. The same
   key gives multi-recipient fan-out its dedup semantics.
7. **Recorded authentication is signed metadata, never sender headers.** The
   SPF/DKIM/DMARC results and the provider spam verdict the cell stores come
   only from the signed relay envelope (step added to the Ed25519 field set
   below), never parsed from message headers. The cell strips or renames any
   inbound `Authentication-Results`, `Received-SPF`, and `X-Spam-*` trace
   headers before storage so a sender cannot pre-inject a forged
   `dkim=pass header.d=github.com` that later reads as genuine evidence.
8. **Feasibility bounds go to the launch spike**: the in-transaction latency
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

The pipeline items above describe the production requirements. In the pilot,
provider-id idempotency is replaced by non-destructive suspected-duplicate
grouping, raw MIME is capped at 5 MiB, structured auth/spam fields are
`unknown`, quarantine classification is unavailable, and attachment retrieval
and raw-MIME reads are disabled even though attachment bytes remain inside the
stored raw MIME.

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
- **Code consumption is client-side extraction over a bounded read
  (settled 2026-07-21).** An emailed OTP is attacker-controlled prose, not a
  platform-owned seed, so pattern-matching it is not the model-free
  backend's job — doing it on the backend would contradict this doc's own
  Non-Goal ("no auto-extraction of meaning") and overstate the
  [totp-2fa.md](totp-2fa.md) analogy (that computes a code from an enrolled
  seed; this reads a number out of untrusted text). The backend surface is
  an ordinary scoped read of one message; the active client extracts the
  code with its own inference or a conservative local deterministic helper.
  The first helper scans the subject followed by the same UTF-8-safe first
  64 KiB decoded-text projection visible through MCP `email.read`. It
  recognizes only locally keyword-associated standalone ASCII numeric
  candidates of 4–8 digits, excludes URL-embedded values, collapses duplicates
  with occurrence counts, and preserves first-seen order. It returns at most
  32 distinct candidates. A truncated content projection or candidate overflow
  forces `ambiguous`, regardless of how many returned values are visible; an
  unparsed message fails as unavailable rather than reporting a false `none`.
  It never selects or uses a candidate, follows a link, or calls
  `code.consume`. Two requirements the client flow must meet, because email OTP
  is a live attack surface:
  - **Sender binding at point of use.** The read result carries the signed
    authentication results and the `From` domain, and the consuming flow
    asserts the expected service/sender before using a code — otherwise an
    attacker who knows the address races a look-alike "your code is NNNNNN"
    message and the client consumes the wrong one.
  - **Single-use marking.** Consuming a code marks that message
    code-consumed, so a repeated call or a prompt-injected re-extraction is
    visible as an anomaly rather than silently re-revealing it.
  Unlike a sealed secret, the code is not separately stored: it lives inside
  the stored message like any other content (see the plaintext-at-rest note
  under Abuse, Privacy, And Metering), and nothing writes a second copy into
  logs, diagnostics, or a dedicated field.
  Sender binding remains a production requirement. Because the pilot has no
  authoritative sender-auth metadata, its narrower exception is limited to an
  already-active, expected, low-risk workflow; it labels the sender unverified
  and prohibits financial, identity, recovery, or other consequential use.
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
- **One address per agent (settled 2026-07-21).** Each agent has exactly one
  auto-provisioned, name-derived address from the sanitization pipeline;
  addresses are 1:1 with agents, message rows key to the owning agent, and
  the plan "address count" gate is trivially one in v1. Multiple named
  addresses per agent are deferred (their mint/list/select verbs and a
  message-to-address FK are post-v1 — see Open Questions). Provisioning is
  automatic at agent creation, failing closed to the operator-override path
  on a collision or empty result.
- **Deletion and tombstone durability (settled 2026-07-21).** Agent deletion
  flips the recipient verdict to `unknown_recipient` and (per data-model.md's
  soft-then-permanent delete) purges the mailbox on permanent delete, but the
  address and its tombstone row must **outlive the agent row** — they must
  not cascade-delete — or a re-created agent with the same name re-provisions
  the identical local part and receives the prior principal's mail. Tombstones
  are durable across account archive export/import; the sanitization
  live-or-tombstoned collision check depends on it. Agent rename mints a new
  address; whether the old address keeps delivering as an alias for a bounded
  window (so a mid-verification rename does not silently break) versus
  immediately returns `unknown_recipient` is an Open Question. Realm deletion
  removes the realm-label KV entry (in-flight relays during the ~60s window
  get the not-mine 451) and purges the realm's mailboxes with the rest of its
  data.
- Immutable message rows with per-mailbox delivery, read, and acknowledgement
  state. Fenced claim/renew/release/complete processing is adapted from the
  migration-0034/0036 messaging pattern, with one difference recorded as an
  Open Question: receive-only mail has no outbound result artifact, so
  `complete` marks handling done rather than linking a durable reply, and the
  deterministic-failure counter needs a defined escalation destination (a
  dead-letter/needs-attention state, since there is no sender to notify).
- Metadata-only, cursor-paginated list; explicit content read; separate read
  and ack, so "the client saw the metadata" and "the agent is done with this
  mail" remain distinct facts.
- A bounded, value-free `email_checkpoint` lane in `self.show` lets active
  clients discover pending mail without polling content. It is separate from
  `message_checkpoint` and carries only `pending`, `mailbox_pending`, effective
  `receive_state`, its `agent_receive_state` / `realm_receive_state`
  components, and an additive `unavailable` projection state. The shared
  foreground policy handles at most one Witself messaging-or-email lane per
  turn, after user work, with no background service or wake behavior.
- Retention is plan-scoped: raw MIME and attachments age out by plan window;
  quarantined spam ages out faster; metadata and content-free audit events
  follow [audit-retention.md](audit-retention.md). Account archives include
  the mailbox (addresses, messages, state) with the same interrupt-on-import
  handling of active claims that messaging archives use.

## Surfaces

The pilot shapes are pinned in [json-contracts.md](json-contracts.md),
[cli-command-surface.md](cli-command-surface.md), and
[mcp-tools.md](mcp-tools.md):

- CLI: `witself email address show`, `witself email list`, `witself email
  read`, `witself email code-candidates`, `witself email code-consumed`,
  `witself email claim|renew|release|complete`, `witself email ack`,
  a bounded `witself email listen` (wait for new mail — the OTP flow needs a
  sanctioned wait rather than a poll loop, mirroring `message.listen`), and
  operator-only `witself email operator receive show|enable|disable` for one
  exact enrolled agent or realm.
- MCP: `witself.email.*` mirroring the CLI, with metadata-only list results
  and untrusted-content framing in every content-bearing tool description.
- API: owner routes are `GET /v1/email/address`, `GET /v1/email`,
  `POST /v1/email:listen`, `GET /v1/email/checkpoint`, and the
  `/v1/email/{message_id}:read|code-consumed|ack|claim|renew|release|complete`
  actions. Value-free operator controls are `GET` / `PATCH`
  `/v1/agents/{agent}/email-receive` and
  `/v1/realms/{realm}/email-receive`. The Worker relay uses the separate
  cell-local signed `POST /v1/internal/agent-email:ingest` endpoint.

List, listen, ack, code-consumed, and ordinary read-state projections never
return raw MIME, attachment bytes, body HTML, or active claim capabilities.
Explicit `read` marks the message read and returns one bounded decoded text
projection; plain text is preferred and HTML is deterministically reduced to
text. Every read result labels the sender unverified and the content untrusted.
The MCP projection additionally limits returned text to 64 KiB and reports
when that adapter-level truncation occurred. `code-candidates` crosses that
same owner-only read boundary and scans the subject plus exactly that UTF-8-safe
64 KiB text projection. It returns the original message context, explicit scan
completeness flags, `none`/`single`/`ambiguous`, and at most 32 distinct
first-seen values with occurrence counts. It fails unavailable unless
`parse_state` is `parsed`; truncation or candidate overflow forces
`ambiguous`. It never follows, selects, uses, or consumes anything.

## Pilot Implementation Checkpoint

The local checkout now contains migrations 0059–0061, scoped
mailbox/address/message storage, durable suspected-duplicate grouping, MIME
bounds, archive export/import, fenced foreground processing, value-free audit events, the
signed cell ingestion endpoint, startup reconciliation for exactly the
configured 5–10 agents, API/CLI/MCP owner surfaces, self/hook
`email_checkpoint` hydration, and the isolated Cloudflare Worker plus
literal-rule lifecycle tooling.

Migration 0060 adds independent per-agent receive state and a durable
one-row-per-realm receive control. Effective receive is enabled only when both
layers are enabled; the realm row survives zero active mailboxes, so deleting
and later reprovisioning pilot agents cannot accidentally clear a realm
disable. Ingestion locks all rows contributing to that effective decision.
Operators may inspect or disable either layer while an account is suspended so
incident containment remains available, but re-enabling either layer requires
an active account. A rejected suspended-account enable is a strict no-op: it
does not change row versions, timestamps, or audit events. Startup
reconciliation is likewise read-only for suspended accounts and performs no
mailbox provisioning or repair.

Migration 0061 adds the default-off retry-canary proof state described above.
Schema 60 is also a deployment compatibility barrier: schema-59 servers ignore
the realm row, and schema-59 exporters omit it. Freeze receive-control changes,
archive export/import, and cell moves until every replica is schema-60 capable.
Do not roll an account back across that barrier after relying on a realm
disable; first disable the edge/process pilot and drain the older replicas.

The checkpoint was deployed on 2026-07-21 and hardened through `v0.0.197` on
2026-07-22: the isolated Worker and KV, seven exact routes, matching cell
feature configuration, synthetic durable-accept canary, stable provider-retry
proof, delayed provider retries, and disable/re-enable rollback were all
verified live. The existing catch-all and control-plane KV remained unchanged.
Production remains blocked on the strict capability gaps above.
Plan-tier retention, quarantine, trusted sender authentication, provider-id
idempotency, and billable receive remain production work rather than features
silently simulated by the pilot.

**Authorization (settled 2026-07-21).** Mail is owner-agent-only, matching
agent messages (the most sensitive existing analog), not the policy-engine-
shareable posture of memories and facts. There is no cross-agent read of
another agent's mailbox in v1: an agent reads only its own mail. New
`email:*` scopes split the surface — a read tier (`address show`, `list`,
`read`, `code-candidates`, `listen`) separate from a processing tier
(`claim`/`renew`/`release`/`complete`, `ack`). Operators get no access to raw
mail content in v1
(content is a private correspondence surface); operator visibility is
metadata/lifecycle only, and any future content access is a separate
governed decision. The client extracting a code is an ordinary scoped read,
so it is not a value-egress tool in the `secret.reveal`/`totp.code` sense;
`--no-value-tools` therefore does not gate it, but `--read-only` still
withholds the processing tier.

Platform notifications get an operator-plane surface (template + event
triggers) rather than agent tools; that design lands with its track.

## Abuse, Privacy, And Metering

Receive-only still carries real obligations:

- Per-mailbox inbound rate and size caps enforced at ingestion; overflow is
  rejected at the provider boundary, not silently dropped after storage.
- **Hostile inbound volume does not bill the victim (settled 2026-07-21).**
  Nobody controls who sends an agent mail, so metering must not hand an
  attacker a lever. Provider-flagged spam, quarantined mail, and traffic
  classified as an abuse flood are excluded from metering and billing;
  inbound caps are accounting-only with no overage charge on received mail,
  and cap accounting is scoped so a single-sender flood cannot crowd out the
  one legitimate verification mail (per-sender/per-source accounting, or an
  equivalent, is a launch requirement, not a nicety). Because a
  receive-disabled mailbox tempfails within a grace window rather than
  hard-bouncing (see the SMTP contract), driving a mailbox to enforcement
  does not permanently suppress its address. A per-sender/per-domain
  denylist and a mark-as-spam feedback surface are required so a campaign
  can be stopped without disabling the whole mailbox; the fallback
  classification when Cloudflare supplies no usable spam verdict is an Open
  Question.
  The limited pilot resolves this conservatively by excluding every received
  pilot message from billable usage and quota enforcement; its counters are
  operational only until authoritative classification exists.
- **Billing dimensions (settled 2026-07-21).** Email gets its own
  `billing-and-limits.md` dimensions rather than reusing the messaging keys
  (the separate-surface rule, and to keep abuse signals distinct):
  `email_received`, `email_sent`, `email_address`, and a dedicated
  `email_storage_byte` for inline raw MIME — mail bytes must not fall under
  the general open-plane `storage_byte`, or 25 MiB messages silently consume
  the general storage cap. Each dimension needs its cap-vs-rate
  classification and overage default recorded in the billing doc's canonical
  table before the slice-1 metering deliverable can be built against the
  `/v1/capabilities`, `/v1/billing/usage`, and Prometheus `limit_dimension`
  machinery. Platform notifications are cost of service and not user-metered.
- Email is switchable per agent and per realm: an operator or plan
  enforcement can turn receive — and later send — off independently without
  deprovisioning addresses.
- Sending, when it ships, stops hard at a per-period threshold. The cap is a
  backend-enforced gate, not client-side advice, because agent-originated
  spam is a first-class threat to the shared domain's reputation and to the
  platform; threshold accounting exists per agent and per realm from the
  first send slice.
- Content never appears in logs, metrics, or diagnostics; audit events for
  provision/ingest/read/ack/purge are content-free, matching messaging. Note
  the honest boundary: message bodies — including any OTP or reset link they
  carry — are open-plane realm data and are therefore **plaintext at rest**
  in the cell for the retention window, exactly like a memory or a stored
  agent message. This is not the sealed plane; the guarantee is only that no
  *second* copy of that content leaks into logs, audit, or a dedicated field,
  not that credential-bearing mail is encrypted at rest. Whether a shorter
  retention floor should apply to mail classified as carrying a transient
  code is an Open Question.
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

1. **Settled 2026-07-21:** `email_checkpoint` is a separate `self.show` lane;
   it is value-free and shares the one-foreground-lane budget with messaging.
2. **First helper settled 2026-07-21:** OTP extraction is client-side (see
   Trust Model). It scans the subject followed by the same UTF-8-safe first
   64 KiB decoded-text projection returned by MCP read. Standalone ASCII
   numeric candidates of 4–8 digits must be locally associated with `code`,
   `verification code`, `security code`, `one-time code`, `one time code`,
   `passcode`, `OTP`, or `PIN`; URL-embedded values are excluded. Duplicate
   values collapse with an occurrence count, first-seen order is stable, and
   at most 32 distinct candidates are returned. The client reports `none`,
   `single`, or `ambiguous`; any text truncation or candidate overflow forces
   `ambiguous`. A message whose `parse_state` is not `parsed` fails unavailable
   instead of producing a false `none`. The helper never follows a link,
   selects or uses a value, or marks the message code-consumed. Alphanumeric
   formats, localization, the audit shape for a code-consuming read, and
   extraction from quarantined messages remain open.
3. **Pilot settled 2026-07-21:** only an attachment count is exposed; raw MIME,
   attachment names, media types, and attachment bytes are unavailable.
   A future production retrieval surface still requires the injection review.
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
   **Run 2026-07-21: strict production gate failed; limited pilot authorized.**
   Full coverage and per-recipient dispatch worked, but Cloudflare exposes
   neither an explicit temporary-reject action nor the trusted structured
   authentication/spam/provider-id fields required by the settled production
   SMTP contract. Development may proceed only within Capability Tiers And
   Authorized Pilot; production promotion remains blocked. See
   [the launch-spike report](agent-email-cloudflare-launch-spike.md).
10. Vanity realm-label policy, when that deferred capability is scheduled:
    reservation and dispute rules, the reserved-word and anti-impersonation
    list, the vanity length cap, per-plan gating, and whether release or
    transfer is ever permitted given address permanence (see Addressing And
    Domain Model).
11. Edge observability and metering: rejected and tempfailed mail never
    reaches a cell, so Worker-side verdict counters (and their value-free
    export into the platform metrics plane) are the only visibility into
    edge drops; decide the mechanism before v1 promotion.

Raised by the 2026-07-21 whole-spec gap review (blocking items were settled
in place; these are the remaining important items):

12. **Pilot settled 2026-07-21:** `read` returns bounded decoded text, prefers
    plain text, deterministically reduces HTML, never returns raw MIME or
    attachment bytes, and surfaces a value-free parse-error code. Parsing is
    bounded to 5 MiB raw MIME, 256 KiB headers, 64 MIME parts, depth 8, and
    1 MiB decoded text; every content surface retains untrusted-input framing.
13. Retention enforcement mechanics: what runs the aging, and the guard so
    aging never silently expires unread/unclaimed mail (especially the
    verification mail the feature exists to receive) — a durable-mailbox
    promise needs an expiry that cannot black-hole pending work.
14. Quarantine lifecycle: a rescue/disposition path (list, inspect, release,
    or discard quarantined mail) and the fix for the checkpoint-exclusion
    trap — legitimate OTP mail misflagged as spam is invisible to the
    checkpoint, so a provisioning flow hangs. Ties to OQ2's
    extraction-on-quarantined decision.
15. `complete` / deterministic-failure semantics for receive-only mail:
    `complete` has no outbound result artifact to link, and failure
    escalation needs a defined destination (a dead-letter / needs-attention
    state, since there is no sender to notify). Recorded inline in Mailbox
    Semantics; the exact state machine is open.
16. **Settled 2026-07-21:** per-agent and per-realm receive controls are
    independent, with a durable realm aggregate that survives zero active
    mailboxes. Effective receive is disabled when either layer is disabled
    (and retired when the mailbox is retired). Settled operator auth protects
    value-free `GET`/`PATCH /v1/agents/{agent}/email-receive` and
    `/v1/realms/{realm}/email-receive`; agents cannot mutate those routes.
    The owner address and `email_checkpoint` projections expose effective,
    agent, and realm state, without exposing another mailbox or granting
    operators access to message content.
17. Self-host parity: the "identical pipeline" claim needs a self-host
    analog for the Cloudflare-specific delivery guarantee and for edge-key
    publication — a self-hoster's own edge and key-publication path, or an
    explicit narrowing of the parity claim.
18. Restrictive sender-auth DNS on the receive domains: publish `SPF -all`
    and `DMARC p=reject` on `agent-mail.witwave.ai` / `witmail.ai` (they
    send no mail in v1) so the domains cannot be spoofed outbound; confirm
    this composes with a future send slice.
19. Edge-key freshness bound: the delist/rotation propagation to cells needs
    a bounded staleness window and a hard-fail on delisted keys, plus the
    ingestion-endpoint hardening and availability posture (the OTP use case
    makes ingestion availability load-bearing).
20. Audit + attribution: register the email audit event family; decide how
    ingestion attributes a token-derived actor when the "sender" is external
    (edge-attributed, not agent-attributed); register cell-side telemetry
    and the synchronous-verdict latency SLO.
21. Compliance posture for stored third-party correspondence: per-message
    purge/redaction, illegal-content handling, and the controller/processor,
    DSAR, and data-residency decisions for mail content held in a cell.
22. Routing-projection integrity: a poisoned realm-label→cell KV entry
    redirects message *content* to an attacker cell, which is stronger than
    the "control-plane compromise is a routing incident, not a data breach"
    invariant elsewhere; decide the integrity control (signed projection
    entries, or cell-side ownership assertion on the relay) so a bad routing
    write cannot exfiltrate mail.
23. Multiple named addresses per agent (deferred past v1): the mint / list /
    select verbs, the message-to-address FK, and primary-address designation
    if the one-address-per-agent rule is later relaxed.

Settled on 2026-07-20 (formerly items 1–2): realm-label derivation and
local-part sanitization (see Addressing And Domain Model), and the Cloudflare
topology, edge authentication, and routing-map publication (see Inbound
Pipeline). Settled on 2026-07-21 by the whole-spec gap review: the SPF/DKIM
trust anchor, mailbox authorization, OTP extraction location, inbound-abuse
billing, kill-switch tempfail, one-address-per-agent, deletion/tombstone
durability, and the email billing dimensions (all in the sections above).

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
