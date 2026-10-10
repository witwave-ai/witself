package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	budgetTestAccountA = "acc_aaaaaaaaaaaaaaaa"
	budgetTestAccountB = "acc_bbbbbbbbbbbbbbbb"
)

type budgetTestClock struct {
	mu  sync.Mutex
	now time.Time
}

func newBudgetTestClock() *budgetTestClock {
	return &budgetTestClock{now: time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)}
}

func (c *budgetTestClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *budgetTestClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

func budgetTestConfig(readRate, readBurst, writeRate, writeBurst int) AccountRequestBudgetConfig {
	return AccountRequestBudgetConfig{
		Enabled: true, ReadPerMinute: readRate, ReadBurst: readBurst,
		WritePerMinute: writeRate, WriteBurst: writeBurst, MaxTrackedAccounts: 100,
	}
}

func budgetTestAdmit(t *testing.T, b *accountRequestBudget, accountID, class string, want bool, retryAfter int64) {
	t.Helper()
	got, retry := b.admit(accountID, class)
	if got != want || retry != retryAfter {
		t.Fatalf("admit(%q, %q) = (%v, %d), want (%v, %d)", accountID, class, got, retry, want, retryAfter)
	}
}

func TestAccountRequestBudgetGCRA(t *testing.T) {
	clock := newBudgetTestClock()
	b := newAccountRequestBudget(budgetTestConfig(60, 3, 60, 3), clock.Now)
	for range 3 {
		budgetTestAdmit(t, b, "a", "read", true, 0)
	}
	budgetTestAdmit(t, b, "a", "read", false, 1)
	clock.Advance(time.Second)
	budgetTestAdmit(t, b, "a", "read", true, 0)
	budgetTestAdmit(t, b, "a", "read", false, 1)
	clock.Advance(time.Hour)
	for range 3 {
		budgetTestAdmit(t, b, "a", "read", true, 0)
	}
	budgetTestAdmit(t, b, "a", "read", false, 1)
	for range 10 {
		budgetTestAdmit(t, b, "a", "read", false, 1)
	}
	clock.Advance(time.Second)
	budgetTestAdmit(t, b, "a", "read", true, 0)
	budgetTestAdmit(t, b, "a", "read", false, 1)
	for _, tc := range []struct {
		rate  int
		retry int64
	}{{7, 9}, {120, 1}, {1, 60}} {
		t.Run(fmt.Sprint(tc.rate), func(t *testing.T) {
			b := newAccountRequestBudget(budgetTestConfig(tc.rate, 1, tc.rate, 1), clock.Now)
			budgetTestAdmit(t, b, "a", "read", true, 0)
			budgetTestAdmit(t, b, "a", "read", false, tc.retry)
		})
	}
}

func TestAccountRequestBudgetClassesAreSeparate(t *testing.T) {
	for _, first := range []string{"read", "write"} {
		t.Run(first, func(t *testing.T) {
			clock := newBudgetTestClock()
			b := newAccountRequestBudget(budgetTestConfig(60, 3, 60, 3), clock.Now)
			second := "write"
			if first == "write" {
				second = "read"
			}
			for range 3 {
				budgetTestAdmit(t, b, "a", first, true, 0)
			}
			budgetTestAdmit(t, b, "a", first, false, 1)
			for range 3 {
				budgetTestAdmit(t, b, "a", second, true, 0)
			}
			budgetTestAdmit(t, b, "a", second, false, 1)
		})
	}
}

func TestAccountRequestBudgetClassification(t *testing.T) {
	cases := []struct{ method, path, class string }{
		{"GET", "/v1/realms", "read"}, {"HEAD", "/v1/realms", "read"},
		{"GET", "/v1/memories:recall", "read"}, {"HEAD", "/v1/memories:recall", "read"},
		{"POST", "/v1/memories:recall", "read"}, {"POST", "/v1/messages:listen", "read"},
		{"POST", "/v1/email:listen", "read"}, {"POST", "/v1/memories", "write"},
		{"POST", "/v1/memories:recall/", "write"}, {"POST", "/v1/memories:recallx", "write"},
		{"POST", "/v1/messages/msg_1:read", "write"}, {"POST", "/v1/secrets/s/fields/f:access", "write"},
		{"POST", "/v1/email/retry-canary:status", "write"}, {"PUT", "/v1/realms", "write"},
		{"PATCH", "/v1/realms", "write"}, {"DELETE", "/v1/realms", "write"},
		{"OPTIONS", "/v1/realms", "write"}, {"get", "/v1/realms", "write"},
		{"CUSTOM", "/v1/memories:recall", "write"},
	}
	for _, tc := range cases {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			r := httptest.NewRequest(tc.method, tc.path, nil)
			if got := classifyAccountRequest(r); got != tc.class {
				t.Fatalf("class = %q, want %q", got, tc.class)
			}
		})
	}
}

type budgetTestStubs struct {
	mu    sync.Mutex
	calls map[string]map[string]int
}

func (s *budgetTestStubs) record(operation, key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.calls[operation] == nil {
		s.calls[operation] = make(map[string]int)
	}
	s.calls[operation][key]++
}

func (s *budgetTestStubs) count(operation, key string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls[operation][key]
}

func newBudgetTestServerConfig(t *testing.T, budget AccountRequestBudgetConfig, clock *budgetTestClock, noTicket bool) (Config, *budgetTestStubs) {
	t.Helper()
	s := &budgetTestStubs{calls: make(map[string]map[string]int)}
	cfg := Config{
		AccountRequestBudget: budget, accountRequestBudgetNow: clock.Now,
		Authenticate: func(ctx context.Context, tok string) (string, string, string, bool, error) {
			s.record("auth", tok)
			if noTicket && ctx.Value(accountRequestBudgetTicketKey{}) != nil {
				t.Error("disabled request carries a budget ticket")
			}
			switch tok {
			case "tok-a":
				return "opr_a", budgetTestAccountA, "active", true, nil
			case "tok-b":
				return "opr_b", budgetTestAccountB, "active", true, nil
			case "tok-err":
				return "", "", "", false, errors.New("authentication unavailable")
			default:
				return "", "", "", false, nil
			}
		},
		AuthenticatePrincipal: func(_ context.Context, tok string) (DomainPrincipal, bool, error) {
			s.record("principal-auth", tok)
			var accountID string
			switch tok {
			case "agent-a":
				accountID = budgetTestAccountA
			case "agent-b":
				accountID = budgetTestAccountB
			default:
				return DomainPrincipal{}, false, nil
			}
			return DomainPrincipal{Kind: PrincipalKindAgent, ID: tok, AccountID: accountID, AccountStatus: "active", AccessProfile: AccessProfileFull, RealmID: "realm_test"}, true, nil
		},
		ListRealms: func(_ context.Context, accountID string) ([]Realm, error) {
			s.record("list-realms", accountID)
			return []Realm{}, nil
		},
		CreateRealm: func(_ context.Context, accountID, name string) (Realm, error) {
			s.record("create-realm", accountID)
			return Realm{ID: "rlm_test", Name: name}, nil
		},
		ListFactSubjects: func(_ context.Context, p DomainPrincipal) ([]FactSubject, error) {
			s.record("list-subjects", p.AccountID)
			return []FactSubject{}, nil
		},
	}
	return cfg, s
}

func budgetTestRequest(h http.Handler, method, path, tok, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	if tok != "" {
		r.Header.Set("Authorization", "Bearer "+tok)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func budgetTestStatus(t *testing.T, h http.Handler, method, path, tok, body string, want int) *httptest.ResponseRecorder {
	t.Helper()
	w := budgetTestRequest(h, method, path, tok, body)
	if w.Code != want {
		t.Fatalf("%s %s = %d, want %d: %s", method, path, w.Code, want, w.Body.String())
	}
	return w
}

func budgetTestScrape(t *testing.T, m *runtimeMetrics) string {
	t.Helper()
	h := metricsMuxFor(m, nil, nil, nil, nil, nil, nil, nil, nil)
	return budgetTestStatus(t, h, "GET", "/metrics", "", "", http.StatusOK).Body.String()
}

func budgetTestMetrics(t *testing.T, scrape string, read, write, tracked, evictions int) {
	t.Helper()
	for _, line := range []string{
		fmt.Sprintf("witself_account_request_budget_rejections_total{class=\"read\"} %d", read),
		fmt.Sprintf("witself_account_request_budget_rejections_total{class=\"write\"} %d", write),
		fmt.Sprintf("witself_account_request_budget_tracked_accounts %d", tracked),
		fmt.Sprintf("witself_account_request_budget_capacity_evictions_total %d", evictions),
	} {
		if !strings.Contains("\n"+scrape, "\n"+line+"\n") {
			t.Errorf("scrape missing exact sample %q", line)
		}
	}
	for _, private := range []string{budgetTestAccountA, budgetTestAccountB, "tok-a", "tok-b", "agent-a", "agent-b"} {
		if strings.Contains(scrape, private) {
			t.Errorf("scrape contains private test value")
		}
	}
}

func TestAccountRequestBudgetNoisyAccountDoesNotAffectQuietAccount(t *testing.T) {
	clock := newBudgetTestClock()
	cfg, stubs := newBudgetTestServerConfig(t, budgetTestConfig(60, 5, 60, 3), clock, false)
	srv := httptest.NewServer(apiMux(cfg))
	defer srv.Close()
	request := func(method, path, tok, body string) (int, error) {
		r, err := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
		if err != nil {
			return 0, err
		}
		r.Header.Set("Authorization", "Bearer "+tok)
		resp, err := srv.Client().Do(r)
		if err != nil {
			return 0, err
		}
		_, readErr := io.Copy(io.Discard, resp.Body)
		closeErr := resp.Body.Close()
		if readErr != nil {
			return 0, readErr
		}
		return resp.StatusCode, closeErr
	}
	var wg sync.WaitGroup
	statuses := make(chan int, 320)
	errs := make(chan error, 320)
	for range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 10 {
				status, err := request("GET", "/v1/realms", "tok-a", "")
				if err != nil {
					errs <- err
				} else {
					statuses <- status
				}
			}
		}()
	}
	wg.Wait()
	close(statuses)
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	counts := make(map[int]int)
	for status := range statuses {
		counts[status]++
	}
	if !reflect.DeepEqual(counts, map[int]int{200: 5, 429: 315}) {
		t.Fatalf("flood statuses = %v", counts)
	}
	if got := stubs.count("list-realms", budgetTestAccountA); got != 5 {
		t.Fatalf("A list calls = %d", got)
	}
	if got := stubs.count("auth", "tok-a"); got != 320 {
		t.Fatalf("flood authentication calls = %d", got)
	}
	check := func(method, path, tok, body string, want int) {
		t.Helper()
		got, err := request(method, path, tok, body)
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("%s %s using %s = %d, want %d", method, path, tok, got, want)
		}
	}
	for range 4 {
		check("GET", "/v1/realms", "tok-b", "", 200)
	}
	for i := 1; i <= 3; i++ {
		check("POST", "/v1/realms", "tok-b", fmt.Sprintf(`{"name":"b%d"}`, i), 201)
	}
	check("GET", "/v1/fact-subjects", "agent-b", "", 200)
	check("GET", "/v1/realms", "tok-b", "", 429)
	check("POST", "/v1/realms", "tok-b", `{"name":"b4"}`, 429)
	for i := 1; i <= 3; i++ {
		check("POST", "/v1/realms", "tok-a", fmt.Sprintf(`{"name":"a%d"}`, i), 201)
	}
	check("POST", "/v1/realms", "tok-a", `{"name":"a4"}`, 429)
	for _, accountID := range []string{budgetTestAccountA, budgetTestAccountB} {
		if got := stubs.count("create-realm", accountID); got != 3 {
			t.Errorf("create calls for %s = %d, want 3", accountID, got)
		}
	}
	check("GET", "/v1/fact-subjects", "agent-a", "", 429)
	if stubs.count("list-subjects", budgetTestAccountA) != 0 || stubs.count("list-subjects", budgetTestAccountB) != 1 {
		t.Fatal("fact subjects must only reach B's handler once")
	}
	clock.Advance(time.Second)
	check("GET", "/v1/realms", "tok-a", "", 200)
	check("GET", "/v1/realms", "tok-a", "", 429)
	check("GET", "/v1/realms", "tok-b", "", 200)
}

func budgetTestEnvelope(t *testing.T, w *httptest.ResponseRecorder, class string) {
	t.Helper()
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", w.Code)
	}
	for name, want := range map[string]string{"Retry-After": "1", "Content-Type": "application/json", "Cache-Control": "private, no-store"} {
		if got := w.Header().Get(name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	var got map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	dimension := "account_request_" + class
	want := map[string]any{
		"schema_version": "witself.v0", "code": "rate_limited", "error": "account request rate limit reached",
		"retryable": true, "retry_after": float64(1),
		"details": map[string]any{"limit_dimension": dimension, "limit_key": dimension + "_per_account_minute", "scope": "account", "retry_after": float64(1)},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("refusal = %#v, want %#v", got, want)
	}
	for _, value := range []string{budgetTestAccountA, budgetTestAccountB, "tok-a", "tok-b", "agent-a", "agent-b", "opr_a", "opr_b", "realm_test"} {
		if strings.Contains(w.Body.String(), value) || strings.Contains(fmt.Sprint(w.Header()), value) {
			t.Error("refusal contains private test value")
		}
	}
}

func TestAccountRequestBudgetRefusalEnvelopeAndMetrics(t *testing.T) {
	clock := newBudgetTestClock()
	cfg, _ := newBudgetTestServerConfig(t, budgetTestConfig(60, 1, 60, 1), clock, false)
	m := newRuntimeMetrics()
	cfg = m.instrumentConfig(cfg)
	h := m.instrument(apiMux(cfg))
	budgetTestMetrics(t, budgetTestScrape(t, m), 0, 0, 0, 0)
	budgetTestStatus(t, h, "GET", "/v1/realms", "tok-a", "", 200)
	for range 2 {
		budgetTestEnvelope(t, budgetTestRequest(h, "GET", "/v1/realms", "tok-a", ""), "read")
	}
	budgetTestStatus(t, h, "POST", "/v1/realms", "tok-a", `{"name":"r1"}`, 201)
	budgetTestEnvelope(t, budgetTestRequest(h, "POST", "/v1/realms", "tok-a", `{"name":"r2"}`), "write")
	budgetTestStatus(t, h, "GET", "/v1/realms", "tok-b", "", 200)
	scrape := budgetTestScrape(t, m)
	budgetTestMetrics(t, scrape, 2, 1, 2, 0)
	if strings.Contains(scrape, `route="unmatched"`) {
		t.Error("budget wrapper lost the route pattern")
	}
	found := false
	for _, line := range strings.Split(scrape, "\n") {
		if strings.HasPrefix(line, "witself_http_requests_total{") && strings.Contains(line, `route="/v1/realms"`) && strings.Contains(line, `status_class="4xx"`) {
			found = true
		}
	}
	if !found {
		t.Error("429 was not counted under /v1/realms with status_class=4xx")
	}
}

func TestAccountRequestBudgetNeverBudgetsUnauthenticatedOrSystemRoutes(t *testing.T) {
	clock := newBudgetTestClock()
	cfg, stubs := newBudgetTestServerConfig(t, budgetTestConfig(1, 1, 1, 1), clock, false)
	cfg.ProvisionToken = "prov-token"
	cfg.ListAdminEventsAll = func(_ context.Context, _ EventFilter) (EventPage, error) {
		stubs.record("admin-events", "all")
		return EventPage{}, nil
	}
	m := newRuntimeMetrics()
	h := apiMux(m.instrumentConfig(cfg))
	eventStatus := 0
	for range 20 {
		budgetTestStatus(t, h, "GET", "/v1/version", "", "", 200)
		budgetTestStatus(t, h, "GET", "/v1/capabilities", "", "", 200)
		budgetTestStatus(t, h, "GET", "/v1/realms", "", "", 401)
		budgetTestStatus(t, h, "GET", "/v1/realms", "tok-x", "", 401)
		budgetTestStatus(t, h, "GET", "/v1/realms", "tok-err", "", 500)
		w := budgetTestRequest(h, "POST", "/v1/events/admin:list", "prov-token", "")
		if eventStatus == 0 {
			eventStatus = w.Code
		}
		if w.Code == 429 || w.Code != eventStatus {
			t.Fatalf("system event status = %d, initial %d", w.Code, eventStatus)
		}
	}
	budgetTestStatus(t, h, "GET", "/v1/realms", "tok-a", "", 200)
	budgetTestMetrics(t, budgetTestScrape(t, m), 0, 0, 1, 0)
	if stubs.count("auth", "tok-x") != 20 || stubs.count("auth", "tok-err") != 20 || stubs.count("auth", "tok-a") != 1 || stubs.count("auth", "") != 0 {
		t.Error("unexpected authentication call counts")
	}
}

func TestAccountRequestBudgetPrecedesStatusAndProfileGates(t *testing.T) {
	for _, route := range []struct {
		name, path string
	}{
		{"domain", "/v1/fact-subjects"},
		{"email", "/v1/email/sent"},
		{"secret-safety", "/v1/vault/rotations/open"},
	} {
		for _, principal := range []struct {
			status, profile string
		}{
			{"suspended", AccessProfileFull},
			{"pending", AccessProfileFull},
			{"active", AccessProfileCuratorPreview},
			{"active", AccessProfileCuratorApply},
		} {
			// Full agents on suspended accounts may use this safety read.
			if route.name == "secret-safety" && principal.status == "suspended" {
				continue
			}
			t.Run(route.name+"/"+principal.status+"/"+principal.profile, func(t *testing.T) {
				clock := newBudgetTestClock()
				cfg, _ := newBudgetTestServerConfig(t, budgetTestConfig(60, 1, 60, 1), clock, false)
				authCalls, handlerCalls, entitlementCalls := 0, 0, 0
				cfg.AuthenticatePrincipal = func(context.Context, string) (DomainPrincipal, bool, error) {
					authCalls++
					return DomainPrincipal{
						Kind: PrincipalKindAgent, ID: "agent-a", AccountID: budgetTestAccountA,
						AccountStatus: principal.status, AccessProfile: principal.profile, RealmID: "realm_test",
					}, true, nil
				}
				cfg.ListFactSubjects = func(context.Context, DomainPrincipal) ([]FactSubject, error) {
					handlerCalls++
					return []FactSubject{}, nil
				}
				cfg.ListAgentEmailOutbox = func(context.Context, DomainPrincipal, AgentEmailOutboundListOptions) (AgentEmailOutboundPage, error) {
					handlerCalls++
					return AgentEmailOutboundPage{}, nil
				}
				cfg.GetOpenVaultKeyRotation = func(context.Context, DomainPrincipal) (*VaultKeyRotation, error) {
					handlerCalls++
					return nil, nil
				}
				cfg.RequireAgentEmailSendEntitlement = func(context.Context, DomainPrincipal) error {
					entitlementCalls++
					return nil
				}
				h := apiMux(cfg)
				budgetTestStatus(t, h, "GET", route.path, "agent-a", "", http.StatusForbidden)
				budgetTestEnvelope(t, budgetTestRequest(h, "GET", route.path, "agent-a", ""), "read")
				if authCalls != 2 || handlerCalls != 0 || entitlementCalls != 0 {
					t.Fatalf("auth=%d handler=%d entitlement=%d calls, want 2, 0, 0", authCalls, handlerCalls, entitlementCalls)
				}
			})
		}
	}
}

func TestAccountRequestBudgetOffByDefaultIsInert(t *testing.T) {
	clock := newBudgetTestClock()
	t.Run("zero-has-no-ticket", func(t *testing.T) {
		cfg, stubs := newBudgetTestServerConfig(t, AccountRequestBudgetConfig{}, clock, true)
		h := apiMux(cfg)
		for i := range 200 {
			budgetTestStatus(t, h, "GET", "/v1/realms", "tok-a", "", 200)
			budgetTestStatus(t, h, "POST", "/v1/realms", "tok-a", fmt.Sprintf(`{"name":"realm%d"}`, i), 201)
		}
		if stubs.count("auth", "tok-a") != 400 || stubs.count("list-realms", budgetTestAccountA) != 200 || stubs.count("create-realm", budgetTestAccountA) != 200 {
			t.Error("disabled requests did not all reach auth and handlers")
		}
	})
	t.Run("disabled-values-equal-zero", func(t *testing.T) {
		zero, _ := newBudgetTestServerConfig(t, AccountRequestBudgetConfig{}, clock, true)
		disabled := budgetTestConfig(1, 1, 1, 1)
		disabled.Enabled = false
		set, _ := newBudgetTestServerConfig(t, disabled, clock, true)
		h1, h2 := apiMux(zero), apiMux(set)
		requests := []struct{ method, path, tok, body string }{
			{"GET", "/v1/realms", "tok-a", ""}, {"POST", "/v1/realms", "tok-a", `{"name":"fixed"}`},
			{"GET", "/v1/version", "", ""}, {"GET", "/v1/realms", "", ""}, {"GET", "/v1/realms", "tok-x", ""},
		}
		for range 50 {
			for _, req := range requests {
				a := budgetTestRequest(h1, req.method, req.path, req.tok, req.body)
				b := budgetTestRequest(h2, req.method, req.path, req.tok, req.body)
				a.Header().Del("Date")
				b.Header().Del("Date")
				if a.Code != b.Code || !reflect.DeepEqual(a.Header(), b.Header()) || !bytes.Equal(a.Body.Bytes(), b.Body.Bytes()) {
					t.Fatalf("disabled config changes %s %s response", req.method, req.path)
				}
			}
		}
	})
	t.Run("no-metrics", func(t *testing.T) {
		m := newRuntimeMetrics()
		cfg, _ := newBudgetTestServerConfig(t, AccountRequestBudgetConfig{}, clock, true)
		h := apiMux(m.instrumentConfig(cfg))
		budgetTestStatus(t, h, "GET", "/v1/realms", "tok-a", "", 200)
		if strings.Contains(budgetTestScrape(t, m), "witself_account_request_budget") {
			t.Error("disabled budget emits metrics")
		}
	})
	t.Run("empty-environment", func(t *testing.T) {
		var keys []string
		got, err := ParseAccountRequestBudgetConfig(func(key string) (string, bool) { keys = append(keys, key); return "", false })
		if err != nil || got != (AccountRequestBudgetConfig{}) {
			t.Fatalf("empty environment = %+v, %v", got, err)
		}
		if !reflect.DeepEqual(keys, []string{"WITSELF_ACCOUNT_REQUEST_BUDGET_ENABLED"}) {
			t.Fatalf("lookups = %v", keys)
		}
	})
}

func TestParseAccountRequestBudgetConfig(t *testing.T) {
	const prefix = "WITSELF_ACCOUNT_REQUEST_BUDGET_"
	rates := []string{"READ_PER_MINUTE", "READ_BURST", "WRITE_PER_MINUTE", "WRITE_BURST"}
	valid := func() map[string]string {
		env := map[string]string{prefix + "ENABLED": "true"}
		for _, name := range rates {
			env[prefix+name] = " 10 "
		}
		return env
	}
	parse := func(env map[string]string) (AccountRequestBudgetConfig, []string, error) {
		var keys []string
		cfg, err := ParseAccountRequestBudgetConfig(func(key string) (string, bool) { keys = append(keys, key); value, ok := env[key]; return value, ok })
		return cfg, keys, err
	}
	t.Run("disabled-ignores-junk", func(t *testing.T) {
		env := valid()
		env[prefix+"ENABLED"] = " false "
		for _, name := range append(append([]string{}, rates...), "MAX_ACCOUNTS") {
			env[prefix+name] = "junk"
		}
		cfg, keys, err := parse(env)
		if err != nil || cfg != (AccountRequestBudgetConfig{}) || !reflect.DeepEqual(keys, []string{prefix + "ENABLED"}) {
			t.Fatalf("disabled parse = %+v, %v, %v", cfg, keys, err)
		}
	})
	t.Run("invalid-boolean", func(t *testing.T) {
		_, keys, err := parse(map[string]string{prefix + "ENABLED": "yes"})
		if err == nil || err.Error() != prefix+"ENABLED must be a boolean" {
			t.Fatalf("error = %v", err)
		}
		if !reflect.DeepEqual(keys, []string{prefix + "ENABLED"}) {
			t.Fatalf("lookups = %v", keys)
		}
	})
	t.Run("valid-default-max-and-lookup-order", func(t *testing.T) {
		got, keys, err := parse(valid())
		want := budgetTestConfig(10, 10, 10, 10)
		want.MaxTrackedAccounts = 10000
		if err != nil || got != want {
			t.Fatalf("parse = %+v, %v; want %+v", got, err, want)
		}
		wantKeys := []string{prefix + "ENABLED", prefix + rates[0], prefix + rates[1], prefix + rates[2], prefix + rates[3], prefix + "MAX_ACCOUNTS"}
		if !reflect.DeepEqual(keys, wantKeys) {
			t.Fatalf("lookup order = %v, want %v", keys, wantKeys)
		}
	})
	for i, name := range rates {
		t.Run(name+"/missing", func(t *testing.T) {
			env := valid()
			delete(env, prefix+name)
			_, keys, err := parse(env)
			want := prefix + name + " is required when WITSELF_ACCOUNT_REQUEST_BUDGET_ENABLED is true"
			if err == nil || err.Error() != want {
				t.Fatalf("error = %v, want %s", err, want)
			}
			if len(keys) != i+2 || keys[len(keys)-1] != prefix+name {
				t.Fatalf("parser did not stop at first missing rate: %v", keys)
			}
		})
		for _, raw := range []string{"0", "-1", "1.5", "abc", "1000001", "7777777"} {
			t.Run(name+"/"+raw, func(t *testing.T) {
				env := valid()
				env[prefix+name] = raw
				_, keys, err := parse(env)
				want := prefix + name + " must be an integer from 1 to 1000000"
				if err == nil || err.Error() != want {
					t.Fatalf("error = %v, want %s", err, want)
				}
				if (raw == "1.5" || raw == "abc" || raw == "7777777") && strings.Contains(err.Error(), raw) {
					t.Error("error echoes raw value")
				}
				if len(keys) != i+2 || keys[len(keys)-1] != prefix+name {
					t.Fatalf("parser did not stop at first invalid rate: %v", keys)
				}
			})
		}
	}
	for _, raw := range []string{"99", "1000001", "7777777", "abc", "1.5"} {
		t.Run("MAX_ACCOUNTS/"+raw, func(t *testing.T) {
			env := valid()
			env[prefix+"MAX_ACCOUNTS"] = raw
			_, _, err := parse(env)
			if err == nil || err.Error() != prefix+"MAX_ACCOUNTS must be an integer from 100 to 1000000" {
				t.Fatalf("error = %v", err)
			}
			if (raw == "7777777" || raw == "abc" || raw == "1.5") && strings.Contains(err.Error(), raw) {
				t.Error("error echoes raw value")
			}
		})
	}
	t.Run("explicit-max", func(t *testing.T) {
		env := valid()
		env[prefix+"MAX_ACCOUNTS"] = "100"
		cfg, _, err := parse(env)
		if err != nil || cfg != budgetTestConfig(10, 10, 10, 10) {
			t.Fatalf("parse = %+v, %v", cfg, err)
		}
	})
	t.Run("validate", func(t *testing.T) {
		for _, cfg := range []AccountRequestBudgetConfig{{}, {Enabled: false, ReadPerMinute: -5}, budgetTestConfig(1, 1, 1, 1), budgetTestConfig(1000000, 1000000, 1000000, 1000000)} {
			if err := cfg.Validate(); err != nil {
				t.Errorf("valid config rejected: %v", err)
			}
		}
		if err := (AccountRequestBudgetConfig{Enabled: true}).Validate(); err == nil {
			t.Error("enabled zero configuration accepted")
		}
		for i := range 5 {
			for _, bad := range []int{0, 1000001} {
				cfg := budgetTestConfig(1, 1, 1, 1)
				fields := []*int{&cfg.ReadPerMinute, &cfg.ReadBurst, &cfg.WritePerMinute, &cfg.WriteBurst, &cfg.MaxTrackedAccounts}
				*fields[i] = bad
				if err := cfg.Validate(); err == nil {
					t.Errorf("invalid field %d value %d accepted", i, bad)
				}
			}
		}
	})
}

func TestAccountRequestBudgetMemoryIsBounded(t *testing.T) {
	clock := newBudgetTestClock()
	b := newAccountRequestBudget(budgetTestConfig(60, 1, 60, 1), clock.Now)
	for i := range 10000 {
		budgetTestAdmit(t, b, fmt.Sprintf("account-%d", i), "read", true, 0)
		if len(b.entries) > 100 {
			t.Fatalf("map has %d entries after account %d", len(b.entries), i)
		}
	}
	if len(b.entries) != 100 || b.capacityEvictions != 9900 {
		t.Fatalf("entries=%d evictions=%d, want 100 and 9900", len(b.entries), b.capacityEvictions)
	}
	clock.Advance(time.Hour)
	for range 50 {
		b.admit("account-9999", "read")
	}
	if len(b.entries) != 1 || b.capacityEvictions != 9900 {
		t.Fatalf("after idle sweep: entries=%d evictions=%d, want 1 and 9900", len(b.entries), b.capacityEvictions)
	}
}

func TestAccountRequestBudgetEvictedAccountRestartsWithFullBurst(t *testing.T) {
	for _, class := range []string{"read", "write"} {
		t.Run(class, func(t *testing.T) {
			// Freeze time so stale TATs cannot refill while the account is absent.
			now := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
			const burst = 3
			cfg := budgetTestConfig(60, burst, 60, burst)
			b := newAccountRequestBudget(cfg, func() time.Time { return now })
			for _, exhaustedClass := range []string{"read", "write"} {
				for range burst {
					budgetTestAdmit(t, b, "evicted", exhaustedClass, true, 0)
				}
				budgetTestAdmit(t, b, "evicted", exhaustedClass, false, 1)
			}
			for i := range cfg.MaxTrackedAccounts {
				budgetTestAdmit(t, b, fmt.Sprintf("new-%d", i), "read", true, 0)
			}
			if b.entries["evicted"] != nil || b.capacityEvictions != 1 {
				t.Fatal("exhausted account was not evicted exactly once")
			}
			for range burst {
				budgetTestAdmit(t, b, "evicted", class, true, 0)
			}
			budgetTestAdmit(t, b, "evicted", class, false, 1)
		})
	}
}

func TestAccountRequestBudgetActiveNoisyAccountIsNotEvicted(t *testing.T) {
	clock := newBudgetTestClock()
	b := newAccountRequestBudget(budgetTestConfig(60, 2, 60, 2), clock.Now)
	budgetTestAdmit(t, b, "a", "read", true, 0)
	budgetTestAdmit(t, b, "a", "read", true, 0)
	for i := range 1000 {
		budgetTestAdmit(t, b, fmt.Sprintf("new-%d", i), "read", true, 0)
		budgetTestAdmit(t, b, "a", "read", false, 1)
		if len(b.entries) > 100 {
			t.Fatalf("map has %d entries", len(b.entries))
		}
	}
}

// ownBodyCalls deliberately excludes nested literals: each authentication
// closure must carry its own admission guard, not borrow one from its parent.
func budgetTestOwnBodyCalls(body *ast.BlockStmt) []*ast.CallExpr {
	var calls []*ast.CallExpr
	ast.Inspect(body, func(node ast.Node) bool {
		if _, nested := node.(*ast.FuncLit); nested {
			return false
		}
		if call, ok := node.(*ast.CallExpr); ok {
			calls = append(calls, call)
		}
		return true
	})
	return calls
}

func TestAccountRequestBudgetCoversEveryBearerAuthPath(t *testing.T) {
	files, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var accountSites []string
	systemSites := 0
	inspect := func(name string, body *ast.BlockStmt) {
		if body == nil {
			return
		}
		var bearer, auth, admits []*ast.CallExpr
		system := false
		for _, call := range budgetTestOwnBodyCalls(body) {
			switch fun := call.Fun.(type) {
			case *ast.Ident:
				switch fun.Name {
				case "bearerToken":
					bearer = append(bearer, call)
				case "auth":
					auth = append(auth, call)
				case "admitAccountRequest":
					admits = append(admits, call)
				}
			case *ast.SelectorExpr:
				pkg, ok := fun.X.(*ast.Ident)
				if ok && pkg.Name == "subtle" && fun.Sel.Name == "ConstantTimeCompare" {
					system = true
				}
			}
		}
		if len(bearer) == 0 {
			return
		}
		if len(bearer) != 1 {
			t.Errorf("%s has %d bearer calls in one body", name, len(bearer))
		}
		if system {
			systemSites++
			return
		}
		accountSites = append(accountSites, name)
		if len(auth) != 1 || len(admits) != 1 {
			t.Errorf("%s has %d auth calls and %d admission calls", name, len(auth), len(admits))
			return
		}
		if admits[0].Pos() <= auth[0].Pos() {
			t.Errorf("%s admission precedes authentication", name)
		}
		// Checking call positions alone permits status/profile refusals to
		// bypass admission. Require the guard immediately after auth's !ok.
		invalidTokenIndex := -1
		for i, stmt := range body.List {
			guard, ok := stmt.(*ast.IfStmt)
			if !ok || guard.Pos() <= auth[0].End() {
				continue
			}
			condition, ok := guard.Cond.(*ast.UnaryExpr)
			if !ok || condition.Op != token.NOT {
				continue
			}
			result, ok := condition.X.(*ast.Ident)
			if ok && result.Name == "ok" {
				invalidTokenIndex = i
				break
			}
		}
		if invalidTokenIndex < 0 || invalidTokenIndex+1 >= len(body.List) {
			t.Errorf("%s has no authentication !ok guard followed by admission", name)
			return
		}
		guard, ok := body.List[invalidTokenIndex+1].(*ast.IfStmt)
		if !ok {
			t.Errorf("%s admission guard must immediately follow authentication !ok guard", name)
			return
		}
		condition, ok := guard.Cond.(*ast.UnaryExpr)
		if !ok || condition.Op != token.NOT || condition.X != admits[0] || guard.Init != nil || guard.Else != nil {
			t.Errorf("%s admission guard must immediately follow authentication !ok guard", name)
			return
		}
		if len(guard.Body.List) != 1 {
			t.Errorf("%s refused admission must return immediately", name)
		} else if ret, ok := guard.Body.List[0].(*ast.ReturnStmt); !ok || len(ret.Results) != 0 {
			t.Errorf("%s refused admission must return immediately", name)
		}
		if len(admits[0].Args) != 3 {
			t.Errorf("%s admission has wrong arity", name)
			return
		}
		validAccount := false
		switch arg := admits[0].Args[2].(type) {
		case *ast.Ident:
			validAccount = arg.Name == "accountID"
		case *ast.SelectorExpr:
			p, ok := arg.X.(*ast.Ident)
			validAccount = ok && p.Name == "p" && arg.Sel.Name == "AccountID"
		}
		if !validAccount {
			t.Errorf("%s admission must use accountID or p.AccountID", name)
		}
	}
	for _, entry := range files {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, filepath.Clean(entry.Name()), nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok {
				ast.Inspect(decl, func(node ast.Node) bool {
					if literal, ok := node.(*ast.FuncLit); ok {
						inspect("package-level", literal.Body)
					}
					return true
				})
				continue
			}
			inspect(fn.Name.Name, fn.Body)
			if fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(node ast.Node) bool {
				if literal, ok := node.(*ast.FuncLit); ok {
					inspect(fn.Name.Name, literal.Body)
				}
				return true
			})
		}
	}
	sort.Strings(accountSites)
	want := []string{"requireAgentEmailSendOperatorAnyStatus", "requireAgentEmailSendPrincipal", "requireDomainPrincipalAnyProfile", "requireOperatorAnyStatus", "secretAgentSafetyHandler"}
	if !reflect.DeepEqual(accountSites, want) {
		t.Errorf("account bearer sites = %v, want %v", accountSites, want)
	}
	if systemSites != 10 {
		t.Errorf("system bearer sites = %d, want 10", systemSites)
	}
}

func TestAccountRequestBudgetDebitsOncePerRequest(t *testing.T) {
	clock := newBudgetTestClock()
	b := newAccountRequestBudget(budgetTestConfig(60, 2, 60, 2), clock.Now)
	h := accountRequestBudgetMux(b, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !admitAccountRequest(w, r, "a") {
			return
		}
		if !admitAccountRequest(w, r, "a") {
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	for _, want := range []int{204, 204, 429} {
		budgetTestStatus(t, h, "GET", "/", "", "", want)
	}
	b = newAccountRequestBudget(budgetTestConfig(60, 1, 60, 1), clock.Now)
	h = accountRequestBudgetMux(b, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !admitAccountRequest(w, r, "  ") {
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	for range 10 {
		budgetTestStatus(t, h, "GET", "/", "", "", 204)
	}
	if len(b.entries) != 0 {
		t.Fatalf("blank account created %d entries", len(b.entries))
	}
}

func TestRunRejectsInvalidAccountRequestBudget(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := Run(ctx, Config{APIAddr: "127.0.0.1:0", HealthAddr: "127.0.0.1:0", MetricsAddr: "127.0.0.1:0", AccountRequestBudget: AccountRequestBudgetConfig{Enabled: true}})
	if err == nil || !strings.Contains(err.Error(), "account request budget") {
		t.Fatalf("Run error = %v, want account request budget validation", err)
	}
}

func TestAccountRequestBudgetAPIMuxRejectsInvalidConfiguration(t *testing.T) {
	defer func() {
		if got := recover(); got != "invalid account request budget configuration" {
			t.Fatalf("panic = %v, want exact invalid-configuration panic", got)
		}
	}()
	apiMux(Config{AccountRequestBudget: AccountRequestBudgetConfig{Enabled: true}})
}

type budgetTestLockResponseWriter struct {
	*httptest.ResponseRecorder
	b                *accountRequestBudget
	checks, failures int
}

func (w *budgetTestLockResponseWriter) probe() {
	w.checks++
	if w.b.mu.TryLock() {
		w.b.mu.Unlock()
	} else {
		w.failures++
	}
}

func (w *budgetTestLockResponseWriter) WriteHeader(status int) {
	w.probe()
	w.ResponseRecorder.WriteHeader(status)
}

func (w *budgetTestLockResponseWriter) Write(p []byte) (int, error) {
	w.probe()
	return w.ResponseRecorder.Write(p)
}

type budgetTestLockMetricsWriter struct {
	bytes.Buffer
	b                *accountRequestBudget
	checks, failures int
}

func (w *budgetTestLockMetricsWriter) Write(p []byte) (int, error) {
	if bytes.Contains(p, []byte("witself_account_request_budget")) {
		w.checks++
		if w.b.mu.TryLock() {
			w.b.mu.Unlock()
		} else {
			w.failures++
		}
	}
	return w.Buffer.Write(p)
}

func TestAccountRequestBudgetNeverWritesUnderItsLock(t *testing.T) {
	clock := newBudgetTestClock()
	b := newAccountRequestBudget(budgetTestConfig(60, 1, 60, 1), clock.Now)
	h := accountRequestBudgetMux(b, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if admitAccountRequest(w, r, "a") {
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	budgetTestStatus(t, h, "GET", "/", "", "", 204)
	w := &budgetTestLockResponseWriter{ResponseRecorder: httptest.NewRecorder(), b: b}
	h.ServeHTTP(w, httptest.NewRequest("GET", "/", nil))
	if w.Code != 429 || w.checks < 2 || w.failures != 0 {
		t.Errorf("refusal status=%d writer checks=%d locked=%d", w.Code, w.checks, w.failures)
	}
	m := newRuntimeMetrics()
	m.accountRequestBudget.Store(b)
	mw := &budgetTestLockMetricsWriter{b: b}
	m.writePrometheus(mw)
	if mw.checks == 0 || mw.failures != 0 {
		t.Errorf("metrics writer checks=%d locked=%d", mw.checks, mw.failures)
	}
}
