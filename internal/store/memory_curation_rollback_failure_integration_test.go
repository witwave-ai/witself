package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/witwave-ai/witself/internal/testenv"
)

func TestMemoryCurationRollbackLateFailurePostgres(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	st, _ := newMigrationTestStore(t, testenv.RequirePostgres(t))
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}
	p := provisionMemoryCurationApplyPrincipal(ctx, t, st)
	source, err := st.CaptureMemory(ctx, p, CaptureMemoryInput{
		Content: "A synthetic source for rollback atomicity.", Kind: "decision",
		Evidence: []MemoryEvidenceInput{{
			Type: "artifact", ResolutionState: MemoryEvidenceResolved,
			ResolvedKind: "artifact", ArtifactExcerpt: []byte("Synthetic source evidence."),
		}},
		IdempotencyKey: "rollback-late-source",
	})
	if err != nil {
		t.Fatal(err)
	}
	requested, err := st.RequestCuration(ctx, p, RequestMemoryCurationInput{
		Scope:         MemoryCurationScope{Sources: []string{MemoryCurationSourceMemory}},
		CoalescingKey: "rollback_late", TriggerReason: "manual_refine",
		IdempotencyKey: "rollback-late-request",
	})
	if err != nil {
		t.Fatal(err)
	}
	started, err := st.StartCuration(ctx, p, StartMemoryCurationInput{
		RequestID: requested.Request.ID, LeaseDuration: time.Minute,
		IdempotencyKey: "rollback-late-start",
	})
	if err != nil {
		t.Fatal(err)
	}
	planned := planHardeningCuration(ctx, t, st, p, started,
		hardeningCreateActions(source.Memory.ID, source.Memory.Version,
			"rollback_late_output", "A synthetic curated output."), "rollback-late-plan")
	applied, err := st.ApplyCuration(ctx, p, started.Run.ID, ApplyMemoryCurationInput{
		FencingGeneration: started.Run.FencingGeneration,
		PlanRevision:      planned.Plan.PlanRevision, PlanHash: planned.Receipt.PlanHash,
		IdempotencyKey: "rollback-late-apply",
	})
	if err != nil {
		t.Fatal(err)
	}
	heads, _ := memoryCurationProducedHeads(applied.Receipt.ActionResults)
	if len(heads) != 1 || len(applied.Receipt.ActionResults) != 1 ||
		len(applied.Receipt.ActionResults[0].CreatedMemoryIDs) != 1 ||
		len(applied.Receipt.ActionResults[0].RelationIDs) != 0 ||
		len(applied.Receipt.ActionResults[0].CandidateIDs) != 0 {
		t.Fatal("fixture must apply only one create, without relation or fact effects")
	}
	createdID := heads[0].MemoryID
	created, err := st.GetMemory(ctx, p, createdID)
	if err != nil {
		t.Fatal(err)
	}
	if source.Memory.Version != 1 || created.Version != 1 ||
		len(source.Memory.Evidence) != 1 || len(created.Evidence) != 1 ||
		source.Memory.Evidence[0].ResolutionState != MemoryEvidenceResolved ||
		created.Evidence[0].ResolutionState != MemoryEvidenceResolved ||
		created.Evidence[0].SourceMemoryID != source.Memory.ID {
		t.Fatal("fixture must retain resolved source and output evidence")
	}
	cursors := rollbackContentionCursors(ctx, t, st, p)
	var advanced bool
	for _, position := range cursors {
		advanced = advanced || position > 0
	}
	if !advanced {
		t.Fatal("fixture must have nonempty committed cursors with a positive position")
	}
	var sequence, generation, active int64
	if err := st.pool.QueryRow(ctx, `
		SELECT c.last_change_seq,c.active_memory_count,l.request_generation
		FROM memory_change_clocks c JOIN memory_curation_lanes l
		  USING (account_id,realm_id,owner_kind,owner_id)
		WHERE c.account_id=$1 AND c.realm_id=$2 AND c.owner_kind='agent' AND c.owner_id=$3`,
		p.AccountID, p.RealmID, p.ID).Scan(&sequence, &active, &generation); err != nil {
		t.Fatal(err)
	}
	if sequence < 1 || generation < 1 || active != 2 {
		t.Fatal("fixture must have positive change/generation clocks and two active memories")
	}
	in := RollbackMemoryCurationInput{
		ApplyReceiptID: applied.Receipt.ID, ExpectedProducedHeads: heads,
		Reason: "verify late receipt failure is atomic", IdempotencyKey: "rollback-late-retry",
	}
	before := rollbackFailureSnapshot(ctx, t, st, p)
	removeFault := installRollbackReceiptSuppression(ctx, t, st, p, started.Run.ID,
		createdID, in, sequence, generation, active)
	_, err = st.RollbackCuration(ctx, p, started.Run.ID, in)
	// A stage-guard exception cannot satisfy this assertion. RETURN NULL makes
	// INSERT ... RETURNING yield no row while PostgreSQL remains committable.
	if !errors.Is(err, pgx.ErrNoRows) || !strings.Contains(err.Error(), "insert curation mutation receipt:") {
		t.Fatalf("late rollback error = %v, want mutation insertion wrapping pgx.ErrNoRows", err)
	}
	if after := rollbackFailureSnapshot(ctx, t, st, p); !reflect.DeepEqual(after, before) {
		t.Fatal("failed rollback committed staged durable state")
	}
	removeFault()

	// A fresh one-connection pool cannot inherit the failed caller's locks.
	// Completing the same-key retry proves its advisory, owner and head locks
	// were released; a leaked transaction must hit this bounded deadline.
	worker := successorRollbackWorker(ctx, t, st, "rollback_late_retry_"+started.Run.ID)
	retryCtx, cancelRetry := context.WithTimeout(ctx, 15*time.Second)
	rolledBack, err := worker.RollbackCuration(retryCtx, p, started.Run.ID, in)
	cancelRetry()
	if err != nil {
		t.Fatalf("same-key retry after late failure did not complete: %v", err)
	}
	if rolledBack.Receipt.ID == "" || rolledBack.Receipt.Replayed ||
		rolledBack.Run.State != MemoryCurationRunRolledBack ||
		rolledBack.Run.RollbackReceiptID != rolledBack.Receipt.ID ||
		rolledBack.ReplayRequest.ID == "" || !rolledBack.ReplayRequest.ReadOnlyReplay ||
		rolledBack.ReplayRequest.State != MemoryCurationRequestQueued ||
		rolledBack.ReplayRequest.ReplayRunID != started.Run.ID ||
		rolledBack.ReplayRequest.RequestGeneration != generation+1 ||
		len(rolledBack.Receipt.ActionResults) != 1 ||
		!reflect.DeepEqual(rolledBack.Receipt.ActionResults[0].CompensationHeads,
			[]MemoryVersionReference{{MemoryID: createdID, Version: 2}}) {
		t.Fatal("same-key retry omitted the single compensation receipt or queued read-only replay")
	}
	var versions, compensations, actions, mutations, replays, events, actualActive int
	var afterSequence, afterGeneration, afterActive int64
	if err := st.pool.QueryRow(ctx, `
		SELECT
		  (SELECT count(*) FROM memory_versions WHERE memory_id=$4),
		  (SELECT count(*) FROM memory_versions WHERE curation_run_id=$5 AND operation='reverted'),
		  (SELECT count(*) FROM memory_curation_actions WHERE run_id=$5 AND state='reverted'),
		  (SELECT count(*) FROM memory_curation_mutations WHERE run_id=$5 AND operation='rollback'),
		  (SELECT count(*) FROM memory_curation_requests WHERE replay_run_id=$5 AND read_only_replay),
		  (SELECT count(*) FROM account_events WHERE account_id=$1 AND verb=$6 AND metadata->>'run_id'=$5),
		  (SELECT count(*) FROM memories m JOIN memory_versions v ON v.memory_id=m.id AND v.version=m.current_version
		    WHERE m.account_id=$1 AND m.realm_id=$2 AND m.owner_kind='agent' AND m.owner_id=$3 AND v.state='active'),
		  (SELECT last_change_seq FROM memory_change_clocks WHERE account_id=$1 AND realm_id=$2 AND owner_kind='agent' AND owner_id=$3),
		  (SELECT active_memory_count FROM memory_change_clocks WHERE account_id=$1 AND realm_id=$2 AND owner_kind='agent' AND owner_id=$3),
		  (SELECT request_generation FROM memory_curation_lanes WHERE account_id=$1 AND realm_id=$2 AND owner_kind='agent' AND owner_id=$3)`,
		p.AccountID, p.RealmID, p.ID, createdID, started.Run.ID, VerbMemoryCurationRolledBack).
		Scan(&versions, &compensations, &actions, &mutations, &replays, &events,
			&actualActive, &afterSequence, &afterActive, &afterGeneration); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual([]int{versions, compensations, actions, mutations, replays, events, actualActive},
		[]int{2, 1, 1, 1, 1, 1, 1}) || afterSequence != sequence+1 ||
		afterActive != active-1 || afterGeneration != generation+1 {
		t.Fatal("same-key retry did not commit exactly one compensation, replay, receipt, event and clock transition")
	}
	compensated, err := st.GetMemory(ctx, p, createdID)
	if err != nil || compensated.Version != 2 || compensated.State != MemoryStateReverted ||
		compensated.Operation != "reverted" || compensated.Content != created.Content {
		t.Fatalf("created memory was not compensated exactly once: %v", err)
	}
	committed := rollbackFailureSnapshot(ctx, t, st, p)
	for _, table := range []string{"evidence", "cursors", "inputs", "relations"} {
		if !reflect.DeepEqual(committed[table], before[table]) {
			t.Fatalf("successful rollback changed preserved %s rows", table)
		}
	}
	replayCtx, cancelReplay := context.WithTimeout(ctx, 15*time.Second)
	replayed, err := worker.RollbackCuration(replayCtx, p, started.Run.ID, in)
	cancelReplay()
	if err != nil {
		t.Fatalf("exact-key replay failed: %v", err)
	}
	if !replayed.Receipt.Replayed {
		t.Fatal("exact-key retry did not replay the successful receipt")
	}
	replayed.Receipt.Replayed = false
	if !reflect.DeepEqual(replayed, rolledBack) {
		t.Fatal("exact-key replay changed the receipt, historical run or replay request")
	}
	if after := rollbackFailureSnapshot(ctx, t, st, p); !reflect.DeepEqual(after, committed) {
		t.Fatal("exact-key replay added effects or changed durable state")
	}
}

// Each section retains complete rows, including hashes, timestamps, immutable
// history and resolved evidence. One SQL statement gives a consistent snapshot
// of every memory/curation table touched by this create-only fixture.
func rollbackFailureSnapshot(ctx context.Context, t *testing.T, st *Store, p Principal) map[string]json.RawMessage {
	t.Helper()
	var raw []byte
	if err := st.pool.QueryRow(ctx, `
		SELECT jsonb_build_object(
		  'memories',(SELECT jsonb_agg(to_jsonb(m) ORDER BY id) FROM memories m
		    WHERE account_id=$1 AND realm_id=$2 AND owner_kind='agent' AND owner_id=$3),
		  'versions',(SELECT jsonb_agg(to_jsonb(v) ORDER BY memory_id,version) FROM memory_versions v
		    WHERE account_id=$1 AND realm_id=$2 AND owner_kind='agent' AND owner_id=$3),
		  'evidence',(SELECT jsonb_agg(to_jsonb(e) ORDER BY id) FROM memory_evidence e
		    WHERE account_id=$1 AND realm_id=$2 AND owner_kind='agent' AND owner_id=$3),
		  'relations',(SELECT jsonb_agg(to_jsonb(r) ORDER BY id) FROM memory_relations r
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
		    WHERE account_id=$1 AND realm_id=$2 AND owner_kind='agent' AND owner_id=$3),
		  'inputs',(SELECT jsonb_agg(to_jsonb(i) ORDER BY run_id,ordinal) FROM memory_curation_run_inputs i
		    WHERE account_id=$1 AND realm_id=$2 AND owner_kind='agent' AND owner_id=$3),
		  'actions',(SELECT jsonb_agg(to_jsonb(a) ORDER BY run_id,ordinal) FROM memory_curation_actions a
		    WHERE account_id=$1 AND realm_id=$2 AND owner_kind='agent' AND owner_id=$3),
		  'mutations',(SELECT jsonb_agg(to_jsonb(m) ORDER BY id) FROM memory_curation_mutations m
		    WHERE account_id=$1 AND realm_id=$2 AND owner_kind='agent' AND owner_id=$3),
		  'events',(SELECT jsonb_agg(to_jsonb(e) ORDER BY id) FROM account_events e WHERE account_id=$1)
		)`, p.AccountID, p.RealmID, p.ID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var out map[string]json.RawMessage
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func installRollbackReceiptSuppression(ctx context.Context, t *testing.T, st *Store, p Principal,
	runID, createdID string, in RollbackMemoryCurationInput, sequence, generation, active int64,
) func() {
	t.Helper()
	name := pgx.Identifier{"rollback_receipt_fault_" + runID}.Sanitize()
	// Database quoting keeps values out of the function body. The trigger is
	// restricted to this exact fixture owner/run/key even in a shared test DB.
	var arguments []string
	if err := st.pool.QueryRow(ctx, `SELECT array_agg(quote_literal(v) ORDER BY n)
		FROM unnest($1::text[]) WITH ORDINALITY AS args(v,n)`, []string{
		p.AccountID, p.RealmID, p.ID, runID, in.IdempotencyKey, createdID,
		strconv.FormatInt(sequence, 10), strconv.FormatInt(generation, 10),
		strconv.FormatInt(active, 10), in.ApplyReceiptID,
	}).Scan(&arguments); err != nil {
		t.Fatal(err)
	}
	remove := func() {
		t.Helper()
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if _, err := st.pool.Exec(cleanupCtx, fmt.Sprintf(
			`DROP TRIGGER IF EXISTS %s ON memory_curation_mutations; DROP FUNCTION IF EXISTS %s()`, name, name)); err != nil {
			t.Errorf("remove scoped rollback receipt fixture: %v", err)
		}
	}
	// Register before DDL so partial installation and any fatal assertion still
	// clean the uniquely owned trigger before the schema/store cleanup runs.
	t.Cleanup(remove)
	if _, err := st.pool.Exec(ctx, fmt.Sprintf(`
		CREATE FUNCTION %s() RETURNS trigger LANGUAGE plpgsql AS $fixture$
		BEGIN
		  IF NEW.account_id IS DISTINCT FROM TG_ARGV[0] OR NEW.realm_id IS DISTINCT FROM TG_ARGV[1]
		     OR NEW.owner_kind <> 'agent' OR NEW.owner_id IS DISTINCT FROM TG_ARGV[2]
		     OR NEW.actor_kind <> 'agent' OR NEW.actor_id IS DISTINCT FROM TG_ARGV[2]
		     OR NEW.run_id IS DISTINCT FROM TG_ARGV[3] OR NEW.operation <> 'rollback'
		     OR NEW.idempotency_key IS DISTINCT FROM TG_ARGV[4] THEN
		    RETURN NEW;
		  END IF;
		  IF NOT (
		    (SELECT count(*)=2 FROM memory_versions WHERE memory_id=TG_ARGV[5])
		    AND EXISTS (SELECT 1 FROM memories m JOIN memory_versions v ON v.memory_id=m.id AND v.version=m.current_version
		      WHERE m.id=TG_ARGV[5] AND v.version=2 AND v.previous_version=1 AND v.state='reverted'
		        AND v.operation='reverted' AND v.lifecycle_reason='curation_rollback'
		        AND v.curation_run_id=NEW.run_id AND v.change_seq=TG_ARGV[6]::bigint+1)
		    AND (SELECT count(*)=1 FROM memory_curation_actions WHERE run_id=NEW.run_id AND primitive='create'
		      AND state='reverted' AND reverted_at IS NOT NULL
		      AND rollback_result->'compensation_heads'=jsonb_build_array(jsonb_build_object('memory_id',TG_ARGV[5],'version',2)))
		    AND EXISTS (SELECT 1 FROM memory_change_clocks WHERE account_id=NEW.account_id AND realm_id=NEW.realm_id
		      AND owner_kind='agent' AND owner_id=NEW.owner_id
		      AND last_change_seq=TG_ARGV[6]::bigint+1 AND active_memory_count=TG_ARGV[8]::bigint-1)
		    AND EXISTS (SELECT 1 FROM memory_curation_lanes WHERE account_id=NEW.account_id AND realm_id=NEW.realm_id
		      AND owner_kind='agent' AND owner_id=NEW.owner_id AND active_run_id IS NULL
		      AND request_generation=TG_ARGV[7]::bigint+1)
		    AND (SELECT count(*)=1 FROM memory_curation_requests WHERE replay_run_id=NEW.run_id
		      AND read_only_replay AND state='queued' AND claimed_run_id IS NULL
		      AND request_generation=TG_ARGV[7]::bigint+1 AND trigger_reason='curation_rollback')
		    AND EXISTS (SELECT 1 FROM memory_curation_runs WHERE id=NEW.run_id AND request_id=NEW.request_id
		      AND state='rolled_back' AND apply_receipt_id=TG_ARGV[9] AND rollback_receipt_id=NEW.receipt_id
		      AND rolled_back_at IS NOT NULL AND terminal_at IS NOT NULL AND terminal_reason_code='curation_rollback')
		    AND NEW.result_state='rolled_back' AND NEW.receipt_id<>''
		    AND NOT EXISTS (SELECT 1 FROM memory_curation_mutations WHERE run_id=NEW.run_id AND operation='rollback')
		  ) THEN
		    RAISE EXCEPTION 'rollback receipt fixture did not observe all staged effects';
		  END IF;
		  RETURN NULL;
		END
		$fixture$;
		CREATE TRIGGER %s BEFORE INSERT ON memory_curation_mutations
		FOR EACH ROW EXECUTE FUNCTION %s(%s)`, name, name, name, strings.Join(arguments, ","))); err != nil {
		t.Fatal(err)
	}
	return remove
}
