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

// Exercise global in-flight admission through the real route, independently of
// the per-agent limit. Controlled callbacks replace mailbox I/O and long polling.
func TestMessageListenGlobalCapacityAndRecovery(t *testing.T) {
	// Pin the intended contract rather than following a production constant.
	const capacity = 128
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	emergency := make(chan struct{})
	principals := make(map[string]*messageListenCapacityRequest)
	var started []*messageListenCapacityRequest
	var listCalls, unexpected atomic.Int32

	t.Cleanup(func() {
		cancel()
		close(emergency)
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cleanupCancel()
		for _, request := range started {
			select {
			case <-request.done:
			case <-cleanupCtx.Done():
				t.Error("message listen global capacity fixture did not drain")
				return
			}
		}
	})

	fixture := func(id, account, realm string, pause bool) *messageListenCapacityRequest {
		request := &messageListenCapacityRequest{
			principal: DomainPrincipal{
				Kind: PrincipalKindAgent, ID: id, AccountID: account,
				RealmID: realm, AccountStatus: "active",
			},
			pause: pause, entered: make(chan struct{}), release: make(chan struct{}),
			returned: make(chan struct{}), done: make(chan struct{}), recorder: httptest.NewRecorder(),
		}
		principals[id] = request
		return request
	}
	initial := make([]*messageListenCapacityRequest, capacity)
	for i := range initial {
		// Sixteen accounts, four realms per account, two distinct agents per realm.
		initial[i] = fixture(fmt.Sprintf("agent_capacity_%03d", i),
			fmt.Sprintf("account_capacity_%02d", i/8),
			fmt.Sprintf("realm_capacity_%02d_%d", i/8, (i/2)%4), true)
	}
	firstOverflow := fixture("agent_overflow_first", "account_overflow_first", "realm_overflow_first", false)
	replacement := fixture("agent_replacement", "account_replacement", "realm_replacement", true)
	secondOverflow := fixture("agent_overflow_second", "account_overflow_second", "realm_overflow_second", false)

	// The registry is complete before requests start and is read-only thereafter.
	handler := apiMux(Config{
		AuthenticatePrincipal: func(_ context.Context, token string) (DomainPrincipal, bool, error) {
			request, ok := principals[token]
			if !ok {
				return DomainPrincipal{}, false, nil
			}
			return request.principal, true, nil
		},
		ListMessages: func(listCtx context.Context, principal DomainPrincipal, _ MessageListOptions) (MessagePage, error) {
			listCalls.Add(1)
			request, ok := principals[principal.ID]
			if !ok || principal != request.principal {
				unexpected.Add(1)
				return MessagePage{}, nil
			}
			if request.calls.Add(1) != 1 {
				unexpected.Add(1)
				return MessagePage{}, nil
			}
			defer close(request.returned)
			close(request.entered)
			// An incorrectly admitted overflow must finish, so a missing global
			// guard fails the HTTP oracle instead of waiting for cleanup or timeout.
			if !request.pause {
				return MessagePage{}, nil
			}
			select {
			case <-request.release:
				return MessagePage{}, nil
			case <-listCtx.Done():
				return MessagePage{}, listCtx.Err()
			case <-emergency:
				return MessagePage{}, nil
			}
		},
	})
	start := func(requestCtx context.Context, request *messageListenCapacityRequest) {
		started = append(started, request)
		req := httptest.NewRequestWithContext(requestCtx, http.MethodPost, "/v1/messages:listen",
			strings.NewReader(`{"wait_seconds":0}`))
		req.Header.Set("Authorization", "Bearer "+request.principal.ID)
		go func() {
			defer close(request.done)
			handler.ServeHTTP(request.recorder, req)
		}()
	}

	for _, request := range initial {
		start(ctx, request)
	}
	for _, request := range initial {
		awaitMessageListenCapacityEntry(ctx, t, request,
			"message listen global capacity did not admit all 128 principals")
	}
	assertMessageListenCapacityPaused(ctx, t, initial)
	if listCalls.Load() != capacity || unexpected.Load() != 0 {
		t.Fatal("message listen global capacity baseline had unexpected callbacks")
	}
	t.Log("128 distinct message listen principals are paused across accounts and realms")

	start(ctx, firstOverflow)
	awaitMessageListenCapacityCompletion(ctx, t, firstOverflow)
	assertMessageListenCapacityRefused(t, firstOverflow, listCalls.Load(), capacity)
	assertMessageListenCapacityPaused(ctx, t, initial)
	t.Log("global message listen overflow refused before any additional callback")

	// Normal completion, without cancellation, must free exactly one global slot.
	close(initial[0].release)
	awaitMessageListenCapacityCompletion(ctx, t, initial[0])
	assertMessageListenCapacitySuccess(t, initial[0])
	assertMessageListenCapacityPaused(ctx, t, initial[1:])
	t.Log("one message listen completed normally while 127 callbacks remain paused")

	start(ctx, replacement)
	awaitMessageListenCapacityEntry(ctx, t, replacement,
		"message listen global capacity did not reuse the completed slot")
	remaining := append([]*messageListenCapacityRequest{replacement}, initial[1:]...)
	assertMessageListenCapacityPaused(ctx, t, remaining)
	if listCalls.Load() != capacity+1 || unexpected.Load() != 0 {
		t.Fatal("message listen global capacity replacement had unexpected callbacks")
	}

	start(ctx, secondOverflow)
	awaitMessageListenCapacityCompletion(ctx, t, secondOverflow)
	assertMessageListenCapacityRefused(t, secondOverflow, listCalls.Load(), capacity+1)
	assertMessageListenCapacityPaused(ctx, t, remaining)

	// Verify the admitted replacement's successful response only after the second
	// overflow proves that its occupied slot restored the global refusal boundary.
	close(replacement.release)
	awaitMessageListenCapacityCompletion(ctx, t, replacement)
	assertMessageListenCapacitySuccess(t, replacement)
	assertMessageListenCapacityPaused(ctx, t, initial[1:])
	if listCalls.Load() != capacity+1 || unexpected.Load() != 0 {
		t.Fatal("message listen global capacity recovery had unexpected callbacks")
	}
	t.Log("one global message listen slot reused successfully and further overflow refused")
}

type messageListenCapacityRequest struct {
	principal DomainPrincipal
	pause     bool
	entered   chan struct{}
	release   chan struct{}
	returned  chan struct{}
	done      chan struct{}
	recorder  *httptest.ResponseRecorder
	calls     atomic.Int32
}

func awaitMessageListenCapacityEntry(ctx context.Context, t *testing.T, request *messageListenCapacityRequest, failure string) {
	t.Helper()
	waitCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	select {
	case <-request.entered:
	case <-request.done:
		// A refused replacement must fail on its actual response, not an entry
		// timeout. Reading the recorder here is safe because the handler returned.
		t.Fatalf("%s: HTTP %d before callback entry", failure, request.recorder.Code)
	case <-waitCtx.Done():
		t.Fatalf("%s: callback entry did not complete", failure)
	}
}

func awaitMessageListenCapacityCompletion(ctx context.Context, t *testing.T, request *messageListenCapacityRequest) {
	t.Helper()
	waitCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	select {
	case <-request.done:
	case <-waitCtx.Done():
		t.Fatal("message listen global capacity request did not complete")
	}
}

func assertMessageListenCapacityPaused(ctx context.Context, t *testing.T, requests []*messageListenCapacityRequest) {
	t.Helper()
	if ctx.Err() != nil {
		t.Fatal("message listen global capacity fixture expired before the admission proof")
	}
	for _, request := range requests {
		if request.calls.Load() != 1 {
			t.Fatal("message listen global capacity callback count was not exactly one per principal")
		}
		select {
		case <-request.returned:
			t.Fatal("message listen global capacity callback did not remain paused")
		case <-request.done:
			t.Fatal("message listen global capacity handler did not remain active")
		default:
		}
	}
}

func assertMessageListenCapacityRefused(t *testing.T, request *messageListenCapacityRequest, calls, wantCalls int32) {
	t.Helper()
	if request.recorder.Code != http.StatusTooManyRequests {
		t.Fatalf("message listen global capacity overflow was not refused: HTTP %d", request.recorder.Code)
	}
	if request.recorder.Header().Get("Retry-After") != "1" ||
		request.recorder.Header().Get("Cache-Control") != "private, no-store" {
		t.Fatal("message listen global capacity refusal headers changed")
	}
	if request.calls.Load() != 0 || calls != wantCalls {
		t.Fatal("message listen global capacity overflow reached the list callback")
	}
}

func assertMessageListenCapacitySuccess(t *testing.T, request *messageListenCapacityRequest) {
	t.Helper()
	var result struct {
		SchemaVersion string    `json:"schema_version"`
		Messages      []Message `json:"messages"`
		TimedOut      bool      `json:"timed_out"`
	}
	if request.recorder.Code != http.StatusOK || request.calls.Load() != 1 ||
		json.Unmarshal(request.recorder.Body.Bytes(), &result) != nil ||
		result.SchemaVersion != "witself.v0" || result.Messages == nil || len(result.Messages) != 0 || !result.TimedOut {
		t.Fatal("message listen global capacity normal completion did not return an empty successful listen")
	}
	if request.recorder.Header().Get("Retry-After") != "" ||
		request.recorder.Header().Get("Cache-Control") != "private, no-store" {
		t.Fatal("message listen global capacity successful response headers changed")
	}
}
