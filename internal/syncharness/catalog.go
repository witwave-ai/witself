package syncharness

import (
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"strings"
)

// CatalogDisposition is the design's work allocation, not a passing test result.
type CatalogDisposition string

const (
	// ImplementCatalog allocates model or catalog implementation to LC-S5.
	ImplementCatalog CatalogDisposition = "I"
	// ScaffoldCatalog retains a required scenario without its runtime adapter.
	ScaffoldCatalog CatalogDisposition = "S"
	// MixedCatalog separates LC-S5 model work from later runtime proof.
	MixedCatalog CatalogDisposition = "I+S"
	// LaterCatalog assigns proof to the milestone retained in the entry's scope.
	LaterCatalog CatalogDisposition = "LATER"
)

// CatalogMetadata is deliberately value-free. Evidence describes current proof,
// whereas Disposition retains the scope assigned by the LC-S5 design inventory.
type CatalogMetadata struct {
	Name        string             `json:"name"`
	Disposition CatalogDisposition `json:"disposition"`
	Evidence    EvidenceStatus     `json:"evidence"`
}

// ScenarioCatalogEntry preserves one H01–H69 design obligation.
type ScenarioCatalogEntry struct {
	CatalogMetadata
	ID        string `json:"id"`
	Rules     string `json:"rules"`
	Stores    string `json:"stores"`
	Fault     string `json:"fault"`
	Assertion string `json:"assertion"`
	Scope     string `json:"scope"`
	Source    string `json:"source"`
}

// OperationFamily retains the adopted contract for one API family.
type OperationFamily struct {
	CatalogMetadata
	Title          string `json:"title"`
	Rules          string `json:"rules"`
	Scope          string `json:"scope"`
	Source         string `json:"source"`
	ActorScope     string `json:"actor_scope"`
	WriteAuthority string `json:"write_authority"`
	Accounting     string `json:"accounting"`
	CausalBasis    string `json:"causal_basis"`
	Movement       string `json:"movement"`
}

// OperationMatrixCell names one family and one required H01 dimension.
type OperationMatrixCell struct {
	CatalogMetadata
	Family             string `json:"family"`
	Dimension          string `json:"dimension"`
	Rules              string `json:"rules"`
	Assertion          string `json:"assertion"`
	RequiredCapability string `json:"required_capability"`
	Fixture            string `json:"fixture"`
}

// SourceInventoryEntry indexes a route, action, job or identified writer seam.
type SourceInventoryEntry struct {
	CatalogMetadata
	Kind   string `json:"kind"`
	Source string `json:"source"`
	Symbol string `json:"symbol"`
	Family string `json:"family"`
}

// WriterCatalogEntry reserves semantic and database-principal coverage review.
type WriterCatalogEntry struct {
	CatalogMetadata
	Source   string `json:"source"`
	Symbol   string `json:"symbol"`
	Role     string `json:"role"`
	Coverage string `json:"coverage"`
}

// TableCatalogEntry records a CREATE TABLE source without claiming capture.
type TableCatalogEntry struct {
	CatalogMetadata
	Source string `json:"source"`
}

// PolicyCatalogEntry retains one of the three intentional SC-8.2 policy modes.
type PolicyCatalogEntry struct {
	CatalogMetadata
	Rules               string `json:"rules"`
	Messaging           string `json:"messaging"`
	HostedEmail         string `json:"hosted_email"`
	ProviderCredentials string `json:"provider_credentials"`
	VaultCapacitySource string `json:"vault_capacity_source"`
	PaidPlanAuthority   string `json:"paid_plan_authority"`
}

// AccountingCatalogEntry retains unknown counts until reviewed per action. A
// nil count is not zero or evidence that a read has no side effects.
type AccountingCatalogEntry struct {
	CatalogMetadata
	Family         string `json:"family"`
	OriginalEvents *int   `json:"original_events"`
	HourCount      *int   `json:"hour_count"`
	DayCount       *int   `json:"day_count"`
	ReplayWork     *int   `json:"replay_work"`
	Reason         string `json:"reason"`
}

// MovementCatalogEntry reserves a policy for each SC-6.8 movement purpose.
type MovementCatalogEntry struct {
	CatalogMetadata
	Source              string `json:"source"`
	SnapshotProjection  string `json:"snapshot_projection"`
	ProvenMove          string `json:"proven_move"`
	BackupRestore       string `json:"backup_restore"`
	SourceFinalization  string `json:"source_finalization"`
	AccountPurge        string `json:"account_purge"`
	EmptyStreamRequired bool   `json:"empty_stream_required"`
}

type catalogManifest[T any] struct {
	Version int    `json:"version"`
	Kind    string `json:"kind"`
	Rules   string `json:"rules,omitempty"`
	Scope   string `json:"scope,omitempty"`
	Entries []T    `json:"entries"`
}

type operationManifest struct {
	Version  int                   `json:"version"`
	Kind     string                `json:"kind"`
	Families []OperationFamily     `json:"families"`
	Cells    []OperationMatrixCell `json:"cells"`
}

type writerManifest struct {
	Version int                  `json:"version"`
	Kind    string               `json:"kind"`
	Rules   string               `json:"rules"`
	Scope   string               `json:"scope"`
	Writers []WriterCatalogEntry `json:"writers"`
	Tables  []TableCatalogEntry  `json:"tables"`
}

// Catalog is an inventory and a future-adapter contract. Loading it cannot
// activate capture, delivery, a writer capability or any production behavior.
type Catalog struct {
	Scenarios  []ScenarioCatalogEntry
	Families   []OperationFamily
	Matrix     []OperationMatrixCell
	Sources    []SourceInventoryEntry
	Writers    []WriterCatalogEntry
	Tables     []TableCatalogEntry
	Policies   []PolicyCatalogEntry
	Accounting []AccountingCatalogEntry
	Movement   []MovementCatalogEntry
}

//go:embed testdata/scenarios.v1.json testdata/operations.v1.json testdata/source_inventory.v1.json testdata/writers.v1.json testdata/policies.v1.json testdata/accounting.v1.json testdata/movement.v1.json
var catalogFiles embed.FS

// EmbeddedCatalog loads the versioned value-free manifests shipped with tests.
func EmbeddedCatalog() (Catalog, error) { return LoadCatalog(catalogFiles) }

// LoadCatalog accepts only version 1 manifests with known fields. It does not
// trust a manifest to promote scaffold results into model or PostgreSQL proof.
func LoadCatalog(fsys fs.FS) (Catalog, error) {
	var c Catalog
	var scenarios catalogManifest[ScenarioCatalogEntry]
	var operations operationManifest
	var sources catalogManifest[SourceInventoryEntry]
	var writers writerManifest
	var policies catalogManifest[PolicyCatalogEntry]
	var accounting catalogManifest[AccountingCatalogEntry]
	var movement catalogManifest[MovementCatalogEntry]
	for _, target := range []struct {
		kind string
		data any
	}{
		{"scenarios", &scenarios}, {"operations", &operations},
		{"source_inventory", &sources}, {"writers", &writers},
		{"policies", &policies}, {"accounting", &accounting}, {"movement", &movement},
	} {
		data, err := fs.ReadFile(fsys, "testdata/"+target.kind+".v1.json")
		if err != nil {
			return c, fmt.Errorf("catalog %s unavailable", target.kind)
		}
		var header struct {
			Version int    `json:"version"`
			Kind    string `json:"kind"`
		}
		if json.Unmarshal(data, &header) != nil || header.Version != 1 || header.Kind != target.kind {
			return c, fmt.Errorf("catalog %s version or kind invalid", target.kind)
		}
		decoder := json.NewDecoder(strings.NewReader(string(data)))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(target.data); err != nil {
			return c, fmt.Errorf("catalog %s schema invalid", target.kind)
		}
		if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
			return c, fmt.Errorf("catalog %s trailing data", target.kind)
		}
	}
	c = Catalog{Scenarios: scenarios.Entries, Families: operations.Families, Matrix: operations.Cells,
		Sources: sources.Entries, Writers: writers.Writers, Tables: writers.Tables,
		Policies: policies.Entries, Accounting: accounting.Entries, Movement: movement.Entries}
	if err := c.Validate(); err != nil {
		return Catalog{}, err
	}
	return c, nil
}

func validateMetadata(m CatalogMetadata, seen map[string]bool) error {
	if m.Name == "" || seen[m.Name] {
		return errors.New("catalog name missing or duplicated")
	}
	seen[m.Name] = true
	switch m.Disposition {
	case ImplementCatalog, ScaffoldCatalog, MixedCatalog, LaterCatalog:
	default:
		return errors.New("catalog disposition missing or invalid")
	}
	if m.Evidence != ScaffoldOnly {
		return errors.New("catalog inventory cannot claim implementation proof")
	}
	return nil
}

// Validate enforces catalog structure, not the future domain assertions.
func (c Catalog) Validate() error {
	if len(c.Scenarios) != 69 || len(c.Families) != 12 || len(c.Matrix) != 72 || len(c.Policies) != 3 {
		return errors.New("catalog required coverage missing")
	}
	seen := make(map[string]bool)
	for i, entry := range c.Scenarios {
		if err := validateMetadata(entry.CatalogMetadata, seen); err != nil {
			return err
		}
		if entry.ID != fmt.Sprintf("H%02d", i+1) || entry.Rules == "" || entry.Stores == "" || entry.Fault == "" || entry.Assertion == "" || entry.Scope == "" || entry.Source == "" {
			return errors.New("scenario obligation incomplete or out of order")
		}
	}
	families := make(map[string]bool)
	for _, family := range c.Families {
		if err := validateMetadata(family.CatalogMetadata, families); err != nil {
			return err
		}
		if family.Rules == "" || family.ActorScope == "" || family.CausalBasis == "" || family.WriteAuthority == "" {
			return errors.New("operation family contract missing")
		}
	}
	dimensions := map[string]bool{"permission": true, "offline": true, "conflict": true, "deletion": true, "retention": true, "unsupported": true}
	seen = make(map[string]bool)
	for _, cell := range c.Matrix {
		if err := validateMetadata(cell.CatalogMetadata, seen); err != nil {
			return err
		}
		if !families[cell.Family] || !dimensions[cell.Dimension] || cell.Name != cell.Family+"/"+cell.Dimension || cell.Assertion == "" || cell.RequiredCapability == "" || cell.Fixture != "H01" {
			return errors.New("operation matrix cell invalid")
		}
	}
	for family := range families {
		for dimension := range dimensions {
			if !seen[family+"/"+dimension] {
				return errors.New("operation matrix coverage missing")
			}
		}
	}
	groups := [][]CatalogMetadata{{}, {}, {}, {}, {}, {}}
	for _, e := range c.Sources {
		if e.Source == "" || e.Symbol == "" || e.Kind == "" || !families[e.Family] {
			return errors.New("source inventory entry incomplete")
		}
		groups[0] = append(groups[0], e.CatalogMetadata)
	}
	for _, e := range c.Writers {
		if e.Role != "UNVERIFIED_DATABASE_PRINCIPAL" || e.Coverage == "" || e.Source == "" {
			return errors.New("writer coverage must remain explicit scaffold")
		}
		groups[1] = append(groups[1], e.CatalogMetadata)
	}
	for _, e := range c.Tables {
		if e.Source == "" {
			return errors.New("table source missing")
		}
		groups[2] = append(groups[2], e.CatalogMetadata)
	}
	for _, e := range c.Policies {
		if e.Rules != "SC-8.2" || e.Messaging == "" || e.HostedEmail == "" || e.ProviderCredentials == "" || e.VaultCapacitySource == "" || e.PaidPlanAuthority == "" {
			return errors.New("policy contract incomplete")
		}
		groups[3] = append(groups[3], e.CatalogMetadata)
	}
	for _, e := range c.Accounting {
		if !families[e.Family] || e.Reason == "" || e.OriginalEvents != nil || e.HourCount != nil || e.DayCount != nil || e.ReplayWork != nil {
			return errors.New("accounting placeholder must not invent a count")
		}
		groups[4] = append(groups[4], e.CatalogMetadata)
	}
	for _, e := range c.Movement {
		if e.Source == "" || !e.EmptyStreamRequired || e.SnapshotProjection != "UNREVIEWED" || e.ProvenMove != "UNREVIEWED" || e.BackupRestore != "UNREVIEWED" || e.SourceFinalization != "UNREVIEWED" || e.AccountPurge != "UNREVIEWED" {
			return errors.New("movement placeholder must name every unreviewed purpose")
		}
		groups[5] = append(groups[5], e.CatalogMetadata)
	}
	for _, group := range groups {
		if len(group) == 0 {
			return errors.New("catalog inventory missing")
		}
		seen = make(map[string]bool)
		for _, meta := range group {
			if err := validateMetadata(meta, seen); err != nil {
				return err
			}
		}
	}
	return nil
}

// MatrixResult reports capability availability, never a domain receipt.
type MatrixResult struct {
	Evidence EvidenceStatus
	Outcome  string
}

// EvaluateOperationMatrix only evaluates fixture capability availability. The
// unknown-family refusal is a catalog default, not a fabricated domain receipt.
func (c Catalog) EvaluateOperationMatrix(family, dimension string) MatrixResult {
	for _, cell := range c.Matrix {
		if cell.Family == family && cell.Dimension == dimension {
			return MatrixResult{Evidence: ScaffoldOnly, Outcome: "adapter_not_implemented"}
		}
	}
	return MatrixResult{Evidence: ModelPass, Outcome: "unsupported"}
}

// EvaluateOperation resolves an exact source-inventory entry before checking its
// family capability. Unknown entrypoints cannot inherit a family permission.
func (c Catalog) EvaluateOperation(name, dimension string) MatrixResult {
	for _, entry := range c.Sources {
		if entry.Name == name {
			return c.EvaluateOperationMatrix(entry.Family, dimension)
		}
	}
	return MatrixResult{Evidence: ModelPass, Outcome: "unsupported"}
}
