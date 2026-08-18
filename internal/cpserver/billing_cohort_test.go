package cpserver

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/billing"
	"github.com/witwave-ai/witself/internal/billing/lifecycle"
	"github.com/witwave-ai/witself/internal/plans"
)

var errBillingCohortProviderCalled = errors.New("billing cohort provider was called")

type billingCohortProviderSpy struct {
	calls atomic.Int64
}

func (p *billingCohortProviderSpy) called() error {
	p.calls.Add(1)
	return errBillingCohortProviderCalled
}

func (p *billingCohortProviderSpy) EnsureCustomer(
	context.Context, string, string,
) (string, error) {
	return "", p.called()
}

func (p *billingCohortProviderSpy) SetupLink(
	context.Context, string,
) (billing.Action, error) {
	return billing.Action{}, p.called()
}

func (p *billingCohortProviderSpy) SetupLinkIdempotent(
	context.Context, string, string,
) (billing.Action, error) {
	return billing.Action{}, p.called()
}

func (p *billingCohortProviderSpy) PortalLink(
	context.Context, string,
) (string, error) {
	return "", p.called()
}

func (p *billingCohortProviderSpy) Subscribe(
	context.Context, string, string,
) (billing.Action, error) {
	return billing.Action{}, p.called()
}

func (p *billingCohortProviderSpy) SubscribeIdempotent(
	context.Context, string, string, string,
) (billing.Action, error) {
	return billing.Action{}, p.called()
}

func (p *billingCohortProviderSpy) ScheduleDowngrade(
	context.Context, string, string,
) (time.Time, error) {
	return time.Time{}, p.called()
}

func (p *billingCohortProviderSpy) ScheduleDowngradeIdempotent(
	context.Context, string, string, string,
) (time.Time, error) {
	return time.Time{}, p.called()
}

func (p *billingCohortProviderSpy) SupportsDowngradeTarget(string) bool {
	p.calls.Add(1)
	return false
}

func (p *billingCohortProviderSpy) CancelPending(context.Context, string) error {
	return p.called()
}

func (p *billingCohortProviderSpy) CancelPendingIdempotent(
	context.Context, string, string,
) error {
	return p.called()
}

func (p *billingCohortProviderSpy) HandleWebhook(*http.Request) ([]billing.Event, error) {
	return nil, p.called()
}

func (p *billingCohortProviderSpy) RecordUsage(
	context.Context, string, string, int64, string,
) error {
	return p.called()
}

func (p *billingCohortProviderSpy) PaymentMethodOnFile(
	context.Context, string,
) (*billing.PaymentMethod, error) {
	return nil, p.called()
}

func (p *billingCohortProviderSpy) ListInvoices(
	context.Context, string,
) ([]billing.Invoice, error) {
	return nil, p.called()
}

func (p *billingCohortProviderSpy) ListPayments(
	context.Context, string,
) ([]billing.Payment, error) {
	return nil, p.called()
}

func (p *billingCohortProviderSpy) NextCharge(
	context.Context, string,
) (*billing.UpcomingCharge, error) {
	return nil, p.called()
}

var _ billing.Provider = (*billingCohortProviderSpy)(nil)
var _ billing.IdempotentSetupper = (*billingCohortProviderSpy)(nil)
var _ billing.IdempotentSubscriber = (*billingCohortProviderSpy)(nil)
var _ billing.IdempotentDowngrader = (*billingCohortProviderSpy)(nil)
var _ billing.DowngradeTargetChecker = (*billingCohortProviderSpy)(nil)
var _ billing.IdempotentPendingCanceller = (*billingCohortProviderSpy)(nil)

type billingCohortStoreSpy struct {
	delegate     *lifecycle.MemStore
	stateCalls   atomic.Int64
	receiptCalls atomic.Int64
}

func newBillingCohortStoreSpy() *billingCohortStoreSpy {
	return &billingCohortStoreSpy{delegate: lifecycle.NewMemStore()}
}

func (s *billingCohortStoreSpy) Get(
	ctx context.Context, accountID string,
) (lifecycle.Record, bool, error) {
	s.stateCalls.Add(1)
	return s.delegate.Get(ctx, accountID)
}

func (s *billingCohortStoreSpy) ByCustomer(
	ctx context.Context, provider, customerID string,
) (lifecycle.Record, bool, error) {
	s.stateCalls.Add(1)
	return s.delegate.ByCustomer(ctx, provider, customerID)
}

func (s *billingCohortStoreSpy) Put(ctx context.Context, record lifecycle.Record) error {
	s.stateCalls.Add(1)
	return s.delegate.Put(ctx, record)
}

func (s *billingCohortStoreSpy) List(ctx context.Context) ([]lifecycle.Record, error) {
	s.stateCalls.Add(1)
	return s.delegate.List(ctx)
}

func (s *billingCohortStoreSpy) ClaimBillingMutationAccount(
	ctx context.Context,
	accountID, operationID string,
	expectedGeneration int64,
	claimToken string,
	now, leaseExpiresAt time.Time,
) (lifecycle.BillingAccountMutationLease, bool, error) {
	s.receiptCalls.Add(1)
	return s.delegate.ClaimBillingMutationAccount(
		ctx, accountID, operationID, expectedGeneration,
		claimToken, now, leaseExpiresAt)
}

func (s *billingCohortStoreSpy) ReleaseBillingMutationAccount(
	ctx context.Context,
	lease lifecycle.BillingAccountMutationLease,
	releasedAt time.Time,
) error {
	s.receiptCalls.Add(1)
	return s.delegate.ReleaseBillingMutationAccount(ctx, lease, releasedAt)
}

func (s *billingCohortStoreSpy) GetBillingMutation(
	ctx context.Context, operationID string,
) (lifecycle.BillingMutationReceipt, bool, error) {
	s.receiptCalls.Add(1)
	return s.delegate.GetBillingMutation(ctx, operationID)
}

func (s *billingCohortStoreSpy) ReceiveBillingMutation(
	ctx context.Context,
	receipt lifecycle.BillingMutationReceipt,
) (lifecycle.BillingMutationReceipt, bool, error) {
	s.receiptCalls.Add(1)
	return s.delegate.ReceiveBillingMutation(ctx, receipt)
}

func (s *billingCohortStoreSpy) ClaimBillingMutation(
	ctx context.Context,
	receipt lifecycle.BillingMutationReceipt,
	claimToken string,
	now, leaseExpiresAt time.Time,
) (lifecycle.BillingMutationReceipt, bool, error) {
	s.receiptCalls.Add(1)
	return s.delegate.ClaimBillingMutation(
		ctx, receipt, claimToken, now, leaseExpiresAt)
}

func (s *billingCohortStoreSpy) CompleteBillingMutation(
	ctx context.Context,
	receipt lifecycle.BillingMutationReceipt,
	result lifecycle.BillingMutationResult,
	completedAt time.Time,
) (lifecycle.BillingMutationReceipt, error) {
	s.receiptCalls.Add(1)
	return s.delegate.CompleteBillingMutation(ctx, receipt, result, completedAt)
}

func (s *billingCohortStoreSpy) SupersedeBillingMutation(
	ctx context.Context,
	receipt lifecycle.BillingMutationReceipt,
	supersededAt time.Time,
) (lifecycle.BillingMutationReceipt, error) {
	s.receiptCalls.Add(1)
	return s.delegate.SupersedeBillingMutation(ctx, receipt, supersededAt)
}

func (s *billingCohortStoreSpy) ReleaseBillingMutation(
	ctx context.Context,
	receipt lifecycle.BillingMutationReceipt,
	releasedAt time.Time,
) error {
	s.receiptCalls.Add(1)
	return s.delegate.ReleaseBillingMutation(ctx, receipt, releasedAt)
}

func (s *billingCohortStoreSpy) PendingBillingMutations(
	ctx context.Context, limit int,
) (lifecycle.BillingMutationPendingBatch, error) {
	s.receiptCalls.Add(1)
	return s.delegate.PendingBillingMutations(ctx, limit)
}

var _ lifecycle.Store = (*billingCohortStoreSpy)(nil)
var _ lifecycle.BillingMutationStore = (*billingCohortStoreSpy)(nil)

type billingCohortHarness struct {
	server    *httptest.Server
	store     *billingCohortStoreSpy
	provider  *billingCohortProviderSpy
	authCalls atomic.Int64
}

func newBillingCohortHarness(
	t *testing.T,
	gate BillingMutationGateFunc,
) *billingCohortHarness {
	t.Helper()
	catalog, err := plans.Load()
	if err != nil {
		t.Fatal(err)
	}
	store := newBillingCohortStoreSpy()
	provider := &billingCohortProviderSpy{}
	providers := map[string]billing.Provider{"spy": provider}
	manager, err := lifecycle.NewManager(lifecycle.Config{
		Catalog: catalog, Providers: providers, Default: "spy",
		Store: store, Applier: noopApplier{},
	})
	if err != nil {
		t.Fatal(err)
	}
	h := &billingCohortHarness{store: store, provider: provider}
	mux := http.NewServeMux()
	if err := Register(mux, Config{
		Manager: manager, Catalog: catalog, Providers: providers,
		BillingMutationGate: gate,
		Authenticate: func(
			_ context.Context,
			accountID, bearer string,
			permission AccountPermission,
		) (AccountAccess, bool, error) {
			h.authCalls.Add(1)
			ok := accountID == "acct_1" && bearer == "owner"
			return AccountAccess{
				ActorID: "opr_owner", Role: "account_owner", Permission: permission,
			}, ok, nil
		},
	}); err != nil {
		t.Fatal(err)
	}
	h.server = httptest.NewServer(mux)
	t.Cleanup(h.server.Close)
	return h
}

func TestBillingMutationCohortDenialPrecedesEveryStateBoundary(t *testing.T) {
	var gateCalls atomic.Int64
	h := newBillingCohortHarness(t, func(_ context.Context, accountID string) (bool, error) {
		gateCalls.Add(1)
		if accountID != "acct_1" {
			t.Errorf("gate account = %q", accountID)
		}
		return false, nil
	})

	status, _ := billingHTTPRequest(t, h.server, http.MethodPost,
		"/v1/accounts/acct_1/billing:portal", "", "", "")
	if status != http.StatusUnauthorized || gateCalls.Load() != 0 {
		t.Fatalf("unauthenticated request = %d, gate calls = %d", status, gateCalls.Load())
	}

	requests := []struct {
		name string
		path string
		body string
		key  string
	}{
		{
			name: "preview", path: "/v1/accounts/acct_1/billing:preview",
			body: `{"operation":"plan_upgrade","plan":"standard","reason":"sandbox preview"}`,
		},
		{
			name: "setup", path: "/v1/accounts/acct_1/billing:setup",
			body: `{"reason":"sandbox setup","confirmed":true}`, key: "cohort-setup-request",
		},
		{name: "portal", path: "/v1/accounts/acct_1/billing:portal"},
		{
			name: "upgrade", path: "/v1/accounts/acct_1/plan:upgrade",
			body: `{"plan":"standard","reason":"sandbox upgrade","confirmed":true}`,
			key:  "cohort-upgrade-request",
		},
		{
			name: "downgrade", path: "/v1/accounts/acct_1/plan:downgrade",
			body: `{"plan":"free","reason":"sandbox downgrade","confirmed":true}`,
			key:  "cohort-downgrade-request",
		},
		{
			name: "cancel", path: "/v1/accounts/acct_1/plan:cancel",
			body: `{"reason":"sandbox cancel","confirmed":true}`, key: "cohort-cancel-request",
		},
	}
	for _, request := range requests {
		t.Run(request.name, func(t *testing.T) {
			status, raw := billingHTTPRequestWithKey(
				t, h.server, http.MethodPost, request.path,
				"owner", "owner@example.com", request.body, request.key)
			if status != http.StatusForbidden {
				t.Fatalf("status = %d body=%s", status, raw)
			}
			doc := decodeBillingHTTPDocument(t, raw)
			if len(doc) != 5 || doc["schema_version"] != "witself.v0" ||
				doc["code"] != "feature_not_enabled" || doc["feature"] != "billing" ||
				doc["error"] != "Sorry, this feature is not enabled on this account." ||
				doc["retryable"] != false {
				t.Fatalf("denial = %v", doc)
			}
		})
	}

	if got := gateCalls.Load(); got != int64(len(requests)) {
		t.Fatalf("gate calls = %d, want %d", got, len(requests))
	}
	if got := h.authCalls.Load(); got != int64(len(requests)) {
		t.Fatalf("authentication calls = %d, want %d", got, len(requests))
	}
	if got := h.store.stateCalls.Load(); got != 0 {
		t.Fatalf("lifecycle state calls after cohort denial = %d", got)
	}
	if got := h.store.receiptCalls.Load(); got != 0 {
		t.Fatalf("durable receipt calls after cohort denial = %d", got)
	}
	if got := h.provider.calls.Load(); got != 0 {
		t.Fatalf("provider/customer calls after cohort denial = %d", got)
	}
}

func TestBillingMutationCohortErrorFailsClosedWithoutLeaking(t *testing.T) {
	var gateCalls atomic.Int64
	h := newBillingCohortHarness(t, func(context.Context, string) (bool, error) {
		gateCalls.Add(1)
		return false, errors.New("allowlist backend secret-cohort-host failed")
	})
	status, raw := billingHTTPRequest(
		t, h.server, http.MethodPost,
		"/v1/accounts/acct_1/billing:portal", "owner", "", "")
	if status != http.StatusServiceUnavailable {
		t.Fatalf("status = %d body=%s", status, raw)
	}
	doc := decodeBillingHTTPDocument(t, raw)
	if doc["code"] != "feature_availability_unavailable" ||
		doc["retryable"] != true || strings.Contains(string(raw), "secret-cohort-host") {
		t.Fatalf("gate error response = %v", doc)
	}
	if gateCalls.Load() != 1 || h.store.stateCalls.Load() != 0 ||
		h.store.receiptCalls.Load() != 0 || h.provider.calls.Load() != 0 {
		t.Fatalf("gate=%d state=%d receipt=%d provider=%d",
			gateCalls.Load(), h.store.stateCalls.Load(),
			h.store.receiptCalls.Load(), h.provider.calls.Load())
	}
}

func TestBillingMutationCohortKeepsReadsAndProjectsAvailability(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{name: "settled denial"},
		{name: "indeterminate denial", err: errors.New("cohort lookup unavailable")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var gateCalls atomic.Int64
			h := newBillingCohortHarness(t, func(context.Context, string) (bool, error) {
				gateCalls.Add(1)
				return false, tc.err
			})
			for _, path := range []string{
				"/v1/accounts/acct_1/plan",
				"/v1/accounts/acct_1/billing",
				"/v1/accounts/acct_1/billing/invoices",
				"/v1/accounts/acct_1/billing/payments",
			} {
				status, raw := billingHTTPRequest(
					t, h.server, http.MethodGet, path, "owner", "", "")
				if status != http.StatusOK {
					t.Fatalf("GET %s = %d %s", path, status, raw)
				}
				if path == "/v1/accounts/acct_1/plan" || path == "/v1/accounts/acct_1/billing" {
					doc := decodeBillingHTTPDocument(t, raw)
					if doc["billing_available"] != false {
						t.Fatalf("GET %s billing_available = %v", path, doc["billing_available"])
					}
				}
			}
			if gateCalls.Load() != 2 {
				t.Fatalf("availability projection gate calls = %d, want 2", gateCalls.Load())
			}
		})
	}
}

func TestBillingMutationCohortAllowsEnabledPreview(t *testing.T) {
	var gateCalls atomic.Int64
	h := newBillingCohortHarness(t, func(context.Context, string) (bool, error) {
		gateCalls.Add(1)
		return true, nil
	})
	status, raw := billingHTTPRequest(
		t, h.server, http.MethodPost,
		"/v1/accounts/acct_1/billing:preview", "owner", "",
		`{"operation":"plan_upgrade","plan":"standard","reason":"allowed sandbox preview"}`)
	if status != http.StatusOK {
		t.Fatalf("enabled preview = %d %s", status, raw)
	}
	if doc := decodeBillingHTTPDocument(t, raw); doc["allowed"] != true {
		t.Fatalf("enabled preview = %v", doc)
	}
	if gateCalls.Load() != 1 || h.store.stateCalls.Load() == 0 ||
		h.store.receiptCalls.Load() != 0 || h.provider.calls.Load() != 0 {
		t.Fatalf("gate=%d state=%d receipt=%d provider=%d",
			gateCalls.Load(), h.store.stateCalls.Load(),
			h.store.receiptCalls.Load(), h.provider.calls.Load())
	}
}
