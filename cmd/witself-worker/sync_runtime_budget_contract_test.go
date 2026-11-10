package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

func TestWorkerLocalRuntimeSyncPostgresBudgetContract(t *testing.T) {
	source, err := os.ReadFile("sync_runtime_test.go")
	if err != nil {
		t.Fatal("read sync runtime test source")
	}
	lf := strings.ReplaceAll(string(source), "\r\n", "\n")
	for _, tc := range []struct {
		name   string
		source string
	}{
		{name: "LF", source: lf},
		{name: "CRLF", source: strings.ReplaceAll(lf, "\n", "\r\n")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			file, err := parser.ParseFile(token.NewFileSet(), "sync_runtime_test.go", strings.ReplaceAll(tc.source, "\r\n", "\n"), 0)
			if err != nil {
				t.Fatal("parse sync runtime test source")
			}
			var body *ast.BlockStmt
			for _, decl := range file.Decls {
				if fn, ok := decl.(*ast.FuncDecl); ok && fn.Name.Name == "TestWorkerLocalRuntimeSyncPostgres" {
					body = fn.Body
					break
				}
			}
			if body == nil {
				t.Fatal("sync startup Postgres test body missing")
			}
			budgetCalls := 0
			ast.Inspect(body, func(node ast.Node) bool {
				if selector, ok := node.(*ast.SelectorExpr); ok {
					if pkg, ok := selector.X.(*ast.Ident); ok && pkg.Name == "time" && selector.Sel.Name == "Minute" {
						t.Error("sync startup retains a time.Minute literal")
					}
				}
				if call, ok := node.(*ast.CallExpr); ok {
					if selector, ok := call.Fun.(*ast.SelectorExpr); ok {
						if pkg, ok := selector.X.(*ast.Ident); ok && pkg.Name == "testenv" && selector.Sel.Name == "MigrationBudget" {
							budgetCalls++
						}
					}
				}
				return true
			})
			if budgetCalls == 0 {
				t.Error("sync startup does not derive a testenv.MigrationBudget")
			}
		})
	}
}
