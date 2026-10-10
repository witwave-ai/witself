package main

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/token"
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
			if cfg.AccountRequestBudget != tc.want {
				t.Errorf("configuration = %+v, want %+v", cfg.AccountRequestBudget, tc.want)
			}
			if got := output.String(); got != tc.wantLog {
				t.Errorf("output = %q, want %q", got, tc.wantLog)
			}
		})
	}
}

func TestServeConfiguresAccountRequestBudgetBeforeRun(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "main.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var serve *ast.FuncDecl
	for _, decl := range file.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Name.Name == "serve" {
			serve = fn
			break
		}
	}
	if serve == nil {
		t.Fatal("serve declaration not found")
	}
	var configureCalls, runCalls []*ast.CallExpr
	ast.Inspect(serve.Body, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		if fn, ok := call.Fun.(*ast.Ident); ok && fn.Name == "configureAccountRequestBudget" {
			configureCalls = append(configureCalls, call)
		}
		if selector, ok := call.Fun.(*ast.SelectorExpr); ok && selector.Sel.Name == "Run" {
			if pkg, ok := selector.X.(*ast.Ident); ok && pkg.Name == "server" {
				runCalls = append(runCalls, call)
			}
		}
		return true
	})
	if len(configureCalls) != 1 {
		t.Fatalf("serve has %d configureAccountRequestBudget calls, want 1", len(configureCalls))
	}
	if len(runCalls) != 1 {
		t.Fatalf("serve has %d server.Run calls, want 1", len(runCalls))
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
