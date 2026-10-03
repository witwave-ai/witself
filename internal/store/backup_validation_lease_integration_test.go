package store

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"reflect"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestBackupValidationLeaseOwnershipPostgres(t *testing.T) {
	ctx, st := openAccountEvacuationTestStore(t)
	accountID := fmt.Sprintf("acc_validation_lease_%d", time.Now().UnixNano())
	baseline := st.pool.Stat().AcquiredConns()
	lease, err := st.AcquireBackupValidationLease(ctx, accountID, "backup_one")
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	if !lease.Held() {
		t.Fatal("new lease has no session")
	}
	for setting, want := range map[string]string{"tcp_keepalives_idle": "30", "tcp_keepalives_interval": "10", "tcp_keepalives_count": "6"} {
		var got string
		if err := lease.conn.QueryRow(ctx, "SELECT current_setting($1)", setting).Scan(&got); err != nil || got != want {
			t.Fatalf("keepalive %s = %s / %v", setting, got, err)
		}
	}
	for _, backupID := range []string{"backup_one", "backup_two"} {
		duplicate, err := st.AcquireBackupValidationLease(ctx, accountID, backupID)
		if duplicate != nil {
			duplicate.Close()
		}
		if !errors.Is(err, ErrBackupValidationLeaseHeld) {
			t.Fatalf("same-account acquire = %v", err)
		}
	}
	if running, err := st.BackupValidationInProgress(ctx, accountID); err != nil || !running {
		t.Fatal("session holder invisible", err)
	}
	if running, err := st.BackupValidationInProgress(ctx, accountID+"_unheld"); err != nil || running {
		t.Fatalf("unheld account validation in progress = %v / %v, want false / nil", running, err)
	}
	other, err := st.AcquireBackupValidationLease(ctx, accountID+"_other", "backup_one")
	if err != nil {
		t.Fatalf("different account blocked: %v", err)
	}
	defer other.Close()
	if running, err := st.BackupValidationInProgress(ctx, accountID+"_other"); err != nil || !running {
		t.Fatal("other account holder invisible", err)
	}
	other.Close()
	lease.Close()
	if lease.Held() {
		t.Fatal("closed lease is held")
	}
	lease.Close()
	waitBackupValidationLeaseReleased(ctx, t, st, accountID, baseline)
	waitBackupValidationLeaseReleased(ctx, t, st, accountID+"_other", baseline)
	importLease, err := st.AcquireAccountImportLease(ctx, accountID, "evacuation_validation_isolation")
	if err != nil {
		t.Fatal(err)
	}
	defer importLease.Close()
	if running, err := st.AccountImportInProgress(ctx, accountID, "evacuation_validation_isolation"); err != nil || !running {
		t.Fatalf("import lease in progress = %v / %v, want true / nil", running, err)
	}
	if running, err := st.BackupValidationInProgress(ctx, accountID); err != nil || running {
		t.Fatalf("import-only account validation in progress = %v / %v, want false / nil", running, err)
	}
}

func TestBackupValidationLeaseArchiveParityPostgres(t *testing.T) {
	ctx, st := openAccountEvacuationTestStore(t)
	a := provisionActiveEvacuationTestAccount(ctx, t, st, "validation-lease-parity")
	const backupID = "backup_validation_parity"
	var archive bytes.Buffer
	if err := st.ExportAccountBackup(ctx, a.AccountID, backupID, "source-cell", "test", &archive); err != nil {
		t.Fatal(err)
	}
	if err := deleteAccountForIntegrationTest(ctx, st, a.AccountID); err != nil {
		t.Fatal(err)
	}
	lease, err := st.AcquireBackupValidationLease(ctx, a.AccountID, backupID)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	var pid int
	if err := lease.conn.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&pid); err != nil {
		t.Fatal(err)
	}
	manifest, err := lease.Validate(ctx, bytes.NewReader(archive.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	// Check on another connection: querying the leased one would itself make
	// pg_stat_activity show an active statement instead of the retained idle state.
	var state string
	if err := st.pool.QueryRow(ctx, "SELECT state FROM pg_stat_activity WHERE pid=$1", pid).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "idle" {
		t.Fatalf("retained validation session state = %q, want idle", state)
	}
	var rows int
	if err := st.pool.QueryRow(ctx, "SELECT count(*) FROM accounts WHERE id=$1", a.AccountID).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("validation committed %d account rows: %v", rows, err)
	}
	if !lease.Held() {
		t.Fatal("validation ended its session")
	}
	if running, err := st.BackupValidationInProgress(ctx, a.AccountID); err != nil || !running {
		t.Fatal("validation lost its account lock", err)
	}
	synchronous, err := st.ValidateAccountBackup(ctx, a.AccountID, backupID, bytes.NewReader(archive.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(manifest, synchronous) {
		t.Fatal("lease validation differs from synchronous manifest")
	}
}

func TestBackupValidationLeaseCancelledStatementPostgres(t *testing.T) {
	ctx, st := openAccountEvacuationTestStore(t)
	accountID := fmt.Sprintf("acc_validation_cancel_%d", time.Now().UnixNano())
	baseline := st.pool.Stat().AcquiredConns()
	lease, err := st.AcquireBackupValidationLease(ctx, accountID, "backup_cancel")
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	if running, err := st.BackupValidationInProgress(ctx, accountID); err != nil || !running {
		t.Fatalf("validation lock before cancelled statement = %v / %v, want true / nil", running, err)
	}
	stmtCtx, cancel := context.WithTimeout(ctx, 30*time.Millisecond)
	defer cancel()
	_, err = lease.conn.Exec(stmtCtx, "SELECT pg_sleep(1)")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("statement not cancelled: %v", err)
	}
	if lease.Held() {
		t.Fatal("cancelled statement left its session open")
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		running, err := st.BackupValidationInProgress(ctx, accountID)
		if err != nil {
			t.Fatal(err)
		}
		if !running {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("cancelled session remains locked before Close")
		}
		time.Sleep(10 * time.Millisecond)
	}
	lease.Close()
	waitBackupValidationLeaseReleased(ctx, t, st, accountID, baseline)
}

func TestBackupValidationLeasePoolUnavailablePostgres(t *testing.T) {
	ctx, st := openAccountEvacuationTestStore(t)
	held := make([]*pgxpool.Conn, 0, st.pool.Config().MaxConns)
	defer func() {
		for _, conn := range held {
			conn.Release()
		}
	}()
	for range st.pool.Config().MaxConns {
		conn, err := st.pool.Acquire(ctx)
		if err != nil {
			t.Fatal(err)
		}
		held = append(held, conn)
	}
	acquireCtx, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
	defer cancel()
	if _, err := st.AcquireBackupValidationLease(acquireCtx, "acc_validation_unavailable", "backup_unavailable"); !errors.Is(err, ErrBackupValidationLeaseUnavailable) {
		t.Fatalf("pool exhaustion = %v", err)
	}
	if _, err := st.AcquireBackupValidationLease(acquireCtx, "acc_validation_unavailable", "bad/id"); !errors.Is(err, ErrBackupIDInvalid) {
		t.Fatalf("backup id was not checked before acquisition: %v", err)
	}
}

type backupValidationRetryableTestError struct{}

func (backupValidationRetryableTestError) Error() string     { return "retryable acquisition failure" }
func (backupValidationRetryableTestError) SafeToRetry() bool { return true }

type backupValidationDeadlineTracer struct{ entered bool }

func (tracer *backupValidationDeadlineTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	tracer.entered = true
	expired, cancel := context.WithDeadline(ctx, time.Now().Add(-time.Second))
	cancel()
	return expired
}

func (*backupValidationDeadlineTracer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {
}

func TestBackupValidationLeaseTransientAcquireErrorsPostgres(t *testing.T) {
	ctx, st := openAccountEvacuationTestStore(t)
	plain := errors.New("ordinary acquisition failure")
	for _, name := range []string{"deadline_before_pool", "deadline_before_lock", "refused_connection", "safe_to_retry", "ordinary_error"} {
		t.Run(name, func(t *testing.T) {
			cfg := st.pool.Config()
			cfg.MinConns = 0
			cfg.MinIdleConns = 0
			callCtx := ctx
			var cancel context.CancelFunc
			want := ErrBackupValidationLeaseUnavailable
			tracer := &backupValidationDeadlineTracer{}
			switch name {
			case "deadline_before_pool":
				callCtx, cancel = context.WithDeadline(ctx, time.Now().Add(-time.Second))
				defer cancel()
			case "deadline_before_lock":
				// Expire only the query context, after pool acquisition, without
				// depending on how quickly a loaded test can open the connection.
				cfg.ConnConfig.Tracer = tracer
			case "refused_connection":
				// Keep the required database address and inject a transport-level
				// refusal, which pgconn wraps as a real *pgconn.ConnectError.
				cfg.ConnConfig.DialFunc = func(context.Context, string, string) (net.Conn, error) {
					return nil, syscall.ECONNREFUSED
				}
			case "safe_to_retry":
				cfg.PrepareConn = func(context.Context, *pgx.Conn) (bool, error) {
					return true, backupValidationRetryableTestError{}
				}
			case "ordinary_error":
				want = plain
				cfg.PrepareConn = func(context.Context, *pgx.Conn) (bool, error) { return true, plain }
			}
			pool, err := pgxpool.NewWithConfig(ctx, cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer pool.Close()
			target := &Store{pool: pool}
			lease, err := target.AcquireBackupValidationLease(callCtx, "acc_validation_transient", "backup_transient")
			if lease != nil {
				lease.Close()
			}
			if !errors.Is(err, want) {
				t.Fatalf("acquisition error = %v, want %v", err, want)
			}
			if name == "deadline_before_lock" && !tracer.entered {
				t.Fatal("deadline did not reach the lock query")
			}
		})
	}
}

// Closing sends teardown without waiting for PostgreSQL or pool destruction.
func waitBackupValidationLeaseReleased(ctx context.Context, t *testing.T, st *Store, accountID string, baseline int32) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		running, err := st.BackupValidationInProgress(ctx, accountID)
		if err != nil {
			t.Fatal(err)
		}
		if !running && st.pool.Stat().AcquiredConns() == baseline {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("closed lease leaked: lock=%t acquired=%d baseline=%d", running, st.pool.Stat().AcquiredConns(), baseline)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
