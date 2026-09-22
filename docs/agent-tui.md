# Agent terminal workspace

`witself tui --agent NAME` opens a native terminal workspace for one authenticated
agent. `witself tui --demo` explores the same interface using synthetic data,
without credentials, network access, a web server, or a browser.

The workspace uses the web Agent Console's observational projections for its
seven sections: Overview, Transcripts, Facts, Memories, Conversations, Email,
and Secrets. The terminal uses a colored monogram instead of rendering the
avatar image. Avatar lifecycle notices remain visible on Overview.

## Quick access

Select a fact or secret field and press `v` to reveal or hide it. Press `c` to
copy the exact value without first displaying it. Secret fields are decrypted
locally through the existing sealed-secret client. The local account, agent,
and vault key must match the authenticated identity; viewing cannot create,
register, replace, or enroll a key. A missing or mismatched key produces an
explicit unavailable state. TOTP enrollment seeds cannot be revealed; use
`witself totp code` for a current code. Binary fields use base64, like the CLI.

Lists and searches remain redacted. Revealed values are kept separately from
passive view data and cleared on hide, navigation, manual refresh, exit, or a 30-second reveal timeout.
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

## Observation and deliberate access

Passive panels share the existing Agent Console handler projections, invoked
in process. There is no local HTTP listener, browser session token, registry
entry, or background daemon. Broad fact reads retain the observational
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
