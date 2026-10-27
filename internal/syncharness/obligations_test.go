package syncharness

import (
	"io/fs"
	"testing"
)

func TestObligationManifestCompleteness(t *testing.T) {
	manifests, err := EmbeddedObligations()
	if err != nil {
		t.Fatal(err)
	}
	files, err := fs.Glob(obligationFiles, "testdata/obligations/*.v1.json")
	if err != nil || len(files) != 69 || len(manifests) != 69 {
		t.Fatal("obligation manifest coverage incomplete")
	}
	counts := map[string]int{}
	for _, manifest := range manifests {
		counts[manifest.Disposition]++
		if manifest.RuntimeStatus != NotRun || manifest.Remaining != ScaffoldOnly {
			t.Fatal("partial fixture promoted to runtime proof")
		}
	}
	if counts["I"]+counts["S"]+counts["SCAFFOLD_ONLY"]+counts["NOT_RUN"] != 69 {
		t.Fatal("unaccounted disposition")
	}
	t.Logf("scoped fixture dispositions: I=%d S=%d SCAFFOLD_ONLY=%d NOT_RUN=%d; runtime NOT_RUN=69, remaining SCAFFOLD_ONLY=69", counts["I"], counts["S"], counts["SCAFFOLD_ONLY"], counts["NOT_RUN"])
}

func TestObligationManifestRejectsMissingExecutionAndFalseProof(t *testing.T) {
	manifests, err := EmbeddedObligations()
	if err != nil {
		t.Fatal(err)
	}
	for _, mutation := range []string{"missing_test", "runtime_pass", "missing_capability", "missing_boundary", "unknown_disposition", "unknown_package"} {
		t.Run(mutation, func(t *testing.T) {
			m := manifests[0]
			m.Tests = append([]ObligationTest(nil), m.Tests...)
			switch mutation {
			case "missing_test":
				m.Tests = nil
			case "runtime_pass":
				m.RuntimeStatus = PostgresPass
			case "missing_capability":
				m.MissingCapabilities = nil
			case "missing_boundary":
				m.Boundary = ""
			case "unknown_disposition":
				m.Disposition = "PASS"
			case "unknown_package":
				m.Tests[0].Package = "production"
			}
			if m.Validate() == nil {
				t.Fatal("misleading manifest accepted")
			}
		})
	}
}
