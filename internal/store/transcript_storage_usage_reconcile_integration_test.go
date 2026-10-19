package store

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/witwave-ai/witself/internal/testenv"
)

type transcriptStorageRowState struct {
	Bytes int64
	Exact bool
	Epoch int64
}

func transcriptStorageStateRows(t *testing.T, st *Store, accountID string) map[int16]transcriptStorageRowState {
	t.Helper()
	rows, err := st.pool.Query(context.Background(), `SELECT shard,retained_bytes,exact,reconcile_epoch FROM transcript_storage_usage WHERE account_id=$1 ORDER BY shard`, accountID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[int16]transcriptStorageRowState{}
	for rows.Next() {
		var shard int16
		var row transcriptStorageRowState
		if err := rows.Scan(&shard, &row.Bytes, &row.Exact, &row.Epoch); err != nil {
			t.Fatal(err)
		}
		out[shard] = row
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func assertTranscriptStoragePending(t *testing.T, st *Store, accountID string) {
	t.Helper()
	rows := transcriptStorageStateRows(t, st, accountID)
	if len(rows) != 16 {
		t.Fatalf("pending rows=%d, want 16", len(rows))
	}
	epochs := map[int64]bool{}
	for _, row := range rows {
		if row.Bytes != 0 || row.Exact || epochs[row.Epoch] {
			t.Fatalf("pending row state invalid: %+v", row)
		}
		epochs[row.Epoch] = true
	}
}

func transcriptStorageFastConfig() TranscriptStorageReconcileConfig {
	cfg := DefaultTranscriptStorageReconcileConfig()
	cfg.DutyCycle = 1
	cfg.PageEntries = 1
	return cfg
}

func transcriptStorageReconcileAndVerify(t *testing.T, st *Store, accountIDs ...string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	summary, err := st.ReconcileTranscriptStorageUsageUntilDone(ctx, transcriptStorageFastConfig())
	if err != nil || summary.UnverifiedEligible != 0 {
		t.Fatalf("reconcile: %+v, error %v", summary, err)
	}
	assertTranscriptStorageUsageMatchesRecount(t, st, accountIDs...)
	for _, accountID := range accountIDs {
		v, err := st.VerifyTranscriptStorageUsage(ctx, accountID)
		if err != nil || !v.Exact || v.Mismatches != 0 {
			t.Fatalf("verify: %+v, error %v", v, err)
		}
	}
}

func transcriptStorageMarkPending(t *testing.T, st *Store, accountID string) {
	t.Helper()
	ctx := context.Background()
	tx, err := st.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := markImportedTranscriptStorageUsagePendingTx(ctx, tx, accountID); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

// Fixture shard choices are deterministic before the SQL shard function exists.
func transcriptStorageConversationAtShard(t *testing.T, st *Store, p Principal, name string, shard byte) Transcript {
	t.Helper()
	tr := createTranscriptStorageTestConversation(t, st, p, name)
	for n := 0; ; n++ {
		candidate := fmt.Sprintf("%s_fixture_%d", tr.ID, n)
		sum := sha256.Sum256([]byte(candidate))
		if sum[0]%16 != shard {
			continue
		}
		if _, err := st.pool.Exec(context.Background(), `UPDATE transcript_conversations SET id=$1 WHERE id=$2`, candidate, tr.ID); err != nil {
			t.Fatal(err)
		}
		tr.ID = candidate
		return tr
	}
}

type transcriptStorageReconcileFixture struct {
	st                   *Store
	dsn                  string
	p                    Principal
	first, second, other Transcript
	entries              []TranscriptEntry
}

func newTranscriptStorageReconcileFixture(t *testing.T) transcriptStorageReconcileFixture {
	t.Helper()
	st, dsn := newMigrationTestStore(t, testenv.RequirePostgres(t))
	migrationTestUpTo(t, dsn, 100)
	p := newTranscriptStorageTestPrincipal(t, st, "reconcile")
	first := transcriptStorageConversationAtShard(t, st, p, "first", 0)
	second := transcriptStorageConversationAtShard(t, st, p, "second", 0)
	other := transcriptStorageConversationAtShard(t, st, p, "other", 1)
	entries := []TranscriptEntry{}
	for i := range 3 {
		entries = append(entries, appendTranscriptStorageTestEntry(t, st, p, first, AppendTranscriptEntryInput{ExternalID: fmt.Sprintf("seed-%d", i), Role: TranscriptRoleUser, Body: "pre-T0 é界 payload"}))
	}
	migrationTestUpTo(t, dsn, 101)
	return transcriptStorageReconcileFixture{st, dsn, p, first, second, other, entries}
}

func transcriptStorageSetHooks(t *testing.T, h *transcriptStorageReconcileHooks) {
	t.Helper()
	old := transcriptStorageReconcileTestHooks
	transcriptStorageReconcileTestHooks = h
	t.Cleanup(func() { transcriptStorageReconcileTestHooks = old })
}

func transcriptStorageFixtureInsert(ctx context.Context, q interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}, f transcriptStorageReconcileFixture, tr Transcript, id string, seq int) error {
	_, err := q.Exec(ctx, `INSERT INTO transcript_entries(id,account_id,transcript_id,realm_id,recorded_by_agent_id,sequence,role,body) VALUES($1,$2,$3,$4,$5,$6,'user','concurrent é界 write')`, id, f.p.AccountID, tr.ID, f.p.RealmID, f.p.ID, seq)
	return err
}

// Keep these predicates shared with the deliberate-contention regression so
// a lease-held pass cannot silently satisfy a no-work assertion.
func transcriptStorageInterleavingPassFailed(result TranscriptStorageReconcilePassResult) bool {
	return result.Err != nil || result.LeaseHeld || result.Error != 0 || result.Timeout != 0 || result.Anomaly != 0
}

func transcriptStorageIneligiblePassFailed(r TranscriptStorageReconcilePassResult) bool {
	return r.Err != nil || r.LeaseHeld || r.Corrected != 0 || r.Confirmed != 0 || r.Busy != 0 || r.Anomaly != 0 || r.Timeout != 0 || r.Error != 0 || r.Skipped != 0 || r.UnverifiedDeferred != 16
}

func transcriptStorageOrphanPassFailed(r TranscriptStorageReconcilePassResult) bool {
	return r.Err != nil || r.LeaseHeld || r.Entries != 0 || r.Corrected != 0 || r.Confirmed != 0 || r.UnverifiedDeferred != 0
}

func TestTranscriptStorageUsageReconcileInterleavingsPostgres(t *testing.T) {
	for _, point := range []string{"afterCounterRead", "afterPage", "beforeApply"} {
		for _, write := range []string{"existing_append", "new_transcript_append", "retention", "negative_delete", "move_shards", "other_shard_append", "rollback", "savepoint"} {
			t.Run(point+"/"+write, func(t *testing.T) {
				f := newTranscriptStorageReconcileFixture(t)
				ctx := context.Background()
				calls := 0
				ran := false
				action := func(ctx context.Context, _ int32) error {
					calls++
					if ran || (point == "afterPage" && calls != 2) {
						return nil
					}
					ran = true
					switch write {
					case "existing_append":
						return transcriptStorageFixtureInsert(ctx, f.st.pool, f, f.first, "interleave_existing", 100)
					case "new_transcript_append":
						created := transcriptStorageConversationAtShard(t, f.st, f.p, "interleave-new-"+point, 0)
						return transcriptStorageFixtureInsert(ctx, f.st.pool, f, created, "interleave_new", 1)
					case "other_shard_append":
						return transcriptStorageFixtureInsert(ctx, f.st.pool, f, f.other, "interleave_other", 1)
					case "negative_delete":
						_, err := f.st.pool.Exec(ctx, `DELETE FROM transcript_entries WHERE id=$1`, f.entries[0].ID)
						return err
					case "move_shards":
						_, err := f.st.pool.Exec(ctx, `UPDATE transcript_entries SET transcript_id=$1 WHERE account_id=$2 AND transcript_id=$3`, f.other.ID, f.p.AccountID, f.first.ID)
						return err
					case "retention":
						if _, err := f.st.SetAccountPlan(ctx, f.p.AccountID, 0, "", "free", map[string]int64{}, map[string]int64{TranscriptRetentionDaysPolicy: 30}, nil); err != nil {
							return err
						}
						if _, err := f.st.pool.Exec(ctx, `UPDATE transcript_conversations SET updated_at=statement_timestamp()-interval '31 days' WHERE id=$1`, f.first.ID); err != nil {
							return err
						}
						r, err := f.st.ProcessTranscriptRetentionBatch(ctx, 10)
						if err == nil && r.Deleted != 1 {
							return errors.New("retention did not delete fixture")
						}
						return err
					case "rollback", "savepoint":
						tx, err := f.st.pool.Begin(ctx)
						if err != nil {
							return err
						}
						defer func() { _ = tx.Rollback(ctx) }()
						if write == "savepoint" {
							if _, err = tx.Exec(ctx, "SAVEPOINT fixture"); err != nil {
								return err
							}
						}
						if err = transcriptStorageFixtureInsert(ctx, tx, f, f.first, "interleave_rollback", 100); err != nil {
							return err
						}
						if write == "savepoint" {
							if _, err = tx.Exec(ctx, "ROLLBACK TO SAVEPOINT fixture"); err != nil {
								return err
							}
							return tx.Commit(ctx)
						}
						return tx.Rollback(ctx)
					}
					return nil
				}
				idleTimeout := 120 * time.Second
				h := &transcriptStorageReconcileHooks{measureIdleTimeout: &idleTimeout}
				switch point {
				case "afterCounterRead":
					h.afterCounterRead = action
				case "afterPage":
					h.afterPage = action
				case "beforeApply":
					h.beforeApply = action
				}
				transcriptStorageSetHooks(t, h)
				result := f.st.reconcileTranscriptStorageUsagePass(ctx, transcriptStorageFastConfig(), newTranscriptStorageReconcileRunner(), nil)
				if transcriptStorageInterleavingPassFailed(result) {
					t.Fatalf("pass: %+v", result)
				}
				if !ran {
					t.Fatal("interleaving hook never ran")
				}
				transcriptStorageReconcileTestHooks = nil
				transcriptStorageReconcileAndVerify(t, f.st, f.p.AccountID)
			})
		}
	}
}

func TestTranscriptStorageUsageReconcileCompareAndSetPostgres(t *testing.T) {
	for _, change := range []string{"second_measurement", "import_reset", "row_recreated"} {
		t.Run(change, func(t *testing.T) {
			f := newTranscriptStorageReconcileFixture(t)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			lease, held, err := f.st.acquireTranscriptStorageReconcileLease(ctx)
			if err != nil || !held {
				t.Fatalf("lease held=%t error=%v", held, err)
			}
			defer lease.close()
			unit := transcriptStorageReconcileUnit{accountID: f.p.AccountID, shard: 0}
			first, outcome, err := lease.measure(ctx, transcriptStorageFastConfig(), unit)
			if err != nil || outcome != "" {
				t.Fatalf("measure %s: %v", outcome, err)
			}
			switch change {
			case "second_measurement":
				second, outcome, err := lease.measure(ctx, transcriptStorageFastConfig(), unit)
				if err != nil || outcome != "" {
					t.Fatalf("second measure %s: %v", outcome, err)
				}
				outcome, err = lease.apply(ctx, unit, second)
				if err != nil || outcome != "corrected" {
					t.Fatalf("first apply %s: %v", outcome, err)
				}
			case "import_reset":
				transcriptStorageMarkPending(t, f.st, f.p.AccountID)
			case "row_recreated":
				if _, err := f.st.pool.Exec(ctx, `DELETE FROM transcript_storage_usage WHERE account_id=$1 AND shard=0`, f.p.AccountID); err != nil {
					t.Fatal(err)
				}
				if err := transcriptStorageFixtureInsert(ctx, f.st.pool, f, f.first, "recreated", 100); err != nil {
					t.Fatal(err)
				}
			}
			before := transcriptStorageStateRows(t, f.st, f.p.AccountID)
			outcome, err = lease.apply(ctx, unit, first)
			if err != nil || outcome != "raced" {
				t.Fatalf("stale apply %s: %v", outcome, err)
			}
			if !reflect.DeepEqual(before, transcriptStorageStateRows(t, f.st, f.p.AccountID)) {
				t.Fatal("stale apply changed row value or epoch")
			}
			// Hand deletion deliberately breaks the model. This case ends at the CAS assertion.
		})
	}
}

func transcriptStoragePoll(ctx context.Context, t *testing.T, condition func() (bool, error)) {
	t.Helper()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		ok, err := condition()
		if err != nil {
			t.Fatal(err)
		}
		if ok {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-ticker.C:
		}
	}
}

func TestTranscriptStorageUsageReconcileDoesNotBlockWritersPostgres(t *testing.T) {
	t.Run("paused_measure", func(t *testing.T) {
		f := newTranscriptStorageReconcileFixture(t)
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		paused := make(chan int32, 1)
		release := make(chan struct{})
		var once sync.Once
		idleTimeout := 120 * time.Second
		transcriptStorageSetHooks(t, &transcriptStorageReconcileHooks{measureIdleTimeout: &idleTimeout, afterPage: func(ctx context.Context, pid int32) error {
			once.Do(func() {
				paused <- pid
				select {
				case <-release:
				case <-ctx.Done():
				}
			})
			return ctx.Err()
		}})
		result := make(chan TranscriptStorageReconcilePassResult, 1)
		go func() {
			result <- f.st.reconcileTranscriptStorageUsagePass(ctx, transcriptStorageFastConfig(), newTranscriptStorageReconcileRunner(), nil)
		}()
		var pid int32
		select {
		case pid = <-paused:
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		if err := transcriptStorageFixtureInsert(ctx, f.st.pool, f, f.first, "unblocked", 100); err != nil {
			t.Fatal(err)
		}
		if _, err := f.st.SetAccountPlan(ctx, f.p.AccountID, 0, "", "free", map[string]int64{}, map[string]int64{TranscriptRetentionDaysPolicy: 30}, nil); err != nil {
			t.Fatal(err)
		}
		if _, err := f.st.pool.Exec(ctx, `UPDATE transcript_conversations SET updated_at=statement_timestamp()-interval '31 days' WHERE id=$1`, f.first.ID); err != nil {
			t.Fatal(err)
		}
		retained, err := f.st.ProcessTranscriptRetentionBatch(ctx, 10)
		if err != nil || retained.Deleted != 1 {
			t.Fatalf("retention: %+v, %v", retained, err)
		}
		var forbidden int
		if err := f.st.pool.QueryRow(ctx, `SELECT count(*) FROM pg_locks WHERE pid=$1 AND ((locktype='relation' AND mode<>'AccessShareLock') OR (locktype IN ('tuple','transactionid') AND NOT granted))`, pid).Scan(&forbidden); err != nil {
			t.Fatal(err)
		}
		if forbidden != 0 {
			t.Fatalf("measure holds or waits on %d forbidden locks", forbidden)
		}
		close(release)
		r := <-result
		if r.Err != nil || r.Error != 0 {
			t.Fatalf("pass: %+v", r)
		}
		transcriptStorageReconcileTestHooks = nil
		transcriptStorageReconcileAndVerify(t, f.st, f.p.AccountID)
	})
	for _, override := range []bool{true, false} {
		t.Run(fmt.Sprintf("held_apply_override_%t", override), func(t *testing.T) {
			f := newTranscriptStorageReconcileFixture(t)
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			tx, err := f.st.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback(context.Background()) }()
			if err := transcriptStorageFixtureInsert(ctx, tx, f, f.first, "held_append", 100); err != nil {
				t.Fatal(err)
			}
			applyReady := make(chan int32, 1)
			var reads atomic.Int32
			var once sync.Once
			timeout := 30 * time.Second
			idleTimeout := 120 * time.Second
			hooks := &transcriptStorageReconcileHooks{measureIdleTimeout: &idleTimeout, afterCounterRead: func(context.Context, int32) error { reads.Add(1); return nil }, beforeApply: func(_ context.Context, pid int32) error { once.Do(func() { applyReady <- pid }); return nil }}
			if override {
				hooks.applyLockTimeout = &timeout
			}
			// Leave just this shard pending so the measurement count is meaningful.
			if _, err := f.st.pool.Exec(ctx, `UPDATE transcript_storage_usage SET exact=true WHERE account_id=$1 AND shard<>0`, f.p.AccountID); err != nil {
				t.Fatal(err)
			}
			transcriptStorageSetHooks(t, hooks)
			result := make(chan TranscriptStorageReconcilePassResult, 1)
			go func() {
				result <- f.st.reconcileTranscriptStorageUsagePass(ctx, transcriptStorageFastConfig(), newTranscriptStorageReconcileRunner(), nil)
			}()
			var pid int32
			select {
			case pid = <-applyReady:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			transcriptStoragePoll(ctx, t, func() (bool, error) {
				var waiting bool
				err := f.st.pool.QueryRow(ctx, `SELECT COALESCE(wait_event_type='Lock',false) FROM pg_stat_activity WHERE pid=$1`, pid).Scan(&waiting)
				return waiting, err
			})
			if !override {
				select {
				case <-time.After(3 * 500 * time.Millisecond):
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
			}
			if err := tx.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			r := <-result
			if r.Err != nil || r.Corrected != 1 || r.Busy != 0 || r.Error != 0 {
				t.Fatalf("apply after release: %+v", r)
			}
			if reads.Load() != 1 {
				t.Fatalf("measure ran %d times, want one", reads.Load())
			}
			transcriptStorageReconcileTestHooks = nil
			transcriptStorageReconcileAndVerify(t, f.st, f.p.AccountID)
		})
	}
}

func TestTranscriptStorageUsageReconcileRestartSafePostgres(t *testing.T) {
	for _, failure := range []string{"cancel_after_page", "error_before_apply", "error_after_apply", "terminate_after_page", "terminate_before_apply"} {
		t.Run(failure, func(t *testing.T) {
			f := newTranscriptStorageReconcileFixture(t)
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			before := transcriptStorageStateRows(t, f.st, f.p.AccountID)
			injected := errors.New("injected interruption")
			ran := false
			hook := func(_ context.Context, pid int32) error {
				if ran {
					return nil
				}
				ran = true
				switch failure {
				case "cancel_after_page":
					cancel()
					return ctx.Err()
				case "terminate_after_page", "terminate_before_apply":
					_, err := f.st.pool.Exec(context.Background(), `SELECT pg_terminate_backend($1)`, pid)
					return err
				default:
					return injected
				}
			}
			h := &transcriptStorageReconcileHooks{}
			switch failure {
			case "cancel_after_page", "terminate_after_page":
				h.afterPage = hook
			case "error_before_apply", "terminate_before_apply":
				h.beforeApply = hook
			case "error_after_apply":
				h.afterApply = hook
			}
			transcriptStorageSetHooks(t, h)
			result := f.st.reconcileTranscriptStorageUsagePass(ctx, transcriptStorageFastConfig(), newTranscriptStorageReconcileRunner(), nil)
			if !ran {
				t.Fatal("failure hook did not run")
			}
			terminated := failure == "terminate_after_page" || failure == "terminate_before_apply"
			if terminated && (result.LeaseHeld || result.Err == nil || result.Err.Error() != "transcript storage reconcile lease lost" || result.Error != 1 || result.units() != 1) {
				t.Fatalf("terminated lease must record only its failing unit and end the pass: %+v", result)
			}
			after := transcriptStorageStateRows(t, f.st, f.p.AccountID)
			if terminated && !reflect.DeepEqual(before, after) {
				t.Fatal("terminated lease changed another candidate")
			}
			if failure == "error_after_apply" {
				if !after[0].Exact || after[0].Bytes == 0 {
					t.Fatal("apply did not commit before crash")
				}
			} else if before[0] != after[0] {
				t.Fatal("interrupted measure changed the unit")
			}
			transcriptStorageReconcileTestHooks = nil
			second, err := Open(context.Background(), f.dsn)
			if err != nil {
				t.Fatal(err)
			}
			defer second.Close()
			if terminated {
				next := second.reconcileTranscriptStorageUsagePass(context.Background(), transcriptStorageFastConfig(), newTranscriptStorageReconcileRunner(), nil)
				if next.Err != nil || next.LeaseHeld || next.Error != 0 || next.Timeout != 0 || next.Corrected+next.Confirmed != 16 || next.UnverifiedEligible != 0 {
					t.Fatalf("next pass did not reacquire and converge: %+v", next)
				}
			}
			transcriptStorageReconcileAndVerify(t, second, f.p.AccountID)
			if failure == "error_after_apply" && transcriptStorageStateRows(t, second, f.p.AccountID)[0] != after[0] {
				t.Fatal("rerun changed already committed exact row")
			}
		})
	}
}

func TestTranscriptStorageUsageReconcileLeasePostgres(t *testing.T) {
	f := newTranscriptStorageReconcileFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	second, err := Open(ctx, f.dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	paused := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	statusBeforePick := false
	idleTimeout := 120 * time.Second
	transcriptStorageSetHooks(t, &transcriptStorageReconcileHooks{measureIdleTimeout: &idleTimeout, afterCounterRead: func(ctx context.Context, _ int32) error {
		once.Do(func() {
			if !statusBeforePick {
				t.Error("working pass did not publish status before first unit")
			}
			close(paused)
			select {
			case <-release:
			case <-ctx.Done():
			}
		})
		return ctx.Err()
	}})
	result := make(chan TranscriptStorageReconcilePassResult, 1)
	go func() {
		result <- f.st.reconcileTranscriptStorageUsagePass(ctx, transcriptStorageFastConfig(), newTranscriptStorageReconcileRunner(), func(s TranscriptStorageReconcileStatus) {
			if s.UnverifiedEligible == 16 {
				statusBeforePick = true
			}
		})
	}()
	select {
	case <-paused:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	before := transcriptStorageStateRows(t, second, f.p.AccountID)
	published := false
	r := second.reconcileTranscriptStorageUsagePass(ctx, transcriptStorageFastConfig(), newTranscriptStorageReconcileRunner(), func(s TranscriptStorageReconcileStatus) {
		published = true
		if s.UnverifiedEligible != 16 {
			t.Errorf("non-leader status eligible=%d, want 16", s.UnverifiedEligible)
		}
	})
	if !r.LeaseHeld || !published || r.Entries != 0 || r.Corrected != 0 {
		t.Fatalf("second pass: %+v, published=%t", r, published)
	}
	if !reflect.DeepEqual(before, transcriptStorageStateRows(t, second, f.p.AccountID)) {
		t.Fatal("non-leader changed rows")
	}
	close(release)
	if r := <-result; r.Err != nil || r.Error != 0 {
		t.Fatalf("leader: %+v", r)
	}
	transcriptStorageReconcileTestHooks = nil
	transcriptStorageReconcileAndVerify(t, second, f.p.AccountID)
}

func TestTranscriptStorageUsageReconcileContentionAssertionsPostgres(t *testing.T) {
	f := newTranscriptStorageReconcileFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	lease, held, err := f.st.acquireTranscriptStorageReconcileLease(ctx)
	if err != nil || !held {
		t.Fatalf("contention fixture lease held=%t error=%v", held, err)
	}
	defer lease.close()

	assertRejected := func(t *testing.T, rejects func(TranscriptStorageReconcilePassResult) bool) {
		t.Helper()
		r := f.st.reconcileTranscriptStorageUsagePass(ctx, transcriptStorageFastConfig(), newTranscriptStorageReconcileRunner(), nil)
		if !r.LeaseHeld {
			t.Fatalf("contention fixture did not hold the lease: %+v", r)
		}
		withoutContention := r
		withoutContention.LeaseHeld = false
		if rejects(withoutContention) {
			t.Fatalf("contention fixture triggered an unrelated rejection: %+v", withoutContention)
		}
		if !rejects(r) {
			t.Fatalf("lease contention accepted by pass assertion: %+v", r)
		}
	}

	t.Run("interleaving", func(t *testing.T) {
		assertRejected(t, transcriptStorageInterleavingPassFailed)
	})
	if _, err := f.st.BeginAccountEvacuation(ctx, f.p.AccountID, "evac_counter_contention", "counter fixture"); err != nil {
		t.Fatal(err)
	}
	t.Run("ineligible", func(t *testing.T) {
		assertRejected(t, transcriptStorageIneligiblePassFailed)
	})
	if _, err := f.st.FinalizeAccountEvacuationSource(ctx, f.p.AccountID, "evac_counter_contention"); err != nil {
		t.Fatal(err)
	}
	t.Run("orphan", func(t *testing.T) {
		assertRejected(t, transcriptStorageOrphanPassFailed)
	})
}

func TestTranscriptStorageUsageReconcileEligibilityPostgres(t *testing.T) {
	for _, kind := range []string{"evacuation", "closed"} {
		for _, beforeApply := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s_before_apply_%t", kind, beforeApply), func(t *testing.T) {
				f := newTranscriptStorageReconcileFixture(t)
				ctx := context.Background()
				changed := false
				calls := 0
				makeIneligible := func() error {
					if changed {
						return nil
					}
					changed = true
					if kind == "evacuation" {
						_, err := f.st.BeginAccountEvacuation(ctx, f.p.AccountID, "evac_counter_eligibility", "counter fixture")
						return err
					}
					var owner string
					if err := f.st.pool.QueryRow(ctx, `SELECT id FROM operators WHERE account_id=$1 AND role='account_owner' LIMIT 1`, f.p.AccountID).Scan(&owner); err != nil {
						return err
					}
					return f.st.CloseAccount(ctx, f.p.AccountID, owner, "counter fixture")
				}
				h := &transcriptStorageReconcileHooks{afterCounterRead: func(context.Context, int32) error { calls++; return nil }}
				if beforeApply {
					h.beforeApply = func(context.Context, int32) error { return makeIneligible() }
				} else if err := makeIneligible(); err != nil {
					t.Fatal(err)
				}
				before := transcriptStorageStateRows(t, f.st, f.p.AccountID)
				transcriptStorageSetHooks(t, h)
				r := f.st.reconcileTranscriptStorageUsagePass(ctx, transcriptStorageFastConfig(), newTranscriptStorageReconcileRunner(), nil)
				if transcriptStorageIneligiblePassFailed(r) {
					t.Fatalf("ineligible pass: %+v", r)
				}
				if beforeApply {
					if r.Raced < 1 {
						t.Fatalf("ineligible before apply: %+v", r)
					}
				} else if r.Raced != 0 || r.Entries != 0 || calls != 0 {
					t.Fatalf("ineligible unit picked: %+v, hooks=%d", r, calls)
				}
				if !reflect.DeepEqual(before, transcriptStorageStateRows(t, f.st, f.p.AccountID)) {
					t.Fatal("ineligible apply changed counter state")
				}
				transcriptStorageReconcileTestHooks = nil
				if kind == "evacuation" {
					if _, err := f.st.FinalizeAccountEvacuationSource(ctx, f.p.AccountID, "evac_counter_eligibility"); err != nil {
						t.Fatal(err)
					}
					if len(transcriptStorageStateRows(t, f.st, f.p.AccountID)) == 0 {
						t.Fatal("finalization removed orphan rows")
					}
					r := f.st.reconcileTranscriptStorageUsagePass(ctx, transcriptStorageFastConfig(), newTranscriptStorageReconcileRunner(), nil)
					if transcriptStorageOrphanPassFailed(r) {
						t.Fatalf("orphan picked: %+v", r)
					}
				} else {
					if _, err := f.st.pool.Exec(ctx, `UPDATE accounts SET closed_at=statement_timestamp()-interval '31 days' WHERE id=$1`, f.p.AccountID); err != nil {
						t.Fatal(err)
					}
					purged, err := f.st.ProcessAccountPurgeBatch(ctx, 10, 30*24*time.Hour)
					if err != nil || purged.PurgedAccounts != 1 {
						t.Fatalf("purge: %+v, %v", purged, err)
					}
					transcriptStorageReconcileAndVerify(t, f.st, f.p.AccountID)
				}
			})
		}
	}
	t.Run("serializable_before_t0", func(t *testing.T) {
		st, dsn := newMigrationTestStore(t, testenv.RequirePostgres(t))
		migrationTestUpTo(t, dsn, 100)
		ctx := context.Background()
		p := newTranscriptStorageTestPrincipal(t, st, "serializable")
		tr := createTranscriptStorageTestConversation(t, st, p, "serializable")
		entry := appendTranscriptStorageTestEntry(t, st, p, tr, AppendTranscriptEntryInput{Role: TranscriptRoleUser, Body: "before snapshot"})
		tx, err := st.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		var count int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM transcript_entries WHERE account_id=$1`, p.AccountID).Scan(&count); err != nil {
			t.Fatal(err)
		}
		migrationTestUpTo(t, dsn, 101)
		_, err = tx.Exec(ctx, `DELETE FROM transcript_entries WHERE id=$1`, entry.ID)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "40001" {
			t.Fatalf("old snapshot delete: %v", err)
		}
		_ = tx.Rollback(ctx)
		if _, err := st.pool.Exec(ctx, `DELETE FROM transcript_entries WHERE id=$1`, entry.ID); err != nil {
			t.Fatal(err)
		}
		transcriptStorageReconcileAndVerify(t, st, p.AccountID)
	})
}

func TestTranscriptStorageUsageReconcileConcurrentWritersPostgres(t *testing.T) {
	st, dsn := newMigrationTestStore(t, testenv.RequirePostgres(t))
	migrationTestUpTo(t, dsn, 100)
	ctx := context.Background()
	principals := []Principal{newTranscriptStorageTestPrincipal(t, st, "concurrent-a"), newTranscriptStorageTestPrincipal(t, st, "concurrent-b")}
	conversations := make([][]Transcript, 2)
	for a, p := range principals {
		for i := range 4 {
			tr := createTranscriptStorageTestConversation(t, st, p, fmt.Sprintf("concurrent-%d", i))
			conversations[a] = append(conversations[a], tr)
			appendTranscriptStorageTestEntry(t, st, p, tr, AppendTranscriptEntryInput{Role: TranscriptRoleUser, Body: "pre-T0"})
		}
	}
	migrationTestUpTo(t, dsn, 101)
	if _, err := st.SetAccountPlan(ctx, principals[0].AccountID, 0, "", "free", map[string]int64{}, map[string]int64{TranscriptRetentionDaysPolicy: 30}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := st.pool.Exec(ctx, `UPDATE transcript_conversations SET updated_at=statement_timestamp()-interval '31 days' WHERE id=$1`, conversations[0][3].ID); err != nil {
		t.Fatal(err)
	}
	// Bound the concurrent workload by the configured unit budget; migration
	// and principal/conversation setup must not consume that workload budget.
	ctx, cancel := context.WithTimeout(ctx, transcriptStorageFastConfig().UnitTimeout)
	defer cancel()
	start := make(chan struct{})
	writerErrors := make(chan error, 5)
	overlapped := make(chan struct{}, 5)
	var wg sync.WaitGroup
	var once sync.Once
	observedOverlap := false
	backoff := 10 * time.Minute
	transcriptStorageSetHooks(t, &transcriptStorageReconcileHooks{backoff: &backoff, afterPage: func(hookCtx context.Context, _ int32) error {
		once.Do(func() {
			// Start every writer only after the measurement snapshot is open.
			// Keep it open until all four appends and retention have returned.
			close(start)
			for range 5 {
				select {
				case <-overlapped:
				case <-hookCtx.Done():
					return
				}
			}
			observedOverlap = true
		})
		return hookCtx.Err()
	}})
	// Cancel and join before restoring hooks or closing the fixture pool, even
	// when reconciliation fails before the barrier opens.
	t.Cleanup(func() { cancel(); wg.Wait() })
	for n := range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case <-start:
			case <-ctx.Done():
				return
			}
			p := principals[n%2]
			for i := range 20 {
				tr := conversations[n%2][i%3]
				_, err := st.AppendTranscriptEntry(ctx, p.AccountID, p.RealmID, p.ID, tr.ID, AppendTranscriptEntryInput{ExternalID: fmt.Sprintf("writer-%d-%d", n, i), Role: TranscriptRoleUser, Body: "concurrent append"})
				if i == 0 {
					overlapped <- struct{}{}
				}
				if err != nil {
					writerErrors <- err
					return
				}
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		select {
		case <-start:
		case <-ctx.Done():
			return
		}
		defer func() { overlapped <- struct{}{} }()
		for {
			retention, err := st.ProcessTranscriptRetentionBatch(ctx, 10)
			if err != nil {
				writerErrors <- err
				return
			}
			if retention.Deleted != 0 {
				if retention.Deleted != 1 {
					writerErrors <- fmt.Errorf("concurrent retention deleted %d conversations, want one", retention.Deleted)
				}
				return
			}
			// Retention deliberately skips an account locked by an appender.
			// Keep its worker active until it deletes the old conversation,
			// while the measurement hook still holds its original snapshot.
			if !waitForMessageRateBucketCleanupInterval(ctx, 20*time.Millisecond) {
				writerErrors <- ctx.Err()
				return
			}
		}
	}()
	summary, err := st.ReconcileTranscriptStorageUsageUntilDone(ctx, transcriptStorageFastConfig())
	if err != nil || summary.Corrected+summary.Confirmed < 1 || summary.Error != 0 || summary.Timeout != 0 {
		t.Fatalf("overlapped measurement: %+v, error %v", summary, err)
	}
	if !observedOverlap {
		t.Fatal("reconcile did not overlap all four appenders and retention")
	}
	wg.Wait()
	close(writerErrors)
	for err := range writerErrors {
		t.Errorf("concurrent writer: %v", err)
	}
	transcriptStorageReconcileTestHooks = nil
	transcriptStorageReconcileAndVerify(t, st, principals[0].AccountID, principals[1].AccountID)
}
