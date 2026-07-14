package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/pressly/goose/v3"
)

var migrationTestSchemaSequence atomic.Uint64

func TestMigration28Postgres(t *testing.T) {
	baseDSN := os.Getenv("WITSELF_TEST_DATABASE_URL")
	if baseDSN == "" {
		t.Skip("WITSELF_TEST_DATABASE_URL is not set")
	}

	t.Run("fresh database applies every migration", func(t *testing.T) {
		st, dsn := newMigrationTestStore(t, baseDSN)
		if err := st.Migrate(); err != nil {
			t.Fatal(err)
		}
		assertMigrationTestVersion(t, dsn, 28)
		assertMigrationTestConstraint(t, st, "facts_owner_agent_id_subject_id_predicate_key", false)
	})

	t.Run("interrupted empty schema 26 install resumes", func(t *testing.T) {
		st, dsn := newMigrationTestStore(t, baseDSN)
		migrationTestUpTo(t, dsn, 26)
		if err := st.Migrate(); err != nil {
			t.Fatal(err)
		}
		assertMigrationTestVersion(t, dsn, 28)
	})

	t.Run("populated schema 26 cannot skip compatibility release", func(t *testing.T) {
		st, dsn := newMigrationTestStore(t, baseDSN)
		migrationTestUpTo(t, dsn, 26)
		if _, err := st.EnsureDefaultAccount(context.Background()); err != nil {
			t.Fatal(err)
		}
		err := st.Migrate()
		if !errors.Is(err, ErrMigrationCompatibilityRequired) {
			t.Fatalf("Migrate error = %v, want errors.Is(_, ErrMigrationCompatibilityRequired)", err)
		}
		assertMigrationTestVersion(t, dsn, 26)
		assertMigrationTestConstraint(t, st, "facts_owner_agent_id_subject_id_predicate_key", true)
		var deletionColumnExists bool
		if err := st.pool.QueryRow(context.Background(), `
			SELECT EXISTS (
			  SELECT 1 FROM pg_attribute
			  WHERE attrelid=to_regclass('facts') AND attname='deleted_at' AND NOT attisdropped
			)`).Scan(&deletionColumnExists); err != nil {
			t.Fatal(err)
		}
		if deletionColumnExists {
			t.Fatal("schema-27 deletion column was applied despite compatibility preflight refusal")
		}
	})

	t.Run("populated schema 27 proceeds", func(t *testing.T) {
		st, dsn := newMigrationTestStore(t, baseDSN)
		migrationTestUpTo(t, dsn, 27)
		if _, err := st.EnsureDefaultAccount(context.Background()); err != nil {
			t.Fatal(err)
		}
		if err := st.Migrate(); err != nil {
			t.Fatal(err)
		}
		assertMigrationTestVersion(t, dsn, 28)
		assertMigrationTestConstraint(t, st, "facts_owner_agent_id_subject_id_predicate_key", false)
	})

	t.Run("wrong-column legacy constraint is rejected", func(t *testing.T) {
		st, dsn := newMigrationTestStore(t, baseDSN)
		migrationTestUpTo(t, dsn, 27)
		if _, err := st.pool.Exec(context.Background(), `
			ALTER TABLE facts DROP CONSTRAINT facts_owner_agent_id_subject_id_predicate_key;
			ALTER TABLE facts ADD CONSTRAINT facts_owner_agent_id_subject_id_predicate_key
			UNIQUE (owner_agent_id, subject_id, created_at)`); err != nil {
			t.Fatal(err)
		}
		err := st.Migrate()
		if err == nil || !strings.Contains(err.Error(), "legacy full-address UNIQUE constraint") {
			t.Fatalf("Migrate error = %v, want strict legacy-constraint precondition", err)
		}
		assertMigrationTestVersion(t, dsn, 27)
		assertMigrationTestConstraint(t, st, "facts_owner_agent_id_subject_id_predicate_key", true)
	})

	t.Run("wrong-column partial index is rejected", func(t *testing.T) {
		st, dsn := newMigrationTestStore(t, baseDSN)
		migrationTestUpTo(t, dsn, 27)
		if _, err := st.pool.Exec(context.Background(), `
			DROP INDEX facts_one_active_address;
			CREATE UNIQUE INDEX facts_one_active_address
			ON facts (owner_agent_id, subject_id, created_at)
			WHERE deleted_at IS NULL`); err != nil {
			t.Fatal(err)
		}
		err := st.Migrate()
		if err == nil || !strings.Contains(err.Error(), "active-address partial UNIQUE index") {
			t.Fatalf("Migrate error = %v, want strict partial-index precondition", err)
		}
		assertMigrationTestVersion(t, dsn, 27)
		assertMigrationTestConstraint(t, st, "facts_owner_agent_id_subject_id_predicate_key", true)
	})

	t.Run("incomplete schema 27 deletion shape is rejected", func(t *testing.T) {
		st, dsn := newMigrationTestStore(t, baseDSN)
		migrationTestUpTo(t, dsn, 27)
		if _, err := st.pool.Exec(context.Background(), `
			ALTER TABLE facts DROP CONSTRAINT facts_replacement_shape`); err != nil {
			t.Fatal(err)
		}
		err := st.Migrate()
		if err == nil || !strings.Contains(err.Error(), "complete schema-27 fact-deletion shape") {
			t.Fatalf("Migrate error = %v, want complete schema-27 shape precondition", err)
		}
		assertMigrationTestVersion(t, dsn, 27)
		assertMigrationTestConstraint(t, st, "facts_owner_agent_id_subject_id_predicate_key", true)
	})

	t.Run("clean down restores legacy constraint and can re-upgrade", func(t *testing.T) {
		st, dsn := newMigrationTestStore(t, baseDSN)
		if err := st.Migrate(); err != nil {
			t.Fatal(err)
		}
		if err := migrationTestDown(t, dsn, false); err != nil {
			t.Fatal(err)
		}
		assertMigrationTestVersion(t, dsn, 27)
		assertMigrationTestConstraint(t, st, "facts_owner_agent_id_subject_id_predicate_key", true)
		if err := st.Migrate(); err != nil {
			t.Fatalf("re-upgrade schema 27 to 28: %v", err)
		}
		assertMigrationTestVersion(t, dsn, 28)
		assertMigrationTestConstraint(t, st, "facts_owner_agent_id_subject_id_predicate_key", false)
	})

	t.Run("down refuses duplicate recreated address without data loss", func(t *testing.T) {
		st, dsn := newMigrationTestStore(t, baseDSN)
		if err := st.Migrate(); err != nil {
			t.Fatal(err)
		}
		insertMigrationTestRecreatedAddress(t, st)
		err := migrationTestDown(t, dsn, true)
		if err == nil || !strings.Contains(err.Error(), "no fact rows were removed") {
			t.Fatalf("Down error = %v, want non-destructive duplicate refusal", err)
		}
		assertMigrationTestVersion(t, dsn, 28)
		assertMigrationTestConstraint(t, st, "facts_owner_agent_id_subject_id_predicate_key", false)
		var rows int
		if err := st.pool.QueryRow(context.Background(), `
			SELECT COUNT(*) FROM facts
			WHERE owner_agent_id='agent_migration' AND subject_id='sub_migration'
			  AND predicate='identity/name'`).Scan(&rows); err != nil {
			t.Fatal(err)
		}
		if rows != 2 {
			t.Fatalf("duplicate address rows after refused Down = %d, want 2", rows)
		}
	})
}

func newMigrationTestStore(t *testing.T, baseDSN string) (*Store, string) {
	t.Helper()
	schema := fmt.Sprintf("witself_migration_%d_%d", os.Getpid(), migrationTestSchemaSequence.Add(1))
	admin, err := sql.Open("pgx", baseDSN)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(`CREATE SCHEMA ` + schema); err != nil {
		_ = admin.Close()
		t.Fatalf("create test schema: %v", err)
	}
	dsn, err := migrationTestDSNWithSearchPath(baseDSN, schema)
	if err != nil {
		_, _ = admin.Exec(`DROP SCHEMA ` + schema + ` CASCADE`)
		_ = admin.Close()
		t.Fatal(err)
	}
	st, err := Open(context.Background(), dsn)
	if err != nil {
		_, _ = admin.Exec(`DROP SCHEMA ` + schema + ` CASCADE`)
		_ = admin.Close()
		t.Fatal(err)
	}
	if err := st.Ping(context.Background()); err != nil {
		st.Close()
		_, _ = admin.Exec(`DROP SCHEMA ` + schema + ` CASCADE`)
		_ = admin.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		st.Close()
		_, _ = admin.Exec(`DROP SCHEMA ` + schema + ` CASCADE`)
		_ = admin.Close()
	})
	return st, dsn
}

func migrationTestDSNWithSearchPath(baseDSN, schema string) (string, error) {
	if strings.HasPrefix(baseDSN, "postgres://") || strings.HasPrefix(baseDSN, "postgresql://") {
		u, err := url.Parse(baseDSN)
		if err != nil {
			return "", fmt.Errorf("parse PostgreSQL test URL: %w", err)
		}
		query := u.Query()
		query.Set("options", "-csearch_path="+schema)
		u.RawQuery = query.Encode()
		return u.String(), nil
	}
	return baseDSN + " options='-csearch_path=" + schema + "'", nil
}

func migrationTestUpTo(t *testing.T, dsn string, version int64) {
	t.Helper()
	db := migrationTestSQLDB(t, dsn)
	defer func() { _ = db.Close() }()
	if err := goose.UpTo(db, "migrations", version); err != nil {
		t.Fatalf("migrate test database to schema %d: %v", version, err)
	}
}

func migrationTestDown(t *testing.T, dsn string, wantError bool) error {
	t.Helper()
	db := migrationTestSQLDB(t, dsn)
	defer func() { _ = db.Close() }()
	err := goose.Down(db, "migrations")
	if !wantError && err != nil {
		t.Fatalf("migrate test database down: %v", err)
	}
	return err
}

func migrationTestSQLDB(t *testing.T, dsn string) *sql.DB {
	t.Helper()
	goose.SetBaseFS(migrationsFS)
	if err := goose.SetDialect("postgres"); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	return db
}

func assertMigrationTestVersion(t *testing.T, dsn string, want int64) {
	t.Helper()
	db := migrationTestSQLDB(t, dsn)
	defer func() { _ = db.Close() }()
	got, err := goose.GetDBVersion(db)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("database schema version = %d, want %d", got, want)
	}
}

func assertMigrationTestConstraint(t *testing.T, st *Store, name string, want bool) {
	t.Helper()
	var got bool
	if err := st.pool.QueryRow(context.Background(), `
		SELECT EXISTS (
		  SELECT 1 FROM pg_constraint
		  WHERE conrelid=to_regclass('facts') AND conname=$1
		)`, name).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("constraint %s exists = %t, want %t", name, got, want)
	}
}

func insertMigrationTestRecreatedAddress(t *testing.T, st *Store) {
	t.Helper()
	hashA := strings.Repeat("a", 64)
	hashB := strings.Repeat("b", 64)
	ctx := context.Background()
	tx, err := st.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	statements := []string{
		`INSERT INTO accounts (id, is_default, display_name)
		 VALUES ('acc_migration', true, 'migration test')`,
		`INSERT INTO realms (id, account_id, name)
		 VALUES ('realm_migration', 'acc_migration', 'default')`,
		`INSERT INTO agents (id, realm_id, name)
		 VALUES ('agent_migration', 'realm_migration', 'migration-agent')`,
		`INSERT INTO fact_subjects
		   (id, account_id, realm_id, owner_agent_id, canonical_key, display_name)
		 VALUES
		   ('sub_migration', 'acc_migration', 'realm_migration', 'agent_migration', 'person_spouse', 'Spouse')`,
		`INSERT INTO facts
		   (id, account_id, realm_id, owner_agent_id, subject_id, predicate)
		 VALUES
		   ('fact_migration_active', 'acc_migration', 'realm_migration', 'agent_migration', 'sub_migration', 'identity/name')`,
	}
	for _, statement := range statements {
		if _, err := tx.Exec(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO facts
		  (id, account_id, realm_id, owner_agent_id, subject_id, predicate,
		   deleted_at, deleted_by_agent_id, delete_receipt_id,
		   delete_idempotency_key_hash, deleted_prior_assertion_id,
		   deleted_assertion_count, deleted_candidate_revision,
		   recreated_at, replacement_fact_id)
		VALUES
		  ('fact_migration_deleted', 'acc_migration', 'realm_migration', 'agent_migration', 'sub_migration', 'identity/name',
		   clock_timestamp() - interval '1 second', 'agent_migration', 'fdel_migration',
		   $1, 'fas_migration_prior', 1, $2,
		   clock_timestamp(), 'fact_migration_active')`, hashA, hashB); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}
