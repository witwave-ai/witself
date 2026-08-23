package lifecycle

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/billing"
	"github.com/witwave-ai/witself/internal/billing/stripe"
	"github.com/witwave-ai/witself/internal/plans"
)

// dunningStripeStub is the minimal Stripe API surface the dunning contract
// touches: the subscription listing ResolveEvent uses for its survivor check.
type dunningStripeStub struct {
	mu   sync.Mutex
	live bool // whether the dunned subscription still exists
}

func (s *dunningStripeStub) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet ||
			!strings.HasPrefix(r.URL.Path, "/v1/subscriptions") {
			t.Errorf("dunning contract touched unexpected Stripe API %s %s",
				r.Method, r.URL.Path)
			http.Error(w, "unexpected", http.StatusBadRequest)
			return
		}
		s.mu.Lock()
		live := s.live
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if !live {
			_, _ = fmt.Fprint(w, `{"data":[],"has_more":false}`)
			return
		}
		// Mid-dunning shape: Smart Retries keeps the subscription live with
		// status past_due while it retries the charge.
		_, _ = fmt.Fprint(w, `{"data":[{
			"id":"sub_dunned_1","customer":"cus_dunned_1","status":"past_due",
			"cancel_at_period_end":false,
			"metadata":{"witself_plan":"standard"},
			"items":{"data":[{
				"current_period_end":1790000000,
				"price":{"id":"price_standard","lookup_key":"witself_standard",
					"product":{"id":"prod_standard","metadata":{"witself_plan":"standard"}}}
			}]}
		}],"has_more":false}`)
	}
}

// deliverStripeWebhook plays one signed provider callback through the exact
// production path: HandleWebhook (signature, decode, normalize) then OnEvents
// (durable receipt, resolve, fold, apply).
func deliverStripeWebhook(
	t *testing.T,
	manager *Manager,
	provider *stripe.Provider,
	now time.Time,
	payload string,
) {
	t.Helper()
	mac := hmac.New(sha256.New, []byte("whsec_dunning_contract"))
	_, _ = fmt.Fprintf(mac, "%d.", now.Unix())
	mac.Write([]byte(payload))
	req := httptest.NewRequest(
		http.MethodPost, "/v1/billing/webhook/stripe", strings.NewReader(payload))
	req.Header.Set("Stripe-Signature", fmt.Sprintf(
		"t=%d,v1=%s", now.Unix(), hex.EncodeToString(mac.Sum(nil))))
	events, err := provider.HandleWebhook(req)
	if err != nil {
		t.Fatalf("HandleWebhook: %v", err)
	}
	if err := manager.OnEvents(context.Background(), "stripe", events); err != nil {
		t.Fatalf("OnEvents: %v", err)
	}
}

// The launch's dunning contract, end to end through real signed webhooks.
//
// Failed renewals are Stripe Smart Retries' job. While it retries, Stripe
// emits invoice.payment_failed and keeps the subscription live as past_due:
// Witself must mark the account past due and MUST NOT revoke anything early.
// When retries are exhausted, the dashboard cancellation policy cancels the
// subscription and Stripe emits customer.subscription.deleted: only then does
// the account fold back to the free plan (Personal), with the downgrade
// pushed to the cell and the dunning marker cleared.
func TestDunningContractSmartRetriesThenCancellationFoldsToFree(t *testing.T) {
	catalog, err := plans.Load()
	if err != nil {
		t.Fatal(err)
	}
	stub := &dunningStripeStub{live: true}
	server := httptest.NewServer(stub.handler(t))
	t.Cleanup(server.Close)
	now := time.Date(2026, 8, 24, 9, 0, 0, 0, time.UTC)
	clk := &clock{t: now}
	provider, err := stripe.New(stripe.Config{
		SecretKey:     "sk_test_dunning",
		WebhookSecret: "whsec_dunning_contract",
		Catalog:       catalog,
		BaseURL:       server.URL,
		Now:           clk.now,
	})
	if err != nil {
		t.Fatal(err)
	}
	store := NewMemStore()
	applier := &recApplier{}
	manager, err := NewManager(Config{
		Catalog:   catalog,
		Providers: map[string]billing.Provider{"stripe": provider},
		Default:   "stripe",
		Store:     store,
		Applier:   applier,
		Now:       clk.now,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Production state after a paid activation.
	if err := store.Put(context.Background(), Record{
		AccountID: "acct_dunned_1", Provider: "stripe",
		CustomerID: "cus_dunned_1",
		Entitled:   "standard", Applied: "standard",
		EntitledAt:            now.Add(-30 * 24 * time.Hour),
		ManagedSubscriptionID: "sub_dunned_1",
	}); err != nil {
		t.Fatal(err)
	}

	record := func() Record {
		t.Helper()
		r, ok, err := store.Get(context.Background(), "acct_dunned_1")
		if err != nil || !ok {
			t.Fatalf("record ok=%v err=%v", ok, err)
		}
		return r
	}

	// Phase 1 — a renewal charge fails; Smart Retries is now working the
	// card. The account is marked past due and keeps everything it paid for.
	failedAt := now.Add(-time.Hour)
	paymentFailed := fmt.Sprintf(`{
		"id":"evt_dunning_fail_1","type":"invoice.payment_failed","created":%d,
		"data":{"object":{"id":"in_dunned_1","customer":"cus_dunned_1","subscription":"sub_dunned_1"}}
	}`, failedAt.Unix())
	deliverStripeWebhook(t, manager, provider, now, paymentFailed)
	afterFail := record()
	if afterFail.Entitled != "standard" || afterFail.Applied != "standard" {
		t.Fatalf("payment failure revoked the plan early: entitled=%q applied=%q; "+
			"dunning is Smart Retries' job, not an instant downgrade",
			afterFail.Entitled, afterFail.Applied)
	}
	if afterFail.PastDueSince == nil ||
		!afterFail.PastDueSince.Equal(failedAt.Truncate(time.Second)) {
		t.Fatalf("PastDueSince = %v, want the first failure time %v",
			afterFail.PastDueSince, failedAt.Truncate(time.Second))
	}

	// Stripe redelivers webhooks; an exact redelivery must change nothing.
	deliverStripeWebhook(t, manager, provider, now, paymentFailed)
	if r := record(); r.PastDueSince == nil || r.Entitled != "standard" {
		t.Fatalf("redelivery disturbed the dunning state: %+v", r)
	}

	// Phase 2 — Smart Retries exhausted; the dashboard cancellation policy
	// cancels the subscription. Stripe deletes it and announces it.
	stub.mu.Lock()
	stub.live = false
	stub.mu.Unlock()
	canceledAt := now.Add(-time.Minute)
	subscriptionDeleted := fmt.Sprintf(`{
		"id":"evt_dunning_cancel_1","type":"customer.subscription.deleted","created":%d,
		"data":{"object":{"id":"sub_dunned_1","customer":"cus_dunned_1"}}
	}`, canceledAt.Unix())
	deliverStripeWebhook(t, manager, provider, now, subscriptionDeleted)

	final := record()
	if final.Entitled != plans.Free {
		t.Fatalf("after terminal cancellation entitled = %q, want %q (Personal)",
			final.Entitled, plans.Free)
	}
	if final.Applied != plans.Free {
		t.Fatalf("after terminal cancellation applied = %q, want %q — the "+
			"downgrade must reach the cell, not only the billing record",
			final.Applied, plans.Free)
	}
	if final.ManagedSubscriptionID != "" {
		t.Fatalf("managed subscription id = %q, want cleared",
			final.ManagedSubscriptionID)
	}
	if final.PastDueSince != nil {
		t.Fatalf("PastDueSince = %v, want cleared: there is no longer a "+
			"failing renewal to be past due on", final.PastDueSince)
	}
	last := applier.last(t)
	if last.accountID != "acct_dunned_1" || last.plan != plans.Free {
		t.Fatalf("last cell apply = %+v, want the free plan for acct_dunned_1", last)
	}
}
