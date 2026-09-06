package store

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/witwave-ai/witself/internal/testenv"
)

// A held owner lane makes every rollback overlap before any can commit. The
// barrier observes PostgreSQL's transitive blocker graph: identical-key retries
// may wait behind another caller rather than directly behind the held lane.
// Assertions below concern durable outcomes, independent of that lock order.
func TestMemoryCurationRollbackContentionPostgres(t *testing.T) {
	dsn := testenv.RequirePostgres(t)
	for _, sameKey := range []bool{true, false} {
		name := "competing_keys"
		if sameKey {
			name = "same_key_retries"
		}
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			st, p, source, started, planned, createdID := prepareMemoryCurationConflictPlan(ctx, t, dsn, true)
			applied, err := st.ApplyCuration(ctx, p, started.Run.ID, ApplyMemoryCurationInput{
				FencingGeneration: started.Run.FencingGeneration,
				PlanRevision:      planned.Plan.PlanRevision, PlanHash: planned.Receipt.PlanHash,
				IdempotencyKey: "rollback-contention-apply",
			})
			if err != nil {
				t.Fatal(err)
			}
			heads, _ := memoryCurationProducedHeads(applied.Receipt.ActionResults)
			if len(heads) != 2 {
				t.Fatalf("produced heads = %d, want 2", len(heads))
			}
			beforeCursors := rollbackContentionCursors(ctx, t, st, p)
			var advanced bool
			for _, position := range beforeCursors {
				advanced = advanced || position > 0
			}
			if !advanced {
				t.Fatal("fixture must have a committed nonzero cursor before rollback")
			}
			var beforeGeneration int64
			if err := st.pool.QueryRow(ctx, `
				SELECT request_generation FROM memory_curation_lanes
				WHERE account_id=$1 AND realm_id=$2 AND owner_kind='agent' AND owner_id=$3`,
				p.AccountID, p.RealmID, p.ID).Scan(&beforeGeneration); err != nil {
				t.Fatal(err)
			}

			// Use an owner in the same realm to catch accidental account-wide
			// serialization as well as changes to someone else's curation state.
			otherAgent, err := st.CreateAgent(ctx, p.AccountID, p.RealmID, "rollback-bystander")
			if err != nil {
				t.Fatal(err)
			}
			other := p
			other.ID = otherAgent.ID
			otherRequest, err := st.RequestCuration(ctx, other, RequestMemoryCurationInput{
				CoalescingKey: "rollback-bystander", TriggerReason: "manual_refine",
				IdempotencyKey: "rollback-bystander-request",
			})
			if err != nil {
				t.Fatal(err)
			}
			otherStarted, err := st.StartCuration(ctx, other, StartMemoryCurationInput{
				RequestID: otherRequest.Request.ID, LeaseDuration: 5 * time.Minute,
				IdempotencyKey: "rollback-bystander-start",
			})
			if err != nil {
				t.Fatal(err)
			}

			blocker, err := st.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = blocker.Rollback(context.Background()) }()
			var blockerPID int
			if err := blocker.QueryRow(ctx, `
				SELECT pg_backend_pid() FROM memory_curation_lanes
				WHERE account_id=$1 AND realm_id=$2 AND owner_kind='agent' AND owner_id=$3
				FOR UPDATE`, p.AccountID, p.RealmID, p.ID).Scan(&blockerPID); err != nil {
				t.Fatal(err)
			}

			const callers = 3
			type outcome struct {
				result RollbackMemoryCurationResult
				err    error
			}
			results := make(chan outcome, callers)
			ready := make(chan struct{})
			workerNames := make([]string, callers)
			for i := range callers {
				// Separate one-connection pools keep blocked callers from starving
				// the observer or the unrelated owner's progress probe.
				cfg, err := pgxpool.ParseConfig(st.dsn)
				if err != nil {
					t.Fatal(err)
				}
				workerNames[i] = fmt.Sprintf("rollback_%s_%d", started.Run.ID, i)
				cfg.ConnConfig.RuntimeParams["application_name"] = workerNames[i]
				cfg.MaxConns = 1
				pool, err := pgxpool.NewWithConfig(ctx, cfg)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(pool.Close)
				worker := &Store{pool: pool}
				key := "rollback-contention"
				if !sameKey {
					key = fmt.Sprintf("%s-%d", key, i)
				}
				go func() {
					select {
					case <-ready:
					case <-ctx.Done():
						results <- outcome{err: ctx.Err()}
						return
					}
					result, err := worker.RollbackCuration(ctx, p, started.Run.ID, RollbackMemoryCurationInput{
						ApplyReceiptID: applied.Receipt.ID, ExpectedProducedHeads: heads,
						Reason: "verify compensation under contention", IdempotencyKey: key,
					})
					results <- outcome{result: result, err: err}
				}()
			}
			close(ready)
			waitForRollbackContention(ctx, t, st, blockerPID, workerNames)

			probeCtx, cancelProbe := context.WithTimeout(ctx, 15*time.Second)
			otherMemory, err := st.CaptureMemory(probeCtx, other, CaptureMemoryInput{
				Content: "Unrelated owner continues during rollback contention.", Kind: "observation",
				Evidence: []MemoryEvidenceInput{{
					Type: "system", ResolutionState: MemoryEvidenceUnavailable, TerminalReasonCode: "not_recorded",
				}},
				IdempotencyKey: "rollback-bystander-progress",
			})
			cancelProbe()
			if err != nil {
				t.Fatalf("unrelated owner could not make progress while rollbacks were blocked: %v", err)
			}
			otherBefore := rollbackContentionOwnerSnapshot(ctx, t, st, other)
			if err := blocker.Commit(ctx); err != nil {
				t.Fatal(err)
			}

			var successful []RollbackMemoryCurationResult
			conflicts, originals := 0, 0
			for range callers {
				select {
				case got := <-results:
					if got.err != nil {
						if !sameKey && errors.Is(got.err, ErrMemoryCurationConflict) {
							conflicts++
							continue
						}
						t.Fatalf("rollback caller failed: %v", got.err)
					}
					if !got.result.Receipt.Replayed {
						originals++
					}
					successful = append(successful, got.result)
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
			}
			wantSuccess, wantConflicts := 1, callers-1
			if sameKey {
				wantSuccess, wantConflicts = callers, 0
			}
			if len(successful) != wantSuccess || conflicts != wantConflicts || originals != 1 {
				t.Fatalf("rollback success/conflict/original counts = %d/%d/%d, want %d/%d/1",
					len(successful), conflicts, originals, wantSuccess, wantConflicts)
			}
			first := successful[0]
			if first.Receipt.ID == "" || first.ReplayRequest.ID == "" ||
				first.Run.State != MemoryCurationRunRolledBack || !first.ReplayRequest.ReadOnlyReplay ||
				first.ReplayRequest.ReplayRunID != started.Run.ID || len(first.Receipt.ActionResults) != 2 {
				t.Fatal("rollback omitted the completed run, compensation receipt, or read-only replay request")
			}
			for _, got := range successful {
				if got.Receipt.ID != first.Receipt.ID || got.ReplayRequest.ID != first.ReplayRequest.ID ||
					!reflect.DeepEqual(got.Receipt.ActionResults, first.Receipt.ActionResults) {
					t.Fatal("concurrent retries did not return the same durable compensation")
				}
			}
			restored, err := st.GetMemory(ctx, p, source.Memory.ID)
			if err != nil || restored.Version != 3 || restored.State != MemoryStateActive ||
				restored.Content != source.Memory.Content || restored.Operation != "reverted" {
				t.Fatalf("replacement was not restored exactly once: %v", err)
			}
			created, err := st.GetMemory(ctx, p, createdID)
			if err != nil || created.Version != 2 || created.State != MemoryStateReverted || created.Operation != "reverted" {
				t.Fatalf("created memory was not compensated exactly once: %v", err)
			}
			var sourceVersions, createdVersions, mutations, replays, events, active int
			var generation int64
			if err := st.pool.QueryRow(ctx, `
				SELECT
				  (SELECT count(*) FROM memory_versions WHERE memory_id=$4),
				  (SELECT count(*) FROM memory_versions WHERE memory_id=$5),
				  (SELECT count(*) FROM memory_curation_mutations WHERE run_id=$6 AND operation='rollback'),
				  (SELECT count(*) FROM memory_curation_requests WHERE replay_run_id=$6 AND read_only_replay),
				  (SELECT count(*) FROM account_events WHERE account_id=$1 AND verb=$7 AND metadata->>'run_id'=$6),
				  (SELECT active_memory_count FROM memory_change_clocks
				    WHERE account_id=$1 AND realm_id=$2 AND owner_kind='agent' AND owner_id=$3),
				  (SELECT request_generation FROM memory_curation_lanes
				    WHERE account_id=$1 AND realm_id=$2 AND owner_kind='agent' AND owner_id=$3)`,
				p.AccountID, p.RealmID, p.ID, source.Memory.ID, createdID, started.Run.ID,
				VerbMemoryCurationRolledBack).Scan(&sourceVersions, &createdVersions, &mutations,
				&replays, &events, &active, &generation); err != nil {
				t.Fatal(err)
			}
			if sourceVersions != 3 || createdVersions != 2 || mutations != 1 || replays != 1 ||
				events != 1 || active != 1 || generation != beforeGeneration+1 {
				t.Fatalf("durable version/mutation/replay/event/active/generation counts = %d/%d/%d/%d/%d/%d/%d, want 3/2/1/1/1/1/%d",
					sourceVersions, createdVersions, mutations, replays, events, active, generation, beforeGeneration+1)
			}
			if !reflect.DeepEqual(rollbackContentionCursors(ctx, t, st, p), beforeCursors) {
				t.Fatal("rollback contention changed committed source cursors")
			}
			if after := rollbackContentionOwnerSnapshot(ctx, t, st, other); after != otherBefore {
				t.Fatal("target rollback changed the unrelated owner's durable memory or curation state")
			}
			otherRun, err := st.GetCurationRun(ctx, other, otherStarted.Run.ID)
			if err != nil || otherRun.State != MemoryCurationRunOpen ||
				otherRun.FencingGeneration != otherStarted.Run.FencingGeneration ||
				otherRun.LeaseExpiresAt == nil || otherStarted.Run.LeaseExpiresAt == nil ||
				!otherRun.LeaseExpiresAt.Equal(*otherStarted.Run.LeaseExpiresAt) {
				t.Fatalf("unrelated curator lost its live fence or lease: %v", err)
			}
			unchanged, err := st.GetMemory(ctx, other, otherMemory.Memory.ID)
			if err != nil || unchanged.Version != 1 || unchanged.Content != otherMemory.Memory.Content {
				t.Fatalf("unrelated memory changed after rollback: %v", err)
			}
		})
	}
}

func waitForRollbackContention(ctx context.Context, t *testing.T, st *Store, blockerPID int, names []string) {
	t.Helper()
	waitCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	for {
		var blocked int
		err := st.pool.QueryRow(waitCtx, `
			WITH RECURSIVE blocked(pid) AS (
			  SELECT $1::int
			  UNION
			  SELECT a.pid FROM pg_stat_activity a
			  JOIN blocked b ON b.pid=ANY(pg_blocking_pids(a.pid))
			  WHERE a.datname=current_database()
			)
			SELECT count(*) FROM pg_stat_activity a
			WHERE a.datname=current_database() AND a.application_name=ANY($2::text[])
			  AND a.wait_event_type='Lock' AND a.pid IN (SELECT pid FROM blocked)`,
			blockerPID, names).Scan(&blocked)
		if err != nil {
			t.Fatalf("observe all %d overlapping rollback calls: %v", len(names), err)
		}
		if blocked == len(names) {
			return
		}
	}
}

func rollbackContentionCursors(ctx context.Context, t *testing.T, st *Store, p Principal) map[string]int64 {
	t.Helper()
	rows, err := st.pool.Query(ctx, `
		SELECT source_kind,source_stream_id,position FROM memory_curation_cursors
		WHERE account_id=$1 AND realm_id=$2 AND owner_kind='agent' AND owner_id=$3`,
		p.AccountID, p.RealmID, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	positions := make(map[string]int64)
	for rows.Next() {
		var kind, stream string
		var position int64
		if err := rows.Scan(&kind, &stream, &position); err != nil {
			t.Fatal(err)
		}
		positions[kind+"\x00"+stream] = position
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return positions
}

func rollbackContentionOwnerSnapshot(ctx context.Context, t *testing.T, st *Store, p Principal) string {
	t.Helper()
	var snapshot string
	if err := st.pool.QueryRow(ctx, `
		SELECT jsonb_build_object(
		  'memories',(SELECT jsonb_agg(to_jsonb(m) ORDER BY id) FROM memories m
		    WHERE account_id=$1 AND realm_id=$2 AND owner_kind='agent' AND owner_id=$3),
		  'clock',(SELECT to_jsonb(c) FROM memory_change_clocks c
		    WHERE account_id=$1 AND realm_id=$2 AND owner_kind='agent' AND owner_id=$3),
		  'lane',(SELECT to_jsonb(l) FROM memory_curation_lanes l
		    WHERE account_id=$1 AND realm_id=$2 AND owner_kind='agent' AND owner_id=$3),
		  'cursors',(SELECT jsonb_agg(to_jsonb(c) ORDER BY source_kind,source_stream_id) FROM memory_curation_cursors c
		    WHERE account_id=$1 AND realm_id=$2 AND owner_kind='agent' AND owner_id=$3),
		  'requests',(SELECT jsonb_agg(to_jsonb(r) ORDER BY id) FROM memory_curation_requests r
		    WHERE account_id=$1 AND realm_id=$2 AND owner_kind='agent' AND owner_id=$3),
		  'runs',(SELECT jsonb_agg(to_jsonb(r) ORDER BY id) FROM memory_curation_runs r
		    WHERE account_id=$1 AND realm_id=$2 AND owner_kind='agent' AND owner_id=$3)
		)::text`, p.AccountID, p.RealmID, p.ID).Scan(&snapshot); err != nil {
		t.Fatal(err)
	}
	return snapshot
}
