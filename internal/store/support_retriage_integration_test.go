package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"
)

// Re-triage against Postgres: category/priority rewrite with a full audit
// event, idempotent no-op, and the closed-ticket refusal. Re-triage is
// metadata, not conversation — state, first_response_at, and
// last_activity_at must be untouched.
func TestRetriageTicketAdminPostgres(t *testing.T) {
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
		fmt.Sprintf("retriage-%d@witwave.ai", suffix),
		fmt.Sprintf("retriage %d", suffix),
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
		Subject:    "cannot send agent email",
		Body:       "delivery fails since this morning",
	})
	if err != nil {
		t.Fatal(err)
	}
	if ticket.Category != TicketCategoryOther || ticket.Priority != TicketPriorityNormal {
		t.Fatalf("fixture defaults moved: %s/%s", ticket.Category, ticket.Priority)
	}

	// Triage pass: technical, urgent.
	after, err := st.RetriageTicketAdmin(ctx, RetriageAdminInput{
		AccountID: accountID, AdminHandle: "scott", TicketID: ticket.ID,
		Category: TicketCategoryTechnical, Priority: TicketPriorityUrgent,
	})
	if err != nil {
		t.Fatal(err)
	}
	if after.Category != TicketCategoryTechnical || after.Priority != TicketPriorityUrgent {
		t.Fatalf("re-triage result = %s/%s", after.Category, after.Priority)
	}
	if after.State != ticket.State || after.FirstResponseAt != nil ||
		!after.LastActivityAt.Equal(ticket.LastActivityAt) {
		t.Fatalf("re-triage touched conversation fields: state=%q first=%v last=%v",
			after.State, after.FirstResponseAt, after.LastActivityAt)
	}

	events := func() []Event {
		t.Helper()
		page, err := st.ListAccountEvents(ctx, accountID, provisioned.OperatorID,
			EventFilter{Verb: VerbSupportTicketRetriaged})
		if err != nil {
			t.Fatal(err)
		}
		return page.Events
	}
	got := events()
	if len(got) != 1 {
		t.Fatalf("retriage events = %d, want 1", len(got))
	}
	var meta map[string]string
	if err := json.Unmarshal(got[0].Metadata, &meta); err != nil {
		t.Fatal(err)
	}
	if got[0].ActorKind != ActorControlPlane ||
		meta["admin_handle"] != "scott" ||
		meta["category_from"] != TicketCategoryOther ||
		meta["category_to"] != TicketCategoryTechnical ||
		meta["priority_from"] != TicketPriorityNormal ||
		meta["priority_to"] != TicketPriorityUrgent {
		t.Fatalf("audit event = actor %q meta %v", got[0].ActorKind, meta)
	}

	// Exact no-op: same values, no second audit event.
	if _, err := st.RetriageTicketAdmin(ctx, RetriageAdminInput{
		AccountID: accountID, AdminHandle: "scott", TicketID: ticket.ID,
		Category: TicketCategoryTechnical, Priority: TicketPriorityUrgent,
	}); err != nil {
		t.Fatal(err)
	}
	if n := len(events()); n != 1 {
		t.Fatalf("no-op re-triage emitted an event: %d", n)
	}

	// Closed tickets are settled history.
	for _, state := range []string{TicketStateResolved, TicketStateClosed} {
		if _, err := st.ChangeAdminTicketState(ctx, ChangeAdminStateInput{
			AccountID: accountID, AdminHandle: "scott",
			TicketID: ticket.ID, NewState: state,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.RetriageTicketAdmin(ctx, RetriageAdminInput{
		AccountID: accountID, AdminHandle: "scott", TicketID: ticket.ID,
		Priority: TicketPriorityLow,
	}); !errors.Is(err, ErrTicketStateInvalid) {
		t.Fatalf("re-triage on closed ticket = %v, want state refusal", err)
	}
}
