package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

func TestServerLocalRuntimeSyncPostgresBudgetContract(t *testing.T) {
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
				if fn, ok := decl.(*ast.FuncDecl); ok && fn.Name.Name == "TestServerLocalRuntimeSyncStartupPostgres" {
					body = fn.Body
					break
				}
			}
			if body == nil {
				t.Fatal("sync startup Postgres test body missing")
			}
			var startup token.Pos
			ast.Inspect(body, func(node ast.Node) bool {
				if call, ok := node.(*ast.CallExpr); ok {
					if fn, ok := call.Fun.(*ast.Ident); ok && fn.Name == "captureMigrationStartup" {
						startup = call.End()
					}
				}
				return true
			})
			if !startup.IsValid() {
				t.Fatal("sync startup capture missing")
			}
			budgetCalls := 0
			ast.Inspect(body, func(node ast.Node) bool {
				if selector, ok := node.(*ast.SelectorExpr); ok {
					if pkg, ok := selector.X.(*ast.Ident); ok && pkg.Name == "time" && selector.Sel.Name == "Minute" {
						t.Error("sync startup retains a time.Minute literal")
					}
				}
				call, ok := node.(*ast.CallExpr)
				if !ok || call.Pos() < startup {
					return true
				}
				selector, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				receiver, ok := selector.X.(*ast.Ident)
				if !ok {
					return true
				}
				if receiver.Name == "testenv" && selector.Sel.Name == "MigrationBudget" {
					budgetCalls++
				}
				if receiver.Name == "db" && selector.Sel.Name == "QueryRow" && len(call.Args) > 0 {
					if ctx, ok := call.Args[0].(*ast.Ident); ok && ctx.Obj != nil {
						if decl, ok := ctx.Obj.Decl.(ast.Node); ok && decl.Pos() < startup {
							t.Error("sync startup observer reuses a pre-startup context")
						}
					}
				}
				return true
			})
			if budgetCalls == 0 {
				t.Error("sync startup does not derive a post-startup testenv.MigrationBudget")
			}
		})
	}
}
