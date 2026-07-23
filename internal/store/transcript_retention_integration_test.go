package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestTranscriptRetentionDeletesOnlyExpiredWholeConversationsPostgres(t *testing.T) {
	dsn := os.Getenv("WITSELF_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("WITSELF_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	st, schemaDSN := newMigrationTestStore(t, dsn)
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}
	provisioned, err := st.ProvisionAccount(ctx,
		"retention-test@witwave.ai", "retention test", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = deleteAccountForIntegrationTest(ctx, st, provisioned.AccountID) }()
	if activated, err := st.ActivateAccount(ctx, provisioned.AccountID); err != nil || !activated {
		t.Fatalf("activate = %v / %v", activated, err)
	}
	realm, err := st.CreateRealm(ctx, provisioned.AccountID, "default")
	if err != nil {
		t.Fatal(err)
	}
	agent, err := st.CreateAgent(ctx, provisioned.AccountID, realm.ID, "recorder")
	if err != nil {
		t.Fatal(err)
	}
	p := Principal{
		Kind: PrincipalAgent, ID: agent.ID, AccountID: provisioned.AccountID,
		RealmID: realm.ID, AccountStatus: "active",
	}
	create := func(externalID string) Transcript {
		t.Helper()
		tr, err := st.CreateTranscript(ctx, p.AccountID, p.RealmID, p.ID,
			CreateTranscriptInput{ExternalID: externalID})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.AppendTranscriptEntry(ctx, p.AccountID, p.RealmID, p.ID, tr.ID,
			AppendTranscriptEntryInput{Role: TranscriptRoleUser, Body: externalID}); err != nil {
			t.Fatal(err)
		}
		return tr
	}
	expired := create("expired")
	recent := create("recent")
	indefinite := create("indefinite")
	if _, err := st.pool.Exec(ctx, `
		UPDATE transcript_conversations
		   SET updated_at=statement_timestamp() - interval '31 days'
		 WHERE id IN ($1,$2)`,
		expired.ID, indefinite.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.SetAccountPlan(ctx, p.AccountID, 0, "", "free",
		map[string]int64{},
		map[string]int64{TranscriptRetentionDaysPolicy: 30}, nil); err != nil {
		t.Fatal(err)
	}
	policyChange, err := st.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := policyChange.Exec(ctx,
		`UPDATE accounts SET plan_policies='{}'::jsonb WHERE id=$1`,
		p.AccountID); err != nil {
		_ = policyChange.Rollback(ctx)
		t.Fatal(err)
	}
	result, err := st.PreviewTranscriptRetentionBatch(ctx, 10)
	if err != nil {
		_ = policyChange.Rollback(ctx)
		t.Fatal(err)
	}
	if result.Eligible != 0 {
		_ = policyChange.Rollback(ctx)
		t.Fatalf("concurrent indefinite policy change raced retention: %+v", result)
	}
	if err := policyChange.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := st.SetAccountPlan(ctx, p.AccountID, 0, "", "free",
		map[string]int64{},
		map[string]int64{TranscriptRetentionDaysPolicy: 30}, nil); err != nil {
		t.Fatal(err)
	}
	result, err = st.PreviewTranscriptRetentionBatch(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if result.Eligible != 1 || !result.EligibleScanCapped || result.Deleted != 0 {
		t.Fatalf("bounded preview result = %+v; want one capped eligible and no deletes", result)
	}

	result, err = st.PreviewTranscriptRetentionBatch(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	// Preview progress is persisted independently from enforcement progress,
	// so this sweep resumes after the one candidate examined above.
	if result.Scanned != 1 || result.Eligible != 1 || result.Deleted != 0 ||
		result.EligibleScanCapped ||
		result.DeferredEvidence != 0 || result.DeferredCuration != 0 {
		t.Fatalf("preview result = %+v; want one resumed eligible and no deletes", result)
	}
	for _, id := range []string{expired.ID, indefinite.ID, recent.ID} {
		if _, _, err := st.GetTranscript(ctx, p, id); err != nil {
			t.Fatalf("preview removed transcript %s: %v", id, err)
		}
	}

	result, err = st.ProcessTranscriptRetentionBatch(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if result.Eligible != 2 || result.Deleted != 2 ||
		result.DeferredEvidence != 0 || result.DeferredCuration != 0 {
		t.Fatalf("result = %+v; want two expired whole conversations", result)
	}
	for _, id := range []string{expired.ID, indefinite.ID} {
		if _, _, err := st.GetTranscript(ctx, p, id); !errors.Is(err, ErrTranscriptNotFound) {
			t.Fatalf("deleted transcript %s error = %v", id, err)
		}
	}
	if _, _, err := st.GetTranscript(ctx, p, recent.ID); err != nil {
		t.Fatalf("recent transcript was removed: %v", err)
	}

	evidenceHeld := create("resolved-evidence-hold")
	evidenceEntry, err := st.AppendTranscriptEntry(
		ctx, p.AccountID, p.RealmID, p.ID, evidenceHeld.ID,
		AppendTranscriptEntryInput{Role: TranscriptRoleAssistant, Body: "evidence"},
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CaptureMemory(ctx, p, CaptureMemoryInput{
		Content: "retention evidence remains available", Kind: "decision",
		Evidence: []MemoryEvidenceInput{{
			ResolutionState: MemoryEvidenceResolved, ResolvedKind: "transcript",
			SourceTranscriptID: evidenceHeld.ID,
			SourceSequenceFrom: evidenceEntry.Sequence, SourceSequenceUntil: evidenceEntry.Sequence,
		}},
		IdempotencyKey: "retention-evidence-capture",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.pool.Exec(ctx, `
		UPDATE transcript_conversations
		   SET updated_at=statement_timestamp() - interval '31 days'
		 WHERE id=$1`, evidenceHeld.ID); err != nil {
		t.Fatal(err)
	}
	result, err = st.PreviewTranscriptRetentionBatch(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if result.Eligible != 0 || result.DeferredEvidence != 1 {
		t.Fatalf("resolved evidence hold result = %+v; want one deferred transcript", result)
	}

	curationHeld := create("active-curation-hold")
	if _, err := st.pool.Exec(ctx, `
		UPDATE transcript_conversations
		   SET updated_at=statement_timestamp() - interval '31 days'
		 WHERE id=$1`, curationHeld.ID); err != nil {
		t.Fatal(err)
	}
	requested, err := st.RequestCuration(ctx, p, RequestMemoryCurationInput{
		TriggerReason: "session_end", IdempotencyKey: "retention-hold-request",
	})
	if err != nil {
		t.Fatal(err)
	}
	started, err := st.StartCuration(ctx, p, StartMemoryCurationInput{
		RequestID: requested.Request.ID, LeaseDuration: time.Minute,
		Client:         MemoryClientProvenance{Runtime: "retention-test", Model: "test"},
		IdempotencyKey: "retention-hold-start",
	})
	if err != nil {
		t.Fatal(err)
	}
	if started.Run.TranscriptInputCount == 0 {
		t.Fatal("active curation run did not freeze the expired transcript")
	}
	var frozenTranscriptSourceInputs, frozenRunInputs, frozenCursorInputs int
	if err := st.pool.QueryRow(ctx, `
		SELECT
		  count(*) FILTER (
		    WHERE transcript_id=$2
		       OR (input_kind='cursor' AND cursor_source_kind='transcript'
		           AND cursor_stream_id=$2)
		  ),
		  count(*),
		  count(*) FILTER (
		    WHERE input_kind='cursor' AND cursor_source_kind='transcript'
		      AND cursor_stream_id=$2
		  )
		FROM memory_curation_run_inputs
		WHERE run_id=$1`, started.Run.ID, curationHeld.ID).Scan(
		&frozenTranscriptSourceInputs, &frozenRunInputs, &frozenCursorInputs,
	); err != nil {
		t.Fatal(err)
	}
	if frozenTranscriptSourceInputs < 2 || frozenCursorInputs != 1 {
		t.Fatalf("frozen transcript source inputs = %d, cursors = %d; want data and cursor inputs",
			frozenTranscriptSourceInputs, frozenCursorInputs)
	}
	result, err = st.PreviewTranscriptRetentionBatch(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if result.Eligible != 0 || result.DeferredCuration != 1 {
		t.Fatalf("active curation hold result = %+v; want one deferred transcript", result)
	}
	if _, err := st.CancelCuration(ctx, p, started.Run.ID, FinishMemoryCurationInput{
		FencingGeneration: started.Run.FencingGeneration,
		Reason:            "retention_test_complete",
		IdempotencyKey:    "retention-hold-cancel",
	}); err != nil {
		t.Fatal(err)
	}
	result, err = st.ProcessTranscriptRetentionBatch(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if result.Eligible != 1 || result.Deleted != 1 || result.DeferredCuration != 0 ||
		result.ReleasedCurationInputs == 0 || result.DeletedCurationCursors == 0 {
		t.Fatalf("terminal curation run still held transcript: %+v", result)
	}
	if _, _, err := st.GetTranscript(ctx, p, curationHeld.ID); !errors.Is(err, ErrTranscriptNotFound) {
		t.Fatalf("terminal-run transcript error = %v; want deleted", err)
	}
	var retainedRunInputs, prunedInputs, attachedPrunedInputs int
	if err := st.pool.QueryRow(ctx, `
		SELECT count(*),
		       count(*) FILTER (WHERE transcript_pruned_at IS NOT NULL),
		       count(*) FILTER (
		         WHERE transcript_pruned_at IS NOT NULL
		           AND (transcript_id IS NOT NULL OR cursor_stream_id IS NOT NULL)
		       )
		  FROM memory_curation_run_inputs
		 WHERE run_id=$1`, started.Run.ID).Scan(
		&retainedRunInputs, &prunedInputs, &attachedPrunedInputs,
	); err != nil {
		t.Fatal(err)
	}
	if retainedRunInputs != frozenRunInputs ||
		prunedInputs != frozenTranscriptSourceInputs || attachedPrunedInputs != 0 {
		t.Fatalf("terminal run inputs = retained:%d pruned:%d attached-pruned:%d; want %d/%d/0",
			retainedRunInputs, prunedInputs, attachedPrunedInputs,
			frozenRunInputs, frozenTranscriptSourceInputs)
	}
	var runInputCount, runTranscriptInputCount, runCursorInputCount int
	if err := st.pool.QueryRow(ctx, `
		SELECT input_count,transcript_input_count,cursor_input_count
		  FROM memory_curation_runs WHERE id=$1`, started.Run.ID).Scan(
		&runInputCount, &runTranscriptInputCount, &runCursorInputCount,
	); err != nil {
		t.Fatal(err)
	}
	if runInputCount != started.Run.InputCount ||
		runTranscriptInputCount != started.Run.TranscriptInputCount ||
		runCursorInputCount != started.Run.CursorInputCount {
		t.Fatalf("terminal run receipt counters changed: got %d/%d/%d, want %d/%d/%d",
			runInputCount, runTranscriptInputCount, runCursorInputCount,
			started.Run.InputCount, started.Run.TranscriptInputCount, started.Run.CursorInputCount)
	}
	var liveTranscriptCursorCount int
	if err := st.pool.QueryRow(ctx, `
		SELECT count(*) FROM memory_curation_cursors
		 WHERE account_id=$1 AND realm_id=$2 AND owner_kind='agent' AND owner_id=$3
		   AND source_kind='transcript' AND source_stream_id=$4`,
		p.AccountID, p.RealmID, p.ID, curationHeld.ID).Scan(
		&liveTranscriptCursorCount,
	); err != nil {
		t.Fatal(err)
	}
	if liveTranscriptCursorCount != 0 {
		t.Fatalf("live transcript cursor count = %d, want 0", liveTranscriptCursorCount)
	}

	kept := create("enterprise-indefinite")
	if _, err := st.pool.Exec(ctx,
		`UPDATE transcript_conversations
		    SET updated_at=statement_timestamp() - interval '400 days'
		  WHERE id=$1`,
		kept.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.SetAccountPlan(ctx, p.AccountID, 0, "", "enterprise",
		map[string]int64{}, map[string]int64{}, nil); err != nil {
		t.Fatal(err)
	}
	result, err = st.ProcessTranscriptRetentionBatch(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if result.Eligible != 0 || result.Deleted != 0 {
		t.Fatalf("indefinite result = %+v", result)
	}
	if _, _, err := st.GetTranscript(ctx, p, kept.ID); err != nil {
		t.Fatalf("indefinite transcript was removed: %v", err)
	}

	// Once a source identity has been pruned, migration 62 cannot reconstruct
	// it. The down guard must refuse before dropping any index or constraint.
	if err := migrationTestDown(t, schemaDSN, true); err == nil ||
		!strings.Contains(err.Error(), "pruned curation inputs exist") {
		t.Fatalf("migration down with pruned inputs error = %v", err)
	}
	assertMigrationTestVersion(t, schemaDSN, int64(SchemaVersion()))
	assertMigrationTestColumn(t, st, "memory_curation_run_inputs", "transcript_pruned_at", true)
	assertMigrationTestIndex(t, st, "memory_curation_run_inputs",
		"memory_curation_run_inputs_by_transcript_cursor", true)
	assertMigrationTestTable(t, st, "transcript_retention_worker_lanes", true)
	assertMigrationTestIndex(t, st, "accounts",
		"accounts_transcript_retention_worker_lane_idx", true)
}

func TestTranscriptRetentionBatchIsFairAndOldestFirstAcrossAccountsPostgres(t *testing.T) {
	dsn := os.Getenv("WITSELF_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("WITSELF_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	st, _ := newMigrationTestStore(t, dsn)
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}
	provision := func(email, name string) Principal {
		t.Helper()
		account, err := st.ProvisionAccount(ctx, email, name, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		if activated, err := st.ActivateAccount(ctx, account.AccountID); err != nil || !activated {
			t.Fatalf("activate %s = %v / %v", name, activated, err)
		}
		realm, err := st.CreateRealm(ctx, account.AccountID, "default")
		if err != nil {
			t.Fatal(err)
		}
		agent, err := st.CreateAgent(ctx, account.AccountID, realm.ID, "recorder")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.SetAccountPlan(ctx, account.AccountID, 0, "", "free",
			map[string]int64{},
			map[string]int64{TranscriptRetentionDaysPolicy: 30}, nil); err != nil {
			t.Fatal(err)
		}
		return Principal{
			Kind: PrincipalAgent, ID: agent.ID, AccountID: account.AccountID,
			RealmID: realm.ID, AccountStatus: "active",
		}
	}
	first := provision("retention-fair-first@witwave.ai", "retention fair first")
	second := provision("retention-fair-second@witwave.ai", "retention fair second")
	createExpired := func(p Principal, externalID string, age time.Duration) Transcript {
		t.Helper()
		transcript, err := st.CreateTranscript(ctx, p.AccountID, p.RealmID, p.ID,
			CreateTranscriptInput{ExternalID: externalID})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.AppendTranscriptEntry(ctx, p.AccountID, p.RealmID, p.ID,
			transcript.ID, AppendTranscriptEntryInput{
				Role: TranscriptRoleUser, Body: externalID,
			}); err != nil {
			t.Fatal(err)
		}
		if _, err := st.pool.Exec(ctx, `
			UPDATE transcript_conversations
			   SET updated_at=statement_timestamp() - $2::interval
			 WHERE id=$1`, transcript.ID, age); err != nil {
			t.Fatal(err)
		}
		return transcript
	}

	// The first account has the globally oldest backlog. A global oldest-first
	// LIMIT would consume both slots there; a newest-first account scan would
	// pick neither account's oldest row.
	firstOldest := createExpired(first, "first-oldest", 100*24*time.Hour)
	firstNewer := createExpired(first, "first-newer", 90*24*time.Hour)
	secondOldest := createExpired(second, "second-oldest", 60*24*time.Hour)
	secondNewer := createExpired(second, "second-newer", 50*24*time.Hour)

	result, err := st.ProcessTranscriptRetentionBatch(ctx, 2)
	if err != nil {
		t.Fatal(err)
	}
	if result.Eligible != 2 || result.Deleted != 2 {
		t.Fatalf("fair batch result = %+v; want two deletions", result)
	}
	for _, item := range []struct {
		p  Principal
		id string
	}{
		{first, firstOldest.ID},
		{second, secondOldest.ID},
	} {
		if _, _, err := st.GetTranscript(ctx, item.p, item.id); !errors.Is(err, ErrTranscriptNotFound) {
			t.Fatalf("oldest transcript %s error = %v; want deleted", item.id, err)
		}
	}
	for _, item := range []struct {
		p  Principal
		id string
	}{
		{first, firstNewer.ID},
		{second, secondNewer.ID},
	} {
		if _, _, err := st.GetTranscript(ctx, item.p, item.id); err != nil {
			t.Fatalf("newer transcript %s was removed: %v", item.id, err)
		}
	}

	// The persisted wraparound cursor resumes at the next account even when the
	// batch size is one. Across two more sweeps, both accounts make progress.
	for sweep := 0; sweep < 2; sweep++ {
		result, err = st.ProcessTranscriptRetentionBatch(ctx, 1)
		if err != nil {
			t.Fatal(err)
		}
		if result.Eligible != 1 || result.Deleted != 1 {
			t.Fatalf("single-account sweep %d = %+v; want one deletion", sweep+1, result)
		}
	}
	for _, item := range []struct {
		p  Principal
		id string
	}{
		{first, firstNewer.ID},
		{second, secondNewer.ID},
	} {
		if _, _, err := st.GetTranscript(ctx, item.p, item.id); !errors.Is(err, ErrTranscriptNotFound) {
			t.Fatalf("round-robin transcript %s error = %v; want deleted", item.id, err)
		}
	}
}

func TestTranscriptRetentionHeldBacklogUsesBoundedPersistentScanPostgres(t *testing.T) {
	dsn := os.Getenv("WITSELF_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("WITSELF_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	st, schemaDSN := newMigrationTestStore(t, dsn)
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}
	account, err := st.ProvisionAccount(ctx,
		"retention-held-prefix@witwave.ai", "retention held prefix", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if activated, err := st.ActivateAccount(ctx, account.AccountID); err != nil || !activated {
		t.Fatalf("activate = %v / %v", activated, err)
	}
	realm, err := st.CreateRealm(ctx, account.AccountID, "default")
	if err != nil {
		t.Fatal(err)
	}
	agent, err := st.CreateAgent(ctx, account.AccountID, realm.ID, "recorder")
	if err != nil {
		t.Fatal(err)
	}
	p := Principal{
		Kind: PrincipalAgent, ID: agent.ID, AccountID: account.AccountID,
		RealmID: realm.ID, AccountStatus: "active",
	}
	if _, err := st.SetAccountPlan(ctx, p.AccountID, 0, "", "free",
		map[string]int64{},
		map[string]int64{TranscriptRetentionDaysPolicy: 30}, nil); err != nil {
		t.Fatal(err)
	}
	createExpired := func(externalID string, ageDays int) (Transcript, TranscriptEntry) {
		t.Helper()
		transcript, err := st.CreateTranscript(ctx, p.AccountID, p.RealmID, p.ID,
			CreateTranscriptInput{ExternalID: externalID})
		if err != nil {
			t.Fatal(err)
		}
		entry, err := st.AppendTranscriptEntry(ctx, p.AccountID, p.RealmID, p.ID,
			transcript.ID, AppendTranscriptEntryInput{
				Role: TranscriptRoleUser, Body: externalID,
			})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.pool.Exec(ctx, `
			UPDATE transcript_conversations
			   SET updated_at=statement_timestamp() - make_interval(days => $2)
			 WHERE id=$1`, transcript.ID, ageDays); err != nil {
			t.Fatal(err)
		}
		return transcript, entry
	}
	for i, age := range []int{100, 90, 80} {
		transcript, entry := createExpired(fmt.Sprintf("held-%d", i), age)
		if _, err := st.CaptureMemory(ctx, p, CaptureMemoryInput{
			Content: fmt.Sprintf("hold transcript %d", i), Kind: "decision",
			Evidence: []MemoryEvidenceInput{{
				ResolutionState:     MemoryEvidenceResolved,
				ResolvedKind:        "transcript",
				SourceTranscriptID:  transcript.ID,
				SourceSequenceFrom:  entry.Sequence,
				SourceSequenceUntil: entry.Sequence,
			}},
			IdempotencyKey: fmt.Sprintf("retention-held-prefix-%d", i),
		}); err != nil {
			t.Fatal(err)
		}
	}
	eligible, _ := createExpired("eligible-after-held-prefix", 70)

	first, err := st.ProcessTranscriptRetentionBatch(ctx, 2)
	if err != nil {
		t.Fatal(err)
	}
	if first.Scanned != 2 || first.DeferredEvidence != 2 ||
		first.Eligible != 0 || first.Deleted != 0 || !first.ScanCapped {
		t.Fatalf("first bounded held-prefix scan = %+v", first)
	}

	// Reopening the store proves the per-account keyset cursor, rather than
	// process memory, carries progress past the first two held rows.
	reopened, err := Open(ctx, schemaDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	second, err := reopened.ProcessTranscriptRetentionBatch(ctx, 2)
	if err != nil {
		t.Fatal(err)
	}
	if second.Scanned != 2 || second.DeferredEvidence != 1 ||
		second.Eligible != 1 || second.Deleted != 1 || !second.ScanCapped {
		t.Fatalf("second bounded held-prefix scan = %+v", second)
	}
	if _, _, err := reopened.GetTranscript(ctx, p, eligible.ID); !errors.Is(err, ErrTranscriptNotFound) {
		t.Fatalf("eligible transcript behind held prefix error = %v; want deleted", err)
	}
}

func TestTranscriptRetentionWorkerCadenceAndModeProgressAreDurablePostgres(t *testing.T) {
	dsn := os.Getenv("WITSELF_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("WITSELF_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	st, _ := newMigrationTestStore(t, dsn)
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}
	account, err := st.ProvisionAccount(ctx,
		"retention-cadence@witwave.ai", "retention cadence", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if activated, err := st.ActivateAccount(ctx, account.AccountID); err != nil || !activated {
		t.Fatalf("activate = %v / %v", activated, err)
	}
	realm, err := st.CreateRealm(ctx, account.AccountID, "default")
	if err != nil {
		t.Fatal(err)
	}
	agent, err := st.CreateAgent(ctx, account.AccountID, realm.ID, "recorder")
	if err != nil {
		t.Fatal(err)
	}
	p := Principal{
		Kind: PrincipalAgent, ID: agent.ID, AccountID: account.AccountID,
		RealmID: realm.ID, AccountStatus: "active",
	}
	if _, err := st.SetAccountPlan(ctx, p.AccountID, 0, "", "free",
		map[string]int64{},
		map[string]int64{TranscriptRetentionDaysPolicy: 30}, nil); err != nil {
		t.Fatal(err)
	}
	for i, age := range []int{60, 50} {
		transcript, err := st.CreateTranscript(ctx, p.AccountID, p.RealmID, p.ID,
			CreateTranscriptInput{ExternalID: fmt.Sprintf("cadence-%d", i)})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.AppendTranscriptEntry(ctx, p.AccountID, p.RealmID, p.ID,
			transcript.ID, AppendTranscriptEntryInput{
				Role: TranscriptRoleUser, Body: transcript.ExternalID,
			}); err != nil {
			t.Fatal(err)
		}
		if _, err := st.pool.Exec(ctx, `
			UPDATE transcript_conversations
			   SET updated_at=statement_timestamp() - make_interval(days => $2)
			 WHERE id=$1`, transcript.ID, age); err != nil {
			t.Fatal(err)
		}
	}

	preview, err := st.PreviewTranscriptRetentionBatch(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if preview.Scanned != 1 || preview.Eligible != 1 {
		t.Fatalf("preview before enforcement = %+v", preview)
	}

	runWorkerOnce := func() TranscriptRetentionBatchResult {
		t.Helper()
		workerCtx, cancel := context.WithCancel(ctx)
		defer cancel()
		results := make(chan TranscriptRetentionBatchResult, 1)
		errs := make(chan error, 2)
		done := make(chan error, 1)
		go func() {
			done <- st.RunTranscriptRetentionWorker(workerCtx,
				TranscriptRetentionWorkerConfig{
					BatchSize: 1, Interval: time.Minute,
					BatchTimeout: 5 * time.Second,
					LaneCount:    defaultTranscriptRetentionWorkerLaneCount,
					Mode:         TranscriptRetentionModeEnforce,
				},
				func(result TranscriptRetentionBatchResult) {
					results <- result
					cancel()
				},
				func(err error) {
					errs <- err
					cancel()
				})
		}()
		select {
		case err := <-errs:
			t.Fatal(err)
		case result := <-results:
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			return result
		case <-time.After(5 * time.Second):
			t.Fatal("timed out waiting for one retention worker attempt")
		}
		return TranscriptRetentionBatchResult{}
	}

	firstWorker := runWorkerOnce()
	if firstWorker.Scanned != 1 || firstWorker.Deleted != 1 {
		t.Fatalf("first enforce worker after preview = %+v", firstWorker)
	}
	var workerLane int
	if err := st.pool.QueryRow(ctx, `
		SELECT get_byte(decode(md5(id), 'hex'), 0) % 16
		  FROM accounts
		 WHERE id=$1`, p.AccountID).Scan(&workerLane); err != nil {
		t.Fatal(err)
	}
	var generationAfterFirst int64
	var nextRunAfterFirst time.Time
	if err := st.pool.QueryRow(ctx, `
			SELECT generation,next_run_at
			  FROM transcript_retention_worker_lanes
			 WHERE mode='enforce' AND lane_id=$1`, workerLane).Scan(
		&generationAfterFirst, &nextRunAfterFirst,
	); err != nil {
		t.Fatal(err)
	}
	if !nextRunAfterFirst.After(time.Now()) {
		t.Fatalf("worker next_run_at = %s; want future durable fence", nextRunAfterFirst)
	}

	secondWorker := runWorkerOnce()
	if secondWorker != (TranscriptRetentionBatchResult{}) {
		t.Fatalf("staggered worker inside cadence fence = %+v; want no-op", secondWorker)
	}
	var generationAfterSecond int64
	if err := st.pool.QueryRow(ctx, `
			SELECT generation
			  FROM transcript_retention_worker_lanes
			 WHERE mode='enforce' AND lane_id=$1`, workerLane,
	).Scan(&generationAfterSecond); err != nil {
		t.Fatal(err)
	}
	if generationAfterSecond != generationAfterFirst {
		t.Fatalf("gated worker advanced generation from %d to %d",
			generationAfterFirst, generationAfterSecond)
	}
	// Sparse-lane draining is deliberate: the two immediate attempts together
	// advance every empty lane once, but the account's non-empty lane remains
	// protected by its own future cadence fence.
	var emptyLanesAdvanced int
	if err := st.pool.QueryRow(ctx, `
		SELECT count(*)
		  FROM transcript_retention_worker_lanes
		 WHERE mode='enforce' AND lane_id<>$1 AND generation=1`,
		workerLane,
	).Scan(&emptyLanesAdvanced); err != nil {
		t.Fatal(err)
	}
	if emptyLanesAdvanced != defaultTranscriptRetentionWorkerLaneCount-1 {
		t.Fatalf("advanced empty worker lanes = %d, want %d",
			emptyLanesAdvanced, defaultTranscriptRetentionWorkerLaneCount-1)
	}

	direct, err := st.ProcessTranscriptRetentionBatch(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if direct.Scanned != 1 || direct.Deleted != 1 {
		t.Fatalf("manual batch inside worker cadence fence = %+v", direct)
	}
	var nextRunAfterDirect time.Time
	if err := st.pool.QueryRow(ctx, `
			SELECT next_run_at
			  FROM transcript_retention_worker_lanes
			 WHERE mode='enforce' AND lane_id=$1`, workerLane,
	).Scan(&nextRunAfterDirect); err != nil {
		t.Fatal(err)
	}
	if !nextRunAfterDirect.Equal(nextRunAfterFirst) {
		t.Fatalf("manual batch changed worker next_run_at from %s to %s",
			nextRunAfterFirst, nextRunAfterDirect)
	}
}

func TestTranscriptRetentionSweepClaimSkipsBusyReplicaPostgres(t *testing.T) {
	dsn := os.Getenv("WITSELF_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("WITSELF_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	st, _ := newMigrationTestStore(t, dsn)
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}
	claim, err := st.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = claim.Rollback(ctx) }()
	var generationBefore int64
	if err := claim.QueryRow(ctx, `
		SELECT generation
		  FROM transcript_retention_sweep_state
		 WHERE singleton
		 FOR UPDATE`).Scan(&generationBefore); err != nil {
		t.Fatal(err)
	}

	noWaitCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	result, err := st.PreviewTranscriptRetentionBatch(noWaitCtx, 10)
	if err != nil {
		t.Fatalf("busy replica preview blocked or failed: %v", err)
	}
	if result != (TranscriptRetentionBatchResult{}) {
		t.Fatalf("busy replica preview = %+v; want clean no-op", result)
	}
	var generationWhileClaimed int64
	if err := st.pool.QueryRow(ctx, `
		SELECT generation FROM transcript_retention_sweep_state WHERE singleton`,
	).Scan(&generationWhileClaimed); err != nil {
		t.Fatal(err)
	}
	if generationWhileClaimed != generationBefore {
		t.Fatalf("losing replica advanced sweep generation from %d to %d",
			generationBefore, generationWhileClaimed)
	}
	workerCfg := DefaultTranscriptRetentionWorkerConfig()
	workerCfg.BatchSize = 10
	workerCfg.Interval = time.Minute
	workerCfg.BatchTimeout = 2 * time.Second
	workerResult, err := st.processTranscriptRetentionWorkerBatch(noWaitCtx, workerCfg)
	if err != nil {
		t.Fatalf("lane worker behind legacy claim failed: %v", err)
	}
	if workerResult != (TranscriptRetentionBatchResult{}) {
		t.Fatalf("lane worker behind legacy claim = %+v; want clean no-op", workerResult)
	}
	var advancedWorkerLanes int
	if err := st.pool.QueryRow(ctx, `
		SELECT count(*)
		  FROM transcript_retention_worker_lanes
		 WHERE generation<>0`).Scan(&advancedWorkerLanes); err != nil {
		t.Fatal(err)
	}
	if advancedWorkerLanes != 0 {
		t.Fatalf("lane worker advanced %d lanes behind legacy claim", advancedWorkerLanes)
	}
	if err := claim.Rollback(ctx); err != nil {
		t.Fatal(err)
	}

	if _, err := st.PreviewTranscriptRetentionBatch(ctx, 10); err != nil {
		t.Fatal(err)
	}
	var generationAfter int64
	if err := st.pool.QueryRow(ctx, `
		SELECT generation FROM transcript_retention_sweep_state WHERE singleton`,
	).Scan(&generationAfter); err != nil {
		t.Fatal(err)
	}
	if generationAfter != generationBefore+1 {
		t.Fatalf("winning replica sweep generation = %d, want %d",
			generationAfter, generationBefore+1)
	}
}

func TestTranscriptRetentionLaneMigrationHandsOffScheduledCadencePostgres(t *testing.T) {
	dsn := os.Getenv("WITSELF_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("WITSELF_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	st, schemaDSN := newMigrationTestStore(t, dsn)
	migrationTestUpTo(t, schemaDSN, 65)

	principal, _ := provisionTranscriptRetentionWorkerPrincipal(
		ctx, t, st, "lane-migration-handoff",
	)
	first := enableTranscriptRetentionAndCreateExpired(
		ctx, t, st, principal, "lane-migration-first",
	)
	second, err := st.CreateTranscript(
		ctx,
		principal.AccountID,
		principal.RealmID,
		principal.ID,
		CreateTranscriptInput{ExternalID: "lane-migration-second"},
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.AppendTranscriptEntry(
		ctx,
		principal.AccountID,
		principal.RealmID,
		principal.ID,
		second.ID,
		AppendTranscriptEntryInput{
			Role: TranscriptRoleUser,
			Body: "lane-migration-second",
		},
	); err != nil {
		t.Fatal(err)
	}
	if _, err := st.pool.Exec(ctx, `
		UPDATE transcript_conversations
		   SET updated_at=statement_timestamp() - interval '31 days'
		 WHERE id=$1`, second.ID); err != nil {
		t.Fatal(err)
	}

	// Model an old scheduled worker that already owns the singleton while
	// migration 66 builds its concurrent account index. The migration must not
	// copy lane state until this claim finishes.
	legacyClaim, err := st.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = legacyClaim.Rollback(ctx) }()
	if _, err := legacyClaim.Exec(ctx, `LOCK TABLE accounts IN SHARE MODE`); err != nil {
		t.Fatal(err)
	}
	var claimedGeneration int64
	if err := legacyClaim.QueryRow(ctx, `
		SELECT generation
		  FROM transcript_retention_sweep_state
		 WHERE singleton
		 FOR UPDATE`).Scan(&claimedGeneration); err != nil {
		t.Fatal(err)
	}
	migrationDone := make(chan error, 1)
	go func() {
		migrationDone <- st.Migrate()
	}()
	indexDeadline := time.Now().Add(10 * time.Second)
	for {
		var indexWaiting bool
		if err := st.pool.QueryRow(ctx, `
			SELECT EXISTS (
			  SELECT 1
			    FROM pg_locks lock
			    JOIN pg_class relation ON relation.oid=lock.relation
			   WHERE relation.relname='accounts'
			     AND relation.relnamespace=(current_schema())::regnamespace
			     AND NOT lock.granted
			)`).Scan(&indexWaiting); err != nil {
			t.Fatal(err)
		}
		if indexWaiting {
			break
		}
		select {
		case err := <-migrationDone:
			t.Fatalf("migration finished before legacy claim was released: %v", err)
		default:
		}
		if time.Now().After(indexDeadline) {
			t.Fatal("timed out waiting for migration 66 concurrent index lock")
		}
		time.Sleep(10 * time.Millisecond)
	}
	var lanesBeforeHandoff int
	if err := st.pool.QueryRow(ctx, `
		SELECT count(*)
		  FROM transcript_retention_worker_lanes`).Scan(&lanesBeforeHandoff); err != nil {
		t.Fatal(err)
	}
	if lanesBeforeHandoff != 0 {
		t.Fatalf("worker lanes seeded before legacy claim handoff = %d, want 0",
			lanesBeforeHandoff)
	}

	const inheritedLaneDue = "2001-01-02 03:04:05+00"
	if _, err := legacyClaim.Exec(ctx, `
		UPDATE transcript_retention_sweep_state
		   SET generation=generation + 1,
		       next_run_at=$1::timestamptz,
		       updated_at=statement_timestamp()
		 WHERE singleton`, inheritedLaneDue); err != nil {
		t.Fatal(err)
	}
	if err := legacyClaim.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-migrationDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for migration 66 handoff")
	}
	assertMigrationTestVersion(t, schemaDSN, 66)

	var singletonParked bool
	if err := st.pool.QueryRow(ctx, `
		SELECT next_run_at='infinity'::timestamptz
		  FROM transcript_retention_sweep_state
		 WHERE singleton`).Scan(&singletonParked); err != nil {
		t.Fatal(err)
	}
	if !singletonParked {
		t.Fatal("migration did not park the legacy scheduled singleton")
	}
	var inheritedDueLanes int
	if err := st.pool.QueryRow(ctx, `
		SELECT count(*)
		  FROM transcript_retention_worker_lanes
		 WHERE next_run_at=$1::timestamptz`, inheritedLaneDue).Scan(
		&inheritedDueLanes,
	); err != nil {
		t.Fatal(err)
	}
	wantLanes := 2 * defaultTranscriptRetentionWorkerLaneCount
	if inheritedDueLanes != wantLanes {
		t.Fatalf("due lanes inherited from singleton = %d, want %d",
			inheritedDueLanes, wantLanes)
	}

	var generationBefore int64
	if err := st.pool.QueryRow(ctx, `
		SELECT generation
		  FROM transcript_retention_sweep_state
		 WHERE singleton`).Scan(&generationBefore); err != nil {
		t.Fatal(err)
	}
	if generationBefore != claimedGeneration+1 {
		t.Fatalf("migration changed legacy generation = %d, want %d",
			generationBefore, claimedGeneration+1)
	}
	oldScheduled, err := st.processTranscriptRetentionBatch(
		ctx, 1, true, time.Minute, 0,
	)
	if err != nil {
		t.Fatal(err)
	}
	if oldScheduled != (TranscriptRetentionBatchResult{}) {
		t.Fatalf("old scheduled batch after lane migration = %+v; want clean no-op",
			oldScheduled)
	}
	var generationAfter int64
	if err := st.pool.QueryRow(ctx, `
		SELECT generation
		  FROM transcript_retention_sweep_state
		 WHERE singleton`).Scan(&generationAfter); err != nil {
		t.Fatal(err)
	}
	if generationAfter != generationBefore {
		t.Fatalf("old scheduled batch advanced singleton generation from %d to %d",
			generationBefore, generationAfter)
	}
	for _, transcript := range []Transcript{first, second} {
		if _, _, err := st.GetTranscript(ctx, principal, transcript.ID); err != nil {
			t.Fatalf("old scheduled batch touched transcript %s: %v",
				transcript.ID, err)
		}
	}

	cfg := DefaultTranscriptRetentionWorkerConfig()
	cfg.BatchSize = 1
	cfg.Interval = time.Minute
	cfg.BatchTimeout = 5 * time.Second
	cfg.Mode = TranscriptRetentionModeEnforce
	workerResult, err := st.processTranscriptRetentionWorkerBatch(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if workerResult.Scanned != 1 || workerResult.Eligible != 1 ||
		workerResult.Deleted != 1 {
		t.Fatalf("lane worker after migration = %+v; want one deletion", workerResult)
	}

	directResult, err := st.ProcessTranscriptRetentionBatch(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if directResult.Scanned != 1 || directResult.Eligible != 1 ||
		directResult.Deleted != 1 {
		t.Fatalf("direct interval-zero batch after migration = %+v; want one deletion",
			directResult)
	}
	for _, transcript := range []Transcript{first, second} {
		if _, _, err := st.GetTranscript(
			ctx, principal, transcript.ID,
		); !errors.Is(err, ErrTranscriptNotFound) {
			t.Fatalf("handoff transcript %s error = %v; want deleted",
				transcript.ID, err)
		}
	}

	const earliestLaneDue = "2040-01-02 03:04:05+00"
	if _, err := st.pool.Exec(ctx, `
		UPDATE transcript_retention_worker_lanes
		   SET next_run_at=$1::timestamptz +
		       (lane_id * interval '1 minute')`, earliestLaneDue); err != nil {
		t.Fatal(err)
	}
	var wantRestored time.Time
	if err := st.pool.QueryRow(ctx, `
		SELECT min(next_run_at)
		  FROM transcript_retention_worker_lanes`).Scan(&wantRestored); err != nil {
		t.Fatal(err)
	}
	if err := migrationTestDown(t, schemaDSN, false); err != nil {
		t.Fatal(err)
	}
	assertMigrationTestVersion(t, schemaDSN, 65)
	assertMigrationTestTable(t, st, "transcript_retention_worker_lanes", false)
	assertMigrationTestIndex(t, st, "accounts",
		"accounts_transcript_retention_worker_lane_idx", false)
	var restored time.Time
	if err := st.pool.QueryRow(ctx, `
		SELECT next_run_at
		  FROM transcript_retention_sweep_state
		 WHERE singleton`).Scan(&restored); err != nil {
		t.Fatal(err)
	}
	if !restored.Equal(wantRestored) {
		t.Fatalf("restored legacy next_run_at = %s, want earliest lane due %s",
			restored, wantRestored)
	}
}

func TestTranscriptRetentionWorkersClaimDifferentLanesConcurrentlyPostgres(t *testing.T) {
	dsn := os.Getenv("WITSELF_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("WITSELF_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	firstStore, schemaDSN := newMigrationTestStore(t, dsn)
	if err := firstStore.Migrate(); err != nil {
		t.Fatal(err)
	}
	secondStore, err := Open(ctx, schemaDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer secondStore.Close()

	first, firstLane := provisionTranscriptRetentionWorkerPrincipal(
		ctx, t, firstStore, "parallel-first",
	)
	var second Principal
	var secondLane int
	for candidate := 0; candidate < 64; candidate++ {
		second, secondLane = provisionTranscriptRetentionWorkerPrincipal(
			ctx, t, firstStore, fmt.Sprintf("parallel-second-%d", candidate),
		)
		if secondLane != firstLane {
			break
		}
	}
	if secondLane == firstLane {
		t.Fatalf("failed to provision accounts in different worker lanes; lane=%d", firstLane)
	}
	firstTranscript := enableTranscriptRetentionAndCreateExpired(
		ctx, t, firstStore, first, "parallel-first",
	)
	secondTranscript := enableTranscriptRetentionAndCreateExpired(
		ctx, t, firstStore, second, "parallel-second",
	)
	if _, err := firstStore.pool.Exec(ctx, `
		UPDATE transcript_retention_worker_lanes
		   SET next_run_at=statement_timestamp() + interval '1 hour'
		 WHERE mode='enforce'`); err != nil {
		t.Fatal(err)
	}
	if _, err := firstStore.pool.Exec(ctx, `
		UPDATE transcript_retention_worker_lanes
		   SET next_run_at='-infinity'::timestamptz
		 WHERE mode='enforce' AND lane_id=ANY($1::smallint[])`,
		[]int16{int16(firstLane), int16(secondLane)}); err != nil {
		t.Fatal(err)
	}

	cfg := DefaultTranscriptRetentionWorkerConfig()
	cfg.BatchSize = 1
	cfg.Interval = time.Minute
	cfg.BatchTimeout = 5 * time.Second
	cfg.Mode = TranscriptRetentionModeEnforce
	type outcome struct {
		result TranscriptRetentionBatchResult
		err    error
	}
	start := make(chan struct{})
	outcomes := make(chan outcome, 2)
	var ready sync.WaitGroup
	ready.Add(2)
	run := func(st *Store) {
		ready.Done()
		<-start
		result, err := st.processTranscriptRetentionWorkerBatch(ctx, cfg)
		outcomes <- outcome{result: result, err: err}
	}
	go run(firstStore)
	go run(secondStore)
	ready.Wait()
	close(start)

	for worker := 0; worker < 2; worker++ {
		select {
		case got := <-outcomes:
			if got.err != nil {
				t.Fatal(got.err)
			}
			if got.result.Scanned != 1 || got.result.Eligible != 1 ||
				got.result.Deleted != 1 {
				t.Fatalf("parallel worker result = %+v; want one deletion", got.result)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("timed out waiting for parallel retention workers")
		}
	}
	for _, item := range []struct {
		principal  Principal
		transcript Transcript
	}{
		{first, firstTranscript},
		{second, secondTranscript},
	} {
		if _, _, err := firstStore.GetTranscript(
			ctx, item.principal, item.transcript.ID,
		); !errors.Is(err, ErrTranscriptNotFound) {
			t.Fatalf("parallel transcript %s error = %v; want deleted",
				item.transcript.ID, err)
		}
	}
	var advanced int
	if err := firstStore.pool.QueryRow(ctx, `
		SELECT count(*)
		  FROM transcript_retention_worker_lanes
		 WHERE mode='enforce'
		   AND lane_id=ANY($1::smallint[])
		   AND generation=1`,
		[]int16{int16(firstLane), int16(secondLane)}).Scan(&advanced); err != nil {
		t.Fatal(err)
	}
	if advanced != 2 {
		t.Fatalf("advanced worker lanes = %d, want 2", advanced)
	}
}

func TestTranscriptRetentionBusyLaneDoesNotBlockAnotherPostgres(t *testing.T) {
	dsn := os.Getenv("WITSELF_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("WITSELF_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	lockingStore, schemaDSN := newMigrationTestStore(t, dsn)
	if err := lockingStore.Migrate(); err != nil {
		t.Fatal(err)
	}
	workerStore, err := Open(ctx, schemaDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer workerStore.Close()

	busy, busyLane := provisionTranscriptRetentionWorkerPrincipal(
		ctx, t, lockingStore, "busy-lane",
	)
	var available Principal
	var availableLane int
	for candidate := 0; candidate < 64; candidate++ {
		available, availableLane = provisionTranscriptRetentionWorkerPrincipal(
			ctx, t, lockingStore, fmt.Sprintf("available-lane-%d", candidate),
		)
		if availableLane != busyLane {
			break
		}
	}
	if availableLane == busyLane {
		t.Fatalf("failed to provision accounts in different worker lanes; lane=%d", busyLane)
	}
	busyTranscript := enableTranscriptRetentionAndCreateExpired(
		ctx, t, lockingStore, busy, "busy-lane",
	)
	availableTranscript := enableTranscriptRetentionAndCreateExpired(
		ctx, t, lockingStore, available, "available-lane",
	)
	if _, err := lockingStore.pool.Exec(ctx, `
		UPDATE transcript_retention_worker_lanes
		   SET next_run_at=statement_timestamp() + interval '1 hour'
		 WHERE mode='enforce'`); err != nil {
		t.Fatal(err)
	}
	if _, err := lockingStore.pool.Exec(ctx, `
		UPDATE transcript_retention_worker_lanes
		   SET next_run_at=CASE
		         WHEN lane_id=$1 THEN '-infinity'::timestamptz
		         ELSE statement_timestamp() - interval '1 hour'
		       END
		 WHERE mode='enforce' AND lane_id IN ($1,$2)`,
		busyLane, availableLane); err != nil {
		t.Fatal(err)
	}

	claim, err := lockingStore.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = claim.Rollback(ctx) }()
	var generation int64
	if err := claim.QueryRow(ctx, `
		SELECT generation
		  FROM transcript_retention_worker_lanes
		 WHERE mode='enforce' AND lane_id=$1
		 FOR UPDATE`, busyLane).Scan(&generation); err != nil {
		t.Fatal(err)
	}

	cfg := DefaultTranscriptRetentionWorkerConfig()
	cfg.BatchSize = 1
	cfg.Interval = time.Minute
	cfg.BatchTimeout = 5 * time.Second
	cfg.Mode = TranscriptRetentionModeEnforce
	result, err := workerStore.processTranscriptRetentionWorkerBatch(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if result.Scanned != 1 || result.Eligible != 1 || result.Deleted != 1 {
		t.Fatalf("worker behind busy lane = %+v; want available-lane deletion", result)
	}
	if _, _, err := lockingStore.GetTranscript(
		ctx, available, availableTranscript.ID,
	); !errors.Is(err, ErrTranscriptNotFound) {
		t.Fatalf("available-lane transcript error = %v; want deleted", err)
	}
	if _, _, err := lockingStore.GetTranscript(ctx, busy, busyTranscript.ID); err != nil {
		t.Fatalf("busy-lane transcript was touched: %v", err)
	}
}

func TestTranscriptRetentionWorkerRefusesIncompleteLaneSetPostgres(t *testing.T) {
	dsn := os.Getenv("WITSELF_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("WITSELF_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	st, _ := newMigrationTestStore(t, dsn)
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}
	principal, _ := provisionTranscriptRetentionWorkerPrincipal(
		ctx, t, st, "incomplete-lanes",
	)
	transcript := enableTranscriptRetentionAndCreateExpired(
		ctx, t, st, principal, "incomplete-lanes",
	)
	if _, err := st.pool.Exec(ctx, `
		DELETE FROM transcript_retention_worker_lanes
		 WHERE mode='enforce' AND lane_id=15`); err != nil {
		t.Fatal(err)
	}

	cfg := DefaultTranscriptRetentionWorkerConfig()
	cfg.BatchSize = 1
	cfg.Interval = time.Minute
	cfg.BatchTimeout = 5 * time.Second
	cfg.Mode = TranscriptRetentionModeEnforce
	if _, err := st.processTranscriptRetentionWorkerBatch(ctx, cfg); err == nil ||
		!strings.Contains(err.Error(), "worker lanes are missing") {
		t.Fatalf("incomplete worker lanes error = %v", err)
	}
	if _, _, err := st.GetTranscript(ctx, principal, transcript.ID); err != nil {
		t.Fatalf("incomplete lane set touched transcript: %v", err)
	}
}

func provisionTranscriptRetentionWorkerPrincipal(
	ctx context.Context,
	t *testing.T,
	st *Store,
	label string,
) (Principal, int) {
	t.Helper()
	account, err := st.ProvisionAccount(
		ctx, label+"@witwave.ai", label, time.Hour,
	)
	if err != nil {
		t.Fatal(err)
	}
	if activated, err := st.ActivateAccount(ctx, account.AccountID); err != nil || !activated {
		t.Fatalf("activate %s = %v / %v", label, activated, err)
	}
	realm, err := st.CreateRealm(ctx, account.AccountID, "default")
	if err != nil {
		t.Fatal(err)
	}
	agent, err := st.CreateAgent(ctx, account.AccountID, realm.ID, "recorder")
	if err != nil {
		t.Fatal(err)
	}
	var lane int
	if err := st.pool.QueryRow(ctx, `
		SELECT get_byte(decode(md5(id), 'hex'), 0) % 16
		  FROM accounts
		 WHERE id=$1`, account.AccountID).Scan(&lane); err != nil {
		t.Fatal(err)
	}
	return Principal{
		Kind: PrincipalAgent, ID: agent.ID, AccountID: account.AccountID,
		RealmID: realm.ID, AccountStatus: "active",
	}, lane
}

func enableTranscriptRetentionAndCreateExpired(
	ctx context.Context,
	t *testing.T,
	st *Store,
	p Principal,
	externalID string,
) Transcript {
	t.Helper()
	if _, err := st.SetAccountPlan(ctx, p.AccountID, 0, "", "free",
		map[string]int64{},
		map[string]int64{TranscriptRetentionDaysPolicy: 30}, nil); err != nil {
		t.Fatal(err)
	}
	transcript, err := st.CreateTranscript(ctx, p.AccountID, p.RealmID, p.ID,
		CreateTranscriptInput{ExternalID: externalID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.AppendTranscriptEntry(
		ctx, p.AccountID, p.RealmID, p.ID, transcript.ID,
		AppendTranscriptEntryInput{
			Role: TranscriptRoleUser, Body: externalID,
		},
	); err != nil {
		t.Fatal(err)
	}
	if _, err := st.pool.Exec(ctx, `
		UPDATE transcript_conversations
		   SET updated_at=statement_timestamp() - interval '31 days'
		 WHERE id=$1`, transcript.ID); err != nil {
		t.Fatal(err)
	}
	return transcript
}

func TestTranscriptRetentionPrunedRunCannotReplayPostgres(t *testing.T) {
	dsn := os.Getenv("WITSELF_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("WITSELF_TEST_DATABASE_URL is not set")
	}
	ctx, st, p := newMemoryCurationHardeningStore(t, dsn)
	source := captureHardeningMemory(ctx, t, st, p,
		"source retained after transcript pruning", "retention-replay-source")
	transcript, err := st.CreateTranscript(ctx, p.AccountID, p.RealmID, p.ID,
		CreateTranscriptInput{ExternalID: "retention-replay-transcript"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.AppendTranscriptEntry(ctx, p.AccountID, p.RealmID, p.ID,
		transcript.ID, AppendTranscriptEntryInput{
			Role: TranscriptRoleUser, Body: "source that will age out",
		}); err != nil {
		t.Fatal(err)
	}
	requested, err := st.RequestCuration(ctx, p, RequestMemoryCurationInput{
		CoalescingKey: "retention_replay", TriggerReason: "manual_refine",
		IdempotencyKey: "retention-replay-request",
	})
	if err != nil {
		t.Fatal(err)
	}
	started, err := st.StartCuration(ctx, p, StartMemoryCurationInput{
		RequestID: requested.Request.ID, LeaseDuration: time.Minute,
		IdempotencyKey: "retention-replay-start",
	})
	if err != nil {
		t.Fatal(err)
	}
	if started.Run.TranscriptInputCount == 0 || started.Run.CursorInputCount == 0 {
		t.Fatalf("original run did not freeze transcript source: %#v", started.Run)
	}
	actions := hardeningCreateActions(source.Memory.ID, source.Memory.Version,
		"retention_replay_output", "curated output before transcript pruning")
	planned := planHardeningCuration(ctx, t, st, p, started, actions,
		"retention-replay-plan")
	applied, err := st.ApplyCuration(ctx, p, started.Run.ID, ApplyMemoryCurationInput{
		FencingGeneration: started.Run.FencingGeneration,
		PlanRevision:      planned.Plan.PlanRevision,
		PlanHash:          planned.Receipt.PlanHash,
		IdempotencyKey:    "retention-replay-apply",
	})
	if err != nil {
		t.Fatal(err)
	}
	produced, _ := memoryCurationProducedHeads(applied.Receipt.ActionResults)
	if _, err := st.SetAccountPlan(ctx, p.AccountID, 0, "", "free",
		map[string]int64{},
		map[string]int64{TranscriptRetentionDaysPolicy: 30}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := st.pool.Exec(ctx, `
		UPDATE transcript_conversations
		   SET updated_at=statement_timestamp() - interval '31 days'
		 WHERE id=$1`, transcript.ID); err != nil {
		t.Fatal(err)
	}
	result, err := st.ProcessTranscriptRetentionBatch(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if result.Deleted != 1 || result.ReleasedCurationInputs == 0 {
		t.Fatalf("prune-before-replay result = %+v", result)
	}
	if _, _, err := st.GetTranscript(ctx, p, transcript.ID); !errors.Is(err, ErrTranscriptNotFound) {
		t.Fatalf("pruned source transcript error = %v; want deleted", err)
	}
	// Applied runs remain rollback-capable after their transcript identities are
	// pruned; compensation uses action provenance, not the deleted source.
	rolledBack, err := st.RollbackCuration(ctx, p, started.Run.ID,
		RollbackMemoryCurationInput{
			ApplyReceiptID:        applied.Receipt.ID,
			ExpectedProducedHeads: produced,
			Reason:                "verify retention replay refusal",
			IdempotencyKey:        "retention-replay-rollback",
		})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.StartCuration(ctx, p, StartMemoryCurationInput{
		RequestID: rolledBack.ReplayRequest.ID, LeaseDuration: time.Minute,
		IdempotencyKey: "retention-replay-refused-start",
	}); !errors.Is(err, ErrMemoryCurationConflict) ||
		!strings.Contains(err.Error(), "pruned by transcript retention") {
		t.Fatalf("replay after transcript pruning error = %v", err)
	}
}
