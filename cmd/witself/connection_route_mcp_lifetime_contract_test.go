package main

import (
	"bytes"
	"context"
	"errors"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/witwave-ai/witself/internal/local"
)

func TestMCPConnectionRouteLifetimeContext(t *testing.T) {
	ctx, cancel := newRouteMCPLifetime()
	defer cancel()
	if _, ok := ctx.Deadline(); ok {
		t.Fatal("route MCP server lifetime must not have a deadline")
	}
	cfg := setupRouteRuntime(t)
	server := newRouteRuntimeServer(t)
	blackholeRouteDirectory(t)
	if err := local.SetRoute(cfg.Account, &local.Route{Kind: "local", Endpoint: server.URL}); err != nil {
		t.Fatal(err)
	}
	clientTransport, serverTransport := mcp.NewInMemoryTransports()
	done := make(chan error, 1)
	go func() { done <- runConfiguredMCPServer(ctx, cfg, mcpServerOptions{}, serverTransport) }()
	// Join even if the handshake or a tool call fails. Keep the client open until
	// cancellation stops the server, so clean client EOF cannot satisfy the proof.
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if !errors.Is(err, context.Canceled) {
				t.Errorf("route MCP cancellation returned %v, want context.Canceled", err)
			}
		case <-time.After(routeMCPStepTimeout):
			t.Error("route MCP cancellation did not stop the served session")
		}
	})
	connectCtx, connectCancel := context.WithTimeout(ctx, routeMCPStepTimeout)
	session, err := mcp.NewClient(&mcp.Implementation{Name: "route-cancel-test", Version: "1"}, nil).Connect(connectCtx, clientTransport, nil)
	connectCancel()
	if err != nil {
		t.Fatal(err)
	}
	callRouteMCP(t, session, "witself.self.show", false)
}

// Pin the fixture's lifetime and shutdown wiring, not elapsed wall time. Parsing
// and formatting ignores comments, whitespace, and Windows checkout CRLFs.
func TestMCPConnectionRouteLifetimeContract(t *testing.T) {
	fset := token.NewFileSet()
	source, err := parser.ParseFile(fset, "connection_route_mcp_hook_test.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	bodies := make(map[string]string)
	for _, decl := range source.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok {
			var body bytes.Buffer
			if err := format.Node(&body, fset, fn.Body); err != nil {
				t.Fatal(err)
			}
			bodies[fn.Name.Name] = body.String()
		}
	}
	for _, check := range []struct {
		name, function, required string
	}{
		{"cancel-only factory", "newRouteMCPLifetime", "return context.WithCancel(context.Background())"},
		{"lifetime factory wiring", "startRouteMCP", "ctx, cancel := newRouteMCPLifetime()"},
		{"server lifetime context", "startRouteMCP", "go func() { done <- runConfiguredMCPServer(ctx, cfg, mcpServerOptions{}, serverTransport) }()"},
		{"separate handshake bound", "startRouteMCP", "connectCtx, connectCancel := context.WithTimeout(ctx, routeMCPStepTimeout)"},
		{"handshake context wiring", "startRouteMCP", ".Connect(connectCtx, clientTransport, nil)"},
		{"cancel before bounded shutdown", "startRouteMCP", "t.Cleanup(func() {\n\t\tcancel()\n\t\tselect {"},
		{"reject deadline exceeded", "startRouteMCP", "case err := <-done:\n\t\t\tif err != nil && !errors.Is(err, context.Canceled) {\n\t\t\t\tt.Logf(\"MCP session lifetime before shutdown failure: %s\", time.Since(started))\n\t\t\t\tt.Errorf(\"MCP serve shutdown: %v\", err)\n\t\t\t}"},
		{"bounded shutdown wait", "startRouteMCP", "case <-time.After(routeMCPStepTimeout):\n\t\t\tt.Logf(\"MCP session lifetime before shutdown failure: %s\", time.Since(started))\n\t\t\tt.Error(\"MCP serve did not stop\")"},
		{"tool call bound", "callRouteMCP", "context.WithTimeout(context.Background(), routeMCPStepTimeout)"},
	} {
		t.Run(check.name, func(t *testing.T) {
			if !strings.Contains(bodies[check.function], check.required) {
				t.Fatalf("route MCP fixture lost %s", check.name)
			}
		})
	}
}
