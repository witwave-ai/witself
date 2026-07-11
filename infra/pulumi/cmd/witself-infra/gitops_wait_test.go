package main

import (
	"context"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pulumi/pulumi/sdk/v3/go/auto"
)

func TestArgoApplicationsReady(t *testing.T) {
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
				mkArgoApp("witself-server", "Synced", "Healthy", ""),
			},
			ready: true,
		},
		{
			name: "parent progressing reports culprit",
			apps: []argoApplication{
				mkArgoApp("bootstrap", "Synced", "Healthy", ""),
				mkArgoApp("apps", "Synced", "Progressing", "waiting for ManagedCertificate/witself-api"),
				mkArgoApp("witself-server", "Synced", "Healthy", ""),
			},
			ready:   false,
			wantWhy: "apps Synced/Progressing: waiting for ManagedCertificate/witself-api",
		},
		{
			name: "out of sync child reports sync and health",
			apps: []argoApplication{
				mkArgoApp("external-dns", "OutOfSync", "Healthy", ""),
			},
			ready:   false,
			wantWhy: "external-dns OutOfSync/Healthy",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ready, why := argoApplicationsReady(tc.apps)
			if ready != tc.ready {
				t.Fatalf("ready = %t, want %t (why %q)", ready, tc.ready, why)
			}
			if tc.wantWhy != "" && !strings.Contains(why, tc.wantWhy) {
				t.Fatalf("why = %q, want substring %q", why, tc.wantWhy)
			}
		})
	}
}

func TestWaitForArgoApplicationsHealthy(t *testing.T) {
	t.Run("waits through progressing parent", func(t *testing.T) {
		lister := &fakeArgoLister{responses: []argoListResponse{
			{apps: []argoApplication{
				mkArgoApp("apps", "Synced", "Progressing", "certificate provisioning"),
				mkArgoApp("witself-server", "Synced", "Healthy", ""),
			}},
			{apps: []argoApplication{
				mkArgoApp("apps", "Synced", "Healthy", ""),
				mkArgoApp("witself-server", "Synced", "Healthy", ""),
			}},
		}}
		if err := waitForArgoApplicationsHealthy(context.Background(), lister, "argocd", time.Second, 5*time.Millisecond); err != nil {
			t.Fatalf("waitForArgoApplicationsHealthy: %v", err)
		}
		if got := lister.calls.Load(); got != 2 {
			t.Fatalf("calls = %d, want 2", got)
		}
	})

	t.Run("timeout includes last diagnostic", func(t *testing.T) {
		lister := &fakeArgoLister{responses: []argoListResponse{
			{apps: []argoApplication{
				mkArgoApp("apps", "Synced", "Progressing", "certificate provisioning"),
			}},
		}}
		err := waitForArgoApplicationsHealthy(context.Background(), lister, "argocd", 20*time.Millisecond, 5*time.Millisecond)
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
		err := waitForArgoApplicationsHealthy(context.Background(), lister, "argocd", time.Hour, time.Hour)
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
				mkArgoApp("apps", "Synced", "Healthy", ""),
			}},
		}}
		if err := waitForArgoApplicationsHealthy(context.Background(), lister, "argocd", time.Second, 5*time.Millisecond); err != nil {
			t.Fatalf("remote flap should heal by retrying: %v", err)
		}
		if got := lister.calls.Load(); got != 2 {
			t.Fatalf("calls = %d, want 2", got)
		}
	})
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
	if !strings.Contains(err.Error(), "no bearer token") {
		t.Fatalf("error = %v, want missing bearer token", err)
	}
}

func mkArgoApp(name, syncStatus, healthStatus, healthMessage string) argoApplication {
	var app argoApplication
	app.Metadata.Name = name
	app.Status.Sync.Status = syncStatus
	app.Status.Health.Status = healthStatus
	app.Status.Health.Message = healthMessage
	return app
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

// TestParseEKSToken pins the get-token JSON extraction and the
// empty-token guard.
func TestParseEKSToken(t *testing.T) {
	tok, err := parseEKSToken([]byte(`{"kind":"ExecCredential","status":{"token":"k8s-aws-v1.abc123","expirationTimestamp":"2026-07-11T00:00:00Z"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if tok != "k8s-aws-v1.abc123" {
		t.Fatalf("token = %q, want k8s-aws-v1.abc123", tok)
	}
	if _, err := parseEKSToken([]byte(`{"status":{}}`)); err == nil {
		t.Fatal("an empty token must be an error")
	}
	if _, err := parseEKSToken([]byte("not json")); err == nil {
		t.Fatal("bad JSON must be an error")
	}
}

// TestNewAWSArgoListerRequiresCluster pins the early guard: no
// eksCluster output means no probe (no SDK call attempted).
func TestNewAWSArgoListerRequiresCluster(t *testing.T) {
	if _, _, err := newAWSArgoListerFromOutputs(context.Background(), auto.OutputMap{}, "us-east-1", ""); err == nil {
		t.Fatal("missing eksCluster output must error before any AWS call")
	}
}
