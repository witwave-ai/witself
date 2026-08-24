package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/plans"
)

// The plan entitlement gate on OpenTicket against Postgres: a plan without
// the support feature refuses, a plan with it admits, and an account whose
// cell never received a snapshot (legacy zero-value plan) is not locked out.
// The operator kill-switch stays independent and is not exercised here.
func TestOpenTicketPlanEntitlementPostgres(t *testing.T) {
	dsn := os.Getenv("WITSELF_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("WITSELF_TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	st, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}

	suffix := time.Now().UnixNano()
	provisioned, err := st.ProvisionAccount(
		ctx,
		fmt.Sprintf("support-ent-%d@witwave.ai", suffix),
		fmt.Sprintf("support ent %d", suffix),
		time.Hour,
	)
	if err != nil {
		t.Fatal(err)
	}
	accountID := provisioned.AccountID
	t.Cleanup(func() {
		cctx, ccancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer ccancel()
		for _, del := range []string{
			`DELETE FROM support_ticket_messages WHERE account_id = $1`,
			`DELETE FROM support_tickets WHERE account_id = $1`,
			`DELETE FROM account_events WHERE account_id = $1`,
			`DELETE FROM tokens WHERE operator_id IN
			   (SELECT id FROM operators WHERE account_id = $1)`,
			`DELETE FROM operators WHERE account_id = $1`,
			`DELETE FROM accounts WHERE id = $1`,
		} {
			if _, err := st.pool.Exec(cctx, del, accountID); err != nil {
				t.Errorf("cleanup %q: %v", del, err)
			}
		}
	})
	if activated, err := st.ActivateAccount(ctx, accountID); err != nil || !activated {
		t.Fatalf("activate = %v / %v", activated, err)
	}

	open := func() error {
		_, _, err := st.OpenTicket(ctx, OpenTicketInput{
			AccountID:  accountID,
			OperatorID: provisioned.OperatorID,
			Subject:    "entitlement probe",
			Body:       "does my plan include support?",
		})
		return err
	}

	// No snapshot ever applied (plan_snapshot_revision 0 — the column default
	// plan='free' notwithstanding): pre-snapshot accounts keep support.
	if err := open(); err != nil {
		t.Fatalf("zero-value plan open = %v, want allowed", err)
	}

	applyPlan := func(plan string, features []string) {
		t.Helper()
		hash, err := plans.SnapshotHash(plan, map[string]int64{}, map[string]int64{}, features)
		if err != nil {
			t.Fatal(err)
		}
		rev := time.Now().UnixNano()
		if _, err := st.SetAccountPlan(
			ctx, accountID, rev, hash, plan,
			map[string]int64{}, map[string]int64{}, features,
		); err != nil {
			t.Fatal(err)
		}
	}

	// The free plan carries no support feature: refused, with the sentinel
	// the HTTP layer maps to an entitlement 403.
	applyPlan("free", []string{"memory", "facts"})
	if err := open(); !errors.Is(err, ErrSupportNotIncluded) {
		t.Fatalf("free-plan open = %v, want ErrSupportNotIncluded", err)
	}

	// A paid plan with the feature admits.
	applyPlan("standard", []string{"memory", "facts", "support"})
	if err := open(); err != nil {
		t.Fatalf("support-plan open = %v, want allowed", err)
	}
}
