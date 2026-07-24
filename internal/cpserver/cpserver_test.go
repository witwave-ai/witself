package cpserver

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/billing"
	"github.com/witwave-ai/witself/internal/billing/fake"
	"github.com/witwave-ai/witself/internal/billing/lifecycle"
	"github.com/witwave-ai/witself/internal/plans"
)

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

type noopApplier struct{}

func (noopApplier) Apply(_ context.Context, _ string, request lifecycle.ApplyRequest) (lifecycle.ApplyAck, error) {
	return lifecycle.ApplyAck{Revision: request.Revision, Hash: request.Hash}, nil
}

type failingApplier struct{}

func (failingApplier) Apply(context.Context, string, lifecycle.ApplyRequest) (lifecycle.ApplyAck, error) {
	return lifecycle.ApplyAck{}, errors.New("cell unavailable")
}

type harness struct {
	srv  *httptest.Server
	fake *fake.Fake
	ck   *clock
}

const testAdminID = "adm_abcdefghijklmnopqrst"

// newHarness builds the full CP HTTP stack over MemStore + the interactive
// fake. Auth stub: token "good" may act on acct_1 only.
func newHarness(t *testing.T) *harness {
	return newHarnessWithApplier(t, noopApplier{})
}

func newHarnessWithApplier(t *testing.T, applier lifecycle.Applier) *harness {
	t.Helper()
	catalog, err := plans.Load()
	if err != nil {
		t.Fatalf("plans.Load: %v", err)
	}
	ck := &clock{t: time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)}
	f := fake.New(fake.Config{Prices: catalog.Prices(), Interactive: true, Now: ck.now})
	providers := map[string]billing.Provider{"fake": f}
	m, err := lifecycle.NewManager(lifecycle.Config{
		Catalog: catalog, Providers: providers, Default: "fake",
		Store: lifecycle.NewMemStore(), Applier: applier, Now: ck.now,
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	mux := http.NewServeMux()
	err = Register(mux, Config{
		Manager: m, Catalog: catalog, Providers: providers,
		Authenticate: func(_ context.Context, accountID, bearer string) (bool, error) {
			return bearer == "good" && accountID == "acct_1", nil
		},
		AdminAuthenticate: func(
			_ context.Context,
			bearer, adminID, handle string,
		) (lifecycle.AdminActor, bool, error) {
			return lifecycle.AdminActor{ID: adminID, Handle: handle},
				bearer == "admin-good" && adminID == testAdminID && handle == "scott", nil
		},
		AdminAccountExists: func(_ context.Context, accountID string) (bool, error) {
			return accountID == "acct_1", nil
		},
	})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &harness{srv: srv, fake: f, ck: ck}
}

// call makes a request and decodes the JSON document.
func (h *harness) call(t *testing.T, method, path, bearer, body string) (int, map[string]any) {
	t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, h.srv.URL+path, rdr)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	if bearer == "admin-good" {
		req.Header.Set("X-Witself-Admin-ID", testAdminID)
		req.Header.Set("X-Witself-Admin-Handle", "scott")
	}
	req.Header.Set("X-Witself-Email", "s@example.com")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	var doc map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		t.Fatalf("decode %s %s: %v", method, path, err)
	}
	return resp.StatusCode, doc
}

func TestCatalogEndpoint(t *testing.T) {
	h := newHarness(t)
	status, doc := h.call(t, "GET", "/v1/plans", "", "")
	if status != 200 || doc["schema_version"] != "witself.plans.v0" {
		t.Fatalf("catalog = %d %v", status, doc)
	}
	if plans := doc["plans"].([]any); len(plans) != 4 {
		t.Fatalf("catalog has %d plans; want 4", len(plans))
	}
}

func TestAuthGates(t *testing.T) {
	h := newHarness(t)
	if status, _ := h.call(t, "GET", "/v1/accounts/acct_1/plan", "", ""); status != 401 {
		t.Fatalf("no bearer = %d; want 401", status)
	}
	if status, _ := h.call(t, "GET", "/v1/accounts/acct_1/plan", "bad", ""); status != 403 {
		t.Fatalf("wrong bearer = %d; want 403", status)
	}
	if status, _ := h.call(t, "GET", "/v1/accounts/acct_2/plan", "good", ""); status != 403 {
		t.Fatalf("wrong account = %d; want 403", status)
	}
}

func TestAdminBridgeRequiresImmutableIDAndHandle(t *testing.T) {
	h := newHarness(t)
	path := h.srv.URL + "/v1/admin/accounts/acct_1/transcript-retention"
	tests := []struct {
		name, adminID, handle string
		want                  int
	}{
		{name: "missing both", want: http.StatusForbidden},
		{name: "missing id", handle: "scott", want: http.StatusForbidden},
		{name: "missing handle", adminID: testAdminID, want: http.StatusForbidden},
		{name: "forged id", adminID: "adm_zyxwvutsrqponmlkjihg", handle: "scott", want: http.StatusForbidden},
		{name: "verified pair", adminID: testAdminID, handle: "scott", want: http.StatusOK},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req, err := http.NewRequest(http.MethodGet, path, nil)
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Authorization", "Bearer admin-good")
			if tc.adminID != "" {
				req.Header.Set("X-Witself-Admin-ID", tc.adminID)
			}
			if tc.handle != "" {
				req.Header.Set("X-Witself-Admin-Handle", tc.handle)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode != tc.want {
				t.Fatalf("status = %d; want %d", resp.StatusCode, tc.want)
			}
		})
	}
}

func TestAdminRetentionOverrideShowsDefaultEffectiveAndAttribution(t *testing.T) {
	h := newHarness(t)
	path := "/v1/admin/accounts/acct_1/transcript-retention"
	if status, _ := h.call(t, "GET", path, "good", ""); status != 403 {
		t.Fatalf("owner token on admin route = %d; want 403", status)
	}
	status, doc := h.call(t, "PUT", path, "admin-good",
		`{"days":60,"reason":"founder evaluation"}`)
	if status != 200 {
		t.Fatalf("PUT retention = %d %v", status, doc)
	}
	view := doc["transcript_retention"].(map[string]any)
	if view["default_days"] != float64(30) ||
		view["effective_days"] != float64(60) || view["overridden"] != true {
		t.Fatalf("retention view = %v", view)
	}
	override := view["override"].(map[string]any)
	if override["actor_id"] != testAdminID ||
		override["actor_handle"] != "scott" ||
		override["reason"] != "founder evaluation" {
		t.Fatalf("override attribution = %v", override)
	}

	// Customer-facing plan status exposes the same effective-vs-default
	// distinction without granting owner tokens an override mutation.
	status, doc = h.call(t, "GET", "/v1/accounts/acct_1/plan", "good", "")
	if status != 200 {
		t.Fatalf("GET plan = %d %v", status, doc)
	}
	view = doc["transcript_retention"].(map[string]any)
	if view["default_days"] != float64(30) || view["effective_days"] != float64(60) {
		t.Fatalf("owner retention view = %v", view)
	}
	if _, exposed := view["override"]; exposed {
		t.Fatalf("customer response exposed internal retention attribution: %v", view)
	}
	if _, exposed := doc["plan_override"]; exposed {
		t.Fatalf("customer response exposed internal plan override: %v", doc)
	}

	status, doc = h.call(t, "DELETE", path, "admin-good",
		`{"reason":"restore current plan default"}`)
	if status != 200 {
		t.Fatalf("DELETE retention = %d %v", status, doc)
	}
	view = doc["transcript_retention"].(map[string]any)
	if view["overridden"] != false || view["effective_days"] != float64(30) {
		t.Fatalf("cleared retention view = %v", view)
	}
	if history := doc["admin_history"].([]any); len(history) != 2 {
		t.Fatalf("admin history = %v; want two transitions", history)
	}
}

func TestAdminCanSetExplicitIndefiniteRetention(t *testing.T) {
	h := newHarness(t)
	status, doc := h.call(t, "PUT",
		"/v1/admin/accounts/acct_1/transcript-retention", "admin-good",
		`{"indefinite":true,"reason":"legal contract"}`)
	if status != 200 {
		t.Fatalf("PUT indefinite = %d %v", status, doc)
	}
	view := doc["transcript_retention"].(map[string]any)
	if view["default_days"] != float64(30) || view["effective_days"] != nil ||
		view["overridden"] != true {
		t.Fatalf("indefinite view = %v", view)
	}
}

func TestAdminLimitOverrideLifecycleAndAttribution(t *testing.T) {
	h := newHarness(t)
	path := "/v1/admin/accounts/acct_1/limit-overrides/stored_secret"

	if status, _ := h.call(t, "GET", path, "good", ""); status != http.StatusForbidden {
		t.Fatalf("owner token on limit override route = %d; want 403", status)
	}
	status, doc := h.call(t, "GET", path, "admin-good", "")
	if status != http.StatusOK {
		t.Fatalf("GET inherited limit = %d %v", status, doc)
	}
	view := doc["limit"].(map[string]any)
	if view["dimension"] != plans.StoredSecretLimit ||
		view["default_max"] != float64(0) || view["effective_max"] != float64(0) ||
		view["overridden"] != false {
		t.Fatalf("inherited limit view = %v", view)
	}

	status, doc = h.call(t, "PUT", path, "admin-good",
		`{"max":0,"reason":"pause founder secret writes"}`)
	if status != http.StatusOK {
		t.Fatalf("PUT zero limit = %d %v", status, doc)
	}
	if doc["billing_plan"] != plans.Free || doc["plan"] != plans.Free {
		t.Fatalf("limit override mutated billing classification: %v", doc)
	}
	view = doc["limit"].(map[string]any)
	if view["default_max"] != float64(0) || view["effective_max"] != float64(0) ||
		view["overridden"] != true {
		t.Fatalf("zero limit view = %v", view)
	}
	override := view["override"].(map[string]any)
	if override["max"] != float64(0) ||
		override["actor_id"] != testAdminID ||
		override["actor_handle"] != "scott" ||
		override["reason"] != "pause founder secret writes" {
		t.Fatalf("zero override attribution = %v", override)
	}
	history := doc["admin_history"].([]any)
	if len(history) != 1 {
		t.Fatalf("history after set = %v", history)
	}

	// Same value is an idempotent retry even with a different reason.
	status, doc = h.call(t, "PUT", path, "admin-good",
		`{"max":0,"reason":"retry must not rewrite attribution"}`)
	if status != http.StatusOK ||
		len(doc["admin_history"].([]any)) != 1 {
		t.Fatalf("idempotent PUT = %d %v", status, doc)
	}

	// Explicit unlimited remains a present, attributed exception to the
	// finite Personal catalog default.
	status, doc = h.call(t, "PUT", path, "admin-good",
		`{"unlimited":true,"reason":"founder account is unlimited"}`)
	if status != http.StatusOK {
		t.Fatalf("PUT unlimited = %d %v", status, doc)
	}
	view = doc["limit"].(map[string]any)
	override = view["override"].(map[string]any)
	if view["effective_max"] != nil || view["overridden"] != true ||
		override["max"] != nil ||
		override["reason"] != "founder account is unlimited" {
		t.Fatalf("unlimited view = %v", view)
	}
	history = doc["admin_history"].([]any)
	unlimitedAudit := history[len(history)-1].(map[string]any)
	limitTo := unlimitedAudit["limit_to"].(map[string]any)
	if unlimitedAudit["limit_to_source"] != "override" ||
		limitTo["max"] != nil {
		t.Fatalf("unlimited audit = %v", unlimitedAudit)
	}

	// Owner status receives effective/default limits but never override
	// attribution or admin history.
	status, ownerDoc := h.call(t, "GET", "/v1/accounts/acct_1/plan", "good", "")
	if status != http.StatusOK {
		t.Fatalf("GET owner plan = %d %v", status, ownerDoc)
	}
	if _, exposed := ownerDoc["limit_overrides"]; exposed {
		t.Fatalf("owner response exposed limit override attribution: %v", ownerDoc)
	}
	if _, exposed := ownerDoc["admin_history"]; exposed {
		t.Fatalf("owner response exposed admin history: %v", ownerDoc)
	}

	status, doc = h.call(t, "DELETE", path, "admin-good",
		`{"reason":"resume catalog inheritance"}`)
	if status != http.StatusOK {
		t.Fatalf("DELETE limit = %d %v", status, doc)
	}
	view = doc["limit"].(map[string]any)
	if view["overridden"] != false || view["default_max"] != float64(0) ||
		view["effective_max"] != float64(0) {
		t.Fatalf("cleared limit view = %v", view)
	}
	history = doc["admin_history"].([]any)
	clearAudit := history[len(history)-1].(map[string]any)
	if clearAudit["limit_from_source"] != "override" ||
		clearAudit["limit_to_source"] != "inherited" {
		t.Fatalf("clear audit = %v", clearAudit)
	}
}

func TestAdminAgentPerRealmUnlimitedOverrideLifecycle(t *testing.T) {
	h := newHarness(t)
	path := "/v1/admin/accounts/acct_1/limit-overrides/agents_per_realm"

	status, doc := h.call(t, "GET", path, "admin-good", "")
	if status != http.StatusOK {
		t.Fatalf("GET inherited = %d %v", status, doc)
	}
	view := doc["limit"].(map[string]any)
	if view["dimension"] != plans.AgentPerRealmLimit ||
		view["default_max"] != float64(10) ||
		view["effective_max"] != float64(10) ||
		view["overridden"] != false {
		t.Fatalf("inherited view = %v", view)
	}

	status, doc = h.call(t, "PUT", path, "admin-good",
		`{"unlimited":true,"reason":"founder agents per realm are unlimited"}`)
	if status != http.StatusOK {
		t.Fatalf("PUT unlimited = %d %v", status, doc)
	}
	if doc["billing_plan"] != plans.Free || doc["plan"] != plans.Free {
		t.Fatalf("override mutated billing classification: %v", doc)
	}
	view = doc["limit"].(map[string]any)
	override := view["override"].(map[string]any)
	if view["effective_max"] != nil ||
		view["overridden"] != true ||
		override["max"] != nil ||
		override["actor_id"] != testAdminID ||
		override["actor_handle"] != "scott" ||
		override["reason"] != "founder agents per realm are unlimited" {
		t.Fatalf("unlimited view = %v", view)
	}
	history := doc["admin_history"].([]any)
	if len(history) != 1 {
		t.Fatalf("history after set = %v", history)
	}

	status, doc = h.call(t, "GET", path, "admin-good", "")
	if status != http.StatusOK {
		t.Fatalf("GET override = %d %v", status, doc)
	}
	view = doc["limit"].(map[string]any)
	if view["overridden"] != true ||
		view["override"].(map[string]any)["reason"] !=
			"founder agents per realm are unlimited" {
		t.Fatalf("persisted override view = %v", view)
	}

	status, doc = h.call(t, "DELETE", path, "admin-good",
		`{"reason":"resume plan inheritance"}`)
	if status != http.StatusOK {
		t.Fatalf("DELETE override = %d %v", status, doc)
	}
	view = doc["limit"].(map[string]any)
	if view["overridden"] != false ||
		view["default_max"] != float64(10) ||
		view["effective_max"] != float64(10) {
		t.Fatalf("cleared view = %v", view)
	}
	history = doc["admin_history"].([]any)
	if len(history) != 2 {
		t.Fatalf("history after clear = %v", history)
	}
	clearAudit := history[1].(map[string]any)
	if clearAudit["limit_from_source"] != "override" ||
		clearAudit["limit_to_source"] != "inherited" {
		t.Fatalf("clear audit = %v", clearAudit)
	}
}

func TestAdminLimitOverrideValidation(t *testing.T) {
	h := newHarness(t)
	for _, dimension := range []string{
		plans.RealmLimit,
		plans.AgentLimit,
		plans.AgentPerRealmLimit,
		plans.StoredSecretLimit,
	} {
		path := "/v1/admin/accounts/acct_1/limit-overrides/" + dimension
		if status, _ := h.call(
			t, "GET", path, "admin-good", "",
		); status != http.StatusOK {
			t.Fatalf("GET valid dimension %q = %d; want 200", dimension, status)
		}
	}
	validPath := "/v1/admin/accounts/acct_1/limit-overrides/stored_secret"
	if status, _ := h.call(t, "GET", validPath, "", ""); status != http.StatusUnauthorized {
		t.Fatalf("missing admin bearer = %d; want 401", status)
	}
	if status, _ := h.call(t, "GET",
		"/v1/admin/accounts/acct_1/limit-overrides/not_a_limit",
		"admin-good", ""); status != http.StatusBadRequest {
		t.Fatalf("unknown dimension = %d; want 400", status)
	}
	for _, body := range []string{
		`{"reason":"missing selection"}`,
		`{"max":0,"unlimited":true,"reason":"two selections"}`,
		`{"unlimited":false,"reason":"false is not a selection"}`,
		`{"max":-1,"reason":"negative"}`,
		`{"max":9007199254740992,"reason":"too large"}`,
		`{"max":1}`,
		`{"max":1,"reason":"unknown field","extra":true}`,
		`{"max":1,"reason":"trailing"} {}`,
	} {
		if status, _ := h.call(t, "PUT", validPath, "admin-good", body); status != http.StatusBadRequest {
			t.Fatalf("PUT %s = %d; want 400", body, status)
		}
	}
	for _, body := range []string{
		`{}`,
		`{"reason":""}`,
		`{"reason":"valid","extra":true}`,
		`{"reason":"valid"} {}`,
	} {
		if status, _ := h.call(t, "DELETE", validPath, "admin-good", body); status != http.StatusBadRequest {
			t.Fatalf("DELETE %s = %d; want 400", body, status)
		}
	}

	req, err := http.NewRequest(http.MethodGet,
		h.srv.URL+validPath+"/extra", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer admin-good")
	req.Header.Set("X-Witself-Admin-ID", testAdminID)
	req.Header.Set("X-Witself-Admin-Handle", "scott")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("extra path segment = %d; want 404", resp.StatusCode)
	}
}

func TestAdminLimitMutationReportsAcceptedWhenCellApplyFails(t *testing.T) {
	h := newHarnessWithApplier(t, failingApplier{})
	status, doc := h.call(t, "PUT",
		"/v1/admin/accounts/acct_1/limit-overrides/agents", "admin-good",
		`{"max":0,"reason":"temporary safety cap"}`)
	if status != http.StatusAccepted {
		t.Fatalf("PUT limit = %d %v; want 202 while apply is pending", status, doc)
	}
	if doc["apply_pending"] != true ||
		doc["desired_revision"] != float64(1) ||
		doc["applied_revision"] != float64(0) {
		t.Fatalf("pending limit apply fence = %v", doc)
	}
}

func TestAdminRetentionMutationReportsAcceptedWhenCellApplyFails(t *testing.T) {
	h := newHarnessWithApplier(t, failingApplier{})
	status, doc := h.call(t, "PUT",
		"/v1/admin/accounts/acct_1/transcript-retention", "admin-good",
		`{"days":60,"reason":"temporary exception"}`)
	if status != http.StatusAccepted {
		t.Fatalf("PUT retention = %d %v; want 202 while apply is pending", status, doc)
	}
	if doc["apply_pending"] != true ||
		doc["desired_revision"] != float64(1) ||
		doc["applied_revision"] != float64(0) {
		t.Fatalf("pending apply fence = %v", doc)
	}
}

func TestAdminEnterpriseBackfillDoesNotChangeBillingPlan(t *testing.T) {
	h := newHarness(t)
	path := "/v1/admin/accounts/acct_1/plan-override"
	status, doc := h.call(t, "PUT", path, "admin-good",
		`{"plan":"enterprise","reason":"founder account backfill"}`)
	if status != 200 {
		t.Fatalf("PUT plan override = %d %v", status, doc)
	}
	if doc["plan"] != "enterprise" || doc["billing_plan"] != "free" {
		t.Fatalf("backfill status = %v; want enterprise effective, free billing", doc)
	}
	view := doc["transcript_retention"].(map[string]any)
	if view["default_days"] != nil || view["effective_days"] != nil {
		t.Fatalf("enterprise retention = %v; want indefinite", view)
	}
	override := doc["plan_override"].(map[string]any)
	if override["actor_id"] != testAdminID ||
		override["actor_handle"] != "scott" ||
		override["reason"] != "founder account backfill" {
		t.Fatalf("plan override attribution = %v", override)
	}

	status, doc = h.call(t, "DELETE", path, "admin-good",
		`{"reason":"rollback test"}`)
	if status != 200 || doc["plan"] != "free" || doc["billing_plan"] != "free" ||
		doc["plan_override"] != nil {
		t.Fatalf("cleared plan override = %d %v", status, doc)
	}
}

func TestAdminOverrideValidation(t *testing.T) {
	h := newHarness(t)
	path := "/v1/admin/accounts/acct_1/transcript-retention"
	for _, body := range []string{
		`{"reason":"missing value"}`,
		`{"days":60,"indefinite":true,"reason":"two values"}`,
		`{"days":0,"reason":"zero is unsafe"}`,
		`{"days":60}`,
	} {
		if status, _ := h.call(t, "PUT", path, "admin-good", body); status != 400 {
			t.Fatalf("PUT %s = %d; want 400", body, status)
		}
	}
}

// TestFullLifecycleOverHTTP drives the decided CLI flow end to end through
// the HTTP layer: status(free) -> upgrade(action+url) -> pending visible ->
// the provider's webhook confirms payment -> status(standard) -> downgrade
// (scheduled) -> cancel -> status shows no pending.
func TestFullLifecycleOverHTTP(t *testing.T) {
	h := newHarness(t)

	// Fresh account: free/free.
	status, doc := h.call(t, "GET", "/v1/accounts/acct_1/plan", "good", "")
	if status != 200 || doc["plan"] != "free" || doc["applied"] != "free" {
		t.Fatalf("initial status = %d %v", status, doc)
	}

	// Upgrade: interactive fake -> action + checkout URL.
	status, doc = h.call(t, "POST", "/v1/accounts/acct_1/plan:upgrade", "good", `{"plan":"standard"}`)
	if status != 200 || doc["kind"] != "action" || doc["url"] == "" {
		t.Fatalf("upgrade = %d %v; want action+url", status, doc)
	}

	// Status shows the pending change truthfully.
	_, doc = h.call(t, "GET", "/v1/accounts/acct_1/plan", "good", "")
	pending, ok := doc["pending"].(map[string]any)
	if !ok || pending["kind"] != "upgrade" || pending["plan"] != "standard" {
		t.Fatalf("pending = %v; want the parked upgrade", doc["pending"])
	}

	// The payer completes checkout; the provider announces it via the
	// webhook ROUTE (the fake's callback shape), not an in-process call.
	h.ck.t = h.ck.t.Add(time.Minute)
	events, err := h.fake.Complete("fake_cus_0001")
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("want 1 activation event, got %d", len(events))
	}
	status, doc = h.call(t, "POST", "/v1/billing/webhook/fake", "",
		`{"customer_id":"fake_cus_0001","type":"subscription_activated","plan":"standard"}`)
	if status != 200 || doc["received"].(float64) != 1 {
		t.Fatalf("webhook = %d %v", status, doc)
	}

	// Entitled + applied, no pending.
	_, doc = h.call(t, "GET", "/v1/accounts/acct_1/plan", "good", "")
	if doc["plan"] != "standard" || doc["applied"] != "standard" || doc["pending"] != nil {
		t.Fatalf("post-payment status = %v; want standard/standard, no pending", doc)
	}

	// Downgrade: scheduled for period end.
	status, doc = h.call(t, "POST", "/v1/accounts/acct_1/plan:downgrade", "good", `{"plan":"free"}`)
	if status != 200 || doc["kind"] != "scheduled" || doc["effective"] == nil {
		t.Fatalf("downgrade = %d %v; want scheduled+effective", status, doc)
	}

	// Cancel restores the status quo.
	if status, doc = h.call(t, "POST", "/v1/accounts/acct_1/plan:cancel", "good", ""); status != 200 || doc["cancelled"] != true {
		t.Fatalf("cancel = %d %v", status, doc)
	}
	_, doc = h.call(t, "GET", "/v1/accounts/acct_1/plan", "good", "")
	if doc["pending"] != nil || doc["plan"] != "standard" {
		t.Fatalf("post-cancel status = %v; want standard, no pending", doc)
	}
}

func TestVerbRefusalsSurfaceVerbatim(t *testing.T) {
	h := newHarness(t)
	status, doc := h.call(t, "POST", "/v1/accounts/acct_1/plan:upgrade", "good", `{"plan":"free"}`)
	if status != 409 || !strings.Contains(doc["error"].(string), "already on") {
		t.Fatalf("upgrade-to-free = %d %v; want the Manager's refusal verbatim", status, doc)
	}
	if status, _ := h.call(t, "POST", "/v1/accounts/acct_1/plan:upgrade", "good", `{}`); status != 400 {
		t.Fatalf("missing plan = %d; want 400", status)
	}
	if status, _ := h.call(t, "POST", "/v1/accounts/acct_1/plan:cancel", "good", ""); status != 409 {
		t.Fatalf("cancel with nothing pending = %d; want 409", status)
	}
}

func TestWebhookRoutes(t *testing.T) {
	h := newHarness(t)
	// Unknown provider: no route mounted.
	req, _ := http.NewRequest("POST", h.srv.URL+"/v1/billing/webhook/stripe", strings.NewReader(`{}`))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Fatalf("unknown provider webhook = %d; want 404", resp.StatusCode)
	}
	// Malformed body: 400 (redelivery of garbage cannot succeed).
	if status, _ := h.call(t, "POST", "/v1/billing/webhook/fake", "", `not json`); status != 400 {
		t.Fatalf("malformed webhook = %d; want 400", status)
	}
	// Unknown customer: could be the claim/fold race on a PAID event — never
	// ACK it away; 500 makes the provider redeliver until it routes.
	if status, _ := h.call(t, "POST", "/v1/billing/webhook/fake", "",
		`{"customer_id":"fake_cus_9999","type":"subscription_activated","plan":"standard"}`); status != 500 {
		t.Fatalf("unknown-customer webhook = %d; want 500 (retry-later)", status)
	}
}

// brokenStore fails every operation — the R2-outage stand-in.
type brokenStore struct{}

func (brokenStore) Get(context.Context, string) (lifecycle.Record, bool, error) {
	return lifecycle.Record{}, false, errStoreDown
}
func (brokenStore) ByCustomer(context.Context, string, string) (lifecycle.Record, bool, error) {
	return lifecycle.Record{}, false, errStoreDown
}
func (brokenStore) Put(context.Context, lifecycle.Record) error { return errStoreDown }
func (brokenStore) List(context.Context) ([]lifecycle.Record, error) {
	return nil, errStoreDown
}

var errStoreDown = errors.New("blob: get registry/accounts/x.json: 503 backend down at https://internal-endpoint")

// TestInfraErrorsAreNot409s: an R2 outage must surface as a generic 500 —
// not a 409 "refusal" carrying raw backend detail the CLI would show a
// customer as a policy decision.
func TestInfraErrorsAreNot409s(t *testing.T) {
	catalog, err := plans.Load()
	if err != nil {
		t.Fatalf("plans.Load: %v", err)
	}
	f := fake.New(fake.Config{Prices: catalog.Prices()})
	providers := map[string]billing.Provider{"fake": f}
	m, err := lifecycle.NewManager(lifecycle.Config{
		Catalog: catalog, Providers: providers, Default: "fake",
		Store: brokenStore{}, Applier: noopApplier{},
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	mux := http.NewServeMux()
	if err := Register(mux, Config{
		Manager: m, Catalog: catalog, Providers: providers,
		Authenticate: func(context.Context, string, string) (bool, error) { return true, nil },
	}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	h := &harness{srv: srv}

	status, doc := h.call(t, "POST", "/v1/accounts/acct_1/plan:upgrade", "any", `{"plan":"standard"}`)
	if status != 500 {
		t.Fatalf("store outage = %d %v; want 500, never a 409 refusal", status, doc)
	}
	if msg := doc["error"].(string); strings.Contains(msg, "internal-endpoint") || strings.Contains(msg, "blob:") {
		t.Fatalf("error body leaks backend detail: %q", msg)
	}
	// A genuine refusal still reads as 409 with its message.
	// (Covered by TestVerbRefusalsSurfaceVerbatim against the real store.)
}
