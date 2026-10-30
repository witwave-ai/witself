package localcell_test

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"strings"
	"testing"
)

// The Linux driver is excluded from the default executable. Inspect its bindings
// independently so it cannot silently bypass the executable hermetic contracts.
// This is source-wiring evidence, not evidence that a native lifecycle ran.
func ownerReceiptCheckDriverWiring(file *ast.File) error {
	invalid := errors.New("owner receipt driver bypasses its hermetic contract")
	functions := make(map[string]*ast.FuncDecl)
	for _, declaration := range file.Decls {
		if function, ok := declaration.(*ast.FuncDecl); ok {
			functions[function.Name.Name] = function
		}
	}
	parent := functions["TestOwnerRebootReceipt"]
	if parent == nil || parent.Body == nil || len(parent.Body.List) == 0 {
		return invalid
	}
	// Phase usage may be validated first, but require a direct call before root
	// setup. A filtered subtest or uncalled closure cannot replace execution.
	guards, roots := 0, 0
	for _, statement := range parent.Body.List {
		if expression, ok := statement.(*ast.ExprStmt); ok && ownerReceiptNamedCall(expression.X, "TestOwnerReceiptRegressionGuards", "t") {
			guards++
		}
		ast.Inspect(statement, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			if name, ok := call.Fun.(*ast.Ident); ok && name.Name == "ownerReceiptRoot" {
				if guards != 1 {
					roots = -1
				} else if roots >= 0 {
					roots++
				}
			}
			return true
		})
	}
	if guards != 1 || roots != 1 {
		return invalid
	}
	for _, name := range []string{"ownerReceiptVerify", "ownerReceiptObserve"} {
		function := functions[name]
		if function == nil || function.Body == nil {
			return invalid
		}
		valid := true
		ast.Inspect(function.Body, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			if identifier, ok := call.Fun.(*ast.Ident); ok && identifier.Name == "ownerReceiptInvoke" {
				if len(call.Args) == 0 {
					valid = false
					return true
				}
				switch ownerReceiptStringLiteral(call.Args[len(call.Args)-1]) {
				case "start", "run", "install", "repair":
					valid = false
				}
			}
			selector, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			receiver, ok := selector.X.(*ast.Ident)
			if !ok {
				return true
			}
			if receiver.Name == "os" && selector.Sel.Name == "RemoveAll" {
				valid = false
			}
			if receiver.Name == "cell" {
				switch selector.Sel.Name {
				case "Inspect", "ServiceDescriptor":
				case "Control":
					if len(call.Args) != 2 {
						valid = false
						break
					}
					action := ownerReceiptStringLiteral(call.Args[1])
					if action != "observe" && action != "stop" {
						valid = false
					}
				default:
					valid = false
				}
			}
			return true
		})
		if !valid {
			return invalid
		}
	}
	cleanup := functions["ownerReceiptCleanup"]
	if cleanup == nil || cleanup.Body == nil {
		return invalid
	}
	delegations, bypass := 0, false
	ast.Inspect(cleanup.Body, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		if ownerReceiptNamedCall(call, "ownerReceiptCleanupArtifacts", "root") {
			delegations++
		}
		if identifier, ok := call.Fun.(*ast.Ident); ok && identifier.Name == "ownerReceiptRemoveArtifacts" {
			bypass = true
		}
		if selector, ok := call.Fun.(*ast.SelectorExpr); ok && selector.Sel.Name == "RemoveAll" {
			bypass = true
		}
		return true
	})
	if delegations != 1 || bypass {
		return invalid
	}
	return nil
}

func ownerReceiptNamedCall(expression ast.Expr, function, argument string) bool {
	call, ok := expression.(*ast.CallExpr)
	if !ok || len(call.Args) != 1 {
		return false
	}
	name, ok := call.Fun.(*ast.Ident)
	value, argumentOK := call.Args[0].(*ast.Ident)
	return ok && argumentOK && name.Name == function && value.Name == argument
}

func ownerReceiptStringLiteral(expression ast.Expr) string {
	literal, ok := expression.(*ast.BasicLit)
	if !ok || literal.Kind != token.STRING {
		return ""
	}
	value, err := strconv.Unquote(literal.Value)
	if err != nil {
		return ""
	}
	return value
}

func TestOwnerReceiptDriverWiring(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "service_receipt_test.go", nil, 0)
	if err != nil || ownerReceiptCheckDriverWiring(file) != nil {
		t.Fatal("owner receipt driver bypasses its hermetic contract")
	}
}

func TestOwnerReceiptDriverWiringRejectsBypasses(t *testing.T) {
	const fixture = `package fixture
func TestOwnerRebootReceipt(t *T) { TestOwnerReceiptRegressionGuards(t); ownerReceiptRoot(t, "synthetic") }
func ownerReceiptVerify(t *T, cell *Cell, root, host string) {
 operation := func(action string) bool {
  cell.Control(t.Context(), "stop")
  return ownerReceiptInvoke(t.Context(), host, root, action)
 }
 _ = operation
}
func ownerReceiptObserve(t *T, cell *Cell) bool { cell.Control(t.Context(), "observe"); return true }
func ownerReceiptCleanup(t *T, cell *Cell, root string) { ownerReceiptCleanupArtifacts(root) }
`
	for _, scenario := range []string{"baseline", "M2_start", "M7_delete", "direct_start", "health", "unexecuted_guards", "late_guards", "cleanup_bypass"} {
		source := fixture
		switch scenario {
		case "M2_start":
			source = strings.Replace(source, "operation :=", `ownerReceiptInvoke(t.Context(), host, root, "start"); operation :=`, 1)
		case "M7_delete":
			source = strings.Replace(source, "return ownerReceiptInvoke", `os.RemoveAll(filepath.Join(root, "pg", "base")); return ownerReceiptInvoke`, 1)
		case "direct_start":
			source = strings.Replace(source, `cell.Control(t.Context(), "observe")`, `cell.Start(t.Context())`, 1)
		case "health":
			source = strings.Replace(source, `cell.Control(t.Context(), "observe")`, `cell.Control(t.Context(), "health")`, 1)
		case "unexecuted_guards":
			source = strings.Replace(source, "TestOwnerReceiptRegressionGuards(t)", "func() { TestOwnerReceiptRegressionGuards(t) }", 1)
		case "late_guards":
			source = strings.Replace(source, `TestOwnerReceiptRegressionGuards(t); ownerReceiptRoot(t, "synthetic")`, `ownerReceiptRoot(t, "synthetic"); TestOwnerReceiptRegressionGuards(t)`, 1)
		case "cleanup_bypass":
			source = strings.Replace(source, "ownerReceiptCleanupArtifacts(root)", `ownerReceiptRemoveArtifacts(root, []string{"../outside"})`, 1)
		}
		file, err := parser.ParseFile(token.NewFileSet(), "fixture.go", source, 0)
		if err != nil {
			t.Fatal("cannot parse synthetic driver wiring")
		}
		if (ownerReceiptCheckDriverWiring(file) == nil) != (scenario == "baseline") {
			t.Fatal("receipt wiring guard accepted an invalid binding")
		}
	}
}
