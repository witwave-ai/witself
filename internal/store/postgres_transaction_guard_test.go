package store

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/witwave-ai/witself/internal/testenv"
)

func init() { testenv.EnablePostgresTransactionGuards() }

// Exercise the real shared prerequisite and testing cleanup stack in a child:
// a missing guard makes the leaking child pass, which fails this parent oracle.
// The child retains independent recovery after the guard, so faults cannot
// contaminate later package tests or leave pool.Close waiting indefinitely.
func TestStorePostgresTransactionGuard(t *testing.T) {
	const childKey = "WITSELF_STORE_TRANSACTION_GUARD_CHILD"
	if mode := os.Getenv(childKey); mode != "" {
		t.Run(mode, func(t *testing.T) {
			if mode == "foreign_holder" {
				testStorePostgresForeignHolder(t)
				return
			}
			if strings.HasPrefix(mode, "invalid_") {
				dsn := "host='private-invalid-dsn-sentinel"
				if mode == "invalid_url_dsn" {
					dsn = "postgres://private-invalid-dsn-sentinel/%zz"
				}
				_ = testenv.GuardPostgresTransactions(t, dsn)
				t.Fatal("transaction guard invalid DSN unexpectedly accepted")
				return
			}
			if strings.HasPrefix(mode, "tagged_") {
				testStorePostgresTaggedDSN(t, mode)
				return
			}
			if mode == "harness_validator_tagged_dsn" {
				dsn := testenv.GuardPostgresTransactions(t, os.Getenv("WITSELF_TEST_DATABASE_URL"))
				parsed, err := pgx.ParseConfig(dsn)
				if err != nil || !strings.HasPrefix(parsed.RuntimeParams["application_name"], "witself-test-") {
					t.Fatal("harness validator control did not receive a process tag")
				}
				if testenv.ValidateHarnessPostgresDSN(dsn) != nil {
					t.Fatal("harness validator refused the guard returned DSN")
				}
				return
			}
			if mode == "active_transaction" {
				testStorePostgresActiveTransactionGuard(t)
				return
			}
			ctx := context.Background()
			var connection *pgx.Conn
			t.Cleanup(func() {
				if connection != nil {
					closeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
					defer cancel()
					if connection.Close(closeCtx) != nil {
						t.Error("transaction guard independent recovery failed")
					}
				}
			})
			var dsn string
			if mode == "direct_guard_leaking_transaction" {
				dsn = testenv.GuardPostgresTransactions(t, os.Getenv("WITSELF_TEST_DATABASE_URL"))
			} else {
				dsn = testenv.RequirePostgres(t)
			}
			if strings.HasPrefix(mode, "foreign_process_") {
				if _, err := fmt.Fprintln(os.Stdout, "transaction guard child baseline ready"); err != nil {
					t.Fatal("transaction guard child readiness failed")
				}
				scanner := bufio.NewScanner(os.Stdin)
				if !scanner.Scan() || scanner.Text() != "continue" {
					t.Fatal("transaction guard child continuation failed")
				}
			}
			var err error
			connection, err = pgx.Connect(ctx, dsn)
			if err != nil {
				t.Fatal("transaction guard control connection failed")
			}
			begin := connection.Begin
			if strings.HasPrefix(mode, "managed_") || strings.HasPrefix(mode, "unchanged_aborted_parent") {
				managed := testenv.TrackPostgresConnection(t, connection)
				begin = managed.Begin
			}
			tx, err := begin(ctx)
			if err != nil {
				t.Fatal("transaction guard control begin failed")
			}
			switch mode {
			case "leaking_transaction", "direct_guard_leaking_transaction", "foreign_process_with_own_leak":
				// Deliberately retained until the independent recovery cleanup.
			case "unchanged_aborted_parent_transaction", "unchanged_aborted_parent_after_commit":
				var old pgx.Tx
				if mode == "unchanged_aborted_parent_after_commit" {
					old = tx
					if err := tx.Commit(ctx); err != nil {
						t.Fatal("transaction guard managed commit failed")
					}
					tx, err = begin(ctx)
					if err != nil {
						t.Fatal("transaction guard managed second begin failed")
					}
				}
				if _, err := tx.Exec(ctx, `SELECT 1/0`); err == nil {
					t.Fatal("transaction guard unchanged parent abort did not fail")
				}
				t.Run("child", func(t *testing.T) {
					_ = testenv.RequirePostgres(t)
					if old != nil && !errors.Is(old.Rollback(ctx), pgx.ErrTxClosed) {
						t.Fatal("transaction guard closed handle rollback changed")
					}
					// No rollback/new Begin occurs on this transaction between
					// the child baseline and teardown. Only query_start changes.
					if _, err := tx.Exec(ctx, `SELECT 1`); err == nil {
						t.Fatal("transaction guard aborted parent accepted query")
					}
				})
				if err := tx.Rollback(ctx); err != nil {
					t.Fatal("transaction guard unchanged parent rollback failed")
				}
			case "aborted_transaction", "replaced_aborted_parent_transaction", "managed_replaced_aborted_parent_transaction", "managed_replaced_aborted_parent_commit":
				if _, err := tx.Exec(ctx, `SELECT 1/0`); err == nil {
					t.Fatal("transaction guard abort control did not fail")
				}
				if mode != "aborted_transaction" {
					t.Run("child", func(t *testing.T) {
						_ = testenv.RequirePostgres(t)
						if mode == "managed_replaced_aborted_parent_commit" {
							if !errors.Is(tx.Commit(ctx), pgx.ErrTxCommitRollback) {
								t.Fatal("transaction guard aborted parent commit did not roll back")
							}
						} else if err := tx.Rollback(ctx); err != nil {
							t.Fatal("transaction guard aborted parent rollback failed")
						}
						// Reuse the exact physical connection: PID and backend
						// start remain unchanged, while the transaction is new.
						var err error
						tx, err = begin(ctx)
						if err != nil {
							t.Fatal("transaction guard replacement begin failed")
						}
						if _, err := tx.Exec(ctx, `SELECT 1/0`); err == nil {
							t.Fatal("transaction guard replacement abort did not fail")
						}
						// Leave this new transaction for the nested child's
						// guard; the parent recovers after that cleanup runs.
					})
					if err := tx.Rollback(ctx); err != nil {
						t.Fatal("transaction guard replacement rollback failed")
					}
				}
			case "closed_transaction", "foreign_process_transaction":
				if err := tx.Rollback(ctx); err != nil {
					t.Fatal("transaction guard positive rollback failed")
				}
			case "live_parent_transaction":
				t.Run("child", func(t *testing.T) { _ = testenv.RequirePostgres(t) })
				if err := tx.Rollback(ctx); err != nil {
					t.Fatal("transaction guard parent rollback failed")
				}
			default:
				t.Fatal("transaction guard child mode invalid")
			}
		})
		return
	}
	_ = testenv.RequirePostgres(t)
	for _, mode := range []string{
		"leaking_transaction", "direct_guard_leaking_transaction", "aborted_transaction", "replaced_aborted_parent_transaction",
		"managed_replaced_aborted_parent_transaction", "managed_replaced_aborted_parent_commit",
		"active_transaction", "closed_transaction", "live_parent_transaction",
		"unchanged_aborted_parent_transaction", "unchanged_aborted_parent_after_commit",
		"tagged_postgres_url_dsn", "tagged_postgresql_url_dsn", "tagged_url_runtime_bytes_dsn", "tagged_keyword_dsn",
		"tagged_keyword_empty_dsn", "tagged_keyword_escape_dsn", "invalid_keyword_dsn", "invalid_url_dsn",
		"foreign_process_transaction", "foreign_process_with_own_leak",
		"harness_validator_tagged_dsn",
	} {
		t.Run(mode, func(t *testing.T) {
			if strings.HasPrefix(mode, "foreign_process_") {
				testStorePostgresForeignProcessGuard(t, mode)
				return
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestStorePostgresTransactionGuard$", "-test.v")
			command.Env = append(os.Environ(), childKey+"="+mode)
			output, err := command.CombinedOutput()
			if mode == "active_transaction" && strings.Contains(string(output), "transaction guard active synchronization missing") {
				t.Fatal("transaction guard active control missing synchronization")
			}
			replacedParent := strings.Contains(mode, "replaced_aborted_parent")
			wantFailure := mode == "leaking_transaction" || mode == "direct_guard_leaking_transaction" || strings.HasPrefix(mode, "invalid_") || mode == "aborted_transaction" || replacedParent || mode == "active_transaction"
			var exitError *exec.ExitError
			if ctx.Err() != nil || (wantFailure && (!errors.As(err, &exitError) || exitError.ExitCode() != 1)) || (!wantFailure && err != nil) {
				t.Fatal("transaction guard child exit did not match deliberate fault")
			}
			if strings.HasPrefix(mode, "invalid_") {
				if !strings.Contains(string(output), "postgres transaction guard DSN parsing failed") || strings.Contains(string(output), "private-invalid-dsn-sentinel") {
					t.Fatal("transaction guard invalid DSN did not fail value-free")
				}
				t.Log("transaction guard invalid DSN rejected without its value")
				return
			}
			leavingTest := "TestStorePostgresTransactionGuard/" + mode
			if replacedParent {
				leavingTest += "/child"
			}
			marker := "postgres transaction guard test=" + leavingTest + " left open transaction state=idle in transaction"
			if mode == "aborted_transaction" || replacedParent {
				marker += " (aborted)"
			} else if mode == "active_transaction" {
				marker = "postgres transaction guard test=" + leavingTest + " left open transaction state=active"
			}
			if strings.Contains(string(output), marker) != wantFailure {
				t.Fatal("transaction guard did not identify the leaving test")
			}
			if !wantFailure && strings.Contains(string(output), "postgres transaction guard test=") {
				t.Fatal("transaction guard healthy child emitted a diagnostic")
			}
			if strings.Contains(string(output), "independent recovery failed") {
				t.Fatal("transaction guard fault control recovery failed")
			}
			t.Logf("transaction guard control=%s expected_failure=%t leaving_test_identified=%t", mode, wantFailure, wantFailure)
		})
	}
}

func testStorePostgresActiveTransactionGuard(t *testing.T) {
	t.Helper()
	var connection, controller *pgx.Conn
	var tx pgx.Tx
	var cancelQuery context.CancelFunc
	var queryDone chan struct{}
	var lockHeld bool
	var lockKey int32
	var observationComplete bool
	// Register every recovery operation before the guard. The guard must see
	// the blocked transaction before cancellation and lock release settle it.
	t.Cleanup(func() {
		if cancelQuery != nil {
			cancelQuery()
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if lockHeld {
			var released bool
			if controller.QueryRow(ctx, `SELECT pg_advisory_unlock(84971,$1::int)`, lockKey).Scan(&released) != nil || !released {
				t.Error("transaction guard independent recovery failed: lock release")
			}
		}
		joined := true
		if queryDone != nil {
			select {
			case <-queryDone:
			case <-ctx.Done():
				joined = false
				t.Error("transaction guard independent recovery failed: query join")
			}
		}
		// Cancellation may already have closed the physical connection. Never
		// reuse a transaction or connection until its query goroutine has ended.
		if tx != nil && joined {
			if err := tx.Rollback(ctx); err != nil && !errors.Is(err, pgx.ErrTxClosed) && !connection.IsClosed() {
				t.Error("transaction guard independent recovery failed: rollback")
			}
		}
		if connection != nil && joined && connection.Close(ctx) != nil {
			t.Error("transaction guard independent recovery failed: query connection close")
		}
		if controller != nil && controller.Close(ctx) != nil {
			t.Error("transaction guard independent recovery failed: controller close")
		}
	})
	// This independent oracle runs after the guard, but before recovery. The
	// expected guard failure cannot hide deletion of the observation loop.
	t.Cleanup(func() {
		if !observationComplete {
			t.Error("transaction guard active synchronization missing")
		}
	})
	dsn := testenv.RequirePostgres(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var err error
	controller, err = pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal("transaction guard lock controller connection failed")
	}
	connection, err = pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal("transaction guard active connection failed")
	}
	lockKey = int32(connection.PgConn().PID())
	if _, err := controller.Exec(ctx, `SELECT pg_advisory_lock(84971,$1::int)`, lockKey); err != nil {
		t.Fatal("transaction guard controller lock failed")
	}
	lockHeld = true
	tx, err = connection.Begin(ctx)
	if err != nil {
		t.Fatal("transaction guard active begin failed")
	}
	queryCtx, queryCancel := context.WithTimeout(context.Background(), 20*time.Second)
	cancelQuery = queryCancel
	queryDone = make(chan struct{})
	go func() {
		defer close(queryDone)
		_, _ = tx.Exec(queryCtx, `SELECT pg_advisory_xact_lock(84971,$1::int)`, lockKey)
	}()
	for {
		var blocked bool
		if controller.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_stat_activity
			WHERE datname=current_database() AND pid=$1 AND state='active'
			AND xact_start IS NOT NULL AND wait_event_type='Lock' AND wait_event='advisory')`, lockKey).Scan(&blocked) != nil {
			t.Fatal("transaction guard active observation failed")
		}
		if blocked {
			observationComplete = true
			return // Keep the query blocked until after the guard's assertion.
		}
	}
}

// The holder deliberately avoids RequirePostgres and GuardPostgresTransactions:
// neither an untagged client nor another application's tag belongs to the child.
func testStorePostgresForeignHolder(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var connections [2]*pgx.Conn
	var transactions [2]pgx.Tx
	t.Cleanup(func() {
		closeCtx, closeCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer closeCancel()
		for i, connection := range connections {
			if transactions[i] != nil && transactions[i].Rollback(closeCtx) != nil {
				t.Error("transaction guard independent recovery failed: foreign rollback")
			}
			if connection != nil && connection.Close(closeCtx) != nil {
				t.Error("transaction guard independent recovery failed: foreign close")
			}
		}
	})
	dsn := os.Getenv("WITSELF_TEST_DATABASE_URL")
	foreignConfig, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatal("transaction guard foreign configuration failed")
	}
	foreignConfig.RuntimeParams["application_name"] = "witself-test-foreign-holder"
	connections[0], err = pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal("transaction guard untagged foreign connection failed")
	}
	connections[1], err = pgx.ConnectConfig(ctx, foreignConfig)
	if err != nil {
		t.Fatal("transaction guard tagged foreign connection failed")
	}
	begin := func() {
		for i, connection := range connections {
			transactions[i], err = connection.Begin(ctx)
			if err != nil {
				t.Fatal("transaction guard foreign begin failed")
			}
		}
	}
	begin()
	if _, err := fmt.Fprintf(os.Stdout, "transaction guard holder ready %d %d\n", connections[0].PgConn().PID(), connections[1].PgConn().PID()); err != nil {
		t.Fatal("transaction guard foreign readiness failed")
	}
	scanner := bufio.NewScanner(os.Stdin)
	if !scanner.Scan() {
		return // Parent cleanup closes stdin even after an early assertion failure.
	}
	if scanner.Text() != "refresh" {
		t.Fatal("transaction guard foreign refresh command invalid")
	}
	for i, tx := range transactions {
		if tx.Rollback(ctx) != nil {
			t.Fatal("transaction guard foreign refresh rollback failed")
		}
		transactions[i] = nil
	}
	begin()
	if _, err := fmt.Fprintln(os.Stdout, "transaction guard holder refreshed"); err != nil {
		t.Fatal("transaction guard foreign refreshed readiness failed")
	}
	if scanner.Scan() || scanner.Err() != nil {
		t.Fatal("transaction guard foreign shutdown command invalid")
	}
}

type postgresGuardProcess struct {
	command *exec.Cmd
	input   io.WriteCloser
	scanner *bufio.Scanner
	output  strings.Builder
	stderr  bytes.Buffer
	waited  bool
}

func startPostgresGuardProcess(ctx context.Context, t *testing.T, mode string) *postgresGuardProcess {
	t.Helper()
	process := &postgresGuardProcess{command: exec.CommandContext(ctx, os.Args[0], "-test.run=^TestStorePostgresTransactionGuard$", "-test.v")}
	process.command.Env = append(os.Environ(), "WITSELF_STORE_TRANSACTION_GUARD_CHILD="+mode)
	process.command.Stderr = &process.stderr
	// Register termination before any pipe setup or process-start assertion.
	t.Cleanup(func() {
		if process.input != nil {
			_ = process.input.Close()
		}
		if process.command.Process != nil && !process.waited {
			_ = process.command.Wait()
			process.waited = true
		}
	})
	var err error
	process.input, err = process.command.StdinPipe()
	if err != nil {
		t.Fatal("transaction guard process input setup failed")
	}
	output, err := process.command.StdoutPipe()
	if err != nil {
		t.Fatal("transaction guard process output setup failed")
	}
	process.scanner = bufio.NewScanner(output)
	if process.command.Start() != nil {
		t.Fatal("transaction guard process start failed")
	}
	return process
}

func (process *postgresGuardProcess) readiness(t *testing.T, prefix string) string {
	t.Helper()
	for process.scanner.Scan() {
		line := process.scanner.Text()
		process.output.WriteString(line + "\n")
		if strings.HasPrefix(line, prefix) {
			return line
		}
	}
	t.Fatal("transaction guard process readiness missing")
	return ""
}

func (process *postgresGuardProcess) finish(t *testing.T) (string, error) {
	t.Helper()
	for process.scanner.Scan() {
		process.output.WriteString(process.scanner.Text() + "\n")
	}
	if process.scanner.Err() != nil {
		t.Error("transaction guard process output read failed")
	}
	err := process.command.Wait()
	process.waited = true
	return process.output.String() + process.stderr.String(), err
}

func testStorePostgresForeignProcessGuard(t *testing.T, mode string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	holder := startPostgresGuardProcess(ctx, t, "foreign_holder")
	line := holder.readiness(t, "transaction guard holder ready ")
	var pids [2]int32
	if count, err := fmt.Sscanf(line, "transaction guard holder ready %d %d", &pids[0], &pids[1]); err != nil || count != 2 || pids[0] == pids[1] {
		t.Fatal("transaction guard holder readiness malformed")
	}
	var observer *pgx.Conn
	t.Cleanup(func() {
		if observer != nil {
			closeCtx, closeCancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer closeCancel()
			if observer.Close(closeCtx) != nil {
				t.Error("transaction guard independent recovery failed: foreign observer close")
			}
		}
	})
	var err error
	observer, err = pgx.Connect(ctx, testenv.RequirePostgres(t))
	if err != nil {
		t.Fatal("transaction guard foreign observer connection failed")
	}
	previous := observePostgresGuardForeignTransactions(ctx, t, observer, pids, nil)
	child := startPostgresGuardProcess(ctx, t, mode)
	child.readiness(t, "transaction guard child baseline ready")
	// Starting the holder first exercises the actual cross-process shape, but
	// unchanged baseline transactions are correctly exempt under fix 4. Refresh
	// both generations after the child's baseline, so dropping the application
	// filter must detect them and cannot accidentally pass this oracle.
	if _, err := io.WriteString(holder.input, "refresh\n"); err != nil {
		t.Fatal("transaction guard foreign refresh request failed")
	}
	holder.readiness(t, "transaction guard holder refreshed")
	observePostgresGuardForeignTransactions(ctx, t, observer, pids, previous)
	if _, err := io.WriteString(child.input, "continue\n"); err != nil {
		t.Fatal("transaction guard child continuation request failed")
	}
	output, childErr := child.finish(t)
	wantFailure := mode == "foreign_process_with_own_leak"
	var exitError *exec.ExitError
	if ctx.Err() != nil || (wantFailure && (!errors.As(childErr, &exitError) || exitError.ExitCode() != 1)) || (!wantFailure && childErr != nil) {
		t.Error("transaction guard foreign control child exit did not match deliberate fault")
	}
	wantDiagnostics := 0
	if wantFailure {
		wantDiagnostics = 1
		marker := "postgres transaction guard test=TestStorePostgresTransactionGuard/" + mode + " left open transaction state=idle in transaction"
		if strings.Count(output, marker) != 1 {
			t.Error("transaction guard foreign control did not identify only the child's leak")
		}
	}
	if got := strings.Count(output, "postgres transaction guard test="); got != wantDiagnostics {
		t.Errorf("transaction guard foreign control diagnostic count=%d want=%d", got, wantDiagnostics)
	}
	if strings.Contains(output, "independent recovery failed") {
		t.Error("transaction guard foreign control child recovery failed")
	}
	if holder.input.Close() != nil {
		t.Error("transaction guard foreign holder shutdown failed")
	}
	holderOutput, holderErr := holder.finish(t)
	if ctx.Err() != nil || holderErr != nil || strings.Contains(holderOutput, "postgres transaction guard test=") || strings.Contains(holderOutput, "independent recovery failed") {
		t.Error("transaction guard foreign holder recovery failed")
	}
	t.Logf("transaction guard control=%s expected_failure=%t foreign_transactions=2 expected_diagnostics=%d", mode, wantFailure, wantDiagnostics)
}

func observePostgresGuardForeignTransactions(ctx context.Context, t *testing.T, observer *pgx.Conn, pids [2]int32, previous map[int32]time.Time) map[int32]time.Time {
	t.Helper()
	for {
		rows, err := observer.Query(ctx, `SELECT pid,xact_start FROM pg_stat_activity
			WHERE datname=current_database() AND backend_type='client backend'
			AND pid=ANY($1::int[]) AND state='idle in transaction' AND xact_start IS NOT NULL`, pids[:])
		if err != nil {
			t.Fatal("transaction guard foreign transaction observation failed")
		}
		observed := make(map[int32]time.Time)
		for rows.Next() {
			var pid int32
			var started time.Time
			if rows.Scan(&pid, &started) != nil {
				rows.Close()
				t.Fatal("transaction guard foreign transaction observation scan failed")
			}
			if previous == nil || !started.Equal(previous[pid]) {
				observed[pid] = started
			}
		}
		rows.Close()
		if rows.Err() != nil {
			t.Fatal("transaction guard foreign transaction observation rows failed")
		}
		if len(observed) == 2 {
			return observed
		}
	}
}

func testStorePostgresTaggedDSN(t *testing.T, mode string) {
	t.Helper()
	dsn := os.Getenv("WITSELF_TEST_DATABASE_URL")
	config, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatal("transaction guard DSN control configuration failed")
	}
	if mode == "tagged_url_runtime_bytes_dsn" {
		separator := "?"
		if strings.Contains(dsn, "?") {
			separator = "&"
		}
		// pgx follows libpq URI semantics: plus, semicolon, and hash are
		// literal query-value bytes, and percent-encoded keys are decoded.
		// A custom GUC accepts the probe without affecting database objects.
		dsn += separator + "applica%74ion_name=prior+tag&witself.guard_probe=literal+plus;semi#hash"
	} else if strings.HasPrefix(mode, "tagged_keyword_") {
		quote := func(value string) string {
			return "'" + strings.NewReplacer(`\`, `\\`, `'`, `\'`).Replace(value) + "'"
		}
		dsn = fmt.Sprintf("host=%s port=%d user=%s password=%s dbname=%s sslmode=disable search_path='public' application_name='prior tag'",
			quote(config.Host), config.Port, quote(config.User), quote(config.Password), quote(config.Database))
		switch mode {
		case "tagged_keyword_empty_dsn":
			dsn += " application_name="
		case "tagged_keyword_escape_dsn":
			dsn += ` application_name=old\`
		}
	} else {
		parsed, parseErr := url.Parse(dsn)
		if parseErr != nil {
			t.Fatal("transaction guard URL control configuration failed")
		}
		parsed.Scheme = strings.TrimSuffix(strings.TrimPrefix(mode, "tagged_"), "_url_dsn")
		query := parsed.Query()
		query.Set("application_name", "prior tag")
		query.Set("search_path", "public")
		parsed.RawQuery = query.Encode()
		dsn = parsed.String()
	}
	before, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatal("transaction guard DSN control preparation failed")
	}
	if mode == "tagged_url_runtime_bytes_dsn" && (before.RuntimeParams["application_name"] != "prior+tag" || before.RuntimeParams["witself.guard_probe"] != "literal+plus;semi#hash") {
		t.Fatal("transaction guard literal URI control preparation changed")
	}
	tagged := testenv.GuardPostgresTransactions(t, dsn)
	after, err := pgx.ParseConfig(tagged)
	if err != nil {
		t.Fatal("transaction guard tagged DSN parsing failed")
	}
	tag := after.RuntimeParams["application_name"]
	if !strings.HasPrefix(tag, "witself-test-") || len(tag) > 63 || tag == before.RuntimeParams["application_name"] {
		t.Fatal("transaction guard process tag shape invalid")
	}
	if before.Host != after.Host || before.Port != after.Port || before.Database != after.Database || before.User != after.User || before.Password != after.Password || (before.TLSConfig == nil) != (after.TLSConfig == nil) {
		t.Fatal("transaction guard tag changed connection settings")
	}
	if len(before.RuntimeParams) != len(after.RuntimeParams) {
		t.Fatal("transaction guard tag changed runtime parameter count")
	}
	for key, value := range before.RuntimeParams {
		if key != "application_name" && after.RuntimeParams[key] != value {
			t.Fatal("transaction guard tag changed unrelated runtime parameter")
		}
	}
	if !strings.HasPrefix(mode, "tagged_keyword_") && mode != "tagged_url_runtime_bytes_dsn" {
		originalURL, originalErr := url.Parse(dsn)
		taggedURL, taggedErr := url.Parse(tagged)
		if originalErr != nil || taggedErr != nil || originalURL.Scheme != taggedURL.Scheme {
			t.Fatal("transaction guard tag changed URL form")
		}
		originalQuery, taggedQuery := originalURL.Query(), taggedURL.Query()
		originalQuery.Del("application_name")
		taggedQuery.Del("application_name")
		if originalQuery.Encode() != taggedQuery.Encode() {
			t.Fatal("transaction guard tag changed unrelated URL parameter")
		}
	}
	repeated, err := pgx.ParseConfig(testenv.GuardPostgresTransactions(t, tagged))
	if err != nil || repeated.RuntimeParams["application_name"] != tag {
		t.Fatal("transaction guard repeated registration changed process tag")
	}
}
