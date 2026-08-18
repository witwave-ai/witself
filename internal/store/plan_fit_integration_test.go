package store

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/id"
	"github.com/witwave-ai/witself/internal/plans"
)

func TestAccountPlanFitReportsEveryFiniteDurableUsageViolationPostgres(t *testing.T) {
	dsn := os.Getenv("WITSELF_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("WITSELF_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	fixture := newCustomDomainEmailFixture(t, dsn, "plan-fit-all")
	if _, err := fixture.store.ApplyAgentEmailCustomDomainRoute(
		ctx, fixture.accountID, fixture.customInput,
	); err != nil {
		t.Fatal(err)
	}

	owner := Principal{
		Kind: PrincipalAgent, ID: fixture.agents[0].ID,
		AccountID: fixture.accountID, RealmID: fixture.realm.ID,
		AgentName: fixture.agents[0].Name, AccountStatus: "active",
		AccessProfile: AccessProfileFull,
	}
	if _, err := fixture.store.CaptureMemory(
		ctx, owner, memoryLimitCaptureInput(1, "plan-fit-memory"),
	); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.store.SetFact(ctx, owner, SetFactInput{
		Predicate: "plan/fit", Value: json.RawMessage(`"finite"`),
		SourceKind: FactSourceAgent,
	}); err != nil {
		t.Fatal(err)
	}
	publicValue := "bounded"
	if _, err := fixture.store.CreateSecret(ctx, owner, CreateSecretInput{
		ID: mustSecretTestID(t, "sec"), Name: "plan fit public secret",
		Template: "generic", IdempotencyKey: "plan-fit-public-secret",
		Fields: []CreateSecretFieldInput{{
			ID: mustSecretTestID(t, "fld"), Name: "username",
			Kind: SecretFieldUsername, Encoding: SecretEncodingUTF8,
			ValueVersion: 1, PublicValue: &publicValue,
		}},
	}); err != nil {
		t.Fatal(err)
	}
	raw := agentEmailCapacityRaw(fixture.address.Address, "plan-fit")
	maximumRaw := int64(len(raw))
	setAgentEmailCapacityPlan(
		ctx, t, fixture.store, fixture.accountID, 1, &maximumRaw, &maximumRaw,
	)
	if _, err := ingestAgentEmailCapacity(
		ctx, fixture.store, fixture.scope, fixture.address.Address, raw,
	); err != nil {
		t.Fatal(err)
	}

	limits := map[string]int64{
		plans.RealmLimit:                             0,
		plans.AgentLimit:                             0,
		plans.AgentPerRealmLimit:                     0,
		plans.StoredMemoryLimit:                      0,
		plans.StoredFactLimit:                        0,
		plans.StoredSecretLimit:                      0,
		plans.AgentEmailAttachmentStorageBytesLimit:  0,
		plans.AgentEmailRealmAliasesPerRealmLimit:    0,
		plans.AgentEmailCustomDomainsPerAccountLimit: 0,
	}
	target := accountPlanFitTestTarget(t, "personal", limits)
	report, err := fixture.store.CheckAccountPlanFit(
		ctx, fixture.accountID, target,
	)
	if err != nil {
		t.Fatal(err)
	}
	wantDimensions := []string{
		plans.RealmLimit,
		plans.AgentLimit,
		plans.AgentPerRealmLimit,
		plans.StoredMemoryLimit,
		plans.StoredFactLimit,
		plans.StoredSecretLimit,
		plans.AgentEmailAttachmentStorageBytesLimit,
		plans.AgentEmailRealmAliasesPerRealmLimit,
		plans.AgentEmailCustomDomainsPerAccountLimit,
	}
	if report.AccountID != fixture.accountID || report.TargetPlan != "personal" ||
		report.TargetSnapshotHash != target.SnapshotHash ||
		len(report.Violations) != len(wantDimensions) {
		t.Fatalf("plan-fit report=%+v", report)
	}
	for index, violation := range report.Violations {
		if violation.Code != PlanFitViolationLimitExceeded ||
			violation.Dimension != wantDimensions[index] ||
			violation.Used <= 0 || violation.Max != 0 ||
			violation.SubjectCount < 1 {
			t.Fatalf("violation[%d]=%+v", index, violation)
		}
	}
	current, err := fixture.store.GetAccountPlan(ctx, fixture.accountID)
	if err != nil {
		t.Fatal(err)
	}
	atomic, err := fixture.store.ApplyAccountPlanIfFits(
		ctx, fixture.accountID, AccountPlanFitApplyTarget{
			Revision: current.Revision + 1, Plan: target.Plan,
			SnapshotHash: target.SnapshotHash, Limits: target.Limits,
			Policies: target.Policies, Features: target.Features,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if atomic.State != PlanFitApplyStateBlocked || atomic.AppliedSnapshot != nil ||
		atomic.CurrentSnapshot == nil || atomic.CurrentSnapshot.Revision != current.Revision ||
		atomic.CurrentSnapshot.Hash != current.Hash ||
		len(atomic.Violations) != len(wantDimensions) {
		t.Fatalf("atomic all-dimension result=%+v", atomic)
	}
}

func TestAccountPlanFitApplyAppliesAndExactReplaySkipsRefitPostgres(t *testing.T) {
	dsn := os.Getenv("WITSELF_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("WITSELF_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	st, _ := newMigrationTestStore(t, dsn)
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}
	account, err := st.ProvisionAccount(
		ctx, "plan-fit-apply-replay@witwave.ai", "plan fit apply replay", time.Hour,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = deleteAccountForIntegrationTest(ctx, st, account.AccountID) }()
	if active, err := st.ActivateAccount(ctx, account.AccountID); err != nil || !active {
		t.Fatalf("activate account=%t error=%v", active, err)
	}
	if _, err := st.CreateRealm(ctx, account.AccountID, "first"); err != nil {
		t.Fatal(err)
	}
	target := accountPlanFitApplyTestTarget(t, 1, "personal", map[string]int64{
		plans.RealmLimit: 1,
	})
	applied, err := st.ApplyAccountPlanIfFits(ctx, account.AccountID, target)
	if err != nil {
		t.Fatal(err)
	}
	if applied.State != PlanFitApplyStateApplied || len(applied.Violations) != 0 ||
		applied.CurrentSnapshot != nil || applied.AppliedSnapshot == nil ||
		applied.AppliedSnapshot.Revision != 1 ||
		applied.AppliedSnapshot.Hash != target.SnapshotHash {
		t.Fatalf("applied=%+v", applied)
	}
	firstAppliedAt := *applied.AppliedSnapshot.AppliedAt

	// Simulate already-retained legacy data that now exceeds the target. An
	// exact retry is an acknowledgement replay and must not turn into a fresh
	// capacity decision or rewrite plan_applied_at.
	extraRealmID, err := id.New("realm")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.pool.Exec(ctx, `
		INSERT INTO realms (id,account_id,name) VALUES ($1,$2,$3)`,
		extraRealmID, account.AccountID, "legacy extra",
	); err != nil {
		t.Fatal(err)
	}
	if err := st.SuspendAccountSystem(
		ctx, account.AccountID, "evacuation", "test exact plan replay",
	); err != nil {
		t.Fatal(err)
	}
	replayed, err := st.ApplyAccountPlanIfFits(ctx, account.AccountID, target)
	if err != nil {
		t.Fatal(err)
	}
	if replayed.State != PlanFitApplyStateApplied || len(replayed.Violations) != 0 ||
		replayed.CurrentSnapshot != nil || replayed.AppliedSnapshot == nil ||
		replayed.AppliedSnapshot.AppliedAt == nil ||
		!replayed.AppliedSnapshot.AppliedAt.Equal(firstAppliedAt) {
		t.Fatalf("replayed=%+v want exact acknowledgement", replayed)
	}
}

func TestAccountPlanFitApplyWaitsForConcurrentCapacityWriterPostgres(t *testing.T) {
	dsn := os.Getenv("WITSELF_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("WITSELF_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	st, _ := newMigrationTestStore(t, dsn)
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}
	account, err := st.ProvisionAccount(
		ctx, "plan-fit-apply-race@witwave.ai", "plan fit apply race", time.Hour,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = deleteAccountForIntegrationTest(ctx, st, account.AccountID) }()
	if active, err := st.ActivateAccount(ctx, account.AccountID); err != nil || !active {
		t.Fatalf("activate account=%t error=%v", active, err)
	}

	writer, err := st.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = writer.Rollback(ctx) }()
	if _, _, err := lockAccountForPlanGate(ctx, writer, account.AccountID); err != nil {
		t.Fatal(err)
	}
	var writerPID int
	if err := writer.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&writerPID); err != nil {
		t.Fatal(err)
	}
	racingRealmID, err := id.New("realm")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Exec(ctx, `
		INSERT INTO realms (id,account_id,name) VALUES ($1,$2,$3)`,
		racingRealmID, account.AccountID, "racing realm",
	); err != nil {
		t.Fatal(err)
	}

	target := accountPlanFitApplyTestTarget(t, 1, "personal", map[string]int64{
		plans.RealmLimit: 0,
	})
	type outcome struct {
		result AccountPlanFitApplyResult
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		result, err := st.ApplyAccountPlanIfFits(ctx, account.AccountID, target)
		done <- outcome{result: result, err: err}
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		var waiting bool
		if err := st.pool.QueryRow(ctx, `
			SELECT EXISTS (
			  SELECT 1
			    FROM pg_locks waiter
			    JOIN pg_locks holder
			      ON holder.locktype=waiter.locktype
			     AND holder.transactionid=waiter.transactionid
			   WHERE waiter.locktype='transactionid'
			     AND NOT waiter.granted AND holder.granted
			     AND holder.pid=$1 AND waiter.pid<>holder.pid
			)`, writerPID).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			break
		}
		select {
		case got := <-done:
			t.Fatalf("fit apply escaped account lock before writer commit: %+v", got)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("fit apply did not wait on the capacity writer account lock")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := writer.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-done:
		if got.err != nil || got.result.State != PlanFitApplyStateBlocked ||
			len(got.result.Violations) != 1 ||
			got.result.Violations[0].Dimension != plans.RealmLimit ||
			got.result.Violations[0].Used != 1 ||
			got.result.AppliedSnapshot != nil || got.result.CurrentSnapshot == nil {
			t.Fatalf("post-writer fit apply=%+v error=%v", got.result, got.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("fit apply did not resume after writer commit")
	}
	current, err := st.GetAccountPlan(ctx, account.AccountID)
	if err != nil {
		t.Fatal(err)
	}
	if current.Revision != 0 || current.Hash != "" {
		t.Fatalf("blocked race changed current plan=%+v", current)
	}
}

func TestAccountPlanFitApplyRejectsStaleFencePostgres(t *testing.T) {
	dsn := os.Getenv("WITSELF_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("WITSELF_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	st, _ := newMigrationTestStore(t, dsn)
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}
	account, err := st.ProvisionAccount(
		ctx, "plan-fit-apply-stale@witwave.ai", "plan fit apply stale", time.Hour,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = deleteAccountForIntegrationTest(ctx, st, account.AccountID) }()
	if active, err := st.ActivateAccount(ctx, account.AccountID); err != nil || !active {
		t.Fatalf("activate account=%t error=%v", active, err)
	}
	current := accountPlanFitApplyTestTarget(t, 2, "professional", map[string]int64{})
	if _, err := st.ApplyAccountPlanIfFits(ctx, account.AccountID, current); err != nil {
		t.Fatal(err)
	}
	older := accountPlanFitApplyTestTarget(t, 1, "personal", map[string]int64{})
	if _, err := st.ApplyAccountPlanIfFits(
		ctx, account.AccountID, older,
	); !errors.Is(err, ErrPlanSnapshotStale) {
		t.Fatalf("older error=%v want stale", err)
	}
	conflicting := accountPlanFitApplyTestTarget(t, 2, "personal", map[string]int64{})
	if _, err := st.ApplyAccountPlanIfFits(
		ctx, account.AccountID, conflicting,
	); !errors.Is(err, ErrPlanSnapshotStale) {
		t.Fatalf("same revision different hash error=%v want stale", err)
	}
}

func TestAccountPlanFitFailsClosedWhenDerivedUsageIsAmbiguousPostgres(t *testing.T) {
	dsn := os.Getenv("WITSELF_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("WITSELF_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	st, _ := newMigrationTestStore(t, dsn)
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}
	owner := newMemoryLimitPrincipal(ctx, t, st, "plan-fit-ambiguous")
	if _, err := st.pool.Exec(ctx, `
		UPDATE agents SET active_fact_count=1 WHERE id=$1`, owner.ID,
	); err != nil {
		t.Fatal(err)
	}
	target := accountPlanFitTestTarget(t, "personal", map[string]int64{
		plans.StoredFactLimit: 100,
	})
	if _, err := st.CheckAccountPlanFit(
		ctx, owner.AccountID, target,
	); !errors.Is(err, ErrPlanFitStateAmbiguous) {
		t.Fatalf("ambiguous fit error=%v", err)
	}
}

func TestAccountPlanFitRejectsIncompleteTargetBeforeReadingPostgres(t *testing.T) {
	dsn := os.Getenv("WITSELF_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("WITSELF_TEST_DATABASE_URL is not set")
	}
	st, _ := newMigrationTestStore(t, dsn)
	if _, err := st.CheckAccountPlanFit(
		context.Background(), "acct_missing", AccountPlanFitTarget{},
	); !errors.Is(err, ErrPlanSnapshotInvalid) {
		t.Fatalf("incomplete target error=%v", err)
	}
}

func accountPlanFitTestTarget(
	t *testing.T,
	plan string,
	limits map[string]int64,
) AccountPlanFitTarget {
	t.Helper()
	policies := map[string]int64{}
	features := []string{}
	hash, err := plans.SnapshotHash(plan, limits, policies, features)
	if err != nil {
		t.Fatal(err)
	}
	return AccountPlanFitTarget{
		Plan: plan, SnapshotHash: hash, Limits: limits,
		Policies: policies, Features: features,
	}
}

func accountPlanFitApplyTestTarget(
	t *testing.T,
	revision int64,
	plan string,
	limits map[string]int64,
) AccountPlanFitApplyTarget {
	t.Helper()
	fit := accountPlanFitTestTarget(t, plan, limits)
	return AccountPlanFitApplyTarget{
		Revision: revision, Plan: fit.Plan, SnapshotHash: fit.SnapshotHash,
		Limits: fit.Limits, Policies: fit.Policies, Features: fit.Features,
	}
}
