package syncharness

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"
)

func mustCatalog(t *testing.T) Catalog {
	t.Helper()
	catalog, err := EmbeddedCatalog()
	if err != nil {
		t.Fatal(err)
	}
	return catalog
}

func TestCatalogObligations(t *testing.T) {
	c := mustCatalog(t)
	expected := approvedCatalogObligations(t)
	if len(expected) != len(c.Scenarios) {
		t.Fatal("design obligation count changed")
	}
	for i, entry := range c.Scenarios {
		t.Run(expected[i].ID, func(t *testing.T) {
			if field := catalogObligationDifference(entry, expected[i]); field != "" {
				t.Errorf("%s differs from approved design fixture inventory", field)
			}
		})
	}
	t.Logf("catalog: H_ids=%d; scenario_evidence=SCAFFOLD_ONLY", len(c.Scenarios))
}

// This expectation is reviewed against the design's fixture inventory, not
// loaded or regenerated from the catalog being checked. The TSV removes only
// Markdown emphasis/code markup and separates stores, fault and disposition.
func approvedCatalogObligations(t *testing.T) []ScenarioCatalogEntry {
	t.Helper()
	data, err := os.ReadFile("testdata/approved_obligations.v1.tsv")
	if err != nil {
		t.Fatal("approved design fixture inventory unavailable")
	}
	reader := csv.NewReader(strings.NewReader(string(data)))
	reader.Comma = '\t'
	reader.Comment = '#'
	reader.FieldsPerRecord = 8
	rows, err := reader.ReadAll()
	if err != nil || len(rows) != 69 {
		t.Fatal("approved design fixture inventory invalid")
	}
	expected := make([]ScenarioCatalogEntry, len(rows))
	for i, row := range rows {
		if row[0] != fmt.Sprintf("H%02d", i+1) {
			t.Fatal("approved design fixture inventory is incomplete or out of order")
		}
		expected[i] = ScenarioCatalogEntry{
			CatalogMetadata: CatalogMetadata{Name: row[1], Disposition: CatalogDisposition(row[6]), Evidence: ScaffoldOnly},
			ID:              row[0], Rules: row[2], Stores: row[3], Fault: row[4], Assertion: row[5], Scope: row[7],
			Source: "docs/research/local-cell/sync-contract.md",
		}
	}
	return expected
}

func catalogObligationDifference(got, want ScenarioCatalogEntry) string {
	for _, field := range []struct{ name, got, want string }{
		{"id", got.ID, want.ID},
		{"name", got.Name, want.Name},
		{"rules", got.Rules, want.Rules},
		{"stores", got.Stores, want.Stores},
		{"fault", got.Fault, want.Fault},
		{"assertion", got.Assertion, want.Assertion},
		{"disposition", string(got.Disposition), string(want.Disposition)},
		{"scope", got.Scope, want.Scope},
		{"evidence", string(got.Evidence), string(want.Evidence)},
		{"source", got.Source, want.Source},
	} {
		if field.got != field.want {
			return field.name
		}
	}
	return ""
}

func TestCatalogObligationsRejectAlteredDesign(t *testing.T) {
	expected := approvedCatalogObligations(t)
	for _, test := range []struct {
		name   string
		index  int
		field  string
		mutate func(*ScenarioCatalogEntry)
	}{
		{"changed_name", 0, "name", func(e *ScenarioCatalogEntry) { e.Name = "AlteredOperationMatrix" }},
		{"changed_rule", 0, "rules", func(e *ScenarioCatalogEntry) { e.Rules = "SC-1.2" }},
		{"changed_stores", 1, "stores", func(e *ScenarioCatalogEntry) { e.Stores = "R" }},
		{"changed_fault", 1, "fault", func(e *ScenarioCatalogEntry) { e.Fault = "no stale replay" }},
		{"reversed_assertion", 1, "assertion", func(e *ScenarioCatalogEntry) {
			e.Assertion = "Resurrection is allowed; stable evidence IDs retain meaning across all writers"
		}},
		{"changed_disposition", 1, "disposition", func(e *ScenarioCatalogEntry) { e.Disposition = ImplementCatalog }},
		{"changed_scope", 11, "scope", func(e *ScenarioCatalogEntry) { e.Scope = "Later M4" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := mustCatalog(t)
			test.mutate(&c.Scenarios[test.index])
			if c.Validate() != nil {
				t.Fatal("negative case must retain valid catalog structure")
			}
			if field := catalogObligationDifference(c.Scenarios[test.index], expected[test.index]); field != test.field {
				t.Fatalf("design correspondence check returned %q; want %q", field, test.field)
			}
		})
	}
}

// These are successful capability-accounting assertions, not successful family
// synchronization cases. Every unimplemented cell is counted and named.
func TestOperationMatrix(t *testing.T) {
	c := mustCatalog(t)
	scaffoldOnly := 0
	for _, cell := range c.Matrix {
		t.Run(cell.Name, func(t *testing.T) {
			result := c.EvaluateOperationMatrix(cell.Family, cell.Dimension)
			if result.Evidence != ScaffoldOnly || result.Outcome != "adapter_not_implemented" {
				t.Fatal("missing adapter was promoted into proof")
			}
			scaffoldOnly++
			t.Log("SCAFFOLD_ONLY: " + cell.RequiredCapability)
		})
	}
	if scaffoldOnly != 72 {
		t.Fatalf("SCAFFOLD_ONLY cells=%d; want 72", scaffoldOnly)
	}
	for _, dimension := range []string{"permission", "offline", "conflict", "deletion", "retention", "unsupported"} {
		result := c.EvaluateOperation("unknown_operation", dimension)
		if result.Evidence != ModelPass || result.Outcome != "unsupported" {
			t.Fatal("unknown operation did not default unsupported")
		}
	}
	for _, operation := range c.Sources {
		result := c.EvaluateOperation(operation.Name, "permission")
		if result.Evidence != ScaffoldOnly {
			t.Fatal("inventoried source entry acquired an unimplemented family permission")
		}
	}
	t.Logf("H01: families=%d dimensions=6 matrix_cells=%d SCAFFOLD_ONLY=%d; M4 proofs=0", len(c.Families), len(c.Matrix), scaffoldOnly)
}

func TestCatalogRejectsMissingOrMisleadingCoverage(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Catalog)
	}{
		{"missing_obligation", func(c *Catalog) { c.Scenarios = c.Scenarios[:68] }},
		{"duplicate_h_id", func(c *Catalog) { c.Scenarios[1].ID = "H01" }},
		{"duplicate_fixture_name", func(c *Catalog) { c.Scenarios[1].Name = c.Scenarios[0].Name }},
		{"missing_disposition", func(c *Catalog) { c.Scenarios[0].Disposition = "" }},
		{"false_runtime_proof", func(c *Catalog) { c.Scenarios[0].Evidence = PostgresPass }},
		{"missing_matrix_cell", func(c *Catalog) { c.Matrix = c.Matrix[:71] }},
		{"duplicate_matrix_cell", func(c *Catalog) { c.Matrix[1] = c.Matrix[0] }},
		{"unknown_family", func(c *Catalog) { c.Matrix[0].Family = "unreviewed" }},
		{"unknown_dimension", func(c *Catalog) { c.Matrix[0].Dimension = "unreviewed" }},
		{"missing_capability", func(c *Catalog) { c.Matrix[0].RequiredCapability = "" }},
		{"invented_role_proof", func(c *Catalog) { c.Writers[0].Role = "VERIFIED" }},
		{"invented_zero_count", func(c *Catalog) { zero := 0; c.Accounting[0].OriginalEvents = &zero }},
		{"missing_movement_purpose", func(c *Catalog) { c.Movement[0].ProvenMove = "" }},
		{"empty_stream_omission", func(c *Catalog) { c.Movement[0].EmptyStreamRequired = false }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := mustCatalog(t)
			test.mutate(&c)
			if c.Validate() == nil {
				t.Fatal("invalid catalog accepted")
			}
		})
	}
}

func TestCatalogVersionAndSchemaFailClosed(t *testing.T) {
	for _, mutation := range []string{"unknown_version", "unknown_field", "trailing_object"} {
		t.Run(mutation, func(t *testing.T) {
			files := fstest.MapFS{}
			err := fs.WalkDir(catalogFiles, "testdata", func(path string, entry fs.DirEntry, err error) error {
				if err != nil || entry.IsDir() {
					return err
				}
				data, readErr := catalogFiles.ReadFile(path)
				files[path] = &fstest.MapFile{Data: data}
				return readErr
			})
			if err != nil {
				t.Fatal(err)
			}
			path := "testdata/scenarios.v1.json"
			data := string(files[path].Data)
			switch mutation {
			case "unknown_version":
				data = strings.Replace(data, `"version": 1`, `"version": 2`, 1)
			case "unknown_field":
				data = strings.Replace(data, `"version": 1`, `"unreviewed": true, "version": 1`, 1)
			case "trailing_object":
				data += "{}"
			}
			files[path].Data = []byte(data)
			if _, err := LoadCatalog(files); err == nil {
				t.Fatal("unsupported manifest accepted")
			}
		})
	}
}

func repositoryFile(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", filepath.FromSlash(name)))
	if err != nil {
		t.Fatalf("catalog source unavailable: %s", name)
	}
	return string(data)
}

func TestOperationCatalogMatchesContractRows(t *testing.T) {
	c := mustCatalog(t)
	contract := repositoryFile(t, "docs/research/local-cell/sync-contract.md")
	for i, family := range c.Families {
		if family.Rules != fmt.Sprintf("SC-1.%d", i+2) || !strings.Contains(contract, "| "+family.Rules+" "+family.Title+" |") {
			t.Fatalf("operation family %d does not match adopted contract", i+1)
		}
		for _, value := range []string{family.ActorScope, family.WriteAuthority, family.Accounting, family.CausalBasis} {
			if !strings.Contains(contract, "| "+value+" |") {
				t.Fatalf("operation family %d contract changed; review the catalog", i+1)
			}
		}
	}
	for _, policy := range c.Policies {
		row := "| " + strings.Join([]string{policy.Name, policy.Messaging, policy.HostedEmail, policy.ProviderCredentials, policy.VaultCapacitySource, policy.PaidPlanAuthority}, " | ") + " |"
		if !strings.Contains(contract, row) {
			t.Fatal("SC-8.2 policy row differs from adopted contract")
		}
	}
	names := []string{c.Policies[0].Name, c.Policies[1].Name, c.Policies[2].Name}
	if !slices.Equal(names, []string{"Local standalone", "Self-managed destination", "SaaS destination"}) {
		t.Fatal("SC-8.2 mode inventory changed")
	}
}

func TestSourceInventoryRequiresReviewOnRouteOrTableChange(t *testing.T) {
	c := mustCatalog(t)
	routes := map[string]bool{}
	for _, entry := range c.Sources {
		if !strings.Contains(repositoryFile(t, entry.Source), entry.Symbol) {
			t.Errorf("source inventory symbol missing: %s", entry.Name)
		}
		if entry.Kind == "route" {
			routes[entry.Source+":"+entry.Symbol] = true
		}
	}
	pattern := regexp.MustCompile(`\b(?:mux|m)\.Handle(?:Func)?\(\s*"([^"\n]+)"`)
	paths, err := filepath.Glob(filepath.Join("..", "server", "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]bool{}
	for _, path := range paths {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		source := "internal/server/" + filepath.Base(path)
		for _, match := range pattern.FindAllStringSubmatch(repositoryFile(t, source), -1) {
			found[source+":"+match[1]] = true
		}
	}
	assertSameInventory(t, "literal route", routes, found)
	tables := map[string]bool{}
	for _, entry := range c.Tables {
		tables[entry.Name] = true
		if !strings.Contains(repositoryFile(t, entry.Source), entry.Name) {
			t.Errorf("table source missing: %s", entry.Name)
		}
	}
	paths, err = filepath.Glob(filepath.Join("..", "store", "migrations", "*.sql"))
	if err != nil {
		t.Fatal(err)
	}
	pattern = regexp.MustCompile(`(?i)\bCREATE\s+TABLE\s+(?:IF\s+NOT\s+EXISTS\s+)?([a-z_][a-z0-9_]*)`)
	found = map[string]bool{}
	for _, path := range paths {
		for _, match := range pattern.FindAllStringSubmatch(repositoryFile(t, "internal/store/migrations/"+filepath.Base(path)), -1) {
			found[match[1]] = true
		}
	}
	assertSameInventory(t, "CREATE TABLE", tables, found)
	t.Logf("source inventory: entries=%d literal_routes=%d tables=%d; semantic coverage=SCAFFOLD_ONLY", len(c.Sources), len(routes), len(tables))
}

func TestSourceInventoryRequiresReviewOnWriterOrActionChange(t *testing.T) {
	c := mustCatalog(t)
	jobs := map[string]bool{}
	actions := map[string]bool{}
	for _, entry := range c.Sources {
		switch entry.Kind {
		case "worker":
			jobs[entry.Name] = true
		case "action":
			actions[entry.Source+":"+entry.Symbol] = true
		}
	}
	found := map[string]bool{}
	for _, match := range regexp.MustCompile(`(?m)^\s*\w+Job\s*=\s*"([^"]+)"`).FindAllStringSubmatch(repositoryFile(t, "cmd/witself-worker/main.go"), -1) {
		found["worker:"+match[1]] = true
	}
	assertSameInventory(t, "worker declaration", jobs, found)
	paths, err := filepath.Glob(filepath.Join("..", "server", "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	found = map[string]bool{}
	for _, path := range paths {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		source := "internal/server/" + filepath.Base(path)
		content := repositoryFile(t, source)
		// Limit this index to modules dispatching the router's action wildcard;
		// metric-label switches are not additional API entrypoints.
		if !strings.Contains(content, `PathValue("action")`) {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), source, content, 0)
		if err != nil {
			t.Fatal("cannot inspect action selectors")
		}
		add := func(expr ast.Expr) {
			literal, ok := expr.(*ast.BasicLit)
			if !ok || literal.Kind != token.STRING {
				return
			}
			value, err := strconv.Unquote(literal.Value)
			if err == nil && value != "" {
				found[source+":"+value] = true
			}
		}
		ast.Inspect(file, func(node ast.Node) bool {
			switch node := node.(type) {
			case *ast.BinaryExpr:
				if (node.Op == token.EQL || node.Op == token.NEQ) && actionSelector(node.X) {
					add(node.Y)
				}
			case *ast.SwitchStmt:
				if actionSelector(node.Tag) {
					for _, statement := range node.Body.List {
						clause, ok := statement.(*ast.CaseClause)
						if !ok {
							continue
						}
						for _, expr := range clause.List {
							add(expr)
						}
					}
				}
			}
			return true
		})
	}
	assertSameInventory(t, "dynamic action selector", actions, found)
}

func actionSelector(expr ast.Expr) bool {
	identifier, ok := expr.(*ast.Ident)
	return ok && (identifier.Name == "operation" || identifier.Name == "action")
}

func assertSameInventory(t *testing.T, kind string, expected, actual map[string]bool) {
	t.Helper()
	for name := range expected {
		if !actual[name] {
			t.Errorf("catalog %s no longer exists: %s", kind, name)
		}
	}
	for name := range actual {
		if !expected[name] {
			t.Errorf("uncataloged %s requires review: %s", kind, name)
		}
	}
}

func TestAccountingAndMovementPlaceholdersAreComplete(t *testing.T) {
	c := mustCatalog(t)
	sources := map[string]bool{}
	counts := map[string]bool{}
	for _, source := range c.Sources {
		sources[source.Name] = true
	}
	for _, accounting := range c.Accounting {
		counts[accounting.Name] = true
	}
	assertSameInventory(t, "accounting vector", sources, counts)
	archive := repositoryFile(t, "internal/store/archive_tables.go")
	streams := map[string]bool{}
	for _, match := range regexp.MustCompile(`\{name: "([^"]+)"`).FindAllStringSubmatch(archive, -1) {
		streams[match[1]] = true
	}
	movement := map[string]bool{}
	for _, entry := range c.Movement {
		if !strings.HasPrefix(entry.Name, "planned:") {
			movement[entry.Name] = true
		}
	}
	assertSameInventory(t, "movement stream", streams, movement)
	t.Logf("placeholders: accounting_vectors=%d movement_streams=%d writers=%d; all SCAFFOLD_ONLY", len(c.Accounting), len(c.Movement), len(c.Writers))
}

func TestEveryManifestEntryHasDisposition(t *testing.T) {
	err := fs.WalkDir(catalogFiles, "testdata", func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		data, err := catalogFiles.ReadFile(path)
		if err != nil {
			return err
		}
		var manifest map[string]json.RawMessage
		if err := json.Unmarshal(data, &manifest); err != nil {
			return err
		}
		for _, key := range []string{"entries", "families", "cells", "writers", "tables"} {
			if raw, exists := manifest[key]; exists {
				var entries []CatalogMetadata
				if err := json.Unmarshal(raw, &entries); err != nil {
					return err
				}
				seen := map[string]bool{}
				for _, entry := range entries {
					if err := validateMetadata(entry, seen); err != nil {
						return err
					}
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
