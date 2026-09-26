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
	assertIdentityCapacityMetrics(ctx, t, st, IdentityCapacityMetrics{
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
	assertIdentityCapacityMetrics(ctx, t, st, IdentityCapacityMetrics{
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
	assertIdentityCapacityMetrics(ctx, t, st, IdentityCapacityMetrics{
		Realms: want, AgentsPerRealm: want, OperatorSeats: want,
	})

	// Partial snapshots are unlimited only for the absent dimension. Give
	// dimensions different results so accidental cross-wiring is observable.
	if _, err := st.pool.Exec(ctx, `
		UPDATE accounts SET plan_limits='{"realms":0,"operator_seats":10}'
		 WHERE id='capacity-below-account-canary'`); err != nil {
		t.Fatal(err)
	}
	assertIdentityCapacityMetrics(ctx, t, st, IdentityCapacityMetrics{
		Realms: IdentityCapacityDimensionMetrics{
			AccountsMeasured: 2, AccountsNearLimit: 1, AccountsUnlimited: 2, MinHeadroomRatio: 0.2,
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

	// Zero caps remain measured but have no elective capacity, even with
	// over-cap usage. Accounts with no live realm use zero agents.
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
		AccountsMeasured: 4, AccountsNearLimit: 1,
		AccountsUnlimited: 1, MinHeadroomRatio: 0.2,
	}
	assertIdentityCapacityMetrics(ctx, t, st, IdentityCapacityMetrics{
		Realms: want, AgentsPerRealm: want, OperatorSeats: want,
	})
	if _, err := st.pool.Exec(ctx, `UPDATE accounts SET plan_limits='{}'`); err != nil {
		t.Fatal(err)
	}
	unlimited := IdentityCapacityDimensionMetrics{AccountsUnlimited: 5, MinHeadroomRatio: 1}
	assertIdentityCapacityMetrics(ctx, t, st, IdentityCapacityMetrics{
		Realms: unlimited, AgentsPerRealm: unlimited, OperatorSeats: unlimited,
	})
}

func assertIdentityCapacityMetrics(ctx context.Context, t *testing.T, st *Store, want IdentityCapacityMetrics) {
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

// Provision accounts through the real seed path: this also detects changes to
// the baseline (especially accidental agent seeding) rather than assuming it
// from hand-written rows. Limits are applied after fixtures are populated so
// we can exercise downgraded and over-cap snapshots as well as ordinary usage.
func TestIdentityCapacityElectiveCapsPostgres(t *testing.T) {
	st, _ := newMigrationTestStore(t, testenv.RequirePostgres(t))
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	baseline := IdentityCapacityDimensionMetrics{AccountsMeasured: 1, MinHeadroomRatio: 1}
	full := IdentityCapacityDimensionMetrics{AccountsMeasured: 1, AccountsNearLimit: 1, AccountsAtLimit: 1}
	near := IdentityCapacityDimensionMetrics{AccountsMeasured: 1, AccountsNearLimit: 1, MinHeadroomRatio: .2}
	below := IdentityCapacityDimensionMetrics{AccountsMeasured: 1, MinHeadroomRatio: .4}
	unlimited := IdentityCapacityDimensionMetrics{AccountsUnlimited: 1, MinHeadroomRatio: 1}
	for _, tc := range []struct {
		name, plan, limits        string
		realms, agents, operators int
		want                      IdentityCapacityMetrics
	}{
		{"Personal structural caps", "free", `{"realms":1,"agents_per_realm":10,"operator_seats":1}`, 1, 0, 1, IdentityCapacityMetrics{baseline, baseline, baseline}},
		{"Personal raised caps full", "free", `{"realms":2,"agents_per_realm":2,"operator_seats":2}`, 2, 2, 2, IdentityCapacityMetrics{full, full, full}},
		{"Professional three seats", "standard", `{"realms":1,"agents_per_realm":100,"operator_seats":3}`, 1, 0, 3, IdentityCapacityMetrics{baseline, baseline, full}},
		{"unlimited", "enterprise", `{}`, 2, 6, 3, IdentityCapacityMetrics{unlimited, unlimited, unlimited}},
		{"explicit null unlimited", "enterprise", `{"realms":null,"agents_per_realm":null,"operator_seats":null}`, 1, 0, 1, IdentityCapacityMetrics{unlimited, unlimited, unlimited}},
		{"exactly eighty percent", "free", `{"realms":5,"agents_per_realm":5,"operator_seats":5}`, 4, 4, 4, IdentityCapacityMetrics{near, near, near}},
		{"below eighty percent", "free", `{"realms":5,"agents_per_realm":5,"operator_seats":5}`, 3, 3, 3, IdentityCapacityMetrics{below, below, below}},
		{"near agents with excluded baseline caps", "free", `{"realms":1,"agents_per_realm":5,"operator_seats":1}`, 1, 4, 1, IdentityCapacityMetrics{baseline, near, baseline}},
		{"one agent is elective", "free", `{"realms":1,"agents_per_realm":1,"operator_seats":1}`, 1, 1, 1, IdentityCapacityMetrics{baseline, full, baseline}},
		{"zero caps", "free", `{"realms":0,"agents_per_realm":0,"operator_seats":0}`, 0, 0, 1, IdentityCapacityMetrics{baseline, baseline, baseline}},
		{"over structural caps", "free", `{"realms":1,"agents_per_realm":0,"operator_seats":1}`, 2, 1, 2, IdentityCapacityMetrics{baseline, baseline, baseline}},
		{"over elective caps", "free", `{"realms":2,"agents_per_realm":2,"operator_seats":2}`, 3, 3, 3, IdentityCapacityMetrics{full, full, full}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			account, err := st.ProvisionAccount(ctx, "capacity@example.test", "capacity", time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := st.ActivateAccount(ctx, account.AccountID); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if _, err := st.pool.Exec(ctx, `UPDATE accounts SET status='suspended' WHERE id=$1`, account.AccountID); err != nil {
					t.Fatal(err)
				}
			})
			for i := range tc.realms {
				realm, err := st.CreateRealm(ctx, account.AccountID, fmt.Sprintf("realm-%d", i))
				if err != nil {
					t.Fatal(err)
				}
				if _, err := st.pool.Exec(ctx, `INSERT INTO agents (id, realm_id, name)
				 SELECT $1 || '-agent-' || n, $1, 'agent-' || n FROM generate_series(1, $2::int) n`, realm.ID, tc.agents); err != nil {
					t.Fatal(err)
				}
			}
			// The first operator comes from provisioning, not the fixture.
			if _, err := st.pool.Exec(ctx, `INSERT INTO operators (id, account_id, role)
			 SELECT $1 || '-operator-' || n, $1, 'account_operator' FROM generate_series(2, $2::int) n`, account.AccountID, tc.operators); err != nil {
				t.Fatal(err)
			}
			if _, err := st.pool.Exec(ctx, `UPDATE accounts SET plan=$2, plan_limits=$3::jsonb WHERE id=$1`, account.AccountID, tc.plan, tc.limits); err != nil {
				t.Fatal(err)
			}
			assertIdentityCapacityMetrics(ctx, t, st, tc.want)
		})
	}
}

func TestIdentityCapacityBootstrapBaselinePostgres(t *testing.T) {
	st, _ := newMigrationTestStore(t, testenv.RequirePostgres(t))
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	account, err := st.ProvisionAccount(ctx, "baseline@example.test", "baseline", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.ActivateAccount(ctx, account.AccountID); err != nil {
		t.Fatal(err)
	}
	// Provisioning creates the owner and nothing else: prove no realm or agent
	// is seeded, so the one-realm baseline below is the stated policy (the first
	// realm every account creates), not something provisioning does.
	var seededRealms, seededAgents int64
	if err := st.pool.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM realms WHERE account_id=$1 AND deleted_at IS NULL),
		(SELECT count(*) FROM agents a JOIN realms r ON r.id=a.realm_id WHERE r.account_id=$1 AND a.deleted_at IS NULL)`,
		account.AccountID).Scan(&seededRealms, &seededAgents); err != nil {
		t.Fatal(err)
	}
	if seededRealms != 0 || seededAgents != 0 {
		t.Fatalf("provisioning seeded %d realms and %d agents; the structural baseline assumes none", seededRealms, seededAgents)
	}
	// The first realm is a separate operation. Compare actual counts after it
	// to the policy consumed by the collector so a changed bootstrap contract
	// cannot silently drift.
	if _, err := st.CreateRealm(ctx, account.AccountID, "default"); err != nil {
		t.Fatal(err)
	}
	var realms, agents, operators int64
	if err := st.pool.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM realms WHERE account_id=$1 AND deleted_at IS NULL),
		(SELECT count(*) FROM agents a JOIN realms r ON r.id=a.realm_id
		 WHERE r.account_id=$1 AND r.deleted_at IS NULL AND a.deleted_at IS NULL),
		(SELECT count(*) FROM operators WHERE account_id=$1 AND deleted_at IS NULL)`,
		account.AccountID).Scan(&realms, &agents, &operators); err != nil {
		t.Fatal(err)
	}
	wantRealms, wantAgents, wantOperators := identityCapacityStructuralMinimums()
	if realms != wantRealms || agents != wantAgents || operators != wantOperators {
		t.Fatalf("bootstrap counts (%d, %d, %d) differ from structural minimums (%d, %d, %d)",
			realms, agents, operators, wantRealms, wantAgents, wantOperators)
	}
}
