package store

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/plans"
)

func TestAccountPlanSnapshotRevisionFencesStaleWritersPostgres(t *testing.T) {
	dsn := os.Getenv("WITSELF_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("WITSELF_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	st, _ := newMigrationTestStore(t, dsn)
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}
	provisioned, err := st.ProvisionAccount(
		ctx, "plan-fence@witwave.ai", "plan fence", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = deleteAccountForIntegrationTest(ctx, st, provisioned.AccountID)
	}()

	limits := map[string]int64{
		plans.AgentLimit:         25,
		plans.AgentPerRealmLimit: 10,
		plans.RealmLimit:         1,
	}
	policies60 := map[string]int64{TranscriptRetentionDaysPolicy: 60}
	features := []string{"facts", "memory"}
	hash60, err := plans.SnapshotHash("free", limits, policies60, features)
	if err != nil {
		t.Fatal(err)
	}
	applied, err := st.SetAccountPlan(
		ctx, provisioned.AccountID, 2, hash60, "free",
		limits, policies60, features,
	)
	if err != nil {
		t.Fatal(err)
	}
	if applied.Revision != 2 || applied.Hash != hash60 ||
		applied.Policies[TranscriptRetentionDaysPolicy] != 60 {
		t.Fatalf("applied = %+v", applied)
	}

	policies30 := map[string]int64{TranscriptRetentionDaysPolicy: 30}
	hash30, err := plans.SnapshotHash("free", limits, policies30, features)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.SetAccountPlan(
		ctx, provisioned.AccountID, 1, hash30, "free",
		limits, policies30, features,
	); !errors.Is(err, ErrPlanSnapshotStale) {
		t.Fatalf("older revision error = %v; want ErrPlanSnapshotStale", err)
	}
	if _, err := st.SetAccountPlan(
		ctx, provisioned.AccountID, 2, hash30, "free",
		limits, policies30, features,
	); !errors.Is(err, ErrPlanSnapshotStale) {
		t.Fatalf("same revision/different hash error = %v; want stale", err)
	}
	if _, err := st.SetAccountPlan(
		ctx, provisioned.AccountID, 0, "", "free",
		limits, policies30, features,
	); !errors.Is(err, ErrPlanSnapshotStale) {
		t.Fatalf("legacy overwrite error = %v; want stale", err)
	}
	if _, err := st.SetAccountPlan(
		ctx, provisioned.AccountID, 3, hash60, "free",
		limits, policies30, features,
	); !errors.Is(err, ErrPlanSnapshotInvalid) {
		t.Fatalf("mismatched digest error = %v; want invalid", err)
	}

	replayed, err := st.SetAccountPlan(
		ctx, provisioned.AccountID, 2, hash60, "free",
		limits, policies60, features,
	)
	if err != nil || replayed.Revision != 2 {
		t.Fatalf("idempotent replay = %+v / %v", replayed, err)
	}
	current, err := st.GetAccountPlan(ctx, provisioned.AccountID)
	if err != nil {
		t.Fatal(err)
	}
	if current.Revision != 2 || current.Hash != hash60 ||
		current.Policies[TranscriptRetentionDaysPolicy] != 60 ||
		current.Limits[plans.AgentPerRealmLimit] != 10 {
		t.Fatalf("current = %+v; stale writer changed the cell", current)
	}
}
