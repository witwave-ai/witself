package worker

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func waitWorkerStarted(t *testing.T, client *http.Client, healthAddr string, bound time.Duration) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), bound)
	defer cancel()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+healthAddr+"/startupz", nil)
		if err != nil {
			t.Fatal("could not construct worker startup request")
		}
		response, err := client.Do(request)
		if err == nil {
			// Drain the small response so the next probe can reuse this connection.
			_, readErr := io.Copy(io.Discard, response.Body)
			_ = response.Body.Close()
			if readErr == nil && response.StatusCode == http.StatusOK {
				return
			}
		}
		// A refused connection before Serve starts is also a retryable startup observation.
		select {
		case <-ctx.Done():
			t.Fatalf("worker did not report started within %s", bound)
		case <-ticker.C:
		}
	}
}

func TestRestartReadinessAssertionsObserveStartupContract(t *testing.T) {
	files, err := filepath.Glob("*_test.go")
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	for _, name := range files {
		file, err := parser.ParseFile(token.NewFileSet(), name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, declaration := range file.Decls {
			function, ok := declaration.(*ast.FuncDecl)
			if !ok || function.Body == nil || !strings.HasPrefix(function.Name.Name, "Test") {
				continue
			}
			// The readiness path can be a range value, rather than a literal in the URL.
			// Conservatively guard every healthAddr request in a test containing /readyz.
			hasReadiness := false
			ast.Inspect(function.Body, func(node ast.Node) bool {
				if literal, ok := node.(*ast.BasicLit); ok && literal.Kind == token.STRING {
					value, _ := strconv.Unquote(literal.Value)
					hasReadiness = hasReadiness || value == "/readyz"
				}
				return true
			})
			if !hasReadiness {
				continue
			}
			observedStartup := false
			for _, statement := range function.Body.List {
				// Require an unconditional, synchronous helper call in this function body.
				if expression, ok := statement.(*ast.ExprStmt); ok {
					if call, ok := expression.X.(*ast.CallExpr); ok && workerTestCallNamed(call, "waitWorkerStarted") &&
						len(call.Args) == 4 && workerTestUsesHealthAddr(call.Args[2]) {
						observedStartup = true
					}
				}
				ast.Inspect(statement, func(node ast.Node) bool {
					call, ok := node.(*ast.CallExpr)
					if !ok || !workerTestCallNamed(call, "getWorkerEndpoint") || len(call.Args) != 3 ||
						!workerTestUsesHealthAddr(call.Args[2]) {
						return true
					}
					checked++
					if !observedStartup {
						t.Errorf("%s: healthAddr readiness assertions must follow waitWorkerStarted in the same function body", function.Name.Name)
					}
					return true
				})
			}
		}
	}
	if checked == 0 {
		t.Fatal("startup contract found no healthAddr readiness assertions")
	}
}

func workerTestCallNamed(call *ast.CallExpr, name string) bool {
	identifier, ok := call.Fun.(*ast.Ident)
	return ok && identifier.Name == name
}

func workerTestUsesHealthAddr(expression ast.Expr) bool {
	found := false
	ast.Inspect(expression, func(node ast.Node) bool {
		if identifier, ok := node.(*ast.Ident); ok && identifier.Name == "healthAddr" {
			found = true
		}
		return true
	})
	return found
}
