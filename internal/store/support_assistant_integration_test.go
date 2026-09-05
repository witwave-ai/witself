package store

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/testenv"
)

// The assistant write path, end to end against Postgres: an assistant reply
// lands under the fixed ("assistant", "assistant") identity regardless of the
// operating admin handle, swings the ticket to awaiting_customer, and sets
// first_response_at — the AI's first answer IS the SLA first response.
func TestReplyAdminTicketAsAssistantPostgres(t *testing.T) {
	dsn := testenv.RequirePostgres(t)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	st, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	// t.Cleanup, not defer: cleanups run LIFO after the test, so the row
	// deletions registered below execute while the pool is still open and
	// the pool closes last. A defer here would close the pool first.
	t.Cleanup(st.Close)
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}

	suffix := time.Now().UnixNano()
	provisioned, err := st.ProvisionAccount(
		ctx,
		fmt.Sprintf("assistant-author-%d@witwave.ai", suffix),
		fmt.Sprintf("assistant author %d", suffix),
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
		Subject:    "how do I rotate an agent token?",
		Body:       "asking so the assistant has something to answer",
	})
	if err != nil {
		t.Fatal(err)
	}

	msg, err := st.ReplyAdminTicket(ctx, ReplyAdminInput{
		AccountID:   accountID,
		AdminHandle: "scott", // the operating admin the runner acts under
		TicketID:    ticket.ID,
		Body:        "Here's how to rotate the token…",
		AsAssistant: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if msg.AuthorKind != MessageAuthorAssistant || msg.AuthorID != AssistantHandle {
		t.Fatalf("assistant reply author = %s:%s, want %s:%s — the labeling "+
			"promise depends on the store forcing this pair",
			msg.AuthorKind, msg.AuthorID,
			MessageAuthorAssistant, AssistantHandle)
	}

	after, messages, err := st.GetTicketAdmin(ctx, accountID, ticket.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.State != TicketStateAwaitingCustomer {
		t.Fatalf("state after assistant reply = %q, want %q",
			after.State, TicketStateAwaitingCustomer)
	}
	if after.FirstResponseAt == nil {
		t.Fatal("first_response_at not set by the assistant reply; the AI's " +
			"first answer is the SLA first response")
	}
	if len(messages) != 2 {
		t.Fatalf("message count = %d, want opening + assistant reply", len(messages))
	}

	// A human admin reply on the same ticket keeps human attribution — the
	// flag must never leak across calls.
	human, err := st.ReplyAdminTicket(ctx, ReplyAdminInput{
		AccountID:   accountID,
		AdminHandle: "scott",
		TicketID:    ticket.ID,
		Body:        "human follow-up",
	})
	if err != nil {
		t.Fatal(err)
	}
	if human.AuthorKind != MessageAuthorFleetAdmin || human.AuthorID != "scott" {
		t.Fatalf("human reply author = %s:%s, want fleet_admin:scott",
			human.AuthorKind, human.AuthorID)
	}
}
