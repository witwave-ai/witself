package client

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

func TestMessageRequestClientContracts(t *testing.T) {
	var calls []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer agent-token" {
			t.Errorf("authorization = %q", r.Header.Get("Authorization"))
		}
		calls = append(calls, r.Method+" "+r.URL.RequestURI())
		var body map[string]json.RawMessage
		if r.Method == http.MethodPost {
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode %s: %v", r.URL.Path, err)
			}
		}
		switch r.URL.Path {
		case "/v1/message-requests":
			if r.Method == http.MethodGet {
				_, _ = w.Write([]byte(`{"requests":[{"id":"mrq_1","state":"open","phase":"awaiting_selection"}],"next_cursor":"next"}`))
				return
			}
			if r.Header.Get("Idempotency-Key") != "open-1" || string(body["body"]) != `"do the work"` || string(body["max_assignees"]) != "2" {
				t.Errorf("create headers/body = %q / %s", r.Header.Get("Idempotency-Key"), body)
			}
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"request":{"id":"mrq_1","state":"open"},"opening_message":{"id":"msg_open","kind":"open_request"}}`))
		case "/v1/message-requests/mrq_1":
			_, _ = w.Write([]byte(`{"request":{"id":"mrq_1","state":"open"},"opening_message":{"id":"msg_open","body":"do the work"},"candidates":[],"offers":[],"selections":[],"claims":[]}`))
		case "/v1/message-requests/mrq_1:offer":
			if r.Header.Get("Idempotency-Key") != "offer-1" || string(body["body"]) != `"I can do it"` {
				t.Errorf("offer headers/body = %q / %s", r.Header.Get("Idempotency-Key"), body)
			}
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"request":{"id":"mrq_1"},"offer":{"agent":{"agent_id":"agent_bob"},"message":{"id":"msg_offer"}}}`))
		case "/v1/message-requests/mrq_1:decline":
			_, _ = w.Write([]byte(`{"request":{"id":"mrq_1","phase":"awaiting_selection"}}`))
		case "/v1/message-requests/mrq_1:select":
			if r.Header.Get("Idempotency-Key") != "select-1" || string(body["selected_agent_ids"]) != `["agent_bob"]` {
				t.Errorf("select headers/body = %q / %s", r.Header.Get("Idempotency-Key"), body)
			}
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"request":{"id":"mrq_1","phase":"assigned"},"selection":{"id":"msel_1"},"claims":[{"claim_id":"mrc_1","state":"reserved"}]}`))
		case "/v1/message-requests/mrq_1:cancel":
			_, _ = w.Write([]byte(`{"request":{"id":"mrq_1","state":"cancelled"}}`))
		case "/v1/message-requests/mrq_1:claim":
			if r.Header.Get("Idempotency-Key") != "claim-1" || string(body["lease_seconds"]) != "120" {
				t.Errorf("claim headers/body = %q / %s", r.Header.Get("Idempotency-Key"), body)
			}
			_, _ = w.Write([]byte(`{"claim":{"claim_id":"mrc_1","state":"claimed","generation":1}}`))
		case "/v1/message-requests/mrq_1:renew":
			_, _ = w.Write([]byte(`{"claim":{"claim_id":"mrc_1","state":"claimed","generation":1}}`))
		case "/v1/message-requests/mrq_1:release":
			_, _ = w.Write([]byte(`{"claim":{"claim_id":"mrc_1","state":"released","generation":1,"failure_count":1}}`))
		case "/v1/message-requests/mrq_1:complete":
			if r.Header.Get("Idempotency-Key") != "complete-1" || string(body["body"]) != `"done"` {
				t.Errorf("complete headers/body = %q / %s", r.Header.Get("Idempotency-Key"), body)
			}
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"request":{"id":"mrq_1","state":"completed"},"claim":{"claim_id":"mrc_1","state":"completed","generation":1},"message":{"id":"msg_result","kind":"result"}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	ctx := context.Background()
	created, err := CreateMessageRequest(ctx, server.URL, "agent-token", CreateMessageRequestInput{
		Body: "do the work", MaxAssignees: 2, OfferWindowSeconds: 30,
		ExpiresInSeconds: 3600, IdempotencyKey: "open-1",
	})
	if err != nil || created.Request.ID != "mrq_1" || created.OpeningMessage.ID != "msg_open" {
		t.Fatalf("create = %#v / %v", created, err)
	}
	page, err := ListMessageRequests(ctx, server.URL, "agent-token", MessageRequestListOptions{
		State: "open", Phase: "awaiting_selection", Role: "coordinator", Limit: 5, Cursor: "cursor",
	})
	if err != nil || len(page.Requests) != 1 || page.NextCursor != "next" {
		t.Fatalf("list = %#v / %v", page, err)
	}
	detail, err := GetMessageRequest(ctx, server.URL, "agent-token", "mrq_1")
	if err != nil || detail.OpeningMessage.Body != "do the work" {
		t.Fatalf("get = %#v / %v", detail, err)
	}
	offer, err := OfferMessageRequest(ctx, server.URL, "agent-token", "mrq_1", OfferMessageRequestInput{Body: "I can do it", IdempotencyKey: "offer-1"})
	if err != nil || offer.Offer.Message.ID != "msg_offer" {
		t.Fatalf("offer = %#v / %v", offer, err)
	}
	if declined, err := DeclineMessageRequest(ctx, server.URL, "agent-token", "mrq_1", "decline-1"); err != nil || declined.Phase != "awaiting_selection" {
		t.Fatalf("decline = %#v / %v", declined, err)
	}
	selected, err := SelectMessageRequest(ctx, server.URL, "agent-token", "mrq_1", SelectMessageRequestInput{
		SelectedAgentIDs: []string{"agent_bob"}, ReservationSeconds: 120, IdempotencyKey: "select-1",
	})
	if err != nil || selected.Selection.ID != "msel_1" || len(selected.Claims) != 1 {
		t.Fatalf("select = %#v / %v", selected, err)
	}
	if claim, err := ClaimMessageRequest(ctx, server.URL, "agent-token", "mrq_1", ClaimMessageRequestInput{LeaseSeconds: 120, IdempotencyKey: "claim-1"}); err != nil || claim.State != "claimed" {
		t.Fatalf("claim = %#v / %v", claim, err)
	}
	if claim, err := RenewMessageRequest(ctx, server.URL, "agent-token", "mrq_1", RenewMessageRequestInput{ClaimID: "mrc_1", Generation: 1, LeaseSeconds: 120}); err != nil || claim.State != "claimed" {
		t.Fatalf("renew = %#v / %v", claim, err)
	}
	if claim, err := ReleaseMessageRequest(ctx, server.URL, "agent-token", "mrq_1", ReleaseMessageRequestInput{ClaimID: "mrc_1", Generation: 1, DeterministicFailure: true}); err != nil || claim.FailureCount != 1 {
		t.Fatalf("release = %#v / %v", claim, err)
	}
	completed, err := CompleteMessageRequest(ctx, server.URL, "agent-token", "mrq_1", CompleteMessageRequestInput{ClaimID: "mrc_1", Generation: 1, Body: "done", IdempotencyKey: "complete-1"})
	if err != nil || completed.Message.ID != "msg_result" || completed.Request.State != "completed" {
		t.Fatalf("complete = %#v / %v", completed, err)
	}
	if cancelled, err := CancelMessageRequest(ctx, server.URL, "agent-token", "mrq_1"); err != nil || cancelled.State != "cancelled" {
		t.Fatalf("cancel = %#v / %v", cancelled, err)
	}

	want := []string{
		"POST /v1/message-requests",
		"GET /v1/message-requests?cursor=cursor&limit=5&phase=awaiting_selection&role=coordinator&state=open",
		"GET /v1/message-requests/mrq_1", "POST /v1/message-requests/mrq_1:offer",
		"POST /v1/message-requests/mrq_1:decline", "POST /v1/message-requests/mrq_1:select",
		"POST /v1/message-requests/mrq_1:claim", "POST /v1/message-requests/mrq_1:renew",
		"POST /v1/message-requests/mrq_1:release", "POST /v1/message-requests/mrq_1:complete",
		"POST /v1/message-requests/mrq_1:cancel",
	}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("calls = %#v, want %#v", calls, want)
	}
}
