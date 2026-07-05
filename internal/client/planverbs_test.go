package client_test

import (
	"context"
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

func (noopApplier) Apply(context.Context, string, string, map[string]int64, []string) error {
	return nil
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
