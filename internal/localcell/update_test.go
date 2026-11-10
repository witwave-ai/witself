package localcell

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
)

func TestUpdateFixedCauses(t *testing.T) {
	for cause, want := range map[fixedCause]string{
		errMaintenanceFenceActive: "local_maintenance_fence_active",
		errUpdatePartialState:     "local_update_partial_state",
	} {
		if cause.Error() != want {
			t.Fatal("update fixed cause changed")
		}
	}
}

// Keep the future update protocol unreachable until its own integration slice
// proves legacy parity, custody and recovery at each public boundary.
func TestUpdateFoundationsRemainUnwired(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal("cannot inventory local cell sources")
	}
	sources := map[string]*ast.File{}
	helpers := map[string]bool{"readUpdateJournal": true, "validUpdateJournalStep": true, "valid": true}
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), name, nil, 0)
		if err != nil {
			t.Fatal("cannot parse local cell source")
		}
		sources[name] = file
		if name == "update_record.go" || name == "update_fence.go" {
			for _, decl := range file.Decls {
				if fn, ok := decl.(*ast.FuncDecl); ok {
					if fn.Name.IsExported() {
						t.Fatal("update helper became public")
					}
					helpers[fn.Name.Name] = true
				}
			}
		}
	}
	if len(helpers) <= 3 {
		t.Fatal("update helper inventory is empty")
	}
	for _, file := range sources {
		ast.Inspect(file, func(node ast.Node) bool {
			switch node := node.(type) {
			case *ast.TypeSpec:
				if strings.HasPrefix(node.Name.Name, "Update") {
					t.Fatal("public update type was reintroduced")
				}
			case *ast.FuncDecl:
				if node.Name.Name == "Update" || node.Name.Name == "repairUpdate" {
					t.Fatal("public update or Repair bridge was reintroduced")
				}
				if helpers[node.Name.Name] {
					return false
				}
				ast.Inspect(node.Body, func(child ast.Node) bool {
					switch child := child.(type) {
					case *ast.SelectorExpr:
						if helpers[child.Sel.Name] {
							t.Fatalf("existing function %s reaches update helper %s", node.Name.Name, child.Sel.Name)
						}
					case *ast.Ident:
						if child.Name != "valid" && helpers[child.Name] {
							t.Fatalf("existing function %s reaches update helper %s", node.Name.Name, child.Name)
						}
					}
					return true
				})
				return false
			}
			return true
		})
	}
}
