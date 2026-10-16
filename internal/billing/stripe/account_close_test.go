package stripe

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/witwave-ai/witself/internal/plans"
)

type closureStripeStub struct {
	account          string
	sessionStatus    string
	sessionMode      string
	statuses         map[string]string
	foreignPlan      bool
	foreignCustomer  bool
	listOverride     string
	checkoutOverride string
	lostDeleteReply  bool
	lostExpiryReply  bool
	completeRace     bool
	failDelete       bool
	wrongDeleteReply bool
	newSubOnReadback bool
	expired          int
	deleted          int
	deleteForms      []url.Values
	keys             []string
}

func (s *closureStripeStub) session() map[string]any {
	return map[string]any{"id": "cs_open", "customer": "cus_owned", "mode": s.sessionMode,
		"status": s.sessionStatus, "subscription": "sub_race", "metadata": map[string]string{"witself_plan": "standard"}}
}

func (s *closureStripeStub) sub(id string) map[string]any {
	customer := "cus_owned"
	if s.foreignCustomer {
		customer = "cus_foreign"
	}
	plan := "standard"
	if s.foreignPlan {
		plan = "foreign"
	}
	return map[string]any{"id": id, "customer": customer, "status": s.statuses[id],
		"cancel_at_period_end": true, "metadata": map[string]string{"witself_plan": plan},
		"items": map[string]any{"data": []any{map[string]any{"price": map[string]any{
			"id": "price_owned", "lookup_key": lookupKey("standard"), "product": "prod_owned"}}}}}
}

func (s *closureStripeStub) handle(w http.ResponseWriter, r *http.Request) {
	write := func(v any) { _ = json.NewEncoder(w).Encode(v) }
	switch {
	case r.Method == "GET" && r.URL.Path == "/v1/customers/cus_owned":
		write(map[string]any{"id": "cus_owned", "metadata": map[string]string{"witself_account": s.account}})
	case r.Method == "GET" && r.URL.Path == "/v1/checkout/sessions":
		if r.URL.Query().Get("status") != "open" || r.URL.Query().Get("customer") != "cus_owned" {
			http.Error(w, "invalid filter", http.StatusBadRequest)
			return
		}
		if s.checkoutOverride != "" {
			_, _ = io.WriteString(w, s.checkoutOverride)
			return
		}
		data := []any{}
		if s.sessionStatus == "open" {
			data = append(data, s.session())
		}
		write(map[string]any{"data": data, "has_more": false})
	case r.Method == "POST" && r.URL.Path == "/v1/checkout/sessions/cs_open/expire":
		s.keys = append(s.keys, r.Header.Get("Idempotency-Key"))
		s.expired++
		s.sessionStatus = "expired"
		if s.completeRace {
			s.sessionStatus = "complete"
			s.statuses["sub_race"] = "incomplete"
			http.Error(w, "already completed", http.StatusBadRequest)
			return
		}
		if s.lostExpiryReply {
			http.Error(w, "lost reply", http.StatusBadGateway)
			return
		}
		write(s.session())
	case r.Method == "GET" && r.URL.Path == "/v1/checkout/sessions/cs_open":
		write(s.session())
	case r.Method == "GET" && r.URL.Path == "/v1/subscriptions":
		if r.URL.Query().Get("status") != "all" || r.URL.Query().Get("customer") != "cus_owned" {
			http.Error(w, "invalid filter", http.StatusBadRequest)
			return
		}
		if s.listOverride != "" {
			_, _ = io.WriteString(w, s.listOverride)
			return
		}
		if s.newSubOnReadback && s.deleted > 0 {
			s.statuses["sub_new"] = "active"
		}
		data := []any{}
		for id := range s.statuses {
			data = append(data, s.sub(id))
		}
		write(map[string]any{"data": data, "has_more": false})
	case strings.HasPrefix(r.URL.Path, "/v1/subscriptions/"):
		id := strings.TrimPrefix(r.URL.Path, "/v1/subscriptions/")
		if _, ok := s.statuses[id]; !ok {
			http.NotFound(w, r)
			return
		}
		if r.Method == "DELETE" {
			body, _ := io.ReadAll(r.Body)
			form, _ := url.ParseQuery(string(body))
			s.deleteForms = append(s.deleteForms, form)
			s.keys = append(s.keys, r.Header.Get("Idempotency-Key"))
			s.deleted++
			if s.failDelete {
				http.Error(w, "private provider error", http.StatusServiceUnavailable)
				return
			}
			s.statuses[id] = "canceled"
			if s.lostDeleteReply {
				http.Error(w, "reply lost", http.StatusBadGateway)
				return
			}
			if s.wrongDeleteReply {
				write(map[string]any{"id": "sub_wrong", "customer": "cus_owned", "status": "canceled"})
				return
			}
		}
		write(s.sub(id))
	default:
		http.NotFound(w, r)
	}
}

func newClosureStripe(t *testing.T) (*closureStripeStub, *Provider) {
	t.Helper()
	s := &closureStripeStub{account: "acct_owned", sessionMode: "subscription", statuses: map[string]string{"sub_owned": "active"}}
	srv := httptest.NewServer(http.HandlerFunc(s.handle))
	t.Cleanup(srv.Close)
	catalog, err := plans.Load()
	if err != nil {
		t.Fatal(err)
	}
	p, err := New(Config{SecretKey: "sk_test_stub", WebhookSecret: "whsec_stub", Catalog: catalog, BaseURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	return s, p
}

func TestAccountClosureCancelsEveryChargeableStatusAndExpiresCheckout(t *testing.T) {
	s, p := newClosureStripe(t)
	s.sessionStatus = "open"
	s.statuses = map[string]string{}
	for _, status := range []string{"active", "trialing", "past_due", "unpaid", "incomplete", "paused", "canceled", "incomplete_expired"} {
		s.statuses["sub_"+status] = status
	}
	if err := p.CancelForAccountClosure(context.Background(), "cus_owned", "acct_owned", "op_close"); err != nil {
		t.Fatal(err)
	}
	if s.expired != 1 || s.deleted != 6 {
		t.Fatalf("expiry=%d cancellation=%d", s.expired, s.deleted)
	}
	for _, form := range s.deleteForms {
		if form.Get("invoice_now") != "false" || form.Get("prorate") != "false" {
			t.Fatalf("unexpected cancellation form: %v", form)
		}
	}
	for _, key := range s.keys {
		if key == "" {
			t.Fatal("missing stable operation key")
		}
	}
	if err := p.CancelForAccountClosure(context.Background(), "cus_owned", "acct_owned", "op_close"); err != nil {
		t.Fatal(err)
	}
	if s.expired != 1 || s.deleted != 6 {
		t.Fatal("retry repeated terminal provider mutations")
	}
}

func TestAccountClosureProviderRaces(t *testing.T) {
	for _, scenario := range []string{"lost cancellation response", "lost expiry response", "checkout completes during expiry"} {
		t.Run(scenario, func(t *testing.T) {
			s, p := newClosureStripe(t)
			s.sessionStatus = "open"
			s.lostDeleteReply = scenario == "lost cancellation response"
			s.lostExpiryReply = scenario == "lost expiry response"
			s.completeRace = scenario == "checkout completes during expiry"
			if err := p.CancelForAccountClosure(context.Background(), "cus_owned", "acct_owned", "op_close"); err != nil {
				t.Fatal(err)
			}
			for _, status := range s.statuses {
				if status != "canceled" {
					t.Fatal("chargeable subscription survived closure")
				}
			}
		})
	}
}

func TestAccountClosureProviderFailsClosed(t *testing.T) {
	tests := []struct {
		name string
		edit func(*closureStripeStub)
	}{
		{"wrong customer owner", func(s *closureStripeStub) { s.account = "acct_other" }},
		{"foreign subscription", func(s *closureStripeStub) { s.foreignCustomer = true }},
		{"foreign plan", func(s *closureStripeStub) { s.foreignPlan = true }},
		{"unknown status", func(s *closureStripeStub) { s.statuses["sub_owned"] = "future_status" }},
		{"subscription pagination", func(s *closureStripeStub) { s.listOverride = `{"data":[],"has_more":true}` }},
		{"missing subscription projection", func(s *closureStripeStub) { s.listOverride = `{}` }},
		{"missing pagination proof", func(s *closureStripeStub) { s.listOverride = `{"data":[]}` }},
		{"checkout pagination", func(s *closureStripeStub) { s.checkoutOverride = `{"data":[],"has_more":true}` }},
		{"missing checkout projection", func(s *closureStripeStub) { s.checkoutOverride = `{}` }},
		{"provider unavailable", func(s *closureStripeStub) { s.failDelete = true }},
		{"wrong cancellation response", func(s *closureStripeStub) { s.wrongDeleteReply = true }},
		{"new subscription at readback", func(s *closureStripeStub) { s.newSubOnReadback = true }},
		{"unrecognized checkout", func(s *closureStripeStub) { s.sessionStatus, s.sessionMode = "open", "payment" }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s, p := newClosureStripe(t)
			tc.edit(s)
			if err := p.CancelForAccountClosure(context.Background(), "cus_owned", "acct_owned", "op_close"); err != errClosureUnconfirmed {
				t.Fatalf("want value-free refusal, got %v", err)
			}
		})
	}
}

func TestAccountClosureRetriesProviderFailure(t *testing.T) {
	s, p := newClosureStripe(t)
	s.failDelete = true
	if p.CancelForAccountClosure(context.Background(), "cus_owned", "acct_owned", "op_close") == nil {
		t.Fatal("unconfirmed cancellation succeeded")
	}
	s.failDelete = false
	if err := p.CancelForAccountClosure(context.Background(), "cus_owned", "acct_owned", "op_close"); err != nil {
		t.Fatal(err)
	}
	if len(s.keys) != 2 || s.keys[0] != s.keys[1] {
		t.Fatal("retry changed provider operation identity")
	}
}
