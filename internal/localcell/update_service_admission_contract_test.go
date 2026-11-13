package localcell

import (
	"fmt"
	"go/ast"
	"go/build/constraint"
	"go/parser"
	"go/token"
	"sort"
	"strings"
	"testing"
)

// These independent pins include every legacy return before the new admission,
// and the first guarded statement. They are not reconstructed from the owner.
var maintenanceServiceAdmissionBoundaries = map[string]string{
	"PrepareService": `if os.Getenv("WITSELF_RUNTIME") != "local" { return errors.New("service_local_runtime_required") }
if err := ctx.Err(); err != nil { return err }
lock, err := c.own(false)
if err != nil { return err }
defer func() { _ = lock.Close() }()
m, err := readManifest(c.cfg.Root)
if err != nil || !m.Installed || m.RemovalRetained { return errors.New("service_installation_required") }
uid, err := serviceOwner()
if err != nil { return err }
d.Version, d.InstallationID, d.UID, d.Root = 1, m.InstallationID, uid, c.cfg.Root
if d.Runtime == "" { d.Runtime = "local" }
if d.Scope == "" { d.Scope = "user" }
if d.DesiredState == "" { d.DesiredState = "running" }
if d.ResourceProfile == "" { d.ResourceProfile = "unconfigured" }
if d.Payload.Generation == "" { d.Payload.Generation = m.InstallationID + "-1" }
d.Revision, d.RepairRevision, d.Faulted = 1, 0, false
if err := validateServiceDescriptor(d); err != nil { return err }
old, err := c.ServiceDescriptor()
if err == nil {
 if old.Adapter != d.Adapter || old.Runtime != d.Runtime || !reflect.DeepEqual(old.Payload, d.Payload) { return ErrServiceOwnershipConflict }
 _, err = c.serviceControlSecret()
 return err
}
if !errors.Is(err, os.ErrNotExist) { return err }
` + maintenanceAdmissionStatement + `
if err := privateDir(c.path("service"), true); err != nil { return err }`,
	"RepairService": `if err := c.SetServiceDesiredState(ctx, "stopped"); err != nil { return err }
if err := c.Repair(ctx); err != nil { return err }
if err := c.stopServiceChildren(ctx); err != nil { return err }
lock, err := c.own(false)
if err != nil { return err }
defer func() { _ = lock.Close() }()
d, err := c.ServiceDescriptor()
if err != nil { return err }
` + maintenanceAdmissionStatement + `
d.Faulted, d.Revision, d.RepairRevision = false, d.Revision+1, d.RepairRevision+1`,
	"reconcileServiceBoot": `lock, err := c.own(false)
if err != nil { return err }
defer func() { _ = lock.Close() }()
boot, err := serviceBootID()
if err != nil { return err }
d, err := c.ServiceDescriptor()
if err != nil { return err }
var previous serviceBootRecord
err = readJSON(c.path("service", "boot.json"), &previous)
if err != nil && !errors.Is(err, os.ErrNotExist) { return err }
if err == nil && (previous.InstallationID != d.InstallationID || previous.BootID == "") { return ErrServiceOwnershipConflict }
` + maintenanceAdmissionStatement + `
if previous.BootID != "" && previous.BootID != boot {
 if err := c.reconcileOldBoot(previous.BootID, boot); err != nil { return err }
}`,
	"Reinstall": `lock, err := c.own(false)
if err != nil { return err }
defer func() { _ = lock.Close() }()
` + maintenanceAdmissionStatement + `
return c.reinstallLocked(ctx, options)`,
}

var maintenanceServiceAdmissionFiles = map[string]string{
	"PrepareService":       "service_descriptor.go",
	"RepairService":        "service_descriptor.go",
	"reconcileServiceBoot": "service_boot.go",
	"Reinstall":            "reinstall.go",
}

var maintenanceServiceAdmissionParameters = map[string]string{
	"PrepareService":       "ctx context.Context, d ServiceDescriptor",
	"RepairService":        "ctx context.Context",
	"reconcileServiceBoot": "",
	"Reinstall":            "ctx context.Context, options ReinstallOptions",
}

// Keep the old four-owner predicate and its independent counts unchanged.
func ordinaryMaintenanceAdmissionOwner(file string, fn *ast.FuncDecl) string {
	if fn.Name.Name == "Remove" && maintenanceRemovalFiles[updateReceiverName(fn)] == file {
		return maintenanceRemovalAdmissionOwner(file, fn)
	}
	if receiver := updateReceiverName(fn); receiver == "systemdUserAdapter" || receiver == "launchdAgentAdapter" {
		return maintenanceUnixAdapterAdmissionOwner(file, fn)
	}
	if updateReceiverName(fn) == "windowsLogonTaskAdapter" {
		return maintenanceAdapterAdmissionOwner(file, fn)
	}
	if _, host := maintenanceHostAdmissionFiles[fn.Name.Name]; host {
		return maintenanceHostAdmissionOwner(file, fn)
	}
	if fn.Name.Name == "setServiceDesiredStateLocked" {
		return maintenanceTransitionSetterOwner(file, fn)
	}
	if _, service := maintenanceServiceAdmissionFiles[fn.Name.Name]; service {
		return maintenanceServiceAdmissionOwner(file, fn)
	}
	return maintenanceAdmissionOwner(file, fn)
}

// The shared predicate approves only the exact declaration and prefix at index
// zero, with the same parser-resolved receiver and continuously held lock.
func maintenanceServiceAdmissionOwner(file string, fn *ast.FuncDecl) string {
	expectedFile, known := maintenanceServiceAdmissionFiles[fn.Name.Name]
	if !known || file != expectedFile || fn.Type.TypeParams != nil || fn.Recv == nil || len(fn.Recv.List) != 1 {
		return "signature"
	}
	receiver := fn.Recv.List[0]
	pointer, pointerOK := receiver.Type.(*ast.StarExpr)
	if !pointerOK || len(receiver.Names) != 1 || receiver.Names[0].Name != "c" || receiver.Names[0].Obj == nil {
		return "signature"
	}
	typ, typeOK := pointer.X.(*ast.Ident)
	if !typeOK || typ.Name != "Cell" {
		return "signature"
	}
	parameters := maintenanceServiceAdmissionParameters[fn.Name.Name]
	if admissionNodeText(&ast.FuncType{Params: fn.Type.Params}) != "func("+parameters+")" {
		return "signature"
	}
	if fn.Type.Results == nil || len(fn.Type.Results.List) != 1 || len(fn.Type.Results.List[0].Names) != 0 {
		return "signature"
	}
	result, resultOK := fn.Type.Results.List[0].Type.(*ast.Ident)
	if !resultOK || result.Name != "error" {
		return "signature"
	}
	want := admissionStatements(maintenanceServiceAdmissionBoundaries[fn.Name.Name])
	if fn.Body == nil || len(want) == 0 || len(fn.Body.List) < len(want) {
		return "position"
	}
	for index, statement := range want {
		if admissionNodeText(fn.Body.List[index]) != statement {
			return "position"
		}
	}
	if !maintenanceServiceContinuousHold(fn) {
		return "continuous_hold"
	}
	return ""
}

// Entry/exit runtime sampling cannot observe the instant of the fence read.
// This source proof pins one acquisition, its identity and one deferred close,
// including every reference after the admitted prefix.
func maintenanceServiceContinuousHold(fn *ast.FuncDecl) bool {
	if fn.Body == nil || fn.Recv == nil || len(fn.Recv.List) != 1 || len(fn.Recv.List[0].Names) != 1 {
		return false
	}
	receiver := fn.Recv.List[0].Names[0]
	var lockIdentifier *ast.Ident
	for _, statement := range fn.Body.List {
		assignment, ok := statement.(*ast.AssignStmt)
		if !ok || assignment.Tok != token.DEFINE {
			continue
		}
		for _, left := range assignment.Lhs {
			if identifier, ok := left.(*ast.Ident); ok && identifier.Name == "lock" {
				if lockIdentifier != nil {
					return false
				}
				lockIdentifier = identifier
			}
		}
	}
	if lockIdentifier == nil || lockIdentifier.Obj == nil {
		return false
	}
	allowedReceiverReferences := map[*ast.Ident]bool{}
	ast.Inspect(fn.Body, func(node ast.Node) bool {
		if selector, ok := node.(*ast.SelectorExpr); ok {
			if identifier, ok := selector.X.(*ast.Ident); ok && identifier.Obj == receiver.Obj {
				allowedReceiverReferences[identifier] = true
			}
		}
		return true
	})
	ownCalls, lockReferences, closes, defers, goroutines := 0, 0, 0, 0, 0
	bindingOK := true
	ast.Inspect(fn.Body, func(node ast.Node) bool {
		switch node := node.(type) {
		case *ast.Ident:
			if node.Obj == lockIdentifier.Obj {
				lockReferences++
			}
			if node.Name == receiver.Name && (node.Obj != receiver.Obj || !allowedReceiverReferences[node]) {
				bindingOK = false
			}
		case *ast.SelectorExpr:
			if node.Sel.Name == "own" || node.Sel.Name == "ownExisting" {
				ownCalls++
			}
			if identifier, ok := node.X.(*ast.Ident); ok && identifier.Obj == lockIdentifier.Obj && node.Sel.Name == "Close" {
				closes++
			}
		case *ast.DeferStmt:
			defers++
		case *ast.GoStmt:
			goroutines++
		}
		return true
	})
	return ownCalls == 1 && lockReferences == 2 && closes == 1 && defers == 1 && goroutines == 0 && bindingOK
}

// Inventory all production text independently of GOOS and build tags.
func maintenanceServiceAdmissionOwnerInventory(sources map[string]string) []string {
	var failures []string
	owners := map[string]int{}
	for name, source := range sources {
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		source = strings.ReplaceAll(source, "\r\n", "\n")
		for _, canonical := range maintenanceServiceAdmissionFiles {
			if name == canonical {
				for _, line := range strings.Split(source, "\n") {
					line = strings.TrimSpace(line)
					if constraint.IsGoBuild(line) || constraint.IsPlusBuild(line) {
						failures = append(failures, name+"_signature")
					}
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
			canonical, expected := maintenanceServiceAdmissionFiles[fn.Name.Name]
			if !expected || updateReceiverName(fn) != "Cell" {
				continue
			}
			owners[fn.Name.Name]++
			if name != canonical {
				failures = append(failures, fn.Name.Name+"_signature")
			}
		}
	}
	for owner := range maintenanceServiceAdmissionFiles {
		if owners[owner] != 1 {
			failures = append(failures, owner+"_owner_count")
		}
	}
	sort.Strings(failures)
	return failures
}

func maintenanceServiceAdmissionContract(sources map[string]string) []string {
	failures := maintenanceServiceAdmissionOwnerInventory(sources)
	failures = append(failures, maintenanceTransitionContract(sources)...)
	failures = append(failures, maintenanceHostAdmissionContract(sources)...)
	failures = append(failures, maintenanceAdapterAdmissionContract(sources["service_windows_task.go"])...)
	failures = append(failures, maintenanceUnixAdapterAdmissionContract(sources)...)
	totalReferences := 0
	for name, source := range sources {
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := admissionReadOnlyParse(name, strings.ReplaceAll(source, "\r\n", "\n"))
		if err != nil {
			failures = append(failures, name+"_parse")
			continue
		}
		fileReferences := 0
		ast.Inspect(file, func(node ast.Node) bool {
			if selector, ok := node.(*ast.SelectorExpr); ok && selector.Sel.Name == "maintenanceFenceAllows" {
				fileReferences++
			}
			return true
		})
		totalReferences += fileReferences
		expectedReferences := 0
		switch name {
		case "lifecycle.go":
			expectedReferences = 4
		case "service_descriptor.go":
			expectedReferences = 3
		case "service_windows_task.go":
			expectedReferences = 3
		case "service_systemd_adapter.go", "service_launchd.go":
			expectedReferences = 2
		case "service_boot.go", "service_health.go", "service_maintenance.go", "reinstall.go":
			expectedReferences = 1
		}
		if fileReferences != expectedReferences {
			failures = append(failures, name+"_reference_count")
		}
		for _, declaration := range file.Decls {
			fn, ok := declaration.(*ast.FuncDecl)
			if !ok {
				continue
			}
			if _, expected := maintenanceServiceAdmissionFiles[fn.Name.Name]; !expected {
				continue
			}
			if invariant := maintenanceServiceAdmissionOwner(name, fn); invariant != "" {
				failures = append(failures, fn.Name.Name+"_"+invariant)
			}
			if !maintenanceServiceContinuousHold(fn) {
				failures = append(failures, fn.Name.Name+"_continuous_hold")
			}
			if len(updateOrdinaryAdmissions(name, fn)) != 1 {
				failures = append(failures, fn.Name.Name+"_literal_zero_direct_error_return")
			}
			references := 0
			ast.Inspect(fn, func(node ast.Node) bool {
				if selector, ok := node.(*ast.SelectorExpr); ok && selector.Sel.Name == "maintenanceFenceAllows" {
					references++
				}
				return true
			})
			if references != 1 {
				failures = append(failures, fn.Name.Name+"_reference_count")
			}
		}
	}
	if totalReferences != 18 {
		failures = append(failures, "global_count")
	}
	sort.Strings(failures)
	return failures
}

func serviceAdmissionClone(sources map[string]string) map[string]string {
	result := make(map[string]string, len(sources))
	for name, source := range sources {
		result[name] = source
	}
	return result
}

func serviceAdmissionFunction(t *testing.T, filename, source, owner string) *ast.FuncDecl {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), filename, source, 0)
	if err != nil {
		t.Fatal("fixture parse failure is not a mutation kill")
	}
	for _, declaration := range file.Decls {
		if fn, ok := declaration.(*ast.FuncDecl); ok && fn.Name.Name == owner {
			return fn
		}
	}
	t.Fatal("fixture owner missing")
	return nil
}

func serviceAdmissionRequireInvariant(t *testing.T, failures []string, invariant string) {
	t.Helper()
	for _, failure := range failures {
		if strings.Contains(failure, "parse") {
			t.Fatal("fixture parse failure is not a mutation kill")
		}
	}
	if !containsAdmissionFailure(failures, invariant) {
		t.Fatalf("mutant survived named invariant %s: %v", invariant, failures)
	}
}

func TestServiceMaintenanceAdmissionSourceContract(t *testing.T) {
	sources := admissionReadSources(t)
	for _, ending := range []string{"LF", "CRLF"} {
		t.Run(ending, func(t *testing.T) {
			input := serviceAdmissionClone(sources)
			if ending == "CRLF" {
				for name, source := range input {
					input[name] = strings.ReplaceAll(strings.ReplaceAll(source, "\r\n", "\n"), "\n", "\r\n")
				}
			}
			if failures := maintenanceServiceAdmissionContract(input); len(failures) != 0 {
				t.Fatalf("service admission invariants: %v", failures)
			}
			if failures := maintenanceAdmissionOwnerInventory(input); len(failures) != 0 {
				t.Fatalf("old lifecycle inventory: %v", failures)
			}
			if failures := maintenanceAdmissionContract(input["lifecycle.go"]); len(failures) != 0 {
				t.Fatalf("old lifecycle admission invariants: %v", failures)
			}
		})
	}
}

func serviceAdmissionEditAt(source string, index int, replace func(ast.Stmt) []ast.Stmt) string {
	return admissionEditStatements(source, func(statements []ast.Stmt) []ast.Stmt {
		result := append([]ast.Stmt{}, statements[:index]...)
		result = append(result, replace(statements[index])...)
		return append(result, statements[index+1:]...)
	})
}

func serviceAdmissionInsert(source string, index int, text string) string {
	file, err := parser.ParseFile(token.NewFileSet(), "fixture.go", "package localcell; func fixture() {"+text+"}", 0)
	if err != nil {
		return "<invalid>"
	}
	added := file.Decls[0].(*ast.FuncDecl).Body.List
	return admissionEditStatements(source, func(statements []ast.Stmt) []ast.Stmt {
		result := append([]ast.Stmt{}, statements[:index]...)
		result = append(result, added...)
		return append(result, statements[index:]...)
	})
}

func serviceAdmissionMutations(owner string) []admissionSourceMutation {
	gateIndex := len(admissionStatements(maintenanceServiceAdmissionBoundaries[owner])) - 2
	gate := admissionIndent(admissionStatements(maintenanceAdmissionStatement)[0])
	replaceGate := func(text string) func(string) string {
		return func(source string) string { return strings.Replace(source, gate, text, 1) }
	}
	prepend := func(text string) func(string) string {
		return func(source string) string { return serviceAdmissionInsert(source, 0, text) }
	}
	moveGate := func(index int) func(string) string {
		return func(source string) string {
			return admissionEditStatements(source, func(statements []ast.Stmt) []ast.Stmt {
				selected := statements[gateIndex]
				statements = append(statements[:gateIndex], statements[gateIndex+1:]...)
				result := append([]ast.Stmt{}, statements[:index]...)
				result = append(result, selected)
				return append(result, statements[index:]...)
			})
		}
	}
	ownIndex, manifestIndex := 0, 3
	if owner == "PrepareService" {
		ownIndex, manifestIndex = 2, 5
	}
	if owner == "RepairService" {
		ownIndex, manifestIndex = 3, 6
	}
	if owner == "Reinstall" {
		ownIndex, manifestIndex = 0, 1
	}
	mutations := []admissionSourceMutation{
		{"DeleteGate", "reference_count", replaceGate("")},
		{"MatchPositiveGeneration", "position", replaceGate(strings.ReplaceAll(maintenanceAdmissionStatement, "Allows(0)", "Allows(7)"))},
		{"IgnoreError", "position", replaceGate("_ = c.maintenanceFenceAllows(0)")},
		{"MoveGateBeforeOwn", "position", moveGate(ownIndex)},
		{"MoveGateBeforeManifestOrBoot", "position", moveGate(manifestIndex)},
		{"MoveGateAfterFirstEffect", "position", moveGate(gateIndex + 1)},
		{"EarlyReturnAtFunctionStart", "position", prepend("if c.cfg.Timeout > 0 { return nil }")},
		{"EarlyReturnImmediatelyBeforeGate", "position", func(source string) string {
			return serviceAdmissionInsert(source, gateIndex, "if c.cfg.Timeout > 0 { return nil }")
		}},
		{"GotoPastGate", "position", replaceGate("if c.cfg.Timeout > 0 { goto admitted }; " + maintenanceAdmissionStatement + "; admitted: {}")},
		{"RebindReceiver", "position", prepend("c = other")},
		{"NamedResultDeferredSwallow", "signature", func(source string) string {
			return prepend("defer func() { result = nil }()")(strings.Replace(source, ") error {", ") (result error) {", 1))
		}},
		{"SwitchReturnBeforeGate", "position", prepend("switch { case c.cfg.Timeout > 0: return nil }")},
		{"SelectReturnBeforeGate", "position", prepend("select { default: return nil }")},
		{"InsertPreludeStatement", "position", prepend("_ = c.cfg.Timeout")},
		{"DropDefer", "continuous_hold", func(source string) string {
			return strings.Replace(source, admissionIndent(admissionStatements(`defer func() { _ = lock.Close() }()`)[0]), "", 1)
		}},
		{"EarlyCloseThenReacquire", "continuous_hold", func(source string) string {
			return serviceAdmissionInsert(source, gateIndex+2, "_ = lock.Close(); lock, err = c.own(false); if err != nil { return err }")
		}},
		{"AliasLock", "continuous_hold", func(source string) string {
			return serviceAdmissionInsert(source, gateIndex+2, "otherLock := lock; _ = otherLock")
		}},
		{"AliasReceiver", "continuous_hold", func(source string) string {
			return serviceAdmissionInsert(source, gateIndex+2, "otherCell := c; _ = otherCell")
		}},
		{"LateReceiverRebind", "continuous_hold", func(source string) string { return serviceAdmissionInsert(source, gateIndex+2, "c = other") }},
		{"LateExtraGate", "reference_count", func(source string) string {
			return serviceAdmissionInsert(source, gateIndex+2, maintenanceAdmissionStatement)
		}},
		{"LateGoAdmission", "continuous_hold", func(source string) string {
			return serviceAdmissionInsert(source, gateIndex+2, "go func() { _ = c.maintenanceFenceAllows(0) }()")
		}},
		{"LateDeferAdmission", "continuous_hold", func(source string) string {
			return serviceAdmissionInsert(source, gateIndex+2, "defer func() { _ = c.maintenanceFenceAllows(0) }()")
		}},
	}
	if owner == "PrepareService" {
		mutations = append(mutations, admissionSourceMutation{"MoveGateBeforeExistingDescriptorReturn", "position", moveGate(gateIndex - 2)})
	}
	for index := 0; index < gateIndex; index++ {
		mutations = append(mutations, admissionSourceMutation{
			fmt.Sprintf("DeleteEachPreludeStatement_%02d", index), "position",
			func(source string) string {
				return serviceAdmissionEditAt(source, index, func(ast.Stmt) []ast.Stmt { return nil })
			},
		})
	}
	return mutations
}

func TestServiceMaintenanceAdmissionSourceContractMutations(t *testing.T) {
	sources := admissionReadSources(t)
	if failures := maintenanceServiceAdmissionContract(sources); len(failures) != 0 {
		t.Fatalf("unchanged control: %v", failures)
	}
	for _, owner := range []string{"PrepareService", "reconcileServiceBoot", "RepairService", "Reinstall"} {
		filename := maintenanceServiceAdmissionFiles[owner]
		original := admissionNodeText(serviceAdmissionFunction(t, filename, sources[filename], owner))
		t.Run(owner, func(t *testing.T) {
			for _, mutation := range serviceAdmissionMutations(owner) {
				t.Run(mutation.name, func(t *testing.T) {
					changed := mutation.change(original)
					if changed == original {
						t.Fatal("mutation did not change its named site")
					}
					for _, ending := range []string{"LF", "CRLF"} {
						t.Run(ending, func(t *testing.T) {
							input := serviceAdmissionClone(sources)
							input[filename] = admissionMutateFunction(t, sources[filename], owner, func(string) string { return changed })
							if ending == "CRLF" {
								input[filename] = strings.ReplaceAll(strings.ReplaceAll(input[filename], "\r\n", "\n"), "\n", "\r\n")
							}
							serviceAdmissionRequireInvariant(t, maintenanceServiceAdmissionContract(input), owner+"_"+mutation.invariant)
						})
					}
				})
			}
		})
	}
}

func serviceAdmissionApprovedOwners(t *testing.T) map[string]string {
	t.Helper()
	sources := admissionReadSources(t)
	result := make(map[string]string)
	for owner, filename := range maintenanceServiceAdmissionFiles {
		fn := serviceAdmissionFunction(t, filename, sources[filename], owner)
		gate := admissionStatements(maintenanceAdmissionStatement)[0]
		found := false
		for index, statement := range fn.Body.List {
			if admissionNodeText(statement) != gate {
				continue
			}
			if index+1 >= len(fn.Body.List) {
				t.Fatal("approved owner has no first guarded statement")
			}
			fn.Body.List = fn.Body.List[:index+2]
			fn.Body.List = append(fn.Body.List, &ast.ReturnStmt{Results: []ast.Expr{ast.NewIdent("nil")}})
			found = true
			break
		}
		if !found {
			t.Fatal("approved owner gate missing")
		}
		result[owner] = admissionNodeText(fn)
	}
	return result
}

const serviceAdmissionFoundation = `package localcell
func (c *Cell) maintenanceFenceAllows(generation int) error { return nil }
func (c *Cell) readMaintenanceFence() error { return nil }
func (c *Cell) updateStep() error { return nil }
func (c *Cell) readUpdateJournal() error { return nil }
`

// These call the shared approval predicate directly; a different contract's
// rejection cannot conceal a weakened parameter, file, receiver or prefix pin.
func TestServiceMaintenanceAdmissionGuardFixtures(t *testing.T) {
	approved := serviceAdmissionApprovedOwners(t)
	t.Run("EightExactAdmissions", func(t *testing.T) {
		sources := admissionReadSources(t)
		if failures := updateFoundationViolations(sources); len(failures) != 0 {
			t.Fatalf("eight exact admissions refused: %v", failures)
		}
		if failures := maintenanceServiceAdmissionContract(sources); len(failures) != 0 {
			t.Fatalf("eight exact admission counts: %v", failures)
		}
	})
	for _, owner := range []string{"PrepareService", "reconcileServiceBoot", "RepairService", "Reinstall"} {
		t.Run(owner, func(t *testing.T) {
			original := approved[owner]
			filename := maintenanceServiceAdmissionFiles[owner]
			fn := serviceAdmissionFunction(t, filename, "package localcell\n"+original, owner)
			if got := maintenanceServiceAdmissionOwner(filename, fn); got != "" {
				t.Fatalf("unchanged predicate control: %s", got)
			}
			type predicateFixture struct{ name, filename, source, invariant string }
			fixtures := []predicateFixture{
				{"PredicateAtAnyIndex", filename, serviceAdmissionInsert(original, 0, "return nil"), "position"},
				{"DropFileCheck", "other.go", original, "signature"},
				{"DropReceiverCheck", filename, strings.Replace(original, "*Cell", "*Other", 1), "signature"},
				{"ValueReceiver", filename, strings.Replace(original, "*Cell", "Cell", 1), "signature"},
				{"DropUnnamedResultCheck", filename, strings.Replace(original, ") error {", ") (result error) {", 1), "signature"},
				{"ExtraResult", filename, strings.Replace(original, ") error {", ") (bool, error) {", 1), "signature"},
				{"WrongReceiverBinding", filename, strings.Replace(original, "(c *Cell)", "(other *Cell)", 1), "signature"},
			}
			wrongParameters := strings.Replace(original, owner+"(", owner+"(extra int, ", 1)
			if owner == "reconcileServiceBoot" {
				wrongParameters = strings.Replace(original, owner+"()", owner+"(extra int)", 1)
			}
			fixtures = append(fixtures, predicateFixture{"DropParameterCheck", filename, wrongParameters, "signature"})
			for _, mutation := range serviceAdmissionMutations(owner) {
				// Count pins are intentionally independent of the approval predicate.
				if mutation.invariant == "reference_count" {
					continue
				}
				invariant := mutation.invariant
				if mutation.name == "DropDefer" {
					invariant = "position"
				}
				fixtures = append(fixtures, predicateFixture{mutation.name, filename, mutation.change(original), invariant})
			}
			for _, fixture := range fixtures {
				t.Run(fixture.name, func(t *testing.T) {
					for _, ending := range []string{"LF", "CRLF"} {
						t.Run(ending, func(t *testing.T) {
							source := "package localcell\n" + fixture.source
							if ending == "CRLF" {
								source = strings.ReplaceAll(source, "\n", "\r\n")
							}
							fn := serviceAdmissionFunction(t, fixture.filename, source, owner)
							got := maintenanceServiceAdmissionOwner(fixture.filename, fn)
							if got != fixture.invariant {
								t.Fatalf("shared predicate must report %s for %s, got %q", fixture.invariant, fixture.name, got)
							}
							if len(updateOrdinaryAdmissions(fixture.filename, fn)) != 0 {
								t.Fatal("shared ordinary admission predicate approved rejected owner")
							}
						})
					}
				})
			}
			gate := admissionIndent(admissionStatements(maintenanceAdmissionStatement)[0])
			for _, fixture := range []struct{ name, replacement string }{
				{"AliasOrMethodValue", "gate := c.maintenanceFenceAllows; _ = gate"},
				{"AliasCall", "gate := c.maintenanceFenceAllows; _ = gate(0)"},
				{"Closure", "func() { _ = c.maintenanceFenceAllows(0) }()"},
				{"Deferred", "defer func() { _ = c.maintenanceFenceAllows(0) }()"},
				{"Go", "go func() { _ = c.maintenanceFenceAllows(0) }()"},
				{"Nonzero", strings.ReplaceAll(maintenanceAdmissionStatement, "Allows(0)", "Allows(7)")},
				{"HexZero", strings.ReplaceAll(maintenanceAdmissionStatement, "Allows(0)", "Allows(0x0)")},
				{"Variable", strings.ReplaceAll(maintenanceAdmissionStatement, "Allows(0)", "Allows(generation)")},
				{"WrapFenceWithPrivateSuffix", strings.ReplaceAll(maintenanceAdmissionStatement, "return err", "return fmt.Errorf(\"private-suffix: %w\", err)")},
				{"WrongHelper", strings.ReplaceAll(maintenanceAdmissionStatement, "maintenanceFenceAllows(0)", "readMaintenanceFence()")},
			} {
				t.Run(fixture.name, func(t *testing.T) {
					changed := strings.Replace(original, gate, fixture.replacement, 1)
					if changed == original {
						t.Fatal("mutation did not change named gate")
					}
					source := "package localcell\n" + changed
					serviceAdmissionFunction(t, filename, source, owner)
					failures := updateFoundationViolations(map[string]string{"update_fence.go": serviceAdmissionFoundation, filename: source})
					if len(failures) == 0 {
						t.Fatal("foundation guard admitted malformed service gate")
					}
				})
			}
			firstEffectEnd := len(admissionStatements(maintenanceServiceAdmissionBoundaries[owner]))
			for _, fixture := range []struct{ name, text, invariant string }{
				{"AllowEveryAdmittedSelector", maintenanceAdmissionStatement, owner + "_literal_zero_direct_error_return"},
				{"AddOtherUpdateHelperInsideApprovedOwner", "_ = c.updateStep()", "selects update helper updateStep"},
				{"SkipNewServiceGateInGuardViaFileExemption", "_ = c.readMaintenanceFence()", "selects update helper readMaintenanceFence"},
				{"LateAlias", "gate := c.maintenanceFenceAllows; _ = gate", "selects update helper maintenanceFenceAllows"},
			} {
				t.Run(fixture.name, func(t *testing.T) {
					changed := serviceAdmissionInsert(original, firstEffectEnd, fixture.text)
					source := "package localcell\n" + changed
					fn := serviceAdmissionFunction(t, filename, source, owner)
					if got := maintenanceServiceAdmissionOwner(filename, fn); got != "" {
						t.Fatalf("late-call control prefix changed: %s", got)
					}
					failures := updateFoundationViolations(map[string]string{"update_fence.go": serviceAdmissionFoundation, filename: source})
					if !strings.Contains(strings.Join(failures, ";"), fixture.invariant) {
						t.Fatalf("foundation guard missed %s: %v", fixture.invariant, failures)
					}
				})
			}
		})
	}
	serviceAdmissionInventoryFixtures(t, approved)
	for _, fixture := range []struct{ name, shipped, foundation, invariant string }{
		{"AddFenceToOtherProductionOwner", `func (c *Cell) Stop(ctx context.Context) error { ` + maintenanceAdmissionStatement + `; return nil }`, "", "selects update helper maintenanceFenceAllows"},
		{"PackageInitializerDirect", `var result = (&Cell{}).maintenanceFenceAllows(0)`, "", "package initializer selects update helper"},
		{"PackageInitializerClosure", `var result = func(c *Cell) error { return c.maintenanceFenceAllows(0) }`, "", "package initializer selects update helper"},
		{"FoundationInitializer", "", `var result = (&Cell{}).maintenanceFenceAllows(0)`, "update foundations run code at package initialization"},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			failures := updateFoundationViolations(map[string]string{"other.go": "package localcell\n" + fixture.shipped, "update_fence.go": serviceAdmissionFoundation + fixture.foundation})
			if !strings.Contains(strings.Join(failures, ";"), fixture.invariant) {
				t.Fatalf("foundation guard missed %s: %v", fixture.invariant, failures)
			}
		})
	}
}

func serviceAdmissionInventoryFixtures(t *testing.T, approved map[string]string) {
	t.Helper()
	control := admissionReadSources(t)
	if failures := maintenanceServiceAdmissionContract(control); len(failures) != 0 {
		t.Fatalf("inventory unchanged control: %v", failures)
	}
	for _, owner := range []string{"PrepareService", "reconcileServiceBoot", "RepairService", "Reinstall"} {
		t.Run(owner+"_Inventory", func(t *testing.T) {
			filename := maintenanceServiceAdmissionFiles[owner]
			for _, fixture := range []struct {
				name, target, prefix, invariant string
				move                            bool
			}{
				{"BuildConstrainedDuplicateOwnerPlatformSuffix", "service_duplicate_windows.go", "", owner + "_owner_count", false},
				{"BuildConstrainedDuplicateOwnerExplicitTag", "service_duplicate.go", "//go:build serviceproof\n\n", owner + "_owner_count", false},
				{"SoleOwnerWrongFile", "other.go", "", owner + "_signature", true},
				{"CanonicalOwnerBuildTag", filename, "//go:build serviceproof\n\n", filename + "_signature", false},
				{"CanonicalOwnerLegacyBuildTag", filename, "// +build serviceproof\n\n", filename + "_signature", false},
			} {
				t.Run(fixture.name, func(t *testing.T) {
					for _, ending := range []string{"LF", "CRLF"} {
						t.Run(ending, func(t *testing.T) {
							sources := serviceAdmissionClone(control)
							sources[fixture.target] = fixture.prefix + "package localcell\n" + approved[owner]
							if fixture.move {
								sources[filename] = "package localcell\n"
							}
							if ending == "CRLF" {
								for name, source := range sources {
									sources[name] = strings.ReplaceAll(source, "\n", "\r\n")
								}
							}
							serviceAdmissionRequireInvariant(t, maintenanceServiceAdmissionOwnerInventory(sources), fixture.invariant)
						})
					}
				})
			}
			for _, witness := range []struct{ name, invariant string }{
				{"ReferenceCount", filename + "_reference_count"},
				{"OwnerReferenceCount", owner + "_reference_count"},
				{"GlobalCount", "global_count"},
			} {
				t.Run(witness.name, func(t *testing.T) {
					sources := serviceAdmissionClone(control)
					sources[filename] = admissionMutateFunction(t, sources[filename], owner, func(source string) string {
						return serviceAdmissionInsert(source, len(admissionStatements(maintenanceServiceAdmissionBoundaries[owner])), maintenanceAdmissionStatement)
					})
					// The complete original prefix and first effect are still approved. Each
					// count assertion stands alone even if another contract also rejects it.
					fn := serviceAdmissionFunction(t, filename, sources[filename], owner)
					if got := maintenanceServiceAdmissionOwner(filename, fn); got != "" {
						t.Fatalf("late gate changed original prefix: %s", got)
					}
					serviceAdmissionRequireInvariant(t, maintenanceServiceAdmissionContract(sources), witness.invariant)
				})
			}
		})
	}
}
