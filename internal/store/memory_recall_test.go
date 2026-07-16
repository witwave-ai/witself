package store

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"
)

func TestMemoryRecallOptionsAndCursor(t *testing.T) {
	if _, err := normalizeMemoryRecallOptions(MemoryRecallOptions{}); !errors.Is(err, ErrMemoryInputInvalid) {
		t.Fatalf("empty recall error = %v", err)
	}
	if _, err := normalizeMemoryRecallOptions(MemoryRecallOptions{Query: "database", Limit: 101}); !errors.Is(err, ErrMemoryInputInvalid) {
		t.Fatalf("oversized recall error = %v", err)
	}
	now := time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)
	hash, err := memoryRequestHash(map[string]string{"query": "database"})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := encodeMemoryRecallCursor(memoryRecallCursor{
		Version: 2, QueryHash: hash, AsOf: now, SnapshotChangeSeq: 42,
		SnapshotDeletedMemoryCount: 3, AfterScore: 0.5,
		UpdatedAt: now.Add(-time.Minute), MemoryID: "mem_test",
	})
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := decodeMemoryRecallCursor(raw)
	if err != nil {
		t.Fatal(err)
	}
	if decoded.QueryHash != hash || decoded.MemoryID != "mem_test" ||
		decoded.SnapshotChangeSeq != 42 || decoded.SnapshotDeletedMemoryCount != 3 ||
		decoded.AfterScore != 0.5 {
		t.Fatalf("decoded cursor = %#v", decoded)
	}
	if _, err := decodeMemoryRecallCursor("not-base64"); !errors.Is(err, ErrMemoryInputInvalid) {
		t.Fatalf("bad cursor error = %v", err)
	}
	decoded.SnapshotChangeSeq = 0
	if _, err := encodeMemoryRecallCursor(decoded); !errors.Is(err, ErrMemoryInputInvalid) {
		t.Fatalf("missing-watermark cursor error = %v", err)
	}
}

func TestMemoryRecallPostgres(t *testing.T) {
	dsn := os.Getenv("WITSELF_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("WITSELF_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	st, _ := newMigrationTestStore(t, dsn)
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}
	provisioned, err := st.ProvisionAccount(ctx, "memory-recall@witwave.ai", "memory recall", time.Hour)
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
	p := Principal{Kind: PrincipalAgent, ID: agent.ID, AccountID: provisioned.AccountID, RealmID: realm.ID, AccountStatus: "active"}

	capture := func(key, content, kind string, salience float64, sensitive bool) Memory {
		t.Helper()
		result, err := st.CaptureMemory(ctx, p, CaptureMemoryInput{
			Content: content, Kind: kind, Tags: []string{"architecture"},
			Salience: &salience, Sensitive: sensitive, CaptureReason: "test",
			Evidence: []MemoryEvidenceInput{{
				ResolutionState:    MemoryEvidenceUnavailable,
				TerminalReasonCode: "test_fixture",
			}},
			IdempotencyKey: key,
		})
		if err != nil {
			t.Fatal(err)
		}
		return result.Memory
	}
	first := capture("recall-1", "PostgreSQL database decision for durable memory.", "decision", 0.4, false)
	second := capture("recall-2", "The durable memory database decision uses PostgreSQL.", "decision", 0.9, false)
	sensitive := capture("recall-3", "Private PostgreSQL database decision details.", "decision", 0.95, true)
	unrelated := capture("recall-4", "Unrelated Redis cache note.", "note", 1.0, false)

	page, err := st.RecallMemories(ctx, p, MemoryRecallOptions{Query: "PostgreSQL database", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if page.RetrievalMode != "lexical" || page.Degraded || page.VectorCoverage != 0 || len(page.Hits) != 3 {
		t.Fatalf("recall page = %#v", page)
	}
	seen := map[string]MemoryRecallHit{}
	for _, hit := range page.Hits {
		seen[hit.Memory.ID] = hit
		if hit.Score.Lexical <= 0 || hit.Score.Salience < 0 || hit.Score.Recency <= 0 || hit.Score.Total <= 0 {
			t.Fatalf("invalid recall score = %#v", hit.Score)
		}
	}
	if _, ok := seen[first.ID]; !ok {
		t.Fatalf("first memory absent: %#v", page.Hits)
	}
	if _, ok := seen[second.ID]; !ok {
		t.Fatalf("second memory absent: %#v", page.Hits)
	}
	if hit := seen[sensitive.ID]; !hit.Memory.Redacted || hit.Memory.Content != "" || hit.Memory.ContentHash != "" || len(hit.Memory.Tags) != 0 {
		t.Fatalf("sensitive recall was not redacted = %#v", hit)
	}
	excluded, err := st.RecallMemories(ctx, p, MemoryRecallOptions{
		Query: "PostgreSQL database", ExcludeSensitive: true, Limit: 10,
	})
	if err != nil || len(excluded.Hits) != 2 {
		t.Fatalf("sensitive-excluded recall = %#v / %v", excluded, err)
	}
	for _, hit := range excluded.Hits {
		if hit.Memory.Sensitive || hit.Memory.Redacted || hit.Memory.ID == sensitive.ID {
			t.Fatalf("sensitive memory survived excluded recall: %#v", hit)
		}
	}
	excludedFirst, err := st.RecallMemories(ctx, p, MemoryRecallOptions{
		Query: "PostgreSQL database", ExcludeSensitive: true, Limit: 1,
	})
	if err != nil || excludedFirst.NextCursor == "" {
		t.Fatalf("excluded recall first page = %#v / %v", excludedFirst, err)
	}
	if _, err := st.RecallMemories(ctx, p, MemoryRecallOptions{
		Query: "PostgreSQL database", Limit: 1, Cursor: excludedFirst.NextCursor,
	}); !errors.Is(err, ErrMemoryInputInvalid) {
		t.Fatalf("excluded recall cursor accepted changed sensitivity filter: %v", err)
	}
	if _, err := st.RecallMemories(ctx, p, MemoryRecallOptions{
		Query: "PostgreSQL database", IncludeSensitive: true, ExcludeSensitive: true,
	}); !errors.Is(err, ErrMemoryInputInvalid) {
		t.Fatalf("contradictory sensitive recall options error = %v", err)
	}

	firstPage, err := st.RecallMemories(ctx, p, MemoryRecallOptions{Query: "PostgreSQL database", Limit: 1})
	if err != nil || len(firstPage.Hits) != 1 || firstPage.NextCursor == "" {
		t.Fatalf("first recall page = %#v / %v", firstPage, err)
	}
	issuedCursor, err := decodeMemoryRecallCursor(firstPage.NextCursor)
	if err != nil || issuedCursor.SnapshotChangeSeq < 1 {
		t.Fatalf("issued cursor = %#v / %v", issuedCursor, err)
	}

	// Mutate every category that a timestamp-only cursor gets wrong. A matching
	// memory adjusted away and a matching memory forgotten must remain in the
	// traversal at their historical versions. A nonmatch adjusted into the
	// query and a new capture must not enter it.
	adjustTarget := first
	if firstPage.Hits[0].Memory.ID == first.ID {
		adjustTarget = second
	}
	var forgetTarget Memory
	for _, candidate := range []Memory{first, second, sensitive} {
		if candidate.ID != firstPage.Hits[0].Memory.ID && candidate.ID != adjustTarget.ID {
			forgetTarget = candidate
			break
		}
	}
	adjustedAway := "This adjusted capsule no longer discusses the original subject."
	zeroSalience := 0.0
	if _, err := st.AdjustMemory(ctx, p, adjustTarget.ID, AdjustMemoryInput{
		ExpectedVersion: adjustTarget.Version, Content: &adjustedAway,
		Salience: &zeroSalience, IdempotencyKey: "recall-adjust-away",
	}); err != nil {
		t.Fatal(err)
	}
	adjustedIntoQuery := "A later PostgreSQL database decision that was not in the snapshot."
	if _, err := st.AdjustMemory(ctx, p, unrelated.ID, AdjustMemoryInput{
		ExpectedVersion: unrelated.Version, Content: &adjustedIntoQuery,
		IdempotencyKey: "recall-adjust-into-query",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ForgetMemory(ctx, p, forgetTarget.ID, MemoryLifecycleInput{
		ExpectedVersion: forgetTarget.Version, Reason: "after first recall page",
		IdempotencyKey: "recall-forget-between-pages",
	}); err != nil {
		t.Fatal(err)
	}
	late := capture("recall-late", "Late PostgreSQL database decision.", "decision", 0, false)
	snapshotHits := map[string]MemoryRecallHit{
		firstPage.Hits[0].Memory.ID: firstPage.Hits[0],
	}
	for page := firstPage; page.NextCursor != ""; {
		page, err = st.RecallMemories(ctx, p, MemoryRecallOptions{
			Query: "PostgreSQL database", Limit: 1, Cursor: page.NextCursor,
		})
		if err != nil {
			t.Fatal(err)
		}
		for _, hit := range page.Hits {
			if _, duplicate := snapshotHits[hit.Memory.ID]; duplicate {
				t.Fatalf("memory appeared twice in one snapshot: %#v", hit)
			}
			snapshotHits[hit.Memory.ID] = hit
		}
	}
	if len(snapshotHits) != 3 {
		t.Fatalf("snapshot traversal has %d hits, want 3: %#v", len(snapshotHits), snapshotHits)
	}
	for _, expected := range []Memory{first, second, sensitive} {
		if _, ok := snapshotHits[expected.ID]; !ok {
			t.Fatalf("snapshot lost original memory %s: %#v", expected.ID, snapshotHits)
		}
	}
	for _, excluded := range []Memory{unrelated, late} {
		if _, ok := snapshotHits[excluded.ID]; ok {
			t.Fatalf("post-snapshot match %s entered traversal: %#v", excluded.ID, snapshotHits[excluded.ID])
		}
	}
	if hit := snapshotHits[adjustTarget.ID]; hit.Memory.Version != adjustTarget.Version ||
		hit.Memory.Content != adjustTarget.Content || hit.Memory.State != MemoryStateActive ||
		!hit.Memory.UpdatedAt.Equal(adjustTarget.UpdatedAt) {
		t.Fatalf("adjusted memory was not reconstructed at snapshot head: %#v", hit)
	}
	if hit := snapshotHits[forgetTarget.ID]; hit.Memory.Version != forgetTarget.Version ||
		hit.Memory.State != MemoryStateActive || !hit.Memory.UpdatedAt.Equal(forgetTarget.UpdatedAt) {
		t.Fatalf("forgotten memory was not reconstructed at snapshot head: %#v", hit)
	}
	if hit := snapshotHits[sensitive.ID]; !hit.Memory.Redacted || hit.Memory.LifecycleReason != "" {
		t.Fatalf("sensitive snapshot hit leaked broad-read fields: %#v", hit)
	}
	if _, err := st.RecallMemories(ctx, p, MemoryRecallOptions{Query: "different", Limit: 1, Cursor: firstPage.NextCursor}); !errors.Is(err, ErrMemoryInputInvalid) {
		t.Fatalf("mismatched cursor error = %v", err)
	}
	forged := issuedCursor
	forged.SnapshotChangeSeq = 1 << 62
	forgedCursor, err := encodeMemoryRecallCursor(forged)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.RecallMemories(ctx, p, MemoryRecallOptions{
		Query: "PostgreSQL database", Limit: 1, Cursor: forgedCursor,
	}); !errors.Is(err, ErrMemoryInputInvalid) {
		t.Fatalf("future-watermark cursor error = %v", err)
	}

	filtered, err := st.RecallMemories(ctx, p, MemoryRecallOptions{Tags: []string{"architecture"}, Kind: "note", Limit: 10})
	if err != nil || len(filtered.Hits) != 1 || filtered.Hits[0].Memory.Kind != "note" {
		t.Fatalf("structured recall = %#v / %v", filtered, err)
	}

	deleteFirst := capture("recall-delete-1", "Snapshot deletion sentinel database alpha.", "decision", 0.8, false)
	deleteSecond := capture("recall-delete-2", "Snapshot deletion sentinel database beta.", "decision", 0.7, false)
	deletePage, err := st.RecallMemories(ctx, p, MemoryRecallOptions{
		Query: "Snapshot deletion sentinel", Limit: 1,
	})
	if err != nil || len(deletePage.Hits) != 1 || deletePage.NextCursor == "" {
		t.Fatalf("pre-deletion recall page = %#v / %v", deletePage, err)
	}
	deleteTarget := deleteFirst
	if deletePage.Hits[0].Memory.ID == deleteFirst.ID {
		deleteTarget = deleteSecond
	}
	preview, err := st.DeleteMemory(ctx, p, DeleteMemoryInput{MemoryID: deleteTarget.ID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.DeleteMemory(ctx, p, DeleteMemoryInput{
		MemoryID: deleteTarget.ID, ExpectedVersion: preview.PriorVersion,
		ScrubSetRevision: preview.ScrubSetRevision, ReasonCode: "direct_user_request",
		IdempotencyKey: "recall-delete-between-pages", Apply: true,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.RecallMemories(ctx, p, MemoryRecallOptions{
		Query: "Snapshot deletion sentinel", Limit: 1, Cursor: deletePage.NextCursor,
	}); !errors.Is(err, ErrMemoryDeleted) {
		t.Fatalf("deleted-snapshot cursor error = %v", err)
	}
}
