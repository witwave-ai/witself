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
