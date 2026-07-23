package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
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
					Mode: TranscriptRetentionModeEnforce,
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
	var generationAfterFirst int64
	var nextRunAfterFirst time.Time
	if err := st.pool.QueryRow(ctx, `
		SELECT generation,next_run_at
		  FROM transcript_retention_sweep_state
		 WHERE singleton`).Scan(&generationAfterFirst, &nextRunAfterFirst); err != nil {
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
		SELECT generation FROM transcript_retention_sweep_state WHERE singleton`,
	).Scan(&generationAfterSecond); err != nil {
		t.Fatal(err)
	}
	if generationAfterSecond != generationAfterFirst {
		t.Fatalf("gated worker advanced generation from %d to %d",
			generationAfterFirst, generationAfterSecond)
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
		SELECT next_run_at FROM transcript_retention_sweep_state WHERE singleton`,
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
