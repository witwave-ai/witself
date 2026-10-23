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
		if err != nil || !reflect.DeepEqual(got, AccountRequestBudgetConfig{}) {
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
		if err != nil || !reflect.DeepEqual(cfg, AccountRequestBudgetConfig{}) || !reflect.DeepEqual(keys, []string{prefix + "ENABLED"}) {
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
		if err != nil || !reflect.DeepEqual(got, want) {
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
		if err != nil || !reflect.DeepEqual(cfg, budgetTestConfig(10, 10, 10, 10)) {
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
	want := []string{"requireAgentEmailSendOperatorAnyStatus", "requireAgentEmailSendPrincipal", "requireDomainPrincipalAnyProfile", "requireOperatorAnyStatus", "secretAgentSafetyHandler", "transcriptRedactionSafetyHandler"}
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

func TestParseAccountRequestBudgetPlans(t *testing.T) {
	const name = "WITSELF_ACCOUNT_REQUEST_BUDGET_PLANS"
	const objectError = name + " must be a JSON object of plan ids to read and write budgets"
	const countError = name + " must name 1 to 16 plans"
	const idError = name + " has a plan id that is not 1-32 lowercase letters, digits or underscores starting with a letter"
	const keyError = name + " plan \"free\" must set exactly read_per_minute, read_burst, write_per_minute and write_burst"
	const validBudget = `{"read_per_minute":300,"read_burst":5,"write_per_minute":60,"write_burst":2}`
	parse := func(raw string, present bool) (map[string]AccountRequestBudgetTier, error) {
		t.Helper()
		return ParseAccountRequestBudgetPlans(func(key string) (string, bool) {
			if key != name {
				t.Fatalf("unexpected environment lookup %q", key)
			}
			return raw, present
		})
	}
	t.Run("absent", func(t *testing.T) {
		got, err := parse("", false)
		if err != nil || got != nil {
			t.Fatalf("absent plans = %v, %v; want nil, nil", got, err)
		}
	})
	t.Run("disabled-ignores-invalid-plans", func(t *testing.T) {
		var keys []string
		cfg, err := ParseAccountRequestBudgetConfig(func(key string) (string, bool) {
			keys = append(keys, key)
			if key == "WITSELF_ACCOUNT_REQUEST_BUDGET_ENABLED" {
				return "false", true
			}
			return "invalid JSON", true
		})
		if err != nil || cfg.Enabled || !reflect.DeepEqual(keys, []string{"WITSELF_ACCOUNT_REQUEST_BUDGET_ENABLED"}) {
			t.Fatalf("disabled plans = enabled %v, lookups %v, error %v", cfg.Enabled, keys, err)
		}
	})
	t.Run("three-valid-plans", func(t *testing.T) {
		got, err := parse(`{"team":{"read_per_minute":1000000,"read_burst":1,"write_per_minute":200,"write_burst":4},"free":`+validBudget+`,"standard":{"read_per_minute":600,"read_burst":10,"write_per_minute":120,"write_burst":3}}`, true)
		want := map[string]AccountRequestBudgetTier{
			"free":     {ReadPerMinute: 300, ReadBurst: 5, WritePerMinute: 60, WriteBurst: 2},
			"standard": {ReadPerMinute: 600, ReadBurst: 10, WritePerMinute: 120, WriteBurst: 3},
			"team":     {ReadPerMinute: 1000000, ReadBurst: 1, WritePerMinute: 200, WriteBurst: 4},
		}
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("plans = %#v, %v; want %#v", got, err, want)
		}
	})
	cases := []struct{ name, raw, want string }{
		{"array", `[]`, objectError}, {"null", `null`, objectError}, {"string", `"plans"`, objectError},
		{"invalid-json", `{`, objectError}, {"trailing-json", `{} {}`, objectError}, {"empty", `{}`, countError},
		{"missing-key", `{"free":{"read_per_minute":300,"read_burst":5,"write_per_minute":60}}`, keyError},
		{"extra-key", `{"free":{"read_per_minute":300,"read_burst":5,"write_per_minute":60,"write_burst":2,"unknown":1}}`, keyError},
		{"null-budget", `{"free":null}`, keyError}, {"array-budget", `{"free":[]}`, keyError},
	}
	tooMany := make(map[string]json.RawMessage)
	for i := range 17 {
		tooMany[fmt.Sprintf("p%d", i)] = json.RawMessage(validBudget)
	}
	manyJSON, err := json.Marshal(tooMany)
	if err != nil {
		t.Fatal(err)
	}
	cases = append(cases, struct{ name, raw, want string }{"seventeen-plans", string(manyJSON), countError})
	for _, id := range []string{"Free", "a-b", "9a", strings.Repeat("a", 33), "9zz_canary_741"} {
		cases = append(cases, struct{ name, raw, want string }{"invalid-id-" + id, fmt.Sprintf(`{%q:%s}`, id, validBudget), idError})
	}
	for _, key := range []string{"read_per_minute", "read_burst", "write_per_minute", "write_burst"} {
		for _, value := range []string{"0", "1000001", "1.5", "1e3", `"300"`, "-1", "987654321", "null", "true"} {
			budget := map[string]json.RawMessage{"read_per_minute": json.RawMessage("300"), "read_burst": json.RawMessage("5"), "write_per_minute": json.RawMessage("60"), "write_burst": json.RawMessage("2")}
			budget[key] = json.RawMessage(value)
			raw, marshalErr := json.Marshal(map[string]any{"free": budget})
			if marshalErr != nil {
				t.Fatal(marshalErr)
			}
			cases = append(cases, struct{ name, raw, want string }{key + "/" + value, string(raw), name + " plan \"free\" " + key + " must be an integer from 1 to 1000000"})
		}
	}
	// Sorting applies to ids, but each id's key-set check precedes its numeric checks.
	cases = append(cases,
		struct{ name, raw, want string }{"sorted-id-order", `{"z":{},"a":{"read_per_minute":0,"read_burst":1,"write_per_minute":1,"write_burst":1}}`, name + " plan \"a\" read_per_minute must be an integer from 1 to 1000000"},
		struct{ name, raw, want string }{"key-set-before-ranges", `{"free":{"read_per_minute":0}}`, keyError},
		struct{ name, raw, want string }{"range-key-order", `{"free":{"read_per_minute":0,"read_burst":0,"write_per_minute":0,"write_burst":0}}`, name + " plan \"free\" read_per_minute must be an integer from 1 to 1000000"},
	)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parse(tc.raw, true)
			if err == nil || err.Error() != tc.want {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
			for _, canary := range []string{"9zz_canary_741", "zz_canary_741", "987654321"} {
				if strings.Contains(err.Error(), canary) {
					t.Error("error contains a supplied private canary")
				}
			}
		})
	}
}

func budgetTestPlanConfig() AccountRequestBudgetConfig {
	cfg := budgetTestConfig(60, 3, 60, 3)
	cfg.Plans = map[string]AccountRequestBudgetTier{
		"free": {ReadPerMinute: 60, ReadBurst: 1, WritePerMinute: 60, WriteBurst: 1},
		"team": {ReadPerMinute: 60, ReadBurst: 5, WritePerMinute: 60, WriteBurst: 2},
	}
	return cfg
}

func budgetTestPlanRequest(ctx context.Context, b *accountRequestBudget, accountID, class string) *httptest.ResponseRecorder {
	method := http.MethodGet
	if class == "write" {
		method = http.MethodPost
	}
	h := accountRequestBudgetMux(b, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if admitAccountRequest(w, r, accountID) {
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(method, "/", nil).WithContext(ctx))
	return w
}

func budgetTestPlanStatus(t *testing.T, b *accountRequestBudget, accountID, class string, want int) {
	t.Helper()
	if got := budgetTestPlanRequest(context.Background(), b, accountID, class); got.Code != want {
		t.Fatalf("account %q class %q status = %d, want %d", accountID, class, got.Code, want)
	}
}

func budgetTestAwait[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for request-budget test goroutine")
		var zero T
		return zero
	}
}

func TestAccountRequestBudgetPlanTiers(t *testing.T) {
	clock := newBudgetTestClock()
	b := newAccountRequestBudget(budgetTestPlanConfig(), clock.Now)
	b.getAccountPlan = func(_ context.Context, accountID string) (PlanSnapshotRecord, error) {
		if accountID == "err" {
			return PlanSnapshotRecord{}, errors.New("lookup failed")
		}
		return PlanSnapshotRecord{Plan: map[string]string{"free": "free", "team": "team", "ent": "enterprise"}[accountID]}, nil
	}
	for _, tc := range []struct {
		account    string
		readBurst  int
		writeBurst int
	}{{"free", 1, 1}, {"team", 5, 2}, {"ent", 3, 3}, {"err", 3, 3}} {
		t.Run(tc.account, func(t *testing.T) {
			for _, class := range []string{"read", "write"} {
				burst := tc.readBurst
				if class == "write" {
					burst = tc.writeBurst
				}
				for range burst {
					budgetTestPlanStatus(t, b, tc.account, class, http.StatusNoContent)
				}
				budgetTestPlanStatus(t, b, tc.account, class, http.StatusTooManyRequests)
			}
		})
	}
	// The original helper remains explicitly cell-wide even with plans configured.
	for range 3 {
		budgetTestAdmit(t, b, "direct", "read", true, 0)
	}
	budgetTestAdmit(t, b, "direct", "read", false, 1)
}

func TestAccountRequestBudgetPlanCache(t *testing.T) {
	for _, failed := range []bool{false, true} {
		t.Run(fmt.Sprintf("lookup-error-%v", failed), func(t *testing.T) {
			clock := newBudgetTestClock()
			b := newAccountRequestBudget(budgetTestPlanConfig(), clock.Now)
			calls := 0
			b.getAccountPlan = func(context.Context, string) (PlanSnapshotRecord, error) {
				calls++
				if failed {
					return PlanSnapshotRecord{}, errors.New("lookup unavailable")
				}
				return PlanSnapshotRecord{Plan: "free"}, nil
			}
			budgetTestPlanRequest(context.Background(), b, "a", "read")
			if calls != 1 {
				t.Fatalf("initial reader calls = %d, want 1", calls)
			}
			if failed {
				clock.Advance(4 * time.Second)
			} else {
				clock.Advance(59 * time.Second)
			}
			for range 100 {
				budgetTestPlanRequest(context.Background(), b, "a", "read")
			}
			if calls != 1 {
				t.Fatalf("cached reader calls = %d, want 1", calls)
			}
			clock.Advance(2 * time.Second)
			budgetTestPlanRequest(context.Background(), b, "a", "read")
			if calls != 2 {
				t.Fatalf("expired reader calls = %d, want 2", calls)
			}
		})
	}
}

func TestAccountRequestBudgetPlanCacheSurvivesSweep(t *testing.T) {
	clock := newBudgetTestClock()
	b := newAccountRequestBudget(budgetTestPlanConfig(), clock.Now)
	calls := map[string]int{}
	b.getAccountPlan = func(_ context.Context, accountID string) (PlanSnapshotRecord, error) {
		calls[accountID]++
		return PlanSnapshotRecord{Plan: "free"}, nil
	}
	for i := range 26 {
		account, other := "a", "b"
		if i%2 != 0 {
			account, other = other, account
		}
		budgetTestPlanStatus(t, b, account, "read", http.StatusNoContent)
		if b.entries[other] != nil {
			t.Fatalf("idle bucket %q was not swept at request %d", other, i)
		}
		if i != 25 {
			clock.Advance(2 * time.Second)
		}
	}
	if !reflect.DeepEqual(calls, map[string]int{"a": 1, "b": 1}) {
		t.Fatalf("reader calls during 50s of bucket sweeping = %v", calls)
	}
}

func TestAccountRequestBudgetPlanCacheBoundedLRU(t *testing.T) {
	clock := newBudgetTestClock()
	b := newAccountRequestBudget(budgetTestPlanConfig(), clock.Now)
	calls := map[string]int{}
	b.getAccountPlan = func(_ context.Context, accountID string) (PlanSnapshotRecord, error) {
		calls[accountID]++
		return PlanSnapshotRecord{Plan: "free"}, nil
	}
	for i := range b.maxAccounts {
		budgetTestPlanRequest(context.Background(), b, fmt.Sprintf("account-%d", i), "read")
	}
	budgetTestPlanRequest(context.Background(), b, "account-0", "read")
	budgetTestPlanRequest(context.Background(), b, "overflow", "read")
	if len(b.tierCache) != b.maxAccounts || b.tierLRU.Len() != b.maxAccounts {
		t.Fatalf("tier cache map/list sizes = %d/%d, want %d", len(b.tierCache), b.tierLRU.Len(), b.maxAccounts)
	}
	if b.tierCache["account-0"] == nil || b.tierCache["account-1"] != nil {
		t.Fatal("tier cache did not preserve the recently used entry and evict the LRU tail")
	}
	budgetTestPlanRequest(context.Background(), b, "account-0", "read")
	budgetTestPlanRequest(context.Background(), b, "account-1", "read")
	if calls["account-0"] != 1 || calls["account-1"] != 2 {
		t.Fatalf("reader calls after eviction: hot=%d evicted=%d", calls["account-0"], calls["account-1"])
	}
}

func TestAccountRequestBudgetPlanLookupIgnoresClientCancel(t *testing.T) {
	clock := newBudgetTestClock()
	b := newAccountRequestBudget(budgetTestPlanConfig(), clock.Now)
	entered := make(chan context.Context, 1)
	release := make(chan struct{})
	calls := 0
	b.getAccountPlan = func(ctx context.Context, _ string) (PlanSnapshotRecord, error) {
		calls++
		entered <- ctx
		<-release
		if errors.Is(ctx.Err(), context.Canceled) {
			t.Error("plan lookup inherited client cancellation")
			return PlanSnapshotRecord{}, ctx.Err()
		}
		return PlanSnapshotRecord{Plan: "free"}, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- budgetTestPlanRequest(ctx, b, "a", "read") }()
	lookupCtx := budgetTestAwait(t, entered)
	cancel()
	if errors.Is(lookupCtx.Err(), context.Canceled) {
		t.Error("lookup context canceled with client before reader returned")
	}
	close(release)
	if got := budgetTestAwait(t, done); got.Code != http.StatusNoContent {
		t.Fatalf("initial canceled-client request status = %d", got.Code)
	}
	budgetTestPlanStatus(t, b, "a", "read", http.StatusTooManyRequests)
	if calls != 1 || !b.tierCache["a"].resolved {
		t.Fatalf("canceled-client lookup was not cached: calls=%d resolved=%v", calls, b.tierCache["a"].resolved)
	}
}

func TestAccountRequestBudgetPlanErrorKeepsLastTier(t *testing.T) {
	clock := newBudgetTestClock()
	cfg := budgetTestPlanConfig()
	cfg.Plans["free"] = AccountRequestBudgetTier{ReadPerMinute: 1, ReadBurst: 1, WritePerMinute: 1, WriteBurst: 1}
	b := newAccountRequestBudget(cfg, clock.Now)
	calls := 0
	b.getAccountPlan = func(context.Context, string) (PlanSnapshotRecord, error) {
		calls++
		if calls > 1 {
			return PlanSnapshotRecord{}, errors.New("reader unavailable")
		}
		return PlanSnapshotRecord{Plan: "free"}, nil
	}
	budgetTestPlanStatus(t, b, "a", "read", http.StatusNoContent)
	clock.Advance(59 * time.Second)
	budgetTestPlanStatus(t, b, "a", "write", http.StatusNoContent)
	clock.Advance(2 * time.Second)
	budgetTestPlanStatus(t, b, "a", "write", http.StatusTooManyRequests)
	if calls != 2 || !b.tierCache["a"].resolved || b.tierCache["a"].tier != b.planIndex["free"] {
		t.Fatalf("failed refresh lost resolved tier or calls: calls=%d entry=%+v", calls, b.tierCache["a"])
	}
	clock.Advance(4 * time.Second)
	budgetTestPlanStatus(t, b, "a", "write", http.StatusTooManyRequests)
	if calls != 2 {
		t.Fatalf("retried before failure TTL: calls=%d", calls)
	}
	clock.Advance(2 * time.Second)
	budgetTestPlanStatus(t, b, "a", "write", http.StatusTooManyRequests)
	if calls != 3 {
		t.Fatalf("did not retry after failure TTL: calls=%d", calls)
	}
}

func TestAccountRequestBudgetPlanOlderLookupDoesNotOverwrite(t *testing.T) {
	for _, olderError := range []bool{false, true} {
		t.Run(fmt.Sprintf("older-error-%v", olderError), func(t *testing.T) {
			clock := newBudgetTestClock()
			b := newAccountRequestBudget(budgetTestPlanConfig(), clock.Now)
			entered := make(chan int, 2)
			releases := []chan struct{}{make(chan struct{}), make(chan struct{})}
			var mu sync.Mutex
			calls := 0
			b.getAccountPlan = func(context.Context, string) (PlanSnapshotRecord, error) {
				mu.Lock()
				index := calls
				calls++
				mu.Unlock()
				entered <- index
				<-releases[index]
				if index == 0 {
					if olderError {
						return PlanSnapshotRecord{}, errors.New("older lookup failed")
					}
					return PlanSnapshotRecord{Plan: "free"}, nil
				}
				return PlanSnapshotRecord{Plan: "team"}, nil
			}
			done := make(chan *httptest.ResponseRecorder, 2)
			for i := range 2 {
				go func() { done <- budgetTestPlanRequest(context.Background(), b, "a", "read") }()
				if index := budgetTestAwait(t, entered); index != i {
					t.Fatalf("lookup order = %d, want %d", index, i)
				}
			}
			close(releases[1])
			if got := budgetTestAwait(t, done); got.Code != http.StatusNoContent {
				t.Fatalf("newer request status = %d", got.Code)
			}
			close(releases[0])
			if got := budgetTestAwait(t, done); got.Code != http.StatusNoContent {
				t.Fatalf("older request status = %d", got.Code)
			}
			if entry := b.tierCache["a"]; entry == nil || entry.tier != b.planIndex["team"] || !entry.resolved {
				t.Fatalf("newer team lookup overwritten: %+v", entry)
			}
			for range 3 {
				budgetTestPlanStatus(t, b, "a", "read", http.StatusNoContent)
			}
			budgetTestPlanStatus(t, b, "a", "read", http.StatusTooManyRequests)
		})
	}
}

func TestAccountRequestBudgetPlanLookupTimesOut(t *testing.T) {
	clock := newBudgetTestClock()
	b := newAccountRequestBudget(budgetTestPlanConfig(), clock.Now)
	const timeout = 50 * time.Millisecond
	b.planLookupTimeout = timeout
	lookupError := make(chan error, 1)
	calls := 0
	b.getAccountPlan = func(ctx context.Context, _ string) (PlanSnapshotRecord, error) {
		calls++
		<-ctx.Done()
		lookupError <- ctx.Err()
		return PlanSnapshotRecord{}, ctx.Err()
	}
	done := make(chan *httptest.ResponseRecorder, 1)
	start := time.Now()
	go func() { done <- budgetTestPlanRequest(context.Background(), b, "a", "read") }()
	select {
	case got := <-done:
		if got.Code != http.StatusNoContent {
			t.Fatalf("timed-out lookup request status = %d", got.Code)
		}
	case <-time.After(100 * timeout):
		t.Fatal("plan lookup did not respect its bounded timeout")
	}
	if elapsed := time.Since(start); elapsed < timeout {
		t.Fatalf("lookup returned after %v, before timeout %v", elapsed, timeout)
	}
	if err := budgetTestAwait(t, lookupError); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("reader error = %v, want deadline exceeded", err)
	}
	for range 2 {
		budgetTestPlanStatus(t, b, "a", "read", http.StatusNoContent)
	}
	budgetTestPlanStatus(t, b, "a", "read", http.StatusTooManyRequests)
	if calls != 1 {
		t.Fatalf("timeout retried within frozen retry TTL: calls=%d", calls)
	}
}

func TestAccountRequestBudgetPlanLookupOutsideLock(t *testing.T) {
	clock := newBudgetTestClock()
	b := newAccountRequestBudget(budgetTestPlanConfig(), clock.Now)
	// Keep the test's release channel, rather than the production timeout, in control.
	b.planLookupTimeout = time.Hour
	entered := make(chan bool, 1)
	release := make(chan struct{})
	defer close(release)
	b.getAccountPlan = func(_ context.Context, accountID string) (PlanSnapshotRecord, error) {
		if accountID == "a" {
			unlocked := b.mu.TryLock()
			if unlocked {
				b.mu.Unlock()
			}
			entered <- unlocked
			<-release
		}
		return PlanSnapshotRecord{Plan: "team"}, nil
	}
	budgetTestPlanStatus(t, b, "b", "read", http.StatusNoContent)
	aDone := make(chan *httptest.ResponseRecorder, 1)
	go func() { aDone <- budgetTestPlanRequest(context.Background(), b, "a", "read") }()
	if unlocked := budgetTestAwait(t, entered); !unlocked {
		t.Error("reader called while budget mutex is held")
	}
	bDone := make(chan *httptest.ResponseRecorder, 1)
	go func() { bDone <- budgetTestPlanRequest(context.Background(), b, "b", "read") }()
	if got := budgetTestAwait(t, bDone); got.Code != http.StatusNoContent {
		t.Fatalf("cached account B status = %d while A lookup blocked", got.Code)
	}
	select {
	case <-aDone:
		t.Error("account A completed before its reader was released")
	default:
	}
}

func TestAccountRequestBudgetPlanChangeRefills(t *testing.T) {
	for _, nextPlan := range []string{"free", "team"} {
		t.Run(nextPlan, func(t *testing.T) {
			clock := newBudgetTestClock()
			cfg := budgetTestPlanConfig()
			cfg.Plans["free"] = AccountRequestBudgetTier{ReadPerMinute: 2, ReadBurst: 1, WritePerMinute: 2, WriteBurst: 1}
			b := newAccountRequestBudget(cfg, clock.Now)
			calls := 0
			b.getAccountPlan = func(context.Context, string) (PlanSnapshotRecord, error) {
				calls++
				if calls > 1 {
					return PlanSnapshotRecord{Plan: nextPlan}, nil
				}
				return PlanSnapshotRecord{Plan: "free"}, nil
			}
			for _, class := range []string{"read", "write"} {
				budgetTestPlanStatus(t, b, "a", class, http.StatusNoContent)
				budgetTestPlanStatus(t, b, "a", class, http.StatusTooManyRequests)
			}
			// Refill naturally at 59s and exhaust again while the plan is still cached.
			// At 61s neither class can refill naturally, but the plan must refresh.
			clock.Advance(59 * time.Second)
			for _, class := range []string{"read", "write"} {
				budgetTestPlanStatus(t, b, "a", class, http.StatusNoContent)
				budgetTestPlanStatus(t, b, "a", class, http.StatusTooManyRequests)
			}
			clock.Advance(2 * time.Second)
			for _, class := range []string{"read", "write"} {
				if nextPlan == "team" {
					burst := 5
					if class == "write" {
						burst = 2
					}
					for range burst {
						budgetTestPlanStatus(t, b, "a", class, http.StatusNoContent)
					}
				}
				budgetTestPlanStatus(t, b, "a", class, http.StatusTooManyRequests)
			}
			if calls != 2 {
				t.Fatalf("reader calls = %d, want 2", calls)
			}
		})
	}
}

func TestAccountRequestBudgetNoPlansNeverLooksUp(t *testing.T) {
	clock := newBudgetTestClock()
	b := newAccountRequestBudget(budgetTestConfig(60, 3, 60, 3), clock.Now)
	calls := 0
	b.getAccountPlan = func(context.Context, string) (PlanSnapshotRecord, error) {
		calls++
		return PlanSnapshotRecord{Plan: "free"}, nil
	}
	for i := range 50 {
		budgetTestPlanRequest(context.Background(), b, fmt.Sprintf("account-%d", i%5), "read")
	}
	if calls != 0 {
		t.Fatalf("reader called %d times without plans", calls)
	}
}

func TestAccountRequestBudgetPlansNeedPlanReader(t *testing.T) {
	t.Run("run", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		err := Run(ctx, Config{APIAddr: "127.0.0.1:0", HealthAddr: "127.0.0.1:0", MetricsAddr: "127.0.0.1:0", AccountRequestBudget: budgetTestPlanConfig()})
		if err == nil || err.Error() != "account request budget: per-plan budgets need the account plan reader" {
			t.Fatalf("Run error = %v, want missing account plan reader error", err)
		}
	})
	t.Run("api-mux", func(t *testing.T) {
		defer func() {
			if got := recover(); got != "invalid account request budget configuration" {
				t.Fatalf("panic = %v, want invalid budget configuration", got)
			}
		}()
		apiMux(Config{AccountRequestBudget: budgetTestPlanConfig()})
	})
	t.Run("validate", func(t *testing.T) {
		cfg := budgetTestPlanConfig()
		if err := cfg.Validate(); err != nil {
			t.Fatalf("valid programmatic plans rejected: %v", err)
		}
		for _, id := range []string{"Free", "9zz_canary_741", "a-b", strings.Repeat("a", 33)} {
			cfg := budgetTestPlanConfig()
			cfg.Plans = map[string]AccountRequestBudgetTier{id: cfg.Plans["free"]}
			const want = "WITSELF_ACCOUNT_REQUEST_BUDGET_PLANS has a plan id that is not 1-32 lowercase letters, digits or underscores starting with a letter"
			if err := cfg.Validate(); err == nil || err.Error() != want {
				t.Fatalf("invalid programmatic plan id error = %v", err)
			}
		}
		for i, key := range []string{"read_per_minute", "read_burst", "write_per_minute", "write_burst"} {
			for _, value := range []int{0, -1, 1000001} {
				cfg := budgetTestPlanConfig()
				tier := cfg.Plans["free"]
				fields := []*int{&tier.ReadPerMinute, &tier.ReadBurst, &tier.WritePerMinute, &tier.WriteBurst}
				*fields[i] = value
				cfg.Plans["free"] = tier
				want := "WITSELF_ACCOUNT_REQUEST_BUDGET_PLANS plan \"free\" " + key + " must be an integer from 1 to 1000000"
				if err := cfg.Validate(); err == nil || err.Error() != want {
					t.Fatalf("invalid programmatic plan bound error = %v, want %s", err, want)
				}
			}
		}
		cfg = budgetTestPlanConfig()
		for i := range 17 {
			cfg.Plans[fmt.Sprintf("p%d", i)] = cfg.Plans["free"]
		}
		if err := cfg.Validate(); err == nil || err.Error() != "WITSELF_ACCOUNT_REQUEST_BUDGET_PLANS must name 1 to 16 plans" {
			t.Fatalf("too many programmatic plans error = %v", err)
		}
		cfg.Enabled = false
		if err := cfg.Validate(); err != nil {
			t.Fatalf("disabled programmatic plans rejected: %v", err)
		}
	})
}

func TestAccountRequestBudgetPlansHTTP(t *testing.T) {
	clock := newBudgetTestClock()
	budget := budgetTestConfig(60, 10, 60, 10)
	budget.Plans = map[string]AccountRequestBudgetTier{
		"zz_plan_canary_a": {ReadPerMinute: 60, ReadBurst: 1, WritePerMinute: 60, WriteBurst: 1},
		"zz_plan_canary_b": {ReadPerMinute: 60, ReadBurst: 3, WritePerMinute: 60, WriteBurst: 2},
	}
	cfg, _ := newBudgetTestServerConfig(t, budget, clock, false)
	cfg.GetAccountPlan = func(_ context.Context, accountID string) (PlanSnapshotRecord, error) {
		if accountID == budgetTestAccountA {
			return PlanSnapshotRecord{Plan: "zz_plan_canary_a"}, nil
		}
		return PlanSnapshotRecord{Plan: "zz_plan_canary_b"}, nil
	}
	cfg.metrics = newRuntimeMetrics()
	srv := httptest.NewServer(apiMux(cfg))
	defer srv.Close()
	request := func(method, tok string, want int) (http.Header, []byte) {
		t.Helper()
		var requestBody io.Reader
		if method == http.MethodPost {
			requestBody = strings.NewReader(`{"name":"budget-plan-test"}`)
		}
		r, err := http.NewRequest(method, srv.URL+"/v1/realms", requestBody)
		if err != nil {
			t.Fatal(err)
		}
		r.Header.Set("Authorization", "Bearer "+tok)
		response, err := srv.Client().Do(r)
		if err != nil {
			t.Fatal(err)
		}
		body, readErr := io.ReadAll(response.Body)
		closeErr := response.Body.Close()
		if readErr != nil || closeErr != nil {
			t.Fatalf("response body read=%v close=%v", readErr, closeErr)
		}
		if response.StatusCode != want {
			t.Fatalf("HTTP status = %d, want %d", response.StatusCode, want)
		}
		return response.Header, body
	}
	request(http.MethodGet, "tok-a", http.StatusOK)
	headers, body := request(http.MethodGet, "tok-a", http.StatusTooManyRequests)
	want := httptest.NewRecorder()
	writeAccountRequestBudgetError(want, "read", 1)
	if !bytes.Equal(body, want.Body.Bytes()) {
		t.Fatalf("plan refusal changed envelope: %s", body)
	}
	for _, key := range []string{"Retry-After", "Content-Type", "Cache-Control"} {
		if headers.Get(key) != want.Header().Get(key) {
			t.Errorf("plan refusal header %s = %q, want %q", key, headers.Get(key), want.Header().Get(key))
		}
	}
	for range 3 {
		request(http.MethodGet, "tok-b", http.StatusOK)
	}
	scrape := budgetTestScrape(t, cfg.metrics)
	budgetTestMetrics(t, scrape, 1, 0, 2, 0)
	request(http.MethodGet, "tok-b", http.StatusTooManyRequests)
	for _, tc := range []struct {
		token string
		burst int
	}{{"tok-a", 1}, {"tok-b", 2}} {
		for range tc.burst {
			request(http.MethodPost, tc.token, http.StatusCreated)
		}
		request(http.MethodPost, tc.token, http.StatusTooManyRequests)
	}
	scrape = budgetTestScrape(t, cfg.metrics)
	budgetTestMetrics(t, scrape, 2, 2, 2, 0)
	for _, canary := range []string{"zz_plan_canary_a", "zz_plan_canary_b"} {
		if strings.Contains(scrape, canary) {
			t.Error("metrics contain a plan-id canary")
		}
	}
}
