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
for Transactions, Transcripts, Facts, Memories, Secrets, Email, and Messages.
Press `o` for Overview, `l` for Timeline, `e` for Recent updates, or `d` for
workspace details, capacity, plan, retention, and salient memories. Use arrow
keys to select a category and Enter to open it. Tab switches to scrolling;
`a` switches graphs to ASCII characters.

Inventory and recorded activity are separate. Facts and active-memory counts
are exact when available; other counts describe a bounded page of recent
records. Sparklines scale independently per row. Timeline uses fixed count
ranges across 24 UTC hour buckets, including the partial current hour.
The graph labels identify entries recorded, fact deliveries, secret accesses,
accepted email sends, and messages sent. These quantities have different units
and are never combined into a total. Metering may omit unrecorded activity.
Transactions have no defined metric yet, and memory activity history is
unavailable. Missing or disabled data never becomes a zero graph.

Updates show only category, timestamp, and a fixed description from the bounded
loaded records; they are not a complete audit log. Pending-work notices do not
imply unread-message counts. The summary does not show private record content
or fetch revealed values. It refreshes through a shared passive projection
cached for at most 30 seconds; a failed refresh labels retained data as stale.

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
only after verifying its canonical account, realm, and agent identity.
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
quit the TUI. Its connection credentials pass through a private pipe. Access
URLs never appear in the TUI, its status messages, or ordinary errors. Browser
opening uses the operating system's native opener on macOS, Linux, and Windows;
it occurs on the computer running the TUI.

## Observation and deliberate access

Passive panels share the existing Agent Console handler projections, invoked
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
surface; the new secret-field reveal is confined to the terminal workspace.
