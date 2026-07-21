package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/witwave-ai/witself/internal/agentemailcode"
	"github.com/witwave-ai/witself/internal/client"
)

func TestEmailCLIAddressListAndRead(t *testing.T) {
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.Method+" "+r.URL.RequestURI())
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/email/address":
			_, _ = w.Write([]byte(`{"address":{"id":"eaddr_aaaaaaaaaaaaaaaa","address":"owner.realm@example.com","receive_state":"enabled"}}`))
		case "/v1/email":
			_, _ = w.Write([]byte(`{"messages":[{"id":"emsg_aaaaaaaaaaaaaaaa","header_from":"sender@example.com","subject":"code","sender_verification_state":"unverified","read_state":{"state":"unread"},"processing":{"state":"available"}}]}`))
		case "/v1/email/emsg_aaaaaaaaaaaaaaaa:read":
			_, _ = w.Write([]byte(`{"message":{"id":"emsg_aaaaaaaaaaaaaaaa","parse_state":"parsed","header_from":"sender@example.com","subject":"code","text":"123456","sender_verification_state":"unverified","read_state":{"state":"read"},"processing":{"state":"available"}}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	tokenFile := filepath.Join(t.TempDir(), "agent.token")
	if err := os.WriteFile(tokenFile, []byte("witself_agt_test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	base := []string{"--endpoint", srv.URL, "--token-file", tokenFile, "--json"}
	for _, args := range [][]string{
		append([]string{"email", "address", "show"}, base...),
		append([]string{"email", "list", "--unread", "--limit", "5"}, base...),
		append([]string{"email", "read", "emsg_aaaaaaaaaaaaaaaa"}, base...),
		append([]string{"email", "code-candidates", "emsg_aaaaaaaaaaaaaaaa"}, base...),
	} {
		if code := run(args); code != 0 {
			t.Fatalf("run(%v) = %d", args, code)
		}
	}
	if len(paths) != 4 || paths[0] != "GET /v1/email/address" ||
		paths[1] != "GET /v1/email?limit=5&unread=true" ||
		paths[2] != "POST /v1/email/emsg_aaaaaaaaaaaaaaaa:read" ||
		paths[3] != "POST /v1/email/emsg_aaaaaaaaaaaaaaaa:read" {
		t.Fatalf("paths = %#v", paths)
	}
}

func TestEmailCLIOperatorReceiveControls(t *testing.T) {
	var requests []string
	var states []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.Method+" "+r.URL.Path)
		if r.Method == http.MethodPatch {
			var body map[string]string
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			states = append(states, body["receive_state"])
		}
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "/agents/") {
			_, _ = w.Write([]byte(`{"control":{"agent_id":"agent_aaaaaaaaaaaaaaaa","receive_state":"disabled","agent_receive_state":"disabled","realm_receive_state":"enabled","row_version":2}}`))
			return
		}
		_, _ = w.Write([]byte(`{"control":{"realm_id":"realm_aaaaaaaaaaaaaaaa","receive_state":"enabled","mailbox_count":5,"row_version":3}}`))
	}))
	defer srv.Close()
	tokenFile := filepath.Join(t.TempDir(), "operator.token")
	if err := os.WriteFile(tokenFile, []byte("witself_opr_test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	base := []string{"--endpoint", srv.URL, "--token-file", tokenFile, "--json"}
	commands := [][]string{
		append([]string{"email", "operator", "receive", "show", "--agent-id", "agent_aaaaaaaaaaaaaaaa"}, base...),
		append([]string{"email", "operator", "receive", "disable", "--agent-id", "agent_aaaaaaaaaaaaaaaa"}, base...),
		append([]string{"email", "operator", "receive", "show", "--realm-id", "realm_aaaaaaaaaaaaaaaa"}, base...),
		append([]string{"email", "operator", "receive", "enable", "--realm-id", "realm_aaaaaaaaaaaaaaaa"}, base...),
	}
	for _, command := range commands {
		if code := run(command); code != 0 {
			t.Fatalf("run(%v) = %d", command, code)
		}
	}
	wantRequests := []string{
		"GET /v1/agents/agent_aaaaaaaaaaaaaaaa/email-receive",
		"PATCH /v1/agents/agent_aaaaaaaaaaaaaaaa/email-receive",
		"GET /v1/realms/realm_aaaaaaaaaaaaaaaa/email-receive",
		"PATCH /v1/realms/realm_aaaaaaaaaaaaaaaa/email-receive",
	}
	if !reflect.DeepEqual(requests, wantRequests) || !reflect.DeepEqual(states, []string{"disabled", "enabled"}) {
		t.Fatalf("requests = %#v states = %#v", requests, states)
	}
	if code := run(append([]string{
		"email", "operator", "receive", "disable",
		"--agent-id", "agent_aaaaaaaaaaaaaaaa", "--realm-id", "realm_aaaaaaaaaaaaaaaa",
	}, base...)); code != 2 {
		t.Fatalf("ambiguous target code = %d, want 2", code)
	}
}

func TestBuildAgentEmailCodeCandidatesResult(t *testing.T) {
	tests := []struct {
		name       string
		subject    string
		text       string
		state      string
		candidates []agentEmailCodeCandidateProjection
	}{
		{name: "none", text: "Order 123456 shipped on 2026-07-21.", state: "none", candidates: []agentEmailCodeCandidateProjection{}},
		{name: "single", text: "Your verification code is 123456.", state: "single", candidates: []agentEmailCodeCandidateProjection{{Value: "123456", Occurrences: 1}}},
		{name: "subject body boundary", subject: "Your verification code", text: "123456", state: "single", candidates: []agentEmailCodeCandidateProjection{{Value: "123456", Occurrences: 1}}},
		{name: "repeated", text: "OTP: 4821. Use code 4821.", state: "single", candidates: []agentEmailCodeCandidateProjection{{Value: "4821", Occurrences: 2}}},
		{name: "ambiguous", text: "Verification code: 111111. Backup code: 222222.", state: "ambiguous", candidates: []agentEmailCodeCandidateProjection{{Value: "111111", Occurrences: 1}, {Value: "222222", Occurrences: 1}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result, err := buildAgentEmailCodeCandidatesResult("emsg_aaaaaaaaaaaaaaaa", client.AgentEmailMessage{
				ID: "emsg_aaaaaaaaaaaaaaaa", HeaderFrom: "sender@example.com",
				ParseState: "parsed", Subject: test.subject, Text: test.text,
			})
			if err != nil {
				t.Fatal(err)
			}
			if result.SelectionState != test.state || !reflect.DeepEqual(result.Candidates, test.candidates) {
				t.Fatalf("result = %#v, want state %q candidates %#v", result, test.state, test.candidates)
			}
			if result.SenderVerificationState != "unverified" || result.ContentTrust != "untrusted" ||
				result.ScanScope != "subject_and_bounded_text" || result.ContentTruncated || result.CandidateOverflow ||
				result.CodeConsumptionPerformed || !strings.Contains(result.Warning, "code-consumed was not called") {
				t.Fatalf("safety labels = %#v", result)
			}
		})
	}
}

func TestBuildAgentEmailCodeCandidatesFailsClosedOrReportsIncompleteScan(t *testing.T) {
	if _, err := buildAgentEmailCodeCandidatesResult("emsg_aaaaaaaaaaaaaaaa", client.AgentEmailMessage{
		ID: "emsg_aaaaaaaaaaaaaaaa", ParseState: "error", Text: "Verification code: 123456",
	}); err == nil {
		t.Fatal("parse failure produced a candidate result")
	}

	result, err := buildAgentEmailCodeCandidatesResult("emsg_aaaaaaaaaaaaaaaa", client.AgentEmailMessage{
		ID: "emsg_aaaaaaaaaaaaaaaa", ParseState: "parsed", Subject: "Your code",
		Text: "123456\n" + strings.Repeat("x", maxMCPAgentEmailTextBytes),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.ContentTruncated || result.SelectionState != "ambiguous" || len(result.Candidates) != 1 ||
		result.Candidates[0].Value != "123456" {
		t.Fatalf("truncated result = %#v", result)
	}

	var overflowText strings.Builder
	for value := 1000; value <= 1000+agentemailcode.MaximumCandidates; value++ {
		_, _ = fmt.Fprintf(&overflowText, "Code: %d\n", value)
	}
	overflow, err := buildAgentEmailCodeCandidatesResult("emsg_aaaaaaaaaaaaaaaa", client.AgentEmailMessage{
		ID: "emsg_aaaaaaaaaaaaaaaa", ParseState: "parsed", Text: overflowText.String(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !overflow.CandidateOverflow || overflow.SelectionState != "ambiguous" ||
		len(overflow.Candidates) != agentemailcode.MaximumCandidates {
		t.Fatalf("overflow result = %#v", overflow)
	}
}

func TestPrintAgentEmailCodeCandidatesIsSafeAndExplicit(t *testing.T) {
	result, err := buildAgentEmailCodeCandidatesResult("emsg_aaaaaaaaaaaaaaaa", client.AgentEmailMessage{
		ID: "emsg_aaaaaaaaaaaaaaaa", HeaderFrom: "sender@example.com\x1b\nspoof",
		ParseState: "parsed", Subject: "verification\tcode", Text: "Verification code: 111111. Backup code: 222222.",
	})
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	printAgentEmailCodeCandidates(&output, result)
	got := output.String()
	for _, want := range []string{
		"from: sender@example.com spoof (unverified)",
		"content: untrusted",
		"selection: ambiguous",
		"- 111111 (1 occurrence)",
		"- 222222 (1 occurrence)",
		"no candidate was selected or used; code-consumed was not called",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("plain output omitted %q: %q", want, got)
		}
	}
	if strings.Contains(got, "\x1b") || strings.Contains(got, "\nspoof") {
		t.Fatalf("plain output retained terminal or line injection: %q", got)
	}
}

func TestEmailCLIClaimSendsFenceInputs(t *testing.T) {
	var body map[string]any
	var key string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key = r.Header.Get("Idempotency-Key")
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write([]byte(`{"processing":{"state":"claimed","claim_id":"ecl_aaaaaaaaaaaaaaaa","generation":1,"failure_count":0}}`))
	}))
	defer srv.Close()
	tokenFile := filepath.Join(t.TempDir(), "agent.token")
	if err := os.WriteFile(tokenFile, []byte("witself_agt_test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code := run([]string{
		"email", "claim", "emsg_aaaaaaaaaaaaaaaa", "--endpoint", srv.URL,
		"--token-file", tokenFile, "--lease", "2m", "--idempotency-key", "claim-1", "--json",
	}); code != 0 {
		t.Fatalf("run = %d", code)
	}
	if key != "claim-1" || body["lease_seconds"] != float64(120) {
		t.Fatalf("request = key %q body %#v", key, body)
	}
}
