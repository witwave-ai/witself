package localcell

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// Windows does not expose a process-group query. Pin the creation expression
// on every host as well as exercising actual Ctrl-Break isolation on Windows.
func TestWindowsLaunchRequiresSuspensionGroupAndAtomicJob(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "launch_windows.go", nil, 0)
	if err != nil {
		t.Fatal("cannot inspect Windows launch contract")
	}
	flags := map[string]bool{}
	atomicJob := false
	creationUsesFlags := false
	ast.Inspect(file, func(node ast.Node) bool {
		switch node := node.(type) {
		case *ast.AssignStmt:
			if len(node.Lhs) != 1 || len(node.Rhs) != 1 {
				break
			}
			name, ok := node.Lhs[0].(*ast.Ident)
			if !ok || name.Name != "flags" {
				break
			}
			ast.Inspect(node.Rhs[0], func(child ast.Node) bool {
				if selector, ok := child.(*ast.SelectorExpr); ok {
					flags[selector.Sel.Name] = true
				}
				return true
			})
		case *ast.CallExpr:
			selector, ok := node.Fun.(*ast.SelectorExpr)
			if !ok {
				break
			}
			if selector.Sel.Name == "Update" && len(node.Args) > 0 {
				name, ok := node.Args[0].(*ast.Ident)
				atomicJob = atomicJob || (ok && name.Name == "procThreadAttributeJobList")
			}
			if selector.Sel.Name == "CreateProcess" && len(node.Args) > 5 {
				name, ok := node.Args[5].(*ast.Ident)
				creationUsesFlags = ok && name.Name == "flags"
			}
		}
		return true
	})
	if !atomicJob || !creationUsesFlags {
		t.Fatal("Windows creation lost atomic job assignment or explicit flags")
	}
	for _, required := range []string{"CREATE_SUSPENDED", "CREATE_NEW_PROCESS_GROUP", "CREATE_UNICODE_ENVIRONMENT", "EXTENDED_STARTUPINFO_PRESENT"} {
		if !flags[required] {
			t.Errorf("Windows launch flags omit %s", required)
		}
	}
}
