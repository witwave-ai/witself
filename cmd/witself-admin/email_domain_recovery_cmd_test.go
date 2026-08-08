package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/witwave-ai/witself/internal/client"
)

func TestEmailDomainRecoveryCLIExactRoutesAndCredential(t *testing.T) {
	var calls []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.RequestURI())
		if got := r.Header.Get("Authorization"); got != "Bearer admin-token" {
			t.Fatalf("authorization = %q", got)
		}
		if got := r.Header.Get(client.AgentEmailDomainRecoveryHeader); got != "recovery-token" {
			t.Fatalf("recovery header = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/admin/agent-email-domain-journal":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"schema_version": "witself.agent-email-domain-recovery.v1",
				"enabled":        true, "required": false, "pending": false,
				"forked": false, "healthy": true,
				"remote_head_checked": true, "remote_head_healthy": true,
			})
		case "/v1/admin/agent-email-domain-journal:bootstrap",
			"/v1/admin/agent-email-domain-journal:checkpoint":
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if body["reason"] == nil || body["idempotency_key"] == nil {
				t.Fatalf("maintenance body = %#v", body)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"schema_version": "witself.agent-email-domain-recovery.v1",
				"kind":           "checkpoint", "phase": "complete", "complete": true,
			})
		case "/v1/admin/agent-email-domain-recoveries":
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if body["expected_head"] == nil || body["reason"] == nil ||
				body["idempotency_key"] == nil {
				t.Fatalf("recovery start body = %#v", body)
			}
			_ = json.NewEncoder(w).Encode(recoveryFixture())
		case "/v1/admin/agent-email-domain-recoveries/aedrec_aaaaaaaaaaaaaaaa":
			_ = json.NewEncoder(w).Encode(recoveryFixture())
		case "/v1/admin/agent-email-domain-recoveries/aedrec_aaaaaaaaaaaaaaaa:advance",
			"/v1/admin/agent-email-domain-recoveries/aedrec_aaaaaaaaaaaaaaaa:verify":
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if body["expected_action_fence"] == nil || body["idempotency_key"] == nil {
				t.Fatalf("recovery action body = %#v", body)
			}
			_ = json.NewEncoder(w).Encode(recoveryFixture())
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	dir := t.TempDir()
	recoveryTokenFile := filepath.Join(dir, "domain-recovery.token")
	if err := os.WriteFile(recoveryTokenFile, []byte("recovery-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	common := []string{
		"--endpoint", server.URL,
		"--token", "admin-token",
		"--recovery-token-file", recoveryTokenFile,
		"--json",
	}
	runCommand := func(args ...string) {
		t.Helper()
		if code := run(append(args, common...)); code != 0 {
			t.Fatalf("%v exit code = %d", args, code)
		}
	}
	runCommand("email-domain", "journal", "status")
	runCommand("email-domain", "journal", "bootstrap",
		"--reason", "bootstrap authority", "--idempotency-key", "bootstrap-1")
	runCommand("email-domain", "journal", "checkpoint",
		"--reason", "checkpoint authority", "--idempotency-key", "checkpoint-1")
	runCommand("email-domain", "recovery", "start",
		"--recovery", "aedrec_aaaaaaaaaaaaaaaa",
		"--source-stream", "aedj_bbbbbbbbbbbbbbbb",
		"--expected-sequence", "3", "--expected-hash", strings.Repeat("c", 64),
		"--reason", "sealed restore drill", "--idempotency-key", "start-1")
	runCommand("email-domain", "recovery", "status",
		"--recovery", "aedrec_aaaaaaaaaaaaaaaa")
	runCommand("email-domain", "recovery", "advance",
		"--recovery", "aedrec_aaaaaaaaaaaaaaaa",
		"--expected-action-fence", strings.Repeat("d", 64),
		"--idempotency-key", "advance-1")
	runCommand("email-domain", "recovery", "verify",
		"--recovery", "aedrec_aaaaaaaaaaaaaaaa",
		"--expected-action-fence", strings.Repeat("e", 64),
		"--idempotency-key", "verify-1")

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

func recoveryFixture() map[string]any {
	return map[string]any{
		"schema_version":   "witself.agent-email-domain-recovery.v1",
		"recovery_id":      "aedrec_aaaaaaaaaaaaaaaa",
		"source_stream_id": "aedj_bbbbbbbbbbbbbbbb",
		"phase":            "replay", "action_fence": strings.Repeat("f", 64),
	}
}

func TestEmailDomainRecoveryCLIRejectsUnsafeInputs(t *testing.T) {
	t.Setenv("WITSELF_HOME", t.TempDir())
	t.Setenv("WITSELF_AGENT_EMAIL_DOMAIN_RECOVERY_TOKEN_FILE", "")
	if code := emailDomainAdminJournal([]string{"bootstrap",
		"--reason", "missing key"}); code != 2 {
		t.Fatalf("missing idempotency key exit code = %d", code)
	}
	if code := emailDomainAdminRecovery([]string{"advance",
		"--recovery", "aedrec_aaaaaaaaaaaaaaaa"}); code != 2 {
		t.Fatalf("missing action fence exit code = %d", code)
	}
}

func TestAgentEmailDomainRecoveryTokenFilePermissions(t *testing.T) {
	dir := t.TempDir()
	ownerOnly := filepath.Join(dir, "owner-only.token")
	if err := os.WriteFile(ownerOnly, []byte("owner-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := readAgentEmailDomainRecoveryTokenFile(ownerOnly); err != nil ||
		got != "owner-secret" {
		t.Fatalf("owner-only token = %q, err = %v", got, err)
	}

	readable := filepath.Join(dir, "readable.token")
	if err := os.WriteFile(readable, []byte("shared-secret\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := readAgentEmailDomainRecoveryTokenFile(readable)
	if runtime.GOOS == "windows" {
		if err != nil || got != "shared-secret" {
			t.Fatalf("Windows token read = %q, err = %v", got, err)
		}
	} else if err == nil || !strings.Contains(err.Error(), "owner-only") {
		t.Fatalf("group/world-readable token error = %v", err)
	}
}
