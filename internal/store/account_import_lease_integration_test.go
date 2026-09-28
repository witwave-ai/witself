package store

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestAccountImportLeaseOwnershipPostgres(t *testing.T) {
	ctx, st := openAccountEvacuationTestStore(t)
	accountID := fmt.Sprintf("acc_lease_%d", time.Now().UnixNano())
	const epoch = "evac_lease"
	baseline := st.pool.Stat().AcquiredConns()
	lease, err := st.AcquireAccountImportLease(ctx, accountID, epoch)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	for setting, want := range map[string]string{"tcp_keepalives_idle": "30", "tcp_keepalives_interval": "10", "tcp_keepalives_count": "6"} {
		var got string
		if err := lease.conn.QueryRow(ctx, "SELECT current_setting($1)", setting).Scan(&got); err != nil || got != want {
			t.Fatalf("keepalive %s = %s / %v", setting, got, err)
		}
	}
	if _, err := st.AcquireAccountImportLease(ctx, accountID, epoch); !errors.Is(err, ErrAccountImportLeaseHeld) {
		t.Fatalf("duplicate acquire: %v", err)
	}
	if running, err := st.AccountImportInProgress(ctx, accountID, epoch); err != nil || !running {
		t.Fatal("session holder invisible", err)
	}
	if running, err := st.AccountImportInProgress(ctx, accountID, "other_epoch"); err != nil || running {
		t.Fatal("lock not epoch scoped", err)
	}
	_, _, err = st.ImportAccountEvacuation(ctx, accountID, epoch, bytes.NewReader(nil))
	if !errors.Is(err, ErrAccountImportBusy) {
		t.Fatalf("sync importer bypassed lease: %v", err)
	}
	// The leased transaction gets through the lock and fails on the empty archive.
	_, _, err = lease.Import(ctx, bytes.NewReader(nil))
	if err == nil || errors.Is(err, ErrAccountImportBusy) {
		t.Fatal("lease transaction lock is not reentrant")
	}
	lease.Close()
	lease.Close()
	if running, err := st.AccountImportInProgress(ctx, accountID, epoch); err != nil || running {
		t.Fatal("closed lease remains locked", err)
	}
	if st.pool.Stat().AcquiredConns() != baseline {
		t.Fatal("pool connection leaked")
	}
	conn, err := st.pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	var keepalive string
	if err := conn.QueryRow(ctx, "SHOW tcp_keepalives_idle").Scan(&keepalive); err != nil {
		t.Fatal(err)
	}
	if keepalive == "30" {
		t.Fatal("declined acquire leaked keepalive settings")
	}
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var held bool
	if err := tx.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock(`+AccountImportAdvisoryKeySQL+`)`, accountID, epoch).Scan(&held); err != nil || !held {
		t.Fatal("transaction lock failed", err)
	}
	if _, err := st.AcquireAccountImportLease(ctx, accountID, epoch); !errors.Is(err, ErrAccountImportLeaseHeld) {
		t.Fatal("transaction holder ignored", err)
	}
	if running, err := st.AccountImportInProgress(ctx, accountID, epoch); err != nil || !running {
		t.Fatal("transaction holder invisible", err)
	}
}

func TestAccountImportLeaseCancelledStatementPostgres(t *testing.T) {
	ctx, st := openAccountEvacuationTestStore(t)
	accountID := fmt.Sprintf("acc_cancel_%d", time.Now().UnixNano())
	baseline := st.pool.Stat().AcquiredConns()
	lease, err := st.AcquireAccountImportLease(ctx, accountID, "evac_cancel")
	if err != nil {
		t.Fatal(err)
	}
	stmtCtx, cancel := context.WithTimeout(ctx, 30*time.Millisecond)
	defer cancel()
	_, err = lease.conn.Exec(stmtCtx, "SELECT pg_sleep(10)")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("statement not cancelled: %v", err)
	}
	lease.Close()
	// Closing a cancelled pgx connection sends teardown without waiting for
	// the backend to process it; observe eventual release within the drain bound.
	deadline := time.Now().Add(2 * time.Second)
	for {
		running, err := st.AccountImportInProgress(ctx, accountID, "evac_cancel")
		if err != nil {
			t.Fatal(err)
		}
		if !running {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("cancelled session lock leaked")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if st.pool.Stat().AcquiredConns() != baseline {
		t.Fatal("cancelled connection leaked")
	}
}

func TestAccountImportReceiptBranchesPostgres(t *testing.T) {
	ctx, st := openAccountEvacuationTestStore(t)
	for _, tc := range []struct {
		name, current, role, last, outcome string
		present, completed                 bool
		want                               error
	}{
		{name: "no markers", want: ErrAccountExists},
		{name: "same null role", current: "E", want: ErrAccountEvacuationMismatch},
		{name: "same source", current: "E", role: "source"},
		{name: "same target", current: "E", role: "target", present: true},
		{name: "completed", last: "E", outcome: "completed", present: true, completed: true},
		{name: "stale completed under new live epoch", current: "E2", role: "source", last: "E", outcome: "completed", want: ErrAccountEvacuationInProgress},
		{name: "different live", current: "E2", role: "target", want: ErrAccountEvacuationInProgress},
		{name: "aborted", last: "E", outcome: "aborted", want: ErrAccountExists},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := provisionActiveEvacuationTestAccount(ctx, t, st, "receipt")
			tx, err := st.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback(ctx) }()
			if tc.current != "" {
				if err := setEvacuationAuthorityTx(ctx, tx, tc.current); err != nil {
					t.Fatal(err)
				}
			}
			_, err = tx.Exec(ctx, `UPDATE accounts SET evacuation_id=NULLIF($2,''), evacuation_started_at=CASE WHEN $2='' THEN NULL ELSE now() END,
   evacuation_role=NULLIF($3,''), last_evacuation_id=NULLIF($4,''), last_evacuation_completed_at=CASE WHEN $4='' THEN NULL ELSE now() END,
   last_evacuation_outcome=NULLIF($5,'') WHERE id=$1`, a.AccountID, tc.current, tc.role, tc.last, tc.outcome)
			if err != nil {
				t.Fatal(err)
			}
			d, present, err := AccountImportReceipt(ctx, tx, a.AccountID, "E")
			if !errors.Is(err, tc.want) || present != tc.present || d.EvacuationCompleted != tc.completed {
				t.Fatalf("receipt = %v/%v/%v", present, d.EvacuationCompleted, err)
			}
			if present && (!d.AlreadyImported || d.CurrentStatus != "active" || d.EvacuationRole != "target") {
				t.Fatal("inexact receipt")
			}
		})
	}
	if _, present, err := AccountImportReceipt(ctx, st.pool, "absent_account", "E"); err != nil || present {
		t.Fatal("missing row is not absent", err)
	}
}

func TestAccountImportLeaseArchiveParityPostgres(t *testing.T) {
	for _, fresh := range []bool{false, true} {
		t.Run(fmt.Sprintf("fresh_%v", fresh), func(t *testing.T) {
			ctx, st := openAccountEvacuationTestStore(t)
			a := provisionActiveEvacuationTestAccount(ctx, t, st, "lease-parity")
			const epoch = "evac_parity"
			if _, err := st.BeginAccountEvacuation(ctx, a.AccountID, epoch, "lease parity"); err != nil {
				t.Fatal(err)
			}
			var archive bytes.Buffer
			if err := st.ExportAccountEvacuation(ctx, a.AccountID, epoch, "source-cell", "test", &archive); err != nil {
				t.Fatal(err)
			}
			if fresh {
				if _, err := st.FinalizeAccountEvacuationSource(ctx, a.AccountID, epoch); err != nil {
					t.Fatal(err)
				}
			}
			lease, err := st.AcquireAccountImportLease(ctx, a.AccountID, epoch)
			if err != nil {
				t.Fatal(err)
			}
			defer lease.Close()
			if _, present := lease.Receipt(); present {
				t.Fatal("source/missing row replayed without promotion")
			}
			m, d, err := lease.Import(ctx, bytes.NewReader(archive.Bytes()))
			if err != nil {
				t.Fatal(err)
			}
			if m.AccountID != a.AccountID || d.EvacuationRole != "target" {
				t.Fatal("bad import acknowledgement")
			}
			lease.Close()
			retry, err := st.AcquireAccountImportLease(ctx, a.AccountID, epoch)
			if err != nil {
				t.Fatal(err)
			}
			rd, present := retry.Receipt()
			retry.Close()
			if !present || !rd.AlreadyImported || rd.EvacuationCompleted {
				t.Fatal("missing live receipt")
			}
			_, syncDisposition, err := st.ImportAccountEvacuation(ctx, a.AccountID, epoch, bytes.NewReader(archive.Bytes()))
			if err != nil || syncDisposition != rd {
				t.Fatal("sync/lease parity mismatch", err)
			}
			if _, err := st.CompleteAccountEvacuation(ctx, a.AccountID, epoch); err != nil {
				t.Fatal(err)
			}
			completed, err := st.AcquireAccountImportLease(ctx, a.AccountID, epoch)
			if err != nil {
				t.Fatal(err)
			}
			cd, present := completed.Receipt()
			completed.Close()
			if !present || !cd.EvacuationCompleted {
				t.Fatal("missing completed receipt")
			}
		})
	}
}

func TestAccountImportLeasePoolUnavailablePostgres(t *testing.T) {
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
	if _, err := st.AcquireAccountImportLease(acquireCtx, "acc_unavailable", "evac_unavailable"); !errors.Is(err, ErrAccountImportLeaseUnavailable) {
		t.Fatalf("pool exhaustion = %v", err)
	}
}
