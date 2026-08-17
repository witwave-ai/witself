package store

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"

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
