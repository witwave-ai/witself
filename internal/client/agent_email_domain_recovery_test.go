package client

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func TestAgentEmailDomainRecoveryClientRoutesAndHeaders(t *testing.T) {
	t.Helper()
	var calls []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.RequestURI())
		if got := r.Header.Get("Authorization"); got != "Bearer admin-token" {
			t.Fatalf("Authorization = %q", got)
		}
		if got := r.Header.Get(AgentEmailDomainRecoveryHeader); got != "recovery-token" {
			t.Fatalf("%s = %q", AgentEmailDomainRecoveryHeader, got)
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/admin/agent-email-domain-journal":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"schema_version": "witself.agent-email-domain-recovery.v1",
				"enabled":        true, "required": true, "pending": false,
				"forked": false, "healthy": true,
				"remote_head_checked": true, "remote_head_healthy": true,
				"capacity": map[string]any{
					"ready": true, "used": 5, "max": 10000,
					"remaining": 9995, "near_limit": false, "at_limit": false,
					"breakdown": map[string]any{
						"meta": 1, "audit": 1, "domain": 1, "idempotency": 1,
						"lifecycle_fence": 0, "lifecycle_intent": 0,
						"plan_fence": 0, "plan_intent": 0, "request": 1,
					},
				},
				"head": map[string]any{
					"stream_id": "aedj_aaaaaaaaaaaaaaaa", "sequence": 2,
					"hash": strings.Repeat("a", 64),
				},
			})
		case "/v1/admin/agent-email-domain-journal:bootstrap",
			"/v1/admin/agent-email-domain-journal:checkpoint":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"schema_version": "witself.agent-email-domain-recovery.v1",
				"kind":           "checkpoint", "phase": "complete", "complete": true,
				"frozen": false, "authority_keys": 1, "scanned_keys": 3,
			})
		case "/v1/admin/agent-email-domain-recoveries",
			"/v1/admin/agent-email-domain-recoveries/aedrec_aaaaaaaaaaaaaaaa",
			"/v1/admin/agent-email-domain-recoveries/aedrec_aaaaaaaaaaaaaaaa:advance",
			"/v1/admin/agent-email-domain-recoveries/aedrec_aaaaaaaaaaaaaaaa:verify":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"schema_version":   "witself.agent-email-domain-recovery.v1",
				"recovery_id":      "aedrec_aaaaaaaaaaaaaaaa",
				"source_stream_id": "aedj_aaaaaaaaaaaaaaaa",
				"phase":            "replay", "action_fence": "fence",
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	ctx := context.Background()
	journal, err := GetAdminAgentEmailDomainJournal(ctx, server.URL,
		"admin-token", "recovery-token")
	if err != nil {
		t.Fatal(err)
	}
	if !journal.Healthy || !journal.RemoteHeadChecked ||
		journal.RemoteHeadHealthy == nil || !*journal.RemoteHeadHealthy ||
		journal.DegradationCode != "" {
		t.Fatalf("journal health = %#v", journal)
	}
	if !journal.Capacity.Ready || journal.Capacity.Used == nil ||
		*journal.Capacity.Used != 5 || journal.Capacity.Max != 10000 ||
		journal.Capacity.Remaining == nil || *journal.Capacity.Remaining != 9995 ||
		journal.Capacity.NearLimit == nil || *journal.Capacity.NearLimit ||
		journal.Capacity.AtLimit == nil || *journal.Capacity.AtLimit ||
		journal.Capacity.Breakdown == nil ||
		journal.Capacity.Breakdown.Idempotency != 1 {
		t.Fatalf("journal capacity = %#v", journal.Capacity)
	}
	if _, err := BootstrapAdminAgentEmailDomainJournal(ctx, server.URL,
		"admin-token", "recovery-token", "bootstrap reason", "bootstrap-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := CheckpointAdminAgentEmailDomainJournal(ctx, server.URL,
		"admin-token", "recovery-token", "checkpoint reason", "checkpoint-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := StartAdminAgentEmailDomainRecovery(ctx, server.URL,
		"admin-token", "recovery-token", "aedrec_aaaaaaaaaaaaaaaa",
		"aedj_aaaaaaaaaaaaaaaa", 2, strings.Repeat("b", 64),
		"restore drill", "start-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := GetAdminAgentEmailDomainRecovery(ctx, server.URL,
		"admin-token", "recovery-token", "aedrec_aaaaaaaaaaaaaaaa"); err != nil {
		t.Fatal(err)
	}
	if _, err := AdvanceAdminAgentEmailDomainRecovery(ctx, server.URL,
		"admin-token", "recovery-token", "aedrec_aaaaaaaaaaaaaaaa",
		"advance-1", strings.Repeat("c", 64)); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyAdminAgentEmailDomainRecovery(ctx, server.URL,
		"admin-token", "recovery-token", "aedrec_aaaaaaaaaaaaaaaa",
		"verify-1", strings.Repeat("d", 64)); err != nil {
		t.Fatal(err)
	}

	want := []string{
		"GET /v1/admin/agent-email-domain-journal",
		"POST /v1/admin/agent-email-domain-journal:bootstrap",
		"POST /v1/admin/agent-email-domain-journal:checkpoint",
		"POST /v1/admin/agent-email-domain-recoveries",
		"GET /v1/admin/agent-email-domain-recoveries/aedrec_aaaaaaaaaaaaaaaa",
		"POST /v1/admin/agent-email-domain-recoveries/aedrec_aaaaaaaaaaaaaaaa:advance",
		"POST /v1/admin/agent-email-domain-recoveries/aedrec_aaaaaaaaaaaaaaaa:verify",
	}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("calls = %#v; want %#v", calls, want)
	}
}

func TestAgentEmailDomainRecoveryClientRequiresDistinctCredential(t *testing.T) {
	if _, err := GetAdminAgentEmailDomainJournal(context.Background(),
		"https://self.example", "admin-token", ""); err == nil {
		t.Fatal("missing recovery token did not fail locally")
	}
}
