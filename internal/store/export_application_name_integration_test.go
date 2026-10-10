package store

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/witwave-ai/witself/internal/testenv"
)

func TestExportTransactionApplicationNamePostgres(t *testing.T) {
	for _, test := range []struct {
		name      string
		kind      string
		nameIndex int
		cancel    bool
	}{
		{name: "account", kind: "account", nameIndex: 0},
		{name: "backup", kind: "backup", nameIndex: 1},
		{name: "evacuation", kind: "evacuation", nameIndex: 2},
		{name: "self", kind: "self", nameIndex: 3},
		{name: "backup_cancel", kind: "backup", nameIndex: 1, cancel: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			st, _ := newMigrationTestStore(t, testenv.RequirePostgres(t))
			if err := st.Migrate(); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			t.Cleanup(cancel)
			account := provisionActiveEvacuationTestAccount(ctx, t, st, "export-application-name-"+test.name)
			const evacuationID = "evac_export_application_name"
			switch test.kind {
			case "account":
				if err := st.SuspendAccountSystem(ctx, account.AccountID, "evacuation", "export application name test"); err != nil {
					t.Fatal(err)
				}
			case "evacuation":
				if _, err := st.BeginAccountEvacuation(ctx, account.AccountID, evacuationID, "export application name test"); err != nil {
					t.Fatal(err)
				}
			}

			// Holding the observer keeps it distinct from the export connection.
			// Each query is outside an explicit transaction so activity snapshots
			// are fresh, including after a cancelled export closes its connection.
			obs, err := st.pool.Acquire(ctx)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(obs.Release)
			var sessionDefault string
			if err := obs.QueryRow(ctx, `SHOW application_name`).Scan(&sessionDefault); err != nil {
				t.Fatal(err)
			}

			exportCtx, exportCancel := context.WithCancel(ctx)
			gate := &selfExportSnapshotGate{
				ctx: exportCtx, entered: make(chan struct{}), release: make(chan struct{}),
			}
			var releaseOnce sync.Once
			release := func() { releaseOnce.Do(func() { close(gate.release) }) }
			done := make(chan struct{})
			var exportErr error
			// LIFO cleanup drains the exporter, releases the observer, then
			// allows fixture account cleanup, Store.Close and schema deletion.
			t.Cleanup(func() {
				exportCancel()
				release()
				timer := time.NewTimer(30 * time.Second)
				defer timer.Stop()
				select {
				case <-done:
				case <-timer.C:
					t.Error("application name exporter did not drain after cancellation")
				}
			})
			go func() {
				defer close(done)
				switch test.kind {
				case "account":
					exportErr = st.ExportAccount(exportCtx, account.AccountID, "test-cell", "test", gate)
				case "backup":
					exportErr = st.ExportAccountBackup(exportCtx, account.AccountID, "backup_export_application_name", "test-cell", "test", gate)
				case "evacuation":
					exportErr = st.ExportAccountEvacuation(exportCtx, account.AccountID, evacuationID, "test-cell", "test", gate)
				case "self":
					exportErr = st.ExportAccountSelf(exportCtx, account.AccountID, "test-cell", "test", gate)
				}
			}()
			barrierCtx, barrierCancel := context.WithTimeout(ctx, 30*time.Second)
			select {
			case <-gate.entered:
				barrierCancel()
			case <-done:
				barrierCancel()
				t.Fatalf("export ended before first output: %v", exportErr)
			case <-barrierCtx.Done():
				barrierCancel()
				t.Fatal("export did not reach first output")
			}

			// Identify this schema's exporter by its locks, not by the name
			// under test or by other worktrees' exports in the same database.
			rows, err := obs.Query(ctx, `
				SELECT a.pid, a.backend_type, a.application_name, a.xact_start IS NOT NULL
				  FROM pg_stat_activity a
				 WHERE a.pid <> pg_backend_pid()
				   AND a.datname = current_database()
				   AND a.backend_type = 'client backend'
				   AND EXISTS (
				         SELECT 1 FROM pg_locks l JOIN pg_class c ON c.oid = l.relation
				          WHERE l.pid = a.pid AND l.granted
				            AND c.relnamespace = (SELECT oid FROM pg_namespace WHERE nspname = current_schema()))`)
			if err != nil {
				t.Fatal(err)
			}
			var pid int
			var backendType, applicationName string
			var openTransaction bool
			var matchingRows []string
			for rows.Next() {
				if err := rows.Scan(&pid, &backendType, &applicationName, &openTransaction); err != nil {
					rows.Close()
					t.Fatal(err)
				}
				matchingRows = append(matchingRows, fmt.Sprintf("pid=%d backend_type=%q application_name=%q", pid, backendType, applicationName))
			}
			rows.Close()
			if err := rows.Err(); err != nil {
				t.Fatal(err)
			}
			if len(matchingRows) != 1 {
				t.Fatalf("paused export backends in test schema = %d, want 1; matching rows: %v", len(matchingRows), matchingRows)
			}
			if want := exportApplicationNames[test.nameIndex]; applicationName != want {
				t.Errorf("paused export application_name = %q, want %q", applicationName, want)
			}
			if !openTransaction {
				t.Error("paused export has no open transaction")
			}

			if test.cancel {
				exportCancel()
			} else {
				release()
			}
			select {
			case <-done:
			case <-ctx.Done():
				t.Fatal("export did not drain after release or cancellation")
			}
			if test.cancel {
				if !errors.Is(exportErr, context.Canceled) {
					t.Errorf("cancelled export error = %v, want context.Canceled", exportErr)
				}
			} else if exportErr != nil {
				t.Fatalf("complete paused export: %v", exportErr)
			}

			// P1 observes the exact backend even after it has left the pool.
			// P2 separately inspects all reusable sessions, and still executes
			// when P1 fails so a session-level mutation exposes both leaks.
			checkPID := func() (bool, error) {
				var name string
				err := obs.QueryRow(ctx, `SELECT application_name FROM pg_stat_activity WHERE pid = $1`, pid).Scan(&name)
				if errors.Is(err, pgx.ErrNoRows) {
					return true, nil
				}
				return err == nil && name == sessionDefault, err
			}
			if test.cancel {
				started := time.Now()
				deadline := started.Add(10 * time.Second)
				for {
					reset, err := checkPID()
					if reset && err == nil {
						t.Logf("P1 cancelled export backend reset or exited after %s", time.Since(started))
						break
					}
					if !time.Now().Before(deadline) {
						t.Errorf("P1 cancelled export backend did not reset or exit within 10s: %v", err)
						break
					}
					time.Sleep(50 * time.Millisecond)
				}
			} else if reset, err := checkPID(); err != nil || !reset {
				t.Errorf("P1 export backend did not restore session application_name after commit: %v", err)
			}
			idle := st.pool.AcquireAllIdle(ctx)
			if !test.cancel && len(idle) == 0 {
				t.Error("P2 no idle session available after successful export")
			}
			for _, conn := range idle {
				var name string
				err := conn.QueryRow(ctx, `SHOW application_name`).Scan(&name)
				conn.Release()
				if err != nil {
					t.Errorf("P2 read idle session application_name: %v", err)
				} else if name != sessionDefault {
					t.Errorf("P2 idle session application_name = %q, want session default %q", name, sessionDefault)
				}
			}
		})
	}
}
