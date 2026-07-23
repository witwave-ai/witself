package client_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/witwave-ai/witself/internal/billing"
	"github.com/witwave-ai/witself/internal/billing/fake"
	"github.com/witwave-ai/witself/internal/billing/lifecycle"
	"github.com/witwave-ai/witself/internal/client"
	"github.com/witwave-ai/witself/internal/cpserver"
	"github.com/witwave-ai/witself/internal/plans"
)

type noopApplier struct{}

func (noopApplier) Apply(_ context.Context, _ string, request lifecycle.ApplyRequest) (lifecycle.ApplyAck, error) {
	return lifecycle.ApplyAck{Revision: request.Revision, Hash: request.Hash}, nil
}

// TestCLIPlanFlowAgainstCPServer wires the CP HTTP server, then drives the
// full plan flow through the CLIENT (the same functions the CLI verbs call).
// This is the belt-and-suspenders across the whole billing arc: catalog → CP
// → Manager → Store → client → CLI shape.
func TestCLIPlanFlowAgainstCPServer(t *testing.T) {
	catalog, err := plans.Load()
	if err != nil {
		t.Fatalf("plans.Load: %v", err)
	}
	f := fake.New(fake.Config{Prices: catalog.Prices()})
	providers := map[string]billing.Provider{"fake": f}
	m, err := lifecycle.NewManager(lifecycle.Config{
		Catalog: catalog, Providers: providers, Default: "fake",
		Store: lifecycle.NewMemStore(), Applier: noopApplier{},
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	mux := http.NewServeMux()
	if err := cpserver.Register(mux, cpserver.Config{
		Manager: m, Catalog: catalog, Providers: providers,
		Authenticate: func(_ context.Context, accountID, bearer string) (bool, error) {
			return bearer == "good" && accountID == "acct_1", nil
		},
	}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	ctx := context.Background()

	status, err := client.GetPlan(ctx, srv.URL, "acct_1", "good")
	if err != nil || status.Plan != "free" {
		t.Fatalf("GetPlan initial = %+v, %v; want free", status, err)
	}

	out, err := client.UpgradePlan(ctx, srv.URL, "acct_1", "good", "standard", "s@example.com")
	if err != nil || out.Kind != "done" || out.Plan != "standard" {
		t.Fatalf("UpgradePlan = %+v, %v; want done+standard", out, err)
	}

	status, err = client.GetPlan(ctx, srv.URL, "acct_1", "good")
	if err != nil || status.Plan != "standard" || status.Applied != "standard" {
		t.Fatalf("GetPlan after = %+v; want standard/standard", status)
	}

	if _, err := client.DowngradePlan(ctx, srv.URL, "acct_1", "good", "free", ""); err != nil {
		t.Fatalf("DowngradePlan: %v", err)
	}
	status, _ = client.GetPlan(ctx, srv.URL, "acct_1", "good")
	if status.Pending == nil || status.Pending.Kind != "downgrade" {
		t.Fatalf("status.Pending = %+v; want scheduled downgrade", status.Pending)
	}

	if err := client.CancelPlanChange(ctx, srv.URL, "acct_1", "good"); err != nil {
		t.Fatalf("CancelPlanChange: %v", err)
	}
	status, _ = client.GetPlan(ctx, srv.URL, "acct_1", "good")
	if status.Pending != nil {
		t.Fatalf("pending survived cancel: %+v", status.Pending)
	}

	// A refusal from the Manager surfaces through the client with the
	// message intact (409 → error text preserved).
	if _, err := client.UpgradePlan(ctx, srv.URL, "acct_1", "good", "standard", ""); err == nil ||
		err.Error() == "" {
		t.Fatalf("re-upgrade to same plan = %v; want the refusal message", err)
	}
}

func TestResolveAccountViaBridgeRequiresSecureCellEndpoint(t *testing.T) {
	tests := []struct {
		name     string
		endpoint string
		want     string
		wantErr  bool
	}{
		{
			name:     "https",
			endpoint: "https://cell.example.invalid/",
			want:     "https://cell.example.invalid",
		},
		{name: "plaintext", endpoint: "http://cell.example.invalid", wantErr: true},
		{
			name:     "userinfo",
			endpoint: "https://user:secret@cell.example.invalid",
			wantErr:  true,
		},
		{
			name:     "query",
			endpoint: "https://cell.example.invalid?token=secret",
			wantErr:  true,
		},
		{
			name:     "fragment",
			endpoint: "https://cell.example.invalid#fragment",
			wantErr:  true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet ||
					r.URL.Path != "/v1/internal/accounts/acct_1:resolve" {
					t.Fatalf("request = %s %s", r.Method, r.URL.RequestURI())
				}
				if got := r.Header.Get("Authorization"); got != "Bearer bridge-secret" {
					t.Fatalf("Authorization = %q", got)
				}
				w.Header().Set("Content-Type", "application/json")
				if err := json.NewEncoder(w).Encode(map[string]string{
					"schema_version": "witself.v0",
					"account_id":     "acct_1",
					"state":          "active",
					"cell":           "cell-a",
					"endpoint":       tc.endpoint,
				}); err != nil {
					t.Fatal(err)
				}
			}))
			t.Cleanup(srv.Close)

			got, err := client.ResolveAccountViaBridge(
				context.Background(), srv.URL, "bridge-secret", "acct_1")
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ResolveAccountViaBridge endpoint %q = %q; want error", tc.endpoint, got)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("ResolveAccountViaBridge = %q, %v; want %q", got, err, tc.want)
			}
		})
	}
}
