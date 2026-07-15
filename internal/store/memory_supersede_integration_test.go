package store

import (
	"bytes"
	"context"
	"errors"
	"os"
	"sort"
	"testing"
	"time"
)

func TestMemorySupersessionPostgres(t *testing.T) {
	dsn := os.Getenv("WITSELF_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("WITSELF_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	st, _ := newMigrationTestStore(t, dsn)
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}

	provisioned, err := st.ProvisionAccount(ctx,
		"memory-supersede@witwave.ai", "memory supersede", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = deleteMemorySupersessionTestAccount(ctx, st, provisioned.AccountID) }()
	if activated, err := st.ActivateAccount(ctx, provisioned.AccountID); err != nil || !activated {
		t.Fatalf("activate = %v / %v", activated, err)
	}
	realm, err := st.CreateRealm(ctx, provisioned.AccountID, "default")
	if err != nil {
		t.Fatal(err)
	}
	agent, err := st.CreateAgent(ctx, provisioned.AccountID, realm.ID, "primary")
	if err != nil {
		t.Fatal(err)
	}
	p := Principal{
		Kind: PrincipalAgent, ID: agent.ID, AccountID: provisioned.AccountID,
		RealmID: realm.ID, AccountStatus: "active",
	}

	source, err := st.CaptureMemory(ctx, p, CaptureMemoryInput{
		Content: "The original narrative combines two separate decisions.",
		Kind:    "decision", Tags: []string{"architecture"},
		Evidence: []MemoryEvidenceInput{{
			ResolutionState:    MemoryEvidenceUnavailable,
			TerminalReasonCode: "runtime_did_not_record",
		}},
		IdempotencyKey: "supersede-source-capture",
	})
	if err != nil {
		t.Fatal(err)
	}
	// Seed a distinct mutation key and prove a replacement-key collision rolls
	// back the source version and every partially created row.
	if _, err := st.CaptureMemory(ctx, p, CaptureMemoryInput{
		Content: "An unrelated retained memory.",
		Evidence: []MemoryEvidenceInput{{
			ResolutionState:    MemoryEvidenceUnavailable,
			TerminalReasonCode: "runtime_did_not_record",
		}},
		IdempotencyKey: "already-used-replacement-key",
	}); err != nil {
		t.Fatal(err)
	}
	failed := SupersedeMemoryInput{
		MemoryID: source.Memory.ID, ExpectedVersion: source.Memory.Version,
		Replacements: []CaptureMemoryInput{{
			Content: "This insertion must roll back.",
			Evidence: []MemoryEvidenceInput{{
				ResolutionState:    MemoryEvidenceUnavailable,
				TerminalReasonCode: "runtime_did_not_record",
			}},
			IdempotencyKey: "already-used-replacement-key",
		}},
		IdempotencyKey: "supersede-atomic-failure",
	}
	if _, err := st.SupersedeMemory(ctx, p, failed); !errors.Is(err, ErrMemoryIdempotencyConflict) {
		t.Fatalf("replacement-key collision = %v, want ErrMemoryIdempotencyConflict", err)
	}
	afterFailure, err := st.GetMemory(ctx, p, source.Memory.ID)
	if err != nil {
		t.Fatal(err)
	}
	if afterFailure.Version != 1 || afterFailure.State != MemoryStateActive {
		t.Fatalf("source after rolled-back supersession = %#v", afterFailure)
	}
	var failedRelations int64
	if err := st.pool.QueryRow(ctx, `
		SELECT count(*) FROM memory_relations
		WHERE to_memory_id=$1`, source.Memory.ID).Scan(&failedRelations); err != nil {
		t.Fatal(err)
	}
	if failedRelations != 0 {
		t.Fatalf("relations after rolled-back supersession = %d", failedRelations)
	}

	salience := 0.8
	in := SupersedeMemoryInput{
		MemoryID: source.Memory.ID, ExpectedVersion: source.Memory.Version,
		Reason: "split the combined narrative",
		Client: MemoryClientProvenance{Runtime: "codex", Model: "gpt-5"},
		Replacements: []CaptureMemoryInput{
			{
				Content: "The first architectural decision stands independently.",
				Kind:    "decision", Tags: []string{"architecture", "first"},
				Salience: &salience,
				Evidence: []MemoryEvidenceInput{{
					ResolutionState:     MemoryEvidenceResolved,
					ResolvedKind:        "memory",
					SourceMemoryID:      source.Memory.ID,
					SourceMemoryVersion: source.Memory.Version,
				}},
				Client:         MemoryClientProvenance{Runtime: "codex", Recipe: "split"},
				IdempotencyKey: "supersede-replacement-first",
			},
			{
				Content: "The second architectural decision stands independently.",
				Kind:    "decision", Tags: []string{"architecture", "second"},
				Evidence: []MemoryEvidenceInput{{
					ResolutionState:    MemoryEvidenceUnavailable,
					TerminalReasonCode: "runtime_did_not_record",
				}},
				Client:         MemoryClientProvenance{Runtime: "claude", Recipe: "split"},
				IdempotencyKey: "supersede-replacement-second",
			},
		},
		IdempotencyKey: "supersede-source-split",
	}
	type supersedeCallResult struct {
		result SupersedeMemoryResult
		err    error
	}
	start := make(chan struct{})
	completed := make(chan supersedeCallResult, 2)
	for range 2 {
		go func() {
			<-start
			result, err := st.SupersedeMemory(ctx, p, in)
			completed <- supersedeCallResult{result: result, err: err}
		}()
	}
	close(start)
	var result, concurrentReplay SupersedeMemoryResult
	for range 2 {
		call := <-completed
		if call.err != nil {
			t.Fatal(call.err)
		}
		if call.result.Receipt.Replayed {
			concurrentReplay = call.result
		} else {
			result = call.result
		}
	}
	if result.Receipt.SupersessionSetID == "" ||
		concurrentReplay.Receipt.SupersessionSetID != result.Receipt.SupersessionSetID ||
		!concurrentReplay.Receipt.Replayed {
		t.Fatalf("concurrent exact retry = result %#v / replay %#v", result, concurrentReplay)
	}
	if result.Source.ID != source.Memory.ID || result.Source.Version != 2 ||
		result.Source.PreviousVersion != 1 || result.Source.State != MemoryStateSuperseded ||
		result.Source.Operation != "superseded" || len(result.Replacements) != 2 {
		t.Fatalf("supersession result = %#v", result)
	}
	if result.Receipt.SupersessionSetID == "" ||
		result.Receipt.SupersessionSetRevision != 1 || result.Receipt.Replayed ||
		result.Receipt.Source.MemoryID != source.Memory.ID ||
		result.Receipt.Source.Version != 2 || len(result.Receipt.Replacements) != 2 {
		t.Fatalf("supersession receipt = %#v", result.Receipt)
	}
	if result.Source.SupersessionSetID != result.Receipt.SupersessionSetID ||
		result.Source.SupersessionSetRevision != result.Receipt.SupersessionSetRevision ||
		result.Source.SupersessionReplacementCount != result.Receipt.ReplacementCount ||
		result.Source.SupersessionReplacementDigest != result.Receipt.ReplacementDigest ||
		result.Source.ActiveSupersessionSetID != result.Receipt.SupersessionSetID ||
		result.Source.ActiveSupersessionSetRevision != result.Receipt.SupersessionSetRevision {
		t.Fatalf("supersession source metadata = %#v / receipt %#v", result.Source, result.Receipt)
	}
	contents := []string{result.Replacements[0].Content, result.Replacements[1].Content}
	sort.Strings(contents)
	if contents[0] != "The first architectural decision stands independently." ||
		contents[1] != "The second architectural decision stands independently." {
		t.Fatalf("replacement contents = %#v", contents)
	}
	for _, replacement := range result.Replacements {
		if replacement.Version != 1 || replacement.State != MemoryStateActive ||
			replacement.Operation != "added" || replacement.RequestHash != result.Receipt.RequestHash ||
			len(replacement.Evidence) != 1 {
			t.Fatalf("replacement = %#v", replacement)
		}
	}

	var relationCount, distinctSets, minRevision, maxRevision int64
	var relationSourceVersion, minReplacementVersion, maxReplacementVersion int64
	if err := st.pool.QueryRow(ctx, `
		SELECT count(*), count(DISTINCT supersession_set_id),
		       min(supersession_set_revision), max(supersession_set_revision),
		       min(to_version), min(from_version), max(from_version)
		FROM memory_relations
		WHERE to_memory_id=$1 AND relation_type='supersedes' AND reverted_at IS NULL`,
		source.Memory.ID).Scan(&relationCount, &distinctSets, &minRevision,
		&maxRevision, &relationSourceVersion, &minReplacementVersion, &maxReplacementVersion); err != nil {
		t.Fatal(err)
	}
	if relationCount != 2 || distinctSets != 1 || minRevision != 1 || maxRevision != 1 ||
		relationSourceVersion != 2 || minReplacementVersion != 1 || maxReplacementVersion != 1 {
		t.Fatalf("relation shape = count %d sets %d revisions %d/%d source %d replacements %d/%d",
			relationCount, distinctSets, minRevision, maxRevision,
			relationSourceVersion, minReplacementVersion, maxReplacementVersion)
	}

	history, err := st.GetMemoryHistoryPage(ctx, p, source.Memory.ID, MemoryHistoryOptions{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(history.Versions) != 2 || history.Versions[0].State != MemoryStateActive ||
		history.Versions[1].State != MemoryStateSuperseded {
		t.Fatalf("source history = %#v", history.Versions)
	}
	if supersededVersion := history.Versions[1]; supersededVersion.SupersessionSetID != result.Receipt.SupersessionSetID ||
		supersededVersion.SupersessionSetRevision != result.Receipt.SupersessionSetRevision ||
		supersededVersion.SupersessionReplacementCount != result.Receipt.ReplacementCount ||
		supersededVersion.SupersessionReplacementDigest != result.Receipt.ReplacementDigest ||
		supersededVersion.ActiveSupersessionSetID != result.Receipt.SupersessionSetID ||
		supersededVersion.ActiveSupersessionSetRevision != result.Receipt.SupersessionSetRevision {
		t.Fatalf("superseded history metadata = %#v", supersededVersion)
	}

	replay, err := st.SupersedeMemory(ctx, p, in)
	if err != nil {
		t.Fatal(err)
	}
	if !replay.Receipt.Replayed ||
		replay.Receipt.SupersessionSetID != result.Receipt.SupersessionSetID ||
		replay.Receipt.RequestHash != result.Receipt.RequestHash ||
		replay.Source.ID != result.Source.ID || len(replay.Replacements) != 2 ||
		replay.Replacements[0].ID != result.Replacements[0].ID ||
		replay.Replacements[1].ID != result.Replacements[1].ID {
		t.Fatalf("supersession replay = %#v", replay)
	}
	var supersededAuditCount, replacementAuditCount int64
	if err := st.pool.QueryRow(ctx, `
		SELECT count(*) FROM account_events
		WHERE account_id=$1 AND actor_kind='agent' AND actor_id=$2
		  AND verb='memory.superseded'
		  AND metadata->>'memory_id'=$3 AND metadata->>'version'='2'`,
		p.AccountID, p.ID, source.Memory.ID).Scan(&supersededAuditCount); err != nil {
		t.Fatal(err)
	}
	replacementIDs := []string{result.Replacements[0].ID, result.Replacements[1].ID}
	if err := st.pool.QueryRow(ctx, `
		SELECT count(*) FROM account_events
		WHERE account_id=$1 AND actor_kind='agent' AND actor_id=$2
		  AND verb='memory.added' AND metadata->>'memory_id'=ANY($3)`,
		p.AccountID, p.ID, replacementIDs).Scan(&replacementAuditCount); err != nil {
		t.Fatal(err)
	}
	if supersededAuditCount != 1 || replacementAuditCount != 2 {
		t.Fatalf("supersession audit counts = source %d replacements %d",
			supersededAuditCount, replacementAuditCount)
	}
	if err := st.SuspendAccountSystem(ctx, p.AccountID, "evacuation",
		"supersession archive round trip"); err != nil {
		t.Fatal(err)
	}
	var intactArchive bytes.Buffer
	if err := st.ExportAccount(ctx, p.AccountID, "test-source", "test", &intactArchive); err != nil {
		t.Fatal(err)
	}
	if err := deleteMemorySupersessionTestAccount(ctx, st, p.AccountID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ImportAccount(ctx, p.AccountID, bytes.NewReader(intactArchive.Bytes())); err != nil {
		t.Fatal(err)
	}
	if err := st.ResumeAccountSystem(ctx, p.AccountID, "evacuation"); err != nil {
		t.Fatal(err)
	}
	replayAfterArchive, err := st.SupersedeMemory(ctx, p, in)
	if err != nil {
		t.Fatal(err)
	}
	if !replayAfterArchive.Receipt.Replayed ||
		replayAfterArchive.Receipt.ReplacementCount != 2 ||
		replayAfterArchive.Receipt.ReplacementDigest != result.Receipt.ReplacementDigest ||
		replayAfterArchive.Receipt.SupersessionSetID != result.Receipt.SupersessionSetID {
		t.Fatalf("replay after intact archive = %#v", replayAfterArchive)
	}
	changed := in
	changed.Replacements = append([]CaptureMemoryInput(nil), in.Replacements...)
	changed.Replacements[0].Content = "A changed retry payload."
	if _, err := st.SupersedeMemory(ctx, p, changed); !errors.Is(err, ErrMemoryIdempotencyConflict) {
		t.Fatalf("changed supersession retry = %v", err)
	}
	stale := in
	stale.IdempotencyKey = "supersede-stale-source"
	stale.Replacements = append([]CaptureMemoryInput(nil), in.Replacements...)
	stale.Replacements[0].IdempotencyKey = "supersede-stale-replacement-first"
	stale.Replacements[1].IdempotencyKey = "supersede-stale-replacement-second"
	if _, err := st.SupersedeMemory(ctx, p, stale); !errors.Is(err, ErrMemoryConflict) {
		t.Fatalf("stale source supersession = %v", err)
	}

	forgotten, err := st.ForgetMemory(ctx, p, source.Memory.ID, MemoryLifecycleInput{
		ExpectedVersion: result.Source.Version,
		Reason:          "temporarily hide the original",
		IdempotencyKey:  "forget-superseded-source",
	})
	if err != nil {
		t.Fatal(err)
	}
	if forgotten.Memory.ActiveSupersessionSetID != result.Receipt.SupersessionSetID ||
		forgotten.Memory.ActiveSupersessionSetRevision != result.Receipt.SupersessionSetRevision {
		t.Fatalf("forgotten source active supersession = %#v", forgotten.Memory)
	}
	restored, err := st.RestoreMemory(ctx, p, source.Memory.ID, MemoryLifecycleInput{
		ExpectedVersion: forgotten.Memory.Version,
		Reason:          "restore the superseded original",
		IdempotencyKey:  "restore-superseded-source",
	})
	if err != nil {
		t.Fatal(err)
	}
	if restored.Memory.Version != 4 || restored.Memory.State != MemoryStateSuperseded ||
		restored.Memory.SupersessionSetID != "" || restored.Memory.SupersessionSetRevision != 0 ||
		restored.Memory.SupersessionReplacementCount != 0 || restored.Memory.SupersessionReplacementDigest != "" ||
		restored.Memory.ActiveSupersessionSetID != result.Receipt.SupersessionSetID ||
		restored.Memory.ActiveSupersessionSetRevision != result.Receipt.SupersessionSetRevision {
		t.Fatalf("restored superseded source = %#v", restored)
	}
	replayAfterRestore, err := st.SupersedeMemory(ctx, p, in)
	if err != nil {
		t.Fatal(err)
	}
	if !replayAfterRestore.Receipt.Replayed ||
		replayAfterRestore.Source.Version != result.Source.Version ||
		replayAfterRestore.Receipt.SupersessionSetID != result.Receipt.SupersessionSetID {
		t.Fatalf("replay after restore = %#v", replayAfterRestore)
	}

	revision := result.Receipt.SupersessionSetRevision
	reactivated, err := st.ReactivateMemory(ctx, p, source.Memory.ID, MemoryLifecycleInput{
		ExpectedVersion:                 restored.Memory.Version,
		ExpectedSupersessionSetRevision: &revision,
		Reason:                          "keep the original too",
		IdempotencyKey:                  "reactivate-superseded-source",
	})
	if err != nil {
		t.Fatal(err)
	}
	if reactivated.Memory.Version != 5 || reactivated.Memory.State != MemoryStateActive ||
		reactivated.Memory.ActiveSupersessionSetID != "" ||
		reactivated.Memory.ActiveSupersessionSetRevision != 0 {
		t.Fatalf("reactivated source = %#v", reactivated)
	}
	var liveRelations int64
	if err := st.pool.QueryRow(ctx, `
		SELECT count(*) FROM memory_relations
		WHERE supersession_set_id=$1 AND supersession_set_revision=$2
		  AND reverted_at IS NULL`, result.Receipt.SupersessionSetID, revision).
		Scan(&liveRelations); err != nil {
		t.Fatal(err)
	}
	if liveRelations != 0 {
		t.Fatalf("live relations after reactivation = %d", liveRelations)
	}
	historyAfterReactivate, err := st.GetMemoryHistoryPage(ctx, p, source.Memory.ID, MemoryHistoryOptions{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(historyAfterReactivate.Versions) != 5 {
		t.Fatalf("history after reactivation = %#v", historyAfterReactivate.Versions)
	}
	var originalSupersededVersion MemoryVersion
	for _, version := range historyAfterReactivate.Versions {
		if version.ActiveSupersessionSetID != "" || version.ActiveSupersessionSetRevision != 0 {
			t.Fatalf("history retained reverted active supersession = %#v", version)
		}
		if version.Version == result.Source.Version {
			originalSupersededVersion = version
		}
	}
	if originalSupersededVersion.SupersessionSetID != result.Receipt.SupersessionSetID ||
		originalSupersededVersion.SupersessionSetRevision != result.Receipt.SupersessionSetRevision ||
		originalSupersededVersion.SupersessionReplacementCount != result.Receipt.ReplacementCount ||
		originalSupersededVersion.SupersessionReplacementDigest != result.Receipt.ReplacementDigest {
		t.Fatalf("immutable supersession receipt after reactivation = %#v", originalSupersededVersion)
	}
	replayAfterReactivate, err := st.SupersedeMemory(ctx, p, in)
	if err != nil {
		t.Fatal(err)
	}
	if !replayAfterReactivate.Receipt.Replayed ||
		replayAfterReactivate.Receipt.SupersessionSetID != result.Receipt.SupersessionSetID {
		t.Fatalf("replay after reactivation = %#v", replayAfterReactivate)
	}

	deletedReplacement := result.Replacements[0]
	deletePreview, err := st.DeleteMemory(ctx, p, DeleteMemoryInput{MemoryID: deletedReplacement.ID})
	if err != nil {
		t.Fatal(err)
	}
	if deletePreview.Blocked {
		t.Fatalf("reverted replacement deletion unexpectedly blocked = %#v", deletePreview)
	}
	if _, err := applyMemoryDeletePreview(ctx, st, p, deletePreview,
		"delete-reverted-supersession-replacement"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.SupersedeMemory(ctx, p, in); !errors.Is(err, ErrMemoryDeleted) {
		t.Fatalf("replay after replacement deletion = %v, want ErrMemoryDeleted", err)
	}

	if err := st.SuspendAccountSystem(ctx, p.AccountID, "evacuation",
		"partial supersession archive round trip"); err != nil {
		t.Fatal(err)
	}
	var partialArchive bytes.Buffer
	if err := st.ExportAccount(ctx, p.AccountID, "test-source", "test", &partialArchive); err != nil {
		t.Fatal(err)
	}
	if err := deleteMemorySupersessionTestAccount(ctx, st, p.AccountID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ImportAccount(ctx, p.AccountID, bytes.NewReader(partialArchive.Bytes())); err != nil {
		t.Fatal(err)
	}
	if err := st.ResumeAccountSystem(ctx, p.AccountID, "evacuation"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.SupersedeMemory(ctx, p, in); !errors.Is(err, ErrMemoryDeleted) {
		t.Fatalf("replay after partial archive = %v, want ErrMemoryDeleted", err)
	}
}

func deleteMemorySupersessionTestAccount(ctx context.Context, st *Store, accountID string) error {
	tx, err := st.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	for _, statement := range []string{
		`DELETE FROM memory_deleted_references WHERE account_id=$1`,
		`DELETE FROM memory_relations WHERE account_id=$1`,
		`DELETE FROM memory_evidence WHERE account_id=$1`,
		`DELETE FROM memory_versions WHERE account_id=$1`,
		`DELETE FROM memories WHERE account_id=$1`,
		`DELETE FROM memory_change_clocks WHERE account_id=$1`,
	} {
		if _, err := tx.Exec(ctx, statement, accountID); err != nil {
			return err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	return deleteFactTestAccount(ctx, st, accountID)
}
