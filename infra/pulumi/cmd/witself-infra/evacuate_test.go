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

// TestEvacuateCellLoop pins the four stop conditions of the evacuation loop:
// clean sweep terminates, per-account failure surfaces, no-progress halts the
// loop rather than spinning, and the empty-cell case is a no-op.
func TestEvacuateCellLoop(t *testing.T) {
	tests := []struct {
		name       string
		responses  []evacResponse // one per POST :evacuate call
		wantErr    string         // substring; empty means expect no error
		wantCalls  int
	}{
		{
			name: "clean sweep terminates when remaining=0",
			responses: []evacResponse{
				{Evacuated: []ea{{"acc_1", true, ""}, {"acc_2", true, ""}}, Remaining: 1},
				{Evacuated: []ea{{"acc_3", true, ""}}, Remaining: 0},
			},
			wantErr:   "",
			wantCalls: 2,
		},
		{
			name: "per-account failure surfaces immediately",
			responses: []evacResponse{
				{Evacuated: []ea{{"acc_1", true, ""}, {"acc_bad", false, "cell 502"}}, Remaining: 3},
			},
			wantErr:   "evacuation failed for acc_bad",
			wantCalls: 1,
		},
		{
			name: "batch reporting nothing but remaining>0 halts",
			responses: []evacResponse{
				{Evacuated: []ea{}, Remaining: 5},
			},
			wantErr:   "evacuation stalled",
			wantCalls: 1,
		},
		{
			name: "empty cell is a no-op",
			responses: []evacResponse{
				{Evacuated: []ea{}, Remaining: 0},
			},
			wantErr:   "",
			wantCalls: 1,
		},
		{
			// Server acknowledges per-account success but its Remaining
			// count doesn't strictly decrease between batches — a
			// control-plane bug the review panel flagged. Failing loudly
			// here beats spinning forever.
			name: "no progress between batches halts",
			responses: []evacResponse{
				{Evacuated: []ea{{"acc_1", true, ""}}, Remaining: 5},
				{Evacuated: []ea{{"acc_2", true, ""}}, Remaining: 5},
			},
			wantErr:   "not making progress",
			wantCalls: 2,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if !strings.HasSuffix(r.URL.Path, ":evacuate") {
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

			// A throwaway token file the client can read.
			dir := t.TempDir()
			tf := filepath.Join(dir, "fleet.token")
			if err := os.WriteFile(tf, []byte("witself_flt_TEST"), 0o600); err != nil {
				t.Fatal(err)
			}
			cl, err := fleet.NewClient(srv.URL, tf)
			if err != nil {
				t.Fatal(err)
			}
			err = evacuateCell(context.Background(), cl, "cell-a")
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

// The JSON-tag shape must match fleet.EvacuationResult verbatim so the
// server-shaped fixtures decode into what the client expects.
type ea struct {
	AccountID string `json:"account_id"`
	OK        bool   `json:"ok"`
	Error     string `json:"error,omitempty"`
}
type evacResponse struct {
	Evacuated []ea `json:"evacuated"`
	Remaining int  `json:"remaining"`
}
