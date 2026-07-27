package fleet

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

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
				"name":                     "civo-sandbox-usw2-dev",
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
		Name:           "civo-sandbox-usw2-dev",
		Endpoint:       "https://api.cell.example.com",
		Cloud:          "civo",
		Region:         "PHX1",
		RegionCode:     "usw2",
		Channel:        "experimental",
		ProvisionToken: "witself_prv_provision-only",
		BackupToken:    "witself_bak_backup-only",
	})
	if err != nil {
		t.Fatal(err)
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
				"name":                     "civo-sandbox-use1-dev",
				"accepting":                false,
				"backup_validation_target": true,
				"has_backup_token":         true,
			},
		})
	}))
	defer server.Close()

	client := &Client{base: server.URL, token: "fleet-test-token", hc: server.Client()}
	err := client.Register(context.Background(), Cell{
		Name:                   "civo-sandbox-use1-dev",
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
				"name":             "civo-sandbox-use1-dev",
				"accepting":        false,
				"has_backup_token": true,
			},
			wantErr: "did not acknowledge backup_validation_target=true",
		},
		{
			name: "still accepting",
			cellAck: map[string]any{
				"name":                     "civo-sandbox-use1-dev",
				"accepting":                true,
				"backup_validation_target": true,
				"has_backup_token":         true,
			},
			wantErr: "did not acknowledge accepting=false",
		},
		{
			name: "missing accepting",
			cellAck: map[string]any{
				"name":                     "civo-sandbox-use1-dev",
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
				Name:                   "civo-sandbox-use1-dev",
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
			Name:                   "civo-sandbox-use1-dev",
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
				"name":                     "civo-sandbox-usw2-dev",
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
		Name:           "civo-sandbox-usw2-dev",
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
		Name:           "civo-sandbox-usw2-dev",
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
				"name":                     "civo-sandbox-usw2-dev",
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
		Name:           "civo-sandbox-usw2-dev",
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
		Name:     "civo-sandbox-usw2-dev",
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
				"name":                     "civo-sandbox-usw2-dev",
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
