# Native agent workspace

`New(ctx, source, Options)` returns a Bubble Tea model for seven panels. The caller
owns `tea.NewProgram`, alternate-screen setup, and a deferred `Model.Close()`.
`NewDemoSource()` supplies Atlas/studio fixtures entirely in memory.
`Model.Snapshot(width, height)` accepts only that concrete demo implementation;
it never writes files or runs a live source.

The required `Source` API matches the supplied `dashboard.Reader` methods.
`Options.Copy` is an optional, cancellation-aware clipboard callback. No callback
means no private read on copy and an explicit unavailable notice. There is no
OSC52 or stdout fallback. Clipboard lifecycle and terminal restoration belong
to the integrating CLI.

## Deliberate field access

Terminal integrations may implement the optional API:

```go
type SecretSource interface {
    RevealSecret(context.Context, string, string) ([]byte, error)
}
```

`[` and `]` select one field. `v` reveals or hides the selected non-TOTP field;
`c` copies its exact original value without displaying it. These are deliberate
reads only. Passive inventories retain metadata only, including for public
fields. The live adapter owns existing-key custody and refuses TOTP seeds;
this package never initializes or enrolls a key. The browser console contract
is separate and remains metadata-only.

Returned plaintext buffers transfer to the UI and are cleared on every path,
including errors and canceled/stale results. Only sanitized, bounded ephemeral
display text is retained separately; no private value travels in a Bubble Tea
message. Display values clear on hide, field/row/panel navigation, manual
refresh, quit, and a generation-fenced 30-second timer. Copy uses the exact
original bytes converted to the callback's string, never the sanitized display.
Go strings and terminal scrollback cannot be guaranteed physically erased.

The optional interface is implemented by the deterministic demo source and the
CLI's local-vault adapter; it is not part of `dashboard.Reader`.

## Account scope

Account is a separate mode/state, available with `8` only after the closed
`ResourceAccountContext` read verifies the exact schema, manager identity, role
and ordered section allowlist. The seven agent states and `1`–`7` keys remain
unchanged. Account reads use current manager authority for the same account;
the selected agent receives no additional permissions.

`[` / `]` (or left/right) switch permitted subsections. Up/down scroll, and in
Support's selection focus they choose a ticket. Tab switches selection and
scroll focus. Enter explicitly reads one selected support thread; Esc, leaving,
refresh, authority changes and close discard the body and rendered viewport.
Bodies never enter passive data or Bubble Tea messages and are never polled.
Clearing a thread (including opening an overlay) requires a fresh Enter selection;
unchanged authority checks cannot restore it. An expired current request settles
as unavailable, while obsolete requests cannot change a newer selection.
Authority has its own timer, command, cancellation and generation lifecycle,
independent of paused agent display and blocked section/ticket reads.
Fresh Access responses reconcile the same verified account with that lifecycle;
unknown authority removes Account, and older responses cannot restore it.

`x` in Clients is the explicit **Check this device** action. Ordinary refresh
and polling read cached metadata only. The demo's scan returns authored fixtures
without reading disk. Recorded versions, static configuration, check time and
Not run verification are not online or remote-installation claims.
Repeated `x` is ignored while the Clients request is busy.

Plan limits retain their actual units and default/effective values. Billing
keeps exact integer cents and currency, distinguishes unknown from zero, and
reports summary/invoice/payment failures independently. No account write,
invoice-link copy, browser action, or clipboard access is implemented by Account
resources. Existing `b`/`w` web-console controls and shared-theme preferences
remain available. Oversized support threads retain account access and direct
the reader to `witself account support show --ticket TKT_ID`; there is no
automatic retry, fallback or invented pagination.

Every supported plan dimension remains visible. An omitted dimension in a valid
present map means **No plan cap** (platform ceilings still apply); missing/null
maps are unknown, and zero is a real cap. Recognized invalid caps reject the
projection using `plans.ValidateLimits`. Retention uses **Indefinite** only for
explicit null days, preserves positive days and override distinctions, and
reports missing or invalid values as unknown/unavailable.

Focused tests run entirely with fake sources/synthetic homes. Set
`WITSELF_TUI_ACCOUNT_SNAPSHOT_DIR` to an existing external directory when running
`TestAccountLayoutsAndSyntheticSnapshots` to export all six sections at 80×24
and 120×40 in each of the five themes as ANSI and plain-text snapshots.
