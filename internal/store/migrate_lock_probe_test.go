package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"strconv"
	"testing"
)

// Exercise the assertion itself with delayed server-side release. A healthy
// PostgreSQL run usually succeeds on its first probe and cannot catch a return
// to the old one-shot assertion.
func TestMigrationLockAvailabilityRetriesUntilReleased(t *testing.T) {
	for _, availableAfter := range []int{1, 3} {
		t.Run(strconv.Itoa(availableAfter)+" attempts", func(t *testing.T) {
			probe := &migrationLockProbeConn{availableAfter: availableAfter}
			db := sql.OpenDB(migrationLockProbeConnector{probe})
			t.Cleanup(func() { _ = db.Close() })
			conn, err := db.Conn(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = conn.Close() })
			assertMigrationLockAvailable(t, conn)
			if probe.attempts != availableAfter || probe.unlocks != 1 {
				t.Fatalf("lock probe attempts=%d unlocks=%d, want attempts=%d unlocks=1", probe.attempts, probe.unlocks, availableAfter)
			}
		})
	}
}

type migrationLockProbeConnector struct{ conn *migrationLockProbeConn }

func (c migrationLockProbeConnector) Connect(context.Context) (driver.Conn, error) {
	return c.conn, nil
}

func (c migrationLockProbeConnector) Driver() driver.Driver { return migrationLockProbeDriver{} }

type migrationLockProbeDriver struct{}

func (migrationLockProbeDriver) Open(string) (driver.Conn, error) {
	return nil, errors.New("lock probe requires its connector")
}

type migrationLockProbeConn struct {
	availableAfter int
	attempts       int
	unlocks        int
}

func (*migrationLockProbeConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("lock probe does not prepare statements")
}

func (*migrationLockProbeConn) Close() error { return nil }

func (*migrationLockProbeConn) Begin() (driver.Tx, error) {
	return nil, errors.New("lock probe does not begin transactions")
}

func (c *migrationLockProbeConn) QueryContext(ctx context.Context, query string, _ []driver.NamedValue) (driver.Rows, error) {
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if _, bounded := ctx.Deadline(); !bounded {
		return nil, errors.New("lock availability probe must have a bounded context")
	}
	switch query {
	case migrationAdvisoryTryLockSQL:
		c.attempts++
		return &migrationLockProbeRows{value: c.attempts >= c.availableAfter}, nil
	case migrationAdvisoryUnlockSQL:
		c.unlocks++
		return &migrationLockProbeRows{value: true}, nil
	default:
		return nil, errors.New("unexpected lock probe query")
	}
}

type migrationLockProbeRows struct {
	value bool
	done  bool
}

func (*migrationLockProbeRows) Columns() []string { return []string{"acquired"} }
func (*migrationLockProbeRows) Close() error      { return nil }
func (r *migrationLockProbeRows) Next(values []driver.Value) error {
	if r.done {
		return io.EOF
	}
	values[0] = r.value
	r.done = true
	return nil
}
