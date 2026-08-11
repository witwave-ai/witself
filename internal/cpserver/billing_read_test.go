package cpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/billing"
	"github.com/witwave-ai/witself/internal/billing/fake"
	"github.com/witwave-ai/witself/internal/billing/lifecycle"
	"github.com/witwave-ai/witself/internal/client"
	"github.com/witwave-ai/witself/internal/plans"
)

type billingHTTPHarness struct {
	server  *httptest.Server
	manager *lifecycle.Manager
	store   lifecycle.Store
}

func newBillingHTTPHarness(
	t *testing.T,
	provider billing.Provider,
	store lifecycle.Store,
	now func() time.Time,
) *billingHTTPHarness {
	t.Helper()
	catalog, err := plans.Load()
	if err != nil {
		t.Fatal(err)
	}
	if store == nil {
		store = lifecycle.NewMemStore()
	}
	providers := map[string]billing.Provider{}
	defaultProvider := ""
	if provider != nil {
		providers["billing"] = provider
		defaultProvider = "billing"
	}
	manager, err := lifecycle.NewManager(lifecycle.Config{
		Catalog: catalog, Providers: providers, Default: defaultProvider,
		Store: store, Applier: noopApplier{}, Now: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	if err := Register(mux, Config{
		Manager: manager, Catalog: catalog, Providers: providers,
		Authenticate: func(_ context.Context, accountID, bearer string, permission AccountPermission) (AccountAccess, bool, error) {
			ok := accountID == "acct_1" && bearer == "owner"
			return AccountAccess{ActorID: "opr_owner", Role: "account_owner", Permission: permission}, ok, nil
		},
	}); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return &billingHTTPHarness{server: server, manager: manager, store: store}
}

func billingHTTPRequest(
	t *testing.T,
	server *httptest.Server,
	method, path, bearer, email, body string,
) (int, []byte) {
	return billingHTTPRequestWithKey(
		t, server, method, path, bearer, email, body, "")
}

func billingHTTPRequestWithKey(
	t *testing.T,
	server *httptest.Server,
	method, path, bearer, email, body, idempotencyKey string,
) (int, []byte) {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, server.URL+path, reader)
	if err != nil {
		t.Fatal(err)
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	if email != "" {
		req.Header.Set("X-Witself-Email", email)
	}
	if idempotencyKey != "" {
		req.Header.Set("Idempotency-Key", idempotencyKey)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	raw, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return response.StatusCode, raw
}

func decodeBillingHTTPDocument(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode %q: %v", raw, err)
	}
	return out
}

func putBillingHTTPRecord(t *testing.T, store lifecycle.Store, record lifecycle.Record) {
	t.Helper()
	if record.Entitled == "" {
		record.Entitled = plans.Free
	}
	if record.Applied == "" {
		record.Applied = plans.Free
	}
	if err := store.Put(context.Background(), record); err != nil {
		t.Fatal(err)
	}
}

func TestBillingHTTPReadsAreAuthenticatedAndProviderlessSideEffectFree(t *testing.T) {
	store := lifecycle.NewMemStore()
	h := newBillingHTTPHarness(t, nil, store, nil)
	readPaths := []string{
		"/v1/accounts/acct_1/billing",
		"/v1/accounts/acct_1/billing/invoices",
		"/v1/accounts/acct_1/billing/payments",
	}
	for _, path := range readPaths {
		if status, _ := billingHTTPRequest(t, h.server, http.MethodGet, path, "", "", ""); status != http.StatusUnauthorized {
			t.Errorf("unauthenticated %s = %d", path, status)
		}
		if status, _ := billingHTTPRequest(t, h.server, http.MethodGet, path, "wrong", "", ""); status != http.StatusForbidden {
			t.Errorf("wrong token %s = %d", path, status)
		}
		wrongAccount := strings.Replace(path, "acct_1", "acct_2", 1)
		if status, _ := billingHTTPRequest(t, h.server, http.MethodGet, wrongAccount, "owner", "", ""); status != http.StatusForbidden {
			t.Errorf("wrong account %s = %d", path, status)
		}
	}

	status, raw := billingHTTPRequest(t, h.server, http.MethodGet,
		"/v1/accounts/acct_1/billing", "owner", "owner@example.com", "")
	if status != http.StatusOK {
		t.Fatalf("summary = %d %s", status, raw)
	}
	doc := decodeBillingHTTPDocument(t, raw)
	if doc["schema_version"] != "witself.v0" || doc["account_id"] != "acct_1" ||
		doc["billing_available"] != false || doc["configured"] != false ||
		doc["subscription_status"] != "none" || doc["billing_plan"] != "free" ||
		doc["effective_plan"] != "free" || doc["applied_plan"] != "free" ||
		doc["payment_method"] != nil || doc["next_charge"] != nil {
		t.Fatalf("providerless summary = %v", doc)
	}
	for _, path := range readPaths[1:] {
		status, raw = billingHTTPRequest(t, h.server, http.MethodGet, path, "owner", "", "")
		if status != http.StatusOK {
			t.Fatalf("%s = %d %s", path, status, raw)
		}
		collection := decodeBillingHTTPDocument(t, raw)
		key := "invoices"
		if strings.HasSuffix(path, "payments") {
			key = "payments"
		}
		if values, ok := collection[key].([]any); !ok || len(values) != 0 {
			t.Fatalf("%s = %v", path, collection)
		}
	}
	if _, exists, err := store.Get(context.Background(), "acct_1"); err != nil || exists {
		t.Fatalf("providerless reads persisted lifecycle record: exists=%t err=%v", exists, err)
	}
	status, raw = billingHTTPRequest(t, h.server, http.MethodPost,
		"/v1/accounts/acct_1/billing:setup", "owner", "", "")
	if status != http.StatusBadRequest {
		t.Fatalf("providerless invalid setup envelope = %d %s; want 400", status, raw)
	}
	for _, path := range []string{
		"/v1/accounts/acct_1/billing:setup",
		"/v1/accounts/acct_1/billing:portal",
	} {
		var status int
		var raw []byte
		if strings.HasSuffix(path, ":setup") {
			status, raw = billingHTTPRequestWithKey(
				t, h.server, http.MethodPost, path, "owner", "",
				`{"reason":"Check providerless setup","confirmed":true}`,
				"billing-http-providerless-setup")
		} else {
			status, raw = billingHTTPRequest(
				t, h.server, http.MethodPost, path, "owner", "", "")
		}
		if status != http.StatusNotImplemented {
			t.Errorf("providerless mutation %s = %d; want 501", path, status)
			continue
		}
		doc := decodeBillingHTTPDocument(t, raw)
		if doc["code"] != "unsupported_operation" || doc["retryable"] != false ||
			!strings.Contains(doc["error"].(string), "not supported") {
			t.Errorf("providerless mutation %s = %v", path, doc)
		}
	}
}

func TestBillingHTTPFakeProviderEndToEnd(t *testing.T) {
	catalog, err := plans.Load()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 10, 20, 0, 0, 0, time.UTC)
	provider := fake.New(fake.Config{Prices: catalog.Prices(), Now: func() time.Time { return now }})
	h := newBillingHTTPHarness(t, provider, nil, func() time.Time { return now })
	ctx := context.Background()

	req, err := http.NewRequest(http.MethodGet,
		h.server.URL+"/v1/accounts/acct_1/billing", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer owner")
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if got := response.Header.Get("Cache-Control"); got != "no-store" {
		_ = response.Body.Close()
		t.Fatalf("billing Cache-Control = %q", got)
	}
	_ = response.Body.Close()

	setup, err := client.CreateBillingSetup(
		ctx, h.server.URL, "acct_1", "owner", "owner@example.com",
		client.BillingMutationOptions{
			Reason: "Add a payment method", Confirmed: true,
			IdempotencyKey: "billing-http-setup",
		})
	if err != nil || setup.Kind != "done" || setup.URL != "" {
		t.Fatalf("setup = %+v, %v", setup, err)
	}
	upgrade, err := client.UpgradePlan(
		ctx, h.server.URL, "acct_1", "owner", "standard", "owner@example.com",
		client.BillingMutationOptions{
			Reason: "Move to Professional", Confirmed: true,
			IdempotencyKey: "billing-http-upgrade",
		})
	if err != nil || upgrade.Kind != "done" {
		t.Fatalf("upgrade = %+v, %v", upgrade, err)
	}

	summary, err := client.GetBillingSummary(ctx, h.server.URL, "acct_1", "owner")
	if err != nil || !summary.BillingAvailable || !summary.Configured ||
		summary.SubscriptionStatus != "active" || summary.BillingPlan != "standard" ||
		summary.BillingPlanName != "Professional" || summary.EffectivePlan != "standard" ||
		summary.AppliedPlan != "standard" || summary.EntitledAt == nil ||
		summary.PaymentMethod == nil || summary.PaymentMethod.Label != "visa ****4242" ||
		summary.NextCharge == nil || summary.NextCharge.AmountCents != 3000 ||
		summary.NextCharge.Currency != "usd" {
		t.Fatalf("summary = %+v, %v", summary, err)
	}
	invoices, err := client.GetBillingInvoices(ctx, h.server.URL, "acct_1", "owner")
	if err != nil || len(invoices.Invoices) != 1 || invoices.Invoices[0].AmountCents != 3000 ||
		!strings.HasPrefix(invoices.Invoices[0].HostedURL, "https://") {
		t.Fatalf("invoices = %+v, %v", invoices, err)
	}
	payments, err := client.GetBillingPayments(ctx, h.server.URL, "acct_1", "owner")
	if err != nil || len(payments.Payments) != 1 || payments.Payments[0].Status != "succeeded" ||
		!strings.HasPrefix(payments.Payments[0].ReceiptURL, "https://") {
		t.Fatalf("payments = %+v, %v", payments, err)
	}
	portal, err := client.CreateBillingPortal(ctx, h.server.URL, "acct_1", "owner")
	if err != nil || portal.Kind != "action" || !strings.HasPrefix(portal.URL, "https://") {
		t.Fatalf("portal = %+v, %v", portal, err)
	}

	status, raw := billingHTTPRequest(t, h.server, http.MethodGet,
		"/v1/accounts/acct_1/billing", "owner", "", "")
	if status != http.StatusOK || strings.Contains(string(raw), "customer_id") ||
		strings.Contains(string(raw), "customer_private") || strings.Contains(string(raw), `"provider"`) {
		t.Fatalf("summary leaked provider authority: %d %s", status, raw)
	}
}

func TestBillingHTTPSummaryKeepsBillingAndEffectivePlansDistinct(t *testing.T) {
	provider := newBillingHTTPProviderStub()
	store := lifecycle.NewMemStore()
	putBillingHTTPRecord(t, store, lifecycle.Record{
		AccountID: "acct_1", Provider: "billing", CustomerID: "customer_private",
		Entitled: plans.Free, Applied: "enterprise",
		PlanOverride: &lifecycle.AccountPlanOverride{Plan: "enterprise"},
	})
	h := newBillingHTTPHarness(t, provider, store, nil)
	summary, err := client.GetBillingSummary(
		context.Background(), h.server.URL, "acct_1", "owner")
	if err != nil || !summary.Configured || summary.SubscriptionStatus != "none" ||
		summary.BillingPlan != "free" || summary.BillingPlanName != "Personal" ||
		summary.EffectivePlan != "enterprise" || summary.EffectivePlanName != "Enterprise" ||
		summary.AppliedPlan != "enterprise" || summary.EntitledAt != nil {
		t.Fatalf("override summary = %+v, %v", summary, err)
	}
}

func TestBillingHTTPSummaryStatusesAndSafePendingProjection(t *testing.T) {
	now := time.Date(2026, 8, 10, 21, 0, 0, 0, time.UTC)
	provider := newBillingHTTPProviderStub()
	for _, tc := range []struct {
		name   string
		record lifecycle.Record
		want   string
	}{
		{
			name: "pending upgrade",
			record: lifecycle.Record{
				Entitled: plans.Free, Applied: plans.Free,
				Pending: &lifecycle.Pending{
					Kind: lifecycle.PendingUpgrade, Plan: "standard",
					URL: "https://billing.example/checkout/session", Requested: now,
				},
			},
			want: "pending",
		},
		{
			name: "active with pending downgrade",
			record: lifecycle.Record{
				Entitled: "standard", Applied: "standard", EntitledAt: now,
				Pending: &lifecycle.Pending{
					Kind: lifecycle.PendingDowngrade, Plan: plans.Free,
					Requested: now, Effective: now.Add(30 * 24 * time.Hour),
				},
			},
			want: "active",
		},
		{
			name: "past due takes precedence",
			record: lifecycle.Record{
				Entitled: "standard", Applied: "standard", EntitledAt: now,
				PastDueSince: func() *time.Time { value := now; return &value }(),
			},
			want: "past_due",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := lifecycle.NewMemStore()
			tc.record.AccountID = "acct_1"
			tc.record.Provider = "billing"
			tc.record.CustomerID = "customer_private"
			putBillingHTTPRecord(t, store, tc.record)
			h := newBillingHTTPHarness(t, provider, store, func() time.Time { return now })
			summary, err := client.GetBillingSummary(
				context.Background(), h.server.URL, "acct_1", "owner")
			if err != nil || summary.SubscriptionStatus != tc.want {
				t.Fatalf("summary = %+v, %v; want %s", summary, err, tc.want)
			}
			if tc.want == "pending" && (summary.Pending == nil ||
				summary.Pending.PlanName != "Professional" ||
				summary.Pending.URL != "https://billing.example/checkout/session") {
				t.Fatalf("pending = %+v", summary.Pending)
			}
		})
	}
}

func TestBillingHTTPAuthIsolatesReadAndMutationRoutes(t *testing.T) {
	provider := newBillingHTTPProviderStub()
	store := lifecycle.NewMemStore()
	h := newBillingHTTPHarness(t, provider, store, nil)
	paths := []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/v1/accounts/acct_1/billing"},
		{http.MethodGet, "/v1/accounts/acct_1/billing/invoices"},
		{http.MethodGet, "/v1/accounts/acct_1/billing/payments"},
		{http.MethodPost, "/v1/accounts/acct_1/billing:setup"},
		{http.MethodPost, "/v1/accounts/acct_1/billing:portal"},
	}
	for _, route := range paths {
		if status, _ := billingHTTPRequest(t, h.server, route.method, route.path, "", "", ""); status != http.StatusUnauthorized {
			t.Errorf("unauthenticated %s = %d", route.path, status)
		}
		wrongAccount := strings.Replace(route.path, "acct_1", "acct_2", 1)
		if status, _ := billingHTTPRequest(t, h.server, route.method, wrongAccount, "owner", "", ""); status != http.StatusForbidden {
			t.Errorf("wrong account %s = %d", route.path, status)
		}
	}
	if _, exists, err := store.Get(context.Background(), "acct_1"); err != nil || exists {
		t.Fatalf("refused requests changed state: exists=%t err=%v", exists, err)
	}
}

func TestBillingHTTPProviderFailuresAndRefusalsAreSafelyMapped(t *testing.T) {
	privateProviderError := errors.New("provider secret endpoint customer_private")
	provider := newBillingHTTPProviderStub()
	provider.paymentMethodErr = privateProviderError
	provider.invoicesErr = privateProviderError
	provider.paymentsErr = privateProviderError
	provider.portalErr = privateProviderError
	store := lifecycle.NewMemStore()
	putBillingHTTPRecord(t, store, lifecycle.Record{
		AccountID: "acct_1", Provider: "billing", CustomerID: "customer_private",
		Entitled: "standard", Applied: "standard",
	})
	h := newBillingHTTPHarness(t, provider, store, nil)
	for _, route := range []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/v1/accounts/acct_1/billing"},
		{http.MethodGet, "/v1/accounts/acct_1/billing/invoices"},
		{http.MethodGet, "/v1/accounts/acct_1/billing/payments"},
		{http.MethodPost, "/v1/accounts/acct_1/billing:portal"},
	} {
		status, raw := billingHTTPRequest(t, h.server, route.method, route.path, "owner", "", "")
		if status != http.StatusBadGateway ||
			!strings.Contains(string(raw), "billing provider unavailable") ||
			strings.Contains(string(raw), privateProviderError.Error()) ||
			strings.Contains(string(raw), "customer_private") {
			t.Errorf("%s = %d %s", route.path, status, raw)
		}
	}
	setupProvider := newBillingHTTPProviderStub()
	setupProvider.ensureErr = privateProviderError
	setupHarness := newBillingHTTPHarness(t, setupProvider, nil, nil)
	status, raw := billingHTTPRequestWithKey(t, setupHarness.server, http.MethodPost,
		"/v1/accounts/acct_1/billing:setup", "owner", "owner@example.com",
		`{"reason":"Exercise provider failure","confirmed":true}`,
		"billing-http-provider-failure")
	if status != http.StatusBadGateway ||
		!strings.Contains(string(raw), "billing provider unavailable") ||
		strings.Contains(string(raw), privateProviderError.Error()) {
		t.Fatalf("setup provider error = %d %s", status, raw)
	}

	refusalHarness := newBillingHTTPHarness(t, newBillingHTTPProviderStub(), nil, nil)
	status, raw = billingHTTPRequest(t, refusalHarness.server, http.MethodPost,
		"/v1/accounts/acct_1/billing:portal", "owner", "", "")
	if status != http.StatusConflict || !strings.Contains(string(raw), "payment setup") {
		t.Fatalf("portal refusal = %d %s", status, raw)
	}
}

func TestBillingHTTPStoreFailuresAreGeneric500(t *testing.T) {
	privateStoreError := errors.New("r2 secret endpoint unavailable")
	store := billingHTTPFailingStore{
		Store: lifecycle.NewMemStore(), err: privateStoreError,
	}
	h := newBillingHTTPHarness(t, newBillingHTTPProviderStub(), store, nil)
	status, raw := billingHTTPRequest(t, h.server, http.MethodGet,
		"/v1/accounts/acct_1/billing", "owner", "", "")
	if status != http.StatusInternalServerError ||
		!strings.Contains(string(raw), "billing request failed") ||
		strings.Contains(string(raw), privateStoreError.Error()) {
		t.Fatalf("store error = %d %s", status, raw)
	}
}

func TestBillingHTTPValidatesHostedActionsAndDropsUnsafeOptionalLinks(t *testing.T) {
	provider := newBillingHTTPProviderStub()
	provider.setupAction = billing.Action{URL: "http://billing.example/setup"}
	provider.portalURL = "https://user:secret@billing.example/portal"
	provider.invoices = []billing.Invoice{
		{
			Number: "INV-1", Status: "paid",
			PDFURL: "javascript:alert(1)", HostedURL: "https://billing.example/invoices/1",
		},
		{
			Number: "INV-2", Status: "paid",
			PDFURL: "https://billing.example/invoices/%0aevil",
		},
	}
	provider.payments = []billing.Payment{{
		Status: "succeeded", ReceiptURL: "https://user:secret@billing.example/receipt",
	}}
	store := lifecycle.NewMemStore()
	putBillingHTTPRecord(t, store, lifecycle.Record{
		AccountID: "acct_1", Provider: "billing", CustomerID: "customer_private",
		Entitled: "standard", Applied: "standard",
	})
	h := newBillingHTTPHarness(t, provider, store, nil)

	for _, path := range []string{
		"/v1/accounts/acct_1/billing:setup",
		"/v1/accounts/acct_1/billing:portal",
	} {
		var status int
		var raw []byte
		if strings.HasSuffix(path, ":setup") {
			status, raw = billingHTTPRequestWithKey(
				t, h.server, http.MethodPost, path, "owner", "",
				`{"reason":"Validate hosted action","confirmed":true}`,
				"billing-http-unsafe-action")
		} else {
			status, raw = billingHTTPRequest(
				t, h.server, http.MethodPost, path, "owner", "", "")
		}
		if status != http.StatusBadGateway || strings.Contains(string(raw), "billing.example") ||
			strings.Contains(string(raw), "secret") {
			t.Errorf("unsafe action %s = %d %s", path, status, raw)
		}
	}

	status, raw := billingHTTPRequest(t, h.server, http.MethodGet,
		"/v1/accounts/acct_1/billing/invoices", "owner", "", "")
	if status != http.StatusOK {
		t.Fatalf("invoices = %d %s", status, raw)
	}
	doc := decodeBillingHTTPDocument(t, raw)
	invoices := doc["invoices"].([]any)
	first := invoices[0].(map[string]any)
	second := invoices[1].(map[string]any)
	if _, present := first["pdf_url"]; present ||
		first["hosted_url"] != "https://billing.example/invoices/1" {
		t.Fatalf("first invoice links = %v", first)
	}
	if _, present := second["pdf_url"]; present {
		t.Fatalf("encoded-control link survived: %v", second)
	}

	status, raw = billingHTTPRequest(t, h.server, http.MethodGet,
		"/v1/accounts/acct_1/billing/payments", "owner", "", "")
	if status != http.StatusOK {
		t.Fatalf("payments = %d %s", status, raw)
	}
	doc = decodeBillingHTTPDocument(t, raw)
	payment := doc["payments"].([]any)[0].(map[string]any)
	if _, present := payment["receipt_url"]; present {
		t.Fatalf("unsafe receipt survived: %v", payment)
	}
}

func TestBillingHTTPRejectsUnsafeStoredPendingURL(t *testing.T) {
	provider := newBillingHTTPProviderStub()
	store := lifecycle.NewMemStore()
	putBillingHTTPRecord(t, store, lifecycle.Record{
		AccountID: "acct_1", Provider: "billing", CustomerID: "customer_private",
		Entitled: plans.Free, Applied: plans.Free,
		Pending: &lifecycle.Pending{
			Kind: lifecycle.PendingUpgrade, Plan: "standard",
			URL: "https://billing.example/%0ahttps://forged.example", Requested: time.Now(),
		},
	})
	h := newBillingHTTPHarness(t, provider, store, nil)
	status, raw := billingHTTPRequest(t, h.server, http.MethodGet,
		"/v1/accounts/acct_1/billing", "owner", "", "")
	if status != http.StatusBadGateway || strings.Contains(string(raw), "forged") {
		t.Fatalf("unsafe pending URL = %d %s", status, raw)
	}
}

func TestSafeBillingURL(t *testing.T) {
	tests := []struct {
		raw  string
		want bool
	}{
		{"https://billing.example/portal?session=public#done", true},
		{"HTTPS://billing.example/portal", true},
		{"http://billing.example/portal", false},
		{"javascript:alert(1)", false},
		{"file:///tmp/portal", false},
		{"https:billing.example/opaque", false},
		{"https://user:secret@billing.example/portal", false},
		{"https://billing.example/portal\nhttps://forged.example", false},
		{"https://billing.example/%0ahttps://forged.example", false},
		{"https://billing.example/%E2%80%AEspoof", false},
		{"https://billing.example/\\@forged.example", false},
		{"https://billing.example/%5c@forged.example", false},
		{"https://billing.example/\u2028forged", false},
		{" https://billing.example/portal ", false},
	}
	for _, tc := range tests {
		if got := safeBillingURL(tc.raw); got != tc.want {
			t.Errorf("safeBillingURL(%q) = %t; want %t", tc.raw, got, tc.want)
		}
	}
}

func TestBillingHTTPTruncatesProviderCollectionsToNewestBound(t *testing.T) {
	provider := newBillingHTTPProviderStub()
	provider.invoices = make([]billing.Invoice, maxBillingCollectionEntries+1)
	for i := range provider.invoices {
		provider.invoices[i].Number = fmt.Sprintf("INV-%03d", i)
	}
	provider.payments = make([]billing.Payment, maxBillingCollectionEntries+1)
	for i := range provider.payments {
		provider.payments[i].Method = fmt.Sprintf("method-%03d", i)
	}
	store := lifecycle.NewMemStore()
	putBillingHTTPRecord(t, store, lifecycle.Record{
		AccountID: "acct_1", Provider: "billing", CustomerID: "customer_private",
	})
	h := newBillingHTTPHarness(t, provider, store, nil)
	status, raw := billingHTTPRequest(t, h.server, http.MethodGet,
		"/v1/accounts/acct_1/billing/invoices", "owner", "", "")
	if status != http.StatusOK {
		t.Fatalf("oversized invoices = %d %s", status, raw)
	}
	doc := decodeBillingHTTPDocument(t, raw)
	invoices := doc["invoices"].([]any)
	if len(invoices) != maxBillingCollectionEntries ||
		invoices[0].(map[string]any)["number"] != "INV-000" ||
		invoices[len(invoices)-1].(map[string]any)["number"] != "INV-099" {
		t.Fatalf("truncated invoices = %d first=%v last=%v",
			len(invoices), invoices[0], invoices[len(invoices)-1])
	}
	status, raw = billingHTTPRequest(t, h.server, http.MethodGet,
		"/v1/accounts/acct_1/billing/payments", "owner", "", "")
	if status != http.StatusOK {
		t.Fatalf("oversized payments = %d %s", status, raw)
	}
	doc = decodeBillingHTTPDocument(t, raw)
	payments := doc["payments"].([]any)
	if len(payments) != maxBillingCollectionEntries ||
		payments[0].(map[string]any)["method"] != "method-000" ||
		payments[len(payments)-1].(map[string]any)["method"] != "method-099" {
		t.Fatalf("truncated payments = %d first=%v last=%v",
			len(payments), payments[0], payments[len(payments)-1])
	}
}

type billingHTTPProviderStub struct {
	billing.Provider

	ensureCustomerID string
	ensureErr        error
	setupAction      billing.Action
	setupErr         error
	portalURL        string
	portalErr        error
	paymentMethod    *billing.PaymentMethod
	paymentMethodErr error
	nextCharge       *billing.UpcomingCharge
	nextChargeErr    error
	invoices         []billing.Invoice
	invoicesErr      error
	payments         []billing.Payment
	paymentsErr      error
}

func newBillingHTTPProviderStub() *billingHTTPProviderStub {
	return &billingHTTPProviderStub{
		Provider:         fake.New(fake.Config{}),
		ensureCustomerID: "customer_private",
		setupAction:      billing.Action{Done: true},
		portalURL:        "https://billing.example/portal",
	}
}

func (p *billingHTTPProviderStub) EnsureCustomer(
	context.Context,
	string,
	string,
) (string, error) {
	return p.ensureCustomerID, p.ensureErr
}

func (p *billingHTTPProviderStub) SetupLink(
	context.Context,
	string,
) (billing.Action, error) {
	return p.setupAction, p.setupErr
}

func (p *billingHTTPProviderStub) SetupLinkIdempotent(
	ctx context.Context,
	customerID, _ string,
) (billing.Action, error) {
	return p.SetupLink(ctx, customerID)
}

func (p *billingHTTPProviderStub) PortalLink(
	context.Context,
	string,
) (string, error) {
	return p.portalURL, p.portalErr
}

func (p *billingHTTPProviderStub) PaymentMethodOnFile(
	context.Context,
	string,
) (*billing.PaymentMethod, error) {
	if p.paymentMethod == nil {
		return nil, p.paymentMethodErr
	}
	value := *p.paymentMethod
	return &value, p.paymentMethodErr
}

func (p *billingHTTPProviderStub) NextCharge(
	context.Context,
	string,
) (*billing.UpcomingCharge, error) {
	if p.nextCharge == nil {
		return nil, p.nextChargeErr
	}
	value := *p.nextCharge
	return &value, p.nextChargeErr
}

func (p *billingHTTPProviderStub) ListInvoices(
	context.Context,
	string,
) ([]billing.Invoice, error) {
	return append([]billing.Invoice{}, p.invoices...), p.invoicesErr
}

func (p *billingHTTPProviderStub) ListPayments(
	context.Context,
	string,
) ([]billing.Payment, error) {
	return append([]billing.Payment{}, p.payments...), p.paymentsErr
}

type billingHTTPFailingStore struct {
	lifecycle.Store
	err error
}

func (s billingHTTPFailingStore) Get(
	context.Context,
	string,
) (lifecycle.Record, bool, error) {
	return lifecycle.Record{}, false, s.err
}
