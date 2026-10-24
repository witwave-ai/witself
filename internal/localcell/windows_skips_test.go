package localcell

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// Inspect the whole source tree, including helper functions taking *testing.T
// and platform-specific files. Native CI lists each executed skip by test name.
// This source-only check also runs on Windows; it needs no private directories.
func windowsSkipOwnerViolations(file *ast.File, windowsOnly bool) []token.Pos {
	// PW1 privacy and PW2 launch/environment fixtures are implemented. Only
	// PW3 lifecycle fixtures may defer Windows execution, apart from the exact
	// native process-DACL witness that enabled SeDebugPrivilege invalidates.
	owner := regexp.MustCompile(`^LC-S2-PW3: \S`)
	const debugPrivilegeSkip = "LC-S2-PW2: real process DACL denial unavailable with enabled SeDebugPrivilege"
	var violations []token.Pos
	check := func(node ast.Node) {
		ast.Inspect(node, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			method, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || (method.Sel.Name != "Skip" && method.Sel.Name != "Skipf" && method.Sel.Name != "SkipNow") {
				return true
			}
			var message string
			if len(call.Args) > 0 {
				if literal, ok := call.Args[0].(*ast.BasicLit); ok && literal.Kind == token.STRING {
					message, _ = strconv.Unquote(literal.Value)
				}
			}
			if !owner.MatchString(message) && (!windowsOnly || message != debugPrivilegeSkip) {
				violations = append(violations, call.Pos())
			}
			return true
		})
	}
	if windowsOnly {
		check(file)
		return violations
	}
	ast.Inspect(file, func(node ast.Node) bool {
		branch, ok := node.(*ast.IfStmt)
		if !ok {
			return true
		}
		// Inspect every branch, including those in non-Test fixture helpers.
		op, found := windowsGOOSCondition(branch.Cond)
		if !found {
			return true
		}
		if op == token.EQL {
			check(branch.Body)
		} else if branch.Else != nil {
			check(branch.Else)
		}
		return true
	})
	return violations
}

func TestWindowsSkipsNameOwningSlice(t *testing.T) {
	positions := token.NewFileSet()
	for _, directory := range []string{".", "../runtimeconfig", "../tools/localcellharness"} {
		files, err := filepath.Glob(filepath.Join(directory, "*_test.go"))
		if err != nil || len(files) == 0 {
			t.Fatalf("cannot enumerate test sources in %s", directory)
		}
		for _, path := range files {
			file, err := parser.ParseFile(positions, path, nil, parser.ParseComments)
			if err != nil {
				t.Fatalf("cannot parse test source %s", path)
			}
			for _, position := range windowsSkipOwnerViolations(file, strings.HasSuffix(path, "_windows_test.go")) {
				t.Errorf("%s: Windows skip must name LC-S2-PW3 and a reason; only the exact Windows-only LC-S2-PW2 enabled SeDebugPrivilege DACL reason is excepted", positions.Position(position))
			}
		}
	}
}

func TestWindowsSkipOwnerGuardCoversHelpers(t *testing.T) {
	for _, fixture := range []struct {
		name        string
		body        string
		windowsOnly bool
		want        int
	}{
		{"owned-helper", `if runtime.GOOS == "windows" { t.Skip("LC-S2-PW3: lifecycle pending") }`, false, 0},
		{"retired-launch-helper", `if runtime.GOOS == "windows" { t.Skip("LC-S2-PW2: launch pending") }`, false, 1},
		{"retired-windows-launch-helper", `t.Skip("LC-S2-PW2: launch pending")`, true, 1},
		{"enabled-debug-privilege-windows-helper", `t.Skip("LC-S2-PW2: real process DACL denial unavailable with enabled SeDebugPrivilege")`, true, 0},
		{"wrong-debug-privilege-reason", `t.Skip("LC-S2-PW2: real process DACL denial unavailable with disabled SeDebugPrivilege")`, true, 1},
		{"anonymous-debug-privilege-reason", `t.Skip("real process DACL denial unavailable with enabled SeDebugPrivilege")`, true, 1},
		{"debug-privilege-shared-source", `if runtime.GOOS == "windows" { t.Skip("LC-S2-PW2: real process DACL denial unavailable with enabled SeDebugPrivilege") }`, false, 1},
		{"retired-privacy-helper", `if runtime.GOOS == "windows" { t.Skip("LC-S2-PW1: privacy pending") }`, false, 1},
		{"anonymous-helper", `if runtime.GOOS == "windows" { t.Skip("privacy pending") }`, false, 1},
		{"empty-reason", `if runtime.GOOS == "windows" { t.Skip("LC-S2-PW3: ") }`, false, 1},
		{"anonymous-skipf", `if runtime.GOOS == "windows" { t.Skipf("pending: %s", "fixture") }`, false, 1},
		{"anonymous-skipnow", `if runtime.GOOS == "windows" { t.SkipNow() }`, false, 1},
		{"inequality-else", `if runtime.GOOS != "windows" {} else { t.Skip("pending") }`, false, 1},
		{"nested-condition", `if true && (runtime.GOOS == "windows") { t.Skip("pending") }`, false, 1},
		{"windows-only-helper", `t.Skip("pending")`, true, 1},
		{"owned-windows-helper", `t.Skip("LC-S2-PW3: fixture pending")`, true, 0},
		{"other-platform", `if runtime.GOOS == "plan9" { t.Skip("pending") }`, false, 0},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			file, err := parser.ParseFile(token.NewFileSet(), "fixture_test.go", "package fixture\nfunc sharedFixture(t *testing.T) { "+fixture.body+" }", 0)
			if err != nil {
				t.Fatal("cannot parse Windows skip guard fixture")
			}
			if got := len(windowsSkipOwnerViolations(file, fixture.windowsOnly)); got != fixture.want {
				t.Fatalf("helper owner violations = %d, want %d", got, fixture.want)
			}
		})
	}
}

// Pin the pending fixture boundaries so deleting a helper guard cannot silently
// reactivate Windows-unready production paths. Only t.Helper may precede it.
func TestWindowsFixtureSkipsBeforeFilesystem(t *testing.T) {
	for _, fixture := range []struct{ path, helper, owner string }{
		{"lifecycle_hermetic_test.go", "buildLifecycleFixture", "LC-S2-PW3"},
		{"lifecycle_integration_test.go", "buildRealComponents", "LC-S2-PW3"},
		{"lifecycle_integration_test.go", "disposableCell", "LC-S2-PW3"},
	} {
		t.Run(fixture.helper, func(t *testing.T) {
			file, err := parser.ParseFile(token.NewFileSet(), fixture.path, nil, 0)
			if err != nil {
				t.Fatal("cannot parse fixture source")
			}
			var body *ast.BlockStmt
			for _, declaration := range file.Decls {
				if function, ok := declaration.(*ast.FuncDecl); ok && function.Name.Name == fixture.helper {
					body = function.Body
					break
				}
			}
			if body == nil || !startsWithWindowsSkip(body, fixture.owner) {
				t.Fatal("fixture must skip Windows with its owning slice before filesystem work")
			}
		})
	}
}

func startsWithWindowsSkip(body *ast.BlockStmt, owner string) bool {
	statements := body.List
	if len(statements) > 0 {
		if call := testingMethodCall(statements[0], "Helper"); call != nil && len(call.Args) == 0 {
			statements = statements[1:]
		}
	}
	if len(statements) == 0 {
		return false
	}
	branch, ok := statements[0].(*ast.IfStmt)
	if !ok || branch.Init != nil || branch.Else != nil || len(branch.Body.List) != 1 {
		return false
	}
	condition, ok := branch.Cond.(*ast.BinaryExpr)
	if !ok || condition.Op != token.EQL || (!windowsGOOSComparison(condition.X, condition.Y) && !windowsGOOSComparison(condition.Y, condition.X)) {
		return false
	}
	call := testingMethodCall(branch.Body.List[0], "Skip")
	if call == nil || len(call.Args) != 1 {
		return false
	}
	literal, ok := call.Args[0].(*ast.BasicLit)
	if !ok || literal.Kind != token.STRING {
		return false
	}
	message, err := strconv.Unquote(literal.Value)
	return err == nil && strings.HasPrefix(message, owner+": ") && strings.TrimSpace(strings.TrimPrefix(message, owner+": ")) != ""
}

func testingMethodCall(statement ast.Stmt, method string) *ast.CallExpr {
	expression, ok := statement.(*ast.ExprStmt)
	if !ok {
		return nil
	}
	call, ok := expression.X.(*ast.CallExpr)
	if !ok {
		return nil
	}
	selector, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || selector.Sel.Name != method {
		return nil
	}
	receiver, ok := selector.X.(*ast.Ident)
	if !ok || receiver.Name != "t" {
		return nil
	}
	return call
}

// windowsGOOSCondition reports the first runtime.GOOS comparison with "windows"
// found anywhere inside a condition, and its operator.
func windowsGOOSCondition(condition ast.Expr) (token.Token, bool) {
	var op token.Token
	found := false
	ast.Inspect(condition, func(node ast.Node) bool {
		if found {
			return false
		}
		binary, ok := node.(*ast.BinaryExpr)
		if !ok || (binary.Op != token.EQL && binary.Op != token.NEQ) {
			return true
		}
		if windowsGOOSComparison(binary.X, binary.Y) || windowsGOOSComparison(binary.Y, binary.X) {
			op, found = binary.Op, true
			return false
		}
		return true
	})
	return op, found
}

func windowsGOOSComparison(left, right ast.Expr) bool {
	selector, ok := left.(*ast.SelectorExpr)
	if !ok || selector.Sel.Name != "GOOS" {
		return false
	}
	qualifier, ok := selector.X.(*ast.Ident)
	if !ok || qualifier.Name != "runtime" {
		return false
	}
	literal, ok := right.(*ast.BasicLit)
	if !ok || literal.Kind != token.STRING {
		return false
	}
	value, err := strconv.Unquote(literal.Value)
	return err == nil && value == "windows"
}
