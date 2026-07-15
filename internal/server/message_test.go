package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestMessageHTTPContract(t *testing.T) {
	principal := DomainPrincipal{
		Kind: PrincipalKindAgent, ID: "agent_sender", AccountID: "acc_1",
		RealmID: "realm_1", AgentName: "sender", AccountStatus: "active",
	}
	auth := func(context.Context, string) (DomainPrincipal, bool, error) {
		return principal, true, nil
	}
	var sends, replies, listens, reads, acks int
	var sent SendMessageRequest
	var replied ReplyMessageRequest
	var replyParent string
	cfg := Config{
		AuthenticatePrincipal: auth,
		SendMessage: func(_ context.Context, got DomainPrincipal, in SendMessageRequest) (Message, error) {
			sends++
			if got.ID != principal.ID || got.RealmID != principal.RealmID {
				t.Fatalf("principal = %+v", got)
			}
			sent = in
			return testClaimedMessage(true), nil
		},
		ReplyMessage: func(_ context.Context, got DomainPrincipal, parentMessageID string, in ReplyMessageRequest) (Message, error) {
			replies++
			if got.ID != principal.ID || got.RealmID != principal.RealmID {
				t.Fatalf("principal = %+v", got)
			}
			replyParent = parentMessageID
			replied = in
			msg := testClaimedMessage(true)
			msg.ID = "msg_reply"
			msg.ReplyToMessageID = parentMessageID
			msg.CausalDepth = 5
			return msg, nil
		},
		ListMessages: func(_ context.Context, _ DomainPrincipal, opts MessageListOptions) (MessagePage, error) {
			if opts.OldestFirst {
				listens++
				if opts.Direction != "inbox" || !opts.Unacked || opts.Unread || opts.Cursor != "" ||
					opts.From != "peer" || opts.ThreadID != "thr_1" || opts.Kind != "request" || opts.Limit != 3 {
					t.Fatalf("listen options = %+v", opts)
				}
				return MessagePage{Messages: []Message{testClaimedMessage(true)}}, nil
			}
			if opts.Direction != "inbox" || !opts.Unread || opts.Limit != 7 || opts.From != "peer" {
				t.Fatalf("list options = %+v", opts)
			}
			return MessagePage{Messages: []Message{testClaimedMessage(true)}, NextCursor: "cursor-2"}, nil
		},
		ReadMessage: func(_ context.Context, _ DomainPrincipal, messageID string) (Message, error) {
			reads++
			if messageID != "msg_1" {
				t.Fatalf("read id = %q", messageID)
			}
			return testClaimedMessage(true), nil
		},
		AckMessage: func(_ context.Context, _ DomainPrincipal, _ string) (Message, error) {
			acks++
			msg := testClaimedMessage(true)
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
	if got := resp.Header.Get("Cache-Control"); got != "private, no-store" {
		t.Fatalf("send cache control = %q", got)
	}
	var sendResult struct {
		Message Message `json:"message"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&sendResult); err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if sendResult.Message.CausalDepth != 4 || sendResult.Message.Processing.ClaimID != "" || sendResult.Message.Processing.LeaseExpiresAt != nil {
		t.Fatalf("send exposed processing fence = %+v", sendResult.Message.Processing)
	}
	if sends != 1 || sent.To.ID != "peer" || sent.IdempotencyKey != "retry-1" || string(sent.Payload) != `{"task":42}` {
		t.Fatalf("send = %d / %+v", sends, sent)
	}

	for _, actorField := range []string{"from", "sender", "actor", "realm", "realm_id", "causal_depth"} {
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

	req, _ = http.NewRequest(http.MethodPost, srv.URL+"/v1/messages/msg_1:reply", strings.NewReader(`{"subject":"answer","kind":"reply","body":"done","payload":{"ok":true}}`))
	req.Header.Set("Authorization", "Bearer token")
	req.Header.Set("Idempotency-Key", "reply-retry-1")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("reply status = %d", resp.StatusCode)
	}
	if got := resp.Header.Get("Cache-Control"); got != "private, no-store" {
		t.Fatalf("reply cache control = %q", got)
	}
	var replyResult struct {
		Message Message `json:"message"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&replyResult); err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if replies != 1 || replyParent != "msg_1" || replied.Subject != "answer" || replied.Kind != "reply" ||
		replied.Body != "done" || string(replied.Payload) != `{"ok":true}` || replied.IdempotencyKey != "reply-retry-1" {
		t.Fatalf("reply = %d / %q / %+v", replies, replyParent, replied)
	}
	if replyResult.Message.ID != "msg_reply" || replyResult.Message.ReplyToMessageID != "msg_1" ||
		replyResult.Message.CausalDepth != 5 ||
		replyResult.Message.Processing.ClaimID != "" || replyResult.Message.Processing.LeaseExpiresAt != nil {
		t.Fatalf("reply result = %+v", replyResult.Message)
	}

	for _, routingField := range []string{
		"to", "thread_id", "reply_to_message_id", "from", "sender", "actor",
		"account", "account_id", "realm", "realm_id", "causal_depth",
	} {
		body, marshalErr := json.Marshal(map[string]any{"body": "done", routingField: "spoof"})
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		req, _ = http.NewRequest(http.MethodPost, srv.URL+"/v1/messages/msg_1:reply", strings.NewReader(string(body)))
		req.Header.Set("Authorization", "Bearer token")
		resp, err = http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("reply %s spoof status = %d", routingField, resp.StatusCode)
		}
		_ = resp.Body.Close()
	}
	if replies != 1 {
		t.Fatalf("spoof requests reached reply hook: %d", replies)
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
	if got := resp.Header.Get("Cache-Control"); got != "private, no-store" {
		t.Fatalf("list cache control = %q", got)
	}
	if len(page.Messages) != 1 || page.Messages[0].Body != "" || len(page.Messages[0].Payload) != 0 ||
		page.Messages[0].CausalDepth != 4 ||
		page.Messages[0].Processing.State != "claimed" || page.Messages[0].Processing.ClaimID != "" ||
		page.Messages[0].Processing.LeaseExpiresAt != nil || page.NextCursor != "cursor-2" {
		t.Fatalf("metadata-only list = %+v", page)
	}

	req, _ = http.NewRequest(http.MethodPost, srv.URL+"/v1/messages:listen", strings.NewReader(`{"wait_seconds":0,"from_agent":"peer","thread_id":"thr_1","kind":"request","limit":3}`))
	req.Header.Set("Authorization", "Bearer token")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("listen status = %d", resp.StatusCode)
	}
	if got := resp.Header.Get("Cache-Control"); got != "private, no-store" {
		t.Fatalf("listen cache control = %q", got)
	}
	var listenResult MessageListenResult
	if err := json.NewDecoder(resp.Body).Decode(&listenResult); err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if listens != 1 || listenResult.TimedOut || len(listenResult.Messages) != 1 ||
		listenResult.Messages[0].Body != "" || len(listenResult.Messages[0].Payload) != 0 ||
		listenResult.Messages[0].CausalDepth != 4 ||
		listenResult.Messages[0].Processing.ClaimID != "" ||
		listenResult.Messages[0].Processing.LeaseExpiresAt != nil {
		t.Fatalf("metadata-only listen = calls:%d result:%+v", listens, listenResult)
	}
	if reads != 0 || acks != 0 {
		t.Fatalf("listen mutated message state: read/ack calls = %d/%d", reads, acks)
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
		if got := resp.Header.Get("Cache-Control"); got != "private, no-store" {
			t.Fatalf("%s cache control = %q", action, got)
		}
		var result struct {
			Message Message `json:"message"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if action == "read" && (result.Message.Body != "secret body" || len(result.Message.Payload) == 0) {
			t.Fatalf("read response omitted content = %+v", result.Message)
		}
		if action == "ack" && (result.Message.Body != "" || len(result.Message.Payload) != 0) {
			t.Fatalf("ack response exposed content = %+v", result.Message)
		}
		if result.Message.Processing.ClaimID != "" || result.Message.Processing.LeaseExpiresAt != nil {
			t.Fatalf("%s response exposed processing fence = %+v", action, result.Message.Processing)
		}
		if result.Message.CausalDepth != 4 {
			t.Fatalf("%s response causal depth = %d, want 4", action, result.Message.CausalDepth)
		}
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
	if f := caps.Features["message_listen"]; !f.Supported || f.Reason != "" {
		t.Fatalf("message_listen capability = %+v", f)
	}
	if f := caps.Features["message_reply"]; !f.Supported || f.Reason != "" {
		t.Fatalf("message_reply capability = %+v", f)
	}
}

func TestMessageProcessingHTTPContract(t *testing.T) {
	principal := DomainPrincipal{
		Kind: PrincipalKindAgent, ID: "agent_worker", AccountID: "acc_1",
		RealmID: "realm_1", AgentName: "worker", AccountStatus: "active",
	}
	auth := func(context.Context, string) (DomainPrincipal, bool, error) {
		return principal, true, nil
	}
	leaseExpiry := time.Date(2026, 7, 15, 1, 2, 3, 0, time.UTC)
	completedAt := leaseExpiry.Add(time.Minute)
	var claims, renewals, releases, completions int
	var claimed ClaimMessageRequest
	var renewed RenewMessageClaimRequest
	var released MessageClaimRequest
	var completed CompleteMessageRequest
	cfg := Config{
		AuthenticatePrincipal: auth,
		ClaimMessage: func(_ context.Context, got DomainPrincipal, messageID string, in ClaimMessageRequest) (MessageProcessing, error) {
			claims++
			if got != principal || messageID != "msg_work" {
				t.Fatalf("claim target = %+v / %q", got, messageID)
			}
			claimed = in
			return MessageProcessing{
				State: "claimed", ClaimID: "mcl_1", Generation: 7,
				FailureCount:   2,
				LeaseExpiresAt: &leaseExpiry,
			}, nil
		},
		RenewMessageClaim: func(_ context.Context, got DomainPrincipal, messageID string, in RenewMessageClaimRequest) (MessageProcessing, error) {
			renewals++
			if got != principal || messageID != "msg_work" {
				t.Fatalf("renew target = %+v / %q", got, messageID)
			}
			renewed = in
			return MessageProcessing{
				State: "claimed", ClaimID: in.ClaimID, Generation: in.Generation,
				LeaseExpiresAt: &leaseExpiry,
			}, nil
		},
		ReleaseMessageClaim: func(_ context.Context, got DomainPrincipal, messageID string, in MessageClaimRequest) (MessageProcessing, error) {
			releases++
			if got != principal || messageID != "msg_work" {
				t.Fatalf("release target = %+v / %q", got, messageID)
			}
			released = in
			return MessageProcessing{State: "available", Generation: in.Generation, FailureCount: 3}, nil
		},
		CompleteMessage: func(_ context.Context, got DomainPrincipal, messageID string, in CompleteMessageRequest) (CompleteMessageResult, error) {
			completions++
			if got != principal || messageID != "msg_work" {
				t.Fatalf("complete target = %+v / %q", got, messageID)
			}
			completed = in
			msg := testClaimedMessage(true)
			msg.ID = "msg_result"
			msg.Kind = "result"
			msg.Body = "finished"
			msg.Payload = json.RawMessage(`{"ok":true}`)
			msg.ReplyToMessageID = messageID
			msg.CausalDepth = 5
			return CompleteMessageResult{
				Processing: MessageProcessing{
					State: "completed", Generation: in.Generation,
					CompletedAt: &completedAt, ResultMessageID: msg.ID,
				},
				Message: msg,
			}, nil
		},
	}
	srv := httptest.NewServer(apiMux(cfg))
	defer srv.Close()

	do := func(action, body, idempotencyKey string) (*http.Response, map[string]json.RawMessage) {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/messages/msg_work:"+action, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer token")
		if idempotencyKey != "" {
			req.Header.Set("Idempotency-Key", idempotencyKey)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		var result map[string]json.RawMessage
		if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
			_ = resp.Body.Close()
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		return resp, result
	}

	resp, result := do("claim", `{"lease_seconds":30}`, "claim-retry-1")
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Cache-Control") != "private, no-store" {
		t.Fatalf("claim response = %d / cache %q", resp.StatusCode, resp.Header.Get("Cache-Control"))
	}
	if claims != 1 || claimed.LeaseSeconds != 30 || claimed.IdempotencyKey != "claim-retry-1" {
		t.Fatalf("claim request = %d / %+v", claims, claimed)
	}
	if _, ok := result["message"]; ok {
		t.Fatalf("claim response exposed a message: %s", result["message"])
	}
	var processing MessageProcessing
	if err := json.Unmarshal(result["processing"], &processing); err != nil {
		t.Fatal(err)
	}
	if processing.State != "claimed" || processing.ClaimID != "mcl_1" || processing.Generation != 7 || processing.FailureCount != 2 ||
		processing.LeaseExpiresAt == nil || !processing.LeaseExpiresAt.Equal(leaseExpiry) {
		t.Fatalf("claim processing = %+v", processing)
	}

	resp, result = do("renew", `{"claim_id":"mcl_1","generation":7,"lease_seconds":45}`, "")
	if resp.StatusCode != http.StatusOK || renewals != 1 || renewed.ClaimID != "mcl_1" ||
		renewed.Generation != 7 || renewed.LeaseSeconds != 45 {
		t.Fatalf("renew = status:%d calls:%d request:%+v", resp.StatusCode, renewals, renewed)
	}
	if _, ok := result["processing"]; !ok {
		t.Fatalf("renew response = %v", result)
	}

	resp, result = do("release", `{"claim_id":"mcl_1","generation":7,"deterministic_failure":true}`, "")
	if resp.StatusCode != http.StatusOK || releases != 1 || released.ClaimID != "mcl_1" || released.Generation != 7 || !released.DeterministicFailure {
		t.Fatalf("release = status:%d calls:%d request:%+v", resp.StatusCode, releases, released)
	}
	if err := json.Unmarshal(result["processing"], &processing); err != nil || processing.State != "available" || processing.FailureCount != 3 {
		t.Fatalf("release processing = %+v / %v", processing, err)
	}

	resp, result = do("complete", `{"claim_id":"mcl_1","generation":7,"subject":"answer","kind":"result","body":"finished","payload":{"ok":true}}`, "complete-retry-1")
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("complete status = %d", resp.StatusCode)
	}
	if completions != 1 || completed.ClaimID != "mcl_1" || completed.Generation != 7 ||
		completed.Subject != "answer" || completed.Kind != "result" || completed.Body != "finished" ||
		string(completed.Payload) != `{"ok":true}` || completed.IdempotencyKey != "complete-retry-1" {
		t.Fatalf("complete request = %d / %+v", completions, completed)
	}
	if err := json.Unmarshal(result["processing"], &processing); err != nil ||
		processing.State != "completed" || processing.ResultMessageID != "msg_result" {
		t.Fatalf("complete processing = %+v / %v", processing, err)
	}
	var message Message
	if err := json.Unmarshal(result["message"], &message); err != nil {
		t.Fatal(err)
	}
	if message.ID != "msg_result" || message.Body != "finished" || string(message.Payload) != `{"ok":true}` ||
		message.ReplyToMessageID != "msg_work" || message.CausalDepth != 5 || message.Processing.ClaimID != "" ||
		message.Processing.LeaseExpiresAt != nil {
		t.Fatalf("complete message = %+v", message)
	}

	for _, routingField := range []string{
		"to", "thread_id", "reply_to_message_id", "from", "sender", "actor",
		"account", "account_id", "realm", "realm_id", "causal_depth",
	} {
		body, err := json.Marshal(map[string]any{
			"claim_id": "mcl_1", "generation": 7, "body": "finished", routingField: "spoof",
		})
		if err != nil {
			t.Fatal(err)
		}
		resp, _ = do("complete", string(body), "complete-spoof")
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("complete %s spoof status = %d", routingField, resp.StatusCode)
		}
	}
	if completions != 1 {
		t.Fatalf("spoof requests reached complete hook: %d", completions)
	}

	capResp, err := http.Get(srv.URL + "/v1/capabilities")
	if err != nil {
		t.Fatal(err)
	}
	var caps capabilities
	if err := json.NewDecoder(capResp.Body).Decode(&caps); err != nil {
		_ = capResp.Body.Close()
		t.Fatal(err)
	}
	_ = capResp.Body.Close()
	if feature := caps.Features["message_processing"]; !feature.Supported || feature.Reason != "" {
		t.Fatalf("message_processing capability = %+v", feature)
	}
}

func TestMessageProcessingCapabilityRequiresCompleteSurface(t *testing.T) {
	auth := func(context.Context, string) (DomainPrincipal, bool, error) {
		return DomainPrincipal{Kind: PrincipalKindAgent}, true, nil
	}
	cfg := Config{
		AuthenticatePrincipal: auth,
		ClaimMessage: func(context.Context, DomainPrincipal, string, ClaimMessageRequest) (MessageProcessing, error) {
			return MessageProcessing{}, nil
		},
		RenewMessageClaim: func(context.Context, DomainPrincipal, string, RenewMessageClaimRequest) (MessageProcessing, error) {
			return MessageProcessing{}, nil
		},
		ReleaseMessageClaim: func(context.Context, DomainPrincipal, string, MessageClaimRequest) (MessageProcessing, error) {
			return MessageProcessing{}, nil
		},
	}
	srv := httptest.NewServer(apiMux(cfg))
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/v1/capabilities")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var caps capabilities
	if err := json.NewDecoder(resp.Body).Decode(&caps); err != nil {
		t.Fatal(err)
	}
	if feature := caps.Features["message_processing"]; feature.Supported || feature.Reason != "not_implemented" {
		t.Fatalf("partial message_processing capability = %+v", feature)
	}
}

func TestMessageProcessingErrorMapping(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want int
	}{
		{name: "bad input", err: ErrBadInput, want: http.StatusBadRequest},
		{name: "not found", err: ErrNotFound, want: http.StatusNotFound},
		{name: "forbidden is hidden", err: ErrForbidden, want: http.StatusNotFound},
		{name: "busy", err: ErrBusy, want: http.StatusConflict},
		{name: "stale", err: ErrConflict, want: http.StatusConflict},
		{name: "idempotency conflict", err: ErrIdempotencyConflict, want: http.StatusConflict},
		{name: "internal", err: fmt.Errorf("database unavailable"), want: http.StatusInternalServerError},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Config{
				AuthenticatePrincipal: func(context.Context, string) (DomainPrincipal, bool, error) {
					return DomainPrincipal{
						Kind: PrincipalKindAgent, ID: "agent_worker", AccountID: "acc_1",
						RealmID: "realm_1", AccountStatus: "active",
					}, true, nil
				},
				ClaimMessage: func(context.Context, DomainPrincipal, string, ClaimMessageRequest) (MessageProcessing, error) {
					return MessageProcessing{}, tc.err
				},
			}
			srv := httptest.NewServer(apiMux(cfg))
			defer srv.Close()
			req, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/messages/msg_1:claim", strings.NewReader(`{"lease_seconds":30}`))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Authorization", "Bearer token")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode != tc.want {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tc.want)
			}
			if got := resp.Header.Get("Cache-Control"); got != "private, no-store" {
				t.Fatalf("cache control = %q", got)
			}
		})
	}
}

func TestMessagingHandlersSetNoStoreBeforeAuthentication(t *testing.T) {
	cfg := Config{
		AuthenticatePrincipal: func(context.Context, string) (DomainPrincipal, bool, error) {
			return DomainPrincipal{}, false, nil
		},
		SendMessage: func(context.Context, DomainPrincipal, SendMessageRequest) (Message, error) {
			return Message{}, nil
		},
		ListMessages: func(context.Context, DomainPrincipal, MessageListOptions) (MessagePage, error) {
			return MessagePage{}, nil
		},
		ReadMessage: func(context.Context, DomainPrincipal, string) (Message, error) {
			return Message{}, nil
		},
		AckMessage: func(context.Context, DomainPrincipal, string) (Message, error) {
			return Message{}, nil
		},
		ReplyMessage: func(context.Context, DomainPrincipal, string, ReplyMessageRequest) (Message, error) {
			return Message{}, nil
		},
		ClaimMessage: func(context.Context, DomainPrincipal, string, ClaimMessageRequest) (MessageProcessing, error) {
			return MessageProcessing{}, nil
		},
	}
	srv := httptest.NewServer(apiMux(cfg))
	defer srv.Close()
	for _, tc := range []struct {
		method string
		path   string
		body   string
	}{
		{method: http.MethodPost, path: "/v1/messages", body: `{}`},
		{method: http.MethodGet, path: "/v1/messages"},
		{method: http.MethodPost, path: "/v1/messages:listen", body: `{}`},
		{method: http.MethodPost, path: "/v1/messages/msg_1:read", body: `{}`},
		{method: http.MethodPost, path: "/v1/messages/msg_1:reply", body: `{}`},
		{method: http.MethodPost, path: "/v1/messages/msg_1:claim", body: `{}`},
		{method: http.MethodDelete, path: "/v1/messages"},
		{method: http.MethodGet, path: "/v1/messages:listen"},
		{method: http.MethodGet, path: "/v1/messages/msg_1:ack"},
	} {
		req, err := http.NewRequest(tc.method, srv.URL+tc.path, strings.NewReader(tc.body))
		if err != nil {
			t.Fatal(err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode < 400 {
			t.Fatalf("%s %s unauthenticated status = %d", tc.method, tc.path, resp.StatusCode)
		}
		if got := resp.Header.Get("Cache-Control"); got != "private, no-store" {
			t.Fatalf("%s %s cache control = %q", tc.method, tc.path, got)
		}
	}
}

func TestMessageAckBusyMapsToConflict(t *testing.T) {
	cfg := Config{
		AuthenticatePrincipal: func(context.Context, string) (DomainPrincipal, bool, error) {
			return DomainPrincipal{
				Kind: PrincipalKindAgent, ID: "agt_1", AccountID: "acc_1",
				RealmID: "rlm_1", AccountStatus: "active",
			}, true, nil
		},
		AckMessage: func(context.Context, DomainPrincipal, string) (Message, error) {
			return Message{}, ErrBusy
		},
	}
	srv := httptest.NewServer(apiMux(cfg))
	defer srv.Close()
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/messages/msg_1:ack", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("ack busy status = %d, want 409", resp.StatusCode)
	}
	if got := resp.Header.Get("Cache-Control"); got != "private, no-store" {
		t.Fatalf("ack busy cache control = %q", got)
	}
}

func TestMessageListenRejectsExcessConcurrentRequestsPerAgent(t *testing.T) {
	principal := DomainPrincipal{
		Kind: PrincipalKindAgent, ID: "agent_recipient", AccountID: "acc_1",
		RealmID: "realm_1", AgentName: "recipient", AccountStatus: "active",
	}
	entered := make(chan struct{}, maxConcurrentMessageListensPerAgent+1)
	release := make(chan struct{})
	var listCalls atomic.Int32
	cfg := Config{
		AuthenticatePrincipal: func(context.Context, string) (DomainPrincipal, bool, error) {
			return principal, true, nil
		},
		ListMessages: func(ctx context.Context, _ DomainPrincipal, _ MessageListOptions) (MessagePage, error) {
			listCalls.Add(1)
			entered <- struct{}{}
			select {
			case <-release:
				return MessagePage{}, nil
			case <-ctx.Done():
				return MessagePage{}, ctx.Err()
			}
		},
	}
	srv := httptest.NewServer(apiMux(cfg))
	defer srv.Close()

	type requestResult struct {
		status int
		err    error
	}
	done := make(chan requestResult, maxConcurrentMessageListensPerAgent)
	for range maxConcurrentMessageListensPerAgent {
		go func() {
			req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/messages:listen", strings.NewReader(`{"wait_seconds":0}`))
			req.Header.Set("Authorization", "Bearer token")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				done <- requestResult{err: err}
				return
			}
			defer func() { _ = resp.Body.Close() }()
			done <- requestResult{status: resp.StatusCode}
		}()
	}
	for range maxConcurrentMessageListensPerAgent {
		select {
		case <-entered:
		case <-time.After(2 * time.Second):
			close(release)
			t.Fatal("concurrent listen did not reach the list hook")
		}
	}
	for _, invalid := range []struct {
		name string
		body string
	}{
		{name: "malformed", body: `{"wait_seconds":`},
		{name: "oversized", body: `{"wait_seconds":0,"padding":"` + strings.Repeat("x", 17*1024) + `"}`},
	} {
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/messages:listen", strings.NewReader(invalid.body))
		req.Header.Set("Authorization", "Bearer token")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			close(release)
			t.Fatal(err)
		}
		if resp.StatusCode != http.StatusBadRequest {
			_ = resp.Body.Close()
			close(release)
			t.Fatalf("%s saturated listen status = %d, want 400", invalid.name, resp.StatusCode)
		}
		if got := resp.Header.Get("Cache-Control"); got != "private, no-store" {
			_ = resp.Body.Close()
			close(release)
			t.Fatalf("%s cache control = %q", invalid.name, got)
		}
		if got := resp.Header.Get("Retry-After"); got != "" {
			_ = resp.Body.Close()
			close(release)
			t.Fatalf("%s invalid body consumed limiter: Retry-After=%q", invalid.name, got)
		}
		_ = resp.Body.Close()
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL+"/v1/messages:listen", strings.NewReader(`{"wait_seconds":0}`))
	req.Header.Set("Authorization", "Bearer token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		close(release)
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusTooManyRequests {
		_ = resp.Body.Close()
		close(release)
		t.Fatalf("excess listen status = %d, want 429", resp.StatusCode)
	}
	if got := resp.Header.Get("Retry-After"); got != "1" {
		_ = resp.Body.Close()
		close(release)
		t.Fatalf("Retry-After = %q, want 1", got)
	}
	if got := resp.Header.Get("Cache-Control"); got != "private, no-store" {
		_ = resp.Body.Close()
		close(release)
		t.Fatalf("excess listen cache control = %q", got)
	}
	_ = resp.Body.Close()
	if got := listCalls.Load(); got != int32(maxConcurrentMessageListensPerAgent) {
		close(release)
		t.Fatalf("list hook calls = %d, want %d", got, maxConcurrentMessageListensPerAgent)
	}

	close(release)
	for range maxConcurrentMessageListensPerAgent {
		select {
		case result := <-done:
			if result.err != nil || result.status != http.StatusOK {
				t.Fatal(fmt.Errorf("admitted listen result: status=%d err=%v", result.status, result.err))
			}
		case <-time.After(2 * time.Second):
			t.Fatal("admitted listen did not finish after release")
		}
	}
}

func TestMessageListenImmediateTimeoutShape(t *testing.T) {
	principal := DomainPrincipal{
		Kind: PrincipalKindAgent, ID: "agent_recipient", AccountID: "acc_1",
		RealmID: "realm_1", AgentName: "recipient", AccountStatus: "active",
	}
	var lists, reads, acks int
	cfg := Config{
		AuthenticatePrincipal: func(context.Context, string) (DomainPrincipal, bool, error) {
			return principal, true, nil
		},
		ListMessages: func(_ context.Context, _ DomainPrincipal, opts MessageListOptions) (MessagePage, error) {
			lists++
			if opts.Direction != "inbox" || !opts.Unacked || !opts.OldestFirst {
				t.Fatalf("listen options = %+v", opts)
			}
			return MessagePage{}, nil
		},
		ReadMessage: func(context.Context, DomainPrincipal, string) (Message, error) {
			reads++
			return Message{}, nil
		},
		AckMessage: func(context.Context, DomainPrincipal, string) (Message, error) {
			acks++
			return Message{}, nil
		},
	}
	srv := httptest.NewServer(apiMux(cfg))
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/messages:listen", strings.NewReader(`{"wait_seconds":0}`))
	req.Header.Set("Authorization", "Bearer token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("listen status = %d", resp.StatusCode)
	}
	var result MessageListenResult
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	if !result.TimedOut || result.Messages == nil || len(result.Messages) != 0 {
		t.Fatalf("timeout result = %+v", result)
	}
	if lists != 1 || reads != 0 || acks != 0 {
		t.Fatalf("calls = list:%d read:%d ack:%d", lists, reads, acks)
	}
}

func testMessage(withContent bool) Message {
	msg := Message{
		ID: "msg_1", AccountID: "acc_1", RealmID: "realm_1",
		From: MessageAgent{Kind: "agent", AgentID: "agent_sender", AgentName: "sender"},
		To:   MessageRecipient{Kind: "agent", AgentID: "agent_peer", AgentName: "peer"},
		Kind: "handoff", ThreadID: "thr_1", CausalDepth: 4,
		Delivery:   MessageDelivery{State: "delivered"},
		ReadState:  MessageReadState{State: "unread"},
		Processing: MessageProcessing{State: "available"},
	}
	if withContent {
		msg.Body = "secret body"
		msg.Payload = json.RawMessage(`{"task":42}`)
	}
	return msg
}

func testClaimedMessage(withContent bool) Message {
	msg := testMessage(withContent)
	expires := time.Date(2026, 7, 15, 1, 2, 3, 0, time.UTC)
	msg.Processing = MessageProcessing{
		State: "claimed", ClaimID: "mcl_secret", Generation: 9,
		LeaseExpiresAt: &expires,
	}
	return msg
}
