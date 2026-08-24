package supportrunner

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strconv"
	"strings"
	"testing"
)

func TestSupportRunnerDependencyDiscipline(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package: %v", err)
	}
	banned := []string{
		"github.com/witwave-ai/witself/internal/store",
		"github.com/witwave-ai/witself/internal/billing",
		"github.com/witwave-ai/witself/internal/server",
		"github.com/witwave-ai/witself/internal/cpserver",
		"github.com/witwave-ai/witself/internal/worker",
		"github.com/witwave-ai/witself/internal/agentemail",
		"github.com/witwave-ai/witself/internal/agentemailoutbound",
	}
	clientImports := 0
	fset := token.NewFileSet()
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, parser.ImportsOnly)
		if err != nil {
			t.Errorf("parse %s: %v", name, err)
			continue
		}
		for _, spec := range file.Imports {
			path, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				t.Errorf("unquote import in %s: %v", name, err)
				continue
			}
			for _, prefix := range banned {
				if path == prefix || strings.HasPrefix(path, prefix+"/") {
					t.Errorf("%s imports forbidden dependency %s", name, path)
				}
			}
			if path == "github.com/witwave-ai/witself/internal/client" {
				clientImports++
				if name != "api.go" {
					t.Errorf("%s imports internal/client; only api.go may do so", name)
				}
			}
		}
	}
	if clientImports != 1 {
		t.Fatalf("internal/client import count = %d, want exactly one in api.go", clientImports)
	}
}

func TestSupportRunnerClientSelectorDiscipline(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "api.go", nil, 0)
	if err != nil {
		t.Fatalf("parse api.go: %v", err)
	}
	allowed := map[string]bool{
		"ListAdminTickets":            true,
		"GetAdminTicket":              true,
		"ReplyAdminTicketAsAssistant": true,
		"RetriageAdminTicket":         true,
		"AdminTicket":                 true,
		"AdminTicketFilter":           true,
		"AdminTicketList":             true,
		"GetSupportTicketResult":      true,
		"SupportTicket":               true,
		"SupportTicketMessage":        true,
	}
	requiredCalls := map[string]bool{
		"ListAdminTickets":            false,
		"GetAdminTicket":              false,
		"ReplyAdminTicketAsAssistant": false,
		"RetriageAdminTicket":         false,
	}
	ast.Inspect(file, func(node ast.Node) bool {
		selector, ok := node.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		ident, ok := selector.X.(*ast.Ident)
		if !ok || ident.Name != "client" {
			return true
		}
		name := selector.Sel.Name
		if !allowed[name] {
			t.Errorf("api.go uses forbidden client selector client.%s", name)
		}
		if _, required := requiredCalls[name]; required {
			requiredCalls[name] = true
		}
		return true
	})
	for name, seen := range requiredCalls {
		if !seen {
			t.Errorf("api.go does not use required client call client.%s", name)
		}
	}
}
