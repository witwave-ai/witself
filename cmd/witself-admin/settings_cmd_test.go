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
)

func TestSettingsAdminCLIRoundTrip(t *testing.T) {
	runner := settingsAdminTestRunner()
	reaper := map[string]any{"enabled": false}
	placement := map[string]any{"strategy": "weighted"}
	var calls []string
	var bodies []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.RequestURI())
		if r.Header.Get("Authorization") != "Bearer fleet-token" {
			t.Errorf("authorization = %q", r.Header.Get("Authorization"))
		}
		var body map[string]any
		if r.Method == http.MethodPost {
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode body: %v", err)
			}
			bodies = append(bodies, body)
		}
		response := map[string]any{"schema_version": "witself.v0"}
		switch r.URL.Path {
		case "/v1/placement-runner":
			for key, value := range body {
				runner[key] = value
			}
			response["placement_runner"] = runner
		case "/v1/reaper":
			if body != nil {
				reaper = body
			}
			response["reaper"] = reaper
		case "/v1/placement":
			if body != nil {
				placement = body
			}
			response["placement"] = placement
		case "/v1/placement:run":
			result := settingsAdminTestRunner()
			for key, value := range runner {
				result[key] = value
			}
			for key, value := range body {
				result[key] = value
			}
			result["enabled"] = true
			response["placement_runner"] = result
			response["restore"] = map[string]any{"restored": 2}
			response["rebalance"] = map[string]any{"moved": 1}
		default:
			t.Errorf("unexpected route %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		writeCellsAdminTestJSON(t, w, response)
	}))
	defer srv.Close()
	commands := []struct {
		args []string
		key  string
	}{
		{[]string{"show"}, ""},
		{[]string{"placement-runner", "show"}, "placement_runner"},
		{[]string{"placement-runner", "enable", "--yes"}, "placement_runner"},
		{[]string{"placement-runner", "disable", "--yes"}, "placement_runner"},
		{[]string{"placement-runner", "set", "--restore-archives=false", "--restore-batch=6", "--restore-any-region=true", "--rebalance=false", "--rebalance-batch=3", "--yes"}, "placement_runner"},
		{[]string{"placement-runner", "set", "--restore-batch=7", "--yes"}, "placement_runner"},
		{[]string{"placement-runner", "run", "--yes"}, "run"},
		{[]string{"placement-runner", "run", "--restore-archives=true", "--rebalance-batch=2", "--yes"}, "run"},
		{[]string{"reaper", "show"}, "reaper"},
		{[]string{"reaper", "enable", "--ttl-minutes=15", "--yes"}, "reaper"},
		{[]string{"reaper", "disable", "--yes"}, "reaper"},
		{[]string{"placement", "show"}, "placement"},
		{[]string{"placement", "set", "--strategy=pinned", "--pinned-cell=repair-cell", "--yes"}, "placement"},
		{[]string{"placement", "set", "--strategy=weighted", "--yes"}, "placement"},
	}
	for _, command := range commands {
		args := append([]string{"settings"}, command.args...)
		args = append(args, "--endpoint", srv.URL, "--fleet-token", "fleet-token", "--json")
		stdout, stderr, code := captureEmailAliasAdminCLI(t, func() int { return run(args) })
		if code != 0 {
			t.Fatalf("%v exit=%d stderr=%s", command.args, code, stderr)
		}
		var got map[string]any
		if err := json.Unmarshal([]byte(stdout), &got); err != nil {
			t.Fatalf("%v JSON: %v stdout=%s", command.args, err, stdout)
		}
		want := map[string]any{"schema_version": "witself.v0"}
		switch command.key {
		case "":
			want["placement_runner"], want["reaper"], want["placement"] = runner, reaper, placement
		case "placement_runner":
			want["placement_runner"] = runner
		case "reaper":
			want["reaper"] = reaper
		case "placement":
			want["placement"] = placement
		case "run":
			want["restore"], want["rebalance"] = map[string]any{"restored": float64(2)}, map[string]any{"moved": float64(1)}
			runConfig := make(map[string]any)
			for key, value := range runner {
				runConfig[key] = value
			}
			for key, value := range bodies[len(bodies)-1] {
				runConfig[key] = value
			}
			runConfig["enabled"] = true
			want["placement_runner"] = runConfig
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("%v JSON = %#v; want %#v", command.args, got, want)
		}
		if command.args[0] == "reaper" && command.args[1] == "enable" && !strings.Contains(stderr, "cells must serve :reap") {
			t.Fatalf("reaper enable missing rollout note: %q", stderr)
		}
	}
	wantCalls := []string{
		"GET /v1/placement-runner", "GET /v1/reaper", "GET /v1/placement", "GET /v1/placement-runner",
		"POST /v1/placement-runner", "POST /v1/placement-runner", "POST /v1/placement-runner", "POST /v1/placement-runner",
		"POST /v1/placement:run", "POST /v1/placement:run", "GET /v1/reaper", "POST /v1/reaper", "POST /v1/reaper",
		"GET /v1/placement", "POST /v1/placement", "POST /v1/placement",
	}
	if !reflect.DeepEqual(calls, wantCalls) {
		t.Fatalf("calls = %#v; want %#v", calls, wantCalls)
	}
	wantBodies := []map[string]any{
		{"enabled": true}, {"enabled": false},
		{"restore_archives": false, "restore_batch": float64(6), "restore_any_region": true, "rebalance": false, "rebalance_batch": float64(3)},
		{"restore_batch": float64(7)}, {}, {"restore_archives": true, "rebalance_batch": float64(2)},
		{"enabled": true, "ttl_minutes": float64(15)}, {"enabled": false},
		{"strategy": "pinned", "pinned_cell": "repair-cell"}, {"strategy": "weighted"},
	}
	if !reflect.DeepEqual(bodies, wantBodies) {
		t.Fatalf("bodies = %#v; want %#v", bodies, wantBodies)
	}
}

func TestSettingsAdminCLIRejectsInvalidArgumentsBeforeRequests(t *testing.T) {
	cases := []struct {
		args []string
		want string
	}{
		{[]string{"placement-runner", "enable"}, "requires --yes"},
		{[]string{"placement-runner", "disable"}, "requires --yes"},
		{[]string{"placement-runner", "set", "--restore-batch=2"}, "requires --yes"},
		{[]string{"placement-runner", "run"}, "requires --yes"},
		{[]string{"reaper", "enable", "--ttl-minutes=2"}, "requires --yes"},
		{[]string{"reaper", "disable"}, "requires --yes"},
		{[]string{"placement", "set", "--strategy=weighted"}, "requires --yes"},
		{[]string{"reaper", "enable", "--yes"}, "--ttl-minutes must be at least 1"},
		{[]string{"reaper", "enable", "--ttl-minutes=0", "--yes"}, "--ttl-minutes must be at least 1"},
		{[]string{"reaper", "enable", "--ttl-minutes=-1", "--yes"}, "--ttl-minutes must be at least 1"},
		{[]string{"reaper", "enable", "--ttl-minutes=0.5", "--yes"}, "--ttl-minutes must be at least 1"},
		{[]string{"reaper", "enable", "--ttl-minutes=NaN", "--yes"}, "--ttl-minutes must be at least 1 and finite"},
		{[]string{"reaper", "enable", "--ttl-minutes=+Inf", "--yes"}, "--ttl-minutes must be at least 1 and finite"},
		{[]string{"reaper", "enable", "--ttl-minutes=-Inf", "--yes"}, "--ttl-minutes must be at least 1 and finite"},
		{[]string{"placement", "set", "--strategy=random", "--yes"}, "--strategy must be weighted or pinned"},
		{[]string{"placement", "set", "--yes"}, "--strategy must be weighted or pinned"},
		{[]string{"placement", "set", "--strategy=pinned", "--yes"}, "--pinned-cell"},
		{[]string{"placement", "set", "--strategy=pinned", "--pinned-cell=BAD/cell", "--yes"}, "--pinned-cell"},
		{[]string{"placement", "set", "--strategy=weighted", "--pinned-cell=repair-cell", "--yes"}, "--pinned-cell requires --strategy=pinned"},
		{[]string{"placement-runner", "set", "--yes"}, "set requires at least one"},
		{[]string{"placement-runner", "set", "--restore-batch=0", "--yes"}, "--restore-batch must be between 1 and 10"},
		{[]string{"placement-runner", "run", "--restore-batch=11", "--yes"}, "--restore-batch must be between 1 and 10"},
		{[]string{"placement-runner", "set", "--rebalance-batch=0", "--yes"}, "--rebalance-batch must be between 1 and 5"},
		{[]string{"placement-runner", "run", "--rebalance-batch=6", "--yes"}, "--rebalance-batch must be between 1 and 5"},
		{[]string{"show", "extra"}, "unexpected positional arguments"},
		{[]string{"placement-runner", "run", "--yes=false"}, "requires --yes"},
		{[]string{"placement-runner", "enable", "--restore-batch=4", "--yes"}, "flag provided but not defined"},
		{[]string{"reaper", "disable", "--ttl-minutes=3", "--yes"}, "flag provided but not defined"},
	}
	for _, tc := range cases {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			calls := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { calls++; w.WriteHeader(http.StatusNoContent) }))
			defer srv.Close()
			args := append([]string{"settings"}, tc.args...)
			args = append(args, "--endpoint", srv.URL, "--fleet-token", "fleet-token")
			stdout, stderr, code := captureEmailAliasAdminCLI(t, func() int { return run(args) })
			if code != 2 || calls != 0 || stdout != "" || !strings.Contains(stderr, tc.want) {
				t.Fatalf("exit=%d calls=%d stdout=%q stderr=%q; want %q", code, calls, stdout, stderr, tc.want)
			}
		})
	}
}

func TestSettingsAdminCLIHelpRequiresNoCredentials(t *testing.T) {
	t.Setenv("WITSELF_HOME", t.TempDir())
	t.Setenv("WITSELF_FLEET_TOKEN", "")
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { calls++; w.WriteHeader(http.StatusUnauthorized) }))
	defer srv.Close()
	t.Setenv("WITSELF_CONTROL_PLANE", srv.URL)
	for _, path := range []string{"", "show", "placement-runner", "placement-runner show", "placement-runner enable", "placement-runner disable", "placement-runner set", "placement-runner run", "reaper", "reaper show", "reaper enable", "reaper disable", "placement", "placement show", "placement set"} {
		t.Run(path, func(t *testing.T) {
			args := append([]string{"settings"}, strings.Fields(path)...)
			args = append(args, "--help")
			stdout, stderr, code := captureEmailAliasAdminCLI(t, func() int { return run(args) })
			if code != 0 || !strings.Contains(stdout+stderr, "settings") || strings.Contains(stdout+stderr, "Usage of cells ") {
				t.Fatalf("exit=%d stdout=%q stderr=%q", code, stdout, stderr)
			}
		})
	}
	if calls != 0 {
		t.Fatalf("help made %d requests", calls)
	}
}

func TestSettingsAdminCLIAuthFailures(t *testing.T) {
	for _, path := range []string{"show", "placement-runner show", "placement-runner enable --yes", "placement-runner run --yes", "reaper show", "reaper enable --ttl-minutes=2 --yes", "placement show", "placement set --strategy=weighted --yes"} {
		t.Run(path, func(t *testing.T) {
			calls := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls++
				w.WriteHeader(http.StatusUnauthorized)
				writeCellsAdminTestJSON(t, w, map[string]any{"error": "fleet settings access refused"})
			}))
			defer srv.Close()
			args := append([]string{"settings"}, strings.Fields(path)...)
			args = append(args, "--endpoint", srv.URL, "--token", "bad-token", "--json")
			stdout, stderr, code := captureEmailAliasAdminCLI(t, func() int { return run(args) })
			if code != 1 || calls != 1 || stdout != "" || !strings.Contains(stderr, "fleet settings access refused") {
				t.Fatalf("exit=%d calls=%d stdout=%q stderr=%q", code, calls, stdout, stderr)
			}
		})
	}
}

func TestSettingsAdminCLIRunStepErrors(t *testing.T) {
	for _, key := range []string{"restore_error", "rebalance_error"} {
		t.Run(key, func(t *testing.T) {
			failure := map[string]any{"status": float64(503), "body": map[string]any{"error": "step unavailable"}}
			runner := settingsAdminTestRunner()
			runner["enabled"] = true
			response := map[string]any{"schema_version": "witself.v0", "placement_runner": runner, key: failure}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || r.URL.Path != "/v1/placement:run" {
					t.Errorf("unexpected route %s %s", r.Method, r.URL.Path)
				}
				writeCellsAdminTestJSON(t, w, response)
			}))
			defer srv.Close()
			for _, jsonOutput := range []bool{true, false} {
				args := []string{"settings", "placement-runner", "run", "--yes", "--endpoint", srv.URL, "--fleet-token", "fleet-token"}
				if jsonOutput {
					args = append(args, "--json")
				}
				stdout, stderr, code := captureEmailAliasAdminCLI(t, func() int { return run(args) })
				if code != 1 || !strings.Contains(stdout, key) || !strings.Contains(stdout, "step unavailable") {
					t.Fatalf("json=%v exit=%d stdout=%q stderr=%q", jsonOutput, code, stdout, stderr)
				}
				if jsonOutput {
					var got map[string]any
					if err := json.Unmarshal([]byte(stdout), &got); err != nil || !reflect.DeepEqual(got, response) {
						t.Fatalf("run error JSON = %#v, %v; want %#v", got, err, response)
					}
				}
			}
		})
	}
}

func TestSettingsAdminCLIRunAccountResults(t *testing.T) {
	for _, step := range []struct{ section, entries, disableOther string }{
		{"restore", "restored", "--rebalance=false"},
		{"rebalance", "rebalanced", "--restore-archives=false"},
	} {
		for _, tc := range []struct {
			name string
			oks  []bool
			code int
		}{
			{"failed", []bool{false}, 1},
			{"mixed-failure-first", []bool{false, true}, 1},
			{"mixed-failure-last", []bool{true, false}, 1},
			{"successful", []bool{true, true}, 0},
			{"empty", []bool{}, 0},
		} {
			t.Run(step.section+"/"+tc.name, func(t *testing.T) {
				entries := make([]map[string]any, 0, len(tc.oks))
				for i, ok := range tc.oks {
					entry := map[string]any{"account_id": []string{"account-a", "account-b"}[i], "ok": ok}
					if step.section == "restore" {
						entry["cell"] = "target-cell"
					} else {
						entry["from_cell"] = "source-cell"
						entry["to_cell"] = "target-cell"
					}
					if !ok {
						entry["error"] = "lifecycle coordinator returned 503"
					}
					entries = append(entries, entry)
				}
				// Remaining work alone is not failure: a successful bounded batch
				// can leave accounts for a later pass.
				batch := map[string]any{"schema_version": "witself.v0", step.entries: entries, "remaining": 1}
				runner := settingsAdminTestRunner()
				runner["enabled"] = true
				if step.section == "restore" {
					runner["rebalance"] = false
				} else {
					runner["restore_archives"] = false
				}
				response := map[string]any{"schema_version": "witself.v0", "placement_runner": runner, step.section: batch}
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Method != http.MethodPost || r.URL.Path != "/v1/placement:run" {
						t.Errorf("unexpected route %s %s", r.Method, r.URL.Path)
					}
					writeCellsAdminTestJSON(t, w, response)
				}))
				defer srv.Close()
				for _, jsonOutput := range []bool{true, false} {
					args := []string{"settings", "placement-runner", "run", step.disableOther, "--yes", "--endpoint", srv.URL, "--fleet-token", "fleet-token"}
					if jsonOutput {
						args = append(args, "--json")
					}
					stdout, stderr, code := captureEmailAliasAdminCLI(t, func() int { return run(args) })
					if code != tc.code {
						t.Errorf("json=%v exit=%d; want %d; stdout=%q stderr=%q", jsonOutput, code, tc.code, stdout, stderr)
					}
					if jsonOutput {
						var got, want any
						body, err := json.Marshal(response)
						if err != nil {
							t.Fatal(err)
						}
						if err := json.Unmarshal(body, &want); err != nil {
							t.Fatal(err)
						}
						if err := json.Unmarshal([]byte(stdout), &got); err != nil || !reflect.DeepEqual(got, want) {
							t.Fatalf("run JSON = %#v, %v; want complete response %#v", got, err, want)
						}
					} else {
						body, err := json.Marshal(batch)
						if err != nil {
							t.Fatal(err)
						}
						if !strings.Contains(stdout, string(body)) {
							t.Fatalf("table lost batch results: %q; want %s", stdout, body)
						}
					}
				}
			})
		}
	}
}

func TestSettingsAdminCLINeverExposesForceOrUnsafeRoutes(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { calls++; w.WriteHeader(http.StatusNoContent) }))
	defer srv.Close()
	for _, path := range []string{"placement-runner restore", "placement-runner rebalance", "placement-runner purge", "placement-runner evacuate", "placement-runner run --force", "reaper enable --ttl-minutes=2 --force", "placement set --strategy=weighted --force", "purge"} {
		t.Run(path, func(t *testing.T) {
			args := append([]string{"settings"}, strings.Fields(path)...)
			args = append(args, "--yes", "--endpoint", srv.URL, "--fleet-token", "fleet-token")
			stdout, stderr, code := captureEmailAliasAdminCLI(t, func() int { return run(args) })
			if code != 2 || stdout != "" || !strings.Contains(stderr, "witself-admin settings:") {
				t.Fatalf("exit=%d stdout=%q stderr=%q", code, stdout, stderr)
			}
		})
	}
	if calls != 0 {
		t.Fatalf("unsafe commands made %d requests", calls)
	}
}

func TestSettingsAdminCLIFleetCredentialResolution(t *testing.T) {
	for _, source := range []string{"alias", "environment", "file", "managed"} {
		t.Run(source, func(t *testing.T) {
			root := t.TempDir()
			t.Setenv("WITSELF_HOME", root)
			t.Setenv("WITSELF_FLEET_TOKEN", "")
			calls := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.Header.Get("Authorization") != "Bearer settings-fleet-secret" {
					t.Errorf("incorrect fleet authorization")
				}
				writeCellsAdminTestJSON(t, w, map[string]any{"schema_version": "witself.v0", "reaper": map[string]any{"enabled": false}})
			}))
			defer srv.Close()
			t.Setenv("WITSELF_CONTROL_PLANE", srv.URL)
			args := []string{"settings", "reaper", "show", "--json"}
			switch source {
			case "alias":
				args = append(args, "--token", "settings-fleet-secret")
			case "environment":
				t.Setenv("WITSELF_FLEET_TOKEN", "settings-fleet-secret")
			case "file", "managed":
				path := filepath.Join(root, "fleet.token")
				if source == "managed" {
					if err := os.Mkdir(filepath.Join(root, "tokens"), 0o700); err != nil {
						t.Fatal(err)
					}
					path = filepath.Join(root, "tokens", "fleet.token")
				} else {
					args = append(args, "--token-file", path)
				}
				if err := os.WriteFile(path, []byte("settings-fleet-secret\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			stdout, stderr, code := captureEmailAliasAdminCLI(t, func() int { return run(args) })
			if code != 0 || calls != 1 || strings.Contains(stdout+stderr, "settings-fleet-secret") {
				t.Fatalf("exit=%d calls=%d stdout=%q stderr=%q", code, calls, stdout, stderr)
			}
		})
	}
}

func TestSettingsAdminCLIShowTable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		response := map[string]any{"schema_version": "witself.v0"}
		switch r.URL.Path {
		case "/v1/placement-runner":
			response["placement_runner"] = settingsAdminTestRunner()
		case "/v1/reaper":
			response["reaper"] = map[string]any{"enabled": true, "ttl_minutes": 17}
		case "/v1/placement":
			response["placement"] = map[string]any{"strategy": "pinned", "pinned_cell": "table-cell"}
		default:
			t.Errorf("unexpected route %s", r.URL.Path)
		}
		writeCellsAdminTestJSON(t, w, response)
	}))
	defer srv.Close()
	stdout, stderr, code := captureEmailAliasAdminCLI(t, func() int {
		return run([]string{"settings", "show", "--endpoint", srv.URL, "--fleet-token", "fleet-token"})
	})
	if code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, stderr)
	}
	if !strings.Contains(stderr, "section\tsetting\tvalue") || strings.Contains(stdout, "SECTION") || strings.Contains(stdout, "section\tsetting\tvalue") {
		t.Fatalf("piped table header must go to stderr: stdout=%q stderr=%q", stdout, stderr)
	}
	for _, want := range []string{"placement_runner", "restore_archives", "restore_batch", "restore_any_region", "rebalance", "rebalance_batch", "reaper", "ttl_minutes", "17", "placement", "pinned", "table-cell"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("table missing %q: %s", want, stdout)
		}
	}
}

func settingsAdminTestRunner() map[string]any {
	return map[string]any{"enabled": false, "restore_archives": true, "restore_batch": float64(4), "restore_any_region": false, "rebalance": true, "rebalance_batch": float64(1)}
}

func TestSettingsAdminCLIReaperFractional(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		json bool
	}{
		{"reaper-json", []string{"reaper", "show"}, true},
		{"reaper-table", []string{"reaper", "show"}, false},
		{"all-json", []string{"show"}, true},
		{"all-table", []string{"show"}, false},
		{"enable-json", []string{"reaper", "enable", "--ttl-minutes=1.5", "--yes"}, true},
		{"enable-table", []string{"reaper", "enable", "--ttl-minutes=1.5", "--yes"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls []string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls = append(calls, r.Method+" "+r.URL.Path)
				if r.Header.Get("Authorization") != "Bearer fleet-token" {
					t.Error("missing fleet authorization")
				}
				response := map[string]any{"schema_version": "witself.v0"}
				switch r.URL.Path {
				case "/v1/reaper":
					if r.Method == http.MethodPost {
						var body map[string]any
						if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
							t.Errorf("decode reaper request: %v", err)
						}
						if !reflect.DeepEqual(body, map[string]any{"enabled": true, "ttl_minutes": 1.5}) {
							t.Errorf("fractional reaper request = %#v", body)
						}
					}
					response["reaper"] = map[string]any{"enabled": true, "ttl_minutes": 1.5}
				case "/v1/placement-runner":
					response["placement_runner"] = settingsAdminTestRunner()
				case "/v1/placement":
					response["placement"] = map[string]any{"strategy": "weighted"}
				default:
					t.Errorf("unexpected route %s", r.URL.Path)
				}
				writeCellsAdminTestJSON(t, w, response)
			}))
			defer srv.Close()
			args := append([]string{"settings"}, tc.args...)
			args = append(args, "--endpoint", srv.URL, "--fleet-token", "fleet-token")
			if tc.json {
				args = append(args, "--json")
			}
			stdout, stderr, code := captureEmailAliasAdminCLI(t, func() int { return run(args) })
			if code != 0 {
				t.Fatalf("valid fractional reaper command failed: exit=%d stderr=%s", code, stderr)
			}
			wantCalls := []string{"GET /v1/reaper"}
			if tc.args[0] == "show" {
				wantCalls = []string{"GET /v1/placement-runner", "GET /v1/reaper", "GET /v1/placement"}
			} else if tc.args[1] == "enable" {
				wantCalls = []string{"POST /v1/reaper"}
				if !strings.Contains(stderr, "cells must serve :reap") {
					t.Error("reaper enable missing rollout note")
				}
			}
			if !reflect.DeepEqual(calls, wantCalls) {
				t.Fatalf("calls = %v, want %v", calls, wantCalls)
			}
			if tc.json {
				var got map[string]any
				if err := json.Unmarshal([]byte(stdout), &got); err != nil {
					t.Fatalf("decode output: %v", err)
				}
				want := map[string]any{"schema_version": "witself.v0", "reaper": map[string]any{"enabled": true, "ttl_minutes": 1.5}}
				if tc.args[0] == "show" {
					want["placement_runner"] = settingsAdminTestRunner()
					want["placement"] = map[string]any{"strategy": "weighted"}
				}
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("fractional JSON = %#v, want %#v", got, want)
				}
			} else if !strings.Contains(stdout, "ttl_minutes") || !strings.Contains(stdout, "1.5") {
				t.Fatalf("table lost fractional TTL: %s", stdout)
			}
		})
	}
}
