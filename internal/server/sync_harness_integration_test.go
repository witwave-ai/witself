package server

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/witwave-ai/witself/internal/store"
	"github.com/witwave-ai/witself/internal/testenv"
)

// C has only this ordinary HTTP handler, not a store or replication adapter.
// All stores and W share one PostgreSQL failure domain; this proves isolation,
// not independent durability, capture, or synchronization.
func TestSyncHarnessOrdinaryClientRemoteOnly(t *testing.T) {
	t.Cleanup(func() {
		if !t.Failed() && !t.Skipped() {
			t.Log("POSTGRES_PASS: ordinary authenticated C changed only R; L1/L2/D/W byte snapshots unchanged; harness capture absent")
		}
	})
	t.Setenv("WITSELF_HOME", t.TempDir())
	t.Setenv("DSH_HOME", t.TempDir())
	dsn := testenv.RequirePostgres(t)
	if err := testenv.ValidateHarnessPostgresDSN(dsn); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	ddl, err := os.ReadFile("../store/testdata/sync_harness/schema_v1.sql")
	if err != nil {
		t.Fatal("read explicit harness schema failed")
	}
	type node struct {
		name    string
		store   *store.Store
		db      *sql.DB
		account string
	}
	nodes := make([]node, 0, 5)
	for _, name := range []string{"L1", "L2", "R", "D", "W"} {
		fixture := testenv.NewPostgresSchema(t, dsn)
		db, err := sql.Open("pgx", fixture.DSN())
		if err != nil {
			t.Fatal("open synthetic observer failed")
		}
		t.Cleanup(func() {
			if err := db.Close(); err != nil {
				t.Error("close synthetic observer failed")
			}
			if stats := db.Stats(); stats.InUse != 0 || stats.OpenConnections != 0 {
				t.Error("synthetic observer leaked connections")
			}
		})
		n := node{name: name, db: db}
		if name == "W" {
			if _, err := db.ExecContext(ctx, `CREATE TABLE h_witness_prefixes (
				account_id text NOT NULL, issuer_stream text NOT NULL,
				ordinal bigint NOT NULL CHECK (ordinal >= 0),
				PRIMARY KEY (account_id, issuer_stream))`); err != nil {
				t.Fatal("create synthetic witness failed")
			}
		} else {
			st, err := store.Open(ctx, fixture.DSN())
			if err != nil {
				t.Fatal("open synthetic node failed")
			}
			t.Cleanup(st.Close)
			if err := st.MigrateContext(ctx); err != nil {
				t.Fatal("migrate synthetic node failed")
			}
			var harnessTables int
			if err := db.QueryRowContext(ctx, `SELECT count(*) FROM pg_tables WHERE schemaname=current_schema() AND left(tablename,2)='h_'`).Scan(&harnessTables); err != nil || harnessTables != 0 {
				t.Fatal("ordinary store construction or migrations installed harness tables")
			}
			if _, err := db.ExecContext(ctx, string(ddl)); err != nil {
				t.Fatal("install explicit harness schema failed")
			}
			n.store = st
			n.account, err = st.EnsureDefaultAccount(ctx)
			if err != nil {
				t.Fatal("seed independent account failed")
			}
		}
		nodes = append(nodes, n)
	}
	remote := nodes[2]
	operator, err := remote.store.EnsureRootOperator(ctx, remote.account)
	if err != nil {
		t.Fatal("seed synthetic operator failed")
	}
	credential, _, _, err := remote.store.CreateOperatorToken(ctx, remote.account, operator, "synthetic C", nil)
	if err != nil {
		t.Fatal("mint synthetic C credential failed")
	}
	clientC := apiMux(Config{
		Authenticate: remote.store.AuthenticateOperator,
		CreateRealm: func(ctx context.Context, accountID, name string) (Realm, error) {
			r, err := remote.store.CreateRealm(ctx, accountID, name)
			if errors.Is(err, store.ErrRealmExists) {
				return Realm{}, ErrConflict
			}
			return Realm{ID: r.ID, Name: r.Name}, err
		},
	})
	before := make([]map[string][32]byte, len(nodes))
	for i, n := range nodes {
		before[i] = syncHarnessTableSnapshot(ctx, t, n.db)
	}
	request := func(token string) *httptest.ResponseRecorder {
		r := httptest.NewRequestWithContext(ctx, http.MethodPost, "/v1/realms", strings.NewReader(`{"name":"ordinary-client-C"}`))
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		w := httptest.NewRecorder()
		clientC.ServeHTTP(w, r)
		return w
	}
	for _, badCredential := range []string{"", "fixture-invalid"} {
		if got := request(badCredential).Code; got != http.StatusUnauthorized {
			t.Fatalf("ordinary authentication boundary returned %d, want 401", got)
		}
	}
	for i, n := range nodes {
		if !reflect.DeepEqual(before[i], syncHarnessTableSnapshot(ctx, t, n.db)) {
			t.Fatalf("unauthenticated request changed %s", n.name)
		}
	}
	w := request(credential)
	if w.Code != http.StatusCreated || w.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("ordinary C write returned status %d; want 201 JSON", w.Code)
	}
	realms, err := remote.store.ListRealms(ctx, remote.account)
	if err != nil || len(realms) != 1 || realms[0].Name != "ordinary-client-C" {
		t.Fatal("ordinary C write did not persist exactly one realm on R")
	}
	// Exact pre-harness route response contract, with its database-created ID.
	want, err := json.Marshal(map[string]any{
		"schema_version": "witself.v0", "realm": map[string]string{"id": realms[0].ID, "name": realms[0].Name},
	})
	if err != nil || w.Body.String() != string(want)+"\n" {
		t.Fatal("ordinary hosted realm response bytes changed")
	}
	for i, n := range nodes {
		after := syncHarnessTableSnapshot(ctx, t, n.db)
		if n.name != "R" {
			if !reflect.DeepEqual(before[i], after) {
				t.Fatalf("ordinary C write changed disconnected %s", n.name)
			}
			continue
		}
		if reflect.DeepEqual(before[i], after) {
			t.Fatal("ordinary C write did not change R")
		}
		for table, snapshot := range before[i] {
			if strings.HasPrefix(table, "h_") && snapshot != after[table] {
				t.Fatal("ordinary C write activated harness capture")
			}
		}
		for table, snapshot := range after {
			prior, existed := before[i][table]
			if strings.HasPrefix(table, "h_") && (!existed || prior != snapshot) {
				t.Fatal("ordinary C write added or changed harness state")
			}
		}
	}
}

// These digests remain private in memory. Failure diagnostics name only the
// node, never fixture credentials, table contents, DSNs, or digest values.
func syncHarnessTableSnapshot(ctx context.Context, t *testing.T, db *sql.DB) map[string][32]byte {
	t.Helper()
	rows, err := db.QueryContext(ctx, `SELECT tablename FROM pg_tables WHERE schemaname=current_schema() ORDER BY tablename`)
	if err != nil {
		t.Fatal("enumerate synthetic tables failed")
	}
	defer func() { _ = rows.Close() }()
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal("read synthetic table name failed")
		}
		names = append(names, name)
	}
	if err := rows.Close(); err != nil {
		t.Fatal("close synthetic table enumeration failed")
	}
	if err := rows.Err(); err != nil {
		t.Fatal("enumerate synthetic tables failed")
	}
	result := make(map[string][32]byte, len(names))
	for _, name := range names {
		var data string
		query := `SELECT COALESCE(jsonb_agg(to_jsonb(t) ORDER BY to_jsonb(t)::text), '[]'::jsonb)::text FROM ` + pgx.Identifier{name}.Sanitize() + ` t`
		if err := db.QueryRowContext(ctx, query).Scan(&data); err != nil {
			t.Fatal("snapshot synthetic table failed")
		}
		result[name] = sha256.Sum256([]byte(data))
	}
	return result
}
