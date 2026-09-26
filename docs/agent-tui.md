# Agent terminal workspace

`witself tui --agent NAME` opens a native terminal workspace for one authenticated
agent. `witself tui --demo` explores the same interface using synthetic data,
without credentials, network access, a web server, or a browser.

The workspace uses the web Agent Console's observational projections for its
seven sections: Overview, Transcripts, Facts, Memories, Conversations, Email,
and Secrets. The terminal uses a colored monogram instead of rendering the
avatar image. Avatar lifecycle notices remain visible on Overview.

## Visual summary

Overview shares the web console's passive summary: one colored, labeled row
for Operations, Transcripts, Facts, Memories, Secrets, Email, and Messages.
Press `o` for Overview, `l` for Timeline, `e` for Recent updates, or `d` for
workspace details, capacity, plan, retention, and salient memories. Use arrow
keys to select a category and inspect its breakdown below the rows; Enter opens
a record category. Operations has no inventory. Tab switches to scrolling;
`a` switches graphs to ASCII characters.

Inventory and recorded activity are separate. Facts and active-memory counts
are exact when available; other counts describe a bounded page of recent
records. Sparklines scale independently per row. Timeline uses fixed count
ranges across 24 UTC hour buckets, including the partial current hour.
The graph labels identify entries recorded, fact deliveries, secret accesses,
accepted email sends, and messages sent. These quantities have different units
and are never combined into a total. Metering may omit unrecorded activity.
Operations (OPS, compatibility key `transactions`) counts recorded successful
bounded reads plus writes. Its breakdown keeps reads, writes, records read,
and records written separate; a page is one read and a batch is one write.
Memories retains exact active inventory and graphs created, revised, archived,
restored, and deleted changes, with each quantity in the selected-row detail.
These cooperative, nonbilling metrics cover the core-record catalog, not every
network request or administrative action.

The two new graphs show `?` before the durable tracking marker. Zero means a
valid report with no recorded activity after coverage began. The first tracked
hour and current hour are partial; totals describe only the recorded portion
of this window, and older clients may omit activity. No history is backfilled.
**Not tracked yet** means no marker; **Server update needed** means the activity
endpoint returned 404; **Unavailable** means an error or invalid report;
**Disabled** requires an explicit feature refusal. The five legacy activity
rows remain independent when the new endpoint is absent.
See [agent activity metrics](agent-activity.md) for the covered operations and
retry, batch, and record-count rules.

Updates show only category, timestamp, and a fixed description from the bounded
loaded records; they are not a complete audit log. Pending-work notices do not
imply unread-message counts. The summary does not show private record content
or fetch revealed values. It refreshes through a shared passive projection
cached for at most 30 seconds; a failed refresh labels retained data as stale.

## Navigation

On every list panel (Transcripts, Facts, Memories, Conversations, Email, and
Secrets), Tab or Enter from the inventory collapses the list to its heading and
selected row, giving the detail reader the full content width. Esc or Shift+Tab
from detail scrolling restores the inventory; from detail actions, Shift+Tab
first returns to scrolling. The reader position survives focus changes and
refreshes for the same selected record. Very small terminals prioritize the
reader. Overview and Account mode retain their existing layouts and navigation.

## Quick access

Select a fact or secret field and press `v` to reveal or hide it. Press `c` to
copy the exact value without first displaying it. Secret fields are decrypted
locally through the existing sealed-secret client. The local account, agent,
and vault key must match the authenticated identity; viewing cannot create,
register, replace, or enroll a key. A missing or mismatched key produces an
explicit unavailable state. TOTP enrollment seeds cannot be revealed; use
`witself totp code` for a current code. Binary fields use base64, like the CLI.

Lists and searches remain redacted. Revealed values are kept separately from
passive view data and cleared on hide, navigation, manual refresh, web-console
controls, exit, or a 30-second reveal timeout.
They are not written to saved preferences or logs. Terminal output strips
untrusted control sequences; copying preserves the exact value. The UI's
in-memory clearing is not a guarantee of forensic erasure from process memory,
terminal recording, or screenshots.

Clipboard writes use the installed operating-system utility, never terminal
OSC52 escape sequences. macOS uses `pbcopy`; Linux uses `wl-copy` in Wayland or
`xclip`/`xsel` in X11; Windows uses `clip.exe`. If no supported clipboard is
available, the workspace reports that instead of printing the value as a
fallback. Values travel over process input, not command arguments. A best-effort
clear runs after 45 seconds or when the workspace exits. This can overwrite a
later clipboard value copied in another app; clipboard history and synced
devices can retain copies.

## Web console, one key away

Press `b` to open the selected agent's web console in your default browser.
The workspace starts a local console when needed and reuses an existing one
only after verifying its canonical account, realm, and agent identity, plus the
same manager binding and current manager authorization when Account is enabled.
Agent-only sessions cannot reuse manager-enabled sessions; one manager cannot
reuse another's. A revoked manager session cannot become agent-only for reuse.
Legacy consoles qualify only as agent-only.
Press `w` for its status and controls:

| Key | Action |
| --- | --- |
| `s` | Start without opening the browser |
| `b` / Enter | Start or reuse, then open the browser |
| `x` | Stop the selected agent's verified console |
| `r` | Check its current status |
| Esc | Close the controls |

The header shows whether the console is on or off. The controls distinguish
**started here** from an **existing session**. Quitting the TUI stops a console
it started; a reused console stays running unless you explicitly stop it.
If the browser cannot open, the console stays available and `b` retries.
Unverifiable registry records are left untouched and shown as an identity
conflict. Demo mode never starts a server or opens a browser.

The managed console runs in a separate child process, so stopping it does not
quit the TUI. Its explicit connection credentials pass through a bounded private
stdin pipe; the child reauthenticates both contexts without loading ambient
manager credentials. Access URLs never appear in the TUI, its status messages,
or ordinary errors. Browser
opening uses the operating system's native opener on macOS, Linux, and Windows;
it occurs on the computer running the TUI.

For an already running session, `witself dashboard open --agent NAME` performs
independent current-agent/current-manager verification and an exact live-session
check before opening it. Use the same account and realm selectors. If manual
browser opening is needed, add `--print-url`. Its output is a **private local
session capability**, including Account access when enabled; do not copy it into
logs or reports. Routine status, stop results, and banners hide manager URLs.
See [local opening and reuse](agent-console.md#opening-and-reusing-a-local-console).

## Account navigation

A separate **8 Account** entry appears only when the original current CLI
operator is verified for the selected agent's canonical account. The seven
agent sections retain keys `1`–`7` and their existing agent authority. Account
uses the current manager's authority and grants the agent no new permissions.
Without a verified manager there is no Account entry or Account help/options,
including at compact terminal sizes. Explicit `--endpoint` or `--token-file`
connections remain agent-only.

The Account header identifies the account, current manager/role, selected agent,
and read-only scope. Its subsections are **Overview**, **Clients on this device**,
**Plan & limits**, **Billing**, **Support**, and **Access**, in that order.
Plan & limits and Billing appear only for `account_owner`, `account_admin`, or
`account_billing`; `account_operator` sees the other four. Access describes the
manager's permitted reads, not the agent's privileges.

| Key | Action in Account |
| --- | --- |
| `8` | Open Account when authorized |
| `1`–`7` | Return to the corresponding agent section |
| `[` / `]` or left / right | Move between permitted Account subsections |
| Tab / Shift+Tab | Switch between selection and scrolling |
| Up / down or `k` / `j` | Select a support ticket in selection mode, otherwise scroll |
| Enter | Deliberately open the selected support thread |
| Esc | Clear the selected thread and return to selection |
| `x` | Check this device, only in Clients on this device |
| `r` | Refresh the section and authority; Clients reads the cached report |
| `p` | Pause/resume display refresh; independent authority checks continue |
| `b` / `w` / `t` | Keep browser opening, web-console controls, and theme selection |

Current authorization is checked independently of slow section reads and paused
display refresh. An authority error or revocation clears Account data and removes
navigation; changed identity, role, or permitted sections clears old projections.
Late results cannot restore them. Account has no clipboard, support mutation,
payment, or other domain mutation controls. The existing theme exception remains
agent-scoped.

Clients scans run only on explicit `x` (**Check this device**). The report covers
recorded installs for this account on this device, distinguishing recorded
version, executable presence, static MCP registration, effective verification
**Not run**, and check time. It never runs providers, reads credentials, or
performs network checks. All eight runtime record types are inventoried; only
the four supported default-root MCP registrations are checked. Custom provider
roots and unsupported platforms/topologies remain unsupported or unavailable,
not healthy. This is not fleet inventory or online status.

Support bodies are separate ephemeral selected reads. Esc, refresh, leaving or
hiding the view (including help/theme/web controls), selection changes, or
context changes clear them; reopening requires fresh selection. There is no
background body refresh. Whole-thread responses over 4 MiB produce the fixed
`response_too_large` resource error without removing Account authority. Use
`witself account support show --account NAME --ticket TKT_ID` deliberately to
inspect such a thread. Admitted responses show only the newest 100 messages,
with 16 KiB body caps and truncation markers; no console pagination is offered.

For the shared plan/retention interpretation, exact cents and unknown-versus-zero
billing behavior, partial billing errors, and detailed local check limits, see
[Account in the console guide](agent-console.md#account-current-manager-selected-agent).
Hosted administration and fleet/admin privileges remain outside this workspace.

## Observation and deliberate access

Passive agent panels share the existing Agent Console handler projections, invoked
in process. Ordinary terminal browsing does not start a local HTTP listener,
create a browser session token, or register a console. Those are created only
when you explicitly start or open the web console. Broad fact reads retain the observational
capability check, secret inventories exclude all field values, and email shows
only its bounded metadata projection.

Two deliberate private actions are separate from refresh: exact fact reveal
and recipient-only message body preview. Sensitive memory content also requires
`v` and is kept out of passive caches. The terminal adds exact secret-field
access as a third deliberate action. A secret access records the existing
value-free access receipt and usage event; encrypted material is decrypted only
on the client. Received message previews do not mark a message read or
acknowledged. No action sends or processes messages or email, edits facts or
memories, manages secret lifecycle, or performs curation.

Theme selection is shared with the browser console through the existing
size-capped preference row. Five palettes are available: Console, Paper,
Midnight, Amber, and High contrast. Auto uses the Console dark palette.
An unsuccessful preference save leaves the selected theme active locally and
reports the save failure.

Live refresh fetches the current section and the agent overview with bounded
requests. Filters, selection, and reading position are preserved. Loading,
empty, disabled, unsupported, not-enrolled, and temporarily unavailable states
are distinct. Email Received and Sent remain independent: disabling receipt
does not hide delivery history.

## Validation boundary

The deterministic demo and recording fake-cell tests exercise presentation,
navigation, private-access lifetimes, observational reads, and terminal text
safety without using a real account. Such checks are not evidence of a live
vault enrollment or production cell acceptance. The browser's existing
[presentation and privacy contract](agent-console.md) still applies to its own
surface; secret-field reveal is confined to the terminal workspace. Account
checks use synthetic identities and local fixtures; they do not establish live
customer-account acceptance.

The TUI removes an exact, valid console registry record only when its process is known dead, allowing `b` or `w` to start a replacement; live or unverifiable records remain untouched.
Account authority is checked on each Account tick, every 30 seconds elsewhere, and with exponential retry backoff capped at 60 seconds after failure; a failed check immediately removes Account access, and console discovery reuses the reader's verified principal for at most 30 seconds.
Secret reveal failures show only fixed reasons for an unenrolled key, a key mismatch, unavailable access, or cancellation, without displaying error text or paths.
On Unix, browser launch observes early failure for up to one second, then reports an unknown opening outcome without killing a launched browser.
Managed console children inherit HTTP/HTTPS proxy, proxy exclusion, and custom CA settings; local discovery probes bypass proxies, while cell identity verification uses the standard transport with a ten-second budget.
