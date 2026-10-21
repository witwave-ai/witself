package store

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"maps"
	"strings"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/testenv"
)

func TestMemoryRecallOptionsAndCursor(t *testing.T) {
	if _, err := normalizeMemoryRecallOptions(MemoryRecallOptions{}); !errors.Is(err, ErrMemoryInputInvalid) {
		t.Fatalf("empty recall error = %v", err)
	}
	if _, err := normalizeMemoryRecallOptions(MemoryRecallOptions{Query: "database", Limit: 101}); !errors.Is(err, ErrMemoryInputInvalid) {
		t.Fatalf("oversized recall error = %v", err)
	}
	for _, fallback := range []string{"all_terms", "ANY_TERMS", " any_terms", " "} {
		if _, err := normalizeMemoryRecallOptions(MemoryRecallOptions{Query: "database", QueryFallback: fallback}); !errors.Is(err, ErrMemoryInputInvalid) {
			t.Fatalf("invalid query fallback %q error = %v", fallback, err)
		}
	}
	hybrid, err := normalizeMemoryRecallOptions(MemoryRecallOptions{
		Query: "alpha delta", QueryFallback: "any_terms", VectorProfileID: "profile", QueryVector: []float64{1, 0},
	})
	if err != nil || hybrid.QueryFallback != "" {
		t.Fatalf("hybrid opt-in was not ignored: fallback=%q error=%v", hybrid.QueryFallback, err)
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

func TestMemoryRecallBindingHashMatchesLegacyWithoutOptIn(t *testing.T) {
	// This is the binding shape from HEAD 8f5545dc6548, including field order.
	type legacyMemoryRecallCursorBinding struct {
		Query            string     `json:"query"`
		Kind             string     `json:"kind,omitempty"`
		Tags             []string   `json:"tags,omitempty"`
		Links            []string   `json:"links,omitempty"`
		Origin           string     `json:"origin,omitempty"`
		CaptureReason    string     `json:"capture_reason,omitempty"`
		IncludeSensitive bool       `json:"include_sensitive"`
		ExcludeSensitive bool       `json:"exclude_sensitive,omitempty"`
		OccurredFrom     *time.Time `json:"occurred_from,omitempty"`
		OccurredUntil    *time.Time `json:"occurred_until,omitempty"`
		CapturedFrom     *time.Time `json:"captured_from,omitempty"`
		CapturedUntil    *time.Time `json:"captured_until,omitempty"`
		VectorProfileID  string     `json:"vector_profile_id,omitempty"`
		QueryVectorHash  string     `json:"query_vector_hash,omitempty"`
	}
	start := time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)
	end := start.Add(time.Hour)
	for _, opts := range []MemoryRecallOptions{
		{Query: "alpha delta"},
		{
			Query: "alpha delta", Kind: "decision", Tags: []string{"architecture"}, Links: []string{"fixture:recall"},
			Origin: "agent", CaptureReason: "test", IncludeSensitive: true,
			OccurredFrom: &start, OccurredUntil: &end, CapturedFrom: &start, CapturedUntil: &end,
		},
		{Query: "alpha delta", ExcludeSensitive: true, VectorProfileID: "profile", QueryVector: []float64{1, 0}},
	} {
		queryVectorHash := ""
		if len(opts.QueryVector) > 0 {
			raw, err := json.Marshal(opts.QueryVector)
			if err != nil {
				t.Fatal(err)
			}
			sum := sha256.Sum256(raw)
			queryVectorHash = hex.EncodeToString(sum[:])
		}
		want, err := memoryRequestHash(legacyMemoryRecallCursorBinding{
			Query: opts.Query, Kind: opts.Kind, Tags: opts.Tags, Links: opts.Links,
			Origin: opts.Origin, CaptureReason: opts.CaptureReason,
			IncludeSensitive: opts.IncludeSensitive, ExcludeSensitive: opts.ExcludeSensitive,
			OccurredFrom: opts.OccurredFrom, OccurredUntil: opts.OccurredUntil,
			CapturedFrom: opts.CapturedFrom, CapturedUntil: opts.CapturedUntil,
			VectorProfileID: opts.VectorProfileID, QueryVectorHash: queryVectorHash,
		})
		if err != nil {
			t.Fatal(err)
		}
		got, err := memoryRecallBindingHash(opts)
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatal("recall binding without opt-in differs from the legacy binding")
		}
		opts.QueryFallback = "any_terms"
		opts, err = normalizeMemoryRecallOptions(opts)
		if err != nil {
			t.Fatal(err)
		}
		optedIn, err := memoryRecallBindingHash(opts)
		if err != nil {
			t.Fatal(err)
		}
		if opts.VectorProfileID == "" && optedIn == want {
			t.Fatal("recall binding does not include query_fallback")
		}
		if opts.VectorProfileID != "" && optedIn != want {
			t.Fatal("hybrid recall binding did not ignore query_fallback")
		}
	}
}

func TestMemoryRecallQuerySemanticsPostgres(t *testing.T) {
	dsn := testenv.RequirePostgres(t)
	ctx := context.Background()
	st, _ := newMigrationTestStore(t, dsn)
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}
	provisioned, err := st.ProvisionAccount(ctx, "memory-recall-semantics@witwave.ai", "memory recall semantics", time.Hour)
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

	capture := func(key, content string) Memory {
		t.Helper()
		result, err := st.CaptureMemory(ctx, p, CaptureMemoryInput{
			Content: content, Kind: "note", CaptureReason: "test",
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
	a := capture("recall-semantics-a", "Fixture alpha beta.")
	b := capture("recall-semantics-b", "Fixture alpha gamma.")
	t.Logf("fixture IDs: A=%s B=%s", a.ID, b.ID)

	t.Run("omitted_opt_in_keeps_strict_matching", func(t *testing.T) {
		page, err := st.RecallMemories(ctx, p, MemoryRecallOptions{Query: "alpha delta", Limit: 10})
		if err != nil || len(page.Hits) != 0 || page.QueryFallback != "" {
			t.Fatalf("recall without opt-in: hits=%d fallback=%q error=%v", len(page.Hits), page.QueryFallback, err)
		}
	})

	for _, tt := range []struct {
		query    string
		want     []string
		fallback string
	}{
		{query: "alpha beta", want: []string{a.ID}},
		{query: "alpha delta", want: []string{a.ID, b.ID}, fallback: "any_terms"},
		{query: "beta OR gamma", want: []string{a.ID, b.ID}},
		{query: "beta or gamma", want: []string{a.ID, b.ID}},
		{query: "beta or alpha gamma", want: []string{a.ID, b.ID}},
		{query: "alpha -gamma", want: []string{a.ID}},
		{query: `"alpha beta"`, want: []string{a.ID}},
		{query: `"beta alpha"`},
		{query: "alpha AND beta", want: []string{a.ID, b.ID}, fallback: "any_terms"},
		{query: "zeta eta", fallback: "any_terms"},
	} {
		t.Run(tt.query, func(t *testing.T) {
			page, err := st.RecallMemories(ctx, p, MemoryRecallOptions{Query: tt.query, QueryFallback: "any_terms", Limit: 10})
			if err != nil {
				t.Fatal(err)
			}
			if page.QueryFallback != tt.fallback {
				t.Fatalf("query fallback = %q, want %q", page.QueryFallback, tt.fallback)
			}
			got := make(map[string]bool, len(page.Hits))
			for _, hit := range page.Hits {
				got[hit.Memory.ID] = true
			}
			want := make(map[string]bool, len(tt.want))
			for _, id := range tt.want {
				want[id] = true
			}
			t.Logf("hit-ID set = %v", got)
			if len(page.Hits) != len(want) || !maps.Equal(got, want) {
				t.Fatalf("recall hit-ID set = %v (%d hits), want %v", got, len(page.Hits), want)
			}
		})
	}
	t.Run("compound_version_keeps_its_lexeme", func(t *testing.T) {
		version := capture("recall-version", "Fixture v0.0.337 deployed.")
		capture("recall-version-fragment", "Fixture v0 deployed.")
		page, err := st.RecallMemories(ctx, p, MemoryRecallOptions{
			Query: "v0.0.337 unrelatedword", QueryFallback: "any_terms",
		})
		if err != nil || page.QueryFallback != "any_terms" || len(page.Hits) != 1 || page.Hits[0].Memory.ID != version.ID {
			t.Fatalf("compound version recall: hits=%d fallback=%q error=%v", len(page.Hits), page.QueryFallback, err)
		}
	})
}

func TestMemoryRecallPostgres(t *testing.T) {
	dsn := testenv.RequirePostgres(t)
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

func TestMemoryRecallAnyTermsFallbackPagingPostgres(t *testing.T) {
	dsn := testenv.RequirePostgres(t)
	ctx := context.Background()
	st, _ := newMigrationTestStore(t, dsn)
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}
	provisioned, err := st.ProvisionAccount(ctx, "memory-recall-fallback@witwave.ai", "memory recall fallback", time.Hour)
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
	for i, content := range []string{"Fixture alpha beta.", "Fixture alpha gamma.", "Sensitive fixture alpha beta."} {
		_, err := st.CaptureMemory(ctx, p, CaptureMemoryInput{
			Content: content, Kind: "note", CaptureReason: "test", Sensitive: i == 2,
			Evidence:       []MemoryEvidenceInput{{ResolutionState: MemoryEvidenceUnavailable, TerminalReasonCode: "test_fixture"}},
			IdempotencyKey: content,
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	first, err := st.RecallMemories(ctx, p, MemoryRecallOptions{QueryFallback: "any_terms", Query: "alpha delta", Limit: 1})
	if err != nil || len(first.Hits) != 1 || first.NextCursor == "" || first.QueryFallback != "any_terms" {
		t.Fatalf("first fallback page: hits=%d has_cursor=%v fallback=%q error=%v", len(first.Hits), first.NextCursor != "", first.QueryFallback, err)
	}
	issued, err := decodeMemoryRecallCursor(first.NextCursor)
	if err != nil || issued.Version != 3 || !issued.AnyTerms {
		t.Fatalf("fallback cursor: version=%d any_terms=%v error=%v", issued.Version, issued.AnyTerms, err)
	}
	seen := map[string]bool{first.Hits[0].Memory.ID: true}
	for page := first; page.NextCursor != ""; {
		page, err = st.RecallMemories(ctx, p, MemoryRecallOptions{QueryFallback: "any_terms", Query: "alpha delta", Limit: 1, Cursor: page.NextCursor})
		if err != nil || len(page.Hits) != 1 || page.QueryFallback != "any_terms" {
			t.Fatalf("later fallback page: hits=%d fallback=%q error=%v", len(page.Hits), page.QueryFallback, err)
		}
		id := page.Hits[0].Memory.ID
		if seen[id] {
			t.Fatal("fallback traversal repeated a memory")
		}
		seen[id] = true
	}
	if len(seen) != 3 {
		t.Fatalf("fallback traversal has %d hits, want 3", len(seen))
	}
	if _, err := st.RecallMemories(ctx, p, MemoryRecallOptions{QueryFallback: "any_terms", Query: "alpha epsilon", Cursor: first.NextCursor}); !errors.Is(err, ErrMemoryInputInvalid) || !strings.Contains(err.Error(), "does not match filters") {
		t.Fatalf("changed-query cursor error = %v", err)
	}
	for _, tt := range []struct {
		name     string
		version  int
		anyTerms bool
	}{
		{name: "v3_without_any_terms", version: 3},
		{name: "v2_with_any_terms", version: 2, anyTerms: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			forged := issued
			forged.Version, forged.AnyTerms = tt.version, tt.anyTerms
			if _, err := encodeMemoryRecallCursor(forged); !errors.Is(err, ErrMemoryInputInvalid) {
				t.Fatalf("invalid cursor encoding error = %v", err)
			}
			raw, err := json.Marshal(forged)
			if err != nil {
				t.Fatal(err)
			}
			cursor := base64.RawURLEncoding.EncodeToString(raw)
			if _, err := st.RecallMemories(ctx, p, MemoryRecallOptions{QueryFallback: "any_terms", Query: "alpha delta", Cursor: cursor}); !errors.Is(err, ErrMemoryInputInvalid) {
				t.Fatalf("invalid cursor decoding error = %v", err)
			}
		})
	}
	strict, err := st.RecallMemories(ctx, p, MemoryRecallOptions{QueryFallback: "any_terms", Query: "alpha", Limit: 1})
	if err != nil || strict.NextCursor == "" || strict.QueryFallback != "" {
		t.Fatalf("strict cursor setup: has_cursor=%v fallback=%q error=%v", strict.NextCursor != "", strict.QueryFallback, err)
	}
	strictCursor, err := decodeMemoryRecallCursor(strict.NextCursor)
	if err != nil || strictCursor.Version != 2 || strictCursor.AnyTerms {
		t.Fatalf("strict cursor: version=%d any_terms=%v error=%v", strictCursor.Version, strictCursor.AnyTerms, err)
	}
	// Bind forged cursors to the exact normalized request, so rejection cannot
	// accidentally rely on the unrelated filter-hash mismatch check.
	forge := func(base memoryRecallCursor, opts MemoryRecallOptions, score float64) string {
		t.Helper()
		normalized, err := normalizeMemoryRecallOptions(opts)
		if err != nil {
			t.Fatal(err)
		}
		base.QueryHash, err = memoryRecallBindingHash(normalized)
		if err != nil {
			t.Fatal(err)
		}
		base.AfterScore = score
		cursor, err := encodeMemoryRecallCursor(base)
		if err != nil {
			t.Fatal(err)
		}
		return cursor
	}
	t.Run("fallback_cursor_requires_opt_in", func(t *testing.T) {
		opts := MemoryRecallOptions{Query: "alpha delta"}
		opts.Cursor = forge(issued, opts, 2)
		page, err := st.RecallMemories(ctx, p, opts)
		if !errors.Is(err, ErrMemoryInputInvalid) || strings.Contains(err.Error(), "does not match filters") || len(page.Hits) != 0 {
			t.Fatalf("fallback cursor without opt-in: hits=%d error=%v", len(page.Hits), err)
		}
	})
	t.Run("strict_cursor_never_starts_fallback", func(t *testing.T) {
		opts := MemoryRecallOptions{QueryFallback: "any_terms", Query: "alpha delta"}
		opts.Cursor = forge(strictCursor, opts, -1)
		page, err := st.RecallMemories(ctx, p, opts)
		if err != nil || len(page.Hits) != 0 || page.QueryFallback != "" {
			t.Fatalf("strict cursor started fallback: hits=%d fallback=%q error=%v", len(page.Hits), page.QueryFallback, err)
		}
	})
	profile, err := st.CreateMemoryVectorProfile(ctx, p, CreateMemoryVectorProfileInput{
		Provider: "fixture", Model: "two-axis", Recipe: "content-v1", RecipeVersion: "1",
		Dimensions: 2, DistanceMetric: MemoryVectorMetricCosine, Normalization: MemoryVectorNormalizationL2,
	})
	if err != nil {
		t.Fatal(err)
	}
	hybrid, err := st.RecallMemories(ctx, p, MemoryRecallOptions{
		QueryFallback: "any_terms", Query: "alpha delta", VectorProfileID: profile.ID, QueryVector: []float64{1, 0},
	})
	if err != nil || hybrid.QueryFallback != "" {
		t.Fatalf("hybrid recall fallback = %q, error = %v", hybrid.QueryFallback, err)
	}
	for _, tt := range []struct {
		name string
		opts MemoryRecallOptions
	}{
		{name: "vector_profile_rejects_fallback_cursor", opts: MemoryRecallOptions{QueryFallback: "any_terms", Query: "alpha delta", VectorProfileID: profile.ID, QueryVector: []float64{1, 0}}},
		{name: "quoted_query_rejects_fallback_cursor", opts: MemoryRecallOptions{QueryFallback: "any_terms", Query: `"beta alpha"`}},
		{name: "minus_query_rejects_fallback_cursor", opts: MemoryRecallOptions{QueryFallback: "any_terms", Query: "alpha -gamma"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			opts := tt.opts
			opts.Cursor = forge(issued, opts, 2)
			page, err := st.RecallMemories(ctx, p, opts)
			if !errors.Is(err, ErrMemoryInputInvalid) || strings.Contains(err.Error(), "does not match filters") || len(page.Hits) != 0 {
				t.Fatalf("ineligible fallback cursor: hits=%d error=%v", len(page.Hits), err)
			}
		})
	}
	filtered, err := st.RecallMemories(ctx, p, MemoryRecallOptions{QueryFallback: "any_terms", Query: "alpha delta", Kind: "decision"})
	if err != nil || len(filtered.Hits) != 0 || filtered.QueryFallback != "any_terms" {
		t.Fatalf("kind-filtered fallback: hits=%d fallback=%q error=%v", len(filtered.Hits), filtered.QueryFallback, err)
	}
	excluded, err := st.RecallMemories(ctx, p, MemoryRecallOptions{QueryFallback: "any_terms", Query: "alpha delta", ExcludeSensitive: true})
	if err != nil || len(excluded.Hits) != 2 || excluded.QueryFallback != "any_terms" {
		t.Fatalf("sensitivity-filtered fallback: hits=%d fallback=%q error=%v", len(excluded.Hits), excluded.QueryFallback, err)
	}
	for _, hit := range excluded.Hits {
		if hit.Memory.Sensitive || hit.Memory.Redacted {
			t.Fatal("sensitive memory survived fallback ExcludeSensitive filter")
		}
	}
}
