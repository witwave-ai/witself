package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/testenv"
)

func closedCohortScope(canary string, accountIDs ...string) AgentEmailReceiveScope {
	scope := cohortScope(accountIDs[0], canary, "closed-cell")
	for _, accountID := range accountIDs[1:] {
		scope.AccountIDs[accountID] = true
	}
	return scope
}

func TestAgentEmailCohortClosedAccountStartupPostgres(t *testing.T) {
	st := cohortStore(t, testenv.RequirePostgres(t))
	ctx := context.Background()
	const absent = "acc_zzzzzzzzzzzzzzzz"
	closed, _, closedAgent := deferralAccount(t, st, 1)
	resident, _, residentAgent := deferralAccount(t, st, 2)
	if err := st.CloseAccount(ctx, closed.AccountID, closed.OperatorID, "closed cohort fixture"); err != nil {
		t.Fatalf("close fixture: %T", err)
	}

	requireResidency(t, st, closedCohortScope("", closed.AccountID),
		AgentEmailProductionCohortResidency{1, 0, 0, 0, 1, AgentEmailRetryCanaryNone})
	requireResidency(t, st, closedCohortScope(residentAgent.ID, closed.AccountID, resident.AccountID, absent),
		AgentEmailProductionCohortResidency{3, 1, 0, 1, 1, AgentEmailRetryCanaryReady})
	requireResidency(t, st, closedCohortScope(closedAgent.ID, closed.AccountID),
		AgentEmailProductionCohortResidency{1, 0, 0, 0, 1, AgentEmailRetryCanaryClosed})
	requireResidency(t, st, closedCohortScope(closedAgent.ID, closed.AccountID, resident.AccountID),
		AgentEmailProductionCohortResidency{2, 1, 0, 0, 1, AgentEmailRetryCanaryClosed})

	// A canary in a closed account that the cohort does not list still fails closed.
	if _, err := st.ValidateAgentEmailProductionCohort(ctx, closedCohortScope(closedAgent.ID, resident.AccountID)); !errors.Is(err, ErrAgentEmailPilotNotEnrolled) {
		t.Fatalf("unlisted closed canary: %T", err)
	}

	// Strict operator operations keep refusing a listed closed account.
	strict := closedCohortScope("", closed.AccountID)
	if _, err := st.PreflightAgentEmailProductionCohort(ctx, strict); !errors.Is(err, ErrAccountNotActive) {
		t.Fatalf("strict preflight: %T", err)
	}
	if _, err := st.ReconcileAgentEmailProductionCohortWithOverrides(ctx, strict, nil); !errors.Is(err, ErrAccountNotActive) {
		t.Fatalf("strict reconcile: %T", err)
	}
	if _, err := st.ListAgentEmailProductionCanaryAgents(ctx, strict); !errors.Is(err, ErrAccountNotActive) {
		t.Fatalf("strict manifest: %T", err)
	}

	// Every other status still stops startup.
	pending, err := st.ProvisionAccount(ctx, "closed-pending@example.test", "pending fixture", time.Hour)
	if err != nil {
		t.Fatalf("provision pending fixture: %T", err)
	}
	if _, err := st.ValidateAgentEmailProductionCohort(ctx, closedCohortScope("", pending.AccountID)); !errors.Is(err, ErrAccountNotActive) {
		t.Fatalf("pending account: %T", err)
	}
	other, _, _ := deferralAccount(t, st, 3)
	if _, err := st.pool.Exec(ctx, `UPDATE accounts SET status='verifying' WHERE id=$1`, other.AccountID); err != nil {
		t.Fatalf("set unexpected status: %T", err)
	}
	if _, err := st.ValidateAgentEmailProductionCohort(ctx, closedCohortScope("", closed.AccountID, other.AccountID)); !errors.Is(err, ErrAccountNotActive) {
		t.Fatalf("unexpected status: %T", err)
	}

	// The expiry reaper closes a pending account without an operator; it is then counted.
	if reaped, err := st.ReapPendingAccount(ctx, pending.AccountID, "expired fixture"); err != nil || !reaped {
		t.Fatalf("reap pending fixture: %T", err)
	}
	requireResidency(t, st, closedCohortScope("", pending.AccountID),
		AgentEmailProductionCohortResidency{1, 0, 0, 0, 1, AgentEmailRetryCanaryNone})

	// A canary outside the listed closed accounts still fails closed while one is listed.
	for _, scope := range []AgentEmailReceiveScope{
		closedCohortScope(residentAgent.ID, closed.AccountID), // agent in an unlisted active account
		closedCohortScope(closedAgent.ID, pending.AccountID),  // agent in an unlisted closed account
	} {
		if _, err := st.ValidateAgentEmailProductionCohort(ctx, scope); !errors.Is(err, ErrAgentEmailPilotNotEnrolled) {
			t.Fatalf("canary outside the listed closed accounts: %T", err)
		}
	}
}

func TestAgentEmailCohortClosedAccountIngestPostgres(t *testing.T) {
	st := cohortStore(t, testenv.RequirePostgres(t))
	ctx := context.Background()
	const audience = "closed-cell"
	cases := []struct {
		name     string
		status   string
		disabled bool
		want     error
	}{
		{name: "closed", status: "closed", want: ErrAgentEmailUnknownRecipient},
		{name: "closed_receive_disabled", status: "closed", disabled: true, want: ErrAgentEmailUnknownRecipient},
		{name: "pending", status: "pending", want: ErrAccountNotActive},
		{name: "unexpected_status", status: "verifying", want: ErrAccountNotActive},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			account, realm, agent := deferralAccount(t, st, i+1)
			listed := cohortScope(account.AccountID, "", audience)
			address, err := st.EnsureAgentEmailMailbox(ctx, listed, account.AccountID, realm.ID, agent.ID, "")
			if err != nil {
				t.Fatalf("create fixture mailbox: %T", err)
			}
			if tc.disabled {
				if _, err := st.SetAgentEmailReceiveControl(ctx, listed, account.AccountID, account.OperatorID, agent.ID, AgentEmailReceiveDisabled); err != nil {
					t.Fatalf("disable fixture receive: %T", err)
				}
			}
			if tc.status == "closed" {
				if err := st.CloseAccount(ctx, account.AccountID, account.OperatorID, "closed cohort fixture"); err != nil {
					t.Fatalf("close fixture: %T", err)
				}
			} else if _, err := st.pool.Exec(ctx, `UPDATE accounts SET status=$2 WHERE id=$1`, account.AccountID, tc.status); err != nil {
				t.Fatalf("set fixture status: %T", err)
			}
			before := deferralRowCounts(t, st, account.AccountID)
			_, err = st.IngestAgentEmailPilot(ctx, listed, deferralIngestInput(address.Address, audience))
			if !errors.Is(err, tc.want) {
				t.Fatal("ingest error did not match expected class")
			}
			if tc.want == ErrAgentEmailUnknownRecipient && errors.Is(err, ErrAccountNotActive) {
				t.Fatal("closed account also matched the temporary account class")
			}
			if errors.Is(err, ErrAgentEmailReceiveCohortDeferred) || errors.Is(err, ErrAgentEmailPilotNotEnrolled) {
				t.Fatal("listed account result matched a scope-miss class")
			}
			if deferralRowCounts(t, st, account.AccountID) != before {
				t.Fatal("refused ingest stored a row or debited a rate bucket")
			}
			// The lock decides the class without knowing the receive mode.
			tx, err := st.pool.Begin(ctx)
			if err != nil {
				t.Fatalf("begin lock check: %T", err)
			}
			_, lockErr := lockAgentEmailIngestAccountPolicy(ctx, tx, account.AccountID)
			if err := tx.Rollback(ctx); err != nil {
				t.Fatalf("rollback lock check: %T", err)
			}
			if !errors.Is(lockErr, tc.want) {
				t.Fatal("account-policy lock did not return the expected class")
			}
		})
	}
}
