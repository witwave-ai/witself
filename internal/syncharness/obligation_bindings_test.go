package syncharness

import (
	"slices"
	"testing"
)

// Direct function bindings execute the exact scoped test; a string or source
// inventory match alone cannot satisfy an implemented manifest. Pure tests only.
func TestImplementedObligationBindings(t *testing.T) {
	bindings := map[string]func(*testing.T){
		"TestCoverageInventoryAndSchedule":                          TestCoverageInventoryAndSchedule,
		"TestMovementRefusesCrossAccountReferencesAndAuthorityCopy": TestMovementRefusesCrossAccountReferencesAndAuthorityCopy,
		"TestMovementRegistryCompleteness":                          TestMovementRegistryCompleteness,
		"TestOperationCatalogMatchesContractRows":                   TestOperationCatalogMatchesContractRows,
		"TestOperationMatrix":                                       TestOperationMatrix,
		"TestSourceInventoryRequiresReviewOnWriterOrActionChange":   TestSourceInventoryRequiresReviewOnWriterOrActionChange,
	}
	// Reviewed per-obligation bindings prevent a real but unrelated test from
	// being substituted for the required model invariant.
	expected := map[string][]string{
		"H01": {"TestOperationMatrix"},
		"H10": {"TestOperationCatalogMatchesContractRows"},
		"H33": {"TestSourceInventoryRequiresReviewOnWriterOrActionChange"},
		"H34": {"TestCoverageInventoryAndSchedule"},
		"H42": {"TestCoverageInventoryAndSchedule"},
		"H47": {"TestMovementRegistryCompleteness", "TestMovementRefusesCrossAccountReferencesAndAuthorityCopy"},
	}
	manifests, err := EmbeddedObligations()
	if err != nil {
		t.Fatal(err)
	}
	used := map[string]bool{}
	seenIDs := map[string]bool{}
	for _, manifest := range manifests {
		if manifest.Disposition != "I" {
			continue
		}
		var names []string
		for _, test := range manifest.Tests {
			if test.Package == "syncharness" {
				names = append(names, test.Name)
			}
		}
		if len(names) > 0 {
			if !slices.Equal(names, expected[manifest.ID]) {
				t.Fatalf("%s implementation binding differs from reviewed scoped evidence", manifest.ID)
			}
			seenIDs[manifest.ID] = true
		}
		for _, test := range manifest.Tests {
			if test.Package != "syncharness" {
				continue
			}
			run, ok := bindings[test.Name]
			if !ok {
				t.Fatalf("%s lacks compiled execution binding", manifest.ID)
			}
			used[test.Name] = true
			t.Run(manifest.ID+"/"+test.Name, run)
		}
	}
	if len(used) != len(bindings) || len(seenIDs) != len(expected) {
		t.Fatal("orphaned obligation test binding")
	}
}

func TestRuntimeResultAdapterBindings(t *testing.T) {
	// A symbol inventory is insufficient: this typed pointer must compile and
	// the exact scenario must execute before its artifact can be accepted.
	bindings := map[string]func(CoverageSchedule) (Trace, error){
		"RunCoverageScheduleModel": RunCoverageScheduleModel,
	}
	results, err := EmbeddedRuntimeResults()
	if err != nil {
		t.Fatal(err)
	}
	executed := 0
	for _, record := range results.Records {
		if record.Adapter.Package != "syncharness" {
			continue
		}
		run, ok := bindings[record.Adapter.Symbol]
		if !ok {
			t.Fatal("runtime result lacks a compiled adapter")
		}
		index := slices.IndexFunc(CoverageSchedules(), func(s CoverageSchedule) bool { return s.ID == record.Obligation })
		if index < 0 {
			t.Fatal("runtime result has no executable schedule")
		}
		t.Run(record.Obligation, func(t *testing.T) {
			trace, runErr := run(CoverageSchedules()[index])
			observed, _, _ := fixtureRuntimeRecord(record.Obligation)
			if runErr != nil || trace.Outcome != ModelPass {
				observed.Outcome = Fail
			}
			if observed != record || VerifyRuntimeArtifact(observed, len(trace.Steps)) != nil {
				t.Fatal("executed adapter differs from retained runtime result")
			}
		})
		executed++
	}
	if executed != 10 {
		t.Fatal("runtime schedule execution bindings incomplete")
	}
}
