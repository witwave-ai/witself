package localcell

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

const maintenanceAdmissionStatement = `if err := c.maintenanceFenceAllows(0); err != nil { return err }`

func admissionNodeText(node ast.Node) string {
	var buffer bytes.Buffer
	if err := format.Node(&buffer, token.NewFileSet(), node); err != nil {
		return "<invalid>"
	}
	return buffer.String()
}

func admissionStatements(source string) []string {
	file, err := parser.ParseFile(token.NewFileSet(), "fixture.go", "package localcell; func fixture() {"+source+"}", 0)
	if err != nil {
		return nil
	}
	body := file.Decls[0].(*ast.FuncDecl).Body.List
	result := make([]string, len(body))
	for index, statement := range body {
		result[index] = admissionNodeText(statement)
	}
	return result
}

// These independent pre-admission sequences preserve the legacy refusal order.
// The final statement is the first guarded effect, never part of the allowance.
var maintenanceAdmissionBoundaries = map[string]string{
	"Install": `lock, err := c.own(true)
if err != nil { return err }
defer func() { _ = lock.Close() }()
if _, err := readManifest(c.cfg.Root); !errors.Is(err, os.ErrNotExist) {
 if err != nil { return err }
 return errors.New("local cell is already installed")
}
if entries, err := os.ReadDir(c.path("pg")); err == nil && len(entries) > 0 {
 return errors.New("refusing existing PostgreSQL data; repair required")
} else if err != nil && !errors.Is(err, os.ErrNotExist) {
 return errors.New("cannot inspect PostgreSQL data")
}
` + maintenanceAdmissionStatement + `
if err := c.layout(); err != nil { return err }`,
	"Start": `lock, err := c.own(false)
if err != nil { return err }
defer func() { _ = lock.Close() }()
m, err := readManifest(c.cfg.Root)
if err != nil { return err }
if !m.Installed { return errors.New("local cell installation is incomplete") }
if err := c.serviceStartFence(); err != nil { return err }
` + maintenanceAdmissionStatement + `
m.MigrationWait, m.JobRestartPolicy = c.runtimeSettings(m)`,
	"Repair": `lock, err := c.ownExisting()
if err != nil { return err }
defer func() { _ = lock.Close() }()
m, manifestErr := readManifest(c.cfg.Root)
if manifestErr != nil && !errors.Is(manifestErr, os.ErrNotExist) { return manifestErr }
if m != nil && m.PrincipalLayoutVersion != 0 {
 selected, selectedCancel := context.WithTimeout(parent, c.readinessTimeout("server", m))
 defer selectedCancel()
 ctx = selected
}
` + maintenanceAdmissionStatement + `
progress, err := c.readJournal()`,
	"Remove": `if err := c.exitServiceHostForRemoval(ctx); err != nil { return err }
lock, err := c.ownExisting()
if err != nil { return err }
defer func() { _ = lock.Close() }()
` + maintenanceAdmissionStatement + `
if err := c.setServiceDesiredStateLocked("stopped"); err != nil && !errors.Is(err, os.ErrNotExist) { return err }`,
}

func maintenanceAdmissionContract(source string) []string {
	source = strings.ReplaceAll(source, "\r\n", "\n")
	file, err := parser.ParseFile(token.NewFileSet(), "lifecycle.go", source, 0)
	if err != nil {
		return []string{"parse"}
	}
	var failures []string
	owners := map[string]int{}
	totalReferences := 0
	ast.Inspect(file, func(node ast.Node) bool {
		if selector, ok := node.(*ast.SelectorExpr); ok && selector.Sel.Name == "maintenanceFenceAllows" {
			totalReferences++
		}
		return true
	})
	if totalReferences != 4 {
		failures = append(failures, "global_count")
	}
	for _, declaration := range file.Decls {
		fn, ok := declaration.(*ast.FuncDecl)
		if !ok {
			continue
		}
		boundary, expected := maintenanceAdmissionBoundaries[fn.Name.Name]
		if !expected {
			continue
		}
		owners[fn.Name.Name]++
		fail := func(invariant string) { failures = append(failures, fn.Name.Name+"_"+invariant) }
		admissions := updateOrdinaryAdmissions("lifecycle.go", fn)
		if len(admissions) != 1 {
			fail("literal_zero_direct_error_return")
		}
		if fn.Body == nil {
			fail("position")
			continue
		}
		actual := make([]string, len(fn.Body.List))
		for index, statement := range fn.Body.List {
			actual[index] = admissionNodeText(statement)
		}
		want := admissionStatements(boundary)
		matches := 0
		for index := 0; index+len(want) <= len(actual); index++ {
			if strings.Join(actual[index:index+len(want)], "\n") == strings.Join(want, "\n") {
				matches++
			}
		}
		if len(want) == 0 || matches != 1 {
			fail("position")
		}
		ownCalls, lockReferences, closes := 0, 0, 0
		var lockIdentifier *ast.Ident
		for _, statement := range fn.Body.List {
			assignment, ok := statement.(*ast.AssignStmt)
			if !ok || assignment.Tok != token.DEFINE {
				continue
			}
			for _, left := range assignment.Lhs {
				if identifier, ok := left.(*ast.Ident); ok && identifier.Name == "lock" {
					lockIdentifier = identifier
				}
			}
		}
		ast.Inspect(fn.Body, func(node ast.Node) bool {
			switch node := node.(type) {
			case *ast.Ident:
				if lockIdentifier != nil && node.Obj == lockIdentifier.Obj {
					lockReferences++
				}
			case *ast.SelectorExpr:
				if node.Sel.Name == "own" || node.Sel.Name == "ownExisting" {
					ownCalls++
				}
				if identifier, ok := node.X.(*ast.Ident); ok && lockIdentifier != nil && identifier.Obj == lockIdentifier.Obj && node.Sel.Name == "Close" {
					closes++
				}
			}
			return true
		})
		if ownCalls != 1 || lockIdentifier == nil || lockReferences != 2 || closes != 1 {
			fail("continuous_hold")
		}
	}
	for name := range maintenanceAdmissionBoundaries {
		if owners[name] != 1 {
			failures = append(failures, name+"_owner_count")
		}
	}
	return failures
}

func admissionReadLifecycle(t *testing.T) string {
	t.Helper()
	source, err := os.ReadFile("lifecycle.go")
	if err != nil {
		t.Fatal("cannot read lifecycle source")
	}
	return strings.ReplaceAll(string(source), "\r\n", "\n")
}

func TestMaintenanceFenceAdmissionSourceContract(t *testing.T) {
	source := admissionReadLifecycle(t)
	for _, ending := range []string{"LF", "CRLF"} {
		t.Run(ending, func(t *testing.T) {
			input := source
			if ending == "CRLF" {
				input = strings.ReplaceAll(input, "\n", "\r\n")
			}
			if failures := maintenanceAdmissionContract(input); len(failures) != 0 {
				t.Fatalf("admission invariants: %v", failures)
			}
		})
	}
}

func admissionMutateFunction(t *testing.T, source, name string, mutate func(string) string) string {
	t.Helper()
	positions := token.NewFileSet()
	file, err := parser.ParseFile(positions, "lifecycle.go", source, 0)
	if err != nil {
		t.Fatal("control source does not parse")
	}
	for _, declaration := range file.Decls {
		fn, ok := declaration.(*ast.FuncDecl)
		if !ok || fn.Name.Name != name {
			continue
		}
		start, end := positions.Position(fn.Pos()).Offset, positions.Position(fn.End()).Offset
		before := source[start:end]
		after := mutate(before)
		if after == before {
			t.Fatal("mutation did not change its named site")
		}
		return source[:start] + after + source[end:]
	}
	t.Fatal("mutation owner missing")
	return ""
}

func TestMaintenanceFenceAdmissionSourceContractMutations(t *testing.T) {
	source := admissionReadLifecycle(t)
	if failures := maintenanceAdmissionContract(source); len(failures) != 0 {
		t.Fatalf("unchanged control: %v", failures)
	}
	gate := admissionIndent(admissionStatements(maintenanceAdmissionStatement)[0])
	closeLock := admissionIndent(admissionStatements(`defer func() { _ = lock.Close() }()`)[0])
	type mutation struct {
		name, invariant string
		change          func(string) string
	}
	for _, owner := range []string{"Install", "Start", "Repair", "Remove"} {
		t.Run(owner, func(t *testing.T) {
			mutations := []mutation{
				{"Delete", "literal_zero_direct_error_return", func(body string) string { return strings.Replace(body, gate, "", 1) }},
				{"PositiveGeneration", "literal_zero_direct_error_return", func(body string) string {
					return strings.Replace(body, "maintenanceFenceAllows(0)", "maintenanceFenceAllows(7)", 1)
				}},
				{"VariableGeneration", "literal_zero_direct_error_return", func(body string) string {
					return strings.Replace(body, "maintenanceFenceAllows(0)", "maintenanceFenceAllows(generation)", 1)
				}},
				{"IgnoreError", "literal_zero_direct_error_return", func(body string) string { return strings.Replace(body, gate, "_ = c.maintenanceFenceAllows(0)", 1) }},
				{"DeferredClosure", "literal_zero_direct_error_return", func(body string) string {
					return strings.Replace(body, gate, "defer func() { _ = c.maintenanceFenceAllows(0) }()", 1)
				}},
				{"GoroutineClosure", "literal_zero_direct_error_return", func(body string) string {
					return strings.Replace(body, gate, "go func() { _ = c.maintenanceFenceAllows(0) }()", 1)
				}},
				{"BeforeOwn", "position", func(body string) string {
					body = strings.Replace(body, gate, "", 1)
					return strings.Replace(body, "lock, err :=", gate+"\nlock, err :=", 1)
				}},
				{"CloseAndReacquire", "continuous_hold", func(body string) string {
					return strings.Replace(body, gate, gate+"\n_ = lock.Close()\nlock, err = c.ownExisting()\nif err != nil { return err }", 1)
				}},
				{"OmitDeferredClose", "continuous_hold", func(body string) string { return strings.Replace(body, closeLock, "", 1) }},
				{"NestedOwn", "continuous_hold", func(body string) string {
					return strings.Replace(body, gate, gate+"\nfunc() { _, _ = c.ownExisting() }()", 1)
				}},
				{"WrongReceiver", "literal_zero_direct_error_return", func(body string) string { return strings.Replace(body, "(c *Cell)", "(c *Other)", 1) }},
				{"DuplicateGate", "literal_zero_direct_error_return", func(body string) string { return strings.Replace(body, gate, gate+"\n"+gate, 1) }},
			}
			if owner != "Remove" {
				mutations = append(mutations, mutation{"BeforeManifest", "position", func(body string) string {
					body = strings.Replace(body, gate, "", 1)
					anchor := "m, err := readManifest"
					if owner == "Install" {
						anchor = "if _, err := readManifest"
					}
					if owner == "Repair" {
						anchor = "m, manifestErr := readManifest"
					}
					return strings.Replace(body, anchor, gate+"\n"+anchor, 1)
				}})
			}
			if owner == "Start" {
				mutations = append(mutations, mutation{"BeforeServiceFence", "position", func(body string) string {
					body = strings.Replace(body, gate, "", 1)
					return strings.Replace(body, "if err := c.serviceStartFence()", gate+"\nif err := c.serviceStartFence()", 1)
				}})
			}
			if owner == "Remove" {
				predrain := admissionIndent(admissionStatements(`if err := c.exitServiceHostForRemoval(ctx); err != nil { return err }`)[0])
				mutations = append(mutations,
					mutation{"DeletePredrain", "position", func(body string) string { return strings.Replace(body, predrain, "", 1) }},
					mutation{"BeforePredrain", "position", func(body string) string {
						body = strings.Replace(body, gate, "", 1)
						return strings.Replace(body, predrain, gate+"\n"+predrain, 1)
					}},
				)
			}
			mutations = append(mutations, mutation{"AfterFirstEffect", "position", func(body string) string {
				last := admissionStatements(maintenanceAdmissionBoundaries[owner])
				effect := admissionIndent(last[len(last)-1])
				body = strings.Replace(body, gate, "", 1)
				return strings.Replace(body, effect, effect+"\n"+gate, 1)
			}})
			for _, mutant := range mutations {
				t.Run(mutant.name, func(t *testing.T) {
					// Formatting first gives stable mutation targets on LF and CRLF checkouts.
					file, err := parser.ParseFile(token.NewFileSet(), "lifecycle.go", source, 0)
					if err != nil {
						t.Fatal("control parse failed")
					}
					input := admissionMutateFunction(t, admissionNodeText(file), owner, mutant.change)
					for _, ending := range []string{"LF", "CRLF"} {
						t.Run(ending, func(t *testing.T) {
							if ending == "CRLF" {
								input = strings.ReplaceAll(input, "\n", "\r\n")
							}
							failures := maintenanceAdmissionContract(input)
							if containsAdmissionFailure(failures, "parse") {
								t.Fatal("parse failure is not a mutation kill")
							}
							if !containsAdmissionFailure(failures, owner+"_"+mutant.invariant) {
								t.Fatalf("mutant survived named invariant %s: %v", mutant.invariant, failures)
							}
						})
					}
				})
			}
		})
	}
}

func containsAdmissionFailure(failures []string, wanted string) bool {
	for _, failure := range failures {
		if failure == wanted {
			return true
		}
	}
	return false
}

func TestMaintenanceFenceAdmissionGuardFixtures(t *testing.T) {
	const foundation = `package localcell
func (c *Cell) maintenanceFenceAllows(generation int) error { return nil }
func (c *Cell) readMaintenanceFence() error { return nil }
func (c *Cell) updateStep() error { return nil }
func (c *Cell) readUpdateJournal() error { return nil }
`
	type fixture struct {
		name, file, source, extra string
		rejected                  bool
	}
	var fixtures []fixture
	for _, owner := range []string{"Install", "Start", "Repair", "Remove"} {
		approved := "func (c *Cell) " + owner + "() error { " + maintenanceAdmissionStatement + "; return nil }"
		fixtures = append(fixtures, fixture{owner + "_Approved", "lifecycle.go", approved, "", false})
		for _, item := range []struct{ name, replacement string }{
			{"MethodValue", "gate := c.maintenanceFenceAllows; _ = gate"},
			{"AliasCall", "gate := c.maintenanceFenceAllows; _ = gate(0)"},
			{"Closure", "func() { _ = c.maintenanceFenceAllows(0) }()"},
			{"StoredClosure", "gate := func() { _ = c.maintenanceFenceAllows(0) }; _ = gate"},
			{"Deferred", "defer func() { _ = c.maintenanceFenceAllows(0) }()"},
			{"Goroutine", "go func() { _ = c.maintenanceFenceAllows(0) }()"},
			{"Nonzero", strings.ReplaceAll(maintenanceAdmissionStatement, "Allows(0)", "Allows(7)")},
			{"Variable", strings.ReplaceAll(maintenanceAdmissionStatement, "Allows(0)", "Allows(generation)")},
			{"HexZero", strings.ReplaceAll(maintenanceAdmissionStatement, "Allows(0)", "Allows(0x0)")},
			{"IgnoredError", "_ = c.maintenanceFenceAllows(0)"},
			{"WrappedError", strings.ReplaceAll(maintenanceAdmissionStatement, "return err", "return fmt.Errorf(\"suffix: %w\", err)")},
			{"OtherBinding", strings.ReplaceAll(maintenanceAdmissionStatement, "c.maintenance", "other.maintenance")},
			{"ShadowedReceiver", "{ c := &Cell{}; " + maintenanceAdmissionStatement + " }"},
			{"Duplicate", maintenanceAdmissionStatement + "; " + maintenanceAdmissionStatement},
			{"AdditionalHelper", maintenanceAdmissionStatement + "; _ = c.updateStep()"},
			{"AdditionalReader", maintenanceAdmissionStatement + "; _ = c.readMaintenanceFence()"},
			{"AdditionalJournal", maintenanceAdmissionStatement + "; _ = c.readUpdateJournal()"},
		} {
			fixtures = append(fixtures, fixture{owner + "_" + item.name, "lifecycle.go", strings.Replace(approved, maintenanceAdmissionStatement, item.replacement, 1), "", true})
		}
		fixtures = append(fixtures,
			fixture{owner + "_WrongFile", "other.go", approved, "", true},
			fixture{owner + "_WrongReceiver", "lifecycle.go", strings.Replace(approved, "*Cell", "*Other", 1), "", true},
			fixture{owner + "_ValueReceiver", "lifecycle.go", strings.Replace(approved, "*Cell", "Cell", 1), "", true},
			fixture{owner + "_WrongFunction", "lifecycle.go", strings.Replace(approved, owner+"()", "Stop()", 1), "", true},
		)
	}
	for index, initializer := range []string{
		`var result = (&Cell{}).maintenanceFenceAllows(0)`,
		`var result = (&Cell{}).maintenanceFenceAllows`,
		`var result = func(c *Cell) error { return c.maintenanceFenceAllows(0) }`,
		`var result = func() error { return (&Cell{}).maintenanceFenceAllows(0) }()`,
	} {
		fixtures = append(fixtures, fixture{fmt.Sprintf("Initializer%d", index), "lifecycle.go", initializer, "", true})
	}
	fixtures = append(fixtures,
		fixture{"FoundationAlias", "lifecycle.go", `func (c *Cell) Start() error { return alias(c, 0) }`, `var alias = (*Cell).maintenanceFenceAllows`, true},
		fixture{"FoundationInit", "lifecycle.go", "", `func init() {}`, true},
		fixture{"FoundationInitializer", "lifecycle.go", "", `var result = (&Cell{}).maintenanceFenceAllows(0)`, true},
	)
	for _, fixture := range fixtures {
		t.Run(fixture.name, func(t *testing.T) {
			for _, ending := range []string{"LF", "CRLF"} {
				t.Run(ending, func(t *testing.T) {
					sources := map[string]string{"update_fence.go": foundation + fixture.extra, fixture.file: "package localcell\n" + fixture.source}
					if ending == "CRLF" {
						for name, source := range sources {
							sources[name] = strings.ReplaceAll(source, "\n", "\r\n")
						}
					}
					failures := updateFoundationViolations(sources)
					if strings.Contains(strings.Join(failures, ";"), "cannot parse") {
						t.Fatal("guard fixture parse failure is not a kill")
					}
					if (len(failures) != 0) != fixture.rejected {
						t.Fatalf("guard rejected = %v, want %v: %v", len(failures) != 0, fixture.rejected, failures)
					}
				})
			}
		})
	}
}

func admissionIndent(source string) string { return "\t" + strings.ReplaceAll(source, "\n", "\n\t") }
