package supportrunner

import "testing"

func TestDeterministicGate(t *testing.T) {
	tests := []struct {
		name       string
		category   string
		subject    string
		customer   string
		wantGate   bool
		wantChange retriage
	}{
		{name: "security category", category: ticketCategorySecurity, wantGate: true, wantChange: retriage{Priority: ticketPriorityUrgent}},
		{name: "billing category", category: ticketCategoryBilling, wantGate: true},
		{name: "refund", subject: "Need a refund", wantGate: true},
		{name: "chargeback", customer: "I may CHARGEBACK this", wantGate: true},
		{name: "dispute", customer: "I dispute the charge", wantGate: true},
		{name: "legal", customer: "This is a legal matter", wantGate: true},
		{name: "lawyer", customer: "My lawyer asked", wantGate: true},
		{name: "attorney", customer: "An attorney will write", wantGate: true},
		{name: "subpoena", customer: "We received a subpoena", wantGate: true},
		{name: "court", customer: "See you in court", wantGate: true},
		{name: "delete my data", customer: "Please delete   my data", wantGate: true},
		{name: "data deletion", customer: "Requesting data deletion", wantGate: true},
		{name: "erasure", customer: "I request erasure", wantGate: true},
		{name: "forgotten", customer: "Exercise my right to be forgotten", wantGate: true},
		{name: "gdpr", customer: "This is under GDPR.", wantGate: true},
		{name: "vulnerability", customer: "I found a vulnerability", wantGate: true, wantChange: retriage{Category: ticketCategorySecurity, Priority: ticketPriorityUrgent}},
		{name: "breach", customer: "Possible breach", wantGate: true, wantChange: retriage{Category: ticketCategorySecurity, Priority: ticketPriorityUrgent}},
		{name: "hacked", customer: "We were hacked", wantGate: true, wantChange: retriage{Category: ticketCategorySecurity, Priority: ticketPriorityUrgent}},
		{name: "compromised", customer: "Credentials are compromised", wantGate: true, wantChange: retriage{Category: ticketCategorySecurity, Priority: ticketPriorityUrgent}},
		{name: "speak human", customer: "I need to speak to a human", wantGate: true},
		{name: "talk human", customer: "Can I talk to a human?", wantGate: true},
		{name: "real person", customer: "Get me a real person", wantGate: true},
		{name: "ordinary", category: ticketCategoryTechnical, subject: "CLI issue", customer: "How do I configure it?"},
		{name: "word boundary refund", customer: "The value is refundable"},
		{name: "word boundary legal", customer: "The document is legalistic"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			thread := gateThread(test.category, test.subject, test.customer, authorKindOwner)
			got := deterministicGate(thread)
			if got.Escalate != test.wantGate || got.Retriage != test.wantChange {
				t.Fatalf("gate = %+v, want escalate=%t retriage=%+v", got, test.wantGate, test.wantChange)
			}
		})
	}
}

func TestDeterministicGateScansOnlyCustomerBodies(t *testing.T) {
	thread := gateThread(ticketCategoryTechnical, "ordinary question", "legal refund breach", authorKindAssistant)
	thread.Messages = append(thread.Messages, ticketMessage{
		ID: "tkm_last", AuthorKind: authorKindOwner, Body: "How do I fix this?",
	})
	thread.Ticket.LastMessageID = "tkm_last"
	if got := deterministicGate(thread); got.Escalate {
		t.Fatalf("assistant-authored keywords gated ticket: %+v", got)
	}
}

func TestDeterministicGateMergesSecurityKeywordWithCategoryGate(t *testing.T) {
	thread := gateThread(ticketCategoryBilling, "Billing breach", "details", authorKindOwner)
	want := retriage{Category: ticketCategorySecurity, Priority: ticketPriorityUrgent}
	if got := deterministicGate(thread); !got.Escalate || got.Retriage != want {
		t.Fatalf("gate = %+v, want merged security re-triage %+v", got, want)
	}
}

func gateThread(category, subject, body, authorKind string) ticketThread {
	if category == "" {
		category = ticketCategoryOther
	}
	return ticketThread{
		Ticket: ticket{
			ID:            "tkt_1",
			AccountID:     "acc_1",
			Subject:       subject,
			Category:      category,
			State:         ticketStateAwaitingAdmin,
			Priority:      ticketPriorityNormal,
			LastMessageID: "tkm_1",
		},
		Messages: []ticketMessage{{
			ID: "tkm_1", AuthorKind: authorKind, Body: body,
		}},
	}
}
