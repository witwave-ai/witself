package localcell

import (
	"testing"
	"time"
)

// NewPostgresRecoveryCellForTest keeps the synthetic recovery root private on
// every platform without requiring a fixture postmaster or native PID lookup.
func NewPostgresRecoveryCellForTest(t *testing.T) (*Cell, string) {
	t.Helper()
	c, _ := unitCell(t)
	if err := testMkdir(c.path("pg"), 0o700); err != nil {
		t.Fatal("cannot create recovery identity fixture directory")
	}
	return c, c.cfg.Root
}

// SetPostgresRecoveryObserversForTest changes only the recovery-record check.
func SetPostgresRecoveryObserversForTest(c *Cell, identity func(int) (string, bool, error), startedAt func(int) (time.Time, error)) {
	c.recoveryProcessIdentity = identity
	c.recoveryProcessStartedAt = startedAt
}

// RecoverPostgresRecordForTest exercises recovery without starting or signaling
// the synthetic PID supplied by the test's identity observer.
func RecoverPostgresRecordForTest(c *Cell) error { return c.recoverPostgresRecord() }
