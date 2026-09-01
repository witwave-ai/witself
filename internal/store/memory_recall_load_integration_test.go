package store

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"math"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/loadquality"
)

const (
	memoryRecallLoadEnabled    = "WITSELF_MEMORY_RECALL_LOAD"
	memoryRecallLoadProbeLimit = 20
	memoryRecallLoadOverall    = 9 * time.Minute
	memoryRecallLoadDeadline   = 2 * time.Minute
	// This is the public, documented candidate budget returned by vector recall.
	// The production constant is deliberately not used by the harness: workload
	// assertions observe the public page metadata instead of depending on an
	// internal implementation symbol.
	memoryRecallLoadCandidateBudget = loadquality.RecallCandidateLimit
)

const (
	memoryRecallLoadLadderTag = "recall_load_ladder"
	memoryRecallLoadKind      = "observation"
	memoryRecallLoadQuery     = "cardinality ladder beacon"
)

type memoryRecallLoadTenant struct {
	principal Principal
	memories  []Memory
}

type memoryRecallLoadPrincipals struct {
	ladder []Principal
	peer   Principal
}

// TestNarrativeMemoryRecallLoadPostgres is the third opt-in executable slice
// for narrative-memory production readiness. It drives PostgreSQL lexical and
// client-vector recall only. Every vector is generated locally from the signed
// deterministic seed; no model, embedding provider, runtime client, MCP,
// credential, secret, or sealed-plane surface is called.
func TestNarrativeMemoryRecallLoadPostgres(t *testing.T) {
	if os.Getenv(memoryRecallLoadEnabled) != "1" {
		t.Skip(memoryRecallLoadEnabled + "=1 is required")
	}
	dsn := strings.TrimSpace(os.Getenv("WITSELF_TEST_DATABASE_URL"))
	if dsn == "" {
		t.Fatal("WITSELF_TEST_DATABASE_URL is required when memory recall load testing is enabled")
	}
	opts, err := loadquality.ParseRecallOptions(os.Getenv)
	if err != nil {
		t.Fatal(err)
	}

	startedAt := time.Now().UTC()
	ctx, cancel := context.WithTimeout(context.Background(), memoryRecallLoadOverall)
	defer cancel()
	st, _ := newMigrationTestStore(t, dsn)
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}
	var postgresVersion string
	if err := st.pool.QueryRow(ctx, `SHOW server_version`).Scan(&postgresVersion); err != nil {
		t.Fatalf("read PostgreSQL version: %v", err)
	}

	principals, err := provisionMemoryRecallLoadPrincipals(ctx, st, opts.Seed, len(opts.Cardinalities))
	if err != nil {
		t.Fatal(err)
	}

	var cardinalityMeasurements []loadquality.RecallCardinalityMeasurement
	var cardinalityOutcome loadquality.RecallCardinalityLadderOutcome
	var tenants []memoryRecallLoadTenant
	var coverageMeasurements []loadquality.RecallVectorCoverageMeasurement
	var coverageOutcome loadquality.RecallVectorCoverageOutcome
	var qualityStats loadquality.OperationStats
	var qualityOutcome loadquality.RecallHybridQualityOutcome
	var safetyStats loadquality.OperationStats
	var safetyOutcome loadquality.RecallVectorSafetyOutcome
	var paginationStats loadquality.OperationStats
	var paginationOutcome loadquality.RecallPaginationOutcome

	runWorkload := func(name string, fn func(context.Context) error) {
		t.Helper()
		workloadCtx, cancelWorkload := context.WithTimeout(ctx, memoryRecallLoadDeadline)
		defer cancelWorkload()
		if err := fn(workloadCtx); err != nil {
			t.Fatalf("%s workload: %v", name, err)
		}
	}
	runWorkload("cardinality ladder", func(workloadCtx context.Context) error {
		var workloadErr error
		tenants, cardinalityMeasurements, cardinalityOutcome, workloadErr = runMemoryRecallCardinalityLadder(
			workloadCtx, st, principals.ladder, opts,
		)
		return workloadErr
	})
	runWorkload("vector coverage", func(workloadCtx context.Context) error {
		var workloadErr error
		coverageMeasurements, coverageOutcome, workloadErr = runMemoryRecallVectorCoverage(
			workloadCtx, st, tenants[0], opts,
		)
		return workloadErr
	})
	runWorkload("hybrid relevance quality", func(workloadCtx context.Context) error {
		var workloadErr error
		qualityStats, qualityOutcome, workloadErr = runMemoryRecallHybridQuality(
			workloadCtx, st, principals.ladder[len(principals.ladder)-1], opts,
		)
		return workloadErr
	})
	runWorkload("vector safety", func(workloadCtx context.Context) error {
		var workloadErr error
		safetyStats, safetyOutcome, workloadErr = runMemoryRecallVectorSafety(
			workloadCtx, st, principals.ladder[len(principals.ladder)-1], principals.peer,
			principals.ladder[0], opts,
		)
		return workloadErr
	})
	runWorkload("pagination ordering", func(workloadCtx context.Context) error {
		var workloadErr error
		paginationStats, paginationOutcome, workloadErr = runMemoryRecallPagination(
			workloadCtx, st, tenants[len(tenants)-1], opts,
		)
		return workloadErr
	})

	result := loadquality.RecallResult{
		Schema:            loadquality.RecallResultSchemaV1,
		HarnessVersion:    loadquality.RecallHarnessVersion,
		StartedAt:         startedAt,
		CompletedAt:       time.Now().UTC(),
		Outcome:           "pass",
		PostgreSQLVersion: strings.TrimSpace(postgresVersion),
		Environment:       loadquality.RecallEnvironment(opts),
		Workload: loadquality.RecallWorkload{
			Seed: opts.Seed, SyntheticAccounts: len(opts.Cardinalities),
			SyntheticAgents: len(opts.Cardinalities) + 1,
			Cardinalities:   append([]int(nil), opts.Cardinalities...),
			QueryIterations: opts.QueryIterations, Concurrency: opts.Concurrency,
			VectorDimensions:    opts.VectorDimensions,
			CoveragePercentages: append([]int(nil), opts.CoveragePercentages...),
			PaginationLimit:     opts.PaginationLimit, ResultBudget: opts.ResultBudget,
		},
		Measurements: loadquality.RecallMeasurements{
			CardinalityLadder: cardinalityMeasurements,
			VectorCoverage:    coverageMeasurements,
			HybridQuality:     qualityStats,
			VectorSafety:      safetyStats,
			Pagination:        paginationStats,
		},
		Outcomes: loadquality.RecallOutcomes{
			CardinalityLadder: cardinalityOutcome,
			VectorCoverage:    coverageOutcome,
			HybridQuality:     qualityOutcome,
			VectorSafety:      safetyOutcome,
			Pagination:        paginationOutcome,
		},
	}
	raw, err := loadquality.WriteRecallResult(opts.ResultsPath, result)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("sanitized memory-recall load result written to %s", opts.ResultsPath)
	t.Logf("sanitized memory-recall load result:\n%s", raw)
}

func provisionMemoryRecallLoadPrincipals(
	ctx context.Context,
	st *Store,
	seed int64,
	ladderCount int,
) (memoryRecallLoadPrincipals, error) {
	if ladderCount < 1 {
		return memoryRecallLoadPrincipals{}, errors.New("memory recall load requires a cardinality tenant")
	}
	token := memoryRecallLoadToken(seed, "principals")
	type accountFixture struct {
		id    string
		realm Realm
	}
	accounts := make([]accountFixture, 0, ladderCount)
	for accountIndex := 0; accountIndex < ladderCount; accountIndex++ {
		provisioned, err := st.ProvisionAccount(
			ctx,
			fmt.Sprintf("memory-recall-load-%s-%d@example.invalid", token, accountIndex),
			fmt.Sprintf("memory recall load %s %d", token, accountIndex),
			time.Hour,
		)
		if err != nil {
			return memoryRecallLoadPrincipals{}, fmt.Errorf("provision synthetic recall account %d: %w", accountIndex, err)
		}
		activated, err := st.ActivateAccount(ctx, provisioned.AccountID)
		if err != nil {
			return memoryRecallLoadPrincipals{}, fmt.Errorf("activate synthetic recall account %d: %w", accountIndex, err)
		}
		if !activated {
			return memoryRecallLoadPrincipals{}, fmt.Errorf("activate synthetic recall account %d: account was not activated", accountIndex)
		}
		realm, err := st.CreateRealm(ctx, provisioned.AccountID, "recall-load")
		if err != nil {
			return memoryRecallLoadPrincipals{}, fmt.Errorf("create synthetic recall realm %d: %w", accountIndex, err)
		}
		accounts = append(accounts, accountFixture{id: provisioned.AccountID, realm: realm})
	}

	createPrincipal := func(account accountFixture, name string) (Principal, error) {
		agent, err := st.CreateAgent(ctx, account.id, account.realm.ID, name)
		if err != nil {
			return Principal{}, err
		}
		return Principal{
			Kind: PrincipalAgent, ID: agent.ID, AccountID: account.id,
			RealmID: account.realm.ID, AgentName: agent.Name, RealmName: account.realm.Name,
			AccountStatus: "active",
		}, nil
	}

	out := memoryRecallLoadPrincipals{ladder: make([]Principal, 0, ladderCount)}
	for tenantIndex := 0; tenantIndex < ladderCount; tenantIndex++ {
		principal, err := createPrincipal(
			accounts[tenantIndex],
			fmt.Sprintf("recall-cardinality-%02d-%s", tenantIndex, memoryRecallLoadToken(seed, "ladder", fmt.Sprint(tenantIndex))),
		)
		if err != nil {
			return memoryRecallLoadPrincipals{}, fmt.Errorf("create synthetic cardinality agent %d: %w", tenantIndex, err)
		}
		out.ladder = append(out.ladder, principal)
	}
	var err error
	out.peer, err = createPrincipal(accounts[len(accounts)-1], "recall-quality-peer-"+memoryRecallLoadToken(seed, "peer"))
	if err != nil {
		return memoryRecallLoadPrincipals{}, fmt.Errorf("create synthetic quality peer: %w", err)
	}
	return out, nil
}

func runMemoryRecallCardinalityLadder(
	ctx context.Context,
	st *Store,
	principals []Principal,
	opts loadquality.RecallOptions,
) ([]memoryRecallLoadTenant, []loadquality.RecallCardinalityMeasurement, loadquality.RecallCardinalityLadderOutcome, error) {
	if len(principals) != len(opts.Cardinalities) {
		return nil, nil, loadquality.RecallCardinalityLadderOutcome{}, errors.New("cardinality principal allocation mismatch")
	}
	tenants := make([]memoryRecallLoadTenant, 0, len(opts.Cardinalities))
	measurements := make([]loadquality.RecallCardinalityMeasurement, 0, len(opts.Cardinalities))
	seededMemories := 0
	recallCalls := 0
	allLexical := true
	for tenantIndex, memoryCount := range opts.Cardinalities {
		tenant := memoryRecallLoadTenant{
			principal: principals[tenantIndex],
			memories:  make([]Memory, 0, memoryCount),
		}
		for memoryIndex := 0; memoryIndex < memoryCount; memoryIndex++ {
			salience := memoryRecallLoadSalience(memoryIndex, memoryCount)
			captured, err := captureMemoryRecallLoadFixture(ctx, st, tenant.principal,
				fmt.Sprintf("ladder-%d-%d", tenantIndex, memoryIndex),
				fmt.Sprintf("Cardinality ladder beacon synthetic record %s.", memoryRecallLoadToken(opts.Seed, "content", fmt.Sprint(tenantIndex), fmt.Sprint(memoryIndex))),
				memoryRecallLoadKind, []string{memoryRecallLoadLadderTag}, salience, false,
			)
			if err != nil {
				return nil, nil, loadquality.RecallCardinalityLadderOutcome{}, fmt.Errorf("capture cardinality tenant %d memory %d: %w", tenantIndex, memoryIndex, err)
			}
			tenant.memories = append(tenant.memories, captured)
		}
		seededMemories += len(tenant.memories)
		tenants = append(tenants, tenant)

		durations, wall, lexical, err := runMemoryRecallConcurrent(
			ctx, opts.QueryIterations, opts.Concurrency, "lexical",
			func(callCtx context.Context) (MemoryRecallPage, error) {
				return st.RecallMemories(callCtx, tenant.principal, MemoryRecallOptions{
					Query: memoryRecallLoadQuery, Kind: memoryRecallLoadKind,
					Tags: []string{memoryRecallLoadLadderTag}, ExcludeSensitive: true,
					Limit: memoryRecallLoadSmaller(memoryRecallLoadProbeLimit, memoryCount),
				})
			},
			func(page MemoryRecallPage) error {
				if page.RetrievalMode != "lexical" || page.Degraded || page.VectorCoverage != 0 {
					return errors.New("lexical cardinality recall returned incomplete mode metadata")
				}
				// Every seeded ladder memory matches the fixed query, so a
				// healthy store returns exactly the requested page size; any
				// shortfall is a limit/materialization regression, not noise.
				if len(page.Hits) != memoryRecallLoadSmaller(memoryRecallLoadProbeLimit, memoryCount) {
					return fmt.Errorf(
						"lexical cardinality recall returned %d hits, want %d",
						len(page.Hits),
						memoryRecallLoadSmaller(memoryRecallLoadProbeLimit, memoryCount),
					)
				}
				for _, hit := range page.Hits {
					if hit.Score.Lexical <= 0 || !memoryRecallLoadLexicalScoreValid(hit.Score) {
						return errors.New("lexical cardinality recall returned an invalid score signal")
					}
				}
				return nil
			},
		)
		if err != nil {
			return nil, nil, loadquality.RecallCardinalityLadderOutcome{}, fmt.Errorf("recall cardinality tenant %d: %w", tenantIndex, err)
		}
		stats, err := loadquality.Summarize(durations, wall)
		if err != nil {
			return nil, nil, loadquality.RecallCardinalityLadderOutcome{}, fmt.Errorf("summarize cardinality tenant %d: %w", tenantIndex, err)
		}
		measurements = append(measurements, loadquality.RecallCardinalityMeasurement{
			MemoryCount: memoryCount, LexicalRecall: stats,
		})
		recallCalls += len(durations)
		allLexical = allLexical && lexical
	}
	outcome := loadquality.RecallCardinalityLadderOutcome{
		Tenants: len(tenants), SeededMemories: seededMemories,
		RecallCalls: recallCalls, AllLexical: allLexical,
	}
	outcome.AllComplete = len(measurements) == len(opts.Cardinalities) &&
		seededMemories == memoryRecallLoadSum(opts.Cardinalities) &&
		recallCalls == len(opts.Cardinalities)*opts.QueryIterations && allLexical
	if !outcome.AllComplete {
		return nil, nil, loadquality.RecallCardinalityLadderOutcome{}, errors.New("cardinality ladder did not complete its declared workload")
	}
	return tenants, measurements, outcome, nil
}

func runMemoryRecallVectorCoverage(
	ctx context.Context,
	st *Store,
	tenant memoryRecallLoadTenant,
	opts loadquality.RecallOptions,
) ([]loadquality.RecallVectorCoverageMeasurement, loadquality.RecallVectorCoverageOutcome, error) {
	eligibleMemories := len(tenant.memories)
	if eligibleMemories < 1 || eligibleMemories > memoryRecallLoadCandidateBudget {
		return nil, loadquality.RecallVectorCoverageOutcome{}, fmt.Errorf("coverage fixture cardinality must be 1-%d", memoryRecallLoadCandidateBudget)
	}
	measurements := make([]loadquality.RecallVectorCoverageMeasurement, 0, len(opts.CoveragePercentages))
	cases := make([]loadquality.RecallVectorCoverageCase, 0, len(opts.CoveragePercentages))
	createdProfiles := make(map[string]MemoryVectorProfile, len(opts.CoveragePercentages))
	for caseIndex, coveragePercent := range opts.CoveragePercentages {
		profile, err := createMemoryRecallLoadProfile(ctx, st, tenant.principal, opts,
			fmt.Sprintf("coverage-%03d", coveragePercent))
		if err != nil {
			return nil, loadquality.RecallVectorCoverageOutcome{}, fmt.Errorf("create %d percent coverage profile: %w", coveragePercent, err)
		}
		createdProfiles[profile.ID] = profile
		attachCount, err := loadquality.RecallCoverageCount(eligibleMemories, coveragePercent)
		if err != nil {
			return nil, loadquality.RecallVectorCoverageOutcome{}, fmt.Errorf("calculate %d percent vector coverage: %w", coveragePercent, err)
		}
		if attachCount < 1 {
			return nil, loadquality.RecallVectorCoverageOutcome{}, fmt.Errorf("%d percent coverage rounds to zero attached vectors", coveragePercent)
		}
		attachments := make([]memoryRecallLoadVectorAttachment, 0, attachCount)
		for memoryIndex := 0; memoryIndex < attachCount; memoryIndex++ {
			vector, vectorErr := memoryRecallLoadVector(
				opts.Seed, int64(10_000+caseIndex*eligibleMemories+memoryIndex), opts.VectorDimensions,
			)
			if vectorErr != nil {
				return nil, loadquality.RecallVectorCoverageOutcome{}, fmt.Errorf("generate %d percent coverage vector: %w", coveragePercent, vectorErr)
			}
			attachments = append(attachments, memoryRecallLoadVectorAttachment{
				memory: tenant.memories[memoryIndex],
				vector: vector,
			})
		}
		attachDurations, attachWall, err := putMemoryRecallLoadVectors(
			ctx, st, tenant.principal, profile, attachments, opts.Concurrency,
		)
		if err != nil {
			return nil, loadquality.RecallVectorCoverageOutcome{}, fmt.Errorf("attach %d percent coverage vectors: %w", coveragePercent, err)
		}
		attachStats, err := loadquality.Summarize(attachDurations, attachWall)
		if err != nil {
			return nil, loadquality.RecallVectorCoverageOutcome{}, fmt.Errorf("summarize %d percent vector attachment: %w", coveragePercent, err)
		}

		queryVector, err := memoryRecallLoadVector(opts.Seed, int64(20_000+caseIndex), opts.VectorDimensions)
		if err != nil {
			return nil, loadquality.RecallVectorCoverageOutcome{}, fmt.Errorf("generate %d percent query vector: %w", coveragePercent, err)
		}
		var observed MemoryRecallPage
		observedSet := false
		var observedMu sync.Mutex
		recallDurations, recallWall, hybrid, err := runMemoryRecallConcurrent(
			ctx, opts.QueryIterations, opts.Concurrency, "hybrid",
			func(callCtx context.Context) (MemoryRecallPage, error) {
				return st.RecallMemories(callCtx, tenant.principal, MemoryRecallOptions{
					Kind: memoryRecallLoadKind, Tags: []string{memoryRecallLoadLadderTag},
					ExcludeSensitive: true, VectorProfileID: profile.ID,
					QueryVector: queryVector,
					Limit:       memoryRecallLoadSmaller(memoryRecallLoadProbeLimit, eligibleMemories),
				})
			},
			func(page MemoryRecallPage) error {
				observedMu.Lock()
				defer observedMu.Unlock()
				expectedCoverage := float64(attachCount) / float64(eligibleMemories)
				if page.RetrievalMode != "hybrid" || page.VectorCandidates != eligibleMemories ||
					page.VectorMatches != attachCount || math.Abs(page.VectorCoverage-expectedCoverage) > 1e-12 ||
					page.CandidateTruncated || page.CandidateLimit != memoryRecallLoadCandidateBudget {
					return errors.New("vector coverage recall returned incorrect aggregate metadata")
				}
				if coveragePercent == 100 {
					if page.Degraded || page.DegradedReason != "" {
						return errors.New("full vector coverage unexpectedly degraded")
					}
				} else if !page.Degraded || page.DegradedReason != "partial_vector_coverage" {
					return errors.New("partial vector coverage did not report degradation")
				}
				// Metadata alone can survive a limit/assembly regression that
				// drops rows; every eligible memory matches the tag filter, so
				// the page must materialize exactly the requested size.
				if len(page.Hits) != memoryRecallLoadSmaller(memoryRecallLoadProbeLimit, eligibleMemories) {
					return fmt.Errorf(
						"vector coverage recall returned %d hits, want %d",
						len(page.Hits),
						memoryRecallLoadSmaller(memoryRecallLoadProbeLimit, eligibleMemories),
					)
				}
				for _, hit := range page.Hits {
					if !memoryRecallLoadHybridScoreValid(hit.Score) {
						return errors.New("vector coverage recall returned an invalid score signal")
					}
				}
				if !observedSet {
					observed = page
					observedSet = true
					return nil
				}
				if page.RetrievalMode != observed.RetrievalMode || page.Degraded != observed.Degraded ||
					page.DegradedReason != observed.DegradedReason || page.VectorCandidates != observed.VectorCandidates ||
					page.VectorMatches != observed.VectorMatches || page.VectorCoverage != observed.VectorCoverage ||
					page.CandidateLimit != observed.CandidateLimit || page.CandidateTruncated != observed.CandidateTruncated {
					return errors.New("vector coverage metadata changed across identical calls")
				}
				return nil
			},
		)
		if err != nil {
			return nil, loadquality.RecallVectorCoverageOutcome{}, fmt.Errorf("recall %d percent vector coverage: %w", coveragePercent, err)
		}
		recallStats, err := loadquality.Summarize(recallDurations, recallWall)
		if err != nil {
			return nil, loadquality.RecallVectorCoverageOutcome{}, fmt.Errorf("summarize %d percent hybrid recall: %w", coveragePercent, err)
		}
		observedMu.Lock()
		observedPage := observed
		hasObservation := observedSet
		observedMu.Unlock()
		if !hasObservation {
			return nil, loadquality.RecallVectorCoverageOutcome{}, errors.New("vector coverage produced no recall observation")
		}
		measurements = append(measurements, loadquality.RecallVectorCoverageMeasurement{
			CoveragePercent: coveragePercent, VectorAttach: attachStats, HybridRecall: recallStats,
		})
		cases = append(cases, loadquality.RecallVectorCoverageCase{
			CoveragePercent: coveragePercent, EligibleMemories: eligibleMemories,
			AttachedVectors: attachCount, RecallCalls: len(recallDurations),
			VectorCandidates: observedPage.VectorCandidates, VectorMatches: observedPage.VectorMatches,
			ReportedVectorCoverage: observedPage.VectorCoverage, Degraded: observedPage.Degraded,
			CandidateLimit: observedPage.CandidateLimit, CandidateTruncated: observedPage.CandidateTruncated,
			HybridUsed: hybrid, MetadataStable: true,
		})
	}
	profiles, err := st.ListMemoryVectorProfiles(ctx, tenant.principal)
	if err != nil {
		return nil, loadquality.RecallVectorCoverageOutcome{}, fmt.Errorf("list vector coverage profiles: %w", err)
	}
	listed := make(map[string]MemoryVectorProfile, len(profiles))
	for _, profile := range profiles {
		listed[profile.ID] = profile
	}
	allProfilesListed := len(createdProfiles) == len(opts.CoveragePercentages) &&
		len(listed) == len(createdProfiles)
	for profileID, created := range createdProfiles {
		listedProfile, ok := listed[profileID]
		if !ok || !memoryRecallLoadSameProfile(created, listedProfile) {
			allProfilesListed = false
		}
	}
	if !allProfilesListed {
		return nil, loadquality.RecallVectorCoverageOutcome{}, errors.New("created vector coverage profiles were not all listed")
	}
	return measurements, loadquality.RecallVectorCoverageOutcome{
		Cases: cases, AllProfilesListed: true,
	}, nil
}

type memoryRecallLoadQualityCase struct {
	name            string
	query           string
	tag             string
	target          Memory
	queryVector     []float64
	maximumRank     int
	expectedHits    int
	wantVectorUsed  bool
	wantLexicalUsed bool
	wantSimilarity  bool
}

func runMemoryRecallHybridQuality(
	ctx context.Context,
	st *Store,
	principal Principal,
	opts loadquality.RecallOptions,
) (loadquality.OperationStats, loadquality.RecallHybridQualityOutcome, error) {
	profile, err := createMemoryRecallLoadProfile(ctx, st, principal, opts, "quality")
	if err != nil {
		return loadquality.OperationStats{}, loadquality.RecallHybridQualityOutcome{}, fmt.Errorf("create quality vector profile: %w", err)
	}
	qualityCases, err := seedMemoryRecallHybridQuality(ctx, st, principal, profile, opts)
	if err != nil {
		return loadquality.OperationStats{}, loadquality.RecallHybridQualityOutcome{}, err
	}

	durations := make([]time.Duration, 0, len(qualityCases)*opts.QueryIterations)
	caseResults := make([]loadquality.RecallHybridRelevanceCase, 0, len(qualityCases))
	scoreComponentsVerified := true
	started := time.Now()
	for _, qualityCase := range qualityCases {
		observedRank := 0
		var observedHit MemoryRecallHit
		for iteration := 0; iteration < opts.QueryIterations; iteration++ {
			operationStarted := time.Now()
			page, recallErr := st.RecallMemories(ctx, principal, MemoryRecallOptions{
				Query: qualityCase.query, Tags: []string{qualityCase.tag},
				ExcludeSensitive: true, VectorProfileID: profile.ID,
				QueryVector: qualityCase.queryVector, Limit: 10,
			})
			durations = append(durations, time.Since(operationStarted))
			if recallErr != nil {
				return loadquality.OperationStats{}, loadquality.RecallHybridQualityOutcome{}, fmt.Errorf("quality case %s recall: %w", qualityCase.name, recallErr)
			}
			if page.RetrievalMode != "hybrid" {
				return loadquality.OperationStats{}, loadquality.RecallHybridQualityOutcome{}, fmt.Errorf("quality case %s did not use hybrid recall", qualityCase.name)
			}
			// Rank 1 on a page that lost its distractors proves nothing about
			// signal dominance; the seeded fixture count is exact, so the page
			// must contain every distractor alongside the target.
			if len(page.Hits) != qualityCase.expectedHits {
				return loadquality.OperationStats{}, loadquality.RecallHybridQualityOutcome{}, fmt.Errorf("quality case %s returned %d hits, want %d", qualityCase.name, len(page.Hits), qualityCase.expectedHits)
			}
			rank, hit := memoryRecallLoadRank(page, qualityCase.target.ID)
			if rank < 1 || rank > qualityCase.maximumRank {
				return loadquality.OperationStats{}, loadquality.RecallHybridQualityOutcome{}, fmt.Errorf("quality case %s observed rank %d, want 1..%d", qualityCase.name, rank, qualityCase.maximumRank)
			}
			if hit.Score.VectorUsed != qualityCase.wantVectorUsed ||
				(hit.Score.Lexical > 0) != qualityCase.wantLexicalUsed ||
				(hit.Score.Similarity != 0) != qualityCase.wantSimilarity {
				return loadquality.OperationStats{}, loadquality.RecallHybridQualityOutcome{}, fmt.Errorf("quality case %s returned unexpected target signals", qualityCase.name)
			}
			if !memoryRecallLoadHybridScoreValid(hit.Score) {
				return loadquality.OperationStats{}, loadquality.RecallHybridQualityOutcome{}, fmt.Errorf("quality case %s returned inconsistent score components", qualityCase.name)
			}
			for _, pageHit := range page.Hits {
				if !memoryRecallLoadHybridScoreValid(pageHit.Score) {
					return loadquality.OperationStats{}, loadquality.RecallHybridQualityOutcome{}, fmt.Errorf("quality case %s returned an inconsistent distractor score", qualityCase.name)
				}
			}
			if iteration == 0 {
				observedRank = rank
				observedHit = hit
			} else if rank != observedRank || hit.Score.VectorUsed != observedHit.Score.VectorUsed ||
				(hit.Score.Lexical > 0) != (observedHit.Score.Lexical > 0) ||
				(hit.Score.Similarity != 0) != (observedHit.Score.Similarity != 0) {
				return loadquality.OperationStats{}, loadquality.RecallHybridQualityOutcome{}, fmt.Errorf("quality case %s changed across identical calls", qualityCase.name)
			}
		}
		caseResults = append(caseResults, loadquality.RecallHybridRelevanceCase{
			Name: qualityCase.name, Passed: true, ObservedRank: observedRank,
			MaximumRank: qualityCase.maximumRank, VectorUsed: observedHit.Score.VectorUsed,
			LexicalUsed:    observedHit.Score.Lexical > 0,
			SimilarityUsed: observedHit.Score.Similarity != 0,
		})
	}
	wall := time.Since(started)
	stats, err := loadquality.Summarize(durations, wall)
	if err != nil {
		return loadquality.OperationStats{}, loadquality.RecallHybridQualityOutcome{}, err
	}
	outcome := loadquality.RecallHybridQualityOutcome{
		Cases: caseResults, RecallCalls: len(durations),
		ScoreComponentsVerified: scoreComponentsVerified, AllRanksPassed: true,
	}
	return stats, outcome, nil
}

func seedMemoryRecallHybridQuality(
	ctx context.Context,
	st *Store,
	principal Principal,
	profile MemoryVectorProfile,
	opts loadquality.RecallOptions,
) ([]memoryRecallLoadQualityCase, error) {
	const (
		vectorOnlyTag  = "quality_vector_only"
		lexicalOnlyTag = "quality_lexical_only"
		bothSignalsTag = "quality_both_signals"
	)
	queryA, err := memoryRecallLoadVector(opts.Seed, 30_001, opts.VectorDimensions)
	if err != nil {
		return nil, fmt.Errorf("generate vector-only query vector: %w", err)
	}
	targetA, err := captureMemoryRecallLoadFixture(ctx, st, principal, "quality-vector-target",
		"Remote cedar constellation archive.", "decision", []string{vectorOnlyTag}, 0.5, false)
	if err != nil {
		return nil, fmt.Errorf("capture vector-only target: %w", err)
	}
	distractorA, err := captureMemoryRecallLoadFixture(ctx, st, principal, "quality-vector-distractor",
		"Oblique lantern vector probe phrase.", "decision", []string{vectorOnlyTag}, 0.5, false)
	if err != nil {
		return nil, fmt.Errorf("capture vector-only distractor: %w", err)
	}
	if err := putMemoryRecallLoadVector(ctx, st, principal, profile, targetA, queryA); err != nil {
		return nil, fmt.Errorf("attach vector-only target: %w", err)
	}
	if err := putMemoryRecallLoadVector(ctx, st, principal, profile, distractorA, memoryRecallLoadNegated(queryA)); err != nil {
		return nil, fmt.Errorf("attach vector-only distractor: %w", err)
	}

	queryB, err := memoryRecallLoadVector(opts.Seed, 30_002, opts.VectorDimensions)
	if err != nil {
		return nil, fmt.Errorf("generate lexical-only query vector: %w", err)
	}
	targetB, err := captureMemoryRecallLoadFixture(ctx, st, principal, "quality-lexical-target",
		"Copper meadow lexical beacon.", "decision", []string{lexicalOnlyTag}, 0.5, false)
	if err != nil {
		return nil, fmt.Errorf("capture lexical-only target: %w", err)
	}
	distractorB, err := captureMemoryRecallLoadFixture(ctx, st, principal, "quality-lexical-distractor",
		"Remote opaque archive.", "decision", []string{lexicalOnlyTag}, 0.5, false)
	if err != nil {
		return nil, fmt.Errorf("capture lexical-only distractor: %w", err)
	}
	if err := putMemoryRecallLoadVector(ctx, st, principal, profile, distractorB, memoryRecallLoadNegated(queryB)); err != nil {
		return nil, fmt.Errorf("attach lexical-only distractor: %w", err)
	}

	queryC, err := memoryRecallLoadVector(opts.Seed, 30_003, opts.VectorDimensions)
	if err != nil {
		return nil, fmt.Errorf("generate both-signals query vector: %w", err)
	}
	targetC, err := captureMemoryRecallLoadFixture(ctx, st, principal, "quality-both-target",
		"Nimbus orchard hybrid beacon.", "decision", []string{bothSignalsTag}, 0.5, false)
	if err != nil {
		return nil, fmt.Errorf("capture both-signals target: %w", err)
	}
	lexicalDistractorC, err := captureMemoryRecallLoadFixture(ctx, st, principal, "quality-both-lexical-distractor",
		"Nimbus orchard hybrid beacon distractor.", "decision", []string{bothSignalsTag}, 0.5, false)
	if err != nil {
		return nil, fmt.Errorf("capture both-signals lexical distractor: %w", err)
	}
	vectorDistractorC, err := captureMemoryRecallLoadFixture(ctx, st, principal, "quality-both-vector-distractor",
		"Distant quartz registry.", "decision", []string{bothSignalsTag}, 0.5, false)
	if err != nil {
		return nil, fmt.Errorf("capture both-signals vector distractor: %w", err)
	}
	if err := putMemoryRecallLoadVector(ctx, st, principal, profile, targetC, queryC); err != nil {
		return nil, fmt.Errorf("attach both-signals target: %w", err)
	}
	if err := putMemoryRecallLoadVector(ctx, st, principal, profile, vectorDistractorC, queryC); err != nil {
		return nil, fmt.Errorf("attach both-signals vector distractor: %w", err)
	}
	_ = lexicalDistractorC // Its deliberate lack of a vector is part of the case.

	return []memoryRecallLoadQualityCase{
		{
			name: loadquality.RecallHybridCaseVectorOnly, query: "oblique lantern vector probe",
			tag: vectorOnlyTag, target: targetA, queryVector: queryA, maximumRank: 1, expectedHits: 2,
			wantVectorUsed: true, wantLexicalUsed: false, wantSimilarity: true,
		},
		{
			name: loadquality.RecallHybridCaseLexicalOnly, query: "copper meadow lexical beacon",
			tag: lexicalOnlyTag, target: targetB, queryVector: queryB, maximumRank: 1, expectedHits: 2,
			wantVectorUsed: false, wantLexicalUsed: true, wantSimilarity: false,
		},
		{
			name: loadquality.RecallHybridCaseBothSignals, query: "nimbus orchard hybrid beacon",
			tag: bothSignalsTag, target: targetC, queryVector: queryC, maximumRank: 1, expectedHits: 3,
			wantVectorUsed: true, wantLexicalUsed: true, wantSimilarity: true,
		},
	}, nil
}

func runMemoryRecallVectorSafety(
	ctx context.Context,
	st *Store,
	owner Principal,
	peer Principal,
	other Principal,
	opts loadquality.RecallOptions,
) (loadquality.OperationStats, loadquality.RecallVectorSafetyOutcome, error) {
	ownerProfile, err := createMemoryRecallLoadProfile(ctx, st, owner, opts, "safety")
	if err != nil {
		return loadquality.OperationStats{}, loadquality.RecallVectorSafetyOutcome{}, fmt.Errorf("create owner safety vector profile: %w", err)
	}
	peerProfile, err := createMemoryRecallLoadProfile(ctx, st, peer, opts, "safety")
	if err != nil {
		return loadquality.OperationStats{}, loadquality.RecallVectorSafetyOutcome{}, fmt.Errorf("create peer safety vector profile: %w", err)
	}
	otherProfile, err := createMemoryRecallLoadProfile(ctx, st, other, opts, "safety")
	if err != nil {
		return loadquality.OperationStats{}, loadquality.RecallVectorSafetyOutcome{}, fmt.Errorf("create other-tenant safety vector profile: %w", err)
	}
	if ownerProfile.ID == peerProfile.ID || ownerProfile.ID == otherProfile.ID || peerProfile.ID == otherProfile.ID {
		return loadquality.OperationStats{}, loadquality.RecallVectorSafetyOutcome{}, errors.New("owner-scoped safety profiles unexpectedly share an id")
	}

	const safetyTag = "quality_vector_safety"
	const safetyQuery = "private heliotrope vector safeguard"
	salience := 0.8
	sensitive, err := captureMemoryRecallLoadFixture(ctx, st, owner, "safety-sensitive",
		"Private heliotrope vector safeguard.", "decision", []string{safetyTag}, salience, true)
	if err != nil {
		return loadquality.OperationStats{}, loadquality.RecallVectorSafetyOutcome{}, fmt.Errorf("capture sensitive vector fixture: %w", err)
	}
	queryVector, err := memoryRecallLoadVector(opts.Seed, 40_001, opts.VectorDimensions)
	if err != nil {
		return loadquality.OperationStats{}, loadquality.RecallVectorSafetyOutcome{}, fmt.Errorf("generate safety query vector: %w", err)
	}
	if err := putMemoryRecallLoadVector(ctx, st, owner, ownerProfile, sensitive, queryVector); err != nil {
		return loadquality.OperationStats{}, loadquality.RecallVectorSafetyOutcome{}, fmt.Errorf("attach sensitive fixture vector: %w", err)
	}

	type safetyCall struct {
		principal Principal
		profile   MemoryVectorProfile
		include   bool
	}
	calls := []safetyCall{
		{principal: owner, profile: ownerProfile},
		{principal: owner, profile: ownerProfile, include: true},
		{principal: peer, profile: peerProfile},
		{principal: other, profile: otherProfile},
	}
	pages := make([]MemoryRecallPage, 0, len(calls))
	durations := make([]time.Duration, 0, len(calls))
	started := time.Now()
	for callIndex, call := range calls {
		operationStarted := time.Now()
		page, recallErr := st.RecallMemories(ctx, call.principal, MemoryRecallOptions{
			Query: safetyQuery, Tags: []string{safetyTag}, IncludeSensitive: call.include,
			VectorProfileID: call.profile.ID, QueryVector: queryVector, Limit: 10,
		})
		durations = append(durations, time.Since(operationStarted))
		if recallErr != nil {
			return loadquality.OperationStats{}, loadquality.RecallVectorSafetyOutcome{}, fmt.Errorf("vector safety recall %d: %w", callIndex, recallErr)
		}
		if page.RetrievalMode != "hybrid" {
			return loadquality.OperationStats{}, loadquality.RecallVectorSafetyOutcome{}, fmt.Errorf("vector safety recall %d did not use a supplied vector", callIndex)
		}
		pages = append(pages, page)
	}
	wall := time.Since(started)
	stats, err := loadquality.Summarize(durations, wall)
	if err != nil {
		return loadquality.OperationStats{}, loadquality.RecallVectorSafetyOutcome{}, err
	}
	if len(pages[0].Hits) != 1 || len(pages[1].Hits) != 1 {
		return loadquality.OperationStats{}, loadquality.RecallVectorSafetyOutcome{}, errors.New("owner safety recalls did not return exactly one fixture")
	}
	broad := pages[0].Hits[0].Memory
	exact := pages[1].Hits[0].Memory
	sensitiveBroadRedacted := broad.ID == sensitive.ID && broad.Sensitive && broad.Redacted &&
		broad.Content == "" && broad.ContentHash == "" && len(broad.Tags) == 0
	sensitiveExactOwnerVisible := exact.ID == sensitive.ID && exact.Sensitive && !exact.Redacted &&
		exact.Content == "Private heliotrope vector safeguard."
	crossAgentIsolated := len(pages[2].Hits) == 0
	crossAccountIsolated := len(pages[3].Hits) == 0
	if !sensitiveBroadRedacted || !sensitiveExactOwnerVisible || !crossAgentIsolated || !crossAccountIsolated {
		return loadquality.OperationStats{}, loadquality.RecallVectorSafetyOutcome{}, errors.New("vector safety redaction or isolation assertion failed")
	}
	return stats, loadquality.RecallVectorSafetyOutcome{
		RecallCalls: len(durations), SensitiveBroadRedacted: sensitiveBroadRedacted,
		SensitiveExactOwnerVisible: sensitiveExactOwnerVisible,
		CrossAgentIsolated:         crossAgentIsolated, CrossAccountIsolated: crossAccountIsolated,
		AllVectorQueries: true,
	}, nil
}

func runMemoryRecallPagination(
	ctx context.Context,
	st *Store,
	tenant memoryRecallLoadTenant,
	opts loadquality.RecallOptions,
) (loadquality.OperationStats, loadquality.RecallPaginationOutcome, error) {
	if len(tenant.memories) <= memoryRecallLoadCandidateBudget {
		return loadquality.OperationStats{}, loadquality.RecallPaginationOutcome{}, fmt.Errorf("pagination fixture must exceed %d memories", memoryRecallLoadCandidateBudget)
	}
	profile, err := createMemoryRecallLoadProfile(ctx, st, tenant.principal, opts, "pagination")
	if err != nil {
		return loadquality.OperationStats{}, loadquality.RecallPaginationOutcome{}, fmt.Errorf("create pagination vector profile: %w", err)
	}
	queryVector, err := memoryRecallLoadVector(opts.Seed, 50_001, opts.VectorDimensions)
	if err != nil {
		return loadquality.OperationStats{}, loadquality.RecallPaginationOutcome{}, fmt.Errorf("generate pagination query vector: %w", err)
	}
	attachments := make([]memoryRecallLoadVectorAttachment, 0, memoryRecallLoadCandidateBudget)
	for memoryIndex := 0; memoryIndex < memoryRecallLoadCandidateBudget; memoryIndex++ {
		attachments = append(attachments, memoryRecallLoadVectorAttachment{
			memory: tenant.memories[memoryIndex], vector: queryVector,
		})
	}
	if _, _, err := putMemoryRecallLoadVectors(ctx, st, tenant.principal, profile, attachments, opts.Concurrency); err != nil {
		return loadquality.OperationStats{}, loadquality.RecallPaginationOutcome{}, fmt.Errorf("attach pagination vectors: %w", err)
	}

	const repeatRuns = loadquality.DefaultRecallPaginationRepeats
	orders := make([][]string, 0, repeatRuns)
	pagesPerRun := make([]int, 0, repeatRuns)
	hitsPerRun := make([]int, 0, repeatRuns)
	durations := make([]time.Duration, 0, repeatRuns*((opts.ResultBudget+opts.PaginationLimit-1)/opts.PaginationLimit))
	pageLimitsHonored := true
	resultBudgetReached := true
	noDuplicateIDs := true
	candidateTruncated := true
	candidateLimit := 0
	vectorCandidates := 0
	vectorMatches := 0
	reportedVectorCoverage := 0.0
	started := time.Now()
	for runIndex := 0; runIndex < repeatRuns; runIndex++ {
		cursor := ""
		order := make([]string, 0, opts.ResultBudget)
		seen := make(map[string]struct{}, opts.ResultBudget)
		pageCount := 0
		for len(order) < opts.ResultBudget {
			remaining := opts.ResultBudget - len(order)
			requestLimit := memoryRecallLoadSmaller(opts.PaginationLimit, remaining)
			operationStarted := time.Now()
			page, recallErr := st.RecallMemories(ctx, tenant.principal, MemoryRecallOptions{
				Kind: memoryRecallLoadKind, Tags: []string{memoryRecallLoadLadderTag},
				ExcludeSensitive: true, VectorProfileID: profile.ID,
				QueryVector: queryVector, Limit: requestLimit, Cursor: cursor,
			})
			durations = append(durations, time.Since(operationStarted))
			if recallErr != nil {
				return loadquality.OperationStats{}, loadquality.RecallPaginationOutcome{}, fmt.Errorf("pagination run %d page %d: %w", runIndex, pageCount, recallErr)
			}
			pageCount++
			if page.RetrievalMode != "hybrid" || !page.Degraded ||
				page.DegradedReason != "candidate_budget_exceeded" || !page.CandidateTruncated ||
				page.CandidateLimit != memoryRecallLoadCandidateBudget ||
				page.VectorCandidates != memoryRecallLoadCandidateBudget ||
				page.VectorMatches != memoryRecallLoadCandidateBudget || page.VectorCoverage != 1 {
				return loadquality.OperationStats{}, loadquality.RecallPaginationOutcome{}, fmt.Errorf("pagination run %d page %d lost bounded-universe metadata", runIndex, pageCount)
			}
			candidateTruncated = candidateTruncated && page.CandidateTruncated
			if candidateLimit == 0 {
				candidateLimit = page.CandidateLimit
				vectorCandidates = page.VectorCandidates
				vectorMatches = page.VectorMatches
				reportedVectorCoverage = page.VectorCoverage
			} else if candidateLimit != page.CandidateLimit {
				return loadquality.OperationStats{}, loadquality.RecallPaginationOutcome{}, errors.New("pagination candidate limit changed across pages")
			} else if vectorCandidates != page.VectorCandidates || vectorMatches != page.VectorMatches ||
				reportedVectorCoverage != page.VectorCoverage {
				return loadquality.OperationStats{}, loadquality.RecallPaginationOutcome{}, errors.New("pagination vector metadata changed across pages")
			}
			if len(page.Hits) > requestLimit || (len(page.Hits) != requestLimit && len(order)+len(page.Hits) < opts.ResultBudget) {
				return loadquality.OperationStats{}, loadquality.RecallPaginationOutcome{}, errors.New("pagination page did not honor the requested limit")
			}
			for _, hit := range page.Hits {
				if _, duplicate := seen[hit.Memory.ID]; duplicate {
					return loadquality.OperationStats{}, loadquality.RecallPaginationOutcome{}, errors.New("pagination repeated an id within one run")
				}
				seen[hit.Memory.ID] = struct{}{}
				order = append(order, hit.Memory.ID)
			}
			if len(order) >= opts.ResultBudget {
				break
			}
			if page.NextCursor == "" {
				return loadquality.OperationStats{}, loadquality.RecallPaginationOutcome{}, errors.New("pagination exhausted before the declared result budget")
			}
			cursor = page.NextCursor
		}
		orders = append(orders, order)
		pagesPerRun = append(pagesPerRun, pageCount)
		hitsPerRun = append(hitsPerRun, len(order))
	}
	wall := time.Since(started)
	stats, err := loadquality.Summarize(durations, wall)
	if err != nil {
		return loadquality.OperationStats{}, loadquality.RecallPaginationOutcome{}, err
	}
	orderingStable := len(orders) == repeatRuns && memoryRecallLoadSameStrings(orders[0], orders[1])
	if !orderingStable {
		return loadquality.OperationStats{}, loadquality.RecallPaginationOutcome{}, errors.New("identical pagination queries returned different in-run id order")
	}
	expectedPages := (opts.ResultBudget + opts.PaginationLimit - 1) / opts.PaginationLimit
	for runIndex := range pagesPerRun {
		if pagesPerRun[runIndex] != expectedPages || hitsPerRun[runIndex] != opts.ResultBudget {
			return loadquality.OperationStats{}, loadquality.RecallPaginationOutcome{}, errors.New("pagination counters do not match the declared workload")
		}
	}
	return stats, loadquality.RecallPaginationOutcome{
		RepeatRuns: repeatRuns, PagesPerRun: pagesPerRun, HitsPerRun: hitsPerRun,
		RecallCalls: len(durations), ResultBudget: opts.ResultBudget,
		AttachedVectors:  memoryRecallLoadCandidateBudget,
		VectorCandidates: vectorCandidates, VectorMatches: vectorMatches,
		ReportedVectorCoverage: reportedVectorCoverage,
		TenantVectorFraction:   loadquality.RecallRatio(memoryRecallLoadCandidateBudget, len(tenant.memories)),
		CandidateLimit:         candidateLimit, CandidateTruncated: candidateTruncated,
		PageLimitsHonored: pageLimitsHonored, ResultBudgetReached: resultBudgetReached,
		NoDuplicateIDs: noDuplicateIDs, OrderingStable: orderingStable,
	}, nil
}

type memoryRecallLoadVectorAttachment struct {
	memory Memory
	vector []float64
}

func createMemoryRecallLoadProfile(
	ctx context.Context,
	st *Store,
	principal Principal,
	opts loadquality.RecallOptions,
	recipeVersion string,
) (MemoryVectorProfile, error) {
	return st.CreateMemoryVectorProfile(ctx, principal, CreateMemoryVectorProfileInput{
		Provider: "synthetic", Model: "sha256-expanded-unit", Recipe: "recall-load",
		RecipeVersion: recipeVersion, Dimensions: opts.VectorDimensions,
		DistanceMetric: MemoryVectorMetricCosine, Normalization: MemoryVectorNormalizationL2,
	})
}

func putMemoryRecallLoadVector(
	ctx context.Context,
	st *Store,
	principal Principal,
	profile MemoryVectorProfile,
	memory Memory,
	vector []float64,
) error {
	_, err := st.PutMemoryVector(ctx, principal, PutMemoryVectorInput{
		ProfileID: profile.ID, MemoryID: memory.ID, MemoryVersion: memory.Version,
		ContentHash: memory.ContentHash, Vector: vector,
	})
	return err
}

func putMemoryRecallLoadVectors(
	ctx context.Context,
	st *Store,
	principal Principal,
	profile MemoryVectorProfile,
	attachments []memoryRecallLoadVectorAttachment,
	concurrency int,
) ([]time.Duration, time.Duration, error) {
	if len(attachments) < 1 {
		return nil, 0, errors.New("vector attachment workload is empty")
	}
	tasks := make(chan memoryRecallLoadVectorAttachment, len(attachments))
	for _, attachment := range attachments {
		tasks <- attachment
	}
	close(tasks)
	type result struct {
		duration time.Duration
		err      error
	}
	results := make(chan result, len(attachments))
	workerCount := memoryRecallLoadSmaller(concurrency, len(attachments))
	var workers sync.WaitGroup
	started := time.Now()
	for workerIndex := 0; workerIndex < workerCount; workerIndex++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for attachment := range tasks {
				operationStarted := time.Now()
				err := putMemoryRecallLoadVector(ctx, st, principal, profile, attachment.memory, attachment.vector)
				results <- result{duration: time.Since(operationStarted), err: err}
			}
		}()
	}
	workers.Wait()
	wall := time.Since(started)
	close(results)
	durations := make([]time.Duration, 0, len(attachments))
	for item := range results {
		durations = append(durations, item.duration)
		if item.err != nil {
			return nil, 0, item.err
		}
	}
	if len(durations) != len(attachments) {
		return nil, 0, fmt.Errorf("vector attachment measurement count %d, want %d", len(durations), len(attachments))
	}
	return durations, wall, nil
}

func runMemoryRecallConcurrent(
	ctx context.Context,
	calls int,
	concurrency int,
	expectedMode string,
	recall func(context.Context) (MemoryRecallPage, error),
	validate func(MemoryRecallPage) error,
) ([]time.Duration, time.Duration, bool, error) {
	if calls < 1 || concurrency < 1 || expectedMode != "lexical" && expectedMode != "hybrid" {
		return nil, 0, false, errors.New("concurrent recall requires positive calls and concurrency")
	}
	tasks := make(chan struct{}, calls)
	for callIndex := 0; callIndex < calls; callIndex++ {
		tasks <- struct{}{}
	}
	close(tasks)
	type result struct {
		duration time.Duration
		mode     string
		err      error
	}
	results := make(chan result, calls)
	workerCount := memoryRecallLoadSmaller(concurrency, calls)
	var workers sync.WaitGroup
	started := time.Now()
	for workerIndex := 0; workerIndex < workerCount; workerIndex++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for range tasks {
				operationStarted := time.Now()
				page, err := recall(ctx)
				duration := time.Since(operationStarted)
				if err == nil {
					err = validate(page)
				}
				results <- result{duration: duration, mode: page.RetrievalMode, err: err}
			}
		}()
	}
	workers.Wait()
	wall := time.Since(started)
	close(results)
	durations := make([]time.Duration, 0, calls)
	allExpectedMode := true
	for item := range results {
		durations = append(durations, item.duration)
		if item.err != nil {
			return nil, 0, false, item.err
		}
		allExpectedMode = allExpectedMode && item.mode == expectedMode
	}
	if len(durations) != calls {
		return nil, 0, false, fmt.Errorf("recall measurement count %d, want %d", len(durations), calls)
	}
	return durations, wall, allExpectedMode, nil
}

func captureMemoryRecallLoadFixture(
	ctx context.Context,
	st *Store,
	principal Principal,
	key string,
	content string,
	kind string,
	tags []string,
	salience float64,
	sensitive bool,
) (Memory, error) {
	result, err := st.CaptureMemory(ctx, principal, CaptureMemoryInput{
		Content: content, Kind: kind, Tags: tags, Salience: &salience,
		Sensitive: sensitive, CaptureReason: "load_test",
		Evidence: []MemoryEvidenceInput{{
			ResolutionState:    MemoryEvidenceUnavailable,
			TerminalReasonCode: "synthetic_fixture",
		}},
		Client: MemoryClientProvenance{
			Runtime: "recall-load", Recipe: loadquality.RecallResultSchemaV1,
			RecipeVersion: loadquality.RecallHarnessVersion,
		},
		IdempotencyKey: "recall-load-" + memoryRecallLoadToken(0, key),
	})
	if err != nil {
		return Memory{}, err
	}
	return result.Memory, nil
}

// memoryRecallLoadVector delegates to the DB-free deterministic contract used
// by the result package. The helper expands binary SHA-256(seed,index,block)
// material and unit-normalizes it without a runtime RNG sequence.
func memoryRecallLoadVector(seed int64, index int64, dimensions int) ([]float64, error) {
	return loadquality.DeterministicVector(seed, index, dimensions)
}

func memoryRecallLoadNegated(vector []float64) []float64 {
	out := make([]float64, len(vector))
	for componentIndex, component := range vector {
		out[componentIndex] = -component
	}
	return out
}

func memoryRecallLoadSalience(index int, memoryCount int) float64 {
	headCount := memoryRecallLoadSmaller(memoryCount, memoryRecallLoadCandidateBudget)
	if index >= headCount {
		return 0.1
	}
	if headCount == 1 {
		return 1
	}
	return 1 - 0.4*float64(index)/float64(headCount-1)
}

func memoryRecallLoadLexicalScoreValid(score MemoryRecallScore) bool {
	want := 0.60*score.Lexical + 0.25*score.Salience + 0.15*score.Recency
	return !score.VectorUsed && score.Similarity == 0 &&
		!math.IsNaN(score.Total) && !math.IsInf(score.Total, 0) &&
		math.Abs(score.Total-want) <= 1e-12
}

func memoryRecallLoadHybridScoreValid(score MemoryRecallScore) bool {
	want := 0.50*score.Similarity + 0.30*score.Lexical +
		0.12*score.Salience + 0.08*score.Recency
	return !math.IsNaN(score.Total) && !math.IsInf(score.Total, 0) &&
		math.Abs(score.Total-want) <= 1e-12
}

func memoryRecallLoadRank(page MemoryRecallPage, memoryID string) (int, MemoryRecallHit) {
	for hitIndex, hit := range page.Hits {
		if hit.Memory.ID == memoryID {
			return hitIndex + 1, hit
		}
	}
	return 0, MemoryRecallHit{}
}

func memoryRecallLoadToken(seed int64, parts ...string) string {
	value := fmt.Sprintf("%d", seed)
	for _, part := range parts {
		value += "\x00" + part
	}
	digest := sha256.Sum256([]byte(value))
	return fmt.Sprintf("%x", digest[:6])
}

func memoryRecallLoadSmaller(left int, right int) int {
	if left < right {
		return left
	}
	return right
}

func memoryRecallLoadSum(values []int) int {
	total := 0
	for _, value := range values {
		total += value
	}
	return total
}

func memoryRecallLoadSameStrings(left []string, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func memoryRecallLoadSameProfile(left MemoryVectorProfile, right MemoryVectorProfile) bool {
	return left.ID == right.ID && left.Provider == right.Provider &&
		left.Model == right.Model && left.Recipe == right.Recipe &&
		left.RecipeVersion == right.RecipeVersion && left.Dimensions == right.Dimensions &&
		left.DistanceMetric == right.DistanceMetric && left.Normalization == right.Normalization &&
		left.ContractHash == right.ContractHash && left.CreatedAt.Equal(right.CreatedAt)
}
