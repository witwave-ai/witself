# ADR 0004: Local Read-Only Agent Dashboard Served By The CLI

Status: accepted; implementation in progress (2026-07-19).

## Context

Operators working next to a running agent want to watch it work: which
transcripts are growing, which memories were captured or curated, and which
sealed secrets the agent owns. Today the only live window is the CLI, one
query at a time.

[post-v0-roadmap.md](../post-v0-roadmap.md) defers a "Web Dashboard" and
[requirements.md](../requirements.md) requires that managed-service
administration never need one. Both constraints are about the operator/admin
web surface for account, billing, and fleet workflows. Neither rules out a
strictly local, strictly read-only convenience view, but the roadmap binds any
dashboard to one rule: it must reuse the same public API, authorization,
audit, and redaction rules as the CLI rather than adding a privileged
web-only path.

The repo also deliberately retired daemon-style local runners; long-lived
local processes are foreground commands the operator starts and stops.

## Decision

Ship `witself dashboard serve`: a foreground CLI command that serves a local,
read-only, live-updating HTTP dashboard for exactly one agent.

### Same API, same authz, same redaction

The dashboard process is a thin proxy over the existing `/v1` read API using
the agent's own token via the standard connection resolution
(`-account`/`-realm`/`-agent`/`-endpoint`/`-token-file`, defaulting like every
other agent command). No widened reads: transcripts and
self digests use `observational=true` reads, messages use the passive
metadata-only list (never `:read`), broad memory reads stay redacted by
default, and the avatar SVG is re-run through the canonical sanitizer with its
hash verified before it is served — the same gate `witself self card` applies.
Inter-agent chat renders as thread-grouped conversation views built from that
same passive list; message bodies stay absent by construction, because the
only body read today (`:read`) mutates read-state, and the proxy additionally
zeroes any body or payload field before a page reaches the browser, so the
metadata-only guarantee holds even against a cell that returns them. Showing
bodies is a
deliberate follow-up: a server-side observational message body read in the
public API, consistent with the existing observational read family.
Receive-only agent email is a separate metadata-only surface built from
exactly `GET /v1/email/address` and `GET /v1/email`; the dashboard never calls
`:listen`, `:read`, `:ack`, or any processing action. The proxy rebuilds both
responses through a narrow allow-list rather than trusting the current wire
shape. The browser receives the display address and effective/agent/realm
receive states plus sender, subject, receive time, read/ack state, processing state, attachment count,
duplicate warning, and edge verdict metadata. It receives no email/message,
mailbox, owner, address, duplicate-target, or processing-fence identifiers; no
decoded text or raw MIME/header material; and no attachment details. Omitting
the message id is deliberate: the observational page has no per-message
action, so browser code has no target with which to form a read,
acknowledgement, or claim request. The upstream cursor contains a message id,
so this observational surface deliberately omits cursor pagination and shows
only a bounded newest page. Unread and unacknowledged filters are forwarded to
the passive list. Live updates re-read that bounded newest page through SSE and
never occupy an agent email `:listen` slot. A pre-feature cell
or an unenrolled agent gets a settled unavailable/not-enrolled panel rather
than a broken view, and the Overview checkpoint links to the email pane when
the value-free `email_checkpoint` says work is pending. Sender and subject are
always rendered as untrusted external input; attachment bodies remain deferred
pending their separate security review.
Facts render as a fifth surface from the redacted `observational=true` fact
list (never `include_sensitive`; the plain list records ranking-eligible
search usage, so a cell without observational fact reads gets a clear 501
rather than a silently perturbing fallback). Cells released before the
observational parameter existed ignore it and silently run the plain
usage-recording read instead of answering 501, so the proxy memoizes one
capability probe per serve — an unexecutable read pairing an unparseable
`observational` value with an unparseable `limit`, which either vintage
rejects with 400 before touching the store — and refuses every broad fact
read (the list, the SSE facts tick, and the history sensitivity check)
unless the cell proves it parses the parameter. The eye-icon reveal is a
deliberate, user-initiated per-fact observational exact read; on older cells
the plain exact read runs instead — via the 501 fallback, or directly where
the ignored parameter makes the first read plain — and recording one
legitimate delivery usage for an intentional lookup is acceptable. Sensitive
values appear only in that single reveal response — never in lists, SSE frames,
the registry, or logs — and the proxy strips sensitive values from broad
payloads by construction, exactly as it strips message bodies, so even a
misbehaving cell cannot leak them; assertion history stays locked for
sensitive facts (no per-assertion reveal in v1).
Sealed secrets render as a sixth surface now that the sealed plane has
landed: metadata only — names, field names and sensitivity flags, lifecycle,
timestamps, counts, and the public vault-key binding identifiers — from the
two public GET routes (never a lifecycle action or the field `:access`
material read, which delivers ciphertext and records audit and usage; the
list and get reads are pure metadata selects with no audit or usage side
effect, so the pane may live-poll). Unlike facts there is no eye-icon reveal
here by design: the vault key is client custody and the backend stores
ciphertext only, so the only thing a reveal could fetch is encrypted
material this dashboard must never ship to a browser. The proxy strips all
cryptographic material by construction — every secrets payload is rebuilt
through an allow-list projection, so ciphertext, sealed envelopes, wrapped
DEKs, key bytes, and even explicitly public field values cannot transit,
even from a misbehaving cell. Availability is feature-detected per cell: a
cell released before the sealed plane has no `/v1/secrets` routes, and its
404 surfaces as a clean "sealed plane not available on this cell" state
rather than a generic error. That 404 is indistinguishable from one minted
by fronting infrastructure mid-deploy, so the negative answer expires after
about a minute and is re-proven rather than memoized for the life of the
serve.

Read-only means the dashboard writes no agent content: no memories, facts,
messages, email, secrets, usage, or rankings. Its sole, deliberate write is
its own namespaced UI preferences row — today just the theme choice — behind a
dedicated pair of self endpoints (`GET`/`PUT
/v1/self/dashboard-preferences`), agent-token-only and own-row-only. The row
is a size-capped (4 KiB), strictly validated flat document
(`{"schema":"witself.dashboard-prefs.v1","theme":...}`, unknown keys
refused), last-write-wins with no revision machinery, readable with no usage
recording, and carries no audit verb by the ledger's own bar (the value-free
`agent_activity` precedent: a theme flip is not something owners need to
see, and would spam the ledger on every toggle). On the proxy, `PUT
/api/prefs` is the single mutating route: every other method is refused, the
body is size-capped before it is decoded, and only the validated
`{"schema","theme"}` shape is ever forwarded upstream. The stored theme name
is still validated against the embedded theme list in the browser before it
can become a stylesheet URL, so a tampered row can only ever select an
embedded pack.

### Local-only by construction

The listener binds `127.0.0.1` only, validates the `Host` header, and
requires a per-process random URL token delivered once at startup. Opening
the `?token=` URL exchanges it for an `HttpOnly` `SameSite=Strict` session
cookie holding a distinct per-process random value — never the URL token
itself. Browsers do not isolate cookies by port (RFC 6265), so any cookie set
for `127.0.0.1` rides along on requests to every other loopback listener the
operator visits; the exchange keeps the printed credential out of that
channel, a leaked session value dies with the process, and the cookie name is
scoped by listener port so concurrently served agents neither clobber nor
accept each other's sessions. Because that port-blindness also makes every
other loopback listener same-site, the cookie alone cannot keep hostile local
pages out: responses therefore carry `no-store` and a same-origin
content-security policy with `frame-ancestors 'none'` (plus
`X-Frame-Options: DENY`) so nothing may embed the authenticated app, and any
request a browser tags with a cross-origin `Sec-Fetch-Site` is refused before
it reaches a handler, so such pages can neither hold live-update connections
nor drive authenticated upstream polling. All assets are `go:embed`ded (no
Node toolchain, no external requests). The default port is derived deterministically from the
agent ID into a high still-valid range (50000-59999) with next-free fallback,
and each serve registers itself in `~/.witself/dashboards/<agentID>.json`
(0600, atomic write; staleness = PID liveness plus a marker-header probe of
the recorded port, so PID reuse after a crash cannot wedge the slot) so local
tooling can discover running dashboards.

### One agent per instance, foreground lifecycle

One `serve` = one agent identity, verified against `GET /v1/self` at startup.
The process runs in the foreground with signal-driven graceful shutdown, like
`mcp serve`. Live updates are produced by cursor polling of the cell read API
fanned out to the browser over server-sent events; the backend gains no push
path and the dashboard never competes for the agent's `message:listen` slots.

### Launch and discovery: the CLI is the only launcher

The MCP server gains no tool that spawns, stops, or adopts a dashboard
process. Witself MCP tools are cell API operations; a process-manager tool
would recreate the daemon-style local runner this repo deliberately retired
and hand every MCP-connected runtime spawn authority it does not need.
"Show me the dashboard" from an AI runtime is agent-driven CLI use instead:
the agent checks `witself dashboard status -json` (registry read plus PID
liveness), starts `witself dashboard serve` itself as a runtime-managed
background task when none is live, and opens the tokened URL in its
integrated browser. The process then belongs to the runtime's task manager
and ends with the session rather than becoming an orphan daemon.
`witself dashboard stop` completes that runtime-managed lifecycle: it is the
same purely local registry read, and it delivers SIGINT only after the
marker-header liveness probe confirms a live dashboard and the entry's own
tokened URL proves the answering serve minted that entry's access token (the
marker proves only "some dashboard" — another agent's serve can occupy the
recorded port after a crash — while only the owner answers the `?token=`
exchange with a redirect) — never to a bare recorded PID, which may have
been reused by an unrelated process — then waits briefly for the serve to
shut down and release its registry entry. So that
tooling which did not start the serve can still open it, the 0600 registry
entry records the tokened URL; the same-user exposure is identical to the
agent token file, which already grants the underlying reads. A read-only
MCP `dashboard.status` discovery tool remains an option for shell-less
runtimes later; nothing ever grants MCP spawn authority.

### Explicitly not the deferred web dashboard

The operator/admin web dashboard remains deferred per the roadmap. A future
operator surface follows the control-plane fan-out topology; it will not
scrape local per-agent dashboards. The local registry only powers same-machine
discovery.

## Consequences

- Operators get a live window into transcripts, memories, facts, receive-only
  email metadata, and sealed secret metadata without any widened read
  capability or privileged path.
- Read-only means the dashboard writes no agent content. Its sole write is
  its own size-capped, strictly-validated, agent-scoped UI preferences row
  via a dedicated endpoint (`PUT /v1/self/dashboard-preferences`, proxied as
  the guarded `PUT /api/prefs`) — no content mutation, no ranking effects —
  and the preferences ride account export/import, so the theme choice
  follows the agent across machines and cells.
- Viewing the dashboard does not perturb the agent: observational and passive
  reads keep retrieval usage and read-state untouched. The one deliberate
  exception is the eye-icon reveal on a cell without observational fact
  reads, where the plain exact read records a single legitimate delivery
  usage for a lookup the user explicitly clicked for; broad fact reads never
  take that fallback — the memoized capability probe refuses them on cells
  that would silently ignore `observational=true`, and such cells also keep
  assertion-history values locked.
- The UI is themeable via embedded CSS-variable theme packs (default: dark
  console look); adding a theme is a drop-in CSS file, not a build change —
  the picker enumerates the embedded theme directory at runtime, so no JS or
  HTML edit is involved.
- A second serve for the same agent on the same machine is refused while the
  first is alive (registry PID liveness plus a dashboard-marker port probe),
  keeping the deterministic port meaningful. The registry claim happens under
  a file lock once the new listener is already answering, so two serves
  racing past the liveness check still resolve to exactly one registered
  survivor; shutdown only removes the registry entry it still owns, so a
  losing serve cannot delete the survivor's discovery record.
- The dashboard inherits CLI trust: anyone with the agent token file could
  already read this data via the CLI; the URL token only guards the local
  HTTP surface against other local users and DNS-rebinding browsers.

## Alternatives Considered

- Server-side dashboard routes or a hosted web app: rejected — creates the
  privileged web surface the roadmap forbids and couples cells to UI.
- Reusing `mcp serve` with an HTTP transport: rejected — MCP is a tool
  surface for model runtimes, not a browser UI, and mixing them muddies the
  read-only guarantee.
- A daemonized background dashboard: rejected — the repo deliberately retired
  daemon-style runners; foreground matches `mcp serve` and operator intuition.
- Ephemeral random ports only: rejected — bookmarks and multi-agent muscle
  memory want stable per-agent ports; determinism with fallback keeps both.

## Related

- [post-v0-roadmap.md](../post-v0-roadmap.md)
- [requirements.md](../requirements.md)
- [cli-command-surface.md](../cli-command-surface.md)
- [context-hydration.md](../context-hydration.md)
- [ADR 0001](0001-consolidate-witpass-into-witself.md)
- [ADR 0002](0002-client-side-narrative-memory.md)
