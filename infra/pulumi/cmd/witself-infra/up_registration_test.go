package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pulumi/pulumi/sdk/v3/go/auto"
	"github.com/witwave-ai/witself/infra/pulumi/internal/fleet"
)

// Only stack selection and output reads are needed by the post-provisioning
// registration path. This command never invokes Pulumi or a cloud provider.
type upRegistrationPulumi struct {
	auto.PulumiCommand
	t     *testing.T
	calls [][]string
}

func (p *upRegistrationPulumi) Run(_ context.Context, _ string, _ io.Reader, _, _ []io.Writer, _ []string, args ...string) (string, string, int, error) {
	p.calls = append(p.calls, append([]string(nil), args...))
	if len(args) > 1 && args[0] == "stack" {
		switch args[1] {
		case "select":
			return "", "", 0, nil
		case "output":
			provision, backup := "[secret]", "[secret]"
			for _, arg := range args {
				if arg == "--show-secrets" {
					provision, backup = "witself_prv_synthetic-provision", "witself_bak_synthetic-backup"
				}
			}
			body, err := json.Marshal(map[string]string{
				"apiHost": "api.synthetic.invalid", "provisionToken": provision, "backupToken": backup,
			})
			if err != nil {
				p.t.Fatal("encode synthetic stack outputs")
			}
			return string(body), "", 0, nil
		}
	}
	p.t.Fatal("unexpected fake Pulumi command")
	return "", "", 1, fmt.Errorf("unexpected fake Pulumi command")
}

func TestUpRegistrationRegistryIdentity(t *testing.T) {
	const inventoryName = "civo-fixture-use1-serving"
	const mappedName = "civo-fixture-usw2-dev"
	for _, tc := range []struct {
		name, registryName                  string
		inventory, draining, ignoredMapping bool
	}{
		{name: "mapped inventory", registryName: mappedName, inventory: true},
		{name: "unmapped inventory", registryName: inventoryName, inventory: true},
		{name: "bare flags without inventory", registryName: inventoryName},
		{name: "bare flags ignore existing mapping", registryName: inventoryName, ignoredMapping: true},
		{name: "mapped draining", registryName: mappedName, inventory: true, draining: true},
		{name: "unmapped draining", registryName: inventoryName, inventory: true, draining: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("WITSELF_HOME", t.TempDir())
			t.Setenv("DSH_HOME", t.TempDir())
			t.Setenv("WITSELF_FLEET_TOKEN", "synthetic-fleet-token")
			configPath := filepath.Join(t.TempDir(), "missing-inventory.yaml")
			selector := ""
			if tc.inventory || tc.ignoredMapping {
				if tc.inventory {
					selector = inventoryName
				}
				entry := "{}"
				if tc.registryName != inventoryName || tc.ignoredMapping {
					entry = "{registry_name: " + mappedName + "}"
				}
				configPath = writeConfig(t, "version: 1\ncells:\n  "+inventoryName+": "+entry+"\n")
			}
			registryName, err := upRegistryName(inventoryName, selector, configPath)
			if err != nil {
				t.Fatal(err)
			}
			if registryName != tc.registryName {
				t.Fatalf("registry name = %q, want %q", registryName, tc.registryName)
			}

			var mu sync.Mutex
			var paths, registered []string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				paths = append(paths, r.Method+" "+r.URL.Path)
				switch {
				case r.Method == http.MethodPost && r.URL.Path == "/v1/cells":
					var cell fleet.Cell
					if err := json.NewDecoder(r.Body).Decode(&cell); err != nil {
						t.Error("decode synthetic registration")
						http.Error(w, "invalid registration", http.StatusBadRequest)
						return
					}
					registered = append(registered, cell.Name)
					if cell.Accepting == nil || *cell.Accepting != !tc.draining {
						t.Error("registration accepting state differs")
					}
					_ = json.NewEncoder(w).Encode(map[string]any{
						"schema_version": "witself.v0",
						"cell": map[string]any{
							"name": cell.Name, "accepting": !tc.draining,
							"backup_validation_target": false, "has_backup_token": true,
						},
					})
				case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, ":probe"):
					// Record every name, returning immediately even if a mutation
					// misroutes the probe; the exact path assertion catches it.
					_ = json.NewEncoder(w).Encode(fleet.ProbeResult{OK: true})
				case r.URL.Path == "/v1/placement:restore":
					// Exercise the compatibility path where restore's cell
					// identity appears in the request, unlike global placement.
					http.NotFound(w, r)
				case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, ":restore"):
					_ = json.NewEncoder(w).Encode(fleet.RestoreResult{})
				default:
					http.NotFound(w, r)
				}
			}))
			defer srv.Close()

			command := &upRegistrationPulumi{t: t}
			ctx := context.Background()
			stack, err := openCellStack(ctx, "local", "up", inventoryName, "",
				auto.WorkDir(t.TempDir()), auto.Pulumi(command))
			if err != nil {
				t.Fatal(err)
			}
			if stack.Name() != inventoryName {
				t.Fatalf("stack name = %q, want inventory name", stack.Name())
			}
			log, err := os.CreateTemp(t.TempDir(), "registration-stderr")
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = log.Close() }()
			oldStderr := os.Stderr
			os.Stderr = log
			defer func() { os.Stderr = oldStderr }()
			if _, err := registerCell(ctx, stack, srv.URL, "", inventoryName, registryName,
				"civo", "nyc1", "use1", "experimental", false, tc.draining); err != nil {
				t.Fatal(err)
			}
			status, err := os.ReadFile(log.Name())
			if err != nil {
				t.Fatal(err)
			}
			identity := inventoryName
			if tc.registryName != inventoryName {
				identity = fmt.Sprintf("%q (fleet registry entry %q)", inventoryName, tc.registryName)
			}
			wantStatus := fmt.Sprintf("cell %s registered with control plane %s\n", identity, srv.URL)
			if tc.draining {
				wantStatus = fmt.Sprintf("cell %s registered with control plane %s, accepting=false (register_draining). Open it with: witself-admin cells undrain %s; then remove register_draining from the cell record before the next up\n", identity, srv.URL, tc.registryName)
			}
			if string(status) != wantStatus {
				t.Fatalf("registration status = %q, want %q", status, wantStatus)
			}
			if !tc.draining {
				client, err := fleet.NewClient(srv.URL, "")
				if err != nil {
					t.Fatal(err)
				}
				if err := waitForCellHealthy(ctx, client, registryName, time.Second, time.Millisecond, time.Second); err != nil {
					t.Fatal(err)
				}
				if err := restoreCell(ctx, client, registryName, false); err != nil {
					t.Fatal(err)
				}
			}
			mu.Lock()
			gotPaths := append([]string(nil), paths...)
			gotRegistered := append([]string(nil), registered...)
			mu.Unlock()
			if !reflect.DeepEqual(gotRegistered, []string{tc.registryName}) {
				t.Fatalf("registered names = %v, want %q", gotRegistered, tc.registryName)
			}
			wantPaths := []string{"POST /v1/cells"}
			if !tc.draining {
				wantPaths = append(wantPaths, "POST /v1/cells/"+tc.registryName+":probe",
					"POST /v1/placement:restore", "POST /v1/cells/"+tc.registryName+":restore")
			}
			if !reflect.DeepEqual(gotPaths, wantPaths) {
				t.Fatalf("control-plane requests = %v, want %v", gotPaths, wantPaths)
			}
			wantCommands := [][]string{
				{"stack", "select", "--stack", inventoryName},
				{"stack", "output", "--json", "--stack", inventoryName},
				{"stack", "output", "--json", "--show-secrets", "--stack", inventoryName},
			}
			if !reflect.DeepEqual(command.calls, wantCommands) {
				t.Fatalf("Pulumi commands = %v, want inventory-name commands %v", command.calls, wantCommands)
			}
		})
	}
}

func TestUpRegistryNameRequiresSelectedInventory(t *testing.T) {
	t.Setenv("WITSELF_HOME", t.TempDir())
	t.Setenv("DSH_HOME", t.TempDir())
	const cellName = "civo-fixture-use1-serving"
	name, err := upRegistryName(cellName, cellName, filepath.Join(t.TempDir(), "missing-inventory.yaml"))
	if err == nil || name != "" {
		t.Fatalf("missing selected inventory: name = %q, error = %v", name, err)
	}
}

// Provider preflight and provisioning are deliberately outside the synthetic
// function tests above. Pin their actual run call sites so those tests cannot
// pass while main wires inventory names back into the fleet post-steps.
func TestUpRegistrationMainWiring(t *testing.T) {
	fset := token.NewFileSet()
	source, err := parser.ParseFile(fset, "main.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var runBody *ast.BlockStmt
	for _, decl := range source.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Name.Name == "run" {
			runBody = fn.Body
		}
	}
	if runBody == nil {
		t.Fatal("main.go has no run function")
	}
	wantArgs := map[string]map[int]string{
		"upRegistryName":     {0: "cellName", 1: "*cellSelector", 2: "*configPath"},
		"registerCell":       {4: "cellName", 5: "registryName"},
		"waitForCellHealthy": {2: "registryName"},
		"restoreCell":        {2: "registryName"},
		"openCellStack":      {3: "cellName"},
	}
	counts := map[string]int{}
	ast.Inspect(runBody, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		fn, ok := call.Fun.(*ast.Ident)
		if !ok {
			return true
		}
		args, relevant := wantArgs[fn.Name]
		if !relevant {
			return true
		}
		counts[fn.Name]++
		for position, want := range args {
			if position >= len(call.Args) {
				t.Errorf("%s argument %d is missing", fn.Name, position)
				continue
			}
			var got bytes.Buffer
			if err := printer.Fprint(&got, fset, call.Args[position]); err != nil {
				t.Fatal(err)
			}
			if got.String() != want {
				t.Errorf("%s argument %d = %s, want %s", fn.Name, position, got.String(), want)
			}
		}
		return true
	})
	for name := range wantArgs {
		if counts[name] != 1 {
			t.Errorf("run calls %s %d times, want once", name, counts[name])
		}
	}
}
