package client

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func fleetTestCell() map[string]any {
	return map[string]any{
		"name":                     "civo-sandbox-usw2-dev",
		"endpoint":                 "https://cell.example.com",
		"cloud":                    "civo",
		"region":                   "PHX1",
		"region_code":              "usw2",
		"channel":                  "experimental",
		"weight":                   float64(0),
		"accepting":                true,
		"backup_validation_target": false,
		"has_provision_token":      true,
		"has_backup_token":         true,
	}
}

func TestFleetSetAcceptingSendsOnlyAuthoritativeMutation(t *testing.T) {
	for _, accepting := range []bool{false, true} {
		name := "drain"
		if accepting {
			name = "undrain"
		}
		t.Run(name, func(t *testing.T) {
			var requests []string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests = append(requests, r.Method+" "+r.URL.RequestURI())
				if r.Header.Get("Authorization") != "Bearer fleet-test-token" {
					t.Error("missing fleet authorization")
				}
				if r.Method != http.MethodPatch || r.URL.RequestURI() != "/v1/cells/civo-sandbox-usw2-dev" {
					t.Error("accepting repair attempted a snapshot read or registration upsert")
					http.NotFound(w, r)
					return
				}
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Errorf("decode accepting mutation: %v", err)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				if !reflect.DeepEqual(body, map[string]any{"accepting": accepting}) {
					t.Errorf("mutation replays registration metadata: %#v", body)
				}
				if r.Header.Get("Content-Type") != "application/json" {
					t.Error("mutation missing JSON content type")
				}
				cell := fleetTestCell()
				cell["accepting"] = accepting
				cell["provision_token"] = "private-provision-value"
				cell["backup_token"] = "private-backup-value"
				_ = json.NewEncoder(w).Encode(map[string]any{"schema_version": "witself.v0", "cell": cell})
			}))
			defer server.Close()
			result, err := SetFleetCellAccepting(context.Background(), server.URL+"/", "fleet-test-token", "civo-sandbox-usw2-dev", accepting)
			if err != nil {
				t.Fatal(err)
			}
			if result.SchemaVersion != "witself.v0" || result.Cell.Accepting == nil || *result.Cell.Accepting != accepting {
				t.Fatalf("unexpected acknowledgement: %#v", result)
			}
			if result.Cell.ProvisionToken != "" || result.Cell.BackupToken != "" {
				t.Fatal("repair response retained credentials")
			}
			if want := []string{"PATCH /v1/cells/civo-sandbox-usw2-dev"}; !reflect.DeepEqual(requests, want) {
				t.Fatalf("requests = %v, want %v", requests, want)
			}
		})
	}
}

func TestFleetRegisterCredentialContractAndAcknowledgement(t *testing.T) {
	accepting := false
	var body FleetCell
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/cells" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") != "Bearer fleet-test-token" {
			t.Error("missing fleet authorization")
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode registration: %v", err)
			return
		}
		response := body
		response.HasProvisionToken, response.HasBackupToken = true, true
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(FleetCellResult{SchemaVersion: "witself.v0", Cell: response})
	}))
	defer server.Close()
	cell := FleetCell{
		Name: "civo-sandbox-usw2-dev", Endpoint: "https://cell.example.com",
		Accepting: &accepting, BackupValidationTarget: true,
		ProvisionToken: "witself_prv_test", BackupToken: "witself_bak_test",
	}
	result, err := RegisterFleetCell(context.Background(), server.URL, "fleet-test-token", cell)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(body, cell) {
		t.Fatal("registration changed supplied credential or isolation metadata")
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), cell.ProvisionToken) || strings.Contains(string(encoded), cell.BackupToken) {
		t.Fatal("registration JSON exposed a write-only credential")
	}
	if !result.Cell.HasProvisionToken || !result.Cell.HasBackupToken || !result.Cell.BackupValidationTarget {
		t.Fatal("registration acknowledgement lost public credential or isolation metadata")
	}
}

func TestFleetRegisterRejectsAmbiguousAcknowledgements(t *testing.T) {
	tests := []struct {
		name   string
		change func(map[string]any, map[string]any)
	}{
		{"schema", func(out, _ map[string]any) { out["schema_version"] = "other" }},
		{"name", func(_, cell map[string]any) { cell["name"] = "other" }},
		{"missing accepting", func(_, cell map[string]any) { delete(cell, "accepting") }},
		{"still accepting", func(_, cell map[string]any) { cell["accepting"] = true }},
		{"missing isolation", func(_, cell map[string]any) { delete(cell, "backup_validation_target") }},
		{"wrong isolation", func(_, cell map[string]any) { cell["backup_validation_target"] = true }},
		{"missing backup credential", func(_, cell map[string]any) { cell["has_backup_token"] = false }},
		{"missing provision credential", func(_, cell map[string]any) { cell["has_provision_token"] = false }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cell := fleetTestCell()
			cell["accepting"] = false
			out := map[string]any{"schema_version": "witself.v0", "cell": cell}
			test.change(out, cell)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_ = json.NewEncoder(w).Encode(out)
			}))
			defer server.Close()
			accepting := false
			result, err := RegisterFleetCell(context.Background(), server.URL, "fleet-test-token", FleetCell{
				Name: "civo-sandbox-usw2-dev", Endpoint: "https://cell.example.com", Accepting: &accepting, HasBackupToken: true, HasProvisionToken: true,
			})
			if err == nil || result != nil {
				t.Fatalf("ambiguous acknowledgement accepted: result=%v error=%v", result, err)
			}
		})
	}
}

func TestFleetListRejectsAmbiguousMetadata(t *testing.T) {
	tests := []struct {
		name   string
		change func(map[string]any, map[string]any)
	}{
		{"missing schema", func(out, _ map[string]any) { delete(out, "schema_version") }},
		{"missing cells", func(out, _ map[string]any) { delete(out, "cells") }},
		{"null cells", func(out, _ map[string]any) { out["cells"] = nil }},
		{"missing accepting", func(_, cell map[string]any) { delete(cell, "accepting") }},
		{"missing isolation", func(_, cell map[string]any) { delete(cell, "backup_validation_target") }},
		{"invalid name", func(_, cell map[string]any) { cell["name"] = "../other" }},
		{"duplicate cell", func(out, cell map[string]any) { out["cells"] = []any{cell, cell} }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cell := fleetTestCell()
			out := map[string]any{"schema_version": "witself.v0", "cells": []any{cell}}
			test.change(out, cell)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_ = json.NewEncoder(w).Encode(out)
			}))
			defer server.Close()
			if cells, err := ListFleetCells(context.Background(), server.URL, "fleet-test-token"); err == nil || cells != nil {
				t.Fatalf("ambiguous list accepted: cells=%v error=%v", cells, err)
			}
		})
	}
}

func TestFleetListEmptyAndGetMissing(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"schema_version":"witself.v0","cells":[]}`))
	}))
	defer server.Close()
	cells, err := ListFleetCells(context.Background(), server.URL, "fleet-test-token")
	if err != nil || cells == nil || len(cells) != 0 {
		t.Fatalf("empty list = %v, error = %v", cells, err)
	}
	if _, err := GetFleetCell(context.Background(), server.URL, "fleet-test-token", "missing"); !errors.Is(err, ErrFleetCellNotRegistered) {
		t.Fatalf("missing cell error = %v", err)
	}
}

func TestFleetRepairAuthFailures(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"unauthorized"}`))
	}))
	defer server.Close()
	ctx := context.Background()
	operations := map[string]func() error{
		"list": func() error { _, err := ListFleetCells(ctx, server.URL, "wrong"); return err },
		"show": func() error { _, err := GetFleetCell(ctx, server.URL, "wrong", "cell-one"); return err },
		"register": func() error {
			_, err := RegisterFleetCell(ctx, server.URL, "wrong", FleetCell{Name: "cell-one", Endpoint: "https://cell.example.com"})
			return err
		},
		"drain":      func() error { _, err := SetFleetCellAccepting(ctx, server.URL, "wrong", "cell-one", false); return err },
		"undrain":    func() error { _, err := SetFleetCellAccepting(ctx, server.URL, "wrong", "cell-one", true); return err },
		"deregister": func() error { return DeleteFleetCell(ctx, server.URL, "wrong", "cell-one") },
	}
	for name, operation := range operations {
		t.Run(name, func(t *testing.T) {
			if err := operation(); !errors.Is(err, ErrUnauthorized) {
				t.Fatalf("authentication error = %v", err)
			}
		})
	}
}

func TestFleetDeleteUsesOnlySafeRouteAndPreservesRefusal(t *testing.T) {
	for _, message := range []string{"", "cell must be drained first (re-register with accepting=false)", "accounts still live on this cell"} {
		name := message
		if name == "" {
			name = "success"
		}
		t.Run(name, func(t *testing.T) {
			requests := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				if r.Method != http.MethodDelete || r.URL.RequestURI() != "/v1/cells/cell-one" || r.Header.Get("Authorization") != "Bearer fleet-test-token" {
					t.Errorf("unexpected request: %s %s", r.Method, r.URL.RequestURI())
					http.NotFound(w, r)
					return
				}
				if message == "" {
					w.WriteHeader(http.StatusNoContent)
					return
				}
				w.WriteHeader(http.StatusConflict)
				_ = json.NewEncoder(w).Encode(map[string]any{"error": message})
			}))
			defer server.Close()
			err := DeleteFleetCell(context.Background(), server.URL, "fleet-test-token", "cell-one")
			if message == "" {
				if err != nil {
					t.Fatal(err)
				}
			} else if !errors.Is(err, ErrConflict) || err.Error() != message {
				t.Fatalf("refusal = %v, want exact server message %q", err, message)
			}
			if requests != 1 {
				t.Fatalf("request count = %d, want one safe DELETE", requests)
			}
		})
	}
}

func TestFleetUndrainPreservesAuthoritativeIsolationRefusal(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Method != http.MethodPatch || r.URL.RequestURI() != "/v1/cells/civo-sandbox-usw2-dev" {
			t.Error("undrain attempted a snapshot read or fallback upsert")
		}
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":"backup validation target cannot accept placements"}`))
	}))
	defer server.Close()
	_, err := SetFleetCellAccepting(context.Background(), server.URL, "fleet-test-token", "civo-sandbox-usw2-dev", true)
	if !errors.Is(err, ErrConflict) || err.Error() != "backup validation target cannot accept placements" || requests != 1 {
		t.Fatalf("backup target undrain: error=%v requests=%d", err, requests)
	}
}

func TestFleetDeleteRejectsRouteInjection(t *testing.T) {
	for _, name := range []string{"", "cell-one:purge", "../cell-one", "Cell-One", strings.Repeat("a", 65)} {
		if err := DeleteFleetCell(context.Background(), "invalid endpoint", "fleet-test-token", name); err == nil || !strings.Contains(err.Error(), "cell name must contain") {
			t.Errorf("invalid name %q was not rejected before a request: %v", name, err)
		}
	}
}

func TestFleetEndpointsCannotRedirectRepairRoutes(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	ctx := context.Background()
	operations := map[string]func(string) error{
		"list": func(ep string) error { _, err := ListFleetCells(ctx, ep, "private-token"); return err },
		"show": func(ep string) error { _, err := GetFleetCell(ctx, ep, "private-token", "cell-one"); return err },
		"register": func(ep string) error {
			_, err := RegisterFleetCell(ctx, ep, "private-token", FleetCell{Name: "cell-one", Endpoint: "https://cell.example.com"})
			return err
		},
		"drain": func(ep string) error {
			_, err := SetFleetCellAccepting(ctx, ep, "private-token", "cell-one", false)
			return err
		},
		"undrain": func(ep string) error {
			_, err := SetFleetCellAccepting(ctx, ep, "private-token", "cell-one", true)
			return err
		},
		"deregister": func(ep string) error { return DeleteFleetCell(ctx, ep, "private-token", "cell-one") },
	}
	for name, operation := range operations {
		for _, suffix := range []string{"/v1/cells/production:purge#", "/v1/cells/production:purge?", "#", "?", "/#", "/?", "#fragment", "?query=value", "/prefix", "/%2F", "/v1/cells/production%3Apurge"} {
			t.Run(name+suffix, func(t *testing.T) {
				err := operation(server.URL + suffix)
				if err == nil || !strings.Contains(err.Error(), "control-plane endpoint") || requests != 0 {
					t.Fatalf("unsafe endpoint: error=%v requests=%d", err, requests)
				}
				if strings.Contains(err.Error(), "private-token") || strings.Contains(err.Error(), "production") {
					t.Fatal("endpoint validation exposed credentials or the supplied target")
				}
			})
		}
	}
}

func TestFleetRequestURLRequiresOriginAndExactRoute(t *testing.T) {
	for _, endpoint := range []string{"https://control.example", "https://control.example/", "http://127.0.0.1:5599"} {
		for _, route := range []string{"/v1/cells", "/v1/cells/cell-one"} {
			got, err := fleetRequestURL(endpoint, route)
			if err != nil || got != strings.TrimSuffix(endpoint, "/")+route {
				t.Errorf("valid origin: got %q, error=%v", got, err)
			}
		}
	}
	for _, endpoint := range []string{"", "//control.example", "https:control.example", "https://", "ftp://control.example", "https://user:secret@control.example", "https://control.example/%2f"} {
		if _, err := fleetRequestURL(endpoint, "/v1/cells"); err == nil {
			t.Errorf("accepted invalid endpoint %q", endpoint)
		}
	}
	if _, err := fleetRequestURL("https://control.example", "/v1/cells/other#"); err == nil {
		t.Fatal("accepted a route whose serialized request path differs")
	}
}

func TestFleetSetAcceptingNeverFallsBackOnOlderControlPlanes(t *testing.T) {
	for _, status := range []int{http.StatusNotFound, http.StatusMethodNotAllowed, http.StatusNotImplemented} {
		requests := 0
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requests++
			if r.Method != http.MethodPatch || r.URL.RequestURI() != "/v1/cells/cell-one" {
				t.Error("attempted an unsafe fallback route")
			}
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"error":"accepting-only mutation unavailable"}`))
		}))
		_, err := SetFleetCellAccepting(context.Background(), server.URL, "fleet-test-token", "cell-one", true)
		server.Close()
		if err == nil || requests != 1 {
			t.Fatalf("status %d: error=%v requests=%d", status, err, requests)
		}
	}
}

func TestFleetSetAcceptingRejectsAmbiguousAcknowledgements(t *testing.T) {
	for _, change := range []func(map[string]any, map[string]any){
		func(out, _ map[string]any) { out["schema_version"] = "other" },
		func(_, cell map[string]any) { cell["name"] = "other" },
		func(_, cell map[string]any) { delete(cell, "accepting") },
		func(_, cell map[string]any) { cell["accepting"] = false },
		func(_, cell map[string]any) { delete(cell, "backup_validation_target") },
		func(_, cell map[string]any) { cell["backup_validation_target"] = true },
	} {
		cell := fleetTestCell()
		out := map[string]any{"schema_version": "witself.v0", "cell": cell}
		change(out, cell)
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(out)
		}))
		result, err := SetFleetCellAccepting(context.Background(), server.URL, "fleet-test-token", "civo-sandbox-usw2-dev", true)
		server.Close()
		if err == nil || result != nil {
			t.Fatalf("ambiguous acknowledgement accepted: result=%v error=%v", result, err)
		}
	}
}
