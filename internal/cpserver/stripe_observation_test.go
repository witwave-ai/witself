package cpserver

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/billing"
	"github.com/witwave-ai/witself/internal/billing/fake"
	"github.com/witwave-ai/witself/internal/billing/lifecycle"
	"github.com/witwave-ai/witself/internal/billing/stripe"
	"github.com/witwave-ai/witself/internal/plans"
)

type stripeObservationStore struct {
	*lifecycle.MemStore
	failReceive bool
}

func (s *stripeObservationStore) ReceiveEvent(ctx context.Context, receipt lifecycle.EventReceipt) (lifecycle.EventReceipt, bool, error) {
	if s.failReceive {
		return lifecycle.EventReceipt{}, false, errors.New("fixture receipt write failed")
	}
	return s.MemStore.ReceiveEvent(ctx, receipt)
}

type stripeObservationTransport struct{ t *testing.T }

func (s stripeObservationTransport) RoundTrip(*http.Request) (*http.Response, error) {
	s.t.Error("observation fixture attempted an unexpected provider request")
	return nil, errors.New("provider network is disabled")
}

func TestStripeWebhookObservationPreservesResponses(t *testing.T) {
	catalog, err := plans.Load()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	const secret = "fixture_signing_material"
	provider, err := stripe.New(stripe.Config{
		SecretKey: "sk_test_fixture", WebhookSecret: secret, Catalog: catalog,
		Now:        func() time.Time { return now },
		HTTPClient: &http.Client{Transport: stripeObservationTransport{t}},
	})
	if err != nil {
		t.Fatal(err)
	}
	payload := fmt.Sprintf(`{"id":"evt_private_fixture","type":"customer.subscription.deleted","created":%d,"data":{"object":{"id":"sub_private_fixture","customer":"cus_private_fixture"}}}`, now.Unix())
	noop := fmt.Sprintf(`{"id":"evt_noop_private_fixture","type":"ignored.fixture","created":%d,"data":{"object":{}}}`, now.Unix())
	type response struct {
		status int
		body   string
	}
	run := func(observer *PlanLifecycleObserver) ([]response, StripeObservation) {
		t.Helper()
		store := &stripeObservationStore{MemStore: lifecycle.NewMemStore()}
		manager, err := lifecycle.NewManager(lifecycle.Config{
			Catalog: catalog, Providers: map[string]billing.Provider{"stripe": provider},
			Default: "stripe", Store: store, Applier: noopApplier{}, Now: func() time.Time { return now },
		})
		if err != nil {
			t.Fatal(err)
		}
		handler := webhook(Config{Manager: manager, LifecycleObserver: observer}, "stripe", provider)
		var out []response
		for _, attempt := range []struct {
			body   string
			signed bool
			fail   bool
		}{{payload, false, false}, {payload, true, false}, {payload, true, false}, {payload, true, true}, {noop, true, false}} {
			store.failReceive = attempt.fail
			req := httptest.NewRequest(http.MethodPost, "/v1/billing/webhook/stripe", strings.NewReader(attempt.body))
			if attempt.signed {
				mac := hmac.New(sha256.New, []byte(secret))
				_, _ = fmt.Fprintf(mac, "%d.%s", now.Unix(), attempt.body)
				req.Header.Set("Stripe-Signature", fmt.Sprintf("t=%d,v1=%s", now.Unix(), hex.EncodeToString(mac.Sum(nil))))
			}
			rec := httptest.NewRecorder()
			handler(rec, req)
			out = append(out, response{rec.Code, rec.Body.String()})
		}
		var observation StripeObservation
		if observer != nil {
			observation = observer.StripeSnapshot(now, manager.StripeWebhookReplays())
		}
		return out, observation
	}
	without, _ := run(nil)
	with, observation := run(NewPlanLifecycleObserver(true))
	want := []response{
		{400, "{\"error\":\"webhook rejected\",\"schema_version\":\"witself.v0\"}\n"},
		{200, "{\"received\":1,\"schema_version\":\"witself.v0\"}\n"},
		{200, "{\"received\":1,\"schema_version\":\"witself.v0\"}\n"},
		{500, "{\"error\":\"could not process events\",\"schema_version\":\"witself.v0\"}\n"},
		{200, "{\"received\":0,\"schema_version\":\"witself.v0\"}\n"},
	}
	if !reflect.DeepEqual(with, want) || !reflect.DeepEqual(with, without) {
		t.Fatalf("webhook HTTP contract changed: with=%v without=%v", with, without)
	}
	if observation.WebhookEvents != (StripeWebhookEvents{Verified: 4, Rejected: 1, Replayed: 1}) {
		t.Fatalf("webhook counters = %+v", observation.WebhookEvents)
	}
	encoded, err := json.Marshal(observation)
	if err != nil {
		t.Fatal(err)
	}
	for _, private := range []string{"evt_private_fixture", "sub_private_fixture", "cus_private_fixture", "fixture receipt write failed"} {
		if strings.Contains(string(encoded), private) {
			t.Fatal("private event detail escaped the observation projection")
		}
	}
}

func TestStripeObservationTickIsAuthenticatedAndStripeOnly(t *testing.T) {
	for _, providerName := range []string{"", "fake", "stripe"} {
		t.Run(providerName, func(t *testing.T) {
			catalog, err := plans.Load()
			if err != nil {
				t.Fatal(err)
			}
			providers := map[string]billing.Provider{}
			if providerName != "" {
				providers[providerName] = fake.New(fake.Config{Prices: catalog.Prices()})
			}
			manager, err := lifecycle.NewManager(lifecycle.Config{
				Catalog: catalog, Providers: providers, Default: providerName,
				Store: lifecycle.NewMemStore(), Applier: noopApplier{},
			})
			if err != nil {
				t.Fatal(err)
			}
			observer := NewPlanLifecycleObserver(providerName != "")
			mux := http.NewServeMux()
			if err := Register(mux, Config{
				Manager: manager, Catalog: catalog, Providers: providers, LifecycleObserver: observer,
				Authenticate: func(context.Context, string, string, AccountPermission) (AccountAccess, bool, error) {
					return AccountAccess{}, false, nil
				},
				InternalAuthenticate: func(_ context.Context, token string) (bool, error) { return token == "fixture", nil },
			}); err != nil {
				t.Fatal(err)
			}
			if providerName == "fake" {
				for _, payload := range []string{`bad json`, `{"customer_id":"private_fixture","type":"payment_failed"}`} {
					rec := httptest.NewRecorder()
					mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/billing/webhook/fake", strings.NewReader(payload)))
					if rec.Code != 400 && rec.Code != 500 {
						t.Fatalf("fake fixture response = %d", rec.Code)
					}
				}
				if got := observer.StripeSnapshot(time.Now(), manager.StripeWebhookReplays()).WebhookEvents; got != (StripeWebhookEvents{}) {
					t.Fatalf("fake provider affected Stripe counters: %+v", got)
				}
			}
			for _, authorized := range []bool{false, true} {
				req := httptest.NewRequest(http.MethodPost, "/v1/plan-lifecycle:tick", strings.NewReader(`{"account_ids":[]}`))
				if authorized {
					req.Header.Set("Authorization", "Bearer fixture")
				}
				rec := httptest.NewRecorder()
				mux.ServeHTTP(rec, req)
				var doc map[string]json.RawMessage
				if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
					t.Fatal(err)
				}
				_, present := doc["stripe_observation"]
				if present != (authorized && providerName == "stripe") {
					t.Fatalf("unexpected observation visibility: authorized=%v provider=%q", authorized, providerName)
				}
				if !authorized {
					if rec.Code != http.StatusUnauthorized {
						t.Fatalf("unauthenticated tick = %d", rec.Code)
					}
					continue
				}
				if rec.Code != http.StatusOK {
					t.Fatalf("tick = %d", rec.Code)
				}
				if present {
					var observation StripeObservation
					if err := json.Unmarshal(doc["stripe_observation"], &observation); err != nil {
						t.Fatal(err)
					}
					if observation.SchemaVersion != 1 || !observation.ReconciliationObserved || !observation.ReconciliationComplete || observation.OldestPendingAt != nil || observation.ObservedAt.IsZero() {
						t.Fatalf("successful empty tick observation = %+v", observation)
					}
				}
			}
		})
	}
}

func TestStripeObservationRetainsUncertainBacklog(t *testing.T) {
	now := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	oldest := now.Add(-time.Hour)
	later := now.Add(-time.Minute)
	observer := NewPlanLifecycleObserver(true)
	steps := []struct {
		name     string
		view     BillingMutationBatchView
		failed   int
		observed bool
		complete bool
		oldest   *time.Time
		failures uint64
	}{
		{"initial scan failure", BillingMutationBatchView{}, 0, false, false, nil, 1},
		{"empty capped scan", BillingMutationBatchView{Succeeded: true, ScanCapped: true}, 0, false, false, nil, 1},
		{"known incomplete backlog", BillingMutationBatchView{ScanCapped: true, OldestObservedPendingAt: &oldest, Failed: 2}, 3, true, false, &oldest, 6},
		{"later partial sample", BillingMutationBatchView{Succeeded: true, ScanCapped: true, OldestObservedPendingAt: &later}, 0, true, false, &oldest, 6},
		{"failed empty scan", BillingMutationBatchView{}, 0, true, false, &oldest, 7},
		{"complete later sample", BillingMutationBatchView{Succeeded: true, OldestObservedPendingAt: &later}, 0, true, true, &later, 7},
		{"authoritative empty scan", BillingMutationBatchView{Succeeded: true}, 0, true, true, nil, 7},
		{"failure after empty scan", BillingMutationBatchView{}, 0, false, false, nil, 8},
	}
	for _, step := range steps {
		t.Run(step.name, func(t *testing.T) {
			observer.complete(now, PlanLifecycleSummary{Failed: step.failed, BillingMutations: step.view}, step.view.Succeeded)
			got := observer.StripeSnapshot(now, 0)
			if got.ReconciliationObserved != step.observed || got.ReconciliationComplete != step.complete || !reflect.DeepEqual(got.OldestPendingAt, step.oldest) || got.ReconciliationFailures != step.failures {
				t.Fatalf("observation = %+v", got)
			}
			if got.OldestPendingAt != nil {
				*got.OldestPendingAt = now
				if !reflect.DeepEqual(observer.StripeSnapshot(now, 0).OldestPendingAt, step.oldest) {
					t.Fatal("snapshot timestamp aliases retained observer state")
				}
			}
		})
	}
}

func TestStripeObservationConcurrentCountersAndSnapshots(t *testing.T) {
	observer := NewPlanLifecycleObserver(true)
	const workers = 64
	var wg sync.WaitGroup
	for range workers {
		wg.Go(func() {
			observer.observeStripeVerification(true)
			observer.observeStripeVerification(false)
			observer.complete(time.Now(), PlanLifecycleSummary{Failed: 1, BillingMutations: BillingMutationBatchView{Succeeded: true}}, false)
			_ = observer.StripeSnapshot(time.Now(), 0)
		})
	}
	wg.Wait()
	got := observer.StripeSnapshot(time.Now(), 0)
	if got.WebhookEvents != (StripeWebhookEvents{Verified: workers, Rejected: workers}) || got.ReconciliationFailures != workers {
		t.Fatalf("concurrent increments lost: %+v", got)
	}
}
