package store

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"sort"
	"testing"
	"time"
)

func TestMemoryVectorHybridRecallPostgres(t *testing.T) {
	dsn := os.Getenv("WITSELF_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("WITSELF_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	st, _ := newMigrationTestStore(t, dsn)
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}
	provisioned, err := st.ProvisionAccount(ctx, "memory-vector@witwave.ai", "memory vector", time.Hour)
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
	capture := func(key, content string, salience float64) Memory {
		t.Helper()
		result, err := st.CaptureMemory(ctx, p, CaptureMemoryInput{
			Content: content, Kind: "decision", Salience: &salience,
			CaptureReason: "test", Evidence: []MemoryEvidenceInput{{
				ResolutionState: MemoryEvidenceUnavailable, TerminalReasonCode: "test_fixture",
			}}, IdempotencyKey: key,
		})
		if err != nil {
			t.Fatal(err)
		}
		return result.Memory
	}
	first := capture("vector-memory-1", "PostgreSQL durable database decision alpha.", 0.2)
	second := capture("vector-memory-2", "PostgreSQL durable database decision beta.", 0.9)
	third := capture("vector-memory-3", "Unrelated incident response lesson.", 0.7)
	profile, err := st.CreateMemoryVectorProfile(ctx, p, CreateMemoryVectorProfileInput{
		Provider: "fixture", Model: "two-axis", Recipe: "content-v1",
		RecipeVersion: "1", Dimensions: 2, DistanceMetric: MemoryVectorMetricCosine,
		Normalization: MemoryVectorNormalizationL2,
	})
	if err != nil {
		t.Fatal(err)
	}
	replayedProfile, err := st.CreateMemoryVectorProfile(ctx, p, CreateMemoryVectorProfileInput{
		Provider: "fixture", Model: "two-axis", Recipe: "content-v1",
		RecipeVersion: "1", Dimensions: 2, DistanceMetric: MemoryVectorMetricCosine,
		Normalization: MemoryVectorNormalizationL2,
	})
	if err != nil || replayedProfile.ID != profile.ID {
		t.Fatalf("profile replay = %#v / %v", replayedProfile, err)
	}
	profiles, err := st.ListMemoryVectorProfiles(ctx, p)
	if err != nil || len(profiles) != 1 {
		t.Fatalf("profiles = %#v / %v", profiles, err)
	}

	lexical, err := st.RecallMemories(ctx, p, MemoryRecallOptions{Query: "PostgreSQL durable", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	fallback, err := st.RecallMemories(ctx, p, MemoryRecallOptions{
		Query: "PostgreSQL durable", VectorProfileID: profile.ID,
		QueryVector: []float64{1, 0}, Limit: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if fallback.RetrievalMode != "lexical" || !fallback.Degraded || fallback.DegradedReason != "no_compatible_vectors" || len(fallback.Hits) != len(lexical.Hits) {
		t.Fatalf("zero-coverage fallback = %#v; lexical = %#v", fallback, lexical)
	}
	for i := range lexical.Hits {
		wantTotal := 0.60*fallback.Hits[i].Score.Lexical +
			0.25*fallback.Hits[i].Score.Salience + 0.15*fallback.Hits[i].Score.Recency
		if fallback.Hits[i].Memory.ID != lexical.Hits[i].Memory.ID ||
			math.Abs(fallback.Hits[i].Score.Total-wantTotal) > 1e-12 {
			t.Fatalf("fallback hit %d differs: %#v / %#v", i, fallback.Hits[i], lexical.Hits[i])
		}
	}
	put := func(memory Memory, vector []float64) MemoryVectorReceipt {
		t.Helper()
		receipt, err := st.PutMemoryVector(ctx, p, PutMemoryVectorInput{ProfileID: profile.ID,
			MemoryID: memory.ID, MemoryVersion: memory.Version,
			ContentHash: memory.ContentHash, Vector: vector})
		if err != nil {
			t.Fatal(err)
		}
		return receipt
	}
	firstReceipt := put(first, []float64{1, 0})
	put(second, []float64{0, 1})
	replayed, err := st.PutMemoryVector(ctx, p, PutMemoryVectorInput{ProfileID: profile.ID,
		MemoryID: first.ID, MemoryVersion: first.Version, ContentHash: first.ContentHash,
		Vector: []float64{1, 0}})
	if err != nil || !replayed.Replayed || replayed.VectorHash != firstReceipt.VectorHash {
		t.Fatalf("vector replay = %#v / %v", replayed, err)
	}
	if _, err := st.PutMemoryVector(ctx, p, PutMemoryVectorInput{ProfileID: profile.ID,
		MemoryID: first.ID, MemoryVersion: first.Version, ContentHash: first.ContentHash,
		Vector: []float64{0, 1}}); !errors.Is(err, ErrMemoryConflict) {
		t.Fatalf("immutable vector error = %v", err)
	}

	hybrid, err := st.RecallMemories(ctx, p, MemoryRecallOptions{
		VectorProfileID: profile.ID, QueryVector: []float64{1, 0}, Limit: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if hybrid.RetrievalMode != "hybrid" || !hybrid.Degraded || hybrid.DegradedReason != "partial_vector_coverage" ||
		hybrid.VectorCandidates != 3 || hybrid.VectorMatches != 2 || math.Abs(hybrid.VectorCoverage-2.0/3.0) > 1e-12 ||
		len(hybrid.Hits) != 3 || hybrid.Hits[0].Memory.ID != first.ID || !hybrid.Hits[0].Score.VectorUsed {
		t.Fatalf("hybrid recall = %#v (third=%s)", hybrid, third.ID)
	}
	page, err := st.RecallMemories(ctx, p, MemoryRecallOptions{
		VectorProfileID: profile.ID, QueryVector: []float64{1, 0}, Limit: 1,
	})
	if err != nil || len(page.Hits) != 1 || page.NextCursor == "" {
		t.Fatalf("first hybrid page = %#v / %v", page, err)
	}
	firstHybridCursor := page.NextCursor
	seenHybrid := map[string]bool{page.Hits[0].Memory.ID: true}
	for page.NextCursor != "" {
		page, err = st.RecallMemories(ctx, p, MemoryRecallOptions{
			VectorProfileID: profile.ID, QueryVector: []float64{1, 0},
			Limit: 1, Cursor: page.NextCursor,
		})
		if err != nil {
			t.Fatal(err)
		}
		if page.VectorCandidates != 3 || page.VectorMatches != 2 ||
			page.DegradedReason != "partial_vector_coverage" {
			t.Fatalf("hybrid page metadata = %#v", page)
		}
		for _, hit := range page.Hits {
			if seenHybrid[hit.Memory.ID] {
				t.Fatalf("hybrid cursor repeated memory %s", hit.Memory.ID)
			}
			seenHybrid[hit.Memory.ID] = true
		}
	}
	if len(seenHybrid) != 3 {
		t.Fatalf("hybrid cursor visited %d memories, want 3: %#v", len(seenHybrid), seenHybrid)
	}
	if _, err := st.RecallMemories(ctx, p, MemoryRecallOptions{
		VectorProfileID: profile.ID, QueryVector: []float64{0, 1},
		Limit: 1, Cursor: firstHybridCursor,
	}); !errors.Is(err, ErrMemoryInputInvalid) {
		t.Fatalf("hybrid cursor accepted a different query vector: %v", err)
	}

	// Fill to one slot below the hard owner ceiling, then race two distinct
	// contracts for the final slot. The transaction advisory lock must admit
	// exactly one; an exact existing contract must remain replayable at capacity.
	for i := 1; i < maxMemoryVectorProfiles-1; i++ {
		if _, err := st.CreateMemoryVectorProfile(ctx, p, CreateMemoryVectorProfileInput{
			Provider: "fixture", Model: "cap-model", Recipe: "cap-recipe",
			RecipeVersion: fmt.Sprintf("%03d", i), Dimensions: 2,
			DistanceMetric: MemoryVectorMetricCosine,
			Normalization:  MemoryVectorNormalizationL2,
		}); err != nil {
			t.Fatalf("create profile %d below cap: %v", i, err)
		}
	}
	start := make(chan struct{})
	errs := make(chan error, 2)
	for _, version := range []string{"race-a", "race-b"} {
		version := version
		go func() {
			<-start
			_, err := st.CreateMemoryVectorProfile(ctx, p, CreateMemoryVectorProfileInput{
				Provider: "fixture", Model: "cap-model", Recipe: "cap-recipe",
				RecipeVersion: version, Dimensions: 2,
				DistanceMetric: MemoryVectorMetricCosine,
				Normalization:  MemoryVectorNormalizationL2,
			})
			errs <- err
		}()
	}
	close(start)
	succeeded, refused := 0, 0
	for range 2 {
		err := <-errs
		switch {
		case err == nil:
			succeeded++
		case errors.Is(err, ErrMemoryInputInvalid):
			refused++
		default:
			t.Fatalf("concurrent cap result: %v", err)
		}
	}
	if succeeded != 1 || refused != 1 {
		t.Fatalf("concurrent cap results succeeded=%d refused=%d", succeeded, refused)
	}
	profiles, err = st.ListMemoryVectorProfiles(ctx, p)
	if err != nil || len(profiles) != maxMemoryVectorProfiles {
		t.Fatalf("profiles at cap = %d / %v", len(profiles), err)
	}
	replayedProfile, err = st.CreateMemoryVectorProfile(ctx, p, CreateMemoryVectorProfileInput{
		Provider: "fixture", Model: "two-axis", Recipe: "content-v1",
		RecipeVersion: "1", Dimensions: 2,
		DistanceMetric: MemoryVectorMetricCosine,
		Normalization:  MemoryVectorNormalizationL2,
	})
	if err != nil || replayedProfile.ID != profile.ID {
		t.Fatalf("profile replay at cap = %#v / %v", replayedProfile, err)
	}
}

// TestMemoryVectorHybridCursorPinsBoundedUniversePostgres proves two cursor
// contracts together: tied scores traverse stably by the explicit timestamp/id
// tie-breakers, and a candidate-budget traversal pages only the deterministic
// 256-row snapshot subset selected on its first page. The omitted tail never
// enters later pages, and every page keeps the non-exhaustive metadata visible.
func TestMemoryVectorHybridCursorPinsBoundedUniversePostgres(t *testing.T) {
	dsn := os.Getenv("WITSELF_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("WITSELF_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	st, _ := newMigrationTestStore(t, dsn)
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}
	provisioned, err := st.ProvisionAccount(ctx, "memory-vector-cursor@witwave.ai", "memory vector cursor", time.Hour)
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

	const candidateCount = maxMemoryHybridCandidates + 4
	salience := 0.5
	memories := make([]Memory, 0, candidateCount)
	for i := 0; i < candidateCount; i++ {
		result, err := st.CaptureMemory(ctx, p, CaptureMemoryInput{
			Content: "Stable tied candidate.", Kind: "decision", Salience: &salience,
			CaptureReason: "test", Evidence: []MemoryEvidenceInput{{
				ResolutionState: MemoryEvidenceUnavailable, TerminalReasonCode: "test_fixture",
			}}, IdempotencyKey: fmt.Sprintf("vector-cursor-%03d", i),
		})
		if err != nil {
			t.Fatalf("capture candidate %d: %v", i, err)
		}
		memories = append(memories, result.Memory)
	}
	// Equalize every ranking signal other than id. This is intentionally a
	// test-only database adjustment; production writes keep versions immutable.
	tiedAt := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	if _, err := st.pool.Exec(ctx, `
		UPDATE memory_versions SET created_at=$1
		WHERE account_id=$2 AND realm_id=$3 AND owner_kind='agent' AND owner_id=$4`,
		tiedAt, p.AccountID, p.RealmID, p.ID); err != nil {
		t.Fatal(err)
	}
	profile, err := st.CreateMemoryVectorProfile(ctx, p, CreateMemoryVectorProfileInput{
		Provider: "fixture", Model: "tied-axis", Recipe: "content-v1",
		RecipeVersion: "1", Dimensions: 2, DistanceMetric: MemoryVectorMetricCosine,
		Normalization: MemoryVectorNormalizationL2,
	})
	if err != nil {
		t.Fatal(err)
	}
	for i, memory := range memories {
		if _, err := st.PutMemoryVector(ctx, p, PutMemoryVectorInput{
			ProfileID: profile.ID, MemoryID: memory.ID, MemoryVersion: memory.Version,
			ContentHash: memory.ContentHash, Vector: []float64{1, 0},
		}); err != nil {
			t.Fatalf("put candidate vector %d: %v", i, err)
		}
	}

	expected := make([]string, len(memories))
	for i := range memories {
		expected[i] = memories[i].ID
	}
	sort.Sort(sort.Reverse(sort.StringSlice(expected)))
	expected = expected[:maxMemoryHybridCandidates]

	const pageLimit = 100
	var cursor string
	visited := make([]string, 0, maxMemoryHybridCandidates)
	seen := make(map[string]bool, maxMemoryHybridCandidates)
	pages := 0
	for {
		page, err := st.RecallMemories(ctx, p, MemoryRecallOptions{
			VectorProfileID: profile.ID, QueryVector: []float64{1, 0},
			Limit: pageLimit, Cursor: cursor,
		})
		if err != nil {
			t.Fatal(err)
		}
		pages++
		if !page.CandidateTruncated || page.CandidateLimit != maxMemoryHybridCandidates ||
			!page.Degraded || page.DegradedReason != "candidate_budget_exceeded" ||
			page.RetrievalMode != "hybrid" || page.VectorCandidates != maxMemoryHybridCandidates ||
			page.VectorMatches != maxMemoryHybridCandidates || page.VectorCoverage != 1 {
			t.Fatalf("page %d lost bounded-universe metadata: %#v", pages, page)
		}
		for _, hit := range page.Hits {
			if seen[hit.Memory.ID] {
				t.Fatalf("page %d repeated tied candidate %s", pages, hit.Memory.ID)
			}
			seen[hit.Memory.ID] = true
			visited = append(visited, hit.Memory.ID)
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	if pages != 3 || len(visited) != maxMemoryHybridCandidates {
		t.Fatalf("bounded traversal pages=%d candidates=%d, want 3/%d", pages, len(visited), maxMemoryHybridCandidates)
	}
	for i := range expected {
		if visited[i] != expected[i] {
			t.Fatalf("bounded traversal candidate %d = %s, want %s", i, visited[i], expected[i])
		}
	}
}
