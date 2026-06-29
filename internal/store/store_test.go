package store

import (
	"context"
	"testing"
)

// Open should reject a malformed DSN without needing a live database (the bad
// DSN fails at parse time). Live Ping behavior is exercised via the server's
// readiness handler, not here, so the unit tests stay DB-free.
func TestOpenRejectsBadDSN(t *testing.T) {
	if _, err := Open(context.Background(), "this is not a dsn"); err == nil {
		t.Error("Open with malformed DSN = nil error, want error")
	}
}
