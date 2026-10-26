package main

import (
	"bytes"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"reflect"
	"strings"
	"testing"

	"github.com/witwave-ai/witself/internal/server"
)

func TestConfigureAccountRequestBudget(t *testing.T) {
	const prefix = "WITSELF_ACCOUNT_REQUEST_BUDGET_"
	prior := server.AccountRequestBudgetConfig{
		Enabled: true, ReadPerMinute: 20, ReadBurst: 2,
		WritePerMinute: 10, WriteBurst: 1, MaxTrackedAccounts: 100,
	}
	for _, tc := range []struct {
		name    string
		env     map[string]string
		want    server.AccountRequestBudgetConfig
		wantLog string
		wantErr string
	}{
		{name: "absent"},
		{
			name: "disabled",
			env: map[string]string{
				prefix + "ENABLED":         "false",
				prefix + "READ_PER_MINUTE": "invalid",
			},
		},
		{
			name: "enabled",
			env: map[string]string{
				prefix + "ENABLED": "true", prefix + "READ_PER_MINUTE": "600",
				prefix + "READ_BURST": "60", prefix + "WRITE_PER_MINUTE": "120",
				prefix + "WRITE_BURST": "20", prefix + "MAX_ACCOUNTS": "500",
			},
			want: server.AccountRequestBudgetConfig{
				Enabled: true, ReadPerMinute: 600, ReadBurst: 60,
				WritePerMinute: 120, WriteBurst: 20, MaxTrackedAccounts: 500,
			},
			wantLog: "witself-server: account request budget enabled read_per_minute=600 read_burst=60 write_per_minute=120 write_burst=20 max_tracked_accounts=500\n",
		},
		{
			name:    "invalid",
			env:     map[string]string{prefix + "ENABLED": "true", prefix + "READ_PER_MINUTE": "invalid"},
			want:    prior,
			wantErr: prefix + "READ_PER_MINUTE must be an integer from 1 to 1000000",
		},
		{
			name: "enabled-with-plans-sorted",
			env: map[string]string{
				prefix + "ENABLED": "true", prefix + "READ_PER_MINUTE": "600",
				prefix + "READ_BURST": "60", prefix + "WRITE_PER_MINUTE": "120",
				prefix + "WRITE_BURST": "20", prefix + "MAX_ACCOUNTS": "500",
				prefix + "PLANS": `{"team":{"read_per_minute":300,"read_burst":5,"write_per_minute":120,"write_burst":4},"standard":{"read_per_minute":180,"read_burst":3,"write_per_minute":90,"write_burst":3},"free":{"read_per_minute":60,"read_burst":1,"write_per_minute":30,"write_burst":2}}`,
			},
			want: server.AccountRequestBudgetConfig{
				Enabled: true, ReadPerMinute: 600, ReadBurst: 60,
				WritePerMinute: 120, WriteBurst: 20, MaxTrackedAccounts: 500,
				Plans: map[string]server.AccountRequestBudgetTier{
					"free":     {ReadPerMinute: 60, ReadBurst: 1, WritePerMinute: 30, WriteBurst: 2},
					"standard": {ReadPerMinute: 180, ReadBurst: 3, WritePerMinute: 90, WriteBurst: 3},
					"team":     {ReadPerMinute: 300, ReadBurst: 5, WritePerMinute: 120, WriteBurst: 4},
				},
			},
			wantLog: "witself-server: account request budget enabled read_per_minute=600 read_burst=60 write_per_minute=120 write_burst=20 max_tracked_accounts=500\n" +
				"witself-server: account request budget plan=free read_per_minute=60 read_burst=1 write_per_minute=30 write_burst=2\n" +
				"witself-server: account request budget plan=standard read_per_minute=180 read_burst=3 write_per_minute=90 write_burst=3\n" +
				"witself-server: account request budget plan=team read_per_minute=300 read_burst=5 write_per_minute=120 write_burst=4\n",
		},
		{
			name: "disabled-ignores-invalid-plans",
			env:  map[string]string{prefix + "ENABLED": "false", prefix + "PLANS": "invalid"},
		},
		{
			name: "absent-enable-ignores-invalid-plans",
			env:  map[string]string{prefix + "PLANS": "invalid"},
		},
		{
			name: "invalid-plans-preserves-configuration-and-log",
			env: map[string]string{
				prefix + "ENABLED": "true", prefix + "READ_PER_MINUTE": "600",
				prefix + "READ_BURST": "60", prefix + "WRITE_PER_MINUTE": "120",
				prefix + "WRITE_BURST": "20", prefix + "PLANS": "[]",
			},
			want:    prior,
			wantErr: prefix + "PLANS must be a JSON object of plan ids to read and write budgets",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := server.Config{AccountRequestBudget: prior}
			var output bytes.Buffer
			err := configureAccountRequestBudget(&cfg, func(key string) (string, bool) {
				v, ok := tc.env[key]
				return v, ok
			}, &output)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("configureAccountRequestBudget: %v", err)
				}
			} else if err == nil || err.Error() != tc.wantErr {
				t.Fatalf("error = %v, want %q", err, tc.wantErr)
			}
			if !reflect.DeepEqual(cfg.AccountRequestBudget, tc.want) {
				t.Errorf("configuration = %+v, want %+v", cfg.AccountRequestBudget, tc.want)
			}
			if got := output.String(); got != tc.wantLog {
				t.Errorf("output = %q, want %q", got, tc.wantLog)
			}
		})
	}
	t.Run("plan-log-iteration-is-explicitly-sorted", func(t *testing.T) {
		// Parsing rebuilds the plan map in sorted insertion order, so JSON order
		// alone cannot deterministically expose an unsorted map iteration.
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, "account_request_budget.go", nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		var configure *ast.FuncDecl
		for _, decl := range file.Decls {
			if fn, ok := decl.(*ast.FuncDecl); ok && fn.Name.Name == "configureAccountRequestBudget" {
				configure = fn
				break
			}
		}
		if configure == nil {
			t.Fatal("configureAccountRequestBudget declaration not found")
		}
		var planLogRanges []*ast.RangeStmt
		ast.Inspect(configure.Body, func(node ast.Node) bool {
			loop, ok := node.(*ast.RangeStmt)
			if !ok {
				return true
			}
			ast.Inspect(loop.Body, func(node ast.Node) bool {
				call, ok := node.(*ast.CallExpr)
				if !ok || len(call.Args) < 2 {
					return true
				}
				selector, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || selector.Sel.Name != "Fprintf" {
					return true
				}
				pkg, ok := selector.X.(*ast.Ident)
				if !ok || pkg.Name != "fmt" {
					return true
				}
				if literal, ok := call.Args[1].(*ast.BasicLit); ok && literal.Kind == token.STRING &&
					strings.Contains(literal.Value, "witself-server: account request budget plan=") {
					planLogRanges = append(planLogRanges, loop)
				}
				return true
			})
			return true
		})
		if len(planLogRanges) != 1 {
			t.Fatalf("configureAccountRequestBudget has %d plan log ranges, want 1", len(planLogRanges))
		}
		var expression bytes.Buffer
		if err := format.Node(&expression, fset, planLogRanges[0].X); err != nil {
			t.Fatal(err)
		}
		const want = "slices.Sorted(maps.Keys(budget.Plans))"
		if got := expression.String(); got != want {
			t.Fatalf("plan log iteration = %s, want %s", got, want)
		}
	})
}

func TestServeConfiguresAccountRequestBudgetBeforeRun(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "main.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var serve, startup *ast.FuncDecl
	for _, decl := range file.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok {
			switch fn.Name.Name {
			case "serve":
				serve = fn
			case "serveWithStartup":
				startup = fn
			}
		}
	}
	if serve == nil || startup == nil {
		t.Fatal("serve or serveWithStartup declaration not found")
	}
	// Follow the production delegation, including the actual listener binding,
	// so moving startup behind its injectable seam does not weaken this guard.
	if len(serve.Body.List) != 1 {
		t.Fatal("serve must delegate directly to serveWithStartup")
	}
	result, ok := serve.Body.List[0].(*ast.ReturnStmt)
	if !ok || len(result.Results) != 1 {
		t.Fatal("serve must return serveWithStartup's result")
	}
	delegation, ok := result.Results[0].(*ast.CallExpr)
	if !ok || len(delegation.Args) != 1 {
		t.Fatal("serve must pass its production startup boundaries")
	}
	callee, ok := delegation.Fun.(*ast.Ident)
	if !ok || callee.Name != "serveWithStartup" {
		t.Fatal("serve must call serveWithStartup")
	}
	boundaries, ok := delegation.Args[0].(*ast.CompositeLit)
	if !ok {
		t.Fatal("serve must supply production startup boundaries")
	}
	listenerBound := false
	for _, entry := range boundaries.Elts {
		field, ok := entry.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		name, ok := field.Key.(*ast.Ident)
		if !ok || name.Name != "run" {
			continue
		}
		var expression bytes.Buffer
		if err := format.Node(&expression, fset, field.Value); err != nil {
			t.Fatal(err)
		}
		listenerBound = expression.String() == "server.Run"
	}
	if !listenerBound {
		t.Fatal("serve must bind the startup listener runner to server.Run")
	}
	var configureCalls, runCalls []*ast.CallExpr
	ast.Inspect(startup.Body, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		if fn, ok := call.Fun.(*ast.Ident); ok && fn.Name == "configureAccountRequestBudget" {
			configureCalls = append(configureCalls, call)
		}
		if selector, ok := call.Fun.(*ast.SelectorExpr); ok && selector.Sel.Name == "run" {
			if receiver, ok := selector.X.(*ast.Ident); ok && receiver.Name == "startup" {
				runCalls = append(runCalls, call)
			}
		}
		return true
	})
	if len(configureCalls) != 1 {
		t.Fatalf("serveWithStartup has %d configureAccountRequestBudget calls, want 1", len(configureCalls))
	}
	if len(runCalls) != 1 {
		t.Fatalf("serveWithStartup has %d listener runner calls, want 1", len(runCalls))
	}
	call := configureCalls[0]
	if len(call.Args) != 3 {
		t.Fatalf("configureAccountRequestBudget has %d arguments, want 3", len(call.Args))
	}
	address, ok := call.Args[0].(*ast.UnaryExpr)
	if !ok || address.Op != token.AND {
		t.Fatal("configureAccountRequestBudget first argument must be &cfg")
	}
	if id, ok := address.X.(*ast.Ident); !ok || id.Name != "cfg" {
		t.Fatal("configureAccountRequestBudget first argument must be &cfg")
	}
	lookup, ok := call.Args[1].(*ast.SelectorExpr)
	if !ok || lookup.Sel.Name != "LookupEnv" {
		t.Fatal("configureAccountRequestBudget second argument must be os.LookupEnv")
	}
	if pkg, ok := lookup.X.(*ast.Ident); !ok || pkg.Name != "os" {
		t.Fatal("configureAccountRequestBudget second argument must be os.LookupEnv")
	}
	if call.Pos() >= runCalls[0].Pos() {
		t.Fatal("configureAccountRequestBudget must run before server.Run")
	}
}
