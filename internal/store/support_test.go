package store

import "testing"

// TestAdminHandleShape pins the admin-handle shape guard the store uses
// to reject malformed handles from a compromised or buggy control-plane
// caller. Must agree with server.validateAdminHandle and the Cloudflare
// Worker's ADMIN_HANDLE regex; drift here is a cross-tier integration bug.
func TestAdminHandleShape(t *testing.T) {
	tests := map[string]bool{
		"sarah":                true,
		"s2":                   true,
		"sarah_jones":          true,
		"a-b":                  true,
		"abcdefghijklmnopqrst": true,
		"":                     false,
		"a":                    false,
		"S":                    false,
		"1abc":                 false,
		"-abc":                 false,
		"has space":            false,
		"has.dot":              false,
		"has/slash":            false,
		"UPPER":                false,
		"way_too_long_way_too_long_beyond_32_chars": false,
	}
	for h, want := range tests {
		got := adminHandleRE.MatchString(h)
		if got != want {
			t.Errorf("adminHandleRE.MatchString(%q) = %v, want %v", h, got, want)
		}
	}
}

// TestIsKnownTicketState pins the state-machine domain: transitions in
// legalTransitions and the CHECK-free enforcement in ChangeTicketState /
// ChangeAdminTicketState depend on this set being exactly right. A stray
// state slipping through would let the admin path bypass the transition
// map at the "unknown state" branch.
func TestIsKnownTicketState(t *testing.T) {
	for _, s := range []string{
		TicketStateOpen, TicketStateAwaitingAdmin, TicketStateAwaitingCustomer,
		TicketStateResolved, TicketStateClosed,
	} {
		if !isKnownTicketState(s) {
			t.Errorf("isKnownTicketState(%q) = false, want true", s)
		}
	}
	for _, s := range []string{"", "reopened", "pending", "OPEN", " open"} {
		if isKnownTicketState(s) {
			t.Errorf("isKnownTicketState(%q) = true, want false", s)
		}
	}
}
