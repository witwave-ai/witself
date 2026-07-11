package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestMessageSendUsesAgentTokenAndCanonicalRequest(t *testing.T) {
	var got struct {
		To struct {
			Kind string `json:"kind"`
			ID   string `json:"id"`
		} `json:"to"`
		Body    string         `json:"body"`
		Payload map[string]any `json:"payload"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/messages" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") != "Bearer witself_agt_scott" {
			t.Errorf("authorization = %q", r.Header.Get("Authorization"))
		}
		if r.Header.Get("Idempotency-Key") != "retry-42" {
			t.Errorf("idempotency key = %q", r.Header.Get("Idempotency-Key"))
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatal(err)
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"message":{"id":"msg_1","kind":"handoff","thread_id":"thr_1","from":{"kind":"agent","agent_id":"agent_1","agent_name":"scott"},"to":{"kind":"agent","agent_id":"agent_2","agent_name":"peer"},"delivery":{"state":"delivered"},"read_state":{"state":"unread"}}}`))
	}))
	defer srv.Close()

	dir := t.TempDir()
	tokenFile := filepath.Join(dir, "scott.token")
	payloadFile := filepath.Join(dir, "payload.json")
	if err := os.WriteFile(tokenFile, []byte("witself_agt_scott\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(payloadFile, []byte(`{"task":42}`), 0o600); err != nil {
		t.Fatal(err)
	}
	code := run([]string{
		"message", "send", "--endpoint", srv.URL, "--token-file", tokenFile,
		"--to", "peer", "--kind", "handoff", "--body", "your turn",
		"--payload-file", payloadFile, "--idempotency-key", "retry-42", "--json",
	})
	if code != 0 {
		t.Fatalf("run code = %d", code)
	}
	if got.To.Kind != "agent" || got.To.ID != "peer" || got.Body != "your turn" || got.Payload["task"] != float64(42) {
		t.Fatalf("request = %+v", got)
	}
}
