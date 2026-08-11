package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/billing"
	"github.com/witwave-ai/witself/internal/plans"
)

type billingReadProviderStub struct {
	mu sync.Mutex

	ensureCalls     int
	ensureIDs       []string
	ensureEntered   chan struct{}
	ensureRelease   chan struct{}
	ensureErr       error
	setupAction     billing.Action
	setupErr        error
	setupCustomers  []string
	portalURL       string
	portalErr       error
	portalCustomers []string

	paymentMethod          *billing.PaymentMethod
	paymentMethodErr       error
	paymentMethodCustomers []string
	nextCharge             *billing.UpcomingCharge
	nextChargeErr          error
	nextChargeCustomers    []string
	invoices               []billing.Invoice
	invoicesErr            error
	invoiceCustomers       []string
	payments               []billing.Payment
	paymentsErr            error
	paymentCustomers       []string
}

var _ billing.Provider = (*billingReadProviderStub)(nil)

func (p *billingReadProviderStub) EnsureCustomer(
	ctx context.Context,
	_, _ string,
) (string, error) {
	p.mu.Lock()
	p.ensureCalls++
	call := p.ensureCalls
	id := fmt.Sprintf("stub_customer_%d", call)
	if call <= len(p.ensureIDs) {
		id = p.ensureIDs[call-1]
	}
	entered, release, err := p.ensureEntered, p.ensureRelease, p.ensureErr
	p.mu.Unlock()
	if entered != nil {
		select {
		case entered <- struct{}{}:
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	if release != nil {
		select {
		case <-release:
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	return id, err
}

func (p *billingReadProviderStub) SetupLink(
	_ context.Context,
	customerID string,
) (billing.Action, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.setupCustomers = append(p.setupCustomers, customerID)
	return p.setupAction, p.setupErr
}

func (p *billingReadProviderStub) PortalLink(
	_ context.Context,
	customerID string,
) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.portalCustomers = append(p.portalCustomers, customerID)
	return p.portalURL, p.portalErr
}

func (*billingReadProviderStub) Subscribe(
	context.Context, string, string,
) (billing.Action, error) {
	return billing.Action{}, errors.New("unexpected Subscribe call")
}

func (*billingReadProviderStub) ScheduleDowngrade(
	context.Context, string, string,
) (time.Time, error) {
	return time.Time{}, errors.New("unexpected ScheduleDowngrade call")
}

func (*billingReadProviderStub) CancelPending(context.Context, string) error {
	return errors.New("unexpected CancelPending call")
}

func (*billingReadProviderStub) HandleWebhook(*http.Request) ([]billing.Event, error) {
	return nil, errors.New("unexpected HandleWebhook call")
}

func (*billingReadProviderStub) RecordUsage(
	context.Context, string, string, int64, string,
) error {
	return errors.New("unexpected RecordUsage call")
}

func (p *billingReadProviderStub) PaymentMethodOnFile(
	_ context.Context,
	customerID string,
) (*billing.PaymentMethod, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.paymentMethodCustomers = append(p.paymentMethodCustomers, customerID)
	if p.paymentMethod == nil {
		return nil, p.paymentMethodErr
	}
	value := *p.paymentMethod
	return &value, p.paymentMethodErr
}

func (p *billingReadProviderStub) ListInvoices(
	_ context.Context,
	customerID string,
) ([]billing.Invoice, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.invoiceCustomers = append(p.invoiceCustomers, customerID)
	return append([]billing.Invoice{}, p.invoices...), p.invoicesErr
}

func (p *billingReadProviderStub) ListPayments(
	_ context.Context,
	customerID string,
) ([]billing.Payment, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.paymentCustomers = append(p.paymentCustomers, customerID)
	return append([]billing.Payment{}, p.payments...), p.paymentsErr
}

func (p *billingReadProviderStub) NextCharge(
	_ context.Context,
	customerID string,
) (*billing.UpcomingCharge, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.nextChargeCustomers = append(p.nextChargeCustomers, customerID)
	if p.nextCharge == nil {
		return nil, p.nextChargeErr
	}
	value := *p.nextCharge
	return &value, p.nextChargeErr
}

func newBillingReadManager(
	t *testing.T,
	store Store,
	providers map[string]billing.Provider,
	defaultProvider string,
) *Manager {
	t.Helper()
	catalog, err := plans.Load()
	if err != nil {
		t.Fatal(err)
	}
	manager, err := NewManager(Config{
		Catalog: catalog, Providers: providers, Default: defaultProvider,
		Store: store, Applier: &recApplier{},
	})
	if err != nil {
		t.Fatal(err)
	}
	return manager
}

func putBillingReadRecord(t *testing.T, store Store, record Record) {
	t.Helper()
	if err := store.Put(context.Background(), record); err != nil {
		t.Fatal(err)
	}
}

func TestBillingReadsUnknownAccountsAreSideEffectFree(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name      string
		providers map[string]billing.Provider
		defaultID string
	}{
		{
			name: "provider configured",
			providers: map[string]billing.Provider{
				"billing": &billingReadProviderStub{},
			},
			defaultID: "billing",
		},
		{name: "providerless"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := NewMemStore()
			manager := newBillingReadManager(
				t, store, tc.providers, tc.defaultID)

			summary, err := manager.ReadBillingSummary(
				ctx, "acct_unknown", "owner@example.com")
			if err != nil || summary.Configured ||
				summary.PaymentMethod != nil || summary.NextCharge != nil {
				t.Fatalf("summary = %+v, %v", summary, err)
			}
			invoices, err := manager.ListBillingInvoices(
				ctx, "acct_unknown", "owner@example.com")
			if err != nil || invoices == nil || len(invoices) != 0 {
				t.Fatalf("invoices = %#v, %v; want non-nil empty", invoices, err)
			}
			payments, err := manager.ListBillingPayments(
				ctx, "acct_unknown", "owner@example.com")
			if err != nil || payments == nil || len(payments) != 0 {
				t.Fatalf("payments = %#v, %v; want non-nil empty", payments, err)
			}
			records, err := store.List(ctx)
			if err != nil || len(records) != 0 {
				t.Fatalf("reads persisted records = %d, %v", len(records), err)
			}
		})
	}
}

func TestBillingSummaryUsesPinnedProviderActualCharge(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 10, 20, 0, 0, 0, time.UTC)
	oldProvider := &billingReadProviderStub{
		paymentMethod: &billing.PaymentMethod{Label: "visa ****4242"},
		nextCharge: &billing.UpcomingCharge{
			Date: now.AddDate(0, 1, 0), AmountCents: 1234, Currency: "usd",
		},
	}
	newProvider := &billingReadProviderStub{
		paymentMethod: &billing.PaymentMethod{Label: "wrong ****0000"},
		nextCharge:    &billing.UpcomingCharge{AmountCents: 9999, Currency: "usd"},
	}
	store := NewMemStore()
	putBillingReadRecord(t, store, Record{
		AccountID: "acct_paid", Provider: "old", CustomerID: "customer_private",
		Entitled: "standard", Applied: "standard",
	})
	manager := newBillingReadManager(t, store, map[string]billing.Provider{
		"old": oldProvider, "new": newProvider,
	}, "new")

	summary, err := manager.ReadBillingSummary(ctx, "acct_paid", "")
	if err != nil {
		t.Fatal(err)
	}
	if !summary.Configured || summary.PaymentMethod == nil ||
		summary.PaymentMethod.Label != "visa ****4242" ||
		summary.NextCharge == nil || summary.NextCharge.AmountCents != 1234 ||
		summary.NextCharge.Currency != "usd" {
		t.Fatalf("summary = %+v", summary)
	}
	// Professional's current catalog list price is $30. The provider's
	// $12.34 preview proves the summary did not substitute catalog pricing.
	if summary.NextCharge.AmountCents == 3000 {
		t.Fatal("summary masqueraded the catalog list price as the actual charge")
	}
	oldProvider.mu.Lock()
	oldPaymentCalls := append([]string{}, oldProvider.paymentMethodCustomers...)
	oldChargeCalls := append([]string{}, oldProvider.nextChargeCustomers...)
	oldProvider.mu.Unlock()
	newProvider.mu.Lock()
	newCalls := len(newProvider.paymentMethodCustomers) +
		len(newProvider.nextChargeCustomers)
	newProvider.mu.Unlock()
	if !reflect.DeepEqual(oldPaymentCalls, []string{"customer_private"}) ||
		!reflect.DeepEqual(oldChargeCalls, []string{"customer_private"}) ||
		newCalls != 0 {
		t.Fatalf("provider routing old payment=%v charge=%v new_calls=%d",
			oldPaymentCalls, oldChargeCalls, newCalls)
	}

	// The public summary type has no slot in which a provider or customer id
	// could accidentally be serialized by a later transport layer.
	typ := reflect.TypeOf(BillingSummary{})
	for _, field := range []string{"Provider", "ProviderName", "CustomerID"} {
		if _, ok := typ.FieldByName(field); ok {
			t.Fatalf("BillingSummary exposes %s", field)
		}
	}
}

func TestBillingHistoryUsesPinnedProviderAndCopiesSlices(t *testing.T) {
	ctx := context.Background()
	oldProvider := &billingReadProviderStub{
		invoices: []billing.Invoice{{Number: "inv-actual", AmountCents: 2500}},
		payments: []billing.Payment{{AmountCents: 2500, Status: "succeeded"}},
	}
	newProvider := &billingReadProviderStub{
		invoices: []billing.Invoice{{Number: "inv-wrong"}},
		payments: []billing.Payment{{Status: "wrong"}},
	}
	store := NewMemStore()
	putBillingReadRecord(t, store, Record{
		AccountID: "acct_paid", Provider: "old", CustomerID: "customer_private",
		Entitled: "standard", Applied: "standard",
	})
	manager := newBillingReadManager(t, store, map[string]billing.Provider{
		"old": oldProvider, "new": newProvider,
	}, "new")

	invoices, err := manager.ListBillingInvoices(ctx, "acct_paid", "")
	if err != nil || len(invoices) != 1 || invoices[0].Number != "inv-actual" {
		t.Fatalf("invoices = %+v, %v", invoices, err)
	}
	payments, err := manager.ListBillingPayments(ctx, "acct_paid", "")
	if err != nil || len(payments) != 1 || payments[0].Status != "succeeded" {
		t.Fatalf("payments = %+v, %v", payments, err)
	}
	invoices[0].Number = "mutated"
	payments[0].Status = "mutated"
	oldProvider.mu.Lock()
	defer oldProvider.mu.Unlock()
	if oldProvider.invoices[0].Number != "inv-actual" ||
		oldProvider.payments[0].Status != "succeeded" {
		t.Fatal("caller mutation aliased provider history")
	}
}

func TestCreateBillingSetupPinsCustomerAndEnablesPortal(t *testing.T) {
	ctx := context.Background()
	provider := &billingReadProviderStub{
		ensureIDs:   []string{"customer_private"},
		setupAction: billing.Action{Done: true},
		portalURL:   "https://billing.example/portal/private",
	}
	store := NewMemStore()
	manager := newBillingReadManager(t, store,
		map[string]billing.Provider{"billing": provider}, "billing")

	if _, err := manager.CreateBillingPortal(
		ctx, "acct_new", "owner@example.com"); !errors.Is(err, ErrRefusal) {
		t.Fatalf("portal before setup error = %v; want ErrRefusal", err)
	}
	action, err := manager.CreateBillingSetup(
		ctx, "acct_new", "owner@example.com")
	if err != nil || !action.Done || action.URL != "" {
		t.Fatalf("setup = %+v, %v", action, err)
	}
	record, ok, err := store.Get(ctx, "acct_new")
	if err != nil || !ok || record.Provider != "billing" ||
		record.CustomerID != "customer_private" ||
		record.Email != "owner@example.com" {
		t.Fatalf("persisted record = %+v, ok=%t err=%v", record, ok, err)
	}
	if _, err := manager.CreateBillingSetup(
		ctx, "acct_new", "owner@example.com"); err != nil {
		t.Fatal(err)
	}
	provider.mu.Lock()
	ensureCalls := provider.ensureCalls
	setupCustomers := append([]string{}, provider.setupCustomers...)
	provider.mu.Unlock()
	if ensureCalls != 1 || !reflect.DeepEqual(
		setupCustomers, []string{"customer_private", "customer_private"}) {
		t.Fatalf("ensure_calls=%d setup_customers=%v", ensureCalls, setupCustomers)
	}
	portal, err := manager.CreateBillingPortal(ctx, "acct_new", "")
	if err != nil || portal != "https://billing.example/portal/private" {
		t.Fatalf("portal = %q, %v", portal, err)
	}
}

func TestCreateBillingSetupHonorsExistingProviderPin(t *testing.T) {
	ctx := context.Background()
	oldProvider := &billingReadProviderStub{
		ensureIDs:   []string{"old_customer"},
		setupAction: billing.Action{Done: true},
		portalURL:   "https://old.example/portal",
	}
	newProvider := &billingReadProviderStub{
		ensureIDs:   []string{"new_customer"},
		setupAction: billing.Action{Done: true},
		portalURL:   "https://new.example/portal",
	}
	store := NewMemStore()
	putBillingReadRecord(t, store, Record{
		AccountID: "acct_pinned", Provider: "old",
		Entitled: plans.Free, Applied: plans.Free,
	})
	manager := newBillingReadManager(t, store, map[string]billing.Provider{
		"old": oldProvider, "new": newProvider,
	}, "new")

	if _, err := manager.CreateBillingSetup(
		ctx, "acct_pinned", "owner@example.com"); err != nil {
		t.Fatal(err)
	}
	record, ok, err := store.Get(ctx, "acct_pinned")
	if err != nil || !ok || record.Provider != "old" ||
		record.CustomerID != "old_customer" {
		t.Fatalf("record = %+v ok=%t err=%v", record, ok, err)
	}
	portal, err := manager.CreateBillingPortal(ctx, "acct_pinned", "")
	if err != nil || portal != "https://old.example/portal" {
		t.Fatalf("portal = %q, %v", portal, err)
	}
	oldProvider.mu.Lock()
	oldEnsureCalls := oldProvider.ensureCalls
	oldPortalCustomers := append([]string{}, oldProvider.portalCustomers...)
	oldProvider.mu.Unlock()
	newProvider.mu.Lock()
	newEnsureCalls := newProvider.ensureCalls
	newPortalCalls := len(newProvider.portalCustomers)
	newProvider.mu.Unlock()
	if oldEnsureCalls != 1 ||
		!reflect.DeepEqual(oldPortalCustomers, []string{"old_customer"}) ||
		newEnsureCalls != 0 || newPortalCalls != 0 {
		t.Fatalf("old ensure=%d portal=%v; new ensure=%d portal_calls=%d",
			oldEnsureCalls, oldPortalCustomers, newEnsureCalls, newPortalCalls)
	}
}

func TestBillingMutationsRefuseProviderlessMode(t *testing.T) {
	ctx := context.Background()
	store := NewMemStore()
	manager := newBillingReadManager(t, store, nil, "")
	if _, err := manager.CreateBillingSetup(
		ctx, "acct_manual", "owner@example.com"); !errors.Is(err, ErrBillingUnavailable) {
		t.Fatalf("setup error = %v; want ErrBillingUnavailable", err)
	}
	if _, err := manager.CreateBillingPortal(
		ctx, "acct_manual", "owner@example.com"); !errors.Is(err, ErrBillingUnavailable) {
		t.Fatalf("portal error = %v; want ErrBillingUnavailable", err)
	}
	records, err := store.List(ctx)
	if err != nil || len(records) != 0 {
		t.Fatalf("providerless mutations persisted records = %d, %v",
			len(records), err)
	}
}

func TestConcurrentBillingSetupUsesPersistedCustomerAuthority(t *testing.T) {
	ctx := context.Background()
	provider := &billingReadProviderStub{
		ensureIDs:     []string{"customer_one", "customer_two"},
		ensureEntered: make(chan struct{}, 2),
		ensureRelease: make(chan struct{}),
		setupAction:   billing.Action{Done: true},
	}
	store := NewMemStore()
	manager := newBillingReadManager(t, store,
		map[string]billing.Provider{"billing": provider}, "billing")

	type result struct {
		action billing.Action
		err    error
	}
	results := make(chan result, 2)
	for range 2 {
		go func() {
			action, err := manager.CreateBillingSetup(
				ctx, "acct_race", "owner@example.com")
			results <- result{action: action, err: err}
		}()
	}
	for range 2 {
		<-provider.ensureEntered
	}
	close(provider.ensureRelease)
	for range 2 {
		got := <-results
		if got.err != nil || !got.action.Done {
			t.Fatalf("concurrent setup = %+v, %v", got.action, got.err)
		}
	}

	record, ok, err := store.Get(ctx, "acct_race")
	if err != nil || !ok {
		t.Fatalf("persisted record ok=%t err=%v", ok, err)
	}
	provider.mu.Lock()
	setupCustomers := append([]string{}, provider.setupCustomers...)
	provider.mu.Unlock()
	if len(setupCustomers) != 2 {
		t.Fatalf("setup customers = %v", setupCustomers)
	}
	for _, customerID := range setupCustomers {
		if customerID != record.CustomerID {
			t.Fatalf("setup used losing customer %q; persisted=%q",
				customerID, record.CustomerID)
		}
	}
	if record.CustomerID != "customer_one" && record.CustomerID != "customer_two" {
		t.Fatalf("unexpected persisted customer %q", record.CustomerID)
	}
}

func TestBillingProviderErrorsAreClassified(t *testing.T) {
	ctx := context.Background()
	upstream := errors.New("provider private detail")

	t.Run("summary payment method", func(t *testing.T) {
		provider := &billingReadProviderStub{paymentMethodErr: upstream}
		manager := managerWithBillingCustomer(t, provider)
		_, err := manager.ReadBillingSummary(ctx, "acct_paid", "")
		assertProviderRequestError(t, err, upstream)
	})
	t.Run("summary next charge", func(t *testing.T) {
		provider := &billingReadProviderStub{nextChargeErr: upstream}
		manager := managerWithBillingCustomer(t, provider)
		_, err := manager.ReadBillingSummary(ctx, "acct_paid", "")
		assertProviderRequestError(t, err, upstream)
	})
	t.Run("invoices", func(t *testing.T) {
		provider := &billingReadProviderStub{invoicesErr: upstream}
		manager := managerWithBillingCustomer(t, provider)
		_, err := manager.ListBillingInvoices(ctx, "acct_paid", "")
		assertProviderRequestError(t, err, upstream)
	})
	t.Run("payments", func(t *testing.T) {
		provider := &billingReadProviderStub{paymentsErr: upstream}
		manager := managerWithBillingCustomer(t, provider)
		_, err := manager.ListBillingPayments(ctx, "acct_paid", "")
		assertProviderRequestError(t, err, upstream)
	})
	t.Run("ensure customer", func(t *testing.T) {
		provider := &billingReadProviderStub{ensureErr: upstream}
		store := NewMemStore()
		manager := newBillingReadManager(t, store,
			map[string]billing.Provider{"billing": provider}, "billing")
		_, err := manager.CreateBillingSetup(ctx, "acct_new", "")
		assertProviderRequestError(t, err, upstream)
	})
	t.Run("setup", func(t *testing.T) {
		provider := &billingReadProviderStub{setupErr: upstream}
		manager := managerWithBillingCustomer(t, provider)
		_, err := manager.CreateBillingSetup(ctx, "acct_paid", "")
		assertProviderRequestError(t, err, upstream)
	})
	t.Run("portal", func(t *testing.T) {
		provider := &billingReadProviderStub{portalErr: upstream}
		manager := managerWithBillingCustomer(t, provider)
		_, err := manager.CreateBillingPortal(ctx, "acct_paid", "")
		assertProviderRequestError(t, err, upstream)
	})
}

func TestBillingProviderInvalidResultsAreClassified(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		run  func(*Manager) error
		stub *billingReadProviderStub
	}{
		{
			name: "empty customer id",
			stub: &billingReadProviderStub{ensureIDs: []string{" "}},
			run: func(manager *Manager) error {
				_, err := manager.CreateBillingSetup(ctx, "acct_new", "")
				return err
			},
		},
		{
			name: "empty setup action",
			stub: &billingReadProviderStub{},
			run: func(manager *Manager) error {
				_, err := manager.CreateBillingSetup(ctx, "acct_paid", "")
				return err
			},
		},
		{
			name: "ambiguous setup action",
			stub: &billingReadProviderStub{
				setupAction: billing.Action{Done: true, URL: "https://example.invalid"},
			},
			run: func(manager *Manager) error {
				_, err := manager.CreateBillingSetup(ctx, "acct_paid", "")
				return err
			},
		},
		{
			name: "unsafe setup action",
			stub: &billingReadProviderStub{
				setupAction: billing.Action{URL: " https://example.invalid "},
			},
			run: func(manager *Manager) error {
				_, err := manager.CreateBillingSetup(ctx, "acct_paid", "")
				return err
			},
		},
		{
			name: "empty portal URL",
			stub: &billingReadProviderStub{},
			run: func(manager *Manager) error {
				_, err := manager.CreateBillingPortal(ctx, "acct_paid", "")
				return err
			},
		},
		{
			name: "unsafe portal URL",
			stub: &billingReadProviderStub{portalURL: " https://example.invalid "},
			run: func(manager *Manager) error {
				_, err := manager.CreateBillingPortal(ctx, "acct_paid", "")
				return err
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var manager *Manager
			if tc.name == "empty customer id" {
				store := NewMemStore()
				manager = newBillingReadManager(t, store,
					map[string]billing.Provider{"billing": tc.stub}, "billing")
			} else {
				manager = managerWithBillingCustomer(t, tc.stub)
			}
			if err := tc.run(manager); !errors.Is(err, ErrProviderRequest) {
				t.Fatalf("error = %v; want ErrProviderRequest", err)
			}
		})
	}
}

type billingReadFailingStore struct {
	Store
	err error
}

func (s billingReadFailingStore) Get(
	context.Context,
	string,
) (Record, bool, error) {
	return Record{}, false, s.err
}

func TestBillingStoreErrorsRemainDistinctFromProviderFailures(t *testing.T) {
	storeErr := errors.New("registry unavailable")
	manager := newBillingReadManager(t, billingReadFailingStore{
		Store: NewMemStore(), err: storeErr,
	}, map[string]billing.Provider{
		"billing": &billingReadProviderStub{},
	}, "billing")
	_, err := manager.ReadBillingSummary(
		context.Background(), "acct_paid", "")
	if !errors.Is(err, storeErr) || errors.Is(err, ErrProviderRequest) {
		t.Fatalf("error = %v; want raw store classification", err)
	}
}

func managerWithBillingCustomer(
	t *testing.T,
	provider billing.Provider,
) *Manager {
	t.Helper()
	store := NewMemStore()
	putBillingReadRecord(t, store, Record{
		AccountID: "acct_paid", Provider: "billing", CustomerID: "customer_private",
		Entitled: "standard", Applied: "standard",
	})
	return newBillingReadManager(t, store,
		map[string]billing.Provider{"billing": provider}, "billing")
}

func assertProviderRequestError(t *testing.T, err, upstream error) {
	t.Helper()
	if !errors.Is(err, ErrProviderRequest) || !errors.Is(err, upstream) {
		t.Fatalf("error = %v; want ErrProviderRequest and upstream", err)
	}
}
