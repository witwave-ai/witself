package store

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/witwave-ai/witself/internal/syncmodel"
)

// Original actions and both derived buckets are written in the caller's domain
// transaction. Duplicate/replay work never creates another original event.
func syncHarnessRecordAction(ctx context.Context, tx pgx.Tx, account string, operation []byte, class string) error {
	var inserted bool
	err := tx.QueryRow(ctx, `WITH added AS (
 INSERT INTO h_logical_actions(account_id,operation_id,action_class) VALUES($1,$2,$3)
 ON CONFLICT DO NOTHING RETURNING 1) SELECT EXISTS(SELECT 1 FROM added)`, account, operation, class).Scan(&inserted)
	if err != nil {
		return errors.New("record fixture action failed")
	}
	if !inserted {
		var same bool
		if err := tx.QueryRow(ctx, `SELECT action_class=$3 FROM h_logical_actions WHERE account_id=$1 AND operation_id=$2 AND action_ordinal=1`, account, operation, class).Scan(&same); err != nil || !same {
			return errors.New("fixture action identity conflict")
		}
		return nil
	}
	_, err = tx.Exec(ctx, `INSERT INTO h_rollups(account_id,actor,dimension,bucket,bucket_start,count,quantity)
 SELECT account_id,actor,action_class,b,date_trunc(b,occurred_at AT TIME ZONE 'UTC') AT TIME ZONE 'UTC',1,quantity
 FROM h_logical_actions CROSS JOIN (VALUES('hour'),('day')) buckets(b)
 WHERE account_id=$1 AND operation_id=$2 AND action_ordinal=1
 ON CONFLICT(account_id,actor,dimension,bucket,bucket_start) DO UPDATE
 SET count=h_rollups.count+EXCLUDED.count,quantity=h_rollups.quantity+EXCLUDED.quantity`, account, operation)
	if err != nil {
		return errors.New("roll up fixture action failed")
	}
	return nil
}

func syncHarnessRebuildRollups(ctx context.Context, tx pgx.Tx, account string) error {
	// Serializes original writers as well as competing rebuilds.
	if _, err := tx.Exec(ctx, `LOCK TABLE h_logical_actions IN SHARE ROW EXCLUSIVE MODE`); err != nil {
		return errors.New("lock fixture rebuild failed")
	}
	if _, err := tx.Exec(ctx, `DELETE FROM h_rollups WHERE account_id=$1`, account); err != nil {
		return errors.New("clear derived fixture rollups failed")
	}
	_, err := tx.Exec(ctx, `INSERT INTO h_rollups(account_id,actor,dimension,bucket,bucket_start,count,quantity)
 SELECT account_id,actor,action_class,b,date_trunc(b,occurred_at AT TIME ZONE 'UTC') AT TIME ZONE 'UTC',count(*),sum(quantity)
 FROM h_logical_actions CROSS JOIN (VALUES('hour'),('day')) buckets(b) WHERE account_id=$1
 GROUP BY account_id,actor,action_class,b,date_trunc(b,occurred_at AT TIME ZONE 'UTC')`, account)
	if err != nil {
		return errors.New("rebuild derived fixture rollups failed")
	}
	return nil
}

type syncHarnessAccountingVector struct {
	Name        string `json:"name"`
	Class       string `json:"action_class"`
	Events      int    `json:"original_events"`
	Hour        int    `json:"hour_count"`
	Day         int    `json:"day_count"`
	Replay      int    `json:"replay_work"`
	DeltaEvents int    `json:"delta_original_events"`
	DeltaHour   int    `json:"delta_hour_count"`
	DeltaDay    int    `json:"delta_day_count"`
}

func TestSyncHarnessLogicalAccountingMatrix(t *testing.T) {
	top := newSyncHarnessTopology(t)
	node := top.nodes[2]
	ctx := context.Background()
	data, err := os.ReadFile("../syncharness/testdata/accounting_local.v1.json")
	if err != nil {
		t.Fatal("read miniature accounting catalog failed")
	}
	var catalog struct {
		Version int                           `json:"version"`
		Entries []syncHarnessAccountingVector `json:"entries"`
	}
	if err := json.Unmarshal(data, &catalog); err != nil || catalog.Version != 1 || len(catalog.Entries) != 11 {
		t.Fatal("invalid miniature accounting catalog")
	}
	operation := make([]byte, 16)
	operation[0] = 61
	previousEvents, previousHour, previousDay := 0, 0, 0
	expectedDimensions := make(map[string]bool)
	for _, vector := range catalog.Entries {
		t.Run(vector.Name, func(t *testing.T) {
			expectedClass := "fixture_mutation"
			switch vector.Name {
			case "audited_read":
				expectedClass = "fixture_audited_read"
			case "compound":
				expectedClass = "fixture_compound"
			case "refusal":
				expectedClass = "fixture_conflict"
			}
			if vector.Class != expectedClass {
				t.Fatal("catalog action class differs from miniature operation")
			}
			tx, err := node.store.pool.Begin(ctx)
			if err != nil {
				t.Fatal("begin accounting fixture failed")
			}
			defer func() { _ = tx.Rollback(ctx) }()
			switch vector.Name {
			case "original_mutation", "snapshot", "replay", "duplicate", "lost_ack", "regroup":
				if err := syncHarnessRecordAction(ctx, tx, syncHarnessAccount, operation, vector.Class); err != nil {
					t.Fatal(err)
				}
				if vector.Name != "original_mutation" {
					if _, err := tx.Exec(ctx, `INSERT INTO h_replay_work(account_id,event_id,operation_id,node,reason) VALUES($1,$2,$3,'R',$2)`, syncHarnessAccount, vector.Name, operation); err != nil {
						t.Fatal("record replay work failed")
					}
				}
			case "rebuild":
				if err := syncHarnessRebuildRollups(ctx, tx, syncHarnessAccount); err != nil {
					t.Fatal(err)
				}
			case "refusal":
				keys := newSyncHarnessKeys()
				seedSyncHarnessAdmission(t, node, keys)
				seedSyncHarnessCapture(t, node)
				envelope := syncHarnessEnvelope(syncHarnessAccount)
				envelope.ID = syncmodel.OperationID{64}
				outcome, err := syncHarnessApply(ctx, node, keys, envelope, true, "")
				if err != nil || outcome.Status != syncmodel.StatusConflict {
					t.Fatal("catalog refusal did not commit immutable conflict receipt")
				}
			case "passive_read":
				var n int
				if err := tx.QueryRow(ctx, `SELECT count(*) FROM h_fixture_objects`).Scan(&n); err != nil {
					t.Fatal("read projection failed")
				}
			case "audited_read":
				audited := make([]byte, 16)
				audited[0] = 62
				if err := syncHarnessRecordAction(ctx, tx, syncHarnessAccount, audited, vector.Class); err != nil {
					t.Fatal(err)
				}
			case "compound":
				compound := make([]byte, 16)
				compound[0] = 63
				if err := syncHarnessRecordAction(ctx, tx, syncHarnessAccount, compound, vector.Class); err != nil {
					t.Fatal(err)
				}
			default:
				t.Fatal("unimplemented miniature accounting vector")
			}
			if err := tx.Commit(ctx); err != nil {
				t.Fatal("commit accounting fixture failed")
			}
			var events, hour, day, replay int
			if err := node.store.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM h_logical_actions WHERE account_id=$1),
   (SELECT COALESCE(sum(count),0) FROM h_rollups WHERE account_id=$1 AND bucket='hour'),
   (SELECT COALESCE(sum(count),0) FROM h_rollups WHERE account_id=$1 AND bucket='day'),
   (SELECT count(*) FROM h_replay_work WHERE account_id=$1)`, syncHarnessAccount).Scan(&events, &hour, &day, &replay); err != nil {
				t.Fatal("observe accounting vector failed")
			}
			if events != vector.Events || hour != vector.Hour || day != vector.Day || replay != vector.Replay {
				t.Fatalf("count vector got %d/%d/%d/%d want %d/%d/%d/%d", events, hour, day, replay, vector.Events, vector.Hour, vector.Day, vector.Replay)
			}
			if events-previousEvents != vector.DeltaEvents || hour-previousHour != vector.DeltaHour || day-previousDay != vector.DeltaDay {
				t.Fatal("per-action catalog vector differs from event/hour/day changes")
			}
			previousEvents, previousHour, previousDay = events, hour, day
			if vector.DeltaEvents > 0 {
				expectedDimensions[vector.Class] = true
			}
			rows, err := node.store.pool.Query(ctx, `SELECT actor,dimension,bucket,bucket_start,count,quantity FROM h_rollups WHERE account_id=$1`, syncHarnessAccount)
			if err != nil {
				t.Fatal("inspect exact rollup rows failed")
			}
			seen := make(map[string]bool)
			for rows.Next() {
				var actor, dimension, bucket string
				var start time.Time
				var count, quantity int
				if err := rows.Scan(&actor, &dimension, &bucket, &start, &count, &quantity); err != nil {
					rows.Close()
					t.Fatal("decode rollup row failed")
				}
				wantStart := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
				if bucket == "hour" {
					wantStart = wantStart.Add(12 * time.Hour)
				}
				key := dimension + "/" + bucket
				if actor != "fixture_actor" || !expectedDimensions[dimension] || (bucket != "hour" && bucket != "day") || !start.Equal(wantStart) || count != 1 || quantity != 1 || seen[key] {
					rows.Close()
					t.Fatal("rollup provenance, UTC bucket or quantity differs")
				}
				seen[key] = true
			}
			rowErr := rows.Err()
			rows.Close()
			if rowErr != nil || len(seen) != 2*len(expectedDimensions) {
				t.Fatal("exact hour/day rollup coverage differs")
			}
		})
	}
	// A different account reusing the same operation remains independent.
	tx, err := node.store.pool.Begin(ctx)
	if err != nil {
		t.Fatal("begin isolated accounting failed")
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := syncHarnessRecordAction(ctx, tx, "h_adversary_R", operation, "fixture_mutation"); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal("commit isolated accounting failed")
	}
	var count int
	if err := node.store.pool.QueryRow(ctx, `SELECT count(*) FROM h_logical_actions WHERE account_id=$1`, syncHarnessAccount).Scan(&count); err != nil || count != 3 {
		t.Fatal("accounting crossed account scope")
	}
	// Altered identity must fail rather than silently be counted as a retry.
	tx, err = node.store.pool.Begin(ctx)
	if err != nil {
		t.Fatal("begin conflicting accounting failed")
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := syncHarnessRecordAction(ctx, tx, syncHarnessAccount, operation, "fixture_audited_read"); err == nil {
		t.Fatal("altered original action accepted")
	}
	if !t.Failed() {
		t.Log("POSTGRES_PASS: catalog count vectors across original/snapshot/replay/duplicate/lost-ACK/rebuild/regroup/passive/audited/compound/refusal; separate replay ledger and account isolation")
	}
}

func TestSyncHarnessConcurrentRollupRebuild(t *testing.T) {
	top := newSyncHarnessTopology(t)
	node := top.nodes[2]
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	seed, err := node.store.pool.Begin(ctx)
	if err != nil {
		t.Fatal("begin rebuild fixture seed failed")
	}
	defer func() { _ = seed.Rollback(context.Background()) }()
	operation := make([]byte, 16)
	operation[0] = 65
	if err := syncHarnessRecordAction(ctx, seed, syncHarnessAccount, operation, "fixture_mutation"); err != nil {
		t.Fatal(err)
	}
	if err := seed.Commit(ctx); err != nil {
		t.Fatal("commit rebuild fixture seed failed")
	}
	tx1, err := node.store.pool.Begin(ctx)
	if err != nil {
		t.Fatal("begin first rollup rebuild failed")
	}
	defer func() { _ = tx1.Rollback(context.Background()) }()
	if err := syncHarnessRebuildRollups(ctx, tx1, syncHarnessAccount); err != nil {
		t.Fatal(err)
	}
	// The worker owns its transaction. The observer receives only its fixture
	// backend ID, then verifies the table-lock wait before releasing tx1.
	started := make(chan int32, 1)
	done := make(chan error, 1)
	go func() {
		tx2, err := node.store.pool.Begin(ctx)
		if err != nil {
			done <- errors.New("begin competing rollup rebuild failed")
			return
		}
		defer func() { _ = tx2.Rollback(context.Background()) }()
		var pid int32
		if err := tx2.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
			done <- errors.New("identify competing rollup rebuild failed")
			return
		}
		started <- pid
		if err := syncHarnessRebuildRollups(ctx, tx2, syncHarnessAccount); err != nil {
			done <- err
			return
		}
		if err := tx2.Commit(ctx); err != nil {
			done <- errors.New("commit competing rollup rebuild failed")
			return
		}
		done <- nil
	}()
	var pid int32
	select {
	case pid = <-started:
	case <-done:
		t.Fatal("competing rollup rebuild ended before lock observation")
	case <-ctx.Done():
		t.Fatal("competing rollup rebuild did not start")
	}
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	for {
		var waiting bool
		if err := node.store.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_locks WHERE pid=$1 AND locktype='relation' AND relation='h_logical_actions'::regclass AND mode='ShareRowExclusiveLock' AND NOT granted)`, pid).Scan(&waiting); err != nil {
			t.Fatal("inspect competing fixture rebuild lock failed")
		}
		if waiting {
			break
		}
		select {
		case <-done:
			t.Fatal("competing rollup rebuild completed without table serialization")
		case <-ctx.Done():
			t.Fatal("competing rollup rebuild table-lock wait was not observed")
		case <-tick.C:
		}
	}
	if err := tx1.Commit(ctx); err != nil {
		t.Fatal("commit first rollup rebuild failed")
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal("competing rollup rebuild was not serialized successfully")
		}
	case <-ctx.Done():
		t.Fatal("competing rollup rebuild did not finish after first commit")
	}
	var events, hour, day int
	if err := node.store.pool.QueryRow(ctx, `SELECT
 (SELECT count(*) FROM h_logical_actions WHERE account_id=$1),
 (SELECT COALESCE(sum(count),0) FROM h_rollups WHERE account_id=$1 AND bucket='hour'),
 (SELECT COALESCE(sum(count),0) FROM h_rollups WHERE account_id=$1 AND bucket='day')`, syncHarnessAccount).Scan(&events, &hour, &day); err != nil || events != 1 || hour != 1 || day != 1 {
		t.Fatal("competing rebuilds changed original/hour/day count vector")
	}
	if !t.Failed() {
		t.Log("POSTGRES_PASS: competing rebuild waits on fixture-owned ShareRowExclusiveLock then succeeds; original/hour/day counts remain 1/1/1")
	}
}
