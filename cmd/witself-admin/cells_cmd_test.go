package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestCellsAdminCLIRepairRoundTrip(t *testing.T) {
	var calls []string
	var registrations []map[string]any
	var acceptingUpdates []map[string]any
	cell := cellsAdminTestCell("repair-cell", false)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.RequestURI())
		if got := r.Header.Get("Authorization"); got != "Bearer fleet-token" {
			t.Errorf("authorization = %q", got)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/cells":
			writeCellsAdminTestJSON(t, w, map[string]any{
				"schema_version": "witself.v0", "cells": []map[string]any{cell},
			})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/cells":
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode registration: %v", err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			registrations = append(registrations, body)
			cell = cellsAdminTestCell("repair-cell", body["accepting"] != false)
			writeCellsAdminTestJSON(t, w, map[string]any{"schema_version": "witself.v0", "cell": cell})
		case r.Method == http.MethodPatch && r.URL.Path == "/v1/cells/repair-cell":
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode accepting update: %v", err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			acceptingUpdates = append(acceptingUpdates, body)
			cell["accepting"] = body["accepting"]
			writeCellsAdminTestJSON(t, w, map[string]any{"schema_version": "witself.v0", "cell": cell})
		case r.Method == http.MethodDelete && r.URL.Path == "/v1/cells/repair-cell":
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.RequestURI())
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	commands := []struct {
		args      []string
		accepting bool
	}{
		{[]string{"show", "repair-cell"}, false},
		{[]string{"register", "repair-cell", "--cell-endpoint", "https://repair.example", "--cloud", "civo", "--region", "NYC1", "--region-code", "use1", "--channel", "stable", "--weight", "2", "--accepting=false"}, false},
		{[]string{"drain", "repair-cell"}, false},
		{[]string{"undrain", "repair-cell"}, true},
		{[]string{"drain", "repair-cell"}, false},
		{[]string{"deregister", "repair-cell", "--yes", "--yes-cell=repair-cell"}, false},
	}
	for _, command := range commands {
		args := append([]string{"cells"}, command.args...)
		args = append(args, "--endpoint", srv.URL, "--fleet-token", "fleet-token", "--json")
		stdout, stderr, code := captureEmailAliasAdminCLI(t, func() int { return run(args) })
		if code != 0 {
			t.Fatalf("%s exit=%d stderr=%s", command.args[0], code, stderr)
		}
		var envelope map[string]any
		if err := json.Unmarshal([]byte(stdout), &envelope); err != nil {
			t.Fatalf("%s JSON: %v; stdout=%s", command.args[0], err, stdout)
		}
		if envelope["schema_version"] != "witself.v0" {
			t.Fatalf("%s schema = %#v", command.args[0], envelope)
		}
		if command.args[0] == "deregister" {
			want := map[string]any{"schema_version": "witself.v0", "name": "repair-cell", "deleted": true}
			if !reflect.DeepEqual(envelope, want) {
				t.Fatalf("deregister JSON = %#v; want %#v", envelope, want)
			}
			continue
		}
		resultCell, ok := envelope["cell"].(map[string]any)
		if !ok || len(envelope) != 2 || resultCell["name"] != "repair-cell" || resultCell["accepting"] != command.accepting {
			t.Fatalf("%s JSON = %#v", command.args[0], envelope)
		}
	}
	wantCalls := []string{
		"GET /v1/cells",
		"POST /v1/cells",
		"PATCH /v1/cells/repair-cell",
		"PATCH /v1/cells/repair-cell",
		"PATCH /v1/cells/repair-cell",
		"DELETE /v1/cells/repair-cell",
	}
	if !reflect.DeepEqual(calls, wantCalls) {
		t.Fatalf("calls = %#v; want %#v", calls, wantCalls)
	}
	if len(registrations) != 1 {
		t.Fatalf("registrations = %#v", registrations)
	}
	for i, body := range registrations {
		for key, want := range map[string]any{
			"name": "repair-cell", "endpoint": "https://repair.example", "cloud": "civo",
			"region": "NYC1", "region_code": "use1", "channel": "stable", "weight": float64(2),
			"accepting":                false,
			"backup_validation_target": false,
		} {
			if body[key] != want {
				t.Errorf("registration %d %s = %#v; want %#v", i, key, body[key], want)
			}
		}
		for _, key := range []string{"provision_token", "backup_token"} {
			if _, ok := body[key]; ok {
				t.Errorf("registration %d replayed write-only credential %s", i, key)
			}
		}
	}
	wantUpdates := []map[string]any{{"accepting": false}, {"accepting": true}, {"accepting": false}}
	if !reflect.DeepEqual(acceptingUpdates, wantUpdates) {
		t.Fatalf("accepting updates = %#v; want %#v", acceptingUpdates, wantUpdates)
	}
}

func TestCellsAdminRegisterCredentialFiles(t *testing.T) {
	tokenDir := t.TempDir()
	provisionPath := filepath.Join(tokenDir, "provision.token")
	backupPath := filepath.Join(tokenDir, "backup.token")
	for path, value := range map[string]string{
		provisionPath: "witself_prv_registration-secret\n",
		backupPath:    "witself_bak_registration-secret\n",
	} {
		if err := os.WriteFile(path, []byte(value), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/cells" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode registration: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if body["provision_token"] != "witself_prv_registration-secret" || body["backup_token"] != "witself_bak_registration-secret" || body["backup_validation_target"] != true || body["accepting"] != false {
			t.Error("registration did not preserve credential files and backup-target isolation")
		}
		cell := cellsAdminTestCell("repair-cell", false)
		cell["backup_validation_target"] = true
		writeCellsAdminTestJSON(t, w, map[string]any{"schema_version": "witself.v0", "cell": cell})
	}))
	defer srv.Close()
	stdout, stderr, code := captureEmailAliasAdminCLI(t, func() int {
		return run([]string{
			"cells", "register", "--endpoint", srv.URL, "--token", "fleet-token",
			"--cell-endpoint", "https://repair.example", "--accepting=false", "--backup-validation-target",
			"--provision-token-file", provisionPath, "--backup-token-file", backupPath, "--json", "repair-cell",
		})
	})
	if code != 0 {
		t.Fatalf("register exit=%d stderr=%s", code, stderr)
	}
	if strings.Contains(stdout+stderr, "registration-secret") {
		t.Fatal("credential material appeared in CLI output")
	}
	var envelope struct {
		Cell struct {
			BackupValidationTarget bool `json:"backup_validation_target"`
			HasBackupToken         bool `json:"has_backup_token"`
		} `json:"cell"`
	}
	if err := json.Unmarshal([]byte(stdout), &envelope); err != nil || !envelope.Cell.BackupValidationTarget || !envelope.Cell.HasBackupToken {
		t.Fatalf("registration JSON = %s; err=%v", stdout, err)
	}
}

func TestCellsAdminRegisterDefaultsDrained(t *testing.T) {
	for _, test := range []struct {
		name      string
		flags     []string
		accepting bool
	}{
		{name: "default"},
		{name: "explicit-accepting", flags: []string{"--accepting=true"}, accepting: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.Method != http.MethodPost || r.URL.Path != "/v1/cells" {
					t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
				}
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Errorf("decode registration: %v", err)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				if body["accepting"] != test.accepting || body["weight"] != float64(1) || body["backup_validation_target"] != false {
					t.Errorf("registration defaults = %#v", body)
				}
				writeCellsAdminTestJSON(t, w, map[string]any{"schema_version": "witself.v0", "cell": body})
			}))
			defer srv.Close()
			args := []string{"cells", "register", "repair-cell", "--cell-endpoint", "https://repair.example/", "--endpoint", srv.URL, "--token", "fleet-token", "--json"}
			args = append(args, test.flags...)
			stdout, stderr, code := captureEmailAliasAdminCLI(t, func() int { return run(args) })
			if code != 0 || calls != 1 {
				t.Fatalf("exit=%d calls=%d stdout=%q stderr=%q", code, calls, stdout, stderr)
			}
		})
	}
}

func TestCellsAdminListJSON(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Method != http.MethodGet || r.URL.RequestURI() != "/v1/admin/cells" || r.Header.Get("Authorization") != "Bearer admin-token" {
			t.Errorf("unexpected list request %s %s", r.Method, r.URL.RequestURI())
		}
		cell := cellsAdminTestCell("repair-cell", false)
		cell["account_count"], cell["archived_count"] = 3, 2
		cell["version"] = "v0.0.test"
		writeCellsAdminTestJSON(t, w, map[string]any{"cells": []map[string]any{cell}})
	}))
	defer srv.Close()
	stdout, stderr, code := captureEmailAliasAdminCLI(t, func() int {
		return run([]string{"cells", "list", "--endpoint", srv.URL, "--token", "admin-token", "--json"})
	})
	if code != 0 || calls != 1 {
		t.Fatalf("exit=%d calls=%d stderr=%q", code, calls, stderr)
	}
	var envelope map[string]any
	if err := json.Unmarshal([]byte(stdout), &envelope); err != nil {
		t.Fatal(err)
	}
	cells, ok := envelope["cells"].([]any)
	if !ok || len(envelope) != 1 || len(cells) != 1 {
		t.Fatalf("list JSON = %#v", envelope)
	}
	cell, ok := cells[0].(map[string]any)
	if !ok || cell["name"] != "repair-cell" || cell["account_count"] != float64(3) || cell["archived_count"] != float64(2) || cell["version"] != "v0.0.test" {
		t.Fatalf("list cell = %#v", cells[0])
	}
}

func TestCellsAdminRepairRedactsCredentialValues(t *testing.T) {
	for _, verb := range []string{"show", "register", "drain", "undrain"} {
		for _, format := range []string{"table", "json"} {
			t.Run(verb+"/"+format, func(t *testing.T) {
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					cell := cellsAdminTestCell("repair-cell", false)
					cell["provision_token"] = "witself_prv_do-not-render"
					cell["backup_token"] = "witself_bak_do-not-render"
					if r.Method == http.MethodGet && r.URL.Path == "/v1/cells" {
						writeCellsAdminTestJSON(t, w, map[string]any{"schema_version": "witself.v0", "cells": []map[string]any{cell}})
						return
					}
					register := r.Method == http.MethodPost && r.URL.Path == "/v1/cells"
					acceptingUpdate := r.Method == http.MethodPatch && r.URL.Path == "/v1/cells/repair-cell"
					if !register && !acceptingUpdate {
						t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
					}
					var body map[string]any
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Errorf("decode registration: %v", err)
						w.WriteHeader(http.StatusBadRequest)
						return
					}
					for _, key := range []string{"provision_token", "backup_token"} {
						if _, ok := body[key]; ok {
							t.Errorf("replayed response credential %s", key)
						}
					}
					cell["accepting"] = body["accepting"]
					writeCellsAdminTestJSON(t, w, map[string]any{"schema_version": "witself.v0", "cell": cell})
				}))
				defer srv.Close()
				args := []string{"cells", verb, "repair-cell", "--endpoint", srv.URL, "--token", "fleet-token"}
				if verb == "register" {
					args = append(args, "--cell-endpoint", "https://repair.example")
				}
				if format == "json" {
					args = append(args, "--json")
				}
				stdout, stderr, code := captureEmailAliasAdminCLI(t, func() int { return run(args) })
				if code != 0 || !strings.Contains(stdout, "repair-cell") || strings.Contains(stdout+stderr, "do-not-render") {
					t.Fatalf("exit=%d stdout=%q stderr=%q", code, stdout, stderr)
				}
				if format == "table" && !strings.Contains(stdout+stderr, "has-backup-token") {
					t.Fatalf("missing credential-presence column: stdout=%q stderr=%q", stdout, stderr)
				}
			})
		}
	}
}

func TestCellsAdminRepairAuthenticationFailures(t *testing.T) {
	for _, verb := range []string{"list", "show", "register", "drain", "undrain", "deregister"} {
		t.Run(verb, func(t *testing.T) {
			calls := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.Header.Get("Authorization") != "Bearer invalid-fleet-token" {
					t.Error("missing explicitly selected fleet credential")
				}
				w.WriteHeader(http.StatusUnauthorized)
				writeCellsAdminTestJSON(t, w, map[string]any{"error": "unauthorized"})
			}))
			defer srv.Close()
			args := []string{"cells", verb, "repair-cell", "--endpoint", srv.URL, "--token", "invalid-fleet-token", "--json"}
			if verb == "list" {
				args = append(args[:2], args[3:]...)
			}
			switch verb {
			case "register":
				args = append(args, "--cell-endpoint", "https://repair.example")
			case "deregister":
				args = append(args, "--yes", "--yes-cell=repair-cell")
			}
			stdout, stderr, code := captureEmailAliasAdminCLI(t, func() int { return run(args) })
			if code != 1 || calls != 1 || stdout != "" || !strings.Contains(stderr, "unauthorized") {
				t.Fatalf("exit=%d calls=%d stdout=%q stderr=%q", code, calls, stdout, stderr)
			}
		})
	}
}

func TestCellsAdminDeregisterPreservesControlPlaneRefusals(t *testing.T) {
	for _, message := range []string{
		"accounts still live on this cell",
		"account reservations or residents still belong to this cell",
		"cell must be drained first (re-register with accepting=false)",
	} {
		t.Run(message, func(t *testing.T) {
			var calls []string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls = append(calls, r.Method+" "+r.URL.RequestURI())
				if r.Method != http.MethodDelete || r.URL.Path != "/v1/cells/repair-cell" {
					t.Errorf("unexpected request %s %s", r.Method, r.URL.RequestURI())
				}
				w.WriteHeader(http.StatusConflict)
				writeCellsAdminTestJSON(t, w, map[string]any{"error": message})
			}))
			defer srv.Close()
			stdout, stderr, code := captureEmailAliasAdminCLI(t, func() int {
				return run([]string{"cells", "deregister", "repair-cell", "--endpoint", srv.URL, "--token", "fleet-token", "--yes", "--yes-cell=repair-cell", "--json"})
			})
			if code != 1 || stdout != "" || stderr != "witself-admin cells: "+message+"\n" {
				t.Fatalf("exit=%d stdout=%q stderr=%q", code, stdout, stderr)
			}
			if want := []string{"DELETE /v1/cells/repair-cell"}; !reflect.DeepEqual(calls, want) {
				t.Fatalf("calls = %#v; want %#v", calls, want)
			}
		})
	}
}

func TestCellsAdminDeregisterRequiresBothConfirmations(t *testing.T) {
	input, err := os.CreateTemp(t.TempDir(), "stdin")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = input.Close() }()
	oldStdin := os.Stdin
	os.Stdin = input
	defer func() { os.Stdin = oldStdin }()
	for _, test := range []struct {
		name  string
		flags []string
		want  string
	}{
		{"missing-both", nil, "requires --yes"},
		{"missing-name", []string{"--yes"}, "noninteractive deregister requires --yes --yes-cell=repair-cell"},
		{"missing-yes", []string{"--yes-cell=repair-cell"}, "requires --yes"},
		{"wrong-name", []string{"--yes", "--yes-cell=another-cell"}, "must exactly match"},
		{"padded-name", []string{"--yes", "--yes-cell=repair-cell "}, "must exactly match"},
	} {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls++
				w.WriteHeader(http.StatusNoContent)
			}))
			defer srv.Close()
			args := []string{"cells", "deregister", "repair-cell", "--endpoint", srv.URL, "--token", "fleet-token", "--json"}
			args = append(args, test.flags...)
			stdout, stderr, code := captureEmailAliasAdminCLI(t, func() int { return run(args) })
			if code != 2 || calls != 0 || stdout != "" || !strings.Contains(stderr, test.want) {
				t.Fatalf("exit=%d calls=%d stdout=%q stderr=%q", code, calls, stdout, stderr)
			}
		})
	}
}

func TestCellsAdminRepairNeverExposesForceOrPurge(t *testing.T) {
	for _, verb := range []string{"register", "drain", "undrain", "deregister", "purge"} {
		t.Run(verb, func(t *testing.T) {
			calls := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls++
				w.WriteHeader(http.StatusNoContent)
			}))
			defer srv.Close()
			args := []string{"cells", verb, "repair-cell", "--endpoint", srv.URL, "--token", "fleet-token", "--force"}
			if verb == "deregister" {
				args = append(args, "--yes", "--yes-cell=repair-cell")
			}
			stdout, stderr, code := captureEmailAliasAdminCLI(t, func() int { return run(args) })
			want := "flag provided but not defined: -force"
			if verb == "purge" {
				want = "unknown subcommand"
			}
			if code != 2 || calls != 0 || stdout != "" || !strings.Contains(stderr, want) {
				t.Fatalf("exit=%d calls=%d stdout=%q stderr=%q", code, calls, stdout, stderr)
			}
		})
	}
}

func TestCellsAdminRepairRejectsEndpointRouteInjection(t *testing.T) {
	for _, verb := range []string{"register", "drain", "undrain", "deregister"} {
		for _, source := range []string{"flag", "environment"} {
			for _, endpoint := range []struct {
				name  string
				value func(string) string
			}{
				{"purge-fragment", func(origin string) string { return origin + "/v1/cells/production:purge#" }},
				{"purge-query", func(origin string) string { return origin + "/v1/cells/production:purge?" }},
				{"empty-fragment", func(origin string) string { return origin + "#" }},
				{"fragment", func(origin string) string { return origin + "#endpoint-secret" }},
				{"empty-query", func(origin string) string { return origin + "?" }},
				{"query", func(origin string) string { return origin + "?token=endpoint-secret" }},
				{"base-path", func(origin string) string { return origin + "/v1/cells/production:purge" }},
				{"encoded-path", func(origin string) string { return origin + "/%2fv1/cells/production:purge" }},
				{"userinfo", func(origin string) string {
					return strings.Replace(origin, "http://", "http://endpoint-user:endpoint-secret@", 1)
				}},
			} {
				t.Run(verb+"/"+source+"/"+endpoint.name, func(t *testing.T) {
					calls := 0
					srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
						calls++
						w.WriteHeader(http.StatusNoContent)
					}))
					defer srv.Close()
					t.Setenv("WITSELF_CONTROL_PLANE", srv.URL)
					args := []string{"cells", verb, "repair-cell", "--token", "fleet-secret", "--json"}
					if source == "flag" {
						args = append(args, "--endpoint", endpoint.value(srv.URL))
					} else {
						t.Setenv("WITSELF_CONTROL_PLANE", endpoint.value(srv.URL))
					}
					switch verb {
					case "register":
						args = append(args, "--cell-endpoint", "https://repair.example")
					case "deregister":
						args = append(args, "--yes", "--yes-cell=repair-cell")
					}
					stdout, stderr, code := captureEmailAliasAdminCLI(t, func() int { return run(args) })
					if code == 0 || calls != 0 || stdout != "" || stderr == "" {
						t.Fatalf("exit=%d requests=%d stdout=%q stderr=%q", code, calls, stdout, stderr)
					}
					for _, value := range []string{"fleet-secret", "endpoint-user", "endpoint-secret"} {
						if strings.Contains(stdout+stderr, value) {
							t.Fatal("credential material appeared in CLI output")
						}
					}
				})
			}
		}
	}
}

func TestCellsAdminRepairAcceptsOriginEndpoints(t *testing.T) {
	for _, verb := range []string{"register", "drain", "undrain", "deregister"} {
		for _, source := range []string{"flag", "environment"} {
			for _, trailingSlash := range []bool{false, true} {
				name := verb + "/" + source + "/origin"
				if trailingSlash {
					name += "-trailing-slash"
				}
				t.Run(name, func(t *testing.T) {
					calls := 0
					srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						calls++
						wantMethod, wantRoute := http.MethodPatch, "/v1/cells/repair-cell"
						switch verb {
						case "register":
							wantMethod, wantRoute = http.MethodPost, "/v1/cells"
						case "deregister":
							wantMethod = http.MethodDelete
						}
						if r.Method != wantMethod || r.URL.RequestURI() != wantRoute || r.Header.Get("Authorization") != "Bearer fleet-token" {
							t.Errorf("unexpected request: %s %s", r.Method, r.URL.RequestURI())
						}
						if verb == "deregister" {
							w.WriteHeader(http.StatusNoContent)
							return
						}
						writeCellsAdminTestJSON(t, w, map[string]any{
							"schema_version": "witself.v0", "cell": cellsAdminTestCell("repair-cell", verb == "undrain"),
						})
					}))
					defer srv.Close()
					endpoint := srv.URL
					if trailingSlash {
						endpoint += "/"
					}
					t.Setenv("WITSELF_CONTROL_PLANE", srv.URL)
					args := []string{"cells", verb, "repair-cell", "--token", "fleet-token", "--json"}
					if source == "flag" {
						args = append(args, "--endpoint", endpoint)
					} else {
						t.Setenv("WITSELF_CONTROL_PLANE", endpoint)
					}
					switch verb {
					case "register":
						args = append(args, "--cell-endpoint", "https://repair.example")
					case "deregister":
						args = append(args, "--yes", "--yes-cell=repair-cell")
					}
					stdout, stderr, code := captureEmailAliasAdminCLI(t, func() int { return run(args) })
					if code != 0 || calls != 1 || !json.Valid([]byte(stdout)) {
						t.Fatalf("exit=%d requests=%d stdout=%q stderr=%q", code, calls, stdout, stderr)
					}
				})
			}
		}
	}
}

func TestConfirmCellDeregisterExactName(t *testing.T) {
	for _, test := range []struct {
		name        string
		yesCell     string
		interactive bool
		input       io.Reader
		wantErr     bool
		wantPrompt  bool
	}{
		{name: "flag-exact", yesCell: "repair-cell"},
		{name: "flag-mismatch", yesCell: "another-cell", wantErr: true},
		{name: "flag-leading-space", yesCell: " repair-cell", wantErr: true},
		{name: "flag-trailing-space", yesCell: "repair-cell ", wantErr: true},
		{name: "noninteractive", input: strings.NewReader("repair-cell\n"), wantErr: true},
		{name: "interactive-exact", interactive: true, input: strings.NewReader("repair-cell\n"), wantPrompt: true},
		{name: "interactive-mismatch", interactive: true, input: strings.NewReader("another-cell\n"), wantErr: true, wantPrompt: true},
		{name: "interactive-leading-space", interactive: true, input: strings.NewReader(" repair-cell\n"), wantErr: true, wantPrompt: true},
		{name: "interactive-trailing-space", interactive: true, input: strings.NewReader("repair-cell \n"), wantErr: true, wantPrompt: true},
		{name: "interactive-empty", interactive: true, input: strings.NewReader(""), wantErr: true, wantPrompt: true},
		{name: "interactive-nil", interactive: true, wantErr: true, wantPrompt: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			var prompt bytes.Buffer
			err := confirmCellDeregister("repair-cell", test.yesCell, test.interactive, test.input, &prompt)
			if (err != nil) != test.wantErr {
				t.Fatalf("error = %v; wantErr=%t", err, test.wantErr)
			}
			if (prompt.Len() > 0) != test.wantPrompt {
				t.Fatalf("prompt = %q; wantPrompt=%t", prompt.String(), test.wantPrompt)
			}
		})
	}
}

func TestCellsAdminRepairTokenSources(t *testing.T) {
	root := t.TempDir()
	t.Setenv("WITSELF_HOME", root)
	t.Setenv("WITSELF_FLEET_TOKEN", "fleet-from-env")
	if err := os.MkdirAll(filepath.Join(root, "tokens"), 0o700); err != nil {
		t.Fatal(err)
	}
	managed := filepath.Join(root, "tokens", "fleet.token")
	explicit := filepath.Join(root, "explicit.token")
	for path, value := range map[string]string{managed: "fleet-from-managed\n", explicit: "fleet-from-file\n"} {
		if err := os.WriteFile(path, []byte(value), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, test := range []struct {
		name string
		args []string
		want string
	}{
		{"fleet-flag", []string{"--fleet-token", "fleet-from-flag"}, "fleet-from-flag"},
		{"token-alias", []string{"--token", "fleet-from-alias"}, "fleet-from-alias"},
		{"token-file", []string{"--token-file", explicit}, "fleet-from-file"},
		{"environment", nil, "fleet-from-env"},
		{"managed", nil, "fleet-from-managed"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if test.name == "managed" {
				t.Setenv("WITSELF_FLEET_TOKEN", "")
			}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer "+test.want {
					t.Error("wrong fleet token source")
				}
				writeCellsAdminTestJSON(t, w, map[string]any{"schema_version": "witself.v0", "cells": []map[string]any{cellsAdminTestCell("repair-cell", false)}})
			}))
			defer srv.Close()
			args := []string{"cells", "show", "--endpoint", srv.URL, "--json"}
			args = append(args, test.args...)
			args = append(args, "repair-cell")
			_, stderr, code := captureEmailAliasAdminCLI(t, func() int { return run(args) })
			if code != 0 {
				t.Fatalf("exit=%d stderr=%s", code, stderr)
			}
		})
	}
}

func TestCellsAdminRepairHelpRequiresNoCredentials(t *testing.T) {
	t.Setenv("WITSELF_HOME", t.TempDir())
	t.Setenv("WITSELF_FLEET_TOKEN", "")
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	t.Setenv("WITSELF_CONTROL_PLANE", srv.URL)
	for _, verb := range []string{"show", "register", "drain", "undrain", "deregister"} {
		t.Run(verb, func(t *testing.T) {
			stdout, stderr, code := captureEmailAliasAdminCLI(t, func() int {
				return run([]string{"cells", verb, "--help"})
			})
			if code != 0 || stdout != "" || !strings.Contains(stderr, "Usage of cells "+verb) {
				t.Fatalf("exit=%d stdout=%q stderr=%q", code, stdout, stderr)
			}
		})
	}
	if calls != 0 {
		t.Fatalf("help made %d control-plane requests", calls)
	}
}

func TestCellsAdminRepairRejectsInvalidArgumentsBeforeRequests(t *testing.T) {
	for _, args := range [][]string{
		{"show", "Repair-Cell"},
		{"show", "repair/cell"},
		{"show", "repair-cell", "another-cell"},
		{"show", "--token", "fleet-token", "repair-cell", "another-cell"},
		{"register", "repair-cell", "--cell-endpoint", "http://repair.example"},
		{"register", "repair-cell", "--cell-endpoint", "https://repair.example", "--weight=NaN"},
		{"register", "repair-cell", "--cell-endpoint", "https://repair.example", "--channel=unknown"},
		{"register", "repair-cell", "--cell-endpoint", "https://repair.example", "--backup-validation-target", "--accepting=true"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			calls := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls++
				w.WriteHeader(http.StatusNoContent)
			}))
			defer srv.Close()
			command := append([]string{"cells"}, args...)
			command = append(command, "--endpoint", srv.URL, "--token", "fleet-token")
			stdout, stderr, code := captureEmailAliasAdminCLI(t, func() int { return run(command) })
			if code != 2 || calls != 0 || stdout != "" || stderr == "" {
				t.Fatalf("exit=%d calls=%d stdout=%q stderr=%q", code, calls, stdout, stderr)
			}
		})
	}
}

type cellsAdminRoundTripFunc func(*http.Request) (*http.Response, error)

func (f cellsAdminRoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func TestCellsAdminRegisterRejectsCellEndpointDelimitersBeforeRequests(t *testing.T) {
	for _, suffix := range []string{"?", "#", "?query=value", "#fragment"} {
		t.Run(suffix, func(t *testing.T) {
			calls := 0
			originalTransport := http.DefaultTransport
			t.Cleanup(func() { http.DefaultTransport = originalTransport })
			http.DefaultTransport = cellsAdminRoundTripFunc(func(*http.Request) (*http.Response, error) {
				calls++
				return &http.Response{
					StatusCode: http.StatusNoContent,
					Body:       http.NoBody,
					Header:     make(http.Header),
				}, nil
			})
			stdout, stderr, code := captureEmailAliasAdminCLI(t, func() int {
				return run([]string{
					"cells", "register", "repair-cell", "--cell-endpoint", "https://repair.example" + suffix,
					"--endpoint", "https://control-plane.example", "--token", "fleet-token",
				})
			})
			if code != 2 || calls != 0 || stdout != "" || !strings.Contains(stderr, "--cell-endpoint must be an HTTPS URL without credentials, query, or fragment") {
				t.Fatalf("exit=%d requests=%d stdout=%q stderr=%q", code, calls, stdout, stderr)
			}
		})
	}
}

func cellsAdminTestCell(name string, accepting bool) map[string]any {
	return map[string]any{
		"name": name, "endpoint": "https://repair.example", "cloud": "civo", "region": "NYC1",
		"region_code": "use1", "channel": "stable", "weight": 2, "accepting": accepting,
		"backup_validation_target": false, "has_provision_token": true, "has_backup_token": true,
	}
}

func writeCellsAdminTestJSON(t *testing.T, w http.ResponseWriter, body any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(body); err != nil {
		t.Errorf("encode test response: %v", err)
	}
}
