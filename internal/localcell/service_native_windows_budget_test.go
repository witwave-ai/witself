package localcell

import (
	"errors"
	"go/ast"
	"go/constant"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
	"time"
)

const windowsSyntheticChildBridgeSource = "service_control_child_bridge_windows_test.go"
const windowsSyntheticChildSource = "service_control_child_windows_test.go"

const windowsSyntheticHarnessSource = "../tools/localcellharness/service_subprocess_windows_test.go"

// Source evidence only: the Windows runtime receipt still runs on Windows.
// Count enrollment in the selected sources and evaluate both package-local
// budgets and timeout expressions so increasing either budget or adding an
// enrolled test in those sources fails closed. The child share is counted per
// matrix row.
func TestWindowsSyntheticRuntimeBudget(t *testing.T) {
	sources := windowsSyntheticBudgetSources(t)
	worst, err := windowsSyntheticBudgetContract(sources)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("windows_synthetic worst_case_seconds=%.0f", worst.Seconds())
}

func TestWindowsSyntheticRuntimeBudgetMutations(t *testing.T) {
	for _, name := range []string{"local_budget_doubled", "harness_budget_doubled", "extra_enrolled_test", "child_budget_increased", "child_budget_unbound", "child_row_budget_unbound", "child_extra_row", "child_row_context_unbounded"} {
		t.Run(name, func(t *testing.T) {
			sources := windowsSyntheticBudgetSources(t)
			path := "service_control_windows_test.go"
			if name == "harness_budget_doubled" {
				path = windowsSyntheticHarnessSource
			}
			switch name {
			case "child_budget_increased":
				path = windowsSyntheticChildBridgeSource
				sources[path] = []byte(strings.Replace(string(sources[path]), "windowsSyntheticTestBudget / 15", "windowsSyntheticTestBudget / 10", 1))
			case "child_budget_unbound":
				path = windowsSyntheticChildSource
				sources[path] = []byte(strings.Replace(string(sources[path]), "localcell.NativeServiceChildSyntheticBudget)", "90*time.Second)", 1))
			case "child_row_budget_unbound", "child_extra_row", "child_row_context_unbounded":
				path = windowsSyntheticChildSource
				original := string(sources[path])
				from := "context.WithTimeout(t.Context(), localcell.NativeServiceChildSyntheticBudget)"
				to := "context.WithTimeout(t.Context(), 90*time.Second)"
				switch name {
				case "child_extra_row":
					from, to = `{"full", os.Environ},`, "{\"full\", os.Environ},\n\t\t{\"extra\", os.Environ},"
				case "child_row_context_unbounded":
					to = "context.WithCancel(t.Context())"
				}
				mutated := strings.Replace(original, from, to, 1)
				if mutated == original {
					t.Fatal("synthetic budget mutation no longer applies")
				}
				sources[path] = []byte(mutated)
			case "extra_enrolled_test":
				sources[path] = append(sources[path], []byte("\nfunc TestWindowsExtraBudget(t *testing.T) { _ = windowsSyntheticTestContext(t) }\n")...)
			default:
				original := string(sources[path])
				mutated := strings.Replace(original, "windowsSyntheticTestBudget = 90 * time.Second", "windowsSyntheticTestBudget = 180 * time.Second", 1)
				if mutated == original {
					t.Fatal("synthetic budget mutation no longer applies")
				}
				sources[path] = []byte(mutated)
			}
			if _, err := windowsSyntheticBudgetContract(sources); err == nil {
				t.Fatal("synthetic budget contract accepted an excessive runtime allowance")
			}
		})
	}
}

func windowsSyntheticBudgetSources(t *testing.T) map[string][]byte {
	t.Helper()
	sources := make(map[string][]byte)
	for _, path := range []string{"service_control_windows_test.go", "service_boot_windows_test.go", windowsSyntheticHarnessSource, windowsSyntheticChildBridgeSource, windowsSyntheticChildSource} {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal("cannot read Windows synthetic budget source")
		}
		sources[path] = data
	}
	return sources
}

func windowsSyntheticBudgetContract(sources map[string][]byte) (time.Duration, error) {
	invalid := errors.New("Windows synthetic runtime budget is unaccounted or exceeds 96 seconds")
	files := make(map[string]*ast.File)
	for path, source := range sources {
		file, err := parser.ParseFile(token.NewFileSet(), path, source, 0)
		if err != nil {
			return 0, invalid
		}
		files[path] = file
	}
	localBudget := windowsSyntheticBudgetConstant(files["service_control_windows_test.go"])
	harnessBudget := windowsSyntheticBudgetConstant(files[windowsSyntheticHarnessSource])
	childShare := windowsSyntheticBudgetNamedConstant(files[windowsSyntheticChildBridgeSource], "NativeServiceChildSyntheticBudget", localBudget)
	if localBudget == nil || harnessBudget == nil || childShare == nil {
		return 0, invalid
	}
	rowCount := windowsSyntheticChildBudgetRows(files[windowsSyntheticChildSource])
	if rowCount < 1 {
		return 0, invalid
	}
	childShare = constant.BinaryOp(childShare, token.MUL, constant.MakeInt64(rowCount))
	var localShare, harnessShare constant.Value
	enrolled := int64(0)
	for path, file := range files {
		if path == windowsSyntheticChildBridgeSource || path == windowsSyntheticChildSource {
			continue // The child matrix is accounted as rows × the exported share.
		}
		for _, declaration := range file.Decls {
			function, ok := declaration.(*ast.FuncDecl)
			if !ok || function.Body == nil {
				continue
			}
			if path == windowsSyntheticHarnessSource {
				if function.Name.Name == "TestWindowsServiceForegroundSubprocessCommands" {
					harnessShare = windowsSyntheticBudgetTimeout(function, harnessBudget)
				}
				continue
			}
			if function.Name.Name == "windowsSyntheticTestContext" {
				localShare = windowsSyntheticBudgetTimeout(function, localBudget)
			}
			if !strings.HasPrefix(function.Name.Name, "Test") {
				continue
			}
			// Each enrolled top-level test owns one deadline; subtests and child
			// processes must share it, never allocate another full allowance.
			calls := 0
			ast.Inspect(function.Body, func(node ast.Node) bool {
				if call, ok := node.(*ast.CallExpr); ok {
					if name, ok := call.Fun.(*ast.Ident); ok && name.Name == "windowsSyntheticTestContext" {
						calls++
					}
				}
				return true
			})
			if calls != 1 {
				return 0, invalid
			}
			enrolled++
		}
	}
	if enrolled == 0 || localShare == nil || harnessShare == nil {
		return 0, invalid
	}
	worst := constant.BinaryOp(constant.BinaryOp(constant.BinaryOp(localShare, token.MUL, constant.MakeInt64(enrolled)), token.ADD, harnessShare), token.ADD, childShare)
	nanos, ok := constant.Int64Val(worst)
	if !ok || nanos <= 0 || nanos > int64(96*time.Second) {
		return 0, invalid
	}
	return time.Duration(nanos), nil
}

func windowsSyntheticChildBudgetRows(file *ast.File) int64 {
	if file == nil {
		return 0
	}
	var child *ast.FuncDecl
	for _, declaration := range file.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if ok && function.Name.Name == "TestServiceControlChildProcessClient" {
			if child != nil || function.Body == nil {
				return 0
			}
			child = function
		}
	}
	if child == nil {
		return 0
	}
	rowCount, bounds := int64(0), 0
	for _, statement := range child.Body.List {
		assignment, ok := statement.(*ast.AssignStmt)
		if !ok || len(assignment.Rhs) != 1 {
			continue
		}
		if len(assignment.Lhs) == 1 {
			if name, ok := assignment.Lhs[0].(*ast.Ident); ok && name.Name == "rows" {
				literal, ok := assignment.Rhs[0].(*ast.CompositeLit)
				if !ok || assignment.Tok != token.DEFINE || rowCount != 0 {
					return 0
				}
				slice, ok := literal.Type.(*ast.ArrayType)
				if !ok || slice.Len != nil {
					return 0
				}
				if _, ok := slice.Elt.(*ast.StructType); !ok || len(literal.Elts) == 0 {
					return 0
				}
				rowCount = int64(len(literal.Elts))
			}
		}
		call, ok := assignment.Rhs[0].(*ast.CallExpr)
		if !ok || !windowsSyntheticBudgetSelector(call.Fun, "context", "WithTimeout") {
			continue
		}
		bounds++
		if len(call.Args) != 2 || !windowsSyntheticBudgetTestContext(call.Args[0]) || !windowsSyntheticChildOuterLimit(call.Args[1]) {
			return 0
		}
	}
	if bounds != 1 || !windowsSyntheticChildRowBounded(child.Body) {
		return 0
	}
	return rowCount
}

func windowsSyntheticChildOuterLimit(expression ast.Expr) bool {
	product, ok := expression.(*ast.BinaryExpr)
	if !ok || product.Op != token.MUL || !windowsSyntheticBudgetSelector(product.Y, "localcell", "NativeServiceChildSyntheticBudget") {
		return false
	}
	duration, ok := product.X.(*ast.CallExpr)
	if !ok || !windowsSyntheticBudgetSelector(duration.Fun, "time", "Duration") || len(duration.Args) != 1 {
		return false
	}
	length, ok := duration.Args[0].(*ast.CallExpr)
	if !ok || len(length.Args) != 1 {
		return false
	}
	callee, ok := length.Fun.(*ast.Ident)
	if !ok || callee.Name != "len" {
		return false
	}
	rows, ok := length.Args[0].(*ast.Ident)
	return ok && rows.Name == "rows"
}

func windowsSyntheticChildRowBounded(body *ast.BlockStmt) bool {
	runs := 0
	var row *ast.FuncLit
	ast.Inspect(body, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if ok && windowsSyntheticBudgetSelector(call.Fun, "t", "Run") {
			runs++
			if len(call.Args) == 2 {
				row, _ = call.Args[1].(*ast.FuncLit)
			}
		}
		return true
	})
	if runs != 1 || row == nil {
		return false
	}
	bounds := 0
	for _, statement := range row.Body.List {
		assignment, ok := statement.(*ast.AssignStmt)
		if !ok || len(assignment.Rhs) != 1 {
			continue
		}
		call, ok := assignment.Rhs[0].(*ast.CallExpr)
		if !ok || !windowsSyntheticBudgetSelector(call.Fun, "context", "WithTimeout") {
			continue
		}
		if len(call.Args) != 2 {
			return false
		}
		if _, ok := call.Args[1].(*ast.SelectorExpr); !ok {
			continue // Readiness and command allowances are row-local identifiers.
		}
		bounds++
		if !windowsSyntheticBudgetTestContext(call.Args[0]) || !windowsSyntheticBudgetSelector(call.Args[1], "localcell", "NativeServiceChildSyntheticBudget") {
			return false
		}
	}
	return bounds == 1
}

func windowsSyntheticBudgetSelector(expression ast.Expr, name, member string) bool {
	selector, ok := expression.(*ast.SelectorExpr)
	if !ok || selector.Sel.Name != member {
		return false
	}
	identifier, ok := selector.X.(*ast.Ident)
	return ok && identifier.Name == name
}

func windowsSyntheticBudgetTestContext(expression ast.Expr) bool {
	call, ok := expression.(*ast.CallExpr)
	return ok && len(call.Args) == 0 && windowsSyntheticBudgetSelector(call.Fun, "t", "Context")
}

func windowsSyntheticBudgetConstant(file *ast.File) constant.Value {
	return windowsSyntheticBudgetNamedConstant(file, "windowsSyntheticTestBudget", nil)
}

func windowsSyntheticBudgetNamedConstant(file *ast.File, name string, budget constant.Value) constant.Value {
	if file == nil {
		return nil
	}
	for _, declaration := range file.Decls {
		group, ok := declaration.(*ast.GenDecl)
		if !ok || group.Tok != token.CONST {
			continue
		}
		for _, spec := range group.Specs {
			value, ok := spec.(*ast.ValueSpec)
			if ok && len(value.Names) == 1 && value.Names[0].Name == name && len(value.Values) == 1 {
				return windowsSyntheticBudgetValue(value.Values[0], budget)
			}
		}
	}
	return nil
}

func windowsSyntheticBudgetTimeout(function *ast.FuncDecl, budget constant.Value) constant.Value {
	for _, statement := range function.Body.List {
		assignment, ok := statement.(*ast.AssignStmt)
		if !ok || len(assignment.Rhs) != 1 {
			continue
		}
		call, ok := assignment.Rhs[0].(*ast.CallExpr)
		if !ok || len(call.Args) != 2 {
			continue
		}
		selector, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || selector.Sel.Name != "WithTimeout" {
			continue
		}
		name, ok := selector.X.(*ast.Ident)
		if ok && name.Name == "context" {
			return windowsSyntheticBudgetValue(call.Args[1], budget)
		}
	}
	return nil
}

func windowsSyntheticBudgetValue(expression ast.Expr, budget constant.Value) constant.Value {
	switch value := expression.(type) {
	case *ast.BasicLit:
		if value.Kind == token.INT {
			return constant.MakeFromLiteral(value.Value, token.INT, 0)
		}
	case *ast.Ident:
		if value.Name == "windowsSyntheticTestBudget" {
			return budget
		}
	case *ast.SelectorExpr:
		if name, ok := value.X.(*ast.Ident); ok && name.Name == "time" && value.Sel.Name == "Second" {
			return constant.MakeInt64(int64(time.Second))
		}
	case *ast.ParenExpr:
		return windowsSyntheticBudgetValue(value.X, budget)
	case *ast.BinaryExpr:
		left, right := windowsSyntheticBudgetValue(value.X, budget), windowsSyntheticBudgetValue(value.Y, budget)
		if left == nil || right == nil || constant.Sign(left) <= 0 || constant.Sign(right) <= 0 {
			return nil
		}
		switch value.Op {
		case token.MUL, token.ADD:
			return constant.BinaryOp(left, value.Op, right)
		case token.QUO:
			return constant.BinaryOp(left, token.QUO_ASSIGN, right)
		}
	}
	return nil
}
