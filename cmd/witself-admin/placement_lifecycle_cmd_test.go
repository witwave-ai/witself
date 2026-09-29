package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/witwave-ai/witself/internal/client"
)

func lifecycleCLIFixture(t *testing.T, name string) map[string]any {
	t.Helper()
	data, err := os.ReadFile("../../infra/cloudflare/control-plane/test/fixtures/account-lifecycle-status.golden.json")
	if err != nil {
		t.Fatal(err)
	}
	var v struct {
		Cases map[string]map[string]any `json:"cases"`
	}
	if err := json.Unmarshal(data, &v); err != nil {
		t.Fatal(err)
	}
	return v.Cases[name]
}
func lifecycleCLIEnvironment(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("WITSELF_HOME", home)
	t.Setenv("WITSELF_FLEET_TOKEN", "fixture-fleet")
	return home
}
func TestPlacementLifecycleOutput(t *testing.T) {
	for _, name := range []string{"import_running", "export_running", "export_unstreamed", "restore_completed", "quarantined", "uninitialized_resident", "needs_operator", "unavailable", "drift", "idle"} {
		t.Run(name, func(t *testing.T) {
			lifecycleCLIEnvironment(t)
			fixture := name
			if name == "export_unstreamed" || name == "needs_operator" || name == "unavailable" || name == "drift" || name == "idle" {
				fixture = "import_running"
			}
			body := lifecycleCLIFixture(t, fixture)
			switch name {
			case "export_unstreamed":
				body["operation"].(map[string]any)["export_job"] = map[string]any{"stream_attempts": 1, "verify_attempts": 0, "streamed": false}
			case "needs_operator":
				op := body["operation"].(map[string]any)
				op["retryable"] = false
				op["last_error"] = "invalid or corrupt archive"
			case "unavailable":
				body["directory"] = nil
			case "drift":
				body["directory"] = map[string]any{"live_cell": "other", "archived_cell": "source"}
			case "idle":
				body["operation"] = nil
				body["driver_active"] = false
				body["alarm_at"] = nil
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer fixture-fleet" {
					t.Error("wrong fixture credential")
				}
				_ = json.NewEncoder(w).Encode(body)
			}))
			defer server.Close()
			for _, asJSON := range []bool{false, true} {
				args := []string{"lifecycle", "--account-id", "acct_runtime", "--endpoint", server.URL}
				if asJSON {
					args = append(args, "--json")
				}
				out, stderr, code := captureEmailAliasAdminCLI(t, func() int { return placementCmd(args) })
				wantStderr := "field\tvalue\n"
				if asJSON {
					wantStderr = ""
				}
				if code != 0 || stderr != wantStderr {
					t.Fatalf("command failed: %d %s", code, stderr)
				}
				if strings.Contains(out, "fixture-fleet") {
					t.Fatal("credential printed")
				}
				if asJSON {
					var decoded map[string]any
					if err := json.Unmarshal([]byte(out), &decoded); err != nil {
						t.Fatal(err)
					}
					if len(decoded) != 1 || decoded["account_lifecycle"] == nil {
						t.Fatal("wrong JSON envelope")
					}
					actual, err := json.Marshal(decoded["account_lifecycle"])
					if err != nil {
						t.Fatal(err)
					}
					expected, err := json.Marshal(body)
					if err != nil {
						t.Fatal(err)
					}
					var got, want client.FleetAccountLifecycle
					if err := json.Unmarshal(actual, &got); err != nil {
						t.Fatal(err)
					}
					if err := json.Unmarshal(expected, &want); err != nil {
						t.Fatal(err)
					}
					if want.Operation != nil && (got.Operation == nil || !reflect.DeepEqual(got.Operation.ExportJob, want.Operation.ExportJob)) {
						t.Fatal("JSON export job differs")
					}
					continue
				}
				for _, label := range []string{"account", "initialized", "location", "directory", "epoch", "revision", "driver", "alarm", "operation", "export job", "import job", "last completed", "quarantine", "observed"} {
					if !strings.Contains(out, label) {
						t.Fatalf("missing row %s", label)
					}
				}
				if strings.Count(out, "export job") != 1 || strings.Index(out, "export job") >= strings.Index(out, "import job") {
					t.Fatal("export row must occur once before import row")
				}
				wantExport := "none"
				switch name {
				case "export_running":
					wantExport = "stream attempt 2; verify attempt 1; streamed yes at 2026-09-28T12:00:00Z; stream ms 1000"
				case "export_unstreamed":
					wantExport = "stream attempt 1; verify attempt 0; streamed no at -; stream ms -"
				}
				for _, line := range strings.Split(out, "\n") {
					if strings.HasPrefix(line, "export job") && strings.TrimSpace(strings.TrimPrefix(line, "export job")) != wantExport {
						t.Fatal("incorrect export row")
					}
				}
				switch name {
				case "import_running":
					for _, text := range []string{"attempt 1; next alarm action poll; retry at -; first started 2026-09-28T12:00:00Z", "source -> target", "cell import_target"} {
						if !strings.Contains(out, text) {
							t.Fatalf("missing table value %s", text)
						}
					}
				case "needs_operator":
					if !strings.Contains(out, "needs operator") {
						t.Fatal("missing attention")
					}
				case "unavailable":
					if !strings.Contains(out, "unavailable") {
						t.Fatal("missing unavailable")
					}
				case "drift":
					if !strings.Contains(out, "live other; archived source") {
						t.Fatal("missing drift")
					}
				case "idle":
					if strings.Contains(out, "evacuation") || strings.Contains(out, "attention") {
						t.Fatal("operation-only rows for idle account")
					}
				}
			}
		})
	}
}

func TestPlacementLifecycleCredentials(t *testing.T) {
	for _, source := range []string{"environment", "file", "managed", "bad-file", "empty-file", "empty-path", "missing"} {
		t.Run(source, func(t *testing.T) {
			home := lifecycleCLIEnvironment(t)
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.Header.Get("Authorization") != "Bearer fixture-fleet" {
					t.Error("incorrect credential")
				}
				_ = json.NewEncoder(w).Encode(lifecycleCLIFixture(t, "uninitialized_resident"))
			}))
			defer server.Close()
			args := []string{"--account-id", "acct_runtime", "--endpoint", server.URL}
			switch source {
			case "file", "empty-file":
				path := filepath.Join(home, "fixture-credential")
				value := "fixture-fleet\n"
				if source == "empty-file" {
					value = "\n"
				}
				if err := os.WriteFile(path, []byte(value), 0o600); err != nil {
					t.Fatal(err)
				}
				args = append(args, "--token-file", path)
			case "managed":
				t.Setenv("WITSELF_FLEET_TOKEN", "")
				path, err := managedTokenPath("fleet.token")
				if err != nil {
					t.Fatal(err)
				}
				if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte("fixture-fleet\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "bad-file":
				args = append(args, "--token-file", filepath.Join(home, "missing"))
			case "empty-path":
				args = append(args, "--token-file", "")
			case "missing":
				t.Setenv("WITSELF_FLEET_TOKEN", "")
			}
			out, stderr, code := captureEmailAliasAdminCLI(t, func() int { return placementLifecycleCmd(args) })
			fail := source == "bad-file" || source == "empty-file" || source == "empty-path" || source == "missing"
			if fail {
				if code != 2 || calls != 0 || out != "" || strings.TrimSpace(stderr) != "fleet token unavailable; use --token-file or the managed fleet token" {
					t.Fatal("missing credential fallback or unsafe diagnostic")
				}
			} else if code != 0 || calls != 1 {
				t.Fatal("credential source failed")
			}
		})
	}
}

func TestPlacementLifecycleArgumentsAndHelp(t *testing.T) {
	for _, args := range [][]string{{}, {"--account-id", "bad/id"}, {"--account-id", "bad space"}, {"--account-id", strings.Repeat("a", 129)}, {"--account-id", "acct_runtime", "positional"}, {"--account-id", "acct_runtime", "--fleet-token", "fixture-value"}, {"--account-id", "acct_runtime", "--token", "fixture-value"}, {"--help"}} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			lifecycleCLIEnvironment(t)
			t.Setenv("WITSELF_FLEET_TOKEN", "")
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls++ }))
			defer server.Close()
			runArgs := append([]string{"--endpoint", server.URL}, args...)
			out, stderr, code := captureEmailAliasAdminCLI(t, func() int { return placementLifecycleCmd(runArgs) })
			want := 2
			if len(args) == 1 && args[0] == "--help" {
				want = 0
			}
			if code != want || calls != 0 {
				t.Fatal("arguments or help attempted request")
			}
			if strings.Contains(out+stderr, "fixture-value") {
				t.Fatal("argv token printed")
			}
		})
	}
}

func TestPlacementLifecycleFixedErrors(t *testing.T) {
	for _, status := range []int{401, 404, 429, 500} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			lifecycleCLIEnvironment(t)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(status)
				_ = json.NewEncoder(w).Encode(map[string]any{"error": "private-backend-marker fixture-fleet"})
			}))
			defer server.Close()
			out, stderr, code := captureEmailAliasAdminCLI(t, func() int {
				return placementLifecycleCmd([]string{"--account-id", "acct_runtime", "--endpoint", server.URL})
			})
			want := "lifecycle status request failed; inspect control-plane diagnostics"
			if status == 401 {
				want = "not authorized (check the fleet token)"
			}
			if status == 404 {
				want = "account not found, or the control plane predates the lifecycle status route"
			}
			if code != 1 || out != "" || strings.TrimSpace(stderr) != want {
				t.Fatal("non-fixed failure output")
			}
		})
	}
}
