package store

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/stdlib"

	"github.com/witwave-ai/witself/internal/testenv"
)

const transcriptStorageLockHold = 1500 * time.Millisecond

type transcriptStorageLockFixture struct {
	st           *Store
	dsn          string
	principal    Principal
	conversation Transcript
	next         int64
}

func newTranscriptStorageLockFixture(t *testing.T) *transcriptStorageLockFixture {
	t.Helper()
	st, dsn := newMigrationTestStore(t, testenv.RequirePostgres(t))
	migrationTestUpTo(t, dsn, 100)
	p := newTranscriptStorageTestPrincipal(t, st, "lock-bounded")
	return &transcriptStorageLockFixture{st: st, dsn: dsn, principal: p,
		conversation: createTranscriptStorageTestConversation(t, st, p, "lock-bounded")}
}

func (f *transcriptStorageLockFixture) insert(ctx context.Context, q interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}, sequence int64) error {
	// A cold pgx cached statement first describes the INSERT in a separate
	// round trip. That can queue twice, once for describe and once for execute,
	// so use one execution when measuring a single queued statement's stall.
	_, err := q.Exec(ctx, `INSERT INTO transcript_entries
		(id, account_id, transcript_id, realm_id, recorded_by_agent_id, sequence, role, body)
		VALUES ($1,$2,$3,$4,$5,$6,'user','lock fixture')`,
		pgx.QueryExecModeExec, fmt.Sprintf("lock-entry-%d", sequence), f.principal.AccountID, f.conversation.ID,
		f.principal.RealmID, f.principal.ID, sequence)
	return err
}

func (f *transcriptStorageLockFixture) holder(t *testing.T) (pgx.Tx, time.Time) {
	t.Helper()
	tx, err := f.st.pool.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
	started := time.Now()
	f.next++
	if err := f.insert(context.Background(), tx, f.next); err != nil {
		t.Fatal(err)
	}
	return tx, started
}

func (f *transcriptStorageLockFixture) connection(t *testing.T, role string) *pgx.Conn {
	t.Helper()
	cfg, err := pgx.ParseConfig(f.dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.RuntimeParams["application_name"] = "migration-0101-" + role + "-" + strings.ReplaceAll(t.Name(), "/", "-")
	conn, err := pgx.ConnectConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	return conn
}

type transcriptStorageMigrationRun struct {
	pid     int32
	started time.Time
	result  <-chan error
	socket  net.Conn
}

type transcriptStorageLockTimeoutNotice struct {
	attempt int
	timeout time.Duration
	err     error
}

func (f *transcriptStorageLockFixture) migrateObservedLocks(t *testing.T, attempts int) (transcriptStorageMigrationRun, <-chan transcriptStorageLockTimeoutNotice) {
	t.Helper()
	settings, block := transcriptStorageMigrationLockStatements(t)
	const lock = "LOCK TABLE transcript_entries IN SHARE ROW EXCLUSIVE MODE;"
	if strings.Count(block, lock) != 1 {
		t.Fatal("migration lock statement missing or ambiguous")
	}
	// PostgreSQL cannot expose another backend's current_setting. Add only an
	// observation notice to the parsed production block, immediately before
	// each lock request, so the bound uses the actual session setting.
	block = strings.Replace(block, lock, `RAISE NOTICE 'witself-tr10-lock-timeout % %', attempt,
		(extract(epoch FROM current_setting('lock_timeout')::interval)*1000)::bigint;
		`+lock, 1)
	notices := make(chan transcriptStorageLockTimeoutNotice, attempts)
	cfg, err := pgx.ParseConfig(f.dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.RuntimeParams["application_name"] = "migration-0101-observed-" + strings.ReplaceAll(t.Name(), "/", "-")
	cfg.RuntimeParams["witself.migration_0101_lock_attempts"] = fmt.Sprint(attempts)
	cfg.RuntimeParams["witself.migration_0101_long_holder_ms"] = "60000"
	cfg.OnNotice = func(_ *pgconn.PgConn, notice *pgconn.Notice) {
		if !strings.HasPrefix(notice.Message, "witself-tr10-lock-timeout ") {
			return
		}
		var observed transcriptStorageLockTimeoutNotice
		var ms int64
		_, observed.err = fmt.Sscanf(notice.Message, "witself-tr10-lock-timeout %d %d", &observed.attempt, &ms)
		observed.timeout = time.Duration(ms) * time.Millisecond
		notices <- observed
	}
	conn, err := pgx.ConnectConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	pid := int32(conn.PgConn().PID())
	t.Cleanup(func() {
		var ignored bool
		_ = f.st.pool.QueryRow(context.Background(), `SELECT pg_terminate_backend($1)`, pid).Scan(&ignored)
		_ = conn.Close(context.Background())
	})
	if _, err := conn.Exec(context.Background(), `BEGIN`); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(context.Background(), settings); err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	started := time.Now()
	go func() {
		_, err := conn.Exec(context.Background(), block)
		_, _ = conn.Exec(context.Background(), `ROLLBACK`)
		result <- err
	}()
	return transcriptStorageMigrationRun{pid: pid, started: started, result: result}, notices
}

func (f *transcriptStorageLockFixture) migrate(t *testing.T, attempts int, longHolderMS int, captureSocket bool) transcriptStorageMigrationRun {
	t.Helper()
	cfg, err := pgx.ParseConfig(f.dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.RuntimeParams["application_name"] = "migration-0101-" + strings.ReplaceAll(t.Name(), "/", "-")
	cfg.RuntimeParams["witself.migration_0101_lock_attempts"] = fmt.Sprint(attempts)
	cfg.RuntimeParams["witself.migration_0101_long_holder_ms"] = fmt.Sprint(longHolderMS)
	var socket net.Conn
	if captureSocket {
		var dialed atomic.Bool
		cfg.DialFunc = func(ctx context.Context, network, address string) (net.Conn, error) {
			// pgx otherwise reacts to the closed socket by opening another
			// connection and sending CancelRequest. A killed client cannot do
			// that: this fixture must observe PostgreSQL detecting the FIN.
			if dialed.Swap(true) {
				return nil, errors.New("closed migration client cannot open another connection")
			}
			conn, err := (&net.Dialer{}).DialContext(ctx, network, address)
			if err == nil {
				socket = conn
			}
			return conn, err
		}
	}
	db := stdlib.OpenDB(*cfg)
	db.SetMaxOpenConns(1)
	var pid int32
	if err := db.QueryRow(`SELECT pg_backend_pid()`).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Bound even a deliberately broken mutation; identify only this backend.
		var ignored bool
		_ = f.st.pool.QueryRow(context.Background(), `SELECT pg_terminate_backend($1)`, pid).Scan(&ignored)
		_ = db.Close()
	})
	result := make(chan error, 1)
	started := time.Now()
	go func() { result <- migrationTestUpToDB(db, 101) }()
	return transcriptStorageMigrationRun{pid: pid, started: started, result: result, socket: socket}
}

func transcriptStorageLockPoll(t *testing.T, budget time.Duration, predicate func() (bool, error)) {
	t.Helper()
	timer := time.NewTimer(budget)
	defer timer.Stop()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		ok, err := predicate()
		if err != nil {
			t.Fatal(err)
		}
		if ok {
			return
		}
		select {
		case <-timer.C:
			t.Fatalf("condition did not complete within %s", budget)
		case <-ticker.C:
		}
	}
}

func (f *transcriptStorageLockFixture) waiting(pid int32) (string, error) {
	var started string
	err := f.st.pool.QueryRow(context.Background(), `SELECT COALESCE((SELECT waitstart::text
		FROM pg_locks WHERE pid=$1 AND relation='transcript_entries'::regclass
		AND mode='ShareRowExclusiveLock' AND NOT granted),'')`, pid).Scan(&started)
	return started, err
}

func (f *transcriptStorageLockFixture) waitAttempt(t *testing.T, pid int32, attempt int, budget time.Duration) {
	t.Helper()
	last := ""
	seen := 0
	transcriptStorageLockPoll(t, budget, func() (bool, error) {
		started, err := f.waiting(pid)
		if started != "" && started != last {
			seen++
			last = started
		}
		return seen >= attempt, err
	})
}

func transcriptStorageMigrationResult(t *testing.T, run transcriptStorageMigrationRun, budget time.Duration, attempts int) error {
	t.Helper()
	timer := time.NewTimer(budget)
	defer timer.Stop()
	select {
	case err := <-run.result:
		t.Logf("migration attempts=%d elapsed=%s", attempts, time.Since(run.started))
		return err
	case <-timer.C:
		t.Fatalf("migration exceeded %s test observation budget", budget)
		return nil
	}
}

func assertTranscriptStorageMigrationBusy(t *testing.T, err error, attempts int) {
	t.Helper()
	var pgErr *pgconn.PgError
	want := fmt.Sprintf("migration 0101: transcript_entries stayed busy for %d lock attempts", attempts)
	if !errors.As(err, &pgErr) || pgErr.Code != "55P03" || pgErr.Message != want {
		t.Fatalf("migration failure must be 55P03 with exact budget message; got %v", err)
	}
}

func assertTranscriptStorageMigrationAbsent(t *testing.T, f *transcriptStorageLockFixture) {
	t.Helper()
	assertMigrationTestVersion(t, f.dsn, 100)
	var absent bool
	err := f.st.pool.QueryRow(context.Background(), `SELECT
		to_regclass('transcript_storage_usage') IS NULL AND
		to_regclass('transcript_storage_usage_epoch_seq') IS NULL AND
		to_regprocedure('witself_transcript_entry_storage_bytes(text,text,text,jsonb,text,text,jsonb)') IS NULL AND
		to_regprocedure('witself_transcript_storage_shard(text)') IS NULL AND
		to_regprocedure('witself_transcript_storage_usage_insert()') IS NULL AND
		to_regprocedure('witself_transcript_storage_usage_delete()') IS NULL AND
		to_regprocedure('witself_transcript_storage_usage_update()') IS NULL AND
		NOT EXISTS (SELECT 1 FROM pg_trigger WHERE tgrelid='transcript_entries'::regclass
		AND tgname LIKE 'transcript_entries_storage_usage_%')`).Scan(&absent)
	if err != nil || !absent {
		t.Fatalf("rolled-back migration objects absent=%v: %v", absent, err)
	}
}

func transcriptStorageMigrationLockStatements(t *testing.T) (string, string) {
	t.Helper()
	raw, err := migrationsFS.ReadFile("migrations/0101_add_transcript_storage_usage.sql")
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	lower := strings.ToLower(text)
	settingsStart := strings.Index(lower, "select set_config(")
	lockStatement := strings.Index(lower, "lock table transcript_entries in share row exclusive mode;")
	if settingsStart < 0 || lockStatement < 0 {
		t.Fatal("migration settings or lock block missing")
	}
	// The capability attempt now has its own DO block before the lock block.
	// Keep both settings statements and identify the actual lock block by its lock.
	blockStart := strings.LastIndex(lower[:lockStatement], "do $$")
	if blockStart < settingsStart {
		t.Fatal("migration lock block missing or before settings")
	}
	blockEnd := strings.Index(lower[blockStart:], "-- +goose statementend")
	if blockEnd < 0 {
		t.Fatal("migration settings or lock block terminator missing")
	}
	return text[settingsStart:blockStart], text[blockStart : blockStart+blockEnd]
}

func (f *transcriptStorageLockFixture) lateTimeout(t *testing.T) time.Duration {
	t.Helper()
	var lateMS int
	if err := f.st.pool.QueryRow(context.Background(), `SELECT LEAST(GREATEST(
		(extract(epoch FROM current_setting('deadlock_timeout')::interval)*1000)::int+1000,2000),4000)`).Scan(&lateMS); err != nil {
		t.Fatal(err)
	}
	return time.Duration(lateMS) * time.Millisecond
}

func TestTranscriptStorageUsageSchema101LockBoundedPostgres(t *testing.T) {
	t.Run("a_release_holder", func(t *testing.T) {
		f := newTranscriptStorageLockFixture(t)
		holder, _ := f.holder(t)
		run := f.migrate(t, 20, 10000, false)
		f.waitAttempt(t, run.pid, 1, 3*transcriptStorageLockHold)
		timer := time.NewTimer(time.Until(run.started.Add(transcriptStorageLockHold)))
		<-timer.C
		if err := holder.Commit(context.Background()); err != nil {
			t.Fatal(err)
		}
		if err := transcriptStorageMigrationResult(t, run, 2*transcriptStorageLockHold, 20); err != nil {
			t.Fatal(err)
		}
		elapsed := time.Since(run.started)
		if elapsed < transcriptStorageLockHold || elapsed > 3*transcriptStorageLockHold {
			t.Fatalf("migration elapsed %s outside [%s,%s]", elapsed, transcriptStorageLockHold, 3*transcriptStorageLockHold)
		}
		assertMigrationTestVersion(t, f.dsn, 101)
	})
	t.Run("b_budget_exhausted", func(t *testing.T) {
		f := newTranscriptStorageLockFixture(t)
		_, _ = f.holder(t)
		run := f.migrate(t, 2, 10000, false)
		assertTranscriptStorageMigrationBusy(t, transcriptStorageMigrationResult(t, run, 6*time.Second, 2), 2)
		assertTranscriptStorageMigrationAbsent(t, f)
	})
	for _, attempt := range []int{1, 4} {
		t.Run(fmt.Sprintf("c_attempt_%d_writer_stall", attempt), func(t *testing.T) {
			f := newTranscriptStorageLockFixture(t)
			writer := f.connection(t, "writer")
			// Keep the writer's granted lock visible until the poller sees it.
			// This changes only its post-grant lifetime, not its lock request.
			writerTx, err := writer.Begin(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = writerTx.Rollback(context.Background()) })
			_, _ = f.holder(t)
			lateTimeout := f.lateTimeout(t)
			limit := time.Second
			if attempt > 3 {
				limit = lateTimeout
			}
			run, notices := f.migrateObservedLocks(t, attempt+1)
			f.waitAttempt(t, run.pid, attempt, 3*time.Duration(attempt)*limit)
			var migratorWait time.Time
			if err := f.st.pool.QueryRow(context.Background(), `SELECT waitstart FROM pg_locks
				WHERE pid=$1 AND relation='transcript_entries'::regclass
				AND mode='ShareRowExclusiveLock' AND NOT granted`, run.pid).Scan(&migratorWait); err != nil {
				t.Fatalf("attempt %d waiting request disappeared: %v", attempt, err)
			}
			var observed transcriptStorageLockTimeoutNotice
			for range attempt {
				select {
				case observed = <-notices:
					if observed.err != nil {
						t.Fatal(observed.err)
					}
				case <-time.After(limit):
					t.Fatal("queued attempt did not report its lock_timeout")
				}
			}
			// Keep this expectation independent of the reported setting: a
			// mutation must not enlarge its own accepted stall bound.
			if observed.attempt != attempt || observed.timeout != limit {
				t.Fatalf("attempt %d lock_timeout=%s, want %s (reported attempt %d)", attempt, observed.timeout, limit, observed.attempt)
			}
			writerDone := make(chan error, 1)
			go func() { writerDone <- f.insert(context.Background(), writerTx, 100) }()
			var firstWaiting, grantedAt time.Time
			transcriptStorageLockPoll(t, 3*limit, func() (bool, error) {
				var observedAt time.Time
				var present, granted bool
				err := f.st.pool.QueryRow(context.Background(), `SELECT clock_timestamp(),
					EXISTS(SELECT 1 FROM pg_locks WHERE pid=$1 AND relation='transcript_entries'::regclass
					AND mode='RowExclusiveLock'),
					EXISTS(SELECT 1 FROM pg_locks WHERE pid=$1 AND relation='transcript_entries'::regclass
					AND mode='RowExclusiveLock' AND granted)`, writer.PgConn().PID()).Scan(&observedAt, &present, &granted)
				if err != nil {
					return false, err
				}
				if present && !granted && firstWaiting.IsZero() {
					firstWaiting = observedAt
				}
				if granted {
					grantedAt = observedAt
				}
				return granted, nil
			})
			if firstWaiting.IsZero() || firstWaiting.Before(migratorWait) {
				t.Fatalf("writer's queued request was not observed after migrator waitstart: migrator=%s writer=%s", migratorWait, firstWaiting)
			}
			stall := grantedAt.Sub(firstWaiting)
			if stall > 3*observed.timeout/2 {
				t.Fatalf("writer lock stall=%s exceeds bound=%s", stall, 3*observed.timeout/2)
			}
			if err := <-writerDone; err != nil {
				t.Fatal(err)
			}
			if err := writerTx.Commit(context.Background()); err != nil {
				t.Fatal(err)
			}
			var completedAt time.Time
			if err := writer.QueryRow(context.Background(), `SELECT clock_timestamp()`).Scan(&completedAt); err != nil {
				t.Fatal(err)
			}
			t.Logf("attempt=%d lock-timeout=%s writer-lock-stall=%s bound=%s migrator-to-grant=%s post-grant-latency=%s (unbounded, includes observation and commit)",
				attempt, observed.timeout, stall, 3*observed.timeout/2, grantedAt.Sub(migratorWait), completedAt.Sub(grantedAt))
			assertTranscriptStorageMigrationBusy(t, transcriptStorageMigrationResult(t, run, 3*time.Duration(attempt+1)*limit, attempt+1), attempt+1)
			assertTranscriptStorageMigrationAbsent(t, f)
		})
	}
	t.Run("d_long_holder_never_queues", func(t *testing.T) {
		f := newTranscriptStorageLockFixture(t)
		writer := f.connection(t, "writer")
		holder, _ := f.holder(t)
		transcriptStorageLockPoll(t, 3*400*time.Millisecond, func() (bool, error) {
			var old bool
			err := f.st.pool.QueryRow(context.Background(), `SELECT EXISTS(SELECT 1 FROM pg_locks l
			JOIN pg_stat_activity a ON a.pid=l.pid WHERE l.relation='transcript_entries'::regclass
			AND a.pid=$1 AND l.mode='RowExclusiveLock' AND l.granted AND a.xact_start<clock_timestamp()-interval '400ms')`, holder.Conn().PgConn().PID()).Scan(&old)
			return old, err
		})
		run := f.migrate(t, 4, 200, false)
		f.assertNoQueuedMigration(t, run, writer, 4)
		assertTranscriptStorageMigrationAbsent(t, f)
	})
	t.Run("e_terminated_backend_rolls_back", func(t *testing.T) {
		f := newTranscriptStorageLockFixture(t)
		holder, _ := f.holder(t)
		run := f.migrate(t, 20, 10000, false)
		f.waitAttempt(t, run.pid, 1, 3*transcriptStorageLockHold)
		var terminated bool
		if err := f.st.pool.QueryRow(context.Background(), `SELECT pg_terminate_backend($1)`, run.pid).Scan(&terminated); err != nil || !terminated {
			t.Fatalf("terminate=%v: %v", terminated, err)
		}
		if err := transcriptStorageMigrationResult(t, run, 3*transcriptStorageLockHold, 20); err == nil {
			t.Fatal("terminated migration succeeded")
		}
		assertTranscriptStorageMigrationAbsent(t, f)
		if err := holder.Commit(context.Background()); err != nil {
			t.Fatal(err)
		}
		migrationTestUpTo(t, f.dsn, 101)
		assertMigrationTestVersion(t, f.dsn, 101)
	})
	t.Run("f_fin_while_queued", func(t *testing.T) {
		f := newTranscriptStorageLockFixture(t)
		holder, _ := f.holder(t)
		run := f.migrate(t, 20, 10000, true)
		f.waitAttempt(t, run.pid, 1, 3*time.Second)
		if err := run.socket.Close(); err != nil {
			t.Fatal(err)
		}
		started := time.Now()
		f.waitBackendGone(t, run.pid, 3*time.Second)
		t.Logf("closed-client backend disappeared after %s", time.Since(started))
		if err := transcriptStorageMigrationResult(t, run, 3*time.Second, 20); err == nil {
			t.Fatal("orphan migration succeeded")
		}
		assertTranscriptStorageMigrationAbsent(t, f)
		if err := holder.Commit(context.Background()); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := f.insert(ctx, f.st.pool, 100); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("f_silent_client_holding_lock", func(t *testing.T) {
		f := newTranscriptStorageLockFixture(t)
		conn, err := pgx.Connect(context.Background(), f.dsn)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = conn.Close(context.Background()) }()
		var pid int32
		if err := conn.QueryRow(context.Background(), `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
			t.Fatal(err)
		}
		if _, err := conn.Exec(context.Background(), `BEGIN`); err != nil {
			t.Fatal(err)
		}
		settings, block := transcriptStorageMigrationLockStatements(t)
		if _, err := conn.Exec(context.Background(), settings); err != nil {
			t.Fatal(err)
		}
		if _, err := conn.Exec(context.Background(), block); err != nil {
			t.Fatal(err)
		}
		started := time.Now()
		writerDone := make(chan error, 1)
		ctx, cancel := context.WithTimeout(context.Background(), 3*5*time.Second)
		defer cancel()
		go func() { writerDone <- f.insert(ctx, f.st.pool, 100) }()
		f.waitBackendGone(t, pid, 3*5*time.Second)
		if err := <-writerDone; err != nil {
			t.Fatal(err)
		}
		t.Logf("silent client and queued writer completed after %s", time.Since(started))
		assertTranscriptStorageMigrationAbsent(t, f)
	})
	t.Run("g_late_long_holder", func(t *testing.T) {
		f := newTranscriptStorageLockFixture(t)
		const attempts = 8
		const longHolder = 1500 * time.Millisecond
		const earlyTimeout = time.Second
		lateTimeout := f.lateTimeout(t)
		// Open M before either holder so connection startup cannot age H0 past
		// the probe threshold before its first lock request.
		m, err := pgx.Connect(context.Background(), f.dsn)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = m.Close(context.Background()) }()
		writerConn := f.connection(t, "writer")
		pid := int32(m.PgConn().PID())
		if _, err := m.Exec(context.Background(), `BEGIN`); err != nil {
			t.Fatal(err)
		}
		settings, block := transcriptStorageMigrationLockStatements(t)
		if _, err := m.Exec(context.Background(), settings); err != nil {
			t.Fatal(err)
		}
		if _, err := m.Exec(context.Background(), `SELECT
			set_config('witself.migration_0101_long_holder_ms', '1500', true),
			set_config('witself.migration_0101_lock_attempts', '8', true)`); err != nil {
			t.Fatal(err)
		}
		h0, _ := f.holder(t)
		budget := attempts * (lateTimeout + earlyTimeout + 300*time.Millisecond)
		ctx, cancel := context.WithTimeout(context.Background(), budget)
		defer cancel()
		result := make(chan error, 1)
		started := time.Now()
		go func() {
			_, err := m.Exec(ctx, block)
			result <- err
		}()
		f.waitAttempt(t, pid, 1, 3*earlyTimeout)
		firstWait, err := f.waiting(pid)
		if err != nil || firstWait == "" {
			t.Fatalf("first waiting request disappeared: %v", err)
		}
		t.Logf("late-holder trace elapsed=%s request=1 waitstart=%s", time.Since(started), firstWait)
		// H1 must begin after the first pg_stat_activity snapshot. Its INSERT
		// queues behind attempt one, then obtains its RowExclusiveLock.
		h1, err := pgx.Connect(context.Background(), f.dsn)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = h1.Close(context.Background()) }()
		if _, err := h1.Exec(context.Background(), `BEGIN`); err != nil {
			t.Fatal(err)
		}
		if err := f.insert(ctx, h1, 2); err != nil {
			t.Fatal(err)
		}
		if err := h0.Commit(context.Background()); err != nil {
			t.Fatal(err)
		}
		// Observe age on the server, not from a client timestamp. A request
		// already in flight is allowed; a changed waitstart denotes a new one.
		lastWait := firstWait
		requests := 1
		windowSeen := false
		writerCount := 0
		var writer <-chan error
		var writerStarted time.Time
		var maxWriterElapsed time.Duration
		ticker := time.NewTicker(20 * time.Millisecond)
		defer ticker.Stop()
		var migrationErr error
	observe:
		for {
			select {
			case migrationErr = <-result:
				break observe
			case <-ctx.Done():
				t.Fatalf("late-holder migration exceeded timing-derived budget %s: %v", budget, ctx.Err())
			case err := <-writer:
				elapsed := time.Since(writerStarted)
				maxWriterElapsed = max(maxWriterElapsed, elapsed)
				if err != nil || elapsed > 3*lateTimeout/2 {
					t.Fatalf("late-holder writer elapsed=%s bound=%s: %v", elapsed, 3*lateTimeout/2, err)
				}
				writerCount++
				writer = nil
			case <-ticker.C:
				var ageMS float64
				var waiting string
				err := f.st.pool.QueryRow(ctx, `SELECT
					extract(epoch FROM clock_timestamp()-a.xact_start)*1000,
					COALESCE((SELECT waitstart::text FROM pg_locks WHERE pid=$1
					AND relation='transcript_entries'::regclass
					AND mode='ShareRowExclusiveLock' AND NOT granted),'')
					FROM pg_stat_activity a WHERE a.pid=$2`, pid, h1.PgConn().PID()).Scan(&ageMS, &waiting)
				if err != nil {
					t.Fatal(err)
				}
				window := ageMS > float64((longHolder+lateTimeout)/time.Millisecond)
				if waiting != "" && waiting != lastWait {
					requests++
					lastWait = waiting
					t.Logf("late-holder trace elapsed=%s holder-age=%.1fms request=%d waitstart=%s protected-window=%v",
						time.Since(started), ageMS, requests, waiting, window)
					if window {
						t.Fatal("assertion 1: migration started a new request behind a late long holder")
					}
				}
				if window && !windowSeen {
					windowSeen = true
					t.Logf("late-holder protected window elapsed=%s holder-age=%.1fms threshold=%s late-timeout=%s",
						time.Since(started), ageMS, longHolder, lateTimeout)
				}
				if window && writer == nil {
					writerResult := make(chan error, 1)
					writer = writerResult
					writerStarted = time.Now()
					sequence := int64(100 + writerCount)
					go func() {
						writerCtx, writerCancel := context.WithTimeout(ctx, 3*lateTimeout/2)
						defer writerCancel()
						writerResult <- f.insert(writerCtx, writerConn, sequence)
					}()
				}
			}
		}
		if writer != nil {
			err := <-writer
			writerElapsed := time.Since(writerStarted)
			maxWriterElapsed = max(maxWriterElapsed, writerElapsed)
			if err != nil || writerElapsed > 3*lateTimeout/2 {
				t.Fatalf("late-holder final writer elapsed=%s bound=%s: %v", writerElapsed, 3*lateTimeout/2, err)
			}
			writerCount++
		}
		elapsed := time.Since(started)
		t.Logf("late-holder total=%s requests=%d limit=%d window-writers=%d max-writer=%s writer-bound=%s",
			elapsed, requests, attempts, writerCount, maxWriterElapsed, 3*lateTimeout/2)
		assertTranscriptStorageMigrationBusy(t, migrationErr, attempts)
		if !windowSeen || writerCount == 0 || requests > attempts || elapsed >= 90*time.Second {
			t.Fatalf("late-holder evidence incomplete: window=%v writers=%d requests=%d elapsed=%s", windowSeen, writerCount, requests, elapsed)
		}
		if _, err := m.Exec(context.Background(), `ROLLBACK`); err != nil {
			t.Fatal(err)
		}
		assertTranscriptStorageMigrationAbsent(t, f)
		// H1 remains open until the DO block has returned and M has rolled back.
		if _, err := h1.Exec(context.Background(), `COMMIT`); err != nil {
			t.Fatal(err)
		}
	})
}

func (f *transcriptStorageLockFixture) waitBackendGone(t *testing.T, pid int32, budget time.Duration) {
	t.Helper()
	transcriptStorageLockPoll(t, budget, func() (bool, error) {
		var absent bool
		err := f.st.pool.QueryRow(context.Background(), `SELECT NOT EXISTS(SELECT 1 FROM pg_stat_activity WHERE pid=$1)`, pid).Scan(&absent)
		return absent, err
	})
}

func (f *transcriptStorageLockFixture) assertNoQueuedMigration(t *testing.T, run transcriptStorageMigrationRun, writerConn *pgx.Conn, attempts int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	writer := make(chan error, 1)
	go func() { writer <- f.insert(ctx, writerConn, 100) }()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	timer := time.NewTimer(3 * time.Duration(attempts) * time.Second)
	defer timer.Stop()
	for {
		select {
		case err := <-writer:
			if err != nil {
				t.Fatalf("independent writer blocked: %v", err)
			}
			writer = nil
		case err := <-run.result:
			assertTranscriptStorageMigrationBusy(t, err, attempts)
			if writer != nil {
				if err := <-writer; err != nil {
					t.Fatalf("independent writer did not complete while holder remained open: %v", err)
				}
			}
			t.Logf("non-queuing migration attempts=%d elapsed=%s", attempts, time.Since(run.started))
			return
		case <-ticker.C:
			waiting, err := f.waiting(run.pid)
			if err != nil {
				t.Fatal(err)
			}
			if waiting != "" {
				t.Fatal("migration queued behind a long holder")
			}
		case <-timer.C:
			t.Fatal("long-holder migration exceeded its observation budget")
		}
	}
}
