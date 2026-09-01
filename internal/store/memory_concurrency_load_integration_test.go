package store

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/loadquality"
)

const (
	memoryConcurrencyLoadEnabled   = "WITSELF_MEMORY_CONCURRENCY_LOAD"
	memoryConcurrencyLoadSeedTag   = "concurrency_load_seed"
	memoryConcurrencyLoadMixedTag  = "concurrency_load_mixed"
	memoryConcurrencyLoadFanoutTag = "concurrency_load_fanout"
	memoryConcurrencyLoadKind      = "observation"
	// Setup and migration receive one additional non-scaling budget unit. The
	// four scaling phases each receive one unit per fixed eight-principal batch.
	memoryConcurrencyLoadSetupGuard = loadquality.ConcurrencyAgentBatchDeadline
)

var memoryConcurrencyLoadEmptyPlan = json.RawMessage(
	`{"schema":"witself.memory-plan.v1","draft_revision":1,"actions":[]}`,
)

type memoryConcurrencyLoadSeedMemory struct {
	memory  Memory
	marker  string
	content string
}

type memoryConcurrencyLoadPrincipal struct {
	index        int
	accountIndex int
	realmIndex   int
	agentIndex   int
	principal    Principal
	canary       string
	sensitiveTag string
	visible      []memoryConcurrencyLoadSeedMemory
	sensitive    Memory
	mixed        []Memory
}

type memoryConcurrencyLoadTiming struct {
	durations []time.Duration
	wall      time.Duration
}

func (timing *memoryConcurrencyLoadTiming) addPhase(durations []time.Duration, wall time.Duration) {
	timing.durations = append(timing.durations, durations...)
	timing.wall += wall
}

func (timing memoryConcurrencyLoadTiming) summarize() (loadquality.OperationStats, error) {
	return loadquality.Summarize(timing.durations, timing.wall)
}

type memoryConcurrencyLoadCurationCursor struct {
	sourceKind    string
	streamID      string
	expectedPrior int64
	upper         int64
}

type memoryConcurrencyLoadCurationJob struct {
	fixture      *memoryConcurrencyLoadPrincipal
	request      MemoryCurationRequest
	started      StartMemoryCurationResult
	cursor       memoryConcurrencyLoadCurationCursor
	planRevision int64
	planHash     string
}

type memoryConcurrencyLoadCursorKey struct {
	accountID string
	realmID   string
	ownerID   string
	source    string
	streamID  string
}

// TestNarrativeMemoryConcurrencyLoadPostgres is the fifth opt-in executable
// production-readiness slice for narrative memory. It creates a complete
// account/realm/agent grid in one fresh disposable schema, measures mixed-owner
// operation overlap sampled by isolation probes, races curation claims, and
// fans one sensitive query across the fleet.
//
// Every fixture is derived locally from one signed 64-bit seed. The harness
// invokes no AI, model, embedding provider, runtime client, MCP, credential,
// secret, or sealed-plane surface.
func TestNarrativeMemoryConcurrencyLoadPostgres(t *testing.T) {
	if os.Getenv(memoryConcurrencyLoadEnabled) != "1" {
		t.Skip(memoryConcurrencyLoadEnabled + "=1 is required")
	}
	dsn := strings.TrimSpace(os.Getenv("WITSELF_TEST_DATABASE_URL"))
	if dsn == "" {
		t.Fatal("WITSELF_TEST_DATABASE_URL is required when memory concurrency load testing is enabled")
	}
	opts, err := loadquality.ParseConcurrencyOptions(os.Getenv)
	if err != nil {
		t.Fatal(err)
	}
	principalCount, err := loadquality.ConcurrencyPrincipalCount(
		opts.Accounts, opts.RealmsPerAccount, opts.AgentsPerRealm,
	)
	if err != nil {
		t.Fatal(err)
	}
	phaseDeadline, err := loadquality.ConcurrencyPhaseDeadline(principalCount)
	if err != nil {
		t.Fatal(err)
	}
	// There are exactly four scaling phases below. The overall guard is their
	// sum plus one setup/migration unit; it is never a flat loop-wide timeout.
	overallDeadline := 4*phaseDeadline + memoryConcurrencyLoadSetupGuard
	startedAt := time.Now().UTC()
	ctx, cancel := context.WithTimeout(context.Background(), overallDeadline)
	defer cancel()

	st, _ := newMigrationTestStore(t, dsn)
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}
	var postgresVersion string
	if err := st.pool.QueryRow(ctx, `SHOW server_version`).Scan(&postgresVersion); err != nil {
		t.Fatalf("read PostgreSQL version: %v", err)
	}

	runPhase := func(name string, fn func(context.Context) error) {
		t.Helper()
		phaseCtx, cancelPhase := context.WithTimeout(ctx, phaseDeadline)
		defer cancelPhase()
		if phaseErr := fn(phaseCtx); phaseErr != nil {
			t.Fatalf("%s workload: %v", name, phaseErr)
		}
	}

	var fixtures []*memoryConcurrencyLoadPrincipal
	var seedStats loadquality.OperationStats
	var topologyOutcome loadquality.ConcurrencyTopologyOutcome
	runPhase("topology and seed", func(phaseCtx context.Context) error {
		var phaseErr error
		fixtures, seedStats, topologyOutcome, phaseErr = provisionMemoryConcurrencyLoadTopology(
			phaseCtx, st, opts,
		)
		return phaseErr
	})
	if len(fixtures) != principalCount {
		t.Fatalf("topology returned %d principals, want %d", len(fixtures), principalCount)
	}

	var mixedCaptureStats loadquality.OperationStats
	var mixedRecallStats loadquality.OperationStats
	var mixedAdjustStats loadquality.OperationStats
	var isolationStats loadquality.OperationStats
	var mixedOutcome loadquality.ConcurrencyMixedOperationsOutcome
	var isolationOutcome loadquality.ConcurrencyIsolationOutcome
	runPhase("mixed operations and isolation", func(phaseCtx context.Context) error {
		var phaseErr error
		mixedCaptureStats, mixedRecallStats, mixedAdjustStats, isolationStats,
			mixedOutcome, isolationOutcome, phaseErr = runMemoryConcurrencyMixedIsolation(
			phaseCtx, st, fixtures, opts,
		)
		return phaseErr
	})

	var curationRequestStats loadquality.OperationStats
	var curationClaimStats loadquality.OperationStats
	var curationApplyStats loadquality.OperationStats
	var curationOutcome loadquality.ConcurrencyCurationClaimsOutcome
	runPhase("curation claims and cursor isolation", func(phaseCtx context.Context) error {
		var phaseErr error
		curationRequestStats, curationClaimStats, curationApplyStats,
			curationOutcome, phaseErr = runMemoryConcurrencyCuration(
			phaseCtx, st, fixtures, opts,
		)
		return phaseErr
	})

	var sensitiveFanoutStats loadquality.OperationStats
	var sensitiveFanoutOutcome loadquality.ConcurrencySensitiveFanoutOutcome
	runPhase("sensitive fanout", func(phaseCtx context.Context) error {
		var phaseErr error
		sensitiveFanoutStats, sensitiveFanoutOutcome, phaseErr = runMemoryConcurrencySensitiveFanout(
			phaseCtx, st, fixtures, opts,
		)
		return phaseErr
	})

	result := loadquality.ConcurrencyResult{
		Schema:            loadquality.ConcurrencyResultSchemaV1,
		HarnessVersion:    loadquality.ConcurrencyHarnessVersion,
		StartedAt:         startedAt,
		CompletedAt:       time.Now().UTC(),
		Outcome:           "pass",
		PostgreSQLVersion: strings.TrimSpace(postgresVersion),
		Environment:       loadquality.ConcurrencyEnvironment(opts),
		Workload: loadquality.ConcurrencyWorkload{
			Seed: opts.Seed, SyntheticAccounts: opts.Accounts,
			RealmsPerAccount: opts.RealmsPerAccount, AgentsPerRealm: opts.AgentsPerRealm,
			SyntheticRealms:     opts.Accounts * opts.RealmsPerAccount,
			SyntheticPrincipals: principalCount, SeedMemoriesPerAgent: opts.SeedMemoriesPerAgent,
			WorkersPerAgent: opts.WorkersPerAgent, OperationsPerWorker: opts.OperationsPerWorker,
			IsolationIterations: opts.IsolationIterations, ClaimWorkers: opts.ClaimWorkers,
		},
		Measurements: loadquality.ConcurrencyMeasurements{
			Seed: seedStats, MixedCapture: mixedCaptureStats, MixedRecall: mixedRecallStats,
			MixedAdjust: mixedAdjustStats, IsolationProbe: isolationStats,
			CurationRequest: curationRequestStats, CurationClaim: curationClaimStats,
			CurationApply: curationApplyStats, SensitiveFanout: sensitiveFanoutStats,
		},
		Outcomes: loadquality.ConcurrencyOutcomes{
			Topology: topologyOutcome, MixedOperations: mixedOutcome,
			Isolation: isolationOutcome, CurationClaims: curationOutcome,
			SensitiveFanout: sensitiveFanoutOutcome,
		},
	}
	raw, err := loadquality.WriteConcurrencyResult(opts.ResultsPath, result)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("sanitized memory-concurrency load result written to %s", opts.ResultsPath)
	t.Logf("sanitized memory-concurrency load result:\n%s", raw)
}

func provisionMemoryConcurrencyLoadTopology(
	ctx context.Context,
	st *Store,
	opts loadquality.ConcurrencyOptions,
) ([]*memoryConcurrencyLoadPrincipal, loadquality.OperationStats, loadquality.ConcurrencyTopologyOutcome, error) {
	principalCount, err := loadquality.ConcurrencyPrincipalCount(
		opts.Accounts, opts.RealmsPerAccount, opts.AgentsPerRealm,
	)
	if err != nil {
		return nil, loadquality.OperationStats{}, loadquality.ConcurrencyTopologyOutcome{}, err
	}
	fixtures := make([]*memoryConcurrencyLoadPrincipal, 0, principalCount)
	fleetToken := memoryConcurrencyLoadToken(opts.Seed, "fleet")
	for accountIndex := 0; accountIndex < opts.Accounts; accountIndex++ {
		provisioned, provisionErr := st.ProvisionAccount(
			ctx,
			fmt.Sprintf("memory-concurrency-%s-%d@example.invalid", fleetToken, accountIndex),
			fmt.Sprintf("memory concurrency %s %d", fleetToken, accountIndex),
			time.Hour,
		)
		if provisionErr != nil {
			return nil, loadquality.OperationStats{}, loadquality.ConcurrencyTopologyOutcome{},
				fmt.Errorf("provision synthetic account %d: %w", accountIndex, provisionErr)
		}
		activated, activateErr := st.ActivateAccount(ctx, provisioned.AccountID)
		if activateErr != nil {
			return nil, loadquality.OperationStats{}, loadquality.ConcurrencyTopologyOutcome{},
				fmt.Errorf("activate synthetic account %d: %w", accountIndex, activateErr)
		}
		if !activated {
			return nil, loadquality.OperationStats{}, loadquality.ConcurrencyTopologyOutcome{},
				fmt.Errorf("activate synthetic account %d returned false", accountIndex)
		}
		for realmIndex := 0; realmIndex < opts.RealmsPerAccount; realmIndex++ {
			realm, realmErr := st.CreateRealm(
				ctx, provisioned.AccountID,
				fmt.Sprintf("concurrency-r%02d-%s", realmIndex,
					memoryConcurrencyLoadToken(opts.Seed, "realm", fmt.Sprint(accountIndex), fmt.Sprint(realmIndex))),
			)
			if realmErr != nil {
				return nil, loadquality.OperationStats{}, loadquality.ConcurrencyTopologyOutcome{},
					fmt.Errorf("create synthetic realm %d/%d: %w", accountIndex, realmIndex, realmErr)
			}
			for agentIndex := 0; agentIndex < opts.AgentsPerRealm; agentIndex++ {
				agent, agentErr := st.CreateAgent(
					ctx, provisioned.AccountID, realm.ID,
					fmt.Sprintf("concurrency-a%02d-%s", agentIndex,
						memoryConcurrencyLoadToken(opts.Seed, "agent", fmt.Sprint(accountIndex), fmt.Sprint(realmIndex), fmt.Sprint(agentIndex))),
				)
				if agentErr != nil {
					return nil, loadquality.OperationStats{}, loadquality.ConcurrencyTopologyOutcome{},
						fmt.Errorf("create synthetic agent %d/%d/%d: %w", accountIndex, realmIndex, agentIndex, agentErr)
				}
				fixtureIndex := len(fixtures)
				fixtures = append(fixtures, &memoryConcurrencyLoadPrincipal{
					index: fixtureIndex, accountIndex: accountIndex, realmIndex: realmIndex,
					agentIndex: agentIndex,
					principal: Principal{
						Kind: PrincipalAgent, ID: agent.ID, AccountID: provisioned.AccountID,
						RealmID: realm.ID, AgentName: agent.Name, RealmName: realm.Name,
						AccountStatus: "active",
					},
					canary:       "pcanary" + memoryConcurrencyLoadToken(opts.Seed, "canary", fmt.Sprint(fixtureIndex)),
					sensitiveTag: "sensitive" + memoryConcurrencyLoadToken(opts.Seed, "sensitive", fmt.Sprint(fixtureIndex)),
					visible:      make([]memoryConcurrencyLoadSeedMemory, 0, opts.SeedMemoriesPerAgent),
					mixed:        make([]Memory, 0, opts.WorkersPerAgent*opts.OperationsPerWorker),
				})
			}
		}
	}
	if len(fixtures) != principalCount {
		return nil, loadquality.OperationStats{}, loadquality.ConcurrencyTopologyOutcome{},
			fmt.Errorf("created %d principals, want %d", len(fixtures), principalCount)
	}

	broadMarker := "broad" + memoryConcurrencyLoadToken(opts.Seed, "broad")
	canaries := make(map[string]struct{}, principalCount)
	sensitiveMarkers := make(map[string]struct{}, principalCount)
	memoryMarkers := make(map[string]struct{}, principalCount*opts.SeedMemoriesPerAgent)
	seedDurations := make([]time.Duration, 0, principalCount*(opts.SeedMemoriesPerAgent+1))
	seedWall := time.Duration(0)
	for _, fixture := range fixtures {
		if _, duplicate := canaries[fixture.canary]; duplicate {
			return nil, loadquality.OperationStats{}, loadquality.ConcurrencyTopologyOutcome{},
				fmt.Errorf("principal %d has a duplicate canary", fixture.index)
		}
		canaries[fixture.canary] = struct{}{}
		if _, duplicate := sensitiveMarkers[fixture.sensitiveTag]; duplicate {
			return nil, loadquality.OperationStats{}, loadquality.ConcurrencyTopologyOutcome{},
				fmt.Errorf("principal %d has a duplicate sensitive marker", fixture.index)
		}
		sensitiveMarkers[fixture.sensitiveTag] = struct{}{}
		for memoryIndex := 0; memoryIndex < opts.SeedMemoriesPerAgent; memoryIndex++ {
			marker := "memory" + memoryConcurrencyLoadToken(
				opts.Seed, "seed", fmt.Sprint(fixture.index), fmt.Sprint(memoryIndex),
			)
			if _, duplicate := memoryMarkers[marker]; duplicate {
				return nil, loadquality.OperationStats{}, loadquality.ConcurrencyTopologyOutcome{},
					fmt.Errorf("principal %d memory %d has a duplicate canary marker", fixture.index, memoryIndex)
			}
			memoryMarkers[marker] = struct{}{}
			content := strings.Join([]string{broadMarker, fixture.canary, marker}, " ")
			salience := float64((memoryIndex%8)+1) / 16
			captured, duration, captureErr := captureMemoryConcurrencyLoadFixture(
				ctx, st, fixture.principal, content, []string{memoryConcurrencyLoadSeedTag},
				salience, false,
				"seed-"+memoryConcurrencyLoadToken(opts.Seed, fmt.Sprint(fixture.index), fmt.Sprint(memoryIndex)),
				nil,
			)
			if captureErr != nil {
				return nil, loadquality.OperationStats{}, loadquality.ConcurrencyTopologyOutcome{},
					fmt.Errorf("seed principal %d memory %d: %w", fixture.index, memoryIndex, captureErr)
			}
			seedDurations = append(seedDurations, duration)
			seedWall += duration
			if err := validateMemoryConcurrencyLoadValue(captured, fixture.principal, content, false); err != nil {
				return nil, loadquality.OperationStats{}, loadquality.ConcurrencyTopologyOutcome{},
					fmt.Errorf("seed principal %d memory %d: %w", fixture.index, memoryIndex, err)
			}
			fixture.visible = append(fixture.visible, memoryConcurrencyLoadSeedMemory{
				memory: captured, marker: marker, content: content,
			})
		}
		sensitiveContent := strings.Join([]string{broadMarker, fixture.canary, fixture.sensitiveTag}, " ")
		captured, duration, captureErr := captureMemoryConcurrencyLoadFixture(
			ctx, st, fixture.principal, sensitiveContent, []string{memoryConcurrencyLoadSeedTag},
			0.75, true,
			"sensitive-"+memoryConcurrencyLoadToken(opts.Seed, fmt.Sprint(fixture.index)),
			nil,
		)
		if captureErr != nil {
			return nil, loadquality.OperationStats{}, loadquality.ConcurrencyTopologyOutcome{},
				fmt.Errorf("seed principal %d sensitive memory: %w", fixture.index, captureErr)
		}
		seedDurations = append(seedDurations, duration)
		seedWall += duration
		if err := validateMemoryConcurrencyLoadValue(captured, fixture.principal, sensitiveContent, true); err != nil {
			return nil, loadquality.OperationStats{}, loadquality.ConcurrencyTopologyOutcome{},
				fmt.Errorf("seed principal %d sensitive memory: %w", fixture.index, err)
		}
		fixture.sensitive = captured
	}
	seedStats, err := loadquality.Summarize(seedDurations, seedWall)
	if err != nil {
		return nil, loadquality.OperationStats{}, loadquality.ConcurrencyTopologyOutcome{}, err
	}
	canaryCount := principalCount * opts.SeedMemoriesPerAgent
	if len(canaries) != principalCount || len(sensitiveMarkers) != principalCount ||
		len(memoryMarkers) != canaryCount {
		return nil, loadquality.OperationStats{}, loadquality.ConcurrencyTopologyOutcome{},
			fmt.Errorf("unique marker counts principal=%d sensitive=%d memory=%d",
				len(canaries), len(sensitiveMarkers), len(memoryMarkers))
	}
	return fixtures, seedStats, loadquality.ConcurrencyTopologyOutcome{
		Accounts: opts.Accounts, Realms: opts.Accounts * opts.RealmsPerAccount,
		Principals: principalCount, CanaryMemories: canaryCount,
		SensitiveMemories: principalCount, SeededMemories: canaryCount + principalCount,
		AllPrincipalsSeeded: true, AllCanariesUnique: true, AllSensitiveSeeded: true,
	}, nil
}

type memoryConcurrencyLoadMixedWorkerResult struct {
	principalIndex   int
	workerIndex      int
	completed        int
	ownerChecks      int
	recallHits       int
	captureDurations []time.Duration
	recallDurations  []time.Duration
	adjustDurations  []time.Duration
	captures         []Memory
	err              error
}

type memoryConcurrencyLoadIsolationResult struct {
	principalIndex           int
	probeRounds              int
	broadCalls               int
	broadHits                int
	broadVisible             int
	broadSensitiveRedactions int
	ownCalls                 int
	ownHits                  int
	crossAccountCalls        int
	crossRealmCalls          int
	crossAgentCalls          int
	markerScans              int
	durations                []time.Duration
	err                      error
}

func runMemoryConcurrencyMixedIsolation(
	ctx context.Context,
	st *Store,
	fixtures []*memoryConcurrencyLoadPrincipal,
	opts loadquality.ConcurrencyOptions,
) (
	loadquality.OperationStats,
	loadquality.OperationStats,
	loadquality.OperationStats,
	loadquality.OperationStats,
	loadquality.ConcurrencyMixedOperationsOutcome,
	loadquality.ConcurrencyIsolationOutcome,
	error,
) {
	workerCount := len(fixtures) * opts.WorkersPerAgent
	mixedCalls := workerCount * opts.OperationsPerWorker
	probeCount := len(fixtures)
	ready := make(chan struct{}, workerCount+probeCount)
	start := make(chan struct{})
	probeBegun := make(chan struct{}, probeCount)
	probesBegun := make(chan struct{})
	mixedResults := make(chan memoryConcurrencyLoadMixedWorkerResult, workerCount)
	mixedOperationsDone := make(chan time.Time, workerCount)
	isolationResults := make(chan memoryConcurrencyLoadIsolationResult, probeCount)
	var startOnce sync.Once
	releaseStart := func() {
		startOnce.Do(func() { close(start) })
	}
	defer releaseStart()
	var probesBegunOnce sync.Once
	releaseProbesBegun := func() {
		probesBegunOnce.Do(func() { close(probesBegun) })
	}
	defer releaseProbesBegun()

	var mixedInFlight atomic.Int64
	var overlapSamples atomic.Int64
	var phaseReleased atomic.Bool
	var startedParticipants atomic.Int64
	var prematureStarts atomic.Int64
	markParticipantStarted := func() {
		if !phaseReleased.Load() {
			prematureStarts.Add(1)
		}
		startedParticipants.Add(1)
	}

	var mixedWorkers sync.WaitGroup
	for _, fixture := range fixtures {
		for workerIndex := 0; workerIndex < opts.WorkersPerAgent; workerIndex++ {
			mixedWorkers.Add(1)
			go func(f *memoryConcurrencyLoadPrincipal, worker int) {
				defer mixedWorkers.Done()
				ready <- struct{}{}
				<-start
				markParticipantStarted()
				<-probesBegun
				workerResult := runMemoryConcurrencyLoadMixedWorker(
					ctx, st, f, worker, opts, &mixedInFlight,
				)
				mixedResults <- workerResult
				mixedOperationsDone <- time.Now()
			}(fixture, workerIndex)
		}
	}

	var isolationWorkers sync.WaitGroup
	for _, fixture := range fixtures {
		isolationWorkers.Add(1)
		go func(f *memoryConcurrencyLoadPrincipal) {
			defer isolationWorkers.Done()
			ready <- struct{}{}
			<-start
			probeBegun <- struct{}{}
			markParticipantStarted()
			isolationResults <- runMemoryConcurrencyLoadIsolationProbe(
				ctx, st, fixtures, f, opts, &mixedInFlight, &overlapSamples,
			)
		}(fixture)
	}

	participantCount := workerCount + probeCount
	readyParticipants := 0
	for readyParticipants < participantCount {
		select {
		case <-ready:
			readyParticipants++
		case <-ctx.Done():
			return loadquality.OperationStats{}, loadquality.OperationStats{},
				loadquality.OperationStats{}, loadquality.OperationStats{},
				loadquality.ConcurrencyMixedOperationsOutcome{}, loadquality.ConcurrencyIsolationOutcome{},
				fmt.Errorf("wait for whole-fleet start gate: %w", ctx.Err())
		}
	}
	wholeFleetStartSynchronized := readyParticipants == participantCount && startedParticipants.Load() == 0
	if !wholeFleetStartSynchronized {
		return loadquality.OperationStats{}, loadquality.OperationStats{},
			loadquality.OperationStats{}, loadquality.OperationStats{},
			loadquality.ConcurrencyMixedOperationsOutcome{}, loadquality.ConcurrencyIsolationOutcome{},
			fmt.Errorf("whole-fleet start gate ready/started = %d/%d, want %d/0",
				readyParticipants, startedParticipants.Load(), participantCount)
	}
	phaseStarted := time.Now()
	phaseReleased.Store(true)
	releaseStart()
	for begun := 0; begun < probeCount; begun++ {
		select {
		case <-probeBegun:
		case <-ctx.Done():
			return loadquality.OperationStats{}, loadquality.OperationStats{},
				loadquality.OperationStats{}, loadquality.OperationStats{},
				loadquality.ConcurrencyMixedOperationsOutcome{}, loadquality.ConcurrencyIsolationOutcome{},
				fmt.Errorf("wait for isolation probes-begun rendezvous: %w", ctx.Err())
		}
	}
	releaseProbesBegun()

	isolationWorkers.Wait()
	isolationWall := time.Since(phaseStarted)
	close(isolationResults)
	mixedWorkers.Wait()
	close(mixedResults)
	close(mixedOperationsDone)
	if startedParticipants.Load() != int64(participantCount) || prematureStarts.Load() != 0 {
		return loadquality.OperationStats{}, loadquality.OperationStats{},
			loadquality.OperationStats{}, loadquality.OperationStats{},
			loadquality.ConcurrencyMixedOperationsOutcome{}, loadquality.ConcurrencyIsolationOutcome{},
			fmt.Errorf("whole-fleet start completion started/premature = %d/%d, want %d/0",
				startedParticipants.Load(), prematureStarts.Load(), participantCount)
	}
	latestMixedCompletion := phaseStarted
	for completedAt := range mixedOperationsDone {
		if completedAt.After(latestMixedCompletion) {
			latestMixedCompletion = completedAt
		}
	}
	mixedWall := latestMixedCompletion.Sub(phaseStarted)

	captureDurations := make([]time.Duration, 0, mixedCalls)
	recallDurations := make([]time.Duration, 0, mixedCalls)
	adjustDurations := make([]time.Duration, 0, mixedCalls)
	completed := 0
	ownerChecks := 0
	recallHits := 0
	workerResults := 0
	var firstErr error
	for workerResult := range mixedResults {
		workerResults++
		if workerResult.err != nil && firstErr == nil {
			firstErr = fmt.Errorf("mixed principal %d worker %d: %w",
				workerResult.principalIndex, workerResult.workerIndex, workerResult.err)
		}
		completed += workerResult.completed
		ownerChecks += workerResult.ownerChecks
		recallHits += workerResult.recallHits
		captureDurations = append(captureDurations, workerResult.captureDurations...)
		recallDurations = append(recallDurations, workerResult.recallDurations...)
		adjustDurations = append(adjustDurations, workerResult.adjustDurations...)
		fixtures[workerResult.principalIndex].mixed = append(
			fixtures[workerResult.principalIndex].mixed, workerResult.captures...,
		)
	}
	if firstErr != nil {
		return loadquality.OperationStats{}, loadquality.OperationStats{},
			loadquality.OperationStats{}, loadquality.OperationStats{},
			loadquality.ConcurrencyMixedOperationsOutcome{}, loadquality.ConcurrencyIsolationOutcome{}, firstErr
	}
	if workerResults != workerCount || completed != mixedCalls || ownerChecks != mixedCalls ||
		recallHits != mixedCalls || len(captureDurations) != mixedCalls ||
		len(recallDurations) != mixedCalls || len(adjustDurations) != mixedCalls {
		return loadquality.OperationStats{}, loadquality.OperationStats{},
			loadquality.OperationStats{}, loadquality.OperationStats{},
			loadquality.ConcurrencyMixedOperationsOutcome{}, loadquality.ConcurrencyIsolationOutcome{},
			fmt.Errorf("mixed completion mismatch workers=%d/%d batches=%d/%d", workerResults,
				workerCount, completed, mixedCalls)
	}
	for _, fixture := range fixtures {
		expectedMixed := opts.WorkersPerAgent * opts.OperationsPerWorker
		if len(fixture.mixed) != expectedMixed {
			return loadquality.OperationStats{}, loadquality.OperationStats{},
				loadquality.OperationStats{}, loadquality.OperationStats{},
				loadquality.ConcurrencyMixedOperationsOutcome{}, loadquality.ConcurrencyIsolationOutcome{},
				fmt.Errorf("principal %d retained %d mixed fixtures, want %d", fixture.index,
					len(fixture.mixed), expectedMixed)
		}
	}

	isolationDurations := make([]time.Duration, 0,
		len(fixtures)*opts.IsolationIterations*loadquality.ConcurrencyIsolationCallsPerProbeRound)
	probeResults := 0
	probeRounds := 0
	broadCalls := 0
	broadHits := 0
	broadVisible := 0
	broadRedactions := 0
	ownCalls := 0
	ownHits := 0
	crossAccountCalls := 0
	crossRealmCalls := 0
	crossAgentCalls := 0
	markerScans := 0
	for probeResult := range isolationResults {
		probeResults++
		if probeResult.err != nil && firstErr == nil {
			firstErr = fmt.Errorf("isolation principal %d: %w", probeResult.principalIndex, probeResult.err)
		}
		probeRounds += probeResult.probeRounds
		broadCalls += probeResult.broadCalls
		broadHits += probeResult.broadHits
		broadVisible += probeResult.broadVisible
		broadRedactions += probeResult.broadSensitiveRedactions
		ownCalls += probeResult.ownCalls
		ownHits += probeResult.ownHits
		crossAccountCalls += probeResult.crossAccountCalls
		crossRealmCalls += probeResult.crossRealmCalls
		crossAgentCalls += probeResult.crossAgentCalls
		markerScans += probeResult.markerScans
		isolationDurations = append(isolationDurations, probeResult.durations...)
	}
	if firstErr != nil {
		return loadquality.OperationStats{}, loadquality.OperationStats{},
			loadquality.OperationStats{}, loadquality.OperationStats{},
			loadquality.ConcurrencyMixedOperationsOutcome{}, loadquality.ConcurrencyIsolationOutcome{}, firstErr
	}
	wantProbeRounds := len(fixtures) * opts.IsolationIterations
	wantProbeCalls := wantProbeRounds * loadquality.ConcurrencyIsolationCallsPerProbeRound
	if probeResults != probeCount || probeRounds != wantProbeRounds ||
		len(isolationDurations) != wantProbeCalls {
		return loadquality.OperationStats{}, loadquality.OperationStats{},
			loadquality.OperationStats{}, loadquality.OperationStats{},
			loadquality.ConcurrencyMixedOperationsOutcome{}, loadquality.ConcurrencyIsolationOutcome{},
			fmt.Errorf("isolation completion mismatch probes=%d/%d rounds=%d/%d calls=%d/%d",
				probeResults, probeCount, probeRounds, wantProbeRounds, len(isolationDurations), wantProbeCalls)
	}
	overlapOperationSamples := int(overlapSamples.Load())
	if overlapOperationSamples < 0 || overlapOperationSamples > wantProbeCalls {
		return loadquality.OperationStats{}, loadquality.OperationStats{},
			loadquality.OperationStats{}, loadquality.OperationStats{},
			loadquality.ConcurrencyMixedOperationsOutcome{}, loadquality.ConcurrencyIsolationOutcome{},
			fmt.Errorf("mixed-operation overlap samples = %d, want 0..%d",
				overlapOperationSamples, wantProbeCalls)
	}
	if opts.IsolationIterations >= 2 && overlapOperationSamples < 1 {
		return loadquality.OperationStats{}, loadquality.OperationStats{},
			loadquality.OperationStats{}, loadquality.OperationStats{},
			loadquality.ConcurrencyMixedOperationsOutcome{}, loadquality.ConcurrencyIsolationOutcome{},
			fmt.Errorf("mixed-operation overlap samples = %d, want at least 1 for %d isolation iterations",
				overlapOperationSamples, opts.IsolationIterations)
	}

	captureStats, err := loadquality.Summarize(captureDurations, mixedWall)
	if err != nil {
		return loadquality.OperationStats{}, loadquality.OperationStats{},
			loadquality.OperationStats{}, loadquality.OperationStats{},
			loadquality.ConcurrencyMixedOperationsOutcome{}, loadquality.ConcurrencyIsolationOutcome{}, err
	}
	recallStats, err := loadquality.Summarize(recallDurations, mixedWall)
	if err != nil {
		return loadquality.OperationStats{}, loadquality.OperationStats{},
			loadquality.OperationStats{}, loadquality.OperationStats{},
			loadquality.ConcurrencyMixedOperationsOutcome{}, loadquality.ConcurrencyIsolationOutcome{}, err
	}
	adjustStats, err := loadquality.Summarize(adjustDurations, mixedWall)
	if err != nil {
		return loadquality.OperationStats{}, loadquality.OperationStats{},
			loadquality.OperationStats{}, loadquality.OperationStats{},
			loadquality.ConcurrencyMixedOperationsOutcome{}, loadquality.ConcurrencyIsolationOutcome{}, err
	}
	isolationStats, err := loadquality.Summarize(isolationDurations, isolationWall)
	if err != nil {
		return loadquality.OperationStats{}, loadquality.OperationStats{},
			loadquality.OperationStats{}, loadquality.OperationStats{},
			loadquality.ConcurrencyMixedOperationsOutcome{}, loadquality.ConcurrencyIsolationOutcome{}, err
	}

	mixedOutcome := loadquality.ConcurrencyMixedOperationsOutcome{
		Workers: workerCount, OperationBatches: mixedCalls,
		CaptureCalls: mixedCalls, RecallCalls: mixedCalls, AdjustCalls: mixedCalls,
		RecallHits: recallHits, OwnerChecks: ownerChecks, ForeignHits: 0,
		OverlapOperationSamples: overlapOperationSamples,
		ExactRecallValues:       true, ExactAdjustValues: true, AllHitsExactOwner: true,
		AllOperationsComplete: true, WholeFleetStartSynchronized: wholeFleetStartSynchronized,
	}
	isolationOutcome := loadquality.ConcurrencyIsolationOutcome{
		ProbeAgents: len(fixtures), ProbeRounds: probeRounds,
		BroadRecallCalls: broadCalls, BroadHits: broadHits,
		BroadVisibleCanaries: broadVisible, BroadSensitiveRedactions: broadRedactions,
		OwnControlRecallCalls: ownCalls, OwnControlHits: ownHits,
		CrossAccountRecallCalls: crossAccountCalls, CrossRealmRecallCalls: crossRealmCalls,
		CrossAgentRecallCalls: crossAgentCalls, MarkerScans: markerScans,
		ForeignHits: 0, ForeignCanaryHits: 0, SensitiveContentHits: 0,
		BroadCountsExact: true, OwnCountsExact: true, AllHitsExactOwner: true,
		NoForeignCanaries: true, NoSensitiveContent: true,
		CrossAccountIsolated: true, CrossRealmIsolated: true, CrossAgentIsolated: true,
	}
	return captureStats, recallStats, adjustStats, isolationStats, mixedOutcome, isolationOutcome, nil
}

func runMemoryConcurrencyLoadMixedWorker(
	ctx context.Context,
	st *Store,
	fixture *memoryConcurrencyLoadPrincipal,
	workerIndex int,
	opts loadquality.ConcurrencyOptions,
	mixedInFlight *atomic.Int64,
) memoryConcurrencyLoadMixedWorkerResult {
	out := memoryConcurrencyLoadMixedWorkerResult{
		principalIndex: fixture.index, workerIndex: workerIndex,
		captureDurations: make([]time.Duration, 0, opts.OperationsPerWorker),
		recallDurations:  make([]time.Duration, 0, opts.OperationsPerWorker),
		adjustDurations:  make([]time.Duration, 0, opts.OperationsPerWorker),
		captures:         make([]Memory, 0, opts.OperationsPerWorker),
	}
	for operationIndex := 0; operationIndex < opts.OperationsPerWorker; operationIndex++ {
		marker := "mixed" + memoryConcurrencyLoadToken(
			opts.Seed, fmt.Sprint(fixture.index), fmt.Sprint(workerIndex), fmt.Sprint(operationIndex),
		)
		captured, captureDuration, err := captureMemoryConcurrencyLoadFixture(
			ctx, st, fixture.principal, marker, []string{memoryConcurrencyLoadMixedTag},
			0.5, false,
			"mixed-"+memoryConcurrencyLoadToken(opts.Seed, fmt.Sprint(fixture.index),
				fmt.Sprint(workerIndex), fmt.Sprint(operationIndex)),
			mixedInFlight,
		)
		out.captureDurations = append(out.captureDurations, captureDuration)
		if err != nil {
			out.err = fmt.Errorf("capture operation %d: %w", operationIndex, err)
			return out
		}
		if err := validateMemoryConcurrencyLoadValue(captured, fixture.principal, marker, false); err != nil {
			out.err = fmt.Errorf("capture operation %d: %w", operationIndex, err)
			return out
		}
		recallOptions := MemoryRecallOptions{
			Query: marker, Tags: []string{memoryConcurrencyLoadMixedTag}, Limit: 2,
		}
		recallStarted := time.Now()
		mixedInFlight.Add(1)
		page, err := st.RecallMemories(ctx, fixture.principal, recallOptions)
		mixedInFlight.Add(-1)
		out.recallDurations = append(out.recallDurations, time.Since(recallStarted))
		if err != nil {
			out.err = fmt.Errorf("recall operation %d: %w", operationIndex, err)
			return out
		}
		if err := validateMemoryConcurrencyLoadLexicalPage(page); err != nil {
			out.err = fmt.Errorf("recall operation %d: %w", operationIndex, err)
			return out
		}
		if len(page.Hits) != 1 || page.NextCursor != "" {
			out.err = fmt.Errorf("recall operation %d returned %d hits, want exactly 1",
				operationIndex, len(page.Hits))
			return out
		}
		hit := page.Hits[0].Memory
		if !memoryConcurrencyLoadExactRecallMatches(hit, captured) ||
			!memoryConcurrencyLoadOwnerMatches(hit, fixture.principal) {
			out.err = fmt.Errorf("recall operation %d returned a non-exact owner value", operationIndex)
			return out
		}
		out.recallHits++
		out.ownerChecks++

		salience := float64(16+(operationIndex%32)) / 64
		adjustInput := AdjustMemoryInput{
			ExpectedVersion: captured.Version, Salience: &salience,
			IdempotencyKey: "concurrency-adjust-" + memoryConcurrencyLoadToken(
				opts.Seed, fmt.Sprint(fixture.index), fmt.Sprint(workerIndex), fmt.Sprint(operationIndex),
			),
		}
		adjustStarted := time.Now()
		mixedInFlight.Add(1)
		adjusted, err := st.AdjustMemory(ctx, fixture.principal, captured.ID, adjustInput)
		mixedInFlight.Add(-1)
		out.adjustDurations = append(out.adjustDurations, time.Since(adjustStarted))
		if err != nil {
			out.err = fmt.Errorf("adjust operation %d: %w", operationIndex, err)
			return out
		}
		if adjusted.Memory.ID != captured.ID || adjusted.Memory.Version != captured.Version+1 ||
			adjusted.Memory.Content != marker || adjusted.Memory.Salience != salience ||
			adjusted.Memory.Sensitive || adjusted.Memory.Redacted ||
			!memoryConcurrencyLoadOwnerMatches(adjusted.Memory, fixture.principal) {
			out.err = fmt.Errorf("adjust operation %d returned a non-exact value", operationIndex)
			return out
		}
		out.captures = append(out.captures, adjusted.Memory)
		out.completed++
	}
	return out
}

func runMemoryConcurrencyLoadIsolationProbe(
	ctx context.Context,
	st *Store,
	fixtures []*memoryConcurrencyLoadPrincipal,
	caller *memoryConcurrencyLoadPrincipal,
	opts loadquality.ConcurrencyOptions,
	mixedInFlight *atomic.Int64,
	overlapSamples *atomic.Int64,
) memoryConcurrencyLoadIsolationResult {
	out := memoryConcurrencyLoadIsolationResult{
		principalIndex: caller.index,
		durations: make([]time.Duration, 0,
			opts.IsolationIterations*loadquality.ConcurrencyIsolationCallsPerProbeRound),
	}
	targets := []*memoryConcurrencyLoadPrincipal{
		memoryConcurrencyLoadFixtureAt(fixtures, opts,
			(caller.accountIndex+1)%opts.Accounts, caller.realmIndex, caller.agentIndex),
		memoryConcurrencyLoadFixtureAt(fixtures, opts,
			caller.accountIndex, (caller.realmIndex+1)%opts.RealmsPerAccount, caller.agentIndex),
		memoryConcurrencyLoadFixtureAt(fixtures, opts,
			caller.accountIndex, caller.realmIndex, (caller.agentIndex+1)%opts.AgentsPerRealm),
	}
	for iteration := 0; iteration < opts.IsolationIterations; iteration++ {
		broadOptions := MemoryRecallOptions{
			Query: "broad" + memoryConcurrencyLoadToken(opts.Seed, "broad"),
			Tags:  []string{memoryConcurrencyLoadSeedTag},
			Limit: opts.SeedMemoriesPerAgent + 1,
		}
		broadStarted := time.Now()
		memoryConcurrencyLoadSampleOverlap(mixedInFlight, overlapSamples)
		broad, err := st.RecallMemories(ctx, caller.principal, broadOptions)
		out.durations = append(out.durations, time.Since(broadStarted))
		out.broadCalls++
		if err != nil {
			out.err = fmt.Errorf("broad recall iteration %d: %w", iteration, err)
			return out
		}
		if err := validateMemoryConcurrencyLoadLexicalPage(broad); err != nil {
			out.err = fmt.Errorf("broad recall iteration %d: %w", iteration, err)
			return out
		}
		visible, redactions, scans, err := validateMemoryConcurrencyLoadBroadRecall(
			broad, fixtures, caller, opts.SeedMemoriesPerAgent,
		)
		if err != nil {
			out.err = fmt.Errorf("broad recall iteration %d: %w", iteration, err)
			return out
		}
		out.broadHits += len(broad.Hits)
		out.broadVisible += visible
		out.broadSensitiveRedactions += redactions
		out.markerScans += scans

		for dimension, target := range targets {
			expected := target.visible[0]
			ownerOptions := MemoryRecallOptions{
				Query: expected.marker, Tags: []string{memoryConcurrencyLoadSeedTag}, Limit: 2,
			}
			ownerStarted := time.Now()
			memoryConcurrencyLoadSampleOverlap(mixedInFlight, overlapSamples)
			ownerPage, ownerErr := st.RecallMemories(ctx, target.principal, ownerOptions)
			out.durations = append(out.durations, time.Since(ownerStarted))
			out.ownCalls++
			if ownerErr != nil {
				out.err = fmt.Errorf("dimension %d owner control iteration %d: %w", dimension, iteration, ownerErr)
				return out
			}
			if err := validateMemoryConcurrencyLoadLexicalPage(ownerPage); err != nil {
				out.err = fmt.Errorf("dimension %d owner control iteration %d: %w", dimension, iteration, err)
				return out
			}
			if err := validateMemoryConcurrencyLoadControlRecall(ownerPage, fixtures, target, expected); err != nil {
				out.err = fmt.Errorf("dimension %d owner control iteration %d: %w", dimension, iteration, err)
				return out
			}
			out.ownHits++
			out.markerScans++

			foreignOptions := MemoryRecallOptions{
				Query: expected.marker, Tags: []string{memoryConcurrencyLoadSeedTag}, Limit: 2,
			}
			foreignStarted := time.Now()
			memoryConcurrencyLoadSampleOverlap(mixedInFlight, overlapSamples)
			foreignPage, foreignErr := st.RecallMemories(ctx, caller.principal, foreignOptions)
			out.durations = append(out.durations, time.Since(foreignStarted))
			if foreignErr != nil {
				out.err = fmt.Errorf("dimension %d foreign probe iteration %d: %w", dimension, iteration, foreignErr)
				return out
			}
			if err := validateMemoryConcurrencyLoadLexicalPage(foreignPage); err != nil {
				out.err = fmt.Errorf("dimension %d foreign probe iteration %d: %w", dimension, iteration, err)
				return out
			}
			if len(foreignPage.Hits) != 0 || foreignPage.NextCursor != "" {
				out.err = fmt.Errorf("dimension %d foreign probe iteration %d returned %d rows, want 0",
					dimension, iteration, len(foreignPage.Hits))
				return out
			}
			switch dimension {
			case 0:
				out.crossAccountCalls++
			case 1:
				out.crossRealmCalls++
			case 2:
				out.crossAgentCalls++
			}
		}
		out.probeRounds++
	}
	return out
}

func memoryConcurrencyLoadSampleOverlap(mixedInFlight, overlapSamples *atomic.Int64) {
	if mixedInFlight.Load() > 0 {
		overlapSamples.Add(1)
	}
}

func validateMemoryConcurrencyLoadBroadRecall(
	page MemoryRecallPage,
	fixtures []*memoryConcurrencyLoadPrincipal,
	caller *memoryConcurrencyLoadPrincipal,
	visibleCount int,
) (int, int, int, error) {
	if len(page.Hits) != visibleCount+1 || page.NextCursor != "" {
		return 0, 0, 0, fmt.Errorf("returned %d rows, want exactly %d", len(page.Hits), visibleCount+1)
	}
	expected := make(map[string]memoryConcurrencyLoadSeedMemory, visibleCount)
	for _, seeded := range caller.visible {
		expected[seeded.memory.ID] = seeded
	}
	seen := make(map[string]struct{}, visibleCount+1)
	visible := 0
	redactions := 0
	for _, hit := range page.Hits {
		memory := hit.Memory
		if _, duplicate := seen[memory.ID]; duplicate {
			return 0, 0, 0, errors.New("broad recall returned a duplicate memory")
		}
		seen[memory.ID] = struct{}{}
		if !memoryConcurrencyLoadOwnerMatches(memory, caller.principal) {
			return 0, 0, 0, errors.New("broad recall returned a foreign owner")
		}
		if memory.ID == caller.sensitive.ID {
			if memory.Version != caller.sensitive.Version || memory.Kind != caller.sensitive.Kind ||
				memory.Salience != caller.sensitive.Salience || memory.State != caller.sensitive.State ||
				!memory.Sensitive || !memory.Redacted || memory.Content != "" ||
				memory.ContentHash != "" || len(memory.Tags) != 0 || len(memory.Links) != 0 ||
				memory.CaptureReason != "" || memory.LifecycleReason != "" ||
				memory.OccurredFrom != nil || memory.OccurredUntil != nil ||
				memory.IdempotencyKey != "" || memory.RequestHash != "" ||
				memory.Client != (MemoryClientProvenance{}) || len(memory.Evidence) != 0 {
				return 0, 0, 0, errors.New("broad recall sensitive row was not fully redacted")
			}
			redactions++
		} else {
			seeded, ok := expected[memory.ID]
			if !ok || !memoryConcurrencyLoadVisibleRecallMatches(memory, seeded) {
				return 0, 0, 0, errors.New("broad recall returned a non-exact visible row")
			}
			visible++
		}
		if err := scanMemoryConcurrencyLoadMarkers(memory.Content, fixtures, caller.index); err != nil {
			return 0, 0, 0, err
		}
	}
	if visible != visibleCount || redactions != 1 || len(seen) != visibleCount+1 {
		return 0, 0, 0, fmt.Errorf("visible/redacted rows = %d/%d, want %d/1", visible, redactions, visibleCount)
	}
	return visible, redactions, len(page.Hits), nil
}

func validateMemoryConcurrencyLoadControlRecall(
	page MemoryRecallPage,
	fixtures []*memoryConcurrencyLoadPrincipal,
	target *memoryConcurrencyLoadPrincipal,
	expected memoryConcurrencyLoadSeedMemory,
) error {
	if len(page.Hits) != 1 || page.NextCursor != "" {
		return fmt.Errorf("returned %d rows, want exactly 1", len(page.Hits))
	}
	memory := page.Hits[0].Memory
	if !memoryConcurrencyLoadVisibleRecallMatches(memory, expected) ||
		!memoryConcurrencyLoadOwnerMatches(memory, target.principal) {
		return errors.New("returned a non-exact target-owner row")
	}
	return scanMemoryConcurrencyLoadMarkers(memory.Content, fixtures, target.index)
}

func memoryConcurrencyLoadVisibleRecallMatches(
	memory Memory,
	expected memoryConcurrencyLoadSeedMemory,
) bool {
	return memory.Content == expected.content &&
		memoryConcurrencyLoadExactRecallMatches(memory, expected.memory)
}

func memoryConcurrencyLoadExactRecallMatches(actual Memory, expected Memory) bool {
	// Recall intentionally omits evidence payloads. Every other returned field,
	// including ids, owner coordinates, version/change sequence, content/hash,
	// tags, state, provenance, and timestamps, must match the exact fixture.
	expected.Evidence = nil
	return reflect.DeepEqual(actual, expected)
}

func validateMemoryConcurrencyLoadLexicalPage(page MemoryRecallPage) error {
	if page.RetrievalMode != "lexical" || page.Degraded || page.VectorCoverage != 0 ||
		page.VectorProfileID != "" || page.VectorCandidates != 0 || page.VectorMatches != 0 {
		return errors.New("recall returned non-exact lexical mode metadata")
	}
	return nil
}

func scanMemoryConcurrencyLoadMarkers(
	content string,
	fixtures []*memoryConcurrencyLoadPrincipal,
	expectedOwner int,
) error {
	for _, fixture := range fixtures {
		if fixture.index != expectedOwner && strings.Contains(content, fixture.canary) {
			return errors.New("returned content contains a foreign principal canary")
		}
		if strings.Contains(content, fixture.sensitiveTag) {
			return errors.New("returned content contains sensitive content")
		}
	}
	return nil
}

func memoryConcurrencyLoadFixtureAt(
	fixtures []*memoryConcurrencyLoadPrincipal,
	opts loadquality.ConcurrencyOptions,
	accountIndex int,
	realmIndex int,
	agentIndex int,
) *memoryConcurrencyLoadPrincipal {
	index := (accountIndex*opts.RealmsPerAccount+realmIndex)*opts.AgentsPerRealm + agentIndex
	return fixtures[index]
}

type memoryConcurrencyLoadRequestResult struct {
	principalIndex int
	request        RequestMemoryCurationResult
	duration       time.Duration
	err            error
}

type memoryConcurrencyLoadForeignClaimResult struct {
	requestIndex int
	dimension    int
	duration     time.Duration
	err          error
}

type memoryConcurrencyLoadOwnerClaimResult struct {
	requestIndex int
	workerIndex  int
	started      StartMemoryCurationResult
	duration     time.Duration
	err          error
}

func runMemoryConcurrencyCuration(
	ctx context.Context,
	st *Store,
	fixtures []*memoryConcurrencyLoadPrincipal,
	opts loadquality.ConcurrencyOptions,
) (
	loadquality.OperationStats,
	loadquality.OperationStats,
	loadquality.OperationStats,
	loadquality.ConcurrencyCurationClaimsOutcome,
	error,
) {
	principalCount := len(fixtures)
	maximumMemories := opts.SeedMemoriesPerAgent + opts.WorkersPerAgent*opts.OperationsPerWorker
	requestResults := make(chan memoryConcurrencyLoadRequestResult, principalCount)
	requestStart := make(chan struct{})
	var requestWorkers sync.WaitGroup
	for _, fixture := range fixtures {
		requestWorkers.Add(1)
		go func(f *memoryConcurrencyLoadPrincipal) {
			defer requestWorkers.Done()
			<-requestStart
			operationStarted := time.Now()
			requested, err := st.RequestCuration(ctx, f.principal, RequestMemoryCurationInput{
				Scope: MemoryCurationScope{
					Sources:      []string{MemoryCurationSourceMemory},
					MemoryStates: []string{MemoryStateActive},
					MaxMemories:  maximumMemories,
				},
				CoalescingKey: "concurrency_load", TriggerReason: "load_test",
				IdempotencyKey: "concurrency-request-" +
					memoryConcurrencyLoadToken(opts.Seed, fmt.Sprint(f.index)),
			})
			requestResults <- memoryConcurrencyLoadRequestResult{
				principalIndex: f.index, request: requested,
				duration: time.Since(operationStarted), err: err,
			}
		}(fixture)
	}
	requestWallStarted := time.Now()
	close(requestStart)
	requestWorkers.Wait()
	requestWall := time.Since(requestWallStarted)
	close(requestResults)

	requestDurations := make([]time.Duration, 0, principalCount)
	jobs := make([]memoryConcurrencyLoadCurationJob, principalCount)
	for item := range requestResults {
		requestDurations = append(requestDurations, item.duration)
		if item.err != nil {
			return memoryConcurrencyLoadEmptyCurationResult(
				fmt.Errorf("request principal %d: %w", item.principalIndex, item.err),
			)
		}
		fixture := fixtures[item.principalIndex]
		if err := validateMemoryConcurrencyLoadCurationRequest(
			item.request, fixture.principal, maximumMemories,
		); err != nil {
			return memoryConcurrencyLoadEmptyCurationResult(
				fmt.Errorf("request principal %d: %w", item.principalIndex, err),
			)
		}
		jobs[item.principalIndex] = memoryConcurrencyLoadCurationJob{
			fixture: fixture, request: item.request.Request,
		}
	}
	if len(requestDurations) != principalCount {
		return memoryConcurrencyLoadEmptyCurationResult(
			fmt.Errorf("curation request measurements = %d, want %d", len(requestDurations), principalCount),
		)
	}
	requestStats, err := loadquality.Summarize(requestDurations, requestWall)
	if err != nil {
		return memoryConcurrencyLoadEmptyCurationResult(err)
	}

	claimTiming := memoryConcurrencyLoadTiming{}
	foreignAttempts := principalCount * loadquality.ConcurrencyForeignClaimProbesPerRequest
	foreignResults := make(chan memoryConcurrencyLoadForeignClaimResult, foreignAttempts)
	foreignStart := make(chan struct{})
	var foreignWorkers sync.WaitGroup
	for requestIndex := range jobs {
		target := jobs[requestIndex].fixture
		attackers := []*memoryConcurrencyLoadPrincipal{
			memoryConcurrencyLoadFixtureAt(fixtures, opts,
				(target.accountIndex+1)%opts.Accounts, target.realmIndex, target.agentIndex),
			memoryConcurrencyLoadFixtureAt(fixtures, opts,
				target.accountIndex, (target.realmIndex+1)%opts.RealmsPerAccount, target.agentIndex),
			memoryConcurrencyLoadFixtureAt(fixtures, opts,
				target.accountIndex, target.realmIndex, (target.agentIndex+1)%opts.AgentsPerRealm),
		}
		for dimension, attacker := range attackers {
			foreignWorkers.Add(1)
			go func(request int, category int, caller *memoryConcurrencyLoadPrincipal) {
				defer foreignWorkers.Done()
				<-foreignStart
				operationStarted := time.Now()
				_, startErr := st.StartCuration(ctx, caller.principal, StartMemoryCurationInput{
					RequestID:     jobs[request].request.ID,
					Caps:          MemoryCurationInputCaps{MaxMemories: maximumMemories},
					LeaseDuration: maxMemoryCurationLease,
					Client: MemoryClientProvenance{
						Runtime: "concurrency-load", Recipe: loadquality.ConcurrencyResultSchemaV1,
						RecipeVersion: loadquality.ConcurrencyHarnessVersion,
					},
					IdempotencyKey: "concurrency-foreign-claim-" + memoryConcurrencyLoadToken(
						opts.Seed, fmt.Sprint(request), fmt.Sprint(category)),
				})
				foreignResults <- memoryConcurrencyLoadForeignClaimResult{
					requestIndex: request, dimension: category,
					duration: time.Since(operationStarted), err: startErr,
				}
			}(requestIndex, dimension, attacker)
		}
	}
	foreignWallStarted := time.Now()
	close(foreignStart)
	foreignWorkers.Wait()
	foreignWall := time.Since(foreignWallStarted)
	close(foreignResults)
	foreignDurations := make([]time.Duration, 0, foreignAttempts)
	crossRefusals := [3]int{}
	for item := range foreignResults {
		foreignDurations = append(foreignDurations, item.duration)
		if !errors.Is(item.err, ErrMemoryCurationNotFound) {
			return memoryConcurrencyLoadEmptyCurationResult(fmt.Errorf(
				"foreign claim request %d dimension %d error = %v, want ErrMemoryCurationNotFound",
				item.requestIndex, item.dimension, item.err,
			))
		}
		crossRefusals[item.dimension]++
	}
	if len(foreignDurations) != foreignAttempts ||
		crossRefusals != [3]int{principalCount, principalCount, principalCount} {
		return memoryConcurrencyLoadEmptyCurationResult(fmt.Errorf(
			"foreign claim completion mismatch attempts=%d/%d refusals=%v",
			len(foreignDurations), foreignAttempts, crossRefusals,
		))
	}
	claimTiming.addPhase(foreignDurations, foreignWall)

	ownerAttempts := principalCount * opts.ClaimWorkers
	ownerResults := make(chan memoryConcurrencyLoadOwnerClaimResult, ownerAttempts)
	ownerStart := make(chan struct{})
	var ownerWorkers sync.WaitGroup
	for requestIndex := range jobs {
		for workerIndex := 0; workerIndex < opts.ClaimWorkers; workerIndex++ {
			ownerWorkers.Add(1)
			go func(request int, worker int) {
				defer ownerWorkers.Done()
				<-ownerStart
				operationStarted := time.Now()
				started, startErr := st.StartCuration(ctx, jobs[request].fixture.principal,
					StartMemoryCurationInput{
						RequestID:     jobs[request].request.ID,
						Caps:          MemoryCurationInputCaps{MaxMemories: maximumMemories},
						LeaseDuration: maxMemoryCurationLease,
						Client: MemoryClientProvenance{
							Runtime: "concurrency-load", Recipe: loadquality.ConcurrencyResultSchemaV1,
							RecipeVersion: loadquality.ConcurrencyHarnessVersion,
						},
						IdempotencyKey: "concurrency-owner-claim-" + memoryConcurrencyLoadToken(
							opts.Seed, fmt.Sprint(request), fmt.Sprint(worker)),
					},
				)
				ownerResults <- memoryConcurrencyLoadOwnerClaimResult{
					requestIndex: request, workerIndex: worker, started: started,
					duration: time.Since(operationStarted), err: startErr,
				}
			}(requestIndex, workerIndex)
		}
	}
	ownerWallStarted := time.Now()
	close(ownerStart)
	ownerWorkers.Wait()
	ownerWall := time.Since(ownerWallStarted)
	close(ownerResults)
	ownerDurations := make([]time.Duration, 0, ownerAttempts)
	wins := make([]int, principalCount)
	losses := make([]int, principalCount)
	for item := range ownerResults {
		ownerDurations = append(ownerDurations, item.duration)
		if item.err == nil {
			wins[item.requestIndex]++
			if wins[item.requestIndex] != 1 {
				return memoryConcurrencyLoadEmptyCurationResult(
					fmt.Errorf("request %d had more than one claim winner", item.requestIndex),
				)
			}
			if err := validateMemoryConcurrencyLoadCurationStart(
				item.started, jobs[item.requestIndex], maximumMemories,
				opts.SeedMemoriesPerAgent,
				opts.WorkersPerAgent*opts.OperationsPerWorker,
			); err != nil {
				return memoryConcurrencyLoadEmptyCurationResult(
					fmt.Errorf("claim winner request %d: %w", item.requestIndex, err),
				)
			}
			jobs[item.requestIndex].started = item.started
			continue
		}
		if !errors.Is(item.err, ErrMemoryCurationBusy) {
			return memoryConcurrencyLoadEmptyCurationResult(fmt.Errorf(
				"owner claim request %d worker %d error = %v, want ErrMemoryCurationBusy",
				item.requestIndex, item.workerIndex, item.err,
			))
		}
		losses[item.requestIndex]++
	}
	if len(ownerDurations) != ownerAttempts {
		return memoryConcurrencyLoadEmptyCurationResult(
			fmt.Errorf("owner claim measurements = %d, want %d", len(ownerDurations), ownerAttempts),
		)
	}
	for requestIndex := range jobs {
		if wins[requestIndex] != 1 || losses[requestIndex] != opts.ClaimWorkers-1 {
			return memoryConcurrencyLoadEmptyCurationResult(fmt.Errorf(
				"request %d claim wins/losses = %d/%d, want 1/%d",
				requestIndex, wins[requestIndex], losses[requestIndex], opts.ClaimWorkers-1,
			))
		}
	}
	claimTiming.addPhase(ownerDurations, ownerWall)
	claimStats, err := claimTiming.summarize()
	if err != nil {
		return memoryConcurrencyLoadEmptyCurationResult(err)
	}

	type preparationResult struct {
		jobIndex int
		err      error
	}
	prepared := make([]bool, principalCount)
	for batchStart := 0; batchStart < principalCount; batchStart += loadquality.ConcurrencyAgentBatchSize {
		batchIndex := batchStart / loadquality.ConcurrencyAgentBatchSize
		epoch := fmt.Sprintf("prepare-batch-%d", batchIndex)
		if err := renewMemoryConcurrencyLoadCurationJobs(ctx, st, jobs, 0, epoch, opts); err != nil {
			return memoryConcurrencyLoadEmptyCurationResult(err)
		}
		batchEnd := batchStart + loadquality.ConcurrencyAgentBatchSize
		if batchEnd > principalCount {
			batchEnd = principalCount
		}
		batchSize := batchEnd - batchStart
		preparationResults := make(chan preparationResult, batchSize)
		var preparationWorkers sync.WaitGroup
		for jobIndex := batchStart; jobIndex < batchEnd; jobIndex++ {
			preparationWorkers.Add(1)
			go func(index int) {
				defer preparationWorkers.Done()
				preparationResults <- preparationResult{
					jobIndex: index,
					err:      prepareMemoryConcurrencyLoadCurationJob(ctx, st, &jobs[index], opts),
				}
			}(jobIndex)
		}
		preparationWorkers.Wait()
		close(preparationResults)
		batchResults := 0
		for result := range preparationResults {
			if result.jobIndex < batchStart || result.jobIndex >= batchEnd || prepared[result.jobIndex] {
				return memoryConcurrencyLoadEmptyCurationResult(
					errors.New("curation preparation returned a duplicate or out-of-batch job"),
				)
			}
			prepared[result.jobIndex] = true
			batchResults++
			if result.err != nil {
				return memoryConcurrencyLoadEmptyCurationResult(
					fmt.Errorf("prepare curation job %d: %w", result.jobIndex, result.err),
				)
			}
		}
		if batchResults != batchSize {
			return memoryConcurrencyLoadEmptyCurationResult(
				fmt.Errorf("curation preparation batch %d completed %d jobs, want %d",
					batchIndex, batchResults, batchSize),
			)
		}
	}
	for jobIndex, complete := range prepared {
		if !complete {
			return memoryConcurrencyLoadEmptyCurationResult(
				fmt.Errorf("curation preparation omitted job %d", jobIndex),
			)
		}
	}
	if err := renewMemoryConcurrencyLoadCurationJobs(ctx, st, jobs, 0, "pre-cursor-snapshot", opts); err != nil {
		return memoryConcurrencyLoadEmptyCurationResult(err)
	}
	knownCursors := make(map[memoryConcurrencyLoadCursorKey]int64, principalCount)
	for jobIndex := range jobs {
		key := memoryConcurrencyLoadCursorKey{
			accountID: jobs[jobIndex].fixture.principal.AccountID,
			realmID:   jobs[jobIndex].fixture.principal.RealmID,
			ownerID:   jobs[jobIndex].fixture.principal.ID,
			source:    jobs[jobIndex].cursor.sourceKind,
			streamID:  jobs[jobIndex].cursor.streamID,
		}
		if _, duplicate := knownCursors[key]; duplicate {
			return memoryConcurrencyLoadEmptyCurationResult(errors.New("curation jobs share a cursor key"))
		}
		knownCursors[key] = 0
	}
	initialPositions, err := loadMemoryConcurrencyLoadCursorPositions(ctx, st, knownCursors)
	if err != nil {
		return memoryConcurrencyLoadEmptyCurationResult(err)
	}
	if len(initialPositions) != principalCount {
		return memoryConcurrencyLoadEmptyCurationResult(
			fmt.Errorf("cursor snapshot contained %d rows before apply, want %d", len(initialPositions), principalCount),
		)
	}
	for key, position := range initialPositions {
		if position != knownCursors[key] {
			return memoryConcurrencyLoadEmptyCurationResult(
				fmt.Errorf("cursor snapshot position = %d, want exact prior %d", position, knownCursors[key]),
			)
		}
	}

	applyDurations := make([]time.Duration, 0, principalCount)
	applyWall := time.Duration(0)
	for jobIndex := range jobs {
		if jobIndex%loadquality.ConcurrencyAgentBatchSize == 0 {
			epoch := fmt.Sprintf("apply-batch-%d", jobIndex/loadquality.ConcurrencyAgentBatchSize)
			if err := renewMemoryConcurrencyLoadCurationJobs(ctx, st, jobs, jobIndex, epoch, opts); err != nil {
				return memoryConcurrencyLoadEmptyCurationResult(err)
			}
		}
		job := &jobs[jobIndex]
		operationStarted := time.Now()
		applied, applyErr := st.ApplyCuration(ctx, job.fixture.principal, job.started.Run.ID,
			ApplyMemoryCurationInput{
				FencingGeneration: job.started.Run.FencingGeneration,
				PlanRevision:      job.planRevision,
				PlanHash:          job.planHash,
				IdempotencyKey: "concurrency-apply-" +
					memoryConcurrencyLoadToken(opts.Seed, fmt.Sprint(jobIndex)),
			})
		applyDuration := time.Since(operationStarted)
		applyDurations = append(applyDurations, applyDuration)
		applyWall += applyDuration
		if applyErr != nil {
			return memoryConcurrencyLoadEmptyCurationResult(
				fmt.Errorf("apply curation job %d: %w", jobIndex, applyErr),
			)
		}
		if err := validateMemoryConcurrencyLoadCurationApply(applied, *job); err != nil {
			return memoryConcurrencyLoadEmptyCurationResult(
				fmt.Errorf("apply curation job %d: %w", jobIndex, err),
			)
		}
		positions, positionErr := loadMemoryConcurrencyLoadCursorPositions(ctx, st, knownCursors)
		if positionErr != nil {
			return memoryConcurrencyLoadEmptyCurationResult(positionErr)
		}
		if len(positions) != principalCount {
			return memoryConcurrencyLoadEmptyCurationResult(fmt.Errorf(
				"cursor rows after apply %d = %d, want %d", jobIndex, len(positions), principalCount,
			))
		}
		for checkIndex := range jobs {
			checkJob := jobs[checkIndex]
			key := memoryConcurrencyLoadCursorKey{
				accountID: checkJob.fixture.principal.AccountID,
				realmID:   checkJob.fixture.principal.RealmID,
				ownerID:   checkJob.fixture.principal.ID,
				source:    checkJob.cursor.sourceKind, streamID: checkJob.cursor.streamID,
			}
			position, present := positions[key]
			if checkIndex <= jobIndex {
				if !present || position != checkJob.cursor.upper {
					return memoryConcurrencyLoadEmptyCurationResult(fmt.Errorf(
						"cursor %d after apply %d = %d/%v, want exact upper",
						checkIndex, jobIndex, position, present,
					))
				}
			} else if !present || position != checkJob.cursor.expectedPrior {
				return memoryConcurrencyLoadEmptyCurationResult(fmt.Errorf(
					"foreign cursor %d during apply %d = %d/%v, want exact prior",
					checkIndex, jobIndex, position, present,
				))
			}
		}
	}
	applyStats, err := loadquality.Summarize(applyDurations, applyWall)
	if err != nil {
		return memoryConcurrencyLoadEmptyCurationResult(err)
	}
	outcome := loadquality.ConcurrencyCurationClaimsOutcome{
		Requests: principalCount, RequestCalls: principalCount,
		OwnerClaimAttempts: ownerAttempts, OwnerClaimWins: principalCount,
		OwnerClaimLosses:     principalCount * (opts.ClaimWorkers - 1),
		ForeignClaimAttempts: foreignAttempts,
		CrossAccountRefusals: crossRefusals[0], CrossRealmRefusals: crossRefusals[1],
		CrossAgentRefusals: crossRefusals[2], TypedForeignRefusals: foreignAttempts,
		ForeignClaimWins: 0, ApplyCalls: principalCount,
		OwnerCursorAdvances: principalCount, ForeignCursorAdvances: 0,
		SingleWinnerPerRequest: true, AllForeignClaimsTyped: true,
		OnlyOwnerCursorAdvanced: true, AllRequestsApplied: true,
	}
	return requestStats, claimStats, applyStats, outcome, nil
}

func memoryConcurrencyLoadEmptyCurationResult(err error) (
	loadquality.OperationStats,
	loadquality.OperationStats,
	loadquality.OperationStats,
	loadquality.ConcurrencyCurationClaimsOutcome,
	error,
) {
	return loadquality.OperationStats{}, loadquality.OperationStats{}, loadquality.OperationStats{},
		loadquality.ConcurrencyCurationClaimsOutcome{}, err
}

func renewMemoryConcurrencyLoadCurationJobs(
	ctx context.Context,
	st *Store,
	jobs []memoryConcurrencyLoadCurationJob,
	startIndex int,
	epoch string,
	opts loadquality.ConcurrencyOptions,
) error {
	if startIndex < 0 || startIndex >= len(jobs) {
		return errors.New("curation renewal start index is outside the fleet")
	}
	type renewalResult struct {
		jobIndex int
		renewed  RenewMemoryCurationResult
		err      error
	}
	resultCount := len(jobs) - startIndex
	results := make(chan renewalResult, resultCount)
	var workers sync.WaitGroup
	for jobIndex := startIndex; jobIndex < len(jobs); jobIndex++ {
		workers.Add(1)
		go func(index int) {
			defer workers.Done()
			job := jobs[index]
			renewed, err := st.RenewCuration(ctx, job.fixture.principal, job.started.Run.ID,
				RenewMemoryCurationInput{
					FencingGeneration: job.started.Run.FencingGeneration,
					Extension:         maxMemoryCurationLease,
					IdempotencyKey: "concurrency-renew-" + memoryConcurrencyLoadToken(
						opts.Seed, epoch, fmt.Sprint(index)),
				})
			results <- renewalResult{jobIndex: index, renewed: renewed, err: err}
		}(jobIndex)
	}
	workers.Wait()
	close(results)
	seen := make(map[int]struct{}, resultCount)
	for result := range results {
		if result.jobIndex < startIndex || result.jobIndex >= len(jobs) {
			return errors.New("curation renewal returned an invalid job index")
		}
		if _, duplicate := seen[result.jobIndex]; duplicate {
			return errors.New("curation renewal returned a duplicate job")
		}
		seen[result.jobIndex] = struct{}{}
		if result.err != nil {
			return fmt.Errorf("renew curation job %d at %s: %w", result.jobIndex, epoch, result.err)
		}
		job := jobs[result.jobIndex]
		expectedState := MemoryCurationRunOpen
		if job.planRevision > 0 {
			expectedState = MemoryCurationRunPlanned
		}
		if result.renewed.Run.ID != job.started.Run.ID ||
			result.renewed.Run.AccountID != job.fixture.principal.AccountID ||
			result.renewed.Run.RealmID != job.fixture.principal.RealmID ||
			result.renewed.Run.OwnerID != job.fixture.principal.ID ||
			result.renewed.Run.FencingGeneration != job.started.Run.FencingGeneration ||
			result.renewed.Run.State != expectedState || result.renewed.Run.LeaseExpiresAt == nil ||
			result.renewed.Receipt.Operation != "renew" ||
			result.renewed.Receipt.ActorID != job.fixture.principal.ID ||
			result.renewed.Receipt.RunID != job.started.Run.ID ||
			result.renewed.Receipt.FencingGeneration != job.started.Run.FencingGeneration ||
			result.renewed.Receipt.ResultState != expectedState ||
			result.renewed.Receipt.LeaseExpiresAt == nil || result.renewed.Receipt.Replayed {
			return fmt.Errorf("renew curation job %d returned a non-exact fenced value", result.jobIndex)
		}
	}
	if len(seen) != resultCount {
		return fmt.Errorf("curation renewal completed %d jobs, want %d", len(seen), resultCount)
	}
	return nil
}

func validateMemoryConcurrencyLoadCurationRequest(
	result RequestMemoryCurationResult,
	principal Principal,
	maximumMemories int,
) error {
	request := result.Request
	if request.ID == "" || request.State != MemoryCurationRequestQueued ||
		request.AccountID != principal.AccountID || request.RealmID != principal.RealmID ||
		request.OwnerKind != PrincipalAgent || request.OwnerID != principal.ID ||
		request.ActorKind != PrincipalAgent || request.ActorID != principal.ID ||
		request.CoalescingKey != "concurrency_load" || request.TriggerReason != "load_test" ||
		request.RequestGeneration < 1 || request.AttemptCount != 0 || request.ReadOnlyReplay ||
		request.ClaimedRunID != "" || request.FulfilledGeneration != 0 {
		return errors.New("request returned a non-exact owner-scoped queued value")
	}
	if len(request.Scope.Sources) != 1 || request.Scope.Sources[0] != MemoryCurationSourceMemory ||
		len(request.Scope.MemoryStates) != 1 || request.Scope.MemoryStates[0] != MemoryStateActive ||
		request.Scope.IncludeSensitive || request.Scope.MaxMemories != maximumMemories {
		return errors.New("request returned a non-exact memory-only scope")
	}
	if result.Receipt.Operation != "request" || result.Receipt.ActorID != principal.ID ||
		result.Receipt.RequestID != request.ID ||
		result.Receipt.RequestGeneration != request.RequestGeneration ||
		result.Receipt.ResultState != MemoryCurationRequestQueued || result.Receipt.Replayed {
		return errors.New("request returned a non-exact mutation receipt")
	}
	return nil
}

func validateMemoryConcurrencyLoadCurationStart(
	result StartMemoryCurationResult,
	job memoryConcurrencyLoadCurationJob,
	maximumMemories int,
	seedMemories int,
	mixedCount int,
) error {
	run := result.Run
	// Every capture allocates one shared sequence for the memory version and
	// one for its unavailable evidence row. Each mixed adjustment allocates a
	// third sequence, so the exact final watermark is 2*(M+1)+3*(W*O).
	wantMemoryUpper := int64(2*(seedMemories+1) + 3*mixedCount)
	wantEvidenceUpper := wantMemoryUpper
	if run.ID == "" || run.State != MemoryCurationRunOpen || run.RequestID != job.request.ID ||
		run.AccountID != job.fixture.principal.AccountID ||
		run.RealmID != job.fixture.principal.RealmID || run.OwnerKind != PrincipalAgent ||
		run.OwnerID != job.fixture.principal.ID || run.ActorKind != PrincipalAgent ||
		run.ActorID != job.fixture.principal.ID || run.FencingGeneration < 1 ||
		run.RequestGeneration != job.request.RequestGeneration || run.LeaseExpiresAt == nil ||
		run.MemoryInputCount != maximumMemories || run.EvidenceInputCount != 0 ||
		run.TranscriptInputCount != 0 || run.CursorInputCount != 1 ||
		run.InputCount != maximumMemories+1 || run.MemoryChangeUpper != wantMemoryUpper ||
		run.EvidenceChangeUpper != wantEvidenceUpper || result.FirstInputCursor == "" {
		return errors.New("claim winner returned a non-exact run snapshot")
	}
	if result.Request.ID != job.request.ID || result.Request.State != MemoryCurationRequestClaimed ||
		result.Request.ClaimedRunID != run.ID || result.Request.AccountID != run.AccountID ||
		result.Request.RealmID != run.RealmID || result.Request.OwnerID != run.OwnerID {
		return errors.New("claim winner returned a non-exact claimed request")
	}
	if result.Receipt.Operation != "start" || result.Receipt.ActorID != job.fixture.principal.ID ||
		result.Receipt.RequestID != job.request.ID || result.Receipt.RunID != run.ID ||
		result.Receipt.RequestGeneration != run.RequestGeneration ||
		result.Receipt.FencingGeneration != run.FencingGeneration ||
		result.Receipt.ResultState != MemoryCurationRunOpen || result.Receipt.Replayed {
		return errors.New("claim winner returned a non-exact start receipt")
	}
	return nil
}

func prepareMemoryConcurrencyLoadCurationJob(
	ctx context.Context,
	st *Store,
	job *memoryConcurrencyLoadCurationJob,
	opts loadquality.ConcurrencyOptions,
) error {
	expected := make(map[string]Memory,
		opts.SeedMemoriesPerAgent+opts.WorkersPerAgent*opts.OperationsPerWorker)
	for _, seeded := range job.fixture.visible {
		expected[seeded.memory.ID] = seeded.memory
	}
	for _, mixed := range job.fixture.mixed {
		if _, duplicate := expected[mixed.ID]; duplicate {
			return errors.New("curation expected-memory set contains a duplicate")
		}
		expected[mixed.ID] = mixed
	}
	if len(expected) != job.started.Run.MemoryInputCount {
		return fmt.Errorf("expected memory set = %d, want %d", len(expected), job.started.Run.MemoryInputCount)
	}
	seen := make(map[string]struct{}, len(expected))
	cursorCount := 0
	cursor := job.started.FirstInputCursor
	for {
		page, err := st.GetCurationRunInputs(ctx, job.fixture.principal, job.started.Run.ID,
			job.started.Run.FencingGeneration, cursor, 200)
		if err != nil {
			return fmt.Errorf("get run inputs: %w", err)
		}
		if page.Run.ID != job.started.Run.ID || page.Run.FencingGeneration != job.started.Run.FencingGeneration ||
			page.Run.State != MemoryCurationRunOpen || page.Run.AccountID != job.fixture.principal.AccountID ||
			page.Run.RealmID != job.fixture.principal.RealmID || page.Run.OwnerID != job.fixture.principal.ID {
			return errors.New("input page returned a non-exact owner run")
		}
		for _, input := range page.Inputs {
			if input.RunID != job.started.Run.ID || input.Ordinal < 1 {
				return errors.New("input row returned a non-exact run reference")
			}
			switch input.Kind {
			case MemoryCurationInputMemory:
				expectedMemory, ok := expected[input.MemoryID]
				if !ok || input.Memory == nil || input.MemoryVersion != expectedMemory.Version ||
					!memoryConcurrencyLoadCurationMemoryMatches(*input.Memory, expectedMemory, job.fixture.principal) {
					return errors.New("input page returned a non-exact memory row")
				}
				if _, duplicate := seen[input.MemoryID]; duplicate {
					return errors.New("input page returned a duplicate memory row")
				}
				seen[input.MemoryID] = struct{}{}
			case MemoryCurationInputCursor:
				cursorCount++
				if cursorCount != 1 || input.CursorSourceKind != MemoryCurationSourceMemory ||
					input.CursorStreamID == "" || input.CursorExpectedPrior != 0 ||
					input.CursorUpper != job.started.Run.MemoryChangeUpper {
					return errors.New("input page returned a non-exact memory cursor")
				}
				job.cursor = memoryConcurrencyLoadCurationCursor{
					sourceKind: input.CursorSourceKind, streamID: input.CursorStreamID,
					expectedPrior: input.CursorExpectedPrior, upper: input.CursorUpper,
				}
			default:
				return fmt.Errorf("input page returned unexpected kind %q", input.Kind)
			}
		}
		if page.NextCursor == "" {
			break
		}
		if page.NextCursor == cursor {
			return errors.New("input paging cursor did not advance")
		}
		cursor = page.NextCursor
	}
	if len(seen) != len(expected) || cursorCount != 1 ||
		len(seen)+cursorCount != job.started.Run.InputCount {
		return fmt.Errorf("input rows memory/cursor = %d/%d, want %d/1",
			len(seen), cursorCount, len(expected))
	}

	planned, err := st.PlanCuration(ctx, job.fixture.principal, job.started.Run.ID,
		PlanMemoryCurationInput{
			FencingGeneration: job.started.Run.FencingGeneration,
			Draft:             append(json.RawMessage(nil), memoryConcurrencyLoadEmptyPlan...),
			IdempotencyKey: "concurrency-plan-" +
				memoryConcurrencyLoadToken(opts.Seed, fmt.Sprint(job.fixture.index)),
		})
	if err != nil {
		return fmt.Errorf("plan empty curation: %w", err)
	}
	projectedActive := int64(len(expected) + 1)
	if err := validateMemoryConcurrencyLoadEmptyPlan(
		planned.Run, planned.Plan, planned.PreallocatedMemoryIDs, planned.Preview,
		job.started, projectedActive,
	); err != nil {
		return fmt.Errorf("accepted empty plan: %w", err)
	}
	if planned.Receipt.Operation != "plan" || planned.Receipt.ActorID != job.fixture.principal.ID ||
		planned.Receipt.RequestID != job.request.ID || planned.Receipt.RunID != job.started.Run.ID ||
		planned.Receipt.FencingGeneration != job.started.Run.FencingGeneration ||
		planned.Receipt.PlanRevision != planned.Plan.PlanRevision ||
		planned.Receipt.PlanHash == "" || planned.Receipt.ResultState != MemoryCurationRunPlanned ||
		planned.Receipt.Replayed {
		return errors.New("accepted empty plan returned a non-exact receipt")
	}
	stored, err := st.GetCurationPlan(ctx, job.fixture.principal, job.started.Run.ID,
		job.started.Run.FencingGeneration)
	if err != nil {
		return fmt.Errorf("get empty curation plan: %w", err)
	}
	if err := validateMemoryConcurrencyLoadEmptyPlan(
		stored.Run, stored.Plan, stored.PreallocatedMemoryIDs, stored.Preview,
		job.started, projectedActive,
	); err != nil {
		return fmt.Errorf("stored empty plan: %w", err)
	}
	if stored.Run.PlanHash != planned.Receipt.PlanHash ||
		stored.Run.PlanRevision != planned.Receipt.PlanRevision {
		return errors.New("stored empty plan does not exactly match the accepted plan")
	}
	job.planRevision = stored.Run.PlanRevision
	job.planHash = stored.Run.PlanHash
	return nil
}

func memoryConcurrencyLoadCurationMemoryMatches(
	actual Memory,
	expected Memory,
	principal Principal,
) bool {
	// Curation input hydration intentionally omits evidence payloads, exactly
	// like recall. Every other field must match the exact fixture.
	expected.Evidence = nil
	return memoryConcurrencyLoadOwnerMatches(actual, principal) && reflect.DeepEqual(actual, expected)
}

func validateMemoryConcurrencyLoadEmptyPlan(
	run MemoryCurationRun,
	plan MemoryCurationPlan,
	preallocated []MemoryCurationPreallocatedMemoryID,
	preview MemoryCurationImpactPreview,
	started StartMemoryCurationResult,
	projectedActive int64,
) error {
	if run.ID != started.Run.ID || run.State != MemoryCurationRunPlanned ||
		run.AccountID != started.Run.AccountID || run.RealmID != started.Run.RealmID ||
		run.OwnerID != started.Run.OwnerID || run.FencingGeneration != started.Run.FencingGeneration ||
		run.PlanSchema != MemoryCurationPlanSchemaV1 || run.PlanRevision != 1 || run.PlanHash == "" ||
		plan.Schema != MemoryCurationPlanSchemaV1 || plan.PlanRevision != 1 ||
		len(plan.Actions) != 0 || len(preallocated) != 0 {
		return errors.New("empty plan returned non-exact run or plan fields")
	}
	if preview.ActionCount != 0 || preview.CreateActions != 0 || preview.ReplaceActions != 0 ||
		preview.SupersedeActions != 0 || preview.RelateActions != 0 || preview.ProposeFactActions != 0 ||
		preview.NewMemories != 0 || preview.MemoryVersionWrites != 0 || preview.EvidenceRows != 0 ||
		preview.RelationRows != 0 || preview.ExpectedVersionChecks != 0 || preview.FactCandidates != 0 ||
		preview.ActiveMemoryDelta != 0 || preview.ProjectedActiveMemories != projectedActive {
		return errors.New("empty plan returned a non-exact impact preview")
	}
	return nil
}

func validateMemoryConcurrencyLoadCurationApply(
	result ApplyMemoryCurationResult,
	job memoryConcurrencyLoadCurationJob,
) error {
	if result.Run.ID != job.started.Run.ID || result.Run.State != MemoryCurationRunApplied ||
		result.Run.AccountID != job.fixture.principal.AccountID ||
		result.Run.RealmID != job.fixture.principal.RealmID ||
		result.Run.OwnerID != job.fixture.principal.ID ||
		result.Run.FencingGeneration != job.started.Run.FencingGeneration ||
		result.Run.PlanRevision != job.planRevision || result.Run.PlanHash != job.planHash ||
		result.Request.ID != job.request.ID || result.Request.State != MemoryCurationRequestFulfilled ||
		result.Request.FulfilledGeneration != job.request.RequestGeneration ||
		result.FollowUpRequest != nil {
		return errors.New("apply returned non-exact terminal run or request fields")
	}
	receipt := result.Receipt
	if receipt.ID == "" || receipt.Operation != "apply" ||
		receipt.ActorID != job.fixture.principal.ID || receipt.RequestID != job.request.ID ||
		receipt.RunID != job.started.Run.ID ||
		receipt.RequestGeneration != job.started.Run.RequestGeneration ||
		receipt.FencingGeneration != job.started.Run.FencingGeneration ||
		receipt.PlanRevision != job.planRevision || receipt.PlanHash != job.planHash ||
		len(receipt.ActionResults) != 0 || len(receipt.CursorIntervals) != 1 ||
		receipt.FollowUpRequestID != "" || receipt.FollowUpGeneration != 0 || receipt.Replayed {
		return errors.New("apply returned a non-exact receipt")
	}
	interval := receipt.CursorIntervals[0]
	if interval.SourceKind != job.cursor.sourceKind || interval.SourceStreamID != job.cursor.streamID ||
		interval.ExpectedPrior != job.cursor.expectedPrior || interval.Upper != job.cursor.upper {
		return errors.New("apply returned a non-exact owner cursor interval")
	}
	return nil
}

func loadMemoryConcurrencyLoadCursorPositions(
	ctx context.Context,
	st *Store,
	known map[memoryConcurrencyLoadCursorKey]int64,
) (map[memoryConcurrencyLoadCursorKey]int64, error) {
	rows, err := st.pool.Query(ctx, `
		SELECT account_id,realm_id,owner_id,source_kind,source_stream_id,position
		FROM memory_curation_cursors
		WHERE owner_kind='agent'
		ORDER BY account_id,realm_id,owner_id,source_kind,source_stream_id`)
	if err != nil {
		return nil, fmt.Errorf("snapshot curation cursors: %w", err)
	}
	defer rows.Close()
	positions := make(map[memoryConcurrencyLoadCursorKey]int64, len(known))
	for rows.Next() {
		var key memoryConcurrencyLoadCursorKey
		var position int64
		if err := rows.Scan(&key.accountID, &key.realmID, &key.ownerID,
			&key.source, &key.streamID, &position); err != nil {
			return nil, err
		}
		if _, ok := known[key]; !ok {
			return nil, errors.New("cursor snapshot contained an unknown tenant cursor")
		}
		if _, duplicate := positions[key]; duplicate {
			return nil, errors.New("cursor snapshot contained a duplicate tenant cursor")
		}
		positions[key] = position
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return positions, nil
}

type memoryConcurrencyLoadFanoutResult struct {
	principalIndex int
	duration       time.Duration
	err            error
}

func runMemoryConcurrencySensitiveFanout(
	ctx context.Context,
	st *Store,
	fixtures []*memoryConcurrencyLoadPrincipal,
	opts loadquality.ConcurrencyOptions,
) (
	loadquality.OperationStats,
	loadquality.ConcurrencySensitiveFanoutOutcome,
	error,
) {
	owner := fixtures[0]
	marker := "fanout" + memoryConcurrencyLoadToken(opts.Seed, "fanout")
	captured, _, err := captureMemoryConcurrencyLoadFixture(
		ctx, st, owner.principal, marker, []string{memoryConcurrencyLoadFanoutTag},
		0.875, true, "fanout-"+memoryConcurrencyLoadToken(opts.Seed, "capture"),
		nil,
	)
	if err != nil {
		return loadquality.OperationStats{}, loadquality.ConcurrencySensitiveFanoutOutcome{},
			fmt.Errorf("capture sensitive fanout fixture: %w", err)
	}
	if err := validateMemoryConcurrencyLoadValue(captured, owner.principal, marker, true); err != nil {
		return loadquality.OperationStats{}, loadquality.ConcurrencySensitiveFanoutOutcome{},
			fmt.Errorf("capture sensitive fanout fixture: %w", err)
	}

	ownerStarted := time.Now()
	ownerPage, err := st.RecallMemories(ctx, owner.principal, MemoryRecallOptions{
		Query: marker, Tags: []string{memoryConcurrencyLoadFanoutTag}, IncludeSensitive: true, Limit: 2,
	})
	ownerDuration := time.Since(ownerStarted)
	if err != nil {
		return loadquality.OperationStats{}, loadquality.ConcurrencySensitiveFanoutOutcome{},
			fmt.Errorf("owner sensitive fanout recall: %w", err)
	}
	if err := validateMemoryConcurrencyLoadLexicalPage(ownerPage); err != nil {
		return loadquality.OperationStats{}, loadquality.ConcurrencySensitiveFanoutOutcome{},
			fmt.Errorf("owner sensitive fanout recall: %w", err)
	}
	if len(ownerPage.Hits) != 1 || ownerPage.NextCursor != "" {
		return loadquality.OperationStats{}, loadquality.ConcurrencySensitiveFanoutOutcome{},
			fmt.Errorf("owner sensitive fanout recall returned %d rows, want exactly 1", len(ownerPage.Hits))
	}
	ownerHit := ownerPage.Hits[0].Memory
	if !memoryConcurrencyLoadExactRecallMatches(ownerHit, captured) ||
		!memoryConcurrencyLoadOwnerMatches(ownerHit, owner.principal) {
		return loadquality.OperationStats{}, loadquality.ConcurrencySensitiveFanoutOutcome{},
			errors.New("owner sensitive fanout recall returned a non-exact row")
	}
	exact, err := st.GetMemory(ctx, owner.principal, captured.ID)
	if err != nil {
		return loadquality.OperationStats{}, loadquality.ConcurrencySensitiveFanoutOutcome{},
			fmt.Errorf("owner exact sensitive read: %w", err)
	}
	if !reflect.DeepEqual(exact, captured) ||
		!memoryConcurrencyLoadOwnerMatches(exact, owner.principal) {
		return loadquality.OperationStats{}, loadquality.ConcurrencySensitiveFanoutOutcome{},
			errors.New("owner exact sensitive read returned a non-exact row")
	}

	foreignCount := len(fixtures) - 1
	foreignResults := make(chan memoryConcurrencyLoadFanoutResult, foreignCount)
	ready := make(chan struct{}, foreignCount)
	start := make(chan struct{})
	var startOnce sync.Once
	releaseStart := func() {
		startOnce.Do(func() { close(start) })
	}
	defer releaseStart()
	var workers sync.WaitGroup
	for _, fixture := range fixtures[1:] {
		workers.Add(1)
		go func(caller *memoryConcurrencyLoadPrincipal) {
			defer workers.Done()
			ready <- struct{}{}
			<-start
			operationStarted := time.Now()
			page, recallErr := st.RecallMemories(ctx, caller.principal, MemoryRecallOptions{
				Query: marker, Tags: []string{memoryConcurrencyLoadFanoutTag}, IncludeSensitive: true, Limit: 2,
			})
			duration := time.Since(operationStarted)
			if recallErr == nil {
				recallErr = validateMemoryConcurrencyLoadLexicalPage(page)
			}
			if recallErr == nil && (len(page.Hits) != 0 || page.NextCursor != "") {
				recallErr = fmt.Errorf("returned %d rows, want exactly 0", len(page.Hits))
			}
			foreignResults <- memoryConcurrencyLoadFanoutResult{
				principalIndex: caller.index, duration: duration, err: recallErr,
			}
		}(fixture)
	}
	for readyIndex := 0; readyIndex < foreignCount; readyIndex++ {
		select {
		case <-ready:
		case <-ctx.Done():
			return loadquality.OperationStats{}, loadquality.ConcurrencySensitiveFanoutOutcome{},
				fmt.Errorf("wait for sensitive fanout start gate: %w", ctx.Err())
		}
	}
	foreignWallStarted := time.Now()
	releaseStart()
	workers.Wait()
	foreignWall := time.Since(foreignWallStarted)
	close(foreignResults)
	durations := make([]time.Duration, 0, len(fixtures))
	durations = append(durations, ownerDuration)
	for item := range foreignResults {
		durations = append(durations, item.duration)
		if item.err != nil {
			return loadquality.OperationStats{}, loadquality.ConcurrencySensitiveFanoutOutcome{},
				fmt.Errorf("foreign principal %d sensitive fanout: %w", item.principalIndex, item.err)
		}
	}
	if len(durations) != len(fixtures) {
		return loadquality.OperationStats{}, loadquality.ConcurrencySensitiveFanoutOutcome{},
			fmt.Errorf("sensitive fanout measurements = %d, want %d", len(durations), len(fixtures))
	}
	stats, err := loadquality.Summarize(durations, ownerDuration+foreignWall)
	if err != nil {
		return loadquality.OperationStats{}, loadquality.ConcurrencySensitiveFanoutOutcome{}, err
	}
	outcome := loadquality.ConcurrencySensitiveFanoutOutcome{
		QueryCalls: len(fixtures), OwnerQueryCalls: 1, ForeignQueryCalls: foreignCount,
		OwnerHits: 1, ForeignHits: 0, SensitiveContentLeaks: 0,
		OwnerExactReadSucceeded: true, AllForeignQueriesIsolated: true,
	}
	return stats, outcome, nil
}

func captureMemoryConcurrencyLoadFixture(
	ctx context.Context,
	st *Store,
	principal Principal,
	content string,
	tags []string,
	salience float64,
	sensitive bool,
	idempotencyKey string,
	inFlight *atomic.Int64,
) (Memory, time.Duration, error) {
	input := CaptureMemoryInput{
		Content: content, Kind: memoryConcurrencyLoadKind, Tags: tags, Salience: &salience,
		Sensitive: sensitive, CaptureReason: "load_test",
		Evidence: []MemoryEvidenceInput{{
			ResolutionState: MemoryEvidenceUnavailable, TerminalReasonCode: "synthetic_fixture",
		}},
		Client: MemoryClientProvenance{
			Runtime: "concurrency-load", Recipe: loadquality.ConcurrencyResultSchemaV1,
			RecipeVersion: loadquality.ConcurrencyHarnessVersion,
		},
		IdempotencyKey: "concurrency-load-" + idempotencyKey,
	}
	operationStarted := time.Now()
	if inFlight != nil {
		inFlight.Add(1)
	}
	result, err := st.CaptureMemory(ctx, principal, input)
	if inFlight != nil {
		inFlight.Add(-1)
	}
	duration := time.Since(operationStarted)
	if err != nil {
		return Memory{}, duration, err
	}
	return result.Memory, duration, nil
}

func validateMemoryConcurrencyLoadValue(
	memory Memory,
	principal Principal,
	content string,
	sensitive bool,
) error {
	if memory.ID == "" || memory.Version != 1 || memory.PreviousVersion != 0 ||
		memory.Content != content || memory.ContentEncoding != "plain" ||
		memory.Kind != memoryConcurrencyLoadKind || memory.Sensitive != sensitive || memory.Redacted ||
		memory.State != MemoryStateActive || memory.Origin != "self" ||
		memory.CaptureReason != "load_test" || memory.ActorKind != PrincipalAgent ||
		memory.ActorID != principal.ID || memory.Operation != "added" ||
		!memoryConcurrencyLoadOwnerMatches(memory, principal) {
		return errors.New("capture returned a non-exact memory value")
	}
	return nil
}

func memoryConcurrencyLoadOwnerMatches(memory Memory, principal Principal) bool {
	return memory.AccountID == principal.AccountID && memory.RealmID == principal.RealmID &&
		memory.OwnerKind == PrincipalAgent && memory.OwnerID == principal.ID
}

func memoryConcurrencyLoadToken(seed int64, parts ...string) string {
	payload := fmt.Sprintf("%d\x00%s", seed, strings.Join(parts, "\x00"))
	digest := sha256.Sum256([]byte(payload))
	return fmt.Sprintf("%x", digest[:8])
}
