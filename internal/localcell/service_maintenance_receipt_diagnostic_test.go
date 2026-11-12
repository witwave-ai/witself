package localcell

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// Only these literal classes can reach the source-enrolled CI class logger.
// Receipt bytes, including any foreign stderr writer's output, stay private.
func serviceMaintenanceReceiptClass(receipt []byte) string {
	const site = "native service host site=supervisor_reconcile"
	const control = "service_shutdown phase=control operation=none journal_pending=false journal_read_ok=true\n"
	if string(receipt) == site+"\n"+control {
		return "phase_control"
	}
	lines := strings.Split(strings.TrimSuffix(string(receipt), "\n"), "\n")
	if len(lines) > 2 {
		return "extra_lines"
	}
	if !slices.Contains(lines, site) {
		return "missing_site"
	}
	return "other"
}

func TestServiceMaintenanceReceiptClass(t *testing.T) {
	const site = "native service host site=supervisor_reconcile\n"
	const idle = "service_shutdown phase=idle operation=none journal_pending=false journal_read_ok=true\n"
	const control = "service_shutdown phase=control operation=none journal_pending=false journal_read_ok=true\n"
	for _, test := range []struct {
		name, receipt, want string
	}{
		{"completed_control", site + control, "phase_control"},
		{"foreign_line", site + idle + "synthetic private output\n", "extra_lines"},
		{"blank_line", site + idle + "\n", "extra_lines"},
		{"control_and_foreign", site + control + "synthetic private output\n", "extra_lines"},
		{"missing_site", idle, "missing_site"},
		{"wrong_site", "native service host site=supervisor_command\n" + idle, "missing_site"},
		{"empty", "", "missing_site"},
		{"site_only", site, "other"},
		{"missing_final_newline", site + strings.TrimSuffix(control, "\n"), "other"},
		{"changed_field", site + strings.ReplaceAll(idle, "journal_pending=false", "journal_pending=true"), "other"},
		{"private_shape", site + "synthetic private output\n", "other"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if serviceMaintenanceReceiptClass([]byte(test.receipt)) != test.want {
				t.Fatal("fenced shutdown receipt classifier lost its closed vocabulary or mismatch category")
			}
		})
	}
}

// Keep the enrolled logger immediately before the unchanged fatal, with the
// classifier as its only value. Parsing makes this pin insensitive to CRLF.
func TestServiceMaintenanceReceiptDiagnosticContract(t *testing.T) {
	source, err := os.ReadFile("update_service_admission_test.go")
	if err != nil {
		t.Fatal("cannot read fenced shutdown diagnostic source")
	}
	for _, source := range []string{string(source), strings.ReplaceAll(strings.ReplaceAll(string(source), "\r\n", "\n"), "\n", "\r\n")} {
		file, err := parser.ParseFile(token.NewFileSet(), "update_service_admission_test.go", source, 0)
		if err != nil {
			t.Fatal("cannot parse fenced shutdown diagnostic source")
		}
		found, valid := 0, true
		for _, declaration := range file.Decls {
			function, ok := declaration.(*ast.FuncDecl)
			if !ok || function.Name.Name != "TestServiceMaintenanceRunStartupBoundary" {
				continue
			}
			ast.Inspect(function.Body, func(node ast.Node) bool {
				block, ok := node.(*ast.BlockStmt)
				if !ok {
					return true
				}
				for index, statement := range block.List {
					fatal := serviceMaintenanceDiagnosticCall(statement, "Fatal")
					if fatal == nil || len(fatal.Args) != 1 || !serviceMaintenanceDiagnosticLiteral(fatal.Args[0], "Fenced Run changed held shutdown receipt or failure site") {
						continue
					}
					found++
					if index == 0 {
						valid = false
						continue
					}
					log := serviceMaintenanceDiagnosticCall(block.List[index-1], "Logf")
					if log == nil || len(log.Args) != 2 || !serviceMaintenanceDiagnosticLiteral(log.Args[0], "fenced shutdown receipt class=%s") {
						valid = false
						continue
					}
					class, ok := log.Args[1].(*ast.CallExpr)
					if !ok || len(class.Args) != 1 {
						valid = false
						continue
					}
					name, named := class.Fun.(*ast.Ident)
					receipt, argument := class.Args[0].(*ast.Ident)
					valid = valid && named && name.Name == "serviceMaintenanceReceiptClass" && argument && receipt.Name == "receipt"
				}
				return true
			})
		}
		if found != 1 || !valid {
			t.Fatal("fenced shutdown receipt lost its class line before the unchanged fatal")
		}
	}
}

func serviceMaintenanceDiagnosticCall(statement ast.Stmt, method string) *ast.CallExpr {
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

func serviceMaintenanceDiagnosticLiteral(expression ast.Expr, want string) bool {
	literal, ok := expression.(*ast.BasicLit)
	if !ok || literal.Kind != token.STRING {
		return false
	}
	value, err := strconv.Unquote(literal.Value)
	return err == nil && value == want
}
