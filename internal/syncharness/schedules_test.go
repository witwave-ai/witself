package syncharness

import (
	"slices"
	"testing"
)

func TestCoverageInventoryAndSchedule(t *testing.T) {
	schedules := CoverageSchedules()
	if len(schedules) != 10 {
		t.Fatal("missing SC-5 schedule")
	}
	catalog := mustCatalog(t)
	for i, s := range schedules {
		if s.ID != catalog.Scenarios[34+i].ID || s.Name != catalog.Scenarios[34+i].Name {
			t.Fatal("schedule differs from adopted obligation")
		}
		t.Run(s.ID, func(t *testing.T) {
			trace, err := RunCoverageScheduleModel(s)
			if err != nil || trace.Outcome != ModelPass || len(trace.Steps) != len(s.Barriers) {
				t.Fatal("model schedule did not execute every controlled barrier")
			}
			if !slices.Equal(trace.Scenario.Actors, []string{"U", "I", "V", "T", "A", "S"}) {
				t.Fatal("SC-5 session roles missing")
			}
			result := RunCoverageSchedule(s, nil, nil)
			if result.Status != NotRun || result.Passed() || len(result.Missing) != len(s.Required)+1 {
				t.Fatal("missing runtime capability reported proof")
			}
			t.Logf("MODEL_PASS barriers=%d; runtime=NOT_RUN missing=%v", len(trace.Steps), result.Missing)
		})
	}
}

func TestCoverageSchedulesDoNotTrustCapabilityClaims(t *testing.T) {
	for _, s := range CoverageSchedules() {
		result := RunCoverageSchedule(s, s.Required, nil)
		if result.Status != NotRun || !slices.Equal(result.Missing, []string{"runtime_schedule_adapter"}) {
			t.Fatal("capability label manufactured runtime evidence")
		}
	}
}

type coverageAdapter struct{ calls int }

func (a *coverageAdapter) RunCoverageSchedule(CoverageSchedule) error { a.calls++; return nil }

func TestCoverageScheduleCannotDropRequiredCapabilities(t *testing.T) {
	for _, mutation := range []string{"capability", "barrier", "name"} {
		s := CoverageSchedules()[0]
		switch mutation {
		case "capability":
			s.Required = nil
		case "barrier":
			s.Barriers = s.Barriers[1:]
		case "name":
			s.Name = "unregistered"
		}
		adapter := &coverageAdapter{}
		if result := RunCoverageSchedule(s, s.Required, adapter); result.Status != Fail || adapter.calls != 0 {
			t.Fatal("altered schedule manufactured runtime evidence")
		}
	}
}

func TestCoverageScheduleOrderingMutationFails(t *testing.T) {
	for _, index := range []int{0, 1, 2, 3, 4, 5, 6, 7, 8, 9} {
		s := CoverageSchedules()[index]
		// Remove the prerequisite state-changing barrier while preserving a valid
		// causal event queue. A list of names alone would incorrectly still pass.
		s.Barriers = s.Barriers[1:]
		for i := range s.Barriers {
			s.Barriers[i].After = nil
			if i > 0 {
				s.Barriers[i].After = []uint64{uint64(i)}
			}
		}
		trace, err := RunCoverageScheduleModel(s)
		if err == nil && trace.Outcome != Fail {
			t.Fatalf("%s missing causal state was accepted", s.ID)
		}
	}
}
