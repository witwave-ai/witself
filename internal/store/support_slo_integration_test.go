package store

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/testenv"
)

// The SLO read against Postgres distinguishes oldest/newest unanswered ages
// and urgent tickets, then clears each ticket after its first fleet-side reply.
// Counts use deltas because other tests can leave tickets in the shared schema.
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
		during.OldestUnansweredSeconds < 0 ||
		during.NewestUnansweredSeconds < 0 || during.NewestUnansweredSeconds > 60 ||
		during.NewestUnansweredSeconds > during.OldestUnansweredSeconds ||
		during.UnansweredUrgentTickets != before.UnansweredUrgentTickets {
		t.Fatalf("during = %+v (before %+v); want one more unanswered, newest age 0..60 <= oldest, unchanged urgent count",
			during, before)
	}

	if _, err := st.pool.Exec(ctx,
		`UPDATE support_tickets SET opened_at = now() - interval '2 hours' WHERE account_id = $1 AND id = $2`,
		accountID, ticket.ID); err != nil {
		t.Fatal(err)
	}
	urgent, _, err := st.OpenTicket(ctx, OpenTicketInput{
		AccountID:  accountID,
		OperatorID: provisioned.OperatorID,
		Subject:    "urgent slo probe",
		Body:       "an urgent first response is needed",
		Priority:   TicketPriorityUrgent,
	})
	if err != nil {
		t.Fatal(err)
	}
	withUrgent, err := st.ReadSupportSLOMetrics(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if withUrgent.UnansweredTickets != before.UnansweredTickets+2 ||
		withUrgent.UnansweredUrgentTickets != before.UnansweredUrgentTickets+1 ||
		withUrgent.OldestUnansweredSeconds < 7200 ||
		withUrgent.NewestUnansweredSeconds < 0 || withUrgent.NewestUnansweredSeconds > 60 ||
		withUrgent.NewestUnansweredSeconds > withUrgent.OldestUnansweredSeconds {
		t.Fatalf("with urgent = %+v (before %+v); want two more unanswered, one more urgent, oldest age >= 7200, newest age 0..60 <= oldest",
			withUrgent, before)
	}

	if _, err := st.ReplyAdminTicket(ctx, ReplyAdminInput{
		AccountID: accountID, AdminHandle: "scott",
		TicketID: urgent.ID, Body: "answering the urgent ticket first",
	}); err != nil {
		t.Fatal(err)
	}
	afterUrgent, err := st.ReadSupportSLOMetrics(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if afterUrgent.UnansweredUrgentTickets != before.UnansweredUrgentTickets ||
		afterUrgent.UnansweredTickets != before.UnansweredTickets+1 {
		t.Fatalf("after urgent reply = %+v (before %+v); want unchanged urgent count and one more unanswered",
			afterUrgent, before)
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
