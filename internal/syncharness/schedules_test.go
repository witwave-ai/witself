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

func (a *coverageAdapter) RunCoverageSchedule(CoverageSchedule) (RuntimeResultRecord, error) {
	a.calls++
	return RuntimeResultRecord{}, nil
}

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

func TestCoverageScheduleFixtureEvidenceCannotClaimPostgres(t *testing.T) {
	for _, s := range CoverageSchedules() {
		result := RunCoverageSchedule(s, s.Required, FixtureCoverageAdapter{})
		if result.Status != Pass || !result.Passed() || result.EvidenceClass != FixtureEvidence || result.Record == nil || result.Record.DatabaseClass != "none" || len(result.Missing) != len(s.Required) {
			t.Fatal("model-only schedule misclassified its evidence or discharged runtime gaps")
		}
	}
}

type fabricatedCoverageAdapter struct{ mutation string }

func (a fabricatedCoverageAdapter) RunCoverageSchedule(s CoverageSchedule) (RuntimeResultRecord, error) {
	r, err := (FixtureCoverageAdapter{}).RunCoverageSchedule(s)
	switch a.mutation {
	case "class":
		r.EvidenceClass = RealPostgresEvidence
	case "artifact":
		r.Artifact = ""
	case "adapter":
		r.Adapter.Symbol = "NonexistentAdapter"
	case "obligation":
		r.Obligation = "H01"
	case "failure":
		r.Outcome = Fail
	}
	return r, err
}

func TestCoverageScheduleRejectsFabricatedAdapterResults(t *testing.T) {
	for _, mutation := range []string{"class", "artifact", "adapter", "obligation", "failure"} {
		s := CoverageSchedules()[0]
		if result := RunCoverageSchedule(s, s.Required, fabricatedCoverageAdapter{mutation}); result.Status != Fail || result.Passed() {
			t.Fatal("fabricated schedule evidence accepted")
		}
	}
}

type copiedPassAdapter struct{ calls int }

func (a *copiedPassAdapter) RunCoverageSchedule(s CoverageSchedule) (RuntimeResultRecord, error) {
	a.calls++
	// Deliberately execute no model barrier; a retained artifact is not a run.
	results, err := EmbeddedRuntimeResults()
	if err != nil {
		return RuntimeResultRecord{}, err
	}
	for _, r := range results.Records {
		if r.Obligation == s.ID && r.Outcome == Pass {
			return r, nil
		}
	}
	return RuntimeResultRecord{}, ErrRuntimeResult
}

func TestCoverageScheduleRejectsCopiedPassWithoutExecution(t *testing.T) {
	s := CoverageSchedules()[0]
	adapter := &copiedPassAdapter{}
	if result := RunCoverageSchedule(s, s.Required, adapter); result.Passed() || result.Status != Fail || adapter.calls != 0 {
		t.Fatal("unregistered adapter manufactured evidence from a copied retained pass")
	}
	var nilAdapter *FixtureCoverageAdapter
	if result := RunCoverageSchedule(s, s.Required, nilAdapter); result.Passed() || result.Status != Fail {
		t.Fatal("nil compiled adapter accepted")
	}
}

func TestFixtureCoverageAdapterRejectsAlteredSchedule(t *testing.T) {
	s := CoverageSchedules()[0]
	// Same ID and barrier count, and still a successful model execution, but
	// guard installation never occurs. It cannot earn the retained scenario.
	s.Barriers[len(s.Barriers)-1].Action = "writer_hold"
	if trace, err := RunCoverageScheduleModel(s); err != nil || trace.Outcome != ModelPass {
		t.Fatal("counterexample no longer exercises a successful altered model")
	}
	if _, err := (FixtureCoverageAdapter{}).RunCoverageSchedule(s); err == nil {
		t.Fatal("altered scenario earned the original artifact")
	}
}
