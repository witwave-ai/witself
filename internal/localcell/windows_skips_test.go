package localcell

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// Inspect the whole source tree, including helper functions taking *testing.T
// and platform-specific files. Native CI lists each executed skip by test name.
// This source-only check also runs on Windows; it needs no private directories.
func windowsSkipOwnerViolations(file *ast.File, windowsOnly bool) []token.Pos {
	// PW1-PW3 are implemented. Only privilege-dependent symlink creation
	// may skip; the unconditional Windows junction coverage stays active. The
	// one further exception is the exact native process-DACL witness that an
	// enabled SeDebugPrivilege invalidates, and only in Windows-only files.
	allowed := map[string]bool{
		"LC-S2-PW3: Windows spool symlink fixture privilege unavailable; mandatory junction coverage remains active": true,
		"LC-S2-PW3: Windows symlink fixture privileges unavailable":                                                  true,
		"LC-S2-PW3: Windows state symlink fixture privilege unavailable; mandatory junction coverage remains active": true,
	}
	const debugPrivilegeSkip = "LC-S2-PW2: real process DACL denial unavailable with enabled SeDebugPrivilege"
	permitted := func(message string) bool {
		return allowed[message] || (windowsOnly && message == debugPrivilegeSkip)
	}
	var violations []token.Pos
	seen := make(map[token.Pos]bool)
	reject := func(position token.Pos) {
		if !seen[position] {
			seen[position] = true
			violations = append(violations, position)
		}
	}
	// Retired owner literals are forbidden everywhere, including switch cases
	// and shared helpers whose caller supplies the Windows condition.
	ast.Inspect(file, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		method, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || (method.Sel.Name != "Skip" && method.Sel.Name != "Skipf") {
			return true
		}
		if len(call.Args) == 1 {
			if literal, ok := call.Args[0].(*ast.BasicLit); ok && literal.Kind == token.STRING {
				message, _ := strconv.Unquote(literal.Value)
				if permitted(message) {
					return true
				}
			}
		}
		for _, argument := range call.Args {
			ast.Inspect(argument, func(node ast.Node) bool {
				literal, ok := node.(*ast.BasicLit)
				if !ok || literal.Kind != token.STRING {
					return true
				}
				message, _ := strconv.Unquote(literal.Value)
				for _, owner := range []string{"LC-S2-PW0:", "LC-S2-PW1:", "LC-S2-PW2:", "LC-S2-PW3:"} {
					if strings.Contains(message, owner) {
						reject(call.Pos())
					}
				}
				return true
			})
		}
		return true
	})
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
			if !permitted(message) {
				reject(call.Pos())
			}
			return true
		})
	}
	if windowsOnly {
		check(file)
		return violations
	}
	ast.Inspect(file, func(node ast.Node) bool {
		// A GOOS-dependent condition requires ownership in every branch. Do
		// not infer which side runs on Windows: negation, compound conditions
		// and switch defaults can all reach Windows without naming it directly.
		switch branch := node.(type) {
		case *ast.IfStmt:
			if containsRuntimeGOOS(branch.Cond) || containsRuntimeGOOS(branch.Init) {
				check(branch.Body)
				if branch.Else != nil {
					check(branch.Else)
				}
			}
		case *ast.SwitchStmt:
			goosDependent := containsRuntimeGOOS(branch.Tag) || containsRuntimeGOOS(branch.Init)
			for _, statement := range branch.Body.List {
				clause := statement.(*ast.CaseClause)
				for _, condition := range clause.List {
					goosDependent = goosDependent || containsRuntimeGOOS(condition)
				}
			}
			if goosDependent {
				check(branch.Body)
			}
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
				t.Errorf("%s: Windows lifecycle skips are retired; only the documented symlink-privilege skips and the exact Windows-only LC-S2-PW2 enabled SeDebugPrivilege DACL reason are allowed", positions.Position(position))
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
		{"retired-lifecycle-helper", `if runtime.GOOS == "windows" { t.Skip("LC-S2-PW3: lifecycle pending") }`, false, 1},
		{"retired-launch-helper", `if runtime.GOOS == "windows" { t.Skip("LC-S2-PW2: launch pending") }`, false, 1},
		{"retired-windows-launch-helper", `t.Skip("LC-S2-PW2: launch pending")`, true, 1},
		{"enabled-debug-privilege-windows-helper", `t.Skip("LC-S2-PW2: real process DACL denial unavailable with enabled SeDebugPrivilege")`, true, 0},
		{"wrong-debug-privilege-reason", `t.Skip("LC-S2-PW2: real process DACL denial unavailable with disabled SeDebugPrivilege")`, true, 1},
		{"anonymous-debug-privilege-reason", `t.Skip("real process DACL denial unavailable with enabled SeDebugPrivilege")`, true, 1},
		{"debug-privilege-shared-source", `if runtime.GOOS == "windows" { t.Skip("LC-S2-PW2: real process DACL denial unavailable with enabled SeDebugPrivilege") }`, false, 1},
		{"retired-privacy-helper", `if runtime.GOOS == "windows" { t.Skip("LC-S2-PW1: privacy pending") }`, false, 1},
		{"retired-lifecycle-switch", `switch runtime.GOOS { case "windows": t.Skip("LC-S2-PW3: lifecycle pending") }`, false, 1},
		{"retired-launch-switch", `switch runtime.GOOS { case "windows": t.Skipf("LC-S2-PW2: %s", "launch pending") }`, false, 1},
		{"retired-privacy-switch", `switch runtime.GOOS { case "windows": t.Skip("LC-S2-PW1: privacy pending") }`, false, 1},
		{"retired-unconditional-helper", `t.Skip("LC-S2-PW3: lifecycle pending")`, false, 1},
		{"retired-unit-helper", `t.Skip("LC-S2-PW0: unit coverage pending")`, false, 1},
		{"retired-concatenated-helper", `t.Skip("LC-S2-PW3:" + " lifecycle pending")`, false, 1},
		{"altered-symlink-exception", `t.Skip("LC-S2-PW3: Windows symlink fixture privileges unavailable" + " for another reason")`, false, 1},
		{"retired-nested-switch", `if ready { switch runtime.GOOS { case "windows": t.Skipf("LC-S2-PW3: %s", "lifecycle pending") } }`, false, 1},
		{"anonymous-helper", `if runtime.GOOS == "windows" { t.Skip("privacy pending") }`, false, 1},
		{"empty-reason", `if runtime.GOOS == "windows" { t.Skip("LC-S2-PW3: ") }`, false, 1},
		{"anonymous-skipf", `if runtime.GOOS == "windows" { t.Skipf("pending: %s", "fixture") }`, false, 1},
		{"anonymous-skipnow", `if runtime.GOOS == "windows" { t.SkipNow() }`, false, 1},
		{"anonymous-switch-case", `switch runtime.GOOS { case "windows": t.Skip("pending") }`, false, 1},
		{"anonymous-switch-default", `switch runtime.GOOS { case "linux", "darwin": default: t.Skip("pending") }`, false, 1},
		{"anonymous-condition-switch-case", `switch { case runtime.GOOS == "windows": t.Skipf("pending: %s", "fixture") }`, false, 1},
		{"anonymous-condition-switch-default", `switch { case runtime.GOOS != "windows": default: t.SkipNow() }`, false, 1},
		{"inequality-else", `if runtime.GOOS != "windows" {} else { t.Skip("pending") }`, false, 1},
		{"nested-condition", `if true && (runtime.GOOS == "windows") { t.Skip("pending") }`, false, 1},
		{"compound-condition", `if runtime.GOOS != "windows" || !ready { t.Skip("pending") }`, false, 1},
		{"compound-else", `if runtime.GOOS == "windows" && ready {} else { t.Skipf("pending: %s", "fixture") }`, false, 1},
		{"multiple-goos-comparisons", `if runtime.GOOS != "windows" || (runtime.GOOS == "windows" && !ready) { t.Skip("pending") }`, false, 1},
		{"negated-condition", `if !(runtime.GOOS != "windows") { t.Skip("pending") }`, false, 1},
		{"negated-else", `if !(runtime.GOOS == "windows") {} else { t.Skip("pending") }`, false, 1},
		{"condition-both-branches", `if runtime.GOOS == "windows" { t.Skip("pending") } else { t.Skip("pending") }`, false, 2},
		{"goos-if-initializer", `if platform := runtime.GOOS; platform == "windows" { t.Skip("pending") }`, false, 1},
		{"goos-switch-initializer", `switch platform := runtime.GOOS; platform { case "linux", "darwin": default: t.Skip("pending") }`, false, 1},
		{"windows-only-helper", `t.Skip("pending")`, true, 1},
		{"retired-windows-helper", `t.Skip("LC-S2-PW3: fixture pending")`, true, 1},
		{"symlink-privilege", `if runtime.GOOS == "windows" { t.Skip("LC-S2-PW3: Windows symlink fixture privileges unavailable") }`, false, 0},
		{"symlink-privilege-switch", `switch runtime.GOOS { case "windows": t.Skip("LC-S2-PW3: Windows symlink fixture privileges unavailable") }`, false, 0},
		{"symlink-privilege-switch-default", `switch runtime.GOOS { case "linux", "darwin": default: t.Skip("LC-S2-PW3: Windows symlink fixture privileges unavailable") }`, false, 0},
		{"symlink-privilege-compound", `if runtime.GOOS == "windows" && !ready { t.Skip("LC-S2-PW3: Windows symlink fixture privileges unavailable") }`, false, 0},
		{"state-symlink-privilege", `t.Skip("LC-S2-PW3: Windows state symlink fixture privilege unavailable; mandatory junction coverage remains active")`, true, 0},
		{"other-platform", `if runtime.GOOS == "plan9" { t.Skip("pending") }`, false, 1},
		{"non-platform-condition", `if !ready { t.Skip("fixture unavailable") }`, false, 0},
		{"non-platform-switch", `switch ready { case false: t.Skip("fixture unavailable") }`, false, 0},
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

func containsRuntimeGOOS(condition ast.Node) bool {
	if condition == nil {
		return false
	}
	found := false
	ast.Inspect(condition, func(node ast.Node) bool {
		if found {
			return false
		}
		selector, ok := node.(*ast.SelectorExpr)
		if !ok || selector.Sel.Name != "GOOS" {
			return true
		}
		qualifier, ok := selector.X.(*ast.Ident)
		found = ok && qualifier.Name == "runtime"
		return !found
	})
	return found
}
