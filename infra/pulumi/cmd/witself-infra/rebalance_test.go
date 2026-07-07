package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRebalanceFleetLoop(t *testing.T) {
	tests := []struct {
		name      string
		responses []rebalanceResponse
		wantErr   string
		wantCalls int
	}{
		{
			name: "clean sweep terminates when remaining=0",
			responses: []rebalanceResponse{
				{Rebalanced: []rb{{AccountID: "acc_1", OK: true, FromCell: "aws-use1", ToCell: "gcp-use1"}}, Remaining: 1},
				{Rebalanced: []rb{{AccountID: "acc_2", OK: true, FromCell: "aws-use1", ToCell: "gcp-use1"}}, Remaining: 0},
			},
			wantCalls: 2,
		},
		{
			name: "per-account failure surfaces immediately",
			responses: []rebalanceResponse{
				{Rebalanced: []rb{{AccountID: "acc_bad", OK: false, FromCell: "aws-use1", ToCell: "gcp-use1", Error: "import 500"}}, Remaining: 1},
			},
			wantErr:   "rebalance failed for acc_bad",
			wantCalls: 1,
		},
		{
			name: "batch reporting nothing but remaining>0 halts",
			responses: []rebalanceResponse{
				{Rebalanced: []rb{}, Remaining: 2},
			},
			wantErr:   "rebalance stalled",
			wantCalls: 1,
		},
		{
			name: "empty fleet is a no-op",
			responses: []rebalanceResponse{
				{Rebalanced: []rb{}, Remaining: 0},
			},
			wantCalls: 1,
		},
		{
			name: "no progress between batches halts",
			responses: []rebalanceResponse{
				{Rebalanced: []rb{{AccountID: "acc_1", OK: true, FromCell: "aws-use1", ToCell: "gcp-use1"}}, Remaining: 3},
				{Rebalanced: []rb{{AccountID: "acc_2", OK: true, FromCell: "aws-use1", ToCell: "gcp-use1"}}, Remaining: 3},
			},
			wantErr:   "not making progress",
			wantCalls: 2,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/placement:rebalance" {
					http.NotFound(w, r)
					return
				}
				if calls >= len(tc.responses) {
					t.Errorf("unexpected extra call %d", calls+1)
					http.Error(w, "unexpected", http.StatusInternalServerError)
					return
				}
				resp := tc.responses[calls]
				calls++
				_ = json.NewEncoder(w).Encode(resp)
			}))
			defer srv.Close()

			err := rebalanceFleet(context.Background(), srv.URL, writeFleetToken(t), 1, false)
			if calls != tc.wantCalls {
				t.Errorf("calls = %d, want %d", calls, tc.wantCalls)
			}
			if tc.wantErr == "" {
				if err != nil {
					t.Errorf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %v, want substring %q", err, tc.wantErr)
			}
		})
	}
}

func TestRebalanceFleetDryRunCallsOnce(t *testing.T) {
	calls := 0
	var got struct {
		Batch  int  `json:"batch"`
		DryRun bool `json:"dry_run"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/placement:rebalance" {
			http.NotFound(w, r)
			return
		}
		calls++
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode rebalance request: %v", err)
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(rebalanceResponse{
			Rebalanced: []rb{{AccountID: "acc_1", OK: true, FromCell: "aws-use1", ToCell: "gcp-use1", DryRun: true}},
			Remaining:  9,
			DryRun:     true,
		})
	}))
	defer srv.Close()

	if err := rebalanceFleet(context.Background(), srv.URL, writeFleetToken(t), 3, true); err != nil {
		t.Fatalf("rebalanceFleet: %v", err)
	}
	if calls != 1 {
		t.Fatalf("calls = %d, want 1", calls)
	}
	if got.Batch != 3 || !got.DryRun {
		t.Fatalf("request = {batch:%d dry_run:%v}, want {3 true}", got.Batch, got.DryRun)
	}
}

func TestPlacementRunnerFleetEnableAndRun(t *testing.T) {
	var calls []string
	var setBody map[string]any
	var runBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.Path)
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/placement-runner":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"placement_runner": map[string]any{
					"enabled":            false,
					"restore_archives":   true,
					"restore_batch":      4,
					"restore_any_region": false,
					"rebalance":          true,
					"rebalance_batch":    1,
				},
			})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/placement-runner":
			if err := json.NewDecoder(r.Body).Decode(&setBody); err != nil {
				t.Errorf("decode set body: %v", err)
				http.Error(w, "bad request", http.StatusBadRequest)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"placement_runner": setBody})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/placement:run":
			if err := json.NewDecoder(r.Body).Decode(&runBody); err != nil {
				t.Errorf("decode run body: %v", err)
				http.Error(w, "bad request", http.StatusBadRequest)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"placement_runner": runBody,
				"restore":          map[string]any{"restored": []any{}, "remaining": 0},
				"rebalance":        map[string]any{"rebalanced": []any{}, "remaining": 0},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	if err := placementRunnerFleet(context.Background(), srv.URL, writeFleetToken(t), true, false, true); err != nil {
		t.Fatalf("placementRunnerFleet: %v", err)
	}
	wantCalls := []string{
		"GET /v1/placement-runner",
		"POST /v1/placement-runner",
		"POST /v1/placement:run",
	}
	if strings.Join(calls, ",") != strings.Join(wantCalls, ",") {
		t.Fatalf("calls = %v, want %v", calls, wantCalls)
	}
	if setBody["enabled"] != true {
		t.Fatalf("set enabled = %v, want true", setBody["enabled"])
	}
	if runBody["enabled"] != true {
		t.Fatalf("run enabled = %v, want true", runBody["enabled"])
	}
}

func TestPlacementStatusFleetSendsLimit(t *testing.T) {
	var gotLimit string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/placement-status" {
			http.NotFound(w, r)
			return
		}
		gotLimit = r.URL.Query().Get("limit")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"placement_runner": map[string]any{
				"enabled":            true,
				"restore_archives":   true,
				"restore_batch":      4,
				"restore_any_region": false,
				"rebalance":          true,
				"rebalance_batch":    1,
			},
			"cells": []any{
				map[string]any{
					"name":                "aws-sandbox-use1-dev",
					"cloud":               "aws",
					"region_code":         "use1",
					"channel":             "experimental",
					"accepting":           true,
					"has_provision_token": true,
					"account_count":       1,
					"archived_count":      0,
				},
			},
			"archived": map[string]any{"total": 0, "placeable": 0, "unplaced": 0},
			"live":     map[string]any{"total": 1, "movable": 0, "skipped": 0},
		})
	}))
	defer srv.Close()

	if err := placementStatusFleet(context.Background(), srv.URL, writeFleetToken(t), 7); err != nil {
		t.Fatalf("placementStatusFleet: %v", err)
	}
	if gotLimit != "7" {
		t.Fatalf("limit = %q, want 7", gotLimit)
	}
}

func writeFleetToken(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	tf := filepath.Join(dir, "fleet.token")
	if err := os.WriteFile(tf, []byte("witself_flt_TEST"), 0o600); err != nil {
		t.Fatal(err)
	}
	return tf
}

type rb struct {
	AccountID string `json:"account_id"`
	OK        bool   `json:"ok"`
	FromCell  string `json:"from_cell"`
	ToCell    string `json:"to_cell"`
	DryRun    bool   `json:"dry_run,omitempty"`
	Error     string `json:"error,omitempty"`
}

type rebalanceResponse struct {
	Rebalanced []rb `json:"rebalanced"`
	Remaining  int  `json:"remaining"`
	DryRun     bool `json:"dry_run,omitempty"`
}
