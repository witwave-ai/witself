package syncharness

import (
	"bytes"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
)

// ObligationTest names a test function that the package's binding suite directly
// invokes. The name is inventory; compiled function pointers supply execution.
type ObligationTest struct {
	Package string `json:"package"`
	Name    string `json:"name"`
}

// ObligationManifest separates implemented, scoped miniature evidence from the
// full contract obligation. Runtime remains NOT_RUN for absent M4 adapters.
type ObligationManifest struct {
	Version             int                `json:"version"`
	ID                  string             `json:"id"`
	Name                string             `json:"name"`
	DesignAllocation    CatalogDisposition `json:"design_allocation"`
	Disposition         string             `json:"disposition"`
	ImplementedScope    string             `json:"implemented_scope"`
	Tests               []ObligationTest   `json:"tests"`
	RuntimeStatus       EvidenceStatus     `json:"runtime_status"`
	Remaining           EvidenceStatus     `json:"remaining"`
	MissingCapabilities []string           `json:"missing_capabilities"`
	Boundary            string             `json:"boundary"`
	ExternalSignalLimit string             `json:"external_signal_limit,omitempty"`
}

//go:embed testdata/obligations/*.v1.json
var obligationFiles embed.FS

// ErrObligationManifest refuses incomplete evidence or a false runtime claim.
var ErrObligationManifest = errors.New("sync harness: incomplete or misleading obligation manifest")

// Validate never permits an inventory declaration to certify runtime proof.
func (m ObligationManifest) Validate() error {
	if m.Version != 1 || !validSymbol(m.Name) || len(m.ID) != 3 || m.ID[0] != 'H' || m.ImplementedScope == "" || m.Boundary == "" || m.RuntimeStatus != NotRun || m.Remaining != ScaffoldOnly || len(m.MissingCapabilities) == 0 {
		return ErrObligationManifest
	}
	if !slices.Contains([]string{"I", "S", "SCAFFOLD_ONLY", "NOT_RUN"}, m.Disposition) {
		return ErrObligationManifest
	}
	if !slices.Contains([]CatalogDisposition{ImplementCatalog, ScaffoldCatalog, MixedCatalog, LaterCatalog}, m.DesignAllocation) {
		return ErrObligationManifest
	}
	if m.Disposition == "I" && len(m.Tests) == 0 {
		return ErrObligationManifest
	}
	seen := map[ObligationTest]bool{}
	for _, test := range m.Tests {
		if !slices.Contains([]string{"syncmodel", "syncharness"}, test.Package) || !validSymbol(test.Name) || len(test.Name) < 5 || test.Name[:4] != "Test" || seen[test] {
			return ErrObligationManifest
		}
		seen[test] = true
	}
	seenCapabilities := map[string]bool{}
	for _, capability := range m.MissingCapabilities {
		if !validSymbol(capability) || seenCapabilities[capability] {
			return ErrObligationManifest
		}
		seenCapabilities[capability] = true
	}
	if (m.ID == "H59" || m.ID == "H63") && m.ExternalSignalLimit == "" {
		return ErrObligationManifest
	}
	return nil
}

// EmbeddedObligations loads the reviewed allocation for every H ID.
// Runtime results never alter runtime_status or remaining: promoting either
// needs a separate reviewed implementation.
func EmbeddedObligations() ([]ObligationManifest, error) {
	catalog, err := EmbeddedCatalog()
	if err != nil {
		return nil, err
	}
	manifests := make([]ObligationManifest, 0, 69)
	for i, entry := range catalog.Scenarios {
		path := fmt.Sprintf("testdata/obligations/H%02d.v1.json", i+1)
		data, err := obligationFiles.ReadFile(path)
		if err != nil {
			return nil, ErrObligationManifest
		}
		decoder := json.NewDecoder(bytes.NewReader(data))
		decoder.DisallowUnknownFields()
		var manifest ObligationManifest
		if decoder.Decode(&manifest) != nil || !errors.Is(decoder.Decode(new(any)), io.EOF) || manifest.Validate() != nil || manifest.ID != entry.ID || manifest.Name != entry.Name || manifest.DesignAllocation != entry.Disposition {
			return nil, ErrObligationManifest
		}
		manifests = append(manifests, manifest)
	}
	return manifests, nil
}
