package store

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/plans"
	"github.com/witwave-ai/witself/internal/testenv"
)

func TestMemoryActiveLimitBoundaryReplayLoweredCapAndLifecycle(t *testing.T) {
	dsn := testenv.RequirePostgres(t)
	ctx := context.Background()
	st, _ := newMigrationTestStore(t, dsn)
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}
	p := newMemoryLimitPrincipal(ctx, t, st, "boundary")

	status, err := st.GetMemoryLimitStatus(ctx, p)
	if err != nil || !status.Unlimited || status.Max != nil || status.Remaining != nil {
		t.Fatalf("unlimited status = %#v / %v", status, err)
	}
	setMemoryLimitPlan(ctx, t, st, p.AccountID, 1, 10)

	created := make([]MemoryMutationResult, 10)
	for index := range created {
		input := memoryLimitCaptureInput(index, fmt.Sprintf("boundary-%02d", index))
		result, err := st.CaptureMemory(ctx, p, input)
		if err != nil {
			t.Fatalf("capture %d: %v", index, err)
		}
		created[index] = result
	}
	status, err = st.GetMemoryLimitStatus(ctx, p)
	if err != nil || status.Used != 10 || status.Max == nil || *status.Max != 10 ||
		status.Remaining == nil || *status.Remaining != 0 || !status.NearLimit ||
		!status.AtLimit || status.OverLimit {
		t.Fatalf("at-limit status = %#v / %v", status, err)
	}
	replayInput := memoryLimitCaptureInput(9, "boundary-09")
	replay, err := st.CaptureMemory(ctx, p, replayInput)
	if err != nil || !replay.Receipt.Replayed || replay.Memory.ID != created[9].Memory.ID {
		t.Fatalf("capture replay at cap = %#v / %v", replay, err)
	}
	if _, err := st.CaptureMemory(ctx, p, memoryLimitCaptureInput(11, "boundary-blocked")); !errors.Is(err, ErrPlanLimitReached) {
		t.Fatalf("capture beyond cap = %v", err)
	} else {
		var limitErr *MemoryLimitError
		if !errors.As(err, &limitErr) || limitErr.Status.Used != 10 ||
			limitErr.Status.Max == nil || *limitErr.Status.Max != 10 {
			t.Fatalf("typed limit refusal = %#v", err)
		}
	}

	setMemoryLimitPlan(ctx, t, st, p.AccountID, 2, 8)
	status, err = st.GetMemoryLimitStatus(ctx, p)
	if err != nil || status.Used != 10 || !status.NearLimit || status.AtLimit ||
		!status.OverLimit || status.Remaining == nil || *status.Remaining != 0 {
		t.Fatalf("lowered-cap status = %#v / %v", status, err)
	}
	if _, err := st.GetMemory(ctx, p, created[0].Memory.ID); err != nil {
		t.Fatalf("read while over limit: %v", err)
	}
	firstForgotten, err := st.ForgetMemory(ctx, p, created[0].Memory.ID, MemoryLifecycleInput{
		ExpectedVersion: created[0].Memory.Version, IdempotencyKey: "boundary-forget-0",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.ForgetMemory(ctx, p, created[1].Memory.ID, MemoryLifecycleInput{
		ExpectedVersion: created[1].Memory.Version, IdempotencyKey: "boundary-forget-1",
	}); err != nil {
		t.Fatal(err)
	}
	status, err = st.GetMemoryLimitStatus(ctx, p)
	if err != nil || status.Used != 8 || !status.AtLimit || status.OverLimit {
		t.Fatalf("status after forgetting to cap = %#v / %v", status, err)
	}
	if _, err := st.RestoreMemory(ctx, p, created[0].Memory.ID, MemoryLifecycleInput{
		ExpectedVersion: firstForgotten.Memory.Version, IdempotencyKey: "boundary-restore-blocked",
	}); !errors.Is(err, ErrPlanLimitReached) {
		t.Fatalf("restore at cap = %v", err)
	}
	setMemoryLimitPlan(ctx, t, st, p.AccountID, 3, 9)
	if _, err := st.RestoreMemory(ctx, p, created[0].Memory.ID, MemoryLifecycleInput{
		ExpectedVersion: firstForgotten.Memory.Version, IdempotencyKey: "boundary-restore-allowed",
	}); err != nil {
		t.Fatalf("restore after cap increase: %v", err)
	}
}

func TestMemoryActiveLimitSupersessionAndReactivate(t *testing.T) {
	dsn := testenv.RequirePostgres(t)
	ctx := context.Background()
	st, _ := newMigrationTestStore(t, dsn)
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}
	p := newMemoryLimitPrincipal(ctx, t, st, "supersede")
	setMemoryLimitPlan(ctx, t, st, p.AccountID, 1, 1)

	source, err := st.CaptureMemory(ctx, p, memoryLimitCaptureInput(1, "supersede-source"))
	if err != nil {
		t.Fatal(err)
	}
	replaced, err := st.SupersedeMemory(ctx, p, SupersedeMemoryInput{
		MemoryID: source.Memory.ID, ExpectedVersion: source.Memory.Version,
		Replacements:   []CaptureMemoryInput{memoryLimitCaptureInput(2, "supersede-one-for-one")},
		IdempotencyKey: "supersede-one-for-one-operation",
	})
	if err != nil || len(replaced.Replacements) != 1 {
		t.Fatalf("one-for-one supersede at cap = %#v / %v", replaced, err)
	}
	if _, err := st.ReactivateMemory(ctx, p, source.Memory.ID, MemoryLifecycleInput{
		ExpectedVersion:                 replaced.Source.Version,
		ExpectedSupersessionSetRevision: &replaced.Receipt.SupersessionSetRevision,
		IdempotencyKey:                  "reactivate-source-blocked",
	}); !errors.Is(err, ErrPlanLimitReached) {
		t.Fatalf("reactivate at cap = %v", err)
	}
	forgotten, err := st.ForgetMemory(ctx, p, replaced.Replacements[0].ID, MemoryLifecycleInput{
		ExpectedVersion: replaced.Replacements[0].Version,
		IdempotencyKey:  "forget-replacement",
	})
	if err != nil || forgotten.Memory.State != MemoryStateForgotten {
		t.Fatalf("forget replacement = %#v / %v", forgotten, err)
	}
	reactivated, err := st.ReactivateMemory(ctx, p, source.Memory.ID, MemoryLifecycleInput{
		ExpectedVersion:                 replaced.Source.Version,
		ExpectedSupersessionSetRevision: &replaced.Receipt.SupersessionSetRevision,
		IdempotencyKey:                  "reactivate-source-allowed",
	})
	if err != nil || reactivated.Memory.State != MemoryStateActive {
		t.Fatalf("reactivate after freeing slot = %#v / %v", reactivated, err)
	}
	if _, err := st.SupersedeMemory(ctx, p, SupersedeMemoryInput{
		MemoryID: source.Memory.ID, ExpectedVersion: reactivated.Memory.Version,
		Replacements: []CaptureMemoryInput{
			memoryLimitCaptureInput(3, "supersede-positive-a"),
			memoryLimitCaptureInput(4, "supersede-positive-b"),
		},
		IdempotencyKey: "supersede-positive-operation",
	}); !errors.Is(err, ErrPlanLimitReached) {
		t.Fatalf("net-growing supersede at cap = %v", err)
	}
	current, err := st.GetMemory(ctx, p, source.Memory.ID)
	if err != nil || current.State != MemoryStateActive || current.Version != reactivated.Memory.Version {
		t.Fatalf("source after refused supersede = %#v / %v", current, err)
	}
}

func TestMemoryActiveLimitConcurrentCapturesAcrossReplicasAndOwners(t *testing.T) {
	dsn := testenv.RequirePostgres(t)
	ctx := context.Background()
	st, schemaDSN := newMigrationTestStore(t, dsn)
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}
	p := newMemoryLimitPrincipal(ctx, t, st, "race")
	setMemoryLimitPlan(ctx, t, st, p.AccountID, 1, 5)
	replica, err := Open(ctx, schemaDSN)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(replica.Close)
	otherAgent, err := st.CreateAgent(ctx, p.AccountID, p.RealmID, "other owner")
	if err != nil {
		t.Fatal(err)
	}
	other := p
	other.ID = otherAgent.ID
	other.AgentName = otherAgent.Name
	for index := range 5 {
		if _, err := replica.CaptureMemory(ctx, other,
			memoryLimitCaptureInput(index+50, fmt.Sprintf("other-owner-%d", index))); err != nil {
			t.Fatal(err)
		}
	}
	otherStatus, err := st.GetMemoryLimitStatus(ctx, other)
	if err != nil || otherStatus.Used != 5 || !otherStatus.AtLimit {
		t.Fatalf("other owner status = %#v / %v", otherStatus, err)
	}
	for index := range 4 {
		if _, err := st.CaptureMemory(ctx, p,
			memoryLimitCaptureInput(index, fmt.Sprintf("race-prefill-%d", index))); err != nil {
			t.Fatal(err)
		}
	}

	var wg sync.WaitGroup
	var mu sync.Mutex
	successes, refusals := 0, 0
	for index := range 20 {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			target := st
			if index%2 == 1 {
				target = replica
			}
			_, err := target.CaptureMemory(ctx, p,
				memoryLimitCaptureInput(index+100, fmt.Sprintf("race-candidate-%02d", index)))
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				successes++
			case errors.Is(err, ErrPlanLimitReached):
				refusals++
			default:
				t.Errorf("concurrent capture %d: %v", index, err)
			}
		}(index)
	}
	wg.Wait()
	if successes != 1 || refusals != 19 {
		t.Fatalf("concurrent results successes=%d refusals=%d", successes, refusals)
	}
	status, err := st.GetMemoryLimitStatus(ctx, p)
	if err != nil || status.Used != 5 || !status.AtLimit {
		t.Fatalf("status after race = %#v / %v", status, err)
	}
}

func TestMemoryActiveCountProjectionTreatsMissingClockAsZero(t *testing.T) {
	dsn := testenv.RequirePostgres(t)
	ctx := context.Background()
	st, _ := newMigrationTestStore(t, dsn)
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}
	p := newMemoryLimitPrincipal(ctx, t, st, "empty-projection")

	tx, err := st.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var clockExists bool
	if err := tx.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1
			  FROM memory_change_clocks
			 WHERE account_id=$1 AND realm_id=$2
			   AND owner_kind='agent' AND owner_id=$3
		)`,
		p.AccountID, p.RealmID, p.ID,
	).Scan(&clockExists); err != nil {
		t.Fatal(err)
	}
	if clockExists {
		t.Fatal("new owner unexpectedly has a memory change clock")
	}
	count, err := memoryActiveCountTx(ctx, tx, p)
	if err != nil || count != 0 {
		t.Fatalf("missing-clock projection = %d / %v; want zero", count, err)
	}
}

func TestMemoryActiveLimitCurationApplyAndRollback(t *testing.T) {
	dsn := testenv.RequirePostgres(t)
	ctx := context.Background()
	st, _ := newMigrationTestStore(t, dsn)
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}

	t.Run("net-positive apply is refused atomically", func(t *testing.T) {
		fixture := newMemoryCurationPlanFixture(ctx, t, st, "memory-limit-positive", false, 1)
		setMemoryLimitPlan(ctx, t, st, fixture.Principal.AccountID, 1, 1)
		source := fixture.Memories[0].Memory
		draft := MemoryCurationPlanDraft{
			Schema: MemoryCurationPlanSchemaV1, DraftRevision: 1,
			Actions: []MemoryCurationPlanAction{{
				Ordinal: 1, Operation: MemoryCurationOperationCreate,
				Create: &MemoryCurationCreateAction{
					LocalRef: "additional",
					Snapshot: MemoryCurationMemorySnapshot{
						Content: "an additional active memory", Kind: "decision",
						Evidence: []MemoryCurationEvidence{
							memoryCurationEvidenceFromInputRow(source.Evidence[0]),
						},
					},
				},
			}},
		}
		planned, err := st.PlanCuration(ctx, fixture.Principal, fixture.Started.Run.ID,
			PlanMemoryCurationInput{
				FencingGeneration: fixture.Started.Run.FencingGeneration,
				Draft:             marshalCurationPlanDraft(t, draft, false),
				IdempotencyKey:    "memory-limit-positive-plan",
			})
		if err != nil {
			t.Fatal(err)
		}
		if planned.Preview.ActiveMemoryDelta != 1 ||
			planned.Preview.ProjectedActiveMemories != 2 {
			t.Fatalf("positive preview = %#v", planned.Preview)
		}
		if _, err := st.ApplyCuration(ctx, fixture.Principal, fixture.Started.Run.ID,
			ApplyMemoryCurationInput{
				FencingGeneration: fixture.Started.Run.FencingGeneration,
				PlanRevision:      planned.Plan.PlanRevision,
				PlanHash:          planned.Receipt.PlanHash,
				IdempotencyKey:    "memory-limit-positive-apply",
			}); !errors.Is(err, ErrPlanLimitReached) {
			t.Fatalf("positive apply at cap = %v", err)
		}
		status, err := st.GetMemoryLimitStatus(ctx, fixture.Principal)
		if err != nil || status.Used != 1 || !status.AtLimit {
			t.Fatalf("status after refused apply = %#v / %v", status, err)
		}
		if _, err := st.GetMemory(ctx, fixture.Principal,
			planned.PreallocatedMemoryIDs[0].MemoryID); !errors.Is(err, ErrMemoryNotFound) {
			t.Fatalf("refused apply created a memory: %v", err)
		}
	})

	t.Run("merge and compensating rollback remain available", func(t *testing.T) {
		fixture := newMemoryCurationPlanFixture(ctx, t, st, "memory-limit-merge", false, 2)
		setMemoryLimitPlan(ctx, t, st, fixture.Principal.AccountID, 1, 2)
		first := fixture.Memories[0].Memory
		second := fixture.Memories[1].Memory
		draft := MemoryCurationPlanDraft{
			Schema: MemoryCurationPlanSchemaV1, DraftRevision: 1,
			Actions: []MemoryCurationPlanAction{
				{
					Ordinal: 1, Operation: MemoryCurationOperationCreate,
					Create: &MemoryCurationCreateAction{
						LocalRef: "merged",
						Snapshot: MemoryCurationMemorySnapshot{
							Content: "one consolidated active memory", Kind: "decision",
							Evidence: []MemoryCurationEvidence{
								memoryCurationEvidenceFromInputRow(first.Evidence[0]),
								memoryCurationEvidenceFromInputRow(second.Evidence[0]),
							},
						},
					},
				},
				{
					Ordinal: 2, Operation: MemoryCurationOperationSupersede,
					Supersede: &MemoryCurationSupersedeAction{
						Target: MemoryCurationTargetReference{
							MemoryID: first.ID, ExpectedVersion: first.Version,
						},
						Replacements: []MemoryCurationVersionReference{{
							LocalRef: "merged", Version: 1,
						}},
					},
				},
				{
					Ordinal: 3, Operation: MemoryCurationOperationSupersede,
					Supersede: &MemoryCurationSupersedeAction{
						Target: MemoryCurationTargetReference{
							MemoryID: second.ID, ExpectedVersion: second.Version,
						},
						Replacements: []MemoryCurationVersionReference{{
							LocalRef: "merged", Version: 1,
						}},
					},
				},
			},
		}
		planned, err := st.PlanCuration(ctx, fixture.Principal, fixture.Started.Run.ID,
			PlanMemoryCurationInput{
				FencingGeneration: fixture.Started.Run.FencingGeneration,
				Draft:             marshalCurationPlanDraft(t, draft, false),
				IdempotencyKey:    "memory-limit-merge-plan",
			})
		if err != nil {
			t.Fatal(err)
		}
		if planned.Preview.ActiveMemoryDelta != -1 ||
			planned.Preview.ProjectedActiveMemories != 1 {
			t.Fatalf("merge preview = %#v", planned.Preview)
		}
		applied, err := st.ApplyCuration(ctx, fixture.Principal, fixture.Started.Run.ID,
			ApplyMemoryCurationInput{
				FencingGeneration: fixture.Started.Run.FencingGeneration,
				PlanRevision:      planned.Plan.PlanRevision,
				PlanHash:          planned.Receipt.PlanHash,
				IdempotencyKey:    "memory-limit-merge-apply",
			})
		if err != nil {
			t.Fatal(err)
		}
		status, err := st.GetMemoryLimitStatus(ctx, fixture.Principal)
		if err != nil || status.Used != 1 || status.Max == nil || *status.Max != 2 {
			t.Fatalf("status after merge = %#v / %v", status, err)
		}

		setMemoryLimitPlan(ctx, t, st, fixture.Principal.AccountID, 2, 0)
		producedHeads, _ := memoryCurationProducedHeads(applied.Receipt.ActionResults)
		if _, err := st.RollbackCuration(ctx, fixture.Principal, fixture.Started.Run.ID,
			RollbackMemoryCurationInput{
				ApplyReceiptID:        applied.Receipt.ID,
				ExpectedProducedHeads: producedHeads,
				Reason:                "verify rollback remains available above a lowered cap",
				IdempotencyKey:        "memory-limit-merge-rollback",
			}); err != nil {
			t.Fatal(err)
		}
		status, err = st.GetMemoryLimitStatus(ctx, fixture.Principal)
		if err != nil || status.Used != 2 || status.Max == nil || *status.Max != 0 ||
			status.AtLimit || !status.OverLimit {
			t.Fatalf("status after rollback above lowered cap = %#v / %v", status, err)
		}
	})
}

func newMemoryLimitPrincipal(ctx context.Context, t *testing.T, st *Store, suffix string) Principal {
	t.Helper()
	account, err := st.ProvisionAccount(ctx,
		fmt.Sprintf("memory-limit-%s-%d@witwave.ai", suffix, time.Now().UnixNano()),
		"memory limit", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if activated, err := st.ActivateAccount(ctx, account.AccountID); err != nil || !activated {
		t.Fatalf("activate = %t / %v", activated, err)
	}
	realm, err := st.CreateRealm(ctx, account.AccountID, "default")
	if err != nil {
		t.Fatal(err)
	}
	agent, err := st.CreateAgent(ctx, account.AccountID, realm.ID, "owner")
	if err != nil {
		t.Fatal(err)
	}
	return Principal{
		Kind: PrincipalAgent, ID: agent.ID, AccountID: account.AccountID,
		RealmID: realm.ID, AgentName: agent.Name, AccountStatus: "active",
		AccessProfile: AccessProfileFull,
	}
}

func setMemoryLimitPlan(
	ctx context.Context,
	t *testing.T,
	st *Store,
	accountID string,
	revision, limit int64,
) {
	t.Helper()
	limits := map[string]int64{plans.StoredMemoryLimit: limit}
	hash, err := plans.SnapshotHash("test", limits, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.SetAccountPlan(ctx, accountID, revision, hash, "test",
		limits, nil, nil); err != nil {
		t.Fatal(err)
	}
}

func memoryLimitCaptureInput(index int, key string) CaptureMemoryInput {
	return CaptureMemoryInput{
		Content: fmt.Sprintf("bounded memory %d", index), Kind: "session",
		CaptureReason: "test",
		Evidence: []MemoryEvidenceInput{{
			ResolutionState:    MemoryEvidenceUnavailable,
			TerminalReasonCode: "runtime_did_not_record",
		}},
		IdempotencyKey: key,
	}
}
