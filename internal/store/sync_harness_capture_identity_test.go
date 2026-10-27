package store

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/witwave-ai/witself/internal/syncmodel"
)

// Bind before observing a domain effect. The sealer may not choose or rewrite
// the identity of a previously observed touch. An admission-only caller has no
// capture transaction row and therefore needs no binding.
func syncHarnessBindCaptureOperation(ctx context.Context, tx pgx.Tx, e syncmodel.OperationEnvelope) error {
	group := "operation"
	if e.Compound != nil {
		group = e.Compound.GroupID
	}
	if _, err := tx.Exec(ctx, `UPDATE h_tx_admissions SET operation_id=$2,atomic_group=$3 WHERE account_id=$1 AND tx_id=txid_current()`, e.OriginalIncarnation.AccountID, e.ID[:], group); err != nil {
		return errors.New("bind capture operation failed")
	}
	return nil
}

func syncHarnessSeedCaptureOperations(ctx context.Context, t *testing.T, node *syncHarnessNode, keys *syncHarnessKeys, inputs ...syncmodel.OperationEnvelope) {
	t.Helper()
	tx, err := node.store.pool.Begin(ctx)
	if err != nil {
		t.Fatal("begin fixture input seed failed")
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	for _, input := range inputs {
		if _, err := syncHarnessInsertOperation(ctx, tx, keys, input); err != nil {
			t.Fatal("seed fixture capture operation failed")
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal("commit fixture capture inputs failed")
	}
}

func syncHarnessPrepareObservedCapture(ctx context.Context, t *testing.T, node *syncHarnessNode, e syncmodel.OperationEnvelope, object string) pgx.Tx {
	t.Helper()
	tx, err := syncHarnessBeginCapture(ctx, node, e.OriginalIncarnation.AccountID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
	if err := syncHarnessBindCaptureOperation(ctx, tx, e); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO h_fixture_objects(account_id,object_id,owner_node,value) VALUES($1,$2,$3,'synthetic')`, e.OriginalIncarnation.AccountID, object, string(node.id)); err != nil {
		t.Fatal("record fixture capture effect failed")
	}
	return tx
}

func TestSyncHarnessCaptureRollbackContinuity(t *testing.T) {
	top := newSyncHarnessTopology(t)
	node := top.nodes[2]
	keys := newSyncHarnessKeys()
	seedSyncHarnessAdmission(t, node, keys)
	seedSyncHarnessCapture(t, node)
	ctx := context.Background()
	e := syncHarnessEnvelope(syncHarnessAccount)
	e.ID[0] = 71
	tx, err := syncHarnessBeginCapture(ctx, node, syncHarnessAccount)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := syncHarnessInsertOperation(ctx, tx, keys, e); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO h_fixture_objects(account_id,object_id,owner_node,value) VALUES($1,'rolled_back','R','synthetic')`, syncHarnessAccount); err != nil {
		t.Fatal("capture before rollback failed")
	}
	if _, err := syncHarnessSealCapture(ctx, tx, syncHarnessAccount, "rolled_back", e.ID[:]); err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal("actual seal rollback failed")
	}
	e.ID[0] = 72
	e.Origin.OriginSequence++
	if err := syncHarnessLocalCapture(ctx, node, keys, e, "committed"); err != nil {
		t.Fatal(err)
	}
	var maxOrdinal, count int
	if err := node.store.pool.QueryRow(ctx, `SELECT max(finalized),count(*) FROM h_bundles`).Scan(&maxOrdinal, &count); err != nil || maxOrdinal != 1 || count != 1 {
		t.Fatal("actual sealer rollback left a prefix gap")
	}
	if !t.Failed() {
		t.Log("POSTGRES_PASS: actual seal rollback followed by actual local capture has finalized maximum/count 1/1")
	}
}

func TestSyncHarnessCaptureBundleMemberIdentity(t *testing.T) {
	top := newSyncHarnessTopology(t)
	node := top.nodes[2]
	keys := newSyncHarnessKeys()
	seedSyncHarnessAdmission(t, node, keys)
	seedSyncHarnessCapture(t, node)
	ctx := context.Background()
	for i, object := range []string{"first", "second"} {
		e := syncHarnessEnvelope(syncHarnessAccount)
		e.ID[0] = byte(80 + i)
		e.Origin.OriginSequence += syncmodel.Ordinal(i)
		if err := syncHarnessLocalCapture(ctx, node, keys, e, object); err != nil {
			t.Fatal("distinct local capture refused")
		}
		var member, touched []byte
		var memberGroup, touchGroup string
		if err := node.store.pool.QueryRow(ctx, `SELECT m.operation_id,m.atomic_group,t.operation_id,t.atomic_group FROM h_bundles b JOIN h_bundle_operations m USING(account_id,bundle_id) JOIN h_tx_touches t USING(account_id,tx_id) WHERE b.account_id=$1 AND b.bundle_id=$2`, syncHarnessAccount, object).Scan(&member, &memberGroup, &touched, &touchGroup); err != nil {
			t.Fatal("read exact capture identities failed")
		}
		if !bytes.Equal(member, e.ID[:]) || !bytes.Equal(touched, e.ID[:]) || memberGroup != "operation" || touchGroup != memberGroup {
			t.Fatal("bundle does not match the operation/group of its observed effect")
		}
	}
	if !t.Failed() {
		t.Log("POSTGRES_PASS: each distinct capture seals its exact observed operation and atomic group")
	}
}

func TestSyncHarnessCaptureRejectsMismatchedMemberSet(t *testing.T) {
	top := newSyncHarnessTopology(t)
	node := top.nodes[2]
	keys := newSyncHarnessKeys()
	seedSyncHarnessAdmission(t, node, keys)
	seedSyncHarnessCapture(t, node)
	ctx := context.Background()
	e, other := syncHarnessEnvelope(syncHarnessAccount), syncHarnessEnvelope(syncHarnessAccount)
	e.ID[0], other.ID[0] = 82, 83
	other.Origin.OriginSequence++
	syncHarnessSeedCaptureOperations(ctx, t, node, keys, e, other)
	for _, mode := range []string{"wrong_operation", "wrong_group", "extra_operation", "missing_member"} {
		t.Run(mode, func(t *testing.T) {
			tx := syncHarnessPrepareObservedCapture(ctx, t, node, e, mode)
			if _, err := tx.Exec(ctx, `INSERT INTO h_bundles SELECT account_id,$2,tx_id,capture_epoch,lifecycle_epoch,restore_marker,1,true,'original',(SELECT jsonb_agg(semantic_effect ORDER BY touch_id) FROM h_tx_touches WHERE account_id=$1 AND tx_id=txid_current()),(SELECT jsonb_agg(dependencies ORDER BY touch_id) FROM h_tx_touches WHERE account_id=$1 AND tx_id=txid_current()) FROM h_tx_admissions WHERE account_id=$1 AND tx_id=txid_current()`, syncHarnessAccount, mode); err != nil {
				t.Fatal("seed otherwise complete journal failed")
			}
			member, group := e.ID, "operation"
			if mode == "wrong_operation" {
				member = other.ID
			}
			if mode == "wrong_group" {
				group = "unobserved_group"
			}
			if mode != "missing_member" {
				if _, err := tx.Exec(ctx, `INSERT INTO h_bundle_operations VALUES($1,$2,1,$3,$4)`, syncHarnessAccount, mode, member[:], group); err != nil {
					t.Fatal("insert mismatched fixture member failed")
				}
			}
			if mode == "extra_operation" {
				if _, err := tx.Exec(ctx, `INSERT INTO h_bundle_operations VALUES($1,$2,2,$3,'operation')`, syncHarnessAccount, mode, other.ID[:]); err != nil {
					t.Fatal("insert extraneous fixture member failed")
				}
			}
			if _, err := tx.Exec(ctx, `UPDATE h_tx_admissions SET sealed=true WHERE account_id=$1 AND tx_id=txid_current()`, syncHarnessAccount); err != nil {
				t.Fatal("seal malformed fixture journal failed")
			}
			if err := tx.Commit(ctx); err == nil {
				t.Fatal("deferred coverage accepted a different operation/group set")
			}
			if syncHarnessCount(t, node, "h_bundles") != 0 || syncHarnessCount(t, node, "h_fixture_objects") != 0 {
				t.Fatal("mismatched capture escaped rollback")
			}
		})
	}
	if !t.Failed() {
		t.Log("POSTGRES_PASS: wrong, missing and extra bundle operation/group members abort complete semantic journal at deferred coverage")
	}
}
