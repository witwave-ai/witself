package transcriptcapture

import "errors"

// HeldForBinding reports whether an event belongs to another installed identity
// of the requested runtime. It remains queued for that binding to return.
func HeldForBinding(runtime string, event Event, cfg Config) bool {
	runtime, err := NormalizeRuntime(runtime)
	return err == nil && event.Runtime == runtime &&
		errors.Is(EventBindingError(event, cfg), ErrIdentityMismatch)
}
