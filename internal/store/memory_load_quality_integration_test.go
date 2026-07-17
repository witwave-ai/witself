package store

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/loadquality"
)

const memoryLoadQualityEnabled = "WITSELF_MEMORY_LOAD_QUALITY"

// TestNarrativeMemoryLoadQualityPostgres is the first opt-in production-load
// and recall-quality baseline. It creates two synthetic accounts and three
// synthetic agents in a fresh disposable schema, exercises only deterministic
// PostgreSQL memory behavior, writes one sanitized aggregate result, and lets
// newMigrationTestStore drop the entire schema during cleanup.
//
// This is intentionally not an AI evaluation. No model, embedding provider,
// runtime credential, live account, or secret is read or created.
func TestNarrativeMemoryLoadQualityPostgres(t *testing.T) {
	if os.Getenv(memoryLoadQualityEnabled) != "1" {
		t.Skip(memoryLoadQualityEnabled + "=1 is required")
	}
	dsn := strings.TrimSpace(os.Getenv("WITSELF_TEST_DATABASE_URL"))
	if dsn == "" {
		t.Fatal("WITSELF_TEST_DATABASE_URL is required when memory load-quality testing is enabled")
	}
	opts, err := loadquality.ParseOptions(os.Getenv)
	if err != nil {
		t.Fatal(err)
	}
	corpus, corpusDigest, err := loadquality.DefaultCorpus()
	if err != nil {
		t.Fatal(err)
	}
	noise, err := loadquality.GenerateNoise(opts.Seed, opts.NoiseMemories)
	if err != nil {
		t.Fatal(err)
	}

	startedAt := time.Now().UTC()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	st, _ := newMigrationTestStore(t, dsn)
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}
	var postgresVersion string
	if err := st.pool.QueryRow(ctx, `SHOW server_version`).Scan(&postgresVersion); err != nil {
		t.Fatal("read PostgreSQL version")
	}

	owner, peer, otherTenant := provisionMemoryLoadQualityPrincipals(ctx, t, st, opts.Seed)
	memoriesByLabel := make(map[string]Memory, len(corpus.Memories))
	captureDurations := make([]time.Duration, 0, len(corpus.Memories)+len(noise))
	captureStarted := time.Now()
	for _, fixture := range append(append([]loadquality.CorpusMemory(nil), corpus.Memories...), noise...) {
		operationStarted := time.Now()
		captured, err := st.CaptureMemory(ctx, owner, CaptureMemoryInput{
			Content: fixture.Content, Kind: fixture.Kind, Tags: fixture.Tags,
			Salience: &fixture.Salience, Sensitive: fixture.Sensitive,
			CaptureReason: "load_quality",
			Evidence: []MemoryEvidenceInput{{
				ResolutionState:    MemoryEvidenceUnavailable,
				TerminalReasonCode: "synthetic_fixture",
			}},
			Client: MemoryClientProvenance{
				Runtime: "load-quality", Recipe: loadquality.CorpusSchemaV1,
				RecipeVersion: loadquality.HarnessVersion,
			},
			IdempotencyKey: fmt.Sprintf("load-quality-%d-%s", opts.Seed, fixture.Label),
		})
		captureDurations = append(captureDurations, time.Since(operationStarted))
		if err != nil {
			t.Fatalf("capture synthetic memory %s: %v", fixture.Label, err)
		}
		if _, isCorpus := findCorpusMemory(corpus, fixture.Label); isCorpus {
			memoriesByLabel[fixture.Label] = captured.Memory
		}
	}
	captureWall := time.Since(captureStarted)
	captureStats, err := loadquality.Summarize(captureDurations, captureWall)
	if err != nil {
		t.Fatal(err)
	}

	relevanceResults := make([]loadquality.RelevanceCaseResult, 0, len(corpus.RelevanceQueries))
	for _, query := range corpus.RelevanceQueries {
		rank, err := memoryLoadQualityRank(ctx, st, owner, query, memoriesByLabel, true)
		if err != nil {
			t.Fatalf("quality case %s: %v", query.Name, err)
		}
		passed := rank >= 1 && rank <= query.MaximumRank
		relevanceResults = append(relevanceResults, loadquality.RelevanceCaseResult{
			Name: query.Name, Passed: passed, ObservedRank: rank, MaximumRank: query.MaximumRank,
		})
		if !passed {
			t.Fatalf("quality case %s observed rank %d, want 1..%d", query.Name, rank, query.MaximumRank)
		}
	}

	sensitiveFixture, ok := findCorpusMemory(corpus, corpus.SensitiveProbe.ExpectedLabel)
	if !ok {
		t.Fatal("sensitive corpus fixture is missing")
	}
	sensitiveMemory := memoriesByLabel[corpus.SensitiveProbe.ExpectedLabel]
	broadPage, err := st.RecallMemories(ctx, owner, MemoryRecallOptions{
		Query: corpus.SensitiveProbe.Query, Limit: 10,
	})
	if err != nil {
		t.Fatal("run broad sensitive recall")
	}
	broadHit, broadRank := memoryLoadQualityHit(broadPage, sensitiveMemory.ID)
	sensitiveBroadRedacted := broadRank >= 1 && broadRank <= corpus.SensitiveProbe.MaximumRank &&
		broadHit.Memory.Sensitive && broadHit.Memory.Redacted &&
		broadHit.Memory.Content == "" && broadHit.Memory.ContentHash == "" && len(broadHit.Memory.Tags) == 0
	if !sensitiveBroadRedacted {
		t.Fatal("broad sensitive recall did not return a redacted synthetic hit")
	}
	exactPage, err := st.RecallMemories(ctx, owner, MemoryRecallOptions{
		Query: corpus.SensitiveProbe.Query, IncludeSensitive: true, Limit: 10,
	})
	if err != nil {
		t.Fatal("run exact owner sensitive recall")
	}
	exactHit, exactRank := memoryLoadQualityHit(exactPage, sensitiveMemory.ID)
	sensitiveExactOwnerVisible := exactRank >= 1 && exactRank <= corpus.SensitiveProbe.MaximumRank &&
		exactHit.Memory.Sensitive && !exactHit.Memory.Redacted &&
		exactHit.Memory.Content == sensitiveFixture.Content
	if !sensitiveExactOwnerVisible {
		t.Fatal("exact owner sensitive recall did not return the synthetic fixture")
	}

	isolationQuery := corpus.RelevanceQueries[0].Query
	ownerCheck, err := st.RecallMemories(ctx, owner, MemoryRecallOptions{
		Query: isolationQuery, ExcludeSensitive: true, Limit: 10,
	})
	if err != nil || len(ownerCheck.Hits) == 0 {
		t.Fatalf("owner isolation control query returned no fixture: %v", err)
	}
	peerPage, err := st.RecallMemories(ctx, peer, MemoryRecallOptions{
		Query: isolationQuery, ExcludeSensitive: true, Limit: 10,
	})
	if err != nil {
		t.Fatal("run cross-agent isolation query")
	}
	otherTenantPage, err := st.RecallMemories(ctx, otherTenant, MemoryRecallOptions{
		Query: isolationQuery, ExcludeSensitive: true, Limit: 10,
	})
	if err != nil {
		t.Fatal("run cross-tenant isolation query")
	}
	crossAgentIsolated := len(peerPage.Hits) == 0
	crossTenantIsolated := len(otherTenantPage.Hits) == 0
	if !crossAgentIsolated || !crossTenantIsolated {
		t.Fatalf("memory isolation failed: cross-agent=%t cross-tenant=%t", crossAgentIsolated, crossTenantIsolated)
	}

	recallDurations, recallWall, err := runMemoryLoadQualityRecalls(
		ctx, st, owner, corpus.RelevanceQueries, memoriesByLabel,
		opts.QueryIterations, opts.Concurrency,
	)
	if err != nil {
		t.Fatal(err)
	}
	recallStats, err := loadquality.Summarize(recallDurations, recallWall)
	if err != nil {
		t.Fatal(err)
	}

	result := loadquality.Result{
		Schema: loadquality.ResultSchemaV1, HarnessVersion: loadquality.HarnessVersion,
		StartedAt: startedAt, CompletedAt: time.Now().UTC(), Outcome: "pass",
		PostgreSQLVersion: strings.TrimSpace(postgresVersion),
		Environment:       loadquality.Environment(opts),
		Workload: loadquality.Workload{
			Seed: opts.Seed, CorpusSHA256: corpusDigest,
			SyntheticAccounts: 2, SyntheticAgents: 3,
			CorpusMemories: len(corpus.Memories), NoiseMemories: opts.NoiseMemories,
			QueryIterations: opts.QueryIterations, Concurrency: opts.Concurrency,
		},
		Measurements: loadquality.Measurements{Capture: captureStats, Recall: recallStats},
		Quality: loadquality.Quality{
			RelevanceCases: relevanceResults, RelevancePassRate: 1,
			SensitiveBroadRedacted:     sensitiveBroadRedacted,
			SensitiveExactOwnerVisible: sensitiveExactOwnerVisible,
			CrossAgentIsolated:         crossAgentIsolated, CrossTenantIsolated: crossTenantIsolated,
		},
	}
	raw, err := loadquality.WriteResult(opts.ResultsPath, result)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("sanitized load-quality result written to %s", opts.ResultsPath)
	t.Logf("sanitized load-quality result:\n%s", raw)
}

func provisionMemoryLoadQualityPrincipals(
	ctx context.Context,
	t *testing.T,
	st *Store,
	seed int64,
) (Principal, Principal, Principal) {
	t.Helper()
	createAccount := func(label string, agents ...string) []Principal {
		t.Helper()
		provisioned, err := st.ProvisionAccount(
			ctx,
			fmt.Sprintf("memory-load-quality-%d-%s@example.invalid", seed, label),
			"memory load quality "+label,
			time.Hour,
		)
		if err != nil {
			t.Fatal(err)
		}
		if activated, err := st.ActivateAccount(ctx, provisioned.AccountID); err != nil || !activated {
			t.Fatalf("activate synthetic account %s: activated=%t err=%v", label, activated, err)
		}
		realm, err := st.CreateRealm(ctx, provisioned.AccountID, "load-quality")
		if err != nil {
			t.Fatal(err)
		}
		principals := make([]Principal, 0, len(agents))
		for _, name := range agents {
			agent, err := st.CreateAgent(ctx, provisioned.AccountID, realm.ID, name)
			if err != nil {
				t.Fatal(err)
			}
			principals = append(principals, Principal{
				Kind: PrincipalAgent, ID: agent.ID, AccountID: provisioned.AccountID,
				RealmID: realm.ID, AgentName: name, RealmName: realm.Name, AccountStatus: "active",
			})
		}
		return principals
	}
	first := createAccount("primary", "owner", "peer")
	second := createAccount("other", "other")
	return first[0], first[1], second[0]
}

func memoryLoadQualityRank(
	ctx context.Context,
	st *Store,
	principal Principal,
	query loadquality.QueryCase,
	memories map[string]Memory,
	excludeSensitive bool,
) (int, error) {
	expected, ok := memories[query.ExpectedLabel]
	if !ok {
		return 0, errorsForLoadQuality("expected corpus label is missing")
	}
	page, err := st.RecallMemories(ctx, principal, MemoryRecallOptions{
		Query: query.Query, ExcludeSensitive: excludeSensitive, Limit: 20,
	})
	if err != nil {
		return 0, err
	}
	for index, hit := range page.Hits {
		if hit.Memory.ID == expected.ID {
			return index + 1, nil
		}
	}
	return 0, nil
}

func runMemoryLoadQualityRecalls(
	ctx context.Context,
	st *Store,
	principal Principal,
	queries []loadquality.QueryCase,
	memories map[string]Memory,
	iterations int,
	concurrency int,
) ([]time.Duration, time.Duration, error) {
	total := iterations * len(queries)
	tasks := make(chan loadquality.QueryCase, total)
	for iteration := 0; iteration < iterations; iteration++ {
		for _, query := range queries {
			tasks <- query
		}
	}
	close(tasks)
	durations := make(chan time.Duration, total)
	errors := make(chan error, total)
	var workers sync.WaitGroup
	started := time.Now()
	for worker := 0; worker < concurrency; worker++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for query := range tasks {
				operationStarted := time.Now()
				rank, err := memoryLoadQualityRank(ctx, st, principal, query, memories, true)
				durations <- time.Since(operationStarted)
				if err != nil {
					errors <- fmt.Errorf("recall case %s failed: %w", query.Name, err)
					continue
				}
				if rank < 1 || rank > query.MaximumRank {
					errors <- fmt.Errorf("recall case %s observed rank %d", query.Name, rank)
				}
			}
		}()
	}
	workers.Wait()
	wall := time.Since(started)
	close(durations)
	close(errors)
	for err := range errors {
		return nil, 0, err
	}
	out := make([]time.Duration, 0, total)
	for duration := range durations {
		out = append(out, duration)
	}
	if len(out) != total {
		return nil, 0, fmt.Errorf("recall measurement count %d, want %d", len(out), total)
	}
	return out, wall, nil
}

func memoryLoadQualityHit(page MemoryRecallPage, memoryID string) (MemoryRecallHit, int) {
	for index, hit := range page.Hits {
		if hit.Memory.ID == memoryID {
			return hit, index + 1
		}
	}
	return MemoryRecallHit{}, 0
}

func findCorpusMemory(corpus loadquality.Corpus, label string) (loadquality.CorpusMemory, bool) {
	for _, memory := range corpus.Memories {
		if memory.Label == label {
			return memory, true
		}
	}
	return loadquality.CorpusMemory{}, false
}

// errorsForLoadQuality avoids attaching values or durable ids to one internal
// harness invariant error while keeping the call site readable.
func errorsForLoadQuality(message string) error {
	return fmt.Errorf("memory load-quality: %s", message)
}
