package store

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/witwave-ai/witself/internal/testenv"
)

// This oracle deliberately calls the production verifier after the fixture
// provisioner. Checking only the helper's own attribute declarations concealed
// the previous NOLOGIN disagreement with migration preflight.
func TestSyncPrincipalFixtureProvisioningPostgres(t *testing.T) {
	dsn := testenv.RequirePostgres(t)
	if testenv.ValidateHarnessPostgresDSN(dsn) != nil {
		t.Fatal("principal fixture database selection refused")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	db, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal("principal fixture pool unavailable")
	}
	defer db.Close()
	t.Run("fixed_existing", func(t *testing.T) {
		if testenv.EnsureSyncPrincipalRoles(ctx, db, SyncPrincipalRoleNames()...) != nil {
			t.Fatal("principal fixture provisioning refused")
		}
		st := &Store{pool: db}
		if st.VerifySyncPrincipals(ctx) != nil {
			t.Fatal("principal fixture disagrees with production verifier")
		}
	})
	t.Run("transactional_creation", func(t *testing.T) {
		tx, err := db.Begin(ctx)
		if err != nil {
			t.Fatal("principal fixture transaction unavailable")
		}
		defer func() { _ = tx.Rollback(context.Background()) }()
		var suffix string
		if tx.QueryRow(ctx, `SELECT pg_backend_pid()::text||'_'||txid_current()::text`).Scan(&suffix) != nil {
			t.Fatal("principal fixture unique identity unavailable")
		}
		roles := SyncPrincipalRoleNames()
		for i := range roles {
			roles[i] += "_dry_" + suffix
		}
		defer cleanupSyncPrincipalCatalogRoles(t, tx, db, roles)
		// Existing fixed roles cannot exercise CREATE. Fresh suffixed identities
		// exercise that branch without altering any shared role; only catalog
		// names/connection are substituted in the unchanged production verifier.
		if testenv.EnsureSyncPrincipalRoles(ctx, tx, roles...) != nil {
			t.Fatal("principal fixture transactional creation refused")
		}
		st := &Store{pool: db, syncPrincipalTest: &syncPrincipalTestOptions{roles: roles, roleCatalog: tx}}
		if st.VerifySyncPrincipals(ctx) != nil {
			t.Fatal("principal fixture created attributes disagree with production verifier")
		}
	})
}
