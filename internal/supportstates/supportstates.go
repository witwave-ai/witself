// Package supportstates is the single source of truth for the
// support-ticket state machine. Sits below both the store (which
// enforces transitions on write) and the CLIs (which render the
// graph so a TUI can decide which buttons to show and an AI agent
// can decide "can I move this forward"). By living in its own
// pgx-free package, it's cheap for the witself-admin binary to
// import — no server-only deps get dragged in.
package supportstates

// State names. Match support_ticket_messages.state column values;
// the store's TicketStateXxx constants alias these strings so a
// downstream refactor of either side stays consistent.
const (
	StateOpen             = "open"
	StateAwaitingAdmin    = "awaiting_admin"
	StateAwaitingCustomer = "awaiting_customer"
	StateResolved         = "resolved"
	StateClosed           = "closed"
)

// States returns the full state set in the natural lifecycle order:
// open → awaiting_admin ↔ awaiting_customer → resolved → closed.
// The order matters for human-readable renderings (docs, TUIs).
func States() []string {
	return []string{
		StateOpen,
		StateAwaitingAdmin,
		StateAwaitingCustomer,
		StateResolved,
		StateClosed,
	}
}

// TerminalStates lists states from which no transition is legal.
// The store's ChangeTicketState rejects any transition FROM these.
func TerminalStates() []string {
	return []string{StateClosed}
}

// LegalTransitions returns a fresh copy of the transition map so a
// mutating caller can't corrupt the source of truth. Semantics:
//   - open → {awaiting_admin, awaiting_customer, resolved, closed}
//     Kept for completeness; slice-1a's OpenTicket always initialises
//     to awaiting_admin, so 'open' is not reachable through the API,
//     but a restored archive could carry it and a future auto-open
//     path (agent-flagged, admin-authored) could produce it.
//   - awaiting_admin ↔ awaiting_customer  (replies swap the ball)
//   - any non-closed → resolved            (admin closes issue)
//   - resolved → closed                    (customer confirms)
//   - resolved → awaiting_admin            (customer or admin reopens)
//   - closed → {}                          (terminal)
//
// The store's ChangeTicketState / ChangeAdminTicketState enforces
// this map; the tenant + admin ReplyToTicket paths auto-transition
// (to awaiting_admin and awaiting_customer respectively) which is
// implicit in this graph.
func LegalTransitions() map[string][]string {
	src := map[string][]string{
		StateOpen:             {StateAwaitingAdmin, StateAwaitingCustomer, StateResolved, StateClosed},
		StateAwaitingAdmin:    {StateAwaitingCustomer, StateResolved, StateClosed},
		StateAwaitingCustomer: {StateAwaitingAdmin, StateResolved, StateClosed},
		StateResolved:         {StateAwaitingAdmin, StateClosed},
		StateClosed:           {},
	}
	out := make(map[string][]string, len(src))
	for k, v := range src {
		cp := make([]string, len(v))
		copy(cp, v)
		out[k] = cp
	}
	return out
}
