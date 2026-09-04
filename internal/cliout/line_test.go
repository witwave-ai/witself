package cliout

import (
	"errors"
	"strings"
	"testing"
)

func TestLinePreservesUsageFormatting(t *testing.T) {
	var output strings.Builder
	Line(&output, "Usage:", "witself")
	Line(&output)

	if got, want := output.String(), "Usage: witself\n\n"; got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
}

func TestLineIgnoresWriterFailure(_ *testing.T) {
	Line(failingWriter{}, "usage")
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) {
	return 0, errors.New("write failed")
}
