package localcell

import (
	"go/ast"
	"go/parser"
	"go/token"
	"sort"
	"strings"
	"testing"
)

var maintenanceRemovalFiles = map[string]string{
	"systemdUserAdapter":      "service_systemd_adapter.go",
	"launchdAgentAdapter":     "service_launchd.go",
	"windowsLogonTaskAdapter": "service_windows_task.go",
}

func removalCallName(call *ast.CallExpr) string {
	switch fun := call.Fun.(type) {
	case *ast.Ident:
		return fun.Name
	case *ast.SelectorExpr:
		return fun.Sel.Name
	}
	return ""
}

func removalDestructiveCall(call *ast.CallExpr) bool {
	switch removalCallName(call) {
	case "Remove", "removeFile", "removePrivateFile", "removePlistIdentity", "bootout", "syncDirectory":
		return true
	case "command":
		for _, argument := range call.Args {
			switch admissionNodeText(argument) {
			case `"disable"`, `"bootout"`, `"/Delete"`, `"daemon-reload"`:
				return true
			}
		}
	}
	return false
}

// This owner proof is shared with the global fence inventory. Bind the direct
// fence to the exact Cell returned by the operation-lock factory, and permit
// each release only once as a deferred call after its successful acquisition.
func maintenanceRemovalAdmissionOwner(filename string, fn *ast.FuncDecl) string {
	receiver := updateReceiverName(fn)
	if maintenanceRemovalFiles[receiver] != filename || fn.Name.Name != "Remove" || fn.Body == nil || admissionNodeText(fn.Type) != "func(ctx context.Context, d ServiceIdentity) error" {
		return "signature"
	}
	lockName := "serviceAdapterRemovalAdmission"
	if receiver == "windowsLogonTaskAdapter" {
		lockName = "mutationLock"
	}
	var cell, unlock, hostUnlock *ast.Ident
	var operationPosition, hostPosition, fencePosition, drainPosition token.Pos
	lockCalls, hostCalls, fenceCalls, drainCalls, exitCalls := 0, 0, 0, 0, 0
	for _, statement := range fn.Body.List {
		assignment, ok := statement.(*ast.AssignStmt)
		if !ok || assignment.Tok != token.DEFINE || len(assignment.Rhs) != 1 {
			continue
		}
		call, ok := assignment.Rhs[0].(*ast.CallExpr)
		if !ok {
			continue
		}
		switch removalCallName(call) {
		case lockName:
			if len(assignment.Lhs) < 3 {
				return "binding"
			}
			cell, _ = assignment.Lhs[0].(*ast.Ident)
			unlock, _ = assignment.Lhs[len(assignment.Lhs)-2].(*ast.Ident)
			operationPosition = statement.Pos()
		case "serviceAdapterRemovalHostLock":
			if len(assignment.Lhs) != 2 {
				return "binding"
			}
			hostUnlock, _ = assignment.Lhs[0].(*ast.Ident)
			hostPosition = statement.Pos()
		}
	}
	if cell == nil || cell.Obj == nil || unlock == nil || unlock.Obj == nil || hostUnlock == nil || hostUnlock.Obj == nil {
		return "binding"
	}
	allowedRelease := map[*ast.CallExpr]bool{}
	defers := 0
	ast.Inspect(fn.Body, func(node ast.Node) bool {
		if deferred, ok := node.(*ast.DeferStmt); ok {
			allowedRelease[deferred.Call] = true
			defers++
		}
		return true
	})
	invalid := false
	operationReleases, hostReleases := 0, 0
	cellReferences, operationReferences, hostReferences := 0, 0, 0
	var operationReleasePosition, hostReleasePosition token.Pos
	ast.Inspect(fn.Body, func(node ast.Node) bool {
		switch node := node.(type) {
		case *ast.Ident:
			if node.Obj == cell.Obj {
				cellReferences++
			}
			if node.Obj == unlock.Obj {
				operationReferences++
			}
			if node.Obj == hostUnlock.Obj {
				hostReferences++
			}
		case *ast.GoStmt:
			invalid = true
		case *ast.AssignStmt:
			for _, left := range node.Lhs {
				if id, ok := left.(*ast.Ident); ok && (id.Obj == cell.Obj || id.Obj == unlock.Obj || id.Obj == hostUnlock.Obj) && node.Tok != token.DEFINE {
					invalid = true
				}
			}
		case *ast.CallExpr:
			name := removalCallName(node)
			switch name {
			case lockName:
				lockCalls++
			case "serviceAdapterRemovalHostLock":
				hostCalls++
				if len(node.Args) != 1 {
					invalid = true
				} else if id, ok := node.Args[0].(*ast.Ident); !ok || id.Obj != cell.Obj {
					invalid = true
				}
			case "drain":
				drainCalls++
				drainPosition = node.Pos()
			case "exitServiceHostForRemoval":
				exitCalls++
				if node.Pos() >= operationPosition {
					invalid = true
				}
			case "maintenanceFenceAllows":
				fenceCalls++
				fencePosition = node.Pos()
				selector, ok := node.Fun.(*ast.SelectorExpr)
				if !ok || admissionNodeText(node) != "cell.maintenanceFenceAllows(0)" {
					invalid = true
				} else if id, ok := selector.X.(*ast.Ident); !ok || id.Obj != cell.Obj {
					invalid = true
				}
			}
			if id, ok := node.Fun.(*ast.Ident); ok {
				if id.Obj == unlock.Obj {
					operationReleases++
					operationReleasePosition = node.Pos()
					invalid = invalid || !allowedRelease[node]
				}
				if id.Obj == hostUnlock.Obj {
					hostReleases++
					hostReleasePosition = node.Pos()
					invalid = invalid || !allowedRelease[node]
				}
			}
		}
		return true
	})
	if invalid || cellReferences != 3 || operationReferences != 2 || hostReferences != 2 || operationReleasePosition >= hostReleasePosition || lockCalls != 1 || hostCalls != 1 || fenceCalls != 1 || drainCalls != 1 || exitCalls != 1 || operationReleases != 1 || hostReleases != 1 || defers != 2 {
		return "continuous_hold"
	}
	if drainPosition >= operationPosition || operationPosition >= hostPosition || hostPosition >= fencePosition {
		return "order"
	}
	gateStatements := 0
	ast.Inspect(fn.Body, func(node ast.Node) bool {
		if branch, ok := node.(*ast.IfStmt); ok && admissionNodeText(branch) == admissionStatements(maintenanceAdapterAdmissionStatement)[0] {
			gateStatements++
		}
		if call, ok := node.(*ast.CallExpr); ok && removalDestructiveCall(call) && call.Pos() < fencePosition {
			invalid = true
		}
		return true
	})
	if invalid || gateStatements != 1 {
		return "effect_order"
	}
	return ""
}

func maintenanceRemovalSourceContract(sources map[string]string) []string {
	var failures []string
	owners := map[string]int{}
	for filename, source := range sources {
		if strings.HasSuffix(filename, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), filename, strings.ReplaceAll(source, "\r\n", "\n"), 0)
		if err != nil {
			failures = append(failures, "parse")
			continue
		}
		for _, declaration := range file.Decls {
			fn, ok := declaration.(*ast.FuncDecl)
			if !ok {
				continue
			}
			if maintenanceRemovalFiles[updateReceiverName(fn)] == filename && fn.Name.Name == "Remove" {
				owners[filename]++
				if invariant := maintenanceRemovalAdmissionOwner(filename, fn); invariant != "" {
					failures = append(failures, filename+"_"+invariant)
				}
			}
			if fn.Name.Name == "serviceAdapterAdmission" || fn.Name.Name == "serviceAdapterRemovalAdmission" {
				step := "apply"
				if fn.Name.Name == "serviceAdapterRemovalAdmission" {
					step = "remove"
				}
				want := admissionStatements(`return serviceAdapterAdmissionForStep(owner, d, artifactDir, "` + step + `")`)
				if filename != "service_adapter_admission.go" || fn.Body == nil || len(fn.Body.List) != 1 || admissionNodeText(fn.Body.List[0]) != want[0] {
					failures = append(failures, "admission_step_binding")
				}
			}
			if fn.Name.Name == "serviceAdapterRemovalHostLock" {
				owners["host"]++
				text := admissionNodeText(fn)
				if !strings.Contains(text, "servicePrivateRecord(") || !strings.Contains(text, `openLockFile(c.path(".service-host.lock"), true)`) || !strings.Contains(text, "lockFile(") {
					failures = append(failures, "host_acquisition")
				}
				ast.Inspect(fn.Body, func(node ast.Node) bool {
					if call, ok := node.(*ast.CallExpr); ok {
						switch removalCallName(call) {
						case "Write", "WriteFile", "WriteString", "Rename", "Remove", "RemoveAll", "Truncate", "publishServiceLease", "Sleep", "NewTimer":
							failures = append(failures, "host_side_effect")
						}
					}
					if _, ok := node.(*ast.ForStmt); ok {
						failures = append(failures, "host_wait")
					}
					return true
				})
			}
		}
		ast.Inspect(file, func(node ast.Node) bool {
			switch node := node.(type) {
			case *ast.AssignStmt:
				for _, left := range node.Lhs {
					if selector, ok := left.(*ast.SelectorExpr); ok && selector.Sel.Name == "drain" {
						failures = append(failures, "production_drain_assignment")
					}
				}
			case *ast.KeyValueExpr:
				if key, ok := node.Key.(*ast.Ident); ok && key.Name == "drain" {
					failures = append(failures, "production_drain_initializer")
				}
			}
			return true
		})
	}
	for _, filename := range maintenanceRemovalFiles {
		if owners[filename] != 1 {
			failures = append(failures, filename+"_owner_count")
		}
	}
	if owners["host"] != 1 {
		failures = append(failures, "host_owner_count")
	}
	sort.Strings(failures)
	return failures
}

func TestServiceAdapterRemovalSourceContract(t *testing.T) {
	sources := admissionReadSources(t)
	for _, ending := range []string{"LF", "CRLF"} {
		t.Run(ending, func(t *testing.T) {
			input := serviceAdmissionClone(sources)
			if ending == "CRLF" {
				for filename, source := range input {
					input[filename] = strings.ReplaceAll(strings.ReplaceAll(source, "\r\n", "\n"), "\n", "\r\n")
				}
			}
			if failures := maintenanceRemovalSourceContract(input); len(failures) != 0 {
				t.Fatalf("Removal source contract failed %v", failures)
			}
		})
	}
}
