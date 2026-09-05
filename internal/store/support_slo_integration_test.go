package store

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/testenv"
)

// The SLO read against Postgres: empty queue reads zero/zero, an unanswered
// ticket counts with a positive age, and the first fleet-side reply clears it.
func TestReadSupportSLOMetricsPostgres(t *testing.T) {
	dsn := testenv.RequirePostgres(t)
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

	before, err := st.ReadSupportSLOMetrics(ctx)
	if err != nil {
		t.Fatal(err)
	}

	suffix := time.Now().UnixNano()
	provisioned, err := st.ProvisionAccount(
		ctx,
		fmt.Sprintf("support-slo-%d@witwave.ai", suffix),
		fmt.Sprintf("support slo %d", suffix),
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

	ticket, _, err := st.OpenTicket(ctx, OpenTicketInput{
		AccountID:  accountID,
		OperatorID: provisioned.OperatorID,
		Subject:    "slo probe",
		Body:       "how long until someone answers?",
	})
	if err != nil {
		t.Fatal(err)
	}
	during, err := st.ReadSupportSLOMetrics(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if during.UnansweredTickets != before.UnansweredTickets+1 ||
		during.OldestUnansweredSeconds < 0 {
		t.Fatalf("during = %+v (before %+v); want one more unanswered, age >= 0",
			during, before)
	}

	if _, err := st.ReplyAdminTicket(ctx, ReplyAdminInput{
		AccountID: accountID, AdminHandle: "scott",
		TicketID: ticket.ID, Body: "answering within the promise",
	}); err != nil {
		t.Fatal(err)
	}
	after, err := st.ReadSupportSLOMetrics(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if after.UnansweredTickets != before.UnansweredTickets {
		t.Fatalf("after reply unanswered = %d, want %d",
			after.UnansweredTickets, before.UnansweredTickets)
	}
}
