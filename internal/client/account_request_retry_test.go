package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/activity"
)

type accountRetryRequest struct {
	method string
	path   string
	header http.Header
	body   []byte
}

func accountRetryServer(t *testing.T, statuses []int, refusal string, responseHeaders ...http.Header) (string, func() []accountRetryRequest) {
	t.Helper()
	var mu sync.Mutex
	var requests []accountRetryRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read request body: %v", err)
		}
		mu.Lock()
		index := len(requests)
		requests = append(requests, accountRetryRequest{method: r.Method, path: r.URL.RequestURI(), header: r.Header.Clone(), body: body})
		mu.Unlock()
		status := statuses[min(index, len(statuses)-1)]
		w.Header().Set("Content-Type", "application/json")
		for _, headers := range responseHeaders {
			for key, values := range headers {
				w.Header()[key] = values
			}
		}
		w.WriteHeader(status)
		if status == http.StatusOK {
			_, _ = io.WriteString(w, `{"ok":true}`)
		} else {
			_, _ = io.WriteString(w, refusal)
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL + "/v1/example?view=all", func() []accountRetryRequest {
		mu.Lock()
		defer mu.Unlock()
		return append([]accountRetryRequest(nil), requests...)
	}
}

func accountRetryRefusal(t *testing.T, dimension string, seconds int64) string {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"code": "rate_limited", "error": "account request budget exhausted", "retryable": true,
		"retry_after": seconds, "details": map[string]string{"limit_dimension": dimension, "scope": "account"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

func recordAccountRetryWaits(t *testing.T) *[]time.Duration {
	t.Helper()
	realWait := accountRateLimitWait
	var waits []time.Duration
	accountRateLimitWait = func(_ context.Context, d time.Duration) error {
		waits = append(waits, d)
		return nil
	}
	t.Cleanup(func() { accountRateLimitWait = realWait })
	return &waits
}

func TestAccountRequestRateLimitRetries(t *testing.T) {
	for _, scoped := range []bool{false, true} {
		name := "unscoped"
		if scoped {
			name = "activity_retry_scope"
		}
		t.Run(name, func(t *testing.T) {
			waits := recordAccountRetryWaits(t)
			url, requests := accountRetryServer(t, []int{429, 429, 200}, accountRetryRefusal(t, "account_request_read", 1))
			ctx := context.Background()
			if scoped {
				ctx = WithActivityRetryScope(ctx)
			}
			var result struct {
				OK bool `json:"ok"`
			}
			err := doJSONWithHeaders(ctx, http.MethodGet, url, "test-token", map[string]string{"X-Test": "same"}, nil, &result)
			if err != nil || !result.OK {
				t.Fatalf("request result = %v, error = %v", result.OK, err)
			}
			got := requests()
			if len(got) != 3 || !reflect.DeepEqual(*waits, []time.Duration{time.Second, time.Second}) {
				t.Fatalf("requests = %d, waits = %v", len(got), *waits)
			}
			firstID := got[0].header.Get(activity.RequestIDHeader)
			if !activity.ValidRequestID(firstID) {
				t.Fatal("first activity request ID is invalid")
			}
			for _, request := range got {
				if request.header.Get(activity.RequestIDHeader) != firstID {
					t.Error("activity request ID changed across attempts")
				}
				if request.method != http.MethodGet || request.path != "/v1/example?view=all" ||
					request.header.Get("X-Test") != "same" || request.header.Get("Authorization") != "Bearer test-token" {
					t.Error("request method, URL, or headers changed across attempts")
				}
			}
		})
	}
}

func TestAccountRequestRateLimitExhaustion(t *testing.T) {
	waits := recordAccountRetryWaits(t)
	url, requests := accountRetryServer(t, []int{429}, accountRetryRefusal(t, "account_request_write", 1))
	err := doJSON(context.Background(), http.MethodGet, url, "", nil, nil)
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Code != "rate_limited" || !apiErr.Retryable || apiErr.RetryAfter != time.Second ||
		!strings.HasSuffix(err.Error(), "(retry after 1 s)") {
		t.Fatalf("error = %v, want account APIError with retry hint", err)
	}
	if len(requests()) != 3 || !reflect.DeepEqual(*waits, []time.Duration{time.Second, time.Second}) {
		t.Fatalf("requests = %d, waits = %v", len(requests()), *waits)
	}
}

func TestAccountRequestRateLimitRetriesNestedBody(t *testing.T) {
	waits := recordAccountRetryWaits(t)
	nested := `{"error":{"code":"rate_limited","message":"account request budget exhausted","retry_after":1,` +
		`"details":{"limit_dimension":"account_request_read","scope":"account"}}}`
	url, requests := accountRetryServer(t, []int{429, 429, 200}, nested)
	var result struct {
		OK bool `json:"ok"`
	}
	if err := doJSON(context.Background(), http.MethodGet, url, "", nil, &result); err != nil || !result.OK {
		t.Fatalf("request result = %v, error = %v", result.OK, err)
	}
	if len(requests()) != 3 || !reflect.DeepEqual(*waits, []time.Duration{time.Second, time.Second}) {
		t.Fatalf("requests = %d, waits = %v", len(requests()), *waits)
	}
}

func TestAccountRequestRateLimitWaitBounds(t *testing.T) {
	for _, tc := range []struct {
		name         string
		seconds      int64
		header       string
		wantDelay    time.Duration
		wantRequests int
		wantWaits    []time.Duration
	}{
		{name: "long_body", seconds: 30, wantDelay: 30 * time.Second, wantRequests: 1},
		{name: "long_header", header: "30", wantDelay: 30 * time.Second, wantRequests: 1},
		{name: "long_header_short_body", seconds: 2, header: "30", wantDelay: 30 * time.Second, wantRequests: 1},
		{name: "long_body_short_header", seconds: 30, header: "2", wantDelay: 30 * time.Second, wantRequests: 1},
		{name: "above_boundary", seconds: 6, wantDelay: 6 * time.Second, wantRequests: 1},
		{name: "at_boundary", seconds: 5, wantDelay: 5 * time.Second, wantRequests: 3, wantWaits: []time.Duration{5 * time.Second, 5 * time.Second}},
		{name: "short_body", seconds: 2, wantDelay: 2 * time.Second, wantRequests: 3, wantWaits: []time.Duration{2 * time.Second, 2 * time.Second}},
		{name: "short_header", header: "2", wantDelay: 2 * time.Second, wantRequests: 3, wantWaits: []time.Duration{2 * time.Second, 2 * time.Second}},
		{name: "floor", wantRequests: 3, wantWaits: []time.Duration{time.Second, time.Second}},
		{name: "negative", seconds: -1, wantRequests: 3, wantWaits: []time.Duration{time.Second, time.Second}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			waits := recordAccountRetryWaits(t)
			url, requests := accountRetryServer(t, []int{429}, accountRetryRefusal(t, "account_request_read", tc.seconds),
				http.Header{"Retry-After": []string{tc.header}})
			err := doJSON(context.Background(), http.MethodGet, url, "", nil, nil)
			var apiErr *APIError
			if !errors.As(err, &apiErr) || apiErr.RetryAfter != tc.wantDelay ||
				len(requests()) != tc.wantRequests || !reflect.DeepEqual(*waits, tc.wantWaits) {
				t.Fatalf("error = %v, requests = %d, waits = %v", err, len(requests()), *waits)
			}
		})
	}
}

func TestAccountRequestRateLimitDeadline(t *testing.T) {
	waits := recordAccountRetryWaits(t)
	url, requests := accountRetryServer(t, []int{429}, accountRetryRefusal(t, "account_request_read", 1))
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	err := doJSON(ctx, http.MethodGet, url, "", nil, nil)
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.RetryAfter != time.Second || len(requests()) != 1 || len(*waits) != 0 {
		t.Fatalf("error = %v, requests = %d, waits = %v", err, len(requests()), *waits)
	}
}

func TestAccountRequestRateLimitPreservesMessageError(t *testing.T) {
	for _, tc := range []struct {
		name    string
		refusal string
	}{
		{name: "flat", refusal: `{"code":"rate_limited","error":"message rate limit reached","retryable":true,"retry_after":3,"details":{"limit_dimension":"message_sent","limit_key":"message_sent_per_agent_minute","scope":"agent","limit":30,"used":30,"attempted":1,"window_seconds":60,"reset_at":"2026-10-19T12:00:00Z","source":"plan"}}`},
		{name: "nested", refusal: `{"error":{"code":"rate_limited","message":"message rate limit reached","retryable":true,"retry_after":3,"details":{"limit_dimension":"message_sent","limit_key":"message_sent_per_agent_minute","scope":"agent","limit":30,"used":30,"attempted":1,"window_seconds":60,"reset_at":"2026-10-19T12:00:00Z","source":"plan"}}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			waits := recordAccountRetryWaits(t)
			url, requests := accountRetryServer(t, []int{429}, tc.refusal)
			err := doJSON(context.Background(), http.MethodPost, url, "", []byte(`{"body":"hello"}`), nil)
			var rateErr *MessageRateLimitError
			if !errors.Is(err, ErrMessageRateLimited) || !errors.As(err, &rateErr) {
				t.Fatalf("error = %v, want MessageRateLimitError", err)
			}
			if rateErr.LimitDimension != "message_sent" || rateErr.LimitKey != "message_sent_per_agent_minute" || rateErr.Scope != "agent" ||
				rateErr.Limit != 30 || rateErr.Used != 30 || rateErr.Attempted != 1 || rateErr.WindowSeconds != 60 ||
				rateErr.RetryAfter != 3*time.Second || rateErr.ResetAt.Format(time.RFC3339) != "2026-10-19T12:00:00Z" ||
				rateErr.Source != "plan" || !rateErr.Retryable || rateErr.Error() != "message rate limit reached" {
				t.Fatalf("message rate error fields changed: %+v", rateErr)
			}
			if len(requests()) != 1 || len(*waits) != 0 {
				t.Fatalf("requests = %d, waits = %v", len(requests()), *waits)
			}
		})
	}
}

type accountRetryRoundTripFunc func(*http.Request) (*http.Response, error)

func (f accountRetryRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestAccountRequestRateLimitCanceledResponsePreservesMessageError(t *testing.T) {
	for _, tc := range []struct {
		dimension string
		limitKey  string
	}{
		{dimension: "message_sent", limitKey: "message_sent_per_agent_minute"},
		{dimension: "agent_email_sent", limitKey: "agent_email_sent_per_agent_minute"},
	} {
		t.Run(tc.dimension, func(t *testing.T) {
			waits := recordAccountRetryWaits(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			refusal, err := json.Marshal(map[string]any{
				"code": "rate_limited", "error": "message rate limit reached", "retry_after": 3,
				"details": map[string]string{"limit_dimension": tc.dimension, "limit_key": tc.limitKey, "scope": "agent"},
			})
			if err != nil {
				t.Fatal(err)
			}
			calls := 0
			original := http.DefaultTransport
			t.Cleanup(func() { http.DefaultTransport = original })
			http.DefaultTransport = accountRetryRoundTripFunc(func(*http.Request) (*http.Response, error) {
				calls++
				cancel()
				return &http.Response{StatusCode: http.StatusTooManyRequests, Body: io.NopCloser(bytes.NewReader(refusal))}, nil
			})
			err = doJSON(ctx, http.MethodGet, "http://example.invalid/v1/example", "", nil, nil)
			var rateErr *MessageRateLimitError
			if !errors.As(err, &rateErr) || rateErr.LimitDimension != tc.dimension || rateErr.RetryAfter != 3*time.Second ||
				calls != 1 || len(*waits) != 0 {
				t.Fatalf("error = %v, requests = %d, waits = %v", err, calls, *waits)
			}
		})
	}
}

func TestAccountRequestRateLimitCanceledFinalResponsePreservesAPIError(t *testing.T) {
	waits := recordAccountRetryWaits(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	refusal := accountRetryRefusal(t, "account_request_read", 1)
	calls := 0
	original := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = original })
	http.DefaultTransport = accountRetryRoundTripFunc(func(*http.Request) (*http.Response, error) {
		calls++
		if calls == 3 {
			cancel()
		}
		return &http.Response{StatusCode: http.StatusTooManyRequests, Body: io.NopCloser(strings.NewReader(refusal))}, nil
	})
	err := doJSON(ctx, http.MethodGet, "http://example.invalid/v1/example", "", nil, nil)
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.RetryAfter != time.Second || calls != 3 ||
		!reflect.DeepEqual(*waits, []time.Duration{time.Second, time.Second}) {
		t.Fatalf("error = %v, requests = %d, waits = %v", err, calls, *waits)
	}
}

func TestAccountRequestRateLimitDoesNotRetryOtherErrors(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{name: "server_error", status: 503, body: accountRetryRefusal(t, "account_request_read", 1)},
		{name: "other_dimension", status: 429, body: accountRetryRefusal(t, "api_requests", 1)},
		{name: "other_scope", status: 429, body: `{"code":"rate_limited","error":"refused","details":{"limit_dimension":"account_request_read","scope":"agent"}}`},
		{name: "other_code", status: 429, body: `{"code":"limit_exceeded","error":"refused","details":{"limit_dimension":"account_request_read","scope":"account"}}`},
		{name: "malformed_json", status: 429, body: `{"code":"rate_limited"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			waits := recordAccountRetryWaits(t)
			url, requests := accountRetryServer(t, []int{tc.status}, tc.body)
			err := doJSON(context.Background(), http.MethodGet, url, "", nil, nil)
			if err == nil || len(requests()) != 1 || len(*waits) != 0 {
				t.Fatalf("error = %v, requests = %d, waits = %v", err, len(requests()), *waits)
			}
		})
	}
}

func TestAccountRequestRateLimitReplaysBody(t *testing.T) {
	_ = recordAccountRetryWaits(t)
	url, requests := accountRetryServer(t, []int{429, 429, 200}, accountRetryRefusal(t, "account_request_write", 1))
	body := []byte("{\"value\": \"café\", \"lines\": [1, 2]}\n")
	if err := doJSON(context.Background(), http.MethodPost, url, "", body, nil); err != nil {
		t.Fatal(err)
	}
	got := requests()
	if len(got) != 3 {
		t.Fatalf("requests = %d", len(got))
	}
	for i, request := range got {
		if request.method != http.MethodPost || !bytes.Equal(request.body, body) || request.header.Get("Content-Type") != "application/json" {
			t.Errorf("request %d changed POST body, method, or content type", i+1)
		}
	}
}

func TestAccountRequestRateLimitCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	realWait := accountRateLimitWait
	var delay time.Duration
	accountRateLimitWait = func(ctx context.Context, d time.Duration) error {
		delay = d
		cancel()
		return realWait(ctx, d)
	}
	t.Cleanup(func() { accountRateLimitWait = realWait })
	url, requests := accountRetryServer(t, []int{429}, accountRetryRefusal(t, "account_request_read", 5))
	start := time.Now()
	err := doJSON(ctx, http.MethodGet, url, "", nil, nil)
	elapsed := time.Since(start)
	if !errors.Is(err, context.Canceled) || len(requests()) != 1 || delay != 5*time.Second || elapsed >= delay/2 {
		t.Fatalf("error = %v, requests = %d, delay = %v, elapsed = %v", err, len(requests()), delay, elapsed)
	}
}

type accountRetryReadCloser struct {
	reader io.Reader
	read   int
	closed bool
}

func (r *accountRetryReadCloser) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	r.read += n
	return n, err
}

func (r *accountRetryReadCloser) Close() error {
	r.closed = true
	return nil
}

func TestAccountRequestRateLimitBodyIsBoundedAndClosed(t *testing.T) {
	body := &accountRetryReadCloser{reader: strings.NewReader(strings.Repeat("x", 128<<10))}
	resp := &http.Response{Body: body}
	_, retry := accountRequestRateLimitDelay(resp)
	if retry || !body.closed || body.read != 64<<10 {
		t.Fatalf("retry = %v, closed = %v, bytes read = %d", retry, body.closed, body.read)
	}
}

func TestAPIErrorRetryAfterMessage(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  *APIError
		want string
	}{
		{name: "nil", want: "API request failed"},
		{name: "without_delay", err: &APIError{Code: "rate_limited", Message: "refused"}, want: "refused"},
		{name: "other_code", err: &APIError{Code: "other", RetryAfter: time.Second, Message: "refused"}, want: "refused"},
		{name: "default_message", err: &APIError{Code: "rate_limited", RetryAfter: time.Second}, want: "API request failed (retry after 1 s)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.err.Error(); got != tc.want {
				t.Fatalf("Error() = %q, want %q", got, tc.want)
			}
		})
	}
}
