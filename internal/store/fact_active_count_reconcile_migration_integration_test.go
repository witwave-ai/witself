package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestActiveFactCountSchema78ReconcilePostgres(t *testing.T) {
	baseDSN := os.Getenv("WITSELF_TEST_DATABASE_URL")
	if baseDSN == "" {
		t.Skip("WITSELF_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	st, dsn := newMigrationTestStore(t, baseDSN)
	migrationTestUpTo(t, dsn, 77)

	account, err := st.ProvisionAccount(
		ctx, "stored-fact-schema-78@witwave.ai",
		"stored fact schema 78", time.Hour,
	)
	if err != nil {
		t.Fatal(err)
	}
	if activated, err := st.ActivateAccount(ctx, account.AccountID); err != nil || !activated {
		t.Fatalf("activate = %t / %v", activated, err)
	}
	realm, err := st.CreateRealm(ctx, account.AccountID, "default")
	if err != nil {
		t.Fatal(err)
	}
	newOwner := func(name string) Principal {
		t.Helper()
		agent, createErr := st.CreateAgent(ctx, account.AccountID, realm.ID, name)
		if createErr != nil {
			t.Fatal(createErr)
		}
		return Principal{
			Kind: PrincipalAgent, ID: agent.ID, AccountID: account.AccountID,
			RealmID: realm.ID, AccountStatus: "active",
		}
	}
	activeOwner := newOwner("active")
	zeroOwner := newOwner("zero")

	if _, err := st.SetFact(ctx, activeOwner, SetFactInput{
		Predicate: "preferences/active", Value: json.RawMessage(`"active"`),
		SourceKind: FactSourceAgent,
	}); err != nil {
		t.Fatal(err)
	}
	unresolved, err := st.SetFact(ctx, activeOwner, SetFactInput{
		Predicate: "preferences/unresolved", Value: json.RawMessage(`"unresolved"`),
		SourceKind: FactSourceAgent,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.pool.Exec(ctx, `
		UPDATE facts SET resolved_assertion_id=NULL WHERE id=$1`,
		unresolved.ID,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := st.pool.Exec(ctx, `
		UPDATE agents
		   SET active_fact_count=CASE id WHEN $1 THEN 17 WHEN $2 THEN 9 END
		 WHERE id IN ($1,$2)`,
		activeOwner.ID, zeroOwner.ID,
	); err != nil {
		t.Fatal(err)
	}

	migrationTestUpTo(t, dsn, 78)
	assertMigrationTestVersion(t, dsn, 78)
	assertStoredFactCountInvariant(ctx, t, st, activeOwner, 1)
	assertStoredFactCountInvariant(ctx, t, st, zeroOwner, 0)
	var mismatched int64
	if err := st.pool.QueryRow(ctx, `
		SELECT count(*)
		  FROM agents owner
		 WHERE owner.active_fact_count IS DISTINCT FROM (
		   SELECT count(*)::BIGINT
		     FROM facts fact
		    WHERE fact.owner_agent_id=owner.id
		      AND fact.deleted_at IS NULL
		      AND fact.resolved_assertion_id IS NOT NULL
		 )`,
	).Scan(&mismatched); err != nil {
		t.Fatal(err)
	}
	if mismatched != 0 {
		t.Fatalf("schema 78 left %d active fact count mismatches", mismatched)
	}

	migrationTestDownTo(t, dsn, 77)
	assertMigrationTestVersion(t, dsn, 77)
	assertStoredFactCountInvariant(ctx, t, st, activeOwner, 1)
	assertStoredFactCountInvariant(ctx, t, st, zeroOwner, 0)

	if _, err := st.pool.Exec(ctx, `
		UPDATE agents SET active_fact_count=23 WHERE id=$1`,
		activeOwner.ID,
	); err != nil {
		t.Fatal(err)
	}
	migrationTestUpTo(t, dsn, 78)
	assertMigrationTestVersion(t, dsn, 78)
	assertStoredFactCountInvariant(ctx, t, st, activeOwner, 1)
}

func TestActiveFactCountSchema78FenceRetryPostgres(t *testing.T) {
	baseDSN := os.Getenv("WITSELF_TEST_DATABASE_URL")
	if baseDSN == "" {
		t.Skip("WITSELF_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	st, dsn := newMigrationTestStore(t, baseDSN)
	migrationTestUpTo(t, dsn, 77)

	account, err := st.ProvisionAccount(
		ctx, "stored-fact-schema-78-fence@witwave.ai",
		"stored fact schema 78 fence", time.Hour,
	)
	if err != nil {
		t.Fatal(err)
	}
	if activated, err := st.ActivateAccount(ctx, account.AccountID); err != nil || !activated {
		t.Fatalf("activate = %t / %v", activated, err)
	}
	realm, err := st.CreateRealm(ctx, account.AccountID, "default")
	if err != nil {
		t.Fatal(err)
	}
	agent, err := st.CreateAgent(ctx, account.AccountID, realm.ID, "writer")
	if err != nil {
		t.Fatal(err)
	}
	p := Principal{
		Kind: PrincipalAgent, ID: agent.ID, AccountID: account.AccountID,
		RealmID: realm.ID, AccountStatus: "active",
	}
	if _, err := st.SetFact(ctx, p, SetFactInput{
		Predicate: "preferences/active", Value: json.RawMessage(`"active"`),
		SourceKind: FactSourceAgent,
	}); err != nil {
		t.Fatal(err)
	}

	writer, err := st.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = writer.Rollback(ctx) }()
	if _, err := writer.Exec(ctx, `
		UPDATE agents SET active_fact_count=active_fact_count+1 WHERE id=$1`,
		p.ID,
	); err != nil {
		t.Fatal(err)
	}

	migrationDB := migrationTestSQLDB(t, dsn)
	err = migration78PromptContentionError(t, migrationDB, func() {
		_ = writer.Rollback(ctx)
	})
	_ = migrationDB.Close()
	var lockErr *pgconn.PgError
	if !errors.As(err, &lockErr) || lockErr.Code != "55P03" {
		t.Fatalf("schema 78 migration with active writer error=%v, want SQLSTATE 55P03", err)
	}
	assertMigrationTestVersion(t, dsn, 77)

	if err := writer.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	migrationTestUpTo(t, dsn, 78)
	assertMigrationTestVersion(t, dsn, 78)
	assertStoredFactCountInvariant(ctx, t, st, p, 1)
}

func TestActiveFactCountSchema78DirectFactWriteFenceRetryPostgres(t *testing.T) {
	baseDSN := os.Getenv("WITSELF_TEST_DATABASE_URL")
	if baseDSN == "" {
		t.Skip("WITSELF_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	st, dsn := newMigrationTestStore(t, baseDSN)
	migrationTestUpTo(t, dsn, 77)

	account, err := st.ProvisionAccount(
		ctx, "stored-fact-schema-78-direct-write@witwave.ai",
		"stored fact schema 78 direct write", time.Hour,
	)
	if err != nil {
		t.Fatal(err)
	}
	if activated, err := st.ActivateAccount(ctx, account.AccountID); err != nil || !activated {
		t.Fatalf("activate = %t / %v", activated, err)
	}
	realm, err := st.CreateRealm(ctx, account.AccountID, "default")
	if err != nil {
		t.Fatal(err)
	}
	agent, err := st.CreateAgent(ctx, account.AccountID, realm.ID, "legacy-writer")
	if err != nil {
		t.Fatal(err)
	}
	p := Principal{
		Kind: PrincipalAgent, ID: agent.ID, AccountID: account.AccountID,
		RealmID: realm.ID, AccountStatus: "active",
	}
	fact, err := st.SetFact(ctx, p, SetFactInput{
		Predicate: "preferences/active", Value: json.RawMessage(`"active"`),
		SourceKind: FactSourceAgent,
	})
	if err != nil {
		t.Fatal(err)
	}

	writer, err := st.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = writer.Rollback(ctx) }()
	if _, err := writer.Exec(ctx, `
		UPDATE facts SET updated_at=updated_at WHERE id=$1`,
		fact.ID,
	); err != nil {
		t.Fatal(err)
	}

	migrationDB := migrationTestSQLDB(t, dsn)
	err = migration78PromptContentionError(t, migrationDB, func() {
		_ = writer.Rollback(ctx)
	})
	_ = migrationDB.Close()
	var lockErr *pgconn.PgError
	if !errors.As(err, &lockErr) || lockErr.Code != "55P03" {
		t.Fatalf("schema 78 migration with direct fact writer error=%v, want SQLSTATE 55P03", err)
	}
	assertMigrationTestVersion(t, dsn, 77)

	if err := writer.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	migrationTestUpTo(t, dsn, 78)
	assertMigrationTestVersion(t, dsn, 78)
	assertStoredFactCountInvariant(ctx, t, st, p, 1)
}

func migration78PromptContentionError(
	t *testing.T,
	migrationDB *sql.DB,
	release func(),
) error {
	t.Helper()
	errCh := make(chan error, 1)
	go func() {
		errCh <- migrationTestUpToDB(migrationDB, 78)
	}()
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	select {
	case err := <-errCh:
		return err
	case <-timer.C:
		release()
		select {
		case <-errCh:
		case <-time.After(5 * time.Second):
		}
		t.Fatal("schema 78 migration did not fail promptly under write contention")
		return nil
	}
}
