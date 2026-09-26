package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/witwave-ai/witself/internal/activity"
	"github.com/witwave-ai/witself/internal/testenv"
)

func activityTestStore(t *testing.T) (*Store, Principal) {
	t.Helper()
	st, _ := newMigrationTestStore(t, testenv.RequirePostgres(t))
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}
	return st, provisionMemoryCurationApplyPrincipal(context.Background(), t, st)
}
func activityTotals(t *testing.T, st *Store, p Principal) map[string]UsageTotal {
	t.Helper()
	r, err := st.GetActivity(context.Background(), p, ActivityQuery{})
	if err != nil {
		t.Fatal(err)
	}
	return usageTotalsByDimension(r.Totals)
}
func requireActivity(t *testing.T, st *Store, p Principal, dimension string, quantity int64) {
	t.Helper()
	got := activityTotals(t, st, p)[dimension].Quantity
	if got != quantity {
		t.Fatalf("%s = %d, want %d", dimension, got, quantity)
	}
}
func TestActivityPostgresReadScopeAndRollback(t *testing.T) {
	st, p := activityTestStore(t)
	ctx := context.Background()
	empty, err := st.GetActivity(ctx, p, ActivityQuery{})
	if err != nil || empty.TrackingSince != nil || empty.Points == nil || empty.Totals == nil || empty.Truncated || len(empty.Points) != 0 {
		t.Fatalf("empty report: %+v / %v", empty, err)
	}
	// A transaction that never commits cannot establish tracking or quantities.
	tx, err := st.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := recordActivityOperationTx(ctx, tx, p, "facts.set", "rolled-back-effect", 1); err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	empty, err = st.GetActivity(ctx, p, ActivityQuery{})
	if err != nil || empty.TrackingSince != nil {
		t.Fatal("rollback established tracking", err)
	}
	// Legacy, observation and excluded outer calls omit the new catalog.
	key := activity.NewRequestID()
	read := activity.WithRequestID(ctx, key)
	for _, scope := range []context.Context{ctx, activity.WithObservation(read), activity.WithOperation(read, "")} {
		if _, err := st.ListFactsObservational(scope, p, FactListOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	requireActivity(t, st, p, "operation_read", 0)
	// Concurrent exact empty reads are one operation and no record events.
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for range 8 {
		wg.Go(func() { _, err := st.ListFactsObservational(read, p, FactListOptions{}); errs <- err })
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	requireActivity(t, st, p, "operation_read", 1)
	requireActivity(t, st, p, "operation_read_record", 0)
	for range 2 {
		if _, err := st.ListFactsObservational(activity.WithRequestID(ctx, activity.NewRequestID()), p, FactListOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	requireActivity(t, st, p, "operation_read", 3)
	// Reusing the completed empty-read key after data changes must not add a
	// companion record event that the original operation never recorded.
	if _, err := st.SetFact(ctx, p, SetFactInput{Predicate: "test/public", Value: json.RawMessage(`42`), IdempotencyKey: "public-after-empty"}); err != nil {
		t.Fatal(err)
	}
	rows, err := st.ListFactsObservational(read, p, FactListOptions{})
	if err != nil || len(rows) != 1 {
		t.Fatal("retry did not observe the changed synthetic data", len(rows), err)
	}
	requireActivity(t, st, p, "operation_read", 3)
	requireActivity(t, st, p, "operation_read_record", 0)
	fact, err := st.SetFact(ctx, p, SetFactInput{Predicate: "test/private", Value: json.RawMessage(`"synthetic-sensitive"`), Sensitive: true, IdempotencyKey: "fact-first"})
	if err != nil {
		t.Fatal(err)
	}
	deliberate := activity.WithDeliberate(activity.WithObservation(activity.WithRequestID(ctx, activity.NewRequestID())))
	if _, err := st.GetFactObservational(deliberate, p, "", fact.Predicate); err != nil {
		t.Fatal(err)
	}
	requireActivity(t, st, p, "operation_read", 4)
	requireActivity(t, st, p, "operation_read_record", 1)
	var old int
	if err := st.pool.QueryRow(ctx, `SELECT count(*) FROM usage_events WHERE account_id=$1 AND dimension=$2`, p.AccountID, UsageDimensionFactReturned).Scan(&old); err != nil || old != 0 {
		t.Fatal("deliberate reveal changed old usage", old, err)
	}
	other, err := st.CreateAgent(ctx, p.AccountID, p.RealmID, "other-activity")
	if err != nil {
		t.Fatal(err)
	}
	peer := p
	peer.ID = other.ID
	requireActivity(t, st, peer, "operation_read", 0)
	if _, err := st.GetFactObservational(read, peer, "", fact.Predicate); !errors.Is(err, ErrFactNotFound) {
		t.Fatal(err)
	}
	requireActivity(t, st, peer, "operation_read", 0)
	// A failed meter must leave the successful domain read available.
	if _, err := st.pool.Exec(ctx, `CREATE FUNCTION fail_activity_test() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.dimension='operation_read' THEN RAISE EXCEPTION 'synthetic meter failure'; END IF; RETURN NEW; END $$; CREATE TRIGGER fail_activity_test BEFORE INSERT ON usage_events FOR EACH ROW EXECUTE FUNCTION fail_activity_test()`); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ListFactsObservational(activity.WithRequestID(ctx, activity.NewRequestID()), p, FactListOptions{}); err != nil {
		t.Fatal("meter failure replaced success")
	}
	if _, err := st.pool.Exec(ctx, `DROP TRIGGER fail_activity_test ON usage_events; DROP FUNCTION fail_activity_test()`); err != nil {
		t.Fatal(err)
	}
	requireActivity(t, st, p, "operation_read", 4)
	if st.ActivityMeteringFailures() != 1 {
		t.Fatal("missing failure metric")
	}
}
func TestActivityPostgresWritesBatchReplayAndPortability(t *testing.T) {
	st, p := activityTestStore(t)
	ctx := context.Background()
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for range 8 {
		wg.Go(func() {
			_, err := st.SetFact(ctx, p, SetFactInput{Predicate: "test/key", Value: json.RawMessage(`42`), IdempotencyKey: "same-effect"})
			errs <- err
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	requireActivity(t, st, p, "operation_write", 1)
	tr, err := st.CreateTranscript(ctx, p.AccountID, p.RealmID, p.ID, CreateTranscriptInput{ExternalID: "activity-thread"})
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err := st.CreateTranscript(ctx, p.AccountID, p.RealmID, p.ID, CreateTranscriptInput{ExternalID: "activity-thread"}); err != nil {
			t.Fatal(err)
		}
	}
	inputs := []AppendTranscriptEntryInput{{ExternalID: "a", Role: TranscriptRoleUser, Body: "synthetic a"}, {ExternalID: "b", Role: TranscriptRoleAssistant, Body: "synthetic b"}}
	for range 2 {
		if _, err := st.AppendTranscriptEntries(ctx, p.AccountID, p.RealmID, p.ID, tr.ID, inputs); err != nil {
			t.Fatal(err)
		}
	}
	// Rebatching historic rows also cannot create another operation or record event.
	if _, err := st.AppendTranscriptEntry(ctx, p.AccountID, p.RealmID, p.ID, tr.ID, inputs[0]); err != nil {
		t.Fatal(err)
	}
	requireActivity(t, st, p, "operation_write", 3)
	requireActivity(t, st, p, "operation_write_record", 4)
	before, err := st.GetActivity(ctx, p, ActivityQuery{})
	if err != nil || before.TrackingSince == nil {
		t.Fatal(err)
	}
	// Advance the maintenance clock to prove archives preserve authoritative
	// rollups even after every activity event has retired.
	if deleted, err := st.RetireActivityEvents(ctx, time.Now().Add(36*24*time.Hour), 1000); err != nil || deleted != 6 {
		t.Fatal("retire before export", deleted, err)
	}
	if deleted, err := st.RetireActivityEvents(ctx, time.Now().Add(36*24*time.Hour), 1000); err != nil || deleted != 0 {
		t.Fatal("retention not idempotent", deleted, err)
	}
	if err := st.SuspendAccountSystem(ctx, p.AccountID, "evacuation", "synthetic activity archive"); err != nil {
		t.Fatal(err)
	}
	var archive bytes.Buffer
	if err := st.ExportAccount(ctx, p.AccountID, "activity-source", "test", &archive); err != nil {
		t.Fatal(err)
	}
	dest, _ := newMigrationTestStore(t, testenv.RequirePostgres(t))
	if err := dest.Migrate(); err != nil {
		t.Fatal(err)
	}
	if _, err := dest.ImportAccount(ctx, p.AccountID, bytes.NewReader(archive.Bytes())); err != nil {
		t.Fatal(err)
	}
	after, err := dest.GetActivity(ctx, p, ActivityQuery{Since: before.Since, Until: before.Until})
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("archive changed activity: before=%+v after=%+v error=%v", before, after, err)
	}
}
func TestActivityPostgresMemoryChanges(t *testing.T) {
	st, p := activityTestStore(t)
	ctx := context.Background()
	old := time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC)
	in := CaptureMemoryInput{Evidence: []MemoryEvidenceInput{{ResolutionState: MemoryEvidencePending, ExternalLocator: "synthetic://activity"}}, Content: "synthetic private memory", Kind: "decision", Sensitive: true, OccurredFrom: &old, IdempotencyKey: "activity-memory"}
	created, err := st.CaptureMemory(ctx, p, in)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CaptureMemory(ctx, p, in); err != nil {
		t.Fatal(err)
	}
	requireActivity(t, st, p, "memory_created", 1)
	report, err := st.GetActivity(ctx, p, ActivityQuery{})
	if err != nil || report.TrackingSince == nil || report.TrackingSince.Before(time.Now().Add(-time.Minute)) {
		t.Fatal("marker used history time", err)
	}
	forgotten, err := st.ForgetMemory(ctx, p, created.Memory.ID, MemoryLifecycleInput{ExpectedVersion: 1, IdempotencyKey: "activity-forget", Reason: "synthetic"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.RestoreMemory(ctx, p, created.Memory.ID, MemoryLifecycleInput{ExpectedVersion: forgotten.Memory.Version, IdempotencyKey: "activity-restore", Reason: "synthetic"}); err != nil {
		t.Fatal(err)
	}
	requireActivity(t, st, p, "memory_archived", 1)
	requireActivity(t, st, p, "memory_restored", 1)
	requireActivity(t, st, p, "operation_write", 3)
	// Check only value-free activity event columns, never payload tables.
	rows, err := st.pool.Query(ctx, `SELECT metadata::text,subject_id,idempotency_key FROM usage_events WHERE account_id=$1 AND (dimension LIKE 'operation_%' OR dimension LIKE 'memory_%' OR dimension='activity_tracking_started')`, p.AccountID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var metadata, subject, key string
		if err := rows.Scan(&metadata, &subject, &key); err != nil {
			t.Fatal(err)
		}
		for _, value := range []string{metadata, subject, key} {
			if bytes.Contains([]byte(value), []byte("synthetic")) || bytes.Contains([]byte(value), []byte("activity-memory")) {
				t.Fatal("payload or raw mutation key leaked")
			}
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
}

func requireActivityAction(t *testing.T, st *Store, p Principal, operation string, quantity int64) {
	t.Helper()
	d, ok := activity.Lookup(operation)
	if !ok {
		t.Fatal("invalid test operation")
	}
	var count int64
	if err := st.pool.QueryRow(context.Background(), `SELECT count(*) FROM usage_events WHERE account_id=$1 AND realm_id=$2 AND agent_id=$3 AND dimension IN ('operation_read','operation_write') AND metadata->>'category'=$4 AND metadata->>'action'=$5`, p.AccountID, p.RealmID, p.ID, d.Category, d.Action).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != quantity {
		t.Fatalf("%s event count = %d, want %d", operation, count, quantity)
	}
}
func TestActivityPostgresFactDecisionsAndDelete(t *testing.T) {
	st, p := activityTestStore(t)
	ctx := context.Background()
	proposal := ProposeFactInput{SetFactInput: SetFactInput{Predicate: "test/proposal", Value: json.RawMessage(`42`), IdempotencyKey: "propose-activity"}}
	c, err := st.ProposeFact(ctx, p, proposal)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.ProposeFact(ctx, p, proposal); err != nil {
		t.Fatal(err)
	}
	f, err := st.ConfirmFactCandidateIdempotent(ctx, p, c.ID, "confirm-activity")
	if err != nil {
		t.Fatal(err)
	}
	requireActivityAction(t, st, p, "facts.confirm", 1)
	if _, err := st.ConfirmFactCandidateIdempotent(ctx, p, c.ID, "confirm-activity"); err != nil {
		t.Fatal(err)
	}
	requireActivityAction(t, st, p, "facts.confirm", 1)
	proposal.IdempotencyKey = "second-proposal"
	proposal.Value = json.RawMessage(`43`)
	c, err = st.ProposeFact(ctx, p, proposal)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err := st.RejectFactCandidateIdempotent(ctx, p, c.ID, "reject-activity"); err != nil {
			t.Fatal(err)
		}
	}
	requireActivityAction(t, st, p, "facts.propose", 2)
	requireActivityAction(t, st, p, "facts.reject", 1)
	if _, err := st.FactHistory(activity.WithRequestID(ctx, activity.NewRequestID()), p, f.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpcomingFactsObservational(activity.WithRequestID(ctx, activity.NewRequestID()), p, UpcomingFactOptions{}); err != nil {
		t.Fatal(err)
	}
	requireActivityAction(t, st, p, "facts.history", 1)
	requireActivityAction(t, st, p, "facts.upcoming", 1)
	preview, err := st.DeleteFact(ctx, p, DeleteFactInput{FactID: f.ID})
	if err != nil {
		t.Fatal(err)
	}
	requireActivityAction(t, st, p, "facts.delete", 0)
	in := DeleteFactInput{FactID: f.ID, Apply: true, ExpectedResolvedAssertionID: preview.PriorResolvedAssertionID, ExpectedCandidateRevision: preview.CandidateRevision, IdempotencyKey: "delete-activity"}
	for range 2 {
		if _, err := st.DeleteFact(ctx, p, in); err != nil {
			t.Fatal(err)
		}
	}
	requireActivityAction(t, st, p, "facts.delete", 1)
}
func TestActivityPostgresFanoutAndNestedExclusion(t *testing.T) {
	st, p := activityTestStore(t)
	ctx := context.Background()
	a, err := st.CreateAgent(ctx, p.AccountID, p.RealmID, "recipient-a")
	if err != nil {
		t.Fatal(err)
	}
	b, err := st.CreateAgent(ctx, p.AccountID, p.RealmID, "recipient-b")
	if err != nil {
		t.Fatal(err)
	}
	input := SendMessageInput{AudienceKind: MessageRecipientAgents, ToAgents: []string{a.ID, b.ID}, Body: "synthetic fanout", IdempotencyKey: "activity-fanout"}
	msg, err := st.SendMessage(ctx, p, input)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.SendMessage(ctx, p, input); err != nil {
		t.Fatal(err)
	}
	requireActivity(t, st, p, "operation_write", 1)
	requireActivity(t, st, p, "operation_write_record", 1)
	recipient := p
	recipient.ID = a.ID
	for _, scope := range []context.Context{activity.WithOperation(activity.WithRequestID(ctx, activity.NewRequestID()), ""), activity.WithObservation(activity.WithRequestID(ctx, activity.NewRequestID()))} {
		if _, err := st.ListMessages(scope, recipient, MessageFilter{}); err != nil {
			t.Fatal(err)
		}
	}
	requireActivity(t, st, recipient, "operation_read", 0)
	if _, err := st.ListMessages(activity.WithRequestID(ctx, activity.NewRequestID()), recipient, MessageFilter{}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.PeekMessage(activity.WithRequestID(ctx, activity.NewRequestID()), recipient, msg.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ReadMessage(activity.WithRequestID(ctx, activity.NewRequestID()), recipient, msg.ID); err != nil {
		t.Fatal(err)
	}
	reply := ReplyMessageInput{Body: "synthetic reply", IdempotencyKey: "activity-reply"}
	for range 2 {
		if _, err := st.ReplyMessage(ctx, recipient, msg.ID, reply); err != nil {
			t.Fatal(err)
		}
	}
	requireActivity(t, st, recipient, "operation_read", 3)
	requireActivityAction(t, st, recipient, "messages.reply", 1)
	// Excluded outer APIs can create nested messages but cannot claim sends.
	input.IdempotencyKey = "internal-send"
	if _, err := st.SendMessage(activity.WithOperation(ctx, ""), p, input); err != nil {
		t.Fatal(err)
	}
	requireActivity(t, st, p, "operation_write", 1)
}
func TestActivityPostgresCurationNoOp(t *testing.T) {
	st, p := activityTestStore(t)
	ctx := context.Background()
	req, err := st.RequestCuration(ctx, p, RequestMemoryCurationInput{CoalescingKey: "activity-noop", TriggerReason: "manual_refine", IdempotencyKey: "activity-noop-request"})
	if err != nil {
		t.Fatal(err)
	}
	run, err := st.StartCuration(ctx, p, StartMemoryCurationInput{RequestID: req.Request.ID, LeaseDuration: time.Minute, Client: MemoryClientProvenance{Runtime: "test"}, IdempotencyKey: "activity-noop-start"})
	if err != nil {
		t.Fatal(err)
	}
	planned, err := st.PlanCuration(ctx, p, run.Run.ID, PlanMemoryCurationInput{FencingGeneration: run.Run.FencingGeneration, Draft: json.RawMessage(`{"schema":"witself.memory-plan.v1","draft_revision":1,"actions":[]}`), IdempotencyKey: "activity-noop-plan"})
	if err != nil {
		t.Fatal(err)
	}
	in := ApplyMemoryCurationInput{FencingGeneration: run.Run.FencingGeneration, PlanRevision: planned.Plan.PlanRevision, PlanHash: planned.Receipt.PlanHash, IdempotencyKey: "activity-noop-apply"}
	for range 2 {
		if _, err := st.ApplyCuration(ctx, p, run.Run.ID, in); err != nil {
			t.Fatal(err)
		}
	}
	r, err := st.GetActivity(ctx, p, ActivityQuery{})
	if err != nil || r.TrackingSince != nil || len(r.Points) != 0 {
		t.Fatal("curation bookkeeping started activity", err)
	}
}

func TestActivityPostgresMemoryCatalog(t *testing.T) {
	st, p := activityTestStore(t)
	ctx := context.Background()
	capture := func(key string) CaptureMemoryInput {
		return CaptureMemoryInput{Content: "synthetic memory", Kind: "decision", Evidence: []MemoryEvidenceInput{{Type: "system", ResolutionState: MemoryEvidenceUnavailable, TerminalReasonCode: "not_recorded"}}, IdempotencyKey: key}
	}
	original, err := st.CaptureMemory(ctx, p, capture("catalog-original"))
	if err != nil {
		t.Fatal(err)
	}
	read := func() context.Context { return activity.WithRequestID(ctx, activity.NewRequestID()) }
	if _, err := st.GetMemory(read(), p, original.Memory.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ListMemories(read(), p, MemoryListOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetMemoryHistoryPage(read(), p, original.Memory.ID, MemoryHistoryOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.RecallMemories(read(), p, MemoryRecallOptions{Kind: "decision"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.RecallMemories(activity.WithObservation(read()), p, MemoryRecallOptions{Kind: "decision"}); err != nil {
		t.Fatal(err)
	}
	for _, op := range []string{"memories.get", "memories.list", "memories.history", "memories.recall"} {
		requireActivityAction(t, st, p, op, 1)
	}
	changed := "synthetic revised memory"
	adjusted, err := st.AdjustMemory(ctx, p, original.Memory.ID, AdjustMemoryInput{Content: &changed, ExpectedVersion: 1, Reason: "synthetic", IdempotencyKey: "catalog-adjust"})
	if err != nil {
		t.Fatal(err)
	}
	replacement := capture("catalog-replacement")
	supersede := SupersedeMemoryInput{MemoryID: original.Memory.ID, ExpectedVersion: adjusted.Memory.Version, Replacements: []CaptureMemoryInput{replacement}, Reason: "synthetic", IdempotencyKey: "catalog-supersede"}
	replaced, err := st.SupersedeMemory(ctx, p, supersede)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.SupersedeMemory(ctx, p, supersede); err != nil {
		t.Fatal(err)
	}
	revision := replaced.Receipt.SupersessionSetRevision
	if _, err := st.ReactivateMemory(ctx, p, original.Memory.ID, MemoryLifecycleInput{ExpectedVersion: replaced.Source.Version, ExpectedSupersessionSetRevision: &revision, Reason: "synthetic", IdempotencyKey: "catalog-reactivate"}); err != nil {
		t.Fatal(err)
	}
	requireActivityAction(t, st, p, "memories.adjust", 1)
	requireActivityAction(t, st, p, "memories.supersede", 1)
	requireActivityAction(t, st, p, "memories.reactivate", 1)
	requireActivity(t, st, p, "memory_created", 2)
	requireActivity(t, st, p, "memory_revised", 2)
	requireActivity(t, st, p, "memory_restored", 1)
	// A separate deletable record proves preview, purge, and exact replay.
	doomed, err := st.CaptureMemory(ctx, p, capture("catalog-delete"))
	if err != nil {
		t.Fatal(err)
	}
	preview, err := st.DeleteMemory(ctx, p, DeleteMemoryInput{MemoryID: doomed.Memory.ID})
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err := applyMemoryDeletePreview(ctx, st, p, preview, "catalog-delete-apply"); err != nil {
			t.Fatal(err)
		}
	}
	requireActivityAction(t, st, p, "memories.delete", 1)
	requireActivity(t, st, p, "memory_deleted", 1)
	requireActivity(t, st, p, "operation_write_record", 7)
}

// Force an earlier sampled timestamp to lose the marker insertion race. The
// advisory barrier is local to this test transaction and never uses sleep to
// guess which writer won.
func TestActivityPostgresConcurrentFirstMarkerTime(t *testing.T) {
	st, p := activityTestStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	barrier, err := st.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = barrier.Rollback(context.Background()) }()
	if _, err := barrier.Exec(ctx, `SELECT pg_advisory_xact_lock(824921,1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := st.pool.Exec(ctx, `CREATE FUNCTION delay_activity_marker_test() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.dimension='activity_tracking_started' AND current_setting('activity.delay_marker',true)='on' THEN PERFORM pg_advisory_xact_lock(824921,1); END IF; RETURN NEW; END $$; CREATE TRIGGER delay_activity_marker_test BEFORE INSERT ON usage_events FOR EACH ROW EXECUTE FUNCTION delay_activity_marker_test()`); err != nil {
		t.Fatal(err)
	}
	slow, err := st.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = slow.Rollback(context.Background()) }()
	var pid int
	if err := slow.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	if _, err := slow.Exec(ctx, `SELECT set_config('activity.delay_marker','on',true)`); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- recordActivityOperationTx(ctx, slow, p, "facts.set", "slow-effect", 1) }()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		var blocked bool
		if err := st.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_locks WHERE pid=$1 AND locktype='advisory' AND NOT granted)`, pid).Scan(&blocked); err != nil {
			t.Fatal(err)
		}
		if blocked {
			break
		}
		select {
		case err := <-done:
			t.Fatalf("writer failed before barrier: %v", err)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-ticker.C:
		}
	}
	fast, err := st.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = fast.Rollback(context.Background()) }()
	if err := recordActivityOperationTx(ctx, fast, p, "facts.set", "fast-effect", 1); err != nil {
		t.Fatal(err)
	}
	if err := fast.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := barrier.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := slow.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	var early int
	if err := st.pool.QueryRow(ctx, `SELECT count(*) FROM usage_events e JOIN usage_events marker ON marker.account_id=e.account_id AND marker.idempotency_key=$2 WHERE e.account_id=$1 AND e.dimension IN ('operation_write','operation_write_record') AND e.occurred_at < marker.occurred_at`, p.AccountID, activityMarkerKey(p)).Scan(&early); err != nil {
		t.Fatal(err)
	}
	if early != 0 {
		t.Fatalf("%d activity events predate the persisted marker", early)
	}
	requireActivity(t, st, p, "operation_write", 2)
}

func TestActivityPostgresConcurrentLegacyMessageReplay(t *testing.T) {
	st, p := activityTestStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	recipient, err := st.CreateAgent(ctx, p.AccountID, p.RealmID, "legacy-recipient")
	if err != nil {
		t.Fatal(err)
	}
	legacy, err := st.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = legacy.Rollback(context.Background()) }()
	var pid int
	if err := legacy.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.Exec(ctx, `SELECT id FROM agents WHERE id=$1 FOR UPDATE`, recipient.ID); err != nil {
		t.Fatal(err)
	}
	in := SendMessageInput{ToAgent: recipient.ID, Body: "synthetic legacy message", IdempotencyKey: "legacy-concurrent-send"}
	normalized, err := normalizeSendMessageInput(in)
	if err != nil {
		t.Fatal(err)
	}
	// The legacy writer has no activity descriptor and commits without telemetry.
	if _, err := st.insertMessageTx(activity.WithOperation(ctx, ""), legacy, p, recipient, normalized); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := st.SendMessage(ctx, p, in); done <- err }()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		var blocked bool
		if err := st.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE $1=ANY(pg_blocking_pids(pid)))`, pid).Scan(&blocked); err != nil {
			t.Fatal(err)
		}
		if blocked {
			break
		}
		select {
		case err := <-done:
			t.Fatalf("retry finished before legacy commit: %v", err)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-ticker.C:
		}
	}
	if err := legacy.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	r, err := st.GetActivity(ctx, p, ActivityQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if r.TrackingSince != nil || len(r.Points) != 0 {
		t.Fatal("concurrent replay of legacy message started tracking")
	}
}

func TestActivityPostgresLegacyMutationReplaysRemainUntracked(t *testing.T) {
	st, p := activityTestStore(t)
	ctx := context.Background()
	recipient, err := st.CreateAgent(ctx, p.AccountID, p.RealmID, "legacy-peer")
	if err != nil {
		t.Fatal(err)
	}
	var tr Transcript
	var memory MemoryMutationResult
	var candidate FactCandidate
	var secret SecretMutationResult
	public := "synthetic-public"
	secretInput := CreateSecretInput{ID: mustSecretTestID(t, "sec"), Name: "legacy", Template: "login", IdempotencyKey: "legacy-secret", Fields: []CreateSecretFieldInput{{ID: mustSecretTestID(t, "fld"), Name: "username", Kind: SecretFieldUsername, Encoding: SecretEncodingUTF8, ValueVersion: 1, PublicValue: &public}}}
	cases := []struct {
		name string
		run  func(context.Context) error
	}{
		{"facts.set", func(c context.Context) error {
			_, e := st.SetFact(c, p, SetFactInput{Predicate: "legacy/value", Value: json.RawMessage(`42`), IdempotencyKey: "legacy-fact"})
			return e
		}},
		{"facts.propose", func(c context.Context) error {
			var e error
			candidate, e = st.ProposeFact(c, p, ProposeFactInput{SetFactInput: SetFactInput{Predicate: "legacy/proposal", Value: json.RawMessage(`43`), IdempotencyKey: "legacy-proposal"}})
			return e
		}},
		{"facts.confirm", func(c context.Context) error {
			_, e := st.ConfirmFactCandidateIdempotent(c, p, candidate.ID, "legacy-confirm")
			return e
		}},
		{"transcripts.create", func(c context.Context) error {
			var e error
			tr, e = st.CreateTranscript(c, p.AccountID, p.RealmID, p.ID, CreateTranscriptInput{ExternalID: "legacy-transcript"})
			return e
		}},
		{"transcripts.append", func(c context.Context) error {
			_, e := st.AppendTranscriptEntries(c, p.AccountID, p.RealmID, p.ID, tr.ID, []AppendTranscriptEntryInput{{ExternalID: "legacy-a", Role: TranscriptRoleUser, Body: "synthetic"}, {ExternalID: "legacy-b", Role: TranscriptRoleAssistant, Body: "synthetic"}})
			return e
		}},
		{"memories.capture", func(c context.Context) error {
			var e error
			memory, e = st.CaptureMemory(c, p, CaptureMemoryInput{Content: "synthetic legacy", Kind: "decision", Evidence: []MemoryEvidenceInput{{Type: "system", ResolutionState: MemoryEvidenceUnavailable, TerminalReasonCode: "not_recorded"}}, IdempotencyKey: "legacy-memory"})
			return e
		}},
		{"memories.adjust", func(c context.Context) error {
			value := "synthetic adjusted"
			_, e := st.AdjustMemory(c, p, memory.Memory.ID, AdjustMemoryInput{Content: &value, ExpectedVersion: 1, IdempotencyKey: "legacy-adjust", Reason: "synthetic"})
			return e
		}},
		{"memories.forget", func(c context.Context) error {
			_, e := st.ForgetMemory(c, p, memory.Memory.ID, MemoryLifecycleInput{ExpectedVersion: 2, IdempotencyKey: "legacy-forget", Reason: "synthetic"})
			return e
		}},
		{"memories.restore", func(c context.Context) error {
			_, e := st.RestoreMemory(c, p, memory.Memory.ID, MemoryLifecycleInput{ExpectedVersion: 3, IdempotencyKey: "legacy-restore", Reason: "synthetic"})
			return e
		}},
		{"messages.send", func(c context.Context) error {
			_, e := st.SendMessage(c, p, SendMessageInput{ToAgent: recipient.ID, Body: "synthetic legacy", IdempotencyKey: "legacy-message"})
			return e
		}},
		{"secrets.create", func(c context.Context) error { var e error; secret, e = st.CreateSecret(c, p, secretInput); return e }},
		{"secrets.archive", func(c context.Context) error {
			_, e := st.ArchiveSecret(c, p, secret.Secret.ID, SecretLifecycleInput{ExpectedRowVersion: 1, IdempotencyKey: "legacy-archive"})
			return e
		}},
		{"secrets.restore", func(c context.Context) error {
			_, e := st.RestoreSecret(c, p, secret.Secret.ID, SecretLifecycleInput{ExpectedRowVersion: 2, IdempotencyKey: "legacy-restore"})
			return e
		}},
		{"secrets.delete", func(c context.Context) error {
			_, e := st.DeleteSecret(c, p, secret.Secret.ID, SecretLifecycleInput{ExpectedRowVersion: 3, IdempotencyKey: "legacy-delete"})
			return e
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Observation simulates an existing durable effect with no new telemetry.
			if err := tc.run(activity.WithObservation(ctx)); err != nil {
				t.Fatal(err)
			}
			for range 2 {
				if err := tc.run(ctx); err != nil {
					t.Fatal(err)
				}
			}
			r, err := st.GetActivity(ctx, p, ActivityQuery{})
			if err != nil {
				t.Fatal(err)
			}
			if r.TrackingSince != nil || len(r.Points) != 0 {
				t.Fatal("legacy replay began tracking")
			}
		})
	}
}

func TestActivityPostgresDomainRollbackAndRestart(t *testing.T) {
	st, p := activityTestStore(t)
	ctx := context.Background()
	if _, err := st.pool.Exec(ctx, `CREATE FUNCTION fail_activity_write_test() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.dimension='operation_write' THEN RAISE EXCEPTION 'synthetic activity refusal'; END IF; RETURN NEW; END $$; CREATE TRIGGER fail_activity_write_test BEFORE INSERT ON usage_events FOR EACH ROW EXECUTE FUNCTION fail_activity_write_test()`); err != nil {
		t.Fatal(err)
	}
	in := CaptureMemoryInput{Content: "synthetic rollback", Kind: "decision", Evidence: []MemoryEvidenceInput{{Type: "system", ResolutionState: MemoryEvidenceUnavailable, TerminalReasonCode: "not_recorded"}}, IdempotencyKey: "rollback-memory"}
	if _, err := st.CaptureMemory(ctx, p, in); err == nil {
		t.Fatal("write ignored meter failure")
	}
	page, err := st.ListMemories(ctx, p, MemoryListOptions{})
	if err != nil || len(page.Memories) != 0 {
		t.Fatal("domain mutation survived rollback", err)
	}
	r, err := st.GetActivity(ctx, p, ActivityQuery{})
	if err != nil || r.TrackingSince != nil || len(r.Points) != 0 {
		t.Fatal("memory change or marker survived rollback", err)
	}
	if _, err := st.pool.Exec(ctx, `DROP TRIGGER fail_activity_write_test ON usage_events; DROP FUNCTION fail_activity_write_test()`); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CaptureMemory(ctx, p, in); err != nil {
		t.Fatal(err)
	}
	before, err := st.GetActivity(ctx, p, ActivityQuery{})
	if err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(ctx, st.pool.Config().ConnString())
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	after, err := reopened.GetActivity(ctx, p, ActivityQuery{Since: before.Since, Until: before.Until})
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatal("reopened store changed report", err)
	}
}

func TestActivityPostgresBoundedReadsAndIsolation(t *testing.T) {
	st, p := activityTestStore(t)
	ctx := context.Background()
	read := func() context.Context { return activity.WithRequestID(ctx, activity.NewRequestID()) }
	if _, err := st.ListTranscripts(read(), p); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ListSecrets(read(), p, SecretListOptions{}); err != nil {
		t.Fatal(err)
	}
	requireActivity(t, st, p, "operation_read", 2)
	requireActivity(t, st, p, "operation_read_record", 0)
	tr, err := st.CreateTranscript(ctx, p.AccountID, p.RealmID, p.ID, CreateTranscriptInput{ExternalID: "read-pages"})
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err := st.GetTranscriptPageObservational(read(), p, tr.ID, TranscriptPageOptions{Limit: 1}); err != nil {
			t.Fatal(err)
		}
	}
	requireActivityAction(t, st, p, "transcripts.page", 2)
	other := provisionMemoryCurationApplyPrincipal(ctx, t, st)
	if _, err := st.GetTranscriptPageObservational(read(), other, tr.ID, TranscriptPageOptions{Limit: 1}); !errors.Is(err, ErrTranscriptNotFound) {
		t.Fatal("cross-account read", err)
	}
	r, err := st.GetActivity(ctx, other, ActivityQuery{})
	if err != nil || r.TrackingSince != nil || len(r.Points) != 0 {
		t.Fatal("cross-account activity escaped scope", err)
	}
	for _, q := range []ActivityQuery{{Bucket: "day"}, {Since: time.Now().Add(-32 * 24 * time.Hour)}, {Since: time.Now().Add(time.Hour)}} {
		if _, err := st.GetActivity(ctx, p, q); !errors.Is(err, ErrUsageInputInvalid) {
			t.Fatal("invalid window accepted", err)
		}
	}
	old, err := st.GetActivity(ctx, p, ActivityQuery{Since: time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC), Until: time.Date(2000, 1, 2, 0, 0, 0, 0, time.UTC)})
	if err != nil || old.TrackingSince == nil || len(old.Points) != 0 || old.Truncated {
		t.Fatal("history invented or marker windowed", err)
	}
}

func TestActivityPostgresFactHistoryBound(t *testing.T) {
	st, p := activityTestStore(t)
	ctx := context.Background()
	fact, err := st.SetFact(activity.WithObservation(ctx), p, SetFactInput{Predicate: "synthetic/history", Value: json.RawMessage(`1`), IdempotencyKey: "history-root"})
	if err != nil {
		t.Fatal(err)
	}
	// Build synthetic history in one local statement without inventing telemetry.
	if _, err := st.pool.Exec(ctx, `INSERT INTO fact_assertions(id,fact_id,account_id,realm_id,asserted_by_agent_id,value_type,value,source_kind,observed_at,supersedes_id)
 SELECT 'fas_history_'||n,$1,$2,$3,$4,'number','1'::jsonb,'self',clock_timestamp(),CASE WHEN n=1 THEN NULL ELSE 'fas_history_'||(n-1) END FROM generate_series(1,1001) n`, fact.ID, p.AccountID, p.RealmID, p.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.pool.Exec(ctx, `UPDATE facts SET resolved_assertion_id='fas_history_1001' WHERE id=$1`, fact.ID); err != nil {
		t.Fatal(err)
	}
	history, err := st.FactHistory(activity.WithRequestID(ctx, activity.NewRequestID()), p, fact.ID)
	if err != nil || len(history) != 1000 || history[0].ID != "fas_history_1001" || history[999].SupersedesID != "fas_history_1" {
		t.Fatal("oversized history did not return the newest bounded assertions", err)
	}
	if _, err := st.pool.Exec(ctx, `UPDATE facts SET resolved_assertion_id='fas_history_1000' WHERE id=$1`, fact.ID); err != nil {
		t.Fatal(err)
	}
	history, err = st.FactHistory(activity.WithRequestID(ctx, activity.NewRequestID()), p, fact.ID)
	if err != nil || len(history) != 1000 || history[999].SupersedesID != "" {
		t.Fatal("bounded history rejected", len(history), err)
	}
	requireActivity(t, st, p, "operation_read", 2)
	requireActivity(t, st, p, "operation_read_record", 2000)
}

func TestActivityRetentionBoundsAndMarker(t *testing.T) {
	st, p := activityTestStore(t)
	ctx := t.Context()
	now := time.Now().UTC().Truncate(time.Second)
	tx, err := st.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	for _, event := range []struct {
		key, dim, unit string
		at             time.Time
	}{
		{"old-marker", "activity_tracking_started", "activation", now.Add(-40 * 24 * time.Hour)},
		{"old-read", "operation_read", "operation", now.Add(-40 * 24 * time.Hour)},
		{"old-memory", "memory_created", "change", now.Add(-36 * 24 * time.Hour)},
		{"boundary", "operation_write", "operation", now.Add(-35 * 24 * time.Hour)},
		{"recent", "memory_deleted", "change", now},
		{"billing", "message_sent", "message", now.Add(-40 * 24 * time.Hour)},
	} {
		_, err := recordUsageEventTx(ctx, tx, usageEventInput{AccountID: p.AccountID, RealmID: p.RealmID, AgentID: p.ID,
			Dimension: event.dim, Unit: event.unit, Quantity: 1, SubjectType: "activity", SubjectID: "synthetic", IdempotencyKey: event.key, OccurredAt: event.at})
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	for _, limit := range []int{0, -1, 1001} {
		if _, err := st.RetireActivityEvents(ctx, now, limit); !errors.Is(err, ErrUsageInputInvalid) {
			t.Fatal("invalid bound", err)
		}
	}
	for _, want := range []int64{1, 1, 0} {
		if n, err := st.RetireActivityEvents(ctx, now, 1); err != nil || n != want {
			t.Fatal("bounded retention", n, err)
		}
	}
	var events, rollups int
	if err := st.pool.QueryRow(ctx, `SELECT count(*) FROM usage_events WHERE account_id=$1`, p.AccountID).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if err := st.pool.QueryRow(ctx, `SELECT count(*) FROM usage_rollups WHERE account_id=$1`, p.AccountID).Scan(&rollups); err != nil {
		t.Fatal(err)
	}
	if events != 4 || rollups != 12 {
		t.Fatal("marker, boundary, billing or rollups changed", events, rollups)
	}
}

func TestActivityReadMarkerCacheCommitAndRestart(t *testing.T) {
	st, p := activityTestStore(t)
	ctx := t.Context()
	read := func() {
		t.Helper()
		if _, err := st.ListFactsObservational(activity.WithRequestID(ctx, activity.NewRequestID()), p, FactListOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	// First failed activation must not poison the cache.
	if _, err := st.pool.Exec(ctx, `CREATE FUNCTION reject_meter() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.dimension='operation_read' THEN RAISE EXCEPTION 'synthetic'; END IF; RETURN NEW; END $$; CREATE TRIGGER reject_meter BEFORE INSERT ON usage_events FOR EACH ROW EXECUTE FUNCTION reject_meter()`); err != nil {
		t.Fatal(err)
	}
	read()
	if _, err := st.pool.Exec(ctx, `DROP TRIGGER reject_meter ON usage_events; DROP FUNCTION reject_meter()`); err != nil {
		t.Fatal(err)
	}
	read()
	requireActivity(t, st, p, "operation_read", 1)
	before, err := st.GetActivity(ctx, p, ActivityQuery{})
	if err != nil || before.TrackingSince == nil {
		t.Fatal("activation missing", err)
	}
	// A marker-insert trigger proves cached reads don't even attempt the insert.
	if _, err := st.pool.Exec(ctx, `CREATE FUNCTION reject_marker() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.dimension='activity_tracking_started' THEN RAISE EXCEPTION 'synthetic'; END IF; RETURN NEW; END $$; CREATE TRIGGER reject_marker BEFORE INSERT ON usage_events FOR EACH ROW EXECUTE FUNCTION reject_marker()`); err != nil {
		t.Fatal(err)
	}
	read()
	requireActivity(t, st, p, "operation_read", 2)
	if st.ActivityMeteringFailures() != 1 {
		t.Fatal("cached read attempted marker write")
	}
	if _, err := st.pool.Exec(ctx, `DROP TRIGGER reject_marker ON usage_events; DROP FUNCTION reject_marker()`); err != nil {
		t.Fatal(err)
	}
	restarted := &Store{pool: st.pool}
	if _, err := restarted.ListFactsObservational(activity.WithRequestID(ctx, activity.NewRequestID()), p, FactListOptions{}); err != nil {
		t.Fatal(err)
	}
	after, err := st.GetActivity(ctx, p, ActivityQuery{})
	if err != nil || !before.TrackingSince.Equal(*after.TrackingSince) {
		t.Fatal("restart changed marker", err)
	}
	requireActivity(t, st, p, "operation_read", 3)
}

func TestActivityRetentionRunsInMaintenanceLoop(t *testing.T) {
	st, p := activityTestStore(t)
	ctx := t.Context()
	if _, err := st.ListFactsObservational(activity.WithRequestID(ctx, activity.NewRequestID()), p, FactListOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.pool.Exec(ctx, `UPDATE usage_events SET occurred_at=clock_timestamp()-interval '36 days' WHERE account_id=$1 AND dimension='operation_read'`, p.AccountID); err != nil {
		t.Fatal(err)
	}
	workerCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	if err := st.RunMessageRateBucketCleanupWorker(workerCtx, DefaultMessageRateBucketCleanupWorkerConfig(), func(result MessageRateBucketCleanupBatchResult) {
		if result.RateBucketError != nil || result.ActivityRetentionError != nil {
			t.Errorf("maintenance errors: rate_bucket=%v activity_retention=%v", result.RateBucketError, result.ActivityRetentionError)
		}
		cancel()
	}); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := st.pool.QueryRow(ctx, `SELECT count(*) FROM usage_events WHERE account_id=$1 AND dimension='operation_read'`, p.AccountID).Scan(&count); err != nil || count != 0 {
		t.Fatal("maintenance did not retire activity", count, err)
	}
	requireActivity(t, st, p, "operation_read", 1)
}

func TestActivityImportedRollupsCannotUndercountRetainedEvents(t *testing.T) {
	st, p := activityTestStore(t)
	ctx := t.Context()
	if _, err := st.ListFactsObservational(activity.WithRequestID(ctx, activity.NewRequestID()), p, FactListOptions{}); err != nil {
		t.Fatal(err)
	}
	tx, err := st.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `DELETE FROM usage_rollups WHERE account_id=$1 AND dimension='operation_read'`, p.AccountID); err != nil {
		t.Fatal(err)
	}
	if err := validateImportedUsageRollups(ctx, tx, p.AccountID); !errors.Is(err, ErrArchiveContent) {
		t.Fatal("retention exemption accepted missing rollups", err)
	}
}

func TestActivityRetentionWorkerQueryFailure(t *testing.T) {
	st, _ := activityTestStore(t)
	ctx := t.Context()
	// A statement trigger fails the real retention DELETE even with no old rows.
	if _, err := st.pool.Exec(ctx, `CREATE FUNCTION fail_activity_retention_test() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'synthetic retention query failure'; END $$; CREATE TRIGGER fail_activity_retention_test BEFORE DELETE ON usage_events FOR EACH STATEMENT EXECUTE FUNCTION fail_activity_retention_test()`); err != nil {
		t.Fatal(err)
	}
	workerCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var results []MessageRateBucketCleanupBatchResult
	if err := st.RunMessageRateBucketCleanupWorker(workerCtx, DefaultMessageRateBucketCleanupWorkerConfig(), func(result MessageRateBucketCleanupBatchResult) {
		results = append(results, result)
		cancel()
	}); err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].RateBucketError != nil {
		t.Fatalf("rate-bucket success missing: %+v", results)
	}
	var cause *pgconn.PgError
	err := results[0].ActivityRetentionError
	if !errors.As(err, &cause) || cause.Code != "P0001" || !strings.Contains(err.Error(), "activity event retention: ERROR: synthetic retention query failure") {
		t.Fatalf("retention query cause missing: %v", err)
	}
}
