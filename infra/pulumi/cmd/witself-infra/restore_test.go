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

	"github.com/witwave-ai/witself/infra/pulumi/internal/fleet"
)

// TestRestoreCellLoop pins the five stop conditions of the restore loop.
// The shape mirrors TestEvacuateCellLoop because a restore driver that
// spins is exactly as bad as an evacuation driver that spins — worse,
// actually, because the operator is now watching Pulumi's up progress and
// expects it to terminate.
func TestRestoreCellLoop(t *testing.T) {
	tests := []struct {
		name      string
		responses []restoreResponse
		wantErr   string
		wantCalls int
	}{
		{
			name: "clean sweep terminates when remaining=0",
			responses: []restoreResponse{
				{Restored: []ra{{"acc_1", true, ""}, {"acc_2", true, ""}}, Remaining: 1, Region: "us-west-2"},
				{Restored: []ra{{"acc_3", true, ""}}, Remaining: 0, Region: "us-west-2"},
			},
			wantErr:   "",
			wantCalls: 2,
		},
		{
			name: "per-account failure surfaces immediately",
			responses: []restoreResponse{
				{Restored: []ra{{"acc_1", true, ""}, {"acc_bad", false, "import 500"}}, Remaining: 3, Region: "us-west-2"},
			},
			wantErr:   "restore failed for acc_bad",
			wantCalls: 1,
		},
		{
			name: "batch reporting nothing but remaining>0 halts",
			responses: []restoreResponse{
				{Restored: []ra{}, Remaining: 5, Region: "us-west-2"},
			},
			wantErr:   "restore stalled",
			wantCalls: 1,
		},
		{
			name: "empty region is a no-op",
			responses: []restoreResponse{
				{Restored: []ra{}, Remaining: 0, Region: "us-west-2"},
			},
			wantErr:   "",
			wantCalls: 1,
		},
		{
			// The Worker acknowledges per-account success but Remaining
			// doesn't strictly decrease — the mirror of the no-strict-
			// decrease guard in the evacuation loop.
			name: "no progress between batches halts",
			responses: []restoreResponse{
				{Restored: []ra{{"acc_1", true, ""}}, Remaining: 5, Region: "us-west-2"},
				{Restored: []ra{{"acc_2", true, ""}}, Remaining: 5, Region: "us-west-2"},
			},
			wantErr:   "not making progress",
			wantCalls: 2,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if !strings.HasSuffix(r.URL.Path, ":restore") {
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

			dir := t.TempDir()
			tf := filepath.Join(dir, "fleet.token")
			if err := os.WriteFile(tf, []byte("witself_flt_TEST"), 0o600); err != nil {
				t.Fatal(err)
			}
			cl, err := fleet.NewClient(srv.URL, tf)
			if err != nil {
				t.Fatal(err)
			}
			err = restoreCell(context.Background(), cl, "cell-a", false)
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

func TestRestoreCellAllRegionsSendsOverride(t *testing.T) {
	var got struct {
		Batch      int  `json:"batch"`
		AllRegions bool `json:"all_regions"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, ":restore") {
			http.NotFound(w, r)
			return
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode restore request: %v", err)
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(restoreResponse{
			Restored:  []ra{},
			Remaining: 0,
			Region:    "all",
		})
	}))
	defer srv.Close()

	dir := t.TempDir()
	tf := filepath.Join(dir, "fleet.token")
	if err := os.WriteFile(tf, []byte("witself_flt_TEST"), 0o600); err != nil {
		t.Fatal(err)
	}
	cl, err := fleet.NewClient(srv.URL, tf)
	if err != nil {
		t.Fatal(err)
	}
	if err := restoreCell(context.Background(), cl, "cell-a", true); err != nil {
		t.Fatalf("restoreCell: %v", err)
	}
	if got.Batch != 4 {
		t.Fatalf("batch = %d, want 4", got.Batch)
	}
	if !got.AllRegions {
		t.Fatal("all_regions override was not sent")
	}
}

// The JSON-tag shape must match fleet.RestoreResult verbatim so the
// Worker-shaped fixtures decode into what the client expects.
type ra struct {
	AccountID string `json:"account_id"`
	OK        bool   `json:"ok"`
	Error     string `json:"error,omitempty"`
}
type restoreResponse struct {
	Restored  []ra   `json:"restored"`
	Remaining int    `json:"remaining"`
	Region    string `json:"region"`
}
