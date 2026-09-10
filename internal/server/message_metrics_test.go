package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const messageProcessingMetric = "witself_message_processing_operations_total"

func TestRuntimeMetricsMessageProcessingCallbacks(t *testing.T) {
	outcomes := []struct {
		name, result string
		err          error
	}{
		{"success", "success", nil},
		{"feature_disabled", "feature_disabled", ErrFeatureNotEnabled},
		{"rate_limited", "rate_limited", ErrMessageRateLimited},
		{"bad_input", "bad_input", ErrBadInput},
		{"not_found", "not_found", ErrNotFound},
		{"forbidden", "forbidden", ErrForbidden},
		{"plan_limited", "plan_limited", ErrPlanLimit},
		{"busy", "busy", ErrBusy},
		{"conflict", "conflict", ErrConflict},
		{"idempotency_conflict", "conflict", ErrIdempotencyConflict},
		{"error", "error", errors.New("private_database_error_canary")},
		{"cancelled", "error", context.Canceled},
		{"deadline", "error", context.DeadlineExceeded},
		{"typed_feature", "feature_disabled", &FeatureNotEnabledError{Feature: "private_feature_canary"}},
		{"typed_rate", "rate_limited", &MessageRateLimitError{Dimension: "private_dimension_canary", Scope: "private_scope_canary", Source: "private_plan_canary", Limit: 987654321, Used: 987654320, Attempted: 23456789, WindowSeconds: 123456789, RetryAfter: time.Hour}},
	}
	for _, outcome := range outcomes[1:] {
		outcome.name = "wrapped_" + outcome.name
		outcome.err = fmt.Errorf("private_wrapped_error_canary: %w", outcome.err)
		outcomes = append(outcomes, outcome)
	}
	for _, outcome := range outcomes {
		t.Run(outcome.name, func(t *testing.T) {
			metrics := newRuntimeMetrics()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			expires := time.Unix(1234567890, 0)
			p := DomainPrincipal{Kind: PrincipalKindAgent, ID: "private_agent_canary", AccountID: "private_account_canary", RealmID: "private_realm_canary", TokenID: "private_token_canary", TokenExpiresAt: &expires}
			const id = "private_message_request_canary"
			claim := ClaimMessageRequest{LeaseSeconds: 73, IdempotencyKey: "private_retry_canary"}
			renew := RenewMessageClaimRequest{ClaimID: "private_claim_canary", Generation: 987654321, LeaseSeconds: 47}
			release := MessageClaimRequest{ClaimID: "private_claim_canary", Generation: 987654321, DeterministicFailure: true}
			derived := MessageRequestDerivedFields{AccountID: json.RawMessage(`"private_derived_account_canary"`), From: json.RawMessage(`{"id":"private_derived_agent_canary"}`)}
			requestClaim := ClaimMessageRequestRequest{LeaseSeconds: 71, IdempotencyKey: "private_request_retry_canary", MessageRequestDerivedFields: derived}
			requestRenew := RenewMessageRequestRequest{ClaimID: "private_request_claim_canary", Generation: 123456789, LeaseSeconds: 41, MessageRequestDerivedFields: derived}
			requestRelease := ReleaseMessageRequestRequest{ClaimID: "private_request_claim_canary", Generation: 123456789, DeterministicFailure: true, MessageRequestDerivedFields: derived}
			processing := MessageProcessing{State: "private_state_canary", ClaimID: "private_claim_canary", Generation: 987654321, FailureCount: 123456789, LeaseExpiresAt: &expires, CompletedAt: &expires, ResultMessageID: "private_result_message_canary"}
			requestProcessing := MessageRequestClaim{State: "private_state_canary", ClaimID: "private_claim_canary", RequestID: "private_request_canary", SelectionID: "private_selection_canary", Agent: MessageAgent{AgentID: "private_returned_agent_canary"}, Generation: 987654321, FailureCount: 123456789, LeaseExpiresAt: &expires, CompletedAt: &expires, ResultMessageID: "private_result_message_canary"}
			calls := make(map[string]int)
			check := func(operation string, gotCtx context.Context, gotP DomainPrincipal, gotID string, gotIn, wantIn any) {
				if gotCtx != ctx || gotP != p || gotID != id || !reflect.DeepEqual(gotIn, wantIn) {
					t.Fatalf("%s changed callback arguments", operation)
				}
				// A callback can inspect metrics without deadlocking the collector.
				metrics.snapshot()
				calls[operation]++
			}
			cfg := metrics.instrumentConfig(Config{
				ClaimMessage: func(c context.Context, principal DomainPrincipal, target string, in ClaimMessageRequest) (MessageProcessing, error) {
					check("claim", c, principal, target, in, claim)
					return processing, outcome.err
				},
				RenewMessageClaim: func(c context.Context, principal DomainPrincipal, target string, in RenewMessageClaimRequest) (MessageProcessing, error) {
					check("renew", c, principal, target, in, renew)
					return processing, outcome.err
				},
				ReleaseMessageClaim: func(c context.Context, principal DomainPrincipal, target string, in MessageClaimRequest) (MessageProcessing, error) {
					check("release", c, principal, target, in, release)
					return processing, outcome.err
				},
				ClaimMessageRequest: func(c context.Context, principal DomainPrincipal, target string, in ClaimMessageRequestRequest) (MessageRequestClaim, error) {
					check("request_claim", c, principal, target, in, requestClaim)
					return requestProcessing, outcome.err
				},
				RenewMessageRequest: func(c context.Context, principal DomainPrincipal, target string, in RenewMessageRequestRequest) (MessageRequestClaim, error) {
					check("request_renew", c, principal, target, in, requestRenew)
					return requestProcessing, outcome.err
				},
				ReleaseMessageRequest: func(c context.Context, principal DomainPrincipal, target string, in ReleaseMessageRequestRequest) (MessageRequestClaim, error) {
					check("request_release", c, principal, target, in, requestRelease)
					return requestProcessing, outcome.err
				},
			})
			want := messageProcessingZeroSamples()
			for _, call := range []struct {
				operation string
				invoke    func() (any, error)
				result    any
			}{
				{"claim", func() (any, error) { return cfg.ClaimMessage(ctx, p, id, claim) }, processing},
				{"renew", func() (any, error) { return cfg.RenewMessageClaim(ctx, p, id, renew) }, processing},
				{"release", func() (any, error) { return cfg.ReleaseMessageClaim(ctx, p, id, release) }, processing},
				{"request_claim", func() (any, error) { return cfg.ClaimMessageRequest(ctx, p, id, requestClaim) }, requestProcessing},
				{"request_renew", func() (any, error) { return cfg.RenewMessageRequest(ctx, p, id, requestRenew) }, requestProcessing},
				{"request_release", func() (any, error) { return cfg.ReleaseMessageRequest(ctx, p, id, requestRelease) }, requestProcessing},
			} {
				repeats := 1
				if outcome.err == nil {
					repeats = 2 // Successful replays count as calls, not unique transitions.
				}
				for range repeats {
					got, err := call.invoke()
					if got != call.result || err != outcome.err {
						t.Fatalf("%s changed result or error identity", call.operation)
					}
				}
				if calls[call.operation] != repeats {
					t.Fatalf("%s callback calls = %d, want %d", call.operation, calls[call.operation], repeats)
				}
				want[messageProcessingSample(call.operation, outcome.result)] = fmt.Sprint(repeats)
			}
			assertMessageProcessingSamples(t, metrics, want)
		})
	}
}

func messageProcessingSample(operation, result string) string {
	return fmt.Sprintf("%s{operation=%q,result=%q}", messageProcessingMetric, operation, result)
}

func messageProcessingZeroSamples() map[string]string {
	want := make(map[string]string)
	for _, operation := range []string{"claim", "renew", "release", "request_claim", "request_renew", "request_release", "unknown"} {
		for _, result := range []string{"success", "feature_disabled", "rate_limited", "bad_input", "not_found", "forbidden", "plan_limited", "busy", "conflict", "error"} {
			want[messageProcessingSample(operation, result)] = "0"
		}
	}
	return want
}

func assertMessageProcessingSamples(t *testing.T, metrics *runtimeMetrics, want map[string]string) {
	t.Helper()
	var output bytes.Buffer
	metrics.writePrometheus(&output)
	text := output.String()
	assertMetricSamples(t, text, messageProcessingMetric, want)
	if strings.Contains(text, "private_") {
		t.Fatal("metrics exposed a private canary")
	}
	if len(metrics.snapshot().messageProcessing) != 70 {
		t.Fatal("message processing map exceeded the closed label inventory")
	}
}

func TestRuntimeMetricsMessageProcessingInitialAndUnknown(t *testing.T) {
	metrics := newRuntimeMetrics()
	assertMessageProcessingSamples(t, metrics, messageProcessingZeroSamples())
	cfg := metrics.instrumentConfig(Config{})
	if cfg.ClaimMessage != nil || cfg.RenewMessageClaim != nil || cfg.ReleaseMessageClaim != nil ||
		cfg.ClaimMessageRequest != nil || cfg.RenewMessageRequest != nil || cfg.ReleaseMessageRequest != nil {
		t.Fatal("instrumentation enabled an absent callback")
	}
	var output bytes.Buffer
	metrics.writePrometheus(&output)
	for _, line := range []string{
		"# HELP " + messageProcessingMetric + " Completed messaging processing callback calls by operation and result; idempotent replays are counted as calls, not durable transitions or lease events.\n",
		"# TYPE " + messageProcessingMetric + " counter\n",
	} {
		if strings.Count(output.String(), line) != 1 {
			t.Fatalf("missing or duplicate metric declaration %q", line)
		}
	}
	before := metrics.snapshot()
	for i := range 100 {
		metrics.observeMessageProcessingOperation(ErrBusy, fmt.Sprintf("private_operation_%d\"\n", i))
	}
	metrics.observeMessageProcessingOperation(nil, "")
	want := messageProcessingZeroSamples()
	want[messageProcessingSample("unknown", "busy")] = "100"
	want[messageProcessingSample("unknown", "success")] = "1"
	assertMessageProcessingSamples(t, metrics, want)
	// Observation must not mutate an already captured snapshot.
	assertMessageProcessingSamples(t, before, messageProcessingZeroSamples())
	// A new process starts fresh, with every series observable before traffic.
	assertMessageProcessingSamples(t, newRuntimeMetrics(), messageProcessingZeroSamples())
}

func TestRuntimeMetricsMessageProcessingOutcomePrecedence(t *testing.T) {
	errorsByPrecedence := []error{ErrFeatureNotEnabled, ErrMessageRateLimited, ErrBadInput, ErrNotFound, ErrForbidden, ErrPlanLimit, ErrBusy, ErrConflict}
	results := []string{"feature_disabled", "rate_limited", "bad_input", "not_found", "forbidden", "plan_limited", "busy", "conflict"}
	metrics := newRuntimeMetrics()
	want := messageProcessingZeroSamples()
	for i, result := range results {
		// Reversed nesting order must not override the documented result precedence.
		err := error(ErrIdempotencyConflict)
		for _, sentinel := range errorsByPrecedence[i:] {
			err = errors.Join(err, sentinel)
		}
		metrics.observeMessageProcessingOperation(err, "claim")
		want[messageProcessingSample("claim", result)] = "1"
	}
	assertMessageProcessingSamples(t, metrics, want)
}

func TestRuntimeMetricsMessageProcessingConcurrentSnapshots(t *testing.T) {
	metrics := newRuntimeMetrics()
	var calls atomic.Int64
	cfg := metrics.instrumentConfig(Config{
		ClaimMessage: func(context.Context, DomainPrincipal, string, ClaimMessageRequest) (MessageProcessing, error) {
			calls.Add(1)
			return MessageProcessing{}, nil
		},
		RenewMessageRequest: func(context.Context, DomainPrincipal, string, RenewMessageRequestRequest) (MessageRequestClaim, error) {
			calls.Add(1)
			return MessageRequestClaim{}, ErrConflict
		},
	})
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 100 {
				_, _ = cfg.ClaimMessage(context.Background(), DomainPrincipal{}, "private_message_canary", ClaimMessageRequest{})
				_, _ = cfg.RenewMessageRequest(context.Background(), DomainPrincipal{}, "private_request_canary", RenewMessageRequestRequest{})
			}
		})
	}
	wg.Go(func() {
		for range 100 {
			var output bytes.Buffer
			metrics.writePrometheus(&output)
		}
	})
	wg.Wait()
	if calls.Load() != 1600 {
		t.Fatalf("backend calls = %d, want 1600", calls.Load())
	}
	want := messageProcessingZeroSamples()
	want[messageProcessingSample("claim", "success")] = "800"
	want[messageProcessingSample("request_renew", "conflict")] = "800"
	assertMessageProcessingSamples(t, metrics, want)
}

func TestRuntimeMetricsMessageProcessingHTTPContention(t *testing.T) {
	for _, outcome := range []struct {
		result string
		err    error
	}{
		{"busy", ErrBusy},
		{"conflict", ErrConflict},
	} {
		t.Run(outcome.result, func(t *testing.T) {
			metrics := newRuntimeMetrics()
			calls := 0
			cfg := messageProcessingHTTPConfig(&calls, fmt.Errorf("private_error_canary: %w", outcome.err))
			api := metrics.instrument(apiMux(metrics.instrumentConfig(cfg)))
			want := messageProcessingZeroSamples()
			for _, route := range []struct {
				operation, path, body string
			}{
				{"claim", "/v1/messages/msg_1:claim", `{"lease_seconds":30}`},
				{"renew", "/v1/messages/msg_1:renew", `{"claim_id":"private_claim_canary","generation":1,"lease_seconds":30}`},
				{"release", "/v1/messages/msg_1:release", `{"claim_id":"private_claim_canary","generation":1}`},
				{"request_claim", "/v1/message-requests/mrq_abcdefghijklmnop:claim", `{"lease_seconds":30}`},
				{"request_renew", "/v1/message-requests/mrq_abcdefghijklmnop:renew", `{"claim_id":"private_claim_canary","generation":1,"lease_seconds":30}`},
				{"request_release", "/v1/message-requests/mrq_abcdefghijklmnop:release", `{"claim_id":"private_claim_canary","generation":1}`},
			} {
				req := httptest.NewRequest(http.MethodPost, route.path, strings.NewReader(route.body))
				req.Header.Set("Authorization", "Bearer private_token_canary")
				response := httptest.NewRecorder()
				api.ServeHTTP(response, req)
				if response.Code != http.StatusConflict || response.Header().Get("Cache-Control") != "private, no-store" {
					t.Fatalf("%s changed conflict response: %d, %q", route.operation, response.Code, response.Header().Get("Cache-Control"))
				}
				if strings.Contains(response.Body.String(), "private_") {
					t.Fatalf("%s exposed private conflict details", route.operation)
				}
				want[messageProcessingSample(route.operation, outcome.result)] = "1"
			}
			assertMessageProcessingSamples(t, metrics, want)
			metricsResponse := httptest.NewRecorder()
			metricsMuxFor(metrics, nil, nil, nil, nil).ServeHTTP(metricsResponse, httptest.NewRequest(http.MethodGet, "/metrics", nil))
			if metricsResponse.Code != http.StatusOK || calls != 6 {
				t.Fatalf("scrape status %d, processing callbacks %d, want 200/6", metricsResponse.Code, calls)
			}
			assertMetricSamples(t, metricsResponse.Body.String(), messageProcessingMetric, want)
		})
	}
}

func TestRuntimeMetricsMessageProcessingHTTPBeforeCallbacks(t *testing.T) {
	for _, path := range []string{"/v1/messages/msg_1:claim", "/v1/message-requests/mrq_abcdefghijklmnop:claim"} {
		for _, refusal := range []string{"unauthenticated", "operator", "invalid_json", "absent"} {
			t.Run(path+"/"+refusal, func(t *testing.T) {
				metrics := newRuntimeMetrics()
				calls := 0
				cfg := messageProcessingHTTPConfig(&calls, nil)
				body, status := `{"lease_seconds":30}`, http.StatusUnauthorized
				switch refusal {
				case "unauthenticated":
					cfg.AuthenticatePrincipal = func(context.Context, string) (DomainPrincipal, bool, error) { return DomainPrincipal{}, false, nil }
				case "operator":
					cfg.AuthenticatePrincipal = func(context.Context, string) (DomainPrincipal, bool, error) {
						return DomainPrincipal{Kind: PrincipalKindOperator, ID: "private_operator_canary", AccountID: "private_account_canary", AccountStatus: "active"}, true, nil
					}
					status = http.StatusForbidden
				case "invalid_json":
					body, status = "{", http.StatusBadRequest
				case "absent":
					cfg.ClaimMessage, cfg.ClaimMessageRequest = nil, nil
					status = http.StatusNotFound
				}
				api := metrics.instrument(apiMux(metrics.instrumentConfig(cfg)))
				req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
				req.Header.Set("Authorization", "Bearer private_token_canary")
				response := httptest.NewRecorder()
				api.ServeHTTP(response, req)
				if response.Code != status || response.Header().Get("Cache-Control") != "private, no-store" || calls != 0 {
					t.Fatalf("refusal: status %d (want %d), cache %q, backend calls %d", response.Code, status, response.Header().Get("Cache-Control"), calls)
				}
				assertMessageProcessingSamples(t, metrics, messageProcessingZeroSamples())
				var httpCalls uint64
				for _, count := range metrics.snapshot().httpRequests {
					httpCalls += count
				}
				if httpCalls != 1 {
					t.Fatalf("generic HTTP calls = %d, want 1", httpCalls)
				}
			})
		}
	}
}

func messageProcessingHTTPConfig(calls *int, err error) Config {
	callback := func() (MessageProcessing, error) {
		*calls++
		return MessageProcessing{}, err
	}
	requestCallback := func() (MessageRequestClaim, error) {
		*calls++
		return MessageRequestClaim{}, err
	}
	return Config{
		AuthenticatePrincipal: func(context.Context, string) (DomainPrincipal, bool, error) {
			return DomainPrincipal{Kind: PrincipalKindAgent, ID: "private_agent_canary", AccountID: "private_account_canary", RealmID: "private_realm_canary", AccountStatus: "active"}, true, nil
		},
		ClaimMessage: func(context.Context, DomainPrincipal, string, ClaimMessageRequest) (MessageProcessing, error) {
			return callback()
		},
		RenewMessageClaim: func(context.Context, DomainPrincipal, string, RenewMessageClaimRequest) (MessageProcessing, error) {
			return callback()
		},
		ReleaseMessageClaim: func(context.Context, DomainPrincipal, string, MessageClaimRequest) (MessageProcessing, error) {
			return callback()
		},
		ClaimMessageRequest: func(context.Context, DomainPrincipal, string, ClaimMessageRequestRequest) (MessageRequestClaim, error) {
			return requestCallback()
		},
		RenewMessageRequest: func(context.Context, DomainPrincipal, string, RenewMessageRequestRequest) (MessageRequestClaim, error) {
			return requestCallback()
		},
		ReleaseMessageRequest: func(context.Context, DomainPrincipal, string, ReleaseMessageRequestRequest) (MessageRequestClaim, error) {
			return requestCallback()
		},
	}
}

func TestRuntimeMetricsMessageProcessingPanicDoesNotComplete(t *testing.T) {
	metrics := newRuntimeMetrics()
	marker := &struct{ text string }{"private_panic_canary"}
	cfg := metrics.instrumentConfig(Config{
		ClaimMessage: func(context.Context, DomainPrincipal, string, ClaimMessageRequest) (MessageProcessing, error) {
			panic(marker)
		},
	})
	func() {
		defer func() {
			if got := recover(); got != marker {
				t.Fatalf("callback panic changed: %v", got)
			}
		}()
		_, _ = cfg.ClaimMessage(context.Background(), DomainPrincipal{}, "private_message_canary", ClaimMessageRequest{})
	}()
	assertMessageProcessingSamples(t, metrics, messageProcessingZeroSamples())
}
