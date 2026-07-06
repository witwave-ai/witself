package main

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"
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
