package store

import (
	"context"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/testenv"
)

func TestReadIdentityCapacityMetricsPostgres(t *testing.T) {
	dsn := testenv.RequirePostgres(t)
	st, _ := newMigrationTestStore(t, dsn)
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	empty := IdentityCapacityDimensionMetrics{MinHeadroomRatio: 1}
	assertIdentityCapacityMetrics(t, st, ctx, IdentityCapacityMetrics{
		Realms: empty, AgentsPerRealm: empty, OperatorSeats: empty,
	})

	for _, fixture := range []struct {
		id        string
		limits    string
		realms    int
		agents    int
		operators int
		status    string
	}{
		{"capacity-at-account-canary", `{"realms":2,"agents_per_realm":3,"operator_seats":2}`, 2, 3, 2, "active"},
		{"capacity-near-account-canary", `{"realms":5,"agents_per_realm":5,"operator_seats":5}`, 4, 4, 4, "active"},
		{"capacity-unlimited-account-canary", `{}`, 3, 7, 6, "active"},
		{"capacity-below-account-canary", `{"realms":5,"agents_per_realm":10,"operator_seats":4}`, 1, 1, 1, "active"},
		{"capacity-suspended-account-canary", `{"realms":0,"agents_per_realm":0,"operator_seats":0}`, 0, 0, 0, "suspended"},
		{"capacity-closed-account-canary", `{"realms":0,"agents_per_realm":0,"operator_seats":0}`, 0, 0, 0, "closed"},
	} {
		if _, err := st.pool.Exec(ctx, `
			INSERT INTO accounts (id, status, plan, plan_limits)
			VALUES ($1, $2, 'capacity-plan-name-canary', $3::jsonb)`,
			fixture.id, fixture.status, fixture.limits); err != nil {
			t.Fatal(err)
		}
		for realmIndex := range fixture.realms {
			realmID := fmt.Sprintf("%s-realm-canary-%d", fixture.id, realmIndex)
			if _, err := st.pool.Exec(ctx, `INSERT INTO realms (id, account_id, name) VALUES ($1, $2, $1)`,
				realmID, fixture.id); err != nil {
				t.Fatal(err)
			}
			// Every realm has agents. Summing instead of taking the worst
			// realm would incorrectly put the near-limit account at its cap.
			for agentIndex := range fixture.agents {
				agentID := fmt.Sprintf("%s-agent-canary-%d", realmID, agentIndex)
				if _, err := st.pool.Exec(ctx, `INSERT INTO agents (id, realm_id, name) VALUES ($1, $2, $1)`,
					agentID, realmID); err != nil {
					t.Fatal(err)
				}
			}
		}
		for operatorIndex := range fixture.operators {
			operatorID := fmt.Sprintf("%s-operator-canary-%d", fixture.id, operatorIndex)
			if _, err := st.pool.Exec(ctx, `
				INSERT INTO operators (id, account_id, role, is_root)
				VALUES ($1, $2, 'account_owner', $3)`,
				operatorID, fixture.id, operatorIndex == 0); err != nil {
				t.Fatal(err)
			}
		}
	}
	// Deleted resources must not consume live capacity, including agents
	// that themselves remain live underneath a deleted realm.
	for _, query := range []string{
		`INSERT INTO accounts (id, status, plan_limits, closed_at, purged_at)
		 VALUES ('capacity-purged-account-canary', 'active', '{"realms":0,"agents_per_realm":0,"operator_seats":0}', now(), now())`,
		`INSERT INTO realms (id, account_id, name, deleted_at,
		                     email_route_state, email_route_generation, email_route_operation_id)
		 VALUES ('capacity-deleted-realm-canary', 'capacity-below-account-canary', 'deleted', now(),
		         'retired', 2, 'capacity-test-retirement')`,
		`INSERT INTO agents (id, realm_id, name)
		 SELECT 'capacity-deleted-realm-agent-canary-' || n, 'capacity-deleted-realm-canary', 'agent-' || n
		 FROM generate_series(1, 20) n`,
		`INSERT INTO agents (id, realm_id, name, deleted_at)
		 SELECT 'capacity-deleted-agent-canary-' || n, 'capacity-below-account-canary-realm-canary-0', 'deleted-' || n, now()
		 FROM generate_series(1, 20) n`,
		`INSERT INTO operators (id, account_id, role, deleted_at)
		 SELECT 'capacity-deleted-operator-canary-' || n, 'capacity-below-account-canary', 'account_operator', now()
		 FROM generate_series(1, 20) n`,
	} {
		if _, err := st.pool.Exec(ctx, query); err != nil {
			t.Fatal(err)
		}
	}
	want := IdentityCapacityDimensionMetrics{
		AccountsMeasured: 3, AccountsNearLimit: 2, AccountsAtLimit: 1,
		AccountsUnlimited: 1, MinHeadroomRatio: 0,
	}
	assertIdentityCapacityMetrics(t, st, ctx, IdentityCapacityMetrics{
		Realms: want, AgentsPerRealm: want, OperatorSeats: want,
	})

	// Removing the at-limit account's caps exposes the real minimum ratio
	// and proves unlimited accounts are excluded despite their resource use.
	if _, err := st.pool.Exec(ctx, `UPDATE accounts SET plan_limits='{}' WHERE id='capacity-at-account-canary'`); err != nil {
		t.Fatal(err)
	}
	want = IdentityCapacityDimensionMetrics{
		AccountsMeasured: 2, AccountsNearLimit: 1, AccountsUnlimited: 2,
		MinHeadroomRatio: 0.2,
	}
	assertIdentityCapacityMetrics(t, st, ctx, IdentityCapacityMetrics{
		Realms: want, AgentsPerRealm: want, OperatorSeats: want,
	})

	// Partial snapshots are unlimited only for the absent dimension. Give
	// dimensions different results so accidental cross-wiring is observable.
	if _, err := st.pool.Exec(ctx, `
		UPDATE accounts SET plan_limits='{"realms":0,"operator_seats":10}'
		 WHERE id='capacity-below-account-canary'`); err != nil {
		t.Fatal(err)
	}
	assertIdentityCapacityMetrics(t, st, ctx, IdentityCapacityMetrics{
		Realms: IdentityCapacityDimensionMetrics{
			AccountsMeasured: 2, AccountsNearLimit: 2, AccountsAtLimit: 1, AccountsUnlimited: 2,
		},
		AgentsPerRealm: IdentityCapacityDimensionMetrics{
			AccountsMeasured: 1, AccountsNearLimit: 1, AccountsUnlimited: 3, MinHeadroomRatio: 0.2,
		},
		OperatorSeats: want,
	})
	if _, err := st.pool.Exec(ctx, `
		UPDATE accounts SET plan_limits='{"realms":5,"agents_per_realm":10,"operator_seats":4}'
		 WHERE id='capacity-below-account-canary'`); err != nil {
		t.Fatal(err)
	}

	// A zero cap is finite, even with zero usage; over-cap usage is clamped
	// to zero headroom. Accounts with no live realm use zero agents.
	if _, err := st.pool.Exec(ctx, `
		UPDATE accounts SET plan_limits='{"realms":0,"agents_per_realm":0,"operator_seats":0}'
		 WHERE id='capacity-at-account-canary'`); err != nil {
		t.Fatal(err)
	}
	if _, err := st.pool.Exec(ctx, `
		INSERT INTO accounts (id, status, plan_limits)
		VALUES ('capacity-zero-account-canary', 'active', '{"realms":0,"agents_per_realm":0,"operator_seats":0}')`); err != nil {
		t.Fatal(err)
	}
	want = IdentityCapacityDimensionMetrics{
		AccountsMeasured: 4, AccountsNearLimit: 3, AccountsAtLimit: 2,
		AccountsUnlimited: 1, MinHeadroomRatio: 0,
	}
	assertIdentityCapacityMetrics(t, st, ctx, IdentityCapacityMetrics{
		Realms: want, AgentsPerRealm: want, OperatorSeats: want,
	})
	if _, err := st.pool.Exec(ctx, `UPDATE accounts SET plan_limits='{}'`); err != nil {
		t.Fatal(err)
	}
	unlimited := IdentityCapacityDimensionMetrics{AccountsUnlimited: 5, MinHeadroomRatio: 1}
	assertIdentityCapacityMetrics(t, st, ctx, IdentityCapacityMetrics{
		Realms: unlimited, AgentsPerRealm: unlimited, OperatorSeats: unlimited,
	})
}

func assertIdentityCapacityMetrics(t *testing.T, st *Store, ctx context.Context, want IdentityCapacityMetrics) {
	t.Helper()
	got, err := st.ReadIdentityCapacityMetrics(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, dimension := range []struct {
		name      string
		got, want IdentityCapacityDimensionMetrics
	}{
		{"realms", got.Realms, want.Realms},
		{"agents_per_realm", got.AgentsPerRealm, want.AgentsPerRealm},
		{"operator_seats", got.OperatorSeats, want.OperatorSeats},
	} {
		if dimension.got.AccountsMeasured != dimension.want.AccountsMeasured ||
			dimension.got.AccountsNearLimit != dimension.want.AccountsNearLimit ||
			dimension.got.AccountsAtLimit != dimension.want.AccountsAtLimit ||
			dimension.got.AccountsUnlimited != dimension.want.AccountsUnlimited ||
			math.Abs(dimension.got.MinHeadroomRatio-dimension.want.MinHeadroomRatio) > 1e-12 {
			t.Errorf("%s = %+v, want %+v", dimension.name, dimension.got, dimension.want)
		}
	}
}
