package localcell

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/build/constraint"
	"go/format"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
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
	"Install": `timeout := c.cfg.Timeout
if options.ManagedPrincipals {
 timeout = c.readinessTimeout("server", &Manifest{})
}
ctx, cancel := context.WithTimeout(ctx, timeout)
defer cancel()
if options.ManagedPrincipals {
 if _, err := os.Lstat(c.path("manifest.json")); !errors.Is(err, os.ErrNotExist) { return errPrincipalLayout }
 if entries, err := os.ReadDir(c.path("pg")); (err == nil && len(entries) != 0) || (err != nil && !errors.Is(err, os.ErrNotExist)) { return errPrincipalLayout }
}
lock, err := c.own(true)
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
	"Repair": `parent := ctx
ctx, cancel := c.deadline(ctx)
defer cancel()
lock, err := c.ownExisting()
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
	"Remove": `ctx, cancel := c.serviceShutdownDeadline(ctx)
defer cancel()
if err := c.exitServiceHostForRemoval(ctx); err != nil { return err }
lock, err := c.ownExisting()
if err != nil { return err }
defer func() { _ = lock.Close() }()
` + maintenanceAdmissionStatement + `
if err := c.setServiceDesiredStateLocked("stopped"); err != nil && !errors.Is(err, os.ErrNotExist) { return err }`,
}

var maintenanceAdmissionParameters = map[string]string{
	"Install": "ctx context.Context, options InstallOptions",
	"Start":   "ctx context.Context",
	"Repair":  "ctx context.Context",
	"Remove":  "ctx context.Context, options RemoveOptions",
}

// maintenanceAdmissionOwner is the single approval predicate shared by both
// source barriers. An empty result approves the exact signature and prefix;
// otherwise the result names the invariant that refused the declaration.
func maintenanceAdmissionOwner(file string, fn *ast.FuncDecl) string {
	parameters, known := maintenanceAdmissionParameters[fn.Name.Name]
	if file != "lifecycle.go" || !known || fn.Type.TypeParams != nil || fn.Recv == nil || len(fn.Recv.List) != 1 {
		return "signature"
	}
	receiver := fn.Recv.List[0]
	pointer, ok := receiver.Type.(*ast.StarExpr)
	if !ok || len(receiver.Names) != 1 {
		return "signature"
	}
	typ, ok := pointer.X.(*ast.Ident)
	if !ok || typ.Name != "Cell" {
		return "signature"
	}
	if admissionNodeText(&ast.FuncType{Params: fn.Type.Params}) != "func("+parameters+")" {
		return "signature"
	}
	if fn.Type.Results == nil || len(fn.Type.Results.List) != 1 || len(fn.Type.Results.List[0].Names) != 0 {
		return "signature"
	}
	result, ok := fn.Type.Results.List[0].Type.(*ast.Ident)
	if !ok || result.Name != "error" {
		return "signature"
	}
	want := admissionStatements(maintenanceAdmissionBoundaries[fn.Name.Name])
	if fn.Body == nil || len(want) == 0 || len(fn.Body.List) < len(want) {
		return "position"
	}
	for index, statement := range want {
		if admissionNodeText(fn.Body.List[index]) != statement {
			return "position"
		}
	}
	return ""
}

// Scan text regardless of build selection: an alternate platform or tagged
// owner must not silently replace one of the four approved declarations.
func maintenanceAdmissionOwnerInventory(sources map[string]string) []string {
	var failures []string
	owners := map[string]int{}
	for name, source := range sources {
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		source = strings.ReplaceAll(source, "\r\n", "\n")
		if name == "lifecycle.go" {
			for _, line := range strings.Split(source, "\n") {
				line = strings.TrimSpace(line)
				if constraint.IsGoBuild(line) || constraint.IsPlusBuild(line) {
					failures = append(failures, "lifecycle_signature")
				}
			}
		}
		file, err := admissionReadOnlyParse(name, source)
		if err != nil {
			failures = append(failures, name+"_parse")
			continue
		}
		for _, declaration := range file.Decls {
			fn, ok := declaration.(*ast.FuncDecl)
			if !ok {
				continue
			}
			if _, expected := maintenanceAdmissionBoundaries[fn.Name.Name]; !expected || updateReceiverName(fn) != "Cell" {
				continue
			}
			owners[fn.Name.Name]++
			if name != "lifecycle.go" {
				failures = append(failures, fn.Name.Name+"_signature")
			}
		}
	}
	for name := range maintenanceAdmissionBoundaries {
		if owners[name] != 1 {
			failures = append(failures, name+"_owner_count")
		}
	}
	sort.Strings(failures)
	return failures
}

func admissionReadSources(t *testing.T) map[string]string {
	t.Helper()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal("cannot inventory local cell sources")
	}
	sources := make(map[string]string)
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		data, err := os.ReadFile(name)
		if err != nil {
			t.Fatal("cannot read local cell source")
		}
		sources[name] = string(data)
	}
	return sources
}

func maintenanceAdmissionContract(source string) []string {
	source = strings.ReplaceAll(source, "\r\n", "\n")
	file, err := admissionReadOnlyParse("lifecycle.go", source)
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
		_, expected := maintenanceAdmissionBoundaries[fn.Name.Name]
		if !expected {
			continue
		}
		owners[fn.Name.Name]++
		fail := func(invariant string) { failures = append(failures, fn.Name.Name+"_"+invariant) }
		if invariant := maintenanceAdmissionOwner("lifecycle.go", fn); invariant != "" {
			fail(invariant)
		}
		admissions := updateOrdinaryAdmissions("lifecycle.go", fn)
		if len(admissions) != 1 {
			fail("literal_zero_direct_error_return")
		}
		if fn.Body == nil {
			fail("position")
			continue
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
	if failures := maintenanceAdmissionOwnerInventory(admissionReadSources(t)); len(failures) != 0 {
		t.Fatalf("admission invariants: %v", failures)
	}
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

type admissionSourceMutation struct {
	name, invariant string
	change          func(string) string
}

func admissionEditStatements(source string, edit func([]ast.Stmt) []ast.Stmt) string {
	file, err := parser.ParseFile(token.NewFileSet(), "lifecycle.go", "package localcell\n"+source, 0)
	if err != nil {
		return "<invalid>"
	}
	fn := file.Decls[0].(*ast.FuncDecl)
	fn.Body.List = edit(fn.Body.List)
	return admissionNodeText(fn)
}

// These same named edits exercise the approved snippets and each real owner.
func maintenanceAdmissionBypasses() []admissionSourceMutation {
	gate := admissionIndent(admissionStatements(maintenanceAdmissionStatement)[0])
	prepend := func(statement string) func(string) string {
		return func(source string) string { return strings.Replace(source, "{\n", "{\n"+statement+"\n", 1) }
	}
	early := "if c.cfg.Timeout == 42*time.Second { return nil }"
	return []admissionSourceMutation{
		{"EarlyReturnBeforeAdmission", "position", prepend(early)},
		{"UnconditionalReturnBeforeAdmission", "position", prepend("return nil")},
		{"ReceiverRebinding", "position", prepend("c = other")},
		{"NamedResultDeferredSwallow", "signature", func(source string) string {
			return prepend("defer func() { ret = nil }()")(strings.Replace(source, ") error {", ") (ret error) {", 1))
		}},
		{"SwitchReturnBeforeAdmission", "position", prepend("switch c.cfg.Timeout { case 44 * time.Second: return nil }")},
		{"EarlyReturnImmediatelyBeforeAdmission", "position", func(source string) string {
			return strings.Replace(source, gate, early+"\n"+gate, 1)
		}},
		{"GotoPastAdmission", "position", func(source string) string {
			return strings.Replace(source, gate, "if c.cfg.Timeout == 45*time.Second { goto admitted }\n"+gate+"\nadmitted:", 1)
		}},
		{"NamedResultOnly", "signature", func(source string) string { return strings.Replace(source, ") error {", ") (ret error) {", 1) }},
		{"ExtraResult", "signature", func(source string) string { return strings.Replace(source, ") error {", ") (bool, error) {", 1) }},
		{"PreludeStatementDeleted", "position", func(source string) string {
			return admissionEditStatements(source, func(statements []ast.Stmt) []ast.Stmt {
				for index, statement := range statements {
					if strings.HasPrefix(admissionNodeText(statement), "lock, err :=") {
						return append(statements[:index+1], statements[index+2:]...)
					}
				}
				return statements
			})
		}},
		{"PreludeReordered", "position", func(source string) string {
			return admissionEditStatements(source, func(statements []ast.Stmt) []ast.Stmt {
				statements[0], statements[1] = statements[1], statements[0]
				return statements
			})
		}},
	}
}

func TestMaintenanceFenceAdmissionSourceContractMutations(t *testing.T) {
	source := admissionReadLifecycle(t)
	if failures := maintenanceAdmissionContract(source); len(failures) != 0 {
		t.Fatalf("unchanged control: %v", failures)
	}
	gate := admissionIndent(admissionStatements(maintenanceAdmissionStatement)[0])
	closeLock := admissionIndent(admissionStatements(`defer func() { _ = lock.Close() }()`)[0])
	for _, owner := range []string{"Install", "Start", "Repair", "Remove"} {
		t.Run(owner, func(t *testing.T) {
			mutations := []admissionSourceMutation{
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
				mutations = append(mutations, admissionSourceMutation{"BeforeManifest", "position", func(body string) string {
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
				mutations = append(mutations, admissionSourceMutation{"BeforeServiceFence", "position", func(body string) string {
					body = strings.Replace(body, gate, "", 1)
					return strings.Replace(body, "if err := c.serviceStartFence()", gate+"\nif err := c.serviceStartFence()", 1)
				}})
			}
			if owner == "Remove" {
				predrain := admissionIndent(admissionStatements(`if err := c.exitServiceHostForRemoval(ctx); err != nil { return err }`)[0])
				mutations = append(mutations,
					admissionSourceMutation{"DeletePredrain", "position", func(body string) string { return strings.Replace(body, predrain, "", 1) }},
					admissionSourceMutation{"BeforePredrain", "position", func(body string) string {
						body = strings.Replace(body, gate, "", 1)
						return strings.Replace(body, predrain, gate+"\n"+predrain, 1)
					}},
				)
			}
			mutations = append(mutations, admissionSourceMutation{"AfterFirstEffect", "position", func(body string) string {
				last := admissionStatements(maintenanceAdmissionBoundaries[owner])
				effect := admissionIndent(last[len(last)-1])
				body = strings.Replace(body, gate, "", 1)
				return strings.Replace(body, effect, effect+"\n"+gate, 1)
			}})
			mutations = append(mutations, maintenanceAdmissionBypasses()...)
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

// Build fixture controls independently from the pinned strings. A deleted pin
// must fail an approved fixture instead of weakening both sides of the oracle.
func admissionApprovedOwners(t *testing.T) map[string]string {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), "lifecycle.go", admissionReadLifecycle(t), 0)
	if err != nil {
		t.Fatal("control source does not parse")
	}
	owners := make(map[string]string)
	gate := admissionStatements(maintenanceAdmissionStatement)[0]
	for _, declaration := range file.Decls {
		fn, ok := declaration.(*ast.FuncDecl)
		if !ok {
			continue
		}
		parameters, expected := maintenanceAdmissionParameters[fn.Name.Name]
		if !expected {
			continue
		}
		for index, statement := range fn.Body.List {
			if admissionNodeText(statement) != gate {
				continue
			}
			if index+1 >= len(fn.Body.List) {
				t.Fatal("approved owner has no guarded effect")
			}
			var prefix []string
			for _, statement := range fn.Body.List[:index+2] {
				prefix = append(prefix, admissionNodeText(statement))
			}
			original := "func (c *Cell) " + fn.Name.Name + "(" + parameters + ") error {\n" + strings.Join(prefix, "\n") + "\nreturn nil\n}"
			owners[fn.Name.Name] = admissionEditStatements(original, func(statements []ast.Stmt) []ast.Stmt { return statements })
			break
		}
	}
	if len(owners) != 4 {
		t.Fatal("approved fixture owner count differs")
	}
	return owners
}

func TestMaintenanceFenceAdmissionGuardFixtures(t *testing.T) {
	const foundation = `package localcell
func (c *Cell) maintenanceFenceAllows(generation int) error { return nil }
func (c *Cell) readMaintenanceFence() error { return nil }
func (c *Cell) updateStep() error { return nil }
func (c *Cell) readUpdateJournal() error { return nil }
`
	type fixture struct {
		name, file, source, extra, invariant string
		approved, approvedExtra              string
		rejected                             bool
	}
	var fixtures []fixture
	approvedOwners := admissionApprovedOwners(t)
	for _, owner := range []string{"Install", "Start", "Repair", "Remove"} {
		approved := approvedOwners[owner]
		fixtures = append(fixtures, fixture{name: owner + "_Approved", file: "lifecycle.go", source: approved, approved: approved})
		add := func(name, file, source, extra, invariant string) {
			fixtures = append(fixtures, fixture{name: owner + "_" + name, file: file, source: source, extra: extra, invariant: invariant, approved: approved, rejected: true})
		}
		gate := admissionIndent(admissionStatements(maintenanceAdmissionStatement)[0])
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
			add(item.name, "lifecycle.go", strings.Replace(approved, gate, item.replacement, 1), "", owner+"_position")
		}
		add("WrongFile", "other.go", approved, "", owner+"_signature")
		add("WrongReceiver", "lifecycle.go", strings.Replace(approved, "*Cell", "*Other", 1), "", owner+"_signature")
		add("ValueReceiver", "lifecycle.go", strings.Replace(approved, "*Cell", "Cell", 1), "", owner+"_signature")
		add("WrongFunction", "lifecycle.go", strings.Replace(approved, owner+"(", "Stop(", 1), "", "Stop_signature")
		add("WrongParameters", "lifecycle.go", strings.Replace(approved, "ctx context.Context", "ctx string", 1), "", owner+"_signature")
		for _, mutation := range maintenanceAdmissionBypasses() {
			changed := mutation.change(approved)
			// Both control and rebinding variant declare the alternate binding. The
			// named edit is solely the statement inserted in the approved body.
			if mutation.name == "ReceiverRebinding" {
				changed = "var other *Cell\n" + changed
				fixtures = append(fixtures, fixture{name: owner + "_" + mutation.name, file: "lifecycle.go", source: changed, invariant: owner + "_" + mutation.invariant, approved: "var other *Cell\n" + approved, rejected: true})
				continue
			}
			add(mutation.name, "lifecycle.go", changed, "", owner+"_"+mutation.invariant)
		}
	}
	approved := approvedOwners["Start"]
	for index, initializer := range []string{
		`var result = (&Cell{}).maintenanceFenceAllows(0)`,
		`var result = (&Cell{}).maintenanceFenceAllows`,
		`var result = func(c *Cell) error { return c.maintenanceFenceAllows(0) }`,
		`var result = func() error { return (&Cell{}).maintenanceFenceAllows(0) }()`,
	} {
		fixtures = append(fixtures, fixture{name: fmt.Sprintf("Initializer%d", index), file: "lifecycle.go", source: approved + "\n" + initializer, invariant: "package initializer selects update helper", approved: approved, rejected: true})
	}
	fixtures = append(fixtures,
		fixture{name: "FoundationAlias", file: "lifecycle.go", source: strings.Replace(approved, admissionIndent(admissionStatements(maintenanceAdmissionStatement)[0]), `if err := alias(c, 0); err != nil { return err }`, 1), extra: `var alias = (*Cell).maintenanceFenceAllows`, invariant: "references update helper alias", approved: approved, approvedExtra: `var alias = (*Cell).maintenanceFenceAllows`, rejected: true},
		fixture{name: "FoundationInit", file: "lifecycle.go", source: approved, extra: `func init() {}`, invariant: "update foundations contain init", approved: approved, rejected: true},
		fixture{name: "FoundationInitializer", file: "lifecycle.go", source: approved, extra: `var result = (&Cell{}).maintenanceFenceAllows(0)`, invariant: "update foundations run code at package initialization", approved: approved, rejected: true},
	)
	for _, fixture := range fixtures {
		t.Run(fixture.name, func(t *testing.T) {
			if fixture.rejected && fixture.file+fixture.source+fixture.extra == "lifecycle.go"+fixture.approved+fixture.approvedExtra {
				t.Fatal("mutation did not change its named site")
			}
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
					if fixture.rejected && !strings.Contains(strings.Join(failures, ";"), fixture.invariant) {
						t.Fatalf("mutant survived named invariant %s: %v", fixture.invariant, failures)
					}
					owner, form, _ := strings.Cut(fixture.name, "_")
					verifyContract := form == "WrongParameters"
					for _, mutation := range maintenanceAdmissionBypasses() {
						verifyContract = verifyContract || form == mutation.name
					}
					if verifyContract {
						complete := "package localcell\n"
						for _, candidate := range []string{"Install", "Start", "Repair", "Remove"} {
							body := approvedOwners[candidate]
							if candidate == owner {
								body = fixture.source
							}
							complete += body + "\n"
						}
						if ending == "CRLF" {
							complete = strings.ReplaceAll(complete, "\n", "\r\n")
						}
						contractFailures := maintenanceAdmissionContract(complete)
						if containsAdmissionFailure(contractFailures, "parse") {
							t.Fatal("guard fixture parse failure is not a kill")
						}
						if !containsAdmissionFailure(contractFailures, fixture.invariant) {
							t.Fatalf("mutant survived named invariant %s: %v", fixture.invariant, contractFailures)
						}
					}
				})
			}
		})
	}
	// This scan is independently exercised on a complete approved tree so a
	// missing-owner rejection cannot mask a missed duplicate or build constraint.
	control := "package localcell\n"
	for _, owner := range []string{"Install", "Start", "Repair", "Remove"} {
		control += approvedOwners[owner] + "\n"
	}
	for _, fixture := range []struct{ name, extra, prefix, invariant string }{
		{"SecondStartBuildTagged", "//go:build reviewtag\n\npackage localcell\nfunc (c *Cell) Start(ctx context.Context) error { return nil }", "", "Start_owner_count"},
		{"SecondStartGenericBuildTagged", "//go:build reviewtag\n\npackage localcell\nfunc (c *Cell[T]) Start(ctx context.Context) error { return nil }", "", "Start_owner_count"},
		{"LifecycleGoBuild", "", "//go:build reviewtag\n\n", "lifecycle_signature"},
		{"LifecycleLegacyBuild", "", "// +build reviewtag\n\n", "lifecycle_signature"},
		{"LifecycleLegacyBuildNoSpace", "", "//+build reviewtag\n\n", "lifecycle_signature"},
		{"LifecycleGoBuildTab", "", "//go:build\treviewtag\n\n", "lifecycle_signature"},
		{"LifecycleLegacyBuildTab", "", "// +build\treviewtag\n\n", "lifecycle_signature"},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			for _, ending := range []string{"LF", "CRLF"} {
				t.Run(ending, func(t *testing.T) {
					sources := map[string]string{"lifecycle.go": control}
					if failures := maintenanceAdmissionOwnerInventory(sources); len(failures) != 0 {
						t.Fatalf("unchanged control: %v", failures)
					}
					sources["lifecycle.go"] = fixture.prefix + control
					if fixture.extra != "" {
						sources["tagged.go"] = fixture.extra
					}
					if sources["lifecycle.go"] == control && sources["tagged.go"] == "" {
						t.Fatal("mutation did not change its named site")
					}
					if ending == "CRLF" {
						for name, source := range sources {
							sources[name] = strings.ReplaceAll(source, "\n", "\r\n")
						}
					}
					failures := maintenanceAdmissionOwnerInventory(sources)
					if strings.Contains(strings.Join(failures, ";"), "parse") {
						t.Fatal("guard fixture parse failure is not a kill")
					}
					if !containsAdmissionFailure(failures, fixture.invariant) {
						t.Fatalf("mutant survived named invariant %s: %v", fixture.invariant, failures)
					}
				})
			}
		})
	}
}

func admissionIndent(source string) string { return "\t" + strings.ReplaceAll(source, "\n", "\n\t") }

// Appendix C independently closes the two holes a composite contract could
// conceal: accepting every admitted selector, and losing a sole owner's file.
func TestMaintenanceFenceAdmissionAppendixC(t *testing.T) {
	approved := admissionApprovedOwners(t)
	const foundation = `package localcell
func (c *Cell) maintenanceFenceAllows(generation int) error { return nil }
`
	control := "package localcell\n"
	for _, owner := range []string{"Install", "Start", "Repair", "Remove"} {
		control += approved[owner] + "\n"
	}
	if failures := maintenanceAdmissionOwnerInventory(map[string]string{"lifecycle.go": control}); len(failures) != 0 {
		t.Fatalf("appendix C inventory control: %v", failures)
	}
	if failures := updateFoundationViolations(map[string]string{"lifecycle.go": control, "update_fence.go": foundation}); len(failures) != 0 {
		t.Fatalf("appendix C foundation control: %v", failures)
	}
	for _, owner := range []string{"Install", "Start", "Repair", "Remove"} {
		t.Run(owner, func(t *testing.T) {
			t.Run("LateLiteralZeroGate", func(t *testing.T) {
				changed := admissionEditStatements(approved[owner], func(statements []ast.Stmt) []ast.Stmt {
					gateFile, err := parser.ParseFile(token.NewFileSet(), "gate.go", "package localcell; func fixture() {"+maintenanceAdmissionStatement+"}", 0)
					if err != nil {
						t.Fatal("gate fixture parse failure")
					}
					gate := gateFile.Decls[0].(*ast.FuncDecl).Body.List[0]
					// Approved includes the full prefix and first effect, then return nil.
					result := append([]ast.Stmt{}, statements[:len(statements)-1]...)
					result = append(result, gate)
					return append(result, statements[len(statements)-1])
				})
				for _, ending := range []string{"LF", "CRLF"} {
					t.Run(ending, func(t *testing.T) {
						source := "package localcell\n" + changed
						if ending == "CRLF" {
							source = strings.ReplaceAll(source, "\n", "\r\n")
						}
						file, err := parser.ParseFile(token.NewFileSet(), "lifecycle.go", source, 0)
						if err != nil {
							t.Fatal("appendix C parse failure is not a mutation kill")
						}
						fn := file.Decls[0].(*ast.FuncDecl)
						if got := maintenanceAdmissionOwner("lifecycle.go", fn); got != "" {
							t.Fatalf("appendix C changed approved prefix: %s", got)
						}
						failures := updateFoundationViolations(map[string]string{"lifecycle.go": source, "update_fence.go": foundation})
						if !containsAdmissionFailure(failures, owner+"_literal_zero_direct_error_return") {
							t.Fatalf("appendix C foundation accepted late literal-zero gate: %v", failures)
						}
					})
				}
			})
			t.Run("SoleOwnerWrongFile", func(t *testing.T) {
				// Move the real complete declaration, preserving its exact bytes.
				var moved string
				remaining := admissionMutateFunction(t, admissionReadLifecycle(t), owner, func(body string) string {
					moved = body
					return ""
				})
				for _, ending := range []string{"LF", "CRLF"} {
					t.Run(ending, func(t *testing.T) {
						sources := map[string]string{"lifecycle.go": remaining, "other.go": "package localcell\n" + moved}
						if ending == "CRLF" {
							for name, source := range sources {
								sources[name] = strings.ReplaceAll(source, "\n", "\r\n")
							}
						}
						failures := maintenanceAdmissionOwnerInventory(sources)
						if !containsAdmissionFailure(failures, owner+"_signature") {
							t.Fatalf("appendix C inventory lost wrong-file signature: %v", failures)
						}
						if containsAdmissionFailure(failures, owner+"_owner_count") {
							t.Fatal("appendix C must retain exactly one declaration")
						}
					})
				}
			})
		})
	}
}
