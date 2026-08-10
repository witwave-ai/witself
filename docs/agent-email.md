# Witself Agent Email

Status: the original GCP capability-limited receive pilot is retired. Its seven
literal Cloudflare routes and isolated directory entries were disabled and
removed on 2026-07-30; no pilot address is currently active, and the catch-all
was not changed. The durable receipt, provider-managed retry, owner processing,
disable/re-enable rollback, and default-off exact-agent synthetic retry proof
were exercised before retirement. A new Civo canary must use a fresh,
explicitly reviewed 5–10-agent manifest and remains manual-only. This
capability does not add a sender-trust claim or automatic code use.

Production-cell receive checkpoint (implemented for `v0.0.241`, default off):
the server now supports a mutually exclusive exact-account cohort of 1-100
canonical IDs. Serving-pod startup performs only bounded read-only
account/canary validation; existing mailbox creation moved to the explicit,
idempotent, keyset-paged `witself-server agent-email backfill` action, while a
new cohort agent and mailbox commit atomically. The cell-native read-only
`agent-email canary-manifest --output ...` action derives the exact sorted
5-10-address edge manifest from currently enabled stored mailbox routes and
creates it as a new mode-0600 file. It refuses missing mailboxes and includes
the configured retry canary. No live cell, provider route, MX, catch-all,
canonical-delivery, alias-delivery, or custom-domain gate is enabled by this
implementation checkpoint.

Permanent-domain decision (2026-08-03): `witmail.net` is the managed email
apex dedicated solely to agent email. It is not a website, human or employee
mail domain, marketing domain, or generic Witself/Witwave notification sender;
required `postmaster@` and `abuse@` routes are narrow operator-controlled
exceptions. It replaces the unacquired `witmail.ai` target; there is no
`witmail.ai` compatibility surface. The new zone remains deliberately dormant
while its Cloudflare Registrar registration waits for an inter-account move:
restrictive SPF/DKIM/DMARC anti-spoofing records are present, but there is no
MX, Email Routing, catch-all, Worker route, or DNSSEC activation. Cloudflare
does not move zone configuration with the registration, so the reviewed
records and routing must be recreated and verified in the production account
after the move. Keep
every canonical-inventory, canonical-delivery, alias-activation, and
alias-delivery gate dark throughout that work. New canonical addresses and all
new aliases use `witmail.net` only. The retired `agent-mail.witwave.ai` domain
is bounded compatibility for previously issued canonical local parts only: it
must never mint a new alias or receive a broad catch-all.

Agent-email byte limits use a two-phase rollout. Phase A shipped the schema-81
counter/enforcement and administrator surfaces with both catalog keys absent.
After all Phase-A writers converge, Phase B rolls schema 82 to promote any
compatibility rows and rebuild exact account counters. The final Phase-B
catalog then activates Personal `0`/`0`, Professional `10 MiB`/`5 GiB`, and
Team and Enterprise `25 MiB`/`100 GiB` raw-message/attachment-storage limits.
The Founder account remains explicitly unlimited for attachment storage; that
audited override must survive final catalog reconciliation.

This ingress-protection slice adds schema 84 and six rolling count/byte rate
keys across unverified sender/recipient, recipient, and realm scopes. Every
current plan intentionally omits those keys, so independent platform breakers
apply to all enabled accounts. An audited account override can only lower a
breaker; explicit unlimited cannot remove it. The owning cell's PostgreSQL
limiter is the sole authoritative admission point, after the account feature
check, so disabled accounts continue to accept-and-drop without edge changes.

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
  estate. Target: acquire and provision `witmail.net`. Each realm gets its own
  subdomain derived from the realm's unique identifier, and the local part is
  the agent name: `scott@<realm-label>.witmail.net`. This historical shape was
  later replaced by the apex/local-part design below.
- **Send is confirmed but later.** Agents will eventually send — verification
  flows may force it sooner than correspondence does — and the design
  documents it now, but receive ships first and nothing in v1 depends on send.
- **Email is a billing point in both directions.** Sent and received mail are
  metered per period; sending stops hard at a per-period threshold; email is
  switchable on and off per agent and per realm; agent-originated spam
  prevention is a first-class requirement, not a slice-3 afterthought.
- **Attachments stay in Postgres.** V1 stores raw MIME, attachments included,
  directly in the database under hard size caps — no object store or
  file-management layer in this epic. The account attachment allowance counts
  the complete retained raw-MIME size of messages that contain attachments;
  there are no separately stored attachment blobs in this implementation.

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
  accelerating the managed-apex acquisition instead of passing through a
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
  `witwave.ai` zone — until `witmail.net` is acquired and provisioned, then
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
  no model inference; it may reuse provider-adapter implementation patterns,
  but must use a separately isolated sending domain and reputation. It never
  sends from `witmail.net` and may ship in any order relative to the slices
  above.

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
- The pilot's technical ceiling is **25 MiB raw MIME**, matching Cloudflare's
  provider cap. The resolved account plan may lower that ceiling (10 MiB for
  Professional; email is disabled on Personal), and the Worker maps the cell's
  exact plan-aware over-size verdict to a permanent SMTP 552 rejection. Raw
  MIME may still contain attachments and is stored as one message, but neither
  raw MIME nor attachment content is retrievable through API, CLI, or MCP
  during the pilot. Metadata surfaces expose only the attachment count,
  attachment-storage byte counts, and payload-retention state; content reads
  expose bounded decoded text.
- Success is returned only after the owning cell durably commits the message.
  On a cell timeout, transport failure, transient verdict, or unexpected
  exception, the Worker throws one deliberate **sanitized** exception and lets
  Cloudflare manage retry. The spike observed provider temporary-error retries,
  but the pilot neither promises a literal `451` nor depends on a documented
  retry count or schedule. No raw provider error or message content is placed
  in the exception.
- The cell's exact HTTP 429 `{"verdict":"rate_limited"}` response is a
  temporary result. The Worker throws the same sanitized exception used for
  other provider-retry paths, never calls `setReject`, and records only the
  value-free `tempfail_rate_limited` outcome in phase `response`. A zero cap or
  message larger than its effective byte bucket is intrinsically impossible;
  the cell returns the existing permanent verdict so the Worker rejects it
  once instead of amplifying retries.
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
would ordinary-accept the first delivery. Keep automation manual-only, deploy
schema-61-capable code with the canary agent unset, wait for every pod to
converge, then add the exact agent in a config-only rollout and wait again.
Only then arm/send manually; add a schedule only after that run proves the fixed
edge sequence `tempfail_cell_response` / `response` / `503`, then `accepted`.
Rollback reverses this carefully: disable any schedule that has been added,
settle the unused arm or let its 15-minute TTL expire, and only then unset the
agent or downgrade.

The workflow is manual-only. A successful run acknowledges but does not delete
its synthetic message, so a future 15-minute cadence would retain about 96
messages per day until mailbox retention/delete is settled.

**Production receive-only contract:** the Inbound SMTP Transaction Contract
below remains the target. Promotion beyond the internal pilot, catch-all Worker
cutover, billable receive, sender-auth-dependent behavior,
or consequential OTP/link automation stays blocked until the provider path (or
a replacement inbound edge) supplies explicit temporary SMTP semantics,
trusted structured authentication/spam metadata, a stable provider identity,
and the size/latency feasibility evidence required by that contract.

## Addressing And Domain Model

Agents receive addresses shaped `<agent-local-part>.<realm-label>@<base-domain>`
— for example `scott.drz4xnv73ficcrko@witmail.net` — so the realm still anchors
the address the way it anchors identity, avatars, and published signing keys,
but as a local-part segment rather than a subdomain. The subdomain shape from
kickoff was dropped after verification: Cloudflare Email Routing caps a zone
at 30 configured domains (apex plus routing/sending subdomains combined) and
has no wildcard subdomain receive, so per-realm subdomains cannot scale (see
Inbound Pipeline for the full findings). The launch receive domain is
`agent-mail.witwave.ai`, configured for Email Routing inside the existing
`witwave.ai` zone (operator decision, 2026-07-21), with `witmail.net` — the
dedicated Cloudflare-fronted apex registered on 2026-08-03 — as the durable
home. The address shape is identical on both:
`<agent-local-part>.<realm-label>@agent-mail.witwave.ai` for a previously
issued launch identity, the same local part at `witmail.net` after cutover.

One engineering caveat gates production use of the launch domain: Cloudflare
documents catch-all at the zone apex only. The launch spike established that
the zone-global catch-all covers the configured subdomain, but routing that
catch-all to the Worker would also move existing apex traffic. The limited
pilot therefore uses exact-address rules and leaves the catch-all unchanged.
The permanent path is now the `witmail.net` apex. Exact-address routing on
`agent-mail.witwave.ai` is permitted only for previously issued canonical
local parts during compatibility; it is not a fallback for new addresses.
Email Routing custom-address rules are capped per zone, and the address
population grows with every agent, so production coverage on `witmail.net`
still requires the reviewed apex route rather than per-address expansion.

**Realm label (settled).** `<realm-label>` is the realm id body verbatim:
strip the `realm_` prefix and use the remainder. Realm ids are minted as 80
crypto-random bits, base32-encoded and lowercased (`internal/id`), so the body
is always exactly 16 characters from `[a-z2-7]` — a valid DNS hostname label
and local-part atom by construction, deterministic, collision-free by id
uniqueness, and stable for the life of the realm. The label is opaque, which
is acceptable: these addresses mostly serve services, not human recall.
**Realm email aliases (settled 2026-08-01).** The automatic id-body label is
the permanent canonical address identity. A Personal account reserves that
identity but cannot receive email; the `agent_email_receive` entitlement still
controls delivery. Once email is enabled, `scott.drz4xnv73ficcrko@…` works
without replacing or reinstalling any client integration.

Eligible realms may add memorable realm email aliases such as
`scott.acme@…`. An alias is additive: it points at the same realm, agents, and
mailboxes as the canonical label and never replaces the ID-body address. The
control plane owns the globally shared alias namespace, request and approval
lifecycle, plan eligibility, reserved-name registry, and non-reuse history.
Cells store the applied routing projection and mail; the edge directory is a
replaceable projection, never claim authority.

Alias labels are 3-16 lowercase ASCII characters from `[a-z0-9-]`, with no
dot, leading/trailing hyphen, or consecutive hyphens. `xn--` labels and strings
matching the canonical 16-character `[a-z2-7]{16}` form are rejected. The
16-character cap
preserves the existing 47-character agent-segment budget. Claims also pass a
normalized anti-impersonation check. Platform and operational names including
`witself`, `witwave`, `witmail`, `witpass`, `email`, `mail`, `agent`, `admin`,
`support`, `security`, `billing`, `postmaster`, and `noreply` begin reserved.
The reserved set is a versioned, audited control-plane resource managed only
by a Witself platform administrator; customer account and realm administrators
cannot alter or override it. Adding a new reservation does not silently revoke
an already-active customer alias, and retiring a reservation does not bypass
an active claim or tombstone. Witself-owned protected aliases require a
separate privileged internal assignment path.

A customer request is `pending_review` until a platform administrator begins
approval or rejects it. Approval first records a durable `provisioning` intent;
only after the cell fence and edge projection converge does the request become
`approved`. The request is visible as `provisioning` during recovery while the
assignment remains non-public, then the assignment becomes `active`;
administrative suspension, plan suspension, the 30-day
plan grace, and retirement are assignment states rather than request states.
Restoring entitlement reactivates a plan-suspended alias without reinstalling a client.
Activated aliases cannot move to another customer or realm. A rename is a new
request followed by permanent retirement of the old alias. Account lifecycle
operations suspend and republish alias routes under an exact durable fence.
Managed realm close now uses a durable control-plane operation: it first proves
that the realm has no live or pending aliases and no non-retired custom-domain
route, then prepares the exact cell
generation (`live` to `closing`), publishes the permanent canonical-route
tombstone, and only then commits the cell soft-delete (`closing` to `retired`).
The operation id and generation make each phase replay-safe, and the retired
cell row remains in inventory so a later backfill cannot resurrect its route.
Direct managed cell deletion fails closed; portable self-hosted deployments
retain direct empty-realm deletion and write the same terminal tombstone shape.
Custom customer domains
use this same label shape but remain a separate entitlement and verification
lifecycle.

Alias-delivered messages preserve both the exact envelope recipient and the
structured alias claim in cell storage and logical archives. Because schema
`0084` cannot represent that provenance without changing one of those values,
downgrading migration `0085` fails closed before any mutation whenever such a
message exists. Alias claims alone do not block downgrade when no message has
been delivered through them.

Alias creation ships behind the control-plane environment gate
`CP_REALM_EMAIL_ALIAS_ACTIVATION_ENABLED`, which is disabled unless its value is
exactly `true`. The Email Worker has a second fleet-wide, alias-only gate,
`REALM_EMAIL_ALIAS_DELIVERY_ENABLED`, which is also exact-`true` and default-off;
both alias gates remain off in release configuration. Canonical Realm ID
inventory and delivery use three separate exact-`true`, default-off controls:
`CP_REALM_EMAIL_CANONICAL_INVENTORY_ENABLED` starts the bounded control-plane
inventory, `CP_REALM_EMAIL_CANONICAL_DELIVERY_ENABLED` permits that controller
to publish applied routes for email-enabled accounts, and
`REALM_EMAIL_CANONICAL_DELIVERY_ENABLED` is the independent Email Worker
emergency gate. Inventory can safely converge suspended and retired records
while either delivery gate is dark; a canonical route cannot deliver unless
both canonical delivery gates are true. Canonical delivery never depends on
either alias gate. While alias activation is disabled,
customer requests, approval, internal assignment,
and reactivation fail closed; read-only administration plus suspension,
retirement, terminal customer/internal-provisioning abort, reserved-name
management, and audit remain available.

Canonical and managed-alias delivery also share one additive, account-scoped
rollout fence. The control plane reads the canonical sorted CSV
`CP_AGENT_EMAIL_MANAGED_DELIVERY_ACCOUNT_ALLOWLIST`; the Email Worker reads the
byte-identical `AGENT_EMAIL_MANAGED_DELIVERY_ACCOUNT_ALLOWLIST`. Empty or
missing means no account is admitted. Each list accepts at most 100 exact
generated account IDs matching `acc_[a-z2-7]{16}` and rejects whitespace,
duplicates, noncanonical ordering, and every wildcard-like value. The largest
valid CSV is exactly 2,099 ASCII bytes. Managed route payload v2 carries the
exact signed `account_id`; its signed envelope is v3. Customer-owned-domain
payloads intentionally remain v1/signed-v2. A legacy v240 managed
v1/signed-v2 row is accepted only as evidence that the route was previously
known: the new edge forces an authoritative control-plane refresh and never
uses that row for delivery because it cannot prove account membership.
This allows the edge to enforce the cohort even on a fresh current KV hit.
The value is route authority only: it is never logged or emitted to Analytics
Engine. A known route outside the cohort tempfails before an inactive-route
bounce, raw MIME read, or cell request; the control-plane fallback likewise
returns a retryable `409 managed_email_delivery_cohort_held_back`, never a 404.
Invalid cohort configuration fails closed with 503/configuration tempfail.
The two existing route-kind fleet gates remain required, so activation is the
intersection of the exact account cohort and the applicable canonical or alias
gate. Customer-owned-domain routes do not contain this managed-route field and
remain governed only by their independent custom-domain fences.

The edge-token-authenticated read-only endpoint
`GET /v1/email/managed-delivery/readiness` reports only cohort count, the
SHA-256 of the canonical CSV, and the three control-plane gate booleans; it
never returns account IDs. Deployment attestation reports the same value-free
count/digest for the edge, and coordinated dark readiness refuses mismatched,
or invalid cohorts. Its default invocation requires the empty cohort. Before a
deliberate staged activation, supply
`--expected-managed-delivery-cohort CANONICAL_CSV`; the verifier then requires
both deployed Workers to contain those exact bytes while retaining every dark
delivery-gate check, and emits only count and digest. Operators must run that
single comparison before provider activation.

The v241 protocol rollout uses one enforced code-deployment order: control
plane first, then email edge. Before the control-plane deployment, the active
v240 edge's canonical and alias delivery gates must both be verified false and
the target control-plane cohort must be empty.
A new edge reading a v240 managed row forces refresh and tempfails if the old
control plane returns v1 again. A v240 edge rejects a new signed-v3 managed row,
but it can still deliver from a fresh legacy signed-v2 KV row without consulting
the new control plane when its old route-kind gate is true. The v0.0.241
control-plane release command therefore follows the exact active email-edge
version before mutation and refuses a CP-first upgrade unless a v0.0.240 edge
has both gates false and the new control-plane cohort is empty. The v0.0.241
email-edge release command refuses deployment until the control plane is
already v0.0.241 or newer. Under those mandatory dark preconditions, the brief
mixed-version period does not read MIME, contact a cell, or bounce the known
address. Complete both Worker upgrades before installing a nonempty cohort.
Install the control-plane cohort first and the edge cohort
second, run the explicit expected-cohort readiness check, then perform the
separately reviewed provider and gate activation. To remove an account, remove
it from the edge cohort first so even fresh cached v2 rows stop immediately,
then remove it from the control plane.

Every control-plane deploy, email-edge deploy, guarded email-edge rollback,
coordinated route-signing secret ceremony, primary-routing apply, and
catch-all-routing apply is serialized by one global operations lease in the
existing `REALM_EMAIL_ALIASES` Durable Object. The exact operation identifiers
are `control_plane_deploy`, `email_edge_deploy`, `email_edge_rollback`,
`route_signing_secret_provision`, `relay_signing_key_provision`,
`primary_routing_apply`, and `catch_all_routing_apply`. The operator must
provide the existing
`CONTROL_PLANE_EDGE_TOKEN` to these local commands without printing or
persisting it. The command acquires the lease through the authenticated control
plane API, renews it while work is in progress, proves one final renewal before
success, and releases it afterward. A second operation fails closed while the
lease is active; an abandoned lease becomes available only after its bounded
expiry. Do not bypass a conflict or start either Worker deployment, the guarded
rollback, or either provider-routing mutation concurrently.

The route-signing ceremony reveals the desired shared fallback token only after
its complete Witself envelope is validated, uses that value directly to
authenticate the canonical live control-plane lease, and never passes it through
the process environment to Wrangler. It reacquires every dark/live fence under
the lease and renews before and after each secret write. Consequently, a normal
coordinated rerun works only while the desired token already equals the live
control-plane credential. Rotating `CONTROL_PLANE_EDGE_TOKEN` cannot safely hold
a lease authenticated by the credential it is replacing; that is an explicit
break-glass operation requiring the global provider-mutation freeze documented
in the control-plane deployment runbook.

Relay-signing-key rotation is a distinct edge-only ceremony. Its Witself source
contains public key-id and canonical raw-Ed25519-public-key fields plus one
sensitive PKCS#8 private-key field. The live control plane and edge must already
be the exact same target release; the live edge retains the old relay id while
the re-rendered, not-yet-deployed target config names the distinct desired id.
The default provider contract is the exact active `witmail.net` zone in the
same Worker account, proved through the Cloudflare zone API. An explicitly
selected `witwave.ai` contract is legacy evidence only. With empty live managed
cohorts, both delivery flags false, all custom-domain controls dark, the selected
catch-all disabled, owned rules disabled, and no enabled Worker action targeting
the email edge, the command validates the source envelopes and derives the
public key before acquiring `relay_signing_key_provision`.

Under that lease it durably reserves a value-free pending receipt, writes only
`RELAY_ED25519_PRIVATE_KEY` over stdin, and changes no plain variable. Success
requires a new edge deployment/version successor, unchanged control plane and
non-secret resources, unchanged provider state, and final lease evidence before
the receipt is atomically committed. The mode-`0600` receipt binds old/desired
key ids, public-key and target-config digests, target release, provider-scope
digests, and successor ids, never key values or source-secret references. A
failed run retains its pending marker for explicit dark-state reconciliation.
Direct provider and Worker mutations remain globally frozen throughout. The
secret-only successor must be replaced immediately by a deployment from the
same unchanged exact tag; only that tagged deploy installs the reviewed public
`RELAY_KEY_ID`. This ceremony requires an existing relay-secret binding and is
not a first Worker bootstrap path.

The lease coordinates Witself's supported deployment and routing tools; it
cannot fence a person or unrelated automation that writes Email Routing through
the Cloudflare dashboard or API. Freeze those external mutations for the whole
plan/apply window. If that rule is violated, treat the receipt as suspect,
inspect live state, and use the separately planned fail-closed disable path
before continuing.

The sole bootstrap exception is the first dark v0.0.241 control-plane deploy,
when the old control plane returns a literal 404 because it has no lease API.
The deploy command permits that exception only after stable provider reads
prove the exact Git-tagged v0.0.240 control plane, unchanged Durable Object
namespaces, an absent legacy cohort binding, absent canonical inventory and
delivery gates, an exactly v0.0.240 dark email edge, and empty active and target
cohorts. The independent alias-administration gate may remain active because it
does not enable delivery. The one unleased write uses `wrangler deploy
--containers-rollout none` to install only the byte-identical outer v0.0.241
Worker. The command then proves the target and namespace continuity, acquires
the newly installed durable lease, and performs the full Container deployment,
verification, and convergence checks under that lease. There is no bootstrap
exception for the email edge, provider-routing changes, v0.0.242 or later, a
nonempty cohort, or any other response. Cloudflare exposes no compare-and-swap
for that first outer-only upload, so unrelated dashboard and direct API writes
must remain frozen during the bootstrap window.

The guarded edge rollback tool may treat v0.0.240's missing cohort binding as
the empty value only when both the current and candidate alias/canonical gates
are false and the current cohort is empty. That makes v0.0.240 an honest dark
emergency rollback candidate, not an active managed-delivery candidate. It
cannot consume signed-v3 managed KV rows; keep delivery dark until compatible
Workers are restored. Current custom-domain v1/signed-v2 rows are unchanged.
Apply checks the supplied plan hash before acquiring `email_edge_rollback`, then
reconstructs the exact reviewed state, renews immediately before the
interruptible Wrangler deployment, and verifies the selected version at 100
percent while still holding the lease. Collision and lease loss fail closed;
the operation proves a final renewal and exact release and has no bootstrap
bypass. The production-specific rollback pins the lease authority to
`https://self.witwave.ai`, passes only `CONTROL_PLANE_EDGE_TOKEN` into the
lease client, ignores inherited `WITSELF_CONTROL_PLANE` and `CONTROL_PLANE_URL`
selectors, and refuses HTTP redirects.

Do not enable
the gate until every release blocker is closed and acceptance-tested: (1) the
managed domain has a verified catch-all or equivalent full-coverage SMTP route
into the Email Worker; (2) account move, restore, archive, close, and realm
close explicitly reconcile, suspend, and republish canonical plus alias routes
at the account lifecycle fence, so stale KV cannot route to an old cell; and
(3) internal provisioning has authoritative realm prevalidation plus a
deterministic terminal abort/tombstone path for a permanently rejected hidden
intent. Account lifecycle fencing, authoritative preflight, durable terminal
recovery, canonical inventory, and exact realm-close retirement are now
implemented, but production activation still requires explicit acceptance of
the catch-all, full inventory convergence, and restore drill. The global
registry now has a portable append-only authority journal and bounded
empty-target recovery foundation. Each authority mutation is represented by a
create-only, hash-chained R2 after-image before external projection success;
bootstrap and checkpoint freeze authority writes while taking bounded pages.
Recovery accepts only an exact journal head into a named empty Durable Object,
replays and validates the chain, rebuilds derived indexes, compares the complete
authority digest, and permanently seals the target. It never merges into the
fixed `global` authority object and there is no cutover selector or endpoint.
It does not change any activation gate automatically. A successful operator
restore drill remains a hard activation prerequisite. Durable Object
point-in-time recovery remains
useful first aid, but is not the portable recovery contract. The request
registry now has plan-independent technical queue ceilings of eight open
requests per realm and 64 per account. `pending_review` and durable
`provisioning` both consume an
open slot; successful approval, rejection, or terminal abort releases it, while
customer allocation remains counted until retirement. Exact membership-backed
counters make create and ordinary approval O(1), including explicit-unlimited
plans, and exact membership plus aggregate integrity checks fail closed on
drift. Existing registry state is rebuilt in bounded 100-claim alarm pages.
For ready-state corruption, an authenticated administrator can start one
idempotent, audited recovery that clears only derived state, scans canonical
claims, and verifies a second full bounded pass. Count-changing writes remain
fenced throughout and only verified completion restores readiness. Deployments may
lower, but never raise, the compiled ceilings with
`CP_REALM_EMAIL_ALIAS_MAX_PENDING_PER_REALM` and
`CP_REALM_EMAIL_ALIAS_MAX_PENDING_PER_ACCOUNT`. A separate time-window request
rate guard still remains before activation. The Email Worker now shields the
authoritative registry from random valid SMTP labels: positive KV projections
always win; cold misses use SHA-256-keyed 10-second, 1,024-entry in-isolate
suppression and identical-request singleflight; and all control-plane fallbacks
require fixed-key Cloudflare Rate Limiting admission before raw content is
read. Fixed in-isolate windows first enforce 10 cold and 100 known-or-uncertain
leader lookups per 10 seconds; singleflight followers consume neither local nor
Cloudflare admission. Cold misses then use
`REALM_ROUTE_COLD_MISS_LIMITER` at 10 calls per 10 seconds, while stale or
uncertain known-route recovery uses
`REALM_ROUTE_KNOWN_MISS_LIMITER` at 100 calls per 10 seconds. An admitted cold
lookup with no prior evidence may turn an authoritative 404 into one permanent
bounce; its coalesced followers and later suppressed attempts tempfail. Missing,
failed, malformed, or denying limiter bindings also tempfail. Cloudflare's
per-location counters are permissive and eventually consistent, so they add a
shared shield around strict per-isolate windows rather than exact or billable
accounting. Namespace IDs `2201` and `2202` were verified account-unique on
2026-08-02, when only the control-plane recovery namespace `1001` was deployed;
operators must repeat that account-wide preflight before first deployment or
any namespace change.
Keep both alias gates and all three canonical gates unset until their respective
acceptance steps are complete. The current exact-address pilot rules do not
deliver arbitrary new aliases. The canonical inventory is the independent
backfill path for realms that have never performed an alias operation, but it
does no work while its default-off inventory gate is unset.

**Organization-owned inbound domains (dark lifecycle, 2026-08-08; dark routing
foundation).** A customer-owned domain is an account-level resource
and never replaces either a permanent Realm ID address or a managed realm alias.
Its address shape is `agent-name.realm-email-alias@customer-domain`; retaining
the already claimed realm alias avoids making one account-owned apex an
ambiguous cross-realm namespace. A customer-domain route does not create,
rename, or consume another realm-alias claim: it binds one verified domain
allocation to one existing claim, its exact realm, and their shared account.

The resolved plan feature is `agent_email_custom_domain` and its account-wide
limit is `agent_email_custom_domains_per_account`. The Phase-B canonical
catalog sets Personal and Professional to disabled with zero, Team to one, and
Enterprise to feature-present but zero until an administrator applies the
contracted account limit. A missing effective limit is explicit unlimited. The
independent inbound `agent_email_receive` entitlement still controls whether
any eventual route may deliver.

The control plane now implements the request, ownership-verification, plan,
and account-lifecycle authority, but the entire feature remains deliberately
dark. Customer creation requires all three independent controls below:

- `CP_AGENT_EMAIL_CUSTOM_DOMAIN_REQUESTS_ENABLED` must be exactly `true`;
- `CP_AGENT_EMAIL_CUSTOM_DOMAIN_REQUEST_ACCOUNT_ALLOWLIST` must be a valid,
  comma-separated list containing the exact account id, with no wildcard or
  surrounding whitespace; and
- `CP_AGENT_EMAIL_CUSTOM_DOMAIN_AUTHORITY_READY` must be exactly `true` after
  the required authority journal is healthy and a named empty-target recovery
  has been sealed and reviewed.

Request activation also fails closed unless the existing durable
`CP_PLAN_LIFECYCLE_ENABLED` plan scanner is exactly `true`; it is the recovery
path for an `awaiting_cell` intent if the bridge loses the completion response.

All three are absent from committed release configuration. The separate
`CP_AGENT_EMAIL_CUSTOM_DOMAIN_VERIFICATION_ENABLED` exact-`true` gate controls
administrator and scheduled DNS observations; it is absent too. These controls
are independent from all five managed-domain canonical/alias gates. Request
creation canonicalizes strict lowercase ASCII DNS names, rejects IDN/punycode,
wildcards, IP/single-label/malformed input, and protects
Witself/Witwave-operated roots plus every child domain.

Creation returns one stable public DNS challenge:

```text
TXT _witself-verification.<customer-domain>
    witself-domain-verification=aedv_<32-lowercase-base32-characters>
```

Each request receives its own challenge, which remains byte-identical across
exact idempotent replay, list, and administrator show and expires seven days
after issue. More than one account may hold an unverified challenge for the
same canonical domain. A pending request consumes one temporary commercial
allowance reservation and one of the plan-independent maximum eight open
requests for that account, but it creates neither the globally unique domain
allocation nor a permanent domain tombstone. Rejection, retirement before
proof, or expiry releases the reservation and leaves the name available for a
new request.

When verification is enabled, every new pending challenge is immediately due
for the bounded scheduled verifier; a platform administrator may also request
one retry-safe exact TXT lookup. The resolver uses one fixed HTTPS DNS endpoint,
accepts only an exact TXT owner and byte-equal challenge value, bounds the
response, follows no redirect, performs no DNS write, and records the observed
RRset digest, minimum TTL, and DNSSEC-authenticated-data bit. DNSSEC is
evidence, not currently a requirement. The first request whose exact TXT value
is observed atomically changes to `verified` and creates the permanent global
allocation. If a competing pending request later reaches proof, it records
`ownership_verification.state="conflict"`,
`last_result="domain_unavailable"`, and
`availability="unavailable_domain"`. It waits until its original seven-day
challenge deadline and then expires instead of polling DNS indefinitely. This
is the ownership boundary: only verified ownership allocates or permanently
tombstones the name.
That losing observation is recorded as audit action
`custom_domain.verification_conflict`; policy convergence uses the existing
`custom_domain.verification_deferred` action with reason
`account_policy_converging`.

External resolution does not run while the global Durable Object holds its
authority lane. The stateless Worker first obtains a short, durable
`claim_id`/generation fence bound to the exact request revision and ownership
challenge, resolves DNS outside the authority object, stores one reduced
observation under that fence, and then asks the authority to commit it. Raw TXT
answers are not stored in the work record. Commit rechecks the runtime gate,
claim generation, request revision, challenge identity and expiry, account
policy convergence, and plan/lifecycle suspension before changing authority.
A late result cannot remove or commit through a newer claim. Scheduled work is
bounded to five claims per tick with two concurrent resolver lanes; overlapping
ticks cooperate through the durable fences rather than duplicating a lookup.
The Durable Object alarm continues to reconcile local lifecycle/expiry work,
while the top-level scheduled Worker owns external DNS I/O.

Verified requests are rechecked every 24 hours. An authoritative missing value
marks the ownership observation `stale`, changes availability to
`suspended_verification`, and schedules an hourly retry; a resolver outage is
recorded as a deferred observation and retried after 15 minutes without
revoking the last authoritative result. A matching recheck restores verified
availability. The v0.0.238 ownership-verification release records authority
only: it does not publish MX, Cloudflare Email Routing, an edge route, a cell
projection, or mail delivery.
If plan or lifecycle work is between durable pages, verification records
`last_result="policy_converging"` and retries after 15 minutes rather than
observing DNS against a mixed account policy.

Plan application is fenced around the cell commit. Pending requests outside a
new allowance are suspended immediately. Verified allocations outside the new
allowance enter a 30-day `active_grace` window and then become
`suspended_plan`; an upgrade clears plan suspension and resumes ordinary
verification without reinstalling a client. Account move/archive work first
sets `suspended_lifecycle`, then exact-fence republish clears it. Account close
retires every request; a verified allocation becomes a permanent retired
tombstone, while an unverified request never acquires one. The request state is
one of `pending_verification`, `verified`, `rejected`, `expired`, or `retired`;
the independent `availability` field distinguishes plan, lifecycle, and
verification suspension from that durable ownership state.
Rejection, retirement, or expiry of an in-limit request durably queues a
bounded same-plan rebalance so the next eligible suspended request is
promoted; a released capacity slot cannot remain stranded.

The global custom-domain request registry now has a portable, append-only
authority journal and bounded empty-target recovery foundation. Each authority
mutation writes a create-only, hash-chained R2 after-image before the mutation
can report success. Bootstrap and checkpoint freeze authority writes while
they scan bounded pages. Recovery accepts only an exact `aedj_` journal head
and replays into the named empty Durable Object
`recovery:<aedrec_...>`. It rebuilds derived indexes, compares the complete
authority digest, and permanently seals the drill target. The active authority
object remains fixed at `global`: there is no merge, promotion, or cutover
selector. Journal maintenance never changes DNS, Cloudflare Email Routing,
cell projection, delivery, plan policy, or the customer request gate.
The journal and recovery implementation has a hard 10,000-authority-key
ceiling. It is not a customer-facing plan limit. A journal-local capacity record
is bound to the exact stream head and tracks the total plus a fixed prefix
breakdown. Bootstrap, checkpoint, and sealed recovery materialize it; every
later authority mutation computes its insert/delete delta and refuses before
staging a pending record or writing R2 if it would cross the ceiling. Journal
status exposes only value-free `used`, `max`, `remaining`, `near_limit`,
`at_limit`, and prefix counts, and a refusal emits one tenant-free structured
operational event. An existing journal head created before this counter remains
write-fenced until a normal checkpoint installs the exact capacity record.

External DNS waits now use the durable claim/observe/commit flow above, and the
plan lifecycle scanner replays an exact already-accepted desired revision/hash
instead of minting a new revision after a lost bridge completion. When a
scheduled verification has the same durable evidence outcome, it replaces one
journal-local `verification-refresh:<request_id>` record and moves the one
derived due entry. That bounded refresh carries the newest check clocks, retry
counter, recursive-resolver TTL, and next schedule. List and show responses
overlay those operational values onto the public `ownership_verification`
projection, while the authority request and allocation, audit/meta state,
head-bound capacity, journal head, and immutable R2 object count remain
unchanged. The refresh is bound to the exact request revision and challenge;
its generation is part of the claim fence, so an older scheduled result cannot
overwrite a newer refresh.

Writing the first refresh is a forward-only activation boundary for releases
that predate the `verification-refresh:` key class, including v0.0.237. A dark
deployment remains rollback-safe while verification is disabled and no refresh
exists. Once enabled, do not deploy a pre-refresh release: it cannot classify
the local record or preserve its effective due entry. Supporting that downgrade
would require a bounded, tested drain that stops claims, removes every refresh
and work record, restores authority-derived due entries, checkpoints the exact
head, and proves zero refresh keys plus exact derived parity. No such production
drain is exposed in this release, so later live activation is forward-only.

First observations, evidence or ownership-state changes, restorations,
conflicts, expiry, and every newly executed manual verification remain
authority mutations and are audited and journaled. Recovery deliberately drops
local refresh and work records, rebuilds the due queue from the last journaled
request for derived-state parity. The sealed drill object has no alarm and does
not execute that queue. If a future separately reviewed activation protocol
ever made restored state live, its first scheduled check would conservatively
recreate the bounded refresh when the outcome was still unchanged; no such
promotion path exists today. Stable scheduled checks therefore no longer grow
the R2 stream, although genuinely changing DNS and manual checks can still
consume audit capacity until admission closes.
Keep the request and verification gates dark until the refresh and claim
fences have passed a controlled canary, the capacity counter has been
checkpointed and monitored in the live object, and exact plan replay has passed
its canary. Provider route, projection, and delivery topology remains a
separate activation prerequisite.

The schema-88 slice adds only that dark routing foundation. It
does not activate a provider route. Control-plane routing requires both
`CP_AGENT_EMAIL_CUSTOM_DOMAIN_ROUTING_ENABLED=true` and exact account membership
in `CP_AGENT_EMAIL_CUSTOM_DOMAIN_ROUTING_ACCOUNT_ALLOWLIST`; neither name is in
committed release configuration. A non-managed-domain edge transaction has a
third independent exact-`true` gate,
`AGENT_EMAIL_CUSTOM_DOMAIN_DELIVERY_ENABLED`, which is also absent. The request,
verification, routing, and delivery controls do not imply one another.

After those two control-plane gates pass in a future reviewed canary, one route
lookup proves all of the following before it can publish anything: the domain
has one permanent verified allocation; the allocation and request are a
consistent verified-or-retired authority under plan, lifecycle, and ownership
policy; the realm-alias registry returns the exact claim for the same account
and label; and the claim identifies one exact realm.

The controller records the sparse relationship before its first external
write. The custom-domain registry journals one permanent
`route-binding:<domain-request-id>:<realm-alias-claim-id>` authority row, then
the alias registry journals the corresponding permanent
`custom-domain-subscription:<realm-alias-claim-id>` marker. Only after both
registries acknowledge that handshake may the controller persist and apply a
leaf route intent. These records describe only combinations that were actually
materialized; the controller never constructs the potentially unbounded
verified-domain by alias-claim cross product, and it never treats cell inventory
as global membership authority. Neither record is removed when the route is
suspended or retired, so later lifecycle work can still find and preserve the
terminal tombstone.

The cross-registry acknowledgement also closes the only crash window in that
ordering. Once realm close starts, the alias registry refuses a first
subscription for that realm. If the domain binding was journaled immediately
before that fence, the controller can complete the now-retired,
never-subscribed binding without I/O: absence of the subscription proves that
no cell or KV write was allowed to begin.

A domain request or allocation change journals one overwriteable
`route-source-intent:<domain-request-id>` outbox. An alias claim change journals
one overwriteable `custom-domain-sync:<realm-alias-claim-id>` outbox, but only
when that claim has the permanent subscription marker. Derived account, realm,
due, and reverse-binding indexes make both fan-outs bounded and rebuildable.
The custom-domain registry pages the exact bindings, re-proves both source
authorities for every leaf, applies the projection to the account's current
cell, reads it back byte-for-byte, and only then publishes the edge projection.
A lost enqueue, cell, readback, or KV acknowledgement is replayed. A newer
source revision resets the bounded work instead of letting stale desired state
win. Turning the activation gate off creates no subscription or alias outbox for
an account that has never materialized a custom route; existing subscribed
claims still drive suspension and retirement while the gate is dark.

Direct domain and alias administration commits its source authority and durable
outbox before returning, then converges the already-known leaves eventually.
The normal freshness window is 300 seconds. Because the edge accepts an
`updated_at` value up to 300 seconds ahead of its own clock, the formal stale
route window is 600 seconds when that full accepted clock skew is present.
Parent operations use a stronger contract: plan application and account
lifecycle cannot install their final fences until their exact account outbox
indexes are empty, and realm close cannot prepare the cell until every
subscribed route in that realm is proven retired in both the cell and edge
directory. Those barriers wait for positive completion; they do not infer
completion from elapsed cache time or from merely accepting a child task.

The sparse bindings, subscriptions, and source outboxes are journaled authority.
Their reverse, account, realm, and due indexes are derived and rebuilt during
empty-target recovery. Leaf `route-projection-intent` records and the bounded
alias fan-out tasks are journal-local coordination state and recovery drops
them. Only a source or alias-sync outbox that was genuinely pending at the
replayed journal head regains its corresponding derived due entry. A completed
permanent binding is rebuilt as authority plus its sparse reverse indexes; it
does not create a recovery-due key, convergence obligation, or alarm merely
because it exists. The sealed target does not drain recovered outboxes, perform
cell or KV projection, serve routing traffic, or become eligible for automatic
promotion. A future active-restore protocol would need a separately reviewed,
explicit activation step; it is not part of this recovery contract.

The cell stores the exact tuple of domain request, domain-allocation revision,
domain-request state revision, realm-alias claim and revision, realm, account,
domain, label, lifecycle state, and monotonic controller revision. Applied,
retry-suspended, inactive-suspended, and retired rows are explicit; retired rows
are permanent identity tombstones. An applied row requires the referenced
realm-alias projection at the advertised revision and a live realm-email route.
Realm close and account movement therefore treat any non-retired custom-domain
route as live cell authority rather than an ignorable cache.

Combined route policy is applied only while both authorities are applied. If
either source is retired, the custom route is retired. Plan-only loss uses an
`inactive` suspension (generic unknown recipient); lifecycle, stale ownership,
or operational alias convergence uses `retry` (temporary failure), and `retry`
wins when both dispositions are present. The independent account, realm, agent,
and `agent_email_receive` checks still run inside the cell before storage.

The edge reuses the existing schema-v1 route shape and key
`email:realm-route:v1:<domain>:<realm-label>`, adding
`route_kind="custom_domain"` plus the exact domain request/allocation and realm
alias claim/revision fences. The existing authenticated fallback is reused;
there is no second lookup namespace. Before a control-plane response or KV row
leaves its authority, the control plane signs the complete scalar projection
as schema version 2 with a dedicated Ed25519 route-authority key. The edge
accepts one to four public keys for bounded rotation and verifies the exact key
id and signature before trusting `ingest_url` or reading raw MIME. Schema-v1
unsigned rows, unknown keys, malformed keyrings, and any mutation fail closed.
This is independent of the later signed relay envelope: the edge adds no new
relay header. The
cell derives `custom_domain` receipt provenance from the signed envelope
recipient and its local route/alias rows, then stores both
`recipient_custom_domain_request_id` and
`recipient_realm_alias_claim_id`. Unsigned edge metadata can neither choose an
account, realm, nor agent nor manufacture that provenance.

Schema-88 account archives include the custom-domain route table before
messages and retain both identifiers on every custom-domain receipt. Import
validates the complete account/realm/alias/domain binding and never infers a
route from an address. A schema-87 archive upgrades with an empty route stream
and null custom-domain provenance. Downgrade to schema 87 is refused before
mutation if any custom-domain route exists, including a retired tombstone, or
if any custom-domain message exists. Roll application behavior forward or back
while leaving schema 88 intact; never delete authority to force a downgrade.

This foundation may be exercised only with the repository's fake Cloudflare
bindings, synthetic SMTP transaction, and disposable database tests. It makes
no DNS, MX, Cloudflare Email Routing, catch-all, Worker-route, or customer-zone
mutation, and no live route or delivery gate may be enabled in this slice.

Do not enable customer requests or DNS verification in this phase. Parallel,
expiring challenges avoid unverified squatting, but activation still requires
a reviewed account canary, capacity evidence, transfer/quarantine governance,
and operational acceptance of the ownership and lifecycle state machine.

The CLI surface ships with the dark foundation, so a later entitlement or gate
change never requires reinstalling a client. While the request gate is absent,
the request command returns the service's disabled response and list remains
read-only:

```sh
witself email-domain request --domain agents.example.com
witself email-domain list

witself-admin email-domain requests list
witself-admin email-domain requests show --request "$REQUEST_ID"
witself-admin email-domain requests verify \
  --request "$REQUEST_ID" --idempotency-key "$VERIFY_KEY"
witself-admin email-domain requests reject \
  --request "$REQUEST_ID" --reason "Domain is not eligible"
witself-admin email-domain requests retire \
  --request "$REQUEST_ID" --reason "Customer withdrew the request"
witself-admin email-domain audit
```

With the request and verification gates absent, the scheduled verifier is a
bounded no-op and this dark slice performs no DNS lookup or write. Even with
verification deliberately enabled for a future allowlisted canary, it still
performs no Cloudflare zone or Email Routing mutation, MX activation, or
outbound-domain configuration. The dark routing code can create cell and
edge projections only behind its two absent control-plane gates, and the Email
Worker still tempfails every customer-owned domain before lookup while its
separate delivery gate is absent. Routing, customer request creation, ownership
verification, provider onboarding, and delivery require separate activation
reviews; managed-domain activation never implicitly enables this lifecycle.

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
   explicit error, and the operator supplies an explicit agent segment (same
   charset rules) recorded on the address record. New agents accept it through
   `witself agent create --email-agent-segment`. That flag uses the strict
   v0.0.241-only `POST /v1/realms/{realm}/agents:with-email-segment` mutation;
   the ordinary create route never carries the field, and a v0.0.240 server
   returns 404 before creating anything instead of silently ignoring it. The
   supplied segment must already be byte-for-byte canonical lowercase ASCII;
   empty, whitespace-only, normalized, reserved, and unknown-field requests are
   rejected before the create callback. Existing production-cohort agents
   accept an override only through the private, preflighted
   `witself-server agent-email backfill --exception-output ABSOLUTE_PATH
   --overrides ABSOLUTE_PATH` workflow. The exception artifact is private,
   mode `0600`, and created only when operator review is required. No silent
   auto-suffixing — provisioning order never changes an address.

Sanitized segments can never contain a dot, so the address grammar is
unambiguous: after stripping any RFC 5233 subaddress tag (`+tag`), a valid
local part contains exactly one dot, separating agent segment from realm
label. Anything else is structurally invalid and rejected at SMTP time.

Requirements regardless of format:

- Reserved names are blocked at both the agent-segment and full-local-part
  level: the RFC 2142 role set (`postmaster`, `abuse`, `hostmaster`,
  `webmaster`, `noc`, `security`), `mailer-daemon`, `admin`, `root`,
  `noreply`/`no-reply`, and kin. RFC-required roles at the apex
  (`postmaster@witmail.net`, `abuse@witmail.net`) route to the operator, never
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
- The `agent-mail.witwave.ai` to `witmail.net` cutover is a real migration for
  previously issued canonical local parts. External services may hold those
  launch-domain addresses on file, so each reviewed legacy address must keep
  receiving through exact-address compatibility after `witmail.net` activates.
  The legacy domain must not mint new canonical addresses or aliases, and it
  must not gain a broad catch-all. Every address a third party ever saw must
  keep working until its agent is done with the accounts behind it.

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
  the `witmail.net` apex this is the documented zone-apex catch-all; on the
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
  and stored with recipient metadata. The pilot uses the same 25 MiB technical
  ceiling and enforces any lower account plan limit inside the owning cell.
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
   - `rate_limited` with HTTP 429 (a rolling inbound breaker that can free up)
     → the provider's sanitized temporary-retry path;
   - `permanent` with HTTP 410 (a zero cap or one debit larger than its
     effective bucket) → 550 permanent, because waiting cannot make it fit;
   - `mailbox_full` or other permanent-refusal conditions → 550. Recoverable
     rate pressure stays distinct from permanent capacity refusal so the
     Worker never maps a temporary breaker to a permanent bounce.
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
  stored directly in Postgres under the resolved
  `agent_email_max_raw_bytes` limit and the edge provider's defensive ceiling;
  parsing failures preserve the raw bytes and record a parse-error state
  rather than dropping mail. No object store or file-management layer is
  introduced in this epic: retention windows are the pressure valve on table
  growth, and an object-storage adapter is revisited only if measured volume
  demands it.
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
  but may gate retrieval until that review lands (open question). A retained
  message containing attachments charges its complete raw-MIME size to the
  account-wide `agent_email_attachment_storage_bytes` allowance. There is no
  separately stored attachment payload to count. If that pool lacks room, the
  bounded text and metadata remain available while the raw MIME containing the
  attachment bytes is explicitly marked unretained.

The pipeline items above describe the production requirements. In the pilot,
provider-id idempotency is replaced by non-destructive suspected-duplicate
grouping, raw MIME is capped at 25 MiB (or a lower account plan limit),
structured auth/spam fields are
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
- Retention is plan-scoped: raw MIME, including inline attachment bytes, ages
  out by plan window;
  quarantined spam ages out faster; metadata and content-free audit events
  follow [audit-retention.md](audit-retention.md). Account archives include
  the mailbox (addresses, messages, state) with the same interrupt-on-import
  handling of active claims that messaging archives use.

## Surfaces

The pilot shapes are pinned in [json-contracts.md](json-contracts.md),
[cli-command-surface.md](cli-command-surface.md), and
[mcp-tools.md](mcp-tools.md):

- CLI: `witself email status`, `witself email address show`, `witself email
  list`, `witself email read`, `witself email code-candidates`, `witself email
  code-consumed`,
  `witself email claim|renew|release|complete`, `witself email ack`,
  a bounded `witself email listen` (wait for new mail — the OTP flow needs a
  sanctioned wait rather than a poll loop, mirroring `message.listen`), and
  operator-only `witself email operator receive show|enable|disable` for one
  exact enrolled agent or realm.
- MCP: `witself.email.*` mirroring the CLI, with metadata-only list results
  and untrusted-content framing in every content-bearing tool description.
- API: owner routes are `GET /v1/email/address`, `GET /v1/email:status`,
  `GET /v1/email`, `POST /v1/email:listen`, `GET /v1/email/checkpoint`, and the
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

- Six rolling one-minute count/byte breakers are enforced transactionally at
  ingestion: 30 messages and 64 MiB per normalized unverified
  envelope-sender/enrolled-recipient pair, 300 messages and 512 MiB per
  receiving agent, and 5,000 messages and 4 GiB per realm. The sender scope is
  hashed operational state, not authenticated identity; recipient and realm
  breakers remain necessary against spoofing and sender rotation. A refusal
  rolls back every earlier debit, stores no message, creates no billable usage
  event, and takes the temporary provider-retry path rather than being silently
  dropped after storage.
- The six account limit keys are intentionally missing from all current plan
  defaults. Missing or explicit-unlimited means no commercial cap while the
  independent platform breaker remains. An administrator may set an audited
  account maximum at or below that breaker, but cannot raise or disable it.
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
  The limited pilot resolves billing conservatively by excluding received mail
  from usage overage charges. It still enforces the non-billable raw-message and
  account-wide attachment-storage plan caps: a message that cannot fit the
  attachment pool retains bounded text and metadata but not its raw
  attachment-bearing MIME.
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
  The plan caps are separate from those usage observations:
  `agent_email_max_raw_bytes` limits each inbound raw message, while
  `agent_email_attachment_storage_bytes` pools retained attachment-bearing
  MIME bytes across the whole account. The latter counts the complete raw
  message whenever its MIME tree contains an attachment; it is not a
  per-agent allowance and does not imply extracted attachment blobs.
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
3. **Pilot settled 2026-07-21:** only value-free attachment metadata is exposed:
   count, storage byte counts, and payload-retention state. Raw MIME, attachment
   names, media types, and attachment bytes are unavailable. A future production
   retrieval surface still requires the injection review.
4. The quarantine window and the Postgres growth watermarks that would trigger
   revisiting object storage. Ordinary retention windows and inbound storage
   caps are plan-scoped in [billing-and-limits.md](billing-and-limits.md).
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
8. Legacy canonical compatibility duration after `witmail.net` activates.
   New canonical addresses and aliases are `.net`-only; the compatibility
   manifest may contain only previously issued canonical local parts on
   `agent-mail.witwave.ai`. The exact retirement evidence and minimum support
   window for each such address still need to be settled.
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
11. Edge observability baseline is implemented through best-effort, value-free
    Analytics Engine points. `witself.agent-email.edge.v1` records each
    SMTP-facing outcome, including the closed `tempfail_rate_limited` outcome
    with phase `response` for an authoritative retryable cell refusal.
    `witself.agent-email.route-lookup.v1` separately records fixed route result,
    evidence, and route-kind enums plus count, latency, and numeric status; it
    emits exactly one terminal event per recipient lookup and never records an
    address, domain, realm label, limiter key, or tenant identifier. A corrupt
    or failed KV read that continues to the control plane is represented by
    `evidence=uncertain`, not a second early `kv_error` event.
    Export from that edge dataset into the wider platform metrics plane remains
    promotion work.

Raised by the 2026-07-21 whole-spec gap review (blocking items were settled
in place; these are the remaining important items):

12. **Pilot settled 2026-07-21:** `read` returns bounded decoded text, prefers
    plain text, deterministically reduces HTML, never returns raw MIME or
    attachment bytes, and surfaces a value-free parse-error code. Parsing is
    bounded to 25 MiB raw MIME, 256 KiB headers, 64 MIME parts, depth 8, and
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
    and `DMARC p=reject` on `agent-mail.witwave.ai` / `witmail.net` (they
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
