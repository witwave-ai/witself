package supportstates

import (
	"slices"
	"testing"
)

// TestGraphWellFormed pins two invariants the whole system depends on:
// every transition target must be a known state (typo defense), and
// every terminal state must have zero outgoing edges (state-machine
// correctness). If this fails, the CLI graph is wrong AND the store's
// legal-transition check has a hole.
func TestGraphWellFormed(t *testing.T) {
	states := States()
	stateSet := make(map[string]bool, len(states))
	for _, s := range states {
		stateSet[s] = true
	}

	transitions := LegalTransitions()

	// Every state has an entry (even if empty).
	if len(transitions) != len(states) {
		t.Errorf("transitions map has %d entries, want %d (one per state)",
			len(transitions), len(states))
	}
	for _, s := range states {
		if _, ok := transitions[s]; !ok {
			t.Errorf("state %q missing from transitions map", s)
		}
	}
	// Every target is a known state.
	for src, dsts := range transitions {
		for _, d := range dsts {
			if !stateSet[d] {
				t.Errorf("transition %s -> %s: unknown target state", src, d)
			}
		}
	}
	// Terminal states have no outgoing.
	for _, term := range TerminalStates() {
		if got := transitions[term]; len(got) != 0 {
			t.Errorf("terminal state %q has outgoing transitions %v", term, got)
		}
	}
	// No self-loops (a state transitioning to itself is a no-op the
	// store handles separately; it should not appear in the graph).
	for src, dsts := range transitions {
		if slices.Contains(dsts, src) {
			t.Errorf("state %q lists itself as a target — self-loops belong in the store's idempotent-noop branch, not the graph", src)
		}
	}
}

// TestLegalTransitionsReturnsCopy pins that mutating a returned map
// does not leak into subsequent callers. A TUI, an agent, and the
// store all call this — a shared underlying slice would be a nightmare.
func TestLegalTransitionsReturnsCopy(t *testing.T) {
	a := LegalTransitions()
	a[StateOpen] = append(a[StateOpen], "hacked")
	a[StateClosed] = []string{"hacked"}
	b := LegalTransitions()
	if slices.Contains(b[StateOpen], "hacked") {
		t.Errorf("mutation leaked across callers: %v", b[StateOpen])
	}
	if len(b[StateClosed]) != 0 {
		t.Errorf("closed state was corrupted: %v", b[StateClosed])
	}
}
