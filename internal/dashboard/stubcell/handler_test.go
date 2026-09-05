package stubcell_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/dashboard/stubcell"
)

func TestDashboardStubCellHandlers(t *testing.T) {
	server := httptest.NewServer(stubcell.New(stubcell.Config{BearerToken: "fixture-token"}))
	t.Cleanup(server.Close)
	client := &http.Client{Timeout: 5 * time.Second}
	// These are the same IDs and metadata asserted by the pre-existing proxy
	// tests; each response is independently checked through the real handler.
	for _, tc := range []struct {
		path string
		want []string
	}{
		{"/v1/self", []string{`"agent_name":"acceptance"`, `"facts":2`, `"plan_entitlements"`, `"enforced_plan_id":"standard"`, `"mem_1"`}},
		{"/v1/self/dashboard-preferences", []string{`"preferences":null`}},
		{"/v1/self/avatar", []string{`"avatar":`, `"is_active":true`, `"svg_sha256":`, `Acceptance fixture avatar`}},
		{"/v1/transcripts", []string{`"transcripts":[`, `"id":"tr_1"`}},
		{"/v1/transcripts/tr_1", []string{`"id":"tr_1"`}},
		{"/v1/facts?observational=true", []string{`"id":"fact_1"`, `"Scott"`, `"sensitive":true`}},
		{"/v1/memories", []string{`"items":[`, `"id":"mem_1"`}},
		{"/v1/memories/mem_1", []string{`"memory":`, `"id":"mem_1"`}},
		{"/v1/memories/mem_1/history", []string{`"versions":`}},
		{"/v1/messages?direction=inbox", []string{`"id":"msg_1"`, `"subject":"greetings"`}},
		{"/v1/messages?direction=outbox", []string{`"id":"msg_1"`, `"subject":"greetings"`}},
		{"/v1/email/address", []string{`"address":"dash@agents.example"`}},
		{"/v1/email:status", []string{`"maximum_raw_bytes":26214400`, `"used":4096`, `"remaining":4096`, `"max":8192`}},
		{"/v1/email", []string{`"subject":"safe subject"`, `"envelope_sender":"sender@example.net"`, `"attachment_count":2`}},
		{"/v1/email/sent", []string{`"subject":"safe sent subject"`, `"state":"delivered"`, `"to":"person@example.net"`}},
		{"/v1/secrets", []string{`"id":"sec_1"`, `"name":"prod-db"`, `"name":"password"`, `"sensitive_field_count":1`}},
	} {
		t.Run(tc.path, func(t *testing.T) {
			req, err := http.NewRequest(http.MethodGet, server.URL+tc.path, nil)
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Authorization", "Bearer fixture-token")
			response, err := client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = response.Body.Close() }()
			body, err := io.ReadAll(response.Body)
			if err != nil {
				t.Fatal(err)
			}
			if response.StatusCode != http.StatusOK || !json.Valid(body) {
				t.Fatalf("invalid response: %d %s", response.StatusCode, body)
			}
			for _, want := range tc.want {
				if !bytes.Contains(body, []byte(want)) {
					t.Errorf("missing %q: %s", want, body)
				}
			}
			if bytes.Contains(body, []byte(stubcell.SecretCanary)) {
				t.Fatal("standalone cell API exposed secret canary")
			}
			if tc.path == "/v1/secrets" {
				for _, forbidden := range []string{"leaked", "plaintext", "ciphertext", "public_value", "sealed", "private_value"} {
					if bytes.Contains(body, []byte(forbidden)) {
						t.Errorf("standalone secret metadata contains %q", forbidden)
					}
				}
			}
		})
	}
	private, err := json.Marshal(stubcell.LeakySecret())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(private, []byte(stubcell.SecretCanary)) {
		t.Fatal("private fixture is missing its redaction canary")
	}
}

func TestDashboardStubCellRejectsUnauthorizedAndMutatingRequests(t *testing.T) {
	handler := stubcell.New(stubcell.Config{BearerToken: "fixture-token"})
	for _, tc := range []struct {
		method, path, token string
		status              int
	}{
		{http.MethodGet, "/v1/self", "", http.StatusUnauthorized},
		{http.MethodGet, "/v1/self", "wrong", http.StatusUnauthorized},
		{http.MethodPost, "/v1/messages", "fixture-token", http.StatusMethodNotAllowed},
		{http.MethodGet, "/v1/secrets/sec_1/fields/fld_1:access", "fixture-token", http.StatusNotFound},
		{http.MethodGet, "/v1/facts?observational=invalid-probe", "fixture-token", http.StatusBadRequest},
	} {
		request := httptest.NewRequest(tc.method, tc.path, nil)
		request.Header.Set("Authorization", "Bearer "+tc.token)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != tc.status {
			t.Errorf("%s %s = %d, want %d", tc.method, tc.path, response.Code, tc.status)
		}
		if strings.Contains(response.Body.String(), stubcell.SecretCanary) {
			t.Fatal("error exposed secret canary")
		}
	}
}
