package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/syncharness"
	"github.com/witwave-ai/witself/internal/testenv"
)

// This miniature adapter deliberately models one bad lost-ACK retry. It does
// not implement synchronization. Every replay has a new schema and pool, while
// a restart within a replay opens the same schema and reads the retained trace.
type syncHarnessTraceAdapter struct {
	fixture  *testenv.PostgresSchema
	db       *sql.DB
	repaired bool
	restarts int
}

func (a *syncHarnessTraceAdapter) Apply(step syncharness.Step, payloads []syncharness.PayloadShape) (syncharness.Observation, error) {
	o := syncharness.Observation{Invariant: "no_fresh_action", Outcome: syncharness.ModelPass}
	for _, f := range step.Faults {
		o.Assertions = append(o.Assertions, f.Observable)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return o, errors.New("trace transaction failed")
	}
	defer func() { _ = tx.Rollback() }()
	raw, err := json.Marshal(step)
	if err != nil {
		return o, errors.New("trace encode failed")
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO h_recorded_traces(account_id,run_id,event_ordinal,scenario_version,canonical_schedule) VALUES($1,'B04', $2,1,$3)`, syncHarnessAccount, step.Ordinal, raw); err != nil {
		return o, errors.New("trace persist failed")
	}
	if step.Event.Operation == "prepare" {
		if _, err = tx.ExecContext(ctx, `INSERT INTO h_fixture_objects VALUES($1,'prepared','R','synthetic')`, syncHarnessAccount); err != nil {
			return o, errors.New("prepare failed")
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO h_provider_actions VALUES($1,'original')`, syncHarnessAccount); err != nil {
			return o, errors.New("original action failed")
		}
	}
	shape := slices.ContainsFunc(payloads, func(p syncharness.PayloadShape) bool {
		return p.Input == "request" && p.Elements > 0 && slices.Contains(p.Fields, "effect")
	})
	lost := slices.ContainsFunc(step.Faults, func(f syncharness.Fault) bool { return f.Class == syncharness.DelayedLostACK })
	if step.Event.Operation == "retry" && shape && lost {
		action := "fresh"
		if a.repaired {
			action = "original"
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO h_provider_actions SELECT $1,$2 WHERE EXISTS(SELECT 1 FROM h_fixture_objects WHERE account_id=$1 AND object_id='prepared') ON CONFLICT DO NOTHING`, syncHarnessAccount, action); err != nil {
			return o, errors.New("retry failed")
		}
	}
	if err = tx.Commit(); err != nil {
		return o, errors.New("trace commit failed")
	}
	if step.Event.Operation == "prepare" {
		var before, after int
		if a.db.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&before) != nil {
			return o, errors.New("session observation failed")
		}
		if a.db.Close() != nil {
			return o, errors.New("restart close failed")
		}
		a.db, err = sql.Open("pgx", a.fixture.DSN())
		if err != nil {
			return o, errors.New("restart open failed")
		}
		a.db.SetMaxOpenConns(1)
		if a.db.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&after) != nil || before == after {
			return o, errors.New("restart did not replace session")
		}
		var retained []byte
		if a.db.QueryRowContext(ctx, `SELECT canonical_schedule FROM h_recorded_traces WHERE account_id=$1 AND run_id='B04' AND event_ordinal=$2`, syncHarnessAccount, step.Ordinal).Scan(&retained) != nil || !bytes.Equal(raw, retained) {
			return o, errors.New("reopened trace changed")
		}
		a.restarts++
	}
	var actions int
	if a.db.QueryRowContext(ctx, `SELECT count(*) FROM h_provider_actions WHERE account_id=$1`, syncHarnessAccount).Scan(&actions) != nil {
		return o, errors.New("action observation failed")
	}
	if actions > 1 {
		o.Outcome = syncharness.Fail
	}
	return o, nil
}

func (a *syncHarnessTraceAdapter) Close() error {
	err := a.db.Close()
	return errors.Join(err, a.fixture.Close())
}

func TestSyncHarnessRecoveryFixtureReopenAndReducer(t *testing.T) {
	started := time.Now()
	top := newSyncHarnessTopology(t)
	node := top.nodes[2]
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Second)
	defer cancel()
	verifySyncHarnessRecoveryRows(ctx, t, top, node)
	dsn := testenv.RequirePostgres(t)
	var adapters []*syncHarnessTraceAdapter
	newAdapter := func(repaired bool) syncharness.ReplayFactory {
		return func() (syncharness.ReplayAdapter, error) {
			fixture := testenv.NewPostgresSchema(t, dsn)
			db, err := sql.Open("pgx", fixture.DSN())
			if err != nil {
				return nil, errors.New("open miniature topology failed")
			}
			db.SetMaxOpenConns(1)
			a := &syncHarnessTraceAdapter{fixture: fixture, db: db, repaired: repaired}
			t.Cleanup(func() { _ = a.Close() })
			for _, table := range []string{"h_fixture_objects", "h_provider_actions", "h_recorded_traces"} {
				if _, err = db.ExecContext(ctx, `CREATE TABLE `+table+` (LIKE `+node.schema.Name()+`.`+table+` INCLUDING ALL)`); err != nil {
					return nil, errors.New("clone miniature table definition failed")
				}
			}
			adapters = append(adapters, a)
			return a, nil
		}
	}
	// Begin with the committed, already reduced symbolic schedule, then add
	// unrelated events, actors, shape and faults to exercise SQL reduction too.
	raw, err := os.ReadFile("../syncharness/testdata/reduced/B04.v1.json")
	if err != nil {
		t.Fatal(err)
	}
	record, err := syncharness.DecodeRecordedSchedule(raw)
	if err != nil {
		t.Fatal(err)
	}
	scenario := syncharness.Scenario{Version: 1, ID: "B04", Seed: 29, ClockStart: record.Scenario.ClockStart, Inputs: []string{"request", "noise"}, Actors: []syncharness.Actor{{Name: "actor-L1", Identity: []byte{1}}, {Name: "noise-actor", Identity: []byte{2}}}, Capabilities: []string{"miniature-postgres"}, ExpectedInvariant: "no_fresh_action"}
	first, last := record.Steps[0].Event, record.Steps[1].Event
	noise := first
	noise.Actor = "noise-actor"
	noise.Operation = "noise"
	noise.Barrier = "b"
	plan := syncharness.FaultPlan(record.Steps[1].Faults)
	partition, _ := syncharness.FaultContract(syncharness.Partition)
	plan = append(plan, syncharness.Fault{Class: syncharness.Partition, Node: last.Node, Channel: last.Channel, Operation: last.Operation, Checkpoint: last.Checkpoint, Observable: partition})
	scheduler, err := syncharness.NewScheduler(scenario, plan)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range []syncharness.Event{first, noise, last} {
		if _, err = scheduler.Enqueue(event); err != nil {
			t.Fatal(err)
		}
	}
	payloads := []syncharness.PayloadShape{{Input: "request", Fields: []string{"effect", "padding"}, Elements: 2}, {Input: "noise", Fields: []string{"padding"}, Elements: 1}}
	adapter, err := newAdapter(false)()
	if err != nil {
		t.Fatal(err)
	}
	trace, err := scheduler.Run(func(s syncharness.Step) (syncharness.Observation, error) { return adapter.Apply(s, payloads) })
	if err != nil || trace.Outcome != syncharness.Fail {
		t.Fatal("known bad SQL transition did not fail")
	}
	if adapter.Close() != nil {
		t.Fatal("close original topology failed")
	}
	reduction, err := syncharness.Minimize(trace, payloads, newAdapter(false))
	if err != nil {
		t.Fatal(err)
	}
	if len(reduction.Schedule.Steps) != 2 || len(reduction.Schedule.Scenario.Actors) != 1 || len(reduction.Schedule.Payloads) != 1 || reduction.Schedule.Payloads[0].Elements != 1 || len(reduction.Schedule.Payloads[0].Fields) != 1 || len(reduction.Schedule.Steps[1].Faults) != 1 {
		t.Fatal("SQL reduction kept irrelevant dimensions")
	}
	for _, repaired := range []bool{false, true} {
		result, err := syncharness.ReplayRecorded(reduction.Schedule, newAdapter(repaired))
		if err != nil || (result.Outcome == syncharness.Fail) == repaired {
			t.Fatal("fresh SQL topology failed to distinguish repaired transition")
		}
	}
	restarted := 0
	for _, a := range adapters {
		restarted += a.restarts
		if a.db.Stats().OpenConnections != 0 {
			t.Fatal("miniature adapter leaked connection")
		}
	}
	if restarted < 3 {
		t.Fatal("restart evidence missing")
	}
	t.Logf("POSTGRES_PASS: fresh schema per replay; retained exact trace through same-schema connection restart; %d reduction attempts; elapsed %.3fs", reduction.Attempts, time.Since(started).Seconds())
}

func verifySyncHarnessRecoveryRows(ctx context.Context, t *testing.T, top *syncHarnessTopology, node *syncHarnessNode) {
	t.Helper()
	ddl, err := os.ReadFile("testdata/sync_harness/schema_v1.sql")
	if err != nil {
		t.Fatal("read recovery schema failed")
	}
	parts := strings.SplitN(string(ddl), "-- LC-S5D WITNESS DEFINITIONS", 2)
	if len(parts) != 2 {
		t.Fatal("witness definition boundary missing")
	}
	if _, err = top.witnessDB.ExecContext(ctx, parts[1]); err != nil {
		t.Fatal("install external witness definitions failed")
	}
	marker := bytes.Repeat([]byte{7}, 16)
	for _, statement := range []struct {
		q    string
		args []any
	}{
		{`INSERT INTO h_restore_intents(account_id,restore_operation_id,new_marker,transition_id,state,lifecycle_epoch,authority_installation) VALUES($1,$2,$2,$2,'allocated',2,'D')`, []any{syncHarnessAccount, marker}},
		{`INSERT INTO h_authority_heads(account_id,lifecycle_epoch,restore_marker,authority_installation,cas_revision) VALUES($1,2,$2,'D',1)`, []any{syncHarnessAccount, marker}},
		{`INSERT INTO h_restore_witness_heads(account_id,issuer_stream,through_ordinal,prefix_members,cas_revision) VALUES($1,'account',2,$2,1)`, []any{syncHarnessAccount, []byte("synthetic-members")}},
		{`INSERT INTO h_retired_denials(account_id,authority_installation,lifecycle_epoch,reason) VALUES($1,'R',1,'retired')`, []any{syncHarnessAccount}},
	} {
		if _, err = top.witnessDB.ExecContext(ctx, statement.q, statement.args...); err != nil {
			t.Fatal("persist witness recovery row failed")
		}
	}
	if _, err = top.witnessDB.ExecContext(ctx, `UPDATE h_restore_intents SET new_marker=$2 WHERE account_id=$1`, syncHarnessAccount, bytes.Repeat([]byte{8}, 16)); err == nil {
		t.Fatal("durable restore marker rewritten")
	}
	for _, statement := range []struct {
		q    string
		args []any
	}{
		{`INSERT INTO h_graph_authority(account_id,request_id,graph_epoch,graph_revision,authority_installation,state) VALUES($1,'request',1,4,'R','fenced')`, []any{syncHarnessAccount}},
		{`INSERT INTO h_graph_transfers(account_id,transfer_id,request_id,predecessor_epoch,successor_epoch,predecessor_installation,successor_installation,predecessor_lifecycle,predecessor_marker,successor_lifecycle,successor_marker,manifest,final_revision,state,unresolved_execution) VALUES($1,$2,'request',1,2,'R','D',1,$2,1,$2,$2,4,'fenced',true)`, []any{syncHarnessAccount, marker}},
		{`INSERT INTO h_transfer_outbox(account_id,transfer_id,proof_kind,state) VALUES($1,$2,'fence','pending')`, []any{syncHarnessAccount, marker}},
		{`INSERT INTO h_replicas(account_id,replica_id,installation_id,write_lineage_id,enrollment_generation) VALUES($1,'replica','L1','lineage',1)`, []any{syncHarnessAccount}},
		{`INSERT INTO h_grants(account_id,grant_id,replica_id,enrollment_generation,expires_at,deletion_frontier,revocation_frontier) VALUES($1,'grant','replica',1,'2030-01-01',2,2)`, []any{syncHarnessAccount}},
		{`INSERT INTO h_recovery_cases(account_id,recovery_id,protocol_version,issuer_stream,from_ordinal,through_ordinal,state,reason,required_action,convergence,receipt_evidence,journal_evidence,replica_evidence,deletion_coverage,authorization_coverage) VALUES($1,'recovery',1,'account',1,3,'blocked','history_gap_unrecoverable','operator_review',false,true,false,false,'proven','proven')`, []any{syncHarnessAccount}},
		{`INSERT INTO h_recovery_gaps VALUES($1,'recovery','account',2,2,'middle-group')`, []any{syncHarnessAccount}},
		{`INSERT INTO h_transition_checkpoints(account_id,transition_id,transition_kind,state,verified_watermark,pending_operation_ids) VALUES($1,$2,'enrollment','catchup_pending',2,ARRAY[$2]::bytea[])`, []any{syncHarnessAccount, marker}},
	} {
		if _, err = node.store.pool.Exec(ctx, statement.q, statement.args...); err != nil {
			t.Fatal("persist node recovery row failed")
		}
	}
	if _, err = node.store.pool.Exec(ctx, `UPDATE h_graph_transfers SET successor_installation='L2' WHERE account_id=$1`, syncHarnessAccount); err == nil {
		t.Fatal("transfer binding rewritten")
	}
	if _, err = node.store.pool.Exec(ctx, `UPDATE h_graph_transfers SET fence_proof_ref='fence-proof' WHERE account_id=$1`, syncHarnessAccount); err != nil {
		t.Fatal("persist transfer fence proof failed")
	}
	if _, err = node.store.pool.Exec(ctx, `UPDATE h_graph_transfers SET fence_proof_ref='changed-proof' WHERE account_id=$1`, syncHarnessAccount); err == nil {
		t.Fatal("transfer fence proof rewritten")
	}
	if _, err = node.store.pool.Exec(ctx, `UPDATE h_transfer_outbox SET state='pinned',witness_proof_ref='pin-proof' WHERE account_id=$1`, syncHarnessAccount); err != nil {
		t.Fatal("pin transfer outbox failed")
	}
	for _, query := range []string{
		`UPDATE h_transfer_outbox SET state='pending',witness_proof_ref=NULL WHERE account_id=$1`,
		`UPDATE h_transfer_outbox SET witness_proof_ref='changed-proof' WHERE account_id=$1`,
		`DELETE FROM h_transfer_outbox WHERE account_id=$1`,
	} {
		if _, err = node.store.pool.Exec(ctx, query, syncHarnessAccount); err == nil {
			t.Fatal("pinned transfer proof regressed or changed")
		}
	}
	if _, err = node.store.pool.Exec(ctx, `INSERT INTO h_transfer_outbox VALUES('wrong-account',$1,'fence','pending',NULL)`, marker); err == nil {
		t.Fatal("cross-account transfer reference accepted")
	}
	if _, err = node.store.pool.Exec(ctx, `INSERT INTO h_grants(account_id,grant_id,replica_id,enrollment_generation,expires_at,deletion_frontier,revocation_frontier) VALUES('wrong-account','grant','replica',1,'2030-01-01',2,2)`); err == nil {
		t.Fatal("cross-account grant reference accepted")
	}
	if _, err = node.store.pool.Exec(ctx, `INSERT INTO h_recovery_gaps VALUES('wrong-account','recovery','account',2,2,'middle-group')`); err == nil {
		t.Fatal("cross-account recovery gap accepted")
	}
	// A separate real pool reopens the migrated node; no new migration or seed.
	reopened, err := sql.Open("pgx", node.schema.DSN())
	if err != nil {
		t.Fatal("reopen recovery rows failed")
	}
	defer func() { _ = reopened.Close() }()
	var rows int
	if reopened.QueryRowContext(ctx, `SELECT count(*) FROM h_recovery_gaps WHERE account_id=$1 AND from_ordinal=2 AND through_ordinal=2`, syncHarnessAccount).Scan(&rows) != nil || rows != 1 {
		t.Fatal("recovery gap lost on reopen")
	}
	// External rows have no account FK and survive a simulated node restore/purge.
	if _, err = node.store.pool.Exec(ctx, `DELETE FROM h_recovery_gaps WHERE account_id=$1`, syncHarnessAccount); err != nil {
		t.Fatal("simulated restore failed")
	}
	if top.witnessDB.QueryRowContext(ctx, `SELECT count(*) FROM h_restore_intents WHERE account_id=$1`, syncHarnessAccount).Scan(&rows) != nil || rows != 1 {
		t.Fatal("restore intent lost with node restore")
	}
}
