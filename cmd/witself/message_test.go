package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/witwave-ai/witself/internal/client"
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

func TestMessageSendDefaultsToActionableRequest(t *testing.T) {
	var got struct {
		Kind string `json:"kind"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatal(err)
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"message":{"id":"msg_1","kind":"request","thread_id":"thr_1","from":{"kind":"agent","agent_id":"agent_1","agent_name":"scott"},"to":{"kind":"agent","agent_id":"agent_2","agent_name":"peer"},"delivery":{"state":"delivered"},"read_state":{"state":"unread"}}}`))
	}))
	defer srv.Close()
	tokenFile := filepath.Join(t.TempDir(), "scott.token")
	if err := os.WriteFile(tokenFile, []byte("witself_agt_scott\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code := run([]string{
		"message", "send", "--endpoint", srv.URL, "--token-file", tokenFile,
		"--to", "peer", "--body", "please do the work", "--json",
	}); code != 0 {
		t.Fatalf("run code = %d", code)
	}
	if got.Kind != "request" {
		t.Fatalf("default kind = %q, want request", got.Kind)
	}
}

func TestMessageSendSupportsExplicitAndRealmAudiences(t *testing.T) {
	for _, tt := range []struct {
		name       string
		args       []string
		wantKind   string
		wantIDs    []string
		responseTo string
	}{
		{
			name: "explicit agents", args: []string{"--to-agents", "bob,alice", "--to-agents", "charlie"},
			wantKind: "agents", wantIDs: []string{"bob", "alice", "charlie"},
			responseTo: `{"kind":"agents","count":3}`,
		},
		{
			name: "realm", args: []string{"--to-realm"}, wantKind: "realm",
			responseTo: `{"kind":"realm","count":4}`,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var got struct {
				To struct {
					Kind string   `json:"kind"`
					IDs  []string `json:"ids"`
				} `json:"to"`
			}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
					t.Fatal(err)
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusCreated)
				_, _ = fmt.Fprintf(w, `{"message":{"id":"msg_1","kind":"request","thread_id":"thr_1","from":{"kind":"agent","agent_id":"agent_scott","agent_name":"scott"},"to":%s,"delivery":{"state":"delivered"},"read_state":{"state":"unread"}}}`, tt.responseTo)
			}))
			defer srv.Close()

			tokenFile := filepath.Join(t.TempDir(), "scott.token")
			if err := os.WriteFile(tokenFile, []byte("witself_agt_scott\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			args := []string{"message", "send", "--endpoint", srv.URL, "--token-file", tokenFile, "--body", "hello", "--json"}
			args = append(args, tt.args...)
			if code := run(args); code != 0 {
				t.Fatalf("run code = %d", code)
			}
			if got.To.Kind != tt.wantKind || !reflect.DeepEqual(got.To.IDs, tt.wantIDs) {
				t.Fatalf("audience = %#v, want kind %q ids %#v", got.To, tt.wantKind, tt.wantIDs)
			}
		})
	}
}

func TestMessageSendRejectsMissingOrConflictingAudienceWithoutHTTP(t *testing.T) {
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	tokenFile := filepath.Join(t.TempDir(), "scott.token")
	if err := os.WriteFile(tokenFile, []byte("witself_agt_scott\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	base := []string{"message", "send", "--endpoint", srv.URL, "--token-file", tokenFile, "--body", "hello"}
	for _, audience := range [][]string{
		nil,
		{"--to", "bob", "--to-realm"},
		{"--to", "bob", "--to-agents", "alice"},
		{"--to-agents", "alice", "--to-realm"},
	} {
		if code := run(append(append([]string{}, base...), audience...)); code != 2 {
			t.Fatalf("audience %q returned %d, want 2", audience, code)
		}
	}
	if requests != 0 {
		t.Fatalf("invalid audiences made %d HTTP requests", requests)
	}
}

func TestMessageRecipientLabelsDescribeFanout(t *testing.T) {
	if got := messageRecipientLabel(client.MessageRecipient{Kind: "agents", Count: 3}); got != "agents(3)" {
		t.Fatalf("summary label = %q", got)
	}
	if got := messageRecipientDetailLabel(client.MessageRecipient{Kind: "realm", Count: 4}); got != "realm (4 recipients)" {
		t.Fatalf("detail label = %q", got)
	}
}

func TestMessageReplyUsesParentAndContentOnly(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/messages/msg_parent:reply" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") != "Bearer witself_agt_bob" {
			t.Errorf("authorization = %q", r.Header.Get("Authorization"))
		}
		if r.Header.Get("Idempotency-Key") != "reply-42" {
			t.Errorf("idempotency key = %q", r.Header.Get("Idempotency-Key"))
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatal(err)
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"message":{"id":"msg_reply","kind":"reply","thread_id":"thr_1","reply_to_message_id":"msg_parent","from":{"kind":"agent","agent_id":"agent_bob","agent_name":"bob"},"to":{"kind":"agent","agent_id":"agent_scott","agent_name":"scott"},"delivery":{"state":"delivered"},"read_state":{"state":"unread"}}}`))
	}))
	defer srv.Close()

	tokenFile := filepath.Join(t.TempDir(), "bob.token")
	if err := os.WriteFile(tokenFile, []byte("witself_agt_bob\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	code := run([]string{
		"message", "reply", "msg_parent", "--endpoint", srv.URL, "--token-file", tokenFile,
		"--subject", "Re: task", "--kind", "reply", "--body", "done",
		"--idempotency-key", "reply-42", "--json",
	})
	if code != 0 {
		t.Fatalf("run code = %d", code)
	}
	if got["subject"] != "Re: task" || got["kind"] != "reply" || got["body"] != "done" {
		t.Fatalf("request = %#v", got)
	}
	for _, forbidden := range []string{"to", "thread_id", "reply_to_message_id", "from", "account_id", "realm_id"} {
		if _, ok := got[forbidden]; ok {
			t.Fatalf("request unexpectedly contains %q: %#v", forbidden, got)
		}
	}
}

func TestMessageListenUsesWaitableMetadataContract(t *testing.T) {
	type listenRequest struct {
		WaitSeconds int    `json:"wait_seconds"`
		FromAgent   string `json:"from_agent"`
		ThreadID    string `json:"thread_id"`
		Kind        string `json:"kind"`
		Limit       int    `json:"limit"`
	}
	var got []listenRequest
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Method != http.MethodPost || r.URL.Path != "/v1/messages:listen" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") != "Bearer witself_agt_scott" {
			t.Errorf("authorization = %q", r.Header.Get("Authorization"))
		}
		var body listenRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		got = append(got, body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"messages":[],"timed_out":true}`))
	}))
	defer srv.Close()

	tokenFile := filepath.Join(t.TempDir(), "scott.token")
	if err := os.WriteFile(tokenFile, []byte("witself_agt_scott\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	code := run([]string{
		"message", "listen", "--endpoint", srv.URL, "--token-file", tokenFile,
		"--timeout", "0", "--from", "bob", "--conversation", "thr_1",
		"--kind", "request", "--limit", "7", "--json",
	})
	if code != 0 {
		t.Fatalf("run code = %d", code)
	}
	if requests != 1 || len(got) != 1 || got[0].WaitSeconds != 0 || got[0].FromAgent != "bob" || got[0].ThreadID != "thr_1" || got[0].Kind != "request" || got[0].Limit != 7 {
		t.Fatalf("requests = %d, request = %+v", requests, got)
	}

	if code := run([]string{
		"message", "listen", "--endpoint", srv.URL, "--token-file", tokenFile,
		"--timeout", "20", "--json",
	}); code != 0 {
		t.Fatalf("maximum timeout run code = %d", code)
	}
	if requests != 2 || len(got) != 2 || got[1].WaitSeconds != 20 {
		t.Fatalf("maximum timeout requests = %d, request = %+v", requests, got)
	}

	if code := run([]string{
		"message", "listen", "--endpoint", srv.URL, "--token-file", tokenFile,
		"--timeout", "21",
	}); code != 2 {
		t.Fatalf("invalid timeout run code = %d, want 2", code)
	}
	if requests != 2 {
		t.Fatalf("invalid timeout made request; requests = %d", requests)
	}
	if code := run([]string{
		"message", "listen", "--endpoint", srv.URL, "--token-file", tokenFile,
		"--thread", "thr_1", "--conversation", "thr_2",
	}); code != 2 {
		t.Fatalf("disagreeing conversation run code = %d, want 2", code)
	}
	if requests != 2 {
		t.Fatalf("disagreeing conversation made request; requests = %d", requests)
	}
	if strings.TrimSpace(got[0].ThreadID) != "thr_1" {
		t.Fatalf("conversation alias was not normalized: %+v", got)
	}
}

func TestMessageProcessingCommandsUseFencedActionContracts(t *testing.T) {
	type requestRecord struct {
		Action         string
		IdempotencyKey string
		Body           map[string]any
	}
	var got []requestRecord
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || !strings.HasPrefix(r.URL.Path, "/v1/messages/msg_work:") {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") != "Bearer witself_agt_worker" {
			t.Errorf("authorization = %q", r.Header.Get("Authorization"))
		}
		action := strings.TrimPrefix(r.URL.Path, "/v1/messages/msg_work:")
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		got = append(got, requestRecord{Action: action, IdempotencyKey: r.Header.Get("Idempotency-Key"), Body: body})
		w.Header().Set("Content-Type", "application/json")
		if action == "complete" {
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"schema_version":1,"processing":{"state":"completed","claim_id":"mcl_1","generation":3,"result_message_id":"msg_result"},"message":{"id":"msg_result","kind":"result","thread_id":"thr_1","reply_to_message_id":"msg_work","from":{"kind":"agent","agent_id":"agent_worker","agent_name":"worker"},"to":{"kind":"agent","agent_id":"agent_sender","agent_name":"sender"},"delivery":{"state":"delivered"},"read_state":{"state":"unread"}}}`))
			return
		}
		state := "claimed"
		if action == "release" {
			state = "available"
		}
		_, _ = fmt.Fprintf(w, `{"schema_version":1,"processing":{"state":%q,"claim_id":"mcl_1","generation":3}}`, state)
	}))
	defer srv.Close()

	dir := t.TempDir()
	tokenFile := filepath.Join(dir, "worker.token")
	payloadFile := filepath.Join(dir, "result.json")
	if err := os.WriteFile(tokenFile, []byte("witself_agt_worker\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(payloadFile, []byte(`{"ok":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	connection := []string{"--endpoint", srv.URL, "--token-file", tokenFile, "--json"}
	tests := [][]string{
		append([]string{"message", "claim", "msg_work", "--lease", "90s", "--idempotency-key", "claim-key"}, connection...),
		append([]string{"message", "renew", "msg_work", "--claim", "mcl_1", "--generation", "3", "--lease", "2m"}, connection...),
		append([]string{"message", "release", "msg_work", "--claim", "mcl_1", "--generation", "3"}, connection...),
		append([]string{"message", "complete", "msg_work", "--claim", "mcl_1", "--generation", "3", "--subject", "Result", "--kind", "result", "--body", "done", "--payload-file", payloadFile, "--idempotency-key", "complete-key"}, connection...),
	}
	for _, args := range tests {
		if code := run(args); code != 0 {
			t.Fatalf("run(%q) code = %d", args, code)
		}
	}
	if len(got) != 4 {
		t.Fatalf("requests = %#v", got)
	}
	if got[0].Action != "claim" || got[0].IdempotencyKey != "claim-key" || got[0].Body["lease_seconds"] != float64(90) {
		t.Fatalf("claim request = %#v", got[0])
	}
	if got[1].Action != "renew" || got[1].Body["claim_id"] != "mcl_1" || got[1].Body["generation"] != float64(3) || got[1].Body["lease_seconds"] != float64(120) {
		t.Fatalf("renew request = %#v", got[1])
	}
	if got[2].Action != "release" || got[2].Body["claim_id"] != "mcl_1" || got[2].Body["generation"] != float64(3) {
		t.Fatalf("release request = %#v", got[2])
	}
	if got[3].Action != "complete" || got[3].IdempotencyKey != "complete-key" || got[3].Body["claim_id"] != "mcl_1" ||
		got[3].Body["generation"] != float64(3) || got[3].Body["subject"] != "Result" || got[3].Body["kind"] != "result" ||
		got[3].Body["body"] != "done" {
		t.Fatalf("complete request = %#v", got[3])
	}
	payload, ok := got[3].Body["payload"].(map[string]any)
	if !ok || payload["ok"] != true {
		t.Fatalf("complete payload = %#v", got[3].Body["payload"])
	}
	for _, forbidden := range []string{"to", "thread_id", "reply_to_message_id", "from", "sender", "actor", "account_id", "realm_id"} {
		if _, ok := got[3].Body[forbidden]; ok {
			t.Fatalf("complete request unexpectedly contains %q: %#v", forbidden, got[3])
		}
	}
}

func TestMessageProcessingCommandsRejectInvalidCoordinatesWithoutHTTP(t *testing.T) {
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	tokenFile := filepath.Join(t.TempDir(), "worker.token")
	if err := os.WriteFile(tokenFile, []byte("witself_agt_worker\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	connection := []string{"--endpoint", srv.URL, "--token-file", tokenFile}
	tests := [][]string{
		append([]string{"message", "claim", "msg_work"}, connection...),
		append([]string{"message", "claim", "msg_work", "--lease", "29s", "--idempotency-key", "claim-key"}, connection...),
		append([]string{"message", "claim", "msg_work", "--lease", "30.5s", "--idempotency-key", "claim-key"}, connection...),
		append([]string{"message", "claim", "msg_work", "--lease", "16m", "--idempotency-key", "claim-key"}, connection...),
		append([]string{"message", "renew", "msg_work", "--claim", "mcl_1", "--generation", "0"}, connection...),
		append([]string{"message", "release", "msg_work", "--claim", "", "--generation", "3"}, connection...),
		append([]string{"message", "complete", "msg_work", "--claim", "mcl_1", "--generation", "3", "--body", "done"}, connection...),
	}
	for _, args := range tests {
		if code := run(args); code != 2 {
			t.Errorf("run(%q) code = %d, want 2", args, code)
		}
	}
	if requests != 0 {
		t.Fatalf("invalid processing commands made %d HTTP requests", requests)
	}
}

func TestMessageActionsRejectStrayPositionalArgumentsWithoutHTTP(t *testing.T) {
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"message":{"id":"msg_1","kind":"reply","thread_id":"thr_1","from":{"kind":"agent"},"to":{"kind":"agent"},"delivery":{"state":"delivered"},"read_state":{"state":"unread"}}}`))
	}))
	defer srv.Close()

	tokenFile := filepath.Join(t.TempDir(), "agent.token")
	if err := os.WriteFile(tokenFile, []byte("witself_agt_agent\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name string
		args []string
	}{
		{
			name: "reply",
			args: []string{"message", "reply", "msg_parent", "--endpoint", srv.URL, "--token-file", tokenFile, "--body", "done", "stray"},
		},
		{
			name: "read",
			args: []string{"message", "read", "msg_1", "--endpoint", srv.URL, "--token-file", tokenFile, "stray"},
		},
		{
			name: "ack",
			args: []string{"message", "ack", "msg_1", "--endpoint", srv.URL, "--token-file", tokenFile, "stray"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if code := run(tt.args); code != 2 {
				t.Fatalf("run code = %d, want 2", code)
			}
		})
	}
	if requests != 0 {
		t.Fatalf("invalid action arguments made %d HTTP requests", requests)
	}
}
