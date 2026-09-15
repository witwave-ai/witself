package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/pressly/goose/v3"
	"github.com/witwave-ai/witself/internal/testenv"
)

func TestMigration95SupportTicketAdmissionIndexRetryPostgres(t *testing.T) {
	baseDSN := testenv.RequirePostgres(t)
	for _, interrupted := range []bool{true, false} {
		name := "completed before version recorded"
		if interrupted {
			name = "interrupted concurrent build"
		}
		t.Run(name, func(t *testing.T) {
			st, dsn := newMigrationTestStore(t, baseDSN)
			migrationTestUpTo(t, dsn, 94)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			if interrupted {
				migrationDB := migrationTestSQLDB(t, dsn)
				defer func() { _ = migrationDB.Close() }()
				migrationDB.SetMaxOpenConns(1)
				migrationDB.SetMaxIdleConns(1)
				// Pin the backend so cancellation targets only this migration;
				// bound its lifetime even if an assertion fails before cancellation.
				if _, err := migrationDB.ExecContext(ctx, `SET statement_timeout = '15s'`); err != nil {
					t.Fatal(err)
				}
				var migrationPID int
				if err := migrationDB.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&migrationPID); err != nil {
					t.Fatal(err)
				}
				writer, err := st.pool.Begin(ctx)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = writer.Rollback(context.Background()) }()
				if _, err := writer.Exec(ctx, `LOCK TABLE support_tickets IN ROW EXCLUSIVE MODE`); err != nil {
					t.Fatal(err)
				}
				migrationCtx, cancelMigration := context.WithCancel(ctx)
				migrationDone := make(chan struct{})
				var migrationErr error
				go func() {
					defer close(migrationDone)
					migrationErr = goose.UpToContext(migrationCtx, migrationDB, "migrations", 95)
				}()
				defer func() {
					cancelMigration()
					_ = writer.Rollback(context.Background())
					<-migrationDone
				}()

				// Concurrent CREATE commits its invalid catalog entry before
				// waiting for existing writers. Observe that phase before canceling
				// so this reproduces a real interrupted build, not a timing guess.
				ticker := time.NewTicker(10 * time.Millisecond)
				defer ticker.Stop()
				for {
					var waiting bool
					if err := st.pool.QueryRow(ctx, `
						SELECT EXISTS (
						  SELECT 1 FROM pg_stat_progress_create_index
						   WHERE pid = $1
						     AND relid = 'support_tickets'::regclass
						     AND phase = 'waiting for writers before build'
						)`, migrationPID).Scan(&waiting); err != nil {
						t.Fatal(err)
					}
					if waiting {
						break
					}
					select {
					case <-migrationDone:
						t.Fatalf("migration ended before an interruptible concurrent build: %v", migrationErr)
					case <-ctx.Done():
						t.Fatalf("waiting for the concurrent build: %v", ctx.Err())
					case <-ticker.C:
					}
				}
				var canceled bool
				if err := st.pool.QueryRow(ctx, `SELECT pg_cancel_backend($1)`, migrationPID).Scan(&canceled); err != nil {
					t.Fatal(err)
				}
				if !canceled {
					t.Fatal("migration backend did not accept cancellation")
				}
				select {
				case <-migrationDone:
					var pgErr *pgconn.PgError
					if !errors.As(migrationErr, &pgErr) || pgErr.Code != "57014" {
						t.Fatalf("interrupted migration error = %v, want SQLSTATE 57014", migrationErr)
					}
				case <-ctx.Done():
					t.Fatalf("waiting for migration cancellation: %v", ctx.Err())
				}
				if err := writer.Rollback(ctx); err != nil {
					t.Fatal(err)
				}
			} else {
				// Model a completed concurrent build whose Goose version write
				// never committed before the server stopped.
				if _, err := st.pool.Exec(ctx, `
					CREATE INDEX CONCURRENTLY support_tickets_by_account_opened
					ON support_tickets (account_id, opened_at DESC)`); err != nil {
					t.Fatal(err)
				}
			}

			assertMigrationTestVersion(t, dsn, 94)
			var valid bool
			if err := st.pool.QueryRow(ctx, `
				SELECT indisvalid FROM pg_index
				WHERE indexrelid = 'support_tickets_by_account_opened'::regclass`).Scan(&valid); err != nil {
				t.Fatal(err)
			}
			if valid == interrupted {
				t.Fatalf("index validity before retry = %t, want %t", valid, !interrupted)
			}

			migrationTestUpTo(t, dsn, 95)
			assertMigrationTestVersion(t, dsn, 95)
			var ready bool
			var definition string
			if err := st.pool.QueryRow(ctx, `
				SELECT indisvalid, indisready, pg_get_indexdef(indexrelid)
				FROM pg_index
				WHERE indexrelid = 'support_tickets_by_account_opened'::regclass`).Scan(&valid, &ready, &definition); err != nil {
				t.Fatal(err)
			}
			if !valid || !ready || !strings.Contains(definition, "(account_id, opened_at DESC)") {
				t.Fatalf("retried admission index: valid=%t ready=%t definition=%s", valid, ready, definition)
			}
		})
	}
}
