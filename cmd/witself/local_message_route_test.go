package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/witwave-ai/witself/internal/client"
	"github.com/witwave-ai/witself/internal/local"
)

// These principals and policy-free replies are synthetic. This fixture proves
// managed route/command composition; the API workflow tests prove authorization.
const (
	localMessageRouteAccountID = "acc_local_message"
	localMessageRouteRealm     = "message-realm"
	localMessageRouteAgent     = "message-agent"
	localMessageRouteAgentID   = "agent_local_message"
	localMessageRouteBearer    = "synthetic-local-message-bearer"
	localMessageRoutePeerID    = "agent_local_peer"
	localMessageRouteMessageID = "msg_local_message"
	localMessageRouteRequestID = "mrq_local_message"
	localMessageRouteClaimID   = "mrc_local_message"
	localMessageRouteKey       = "local-message-operation"
	localMessageRouteBody      = "synthetic local message content"
)

type localMessageRouteTransport struct {
	base      *http.Transport
	allowed   map[string]bool
	forbidden atomic.Int32
}

func (transport *localMessageRouteTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if !transport.allowed[request.URL.Scheme+"://"+request.URL.Host] {
		transport.forbidden.Add(1)
		return nil, errors.New("fixture refuses outbound destination")
	}
	return transport.base.RoundTrip(request)
}

func localMessageRouteBoundary(t *testing.T, origins ...string) (*atomic.Int32, *localMessageRouteTransport) {
	t.Helper()
	directory := &atomic.Int32{}
	previousLocate := connectionLocate
	connectionLocate = func(context.Context, string, string) (string, string, error) {
		directory.Add(1)
		return "", origins[len(origins)-1], errors.New("fixture directory unavailable")
	}
	previousTransport := http.DefaultTransport
	transport := &localMessageRouteTransport{base: &http.Transport{Proxy: nil}, allowed: map[string]bool{}}
	for _, origin := range origins {
		transport.allowed[origin] = true
	}
	http.DefaultTransport = transport
	t.Cleanup(func() {
		connectionLocate = previousLocate
		http.DefaultTransport = previousTransport
		transport.base.CloseIdleConnections()
	})
	return directory, transport
}

func localMessageRouteHome(t *testing.T, accountName, origin string) {
	t.Helper()
	for _, name := range []string{"HOME", "USERPROFILE", "WITSELF_HOME", "DSH_HOME"} {
		t.Setenv(name, t.TempDir())
	}
	for _, name := range []string{"WITSELF_ACCOUNT", "WITSELF_AGENT", "WITSELF_REALM", "WITSELF_ENDPOINT", "WITSELF_TOKEN_FILE"} {
		t.Setenv(name, "")
	}
	if err := local.Save(accountName, local.Account{ID: localMessageRouteAccountID, Route: &local.Route{Kind: "local", Endpoint: origin}}, "synthetic-unused-owner"); err != nil {
		t.Fatal("save synthetic local account failed")
	}
	path, err := local.AgentTokenPath(accountName, localMessageRouteRealm, localMessageRouteAgent)
	if err != nil {
		t.Fatal("resolve synthetic agent binding failed")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal("create synthetic agent directory failed")
	}
	if err := os.WriteFile(path, []byte(localMessageRouteBearer+"\n"), 0o600); err != nil {
		t.Fatal("save synthetic agent binding failed")
	}
}

// Capture both command streams without writing command content to test logs.
func localMessageRouteRun(t *testing.T, args []string) (int, []byte, []byte) {
	t.Helper()
	stdoutReader, stdoutWriter, err := os.Pipe()
	if err != nil {
		t.Fatal("create stdout capture failed")
	}
	stderrReader, stderrWriter, err := os.Pipe()
	if err != nil {
		_ = stdoutReader.Close()
		_ = stdoutWriter.Close()
		t.Fatal("create stderr capture failed")
	}
	previousOut, previousErr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = stdoutWriter, stderrWriter
	defer func() {
		os.Stdout, os.Stderr = previousOut, previousErr
		_ = stdoutWriter.Close()
		_ = stderrWriter.Close()
		_ = stdoutReader.Close()
		_ = stderrReader.Close()
	}()
	type captured struct {
		data []byte
		err  error
	}
	stdoutResult, stderrResult := make(chan captured, 1), make(chan captured, 1)
	go func() { data, readErr := io.ReadAll(stdoutReader); stdoutResult <- captured{data, readErr} }()
	go func() { data, readErr := io.ReadAll(stderrReader); stderrResult <- captured{data, readErr} }()
	code := run(args)
	_ = stdoutWriter.Close()
	_ = stderrWriter.Close()
	stdout, stderr := <-stdoutResult, <-stderrResult
	if stdout.err != nil || stderr.err != nil {
		t.Fatal("read command capture failed")
	}
	return code, stdout.data, stderr.data
}

type localMessageRouteExchange struct {
	origin, method, path, query, bearer, contentType, key string
	body                                                  map[string]any
}

type localMessageRouteFixture struct {
	mu        sync.Mutex
	exchanges []localMessageRouteExchange
	refuse    bool
}

func (fixture *localMessageRouteFixture) serveHTTP(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	if r.Body != nil {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil && !errors.Is(err, io.EOF) {
			http.Error(w, "invalid fixture request", http.StatusBadRequest)
			return
		}
	}
	fixture.mu.Lock()
	fixture.exchanges = append(fixture.exchanges, localMessageRouteExchange{
		origin: "http://" + r.Host, method: r.Method, path: r.URL.Path, query: r.URL.RawQuery,
		bearer: r.Header.Get("Authorization"), contentType: r.Header.Get("Content-Type"), key: r.Header.Get("Idempotency-Key"), body: body,
	})
	refuse := fixture.refuse
	fixture.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	if refuse {
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"code": "unavailable", "message": "fixture refuses request"}})
		return
	}
	principal := client.MessageAgent{Kind: "agent", AgentID: localMessageRouteAgentID, AgentName: localMessageRouteAgent}
	message := client.Message{ID: localMessageRouteMessageID, AccountID: localMessageRouteAccountID, RealmID: localMessageRouteRealm, From: principal, Body: localMessageRouteBody}
	request := client.MessageRequest{ID: localMessageRouteRequestID, AccountID: localMessageRouteAccountID, RealmID: localMessageRouteRealm, Coordinator: principal, OpeningMessageID: message.ID}
	claim := client.MessageRequestClaim{ClaimID: localMessageRouteClaimID, RequestID: request.ID, Agent: principal, Generation: 7}
	var response any
	switch r.URL.Path {
	case "/v1/messages", "/v1/messages/" + localMessageRouteMessageID + ":read":
		response = map[string]any{"message": message}
	case "/v1/messages:listen":
		response = client.MessageListenResult{Messages: []client.Message{{ID: message.ID, AccountID: message.AccountID, RealmID: message.RealmID, From: principal}}}
	case "/v1/message-requests":
		response = client.CreateMessageRequestResult{Request: request, OpeningMessage: message}
	case "/v1/message-requests/" + localMessageRouteRequestID + ":offer":
		response = client.OfferMessageRequestResult{Request: request, Offer: client.MessageRequestOffer{Agent: principal, Message: message}}
	case "/v1/message-requests/" + localMessageRouteRequestID + ":select":
		response = client.SelectMessageRequestResult{Request: request, Selection: client.MessageRequestSelection{ID: "mrs_local_message", SelectedAgentIDs: []string{localMessageRoutePeerID}}, Claims: []client.MessageRequestClaim{claim}}
	case "/v1/message-requests/" + localMessageRouteRequestID + ":claim":
		response = map[string]any{"claim": claim}
	case "/v1/message-requests/" + localMessageRouteRequestID + ":complete":
		response = client.CompleteMessageRequestResult{Request: request, Claim: claim, Message: message}
	default:
		http.NotFound(w, r)
		return
	}
	_ = json.NewEncoder(w).Encode(response)
}

func (fixture *localMessageRouteFixture) snapshot() []localMessageRouteExchange {
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	return append([]localMessageRouteExchange(nil), fixture.exchanges...)
}

type localMessageRouteStep struct {
	name                                 string
	command                              []string
	path, key                            string
	body                                 map[string]any
	outputField, outputIDField, outputID string
}

func localMessageRouteSteps() []localMessageRouteStep {
	return []localMessageRouteStep{
		{name: "send", command: []string{"message", "send", "--to", localMessageRoutePeerID, "--body", localMessageRouteBody, "--idempotency-key", localMessageRouteKey}, path: "/v1/messages", key: localMessageRouteKey, body: map[string]any{"to": map[string]any{"kind": "agent", "id": localMessageRoutePeerID}, "kind": "request", "body": localMessageRouteBody}, outputField: "message", outputIDField: "id", outputID: localMessageRouteMessageID},
		{name: "listen", command: []string{"message", "listen", "--timeout", "0", "--limit", "2"}, path: "/v1/messages:listen", body: map[string]any{"wait_seconds": float64(0), "limit": float64(2)}, outputField: "messages", outputIDField: "id", outputID: localMessageRouteMessageID},
		{name: "read", command: []string{"message", "read", localMessageRouteMessageID}, path: "/v1/messages/" + localMessageRouteMessageID + ":read", body: map[string]any{}, outputField: "message", outputIDField: "id", outputID: localMessageRouteMessageID},
		{name: "open", command: []string{"message", "request", "open", "--body", localMessageRouteBody, "--offer-window", "2s", "--expires-in", "2m", "--idempotency-key", localMessageRouteKey}, path: "/v1/message-requests", key: localMessageRouteKey, body: map[string]any{"body": localMessageRouteBody, "selection_policy": "client_ranked", "max_assignees": float64(1), "offer_window_seconds": float64(2), "expires_in_seconds": float64(120)}, outputField: "request", outputIDField: "id", outputID: localMessageRouteRequestID},
		{name: "offer", command: []string{"message", "request", "offer", localMessageRouteRequestID, "--body", localMessageRouteBody, "--idempotency-key", localMessageRouteKey}, path: "/v1/message-requests/" + localMessageRouteRequestID + ":offer", key: localMessageRouteKey, body: map[string]any{"body": localMessageRouteBody}, outputField: "request", outputIDField: "id", outputID: localMessageRouteRequestID},
		{name: "select", command: []string{"message", "request", "select", localMessageRouteRequestID, "--selected-agent", localMessageRoutePeerID, "--reservation", "60s", "--idempotency-key", localMessageRouteKey}, path: "/v1/message-requests/" + localMessageRouteRequestID + ":select", key: localMessageRouteKey, body: map[string]any{"selected_agent_ids": []any{localMessageRoutePeerID}, "reservation_seconds": float64(60)}, outputField: "request", outputIDField: "id", outputID: localMessageRouteRequestID},
		{name: "claim", command: []string{"message", "request", "claim", localMessageRouteRequestID, "--lease", "60s", "--idempotency-key", localMessageRouteKey}, path: "/v1/message-requests/" + localMessageRouteRequestID + ":claim", key: localMessageRouteKey, body: map[string]any{"lease_seconds": float64(60)}, outputField: "claim", outputIDField: "claim_id", outputID: localMessageRouteClaimID},
		{name: "complete", command: []string{"message", "request", "complete", localMessageRouteRequestID, "--claim", localMessageRouteClaimID, "--generation", "7", "--body", localMessageRouteBody, "--idempotency-key", localMessageRouteKey}, path: "/v1/message-requests/" + localMessageRouteRequestID + ":complete", key: localMessageRouteKey, body: map[string]any{"claim_id": localMessageRouteClaimID, "generation": float64(7), "body": localMessageRouteBody}, outputField: "message", outputIDField: "id", outputID: localMessageRouteMessageID},
	}
}

func localMessageRouteArgs(step localMessageRouteStep, explicitAccount string) []string {
	args := append([]string(nil), step.command...)
	args = append(args, "--realm", localMessageRouteRealm, "--agent", localMessageRouteAgent, "--json")
	if explicitAccount != "" {
		args = append(args, "--account", explicitAccount)
	}
	return args
}

func TestLocalMessageRouteOffline(t *testing.T) {
	for _, selection := range []struct{ name, account, explicit string }{
		{"named", "message-local", "message-local"}, {"implicit-default", "default", ""},
	} {
		t.Run(selection.name, func(t *testing.T) {
			fixture := &localMessageRouteFixture{}
			server := httptest.NewServer(http.HandlerFunc(fixture.serveHTTP))
			t.Cleanup(server.Close)
			directory, transport := localMessageRouteBoundary(t, server.URL)
			localMessageRouteHome(t, selection.account, server.URL)
			for round := 0; round < 2; round++ {
				if round == 1 {
					// Reopen via the configuration API; no cached connection is reused.
					config, err := local.Load()
					if err != nil || config.Accounts[selection.account].Route == nil {
						t.Fatal("reload saved route failed")
					}
					if err := local.SetRoute(selection.account, config.Accounts[selection.account].Route); err != nil {
						t.Fatal("reopen saved route failed")
					}
				}
				for _, step := range localMessageRouteSteps() {
					t.Run(step.name, func(t *testing.T) {
						before := len(fixture.snapshot())
						code, stdout, _ := localMessageRouteRun(t, localMessageRouteArgs(step, selection.explicit))
						if code != 0 || directory.Load() != 0 {
							t.Fatal("local command must succeed with zero directory calls")
						}
						exchanges := fixture.snapshot()
						if len(exchanges) != before+1 {
							t.Fatal("local command must make exactly one request")
						}
						exchange := exchanges[before]
						if exchange.origin != server.URL || exchange.method != http.MethodPost || exchange.path != step.path || exchange.query != "" {
							t.Fatal("local command request route mismatch")
						}
						if exchange.bearer != "Bearer "+localMessageRouteBearer || exchange.contentType != "application/json" || exchange.key != step.key {
							t.Fatal("local command authentication or headers mismatch")
						}
						if !reflect.DeepEqual(exchange.body, step.body) {
							t.Fatal("local command request body mismatch")
						}
						var output map[string]any
						if json.Unmarshal(stdout, &output) != nil {
							t.Fatal("local command output is not JSON")
						}
						selected := output[step.outputField]
						if step.outputField == "messages" {
							messages, ok := selected.([]any)
							if !ok || len(messages) != 1 {
								t.Fatal("listen output identity missing")
							}
							selected = messages[0]
						}
						object, ok := selected.(map[string]any)
						if !ok || object[step.outputIDField] != step.outputID {
							t.Fatal("local command response identity mismatch")
						}
						principalField := "from"
						if step.outputField == "request" {
							principalField = "coordinator"
						}
						if step.outputField == "claim" {
							principalField = "agent"
							if object["request_id"] != localMessageRouteRequestID {
								t.Fatal("local claim request linkage mismatch")
							}
						} else if object["account_id"] != localMessageRouteAccountID || object["realm_id"] != localMessageRouteRealm {
							t.Fatal("local response account or realm mismatch")
						}
						principal, ok := object[principalField].(map[string]any)
						if !ok || principal["agent_id"] != localMessageRouteAgentID || principal["agent_name"] != localMessageRouteAgent {
							t.Fatal("local response bearer principal mismatch")
						}
						if transport.forbidden.Load() != 0 {
							t.Fatal("local command attempted forbidden outbound connection")
						}
					})
				}
			}
		})
	}
}

func TestLocalMessageRouteUnavailable(t *testing.T) {
	for _, step := range localMessageRouteSteps() {
		t.Run(step.name, func(t *testing.T) {
			fixture := &localMessageRouteFixture{refuse: true}
			server := httptest.NewServer(http.HandlerFunc(fixture.serveHTTP))
			t.Cleanup(server.Close)
			var sentinelCalls atomic.Int32
			sentinel := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				sentinelCalls.Add(1)
				w.WriteHeader(http.StatusNoContent)
			}))
			t.Cleanup(sentinel.Close)
			directory, transport := localMessageRouteBoundary(t, server.URL, sentinel.URL)
			localMessageRouteHome(t, "default", server.URL)
			// The refusing locator carries the sentinel as an unusable alternative.
			// An endpoint refusal must not consult it or repeat the operation.
			code, stdout, stderr := localMessageRouteRun(t, localMessageRouteArgs(step, ""))
			if code != 1 || len(stdout) != 0 || len(stderr) == 0 {
				t.Fatal("local endpoint refusal must fail the command")
			}
			if len(fixture.snapshot()) != 1 {
				t.Fatal("local endpoint refusal must not replay the command")
			}
			if directory.Load() != 0 || sentinelCalls.Load() != 0 || transport.forbidden.Load() != 0 {
				t.Fatal("local endpoint refusal must not fall back")
			}
		})
	}
}
