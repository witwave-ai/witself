package testenv

import (
	"context"
	"crypto/rand"
	"errors"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

var postgresTransactionGuardsEnabled atomic.Bool

var postgresObserverConnect = pgx.Connect
var postgresObserverSleep = time.Sleep

var postgresTransactionProcessTag = sync.OnceValue(func() string {
	return "witself-test-" + strconv.Itoa(os.Getpid()) + "-" + rand.Text()[:12]
})

var postgresTransactionGuards = struct {
	sync.Mutex
	byTest  map[testing.TB]*postgresTransactionGuard
	managed map[postgresBackendIdentity]*PostgresTransactionConnection
}{
	byTest:  make(map[testing.TB]*postgresTransactionGuard),
	managed: make(map[postgresBackendIdentity]*PostgresTransactionConnection),
}

// PostgresTransactionConnection tracks top-level transaction generations for a
// fixture-owned connection. Obtain it before Begin with TrackPostgresConnection.
// Use its Begin/BeginTx methods and the returned transaction's Commit/Rollback for
// every top-level boundary. Savepoints created by Tx.Begin keep the same generation.
// Raw SQL transaction control or boundaries through the original connection (or
// Tx.Conn) bypass tracking and are unsupported on a managed connection.
type PostgresTransactionConnection struct {
	connection *pgx.Conn
	generation uint64 // protected by postgresTransactionGuards
	open       bool
}

// TrackPostgresConnection registers an idle connection for the lifetime of its
// owning fixture. It neither closes the connection nor rolls back transactions;
// those remain the fixture's responsibility. Register with the parent test when
// children borrow its transaction. The connection must use a DSN returned by
// RequirePostgres or GuardPostgresTransactions to participate in observation.
func TrackPostgresConnection(t testing.TB, connection *pgx.Conn) *PostgresTransactionConnection {
	t.Helper()
	if connection.PgConn().TxStatus() != 'I' {
		t.Fatal("postgres transaction tracking requires an idle connection")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	backend := postgresBackendIdentity{pid: int32(connection.PgConn().PID())}
	if connection.QueryRow(ctx, `SELECT backend_start FROM pg_stat_activity WHERE pid=pg_backend_pid()`).Scan(&backend.backendStarted) != nil {
		t.Fatal("postgres transaction tracking backend observation failed")
	}
	postgresTransactionGuards.Lock()
	if _, exists := postgresTransactionGuards.managed[backend]; exists {
		postgresTransactionGuards.Unlock()
		t.Fatal("postgres transaction connection already tracked")
	}
	managed := &PostgresTransactionConnection{connection: connection}
	postgresTransactionGuards.managed[backend] = managed
	postgresTransactionGuards.Unlock()
	t.Cleanup(func() {
		postgresTransactionGuards.Lock()
		defer postgresTransactionGuards.Unlock()
		if postgresTransactionGuards.managed[backend] == managed {
			delete(postgresTransactionGuards.managed, backend)
		}
	})
	return managed
}

// Begin begins a tracked top-level transaction with default options.
func (c *PostgresTransactionConnection) Begin(ctx context.Context) (pgx.Tx, error) {
	return c.BeginTx(ctx, pgx.TxOptions{})
}

// BeginTx begins a new generation only after PostgreSQL accepts Begin. Like
// pgx.Conn, this connection must not be used concurrently with its operations or
// guard observations. Use Tx.Begin for savepoints within an existing transaction.
func (c *PostgresTransactionConnection) BeginTx(ctx context.Context, options pgx.TxOptions) (pgx.Tx, error) {
	if c.connection.PgConn().TxStatus() != 'I' {
		return nil, errors.New("postgres tracked begin requires an idle connection")
	}
	tx, err := c.connection.BeginTx(ctx, options)
	if err != nil {
		return nil, err
	}
	postgresTransactionGuards.Lock()
	c.generation++
	c.open = true
	generation := c.generation
	postgresTransactionGuards.Unlock()
	return &postgresTrackedTransaction{Tx: tx, owner: c, generation: generation}, nil
}

type postgresTrackedTransaction struct {
	pgx.Tx
	owner      *PostgresTransactionConnection
	generation uint64
}

func (tx *postgresTrackedTransaction) ended() {
	postgresTransactionGuards.Lock()
	defer postgresTransactionGuards.Unlock()
	// A deferred rollback on an old, closed handle cannot retire a replacement.
	if tx.owner.generation == tx.generation {
		tx.owner.open = false
	}
}

func (tx *postgresTrackedTransaction) Commit(ctx context.Context) error {
	err := tx.Tx.Commit(ctx)
	// pgx closes the top-level transaction even on commit/rollback failure.
	tx.ended()
	return err
}

func (tx *postgresTrackedTransaction) Rollback(ctx context.Context) error {
	err := tx.Tx.Rollback(ctx)
	tx.ended()
	return err
}

// EnablePostgresTransactionGuards makes RequirePostgres register one teardown
// assertion per test. Call it from a database test package's initialization.
// Prerequisite-only users, including tests using synthetic DSNs, remain inert.
//
// Attribution uses this test process's application_name tag. Child processes
// have their own tags; connections using DSNs that did not pass through
// RequirePostgres or GuardPostgresTransactions are not observed.
// A test that replaces application_name on its own connections through a config
// copy, URL query, or SQL SET opts those connections out of observation (GUARD-2).
// Existing transactions are exempt only while their exact transaction identity
// remains unchanged; this permits a child fixture beneath a live parent fixture.
// Managed connections use explicit generations. Unmanaged connections retain a
// timestamp heuristic: after abort clears xact_start, a rejected query can change
// query_start without replacing the transaction and cause false attribution.
// Fixtures lending aborted transactions to children must use TrackPostgresConnection.
func EnablePostgresTransactionGuards() {
	postgresTransactionGuardsEnabled.Store(true)
}

// GuardPostgresTransactions also covers fixtures receiving an explicit DSN
// instead of obtaining one through RequirePostgres. Registration is idempotent
// for a test. It returns the process-tagged DSN when its package enabled guards,
// and the byte-identical input otherwise. Use its return value for connections.
func GuardPostgresTransactions(t testing.TB, dsn string) string {
	t.Helper()
	if !postgresTransactionGuardsEnabled.Load() {
		return dsn
	}
	if _, err := pgx.ParseConfig(dsn); err != nil {
		t.Fatal("postgres transaction guard DSN parsing failed")
		return ""
	}
	tag := postgresTransactionProcessTag()
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		dsn = postgresURLDSNWithApplicationName(dsn, tag)
	} else {
		dsn = postgresKeywordDSNWithApplicationName(dsn, tag)
	}
	registerPostgresTransactionGuard(t, dsn)
	return dsn
}

// postgresURLDSNWithApplicationName preserves pgx's libpq URI semantics rather
// than parsing the URI with net/url: multi-host authorities are supported, '+'
// is literal, and '#' does not introduce a fragment. pgx already validated it.
func postgresURLDSNWithApplicationName(dsn, tag string) string {
	remaining := dsn[strings.Index(dsn, "://")+3:]
	if index := strings.IndexAny(remaining, "@/"); index >= 0 && remaining[index] == '@' {
		remaining = remaining[index+1:]
	}
	// Locate the query after userinfo, the host list, and any database path.
	// Question marks inside a bracketed host or userinfo are not delimiters.
	for remaining != "" {
		if remaining[0] == '[' {
			remaining = remaining[strings.IndexByte(remaining, ']')+1:]
		} else {
			remaining = postgresDSNTailAtAny(remaining, ":/?,")
		}
		if strings.HasPrefix(remaining, ":") {
			remaining = postgresDSNTailAtAny(remaining[1:], "/?,")
		}
		if !strings.HasPrefix(remaining, ",") {
			break
		}
		remaining = remaining[1:]
	}
	if strings.HasPrefix(remaining, "/") {
		remaining = postgresDSNTailAtAny(remaining, "?")
	}
	if remaining == "" {
		return dsn + "?application_name=" + tag
	}
	query := make([]string, 0)
	for pair := range strings.SplitSeq(remaining[1:], "&") {
		if pair == "" {
			continue
		}
		key, _, _ := strings.Cut(pair, "=")
		decoded, _ := url.PathUnescape(strings.Trim(key, " "))
		if decoded != "application_name" {
			query = append(query, pair)
		}
	}
	query = append(query, "application_name="+tag)
	return dsn[:len(dsn)-len(remaining)] + "?" + strings.Join(query, "&")
}

func postgresDSNTailAtAny(value, delimiters string) string {
	if index := strings.IndexAny(value, delimiters); index >= 0 {
		return value[index:]
	}
	return ""
}

// postgresKeywordDSNWithApplicationName appends the last-wins runtime parameter
// without changing earlier keyword values. pgx has already validated the DSN.
// A final empty value needs quotes before the separator, and a final unquoted
// escape is discarded by pgx rather than allowed to escape the new separator.
func postgresKeywordDSNWithApplicationName(dsn, tag string) string {
	const whitespace = " \t\n\r\v\f"
	remaining := strings.TrimLeft(dsn, whitespace)
	for remaining != "" {
		remaining = strings.TrimLeft(remaining[strings.IndexByte(remaining, '=')+1:], whitespace)
		if remaining == "" {
			return dsn + "'' application_name=" + tag
		}
		quoted := remaining[0] == '\''
		index := 0
		if quoted {
			index++
		}
		for index < len(remaining) {
			if remaining[index] == '\\' {
				index++
				if index == len(remaining) {
					return dsn[:len(dsn)-1] + " application_name=" + tag
				}
			} else if quoted && remaining[index] == '\'' {
				index++
				break
			} else if !quoted && strings.ContainsRune(whitespace, rune(remaining[index])) {
				break
			}
			index++
		}
		remaining = strings.TrimLeft(remaining[index:], whitespace)
	}
	return dsn + " application_name=" + tag
}

type postgresBackendIdentity struct {
	pid            int32
	backendStarted time.Time
}

type postgresTransactionIdentity struct {
	postgresBackendIdentity
	transactionStart time.Time
	managed          *PostgresTransactionConnection
	generation       uint64
}

type postgresTransactionActivity struct {
	identity postgresTransactionIdentity
	state    string
	age      string
}

type postgresTransactionGuard struct {
	observer *pgx.Conn
	baseline map[postgresTransactionIdentity]struct{}
}

func (guard *postgresTransactionGuard) connectObserver(t testing.TB, dsn string) (context.Context, context.CancelFunc, bool) {
	t.Helper()
	for attempt := 1; attempt <= 3; attempt++ {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		started := time.Now()
		observer, err := postgresObserverConnect(ctx, dsn)
		elapsed := time.Since(started)
		if err == nil {
			guard.observer = observer
			return ctx, cancel, true
		}
		cancel()
		kind := postgresObserverConnectKind(err)
		if kind == "auth" || attempt == 3 {
			t.Fatalf("postgres transaction guard observer connection failed kind=%s attempts=%d", kind, attempt)
			return nil, nil, false
		}
		postgresObserverSleep(min(max(2*elapsed, 200*time.Millisecond), 2*time.Second))
	}
	return nil, nil, false
}

func postgresObserverConnectKind(err error) string {
	var timeout net.Error
	if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &timeout) && timeout.Timeout()) {
		return "timeout"
	}
	var postgres *pgconn.PgError
	if errors.As(err, &postgres) {
		if postgres.Code == "53300" || postgres.Code == "53400" {
			return "limit"
		}
		if strings.HasPrefix(postgres.Code, "28") {
			return "auth"
		}
	}
	if errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.EPIPE) {
		return "refused"
	}
	return "other"
}

func registerPostgresTransactionGuard(t testing.TB, dsn string) {
	t.Helper()
	postgresTransactionGuards.Lock()
	if _, exists := postgresTransactionGuards.byTest[t]; exists {
		postgresTransactionGuards.Unlock()
		return
	}
	// Reserve the test before connecting, so repeated prerequisite calls cannot
	// create several observers or register several copies of the same assertion.
	guard := &postgresTransactionGuard{}
	postgresTransactionGuards.byTest[t] = guard
	postgresTransactionGuards.Unlock()
	t.Cleanup(func() {
		defer func() {
			postgresTransactionGuards.Lock()
			delete(postgresTransactionGuards.byTest, t)
			postgresTransactionGuards.Unlock()
		}()
		if guard.observer == nil {
			return
		}
		defer func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if guard.observer.Close(ctx) != nil {
				t.Errorf("postgres transaction guard test=%s observer close failed", t.Name())
			}
		}()
		if guard.baseline == nil {
			return // Setup already failed; no baseline exists for attribution.
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		activities, err := postgresOpenTransactions(ctx, guard.observer)
		if err != nil {
			t.Errorf("postgres transaction guard test=%s observation failed", t.Name())
			return
		}
		for _, activity := range activities {
			if _, inherited := guard.baseline[activity.identity]; inherited {
				continue
			}
			// No PID, transaction ID, application name, SQL, account value, DSN,
			// or underlying driver error is admitted to the teardown diagnostic.
			t.Errorf("postgres transaction guard test=%s left open transaction state=%s age=%s", t.Name(), activity.state, activity.age)
		}
	})
	ctx, cancel, ok := guard.connectObserver(t, dsn)
	if !ok {
		return
	}
	defer cancel()
	observer := guard.observer
	activities, err := postgresOpenTransactions(ctx, observer)
	if err != nil {
		t.Fatal("postgres transaction guard baseline observation failed")
		return
	}
	guard.baseline = make(map[postgresTransactionIdentity]struct{}, len(activities))
	for _, activity := range activities {
		guard.baseline[activity.identity] = struct{}{}
	}
}

func postgresOpenTransactions(ctx context.Context, observer *pgx.Conn) ([]postgresTransactionActivity, error) {
	// PostgreSQL may clear xact_start after abort. Retain query_start as the
	// fallback for unmanaged connections, but prefer explicit generations below:
	// a rejected query can move query_start without a new transaction.
	rows, err := observer.Query(ctx, `SELECT pid,backend_start,coalesce(xact_start,query_start,backend_start),
		CASE state WHEN 'active' THEN 'active'
		WHEN 'idle in transaction' THEN 'idle in transaction'
		WHEN 'idle in transaction (aborted)' THEN 'idle in transaction (aborted)'
		ELSE 'other' END,
		CASE WHEN xact_start IS NULL THEN 'none'
		WHEN clock_timestamp()-xact_start < interval '1 second' THEN 'under-1s'
		WHEN clock_timestamp()-xact_start < interval '1 minute' THEN '1s-to-1m'
		ELSE 'at-least-1m' END
		FROM pg_stat_activity WHERE datname=current_database()
		AND backend_type='client backend' AND pid<>pg_backend_pid()
		AND application_name=$1
		AND (xact_start IS NOT NULL OR state IN ('idle in transaction','idle in transaction (aborted)')) ORDER BY pid`, postgresTransactionProcessTag())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var activities []postgresTransactionActivity
	for rows.Next() {
		var activity postgresTransactionActivity
		if err := rows.Scan(&activity.identity.pid, &activity.identity.backendStarted,
			&activity.identity.transactionStart, &activity.state, &activity.age); err != nil {
			return nil, err
		}
		postgresTransactionGuards.Lock()
		if managed := postgresTransactionGuards.managed[activity.identity.postgresBackendIdentity]; managed != nil && managed.open {
			activity.identity.transactionStart = time.Time{}
			activity.identity.managed = managed
			activity.identity.generation = managed.generation
		}
		postgresTransactionGuards.Unlock()
		activities = append(activities, activity)
	}
	return activities, rows.Err()
}
