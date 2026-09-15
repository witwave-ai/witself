package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestAgentEmailListenCancellationReleasesCapacity covers request-context
// cancellation through the real route, without a network listener or database.
func TestAgentEmailListenCancellationReleasesCapacity(t *testing.T) {
	pilot, _ := testAgentEmailPilotConfig(t)
	principal := DomainPrincipal{
		Kind: PrincipalKindAgent, ID: "agent_aaaaaaaaaaaaaaaa",
		AccountID: "acc_email_listen", RealmID: "realm_aaaaaaaaaaaaaaaa",
		AccountStatus: "active", AccessProfile: AccessProfileFull,
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	firstCtx, cancelFirst := context.WithCancel(ctx)
	secondCtx, cancelSecond := context.WithCancel(ctx)
	emergency := make(chan struct{})
	var started []*agentEmailListenCancellationRequest
	var calls, unexpected atomic.Int32

	t.Cleanup(func() {
		cancelFirst()
		cancelSecond()
		cancel()
		// An independent escape also drains callbacks whose request context was
		// detached. Each callback returns an error, preventing another poll.
		close(emergency)
		drainCtx, drainCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer drainCancel()
		drained := true
		for _, request := range started {
			select {
			case <-request.done:
				continue
			default:
			}
			select {
			case <-request.done:
			case <-drainCtx.Done():
				drained = false
			}
		}
		if !drained {
			t.Error("email listen cancellation fixture did not drain all started handlers")
			return
		}
		t.Log("email listen cancellation fixture drained all started handlers")
	})

	newRequest := func() *agentEmailListenCancellationRequest {
		return &agentEmailListenCancellationRequest{
			recorder: httptest.NewRecorder(), done: make(chan struct{}),
			entered: make(chan context.Context, 1), returned: make(chan struct{}),
		}
	}
	// Pin the intended per-agent capacity independently of production constants.
	initial := [2]*agentEmailListenCancellationRequest{newRequest(), newRequest()}
	overflow, replacement := newRequest(), newRequest()
	handler := apiMux(Config{
		AuthenticatePrincipal: func(_ context.Context, token string) (DomainPrincipal, bool, error) {
			return principal, token == "email-listen-token", nil
		},
		AgentEmailPilot: pilot,
		ListAgentEmails: func(listCtx context.Context, got DomainPrincipal, opts AgentEmailListOptions) (AgentEmailPage, error) {
			call := calls.Add(1)
			if got != principal || !opts.Unacked || !opts.OldestFirst || opts.Limit != 2 {
				unexpected.Add(1)
				return AgentEmailPage{}, errors.New("unexpected email listen fixture callback")
			}
			if call > 2 {
				// Zero-wait probes must finish even if wrongly admitted. A missing
				// guard then fails on HTTP 200 rather than a timeout or failed drain.
				return AgentEmailPage{}, nil
			}
			request := initial[call-1]
			defer close(request.returned)
			request.entered <- listCtx
			select {
			case <-listCtx.Done():
				return AgentEmailPage{}, listCtx.Err()
			case <-emergency:
				return AgentEmailPage{}, errors.New("email listen fixture emergency release")
			}
		},
	})
	start := func(requestCtx context.Context, request *agentEmailListenCancellationRequest, body string) {
		request.ctx = requestCtx
		started = append(started, request)
		req := httptest.NewRequestWithContext(requestCtx, http.MethodPost, "/v1/email:listen", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer email-listen-token")
		go func() {
			defer close(request.done)
			handler.ServeHTTP(request.recorder, req)
		}()
	}

	// Observe entry before starting the next request: callback ordinals now have
	// deterministic owners, even when the scheduler or test order changes.
	start(firstCtx, initial[0], `{"wait_seconds":20,"limit":2}`)
	awaitAgentEmailListenCancellationEntry(ctx, t, initial[0])
	start(secondCtx, initial[1], `{"wait_seconds":20,"limit":2}`)
	awaitAgentEmailListenCancellationEntry(ctx, t, initial[1])
	assertAgentEmailListenCancellationPaused(ctx, t, initial[0])
	assertAgentEmailListenCancellationPaused(ctx, t, initial[1])
	if calls.Load() != 2 || unexpected.Load() != 0 {
		t.Fatal("email listen cancellation baseline had unexpected callbacks")
	}
	t.Log("two same-principal email listen callbacks are paused")

	start(ctx, overflow, `{"wait_seconds":0,"limit":2}`)
	awaitAgentEmailListenCancellationDone(ctx, t, overflow, "overflow refusal")
	if overflow.recorder.Code != http.StatusTooManyRequests {
		t.Fatalf("email listen per-agent overflow: HTTP %d, want 429", overflow.recorder.Code)
	}
	if overflow.recorder.Header().Get("Retry-After") != "1" ||
		overflow.recorder.Header().Get("Cache-Control") != "private, no-store" {
		t.Fatal("email listen per-agent refusal headers changed")
	}
	if calls.Load() != 2 || unexpected.Load() != 0 {
		t.Fatal("email listen overflow reached an additional list callback")
	}
	assertAgentEmailListenCancellationPaused(ctx, t, initial[0])
	assertAgentEmailListenCancellationPaused(ctx, t, initial[1])
	t.Log("email listen overflow refused before any additional callback")

	// Cancel synchronously, then inspect the actual callback context. A detached
	// context fails immediately; emergency cleanup can still join both handlers.
	cancelFirst()
	if !errors.Is(initial[0].callbackCtx.Err(), context.Canceled) {
		t.Fatal("email listen request cancellation did not reach the captured callback context")
	}
	awaitAgentEmailListenCancellationDone(ctx, t, initial[0], "canceled handler completion")
	assertAgentEmailListenCancellationPaused(ctx, t, initial[1])
	if calls.Load() != 2 || unexpected.Load() != 0 {
		t.Fatal("email listen cancellation caused an unexpected list callback")
	}
	t.Log("first email listen canceled and joined while second remains paused")

	// Reuse this same handler and principal while the second slot remains held.
	// Constructing a new handler would erase the limiter and invalidate the proof.
	start(ctx, replacement, `{"wait_seconds":0,"limit":2}`)
	awaitAgentEmailListenCancellationDone(ctx, t, replacement, "slot replacement")
	if replacement.recorder.Code != http.StatusOK {
		t.Fatalf("email listen canceled slot replacement: HTTP %d, want 200", replacement.recorder.Code)
	}
	var result struct {
		SchemaVersion string              `json:"schema_version"`
		Messages      []AgentEmailMessage `json:"messages"`
		TimedOut      bool                `json:"timed_out"`
	}
	if json.Unmarshal(replacement.recorder.Body.Bytes(), &result) != nil ||
		result.SchemaVersion != "witself.v0" || result.Messages == nil || len(result.Messages) != 0 || !result.TimedOut {
		t.Fatal("email listen replacement did not return an empty successful poll")
	}
	if replacement.recorder.Header().Get("Retry-After") != "" ||
		replacement.recorder.Header().Get("Cache-Control") != "private, no-store" {
		t.Fatal("email listen replacement response headers changed")
	}
	if calls.Load() != 3 || unexpected.Load() != 0 {
		t.Fatal("email listen replacement did not enter exactly one list callback")
	}
	assertAgentEmailListenCancellationPaused(ctx, t, initial[1])
	t.Log("email listen cancellation reused capacity with an empty successful poll")
}

type agentEmailListenCancellationRequest struct {
	ctx         context.Context
	callbackCtx context.Context
	recorder    *httptest.ResponseRecorder
	entered     chan context.Context
	returned    chan struct{}
	done        chan struct{}
}

func awaitAgentEmailListenCancellationEntry(ctx context.Context, t *testing.T, request *agentEmailListenCancellationRequest) {
	t.Helper()
	waitCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	select {
	case request.callbackCtx = <-request.entered:
	case <-request.done:
		t.Fatal("email listen cancellation baseline ended before callback entry")
	case <-waitCtx.Done():
		t.Fatal("email listen cancellation baseline did not reach callback entry")
	}
}

func awaitAgentEmailListenCancellationDone(ctx context.Context, t *testing.T, request *agentEmailListenCancellationRequest, phase string) {
	t.Helper()
	waitCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	select {
	case <-request.done:
	case <-waitCtx.Done():
		t.Fatalf("email listen cancellation fixture did not complete %s", phase)
	}
}

func assertAgentEmailListenCancellationPaused(ctx context.Context, t *testing.T, request *agentEmailListenCancellationRequest) {
	t.Helper()
	if ctx.Err() != nil || request.ctx.Err() != nil || request.callbackCtx == nil || request.callbackCtx.Err() != nil {
		t.Fatal("email listen cancellation fixture expired or canceled a paused request")
	}
	select {
	case <-request.returned:
		t.Fatal("email listen cancellation callback did not remain paused")
	case <-request.done:
		t.Fatal("email listen cancellation handler did not remain active")
	default:
	}
}
