package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/witwave-ai/witself/infra/pulumi/internal/fleet"
)

// TestEvacuateCellPendingRows pins how the evacuation loop treats the control
// plane's pending rows: a busy account is neither evacuated nor a stall, and a
// pending row that is not retryable stops the loop with the account named.
func TestEvacuateCellPendingRows(t *testing.T) {
	previous, previousWaits := pendingPause, maxPendingWaits
	pendingPause, maxPendingWaits = 10*time.Millisecond, 3
	defer func() { pendingPause, maxPendingWaits = previous, previousWaits }()

	tests := []struct {
		name         string
		responses    []string
		wantErr      string
		wantCalls    int
		wantPending  int
		wantEvacuted int
		anyErr       bool // wantErr need not mention "not retryable"
	}{
		{
			name: "busy row waits and the next call evacuates",
			responses: []string{
				`{"evacuated":[{"account_id":"acc_busy","ok":true,"pending":true,"retryable":true,"reason":"busy"}],"remaining":1}`,
				`{"evacuated":[{"account_id":"acc_busy","ok":true}],"remaining":0}`,
			},
			wantCalls: 2, wantPending: 1, wantEvacuted: 1,
		},
		{
			name: "pending row does not trip the stall check",
			responses: []string{
				`{"evacuated":[{"account_id":"acc_a","ok":true}],"remaining":1}`,
				`{"evacuated":[{"account_id":"acc_busy","ok":true,"pending":true,"retryable":true}],"remaining":1}`,
				`{"evacuated":[{"account_id":"acc_busy","ok":true}],"remaining":0}`,
			},
			wantCalls: 3, wantPending: 1, wantEvacuted: 2,
		},
		{
			name: "fence that never clears stops after the wait bound",
			responses: []string{
				`{"evacuated":[{"account_id":"acc_busy","ok":true,"pending":true,"retryable":true}],"remaining":1}`,
			},
			wantErr: "still pending after 3 waits", wantCalls: 4, wantPending: 4, anyErr: true,
		},
		{
			name: "mixed batch whose ok rows never retire trips the stall check",
			responses: []string{
				`{"evacuated":[{"account_id":"acc_a","ok":true},{"account_id":"acc_busy","ok":true,"pending":true,"retryable":true}],"remaining":2}`,
				`{"evacuated":[{"account_id":"acc_a","ok":true},{"account_id":"acc_busy","ok":true,"pending":true,"retryable":true}],"remaining":2}`,
			},
			wantErr: "not making progress", wantCalls: 2, wantPending: 2, wantEvacuted: 2, anyErr: true,
		},
		{
			name: "pending row that is not retryable stops with the account named",
			responses: []string{
				`{"evacuated":[{"account_id":"acc_stuck","ok":true,"pending":true,"retryable":false}],"remaining":1}`,
			},
			wantErr: "acc_stuck", wantCalls: 1,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				body := tc.responses[len(tc.responses)-1]
				if calls < len(tc.responses) {
					body = tc.responses[calls]
				}
				calls++
				_, _ = w.Write([]byte(body))
			}))
			defer srv.Close()
			credential := filepath.Join(t.TempDir(), "fixture-credential")
			if err := os.WriteFile(credential, []byte("test-fleet"), 0o600); err != nil {
				t.Fatal(err)
			}
			cl, err := fleet.NewClient(srv.URL, credential)
			if err != nil {
				t.Fatal(err)
			}
			log, err := os.CreateTemp(t.TempDir(), "evacuate-output")
			if err != nil {
				t.Fatal(err)
			}
			old := os.Stderr
			os.Stderr = log
			err = evacuateCell(context.Background(), cl, "cell-a")
			os.Stderr = old
			_ = log.Close()
			if tc.wantErr == "" && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr) || (!tc.anyErr && !strings.Contains(err.Error(), "not retryable"))) {
				t.Fatalf("error = %v, want it to contain %q", err, tc.wantErr)
			}
			if calls != tc.wantCalls {
				t.Fatalf("calls = %d, want %d", calls, tc.wantCalls)
			}
			data, err := os.ReadFile(log.Name())
			if err != nil {
				t.Fatal(err)
			}
			if got := strings.Count(string(data), "pending acc_"); got != tc.wantPending {
				t.Fatalf("pending lines = %d, want %d:\n%s", got, tc.wantPending, data)
			}
			if got := strings.Count(string(data), "evacuated acc_"); got != tc.wantEvacuted {
				t.Fatalf("evacuated lines = %d, want %d:\n%s", got, tc.wantEvacuted, data)
			}
		})
	}
}
