package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/plans"
)

func TestRealmPlanLimitBoundaryDeletionUnlimitedAndConcurrentReplicas(t *testing.T) {
	baseDSN := os.Getenv("WITSELF_TEST_DATABASE_URL")
	if baseDSN == "" {
		t.Skip("WITSELF_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	st, dsn := newMigrationTestStore(t, baseDSN)
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}
	accountID := provisionActiveResourceLimitAccount(ctx, t, st, "realm")
	replica, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(replica.Close)

	setResourceLimitPlan(ctx, t, st, accountID, 1, map[string]int64{
		plans.RealmLimit: 1,
	})

	var wg sync.WaitGroup
	var mu sync.Mutex
	successes, refusals := 0, 0
	created := make([]Realm, 0, 1)
	for index := 0; index < 20; index++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			target := st
			if index%2 == 1 {
				target = replica
			}
			realm, createErr := target.CreateRealm(
				ctx, accountID, fmt.Sprintf("race-%02d", index),
			)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case createErr == nil:
				successes++
				created = append(created, realm)
			case errors.Is(createErr, ErrPlanLimitReached):
				refusals++
				assertPlanLimitDimension(t, createErr, plans.RealmLimit)
			default:
				t.Errorf("concurrent realm create %d: %v", index, createErr)
			}
		}(index)
	}
	wg.Wait()
	if successes != 1 || refusals != 19 {
		t.Fatalf("concurrent realms successes=%d refusals=%d", successes, refusals)
	}

	// Lowering never deletes existing resources; deleting a live empty realm
	// releases its slot.
	setResourceLimitPlan(ctx, t, st, accountID, 2, map[string]int64{
		plans.RealmLimit: 0,
	})
	if got, err := st.ListRealms(ctx, accountID); err != nil || len(got) != 1 {
		t.Fatalf("realms after lowering = %#v / %v", got, err)
	}
	if _, err := st.CreateRealm(ctx, accountID, "blocked-at-zero"); !errors.Is(err, ErrPlanLimitReached) {
		t.Fatalf("zero-cap create error = %v", err)
	}
	if err := st.DeleteRealm(ctx, accountID, created[0].ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateRealm(ctx, accountID, "still-blocked-at-zero"); !errors.Is(err, ErrPlanLimitReached) {
		t.Fatalf("zero cap after delete = %v", err)
	}

	// Missing is unlimited. It is deliberately distinct from zero.
	setResourceLimitPlan(ctx, t, st, accountID, 3, nil)
	for index := 0; index < 3; index++ {
		if _, err := st.CreateRealm(
			ctx, accountID, fmt.Sprintf("unlimited-%02d", index),
		); err != nil {
			t.Fatalf("unlimited realm create %d: %v", index, err)
		}
	}
}

func TestAgentPerRealmPlanLimitAndLegacyCompatibility(t *testing.T) {
	baseDSN := os.Getenv("WITSELF_TEST_DATABASE_URL")
	if baseDSN == "" {
		t.Skip("WITSELF_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	st, _ := newMigrationTestStore(t, baseDSN)
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}
	accountID := provisionActiveResourceLimitAccount(ctx, t, st, "agent-scope")
	firstRealm, err := st.CreateRealm(ctx, accountID, "first")
	if err != nil {
		t.Fatal(err)
	}
	secondRealm, err := st.CreateRealm(ctx, accountID, "second")
	if err != nil {
		t.Fatal(err)
	}

	setResourceLimitPlan(ctx, t, st, accountID, 1, map[string]int64{
		plans.AgentPerRealmLimit: 2,
	})
	firstAgents := make([]Agent, 0, 2)
	for _, realm := range []Realm{firstRealm, secondRealm} {
		for index := 0; index < 2; index++ {
			agent, err := st.CreateAgent(
				ctx, accountID, realm.ID, fmt.Sprintf("%s-%d", realm.Name, index),
			)
			if err != nil {
				t.Fatalf("create agent in %s: %v", realm.Name, err)
			}
			if realm.ID == firstRealm.ID {
				firstAgents = append(firstAgents, agent)
			}
		}
	}
	if _, err := st.CreateAgent(
		ctx, accountID, firstRealm.ID, "first-blocked",
	); !errors.Is(err, ErrPlanLimitReached) {
		t.Fatalf("per-realm boundary error = %v", err)
	} else {
		assertPlanLimitDimension(t, err, plans.AgentPerRealmLimit)
	}

	// A soft-deleted agent no longer consumes capacity, while its tombstone
	// continues to reserve the old name.
	if err := st.DeleteAgent(
		ctx, accountID, firstRealm.ID, firstAgents[0].ID,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateAgent(
		ctx, accountID, firstRealm.ID, "first-replacement",
	); err != nil {
		t.Fatalf("create after delete = %v", err)
	}
	if _, err := st.CreateAgent(
		ctx, accountID, firstRealm.ID, firstAgents[0].Name,
	); !errors.Is(err, ErrAgentExists) {
		t.Fatalf("tombstone name at cap = %v; want ErrAgentExists", err)
	}

	// If both keys reach their boundary together, the canonical product limit
	// wins so callers see the intended agents-per-realm explanation.
	setResourceLimitPlan(ctx, t, st, accountID, 2, map[string]int64{
		plans.AgentLimit:         4,
		plans.AgentPerRealmLimit: 2,
	})
	if _, err := st.CreateAgent(
		ctx, accountID, firstRealm.ID, "both-limits-blocked",
	); !errors.Is(err, ErrPlanLimitReached) {
		t.Fatalf("equal boundary error = %v", err)
	} else {
		assertPlanLimitDimension(t, err, plans.AgentPerRealmLimit)
	}

	// Existing snapshots keep legacy AgentLimit account-wide. If both keys
	// are present, both gates apply; the legacy total still wins when the
	// target realm has room.
	setResourceLimitPlan(ctx, t, st, accountID, 3, map[string]int64{
		plans.AgentLimit:         4,
		plans.AgentPerRealmLimit: 3,
	})
	if _, err := st.CreateAgent(
		ctx, accountID, firstRealm.ID, "legacy-account-blocked",
	); !errors.Is(err, ErrPlanLimitReached) {
		t.Fatalf("legacy boundary error = %v", err)
	} else {
		assertPlanLimitDimension(t, err, plans.AgentLimit)
	}

	// Realm validity wins over either cap so callers receive a truthful 404.
	setResourceLimitPlan(ctx, t, st, accountID, 4, map[string]int64{
		plans.AgentLimit:         0,
		plans.AgentPerRealmLimit: 0,
	})
	if _, err := st.CreateAgent(
		ctx, accountID, "realm_missing", "invalid-target",
	); !errors.Is(err, ErrRealmNotFound) {
		t.Fatalf("invalid realm at zero cap = %v; want ErrRealmNotFound", err)
	}

	// Lowering below current use leaves every live agent readable/listable.
	if got, err := st.ListAgents(ctx, accountID, firstRealm.ID); err != nil || len(got) != 2 {
		t.Fatalf("first realm after lowering = %#v / %v", got, err)
	}
	if got, err := st.ListAgents(ctx, accountID, secondRealm.ID); err != nil || len(got) != 2 {
		t.Fatalf("second realm after lowering = %#v / %v", got, err)
	}

	setResourceLimitPlan(ctx, t, st, accountID, 5, nil)
	if _, err := st.CreateAgent(
		ctx, accountID, firstRealm.ID, "unlimited",
	); err != nil {
		t.Fatalf("missing-limit create = %v", err)
	}
}

func TestAgentPerRealmPlanLimitConcurrentReplicas(t *testing.T) {
	baseDSN := os.Getenv("WITSELF_TEST_DATABASE_URL")
	if baseDSN == "" {
		t.Skip("WITSELF_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	st, dsn := newMigrationTestStore(t, baseDSN)
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}
	accountID := provisionActiveResourceLimitAccount(ctx, t, st, "agent-race")
	realm, err := st.CreateRealm(ctx, accountID, "race")
	if err != nil {
		t.Fatal(err)
	}
	setResourceLimitPlan(ctx, t, st, accountID, 1, map[string]int64{
		plans.AgentPerRealmLimit: 1,
	})
	replica, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(replica.Close)

	var wg sync.WaitGroup
	var mu sync.Mutex
	successes, refusals := 0, 0
	for index := 0; index < 20; index++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			target := st
			if index%2 == 1 {
				target = replica
			}
			_, createErr := target.CreateAgent(
				ctx, accountID, realm.ID, fmt.Sprintf("race-%02d", index),
			)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case createErr == nil:
				successes++
			case errors.Is(createErr, ErrPlanLimitReached):
				refusals++
				assertPlanLimitDimension(t, createErr, plans.AgentPerRealmLimit)
			default:
				t.Errorf("concurrent agent create %d: %v", index, createErr)
			}
		}(index)
	}
	wg.Wait()
	if successes != 1 || refusals != 19 {
		t.Fatalf("concurrent agents successes=%d refusals=%d", successes, refusals)
	}
}

func provisionActiveResourceLimitAccount(
	ctx context.Context,
	t *testing.T,
	st *Store,
	suffix string,
) string {
	t.Helper()
	account, err := st.ProvisionAccount(
		ctx,
		fmt.Sprintf("resource-limit-%s-%d@witwave.ai", suffix, time.Now().UnixNano()),
		"resource limits",
		time.Hour,
	)
	if err != nil {
		t.Fatal(err)
	}
	if activated, err := st.ActivateAccount(
		ctx, account.AccountID,
	); err != nil || !activated {
		t.Fatalf("activate = %t / %v", activated, err)
	}
	t.Cleanup(func() {
		_ = deleteAccountForIntegrationTest(ctx, st, account.AccountID)
	})
	return account.AccountID
}

func setResourceLimitPlan(
	ctx context.Context,
	t *testing.T,
	st *Store,
	accountID string,
	revision int64,
	limits map[string]int64,
) {
	t.Helper()
	hash, err := plans.SnapshotHash("test", limits, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.SetAccountPlan(
		ctx, accountID, revision, hash, "test", limits, nil, nil,
	); err != nil {
		t.Fatal(err)
	}
}

func assertPlanLimitDimension(t *testing.T, err error, dimension string) {
	t.Helper()
	var detail *PlanLimitError
	if !errors.As(err, &detail) || detail.Dimension != dimension {
		t.Errorf("plan limit error = %#v; want dimension %q", err, dimension)
	}
}
