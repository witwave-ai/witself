package store

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/witwave-ai/witself/internal/syncmodel"
)

func seedSyncHarnessCapture(t *testing.T, node *syncHarnessNode) {
	t.Helper()
	ctx := context.Background()
	e := syncHarnessEnvelope(syncHarnessAccount)
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO h_capture VALUES($1,1,1,$2,'capture_v1','active')`, []any{syncHarnessAccount, e.OriginalIncarnation.Restore[:]}},
		{`INSERT INTO h_writer_capabilities(account_id,principal,capability,manifest_version,enabled) VALUES($1,'fixture_writer','capture_v1','v1',true)`, []any{syncHarnessAccount}},
		{`INSERT INTO h_finalized_counter VALUES($1,1,0)`, []any{syncHarnessAccount}},
	} {
		if _, err := node.store.pool.Exec(ctx, q.sql, q.args...); err != nil {
			t.Fatal("seed synthetic capture failed")
		}
	}
}

func syncHarnessBeginCapture(ctx context.Context, node *syncHarnessNode, account string) (pgx.Tx, error) {
	tx, err := node.store.pool.Begin(ctx)
	if err != nil {
		return nil, errors.New("begin capture failed")
	}
	if _, err = tx.Exec(ctx, `SELECT h_admit_capture($1)`, account); err != nil {
		_ = tx.Rollback(ctx)
		return nil, errors.New("capture admission refused")
	}
	return tx, nil
}

// The transaction row lock remains held until COMMIT/ROLLBACK. This counter is
// neither a nontransactional sequence nor MAX over visible rows.
func syncHarnessSealCapture(ctx context.Context, tx pgx.Tx, account, bundle string, operation []byte) (int64, error) {
	var ordinal int64
	if err := tx.QueryRow(ctx, `UPDATE h_finalized_counter SET last_finalized=last_finalized+1 WHERE account_id=$1 AND capture_epoch=1 RETURNING last_finalized`, account).Scan(&ordinal); err != nil {
		return 0, errors.New("finalize capture counter failed")
	}
	_, err := tx.Exec(ctx, `INSERT INTO h_bundles(account_id,bundle_id,tx_id,capture_epoch,lifecycle_epoch,restore_marker,finalized,sealed,origin_class,semantic_effects,dependencies)
 SELECT a.account_id,$2,a.tx_id,a.capture_epoch,a.lifecycle_epoch,a.restore_marker,$3,true,'original',COALESCE((SELECT jsonb_agg(semantic_effect ORDER BY touch_id) FROM h_tx_touches t WHERE t.account_id=a.account_id AND t.tx_id=a.tx_id),'[]'::jsonb),COALESCE((SELECT jsonb_agg(dependencies ORDER BY touch_id) FROM h_tx_touches t WHERE t.account_id=a.account_id AND t.tx_id=a.tx_id),'[]'::jsonb) FROM h_tx_admissions a WHERE account_id=$1 AND tx_id=txid_current()`, account, bundle, ordinal)
	if err != nil {
		return 0, errors.New("seal capture bundle failed")
	}
	if _, err = tx.Exec(ctx, `INSERT INTO h_bundle_operations SELECT $1,$2,1,$3,atomic_group FROM h_tx_admissions a WHERE account_id=$1 AND tx_id=txid_current() AND EXISTS(SELECT 1 FROM h_tx_touches t WHERE t.account_id=a.account_id AND t.tx_id=a.tx_id)`, account, bundle, operation); err != nil {
		return 0, errors.New("seal capture member failed")
	}
	if _, err = tx.Exec(ctx, `UPDATE h_tx_admissions SET sealed=true WHERE account_id=$1 AND tx_id=txid_current()`, account); err != nil {
		return 0, errors.New("seal capture admission failed")
	}
	return ordinal, nil
}

func syncHarnessInsertOperation(ctx context.Context, tx pgx.Tx, keys *syncHarnessKeys, e syncmodel.OperationEnvelope) (syncmodel.Commitment, error) {
	c := syncmodel.Commitment{}
	canonical, err := syncHarnessBoundedEnvelope(e)
	if err != nil {
		return c, err
	}
	account := e.OriginalIncarnation.AccountID
	pending, retained, err := syncHarnessLockResourceBudget(ctx, tx, account)
	if err != nil {
		return c, err
	}
	var originalKey string
	var originalCommitment []byte
	err = tx.QueryRow(ctx, `SELECT comparison_key_id,input_commitment FROM h_operations WHERE account_id=$1 AND operation_id=$2 FOR UPDATE`, account, e.ID[:]).Scan(&originalKey, &originalCommitment)
	if err == nil {
		c, err = syncmodel.InputCommitment(keys, account, syncmodel.ComparisonKeyID(originalKey), e)
		if err != nil || !bytes.Equal(originalCommitment, c[:]) {
			return c, errors.New("capture retry comparison refused")
		}
		return c, syncHarnessBindCaptureOperation(ctx, tx, e)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return c, errors.New("read capture admission failed")
	}
	if pending >= syncmodel.MaxPendingOperations || len(canonical) > syncmodel.MaxTransportBytes-retained {
		return c, syncmodel.ErrResourceCapacity
	}
	c, err = syncmodel.InputCommitment(keys, account, keys.current, e)
	if err != nil {
		return c, errors.New("comparison unavailable")
	}
	_, err = tx.Exec(ctx, `INSERT INTO h_operations(account_id,operation_id,original_lifecycle_epoch,original_restore_marker,original_capture_epoch,original_installation,original_replica,original_write_lineage,original_sequence,operation_kind,envelope_version,schema_version,protocol_version,comparison_key_id,input_commitment,admission_state) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,1,1,1,$11,$12,'admitted')`, e.OriginalIncarnation.AccountID, e.ID[:], int64(e.OriginalIncarnation.Lifecycle), e.OriginalIncarnation.Restore[:], int64(e.Capture), e.Origin.InstallationID, e.Origin.ReplicaID, e.Origin.WriteLineageID, int64(e.Origin.OriginSequence), e.Kind, string(keys.current), c[:])
	if err != nil {
		return c, errors.New("insert capture operation failed")
	}
	if _, err = tx.Exec(ctx, `INSERT INTO h_resource_reservations(account_id,operation_id,envelope_bytes) VALUES($1,$2,$3)`, account, e.ID[:], len(canonical)); err != nil {
		return c, errors.New("reserve capture operation failed")
	}
	if _, err = tx.Exec(ctx, `UPDATE h_resource_budgets SET pending_operations=pending_operations+1,retained_bytes=retained_bytes+$2 WHERE account_id=$1`, account, len(canonical)); err != nil {
		return c, errors.New("reserve capture capacity failed")
	}
	if _, err = tx.Exec(ctx, `INSERT INTO h_operation_payloads(account_id,operation_id,canonical_semantic_envelope) VALUES($1,$2,$3)`, e.OriginalIncarnation.AccountID, e.ID[:], canonical); err != nil {
		return c, errors.New("insert capture payload failed")
	}
	return c, syncHarnessBindCaptureOperation(ctx, tx, e)
}

func syncHarnessLocalCapture(ctx context.Context, node *syncHarnessNode, keys *syncHarnessKeys, e syncmodel.OperationEnvelope, object string) error {
	tx, err := syncHarnessBeginCapture(ctx, node, e.OriginalIncarnation.AccountID)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err = syncHarnessInsertOperation(ctx, tx, keys, e); err != nil {
		return err
	}
	var captured bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM h_bundle_operations WHERE account_id=$1 AND operation_id=$2)`, e.OriginalIncarnation.AccountID, e.ID[:]).Scan(&captured); err != nil {
		return errors.New("read capture retry failed")
	}
	if captured {
		return tx.Rollback(ctx)
	}
	if _, err = tx.Exec(ctx, `INSERT INTO h_fixture_objects(account_id,object_id,owner_node,value) VALUES($1,$2,$3,'synthetic')`, e.OriginalIncarnation.AccountID, object, string(node.id)); err != nil {
		return errors.New("local mutation failed")
	}
	if _, err = syncHarnessSealCapture(ctx, tx, e.OriginalIncarnation.AccountID, object, e.ID[:]); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func syncHarnessCount(t *testing.T, node *syncHarnessNode, table string) int {
	t.Helper()
	var n int
	if err := node.store.pool.QueryRow(context.Background(), `SELECT count(*) FROM `+table).Scan(&n); err != nil {
		t.Fatal("fixture count failed")
	}
	return n
}

func TestSyncHarnessOmittedSemanticJournal(t *testing.T) {
	top := newSyncHarnessTopology(t)
	node := top.nodes[2]
	keys := newSyncHarnessKeys()
	seedSyncHarnessAdmission(t, node, keys)
	seedSyncHarnessCapture(t, node)
	ctx := context.Background()
	if _, err := node.store.pool.Exec(ctx, `INSERT INTO h_fixture_objects VALUES($1,'unadmitted','R','synthetic','[]')`, syncHarnessAccount); err == nil {
		t.Fatal("unadmitted mutation accepted")
	}
	for _, mode := range []string{"omitted", "empty", "partial", "wrong_effect", "wrong_dependency", "post_seal", "precommit_crash", "complete"} {
		t.Run(mode, func(t *testing.T) {
			tx, err := syncHarnessBeginCapture(ctx, node, syncHarnessAccount)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback(ctx) }()
			e := syncHarnessEnvelope(syncHarnessAccount)
			if _, err := syncHarnessInsertOperation(ctx, tx, keys, e); err != nil {
				t.Fatal(err)
			}
			if _, err := tx.Exec(ctx, `INSERT INTO h_fixture_objects VALUES($1,'captured','R','synthetic','["required_dependency"]')`, syncHarnessAccount); err != nil {
				t.Fatal("fixture insert failed")
			}
			if _, err := tx.Exec(ctx, `UPDATE h_fixture_objects SET value='synthetic revision' WHERE account_id=$1`, syncHarnessAccount); err != nil {
				t.Fatal("fixture repeated touch failed")
			}
			if mode == "precommit_crash" {
				if err := tx.Rollback(ctx); err != nil {
					t.Fatal("rollback failed")
				}
			} else {
				if mode != "omitted" {
					if mode == "empty" || mode == "partial" || mode == "wrong_effect" || mode == "wrong_dependency" {
						effects := `'[]'::jsonb`
						deps := `'[]'::jsonb`
						if mode != "empty" {
							effects = `(SELECT jsonb_agg(semantic_effect ORDER BY touch_id) FROM h_tx_touches WHERE account_id=$1 AND tx_id=txid_current())`
							deps = `(SELECT jsonb_agg(dependencies ORDER BY touch_id) FROM h_tx_touches WHERE account_id=$1 AND tx_id=txid_current())`
						}
						if mode == "partial" {
							effects = `(SELECT jsonb_agg(semantic_effect) FROM h_tx_touches WHERE account_id=$1 AND tx_id=txid_current() AND touch_id=1)`
						}
						if mode == "wrong_effect" {
							effects = `'[{},{}]'::jsonb`
						}
						if mode == "wrong_dependency" {
							deps = `'[[],[]]'::jsonb`
						}
						q := `INSERT INTO h_bundles SELECT account_id,'bad',tx_id,capture_epoch,lifecycle_epoch,restore_marker,1,true,'original',` + effects + `,` + deps + ` FROM h_tx_admissions WHERE account_id=$1 AND tx_id=txid_current()`
						if _, err := tx.Exec(ctx, q, syncHarnessAccount); err != nil {
							t.Fatal("insert faulty bundle failed")
						}
						if _, err := tx.Exec(ctx, `INSERT INTO h_bundle_operations VALUES($1,'bad',1,$2,'operation')`, syncHarnessAccount, e.ID[:]); err != nil {
							t.Fatal("insert faulty member failed")
						}
						if _, err := tx.Exec(ctx, `UPDATE h_tx_admissions SET sealed=true WHERE account_id=$1 AND tx_id=txid_current()`, syncHarnessAccount); err != nil {
							t.Fatal("seal faulty semantic journal failed")
						}

					} else if _, err := syncHarnessSealCapture(ctx, tx, syncHarnessAccount, "complete", e.ID[:]); err != nil {
						t.Fatal(err)
					}
				}
				if mode == "post_seal" {
					if _, err := tx.Exec(ctx, `UPDATE h_fixture_objects SET value='late' WHERE account_id=$1`, syncHarnessAccount); err == nil {
						t.Fatal("post-seal mutation accepted")
					}
				}
				err = tx.Commit(ctx)
				if mode == "complete" && err != nil {
					t.Fatal("complete semantic journal refused")
				}
				if mode != "complete" && err == nil {
					t.Fatal("incomplete semantic journal committed")
				}
			}
			want := 0
			if mode == "complete" {
				want = 1
			}
			for _, table := range []string{"h_fixture_objects", "h_operations", "h_operation_payloads", "h_tx_admissions", "h_bundles"} {
				if got := syncHarnessCount(t, node, table); got != want {
					t.Errorf("%s count got %d want %d", table, got, want)
				}
			}
		})
	}
	if syncHarnessCount(t, node, "h_receipts") != 0 {
		t.Fatal("local capture fabricated destination receipt")
	}
	if !t.Failed() {
		t.Log("POSTGRES_PASS: admitted mutation and complete semantic effects/dependencies commit atomically; omitted/empty/partial/substituted journal and post-seal writes refuse; local capture remains provisional")
	}
}

func syncHarnessWaitForRowLock(ctx context.Context, t *testing.T, node *syncHarnessNode, pid int32) {
	t.Helper()
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	for {
		var waiting bool
		if err := node.store.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_locks WHERE pid=$1 AND NOT granted)`, pid).Scan(&waiting); err != nil {
			t.Fatal("inspect fixture lock failed")
		}
		if waiting {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatal("fixture lock wait not observed")
		case <-tick.C:
		}
	}
}

func TestSyncHarnessCommittedPrefixCounter(t *testing.T) {
	top := newSyncHarnessTopology(t)
	node := top.nodes[2]
	keys := newSyncHarnessKeys()
	seedSyncHarnessAdmission(t, node, keys)
	seedSyncHarnessCapture(t, node)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	// Admit the immutable inputs before the finalizer schedule. Account capacity
	// reservation has its own row lock; this schedule isolates the actual sealer.
	firstInput, secondInput := syncHarnessEnvelope(syncHarnessAccount), syncHarnessEnvelope(syncHarnessAccount)
	firstInput.ID[0], secondInput.ID[0] = 51, 52
	secondInput.Origin.OriginSequence++
	syncHarnessSeedCaptureOperations(ctx, t, node, keys, firstInput, secondInput)
	tx1 := syncHarnessPrepareObservedCapture(ctx, t, node, firstInput, "first")
	defer func() { _ = tx1.Rollback(context.Background()) }()
	first, err := syncHarnessSealCapture(ctx, tx1, syncHarnessAccount, "first", firstInput.ID[:])
	if err != nil || first != 1 {
		t.Fatal("first actual sealer failed")
	}
	tx2 := syncHarnessPrepareObservedCapture(ctx, t, node, secondInput, "second")
	defer func() { _ = tx2.Rollback(context.Background()) }()
	var pid int32
	if err := tx2.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
		t.Fatal("inspect second finalizer failed")
	}
	done := make(chan error, 1)
	var second int64
	go func() {
		var sealErr error
		second, sealErr = syncHarnessSealCapture(ctx, tx2, syncHarnessAccount, "second", secondInput.ID[:])
		done <- sealErr
	}()
	syncHarnessWaitForRowLock(ctx, t, node, pid)
	var visible, total int64
	if err := node.store.pool.QueryRow(ctx, `SELECT COALESCE(max(finalized),0),count(*) FROM h_bundles`).Scan(&visible, &total); err != nil || visible != 0 || total != 0 {
		t.Fatal("consumer observed unresolved actual bundle prefix")
	}
	if err := tx1.Commit(ctx); err != nil {
		t.Fatal("commit first actual sealer failed")
	}
	if err := <-done; err != nil || second != 2 {
		t.Fatal("second actual sealer failed or skipped ordinal")
	}
	if err := node.store.pool.QueryRow(ctx, `SELECT max(finalized),count(*) FROM h_bundles`).Scan(&visible, &total); err != nil || visible != 1 || total != 1 {
		t.Fatal("consumer skipped committed first actual bundle")
	}
	if err := tx2.Commit(ctx); err != nil {
		t.Fatal("commit second actual sealer failed")
	}
	if err := node.store.pool.QueryRow(ctx, `SELECT max(finalized),count(*) FROM h_bundles`).Scan(&visible, &total); err != nil || visible != 2 || total != 2 {
		t.Fatal("consumer skipped committed second actual bundle")
	}
	// Both deliberately faulty allocators produce a higher visible row while T1
	// remains unresolved. The independent prefix oracle MUST reject each trace.
	for _, mode := range []string{"sequence", "max"} {
		t.Run("counterexample_"+mode, func(t *testing.T) {
			if _, err := node.store.pool.Exec(ctx, `CREATE TABLE h_bad_`+mode+`(n bigint PRIMARY KEY)`); err != nil {
				t.Fatal("create counterexample table failed")
			}
			allocatorDDL := `CREATE SEQUENCE h_bad_sequence_allocator`
			if mode == "max" {
				allocatorDDL = `CREATE TABLE h_bad_max_reservations(n bigint PRIMARY KEY)`
			}
			if _, err := node.store.pool.Exec(ctx, allocatorDDL); err != nil {
				t.Fatal("create counterexample allocator failed")
			}
			a, _ := node.store.pool.Begin(ctx)
			defer func() { _ = a.Rollback(context.Background()) }()
			b, _ := node.store.pool.Begin(ctx)
			defer func() { _ = b.Rollback(context.Background()) }()
			var n1, n2 int64
			// Both faulty allocators release their reservation before the domain
			// transaction resolves. MAX of committed entries then skips T1.
			if mode == "sequence" {
				if err := a.QueryRow(ctx, `SELECT nextval('h_bad_sequence_allocator')`).Scan(&n1); err != nil {
					t.Fatal("sequence allocation failed")
				}
				if err := b.QueryRow(ctx, `SELECT nextval('h_bad_sequence_allocator')`).Scan(&n2); err != nil {
					t.Fatal("sequence allocation failed")
				}
			} else {
				for _, target := range []*int64{&n1, &n2} {
					if err := node.store.pool.QueryRow(ctx, `INSERT INTO h_bad_max_reservations SELECT COALESCE(max(n),0)+1 FROM h_bad_max_reservations RETURNING n`).Scan(target); err != nil {
						t.Fatal("MAX reservation allocation failed")
					}
				}
			}
			if _, err := a.Exec(ctx, `INSERT INTO h_bad_`+mode+` VALUES($1)`, n1); err != nil {
				t.Fatal("first counterexample row failed")
			}
			if _, err := b.Exec(ctx, `INSERT INTO h_bad_`+mode+` VALUES($1)`, n2); err != nil {
				t.Fatal("second counterexample row failed")
			}
			if err := b.Commit(ctx); err != nil {
				t.Fatal("counterexample commit failed")
			}
			var prefix, count int64
			if err := node.store.pool.QueryRow(ctx, `SELECT max(n),count(*) FROM h_bad_`+mode).Scan(&prefix, &count); err != nil {
				t.Fatal("counterexample observe failed")
			}
			if prefix == count {
				t.Fatal("counterexample incorrectly passed complete-prefix oracle")
			}
		})
	}
	if !t.Failed() {
		t.Log("POSTGRES_PASS: actual second sealer waits; h_bundles consumer sees complete committed prefixes; sequence/MAX counterexamples rejected")
	}
}

func TestSyncHarnessCaptureAdmissionLocks(t *testing.T) {
	top := newSyncHarnessTopology(t)
	node := top.nodes[2]
	seedSyncHarnessCapture(t, node)
	for _, kind := range []string{"account_gate", "capability", "control"} {
		t.Run(kind, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			admitted, err := syncHarnessBeginCapture(ctx, node, syncHarnessAccount)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = admitted.Rollback(context.Background()) }()
			contender, err := node.store.pool.Begin(ctx)
			if err != nil {
				t.Fatal("begin contender failed")
			}
			defer func() { _ = contender.Rollback(context.Background()) }()
			var pid int32
			if err := contender.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
				t.Fatal("observe contender failed")
			}
			statement := `SELECT pg_advisory_xact_lock(hashtextextended(current_schema()||chr(31)||$1||chr(31)||'capture',0))`
			switch kind {
			case "capability":
				statement = `UPDATE h_writer_capabilities SET enabled=false WHERE account_id=$1`
			case "control":
				statement = `UPDATE h_capture SET coverage_state='suspended' WHERE account_id=$1`
			}
			done := make(chan error, 1)
			go func() { _, err := contender.Exec(ctx, statement, syncHarnessAccount); done <- err }()
			syncHarnessWaitForRowLock(ctx, t, node, pid)
			if err := admitted.Rollback(ctx); err != nil {
				t.Fatal("release capture admission failed")
			}
			if err := <-done; err != nil {
				t.Fatal("contender did not resume after admission release")
			}
		})
	}
	if !t.Failed() {
		t.Log("POSTGRES_PASS: fixture-owned exclusive gate, capability revocation and control suspension wait for admitted writer transaction")
	}
}
