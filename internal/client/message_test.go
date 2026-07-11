package client

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestMessageClientContracts(t *testing.T) {
	var sawSend, sawList, sawRead, sawAck bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer agent-token" {
			t.Errorf("authorization = %q", r.Header.Get("Authorization"))
		}
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/messages":
			sawSend = true
			if r.Header.Get("Idempotency-Key") != "retry-1" {
				t.Errorf("idempotency key = %q", r.Header.Get("Idempotency-Key"))
			}
			var body struct {
				To struct {
					Kind string `json:"kind"`
					ID   string `json:"id"`
				} `json:"to"`
				Payload map[string]any `json:"payload"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if body.To.Kind != "agent" || body.To.ID != "peer" || body.Payload["task"] != float64(42) {
				t.Fatalf("send body = %+v", body)
			}
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"message":{"id":"msg_1","kind":"request","thread_id":"thr_1","body":"hello","payload":{"task":42},"from":{"kind":"agent","agent_id":"agent_1","agent_name":"scott"},"to":{"kind":"agent","agent_id":"agent_2","agent_name":"peer"},"delivery":{"state":"delivered"},"read_state":{"state":"unread"}}}`))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/messages":
			sawList = true
			q := r.URL.Query()
			if q.Get("direction") != "inbox" || q.Get("unread") != "true" || q.Get("from") != "peer" || q.Get("limit") != "9" || q.Get("cursor") != "next" {
				t.Fatalf("list query = %s", r.URL.RawQuery)
			}
			_, _ = w.Write([]byte(`{"messages":[{"id":"msg_1","kind":"request","thread_id":"thr_1","from":{"kind":"agent","agent_id":"agent_2","agent_name":"peer"},"to":{"kind":"agent","agent_id":"agent_1","agent_name":"scott"},"delivery":{"state":"delivered"},"read_state":{"state":"unread"}}],"next_cursor":"cursor-2"}`))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/messages/msg_1:read":
			sawRead = true
			_, _ = w.Write([]byte(`{"message":{"id":"msg_1","kind":"request","thread_id":"thr_1","body":"hello","delivery":{"state":"delivered"},"read_state":{"state":"read"},"from":{"kind":"agent"},"to":{"kind":"agent"}}}`))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/messages/msg_1:ack":
			sawAck = true
			_, _ = w.Write([]byte(`{"message":{"id":"msg_1","kind":"request","thread_id":"thr_1","body":"hello","delivery":{"state":"delivered"},"read_state":{"state":"acked"},"from":{"kind":"agent"},"to":{"kind":"agent"}}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	ctx := context.Background()
	msg, err := SendMessage(ctx, srv.URL, "agent-token", SendMessageInput{
		To: "peer", Kind: "request", Body: "hello", Payload: json.RawMessage(`{"task":42}`), IdempotencyKey: "retry-1",
	})
	if err != nil || msg.ID != "msg_1" || string(msg.Payload) != `{"task":42}` {
		t.Fatalf("send = %+v / %v", msg, err)
	}
	page, err := ListMessages(ctx, srv.URL, "agent-token", MessageListOptions{Direction: "inbox", Unread: true, From: "peer", Limit: 9, Cursor: "next"})
	if err != nil || len(page.Messages) != 1 || page.NextCursor != "cursor-2" {
		t.Fatalf("list = %+v / %v", page, err)
	}
	read, err := ReadMessage(ctx, srv.URL, "agent-token", "msg_1")
	if err != nil || read.ReadState.State != "read" {
		t.Fatalf("read = %+v / %v", read, err)
	}
	acked, err := AckMessage(ctx, srv.URL, "agent-token", "msg_1")
	if err != nil || acked.ReadState.State != "acked" {
		t.Fatalf("ack = %+v / %v", acked, err)
	}
	if !sawSend || !sawList || !sawRead || !sawAck {
		t.Fatalf("requests = send:%v list:%v read:%v ack:%v", sawSend, sawList, sawRead, sawAck)
	}
}
