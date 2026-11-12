package localcell

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"sort"
	"strings"
	"testing"
)

const maintenanceAdapterAdmissionStatement = `if err := cell.maintenanceFenceAllows(0); err != nil { return err }`

// The adapter has its own owner proof. Cell owners retain their c receiver and
// their existing statement, so this exception cannot admit a renamed Cell owner.
var maintenanceAdapterAdmissionBoundaries = map[string]string{
	"Apply": `d := plan.Spec
if plan.RequiredPrivilege != "user" { return windowsTaskError(ErrServiceOwnershipConflict, "create", 0, "not_user") }
if err := a.preflight(ctx, d); err != nil { return err }
cell, unlock, err := a.mutationLock(d)
if err != nil { return err }
defer unlock()
observation, err := a.Inspect(ctx, d)
if err != nil { return err }
current, err := a.current(d)
if err != nil || !reflect.DeepEqual(windowsTaskImmutable(current), windowsTaskImmutable(d)) { return windowsTaskError(ErrServiceOwnershipConflict, "create", 0, "stale_plan") }
` + maintenanceAdapterAdmissionStatement + `
if !reflect.DeepEqual(current, d) || d.DesiredState != "running" || d.Faulted { return windowsTaskError(ErrServiceOwnershipConflict, "create", 0, "stale_plan") }
if observation.Owned { return nil }
records, err := a.records(d)`,
	"Start": `if err := a.preflight(ctx, d); err != nil { return err }
cell, unlock, err := a.mutationLock(d)
if err != nil { return err }
defer unlock()
observation, err := a.Inspect(ctx, d)
if err != nil { return err }
current, err := a.current(d)
if err != nil || current.DesiredState != "running" || current.Faulted { return windowsTaskError(ErrServiceDesiredState, "inspect:host_owner", 0, "stopped") }
if !observation.Owned { return windowsTaskError(ErrServiceOwnershipConflict, "inspect:host_owner", 0, "not_owned") }
` + maintenanceAdapterAdmissionStatement + `
name, _ := WindowsLogonTaskName(d)
result, err := a.command(ctx, "/Run", "/TN", name)`,
}

func maintenanceAdapterCellBinding(fn *ast.FuncDecl) *ast.Ident {
	if fn.Body == nil {
		return nil
	}
	for _, statement := range fn.Body.List {
		if admissionNodeText(statement) == "cell, unlock, err := a.mutationLock(d)" {
			return statement.(*ast.AssignStmt).Lhs[0].(*ast.Ident)
		}
	}
	return nil
}

func maintenanceAdapterAdmissionOwner(filename string, fn *ast.FuncDecl) string {
	boundary, known := maintenanceAdapterAdmissionBoundaries[fn.Name.Name]
	if filename != "service_windows_task.go" || !known || fn.Recv == nil || len(fn.Recv.List) != 1 || fn.Type.TypeParams != nil {
		return "signature"
	}
	receiver := fn.Recv.List[0]
	if len(receiver.Names) != 1 || receiver.Names[0].Name != "a" || receiver.Names[0].Obj == nil || admissionNodeText(receiver.Type) != "*windowsLogonTaskAdapter" {
		return "signature"
	}
	parameters := "func(ctx context.Context, d ServiceIdentity) error"
	if fn.Name.Name == "Apply" {
		parameters = "func(ctx context.Context, plan ServicePlan) error"
	}
	if admissionNodeText(fn.Type) != parameters {
		return "signature"
	}
	want := admissionStatements(boundary)
	if fn.Body == nil || len(want) == 0 || len(fn.Body.List) < len(want) {
		return "position"
	}
	for index, statement := range want {
		if admissionNodeText(fn.Body.List[index]) != statement {
			return "position"
		}
	}
	cell := maintenanceAdapterCellBinding(fn)
	if cell == nil || cell.Obj == nil {
		return "binding"
	}
	cellReferences, unlockReferences, lockCalls, fenceReferences, defers := 0, 0, 0, 0, 0
	valid := true
	ast.Inspect(fn.Body, func(node ast.Node) bool {
		switch node := node.(type) {
		case *ast.Ident:
			if node.Name == "cell" {
				cellReferences++
				valid = valid && node.Obj == cell.Obj
			}
			if node.Name == "unlock" {
				unlockReferences++
			}
		case *ast.SelectorExpr:
			switch node.Sel.Name {
			case "mutationLock":
				lockCalls++
			case "maintenanceFenceAllows":
				fenceReferences++
			case "own", "ownExisting":
				valid = false
			}
		case *ast.CallExpr:
			if id, ok := node.Fun.(*ast.Ident); ok && id.Name == "New" {
				valid = false
			}
		case *ast.DeferStmt:
			defers++
		case *ast.GoStmt:
			valid = false
		}
		return true
	})
	if !valid || cellReferences != 2 || unlockReferences != 2 || lockCalls != 1 || fenceReferences != 1 || defers != 1 {
		return "continuous_hold"
	}
	return ""
}

// Pin the complete small lock factory, including the two BASE error identities.
// This proves the returned Cell is exactly the object that took the lock.
const maintenanceAdapterLockBody = `if a.lock != nil { return a.lock(d) }
cell, err := New(Config{Root: d.Root, Timeout: 60 * time.Second})
if err != nil { return nil, nil, windowsTaskError(ErrServiceOwnershipConflict, "inspect:host_owner", 0, "unavailable") }
lock, err := cell.own(false)
if err != nil { return nil, nil, windowsTaskError(ErrServiceOwnershipConflict, "inspect:host_owner", 0, "unavailable") }
return cell, func() { _ = lock.Close() }, nil`

func maintenanceAdapterAdmissionContract(source string) []string {
	file, err := admissionReadOnlyParse("service_windows_task.go", strings.ReplaceAll(source, "\r\n", "\n"))
	if err != nil {
		return []string{"adapter_parse"}
	}
	var failures []string
	owners := map[string]int{}
	for _, declaration := range file.Decls {
		fn, ok := declaration.(*ast.FuncDecl)
		if !ok {
			ast.Inspect(declaration, func(node ast.Node) bool {
				if selector, ok := node.(*ast.SelectorExpr); ok && selector.Sel.Name == "maintenanceFenceAllows" {
					failures = append(failures, "adapter_initializer_reference")
				}
				return true
			})
			continue
		}
		references := 0
		ast.Inspect(fn, func(node ast.Node) bool {
			if selector, ok := node.(*ast.SelectorExpr); ok && selector.Sel.Name == "maintenanceFenceAllows" {
				references++
			}
			return true
		})
		_, enabling := maintenanceAdapterAdmissionBoundaries[fn.Name.Name]
		if enabling {
			owners[fn.Name.Name]++
			if invariant := maintenanceAdapterAdmissionOwner("service_windows_task.go", fn); invariant != "" {
				failures = append(failures, fn.Name.Name+"_"+invariant)
			}
			if references != 1 || len(updateOrdinaryAdmissions("service_windows_task.go", fn)) != 1 {
				failures = append(failures, fn.Name.Name+"_reference_count")
			}
		} else if fn.Name.Name == "Remove" {
			if invariant := maintenanceRemovalAdmissionOwner("service_windows_task.go", fn); invariant != "" {
				failures = append(failures, "Remove_"+invariant)
			}
		} else if references != 0 {
			failures = append(failures, fn.Name.Name+"_unexpected_reference")
		}
		if fn.Name.Name == "mutationLock" {
			owners[fn.Name.Name]++
			want := admissionStatements(maintenanceAdapterLockBody)
			if fn.Body == nil || len(fn.Body.List) != len(want) || admissionNodeText(fn.Type) != "func(d ServiceDescriptor) (*Cell, func(), error)" {
				failures = append(failures, "mutationLock_binding")
				continue
			}
			for index, statement := range want {
				if admissionNodeText(fn.Body.List[index]) != statement {
					failures = append(failures, "mutationLock_binding")
					break
				}
			}
		}
		if fn.Name.Name == "Remove" {
			owners[fn.Name.Name]++
			calls, bindings := 0, 0
			ast.Inspect(fn, func(node ast.Node) bool {
				if selector, ok := node.(*ast.SelectorExpr); ok && selector.Sel.Name == "mutationLock" {
					calls++
				}
				if statement, ok := node.(*ast.AssignStmt); ok && admissionNodeText(statement) == "cell, unlock, err := a.mutationLock(d)" {
					bindings++
				}
				return true
			})
			if calls != 1 || bindings != 1 {
				failures = append(failures, "Remove_binding")
			}
		}
	}
	for _, owner := range []string{"Apply", "Start", "mutationLock", "Remove"} {
		if owners[owner] != 1 {
			failures = append(failures, owner+"_owner_count")
		}
	}
	sort.Strings(failures)
	return failures
}

// Independent parse-valid controls deliberately omit package type declarations.
// A refusal must arise from a source invariant, never from the Go type checker.
const maintenanceAdapterFixture = `package localcell
func (a *windowsLogonTaskAdapter) Apply(ctx context.Context, plan ServicePlan) error {
 d := plan.Spec
 if plan.RequiredPrivilege != "user" { return windowsTaskError(ErrServiceOwnershipConflict, "create", 0, "not_user") }
 if err := a.preflight(ctx, d); err != nil { return err }
 cell, unlock, err := a.mutationLock(d)
 if err != nil { return err }
 defer unlock()
 observation, err := a.Inspect(ctx, d)
 if err != nil { return err }
 current, err := a.current(d)
 if err != nil || !reflect.DeepEqual(windowsTaskImmutable(current), windowsTaskImmutable(d)) { return windowsTaskError(ErrServiceOwnershipConflict, "create", 0, "stale_plan") }
 if err := cell.maintenanceFenceAllows(0); err != nil { return err }
 if !reflect.DeepEqual(current, d) || d.DesiredState != "running" || d.Faulted { return windowsTaskError(ErrServiceOwnershipConflict, "create", 0, "stale_plan") }
 if observation.Owned { return nil }
 records, err := a.records(d)
 return nil
}
func (a *windowsLogonTaskAdapter) Start(ctx context.Context, d ServiceIdentity) error {
 if err := a.preflight(ctx, d); err != nil { return err }
 cell, unlock, err := a.mutationLock(d)
 if err != nil { return err }
 defer unlock()
 observation, err := a.Inspect(ctx, d)
 if err != nil { return err }
 current, err := a.current(d)
 if err != nil || current.DesiredState != "running" || current.Faulted { return windowsTaskError(ErrServiceDesiredState, "inspect:host_owner", 0, "stopped") }
 if !observation.Owned { return windowsTaskError(ErrServiceOwnershipConflict, "inspect:host_owner", 0, "not_owned") }
 if err := cell.maintenanceFenceAllows(0); err != nil { return err }
 name, _ := WindowsLogonTaskName(d)
 result, err := a.command(ctx, "/Run", "/TN", name)
 return nil
}
func (a *windowsLogonTaskAdapter) mutationLock(d ServiceDescriptor) (*Cell, func(), error) {
 if a.lock != nil { return a.lock(d) }
 cell, err := New(Config{Root: d.Root, Timeout: 60 * time.Second})
 if err != nil { return nil, nil, windowsTaskError(ErrServiceOwnershipConflict, "inspect:host_owner", 0, "unavailable") }
 lock, err := cell.own(false)
 if err != nil { return nil, nil, windowsTaskError(ErrServiceOwnershipConflict, "inspect:host_owner", 0, "unavailable") }
 return cell, func() { _ = lock.Close() }, nil
}
func (a *windowsLogonTaskAdapter) Remove(ctx context.Context, d ServiceIdentity) error {
 if a.drain != nil { if err := a.drain(ctx, d); err != nil { return err } } else { cell, err := a.cellForRemoval(d); if err != nil || cell.exitServiceHostForRemoval(ctx) != nil { return err } }
 cell, unlock, err := a.mutationLock(d)
 if err != nil { return err }
 defer unlock()
 releaseHost, err := serviceAdapterRemovalHostLock(cell)
 if err != nil { return err }
 defer releaseHost()
 if err := cell.maintenanceFenceAllows(0); err != nil { return err }
 return nil
}
`

func TestWindowsTaskMaintenanceAdmissionSourceContract(t *testing.T) {
	raw, err := os.ReadFile("service_windows_task.go")
	if err != nil {
		t.Fatal("Cannot read adapter source")
	}
	for _, ending := range []string{"LF", "CRLF"} {
		t.Run(ending, func(t *testing.T) {
			source := strings.ReplaceAll(string(raw), "\r\n", "\n")
			if ending == "CRLF" {
				source = strings.ReplaceAll(source, "\n", "\r\n")
			}
			if failures := maintenanceAdapterAdmissionContract(source); len(failures) != 0 {
				t.Fatalf("Adapter source invariants failed %v", failures)
			}
		})
	}
	gate := maintenanceAdapterAdmissionStatement
	moveGate := func(anchor string, after bool) func(string) string {
		return func(body string) string {
			body = strings.Replace(body, gate, "", 1)
			replacement := gate + "\n" + anchor
			if after {
				replacement = anchor + "\n" + gate
			}
			return strings.Replace(body, anchor, replacement, 1)
		}
	}
	fixtures := []struct {
		name, owner string
		change      func(string) string
	}{
		{"statement_missing", "Apply", func(s string) string { return strings.Replace(s, gate, "", 1) }},
		{"literal_one", "Apply", func(s string) string { return strings.Replace(s, "Allows(0)", "Allows(1)", 1) }},
		{"nonliteral_generation", "Apply", func(s string) string { return strings.Replace(s, "Allows(0)", "Allows(generation)", 1) }},
		{"cell_owner_receiver", "Apply", func(s string) string { return strings.Replace(s, "cell.maintenance", "c.maintenance", 1) }},
		{"error_discarded", "Apply", func(s string) string { return strings.Replace(s, gate, "_ = cell.maintenanceFenceAllows(0)", 1) }},
		{"before_immutable", "Apply", moveGate("if err != nil || !reflect.DeepEqual(windowsTaskImmutable(current), windowsTaskImmutable(d))", false)},
		{"after_owned", "Apply", moveGate("if observation.Owned { return nil }", true)},
		{"after_run", "Start", moveGate(`result, err := a.command(ctx, "/Run", "/TN", name)`, true)},
		{"second_lock", "Apply", func(s string) string {
			return strings.Replace(s, "defer unlock()", "defer unlock()\n_, _, _ = a.mutationLock(d)", 1)
		}},
		{"cell_aliased", "Apply", func(s string) string {
			return strings.Replace(s, "defer unlock()", "defer unlock()\nalias := cell\n_ = alias", 1)
		}},
		{"cell_reassigned", "Apply", func(s string) string { return strings.Replace(s, "defer unlock()", "defer unlock()\ncell = other", 1) }},
		{"missing_defer", "Apply", func(s string) string { return strings.Replace(s, "defer unlock()", "", 1) }},
		{"early_unlock", "Start", func(s string) string { return strings.Replace(s, "name, _ :=", "unlock()\nname, _ :=", 1) }},
		{"remove_reference", "Remove", func(s string) string { return strings.Replace(s, "defer unlock()", "defer unlock()\n"+gate, 1) }},
		{"different_lock_cell", "mutationLock", func(s string) string { return strings.Replace(s, "return cell, func()", "return other, func()", 1) }},
		{"same_root_second_cell", "mutationLock", func(s string) string {
			return strings.Replace(s, "return cell, func()", "other, _ := New(Config{Root: d.Root})\nreturn other, func()", 1)
		}},
		{"method_value", "Apply", func(s string) string {
			return strings.Replace(s, gate, "check := cell.maintenanceFenceAllows\n_ = check", 1)
		}},
		{"helper_wrapper", "Apply", func(s string) string {
			return strings.Replace(s, gate, "if err := gate(cell); err != nil { return err }", 1)
		}},
	}
	if failures := maintenanceAdapterAdmissionContract(maintenanceAdapterFixture); len(failures) != 0 {
		t.Fatalf("Adapter fixture control failed %v", failures)
	}
	for _, fixture := range fixtures {
		t.Run("fixture/"+fixture.name, func(t *testing.T) {
			changed := admissionMutateFunction(t, maintenanceAdapterFixture, fixture.owner, fixture.change)
			for _, ending := range []string{"LF", "CRLF"} {
				t.Run(ending, func(t *testing.T) {
					source := changed
					if ending == "CRLF" {
						source = strings.ReplaceAll(source, "\n", "\r\n")
					}
					if _, err := parser.ParseFile(token.NewFileSet(), "fixture.go", source, 0); err != nil {
						t.Fatal("Adapter fixture parse failure is not a refusal")
					}
					if failures := maintenanceAdapterAdmissionContract(source); len(failures) == 0 {
						t.Fatal("Adapter fixture escaped the source contract")
					}
					if fixture.owner == "Apply" || fixture.owner == "Start" {
						fn := serviceAdmissionFunction(t, "service_windows_task.go", source, fixture.owner)
						if len(updateOrdinaryAdmissions("service_windows_task.go", fn)) != 0 {
							t.Fatal("Adapter fixture escaped the foundation admission guard")
						}
					}
				})
			}
		})
	}
}
