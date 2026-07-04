package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/server"
	"github.com/witwave-ai/witself/internal/store"
)

func TestReadTokenFileRejectsEmpty(t *testing.T) {
	tokenFile := filepath.Join(t.TempDir(), "bootstrap.token")
	if err := os.WriteFile(tokenFile, []byte("\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readTokenFile(tokenFile, true); err == nil {
		t.Fatal("readTokenFile empty file = nil error, want error")
	}
}

func TestBootstrapTokenTTL(t *testing.T) {
	t.Setenv("WITSELF_BOOTSTRAP_TOKEN_TTL", "")
	ttl, err := bootstrapTokenTTL()
	if err != nil {
		t.Fatal(err)
	}
	if ttl != 24*time.Hour {
		t.Fatalf("default TTL = %s, want 24h", ttl)
	}

	t.Setenv("WITSELF_BOOTSTRAP_TOKEN_TTL", "30m")
	ttl, err = bootstrapTokenTTL()
	if err != nil {
		t.Fatal(err)
	}
	if ttl != 30*time.Minute {
		t.Fatalf("configured TTL = %s, want 30m", ttl)
	}

	t.Setenv("WITSELF_BOOTSTRAP_TOKEN_TTL", "0")
	if _, err := bootstrapTokenTTL(); err == nil {
		t.Fatal("zero TTL = nil error, want error")
	}
}

// TestMapSupportErrorNoDoublePrefix pins the fix for the sentinel
// double-print: when store.ErrX and server.ErrX have the same
// Error() text, the mapper must NOT produce "invalid ...: invalid
// ...: real detail". The handler surfaces err.Error() to the
// client, so drift here shows up in every 4xx response body.
func TestMapSupportErrorNoDoublePrefix(t *testing.T) {
	tests := []struct {
		name        string
		in          error
		wantIs      error
		wantMessage string
	}{
		{
			name:        "ticket-input-invalid keeps its detail without doubling the sentinel",
			in:          fmt.Errorf("%w: subject required", store.ErrTicketInputInvalid),
			wantIs:      server.ErrTicketInputInvalid,
			wantMessage: "invalid support ticket input: subject required",
		},
		{
			name:        "ticket-state-invalid keeps its detail without doubling",
			in:          fmt.Errorf("%w: awaiting_admin → open", store.ErrTicketStateInvalid),
			wantIs:      server.ErrTicketStateInvalid,
			wantMessage: "invalid ticket state transition: awaiting_admin → open",
		},
		{
			name:        "bare sentinel from the store maps cleanly to a bare server sentinel",
			in:          store.ErrTicketInputInvalid,
			wantIs:      server.ErrTicketInputInvalid,
			wantMessage: "invalid support ticket input",
		},
		{
			name:        "ticket-not-found bypasses the wrapper entirely (no detail from store)",
			in:          store.ErrTicketNotFound,
			wantIs:      server.ErrTicketNotFound,
			wantMessage: "ticket not found",
		},
		{
			name:        "support-disabled keeps store detail without doubling",
			in:          fmt.Errorf("%w: plan tier does not include support", store.ErrSupportDisabled),
			wantIs:      server.ErrSupportDisabled,
			wantMessage: "support is not enabled for this account: plan tier does not include support",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := mapSupportError(tc.in)
			if !errors.Is(got, tc.wantIs) {
				t.Errorf("errors.Is(%v, %v) = false, want true", got, tc.wantIs)
			}
			if got.Error() != tc.wantMessage {
				t.Errorf("Error() = %q, want %q", got.Error(), tc.wantMessage)
			}
			// The guard: the sentinel text must not appear twice.
			if strings.Count(got.Error(), tc.wantIs.Error()) > 1 {
				t.Errorf("sentinel %q appears more than once in %q", tc.wantIs.Error(), got.Error())
			}
		})
	}
}
