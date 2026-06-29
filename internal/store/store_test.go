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

// The migrations must be embedded in the binary so the server can run them on
// startup with no external files. (Applying them needs a live DB, exercised
// in local/integration runs, not here.)
func TestMigrationsEmbedded(t *testing.T) {
	entries, err := migrationsFS.ReadDir("migrations")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) == 0 {
		t.Fatal("no migrations embedded")
	}
}
