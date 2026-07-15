package client

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"
)

func TestMessageClientContracts(t *testing.T) {
	var sawSend, sawReply, sawList, sawListen, sawRead, sawAck bool
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
			_, _ = w.Write([]byte(`{"message":{"id":"msg_1","kind":"request","thread_id":"thr_1","causal_depth":1,"body":"hello","payload":{"task":42},"from":{"kind":"agent","agent_id":"agent_1","agent_name":"scott"},"to":{"kind":"agent","agent_id":"agent_2","agent_name":"peer"},"delivery":{"state":"delivered"},"read_state":{"state":"unread"}}}`))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/messages/msg_parent:reply":
			sawReply = true
			if r.Header.Get("Idempotency-Key") != "reply-retry-1" {
				t.Errorf("reply idempotency key = %q", r.Header.Get("Idempotency-Key"))
			}
			var body map[string]json.RawMessage
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			for _, forbidden := range []string{
				"to", "thread_id", "reply_to_message_id", "from", "sender", "actor",
				"account", "account_id", "realm", "realm_id",
			} {
				if _, ok := body[forbidden]; ok {
					t.Errorf("reply body included derived field %q: %s", forbidden, body[forbidden])
				}
			}
			var subject, kind, messageBody string
			if err := json.Unmarshal(body["subject"], &subject); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(body["kind"], &kind); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(body["body"], &messageBody); err != nil {
				t.Fatal(err)
			}
			var payload map[string]any
			if err := json.Unmarshal(body["payload"], &payload); err != nil {
				t.Fatal(err)
			}
			if subject != "answer" || kind != "reply" || messageBody != "done" || payload["ok"] != true {
				t.Fatalf("reply body = %s", body)
			}
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"message":{"id":"msg_reply","kind":"reply","thread_id":"thr_1","reply_to_message_id":"msg_parent","causal_depth":2,"body":"done","payload":{"ok":true},"from":{"kind":"agent","agent_id":"agent_1","agent_name":"scott"},"to":{"kind":"agent","agent_id":"agent_2","agent_name":"peer"},"delivery":{"state":"delivered"},"read_state":{"state":"unread"}}}`))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/messages":
			sawList = true
			q := r.URL.Query()
			if q.Get("direction") != "inbox" || q.Get("unread") != "true" || q.Get("from") != "peer" || q.Get("limit") != "9" || q.Get("cursor") != "next" {
				t.Fatalf("list query = %s", r.URL.RawQuery)
			}
			_, _ = w.Write([]byte(`{"messages":[{"id":"msg_1","kind":"request","thread_id":"thr_1","causal_depth":2,"from":{"kind":"agent","agent_id":"agent_2","agent_name":"peer"},"to":{"kind":"agent","agent_id":"agent_1","agent_name":"scott"},"delivery":{"state":"delivered"},"read_state":{"state":"unread"},"processing":{"state":"claimed","claim_id":"mcl_list","generation":3}}],"next_cursor":"cursor-2"}`))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/messages:listen":
			sawListen = true
			var body struct {
				WaitSeconds int    `json:"wait_seconds"`
				FromAgent   string `json:"from_agent"`
				ThreadID    string `json:"thread_id"`
				Kind        string `json:"kind"`
				Limit       int    `json:"limit"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if body.WaitSeconds != 20 || body.FromAgent != "peer" || body.ThreadID != "thr_1" || body.Kind != "request" || body.Limit != 3 {
				t.Fatalf("listen body = %+v", body)
			}
			_, _ = w.Write([]byte(`{"messages":[{"id":"msg_2","kind":"request","thread_id":"thr_1","reply_to_message_id":"msg_parent","causal_depth":3,"from":{"kind":"agent","agent_id":"agent_2","agent_name":"peer"},"to":{"kind":"agent","agent_id":"agent_1","agent_name":"scott"},"delivery":{"state":"delivered"},"read_state":{"state":"unread"},"processing":{"state":"available","generation":0}}],"timed_out":false}`))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/messages/msg_1:read":
			sawRead = true
			_, _ = w.Write([]byte(`{"message":{"id":"msg_1","kind":"request","thread_id":"thr_1","causal_depth":1,"body":"hello","delivery":{"state":"delivered"},"read_state":{"state":"read"},"from":{"kind":"agent"},"to":{"kind":"agent"}}}`))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/messages/msg_1:ack":
			sawAck = true
			_, _ = w.Write([]byte(`{"message":{"id":"msg_1","kind":"request","thread_id":"thr_1","causal_depth":1,"body":"hello","delivery":{"state":"delivered"},"read_state":{"state":"acked"},"from":{"kind":"agent"},"to":{"kind":"agent"}}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	ctx := context.Background()
	msg, err := SendMessage(ctx, srv.URL, "agent-token", SendMessageInput{
		To: "peer", Kind: "request", Body: "hello", Payload: json.RawMessage(`{"task":42}`), IdempotencyKey: "retry-1",
	})
	if err != nil || msg.ID != "msg_1" || msg.CausalDepth != 1 || string(msg.Payload) != `{"task":42}` {
		t.Fatalf("send = %+v / %v", msg, err)
	}
	reply, err := ReplyMessage(ctx, srv.URL, "agent-token", "msg_parent", ReplyMessageInput{
		Subject: "answer", Kind: "reply", Body: "done", Payload: json.RawMessage(`{"ok":true}`), IdempotencyKey: "reply-retry-1",
	})
	if err != nil || reply.ID != "msg_reply" || reply.ReplyToMessageID != "msg_parent" || reply.CausalDepth != 2 || string(reply.Payload) != `{"ok":true}` {
		t.Fatalf("reply = %+v / %v", reply, err)
	}
	page, err := ListMessages(ctx, srv.URL, "agent-token", MessageListOptions{Direction: "inbox", Unread: true, From: "peer", Limit: 9, Cursor: "next"})
	if err != nil || len(page.Messages) != 1 || page.NextCursor != "cursor-2" ||
		page.Messages[0].Processing.State != "claimed" || page.Messages[0].Processing.ClaimID != "mcl_list" ||
		page.Messages[0].Processing.Generation != 3 || page.Messages[0].CausalDepth != 2 {
		t.Fatalf("list = %+v / %v", page, err)
	}
	waitSeconds := 20
	listen, err := ListenMessages(ctx, srv.URL, "agent-token", MessageListenOptions{
		WaitSeconds: &waitSeconds, From: "peer", ThreadID: "thr_1", Kind: "request", Limit: 3,
	})
	if err != nil || listen.TimedOut || len(listen.Messages) != 1 || listen.Messages[0].ID != "msg_2" ||
		listen.Messages[0].ReplyToMessageID != "msg_parent" || listen.Messages[0].CausalDepth != 3 || listen.Messages[0].Processing.State != "available" {
		t.Fatalf("listen = %+v / %v", listen, err)
	}
	read, err := ReadMessage(ctx, srv.URL, "agent-token", "msg_1")
	if err != nil || read.ReadState.State != "read" || read.CausalDepth != 1 {
		t.Fatalf("read = %+v / %v", read, err)
	}
	acked, err := AckMessage(ctx, srv.URL, "agent-token", "msg_1")
	if err != nil || acked.ReadState.State != "acked" || acked.CausalDepth != 1 {
		t.Fatalf("ack = %+v / %v", acked, err)
	}
	if !sawSend || !sawReply || !sawList || !sawListen || !sawRead || !sawAck {
		t.Fatalf("requests = send:%v reply:%v list:%v listen:%v read:%v ack:%v", sawSend, sawReply, sawList, sawListen, sawRead, sawAck)
	}
}

func TestMessageClientAudienceContracts(t *testing.T) {
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		var body struct {
			To struct {
				Kind string   `json:"kind"`
				ID   string   `json:"id"`
				IDs  []string `json:"ids"`
			} `json:"to"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if calls == 1 {
			if body.To.Kind != "agents" || body.To.ID != "" || !reflect.DeepEqual(body.To.IDs, []string{"bob", "alice"}) {
				t.Fatalf("agents audience = %#v", body.To)
			}
			_, _ = w.Write([]byte(`{"message":{"id":"msg_agents","to":{"kind":"agents","count":2}}}`))
			return
		}
		if body.To.Kind != "realm" || body.To.ID != "" || len(body.To.IDs) != 0 {
			t.Fatalf("realm audience = %#v", body.To)
		}
		_, _ = w.Write([]byte(`{"message":{"id":"msg_realm","to":{"kind":"realm","count":3}}}`))
	}))
	defer server.Close()

	agents, err := SendMessage(context.Background(), server.URL, "token", SendMessageInput{
		AudienceKind: "agents", ToAgents: []string{"bob", "alice"}, Body: "hello",
	})
	if err != nil || agents.To.Kind != "agents" || agents.To.Count != 2 {
		t.Fatalf("agents send = %#v / %v", agents, err)
	}
	realm, err := SendMessage(context.Background(), server.URL, "token", SendMessageInput{
		AudienceKind: "realm", Body: "hello realm",
	})
	if err != nil || realm.To.Kind != "realm" || realm.To.Count != 3 {
		t.Fatalf("realm send = %#v / %v", realm, err)
	}
}

func TestMessageProcessingClientContracts(t *testing.T) {
	var sawClaim, sawRenew, sawRelease, sawComplete bool
	assertKeys := func(t *testing.T, body map[string]json.RawMessage, keys ...string) {
		t.Helper()
		if len(body) != len(keys) {
			t.Fatalf("body keys = %v, want exactly %v", body, keys)
		}
		for _, key := range keys {
			if _, ok := body[key]; !ok {
				t.Fatalf("body omitted %q: %v", key, body)
			}
		}
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer agent-token" {
			t.Errorf("authorization = %q", r.Header.Get("Authorization"))
		}
		var body map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/messages/msg_work:claim":
			sawClaim = true
			assertKeys(t, body, "lease_seconds")
			if string(body["lease_seconds"]) != "30" {
				t.Fatalf("claim body = %v", body)
			}
			if got := r.Header.Get("Idempotency-Key"); got != "claim-retry-1" {
				t.Fatalf("claim idempotency key = %q", got)
			}
			_, _ = w.Write([]byte(`{"processing":{"state":"claimed","claim_id":"mcl_1","generation":7,"lease_expires_at":"2026-07-15T01:02:03Z"}}`))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/messages/msg_work:renew":
			sawRenew = true
			assertKeys(t, body, "claim_id", "generation", "lease_seconds")
			if string(body["claim_id"]) != `"mcl_1"` || string(body["generation"]) != "7" || string(body["lease_seconds"]) != "45" {
				t.Fatalf("renew body = %v", body)
			}
			if got := r.Header.Get("Idempotency-Key"); got != "" {
				t.Fatalf("renew unexpectedly sent idempotency key = %q", got)
			}
			_, _ = w.Write([]byte(`{"processing":{"state":"claimed","claim_id":"mcl_1","generation":7,"lease_expires_at":"2026-07-15T01:02:18Z"}}`))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/messages/msg_work:release":
			sawRelease = true
			assertKeys(t, body, "claim_id", "generation", "deterministic_failure")
			if string(body["claim_id"]) != `"mcl_1"` || string(body["generation"]) != "7" ||
				string(body["deterministic_failure"]) != "true" {
				t.Fatalf("release body = %v", body)
			}
			if got := r.Header.Get("Idempotency-Key"); got != "" {
				t.Fatalf("release unexpectedly sent idempotency key = %q", got)
			}
			_, _ = w.Write([]byte(`{"processing":{"state":"available","generation":7,"failure_count":3}}`))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/messages/msg_work:complete":
			sawComplete = true
			assertKeys(t, body, "claim_id", "generation", "subject", "kind", "body", "payload")
			if string(body["claim_id"]) != `"mcl_1"` || string(body["generation"]) != "7" ||
				string(body["subject"]) != `"answer"` || string(body["kind"]) != `"result"` ||
				string(body["body"]) != `"finished"` || string(body["payload"]) != `{"ok":true}` {
				t.Fatalf("complete body = %v", body)
			}
			if got := r.Header.Get("Idempotency-Key"); got != "complete-retry-1" {
				t.Fatalf("complete idempotency key = %q", got)
			}
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"processing":{"state":"completed","generation":7,"completed_at":"2026-07-15T01:03:03Z","result_message_id":"msg_result"},"message":{"id":"msg_result","kind":"result","thread_id":"thr_1","reply_to_message_id":"msg_work","causal_depth":8,"body":"finished","payload":{"ok":true},"from":{"kind":"agent","agent_id":"agent_worker","agent_name":"worker"},"to":{"kind":"agent","agent_id":"agent_sender","agent_name":"sender"},"delivery":{"state":"delivered"},"read_state":{"state":"unread"}}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	ctx := context.Background()
	claim, err := ClaimMessage(ctx, srv.URL, "agent-token", "msg_work", ClaimMessageInput{
		LeaseSeconds: 30, IdempotencyKey: "claim-retry-1",
	})
	if err != nil || claim.State != "claimed" || claim.ClaimID != "mcl_1" || claim.Generation != 7 || claim.LeaseExpiresAt == nil {
		t.Fatalf("claim = %+v / %v", claim, err)
	}
	renewed, err := RenewMessageClaim(ctx, srv.URL, "agent-token", "msg_work", RenewMessageClaimInput{
		ClaimID: "mcl_1", Generation: 7, LeaseSeconds: 45,
	})
	if err != nil || renewed.State != "claimed" || renewed.ClaimID != "mcl_1" || renewed.Generation != 7 {
		t.Fatalf("renew = %+v / %v", renewed, err)
	}
	released, err := ReleaseMessageClaim(ctx, srv.URL, "agent-token", "msg_work", MessageClaimInput{
		ClaimID: "mcl_1", Generation: 7, DeterministicFailure: true,
	})
	if err != nil || released.State != "available" || released.Generation != 7 || released.FailureCount != 3 {
		t.Fatalf("release = %+v / %v", released, err)
	}
	completed, err := CompleteMessage(ctx, srv.URL, "agent-token", "msg_work", CompleteMessageInput{
		ClaimID: "mcl_1", Generation: 7, Subject: "answer", Kind: "result",
		Body: "finished", Payload: json.RawMessage(`{"ok":true}`), IdempotencyKey: "complete-retry-1",
	})
	if err != nil || completed.Processing.State != "completed" || completed.Processing.ResultMessageID != "msg_result" ||
		completed.Message.ID != "msg_result" || completed.Message.Body != "finished" ||
		string(completed.Message.Payload) != `{"ok":true}` || completed.Message.ReplyToMessageID != "msg_work" || completed.Message.CausalDepth != 8 {
		t.Fatalf("complete = %+v / %v", completed, err)
	}
	if !sawClaim || !sawRenew || !sawRelease || !sawComplete {
		t.Fatalf("requests = claim:%v renew:%v release:%v complete:%v", sawClaim, sawRenew, sawRelease, sawComplete)
	}
}

func TestListenMessagesNormalizesEmptyTimeout(t *testing.T) {
	var requestBody map[string]json.RawMessage
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/messages:listen" {
			http.NotFound(w, r)
			return
		}
		if err := json.NewDecoder(r.Body).Decode(&requestBody); err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write([]byte(`{"timed_out":true}`))
	}))
	defer srv.Close()

	result, err := ListenMessages(context.Background(), srv.URL, "agent-token", MessageListenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !result.TimedOut || result.Messages == nil || len(result.Messages) != 0 {
		t.Fatalf("listen timeout = %+v", result)
	}
	if _, ok := requestBody["wait_seconds"]; ok {
		t.Fatalf("default listen request unexpectedly included wait_seconds: %s", requestBody["wait_seconds"])
	}
}

func TestListenMessagesSendsExplicitZeroWait(t *testing.T) {
	var requestBody map[string]json.RawMessage
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/messages:listen" {
			http.NotFound(w, r)
			return
		}
		if err := json.NewDecoder(r.Body).Decode(&requestBody); err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write([]byte(`{"messages":[],"timed_out":true}`))
	}))
	defer srv.Close()

	waitSeconds := 0
	if _, err := ListenMessages(context.Background(), srv.URL, "agent-token", MessageListenOptions{
		WaitSeconds: &waitSeconds,
	}); err != nil {
		t.Fatal(err)
	}
	raw, ok := requestBody["wait_seconds"]
	if !ok {
		t.Fatal("explicit zero wait_seconds was omitted")
	}
	var got int
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got != 0 {
		t.Fatalf("wait_seconds = %d, want 0", got)
	}
}

func TestMessageListenTransportTimeoutCoversServerWait(t *testing.T) {
	zero, maximum := 0, 20
	tests := []struct {
		name string
		wait *int
		want time.Duration
	}{
		{name: "omitted uses server default plus headroom", want: 25 * time.Second},
		{name: "explicit zero keeps ordinary client floor", wait: &zero, want: 15 * time.Second},
		{name: "maximum includes response headroom", wait: &maximum, want: 25 * time.Second},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := messageListenTransportTimeout(MessageListenOptions{WaitSeconds: tc.wait}); got != tc.want {
				t.Fatalf("timeout = %s, want %s", got, tc.want)
			}
		})
	}
}

func TestListenMessagesContextDeadlineWins(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		<-release
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	started := time.Now()
	waitSeconds := 20
	_, err := ListenMessages(ctx, srv.URL, "agent-token", MessageListenOptions{WaitSeconds: &waitSeconds})
	close(release)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("listen error = %v", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("context deadline took %s", elapsed)
	}
}
