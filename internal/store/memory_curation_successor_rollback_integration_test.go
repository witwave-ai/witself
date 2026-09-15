package store

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/witwave-ai/witself/internal/testenv"
)

func TestMemoryCurationSuccessorRollbackPostgres(t *testing.T) {
	dsn := testenv.RequirePostgres(t)
	for _, replay := range []bool{false, true} {
		name := "successor_start_wins"
		if replay {
			name = "exact_rollback_replay_with_active_successor"
		}
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			st, p, source, started, planned, createdID := prepareMemoryCurationConflictPlan(ctx, t, dsn, true)
			applied, err := st.ApplyCuration(ctx, p, started.Run.ID, ApplyMemoryCurationInput{
				FencingGeneration: started.Run.FencingGeneration,
				PlanRevision:      planned.Plan.PlanRevision, PlanHash: planned.Receipt.PlanHash,
				IdempotencyKey: "successor-rollback-apply",
			})
			if err != nil {
				t.Fatal(err)
			}
			heads, _ := memoryCurationProducedHeads(applied.Receipt.ActionResults)
			if len(heads) != 2 {
				t.Fatalf("produced heads = %d, want 2", len(heads))
			}
			cursors := rollbackContentionCursors(ctx, t, st, p)
			var advanced bool
			for _, position := range cursors {
				advanced = advanced || position > 0
			}
			if !advanced {
				t.Fatal("fixture must have a nonempty committed cursor map with a positive position")
			}
			in := RollbackMemoryCurationInput{
				ApplyReceiptID: applied.Receipt.ID, ExpectedProducedHeads: heads,
				Reason: "verify historical rollback against a successor", IdempotencyKey: "successor-rollback",
			}
			var successor StartMemoryCurationResult
			if replay {
				rolledBack, err := st.RollbackCuration(ctx, p, started.Run.ID, in)
				if err != nil {
					t.Fatal(err)
				}
				if rolledBack.Receipt.ID == "" || rolledBack.Receipt.Replayed ||
					len(rolledBack.Receipt.ActionResults) != 2 || !rolledBack.ReplayRequest.ReadOnlyReplay ||
					rolledBack.ReplayRequest.ReplayRunID != started.Run.ID {
					t.Fatal("initial rollback omitted its original compensation receipt or read-only replay")
				}
				successor, err = st.StartCuration(ctx, p, StartMemoryCurationInput{
					RequestID: rolledBack.ReplayRequest.ID, LeaseDuration: 5 * time.Minute,
					IdempotencyKey: "successor-replay-start",
				})
				if err != nil {
					t.Fatal(err)
				}
				ownerBefore := rollbackContentionOwnerSnapshot(ctx, t, st, p)
				historyBefore := successorRollbackHistory(ctx, t, st, p, started.Run.ID)
				retried, err := st.RollbackCuration(ctx, p, started.Run.ID, in)
				if err != nil {
					t.Fatalf("exact rollback retry must replay while successor is active: %v", err)
				}
				if !retried.Receipt.Replayed {
					t.Fatal("exact rollback retry returned a new receipt")
				}
				// The receipt is immutable; its replay request is returned in its
				// current claimed state after the read-only successor has started.
				retried.Receipt.Replayed = false
				if !reflect.DeepEqual(retried.Receipt, rolledBack.Receipt) ||
					!reflect.DeepEqual(retried.Run, rolledBack.Run) ||
					!reflect.DeepEqual(retried.ReplayRequest, successor.Request) {
					t.Fatal("exact retry changed its durable receipt, historical run, or current replay request")
				}
				if rollbackContentionOwnerSnapshot(ctx, t, st, p) != ownerBefore ||
					successorRollbackHistory(ctx, t, st, p, started.Run.ID) != historyBefore {
					t.Fatal("exact rollback retry changed owner state, history, compensation, or receipts")
				}
			} else {
				requested, err := st.RequestCuration(ctx, p, RequestMemoryCurationInput{
					CoalescingKey: "successor-overlap", TriggerReason: "manual_refine",
					IdempotencyKey: "successor-overlap-request",
				})
				if err != nil {
					t.Fatal(err)
				}
				historyBefore := successorRollbackHistory(ctx, t, st, p, started.Run.ID)
				blocker, err := st.pool.Begin(ctx)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = blocker.Rollback(context.Background()) }()
				var blockerPID int
				if err := blocker.QueryRow(ctx, `
					SELECT pg_backend_pid() FROM memory_curation_requests WHERE id=$1 FOR UPDATE`,
					requested.Request.ID).Scan(&blockerPID); err != nil {
					t.Fatal(err)
				}
				startName, rollbackName := "successor_start_"+started.Run.ID, "successor_rollback_"+started.Run.ID
				startWorker := successorRollbackWorker(ctx, t, st, startName)
				rollbackWorker := successorRollbackWorker(ctx, t, st, rollbackName)
				type startOutcome struct {
					result StartMemoryCurationResult
					err    error
				}
				startedResults := make(chan startOutcome, 1)
				rollbackResults := make(chan error, 1)
				go func() {
					out, err := startWorker.StartCuration(ctx, p, StartMemoryCurationInput{
						RequestID: requested.Request.ID, LeaseDuration: 5 * time.Minute,
						IdempotencyKey: "successor-overlap-start",
					})
					startedResults <- startOutcome{result: out, err: err}
				}()
				// Start owns the lane before it blocks on the held request row.
				// Then rollback must join that blocker chain before either commits.
				waitForRollbackContention(ctx, t, st, blockerPID, []string{startName})
				go func() {
					_, err := rollbackWorker.RollbackCuration(ctx, p, started.Run.ID, in)
					rollbackResults <- err
				}()
				waitForRollbackContention(ctx, t, st, blockerPID, []string{startName, rollbackName})
				if err := blocker.Commit(ctx); err != nil {
					t.Fatal(err)
				}
				select {
				case got := <-startedResults:
					if got.err != nil {
						t.Fatalf("successor failed after owning the lane first: %v", got.err)
					}
					successor = got.result
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
				select {
				case err := <-rollbackResults:
					if !errors.Is(err, ErrMemoryCurationBusy) {
						t.Fatalf("historical rollback with active successor error = %v, want Busy", err)
					}
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
				if successorRollbackHistory(ctx, t, st, p, started.Run.ID) != historyBefore {
					t.Fatal("refused historical rollback changed applied outputs, history, clock, or receipts")
				}
			}

			assertSuccessorRollbackState(ctx, t, st, p, successor, started.Run.ID, source.Memory.ID, createdID, replay)
			if !reflect.DeepEqual(rollbackContentionCursors(ctx, t, st, p), cursors) {
				t.Fatal("successor start or historical rollback changed committed cursors")
			}
		})
	}
}

func successorRollbackWorker(ctx context.Context, t *testing.T, st *Store, name string) *Store {
	t.Helper()
	// Dedicated worker pools leave the fixture pool free to observe blockers
	// and state. Buffered result channels and the test context bound cleanup.
	cfg, err := pgxpool.ParseConfig(st.dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["application_name"] = name
	cfg.MaxConns = 1
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return &Store{pool: pool}
}

func assertSuccessorRollbackState(ctx context.Context, t *testing.T, st *Store, p Principal,
	successor StartMemoryCurationResult, historicalRun, sourceID, createdID string, rolledBack bool,
) {
	t.Helper()
	if successor.Run.ID == "" || successor.Run.State != MemoryCurationRunOpen ||
		successor.Run.FencingGeneration < 1 || successor.Run.LeaseExpiresAt == nil ||
		successor.Request.State != MemoryCurationRequestClaimed ||
		successor.Request.ClaimedRunID != successor.Run.ID || successor.Request.ReadOnlyReplay != rolledBack {
		t.Fatal("successor did not retain an open fenced lease and its claimed request")
	}
	run, err := st.GetCurationRun(ctx, p, successor.Run.ID)
	if err != nil || !reflect.DeepEqual(run, successor.Run) {
		t.Fatalf("durable successor run or exact fence/lease changed: %v", err)
	}
	request, err := st.GetCurationRequest(ctx, p, successor.Request.ID)
	if err != nil || !reflect.DeepEqual(request, successor.Request) {
		t.Fatalf("durable successor request changed: %v", err)
	}
	var activeRun string
	var generation, fence int64
	if err := st.pool.QueryRow(ctx, `
		SELECT active_run_id,request_generation,fencing_generation FROM memory_curation_lanes
		WHERE account_id=$1 AND realm_id=$2 AND owner_kind='agent' AND owner_id=$3`,
		p.AccountID, p.RealmID, p.ID).Scan(&activeRun, &generation, &fence); err != nil {
		t.Fatal(err)
	}
	if activeRun != successor.Run.ID || fence != successor.Run.FencingGeneration ||
		generation != successor.Request.RequestGeneration {
		t.Fatal("historical rollback changed active successor ownership or owner generation")
	}
	var sourceVersions, createdVersions, compensations, revertedActions, mutations, replays, events, active, actualActive int
	if err := st.pool.QueryRow(ctx, `
		SELECT
		  (SELECT count(*) FROM memory_versions WHERE memory_id=$4),
		  (SELECT count(*) FROM memory_versions WHERE memory_id=$5),
		  (SELECT count(*) FROM memory_versions WHERE curation_run_id=$6 AND operation='reverted'),
		  (SELECT count(*) FROM memory_curation_actions WHERE run_id=$6 AND state='reverted'),
		  (SELECT count(*) FROM memory_curation_mutations WHERE run_id=$6 AND operation='rollback'),
		  (SELECT count(*) FROM memory_curation_requests WHERE replay_run_id=$6 AND read_only_replay),
		  (SELECT count(*) FROM account_events WHERE account_id=$1 AND verb=$7 AND metadata->>'run_id'=$6),
		  (SELECT active_memory_count FROM memory_change_clocks
		    WHERE account_id=$1 AND realm_id=$2 AND owner_kind='agent' AND owner_id=$3),
		  (SELECT count(*) FROM memories m JOIN memory_versions v ON v.memory_id=m.id AND v.version=m.current_version
		    WHERE m.account_id=$1 AND m.realm_id=$2 AND m.owner_kind='agent' AND m.owner_id=$3 AND v.state='active')`,
		p.AccountID, p.RealmID, p.ID, sourceID, createdID, historicalRun, VerbMemoryCurationRolledBack).
		Scan(&sourceVersions, &createdVersions, &compensations, &revertedActions, &mutations, &replays, &events, &active, &actualActive); err != nil {
		t.Fatal(err)
	}
	n := 0
	if rolledBack {
		n = 1
	}
	want := []int{2 + n, 1 + n, 2 * n, 2 * n, n, n, n, 2 - n, 2 - n}
	got := []int{sourceVersions, createdVersions, compensations, revertedActions, mutations, replays, events, active, actualActive}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("durable versions/compensations/actions/mutations/replays/events/active counts = %v, want %v", got, want)
	}
}

// Start legitimately changes its new run/request and lane, but cannot change
// any historical output or receipt. The #400 owner snapshot separately covers
// all lanes, requests, runs and cursors around an observational receipt retry.
func successorRollbackHistory(ctx context.Context, t *testing.T, st *Store, p Principal, runID string) string {
	t.Helper()
	var snapshot string
	if err := st.pool.QueryRow(ctx, `
		SELECT jsonb_build_object(
		  'memories',(SELECT jsonb_agg(to_jsonb(m) ORDER BY id) FROM memories m
		    WHERE account_id=$1 AND realm_id=$2 AND owner_kind='agent' AND owner_id=$3),
		  'versions',(SELECT jsonb_agg(to_jsonb(v) ORDER BY memory_id,version) FROM memory_versions v
		    WHERE account_id=$1 AND realm_id=$2 AND owner_kind='agent' AND owner_id=$3),
		  'clock',(SELECT to_jsonb(c) FROM memory_change_clocks c
		    WHERE account_id=$1 AND realm_id=$2 AND owner_kind='agent' AND owner_id=$3),
		  'run',(SELECT to_jsonb(r) FROM memory_curation_runs r WHERE id=$4),
		  'request',(SELECT to_jsonb(r) FROM memory_curation_requests r
		    WHERE id=(SELECT request_id FROM memory_curation_runs WHERE id=$4)),
		  'inputs',(SELECT jsonb_agg(to_jsonb(i) ORDER BY ordinal) FROM memory_curation_run_inputs i WHERE run_id=$4),
		  'actions',(SELECT jsonb_agg(to_jsonb(a) ORDER BY ordinal) FROM memory_curation_actions a WHERE run_id=$4),
		  'mutations',(SELECT jsonb_agg(to_jsonb(m) ORDER BY id) FROM memory_curation_mutations m WHERE run_id=$4),
		  'events',(SELECT jsonb_agg(to_jsonb(e) ORDER BY id) FROM account_events e
		    WHERE account_id=$1 AND metadata->>'run_id'=$4)
		)::text`, p.AccountID, p.RealmID, p.ID, runID).Scan(&snapshot); err != nil {
		t.Fatal(err)
	}
	return snapshot
}
