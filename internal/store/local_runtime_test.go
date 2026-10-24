package store

import (
	"context"
	"testing"

	"github.com/witwave-ai/witself/internal/testenv"
)

func TestLocalRuntimeDatabaseStatePostgres(t *testing.T) {
	st, _ := newMigrationTestStore(t, testenv.RequirePostgres(t))
	ctx := context.Background()
	if _, err := st.AppliedSchemaVersion(ctx); err == nil {
		t.Fatal("missing Goose table reported an applied schema")
	}
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}
	if got, err := st.AppliedSchemaVersion(ctx); err != nil || got != SchemaVersion() {
		t.Fatalf("applied schema = %d, error = %v", got, err)
	}
	if _, err := st.LocalBootstrapAccount(ctx); err == nil {
		t.Fatal("missing bootstrap account reported ready")
	}
	accountID, err := st.EnsureDefaultAccount(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := st.LocalBootstrapAccount(ctx); err != nil || got != accountID {
		t.Fatal("active bootstrap account was not resolved", err)
	}
	for _, status := range []string{"pending", "suspended", "closed"} {
		if _, err := st.pool.Exec(ctx, `UPDATE accounts SET status = $1 WHERE id = $2`, status, accountID); err != nil {
			t.Fatal(err)
		}
		if _, err := st.LocalBootstrapAccount(ctx); err == nil {
			t.Fatalf("%s bootstrap account reported ready", status)
		}
	}
	if _, err := st.pool.Exec(ctx, `DELETE FROM accounts WHERE id = $1`, accountID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.LocalBootstrapAccount(ctx); err == nil {
		t.Fatal("deleted bootstrap account reported ready")
	}
	st.Close()
	if _, err := st.AppliedSchemaVersion(ctx); err == nil {
		t.Fatal("closed store reported an applied schema")
	}
}
