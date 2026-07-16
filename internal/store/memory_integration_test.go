package store

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"
)

func TestMemoryPostgresRoundTrip(t *testing.T) {
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
		"memory-test@witwave.ai", "memory test", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = deleteFactTestAccount(ctx, st, provisioned.AccountID) }()
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
	otherAgent, err := st.CreateAgent(ctx, provisioned.AccountID, realm.ID, "other")
	if err != nil {
		t.Fatal(err)
	}
	p := Principal{
		Kind: PrincipalAgent, ID: agent.ID, AccountID: provisioned.AccountID,
		RealmID: realm.ID, AccountStatus: "active",
	}

	occurredFrom := time.Date(2026, 7, 14, 1, 0, 0, 0, time.UTC)
	occurredUntil := occurredFrom.Add(10 * time.Minute)
	salience := 0.8
	capture := CaptureMemoryInput{
		Content: "We chose PostgreSQL as the sole authoritative memory store.",
		Kind:    "decision", Tags: []string{"architecture", "database"},
		Links: []string{"witself://fact/project-database"}, Salience: &salience,
		Sensitive: true, OccurredFrom: &occurredFrom,
		OccurredUntil: &occurredUntil, CaptureReason: "explicit_remember",
		Evidence: []MemoryEvidenceInput{{
			ResolutionState: MemoryEvidencePending,
			ExternalLocator: "codex://thread/thread-1/turn/9",
		}},
		Client:         MemoryClientProvenance{Runtime: "codex", Model: "gpt-5"},
		IdempotencyKey: "capture-database-decision-1",
	}
	created, err := st.CaptureMemory(ctx, p, capture)
	if err != nil {
		t.Fatal(err)
	}
	if created.Memory.Version != 1 || created.Memory.State != MemoryStateActive ||
		len(created.Memory.Evidence) != 1 ||
		created.Memory.Evidence[0].ResolutionState != MemoryEvidencePending ||
		created.Memory.Evidence[0].Type != "conversation" {
		t.Fatalf("created memory = %#v", created)
	}
	pendingID := created.Memory.Evidence[0].ID

	replayed, err := st.CaptureMemory(ctx, p, capture)
	if err != nil {
		t.Fatal(err)
	}
	if created.Receipt.Replayed || !replayed.Receipt.Replayed ||
		replayed.Memory.ID != created.Memory.ID || replayed.Memory.Version != 1 ||
		replayed.Receipt.RequestHash != created.Receipt.RequestHash {
		t.Fatalf("capture replay = %#v", replayed)
	}
	changedCapture := capture
	changedCapture.Content = "different"
	if _, err := st.CaptureMemory(ctx, p, changedCapture); !errors.Is(err, ErrMemoryIdempotencyConflict) {
		t.Fatalf("changed capture retry error = %v", err)
	}

	got, err := st.GetMemory(ctx, p, created.Memory.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Content != capture.Content || !got.Sensitive || len(got.Evidence) != 1 {
		t.Fatalf("exact memory = %#v", got)
	}
	other := p
	other.ID = otherAgent.ID
	if _, err := st.GetMemory(ctx, other, created.Memory.ID); !errors.Is(err, ErrMemoryNotFound) {
		t.Fatalf("cross-owner read error = %v", err)
	}

	listed, err := st.ListMemories(ctx, p, MemoryListOptions{
		State: MemoryStateActive, Tags: []string{"database"},
		OccurredFrom: &occurredFrom,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(listed.Memories) != 1 || !listed.Memories[0].Redacted ||
		listed.Memories[0].Content != "" || listed.Memories[0].ContentHash != "" ||
		len(listed.Memories[0].Tags) != 0 || len(listed.Memories[0].Links) != 0 {
		t.Fatalf("redacted inventory = %#v", listed.Memories)
	}
	count, err := st.CountMemories(ctx, p, MemoryListOptions{State: MemoryStateActive})
	if err != nil || count != 1 {
		t.Fatalf("active count = %d / %v", count, err)
	}
	excluded, err := st.ListMemories(ctx, p, MemoryListOptions{
		State: MemoryStateActive, ExcludeSensitive: true,
	})
	if err != nil || len(excluded.Memories) != 0 {
		t.Fatalf("sensitive-excluded inventory = %#v / %v", excluded.Memories, err)
	}
	excludedCount, err := st.CountMemories(ctx, p, MemoryListOptions{
		State: MemoryStateActive, ExcludeSensitive: true,
	})
	if err != nil || excludedCount != 0 {
		t.Fatalf("sensitive-excluded count = %d / %v", excludedCount, err)
	}
	if _, err := st.ListMemories(ctx, p, MemoryListOptions{
		IncludeSensitive: true, ExcludeSensitive: true,
	}); !errors.Is(err, ErrMemoryInputInvalid) {
		t.Fatalf("contradictory sensitive list options error = %v", err)
	}

	adjustedContent := "PostgreSQL is the sole live authority; indexes are derived."
	adjusted, err := st.AdjustMemory(ctx, p, created.Memory.ID, AdjustMemoryInput{
		ExpectedVersion: 1, Content: &adjustedContent,
		AddTags: []string{"portability"}, RemoveTags: []string{"database"},
		Reason: "record the portability boundary", IdempotencyKey: "adjust-decision-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if adjusted.Memory.Version != 2 || adjusted.Memory.PreviousVersion != 1 ||
		adjusted.Memory.Content != adjustedContent {
		t.Fatalf("adjusted memory = %#v", adjusted)
	}
	if _, err := st.AdjustMemory(ctx, p, created.Memory.ID, AdjustMemoryInput{
		ExpectedVersion: 1, Content: &adjustedContent,
		IdempotencyKey: "stale-adjust-1",
	}); !errors.Is(err, ErrMemoryConflict) {
		t.Fatalf("stale adjustment error = %v", err)
	}

	history, err := st.GetMemoryHistoryPage(ctx, p, created.Memory.ID, MemoryHistoryOptions{Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(history.Versions) != 1 || history.Versions[0].Version != 1 ||
		len(history.Versions[0].Evidence) != 1 || history.NextCursor == "" {
		t.Fatalf("first memory history page = %#v", history)
	}
	history, err = st.GetMemoryHistoryPage(ctx, p, created.Memory.ID, MemoryHistoryOptions{
		Limit: 1, Cursor: history.NextCursor,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(history.Versions) != 1 || history.Versions[0].Version != 2 ||
		len(history.Versions[0].Evidence) != 0 || history.NextCursor != "" {
		t.Fatalf("second memory history page = %#v", history)
	}

	forgotten, err := st.ForgetMemory(ctx, p, created.Memory.ID, MemoryLifecycleInput{
		ExpectedVersion: 2, Reason: "temporarily archive",
		IdempotencyKey: "forget-decision-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if forgotten.Memory.Version != 3 || forgotten.Memory.State != MemoryStateForgotten ||
		forgotten.Memory.PriorState != MemoryStateActive {
		t.Fatalf("forgotten memory = %#v", forgotten)
	}
	active, err := st.ListMemories(ctx, p, MemoryListOptions{})
	if err != nil || len(active.Memories) != 0 {
		t.Fatalf("active list after forget = %#v / %v", active, err)
	}
	forgottenCount, err := st.CountMemories(ctx, p,
		MemoryListOptions{State: MemoryStateForgotten})
	if err != nil || forgottenCount != 1 {
		t.Fatalf("forgotten count = %d / %v", forgottenCount, err)
	}
	restored, err := st.RestoreMemory(ctx, p, created.Memory.ID, MemoryLifecycleInput{
		ExpectedVersion: 3, Reason: "needed again",
		IdempotencyKey: "restore-decision-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if restored.Memory.Version != 4 || restored.Memory.State != MemoryStateActive ||
		restored.Memory.PriorState != "" {
		t.Fatalf("restored memory = %#v", restored)
	}
	if _, err := st.ReactivateMemory(ctx, p, created.Memory.ID, MemoryLifecycleInput{
		ExpectedVersion: 4, IdempotencyKey: "invalid-reactivate-1",
	}); !errors.Is(err, ErrMemoryConflict) {
		t.Fatalf("active reactivate error = %v", err)
	}

	transcript, err := st.CreateTranscript(ctx, p.AccountID, p.RealmID, p.ID,
		CreateTranscriptInput{ExternalID: "memory-test-thread"})
	if err != nil {
		t.Fatal(err)
	}
	entry, err := st.AppendTranscriptEntry(ctx, p.AccountID, p.RealmID, p.ID,
		transcript.ID, AppendTranscriptEntryInput{
			ExternalID: "turn-9", Role: TranscriptRoleUser,
			Body: "Remember the database decision.",
		})
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := st.ResolveMemoryEvidence(ctx, p, pendingID,
		ResolveMemoryEvidenceInput{
			ResolvedKind: "transcript", SourceTranscriptID: transcript.ID,
			SourceSequenceFrom: entry.Sequence, SourceSequenceUntil: entry.Sequence,
			IdempotencyKey: "resolve-evidence-1",
		})
	if err != nil {
		t.Fatal(err)
	}
	if resolved.ResolutionState != MemoryEvidenceResolved ||
		resolved.PendingEvidenceID != pendingID ||
		resolved.SourceTranscriptID != transcript.ID {
		t.Fatalf("resolved evidence = %#v", resolved)
	}
	resolvedReplay, err := st.ResolveMemoryEvidence(ctx, p, pendingID,
		ResolveMemoryEvidenceInput{
			ResolvedKind: "transcript", SourceTranscriptID: transcript.ID,
			SourceSequenceFrom: entry.Sequence, SourceSequenceUntil: entry.Sequence,
			IdempotencyKey: "resolve-evidence-1",
		})
	if err != nil || resolvedReplay.ID != resolved.ID {
		t.Fatalf("evidence replay = %#v / %v", resolvedReplay, err)
	}
	if _, err := st.ResolveMemoryEvidence(ctx, p, pendingID,
		ResolveMemoryEvidenceInput{
			UnresolvableReason: "not_found",
			IdempotencyKey:     "resolve-evidence-2",
		}); !errors.Is(err, ErrMemoryEvidenceConflict) {
		t.Fatalf("second evidence resolution error = %v", err)
	}

	secondSalience := 0.95
	second, err := st.CaptureMemory(ctx, p, CaptureMemoryInput{
		Content: "A later high-salience checkpoint.", Kind: "checkpoint",
		Salience: &secondSalience, CaptureReason: "checkpoint",
		Evidence: []MemoryEvidenceInput{{
			ResolutionState:    MemoryEvidenceUnavailable,
			TerminalReasonCode: "runtime_did_not_record",
		}},
		IdempotencyKey: "capture-checkpoint-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	page1, err := st.ListMemories(ctx, p, MemoryListOptions{
		IncludeSensitive: true, OrderBySalience: true, Limit: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(page1.Memories) != 1 || page1.Memories[0].ID != second.Memory.ID ||
		page1.NextCursor == "" {
		t.Fatalf("first salient page = %#v", page1)
	}
	page2, err := st.ListMemories(ctx, p, MemoryListOptions{
		IncludeSensitive: true, OrderBySalience: true, Limit: 1,
		Cursor: page1.NextCursor,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(page2.Memories) != 1 || page2.Memories[0].ID != created.Memory.ID {
		t.Fatalf("second salient page = %#v", page2)
	}

	// The deferrable head pointer rejects a dangling version at commit.
	if _, err := st.pool.Exec(ctx, `UPDATE memories SET current_version=9999 WHERE id=$1`,
		created.Memory.ID); err == nil {
		t.Fatal("dangling current_version committed")
	}
	current, err := st.GetMemory(ctx, p, created.Memory.ID)
	if err != nil || current.Version != 4 {
		t.Fatalf("head after rejected corruption = %#v / %v", current, err)
	}

	if _, err := st.pool.Exec(ctx, `
		UPDATE memory_change_clocks SET last_change_seq=$1
		WHERE account_id=$2 AND realm_id=$3 AND owner_kind='agent' AND owner_id=$4`,
		maxMemoryChangeSeq-1, p.AccountID, p.RealmID, p.ID); err != nil {
		t.Fatal(err)
	}
	tx, err := st.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	seq, err := allocateMemoryChangeSeq(ctx, tx, p)
	if err != nil || seq != maxMemoryChangeSeq {
		_ = tx.Rollback(ctx)
		t.Fatalf("last allocatable memory change sequence = %d / %v", seq, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	exhaustedTx, err := st.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := allocateMemoryChangeSeq(ctx, exhaustedTx, p); !errors.Is(err, ErrMemoryChangeSeqExhausted) {
		_ = exhaustedTx.Rollback(ctx)
		t.Fatalf("exhausted memory change sequence error = %v", err)
	}
	_ = exhaustedTx.Rollback(ctx)
	if _, err := st.pool.Exec(ctx, `
		UPDATE memory_change_clocks SET last_change_seq=$1
		WHERE account_id=$2 AND realm_id=$3 AND owner_kind='agent' AND owner_id=$4`,
		maxMemoryChangeSeq+1, p.AccountID, p.RealmID, p.ID); err == nil {
		t.Fatal("memory change clock accepted a value beyond its schema ceiling")
	}
}
