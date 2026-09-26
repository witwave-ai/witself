package fleet

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

// Ordinary registration used to pass a typed-nil *registrationAck to do,
// causing json: Unmarshal(nil *fleet.registrationAck) after a successful write.
func TestRegisterWithoutBackupToken(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusCreated} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.Method != http.MethodPost || r.URL.Path != "/v1/cells" {
					t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
				}
				w.WriteHeader(status)
				_, _ = w.Write([]byte(`{"schema_version":"witself.v0","cell":{"name":"cell-a","accepting":true,"backup_validation_target":false}}`))
			}))
			defer server.Close()
			client := &Client{base: server.URL, token: "fleet-test-token", hc: server.Client()}
			if err := client.Register(context.Background(), Cell{Name: "cell-a", Endpoint: "https://cell.example.com"}); err != nil {
				t.Fatalf("successful registration: %v", err)
			}
			if calls != 1 {
				t.Fatalf("registrations = %d, want 1", calls)
			}
		})
	}
}

func TestDrainRedactedCredentials(t *testing.T) {
	for _, backupTarget := range []bool{false, true} {
		name := "ordinary"
		if backupTarget {
			name = "backup validation target"
		}
		t.Run(name, func(t *testing.T) {
			var requests []string
			drains := 0
			cell := map[string]any{
				"name": "cell-a", "endpoint": "https://cell.example.com",
				"accepting": !backupTarget, "backup_validation_target": backupTarget,
				"has_provision_token": true, "has_backup_token": true,
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				request := r.Method + " " + r.URL.Path
				requests = append(requests, request)
				switch request {
				case "PATCH /v1/cells/cell-a":
					var body map[string]any
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Errorf("decode drain: %v", err)
						http.Error(w, "bad request", http.StatusBadRequest)
						return
					}
					want := map[string]any{"accepting": false}
					if !reflect.DeepEqual(body, want) {
						t.Errorf("drain payload = %v, want %v", body, want)
					}
					cell["accepting"] = false
					drains++
					_ = json.NewEncoder(w).Encode(map[string]any{"schema_version": "witself.v0", "cell": cell})
				default:
					t.Errorf("unexpected request: %s", request)
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			client := &Client{base: server.URL, token: "fleet-test-token", hc: server.Client()}
			if err := client.Drain(context.Background(), "cell-a"); err != nil {
				t.Fatalf("drain redacted cell: %v", err)
			}
			if want := []string{"PATCH /v1/cells/cell-a"}; !reflect.DeepEqual(requests, want) {
				t.Errorf("requests = %v, want %v", requests, want)
			}
			if drains != 1 || cell["accepting"] != false {
				t.Errorf("drains = %d, accepting = %v", drains, cell["accepting"])
			}
		})
	}
}

func TestRegisterSendsDistinctCredentialShape(t *testing.T) {
	var body map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/cells" {
			http.NotFound(w, r)
			return
		}
		if got := r.Header.Get("Authorization"); got != "Bearer fleet-test-token" {
			t.Errorf("authorization = %q", got)
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode registration: %v", err)
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"schema_version": "witself.v0",
			"cell": map[string]any{
				"name":                     "civo-sandbox-use1-serving",
				"backup_validation_target": false,
				"has_backup_token":         true,
			},
		})
	}))
	defer server.Close()

	client := &Client{
		base:  server.URL,
		token: "fleet-test-token",
		hc:    server.Client(),
	}
	err := client.Register(context.Background(), Cell{
		Name:              "civo-sandbox-use1-serving",
		Endpoint:          "https://api.cell.example.com",
		Cloud:             "civo",
		Region:            "NYC1",
		RegionCode:        "use1",
		Channel:           "experimental",
		HasProvisionToken: true,
		HasBackupToken:    true,
		ProvisionToken:    "witself_prv_provision-only",
		BackupToken:       "witself_bak_backup-only",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"has_provision_token", "has_backup_token"} {
		if _, present := body[key]; present {
			t.Errorf("registration must omit read-only %s", key)
		}
	}
	if got := body["provision_token"]; got != "witself_prv_provision-only" {
		t.Fatalf("provision_token = %#v", got)
	}
	if got := body["backup_token"]; got != "witself_bak_backup-only" {
		t.Fatalf("backup_token = %#v", got)
	}
	if body["provision_token"] == body["backup_token"] {
		t.Fatal("registration sent the same authority for provisioning and backup")
	}
	if got, ok := body["backup_validation_target"].(bool); !ok || got {
		t.Fatalf("backup_validation_target = %#v, want typed false", body["backup_validation_target"])
	}
}

func TestRegisterBackupValidationTargetIsFailClosed(t *testing.T) {
	falseValue := false
	var body map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode registration: %v", err)
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"schema_version": "witself.v0",
			"cell": map[string]any{
				"name":                     "civo-sandbox-use1-backup",
				"accepting":                false,
				"backup_validation_target": true,
				"has_backup_token":         true,
			},
		})
	}))
	defer server.Close()

	client := &Client{base: server.URL, token: "fleet-test-token", hc: server.Client()}
	err := client.Register(context.Background(), Cell{
		Name:                   "civo-sandbox-use1-backup",
		Endpoint:               "https://api.cell.example.com",
		Accepting:              &falseValue,
		BackupValidationTarget: true,
		ProvisionToken:         "witself_prv_provision-only",
		BackupToken:            "witself_bak_backup-only",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := body["accepting"].(bool); !ok || got {
		t.Fatalf("accepting = %#v, want typed false", body["accepting"])
	}
	if got, ok := body["backup_validation_target"].(bool); !ok || !got {
		t.Fatalf("backup_validation_target = %#v, want typed true", body["backup_validation_target"])
	}
}

func TestRegisterBackupValidationTargetRejectsAmbiguousAcknowledgement(t *testing.T) {
	falseValue := false
	tests := []struct {
		name    string
		cellAck map[string]any
		wantErr string
	}{
		{
			name: "missing marker",
			cellAck: map[string]any{
				"name":             "civo-sandbox-use1-backup",
				"accepting":        false,
				"has_backup_token": true,
			},
			wantErr: "did not acknowledge backup_validation_target=true",
		},
		{
			name: "still accepting",
			cellAck: map[string]any{
				"name":                     "civo-sandbox-use1-backup",
				"accepting":                true,
				"backup_validation_target": true,
				"has_backup_token":         true,
			},
			wantErr: "did not acknowledge accepting=false",
		},
		{
			name: "missing accepting",
			cellAck: map[string]any{
				"name":                     "civo-sandbox-use1-backup",
				"backup_validation_target": true,
				"has_backup_token":         true,
			},
			wantErr: "did not acknowledge accepting=false",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_ = json.NewEncoder(w).Encode(map[string]any{
					"schema_version": "witself.v0",
					"cell":           test.cellAck,
				})
			}))
			defer server.Close()

			client := &Client{base: server.URL, token: "fleet-test-token", hc: server.Client()}
			err := client.Register(context.Background(), Cell{
				Name:                   "civo-sandbox-use1-backup",
				Endpoint:               "https://api.cell.example.com",
				Accepting:              &falseValue,
				BackupValidationTarget: true,
				ProvisionToken:         "witself_prv_provision-only",
				BackupToken:            "witself_bak_backup-only",
			})
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("error = %v, want substring %q", err, test.wantErr)
			}
		})
	}
}

func TestRegisterBackupValidationTargetRejectsAcceptingRequest(t *testing.T) {
	for _, accepting := range []*bool{nil, func() *bool {
		value := true
		return &value
	}()} {
		client := &Client{base: "http://unused.invalid", token: "fleet-test-token", hc: http.DefaultClient}
		err := client.Register(context.Background(), Cell{
			Name:                   "civo-sandbox-use1-backup",
			Accepting:              accepting,
			BackupValidationTarget: true,
		})
		if err == nil || !strings.Contains(err.Error(), "must register with accepting=false") {
			t.Fatalf("accepting=%v error = %v", accepting, err)
		}
	}
}

func TestRegisterRequiresBackupCredentialAcknowledgement(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"schema_version": "witself.v0",
			"cell": map[string]any{
				"name":                     "civo-sandbox-use1-serving",
				"backup_validation_target": false,
				"has_provision_token":      true,
			},
		})
	}))
	defer server.Close()

	client := &Client{
		base:  server.URL,
		token: "fleet-test-token",
		hc:    server.Client(),
	}
	err := client.Register(context.Background(), Cell{
		Name:           "civo-sandbox-use1-serving",
		Endpoint:       "https://api.cell.example.com",
		ProvisionToken: "witself_prv_provision-only",
		BackupToken:    "witself_bak_backup-only",
	})
	if err == nil {
		t.Fatal("legacy registration response unexpectedly accepted")
	}
	if got := err.Error(); !strings.Contains(got, "did not acknowledge backup_token") {
		t.Fatalf("error = %q", got)
	}
}

func TestRegisterRequiresExactCellAcknowledgement(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"schema_version": "witself.v0",
			"cell": map[string]any{
				"name":                     "different-cell",
				"backup_validation_target": false,
				"has_backup_token":         true,
			},
		})
	}))
	defer server.Close()

	client := &Client{
		base:  server.URL,
		token: "fleet-test-token",
		hc:    server.Client(),
	}
	err := client.Register(context.Background(), Cell{
		Name:           "civo-sandbox-use1-serving",
		Endpoint:       "https://api.cell.example.com",
		ProvisionToken: "witself_prv_provision-only",
		BackupToken:    "witself_bak_backup-only",
	})
	if err == nil {
		t.Fatal("wrong-cell registration acknowledgement unexpectedly accepted")
	}
	if got := err.Error(); !strings.Contains(got, `acknowledged cell "different-cell"`) {
		t.Fatalf("error = %q", got)
	}
}

func TestRegisterRequiresCurrentResponseSchema(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"schema_version": "legacy.v0",
			"cell": map[string]any{
				"name":                     "civo-sandbox-use1-serving",
				"backup_validation_target": false,
				"has_backup_token":         true,
			},
		})
	}))
	defer server.Close()

	client := &Client{
		base:  server.URL,
		token: "fleet-test-token",
		hc:    server.Client(),
	}
	err := client.Register(context.Background(), Cell{
		Name:           "civo-sandbox-use1-serving",
		Endpoint:       "https://api.cell.example.com",
		ProvisionToken: "witself_prv_provision-only",
		BackupToken:    "witself_bak_backup-only",
	})
	if err == nil {
		t.Fatal("legacy registration schema unexpectedly accepted")
	}
	if got := err.Error(); !strings.Contains(got, `schema_version "legacy.v0"`) {
		t.Fatalf("error = %q", got)
	}
}

func TestValidateRegistrationCredentials(t *testing.T) {
	tests := []struct {
		name      string
		provision string
		backup    string
		wantErr   string
	}{
		{
			name:      "valid and distinct",
			provision: "witself_prv_provision-only",
			backup:    "witself_bak_backup-only",
		},
		{
			name:      "missing",
			provision: "witself_prv_provision-only",
			wantErr:   "missing or malformed",
		},
		{
			name:      "bad provision prefix",
			provision: "not-a-provision-token",
			backup:    "witself_bak_backup-only",
			wantErr:   "provisionToken output is missing or malformed",
		},
		{
			name:      "wrong prefix",
			provision: "witself_prv_provision-only",
			backup:    "not-a-backup-token",
			wantErr:   "missing or malformed",
		},
		{
			name:      "prefix only",
			provision: "witself_prv_provision-only",
			backup:    "witself_bak_",
			wantErr:   "missing or malformed",
		},
		{
			name:      "same authority",
			provision: "witself_bak_same",
			backup:    "witself_bak_same",
			wantErr:   "must be distinct",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := ValidateRegistrationCredentials(
				test.provision, test.backup,
			)
			if test.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("error = %v, want substring %q", err, test.wantErr)
			}
		})
	}
}

func TestCellJSONOmitsCredentialFieldsAfterRedaction(t *testing.T) {
	raw, err := json.Marshal(Cell{
		Name:     "civo-sandbox-use1-serving",
		Endpoint: "https://api.cell.example.com",
	})
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"provision_token", "backup_token"} {
		if _, ok := body[field]; ok {
			t.Fatalf("redacted cell unexpectedly contains %s: %s", field, raw)
		}
	}
}

func TestListCellsKeepsRedactedCredentialsEmpty(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/cells" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"cells": []any{map[string]any{
				"name":                     "civo-sandbox-use1-backup",
				"endpoint":                 "https://api.cell.example.com",
				"accepting":                false,
				"backup_validation_target": true,
				"has_provision_token":      true,
				"has_backup_token":         true,
			}},
		})
	}))
	defer server.Close()

	client := &Client{
		base:  server.URL,
		token: "fleet-test-token",
		hc:    server.Client(),
	}
	cells, err := client.ListCells(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(cells) != 1 {
		t.Fatalf("cells = %#v", cells)
	}
	if cells[0].ProvisionToken != "" || cells[0].BackupToken != "" {
		t.Fatalf("redacted read reconstructed credentials: %#v", cells[0])
	}
	if !cells[0].HasProvisionToken || !cells[0].HasBackupToken {
		t.Fatalf("redacted credential presence flags lost: %#v", cells[0])
	}
	if !cells[0].BackupValidationTarget ||
		cells[0].Accepting == nil || *cells[0].Accepting {
		t.Fatalf("restore-test isolation fields lost: %#v", cells[0])
	}
}

// Ordinary registrations must validate the same response contract as targets
// carrying backup credentials, including omitted/null isolation fields.
func TestRegisterWithoutBackupTokenRejectsInvalidAcknowledgement(t *testing.T) {
	for _, response := range []string{
		``, `not json`, `null`, `{}`,
		`{"schema_version":"legacy.v0","cell":{"name":"cell-a"}}`,
		`{"schema_version":"witself.v0","cell":{"name":"other"}}`,
		`{"schema_version":"witself.v0","cell":{"name":"cell-a"}}`,
		`{"schema_version":"witself.v0","cell":{"name":"cell-a","backup_validation_target":null}}`,
		`{"schema_version":"witself.v0","cell":{"name":"cell-a","backup_validation_target":true}}`,
		`{"schema_version":"witself.v0","cell":{"name":"cell-a","backup_validation_target":false}}`,
		`{"schema_version":"witself.v0","cell":{"name":"cell-a","backup_validation_target":false,"accepting":null}}`,
		`{"schema_version":"witself.v0","cell":{"name":"cell-a","backup_validation_target":false,"accepting":true}}`,
	} {
		t.Run(response, func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls++
				_, _ = w.Write([]byte(response))
			}))
			defer server.Close()
			client := &Client{base: server.URL, hc: server.Client()}
			accepting := false
			if err := client.Register(context.Background(), Cell{Name: "cell-a", Accepting: &accepting}); err == nil {
				t.Fatal("accepted invalid registration acknowledgement")
			}
			if calls != 1 {
				t.Fatalf("requests = %d, want 1", calls)
			}
		})
	}
}

func TestDrainRejectsInvalidAcknowledgement(t *testing.T) {
	for _, backupTarget := range []bool{false, true} {
		for _, tc := range []struct {
			name     string
			response string
		}{
			{"empty", ``},
			{"malformed", `not json`},
			{"null", `null`},
			{"missing schema", `{"cell":{"name":"cell-a","accepting":false,"backup_validation_target":%t}}`},
			{"wrong schema", `{"schema_version":"legacy.v0","cell":{"name":"cell-a","accepting":false,"backup_validation_target":%t}}`},
			{"missing cell", `{"schema_version":"witself.v0"}`},
			{"missing name", `{"schema_version":"witself.v0","cell":{"accepting":false,"backup_validation_target":%t}}`},
			{"wrong name", `{"schema_version":"witself.v0","cell":{"name":"other","accepting":false,"backup_validation_target":%t}}`},
			{"missing accepting", `{"schema_version":"witself.v0","cell":{"name":"cell-a","backup_validation_target":%t}}`},
			{"null accepting", `{"schema_version":"witself.v0","cell":{"name":"cell-a","accepting":null,"backup_validation_target":%t}}`},
			{"still accepting", `{"schema_version":"witself.v0","cell":{"name":"cell-a","accepting":true,"backup_validation_target":%t}}`},
			{"wrong accepting type", `{"schema_version":"witself.v0","cell":{"name":"cell-a","accepting":"false","backup_validation_target":%t}}`},
			{"missing purpose", `{"schema_version":"witself.v0","cell":{"name":"cell-a","accepting":false}}`},
			{"null purpose", `{"schema_version":"witself.v0","cell":{"name":"cell-a","accepting":false,"backup_validation_target":null}}`},
		} {
			t.Run(fmt.Sprintf("backup=%t/%s", backupTarget, tc.name), func(t *testing.T) {
				calls := 0
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls++
					if r.Method != http.MethodPatch || r.URL.Path != "/v1/cells/cell-a" {
						t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
					}
					_, _ = w.Write([]byte(strings.ReplaceAll(tc.response, "%t", fmt.Sprint(backupTarget))))
				}))
				defer server.Close()
				client := &Client{base: server.URL, hc: server.Client()}
				if err := client.Drain(context.Background(), "cell-a"); err == nil {
					t.Fatal("accepted invalid drain acknowledgement")
				}
				if calls != 1 {
					t.Fatalf("requests = %d, want 1", calls)
				}
			})
		}
	}
}

func TestDrainOlderControlPlaneFailsClosed(t *testing.T) {
	for _, status := range []int{http.StatusNotFound, http.StatusMethodNotAllowed} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var requests []string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests = append(requests, r.Method+" "+r.URL.Path)
				http.Error(w, "unsupported", status)
			}))
			defer server.Close()
			client := &Client{base: server.URL, hc: server.Client()}
			err := client.Drain(context.Background(), "cell-a")
			var unsupported *AcceptingPatchUnsupportedError
			if !errors.As(err, &unsupported) || unsupported.StatusCode != status || !strings.Contains(err.Error(), "v0.0.274 (#378)") {
				t.Fatalf("error = %v, want typed PATCH compatibility error", err)
			}
			if errors.Is(err, ErrNotRegistered) {
				t.Fatal("unsupported response must not authorize teardown")
			}
			if want := []string{"PATCH /v1/cells/cell-a"}; !reflect.DeepEqual(requests, want) {
				t.Fatalf("requests = %v, want %v", requests, want)
			}
		})
	}
}

func TestDrainUnknownCellIsNotRegistered(t *testing.T) {
	var requests []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.Method+" "+r.URL.Path)
		http.Error(w, `{"schema_version":"witself.v0","error":"unknown cell"}`, http.StatusNotFound)
	}))
	defer server.Close()
	client := &Client{base: server.URL, hc: server.Client()}
	err := client.Drain(context.Background(), "cell-a")
	if !errors.Is(err, ErrNotRegistered) {
		t.Fatalf("error = %v, want ErrNotRegistered", err)
	}
	var unsupported *AcceptingPatchUnsupportedError
	if errors.As(err, &unsupported) {
		t.Fatalf("error = %v, authoritative absence is not a compatibility error", err)
	}
	if want := []string{"PATCH /v1/cells/cell-a"}; !reflect.DeepEqual(requests, want) {
		t.Fatalf("requests = %v, want %v", requests, want)
	}
}

func TestDoRejectsTypedNilTargetBeforeRequest(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		_, _ = w.Write([]byte(`{}`))
	}))
	defer server.Close()
	client := &Client{base: server.URL, hc: server.Client()}
	var ack *registrationAck
	code, body, err := client.do(context.Background(), http.MethodPost, "/v1/cells", Cell{Name: "cell-a"}, ack)
	if err == nil || !strings.Contains(err.Error(), "response target must be a non-nil pointer") {
		t.Fatalf("error = %v, want clear typed-nil rejection", err)
	}
	if calls != 0 || code != 0 || body != "" {
		t.Fatalf("typed-nil target reached HTTP: requests=%d status=%d", calls, code)
	}
	// A genuinely nil interface still supports response-free requests.
	if _, _, err := client.do(context.Background(), http.MethodDelete, "/v1/cells/cell-a", nil, nil); err != nil || calls != 1 {
		t.Fatalf("nil interface rejected: calls=%d error=%v", calls, err)
	}
}

func TestDrainRejectsInvalidCellNameBeforeRequest(t *testing.T) {
	client := &Client{}
	for _, name := range []string{"", "Cell-A", "cell-a/other", "cell-a?query", strings.Repeat("a", 65)} {
		if err := client.Drain(context.Background(), name); err == nil || !strings.Contains(err.Error(), "cell name must contain") {
			t.Errorf("name %q: error = %v", name, err)
		}
	}
}
