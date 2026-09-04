// Package cliout provides the small, shared output primitives used by Witself
// command entry points.
package cliout

import (
	"fmt"
	"io"
)

// Line writes one formatted line and intentionally ignores writer failures.
// Usage rendering is best-effort because command exit status is determined by
// the command that requested the usage text, not by the destination writer.
func Line(w io.Writer, args ...any) {
	_, _ = fmt.Fprintln(w, args...)
}
