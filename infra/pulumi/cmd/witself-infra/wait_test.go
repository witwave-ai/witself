package main

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestWaitForCellHealthy pins the four stop conditions of the readiness
// poll: return cleanly on first 200, retry until 200 arrives, honor
// context cancellation immediately, and time out with a helpful message
// when the endpoint never becomes healthy. This is the gate between
// registerCell and restoreCell — a wrong-way retry would either surface a
// benign transient as a hard failure, or spin forever on a cell that
// never came up.
//
// The polls run against an httptest.Server whose host takes the place of
// the real cell endpoint. All timeouts are compressed by 3 orders of
// magnitude so the full suite runs in a couple of seconds.
func TestWaitForCellHealthy(t *testing.T) {
	tests := []struct {
		name       string
		responses  []int         // status code per successive GET
		ctxTimeout time.Duration // 0 = no ctx cancellation
		maxWait    time.Duration
		wantErr    string // substring; empty means expect no error
	}{
		{
			name:      "healthy on first poll",
			responses: []int{200},
			maxWait:   500 * time.Millisecond,
			wantErr:   "",
		},
		{
			name:      "healthy after three transient 503s",
			responses: []int{503, 503, 503, 200},
			maxWait:   2 * time.Second,
			wantErr:   "",
		},
		{
			name:      "healthy after 404 during ALB warmup",
			responses: []int{404, 502, 502, 200},
			maxWait:   2 * time.Second,
			wantErr:   "",
		},
		{
			name:      "times out with reason when endpoint never returns 200",
			responses: []int{503, 503, 503, 503, 503, 503, 503, 503, 503, 503},
			maxWait:   150 * time.Millisecond,
			wantErr:   "did not become healthy",
		},
		{
			name:       "context cancellation returns immediately",
			responses:  []int{503, 503, 503},
			ctxTimeout: 50 * time.Millisecond,
			maxWait:    5 * time.Second,
			wantErr:    "context",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/version" {
					t.Errorf("poll hit unexpected path %q, want /v1/version", r.URL.Path)
				}
				idx := int(calls.Add(1)) - 1
				if idx >= len(tc.responses) {
					idx = len(tc.responses) - 1
				}
				w.WriteHeader(tc.responses[idx])
			}))
			defer srv.Close()

			// httptest gives us http://127.0.0.1:<port>. waitForCellHealthy
			// prefixes https:// itself, so we need to strip the scheme and
			// serve the naked host:port. A little indirection: our poll
			// hits https://<host>, but httptest only speaks HTTP, so we
			// substitute the URL after computing the host.
			u, _ := url.Parse(srv.URL)
			host := u.Host

			// Redirect https requests to the httptest server by replacing
			// the client's transport with one that rewrites scheme+host.
			origClient := httpClientFactory
			httpClientFactory = func() *http.Client {
				return &http.Client{
					Timeout: 10 * time.Second,
					Transport: rewritingTransport{
						base: http.DefaultTransport,
						to:   host,
					},
				}
			}
			defer func() { httpClientFactory = origClient }()

			ctx := context.Background()
			if tc.ctxTimeout > 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, tc.ctxTimeout)
				defer cancel()
			}

			// Poll interval much shorter than maxWait so the retry loop
			// gets a chance to iterate.
			pollEvery := max(tc.maxWait/5, 5*time.Millisecond)
			err := waitForCellHealthy(ctx, host, tc.maxWait, pollEvery)

			if tc.wantErr == "" {
				if err != nil {
					t.Errorf("unexpected error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %v, want substring %q", err, tc.wantErr)
			}
		})
	}
}

// TestWaitForCellHealthyTimeoutPreservesDiagnosticTail pins the fix for
// the review's confirmed finding: the 120-char truncation used to eat
// the diagnostically useful tail of every net/http error. On a real
// hostname like api.aws-sandbox-usw2-dev.cells.witself.witwave.ai the
// `Get "https://<host>/v1/version": ` prefix that net/http prepends is
// 76 chars, leaving 44 for the actual failure — so "no such host",
// "certificate has expired", "unknown authority", and similar diagnostic
// markers all fell off the end of the timeout message. The fix strips
// the redundant prefix and keeps the untruncated last-error for the
// terminal timeout error (per-iteration progress lines still truncate).
func TestWaitForCellHealthyTimeoutPreservesDiagnosticTail(t *testing.T) {
	// Force every transport call to fail with a distinctive tail marker
	// so we can assert it survives to the timeout error.
	origClient := httpClientFactory
	httpClientFactory = func() *http.Client {
		return &http.Client{
			Timeout: 100 * time.Millisecond,
			Transport: failingTransport{err: &net.OpError{
				Op:  "dial",
				Net: "tcp",
				Err: errNoSuchHost{name: "api.aws-sandbox-usw2-dev.cells.witself.witwave.ai"},
			}},
		}
	}
	defer func() { httpClientFactory = origClient }()

	ctx := context.Background()
	err := waitForCellHealthy(ctx,
		"api.aws-sandbox-usw2-dev.cells.witself.witwave.ai",
		50*time.Millisecond, 5*time.Millisecond)

	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
	msg := err.Error()
	if !strings.Contains(msg, "no such host") {
		t.Errorf("timeout error dropped the diagnostic tail — want 'no such host' in: %s", msg)
	}
	// The redundant `Get "<url>": ` prefix must NOT survive to the
	// message (the host is already named up front).
	if strings.Contains(msg, `Get "https://`) {
		t.Errorf("timeout error still contains the redundant Get \"<url>\": prefix: %s", msg)
	}
}

// failingTransport returns a fixed error for every request, so a test can
// stand up any net/http error and observe how waitForCellHealthy handles it.
type failingTransport struct{ err error }

func (t failingTransport) RoundTrip(_ *http.Request) (*http.Response, error) {
	return nil, t.err
}

// errNoSuchHost is what a real DNS-lookup failure produces — a wrapped
// DNSError-shaped thing. We rebuild the minimal surface here so the test
// doesn't depend on net's internal error types (which vary across Go
// versions).
type errNoSuchHost struct{ name string }

func (e errNoSuchHost) Error() string {
	return "lookup " + e.name + " on 169.254.169.253:53: no such host"
}

// rewritingTransport rewrites every outbound request's scheme to http and
// host to the httptest server, so the production waitForCellHealthy code
// (which hard-codes https://) exercises against the local test server.
type rewritingTransport struct {
	base http.RoundTripper
	to   string
}

func (t rewritingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.URL.Scheme = "http"
	req.URL.Host = t.to
	req.Host = t.to
	return t.base.RoundTrip(req)
}
