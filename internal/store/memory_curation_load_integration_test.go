package store

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/loadquality"
)

const memoryCurationLoadEnabled = "WITSELF_MEMORY_CURATION_LOAD"

var exactEmptyMemoryCurationPlan = json.RawMessage(
	`{"schema":"witself.memory-plan.v1","draft_revision":1,"actions":[]}`,
)

type memoryCurationLoadTiming struct {
	durations []time.Duration
	wall      time.Duration
}

func (timing *memoryCurationLoadTiming) add(duration time.Duration) {
	timing.durations = append(timing.durations, duration)
	timing.wall += duration
}

func (timing *memoryCurationLoadTiming) addConcurrent(durations []time.Duration, wall time.Duration) {
	timing.durations = append(timing.durations, durations...)
	timing.wall += wall
}

func (timing memoryCurationLoadTiming) summarize() (loadquality.OperationStats, error) {
	return loadquality.Summarize(timing.durations, timing.wall)
}

type memoryCurationLoadMeasurements struct {
	requestCoalescing memoryCurationLoadTiming
	claimStart        memoryCurationLoadTiming
	inputPage         memoryCurationLoadTiming
	plan              memoryCurationLoadTiming
	planGet           memoryCurationLoadTiming
	apply             memoryCurationLoadTiming
	leaseRenew        memoryCurationLoadTiming
	leaseApplyRace    memoryCurationLoadTiming
	typedRefusal      memoryCurationLoadTiming
	abandon           memoryCurationLoadTiming
}

type memoryCurationLoadExecution struct {
	measurements memoryCurationLoadMeasurements
	outcomes     loadquality.CurationOutcomes
	chainDepth   int
}

type memoryCurationLoadRequest struct {
	principal Principal
	request   MemoryCurationRequest
}

// TestNarrativeMemoryCurationLoadPostgres is the second opt-in executable
// production-readiness slice for narrative memory. It drives only deterministic
// PostgreSQL queue and curation behavior in one fresh disposable schema. It
// performs no client inference and calls no model, embedding, MCP, secret, or
// sealed-plane surface.
func TestNarrativeMemoryCurationLoadPostgres(t *testing.T) {
	if os.Getenv(memoryCurationLoadEnabled) != "1" {
		t.Skip(memoryCurationLoadEnabled + "=1 is required")
	}
	dsn := strings.TrimSpace(os.Getenv("WITSELF_TEST_DATABASE_URL"))
	if dsn == "" {
		t.Fatal("WITSELF_TEST_DATABASE_URL is required when memory curation load testing is enabled")
	}
	opts, err := loadquality.ParseCurationOptions(os.Getenv)
	if err != nil {
		t.Fatal(err)
	}

	startedAt := time.Now().UTC()
	ctx, cancel := context.WithTimeout(context.Background(), 9*time.Minute)
	defer cancel()
	st, _ := newMigrationTestStore(t, dsn)
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}
	var postgresVersion string
	if err := st.pool.QueryRow(ctx, `SHOW server_version`).Scan(&postgresVersion); err != nil {
		t.Fatalf("read PostgreSQL version: %v", err)
	}

	agentCount := 5 + opts.ClaimRequests + len(opts.PagingCardinalities) + opts.LeaseCycles
	principals, err := provisionMemoryCurationLoadPrincipals(ctx, st, opts.Seed, agentCount)
	if err != nil {
		t.Fatal(err)
	}
	next := 0
	coalescingPrincipal := principals[next]
	next++
	claimPrincipals := principals[next : next+opts.ClaimRequests]
	next += opts.ClaimRequests
	pagingPrincipals := principals[next : next+len(opts.PagingCardinalities)]
	next += len(opts.PagingCardinalities)
	lifecyclePrincipal := principals[next]
	next++
	leasePrincipals := principals[next : next+opts.LeaseCycles]
	next += opts.LeaseCycles
	applyRacePrincipal := principals[next]
	next++
	conflictPrincipal := principals[next]
	next++
	abandonPrincipal := principals[next]
	next++
	if next != len(principals) {
		t.Fatalf("curation load principal allocation = %d, want %d", next, len(principals))
	}

	execution := memoryCurationLoadExecution{}
	// Each workload gets its own bounded deadline inside the overall driver
	// context, so a slow-but-legal early workload fails under its own name
	// instead of exhausting the shared budget and misattributing the
	// deadline to whichever later store call happens to be in flight.
	runWorkload := func(name string, fn func(context.Context) error) {
		t.Helper()
		workloadCtx, cancelWorkload := context.WithTimeout(ctx, 2*time.Minute)
		defer cancelWorkload()
		if err := fn(workloadCtx); err != nil {
			t.Fatalf("%s workload: %v", name, err)
		}
	}
	runWorkload("request coalescing", func(c context.Context) error {
		return runMemoryCurationRequestCoalescing(c, st, coalescingPrincipal, opts, &execution)
	})
	runWorkload("claim contention", func(c context.Context) error {
		return runMemoryCurationClaimContention(c, st, claimPrincipals, opts, &execution)
	})
	runWorkload("input paging", func(c context.Context) error {
		return runMemoryCurationInputPaging(c, st, pagingPrincipals, opts, &execution)
	})
	runWorkload("plan lifecycle", func(c context.Context) error {
		return runMemoryCurationPlanLifecycle(c, st, lifecyclePrincipal, opts, &execution)
	})
	runWorkload("lease churn", func(c context.Context) error {
		return runMemoryCurationLeaseChurn(c, st, leasePrincipals, opts, &execution)
	})
	runWorkload("apply race", func(c context.Context) error {
		return runMemoryCurationApplyRace(c, st, applyRacePrincipal, opts, &execution)
	})
	runWorkload("stale plan conflict", func(c context.Context) error {
		return runMemoryCurationStalePlanConflict(c, st, conflictPrincipal, opts, &execution)
	})
	runWorkload("abandon requeue", func(c context.Context) error {
		return runMemoryCurationAbandonRequeue(c, st, abandonPrincipal, opts, &execution)
	})

	measurements, err := execution.measurements.result()
	if err != nil {
		t.Fatal(err)
	}
	result := loadquality.CurationResult{
		Schema:            loadquality.CurationResultSchemaV1,
		HarnessVersion:    loadquality.CurationHarnessVersion,
		StartedAt:         startedAt,
		CompletedAt:       time.Now().UTC(),
		Outcome:           "pass",
		PostgreSQLVersion: strings.TrimSpace(postgresVersion),
		Environment:       loadquality.CurationEnvironment(opts),
		Workload: loadquality.CurationWorkload{
			Seed: opts.Seed, SyntheticAccounts: 2, SyntheticAgents: agentCount,
			CoalescingRequests: opts.CoalescingRequests,
			ClaimRequests:      opts.ClaimRequests, ClaimWorkers: opts.ClaimWorkers,
			PagingCardinalities: append([]int(nil), opts.PagingCardinalities...),
			PageSize:            opts.PageSize, ChainBacklog: opts.ChainBacklog,
			ChainCap: opts.ChainCap, ChainDepth: execution.chainDepth,
			LeaseCycles: opts.LeaseCycles, MaxAttempts: opts.MaxAttempts,
		},
		Measurements: measurements,
		Outcomes:     execution.outcomes,
	}
	raw, err := loadquality.WriteCurationResult(opts.ResultsPath, result)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("sanitized memory-curation load result written to %s", opts.ResultsPath)
	t.Logf("sanitized memory-curation load result:\n%s", raw)
}

func (measurements memoryCurationLoadMeasurements) result() (loadquality.CurationMeasurements, error) {
	type item struct {
		name   string
		timing memoryCurationLoadTiming
		set    func(*loadquality.CurationMeasurements, loadquality.OperationStats)
	}
	items := []item{
		{"request coalescing", measurements.requestCoalescing, func(out *loadquality.CurationMeasurements, stats loadquality.OperationStats) {
			out.RequestCoalescing = stats
		}},
		{"claim start", measurements.claimStart, func(out *loadquality.CurationMeasurements, stats loadquality.OperationStats) { out.ClaimStart = stats }},
		{"input page", measurements.inputPage, func(out *loadquality.CurationMeasurements, stats loadquality.OperationStats) { out.InputPage = stats }},
		{"plan", measurements.plan, func(out *loadquality.CurationMeasurements, stats loadquality.OperationStats) { out.Plan = stats }},
		{"plan get", measurements.planGet, func(out *loadquality.CurationMeasurements, stats loadquality.OperationStats) { out.PlanGet = stats }},
		{"apply", measurements.apply, func(out *loadquality.CurationMeasurements, stats loadquality.OperationStats) { out.Apply = stats }},
		{"lease renew", measurements.leaseRenew, func(out *loadquality.CurationMeasurements, stats loadquality.OperationStats) { out.LeaseRenew = stats }},
		{"lease apply race", measurements.leaseApplyRace, func(out *loadquality.CurationMeasurements, stats loadquality.OperationStats) {
			out.LeaseApplyRace = stats
		}},
		{"typed refusal", measurements.typedRefusal, func(out *loadquality.CurationMeasurements, stats loadquality.OperationStats) {
			out.TypedRefusal = stats
		}},
		{"abandon", measurements.abandon, func(out *loadquality.CurationMeasurements, stats loadquality.OperationStats) { out.Abandon = stats }},
	}
	var out loadquality.CurationMeasurements
	for _, item := range items {
		stats, err := item.timing.summarize()
		if err != nil {
			return loadquality.CurationMeasurements{}, fmt.Errorf("summarize %s: %w", item.name, err)
		}
		item.set(&out, stats)
	}
	return out, nil
}

func provisionMemoryCurationLoadPrincipals(
	ctx context.Context,
	st *Store,
	seed int64,
	agentCount int,
) ([]Principal, error) {
	if agentCount < 2 {
		return nil, errors.New("memory curation load requires at least two synthetic agents")
	}
	seedToken := memoryCurationLoadToken(seed, "accounts")
	type syntheticAccount struct {
		accountID string
		realm     Realm
	}
	accounts := make([]syntheticAccount, 0, 2)
	for index := 0; index < 2; index++ {
		provisioned, err := st.ProvisionAccount(
			ctx,
			fmt.Sprintf("memory-curation-load-%s-%d@example.invalid", seedToken, index),
			fmt.Sprintf("memory curation load %s %d", seedToken, index),
			time.Hour,
		)
		if err != nil {
			return nil, fmt.Errorf("provision synthetic curation account %d: %w", index, err)
		}
		activated, err := st.ActivateAccount(ctx, provisioned.AccountID)
		if err != nil {
			return nil, fmt.Errorf("activate synthetic curation account %d: %w", index, err)
		}
		if !activated {
			return nil, fmt.Errorf("activate synthetic curation account %d: account was not activated", index)
		}
		realm, err := st.CreateRealm(ctx, provisioned.AccountID, "curation-load")
		if err != nil {
			return nil, fmt.Errorf("create synthetic curation realm %d: %w", index, err)
		}
		accounts = append(accounts, syntheticAccount{accountID: provisioned.AccountID, realm: realm})
	}

	principals := make([]Principal, 0, agentCount)
	for index := 0; index < agentCount; index++ {
		account := accounts[index%len(accounts)]
		agent, err := st.CreateAgent(
			ctx, account.accountID, account.realm.ID,
			fmt.Sprintf("curation-load-%03d-%s", index, memoryCurationLoadToken(seed, fmt.Sprint(index))),
		)
		if err != nil {
			return nil, fmt.Errorf("create synthetic curation agent %d: %w", index, err)
		}
		principals = append(principals, Principal{
			Kind: PrincipalAgent, ID: agent.ID, AccountID: account.accountID,
			RealmID: account.realm.ID, AgentName: agent.Name, RealmName: account.realm.Name,
			AccountStatus: "active",
		})
	}
	return principals, nil
}

func memoryCurationLoadToken(seed int64, parts ...string) string {
	input := fmt.Sprintf("%d", seed)
	for _, part := range parts {
		input += "\x00" + part
	}
	digest := sha256.Sum256([]byte(input))
	return fmt.Sprintf("%x", digest[:6])
}

func runMemoryCurationRequestCoalescing(
	ctx context.Context,
	st *Store,
	p Principal,
	opts loadquality.CurationOptions,
	execution *memoryCurationLoadExecution,
) error {
	type result struct {
		request  MemoryCurationRequest
		duration time.Duration
		err      error
	}
	start := make(chan struct{})
	results := make(chan result, opts.CoalescingRequests)
	var workers sync.WaitGroup
	for index := 0; index < opts.CoalescingRequests; index++ {
		workers.Add(1)
		go func(index int) {
			defer workers.Done()
			<-start
			operationStarted := time.Now()
			requested, err := st.RequestCuration(ctx, p, RequestMemoryCurationInput{
				Scope: MemoryCurationScope{
					Sources: []string{MemoryCurationSourceMemory},
				},
				CoalescingKey: "load_coalescing", TriggerReason: "load_test",
				MaxAttempts:    opts.MaxAttempts,
				IdempotencyKey: fmt.Sprintf("curation-load-coalesce-%d-%d", opts.Seed, index),
			})
			results <- result{request: requested.Request, duration: time.Since(operationStarted), err: err}
		}(index)
	}
	wallStarted := time.Now()
	close(start)
	workers.Wait()
	wall := time.Since(wallStarted)
	close(results)

	durations := make([]time.Duration, 0, opts.CoalescingRequests)
	requestIDs := make(map[string]struct{})
	for item := range results {
		durations = append(durations, item.duration)
		if item.err != nil {
			return fmt.Errorf("request coalescing: %w", item.err)
		}
		requestIDs[item.request.ID] = struct{}{}
	}
	execution.measurements.requestCoalescing.addConcurrent(durations, wall)
	page, err := st.ListCurationRequests(ctx, p, MemoryCurationRequestListOptions{
		State: MemoryCurationRequestQueued, Limit: maxMemoryCurationPageSize,
	})
	if err != nil {
		return fmt.Errorf("read coalesced queue depth: %w", err)
	}
	created := len(requestIDs)
	coalesced := opts.CoalescingRequests - created
	outcome := loadquality.CurationRequestCoalescingOutcome{
		Calls: opts.CoalescingRequests, Created: created, Coalesced: coalesced,
		QueueDepth:      len(page.Requests),
		CoalescingRatio: loadquality.CurationRatio(coalesced, opts.CoalescingRequests),
	}
	outcome.AllCoalesced = created == 1 && len(page.Requests) == 1 && page.NextCursor == ""
	if !outcome.AllCoalesced {
		return fmt.Errorf("request coalescing produced %d rows at queue depth %d", created, len(page.Requests))
	}
	execution.outcomes.RequestCoalescing = outcome
	return nil
}

func runMemoryCurationClaimContention(
	ctx context.Context,
	st *Store,
	principals []Principal,
	opts loadquality.CurationOptions,
	execution *memoryCurationLoadExecution,
) error {
	requests := make([]memoryCurationLoadRequest, 0, len(principals))
	for index, p := range principals {
		requested, err := st.RequestCuration(ctx, p, RequestMemoryCurationInput{
			Scope:         MemoryCurationScope{Sources: []string{MemoryCurationSourceMemory}},
			CoalescingKey: "claim_contention", TriggerReason: "load_test",
			MaxAttempts:    opts.MaxAttempts,
			IdempotencyKey: fmt.Sprintf("curation-load-claim-request-%d-%d", opts.Seed, index),
		})
		if err != nil {
			return fmt.Errorf("create claim-contention request %d: %w", index, err)
		}
		requests = append(requests, memoryCurationLoadRequest{principal: p, request: requested.Request})
	}

	type attempt struct {
		requestIndex int
		workerIndex  int
		started      StartMemoryCurationResult
		duration     time.Duration
		err          error
	}
	attemptCount := len(requests) * opts.ClaimWorkers
	results := make(chan attempt, attemptCount)
	start := make(chan struct{})
	var workers sync.WaitGroup
	for workerIndex := 0; workerIndex < opts.ClaimWorkers; workerIndex++ {
		workers.Add(1)
		go func(workerIndex int) {
			defer workers.Done()
			<-start
			// Every worker traverses the requests in the same order, so the
			// barrier release aims all workers at one lane simultaneously and
			// the single-winner check exercises a genuine same-row race rather
			// than staggered lanes that only collide through scheduling drift.
			for offset := 0; offset < len(requests); offset++ {
				requestIndex := offset
				item := requests[requestIndex]
				operationStarted := time.Now()
				started, err := st.StartCuration(ctx, item.principal, StartMemoryCurationInput{
					RequestID: item.request.ID, LeaseDuration: minMemoryCurationLease,
					Client: MemoryClientProvenance{
						Runtime: "curation-load", Recipe: loadquality.CurationResultSchemaV1,
						RecipeVersion: loadquality.CurationHarnessVersion,
					},
					IdempotencyKey: fmt.Sprintf(
						"curation-load-claim-start-%d-%d-%d", opts.Seed, requestIndex, workerIndex,
					),
				})
				results <- attempt{
					requestIndex: requestIndex, workerIndex: workerIndex, started: started,
					duration: time.Since(operationStarted), err: err,
				}
			}
		}(workerIndex)
	}
	wallStarted := time.Now()
	close(start)
	workers.Wait()
	wall := time.Since(wallStarted)
	close(results)

	allDurations := make([]time.Duration, 0, attemptCount)
	winners := make(map[int]StartMemoryCurationResult, len(requests))
	wins, losses := 0, 0
	for item := range results {
		allDurations = append(allDurations, item.duration)
		if item.err == nil {
			if _, duplicate := winners[item.requestIndex]; duplicate {
				return fmt.Errorf("claim contention request %d had multiple winners", item.requestIndex)
			}
			winners[item.requestIndex] = item.started
			wins++
			continue
		}
		if !errors.Is(item.err, ErrMemoryCurationBusy) {
			return fmt.Errorf(
				"claim contention request %d worker %d refusal: %w",
				item.requestIndex, item.workerIndex, item.err,
			)
		}
		losses++
	}
	execution.measurements.claimStart.addConcurrent(allDurations, wall)

	for requestIndex := range requests {
		started, ok := winners[requestIndex]
		if !ok {
			return fmt.Errorf("claim contention request %d had no winner", requestIndex)
		}
		item := requests[requestIndex]
		if _, err := st.CancelCuration(ctx, item.principal, started.Run.ID, FinishMemoryCurationInput{
			FencingGeneration: started.Run.FencingGeneration, Reason: "load_cleanup",
			IdempotencyKey: fmt.Sprintf("curation-load-claim-cancel-%d-%d", opts.Seed, requestIndex),
		}); err != nil {
			return fmt.Errorf("cancel claim-contention winner %d: %w", requestIndex, err)
		}
		operationStarted := time.Now()
		_, err := st.GetCurationRunInputs(
			ctx, item.principal, started.Run.ID, started.Run.FencingGeneration, "", 1,
		)
		duration := time.Since(operationStarted)
		execution.measurements.typedRefusal.add(duration)
		if !errors.Is(err, ErrMemoryCurationFenceMismatch) {
			return fmt.Errorf("cancelled claim winner %d stale-fence error: %w", requestIndex, err)
		}
		execution.outcomes.StalePlanConflict.StaleFenceRefusals++
		execution.outcomes.StalePlanConflict.TypedRefusals++
	}

	outcome := loadquality.CurationClaimContentionOutcome{
		Requests: len(requests), Attempts: attemptCount, Wins: wins, Losses: losses,
		WinRate:  loadquality.CurationRatio(wins, attemptCount),
		LossRate: loadquality.CurationRatio(losses, attemptCount),
	}
	outcome.SingleWinnerPerRequest = wins == len(requests) &&
		losses == attemptCount-len(requests) && len(winners) == len(requests)
	if !outcome.SingleWinnerPerRequest {
		return fmt.Errorf("claim contention wins/losses = %d/%d for %d requests", wins, losses, len(requests))
	}
	execution.outcomes.ClaimContention = outcome
	return nil
}

func runMemoryCurationInputPaging(
	ctx context.Context,
	st *Store,
	principals []Principal,
	opts loadquality.CurationOptions,
	execution *memoryCurationLoadExecution,
) error {
	if len(principals) != len(opts.PagingCardinalities) {
		return errors.New("input paging principal/cardinality mismatch")
	}
	outcome := loadquality.CurationInputPagingOutcome{Runs: len(principals)}
	for runIndex, cardinality := range opts.PagingCardinalities {
		p := principals[runIndex]
		if err := seedMemoryCurationPagingSources(
			ctx, st, p, opts.Seed, runIndex, cardinality,
		); err != nil {
			return err
		}
		scope := MemoryCurationScope{
			Sources: []string{
				MemoryCurationSourceMemory,
				MemoryCurationSourceEvidence,
				MemoryCurationSourceTranscript,
			},
			MemoryStates:         []string{MemoryStateActive},
			MaxMemories:          cardinality,
			MaxEvidence:          cardinality,
			MaxTranscriptEntries: cardinality,
		}
		requested, err := st.RequestCuration(ctx, p, RequestMemoryCurationInput{
			Scope: scope, CoalescingKey: fmt.Sprintf("paging_%d", runIndex),
			TriggerReason: "load_test", MaxAttempts: opts.MaxAttempts,
			IdempotencyKey: fmt.Sprintf("curation-load-paging-request-%d-%d", opts.Seed, runIndex),
		})
		if err != nil {
			return fmt.Errorf("request paging run %d: %w", runIndex, err)
		}
		started, err := st.StartCuration(ctx, p, StartMemoryCurationInput{
			RequestID: requested.Request.ID,
			Caps: MemoryCurationInputCaps{
				MaxMemories: cardinality, MaxEvidence: cardinality,
				MaxTranscriptEntries: cardinality,
			},
			LeaseDuration: minMemoryCurationLease,
			Client: MemoryClientProvenance{
				Runtime: "curation-load", Recipe: loadquality.CurationResultSchemaV1,
				RecipeVersion: loadquality.CurationHarnessVersion,
			},
			IdempotencyKey: fmt.Sprintf("curation-load-paging-start-%d-%d", opts.Seed, runIndex),
		})
		if err != nil {
			return fmt.Errorf("start paging run %d: %w", runIndex, err)
		}
		if started.Run.MemoryInputCount != cardinality ||
			started.Run.EvidenceInputCount != cardinality ||
			started.Run.TranscriptInputCount != cardinality ||
			started.Run.CursorInputCount != cardinality+2 ||
			started.Run.InputCount != 4*cardinality+2 {
			return fmt.Errorf(
				"paging run %d frozen class counts memory/evidence/transcript/cursor/total = %d/%d/%d/%d/%d",
				runIndex, started.Run.MemoryInputCount, started.Run.EvidenceInputCount,
				started.Run.TranscriptInputCount, started.Run.CursorInputCount, started.Run.InputCount,
			)
		}

		seenOrdinals := make(map[int64]struct{}, started.Run.InputCount)
		cursor := started.FirstInputCursor
		for {
			operationStarted := time.Now()
			page, pageErr := st.GetCurationRunInputs(
				ctx, p, started.Run.ID, started.Run.FencingGeneration, cursor, opts.PageSize,
			)
			duration := time.Since(operationStarted)
			execution.measurements.inputPage.add(duration)
			outcome.Pages++
			if pageErr != nil {
				return fmt.Errorf("page curation run %d: %w", runIndex, pageErr)
			}
			if page.Run.ID != started.Run.ID || page.Run.FencingGeneration != started.Run.FencingGeneration {
				return fmt.Errorf("paging run %d returned mismatched run metadata", runIndex)
			}
			for _, input := range page.Inputs {
				if _, duplicate := seenOrdinals[input.Ordinal]; duplicate {
					outcome.DuplicateInputs++
				} else {
					seenOrdinals[input.Ordinal] = struct{}{}
				}
				outcome.Inputs++
			}
			if page.NextCursor == "" {
				break
			}
			cursor = page.NextCursor
			// Keep-alive: long sequential paging under large knob shapes can
			// outlive the un-renewed minimum lease on a loaded host. The renew
			// is deliberately not measured; it is plumbing, not the paged
			// operation under test.
			if outcome.Pages%16 == 0 {
				if _, renewErr := st.RenewCuration(ctx, p, started.Run.ID, RenewMemoryCurationInput{
					FencingGeneration: started.Run.FencingGeneration,
					Extension:         minMemoryCurationLease,
					IdempotencyKey: fmt.Sprintf(
						"curation-load-paging-keepalive-%d-%d-%d", opts.Seed, runIndex, outcome.Pages,
					),
				}); renewErr != nil {
					return fmt.Errorf("keep-alive renew paging run %d: %w", runIndex, renewErr)
				}
			}
		}
		if len(seenOrdinals) != started.Run.InputCount {
			return fmt.Errorf(
				"paging run %d traversed %d inputs, want frozen count %d",
				runIndex, len(seenOrdinals), started.Run.InputCount,
			)
		}
		outcome.ExhaustedRuns++
		if _, err := st.CancelCuration(ctx, p, started.Run.ID, FinishMemoryCurationInput{
			FencingGeneration: started.Run.FencingGeneration, Reason: "load_cleanup",
			IdempotencyKey: fmt.Sprintf("curation-load-paging-cancel-%d-%d", opts.Seed, runIndex),
		}); err != nil {
			return fmt.Errorf("cancel paging run %d: %w", runIndex, err)
		}
	}
	outcome.PagedToExhaustion = outcome.ExhaustedRuns == outcome.Runs && outcome.DuplicateInputs == 0
	if !outcome.PagedToExhaustion {
		return fmt.Errorf("input paging exhausted %d/%d runs with %d duplicates", outcome.ExhaustedRuns, outcome.Runs, outcome.DuplicateInputs)
	}
	execution.outcomes.InputPaging = outcome
	return nil
}

func seedMemoryCurationPagingSources(
	ctx context.Context,
	st *Store,
	p Principal,
	seed int64,
	runIndex int,
	cardinality int,
) error {
	kinds := [...]string{"session", "milestone", "decision", "lesson"}
	for index := 0; index < cardinality; index++ {
		transcript, err := st.CreateTranscript(ctx, p.AccountID, p.RealmID, p.ID,
			CreateTranscriptInput{ExternalID: fmt.Sprintf(
				"curation-load-paging-%s-%d-%d",
				memoryCurationLoadToken(seed, "paging", fmt.Sprint(runIndex), fmt.Sprint(index)),
				runIndex, index,
			)})
		if err != nil {
			return fmt.Errorf("create paging transcript %d/%d: %w", runIndex, index, err)
		}
		entry, err := st.AppendTranscriptEntry(ctx, p.AccountID, p.RealmID, p.ID,
			transcript.ID, AppendTranscriptEntryInput{
				ExternalID: fmt.Sprintf("entry-%d", index), Role: TranscriptRoleUser,
				Body: fmt.Sprintf(
					"Synthetic curation paging fixture %s at ordinal %d.",
					memoryCurationLoadToken(seed, "paging-body", fmt.Sprint(runIndex), fmt.Sprint(index)),
					index,
				),
			})
		if err != nil {
			return fmt.Errorf("append paging transcript %d/%d: %w", runIndex, index, err)
		}
		if _, err := st.CaptureMemory(ctx, p, CaptureMemoryInput{
			Content: fmt.Sprintf(
				"Synthetic curation memory fixture %s at ordinal %d.",
				memoryCurationLoadToken(seed, "paging-memory", fmt.Sprint(runIndex), fmt.Sprint(index)),
				index,
			),
			Kind: kinds[index%len(kinds)], CaptureReason: "load_quality",
			Evidence: []MemoryEvidenceInput{{
				Type: "conversation", ResolutionState: MemoryEvidenceResolved,
				ResolvedKind:       MemoryCurationSourceTranscript,
				SourceTranscriptID: transcript.ID, SourceSequenceFrom: entry.Sequence,
				SourceSequenceUntil: entry.Sequence,
			}},
			Client: MemoryClientProvenance{
				Runtime: "curation-load", Recipe: loadquality.CurationResultSchemaV1,
				RecipeVersion: loadquality.CurationHarnessVersion,
			},
			IdempotencyKey: fmt.Sprintf("curation-load-paging-memory-%d-%d-%d", seed, runIndex, index),
		}); err != nil {
			return fmt.Errorf("capture paging memory %d/%d: %w", runIndex, index, err)
		}
	}
	return nil
}

func getMemoryCurationLoadInputs(
	ctx context.Context,
	st *Store,
	p Principal,
	started StartMemoryCurationResult,
	pageSize int,
) ([]MemoryCurationRunInput, error) {
	inputs := make([]MemoryCurationRunInput, 0, started.Run.InputCount)
	cursor := started.FirstInputCursor
	seenCursors := make(map[string]struct{})
	for {
		if _, duplicate := seenCursors[cursor]; duplicate {
			return nil, errors.New("curation input paging returned a repeated cursor")
		}
		seenCursors[cursor] = struct{}{}
		page, err := st.GetCurationRunInputs(
			ctx, p, started.Run.ID, started.Run.FencingGeneration, cursor, pageSize,
		)
		if err != nil {
			return nil, err
		}
		inputs = append(inputs, page.Inputs...)
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
		// Keep-alive on the same schedule as the paging workload so a large
		// chain backlog cannot outlive the un-renewed minimum lease.
		if len(seenCursors)%16 == 0 {
			if _, renewErr := st.RenewCuration(ctx, p, started.Run.ID, RenewMemoryCurationInput{
				FencingGeneration: started.Run.FencingGeneration,
				Extension:         minMemoryCurationLease,
				IdempotencyKey: fmt.Sprintf(
					"curation-load-read-keepalive-%s-%d", started.Run.ID, len(seenCursors),
				),
			}); renewErr != nil {
				return nil, fmt.Errorf("keep-alive renew input read: %w", renewErr)
			}
		}
	}
	if len(inputs) != started.Run.InputCount {
		return nil, fmt.Errorf("read %d curation inputs, want %d", len(inputs), started.Run.InputCount)
	}
	return inputs, nil
}

func memoryCurationLoadCreateDraft(
	seed int64,
	label string,
	ordinal int,
	inputs []MemoryCurationRunInput,
) (json.RawMessage, error) {
	var transcriptInput *MemoryCurationRunInput
	for index := range inputs {
		if inputs[index].Kind == MemoryCurationSourceTranscript {
			transcriptInput = &inputs[index]
			break
		}
	}
	if transcriptInput == nil || transcriptInput.TranscriptID == "" {
		return nil, errors.New("create plan has no frozen transcript input")
	}
	sequence := transcriptInput.SequenceFrom
	if sequence < 1 && len(transcriptInput.TranscriptEntries) > 0 {
		sequence = transcriptInput.TranscriptEntries[0].Sequence
	}
	if sequence < transcriptInput.SequenceFrom || sequence > transcriptInput.SequenceUntil {
		return nil, fmt.Errorf(
			"create plan evidence sequence %d is outside frozen window %d-%d",
			sequence, transcriptInput.SequenceFrom, transcriptInput.SequenceUntil,
		)
	}
	kinds := [...]string{"session", "milestone", "decision", "lesson"}
	draft := MemoryCurationPlanDraft{
		Schema: MemoryCurationPlanSchemaV1, DraftRevision: 1,
		Actions: []MemoryCurationPlanAction{{
			Ordinal: 1, Operation: MemoryCurationOperationCreate,
			Create: &MemoryCurationCreateAction{
				LocalRef: fmt.Sprintf("load_%d", ordinal),
				Snapshot: MemoryCurationMemorySnapshot{
					Content: fmt.Sprintf(
						"Synthetic curation result %s.",
						memoryCurationLoadToken(seed, label, fmt.Sprint(ordinal)),
					),
					Kind: kinds[ordinal%len(kinds)],
					Evidence: []MemoryCurationEvidence{{
						Type: "conversation", ResolutionState: MemoryEvidenceResolved,
						ResolvedKind:       MemoryCurationSourceTranscript,
						SourceTranscriptID: transcriptInput.TranscriptID,
						SourceSequenceFrom: sequence, SourceSequenceUntil: sequence,
					}},
				},
			},
		}},
	}
	raw, err := json.Marshal(draft)
	if err != nil {
		return nil, fmt.Errorf("marshal create curation plan: %w", err)
	}
	return raw, nil
}

func seedMemoryCurationLoadTranscript(
	ctx context.Context,
	st *Store,
	p Principal,
	seed int64,
	label string,
	entryCount int,
) (string, error) {
	transcript, err := st.CreateTranscript(ctx, p.AccountID, p.RealmID, p.ID,
		CreateTranscriptInput{ExternalID: fmt.Sprintf(
			"curation-load-%s-%s", label, memoryCurationLoadToken(seed, label),
		)})
	if err != nil {
		return "", fmt.Errorf("create %s transcript: %w", label, err)
	}
	for index := 0; index < entryCount; index++ {
		if _, err := st.AppendTranscriptEntry(ctx, p.AccountID, p.RealmID, p.ID,
			transcript.ID, AppendTranscriptEntryInput{
				ExternalID: fmt.Sprintf("%s-entry-%d", label, index),
				Role:       TranscriptRoleUser,
				Body: fmt.Sprintf(
					"Synthetic %s curation backlog entry %s at ordinal %d.",
					label, memoryCurationLoadToken(seed, label, fmt.Sprint(index)), index,
				),
			}); err != nil {
			return "", fmt.Errorf("append %s transcript entry %d: %w", label, index, err)
		}
	}
	return transcript.ID, nil
}

func runMemoryCurationPlanLifecycle(
	ctx context.Context,
	st *Store,
	p Principal,
	opts loadquality.CurationOptions,
	execution *memoryCurationLoadExecution,
) error {
	if _, err := seedMemoryCurationLoadTranscript(
		ctx, st, p, opts.Seed, "lifecycle", opts.ChainBacklog,
	); err != nil {
		return err
	}
	scope := MemoryCurationScope{
		Sources:              []string{MemoryCurationSourceTranscript},
		MaxTranscriptEntries: opts.ChainCap,
	}
	requested, err := st.RequestCuration(ctx, p, RequestMemoryCurationInput{
		Scope: scope, CoalescingKey: "lifecycle", TriggerReason: "load_test",
		MaxAttempts:    opts.MaxAttempts,
		IdempotencyKey: fmt.Sprintf("curation-load-lifecycle-request-%d", opts.Seed),
	})
	if err != nil {
		return fmt.Errorf("request lifecycle curation: %w", err)
	}

	expectedDepth, err := loadquality.CurationChainDepth(opts.ChainBacklog, opts.ChainCap)
	if err != nil {
		return fmt.Errorf("compute lifecycle chain depth: %w", err)
	}
	outcome := loadquality.CurationPlanLifecycleOutcome{}
	request := requested.Request
	for depth := 1; ; depth++ {
		if depth > expectedDepth {
			return fmt.Errorf("lifecycle follow-up chain exceeded expected depth %d", expectedDepth)
		}
		started, err := st.StartCuration(ctx, p, StartMemoryCurationInput{
			RequestID:     request.ID,
			Caps:          MemoryCurationInputCaps{MaxTranscriptEntries: opts.ChainCap},
			LeaseDuration: minMemoryCurationLease,
			Client: MemoryClientProvenance{
				Runtime: "curation-load", Recipe: loadquality.CurationResultSchemaV1,
				RecipeVersion: loadquality.CurationHarnessVersion,
			},
			IdempotencyKey: fmt.Sprintf("curation-load-lifecycle-start-%d-%d", opts.Seed, depth),
		})
		if err != nil {
			return fmt.Errorf("start lifecycle depth %d: %w", depth, err)
		}
		inputs, err := getMemoryCurationLoadInputs(ctx, st, p, started, opts.PageSize)
		if err != nil {
			return fmt.Errorf("read lifecycle depth %d inputs: %w", depth, err)
		}

		empty := depth%2 == 1
		draft := append(json.RawMessage(nil), exactEmptyMemoryCurationPlan...)
		if !empty {
			draft, err = memoryCurationLoadCreateDraft(opts.Seed, "lifecycle", depth, inputs)
			if err != nil {
				return fmt.Errorf("build lifecycle depth %d plan: %w", depth, err)
			}
		}
		operationStarted := time.Now()
		planned, err := st.PlanCuration(ctx, p, started.Run.ID, PlanMemoryCurationInput{
			FencingGeneration: started.Run.FencingGeneration, Draft: draft,
			IdempotencyKey: fmt.Sprintf("curation-load-lifecycle-plan-%d-%d", opts.Seed, depth),
		})
		execution.measurements.plan.add(time.Since(operationStarted))
		if err != nil {
			return fmt.Errorf("plan lifecycle depth %d: %w", depth, err)
		}
		outcome.Plans++

		operationStarted = time.Now()
		stored, err := st.GetCurationPlan(
			ctx, p, started.Run.ID, started.Run.FencingGeneration,
		)
		execution.measurements.planGet.add(time.Since(operationStarted))
		if err != nil {
			return fmt.Errorf("get lifecycle depth %d plan: %w", depth, err)
		}
		outcome.PlanGets++
		if stored.Run.PlanRevision != planned.Plan.PlanRevision ||
			stored.Run.PlanHash != planned.Receipt.PlanHash ||
			len(stored.Plan.Actions) != len(planned.Plan.Actions) {
			return fmt.Errorf("stored lifecycle depth %d plan does not match acceptance", depth)
		}

		operationStarted = time.Now()
		applied, err := st.ApplyCuration(ctx, p, started.Run.ID, ApplyMemoryCurationInput{
			FencingGeneration: started.Run.FencingGeneration,
			PlanRevision:      stored.Run.PlanRevision, PlanHash: stored.Run.PlanHash,
			IdempotencyKey: fmt.Sprintf("curation-load-lifecycle-apply-%d-%d", opts.Seed, depth),
		})
		execution.measurements.apply.add(time.Since(operationStarted))
		if err != nil {
			return fmt.Errorf("apply lifecycle depth %d: %w", depth, err)
		}
		outcome.Applies++
		if empty {
			outcome.EmptyApplies++
			if len(applied.Receipt.CursorIntervals) == 0 {
				return fmt.Errorf("empty lifecycle apply at depth %d did not advance a cursor", depth)
			}
			outcome.EmptyCursorAdvances++
		} else {
			outcome.CreateApplies++
			outcome.CreateActions++
			if len(applied.Receipt.ActionResults) != 1 ||
				len(applied.Receipt.ActionResults[0].CreatedMemoryIDs) != 1 {
				return fmt.Errorf("create lifecycle apply at depth %d did not create exactly one memory", depth)
			}
		}

		outcome.MaxChainDepth = depth
		if applied.FollowUpRequest == nil {
			if depth != expectedDepth {
				return fmt.Errorf("lifecycle drained at depth %d, want %d", depth, expectedDepth)
			}
			outcome.DrainedChains = 1
			outcome.BacklogDrained = true
			execution.chainDepth = depth
			break
		}
		outcome.FollowUpRequests++
		request = *applied.FollowUpRequest
	}
	outcome.EmptyPlanAdvancedCursors = outcome.EmptyCursorAdvances == outcome.EmptyApplies
	if !outcome.EmptyPlanAdvancedCursors {
		return errors.New("not every empty lifecycle plan advanced its frozen cursors")
	}
	execution.outcomes.PlanLifecycle = outcome
	return nil
}

func runMemoryCurationLeaseChurn(
	ctx context.Context,
	st *Store,
	principals []Principal,
	opts loadquality.CurationOptions,
	execution *memoryCurationLoadExecution,
) error {
	outcome := loadquality.CurationLeaseChurnOutcome{Cycles: len(principals)}
	for cycle, p := range principals {
		requested, err := st.RequestCuration(ctx, p, RequestMemoryCurationInput{
			Scope:         MemoryCurationScope{Sources: []string{MemoryCurationSourceMemory}},
			CoalescingKey: "lease_churn", TriggerReason: "load_test",
			MaxAttempts:    opts.MaxAttempts,
			IdempotencyKey: fmt.Sprintf("curation-load-lease-request-%d-%d", opts.Seed, cycle),
		})
		if err != nil {
			return fmt.Errorf("request lease cycle %d: %w", cycle, err)
		}
		started, err := st.StartCuration(ctx, p, StartMemoryCurationInput{
			RequestID: requested.Request.ID, LeaseDuration: minMemoryCurationLease,
			Client: MemoryClientProvenance{
				Runtime: "curation-load", Recipe: loadquality.CurationResultSchemaV1,
				RecipeVersion: loadquality.CurationHarnessVersion,
			},
			IdempotencyKey: fmt.Sprintf("curation-load-lease-start-%d-%d", opts.Seed, cycle),
		})
		if err != nil {
			return fmt.Errorf("start lease cycle %d: %w", cycle, err)
		}

		operationStarted := time.Now()
		renewed, err := st.RenewCuration(ctx, p, started.Run.ID, RenewMemoryCurationInput{
			FencingGeneration: started.Run.FencingGeneration,
			Extension:         minMemoryCurationLease,
			IdempotencyKey:    fmt.Sprintf("curation-load-lease-live-renew-%d-%d", opts.Seed, cycle),
		})
		execution.measurements.leaseRenew.add(time.Since(operationStarted))
		if err != nil {
			return fmt.Errorf("renew live lease cycle %d: %w", cycle, err)
		}
		if renewed.Run.State != MemoryCurationRunOpen {
			return fmt.Errorf("live lease cycle %d returned non-live run state %q", cycle, renewed.Run.State)
		}
		if renewed.Run.LeaseExpiresAt == nil || started.Run.LeaseExpiresAt == nil ||
			!renewed.Run.LeaseExpiresAt.After(*started.Run.LeaseExpiresAt) ||
			renewed.Receipt.LeaseExpiresAt == nil ||
			!renewed.Receipt.LeaseExpiresAt.Equal(*renewed.Run.LeaseExpiresAt) {
			return fmt.Errorf("live lease cycle %d did not extend its exact lease coordinate", cycle)
		}
		outcome.LiveRenewals++

		tag, err := st.pool.Exec(ctx, `
			UPDATE memory_curation_runs
			SET lease_expires_at=clock_timestamp()-interval '1 second'
			WHERE id=$1 AND state IN ('open','planned')`, started.Run.ID)
		if err != nil {
			return fmt.Errorf("expire lease cycle %d: %w", cycle, err)
		}
		if tag.RowsAffected() != 1 {
			return fmt.Errorf("expire lease cycle %d affected %d rows", cycle, tag.RowsAffected())
		}

		operationStarted = time.Now()
		expired, err := st.RenewCuration(ctx, p, started.Run.ID, RenewMemoryCurationInput{
			FencingGeneration: started.Run.FencingGeneration,
			Extension:         minMemoryCurationLease,
			IdempotencyKey:    fmt.Sprintf("curation-load-lease-expired-renew-%d-%d", opts.Seed, cycle),
		})
		execution.measurements.leaseRenew.add(time.Since(operationStarted))
		if !errors.Is(err, ErrMemoryCurationLeaseExpired) {
			return fmt.Errorf("expired lease cycle %d renew error: %w", cycle, err)
		}
		outcome.RenewAfterExpiry++
		if expired.Run.State != MemoryCurationRunInterrupted ||
			expired.Receipt.ResultState != MemoryCurationRunInterrupted ||
			expired.Run.TerminalReasonCode != "lease_expired" {
			return fmt.Errorf("expired lease cycle %d did not durably interrupt", cycle)
		}
		outcome.Reconciliations++
		persisted, err := st.GetCurationRequest(ctx, p, requested.Request.ID)
		if err != nil {
			return fmt.Errorf("read reconciled lease request %d: %w", cycle, err)
		}
		if persisted.State != MemoryCurationRequestRetryWait || persisted.AttemptCount != 1 {
			return fmt.Errorf(
				"reconciled lease request %d state/attempt = %s/%d",
				cycle, persisted.State, persisted.AttemptCount,
			)
		}
		outcome.Requeues++

		operationStarted = time.Now()
		_, err = st.GetCurationRunInputs(
			ctx, p, started.Run.ID, started.Run.FencingGeneration, "", 1,
		)
		execution.measurements.typedRefusal.add(time.Since(operationStarted))
		if !errors.Is(err, ErrMemoryCurationFenceMismatch) {
			return fmt.Errorf("expired lease cycle %d stale-fence error: %w", cycle, err)
		}
		outcome.StaleFenceRefusals++
		execution.outcomes.StalePlanConflict.StaleFenceRefusals++
		execution.outcomes.StalePlanConflict.TypedRefusals++
	}
	outcome.ExpiredRenewReconciled = outcome.RenewAfterExpiry == outcome.Cycles &&
		outcome.Reconciliations == outcome.Cycles && outcome.Requeues == outcome.Cycles
	if !outcome.ExpiredRenewReconciled {
		return errors.New("expired lease renewals did not all reconcile and requeue")
	}
	execution.outcomes.LeaseChurn = outcome
	return nil
}

func runMemoryCurationApplyRace(
	ctx context.Context,
	st *Store,
	p Principal,
	opts loadquality.CurationOptions,
	execution *memoryCurationLoadExecution,
) error {
	if _, err := seedMemoryCurationLoadTranscript(ctx, st, p, opts.Seed, "apply-race", 1); err != nil {
		return err
	}
	requested, err := st.RequestCuration(ctx, p, RequestMemoryCurationInput{
		Scope: MemoryCurationScope{
			Sources: []string{MemoryCurationSourceTranscript}, MaxTranscriptEntries: 1,
		},
		CoalescingKey: "apply_race", TriggerReason: "load_test",
		MaxAttempts:    opts.MaxAttempts,
		IdempotencyKey: fmt.Sprintf("curation-load-apply-race-request-%d", opts.Seed),
	})
	if err != nil {
		return fmt.Errorf("request apply race: %w", err)
	}
	started, err := st.StartCuration(ctx, p, StartMemoryCurationInput{
		RequestID:     requested.Request.ID,
		Caps:          MemoryCurationInputCaps{MaxTranscriptEntries: 1},
		LeaseDuration: minMemoryCurationLease,
		Client: MemoryClientProvenance{
			Runtime: "curation-load", Recipe: loadquality.CurationResultSchemaV1,
			RecipeVersion: loadquality.CurationHarnessVersion,
		},
		IdempotencyKey: fmt.Sprintf("curation-load-apply-race-start-%d", opts.Seed),
	})
	if err != nil {
		return fmt.Errorf("start apply race: %w", err)
	}
	inputs, err := getMemoryCurationLoadInputs(ctx, st, p, started, opts.PageSize)
	if err != nil {
		return fmt.Errorf("read apply-race inputs: %w", err)
	}
	draft, err := memoryCurationLoadCreateDraft(opts.Seed, "apply-race", 1, inputs)
	if err != nil {
		return err
	}
	planned, err := st.PlanCuration(ctx, p, started.Run.ID, PlanMemoryCurationInput{
		FencingGeneration: started.Run.FencingGeneration, Draft: draft,
		IdempotencyKey: fmt.Sprintf("curation-load-apply-race-plan-%d", opts.Seed),
	})
	if err != nil {
		return fmt.Errorf("plan apply race: %w", err)
	}
	stored, err := st.GetCurationPlan(ctx, p, started.Run.ID, started.Run.FencingGeneration)
	if err != nil {
		return fmt.Errorf("get apply-race plan: %w", err)
	}
	if stored.Run.PlanRevision != planned.Plan.PlanRevision || stored.Run.PlanHash != planned.Receipt.PlanHash {
		return errors.New("stored apply-race plan does not match acceptance")
	}

	type result struct {
		applied  ApplyMemoryCurationResult
		duration time.Duration
		err      error
	}
	start := make(chan struct{})
	results := make(chan result, opts.ClaimWorkers)
	var workers sync.WaitGroup
	for worker := 0; worker < opts.ClaimWorkers; worker++ {
		workers.Add(1)
		go func(worker int) {
			defer workers.Done()
			<-start
			operationStarted := time.Now()
			applied, err := st.ApplyCuration(ctx, p, started.Run.ID, ApplyMemoryCurationInput{
				FencingGeneration: started.Run.FencingGeneration,
				PlanRevision:      stored.Run.PlanRevision, PlanHash: stored.Run.PlanHash,
				IdempotencyKey: fmt.Sprintf("curation-load-apply-race-%d-%d", opts.Seed, worker),
			})
			results <- result{applied: applied, duration: time.Since(operationStarted), err: err}
		}(worker)
	}
	wallStarted := time.Now()
	close(start)
	workers.Wait()
	wall := time.Since(wallStarted)
	close(results)

	durations := make([]time.Duration, 0, opts.ClaimWorkers)
	wins, refusals := 0, 0
	for item := range results {
		durations = append(durations, item.duration)
		if item.err == nil {
			wins++
			if item.applied.Run.State != MemoryCurationRunApplied ||
				len(item.applied.Receipt.ActionResults) != 1 ||
				len(item.applied.Receipt.ActionResults[0].CreatedMemoryIDs) != 1 {
				return errors.New("apply-race winner did not produce exactly one memory")
			}
			continue
		}
		if !errors.Is(item.err, ErrMemoryCurationFenceMismatch) &&
			!errors.Is(item.err, ErrMemoryCurationConflict) {
			return fmt.Errorf("apply-race refusal was not typed: %w", item.err)
		}
		refusals++
	}
	execution.measurements.leaseApplyRace.addConcurrent(durations, wall)

	var produced int
	if err := st.pool.QueryRow(ctx, `
		SELECT count(*) FROM memory_versions WHERE curation_run_id=$1`,
		started.Run.ID).Scan(&produced); err != nil {
		return fmt.Errorf("count apply-race memory writes: %w", err)
	}
	outcome := execution.outcomes.LeaseChurn
	outcome.ApplyRaceAttempts = opts.ClaimWorkers
	outcome.ApplyRaceWins = wins
	outcome.ApplyRaceRefusals = refusals
	if wins > 1 {
		outcome.DoubleApplySuccesses = wins - 1
	}
	outcome.NoDoubleApply = wins == 1 && produced == 1
	if !outcome.NoDoubleApply || refusals != opts.ClaimWorkers-1 {
		return fmt.Errorf(
			"apply race wins/refusals/writes = %d/%d/%d, want 1/%d/1",
			wins, refusals, produced, opts.ClaimWorkers-1,
		)
	}
	execution.outcomes.LeaseChurn = outcome
	return nil
}

func wrongMemoryCurationLoadPlanHash(planHash string) string {
	if strings.HasPrefix(planHash, "0") {
		return "1" + planHash[1:]
	}
	return "0" + planHash[1:]
}

func runMemoryCurationStalePlanConflict(
	ctx context.Context,
	st *Store,
	p Principal,
	opts loadquality.CurationOptions,
	execution *memoryCurationLoadExecution,
) error {
	requested, err := st.RequestCuration(ctx, p, RequestMemoryCurationInput{
		Scope:         MemoryCurationScope{Sources: []string{MemoryCurationSourceMemory}},
		CoalescingKey: "stale_plan", TriggerReason: "load_test",
		MaxAttempts:    opts.MaxAttempts,
		IdempotencyKey: fmt.Sprintf("curation-load-conflict-request-%d", opts.Seed),
	})
	if err != nil {
		return fmt.Errorf("request stale-plan workload: %w", err)
	}
	started, err := st.StartCuration(ctx, p, StartMemoryCurationInput{
		RequestID: requested.Request.ID, LeaseDuration: minMemoryCurationLease,
		Client: MemoryClientProvenance{
			Runtime: "curation-load", Recipe: loadquality.CurationResultSchemaV1,
			RecipeVersion: loadquality.CurationHarnessVersion,
		},
		IdempotencyKey: fmt.Sprintf("curation-load-conflict-start-%d", opts.Seed),
	})
	if err != nil {
		return fmt.Errorf("start stale-plan workload: %w", err)
	}
	planned, err := st.PlanCuration(ctx, p, started.Run.ID, PlanMemoryCurationInput{
		FencingGeneration: started.Run.FencingGeneration,
		Draft:             append(json.RawMessage(nil), exactEmptyMemoryCurationPlan...),
		IdempotencyKey:    fmt.Sprintf("curation-load-conflict-plan-%d", opts.Seed),
	})
	if err != nil {
		return fmt.Errorf("plan stale-plan workload: %w", err)
	}
	stored, err := st.GetCurationPlan(ctx, p, started.Run.ID, started.Run.FencingGeneration)
	if err != nil {
		return fmt.Errorf("get stale-plan workload plan: %w", err)
	}
	if stored.Run.PlanHash != planned.Receipt.PlanHash || stored.Run.PlanRevision != planned.Plan.PlanRevision {
		return errors.New("stored stale-plan workload plan does not match acceptance")
	}
	if len(stored.Run.PlanHash) != sha256.Size*2 {
		return errors.New("stored stale-plan workload plan has an invalid hash")
	}

	operationStarted := time.Now()
	_, err = st.ApplyCuration(ctx, p, started.Run.ID, ApplyMemoryCurationInput{
		FencingGeneration: started.Run.FencingGeneration,
		PlanRevision:      stored.Run.PlanRevision,
		PlanHash:          wrongMemoryCurationLoadPlanHash(stored.Run.PlanHash),
		IdempotencyKey:    fmt.Sprintf("curation-load-conflict-wrong-hash-%d", opts.Seed),
	})
	execution.measurements.typedRefusal.add(time.Since(operationStarted))
	if !errors.Is(err, ErrMemoryCurationConflict) {
		return fmt.Errorf("wrong plan hash refusal: %w", err)
	}
	execution.outcomes.StalePlanConflict.WrongPlanHashRefusals++
	execution.outcomes.StalePlanConflict.TypedRefusals++

	operationStarted = time.Now()
	_, err = st.PlanCuration(ctx, p, started.Run.ID, PlanMemoryCurationInput{
		FencingGeneration: started.Run.FencingGeneration,
		Draft:             append(json.RawMessage(nil), exactEmptyMemoryCurationPlan...),
		IdempotencyKey:    fmt.Sprintf("curation-load-conflict-duplicate-plan-%d", opts.Seed),
	})
	execution.measurements.typedRefusal.add(time.Since(operationStarted))
	if !errors.Is(err, ErrMemoryCurationConflict) {
		return fmt.Errorf("duplicate plan refusal: %w", err)
	}
	execution.outcomes.StalePlanConflict.DuplicatePlanRefusals++
	execution.outcomes.StalePlanConflict.TypedRefusals++

	if _, err := st.ApplyCuration(ctx, p, started.Run.ID, ApplyMemoryCurationInput{
		FencingGeneration: started.Run.FencingGeneration,
		PlanRevision:      stored.Run.PlanRevision, PlanHash: stored.Run.PlanHash,
		IdempotencyKey: fmt.Sprintf("curation-load-conflict-exact-apply-%d", opts.Seed),
	}); err != nil {
		return fmt.Errorf("apply exact plan after typed refusals: %w", err)
	}
	conflicts := &execution.outcomes.StalePlanConflict
	conflicts.AllRefusalsTyped = conflicts.TypedRefusals ==
		conflicts.WrongPlanHashRefusals+conflicts.DuplicatePlanRefusals+conflicts.StaleFenceRefusals
	if !conflicts.AllRefusalsTyped {
		return errors.New("typed-refusal total includes an unclassified refusal")
	}
	return nil
}

func runMemoryCurationAbandonRequeue(
	ctx context.Context,
	st *Store,
	p Principal,
	opts loadquality.CurationOptions,
	execution *memoryCurationLoadExecution,
) error {
	scope := MemoryCurationScope{Sources: []string{MemoryCurationSourceMemory}}
	requested, err := st.RequestCuration(ctx, p, RequestMemoryCurationInput{
		Scope: scope, CoalescingKey: "abandon_requeue", TriggerReason: "load_test",
		MaxAttempts:    opts.MaxAttempts,
		IdempotencyKey: fmt.Sprintf("curation-load-abandon-request-%d", opts.Seed),
	})
	if err != nil {
		return fmt.Errorf("request abandon workload: %w", err)
	}
	previewRun, err := st.StartCuration(ctx, p, StartMemoryCurationInput{
		RequestID: requested.Request.ID, LeaseDuration: minMemoryCurationLease,
		Client: MemoryClientProvenance{
			Runtime: "curation-load", Recipe: loadquality.CurationResultSchemaV1,
			RecipeVersion: loadquality.CurationHarnessVersion,
		},
		IdempotencyKey: fmt.Sprintf("curation-load-abandon-preview-start-%d", opts.Seed),
	})
	if err != nil {
		return fmt.Errorf("start abandon preview: %w", err)
	}
	if _, err := st.PlanCuration(ctx, p, previewRun.Run.ID, PlanMemoryCurationInput{
		FencingGeneration: previewRun.Run.FencingGeneration,
		Draft:             append(json.RawMessage(nil), exactEmptyMemoryCurationPlan...),
		IdempotencyKey:    fmt.Sprintf("curation-load-abandon-preview-plan-%d", opts.Seed),
	}); err != nil {
		return fmt.Errorf("plan abandon preview: %w", err)
	}
	before, err := st.GetCurationRequest(ctx, p, requested.Request.ID)
	if err != nil {
		return fmt.Errorf("read abandon preview request before finish: %w", err)
	}
	outcome := loadquality.CurationAbandonRequeueOutcome{
		PreviewAttemptCountBefore: before.AttemptCount,
	}
	operationStarted := time.Now()
	preview, err := st.AbandonCuration(ctx, p, previewRun.Run.ID, FinishMemoryCurationInput{
		FencingGeneration: previewRun.Run.FencingGeneration, Reason: "preview_complete",
		IdempotencyKey: fmt.Sprintf("curation-load-abandon-preview-finish-%d", opts.Seed),
	})
	execution.measurements.abandon.add(time.Since(operationStarted))
	if err != nil {
		return fmt.Errorf("abandon completed preview: %w", err)
	}
	if preview.Run.State != MemoryCurationRunAbandoned {
		return fmt.Errorf("completed preview run state = %q", preview.Run.State)
	}
	outcome.PreviewAbandons = 1
	afterPreview, err := st.GetCurationRequest(ctx, p, requested.Request.ID)
	if err != nil {
		return fmt.Errorf("read completed preview request: %w", err)
	}
	outcome.PreviewAttemptCountAfter = afterPreview.AttemptCount
	if afterPreview.State != MemoryCurationRequestRetryWait {
		return fmt.Errorf("completed preview request state = %q", afterPreview.State)
	}
	outcome.PreviewRequeues = 1
	outcome.PreviewBudgetPreserved = outcome.PreviewAttemptCountBefore == outcome.PreviewAttemptCountAfter
	if !outcome.PreviewBudgetPreserved {
		return fmt.Errorf(
			"preview changed attempt count from %d to %d",
			outcome.PreviewAttemptCountBefore, outcome.PreviewAttemptCountAfter,
		)
	}

	requeued, err := st.RequestCuration(ctx, p, RequestMemoryCurationInput{
		Scope: scope, CoalescingKey: "abandon_requeue", TriggerReason: "load_test",
		MaxAttempts:    opts.MaxAttempts,
		IdempotencyKey: fmt.Sprintf("curation-load-abandon-preview-requeue-%d", opts.Seed),
	})
	if err != nil {
		return fmt.Errorf("make completed preview due: %w", err)
	}
	if requeued.Request.ID != requested.Request.ID || requeued.Request.State != MemoryCurationRequestQueued {
		return errors.New("completed preview did not requeue the same request as due")
	}
	request := requeued.Request
	for attempt := 0; attempt < opts.MaxAttempts; attempt++ {
		started, err := st.StartCuration(ctx, p, StartMemoryCurationInput{
			RequestID: request.ID, LeaseDuration: minMemoryCurationLease,
			Client: MemoryClientProvenance{
				Runtime: "curation-load", Recipe: loadquality.CurationResultSchemaV1,
				RecipeVersion: loadquality.CurationHarnessVersion,
			},
			IdempotencyKey: fmt.Sprintf("curation-load-abandon-start-%d-%d", opts.Seed, attempt),
		})
		if err != nil {
			return fmt.Errorf("start abandon failure attempt %d: %w", attempt, err)
		}
		if attempt%2 == 0 {
			operationStarted = time.Now()
			abandoned, err := st.AbandonCuration(ctx, p, started.Run.ID, FinishMemoryCurationInput{
				FencingGeneration: started.Run.FencingGeneration, Reason: "worker_abandoned",
				IdempotencyKey: fmt.Sprintf("curation-load-abandon-failure-%d-%d", opts.Seed, attempt),
			})
			execution.measurements.abandon.add(time.Since(operationStarted))
			if err != nil {
				return fmt.Errorf("abandon failure attempt %d: %w", attempt, err)
			}
			if abandoned.Run.State != MemoryCurationRunAbandoned {
				return fmt.Errorf("abandon failure attempt %d state = %q", attempt, abandoned.Run.State)
			}
			outcome.FailureAbandons++
		} else {
			tag, err := st.pool.Exec(ctx, `
				UPDATE memory_curation_runs
				SET lease_expires_at=clock_timestamp()-interval '1 second'
				WHERE id=$1 AND state IN ('open','planned')`, started.Run.ID)
			if err != nil {
				return fmt.Errorf("expire abandon attempt %d: %w", attempt, err)
			}
			if tag.RowsAffected() != 1 {
				return fmt.Errorf("expire abandon attempt %d affected %d rows", attempt, tag.RowsAffected())
			}
			operationStarted = time.Now()
			interrupted, err := st.RenewCuration(ctx, p, started.Run.ID, RenewMemoryCurationInput{
				FencingGeneration: started.Run.FencingGeneration,
				Extension:         minMemoryCurationLease,
				IdempotencyKey:    fmt.Sprintf("curation-load-abandon-expired-renew-%d-%d", opts.Seed, attempt),
			})
			execution.measurements.abandon.add(time.Since(operationStarted))
			if !errors.Is(err, ErrMemoryCurationLeaseExpired) {
				return fmt.Errorf("renew expired abandon attempt %d: %w", attempt, err)
			}
			if interrupted.Run.State != MemoryCurationRunInterrupted ||
				interrupted.Run.TerminalReasonCode != "lease_expired" {
				return fmt.Errorf("expired abandon attempt %d did not durably interrupt", attempt)
			}
			outcome.ExpiryInterruptions++
		}

		persisted, err := st.GetCurationRequest(ctx, p, request.ID)
		if err != nil {
			return fmt.Errorf("read abandon request after attempt %d: %w", attempt, err)
		}
		wantAttemptCount := attempt + 1
		if persisted.AttemptCount != wantAttemptCount {
			return fmt.Errorf(
				"abandon attempt %d stored attempt count %d, want %d",
				attempt, persisted.AttemptCount, wantAttemptCount,
			)
		}
		if wantAttemptCount == opts.MaxAttempts {
			if persisted.State != MemoryCurationRequestDeadLetter || persisted.DeadLetteredAt == nil {
				return fmt.Errorf("terminal abandon request state = %q", persisted.State)
			}
			outcome.DeadLetters = 1
			outcome.TerminalAttemptCount = persisted.AttemptCount
			request = persisted
			break
		}
		if persisted.State != MemoryCurationRequestRetryWait {
			return fmt.Errorf("retryable abandon request state = %q", persisted.State)
		}
		outcome.RetryRequeues++
		due, err := st.RequestCuration(ctx, p, RequestMemoryCurationInput{
			Scope: scope, CoalescingKey: "abandon_requeue", TriggerReason: "load_test",
			MaxAttempts:    opts.MaxAttempts,
			IdempotencyKey: fmt.Sprintf("curation-load-abandon-requeue-%d-%d", opts.Seed, attempt),
		})
		if err != nil {
			return fmt.Errorf("make abandon retry %d due: %w", attempt, err)
		}
		if due.Request.ID != request.ID || due.Request.State != MemoryCurationRequestQueued {
			return fmt.Errorf("abandon retry %d did not queue the same request", attempt)
		}
		request = due.Request
	}

	_, err = st.StartCuration(ctx, p, StartMemoryCurationInput{
		RequestID: request.ID, LeaseDuration: minMemoryCurationLease,
		IdempotencyKey: fmt.Sprintf("curation-load-abandon-terminal-start-%d", opts.Seed),
	})
	if !errors.Is(err, ErrMemoryCurationConflict) {
		return fmt.Errorf("post-dead-letter start refusal: %w", err)
	}
	outcome.PostTerminalStartRefusals = 1
	outcome.DeadLetterTerminal = outcome.DeadLetters == 1 &&
		outcome.TerminalAttemptCount == opts.MaxAttempts && outcome.PostTerminalStartRefusals == 1
	if !outcome.DeadLetterTerminal {
		return errors.New("abandon workload did not stop at the dead-letter retry ceiling")
	}
	execution.outcomes.AbandonRequeue = outcome
	return nil
}
