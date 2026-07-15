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
	"time"
)

func TestMessageRequestCommandsUseCanonicalContracts(t *testing.T) {
	type requestRecord struct {
		Method         string
		URI            string
		IdempotencyKey string
		Body           map[string]any
	}
	var got []requestRecord
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer witself_agt_agent" {
			t.Errorf("authorization = %q", r.Header.Get("Authorization"))
		}
		record := requestRecord{Method: r.Method, URI: r.URL.RequestURI(), IdempotencyKey: r.Header.Get("Idempotency-Key")}
		if r.Method == http.MethodPost {
			if err := json.NewDecoder(r.Body).Decode(&record.Body); err != nil {
				t.Fatalf("decode %s: %v", r.URL.Path, err)
			}
		}
		got = append(got, record)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/message-requests":
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"request":{"id":"mrq_1","state":"open","phase":"collecting_offers","max_assignees":2},"opening_message":{"id":"msg_open","kind":"open_request"}}`))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/message-requests":
			_, _ = w.Write([]byte(`{"requests":[{"id":"mrq_1","state":"open","phase":"awaiting_selection","offer_count":2}],"next_cursor":"next"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/message-requests/mrq_1":
			_, _ = w.Write([]byte(`{"request":{"id":"mrq_1","state":"open","phase":"awaiting_selection"},"opening_message":{"id":"msg_open","body":"do the work"},"candidates":[],"offers":[],"selections":[],"claims":[]}`))
		case r.URL.Path == "/v1/message-requests/mrq_1:offer":
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"request":{"id":"mrq_1","state":"open","phase":"awaiting_selection"},"offer":{"agent":{"agent_id":"agent_bob"},"message":{"id":"msg_offer","kind":"offer"}}}`))
		case r.URL.Path == "/v1/message-requests/mrq_1:decline":
			_, _ = w.Write([]byte(`{"request":{"id":"mrq_1","state":"open","phase":"awaiting_selection"}}`))
		case r.URL.Path == "/v1/message-requests/mrq_1:select":
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"request":{"id":"mrq_1","state":"open","phase":"assigned"},"selection":{"id":"msel_1","generation":2,"selected_agent_ids":["agent_bob","agent_charlie"]},"claims":[{"claim_id":"mrc_1","request_id":"mrq_1","state":"reserved","generation":1}]}`))
		case r.URL.Path == "/v1/message-requests/mrq_1:cancel":
			_, _ = w.Write([]byte(`{"request":{"id":"mrq_1","state":"cancelled"}}`))
		case r.URL.Path == "/v1/message-requests/mrq_1:claim":
			_, _ = w.Write([]byte(`{"claim":{"claim_id":"mrc_1","request_id":"mrq_1","state":"claimed","generation":3}}`))
		case r.URL.Path == "/v1/message-requests/mrq_1:renew":
			_, _ = w.Write([]byte(`{"claim":{"claim_id":"mrc_1","request_id":"mrq_1","state":"claimed","generation":3}}`))
		case r.URL.Path == "/v1/message-requests/mrq_1:release":
			_, _ = w.Write([]byte(`{"claim":{"claim_id":"mrc_1","request_id":"mrq_1","state":"released","generation":3,"failure_count":1}}`))
		case r.URL.Path == "/v1/message-requests/mrq_1:complete":
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"request":{"id":"mrq_1","state":"completed"},"claim":{"claim_id":"mrc_1","request_id":"mrq_1","state":"completed","generation":3},"message":{"id":"msg_result","kind":"result"}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	dir := t.TempDir()
	tokenFile := filepath.Join(dir, "agent.token")
	payloadFile := filepath.Join(dir, "payload.json")
	if err := os.WriteFile(tokenFile, []byte("witself_agt_agent\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(payloadFile, []byte(`{"priority":"high"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	connection := []string{"--endpoint", srv.URL, "--token-file", tokenFile, "--json"}
	withConnection := func(args ...string) []string {
		return append(append([]string{}, args...), connection...)
	}
	tests := [][]string{
		withConnection("message", "request", "open", "--subject", "Investigate", "--body", "do the work", "--payload-file", payloadFile, "--max-assignees", "2", "--offer-window", "45s", "--expires-in", "2h", "--idempotency-key", "open-key"),
		withConnection("message", "request", "list", "--state", "open", "--phase", "awaiting_selection", "--role", "coordinator", "--limit", "7", "--cursor", "cursor"),
		append([]string{"message", "request", "show"}, append(connection, "mrq_1")...),
		withConnection("message", "request", "offer", "mrq_1", "--subject", "Proposal", "--body", "I can do it", "--payload-file", payloadFile, "--idempotency-key", "offer-key"),
		withConnection("message", "request", "decline", "mrq_1", "--idempotency-key", "decline-key"),
		withConnection("message", "request", "select", "mrq_1", "--selected-agent", "agent_bob,agent_charlie", "--reservation", "90s", "--idempotency-key", "select-key"),
		withConnection("message", "request", "cancel", "mrq_1"),
		withConnection("message", "request", "claim", "mrq_1", "--lease", "2m", "--idempotency-key", "claim-key"),
		withConnection("message", "request", "renew", "mrq_1", "--claim", "mrc_1", "--generation", "3", "--lease", "3m"),
		withConnection("message", "request", "release", "mrq_1", "--claim", "mrc_1", "--generation", "3", "--deterministic-failure"),
		withConnection("message", "request", "complete", "mrq_1", "--claim", "mrc_1", "--generation", "3", "--subject", "Result", "--body", "done", "--payload-file", payloadFile, "--idempotency-key", "complete-key"),
	}
	for _, args := range tests {
		if code := run(args); code != 0 {
			t.Fatalf("run(%q) code = %d", args, code)
		}
	}

	wantURIs := []string{
		"POST /v1/message-requests",
		"GET /v1/message-requests?cursor=cursor&limit=7&phase=awaiting_selection&role=coordinator&state=open",
		"GET /v1/message-requests/mrq_1",
		"POST /v1/message-requests/mrq_1:offer",
		"POST /v1/message-requests/mrq_1:decline",
		"POST /v1/message-requests/mrq_1:select",
		"POST /v1/message-requests/mrq_1:cancel",
		"POST /v1/message-requests/mrq_1:claim",
		"POST /v1/message-requests/mrq_1:renew",
		"POST /v1/message-requests/mrq_1:release",
		"POST /v1/message-requests/mrq_1:complete",
	}
	actualURIs := make([]string, len(got))
	for i := range got {
		actualURIs[i] = got[i].Method + " " + got[i].URI
	}
	if !reflect.DeepEqual(actualURIs, wantURIs) {
		t.Fatalf("requests = %#v, want %#v", actualURIs, wantURIs)
	}

	assertNumber := func(t *testing.T, body map[string]any, key string, want float64) {
		t.Helper()
		if body[key] != want {
			t.Errorf("%s = %#v, want %v in %#v", key, body[key], want, body)
		}
	}
	if got[0].IdempotencyKey != "open-key" || got[0].Body["subject"] != "Investigate" || got[0].Body["body"] != "do the work" || got[0].Body["selection_policy"] != "client_ranked" {
		t.Errorf("open request = %#v", got[0])
	}
	assertNumber(t, got[0].Body, "max_assignees", 2)
	assertNumber(t, got[0].Body, "offer_window_seconds", 45)
	assertNumber(t, got[0].Body, "expires_in_seconds", 7200)
	if payload, ok := got[0].Body["payload"].(map[string]any); !ok || payload["priority"] != "high" {
		t.Errorf("open payload = %#v", got[0].Body["payload"])
	}
	if got[3].IdempotencyKey != "offer-key" || got[3].Body["subject"] != "Proposal" || got[3].Body["body"] != "I can do it" {
		t.Errorf("offer request = %#v", got[3])
	}
	if got[4].IdempotencyKey != "decline-key" || len(got[4].Body) != 0 {
		t.Errorf("decline request = %#v", got[4])
	}
	if got[5].IdempotencyKey != "select-key" || !reflect.DeepEqual(got[5].Body["selected_agent_ids"], []any{"agent_bob", "agent_charlie"}) {
		t.Errorf("select request = %#v", got[5])
	}
	assertNumber(t, got[5].Body, "reservation_seconds", 90)
	if len(got[6].Body) != 0 {
		t.Errorf("cancel request = %#v", got[6])
	}
	if got[7].IdempotencyKey != "claim-key" {
		t.Errorf("claim idempotency key = %q", got[7].IdempotencyKey)
	}
	assertNumber(t, got[7].Body, "lease_seconds", 120)
	if got[8].Body["claim_id"] != "mrc_1" {
		t.Errorf("renew request = %#v", got[8])
	}
	assertNumber(t, got[8].Body, "generation", 3)
	assertNumber(t, got[8].Body, "lease_seconds", 180)
	if got[9].Body["deterministic_failure"] != true {
		t.Errorf("release request = %#v", got[9])
	}
	if got[10].IdempotencyKey != "complete-key" || got[10].Body["claim_id"] != "mrc_1" || got[10].Body["subject"] != "Result" || got[10].Body["body"] != "done" {
		t.Errorf("complete request = %#v", got[10])
	}
	assertNumber(t, got[10].Body, "generation", 3)
	for _, index := range []int{0, 3, 5, 7, 8, 9, 10} {
		for _, forbidden := range []string{"to", "from", "sender", "actor", "account_id", "realm_id", "coordinator_agent_id", "thread_id", "reply_to_message_id"} {
			if _, exists := got[index].Body[forbidden]; exists {
				t.Errorf("request %d unexpectedly contains %q: %#v", index, forbidden, got[index])
			}
		}
	}
}

func TestMessageRequestCommandsRejectInvalidInputWithoutHTTP(t *testing.T) {
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	tokenFile := filepath.Join(t.TempDir(), "agent.token")
	if err := os.WriteFile(tokenFile, []byte("witself_agt_agent\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	connection := []string{"--endpoint", srv.URL, "--token-file", tokenFile}
	withConnection := func(args ...string) []string {
		return append(append([]string{}, args...), connection...)
	}
	tests := []struct {
		name string
		args []string
	}{
		{"missing request subcommand", []string{"message", "request"}},
		{"unknown request subcommand", []string{"message", "request", "unknown"}},
		{"open missing key", withConnection("message", "request", "open", "--body", "work")},
		{"open empty body", withConnection("message", "request", "open", "--idempotency-key", "key")},
		{"open invalid assignees", withConnection("message", "request", "open", "--body", "work", "--max-assignees", "9", "--idempotency-key", "key")},
		{"open fractional offer window", withConnection("message", "request", "open", "--body", "work", "--offer-window", "1.5s", "--idempotency-key", "key")},
		{"open expiry before offer window", withConnection("message", "request", "open", "--body", "work", "--offer-window", "1m", "--expires-in", "1m", "--idempotency-key", "key")},
		{"list invalid limit", withConnection("message", "request", "list", "--limit", "0")},
		{"show missing id", withConnection("message", "request", "show")},
		{"show invalid id", withConnection("message", "request", "show", "msg_1")},
		{"show stray argument", withConnection("message", "request", "show", "mrq_1", "stray")},
		{"offer missing key", withConnection("message", "request", "offer", "mrq_1", "--body", "proposal")},
		{"offer empty body", withConnection("message", "request", "offer", "mrq_1", "--idempotency-key", "key")},
		{"select missing agents", withConnection("message", "request", "select", "mrq_1", "--idempotency-key", "key")},
		{"select duplicate agent", withConnection("message", "request", "select", "mrq_1", "--selected-agent", "agent_bob,agent_bob", "--idempotency-key", "key")},
		{"select invalid agent", withConnection("message", "request", "select", "mrq_1", "--selected-agent", "bob", "--idempotency-key", "key")},
		{"select fractional reservation", withConnection("message", "request", "select", "mrq_1", "--selected-agent", "agent_bob", "--reservation", "30.5s", "--idempotency-key", "key")},
		{"claim missing key", withConnection("message", "request", "claim", "mrq_1")},
		{"claim short lease", withConnection("message", "request", "claim", "mrq_1", "--lease", "29s", "--idempotency-key", "key")},
		{"claim fractional lease", withConnection("message", "request", "claim", "mrq_1", "--lease", "30.5s", "--idempotency-key", "key")},
		{"renew missing claim", withConnection("message", "request", "renew", "mrq_1", "--generation", "1")},
		{"renew invalid generation", withConnection("message", "request", "renew", "mrq_1", "--claim", "mrc_1", "--generation", "0")},
		{"release invalid claim", withConnection("message", "request", "release", "mrq_1", "--claim", "claim_1", "--generation", "1")},
		{"cancel stray argument", withConnection("message", "request", "cancel", "mrq_1", "stray")},
		{"complete missing key", withConnection("message", "request", "complete", "mrq_1", "--claim", "mrc_1", "--generation", "1", "--body", "done")},
		{"complete empty body", withConnection("message", "request", "complete", "mrq_1", "--claim", "mrc_1", "--generation", "1", "--idempotency-key", "key")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if code := run(tt.args); code != 2 {
				t.Fatalf("run(%q) code = %d, want 2", tt.args, code)
			}
		})
	}
	if requests != 0 {
		t.Fatalf("invalid request commands made %d HTTP requests", requests)
	}
}

func TestMessageRequestDurationValidationBoundaries(t *testing.T) {
	for _, tt := range []struct {
		name string
		got  bool
		want bool
	}{
		{"offer minimum", validMessageRequestOfferWindow(time.Second), true},
		{"offer maximum", validMessageRequestOfferWindow(15 * time.Minute), true},
		{"offer too long", validMessageRequestOfferWindow(15*time.Minute + time.Second), false},
		{"expiry after window", validMessageRequestExpiry(31*time.Second, 30*time.Second), true},
		{"expiry equal window", validMessageRequestExpiry(30*time.Second, 30*time.Second), false},
		{"lease minimum", validMessageRequestLease(30 * time.Second), true},
		{"lease maximum", validMessageRequestLease(15 * time.Minute), true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if tt.got != tt.want {
				t.Fatalf("got %t, want %t", tt.got, tt.want)
			}
		})
	}
}

func TestMessageRequestSelectCSVAgentFlag(t *testing.T) {
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Selected []string `json:"selected_agent_ids"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		got = body.Selected
		_, _ = fmt.Fprintln(w, `{"request":{"id":"mrq_1"},"selection":{"id":"msel_1"},"claims":[]}`)
	}))
	defer srv.Close()
	tokenFile := filepath.Join(t.TempDir(), "agent.token")
	if err := os.WriteFile(tokenFile, []byte("witself_agt_agent\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	args := []string{
		"message", "request", "select", "mrq_1", "--selected-agent", "agent_a, agent_b", "--selected-agent", "agent_c",
		"--idempotency-key", "select-key", "--endpoint", srv.URL, "--token-file", tokenFile, "--json",
	}
	if code := run(args); code != 0 {
		t.Fatalf("run code = %d", code)
	}
	want := []string{"agent_a", "agent_b", "agent_c"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("selected agents = %s, want %s", strings.Join(got, ","), strings.Join(want, ","))
	}
}
