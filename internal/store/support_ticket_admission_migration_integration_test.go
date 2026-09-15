package store

import (
	"context"
	"testing"
	"time"

	"github.com/pressly/goose/v3"
	"github.com/witwave-ai/witself/internal/testenv"
)

func TestMigration95SupportTicketAdmissionAllowsLiveWritesPostgres(t *testing.T) {
	st, dsn := newMigrationTestStore(t, testenv.RequirePostgres(t))
	migrationTestUpTo(t, dsn, 94)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	const accountID, operatorID = "acc_support_migration", "op_support_migration"
	if _, err := st.pool.Exec(ctx, `INSERT INTO accounts(id,display_name,status)
		VALUES($1,'support migration test','active')`, accountID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.pool.Exec(ctx, `INSERT INTO operators(id,account_id,display_name,role)
		VALUES($1,$2,'support migration test','account_owner')`, operatorID, accountID); err != nil {
		t.Fatal(err)
	}

	// Hold an old replica's writer transaction open. A regular index build
	// queues a ShareLock behind it and blocks subsequent support inserts;
	// a concurrent build waits for this transaction while allowing writes.
	writer, err := st.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = writer.Rollback(context.Background()) }()
	if _, err := writer.Exec(ctx, `LOCK TABLE support_tickets IN ROW EXCLUSIVE MODE`); err != nil {
		t.Fatal(err)
	}
	writerPID := writer.Conn().PgConn().PID()
	db := migrationTestSQLDB(t, dsn)
	defer func() { _ = db.Close() }()
	migrationDone := make(chan struct{})
	var migrationErr error
	go func() {
		defer close(migrationDone)
		migrationErr = goose.UpToContext(ctx, db, "migrations", 95)
	}()
	defer func() {
		cancel()
		_ = writer.Rollback(context.Background())
		<-migrationDone
	}()

	// Observe the actual lock wait before issuing writes, so the test also
	// catches a non-concurrent build without depending on index-build speed.
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		var waiting bool
		if err := st.pool.QueryRow(ctx, `SELECT EXISTS (
			SELECT 1 FROM pg_locks
			WHERE relation='support_tickets'::regclass
			  AND $1::int = ANY(pg_blocking_pids(pid))
		)`, writerPID).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			break
		}
		select {
		case <-migrationDone:
			t.Fatalf("migration finished before waiting for the live writer: %v", migrationErr)
		case <-ctx.Done():
			t.Fatal("migration did not wait for the live writer")
		case <-ticker.C:
		}
	}

	writeCtx, cancelWrites := context.WithTimeout(ctx, 10*time.Second)
	defer cancelWrites()
	ticket, _, err := st.OpenTicket(writeCtx, OpenTicketInput{
		AccountID: accountID, OperatorID: operatorID,
		Subject: "during migration", Body: "support stays available",
	})
	if err != nil {
		t.Fatalf("open support ticket while migration waits for another writer: %v", err)
	}
	if _, err := st.ReplyToTicket(writeCtx, accountID, operatorID, ticket.ID, "follow-up during migration"); err != nil {
		t.Fatalf("reply to support ticket during migration: %v", err)
	}
	if err := st.LogEvent(writeCtx, EventInput{
		AccountID: accountID, ActorKind: ActorOwner, ActorID: operatorID,
		Verb: VerbAccountRenamed, Metadata: map[string]any{"display_name": "support migration test"},
	}); err != nil {
		t.Fatalf("unrelated account event during migration: %v", err)
	}
	select {
	case <-migrationDone:
		t.Fatalf("migration finished before the original writer released its lock: %v", migrationErr)
	default:
	}
	if err := writer.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-migrationDone:
		if migrationErr != nil {
			t.Fatal(migrationErr)
		}
	case <-ctx.Done():
		t.Fatal("migration did not finish after the original writer released its lock")
	}
	assertMigrationTestVersion(t, dsn, 95)
	var valid, ready bool
	if err := st.pool.QueryRow(ctx, `SELECT indisvalid, indisready FROM pg_index
		WHERE indexrelid='support_tickets_by_account_opened'::regclass`).Scan(&valid, &ready); err != nil {
		t.Fatal(err)
	}
	if !valid || !ready {
		t.Fatalf("admission index valid/ready = %t/%t, want true/true", valid, ready)
	}
}
