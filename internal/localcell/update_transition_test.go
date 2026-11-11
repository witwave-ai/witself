package localcell

import (
	"slices"
	"strings"
	"testing"
)

func TestUpdateJournalTransitionMatrix(t *testing.T) {
	// These literal edges are the protocol oracle, independent of the production
	// state inventory, validity check and index arithmetic.
	allowed := map[string][]string{
		"":                      {"prepared"},
		"prepared":              {"prepared", "maintenance_requested"},
		"maintenance_requested": {"maintenance_requested", "acknowledged"},
		"acknowledged":          {"acknowledged", "quiesced", "maintenance_requested"},
		"quiesced":              {"quiesced", "recovery_verified"},
		"recovery_verified":     {"recovery_verified", "migrating"},
		"migrating":             {"migrating", "activated"},
		"activated":             {"activated", "health_verified"},
		"health_verified":       {"health_verified", "committed"},
		"committed":             {"committed"},
	}
	states := []string{"prepared", "maintenance_requested", "acknowledged", "quiesced",
		"recovery_verified", "migrating", "activated", "health_verified", "committed"}
	fromStates := append([]string{""}, states...)
	fromStates = append(fromStates, "garbage", "operation", "revert_stop", "PREPARED", "prepared ")
	toStates := append(slices.Clone(states), "", "garbage", "operation")
	// CI's result checker accepts a closed subtest-name alphabet with no quote
	// characters, so the empty state and the trailing space are spelled out.
	label := func(state string) string {
		if state == "" {
			return "empty"
		}
		return strings.ReplaceAll(state, " ", "+")
	}
	for _, from := range fromStates {
		for _, to := range toStates {
			t.Run(label(from)+"/"+label(to), func(t *testing.T) {
				want := slices.Contains(allowed[from], to)
				if got := validUpdateJournalTransition(updateState(from), updateState(to)); got != want {
					t.Fatalf("transition %q -> %q = %t, want %t", from, to, got, want)
				}
			})
		}
	}
}

func TestUpdateJournalInvalidPredecessorCannotAuthorizeAppend(t *testing.T) {
	c, record := updateRecordFixture(t)
	if err := beginUpdateRecordFixture(t, c, record); err != nil {
		t.Fatal(err)
	}
	appendUpdateFixtureEntry(t, c, updateJournalEntry{Operation: "update", Step: "garbage", State: "started"})
	before := updateJournalSnapshot(t, c)
	if err := c.appendUpdateJournalEntry(record, updateJournalEntry{Operation: "update", Step: "prepared", State: "started"}); err != errUpdatePartialState {
		t.Fatalf("invalid predecessor append returned %v, want fixed partial-state refusal", err)
	}
	assertUpdateJournalSnapshot(t, c, before)
}
