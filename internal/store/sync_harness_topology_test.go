package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/witwave-ai/witself/internal/syncharness"
	"github.com/witwave-ai/witself/internal/testenv"
)

const syncHarnessAccount = "h_account_shared"

var syncHarnessNodeIDs = []syncharness.NodeID{syncharness.L1, syncharness.L2, syncharness.R, syncharness.D}

type syncHarnessNode struct {
	id     syncharness.NodeID
	schema *testenv.PostgresSchema
	store  *Store
}

type syncHarnessTopology struct {
	nodes     []*syncHarnessNode
	witness   *testenv.PostgresSchema
	witnessDB *sql.DB
	transport *syncharness.Transport
	closed    bool
}

// The shared database is one physical failure domain. W is only a synthetic
// witness outside the nodes' modeled restore sets, not independent durability.
func newSyncHarnessTopology(t *testing.T) *syncHarnessTopology {
	t.Helper()
	t.Setenv("WITSELF_HOME", t.TempDir())
	t.Setenv("DSH_HOME", t.TempDir())
	dsn := testenv.RequirePostgres(t)
	if err := testenv.ValidateHarnessPostgresDSN(dsn); err != nil {
		t.Fatal(err)
	}
	ddl, err := os.ReadFile("testdata/sync_harness/schema_v1.sql")
	if err != nil {
		t.Fatal("read harness-only schema failed")
	}
	top := &syncHarnessTopology{transport: syncharness.NewTransport(syncharness.L1, syncharness.L2, syncharness.R, syncharness.D, syncharness.W, syncharness.C)}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	for _, id := range syncHarnessNodeIDs {
		fixture := testenv.NewPostgresSchema(t, dsn)
		st, err := Open(context.Background(), fixture.DSN())
		if err != nil {
			t.Fatal("open harness node failed")
		}
		t.Cleanup(func() { closeSyncHarnessStore(t, st) })
		if err := st.Ping(context.Background()); err != nil {
			t.Fatal("ping harness node failed")
		}
		// Allocation does not migrate. Apply the current embedded store schema
		// explicitly, then install the separately opted-in miniature tables.
		if err := st.MigrateContext(ctx); err != nil {
			t.Fatal("migrate harness node failed")
		}
		if _, err := st.pool.Exec(context.Background(), string(ddl)); err != nil {
			t.Fatal("install harness-only tables failed")
		}
		node := &syncHarnessNode{id: id, schema: fixture, store: st}
		seedSyncHarnessNode(t, node)
		top.nodes = append(top.nodes, node)
	}
	top.witness = testenv.NewPostgresSchema(t, dsn)
	top.witnessDB, err = sql.Open("pgx", top.witness.DSN())
	if err != nil {
		t.Fatal("open synthetic witness failed")
	}
	t.Cleanup(func() { _ = top.witnessDB.Close() })
	if _, err := top.witnessDB.Exec(`CREATE TABLE h_witness_prefixes (
		account_id text NOT NULL, issuer_stream text NOT NULL,
		ordinal bigint NOT NULL CHECK (ordinal >= 0),
		PRIMARY KEY(account_id, issuer_stream))`); err != nil {
		t.Fatal("install synthetic witness failed")
	}
	t.Cleanup(func() { top.close(t) })
	return top
}

func seedSyncHarnessNode(t *testing.T, node *syncHarnessNode) {
	t.Helper()
	// The common logical namespace is intentional for the disconnected tests.
	// Physical schemas, realms, agents and adversary accounts remain independent.
	for _, account := range []string{syncHarnessAccount, "h_adversary_" + string(node.id)} {
		realm := account + "_realm_" + string(node.id)
		agent := account + "_agent_" + string(node.id)
		for _, statement := range []struct {
			sql  string
			args []any
		}{
			{`INSERT INTO accounts(id,display_name) VALUES($1,'synthetic harness')`, []any{account}},
			{`INSERT INTO realms(id,account_id,name) VALUES($1,$2,'synthetic')`, []any{realm, account}},
			{`INSERT INTO agents(id,realm_id,name) VALUES($1,$2,'synthetic')`, []any{agent, realm}},
		} {
			if _, err := node.store.pool.Exec(context.Background(), statement.sql, statement.args...); err != nil {
				t.Fatal("seed synthetic principal failed")
			}
		}
	}
}

func closeSyncHarnessStore(t *testing.T, st *Store) {
	t.Helper()
	if got := st.pool.Stat().AcquiredConns(); got != 0 {
		t.Errorf("harness pool has %d acquired connections before close", got)
	}
	st.Close()
	if got := st.pool.Stat().TotalConns(); got != 0 {
		t.Errorf("harness pool has %d connections after close", got)
	}
}

func (top *syncHarnessTopology) close(t *testing.T) {
	t.Helper()
	if top.closed {
		return
	}
	top.closed = true
	for _, node := range top.nodes {
		closeSyncHarnessStore(t, node.store)
		if err := node.schema.Close(); err != nil {
			t.Error(err)
		}
	}
	if stats := top.witnessDB.Stats(); stats.InUse != 0 {
		t.Errorf("witness pool has %d acquired connections before close", stats.InUse)
	}
	if err := top.witnessDB.Close(); err != nil {
		t.Error("close witness pool failed")
	}
	if stats := top.witnessDB.Stats(); stats.InUse != 0 || stats.OpenConnections != 0 {
		t.Error("witness pool connections remain after close")
	}
	if err := top.witness.Close(); err != nil {
		t.Error(err)
	}
	// Each Close verified pg_namespace absence before closing its admin pool.
}

func syncHarnessWrite(t *testing.T, node *syncHarnessNode, account, object string) {
	t.Helper()
	if _, err := node.store.pool.Exec(context.Background(), `INSERT INTO h_fixture_objects(account_id,object_id,owner_node,value) VALUES($1,$2,$3,'synthetic')`, account, object, string(node.id)); err != nil {
		t.Fatal("write synthetic sentinel failed")
	}
}

// disconnectedViolation is an independent observation oracle. In particular it
// checks all raw fixture rows, not just the rows selected by a transport adapter.
func (top *syncHarnessTopology) disconnectedViolation(ctx context.Context) error {
	for _, node := range top.nodes {
		want := 1
		if node.id == syncharness.D {
			want = 0
		}
		var total, own int
		if err := node.store.pool.QueryRow(ctx, `SELECT count(*), count(*) FILTER (WHERE account_id=$1 AND owner_node=$2 AND object_id=$3) FROM h_fixture_objects`, syncHarnessAccount, string(node.id), "sentinel_"+string(node.id)).Scan(&total, &own); err != nil {
			return errors.New("sentinel observation failed")
		}
		if total != want || own != want {
			return errors.New("disconnected sentinel crossed a node boundary")
		}
		for _, table := range []string{"h_receipts", "h_replica_cursors", "h_replica_status", "h_provider_actions", "h_logical_actions", "h_rollups", "account_events", "usage_events", "usage_rollups"} {
			var count int
			if err := node.store.pool.QueryRow(ctx, `SELECT count(*) FROM `+table).Scan(&count); err != nil {
				return errors.New("disconnected counter observation failed")
			}
			if count != 0 {
				return fmt.Errorf("disconnected fixture changed %s", table)
			}
		}
	}
	var witnessCount int
	if err := top.witnessDB.QueryRowContext(ctx, `SELECT count(*) FROM h_witness_prefixes`).Scan(&witnessCount); err != nil || witnessCount != 0 {
		return errors.New("synthetic witness changed")
	}
	return nil
}

func TestSyncHarnessDisconnectedBaseline(t *testing.T) {
	t.Cleanup(func() {
		if !t.Failed() && !t.Skipped() {
			t.Log("POSTGRES_PASS: disconnected L1/L2/R sentinels with nonempty local capture on L1/R, empty D, zero receipts/cursors/status/actions/rollups, untouched W; five schemas dropped and consumer/administrator pools closed")
		}
	})
	top := newSyncHarnessTopology(t)
	for _, from := range []syncharness.NodeID{syncharness.L1, syncharness.L2, syncharness.R, syncharness.D, syncharness.W, syncharness.C} {
		for _, to := range []syncharness.NodeID{syncharness.L1, syncharness.L2, syncharness.R, syncharness.D, syncharness.W, syncharness.C} {
			if top.transport.EdgeEnabled(from, to) {
				t.Fatal("new topology has an enabled transport edge")
			}
		}
	}
	if top.transport.HasAdapter() {
		t.Fatal("new topology has a replication adapter")
	}
	if err := top.transport.Deliver(syncharness.Delivery{From: syncharness.L1, To: syncharness.R, Operation: "sentinel"}); !errors.Is(err, syncharness.ErrCapabilityMissing) {
		t.Fatal("missing adapter did not return a harness capability error")
	}
	keys := newSyncHarnessKeys()
	for _, node := range top.nodes[:3] {
		if node.id == syncharness.L2 {
			syncHarnessWrite(t, node, syncHarnessAccount, "sentinel_"+string(node.id))
			continue
		}
		seedSyncHarnessSharedAdmission(t, node, keys)
		seedSyncHarnessCapture(t, node)
		if err := syncHarnessLocalCapture(context.Background(), node, keys, syncHarnessEnvelope(syncHarnessAccount), "sentinel_"+string(node.id)); err != nil {
			t.Fatal(err)
		}
	}
	if err := top.disconnectedViolation(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, node := range top.nodes {
		want := 0
		if node.id == syncharness.L1 || node.id == syncharness.R {
			want = 1
		}
		for _, table := range []string{"h_operations", "h_operation_payloads", "h_bundles", "h_bundle_operations"} {
			if syncHarnessCount(t, node, table) != want {
				t.Fatal("local capture crossed disconnected node boundary")
			}
		}
		if syncHarnessCount(t, node, "h_publication_outbox") != 0 || syncHarnessCount(t, node, "h_inbound") != 0 || syncHarnessCount(t, node, "h_replay_work") != 0 {
			t.Fatal("local capture created remote publication or replay state")
		}
		var nonprovisional int
		if err := node.store.pool.QueryRow(context.Background(), `SELECT count(*) FROM h_operations WHERE admission_state<>'admitted' OR receipt_ref IS NOT NULL`).Scan(&nonprovisional); err != nil || nonprovisional != 0 {
			t.Fatal("local capture claimed destination acceptance")
		}
	}
	top.close(t)
}

// syncHarnessCopyAdapter is deliberately faulty: it bypasses all protocol and
// authority checks and copies one miniature row. It exists only to challenge the
// disconnected observation oracle, and is never installed by normal fixtures.
type syncHarnessCopyAdapter struct{ source, destination *Store }

func (adapter syncHarnessCopyAdapter) Deliver(delivery syncharness.Delivery) error {
	if delivery.From != syncharness.L1 || delivery.To != syncharness.L2 || delivery.Operation != "malicious_copy" {
		return errors.New("unsupported malicious fixture delivery")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	var account, object, owner, value string
	if err := adapter.source.pool.QueryRow(ctx, `SELECT account_id,object_id,owner_node,value FROM h_fixture_objects`).Scan(&account, &object, &owner, &value); err != nil {
		return errors.New("read mutation source failed")
	}
	if _, err := adapter.destination.pool.Exec(ctx, `INSERT INTO h_fixture_objects(account_id,object_id,owner_node,value) VALUES($1,$2,$3,$4)`, account, object, owner, value); err != nil {
		return errors.New("inject malicious copy failed")
	}
	return nil
}

func TestSyncHarnessDisconnectedBaselineRejectsCopy(t *testing.T) {
	t.Cleanup(func() {
		if !t.Failed() && !t.Skipped() {
			t.Log("POSTGRES_PASS: malicious cross-node sentinel copy rejected by disconnected oracle; five schemas dropped and consumer/administrator pools closed")
		}
	})
	top := newSyncHarnessTopology(t)
	for _, node := range top.nodes[:3] {
		syncHarnessWrite(t, node, syncHarnessAccount, "sentinel_"+string(node.id))
	}
	if err := top.disconnectedViolation(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Copy an actual source row through the explicit malicious test adapter.
	// The independent observer must reject the resulting formerly disconnected
	// topology even though the adapter and transport themselves returned success.
	top.transport.InstallAdapter(syncHarnessCopyAdapter{source: top.nodes[0].store, destination: top.nodes[1].store})
	if err := top.transport.SetEdge(syncharness.L1, syncharness.L2, true); err != nil {
		t.Fatal("enable malicious fixture edge failed")
	}
	if err := top.transport.Deliver(syncharness.Delivery{From: syncharness.L1, To: syncharness.L2, Operation: "malicious_copy"}); err != nil {
		t.Fatal(err)
	}
	if err := top.disconnectedViolation(context.Background()); err == nil {
		t.Fatal("isolation observer accepted malicious cross-node copy")
	}
}

func TestSyncHarnessAdversaryAccountIsolation(t *testing.T) {
	t.Cleanup(func() {
		if !t.Failed() && !t.Skipped() {
			t.Log("POSTGRES_PASS: adversary account write visible only in its own L1 namespace; five schemas dropped and consumer/administrator pools closed")
		}
	})
	top := newSyncHarnessTopology(t)
	syncHarnessWrite(t, top.nodes[0], "h_adversary_L1", "adversary_sentinel")
	for _, node := range top.nodes {
		var logicalCount, allCount int
		if err := node.store.pool.QueryRow(context.Background(), `SELECT count(*) FILTER(WHERE account_id=$1), count(*) FROM h_fixture_objects`, syncHarnessAccount).Scan(&logicalCount, &allCount); err != nil {
			t.Fatal("observe account isolation failed")
		}
		wantAll := 0
		if node.id == syncharness.L1 {
			wantAll = 1
		}
		if logicalCount != 0 || allCount != wantAll {
			t.Fatal("adversary write escaped its account or physical schema")
		}
	}
}

func syncHarnessLockNamespace(node *syncHarnessNode, account string) string {
	return fmt.Sprintf("syncharness/v1/%s/%s/%d:%s", node.schema.Name(), node.id, len(account), account)
}

func TestSyncHarnessAdvisoryLocksUsePhysicalNodeAndAccount(t *testing.T) {
	t.Cleanup(func() {
		if !t.Failed() && !t.Skipped() {
			t.Log("POSTGRES_PASS: same node/account serializes; physical node and adversary account locks remain independent; five schemas dropped and consumer/administrator pools closed")
		}
	})
	top := newSyncHarnessTopology(t)
	ctx := context.Background()
	var held []pgx.Tx
	t.Cleanup(func() {
		for _, tx := range held {
			_ = tx.Rollback(ctx)
		}
	})
	for _, tc := range []struct {
		node    *syncHarnessNode
		account string
		want    bool
	}{
		{top.nodes[0], syncHarnessAccount, true},
		{top.nodes[0], syncHarnessAccount, false},
		{top.nodes[1], syncHarnessAccount, true},
		{top.nodes[0], "h_adversary_L1", true},
	} {
		tx, err := tc.node.store.pool.Begin(ctx)
		if err != nil {
			t.Fatal("open fixture lock transaction failed")
		}
		held = append(held, tx)
		var acquired bool
		if err := tx.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock(hashtextextended($1,0))`, syncHarnessLockNamespace(tc.node, tc.account)).Scan(&acquired); err != nil {
			t.Fatal("observe fixture advisory lock failed")
		}
		if acquired != tc.want {
			t.Fatal("fixture lock namespace did not isolate node/account")
		}
	}
}
