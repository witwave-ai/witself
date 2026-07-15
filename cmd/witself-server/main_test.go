package main

import (
	"context"
	"encoding/json"
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

func TestFactDeletionEnabledFromEnv(t *testing.T) {
	original, wasSet := os.LookupEnv(factDeletionEnv)
	t.Cleanup(func() {
		if wasSet {
			_ = os.Setenv(factDeletionEnv, original)
			return
		}
		_ = os.Unsetenv(factDeletionEnv)
	})

	if err := os.Unsetenv(factDeletionEnv); err != nil {
		t.Fatal(err)
	}
	if enabled, err := factDeletionEnabledFromEnv(); err != nil || enabled {
		t.Fatalf("unset feature flag = (%t, %v), want (false, nil)", enabled, err)
	}

	for _, tc := range []struct {
		value string
		want  bool
	}{
		{value: "false", want: false},
		{value: "true", want: true},
		{value: " TRUE ", want: true},
	} {
		if err := os.Setenv(factDeletionEnv, tc.value); err != nil {
			t.Fatal(err)
		}
		enabled, err := factDeletionEnabledFromEnv()
		if err != nil || enabled != tc.want {
			t.Fatalf("feature flag %q = (%t, %v), want (%t, nil)", tc.value, enabled, err, tc.want)
		}
	}

	for _, value := range []string{"", "enabled", "sometimes"} {
		if err := os.Setenv(factDeletionEnv, value); err != nil {
			t.Fatal(err)
		}
		if _, err := factDeletionEnabledFromEnv(); err == nil || !strings.Contains(err.Error(), factDeletionEnv) {
			t.Fatalf("feature flag %q error = %v, want named validation error", value, err)
		}
	}
}

func TestValidateFactDeletionFeatureSchemaGate(t *testing.T) {
	tests := []struct {
		name          string
		enabled       bool
		schemaVersion int
		wantErr       bool
	}{
		{name: "disabled on phase A schema", enabled: false, schemaVersion: 27},
		{name: "enabled on phase A schema", enabled: true, schemaVersion: 27, wantErr: true},
		{name: "enabled at minimum schema", enabled: true, schemaVersion: 28},
		{name: "enabled after minimum schema", enabled: true, schemaVersion: 29},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validateFactDeletionFeature(tc.enabled, tc.schemaVersion)
			if (err != nil) != tc.wantErr {
				t.Fatalf("validateFactDeletionFeature(%t, %d) error = %v, wantErr %t", tc.enabled, tc.schemaVersion, err, tc.wantErr)
			}
			if err != nil && (!strings.Contains(err.Error(), factDeletionEnv) || !strings.Contains(err.Error(), "28")) {
				t.Fatalf("schema gate error = %q, want flag and minimum schema", err)
			}
		})
	}
}

func TestConfigureFactMutationsDeletionGate(t *testing.T) {
	var disabled server.Config
	configureFactMutations(&disabled, nil, false)
	if disabled.SetFact == nil {
		t.Fatal("ordinary fact set was not wired while deletion is disabled")
	}
	if disabled.DeleteFact != nil {
		t.Fatal("permanent fact deletion was wired while feature is disabled")
	}
	_, err := disabled.SetFact(context.Background(), server.DomainPrincipal{}, server.SetFactRequest{RecreateDeleted: true})
	if !errors.Is(err, server.ErrBadInput) || !strings.Contains(err.Error(), factDeletionEnv) {
		t.Fatalf("disabled recreation error = %v, want mapped client error naming feature flag", err)
	}

	var enabled server.Config
	configureFactMutations(&enabled, nil, true)
	if enabled.SetFact == nil || enabled.DeleteFact == nil {
		t.Fatalf("enabled mutation wiring = SetFact %v, DeleteFact %v; want both wired", enabled.SetFact != nil, enabled.DeleteFact != nil)
	}
}

func TestPrincipalAdaptersPreserveCredentialAuthority(t *testing.T) {
	expiresAt := time.Date(2026, 7, 15, 8, 30, 0, 0, time.UTC)
	stored := store.Principal{
		Kind: store.PrincipalAgent, ID: "agent_1", TokenID: "tok_curator",
		AccessProfile: store.AccessProfileCuratorPreview, TokenExpiresAt: &expiresAt,
		AccountID: "acc_1", RealmID: "realm_1", AgentName: "memory-agent",
		RealmName: "default", AccountStatus: "active",
	}
	domain := toServerPrincipal(stored)
	if domain.TokenID != stored.TokenID || domain.AccessProfile != stored.AccessProfile ||
		domain.TokenExpiresAt == nil || !domain.TokenExpiresAt.Equal(expiresAt) {
		t.Fatalf("store to server principal lost credential fields: %#v", domain)
	}
	roundTrip := toStorePrincipal(domain)
	if roundTrip != stored {
		t.Fatalf("principal round trip = %#v, want %#v", roundTrip, stored)
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

func TestFactDeletionAdapterIsValueFree(t *testing.T) {
	deletedAt := time.Date(2026, 7, 13, 20, 30, 0, 0, time.UTC)
	got := toServerFactDeletion(store.DeleteFactResult{
		FactID: "fact_sensitive", ReceiptID: "fdel_1", SubjectID: "fsub_1", Subject: "spouse",
		Predicate: "identity/name", PriorResolvedAssertionID: "fas_current",
		AssertionCount: 2, CandidateCount: 1, CandidateRevision: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", UsageCount: 7, Sensitive: true,
		DeletedAt: &deletedAt, Applied: true, Replayed: true,
	})
	if got.FactID != "fact_sensitive" || got.ReceiptID != "fdel_1" || got.Subject != "spouse" || got.ResolvedAssertionID != "fas_current" || got.CandidateRevision != "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" || got.DeletionState != "deleted" || !got.Applied || !got.Replayed || got.DeletedAt == nil || !got.DeletedAt.Equal(deletedAt) {
		t.Fatalf("deletion adapter = %#v", got)
	}
	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{`"value"`, `"source"`, `"source_ref"`, `"evidence"`, `"delete_key"`, `"idempotency_key"`, `"request_fingerprint"`, `"prior_resolved_assertion_id"`} {
		if strings.Contains(string(raw), forbidden) {
			t.Fatalf("deletion receipt contains %s: %s", forbidden, raw)
		}
	}

	preview := toServerFactDeletion(store.DeleteFactResult{FactID: "fact_1", PriorResolvedAssertionID: "fas_1"})
	if preview.DeletionState != "active" || preview.Applied || preview.DeletedAt != nil {
		t.Fatalf("preview adapter = %#v", preview)
	}
}

func TestMapFactDeletionErrors(t *testing.T) {
	tests := []struct {
		name string
		in   error
		want error
	}{
		{name: "bad input", in: store.ErrFactInputInvalid, want: server.ErrBadInput},
		{name: "not found", in: store.ErrFactNotFound, want: server.ErrNotFound},
		{name: "forbidden", in: store.ErrFactForbidden, want: server.ErrForbidden},
		{name: "stale assertion", in: store.ErrFactConflict, want: server.ErrConflict},
		{name: "deleted", in: store.ErrFactDeleted, want: server.ErrFactDeleted},
		{name: "idempotency", in: store.ErrFactIdempotencyConflict, want: server.ErrIdempotencyConflict},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := mapFactError(tc.in); !errors.Is(got, tc.want) {
				t.Fatalf("mapFactError(%v) = %v, want errors.Is(_, %v)", tc.in, got, tc.want)
			}
		})
	}
}
