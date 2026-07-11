package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestMessageHTTPContract(t *testing.T) {
	principal := DomainPrincipal{
		Kind: PrincipalKindAgent, ID: "agent_sender", AccountID: "acc_1",
		RealmID: "realm_1", AgentName: "sender", AccountStatus: "active",
	}
	auth := func(context.Context, string) (DomainPrincipal, bool, error) {
		return principal, true, nil
	}
	var sends, reads, acks int
	var sent SendMessageRequest
	cfg := Config{
		AuthenticatePrincipal: auth,
		SendMessage: func(_ context.Context, got DomainPrincipal, in SendMessageRequest) (Message, error) {
			sends++
			if got.ID != principal.ID || got.RealmID != principal.RealmID {
				t.Fatalf("principal = %+v", got)
			}
			sent = in
			return testMessage(true), nil
		},
		ListMessages: func(_ context.Context, _ DomainPrincipal, opts MessageListOptions) (MessagePage, error) {
			if opts.Direction != "inbox" || !opts.Unread || opts.Limit != 7 || opts.From != "peer" {
				t.Fatalf("list options = %+v", opts)
			}
			return MessagePage{Messages: []Message{testMessage(true)}, NextCursor: "cursor-2"}, nil
		},
		ReadMessage: func(_ context.Context, _ DomainPrincipal, messageID string) (Message, error) {
			reads++
			if messageID != "msg_1" {
				t.Fatalf("read id = %q", messageID)
			}
			return testMessage(true), nil
		},
		AckMessage: func(_ context.Context, _ DomainPrincipal, _ string) (Message, error) {
			acks++
			msg := testMessage(true)
			msg.ReadState.State = "acked"
			return msg, nil
		},
	}
	srv := httptest.NewServer(apiMux(cfg))
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/messages", strings.NewReader(`{"to":{"kind":"agent","id":"peer"},"kind":"handoff","body":"hello","payload":{"task":42}}`))
	req.Header.Set("Authorization", "Bearer token")
	req.Header.Set("Idempotency-Key", "retry-1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("send status = %d", resp.StatusCode)
	}
	_ = resp.Body.Close()
	if sends != 1 || sent.To.ID != "peer" || sent.IdempotencyKey != "retry-1" || string(sent.Payload) != `{"task":42}` {
		t.Fatalf("send = %d / %+v", sends, sent)
	}

	for _, actorField := range []string{"from", "sender", "actor", "realm", "realm_id"} {
		body := `{"to":{"kind":"agent","id":"peer"},"body":"hello","` + actorField + `":"spoof"}`
		req, _ = http.NewRequest(http.MethodPost, srv.URL+"/v1/messages", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer token")
		resp, err = http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("%s spoof status = %d", actorField, resp.StatusCode)
		}
		_ = resp.Body.Close()
	}
	if sends != 1 {
		t.Fatalf("spoof requests reached send hook: %d", sends)
	}
	req, _ = http.NewRequest(http.MethodPost, srv.URL+"/v1/messages", strings.NewReader(`{"to":{"kind":"agent","id":"peer","realm":"remote"},"body":"hello"}`))
	req.Header.Set("Authorization", "Bearer token")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("cross-realm recipient status = %d", resp.StatusCode)
	}
	_ = resp.Body.Close()
	if sends != 1 {
		t.Fatalf("cross-realm request reached send hook: %d", sends)
	}

	req, _ = http.NewRequest(http.MethodGet, srv.URL+"/v1/messages?direction=inbox&unread=true&from=peer&limit=7", nil)
	req.Header.Set("Authorization", "Bearer token")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var page struct {
		Messages   []Message `json:"messages"`
		NextCursor string    `json:"next_cursor"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&page); err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if len(page.Messages) != 1 || page.Messages[0].Body != "" || len(page.Messages[0].Payload) != 0 || page.NextCursor != "cursor-2" {
		t.Fatalf("metadata-only list = %+v", page)
	}

	for _, action := range []string{"read", "ack"} {
		req, _ = http.NewRequest(http.MethodPost, srv.URL+"/v1/messages/msg_1:"+action, strings.NewReader(`{}`))
		req.Header.Set("Authorization", "Bearer token")
		resp, err = http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s status = %d", action, resp.StatusCode)
		}
		_ = resp.Body.Close()
	}
	if reads != 1 || acks != 1 {
		t.Fatalf("read/ack calls = %d/%d", reads, acks)
	}

	resp, err = http.Get(srv.URL + "/v1/capabilities")
	if err != nil {
		t.Fatal(err)
	}
	var caps capabilities
	if err := json.NewDecoder(resp.Body).Decode(&caps); err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if f := caps.Features["messaging"]; !f.Supported || f.Reason != "" {
		t.Fatalf("messaging capability = %+v", f)
	}
}

func testMessage(withContent bool) Message {
	msg := Message{
		ID: "msg_1", AccountID: "acc_1", RealmID: "realm_1",
		From: MessageAgent{Kind: "agent", AgentID: "agent_sender", AgentName: "sender"},
		To:   MessageRecipient{Kind: "agent", AgentID: "agent_peer", AgentName: "peer"},
		Kind: "handoff", ThreadID: "thr_1",
		Delivery:  MessageDelivery{State: "delivered"},
		ReadState: MessageReadState{State: "unread"},
	}
	if withContent {
		msg.Body = "secret body"
		msg.Payload = json.RawMessage(`{"task":42}`)
	}
	return msg
}
