package store

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/witwave-ai/witself/internal/syncharness"
)

func TestSyncHarnessLocalFixtureDispositions(t *testing.T) {
	data, err := os.ReadFile("../syncharness/testdata/local_publication.v1.json")
	if err != nil {
		t.Fatal("read local fixture dispositions failed")
	}
	var manifest struct {
		Version int `json:"version"`
		Entries []struct {
			ID          string `json:"id"`
			Name        string `json:"name"`
			Disposition string `json:"disposition"`
			Evidence    string `json:"evidence"`
			Remaining   string `json:"remaining"`
			Boundary    string `json:"boundary"`
		} `json:"entries"`
	}
	if err := json.Unmarshal(data, &manifest); err != nil || manifest.Version != 1 {
		t.Fatal("invalid local fixture dispositions")
	}
	required := map[string]string{"H14": "CompoundNotificationBeforeLink", "H24": "OperationStateCrashMatrix", "H27": "ReceiptCommitSignatureRetry", "H28": "PayloadRetirementCommitmentFloor", "H41": "OmittedSemanticJournal", "H42": "CommittedPrefixCounter", "H56": "PublicationOutboxWitnessBoundary", "H64": "LogicalAccountingMatrix"}
	if len(manifest.Entries) != len(required) {
		t.Fatal("local fixture disposition count differs")
	}
	seen := map[string]bool{}
	for _, e := range manifest.Entries {
		if seen[e.ID] || required[e.ID] != e.Name || e.Disposition != "I local" || e.Evidence != "POSTGRES_PASS" || e.Remaining != "SCAFFOLD_ONLY" || e.Boundary == "" {
			t.Fatal("local evidence promoted or omitted runtime obligation")
		}
		seen[e.ID] = true
	}
	catalog, err := syncharness.EmbeddedCatalog()
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, e := range catalog.Scenarios {
		if e.Evidence == syncharness.ScaffoldOnly {
			count++
		}
	}
	if count != 69 {
		t.Fatal("local fixture proof promoted a full contract obligation")
	}
	t.Logf("local miniature fixtures: I local=%d; full contract obligations SCAFFOLD_ONLY=%d", len(seen), count)
}
