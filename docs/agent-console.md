# Local Agent Console

Status: implemented as the loopback-only `witself dashboard` command. The
stable feature-status id remains `agent-dashboard`; “Agent Console” is the
product-facing name for this local presentation surface.

The native [agent terminal workspace](agent-tui.md) is available through
`witself tui`. It shares these passive projections without a local web server,
uses a monogram instead of an avatar image, and adds deliberate locally
decrypted secret-field reveal and clipboard access. The browser's Secrets
panel remains metadata-only. Press `b` in the TUI to start or reuse a verified
console and open it in the browser; `w` provides status and start/stop controls.

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
| Overview | Shared colored summary with inventory, recorded hourly activity, bounded recent updates, and value-free checkpoints; identity and avatar; workspace details with salient memories, capacity, enforced plan and retention | Private content in the summary; combined totals across activity units; domain mutations; billing administration; cross-agent usage; feature-progress authority |
| Transcripts | Observational transcript inventory and entries | Append, retention-policy changes, and evidence mutation |
| Facts | Observational redacted inventory and history; one explicit exact reveal where authorized | Set, propose, confirm, reject, or delete |
| Memories | Redacted inventory, detail, version history, and evidence | Create, adjust, curate, supersede, forget, restore, or delete |
| Conversations | Passive message/thread metadata and an explicit Show body / Hide body preview for received messages | Payloads, sender-only body access, and listen/read/claim/acknowledge/send/reply actions |
| Email: Received | Managed address and receive state, account-wide storage capacity, sender and subject, receive/read/acknowledgement/processing state, raw-message size, attachment count, aggregate attachment-storage and retained-byte totals, retention warning, duplicate warning, and provider-supplied authentication/spam signals | Email ids, account/realm/owner/mailbox ids, decoded body, raw MIME or headers, per-attachment names, media types, or content bytes, cursors, claim fences, and read/listen/acknowledge/claim/complete/reply actions |
| Email: Sent | From, Reply-To, recipient, subject, request kind, durable outbox state, provider-neutral status/error metadata, and lifecycle timestamps | Send ids, account/realm/owner ids, reply-parent ids, submitted body, idempotency material, worker claims, provider ids or payloads, cursors, and send/reply/retry/cancel actions |
| Secrets | Names, field names and sensitivity flags, lifecycle, timestamps, counts, and public vault-binding metadata | Ciphertext, wrapped keys, field values, reveal, TOTP, lifecycle actions, and runtime injection |

Received senders, subjects, provider-supplied authentication/spam results, and
provider-neutral outbound error codes remain untrusted external data. The proxy
rebuilds both email projections through narrow allow lists and the browser
renders them only as text.

The summary's Overview, Timeline, and Recent updates views share the terminal's
category colors, codes, and metric meanings. Facts and active memories have
exact counts when available; other inventory counts are bounded recent pages.
Recorded activity uses 24 UTC hour buckets, including the partial current hour:
transcript entries, fact deliveries, secret accesses, accepted email sends,
and messages sent. Operations adds recorded reads plus writes, with separate
reads, writes, records-read and records-written quantities. Memories keeps
exact active inventory and adds created, revised, archived, restored and deleted
changes. Fixed subtext exposes both breakdowns in Overview and Timeline.
These are separate units, not a combined transaction count or a complete audit
trail. New totals describe only the recorded portion: `?` marks unknown bins
before tracking, and the first tracked hour and current hour are partial.
Older clients may omit activity. **Not tracked yet** means no durable marker;
**Server update needed** means `/v1/activity` returned 404; errors or invalid
reports are **Unavailable**, and explicit feature refusal is **Disabled**.
The five legacy `/v1/usage` metrics work independently on older servers.
See [agent activity metrics](agent-activity.md) for the counting rules.
Recent updates describe observations from loaded
records, not every change. See the [terminal summary guide](agent-tui.md#visual-summary).

The local `/api/summary` projection is passive, content-free, identity-scoped,
and cached for at most 30 seconds. Browser refresh runs only while Overview is
visible, keeps the selected view and keyboard focus, and labels retained data
as stale after failure. The footer says cache 30s; polling can pause or vary.
Workspace details open explicitly beneath the summary.

### Browse, open, and back

Memories and Conversations share a focus view. Browse the filtered list, use
Up/Down (or Home/End) to select a visible row, and press Enter or Space to open
it. The list collapses to an identity header with a visible Back control, and
the detail uses the available content width. Back or Escape restores the filter,
selected row, keyboard focus, and list scroll position within this page.

Open details keep their `#/memories/:id` and `#/conversations/:key` URLs;
list URLs represent browse mode. Deep links and browser Back/Forward work too.
A deep link's Back loads the inventory if this page has not browsed it yet.
Filter and scroll state stay in the page, not in the URL or persistent storage.
Opening a memory fetches its existing redacted detail and version history;
conversation open/back reuses already loaded metadata. Private content never
auto-reveals: memories the server redacts stay redacted, an exact memory
read shows what it always showed, and received message bodies still require
Show body. Leaving a conversation clears any revealed body.

Conversations fetches a received message's body only when Show body is selected,
using the existing recipient-only observational peek API. The local proxy
returns only the body text; list and live-refresh responses remain stripped of
bodies and payloads. The preview preserves whitespace, renders untrusted content
as plain text, and clears on Hide body or navigation. It changes no read,
acknowledgement, or processing state. A compatible backend is required; missing,
disabled, forbidden, or unavailable previews show a per-message unavailable
state without calling another endpoint or retrying automatically.

The sent lifecycle vocabulary is `queued`, `claimed`, `provider_started`,
`accepted`, `delivered`, `deferred`, `bounced`, `rejected`, `failed`,
`ambiguous`, or `canceled`. It reports the durable cell view; it does not invent
delivery success. Both Received and Sent are bounded newest-first projections
without identifier-bearing browser cursors.

## Entitlement And Compatibility States

The seven agent sections use the selected agent token and never grant a feature. A
disabled account, disabled agent or realm layer, unenrolled agent, pre-feature
cell, or temporarily unavailable upstream produces an explicit disabled,
not-enrolled, or unavailable state instead of a generic broken panel. A later
plan or account-policy change takes effect through the same installed command;
no Console reinstall is required.

The Overview requests `include_plan_entitlements=true` on that token-bound
`GET /v1/self` read. A current cell returns the closed
`witself.agent-entitlements.v1` block sourced only from its already-applied
account snapshot:

- `state` is `applied`, `unmanaged`, or `unavailable`, and `source` is always
  `cell_applied_snapshot`; a pre-feature cell omits the whole block;
- `enforced_plan_id` is a bounded clean identifier and appears only for
  `applied`;
- `features` contains exactly `memory`, `facts`, `secrets`, `messaging`,
  `collaboration`, `agent_email_receive`, and `agent_email_send` booleans;
- `retention_days` contains exactly `transcript_retention_days`,
  `message_retention_days`, and `agent_email_retention_days`; JSON `null`
  means indefinite.

The cell makes no control-plane, catalog, or billing-provider call for this
projection. The browser proxy rebuilds it through a second explicit allow
list, and the Overview card has no action target. Subscription state, payment
method, provider details, revisions, hashes, URLs, pending changes, reasons,
admin/support state, limits, and account or cross-agent usage do not enter this
agent projection. The separate Account area below uses current manager authority.

The `messaging` and `agent_email_receive` booleans reuse their production
entitlement-version marker logic, including the bounded legacy rule that an
applied snapshot predating the marker remains allowed until a modern snapshot
lands. The card therefore reports what the current cell enforces, not merely
raw feature-array membership.

The Console may be accepted as a presentation surface while a domain feature
remains limited or dark, provided it accurately shows the applicable disabled
or unavailable state. Domain readiness remains on that domain's feature-status
row.

## Account: current manager, selected agent

Account is a separate read-only area in both the browser and the
[terminal workspace](agent-tui.md#account-navigation). The existing seven agent
sections keep the selected agent's authority. Account instead uses the original
current CLI operator, independently verified for the **same canonical account**.
No verified manager means no Account navigation or options, not a disabled
entry. Missing account capability does not prevent ordinary agent browsing.
Explicit `--endpoint` or `--token-file` selects an agent-only console, even if
local manager credentials exist.

For a managed account selection, the CLI checks the original account metadata
against the authenticated agent, freshly resolves its trusted directory
endpoint, and compares the fixed origins before loading that account's operator
credential. It does not substitute a default owner or a later secret-vault
selector. Every Account read rechecks current authorization. An authority error,
revocation, or mismatch clears Account content and removes its navigation; a
role or permitted-section change clears prior projections. Delayed results
cannot restore the old context. A resource error such as `response_too_large`
does not itself revoke manager authority.

The browser's separate bottom **Account** entry opens a compact Account header
with a **Read-only** badge, account identity, manager role, and links only to
permitted subsections. Access identifies the current manager separately from
the selected agent. The ordered subsections are:

| Subsection | Read scope |
|---|---|
| Overview | Account ID, display name, status, contact email, and created time |
| Clients on this device | Cached metadata from a deliberate local check of recorded installs for this account |
| Plan & limits | Current/applied plan, effective limits and defaults with units, features, overrides, retention, and reported pending changes and dates |
| Billing | Provider-neutral billing summary, invoices, and payment history, with separate availability for each source |
| Support | Bounded ticket metadata and a deliberately selected, temporary text-only thread |
| Access | Current verified manager identity, role, and available read sections; no token inventory |

Recognized manager roles are `account_owner`, `account_admin`, `account_billing`,
and `account_operator`. Plan & limits and Billing require one of the first
three; `account_operator` sees the other four subsections. Current backend
authorization remains final. These reads add no agent authority, fleet/admin
privilege, payment or subscription action, support reply, or other domain
mutation. Existing CLI/MCP management remains separate.

### Plan and billing interpretation

For a supported limit, absence from a present, valid source map means **No plan
cap**; an absent or null source map means **Unknown**. Zero is a real cap.
Platform ceilings still apply even when no plan cap is set. Effective and default
limits are separate, and the Console does not invent account usage. Explicit
null retention means **Indefinite**; absent retention means **Unknown**.

Billing preserves reported integer cents and currency, including zero; missing
or null amounts stay unknown. The TUI displays exact cents; the browser formats
safely representable integer amounts and shows unknown for amounts it cannot
represent exactly. It does not infer prices from a marketing catalog or add
currencies together. A failed summary, invoice, or payment read remains
unavailable while successful sources can still display; failure is never an
empty successful history. No provider/customer identifiers or payment, setup,
portal, checkout, cancellation, or pending-action URLs are exposed.

### Clients on this device

Use **Check this device** to inspect recorded installs for this account on the
computer running the CLI. Navigation, polling, and **Refresh selected section**
only read the last cached report; they never start a scan. The inventory check
uses local metadata without provider execution, network calls, credential reads,
repair, or enrollment. Account authorization is still checked separately.

The report distinguishes recorded version, recorded executable presence, static
MCP registration status and scope, **Effective verification: Not run**, recorded
installation time when usable, and local check time. Presence does not prove
execution or executable permissions; a recorded version is not a current
version probe. Check time is not last activity, online status, or fleet coverage.

All eight install-record types are considered: Codex, Claude Code, Grok Build,
Cursor, OpenClaw, Antigravity, Copilot, and DSH. Only nonempty canonical account
IDs matching this account qualify. Static MCP registration checks support the
default roots for Codex, Claude Code, Grok Build, and Cursor. Custom provider
roots and the other four configuration topologies are unsupported. Checks do
not verify hooks, plugins, instructions, credentials, or effective runtime
settings. Executable presence checks cover only permitted recorded regular
files in local user bin directories; other paths remain unchecked. The scanner
supports macOS and Linux; other platforms cannot perform the check. Unsupported,
partial, unavailable, and not-checked results do not imply a healthy or complete
installation inventory.

### Support and bounded reads

Support lists at most 100 tickets with an explicit truncation marker. Select
**Read ticket** to fetch a thread; list and refresh reads do not fetch bodies.
Thread text is inert, with no attachment payloads or automatic link execution.
The selected body is ephemeral and cleared on refresh, hiding, navigation, or
identity/role/authorization change. Select the ticket again to read it; old
private text is never automatically restored.

The existing support endpoint returns the whole thread. Its transport response
must fit within **4 MiB before decoding**, even if the eventual display would be
smaller. A larger response produces only the fixed `response_too_large` error
(local HTTP 502 or Reader error) for that resource; Account access remains
available. There is no console pagination or automatic fallback. Deliberately
inspect it through the existing CLI when needed:

```sh
witself account support show --account NAME --ticket TKT_ID
```

For an admitted response, the display retains the newest 100 messages in their
chronological order and caps each body at 16 KiB, with truncation marked. Other
upstream Account responses have a 2 MiB transport cap. Reads have bounded
deadlines and expose fixed errors rather than raw provider diagnostics.

## Opening and reusing a local console

`witself dashboard open --agent NAME` verifies and opens an existing local
session; use the same account and realm selectors as its launch. It
independently re-verifies the current agent and manager, then checks the exact
live session before opening. Add `--print-url` for deliberate manual browser
opening. **That output is private:** it carries the local session capability,
including Account reads when the session has a verified manager. Keep it out of
shared logs, reports, and screenshots. It is not the upstream agent or manager
token.

TUI `b` starts or reuses a console and opens it; `w` manages its lifecycle.
Reuse checks canonical account, realm, and agent identity plus the immutable
manager binding and current availability. Agent-only and manager-enabled
sessions do not substitute for one another, and one manager cannot reuse
another's session. A revoked manager-bound session remains manager-bound even
when Account is unavailable. Legacy consoles qualify only as agent-only.
Mismatching or unverifiable running sessions are preserved and reported as a
conflict; cleanup is fenced to the exact owned instance.

The managed child receives its explicit connection context through a bounded
private stdin bootstrap and reauthenticates both principals; it does not load
ambient manager credentials. Manager bearers never enter browser state, argv,
environment, registry, or routine output. Routine `status`, `stop` text/JSON,
and startup banners suppress manager-session opening URLs. The private local
registry stores that capability in `manager_access_url`, with legacy
`access_url` empty, so older readers cannot expose it. This is same-OS-user
custody, not a privilege boundary against the local account owner.

Before downgrading the CLI, stop manager-enabled consoles with the launching
or a newer CLI's `dashboard stop`, Ctrl-C in the foreground serve, or by quitting
the TUI that owns the child. An older CLI cannot prove ownership of these
sessions and leaves them running; its “no live dashboard to stop” result is not
shutdown confirmation. A TUI that merely reused a console does not own it.

## Surface Taxonomy

- **Local Agent Console (`witself dashboard`)** — this document: one local
  per-agent browser surface on `127.0.0.1`, with agent-token panels and the
  conditional same-account manager read area above.
- **Fleet-admin TUI (`witself-admin dashboard`)** — a separate local terminal
  surface for cells, support, and fleet events. It is not an Agent Console and
  has fleet-admin authority.
- **Hosted console** — deferred. A hosted browser application would require a
  separate feature-status row and its own authentication, sessions, tenancy,
  CSRF, availability, observability, recovery, and managed-rollout evidence.
  It must not scrape or inherit trust from the local Console.

## Release Acceptance

The `dashboard-acceptance` job in `release.yml` builds native clients and runs
headless Chromium on Linux, macOS, and Windows against a shared stub cell,
checking browser authentication, fixture rows in all seven panels, redaction,
and `status`/`stop`. It retains each panel's screenshot, accessibility snapshot,
DOM snapshots, and visible text plus `summary.json` as
`dashboard-acceptance-${{ matrix.target }}-${{ github.run_id }}` for 90 days,
including failure evidence. With Go and Node 22 installed, run
`make dashboard-acceptance` locally; it installs the pinned browser dependencies,
builds temporary clients, and writes evidence to `evidence/dashboard-acceptance`
(use a fresh directory on reruns with `DASHBOARD_ACCEPTANCE_OUT`). The `dashboard-release-acceptance`
catalog gate flips only once a real release has retained the artifact.

Current release acceptance must retain macOS, Linux, and Windows evidence for:

- `serve`, browser authentication, all seven panels, live refresh, `status`,
  and conservative `stop`;
- both Received and Sent email projections, capacity and lifecycle changes,
  and strict absence of bodies, ids, provider payloads, and action targets;
- disabled, not-enrolled, pre-feature, and temporarily unavailable states;
- token isolation, redaction, untrusted-text rendering, SSE and pagination
  bounds, live cell-applied entitlement refresh, old/unmanaged/unavailable
  entitlement states, stale-registry recovery, and theme-preference
  persistence.

The feature remains conditional until that release-specific cross-platform
artifact is retained in the canonical scorecard evidence.

The historical seven-panel release evidence does not certify this Account
extension. Account behavior is checked separately through synthetic CLI, Reader, browser,
and terminal tests. Those checks do not establish live customer-account acceptance.
