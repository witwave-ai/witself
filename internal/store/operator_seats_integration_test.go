package store

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"

	"github.com/witwave-ai/witself/internal/plans"
	"github.com/witwave-ai/witself/internal/testenv"
)

// TestOperatorSeatsCapCreateAndPlanFit proves the seat dimension end to end: the
// create path refuses the seat past the cap, the root owner counts toward it, an
// absent key stays unlimited, and a downgrade target that cannot hold the live
// seats is reported as a plan-fit violation rather than silently accepted.
func TestOperatorSeatsCapCreateAndPlanFit(t *testing.T) {
	baseDSN := testenv.RequirePostgres(t)
	ctx := context.Background()
	st, _ := newMigrationTestStore(t, baseDSN)
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}
	accountID := provisionActiveResourceLimitAccount(ctx, t, st, "seats")

	var ownerID string
	if err := st.pool.QueryRow(ctx,
		`SELECT id FROM operators WHERE account_id = $1 AND is_root`,
		accountID).Scan(&ownerID); err != nil {
		t.Fatalf("resolve seeded owner: %v", err)
	}

	liveSeats := func() int64 {
		t.Helper()
		var n int64
		if err := st.pool.QueryRow(ctx,
			`SELECT count(*) FROM operators WHERE account_id = $1 AND deleted_at IS NULL`,
			accountID).Scan(&n); err != nil {
			t.Fatalf("count seats: %v", err)
		}
		return n
	}

	// The provisioning seed is itself a seat, so a cap of two leaves room for
	// exactly one more operator.
	if got := liveSeats(); got != 1 {
		t.Fatalf("seeded seats = %d, want 1 (the root owner)", got)
	}
	setResourceLimitPlan(ctx, t, st, accountID, 1, map[string]int64{
		plans.OperatorSeatsLimit: 2,
	})

	if _, _, _, err := st.CreateOperator(
		ctx, accountID, ownerID, "second seat", "second token", nil,
	); err != nil {
		t.Fatalf("create within the cap: %v", err)
	}
	if got := liveSeats(); got != 2 {
		t.Fatalf("seats after the second create = %d, want 2", got)
	}

	_, _, _, err := st.CreateOperator(
		ctx, accountID, ownerID, "third seat", "third token", nil,
	)
	if !errors.Is(err, ErrPlanLimitReached) {
		t.Fatalf("create past the cap = %v, want ErrPlanLimitReached", err)
	}
	assertPlanLimitDimension(t, err, plans.OperatorSeatsLimit)
	if got := liveSeats(); got != 2 {
		t.Fatalf("a refused create changed the seat count to %d, want 2", got)
	}

	// A target that cannot hold the live seats must surface as a violation, so
	// a downgrade is refused instead of stranding operators over the new cap.
	report, err := st.CheckAccountPlanFit(ctx, accountID, accountPlanFitTestTarget(
		t, "personal", map[string]int64{plans.OperatorSeatsLimit: 1},
	))
	if err != nil {
		t.Fatalf("plan fit against a one-seat target: %v", err)
	}
	if !slices.ContainsFunc(report.Violations, func(v AccountPlanFitViolation) bool {
		return v.Dimension == plans.OperatorSeatsLimit && v.Used == 2 && v.Max == 1
	}) {
		t.Fatalf("violations = %#v, want an %s violation of 2 over 1",
			report.Violations, plans.OperatorSeatsLimit)
	}

	// A target that can hold them must not report a seat violation at all.
	roomy, err := st.CheckAccountPlanFit(ctx, accountID, accountPlanFitTestTarget(
		t, "team", map[string]int64{plans.OperatorSeatsLimit: 25},
	))
	if err != nil {
		t.Fatalf("plan fit against a roomy target: %v", err)
	}
	if slices.ContainsFunc(roomy.Violations, func(v AccountPlanFitViolation) bool {
		return v.Dimension == plans.OperatorSeatsLimit
	}) {
		t.Fatalf("roomy target reported a seat violation: %#v", roomy.Violations)
	}

	// An absent key is unlimited everywhere else in the vocabulary, and seats
	// are no exception: this is the shipped catalog's behavior today.
	setResourceLimitPlan(ctx, t, st, accountID, 2, map[string]int64{})
	for index := 0; index < 3; index++ {
		if _, _, _, err := st.CreateOperator(
			ctx, accountID, ownerID,
			fmt.Sprintf("uncapped %d", index),
			fmt.Sprintf("uncapped token %d", index), nil,
		); err != nil {
			t.Fatalf("create with no seat cap: %v", err)
		}
	}
	if got := liveSeats(); got != 5 {
		t.Fatalf("uncapped seats = %d, want 5", got)
	}
}
