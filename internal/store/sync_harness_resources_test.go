package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/witwave-ai/witself/internal/syncmodel"
)

func syncHarnessBoundedEnvelope(e syncmodel.OperationEnvelope) ([]byte, error) {
	if len(e.Basis.Dependencies) > syncmodel.MaxDependencies || e.Compound != nil && (len(e.Compound.RequiredFragments) > syncmodel.MaxFragments || len(e.Compound.OrderedMembers) > syncmodel.MaxFragments) {
		return nil, syncmodel.ErrResourceCapacity
	}
	raw, err := json.Marshal(e)
	if err != nil {
		return nil, errors.New("invalid capture envelope")
	}
	if len(raw) > syncmodel.MaxEnvelopeBytes {
		return nil, syncmodel.ErrResourceCapacity
	}
	return e.CanonicalBytes()
}

// The fixture schema is the node namespace; the row is the account namespace.
// The lock is held through insertion and commit (including a no-row first use).
func syncHarnessLockResourceBudget(ctx context.Context, tx pgx.Tx, account string) (int, int, error) {
	if _, err := tx.Exec(ctx, `INSERT INTO h_resource_budgets(account_id) VALUES($1) ON CONFLICT DO NOTHING`, account); err != nil {
		return 0, 0, errors.New("initialize capture capacity failed")
	}
	var pending, retained int
	if err := tx.QueryRow(ctx, `SELECT pending_operations,retained_bytes FROM h_resource_budgets WHERE account_id=$1 FOR UPDATE`, account).Scan(&pending, &retained); err != nil {
		return 0, 0, errors.New("lock capture capacity failed")
	}
	return pending, retained, nil
}

func syncHarnessReserveFragmentBytes(ctx context.Context, tx pgx.Tx, account string, operation []byte, size int) error {
	_, retained, err := syncHarnessLockResourceBudget(ctx, tx, account)
	if err != nil {
		return err
	}
	if size < 0 || size > syncmodel.MaxTransportBytes-retained {
		return syncmodel.ErrResourceCapacity
	}
	result, err := tx.Exec(ctx, `UPDATE h_resource_reservations SET fragment_bytes=fragment_bytes+$3 WHERE account_id=$1 AND operation_id=$2`, account, operation, size)
	if err != nil || result.RowsAffected() != 1 {
		return errors.New("fragment reservation unavailable")
	}
	if _, err = tx.Exec(ctx, `UPDATE h_resource_budgets SET retained_bytes=retained_bytes+$2 WHERE account_id=$1`, account, size); err != nil {
		return errors.New("reserve fragment capacity failed")
	}
	return nil
}

func syncHarnessCapacityEnvelope(t *testing.T, account string, i, size int) syncmodel.OperationEnvelope {
	t.Helper()
	e := syncHarnessEnvelope(account)
	e.ID = syncmodel.OperationID{byte(i), byte(i >> 8), 91}
	e.Origin.OriginSequence = syncmodel.Ordinal(i + 1)
	e.Basis.ObjectID = fmt.Sprintf("capacity_%d", i)
	if size > 0 {
		e.Payload = syncmodel.StringValue("")
		raw, err := json.Marshal(e)
		if err != nil || len(raw) > size {
			t.Fatal("capacity fixture size too small")
		}
		e.Payload = syncmodel.StringValue(strings.Repeat("x", size-len(raw)))
		raw, err = json.Marshal(e)
		if err != nil || len(raw) != size {
			t.Fatal("capacity fixture size mismatch")
		}
	}
	return e
}

func syncHarnessAssertResourceUsage(t *testing.T, node *syncHarnessNode, account string, wantCount, wantBytes int) {
	t.Helper()
	var count, size, payloadCount, payloadSize int
	if err := node.store.pool.QueryRow(context.Background(), `SELECT COALESCE((SELECT pending_operations FROM h_resource_budgets WHERE account_id=$1),0),COALESCE((SELECT retained_bytes FROM h_resource_budgets WHERE account_id=$1),0),(SELECT count(*) FROM h_operation_payloads WHERE account_id=$1),(SELECT COALESCE(sum(octet_length(canonical_semantic_envelope)),0) FROM h_operation_payloads WHERE account_id=$1)`, account).Scan(&count, &size, &payloadCount, &payloadSize); err != nil {
		t.Fatal("observe capacity failed")
	}
	if count != wantCount || size != wantBytes || payloadCount != wantCount || payloadSize != wantBytes {
		t.Fatalf("capacity counts/bytes differ: reservation %d/%d payload %d/%d want %d/%d", count, size, payloadCount, payloadSize, wantCount, wantBytes)
	}
}

func syncHarnessEnvelopeBoundInput(t *testing.T, account, kind string, index, delta int) syncmodel.OperationEnvelope {
	t.Helper()
	e := syncHarnessCapacityEnvelope(t, account, index, 0)
	switch kind {
	case "envelope":
		e = syncHarnessCapacityEnvelope(t, account, index, syncmodel.MaxEnvelopeBytes+delta)
	case "dependencies":
		for i := 0; i < syncmodel.MaxDependencies+delta; i++ {
			e.Basis.Dependencies = append(e.Basis.Dependencies, syncmodel.Dependency{Kind: "generation", AccountID: account, RealmID: e.Authorization.RealmID, OwnerID: e.Authorization.PrincipalID, ObjectID: fmt.Sprintf("dep_%d", i), Generation: syncmodel.GenerationID{1}})
		}
	case "fragments":
		m := newSyncHarnessCompound(fmt.Sprintf("bound_%d", index), 40).manifest
		m.OrderedMembers = nil
		m.RequiredFragments = nil
		for i := 0; i < syncmodel.MaxFragments+delta; i++ {
			name := fmt.Sprintf("fragment_%02d", i)
			m.OrderedMembers = append(m.OrderedMembers, name)
			m.RequiredFragments = append(m.RequiredFragments, name)
		}
		e.Compound = &m
	default:
		t.Fatal("unknown envelope bound fixture")
	}
	return e
}

// Compare the actual retained bytes and every operation/reservation field without
// rendering value-bearing rows in test diagnostics. Exclusion is only used for
// the one expected new admission in the concurrent schedule.
func syncHarnessCaptureQueueSnapshot(t *testing.T, node *syncHarnessNode, account string, exclude []byte) []byte {
	t.Helper()
	var snapshot []byte
	err := node.store.pool.QueryRow(context.Background(), `SELECT jsonb_build_object(
 'operations',(SELECT COALESCE(jsonb_agg(to_jsonb(o) ORDER BY operation_id),'[]') FROM h_operations o WHERE account_id=$1 AND ($2::bytea IS NULL OR operation_id<>$2)),
 'reservations',(SELECT COALESCE(jsonb_agg(to_jsonb(r) ORDER BY operation_id),'[]') FROM h_resource_reservations r WHERE account_id=$1 AND ($2::bytea IS NULL OR operation_id<>$2)),
 'payloads',(SELECT COALESCE(jsonb_agg(to_jsonb(p) ORDER BY operation_id),'[]') FROM h_operation_payloads p WHERE account_id=$1 AND ($2::bytea IS NULL OR operation_id<>$2)))`, account, exclude).Scan(&snapshot)
	if err != nil {
		t.Fatal("snapshot queued capture state failed")
	}
	return snapshot
}

func syncHarnessAssertCaptureAbsent(t *testing.T, node *syncHarnessNode, e syncmodel.OperationEnvelope) {
	t.Helper()
	var rows int
	err := node.store.pool.QueryRow(context.Background(), `SELECT
 (SELECT count(*) FROM h_operations WHERE account_id=$1 AND operation_id=$2)+
 (SELECT count(*) FROM h_operation_payloads WHERE account_id=$1 AND operation_id=$2)+
 (SELECT count(*) FROM h_resource_reservations WHERE account_id=$1 AND operation_id=$2)+
 (SELECT count(*) FROM h_bundle_operations WHERE account_id=$1 AND operation_id=$2)+
 (SELECT count(*) FROM h_fixture_objects WHERE account_id=$1 AND object_id=$3)`, e.OriginalIncarnation.AccountID, e.ID[:], e.Basis.ObjectID).Scan(&rows)
	if err != nil || rows != 0 {
		t.Fatal("refused capture left durable operation, payload, reservation, bundle or mutation")
	}
}

func TestSyncHarnessSQLCaptureEnvelopeBounds(t *testing.T) {
	top := newSyncHarnessTopology(t)
	node := top.nodes[2]
	keys := newSyncHarnessKeys()
	seedSyncHarnessAdmission(t, node, keys)
	seedSyncHarnessCapture(t, node)
	seedSyncHarnessCaptureAccount(t, node, "h_adversary_R")
	other := top.nodes[0]
	seedSyncHarnessSharedAdmission(t, other, keys)
	seedSyncHarnessCapture(t, other)
	ctx := context.Background()
	count, total := 0, 0
	otherAccountBytes, otherNodeBytes := 0, 0
	for kindIndex, kind := range []string{"envelope", "dependencies", "fragments"} {
		t.Run(kind, func(t *testing.T) {
			for _, delta := range []int{-1, 0, 1} {
				e := syncHarnessEnvelopeBoundInput(t, syncHarnessAccount, kind, kindIndex*10+delta+2, delta)
				before := syncHarnessCaptureQueueSnapshot(t, node, syncHarnessAccount, nil)
				err := syncHarnessLocalCapture(ctx, node, keys, e, e.Basis.ObjectID)
				if delta <= 0 {
					if err != nil {
						t.Fatal(err)
					}
					raw, err := e.CanonicalBytes()
					if err != nil {
						t.Fatal("encode admitted fixture failed")
					}
					count++
					total += len(raw)
				} else if !errors.Is(err, syncmodel.ErrResourceCapacity) {
					t.Fatal("SQL bound did not explicitly refuse")
				}
				if delta > 0 {
					if !bytes.Equal(before, syncHarnessCaptureQueueSnapshot(t, node, syncHarnessAccount, nil)) {
						t.Fatal("SQL bound refusal changed queued operations, reservations or payload bytes")
					}
					syncHarnessAssertCaptureAbsent(t, node, e)
				}
				syncHarnessAssertResourceUsage(t, node, syncHarnessAccount, count, total)
			}
			// Reuse the refused operation ID at exactly N in each independent
			// scope: primary refusal must not poison either account or node.
			before := syncHarnessCaptureQueueSnapshot(t, node, syncHarnessAccount, nil)
			for _, control := range []struct {
				name    string
				node    *syncHarnessNode
				account string
				total   *int
			}{
				{"other_account", node, "h_adversary_R", &otherAccountBytes},
				{"other_node", other, syncHarnessAccount, &otherNodeBytes},
			} {
				t.Run(control.name, func(t *testing.T) {
					e := syncHarnessEnvelopeBoundInput(t, control.account, kind, kindIndex*10+3, 0)
					if err := syncHarnessLocalCapture(ctx, control.node, keys, e, e.Basis.ObjectID); err != nil {
						t.Fatal("envelope bound refusal crossed account or node")
					}
					raw, err := e.CanonicalBytes()
					if err != nil {
						t.Fatal("encode boundary control failed")
					}
					*control.total += len(raw)
					syncHarnessAssertResourceUsage(t, control.node, control.account, kindIndex+1, *control.total)
					if !bytes.Equal(before, syncHarnessCaptureQueueSnapshot(t, node, syncHarnessAccount, nil)) {
						t.Fatal("independent boundary admission changed primary queued work")
					}
					syncHarnessAssertResourceUsage(t, node, syncHarnessAccount, count, total)
				})
			}
		})
	}
	if !t.Failed() {
		t.Log("POSTGRES_PASS: SQL capture envelope/dependency/fragment N-1/N/N+1 bounds preserve queued bytes and account/node boundary isolation")
	}
}

func syncHarnessWaitForCaptureGateContenders(ctx context.Context, t *testing.T, node *syncHarnessNode, holder int32) {
	t.Helper()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		var waiters []int32
		// The holder owns only this fixture's schema/account capture gate.
		// Match its complete advisory lock identity, rather than observing
		// arbitrary shared-server sessions or assuming goroutine timing.
		err := node.store.pool.QueryRow(ctx, `SELECT ARRAY(
 SELECT DISTINCT waiting.pid FROM pg_locks held JOIN pg_locks waiting
 ON waiting.locktype=held.locktype AND waiting.database=held.database
 AND waiting.classid=held.classid AND waiting.objid=held.objid AND waiting.objsubid=held.objsubid
 WHERE held.pid=$1 AND held.locktype='advisory' AND held.mode='ExclusiveLock' AND held.granted
 AND waiting.pid<>held.pid AND waiting.mode='ShareLock' AND NOT waiting.granted
 ORDER BY waiting.pid)`, holder).Scan(&waiters)
		if err != nil {
			t.Fatal("observe fixture capture gate waiters failed")
		}
		if len(waiters) == 2 {
			return
		}
		if len(waiters) > 2 {
			t.Fatal("unexpected contenders on fixture-owned capture gate")
		}
		select {
		case <-ctx.Done():
			t.Fatal("both fixture capture gate waits were not observed")
		case <-ticker.C:
		}
	}
}

func TestSyncHarnessSQLCaptureEnvelopeBoundsConcurrentAdmission(t *testing.T) {
	for _, kind := range []string{"envelope", "dependencies", "fragments"} {
		t.Run(kind, func(t *testing.T) {
			top := newSyncHarnessTopology(t)
			node := top.nodes[2]
			keys := newSyncHarnessKeys()
			seedSyncHarnessAdmission(t, node, keys)
			seedSyncHarnessCapture(t, node)
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			queued := syncHarnessCapacityEnvelope(t, syncHarnessAccount, 90, 0)
			if err := syncHarnessLocalCapture(ctx, node, keys, queued, queued.Basis.ObjectID); err != nil {
				t.Fatal("seed queued capture before boundary race failed")
			}
			before := syncHarnessCaptureQueueSnapshot(t, node, syncHarnessAccount, nil)
			boundary := syncHarnessEnvelopeBoundInput(t, syncHarnessAccount, kind, 91, 0)
			crossing := syncHarnessEnvelopeBoundInput(t, syncHarnessAccount, kind, 92, 1)

			// These are per-input limits, so N+1 refuses before the resource
			// budget lock. Use the real capture-admission gate, which both
			// actual local-capture calls enter before envelope preflight.
			// Named barrier: capture_bounds_<kind>_both_waiting.
			gate, err := node.store.pool.Begin(ctx)
			if err != nil {
				t.Fatal("begin capture boundary barrier failed")
			}
			defer func() { _ = gate.Rollback(context.Background()) }()
			var holder int32
			if err := gate.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&holder); err != nil {
				t.Fatal("observe capture boundary barrier session failed")
			}
			if _, err := gate.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended(current_schema()||chr(31)||$1||chr(31)||'capture',0))`, syncHarnessAccount); err != nil {
				t.Fatal("lock fixture capture boundary barrier failed")
			}
			type outcome struct {
				crossing bool
				err      error
			}
			done := make(chan outcome, 2)
			go func() {
				done <- outcome{err: syncHarnessLocalCapture(ctx, node, keys, boundary, boundary.Basis.ObjectID)}
			}()
			go func() {
				done <- outcome{crossing: true, err: syncHarnessLocalCapture(ctx, node, keys, crossing, crossing.Basis.ObjectID)}
			}()
			syncHarnessWaitForCaptureGateContenders(ctx, t, node, holder)
			t.Logf("barrier capture_bounds_%s_both_waiting: both fixture-owned advisory lock waits observed", kind)
			if !bytes.Equal(before, syncHarnessCaptureQueueSnapshot(t, node, syncHarnessAccount, nil)) {
				t.Fatal("capture barrier changed queued work before release")
			}
			if err := gate.Commit(ctx); err != nil {
				t.Fatal("release capture boundary barrier failed")
			}
			admitted, refused := 0, 0
			for range 2 {
				result := <-done
				if result.crossing {
					if !errors.Is(result.err, syncmodel.ErrResourceCapacity) {
						t.Fatal("concurrent N+1 capture did not explicitly refuse")
					}
					refused++
				} else {
					if result.err != nil {
						t.Fatal("concurrent N capture did not commit")
					}
					admitted++
				}
			}
			if admitted != 1 || refused != 1 {
				t.Fatal("concurrent capture boundary outcome count differs")
			}
			if !bytes.Equal(before, syncHarnessCaptureQueueSnapshot(t, node, syncHarnessAccount, boundary.ID[:])) {
				t.Fatal("concurrent capture boundary changed prior queued operation, reservation or payload")
			}
			syncHarnessAssertCaptureAbsent(t, node, crossing)
			queuedBytes, err := queued.CanonicalBytes()
			if err != nil {
				t.Fatal("encode queued capture fixture failed")
			}
			boundaryBytes, err := boundary.CanonicalBytes()
			if err != nil {
				t.Fatal("encode boundary capture fixture failed")
			}
			var retained []byte
			if err := node.store.pool.QueryRow(ctx, `SELECT canonical_semantic_envelope FROM h_operation_payloads WHERE account_id=$1 AND operation_id=$2`, syncHarnessAccount, boundary.ID[:]).Scan(&retained); err != nil || !bytes.Equal(retained, boundaryBytes) {
				t.Fatal("concurrent boundary admission did not retain exact envelope bytes")
			}
			syncHarnessAssertResourceUsage(t, node, syncHarnessAccount, 2, len(queuedBytes)+len(boundaryBytes))
			if !t.Failed() {
				t.Log("POSTGRES_PASS: concurrent per-input boundary admits N once, explicitly refuses N+1 once and preserves prior queued bytes after both actual adapter lock waits")
			}
		})
	}
}

func TestSyncHarnessSQLCaptureCapacity(t *testing.T) {
	t.Run("pending", func(t *testing.T) {
		top := newSyncHarnessTopology(t)
		node := top.nodes[2]
		keys := newSyncHarnessKeys()
		seedSyncHarnessAdmission(t, node, keys)
		seedSyncHarnessCapture(t, node)
		ctx := context.Background()
		total := 0
		for i := 0; i <= syncmodel.MaxPendingOperations; i++ {
			e := syncHarnessCapacityEnvelope(t, syncHarnessAccount, i, 0)
			err := syncHarnessLocalCapture(ctx, node, keys, e, e.Basis.ObjectID)
			if i < syncmodel.MaxPendingOperations {
				if err != nil {
					t.Fatal(err)
				}
				raw, _ := e.CanonicalBytes()
				total += len(raw)
			} else if !errors.Is(err, syncmodel.ErrResourceCapacity) {
				t.Fatal("pending crossing did not explicitly refuse")
			}
			if i >= syncmodel.MaxPendingOperations-2 {
				syncHarnessAssertResourceUsage(t, node, syncHarnessAccount, min(i+1, syncmodel.MaxPendingOperations), total)
			}
		}
		// A full queue still permits the exact retry; another account/node is independent.
		e := syncHarnessCapacityEnvelope(t, syncHarnessAccount, 0, 0)
		if err := syncHarnessLocalCapture(ctx, node, keys, e, e.Basis.ObjectID); err != nil {
			t.Fatal("full queue rejected exact retry")
		}
		syncHarnessAssertResourceUsage(t, node, syncHarnessAccount, syncmodel.MaxPendingOperations, total)
		for _, account := range []string{"h_adversary_R"} {
			seedSyncHarnessCaptureAccount(t, node, account)
			e := syncHarnessCapacityEnvelope(t, account, 0, 0)
			if err := syncHarnessLocalCapture(ctx, node, keys, e, e.Basis.ObjectID); err != nil {
				t.Fatal("pending budget crossed account")
			}
		}
		other := top.nodes[0]
		seedSyncHarnessSharedAdmission(t, other, keys)
		seedSyncHarnessCapture(t, other)
		if err := syncHarnessLocalCapture(ctx, other, keys, e, e.Basis.ObjectID); err != nil {
			t.Fatal("pending budget crossed node")
		}
		if !t.Failed() {
			t.Log("POSTGRES_PASS: SQL pending 255/256/257, exact retry, account and node isolation")
		}
	})
	for _, delta := range []int{-1, 0, 1} {
		t.Run(fmt.Sprintf("transport_%d", delta), func(t *testing.T) {
			top := newSyncHarnessTopology(t)
			node := top.nodes[2]
			keys := newSyncHarnessKeys()
			seedSyncHarnessAdmission(t, node, keys)
			seedSyncHarnessCapture(t, node)
			ctx := context.Background()
			total := 0
			for i := 0; i < 129; i++ {
				size := syncmodel.MaxEnvelopeBytes
				if i == 126 {
					size = 32768 + delta
				}
				if i == 127 {
					size = 32768
				}
				e := syncHarnessCapacityEnvelope(t, syncHarnessAccount, i, size)
				err := syncHarnessLocalCapture(ctx, node, keys, e, e.Basis.ObjectID)
				if i == 128 && delta > 0 {
					if !errors.Is(err, syncmodel.ErrResourceCapacity) {
						t.Fatal("transport crossing did not explicitly refuse")
					}
					syncHarnessAssertResourceUsage(t, node, syncHarnessAccount, 128, total)
				} else {
					if err != nil {
						t.Fatal(err)
					}
					total += size
					syncHarnessAssertResourceUsage(t, node, syncHarnessAccount, i+1, total)
				}
			}
			if total != syncmodel.MaxTransportBytes+delta && delta <= 0 {
				t.Fatal("transport boundary fixture incorrect")
			}
			seedSyncHarnessCaptureAccount(t, node, "h_adversary_R")
			e := syncHarnessCapacityEnvelope(t, "h_adversary_R", 0, syncmodel.MaxEnvelopeBytes)
			if err := syncHarnessLocalCapture(ctx, node, keys, e, e.Basis.ObjectID); err != nil {
				t.Fatal("transport budget crossed account")
			}
			other := top.nodes[0]
			seedSyncHarnessSharedAdmission(t, other, keys)
			seedSyncHarnessCapture(t, other)
			e = syncHarnessCapacityEnvelope(t, syncHarnessAccount, 0, syncmodel.MaxEnvelopeBytes)
			if err := syncHarnessLocalCapture(ctx, other, keys, e, e.Basis.ObjectID); err != nil {
				t.Fatal("transport budget crossed node")
			}
			if !t.Failed() {
				t.Log("POSTGRES_PASS: SQL retained transport exact byte boundary and account/node isolation")
			}
		})
	}
}

func seedSyncHarnessCaptureAccount(t *testing.T, node *syncHarnessNode, account string) {
	t.Helper()
	ctx := context.Background()
	e := syncHarnessEnvelope(account)
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO h_capture VALUES($1,1,1,$2,'capture_v1','active')`, []any{account, e.OriginalIncarnation.Restore[:]}},
		{`INSERT INTO h_writer_capabilities(account_id,principal,capability,manifest_version,enabled) VALUES($1,'fixture_writer','capture_v1','v1',true)`, []any{account}},
		{`INSERT INTO h_finalized_counter VALUES($1,1,0)`, []any{account}},
	} {
		if _, err := node.store.pool.Exec(ctx, q.sql, q.args...); err != nil {
			t.Fatal("seed account capture failed")
		}
	}
}

func TestSyncHarnessSQLCapacityConcurrentAdmission(t *testing.T) {
	for _, kind := range []string{"pending", "transport"} {
		t.Run(kind, func(t *testing.T) {
			top := newSyncHarnessTopology(t)
			node := top.nodes[2]
			keys := newSyncHarnessKeys()
			seedSyncHarnessAdmission(t, node, keys)
			seedSyncHarnessCapture(t, node)
			ctx := context.Background()
			total := 0
			count, size := 255, 0
			if kind == "transport" {
				count, size = 127, syncmodel.MaxEnvelopeBytes
			}
			for i := 0; i < count; i++ {
				e := syncHarnessCapacityEnvelope(t, syncHarnessAccount, i, size)
				if err := syncHarnessLocalCapture(ctx, node, keys, e, e.Basis.ObjectID); err != nil {
					t.Fatal(err)
				}
				raw, _ := e.CanonicalBytes()
				total += len(raw)
			}
			ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
			defer cancel()
			first, err := syncHarnessBeginCapture(ctx, node, syncHarnessAccount)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = first.Rollback(context.Background()) }()
			e := syncHarnessCapacityEnvelope(t, syncHarnessAccount, count, size)
			if _, err = syncHarnessInsertOperation(ctx, first, keys, e); err != nil {
				t.Fatal(err)
			}
			second, err := syncHarnessBeginCapture(ctx, node, syncHarnessAccount)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = second.Rollback(context.Background()) }()
			var pid int32
			if err := second.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
				t.Fatal("observe capacity contender failed")
			}
			other := syncHarnessCapacityEnvelope(t, syncHarnessAccount, count+1, size)
			done := make(chan error, 1)
			go func() { _, err := syncHarnessInsertOperation(ctx, second, keys, other); done <- err }()
			syncHarnessWaitForRowLock(ctx, t, node, pid)
			if _, err = first.Exec(ctx, `INSERT INTO h_fixture_objects(account_id,object_id,owner_node,value) VALUES($1,$2,'R','synthetic')`, syncHarnessAccount, e.Basis.ObjectID); err != nil {
				t.Fatal("boundary domain write failed")
			}
			if _, err = syncHarnessSealCapture(ctx, first, syncHarnessAccount, e.Basis.ObjectID, e.ID[:]); err != nil {
				t.Fatal(err)
			}
			if err = first.Commit(ctx); err != nil {
				t.Fatal("commit boundary admission failed")
			}
			if err := <-done; !errors.Is(err, syncmodel.ErrResourceCapacity) {
				t.Fatal("boundary contender did not refuse after observed lock wait")
			}
			if err := second.Rollback(ctx); err != nil {
				t.Fatal("rollback refused contender failed")
			}
			raw, _ := e.CanonicalBytes()
			total += len(raw)
			syncHarnessAssertResourceUsage(t, node, syncHarnessAccount, count+1, total)
			if !t.Failed() {
				t.Log("POSTGRES_PASS: scoped capacity row lock observed, exactly one admission at boundary")
			}
		})
	}
}
