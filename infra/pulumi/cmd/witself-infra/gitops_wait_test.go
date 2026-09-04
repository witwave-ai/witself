package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pulumi/pulumi/sdk/v3/go/auto"
)

const testGitopsRevision = "main"

func TestExpectedBootstrapArgoApplications(t *testing.T) {
	got := expectedBootstrapArgoApplications("release-2026-09-04")
	want := []argoApplicationExpectation{
		{name: "bootstrap", targetRevision: "release-2026-09-04"},
		{name: "apps", targetRevision: "release-2026-09-04"},
		{name: "platform", targetRevision: "release-2026-09-04"},
	}
	if len(got) != len(want) {
		t.Fatalf("expectation count = %d, want %d: %#v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("expectation %d = %#v, want %#v", i, got[i], want[i])
		}
	}
}

func TestArgoApplicationsReady(t *testing.T) {
	expected := expectedBootstrapArgoApplications(testGitopsRevision)
	tests := []struct {
		name    string
		apps    []argoApplication
		ready   bool
		wantWhy string
	}{
		{
			name:    "empty list waits",
			apps:    nil,
			ready:   false,
			wantWhy: "no Argo CD applications",
		},
		{
			name: "all synced healthy",
			apps: []argoApplication{
				mkArgoApp("bootstrap", "Synced", "Healthy", ""),
				mkArgoApp("apps", "Synced", "Healthy", ""),
				mkArgoApp("platform", "Synced", "Healthy", ""),
				mkArgoApp("witself-server", "Synced", "Healthy", ""),
			},
			ready: true,
		},
		{
			name: "parent progressing reports culprit",
			apps: []argoApplication{
				mkArgoApp("bootstrap", "Synced", "Healthy", ""),
				mkArgoApp("apps", "Synced", "Progressing", "waiting for ManagedCertificate/witself-api"),
				mkArgoApp("platform", "Synced", "Healthy", ""),
				mkArgoApp("witself-server", "Synced", "Healthy", ""),
			},
			ready:   false,
			wantWhy: "apps Synced/Progressing: waiting for ManagedCertificate/witself-api",
		},
		{
			name: "out of sync child reports sync and health",
			apps: []argoApplication{
				mkArgoApp("bootstrap", "Synced", "Healthy", ""),
				mkArgoApp("apps", "Synced", "Healthy", ""),
				mkArgoApp("platform", "Synced", "Healthy", ""),
				mkArgoApp("external-dns", "OutOfSync", "Healthy", ""),
			},
			ready:   false,
			wantWhy: "external-dns OutOfSync/Healthy",
		},
		{
			name: "missing required application waits",
			apps: []argoApplication{
				mkArgoApp("bootstrap", "Synced", "Healthy", ""),
				mkArgoApp("apps", "Synced", "Healthy", ""),
				mkArgoApp("witself-server", "Synced", "Healthy", ""),
			},
			ready:   false,
			wantWhy: "platform missing",
		},
		{
			name: "wrong declared target revision waits",
			apps: func() []argoApplication {
				apps := healthyExpectedArgoApps()
				apps[0].Spec.Sources[1].TargetRevision = "previous"
				return apps
			}(),
			ready:   false,
			wantWhy: "bootstrap declares target revisions",
		},
		{
			name: "stale compared target revision waits",
			apps: func() []argoApplication {
				apps := healthyExpectedArgoApps()
				apps[1].Status.Sync.ComparedTo.Sources[0].TargetRevision = "previous"
				return apps
			}(),
			ready:   false,
			wantWhy: "apps last compared target revisions",
		},
		{
			name: "incomplete compared source set waits",
			apps: func() []argoApplication {
				apps := healthyExpectedArgoApps()
				apps[2].Status.Sync.ComparedTo.Sources = apps[2].Status.Sync.ComparedTo.Sources[:1]
				return apps
			}(),
			ready:   false,
			wantWhy: "platform last compared target revisions",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ready, why := argoApplicationsReady(tc.apps, expected)
			if ready != tc.ready {
				t.Fatalf("ready = %t, want %t (why %q)", ready, tc.ready, why)
			}
			if tc.wantWhy != "" && !strings.Contains(why, tc.wantWhy) {
				t.Fatalf("why = %q, want substring %q", why, tc.wantWhy)
			}
		})
	}
}

func TestArgoApplicationsReadyRequiresHealthyRoot(t *testing.T) {
	apps := []argoApplication{
		mkArgoApp("bootstrap", "Synced", "Progressing", ""),
		mkArgoApp("apps", "Synced", "Healthy", ""),
		mkArgoApp("platform", "Synced", "Healthy", ""),
		mkArgoApp("witself-server", "Synced", "Healthy", ""),
	}
	ready, why := argoApplicationsReady(apps, expectedBootstrapArgoApplications(testGitopsRevision))
	if ready {
		t.Fatal("ready = true, want false for a Progressing bootstrap root")
	}
	if !strings.Contains(why, "bootstrap Synced/Progressing") {
		t.Fatalf("why = %q, want bootstrap diagnostic", why)
	}
}

func TestWaitForArgoApplicationsHealthy(t *testing.T) {
	expected := expectedBootstrapArgoApplications(testGitopsRevision)
	t.Run("waits through progressing parent", func(t *testing.T) {
		lister := &fakeArgoLister{responses: []argoListResponse{
			{apps: []argoApplication{
				mkArgoApp("bootstrap", "Synced", "Healthy", ""),
				mkArgoApp("apps", "Synced", "Progressing", "certificate provisioning"),
				mkArgoApp("platform", "Synced", "Healthy", ""),
				mkArgoApp("witself-server", "Synced", "Healthy", ""),
			}},
			{apps: []argoApplication{
				mkArgoApp("bootstrap", "Synced", "Healthy", ""),
				mkArgoApp("apps", "Synced", "Healthy", ""),
				mkArgoApp("platform", "Synced", "Healthy", ""),
				mkArgoApp("witself-server", "Synced", "Healthy", ""),
			}},
		}}
		if err := waitForArgoApplicationsHealthy(context.Background(), lister, "argocd", expected, time.Second, 5*time.Millisecond); err != nil {
			t.Fatalf("waitForArgoApplicationsHealthy: %v", err)
		}
		if got := lister.calls.Load(); got != 2 {
			t.Fatalf("calls = %d, want 2", got)
		}
	})

	t.Run("timeout includes last diagnostic", func(t *testing.T) {
		lister := &fakeArgoLister{responses: []argoListResponse{
			{apps: []argoApplication{
				mkArgoApp("bootstrap", "Synced", "Healthy", ""),
				mkArgoApp("apps", "Synced", "Progressing", "certificate provisioning"),
				mkArgoApp("platform", "Synced", "Healthy", ""),
			}},
		}}
		err := waitForArgoApplicationsHealthy(context.Background(), lister, "argocd", expected, 20*time.Millisecond, 5*time.Millisecond)
		if err == nil {
			t.Fatal("expected timeout")
		}
		if !strings.Contains(err.Error(), "certificate provisioning") {
			t.Fatalf("error = %v, want last diagnostic", err)
		}
	})

	// Pins the fail-fast fix: an expired-ADC (or any errLocalAuth)
	// failure must abort on the FIRST poll instead of retrying the
	// same doomed local CLI call until maxWait — the observed failure
	// burned 15 minutes of "mint GCP ADC access token" spam before
	// reporting what one poll already knew.
	t.Run("local auth failure aborts immediately", func(t *testing.T) {
		lister := &fakeArgoLister{responses: []argoListResponse{
			{err: fmt.Errorf("%w: mint GCP ADC access token: exit status 1: Reauthentication failed", errLocalAuth)},
		}}
		started := time.Now()
		err := waitForArgoApplicationsHealthy(context.Background(), lister, "argocd", expected, time.Hour, time.Hour)
		if err == nil {
			t.Fatal("expected immediate abort")
		}
		if elapsed := time.Since(started); elapsed > 5*time.Second {
			t.Fatalf("abort took %s — it retried instead of failing fast", elapsed)
		}
		if got := lister.calls.Load(); got != 1 {
			t.Fatalf("calls = %d, want exactly 1 (no retries on local auth failure)", got)
		}
		if !strings.Contains(err.Error(), "press `a`") {
			t.Fatalf("error must carry the remedy, got: %v", err)
		}
	})

	// A REMOTE error (cluster still coming up, API flap) must still
	// retry — only local-credential failures short-circuit.
	t.Run("remote errors still retry", func(t *testing.T) {
		lister := &fakeArgoLister{responses: []argoListResponse{
			{err: fmt.Errorf("query Argo CD applications: HTTP 503: upstream connect error")},
			{apps: []argoApplication{
				mkArgoApp("bootstrap", "Synced", "Healthy", ""),
				mkArgoApp("apps", "Synced", "Healthy", ""),
				mkArgoApp("platform", "Synced", "Healthy", ""),
			}},
		}}
		if err := waitForArgoApplicationsHealthy(context.Background(), lister, "argocd", expected, time.Second, 5*time.Millisecond); err != nil {
			t.Fatalf("remote flap should heal by retrying: %v", err)
		}
		if got := lister.calls.Load(); got != 2 {
			t.Fatalf("calls = %d, want 2", got)
		}
	})
}

func TestWaitForPostUpConvergenceAWSArgoDisabledIsNoOp(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := waitForPostUpConvergence(
		ctx, auto.Stack{}, "aws", "us-west-2", "sandbox", expectedBootstrapArgoApplications(testGitopsRevision), false, 0, 0,
	); err != nil {
		t.Fatalf("disabled AWS post-up convergence = %v, want nil", err)
	}
}

func TestWaitForAWSArgoApplicationsHealthy(t *testing.T) {
	tests := []struct {
		name      string
		responses []argoListResponse
		wantCalls int32
	}{
		{
			name: "already converged short-circuits",
			responses: []argoListResponse{{apps: []argoApplication{
				mkArgoApp("bootstrap", "Synced", "Healthy", ""),
				mkArgoApp("apps", "Synced", "Healthy", ""),
				mkArgoApp("platform", "Synced", "Healthy", ""),
				mkArgoApp("witself-server", "Synced", "Healthy", ""),
			}}},
			wantCalls: 1,
		},
		{
			name: "healthy subset cannot short circuit before required app converges",
			responses: []argoListResponse{
				{apps: []argoApplication{
					mkArgoApp("bootstrap", "Synced", "Healthy", ""),
					mkArgoApp("apps", "Synced", "Healthy", ""),
					mkArgoApp("witself-server", "Synced", "Healthy", ""),
				}},
				{apps: []argoApplication{
					mkArgoApp("bootstrap", "Synced", "Healthy", ""),
					mkArgoApp("apps", "Synced", "Healthy", ""),
					mkArgoApp("platform", "Synced", "Progressing", "platform update pending"),
					mkArgoApp("witself-server", "Synced", "Healthy", ""),
				}},
				{apps: []argoApplication{
					mkArgoApp("bootstrap", "Synced", "Healthy", ""),
					mkArgoApp("apps", "Synced", "Healthy", ""),
					mkArgoApp("platform", "Synced", "Healthy", ""),
					mkArgoApp("witself-server", "Synced", "Healthy", ""),
				}},
			},
			wantCalls: 3,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			lister := &fakeArgoLister{responses: tc.responses}
			var outputCalls, factoryCalls atomic.Int32
			outputs := auto.OutputMap{
				"eksCluster": {Value: "aws-sandbox-usw2-dev"},
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()

			err := waitForAWSArgoApplicationsHealthyWith(
				ctx,
				func(context.Context) (auto.OutputMap, error) {
					outputCalls.Add(1)
					return outputs, nil
				},
				func(_ context.Context, gotOutputs auto.OutputMap, region, profile string) (argoApplicationLister, string, error) {
					factoryCalls.Add(1)
					if outputString(gotOutputs, "eksCluster") != "aws-sandbox-usw2-dev" {
						t.Fatalf("AWS lister did not receive stack outputs: %#v", gotOutputs)
					}
					if region != "us-west-2" {
						t.Fatalf("region = %q, want us-west-2", region)
					}
					if profile != "sandbox" {
						t.Fatalf("profile = %q, want sandbox", profile)
					}
					return lister, "argocd-aws", nil
				},
				"us-west-2",
				"sandbox",
				expectedBootstrapArgoApplications(testGitopsRevision),
				time.Second,
				5*time.Millisecond,
			)
			if err != nil {
				t.Fatalf("waitForAWSArgoApplicationsHealthyWith: %v", err)
			}
			if got := outputCalls.Load(); got != 1 {
				t.Fatalf("stack output calls = %d, want 1", got)
			}
			if got := factoryCalls.Load(); got != 1 {
				t.Fatalf("AWS lister factory calls = %d, want 1", got)
			}
			if got := lister.calls.Load(); got != tc.wantCalls {
				t.Fatalf("Argo list calls = %d, want %d", got, tc.wantCalls)
			}
		})
	}
}

func TestNewAzureArgoListerFromKubeconfig(t *testing.T) {
	caData := testCertificateAuthorityData(t)
	raw := []byte(fmt.Sprintf(`apiVersion: v1
clusters:
- cluster:
    certificate-authority-data: %s
    server: https://aks.example.test:443
  name: cell
users:
- name: operator
  user:
    token: test-token
`, caData))

	lister, err := newAzureArgoListerFromKubeconfig(raw)
	if err != nil {
		t.Fatalf("newAzureArgoListerFromKubeconfig: %v", err)
	}
	if lister.baseURL != "https://aks.example.test:443" {
		t.Fatalf("baseURL = %q, want AKS server URL", lister.baseURL)
	}
	if lister.token != "test-token" {
		t.Fatalf("token = %q, want test-token", lister.token)
	}
}

func TestNewAzureArgoListerFromKubeconfigRequiresToken(t *testing.T) {
	caData := testCertificateAuthorityData(t)
	raw := []byte(fmt.Sprintf(`apiVersion: v1
clusters:
- cluster:
    certificate-authority-data: %s
    server: https://aks.example.test:443
  name: cell
users:
- name: operator
  user: {}
`, caData))

	_, err := newAzureArgoListerFromKubeconfig(raw)
	if err == nil {
		t.Fatal("expected missing token error")
	}
	if !strings.Contains(err.Error(), "no bearer token or client certificate") {
		t.Fatalf("error = %v, want missing bearer token", err)
	}
}

func mkArgoApp(name, syncStatus, healthStatus, healthMessage string) argoApplication {
	var app argoApplication
	app.Metadata.Name = name
	app.Spec.Sources = []argoApplicationSource{
		{TargetRevision: testGitopsRevision},
		{TargetRevision: testGitopsRevision},
	}
	app.Status.Sync.Status = syncStatus
	app.Status.Sync.ComparedTo.Sources = []argoApplicationSource{
		{TargetRevision: testGitopsRevision},
		{TargetRevision: testGitopsRevision},
	}
	app.Status.Health.Status = healthStatus
	app.Status.Health.Message = healthMessage
	return app
}

func healthyExpectedArgoApps() []argoApplication {
	return []argoApplication{
		mkArgoApp("bootstrap", "Synced", "Healthy", ""),
		mkArgoApp("apps", "Synced", "Healthy", ""),
		mkArgoApp("platform", "Synced", "Healthy", ""),
	}
}

type argoListResponse struct {
	apps []argoApplication
	err  error
}

type fakeArgoLister struct {
	responses []argoListResponse
	calls     atomic.Int32
}

func (f *fakeArgoLister) ListArgoApplications(context.Context, string) ([]argoApplication, error) {
	idx := int(f.calls.Add(1)) - 1
	if idx >= len(f.responses) {
		idx = len(f.responses) - 1
	}
	r := f.responses[idx]
	return r.apps, r.err
}

func testCertificateAuthorityData(t *testing.T) string {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	cert := srv.Certificate()
	if cert == nil {
		t.Fatal("test TLS server did not expose a certificate")
	}
	pemData := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw})
	return base64.StdEncoding.EncodeToString(pemData)
}

// TestParseEKSToken pins the get-token JSON extraction and guards both fields
// needed for proactive refresh.
func TestParseEKSToken(t *testing.T) {
	tok, err := parseEKSToken([]byte(`{"kind":"ExecCredential","status":{"token":"k8s-aws-v1.abc123","expirationTimestamp":"2026-07-11T00:00:00Z"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if tok.value != "k8s-aws-v1.abc123" {
		t.Fatalf("token = %q, want k8s-aws-v1.abc123", tok.value)
	}
	if want := time.Date(2026, 7, 11, 0, 0, 0, 0, time.UTC); !tok.expiresAt.Equal(want) {
		t.Fatalf("expiration = %s, want %s", tok.expiresAt, want)
	}
	if _, err := parseEKSToken([]byte(`{"status":{}}`)); err == nil {
		t.Fatal("an empty token must be an error")
	}
	if _, err := parseEKSToken([]byte(`{"status":{"token":"k8s-aws-v1.abc123"}}`)); err == nil {
		t.Fatal("a missing expiration timestamp must be an error")
	}
	if _, err := parseEKSToken([]byte("not json")); err == nil {
		t.Fatal("bad JSON must be an error")
	}
}

func TestAWSArgoPollingRefreshesTokenAcrossExpiry(t *testing.T) {
	start := time.Date(2026, 7, 11, 0, 0, 0, 0, time.UTC)
	var clockNanos atomic.Int64
	clockNanos.Store(start.UnixNano())
	now := func() time.Time { return time.Unix(0, clockNanos.Load()).UTC() }

	var providerCalls, requestCalls atomic.Int32
	provider := func(context.Context) (eksBearerToken, error) {
		call := providerCalls.Add(1)
		return eksBearerToken{
			value:     fmt.Sprintf("token-%d", call),
			expiresAt: now().Add(15 * time.Minute),
		}, nil
	}
	base := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		call := requestCalls.Add(1)
		if got, want := req.Header.Get("Authorization"), fmt.Sprintf("Bearer token-%d", call-1); got != want {
			t.Errorf("request %d Authorization = %q, want %q", call, got, want)
		}
		switch call {
		case 1:
			// Enter the one-minute refresh window before the next poll.
			clockNanos.Store(start.Add(14*time.Minute + 30*time.Second).UnixNano())
			return argoListHTTPResponse(t, []argoApplication{
				mkArgoApp("bootstrap", "Synced", "Healthy", ""),
				mkArgoApp("apps", "Synced", "Healthy", ""),
				mkArgoApp("witself-server", "Synced", "Healthy", ""),
			}), nil
		case 2:
			// The replacement was minted at +14m30s and expires at +29m30s.
			// Cross that expiration before the final convergence poll.
			clockNanos.Store(start.Add(29*time.Minute + 31*time.Second).UnixNano())
			return argoListHTTPResponse(t, []argoApplication{
				mkArgoApp("bootstrap", "Synced", "Healthy", ""),
				mkArgoApp("apps", "Synced", "Healthy", ""),
				mkArgoApp("platform", "Synced", "Progressing", "platform update pending"),
			}), nil
		case 3:
			return argoListHTTPResponse(t, healthyExpectedArgoApps()), nil
		default:
			t.Errorf("unexpected request %d", call)
			return argoListHTTPResponse(t, healthyExpectedArgoApps()), nil
		}
	})
	lister := &tokenArgoLister{
		baseURL: "https://eks.example.test",
		client: &http.Client{Transport: &refreshingEKSBearerTransport{
			base:          base,
			provider:      provider,
			now:           now,
			refreshBefore: awsEKSTokenRefreshBefore,
			token: eksBearerToken{
				value:     "token-0",
				expiresAt: start.Add(15 * time.Minute),
			},
		}},
	}

	err := waitForArgoApplicationsHealthy(
		context.Background(), lister, "argocd", expectedBootstrapArgoApplications(testGitopsRevision), time.Second, time.Millisecond,
	)
	if err != nil {
		t.Fatalf("waitForArgoApplicationsHealthy: %v", err)
	}
	if got := providerCalls.Load(); got != 2 {
		t.Fatalf("EKS token provider calls = %d, want 2", got)
	}
	if got := requestCalls.Load(); got != 3 {
		t.Fatalf("Argo list requests = %d, want 3", got)
	}
}

func TestAWSArgoListerRefreshesAndRetriesOnceOnUnauthorized(t *testing.T) {
	now := time.Date(2026, 7, 11, 0, 0, 0, 0, time.UTC)
	var providerCalls, requestCalls atomic.Int32
	provider := func(context.Context) (eksBearerToken, error) {
		call := providerCalls.Add(1)
		return eksBearerToken{
			value:     fmt.Sprintf("token-%d", call),
			expiresAt: now.Add(15 * time.Minute),
		}, nil
	}
	base := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		call := requestCalls.Add(1)
		if got, want := req.Header.Get("Authorization"), fmt.Sprintf("Bearer token-%d", call-1); got != want {
			t.Errorf("request %d Authorization = %q, want %q", call, got, want)
		}
		if call == 1 {
			return &http.Response{
				StatusCode: http.StatusUnauthorized,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader("expired bearer")),
				Request:    req,
			}, nil
		}
		return argoListHTTPResponse(t, healthyExpectedArgoApps()), nil
	})
	lister := &tokenArgoLister{
		baseURL: "https://eks.example.test",
		client: &http.Client{Transport: &refreshingEKSBearerTransport{
			base:          base,
			provider:      provider,
			now:           func() time.Time { return now },
			refreshBefore: awsEKSTokenRefreshBefore,
			token: eksBearerToken{
				value:     "token-0",
				expiresAt: now.Add(15 * time.Minute),
			},
		}},
	}

	apps, err := lister.ListArgoApplications(context.Background(), "argocd")
	if err != nil {
		t.Fatalf("ListArgoApplications: %v", err)
	}
	if len(apps) != len(healthyExpectedArgoApps()) {
		t.Fatalf("application count = %d, want %d", len(apps), len(healthyExpectedArgoApps()))
	}
	if got := providerCalls.Load(); got != 1 {
		t.Fatalf("EKS token provider calls = %d, want 1", got)
	}
	if got := requestCalls.Load(); got != 2 {
		t.Fatalf("HTTP requests = %d, want 2", got)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func argoListHTTPResponse(t *testing.T, apps []argoApplication) *http.Response {
	t.Helper()
	raw, err := json.Marshal(argoApplicationList{Items: apps})
	if err != nil {
		t.Fatalf("marshal Argo application list: %v", err)
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Body:       io.NopCloser(bytes.NewReader(raw)),
	}
}

// TestNewAWSArgoListerRequiresCluster pins the early guard: no
// eksCluster output means no probe (no SDK call attempted).
func TestNewAWSArgoListerRequiresCluster(t *testing.T) {
	if _, _, err := newAWSArgoListerFromOutputs(context.Background(), auto.OutputMap{}, "us-east-1", ""); err == nil {
		t.Fatal("missing eksCluster output must error before any AWS call")
	}
}
