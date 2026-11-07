// Package listenerdiagnostic emits bounded diagnostics for failed child binds.
package listenerdiagnostic

import (
	"errors"
	"fmt"
	"io"
)

// IsAddressInUse recognizes the native socket error through wrapped errors.
func IsAddressInUse(err error) bool { return errors.Is(err, ErrAddressInUse) }

// Diagnostic returns only the fixed component and failure category. The listen
// error's text can contain addresses or other private values and is never used.
func Diagnostic(component string, err error) string {
	if err == nil {
		return ""
	}
	var prefix string
	switch component {
	case "server":
		prefix = "witself-server: listener bind failed: "
	case "worker":
		prefix = "witself-worker: listener bind failed: "
	default:
		return ""
	}
	if IsAddressInUse(err) {
		return prefix + "address in use"
	}
	return prefix + "other"
}

// Write emits exactly one diagnostic line for a supported component's error.
func Write(w io.Writer, component string, err error) {
	if diagnostic := Diagnostic(component, err); diagnostic != "" {
		_, _ = fmt.Fprintln(w, diagnostic)
	}
}
