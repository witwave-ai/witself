package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestMessageRequestHTTPContract(t *testing.T) {
	now := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	principal := DomainPrincipal{
		Kind: PrincipalKindAgent, ID: "agent_scott", AccountID: "acc_1",
		RealmID: "realm_1", AgentName: "Scott", AccountStatus: "active",
	}
	auth := func(context.Context, string) (DomainPrincipal, bool, error) {
		return principal, true, nil
	}
	requestView := MessageRequest{
		ID: "mrq_abcdefghijklmnop", AccountID: "acc_1", RealmID: "realm_1",
		OpeningMessageID: "msg_open", Coordinator: MessageAgent{Kind: "agent", AgentID: "agent_scott", AgentName: "Scott"},
		SelectionPolicy: "client_ranked", State: "open", Phase: "awaiting_selection",
		MaxAssignees: 2, CandidateCount: 2, OfferCount: 1, DeclineCount: 1,
		SelectedAgentIDs: []string{"agent_bob"}, SelectionGeneration: 1,
		OfferDeadline: now.Add(time.Minute), ExpiresAt: now.Add(time.Hour), CreatedAt: now, UpdatedAt: now,
	}
	opening := messageRequestTestMessage("msg_open", "open_request")
	offerMessage := messageRequestTestMessage("msg_offer", "offer")
	resultMessage := messageRequestTestMessage("msg_result", "result")
	claimView := MessageRequestClaim{
		ClaimID: "mrc_abcdefghijklmnop", RequestID: requestView.ID, SelectionID: "msel_abcdefghijklmnop",
		Agent: MessageAgent{Kind: "agent", AgentID: "agent_bob", AgentName: "Bob"},
		State: "claimed", Generation: 3, FailureCount: 1, SelectedAt: now, UpdatedAt: now,
	}
	selection := MessageRequestSelection{
		ID: claimView.SelectionID, Generation: 1, Coordinator: requestView.Coordinator,
		SelectedAgentIDs: []string{"agent_bob"}, CreatedAt: now,
	}
	offerView := MessageRequestOffer{Agent: claimView.Agent, Message: offerMessage, OfferedAt: now}

	var created CreateMessageRequestRequest
	var listed MessageRequestListOptions
	var gotID string
	var offered OfferMessageRequestRequest
	var declined DeclineMessageRequestRequest
	var selected SelectMessageRequestRequest
	var claimed ClaimMessageRequestRequest
	var renewed RenewMessageRequestRequest
	var released ReleaseMessageRequestRequest
	var completed CompleteMessageRequestRequest
	var cancelled bool
	cfg := Config{
		AuthenticatePrincipal: auth,
		CreateMessageRequest: func(_ context.Context, got DomainPrincipal, in CreateMessageRequestRequest) (CreateMessageRequestResult, error) {
			if got != principal {
				t.Fatalf("create principal = %+v", got)
			}
			created = in
			return CreateMessageRequestResult{Request: requestView, OpeningMessage: opening}, nil
		},
		ListMessageRequests: func(_ context.Context, got DomainPrincipal, opts MessageRequestListOptions) (MessageRequestPage, error) {
			if got != principal {
				t.Fatalf("list principal = %+v", got)
			}
			listed = opts
			return MessageRequestPage{Requests: []MessageRequest{requestView}, NextCursor: "cursor-2"}, nil
		},
		GetMessageRequest: func(_ context.Context, got DomainPrincipal, requestID string) (MessageRequestDetail, error) {
			if got != principal {
				t.Fatalf("get principal = %+v", got)
			}
			gotID = requestID
			return MessageRequestDetail{
				Request: requestView, OpeningMessage: opening,
				Candidates: []MessageRequestCandidate{{Agent: claimView.Agent, ResponseState: "offered", OfferMessageID: offerMessage.ID, CreatedAt: now}},
				Offers:     []MessageRequestOffer{offerView}, Selections: []MessageRequestSelection{selection},
				Claims: []MessageRequestClaim{claimView},
			}, nil
		},
		OfferMessageRequest: func(_ context.Context, _ DomainPrincipal, requestID string, in OfferMessageRequestRequest) (OfferMessageRequestResult, error) {
			if requestID != requestView.ID {
				t.Fatalf("offer request id = %q", requestID)
			}
			offered = in
			return OfferMessageRequestResult{Request: requestView, Offer: offerView}, nil
		},
		DeclineMessageRequest: func(_ context.Context, _ DomainPrincipal, requestID string, in DeclineMessageRequestRequest) (MessageRequest, error) {
			if requestID != requestView.ID {
				t.Fatalf("decline request id = %q", requestID)
			}
			declined = in
			return requestView, nil
		},
		SelectMessageRequest: func(_ context.Context, _ DomainPrincipal, requestID string, in SelectMessageRequestRequest) (SelectMessageRequestResult, error) {
			if requestID != requestView.ID {
				t.Fatalf("select request id = %q", requestID)
			}
			selected = in
			return SelectMessageRequestResult{Request: requestView, Selection: selection, Claims: []MessageRequestClaim{claimView}}, nil
		},
		CancelMessageRequest: func(_ context.Context, _ DomainPrincipal, requestID string) (MessageRequest, error) {
			cancelled = requestID == requestView.ID
			return requestView, nil
		},
		ClaimMessageRequest: func(_ context.Context, _ DomainPrincipal, requestID string, in ClaimMessageRequestRequest) (MessageRequestClaim, error) {
			if requestID != requestView.ID {
				t.Fatalf("claim request id = %q", requestID)
			}
			claimed = in
			return claimView, nil
		},
		RenewMessageRequest: func(_ context.Context, _ DomainPrincipal, requestID string, in RenewMessageRequestRequest) (MessageRequestClaim, error) {
			if requestID != requestView.ID {
				t.Fatalf("renew request id = %q", requestID)
			}
			renewed = in
			return claimView, nil
		},
		ReleaseMessageRequest: func(_ context.Context, _ DomainPrincipal, requestID string, in ReleaseMessageRequestRequest) (MessageRequestClaim, error) {
			if requestID != requestView.ID {
				t.Fatalf("release request id = %q", requestID)
			}
			released = in
			return claimView, nil
		},
		CompleteMessageRequest: func(_ context.Context, _ DomainPrincipal, requestID string, in CompleteMessageRequestRequest) (CompleteMessageRequestResult, error) {
			if requestID != requestView.ID {
				t.Fatalf("complete request id = %q", requestID)
			}
			completed = in
			return CompleteMessageRequestResult{Request: requestView, Claim: claimView, Message: resultMessage}, nil
		},
	}
	srv := httptest.NewServer(apiMux(cfg))
	defer srv.Close()

	resp := messageRequestDo(t, srv.URL, http.MethodPost, "/v1/message-requests",
		`{"subject":"deploy","body":"ship it","payload":{"env":"test"},"max_assignees":2,"offer_window_seconds":20,"expires_in_seconds":600}`,
		map[string]string{"Idempotency-Key": "create-key"})
	assertMessageRequestResponse(t, resp, http.StatusCreated)
	var createResponse map[string]json.RawMessage
	decodeMessageRequestResponse(t, resp, &createResponse)
	if _, ok := createResponse["request"]; !ok {
		t.Fatalf("create response missing request: %v", createResponse)
	}
	if _, ok := createResponse["opening_message"]; !ok {
		t.Fatalf("create response missing opening_message: %v", createResponse)
	}
	if created.Subject != "deploy" || created.Body != "ship it" || created.MaxAssignees != 2 ||
		created.OfferWindowSeconds != 20 || created.ExpiresInSeconds != 600 ||
		created.IdempotencyKey != "create-key" || string(created.Payload) != `{"env":"test"}` {
		t.Fatalf("create input = %+v", created)
	}

	resp = messageRequestDo(t, srv.URL, http.MethodGet,
		"/v1/message-requests?state=open&phase=awaiting_selection&role=coordinator&limit=7&cursor=c1", "", nil)
	assertMessageRequestResponse(t, resp, http.StatusOK)
	var listResponse struct {
		Requests   []MessageRequest `json:"requests"`
		NextCursor string           `json:"next_cursor"`
	}
	decodeMessageRequestResponse(t, resp, &listResponse)
	if listed != (MessageRequestListOptions{State: "open", Phase: "awaiting_selection", Role: "coordinator", Limit: 7, Cursor: "c1"}) ||
		len(listResponse.Requests) != 1 || listResponse.NextCursor != "cursor-2" {
		t.Fatalf("list = opts:%+v response:%+v", listed, listResponse)
	}

	resp = messageRequestDo(t, srv.URL, http.MethodGet, "/v1/message-requests/"+requestView.ID, "", nil)
	assertMessageRequestResponse(t, resp, http.StatusOK)
	var detail MessageRequestDetail
	decodeMessageRequestResponse(t, resp, &detail)
	if gotID != requestView.ID || detail.Request.Phase != "awaiting_selection" || len(detail.Offers) != 1 ||
		detail.OpeningMessage.Processing.ClaimID != "" || detail.Offers[0].Message.Processing.ClaimID != "" {
		t.Fatalf("detail = id:%q %+v", gotID, detail)
	}

	actions := []struct {
		name, body, key string
		status          int
	}{
		{"offer", `{"subject":"fit","body":"I can do it","payload":{"score":9}}`, "offer-key", http.StatusCreated},
		{"decline", `{}`, "decline-key", http.StatusOK},
		{"select", `{"selected_agent_ids":["agent_bob"],"reservation_seconds":45}`, "select-key", http.StatusCreated},
		{"cancel", `{}`, "", http.StatusOK},
		{"claim", `{"lease_seconds":30}`, "claim-key", http.StatusOK},
		{"renew", `{"claim_id":"mrc_abcdefghijklmnop","generation":3,"lease_seconds":60}`, "", http.StatusOK},
		{"release", `{"claim_id":"mrc_abcdefghijklmnop","generation":3,"deterministic_failure":true}`, "", http.StatusOK},
		{"complete", `{"claim_id":"mrc_abcdefghijklmnop","generation":3,"subject":"done","body":"shipped","payload":{"ok":true}}`, "complete-key", http.StatusCreated},
	}
	for _, action := range actions {
		headers := map[string]string{}
		if action.key != "" {
			headers["Idempotency-Key"] = action.key
		}
		resp = messageRequestDo(t, srv.URL, http.MethodPost, "/v1/message-requests/"+requestView.ID+":"+action.name, action.body, headers)
		assertMessageRequestResponse(t, resp, action.status)
		var envelope map[string]json.RawMessage
		decodeMessageRequestResponse(t, resp, &envelope)
		if _, ok := envelope["schema_version"]; !ok {
			t.Fatalf("%s response missing schema version", action.name)
		}
	}
	if offered.Body != "I can do it" || offered.IdempotencyKey != "offer-key" || string(offered.Payload) != `{"score":9}` {
		t.Fatalf("offer = %+v", offered)
	}
	if declined.IdempotencyKey != "decline-key" || len(selected.SelectedAgentIDs) != 1 ||
		selected.SelectedAgentIDs[0] != "agent_bob" || selected.ReservationSeconds != 45 || selected.IdempotencyKey != "select-key" || !cancelled {
		t.Fatalf("decline/select/cancel = %+v / %+v / %v", declined, selected, cancelled)
	}
	if claimed.LeaseSeconds != 30 || claimed.IdempotencyKey != "claim-key" || renewed.ClaimID != claimView.ClaimID ||
		renewed.Generation != 3 || renewed.LeaseSeconds != 60 || released.ClaimID != claimView.ClaimID ||
		!released.DeterministicFailure || completed.Body != "shipped" || completed.IdempotencyKey != "complete-key" ||
		string(completed.Payload) != `{"ok":true}` {
		t.Fatalf("claim lifecycle = claim:%+v renew:%+v release:%+v complete:%+v", claimed, renewed, released, completed)
	}

	resp = messageRequestDo(t, srv.URL, http.MethodGet, "/v1/capabilities", "", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("capabilities status = %d", resp.StatusCode)
	}
	var capabilities struct {
		Features map[string]feature `json:"features"`
	}
	decodeMessageRequestResponse(t, resp, &capabilities)
	if !capabilities.Features["message_requests"].Supported {
		t.Fatalf("message_requests capability = %+v", capabilities.Features["message_requests"])
	}
}

func TestMessageRequestRejectsSpoofedDerivedFields(t *testing.T) {
	principal := DomainPrincipal{Kind: PrincipalKindAgent, ID: "agent_1", AccountID: "acc_1", RealmID: "realm_1", AccountStatus: "active"}
	var createCalls, offerCalls, selectCalls, completeCalls int
	cfg := Config{
		AuthenticatePrincipal: func(context.Context, string) (DomainPrincipal, bool, error) { return principal, true, nil },
		CreateMessageRequest: func(context.Context, DomainPrincipal, CreateMessageRequestRequest) (CreateMessageRequestResult, error) {
			createCalls++
			return CreateMessageRequestResult{}, nil
		},
		OfferMessageRequest: func(context.Context, DomainPrincipal, string, OfferMessageRequestRequest) (OfferMessageRequestResult, error) {
			offerCalls++
			return OfferMessageRequestResult{}, nil
		},
		SelectMessageRequest: func(context.Context, DomainPrincipal, string, SelectMessageRequestRequest) (SelectMessageRequestResult, error) {
			selectCalls++
			return SelectMessageRequestResult{}, nil
		},
		CompleteMessageRequest: func(context.Context, DomainPrincipal, string, CompleteMessageRequestRequest) (CompleteMessageRequestResult, error) {
			completeCalls++
			return CompleteMessageRequestResult{}, nil
		},
	}
	srv := httptest.NewServer(apiMux(cfg))
	defer srv.Close()

	tests := []struct{ path, body string }{
		{"/v1/message-requests", `{"body":"work","from":"agent_spoof"}`},
		{"/v1/message-requests/mrq_abcdefghijklmnop:offer", `{"body":"offer","kind":"offer"}`},
		{"/v1/message-requests/mrq_abcdefghijklmnop:select", `{"selected_agent_ids":["agent_2"],"coordinator_agent_id":"agent_spoof"}`},
		{"/v1/message-requests/mrq_abcdefghijklmnop:complete", `{"claim_id":"mrc_1","generation":1,"body":"done","thread_id":"thr_spoof"}`},
	}
	for _, test := range tests {
		resp := messageRequestDo(t, srv.URL, http.MethodPost, test.path, test.body, nil)
		assertMessageRequestResponse(t, resp, http.StatusBadRequest)
		_ = resp.Body.Close()
	}
	if createCalls != 0 || offerCalls != 0 || selectCalls != 0 || completeCalls != 0 {
		t.Fatalf("spoof reached hook: create=%d offer=%d select=%d complete=%d", createCalls, offerCalls, selectCalls, completeCalls)
	}
}

func TestMessageRequestRejectsDurationOverflowBeforeCallingBackend(t *testing.T) {
	principal := DomainPrincipal{Kind: PrincipalKindAgent, ID: "agent_1", AccountID: "acc_1", RealmID: "realm_1", AccountStatus: "active"}
	var calls int
	cfg := Config{
		AuthenticatePrincipal: func(context.Context, string) (DomainPrincipal, bool, error) { return principal, true, nil },
		CreateMessageRequest: func(context.Context, DomainPrincipal, CreateMessageRequestRequest) (CreateMessageRequestResult, error) {
			calls++
			return CreateMessageRequestResult{}, nil
		},
		SelectMessageRequest: func(context.Context, DomainPrincipal, string, SelectMessageRequestRequest) (SelectMessageRequestResult, error) {
			calls++
			return SelectMessageRequestResult{}, nil
		},
		ClaimMessageRequest: func(context.Context, DomainPrincipal, string, ClaimMessageRequestRequest) (MessageRequestClaim, error) {
			calls++
			return MessageRequestClaim{}, nil
		},
		RenewMessageRequest: func(context.Context, DomainPrincipal, string, RenewMessageRequestRequest) (MessageRequestClaim, error) {
			calls++
			return MessageRequestClaim{}, nil
		},
	}
	srv := httptest.NewServer(apiMux(cfg))
	defer srv.Close()

	// On 64-bit platforms, (2^55+n)*time.Second wraps to n seconds. The
	// negative counterpart can do the same. Both must be rejected as raw
	// protocol values rather than reaching duration conversion in the backend.
	for _, test := range []struct {
		name string
		path string
		body string
	}{
		{"create offer positive overflow", "/v1/message-requests", `{"body":"work","offer_window_seconds":36028797018964268}`},
		{"create offer negative overflow", "/v1/message-requests", `{"body":"work","offer_window_seconds":-36028797018963668}`},
		{"create expiry positive overflow", "/v1/message-requests", `{"body":"work","expires_in_seconds":36028797018967868}`},
		{"create expiry negative overflow", "/v1/message-requests", `{"body":"work","expires_in_seconds":-36028797018960368}`},
		{"select reservation positive overflow", "/v1/message-requests/mrq_abcdefghijklmnop:select", `{"selected_agent_ids":["agent_2"],"reservation_seconds":36028797018964268}`},
		{"select reservation negative overflow", "/v1/message-requests/mrq_abcdefghijklmnop:select", `{"selected_agent_ids":["agent_2"],"reservation_seconds":-36028797018963668}`},
		{"claim positive overflow", "/v1/message-requests/mrq_abcdefghijklmnop:claim", `{"lease_seconds":36028797018964268}`},
		{"claim negative overflow", "/v1/message-requests/mrq_abcdefghijklmnop:claim", `{"lease_seconds":-36028797018963668}`},
		{"renew positive overflow", "/v1/message-requests/mrq_abcdefghijklmnop:renew", `{"claim_id":"mrc_abcdefghijklmnop","generation":1,"lease_seconds":36028797018964268}`},
		{"renew negative overflow", "/v1/message-requests/mrq_abcdefghijklmnop:renew", `{"claim_id":"mrc_abcdefghijklmnop","generation":1,"lease_seconds":-36028797018963668}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			resp := messageRequestDo(t, srv.URL, http.MethodPost, test.path, test.body, nil)
			assertMessageRequestResponse(t, resp, http.StatusBadRequest)
			_ = resp.Body.Close()
		})
	}
	if calls != 0 {
		t.Fatalf("overflow values reached backend %d times", calls)
	}
}

func TestMessageRequestErrorsHideRealmMembershipAndFenceConflicts(t *testing.T) {
	principal := DomainPrincipal{Kind: PrincipalKindAgent, ID: "agent_1", AccountID: "acc_1", RealmID: "realm_1", AccountStatus: "active"}
	for _, test := range []struct {
		name string
		err  error
		want int
	}{
		{"bad input", ErrBadInput, http.StatusBadRequest},
		{"not found", ErrNotFound, http.StatusNotFound},
		{"forbidden hidden", ErrForbidden, http.StatusNotFound},
		{"busy", ErrBusy, http.StatusConflict},
		{"stale fence", ErrConflict, http.StatusConflict},
		{"idempotency", ErrIdempotencyConflict, http.StatusConflict},
		{"internal", context.DeadlineExceeded, http.StatusInternalServerError},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := Config{
				AuthenticatePrincipal: func(context.Context, string) (DomainPrincipal, bool, error) { return principal, true, nil },
				ClaimMessageRequest: func(context.Context, DomainPrincipal, string, ClaimMessageRequestRequest) (MessageRequestClaim, error) {
					return MessageRequestClaim{}, test.err
				},
			}
			srv := httptest.NewServer(apiMux(cfg))
			defer srv.Close()
			resp := messageRequestDo(t, srv.URL, http.MethodPost, "/v1/message-requests/mrq_abcdefghijklmnop:claim", `{"lease_seconds":30}`, nil)
			assertMessageRequestResponse(t, resp, test.want)
			_ = resp.Body.Close()
		})
	}
}

func TestMessageRequestAuthAndMethodMismatchRemainPrivate(t *testing.T) {
	operator := DomainPrincipal{Kind: PrincipalKindOperator, ID: "op_1", AccountID: "acc_1", AccountStatus: "active"}
	cfg := Config{
		AuthenticatePrincipal: func(context.Context, string) (DomainPrincipal, bool, error) { return operator, true, nil },
		CreateMessageRequest: func(context.Context, DomainPrincipal, CreateMessageRequestRequest) (CreateMessageRequestResult, error) {
			t.Fatal("operator reached create hook")
			return CreateMessageRequestResult{}, nil
		},
	}
	srv := httptest.NewServer(apiMux(cfg))
	defer srv.Close()

	resp := messageRequestDo(t, srv.URL, http.MethodPost, "/v1/message-requests", `{"body":"work"}`, nil)
	assertMessageRequestResponse(t, resp, http.StatusForbidden)
	_ = resp.Body.Close()

	resp = messageRequestDo(t, srv.URL, http.MethodDelete, "/v1/message-requests", "", nil)
	assertMessageRequestResponse(t, resp, http.StatusMethodNotAllowed)
	_ = resp.Body.Close()
}

func messageRequestTestMessage(id, kind string) Message {
	lease := time.Date(2026, 7, 15, 12, 1, 0, 0, time.UTC)
	return Message{
		ID: id, AccountID: "acc_1", RealmID: "realm_1", Kind: kind, Body: "content",
		From: MessageAgent{Kind: "agent", AgentID: "agent_scott", AgentName: "Scott"},
		To:   MessageRecipient{Kind: "realm"}, ThreadID: "thr_1", CreatedAt: lease.Add(-time.Minute),
		Processing: MessageProcessing{State: "claimed", ClaimID: "private", Generation: 2, LeaseExpiresAt: &lease},
	}
}

func messageRequestDo(t *testing.T, baseURL, method, path, body string, headers map[string]string) *http.Response {
	t.Helper()
	var reader *strings.Reader
	if body == "" {
		reader = strings.NewReader("")
	} else {
		reader = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, baseURL+path, reader)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer token")
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func assertMessageRequestResponse(t *testing.T, resp *http.Response, wantStatus int) {
	t.Helper()
	if resp.StatusCode != wantStatus {
		defer func() { _ = resp.Body.Close() }()
		var body json.RawMessage
		_ = json.NewDecoder(resp.Body).Decode(&body)
		t.Fatalf("status = %d, want %d; body=%s", resp.StatusCode, wantStatus, body)
	}
	if got := resp.Header.Get("Cache-Control"); got != "private, no-store" {
		t.Fatalf("cache-control = %q", got)
	}
}

func decodeMessageRequestResponse(t *testing.T, resp *http.Response, dst any) {
	t.Helper()
	defer func() { _ = resp.Body.Close() }()
	if err := json.NewDecoder(resp.Body).Decode(dst); err != nil {
		t.Fatal(err)
	}
}
