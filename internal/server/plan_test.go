package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/plans"
)

// planTestServer builds a test server whose lifecycle route is enabled (the
// provisioning pair) with a recording SetAccountPlan.
func planTestServer(
	t *testing.T,
	setPlan func(
		ctx context.Context,
		accountID string,
		revision int64,
		snapshotHash, plan string,
		limits, policies map[string]int64,
		features []string,
	) (PlanSnapshotRecord, error),
) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(apiMux(Config{
		ProvisionToken: "witself_prv_test",
		ProvisionAccountExact: func(context.Context, string, string, string) (ProvisionedAccount, error) {
			return ProvisionedAccount{}, nil
		},
		SetAccountPlan: setPlan,
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestAccountPlanFitEndpointRequiresAndReturnsExactSnapshot(t *testing.T) {
	limits := map[string]int64{
		plans.AgentLimit:        10,
		plans.StoredFactLimit:   100,
		plans.StoredSecretLimit: 50,
	}
	policies := map[string]int64{plans.TranscriptRetentionDaysPolicy: 30}
	features := []string{plans.FactsFeature, plans.MemoryFeature, plans.SecretsFeature}
	hash, err := plans.SnapshotHash("free", limits, policies, features)
	if err != nil {
		t.Fatal(err)
	}
	called := 0
	srv := httptest.NewServer(apiMux(Config{
		ProvisionToken: "witself_prv_test",
		ProvisionAccountExact: func(context.Context, string, string, string) (ProvisionedAccount, error) {
			return ProvisionedAccount{}, nil
		},
		CheckAccountPlanFit: func(
			_ context.Context,
			accountID string,
			target PlanFitTargetRecord,
		) (PlanFitReport, error) {
			called++
			if accountID != "acct_1" || target.Plan != "free" ||
				target.SnapshotHash != hash || target.Limits[plans.AgentLimit] != 10 {
				t.Fatalf("fit callback account=%q target=%+v", accountID, target)
			}
			return PlanFitReport{
				AccountID: accountID, TargetPlan: target.Plan,
				TargetSnapshotHash: target.SnapshotHash,
				Violations: []PlanFitViolation{
					{Code: "limit_exceeded", Dimension: plans.AgentLimit,
						Scope: "account", Used: 12, Max: 10, SubjectCount: 1},
					{Code: "limit_exceeded", Dimension: plans.StoredFactLimit,
						Scope: "agent", Used: 104, Max: 100, SubjectCount: 2},
				},
			}, nil
		},
	}))
	t.Cleanup(srv.Close)

	target := PlanFitTargetRecord{
		Plan: "free", SnapshotHash: hash, Limits: limits,
		Policies: policies, Features: features,
	}
	body, err := json.Marshal(struct {
		SchemaVersion string              `json:"schema_version"`
		Target        PlanFitTargetRecord `json:"target"`
	}{SchemaVersion: "witself.v0", Target: target})
	if err != nil {
		t.Fatal(err)
	}
	response := postPlan(
		t, srv.URL+"/v1/accounts/acct_1:plan-fit",
		"witself_prv_test", string(body),
	)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("fit status=%d want=200", response.StatusCode)
	}
	var report PlanFitReport
	if err := json.NewDecoder(response.Body).Decode(&report); err != nil {
		t.Fatal(err)
	}
	if report.SchemaVersion != "witself.v0" || report.AccountID != "acct_1" ||
		report.TargetSnapshotHash != hash || len(report.Violations) != 2 {
		t.Fatalf("fit report=%+v", report)
	}

	for _, invalid := range []string{
		`{"schema_version":"witself.v0"}`,
		`{"schema_version":"witself.v0","target":{"plan":"free","snapshot_hash":"` +
			strings.Repeat("a", 64) + `","limits":{},"policies":{},"features":[]}}`,
	} {
		response := postPlan(
			t, srv.URL+"/v1/accounts/acct_1:plan-fit",
			"witself_prv_test", invalid,
		)
		if response.StatusCode != http.StatusBadRequest {
			t.Fatalf("invalid fit status=%d want=400 body=%s", response.StatusCode, invalid)
		}
	}
	if called != 1 {
		t.Fatalf("fit callback calls=%d want=1", called)
	}
}

func TestAccountPlanFitApplyEndpointReturnsStrictAtomicOutcomes(t *testing.T) {
	targetLimits := map[string]int64{plans.RealmLimit: 1}
	targetPolicies := map[string]int64{plans.TranscriptRetentionDaysPolicy: 30}
	targetFeatures := []string{plans.FactsFeature, plans.MemoryFeature}
	targetHash, err := plans.SnapshotHash(
		"personal", targetLimits, targetPolicies, targetFeatures,
	)
	if err != nil {
		t.Fatal(err)
	}
	target := PlanFitApplyTargetRecord{
		Revision: 7, Plan: "personal", SnapshotHash: targetHash,
		Limits: targetLimits, Policies: targetPolicies, Features: targetFeatures,
	}
	requestBody, err := json.Marshal(struct {
		SchemaVersion string                   `json:"schema_version"`
		Target        PlanFitApplyTargetRecord `json:"target"`
	}{SchemaVersion: "witself.v0", Target: target})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()

	t.Run("applied", func(t *testing.T) {
		calls := 0
		srv := httptest.NewServer(apiMux(Config{
			ProvisionToken: "witself_prv_test",
			ProvisionAccountExact: func(context.Context, string, string, string) (ProvisionedAccount, error) {
				return ProvisionedAccount{}, nil
			},
			ApplyAccountPlanIfFits: func(
				_ context.Context,
				accountID string,
				got PlanFitApplyTargetRecord,
			) (PlanFitApplyResult, error) {
				calls++
				if accountID != "acct_1" || got.Revision != target.Revision ||
					got.SnapshotHash != target.SnapshotHash ||
					got.Limits[plans.RealmLimit] != 1 {
					t.Fatalf("fit apply target=%+v account=%q", got, accountID)
				}
				return PlanFitApplyResult{
					State: PlanFitApplyStateApplied, AccountID: accountID,
					TargetRevision: target.Revision, TargetPlan: target.Plan,
					TargetSnapshotHash: target.SnapshotHash,
					Violations:         []PlanFitViolation{},
					AppliedSnapshot: &PlanSnapshotRecord{
						AccountID: accountID, Revision: target.Revision,
						SnapshotHash: target.SnapshotHash, Plan: target.Plan,
						Limits: target.Limits, Policies: target.Policies,
						Features: target.Features, AppliedAt: &now,
					},
				}, nil
			},
		}))
		t.Cleanup(srv.Close)
		resp := postPlan(
			t, srv.URL+"/v1/accounts/acct_1:plan-fit-apply",
			"witself_prv_test", string(requestBody),
		)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("applied status=%d want=200", resp.StatusCode)
		}
		var result PlanFitApplyResult
		if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
			t.Fatal(err)
		}
		if result.SchemaVersion != "witself.v0" ||
			result.State != PlanFitApplyStateApplied ||
			len(result.Violations) != 0 || result.CurrentSnapshot != nil ||
			result.AppliedSnapshot == nil || result.AppliedSnapshot.Revision != 7 {
			t.Fatalf("applied result=%+v", result)
		}
		if calls != 1 {
			t.Fatalf("calls=%d want=1", calls)
		}
	})

	t.Run("blocked", func(t *testing.T) {
		currentLimits := map[string]int64{plans.RealmLimit: 10}
		currentPolicies := map[string]int64{plans.TranscriptRetentionDaysPolicy: 90}
		currentFeatures := []string{plans.FactsFeature, plans.MemoryFeature}
		currentHash, err := plans.SnapshotHash(
			"professional", currentLimits, currentPolicies, currentFeatures,
		)
		if err != nil {
			t.Fatal(err)
		}
		srv := httptest.NewServer(apiMux(Config{
			ProvisionToken: "witself_prv_test",
			ProvisionAccountExact: func(context.Context, string, string, string) (ProvisionedAccount, error) {
				return ProvisionedAccount{}, nil
			},
			ApplyAccountPlanIfFits: func(
				_ context.Context,
				accountID string,
				_ PlanFitApplyTargetRecord,
			) (PlanFitApplyResult, error) {
				return PlanFitApplyResult{
					State: PlanFitApplyStateBlocked, AccountID: accountID,
					TargetRevision: target.Revision, TargetPlan: target.Plan,
					TargetSnapshotHash: target.SnapshotHash,
					Violations: []PlanFitViolation{{
						Code: "limit_exceeded", Dimension: plans.RealmLimit,
						Scope: "account", Used: 2, Max: 1, SubjectCount: 1,
					}},
					CurrentSnapshot: &PlanSnapshotRecord{
						AccountID: accountID, Revision: 6, SnapshotHash: currentHash,
						Plan: "professional", Limits: currentLimits,
						Policies: currentPolicies, Features: currentFeatures,
						AppliedAt: &now,
					},
				}, nil
			},
		}))
		t.Cleanup(srv.Close)
		resp := postPlan(
			t, srv.URL+"/v1/accounts/acct_1:plan-fit-apply",
			"witself_prv_test", string(requestBody),
		)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("blocked status=%d want=200", resp.StatusCode)
		}
		var result PlanFitApplyResult
		if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
			t.Fatal(err)
		}
		if result.State != PlanFitApplyStateBlocked ||
			len(result.Violations) != 1 || result.AppliedSnapshot != nil ||
			result.CurrentSnapshot == nil || result.CurrentSnapshot.Revision != 6 {
			t.Fatalf("blocked result=%+v", result)
		}
	})
}

func TestAccountPlanFitApplyEndpointRejectsInvalidRequestsAndResults(t *testing.T) {
	limits := map[string]int64{plans.RealmLimit: 1}
	hash, err := plans.SnapshotHash("personal", limits, map[string]int64{}, []string{})
	if err != nil {
		t.Fatal(err)
	}
	validBody := `{"schema_version":"witself.v0","target":{"revision":2,"plan":"personal","snapshot_hash":"` + hash + `","limits":{"realms":1},"policies":{},"features":[]}}`
	called := 0
	srv := httptest.NewServer(apiMux(Config{
		ProvisionToken: "witself_prv_test",
		ProvisionAccountExact: func(context.Context, string, string, string) (ProvisionedAccount, error) {
			return ProvisionedAccount{}, nil
		},
		ApplyAccountPlanIfFits: func(
			_ context.Context,
			accountID string,
			_ PlanFitApplyTargetRecord,
		) (PlanFitApplyResult, error) {
			called++
			// Invalid mutual-exclusion shape: applied cannot include current.
			return PlanFitApplyResult{
				State: PlanFitApplyStateApplied, AccountID: accountID,
				TargetRevision: 2, TargetPlan: "personal",
				TargetSnapshotHash: hash, Violations: []PlanFitViolation{},
				CurrentSnapshot: &PlanSnapshotRecord{},
			}, nil
		},
	}))
	t.Cleanup(srv.Close)
	for _, body := range []string{
		`{"schema_version":"witself.v0","target":{"revision":0,"plan":"personal","snapshot_hash":"` + hash + `","limits":{"realms":1},"policies":{},"features":[]}}`,
		`{"schema_version":"witself.v0","target":{"revision":2,"plan":"personal","snapshot_hash":"` + strings.Repeat("a", 64) + `","limits":{"realms":1},"policies":{},"features":[]}}`,
		validBody + `{}`,
	} {
		resp := postPlan(
			t, srv.URL+"/v1/accounts/acct_1:plan-fit-apply",
			"witself_prv_test", body,
		)
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("invalid request status=%d body=%s", resp.StatusCode, body)
		}
	}
	if called != 0 {
		t.Fatalf("invalid request callback calls=%d", called)
	}
	if resp := postPlan(
		t, srv.URL+"/v1/accounts/acct_1:plan-fit-apply",
		"wrong", validBody,
	); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong-token status=%d want=401", resp.StatusCode)
	}
	if resp := postPlan(
		t, srv.URL+"/v1/accounts/acct_1:plan-fit-apply",
		"witself_prv_test", validBody,
	); resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("invalid result status=%d want=500", resp.StatusCode)
	}
	if called != 1 {
		t.Fatalf("valid callback calls=%d want=1", called)
	}
}

func postPlan(t *testing.T, url, token, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest("POST", url, bytes.NewReader([]byte(body)))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func TestSetAccountPlanEndpoint(t *testing.T) {
	var gotAccount, gotPlan string
	var gotRevision int64
	var gotHash string
	var gotLimits map[string]int64
	var gotPolicies map[string]int64
	var gotFeatures []string
	setPlanCalls := 0
	srv := planTestServer(t, func(
		_ context.Context,
		accountID string,
		revision int64,
		snapshotHash, plan string,
		limits, policies map[string]int64,
		features []string,
	) (PlanSnapshotRecord, error) {
		setPlanCalls++
		if accountID == "acct_missing" {
			return PlanSnapshotRecord{}, ErrNotFound
		}
		if accountID == "acct_stale" {
			return PlanSnapshotRecord{}, ErrConflict
		}
		gotRevision, gotHash = revision, snapshotHash
		gotAccount, gotPlan, gotLimits, gotPolicies, gotFeatures = accountID, plan, limits, policies, features
		return PlanSnapshotRecord{
			AccountID: accountID, Revision: revision, SnapshotHash: snapshotHash,
			Plan: plan, Limits: limits, Policies: policies, Features: features,
		}, nil
	})

	// Happy path: the control plane applies a snapshot.
	hash := strings.Repeat("a", 64)
	resp := postPlan(t, srv.URL+"/v1/accounts/acct_1:plan", "witself_prv_test",
		`{"revision":7,"snapshot_hash":"`+hash+`","plan":"standard","limits":{"agents":250,"agents_per_realm":100,"realms":10},"policies":{"transcript_retention_days":90,"message_retention_days":90,"messaging_entitlement_version":1},"features":["memory","facts","secrets","messaging","collaboration","support"]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d; want 200", resp.StatusCode)
	}
	if gotAccount != "acct_1" || gotPlan != "standard" ||
		gotLimits["agents"] != 250 ||
		gotLimits["agents_per_realm"] != 100 ||
		gotLimits["realms"] != 10 {
		t.Fatalf("callback got (%q, %q, %v); want the applied snapshot", gotAccount, gotPlan, gotLimits)
	}
	if gotRevision != 7 || gotHash != hash {
		t.Fatalf("fence = %d/%q; want 7/%q", gotRevision, gotHash, hash)
	}
	if len(gotFeatures) != 6 || gotFeatures[2] != "secrets" ||
		gotFeatures[3] != "messaging" {
		t.Fatalf("features = %v; want the standard plan's 6 features", gotFeatures)
	}
	if gotPolicies["transcript_retention_days"] != 90 ||
		gotPolicies["message_retention_days"] != 90 ||
		gotPolicies["messaging_entitlement_version"] != 1 {
		t.Fatalf("policies = %v; want all retention and messaging policies", gotPolicies)
	}
	var body struct {
		Revision int64            `json:"revision"`
		Hash     string           `json:"snapshot_hash"`
		Plan     string           `json:"plan"`
		Limits   map[string]int64 `json:"limits"`
		Policies map[string]int64 `json:"policies"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil ||
		body.Revision != 7 || body.Hash != hash || body.Plan != "standard" ||
		body.Limits["agents"] != 250 ||
		body.Limits["agents_per_realm"] != 100 ||
		body.Policies["transcript_retention_days"] != 90 ||
		body.Policies["message_retention_days"] != 90 ||
		body.Policies["messaging_entitlement_version"] != 1 {
		t.Fatalf("response body = %+v, %v; want the snapshot echoed", body, err)
	}

	// An omitted retention window is the explicit indefinite policy used by
	// account overrides; the entitlement marker must still reach the store.
	resp = postPlan(t, srv.URL+"/v1/accounts/acct_indefinite:plan", "witself_prv_test",
		`{"revision":8,"snapshot_hash":"`+strings.Repeat("b", 64)+`","plan":"enterprise","limits":{},"policies":{"messaging_entitlement_version":1},"features":["collaboration","facts","memory","messaging","secrets","support"]}`)
	if resp.StatusCode != http.StatusOK ||
		gotPolicies["messaging_entitlement_version"] != 1 ||
		len(gotPolicies) != 1 {
		t.Fatalf("indefinite messaging policy status/policies = %d/%v; want 200 and only the entitlement marker",
			resp.StatusCode, gotPolicies)
	}

	// Wrong token -> 401 (constant-time provision check).
	if resp := postPlan(t, srv.URL+"/v1/accounts/acct_1:plan", "witself_prv_WRONG", `{"plan":"standard"}`); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong token status = %d; want 401", resp.StatusCode)
	}
	// Missing plan -> 400.
	if resp := postPlan(t, srv.URL+"/v1/accounts/acct_1:plan", "witself_prv_test", `{"limits":{"agents":1}}`); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("missing plan status = %d; want 400", resp.StatusCode)
	}
	if resp := postPlan(t, srv.URL+"/v1/accounts/acct_1:plan", "witself_prv_test",
		`{"plan":"free","policies":{"transcript_retention_days":0}}`); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("zero retention status = %d; want 400", resp.StatusCode)
	}
	if resp := postPlan(t, srv.URL+"/v1/accounts/acct_1:plan", "witself_prv_test",
		`{"plan":"free","policies":{"message_retention_days":0}}`); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("zero message retention status = %d; want 400", resp.StatusCode)
	}
	if resp := postPlan(t, srv.URL+"/v1/accounts/acct_1:plan", "witself_prv_test",
		`{"plan":"free","policies":{"message_retention_days":36501}}`); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("oversized message retention status = %d; want 400", resp.StatusCode)
	}
	if resp := postPlan(t, srv.URL+"/v1/accounts/acct_1:plan", "witself_prv_test",
		`{"plan":"free","policies":{"messaging_entitlement_version":2}}`); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("unknown messaging entitlement version status = %d; want 400", resp.StatusCode)
	}
	if resp := postPlan(t, srv.URL+"/v1/accounts/acct_1:plan", "witself_prv_test",
		`{"plan":"free","policies":{"unknown_policy":1}}`); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("unknown policy status = %d; want 400", resp.StatusCode)
	}
	if setPlanCalls != 2 {
		t.Fatalf("SetAccountPlan calls after invalid requests = %d; want only the two valid snapshots", setPlanCalls)
	}
	// Unknown account -> 404.
	if resp := postPlan(t, srv.URL+"/v1/accounts/acct_missing:plan", "witself_prv_test", `{"plan":"free"}`); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown account status = %d; want 404", resp.StatusCode)
	}
	if resp := postPlan(t, srv.URL+"/v1/accounts/acct_stale:plan", "witself_prv_test", `{"plan":"free"}`); resp.StatusCode != http.StatusConflict {
		t.Fatalf("stale snapshot status = %d; want 409", resp.StatusCode)
	}
}

func TestCreateRealmPlanLimit(t *testing.T) {
	auth := func(_ context.Context, tok string) (string, string, string, bool, error) {
		if tok == "good" {
			return "opr_x", "acc_y", "active", true, nil
		}
		return "", "", "", false, nil
	}
	create := func(context.Context, string, string) (Realm, error) {
		return Realm{}, &PlanLimitError{
			Dimension: "realms", Used: 1, Max: 1, Plan: "free",
		}
	}
	srv := httptest.NewServer(apiMux(Config{Authenticate: auth, CreateRealm: create}))
	t.Cleanup(srv.Close)

	req, _ := http.NewRequest("POST", srv.URL+"/v1/realms", strings.NewReader(`{"name":"second"}`))
	req.Header.Set("Authorization", "Bearer good")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d; want 403", resp.StatusCode)
	}
	var body struct {
		Error string `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil || !strings.Contains(body.Error, "realms 1/1 on the free plan") {
		t.Fatalf("error body = %+v, %v; the refusal must explain itself", body, err)
	}
}

func TestCreateAgentPlanLimit(t *testing.T) {
	auth := func(_ context.Context, tok string) (string, string, string, bool, error) {
		return "opr_x", "acc_y", "active", tok == "good", nil
	}
	create := func(context.Context, string, string, CreateAgentRequest) (Agent, error) {
		return Agent{}, &PlanLimitError{
			Dimension: "agents_per_realm", Used: 10, Max: 10, Plan: "free",
		}
	}
	srv := httptest.NewServer(apiMux(Config{Authenticate: auth, CreateAgent: create}))
	t.Cleanup(srv.Close)

	req, _ := http.NewRequest("POST", srv.URL+"/v1/realms/realm_1/agents", strings.NewReader(`{"name":"a26"}`))
	req.Header.Set("Authorization", "Bearer good")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d; want 403", resp.StatusCode)
	}
	var body struct {
		Error string `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil ||
		!strings.Contains(body.Error, "agents per realm 10/10") {
		t.Fatalf("error body = %+v, %v; the refusal must explain itself", body, err)
	}
}

func TestCapabilitiesPlanAndBilling(t *testing.T) {
	planInfo := func(context.Context) (string, map[string]int64, map[string]int64, []string, error) {
		return "standard", map[string]int64{"agents": 250, "realms": 10},
			map[string]int64{"transcript_retention_days": 90},
			[]string{"memory", "facts", "secrets", "collaboration", "support"}, nil
	}
	srv := httptest.NewServer(apiMux(Config{AccountID: "acct_default", PlanInfo: planInfo}))
	t.Cleanup(srv.Close)

	get := func() map[string]any {
		t.Helper()
		resp, err := http.Get(srv.URL + "/v1/capabilities")
		if err != nil {
			t.Fatalf("GET capabilities: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		var doc map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return doc
	}

	// Default: plan surfaced, billing not configured -> self_hosted.
	doc := get()
	account := doc["account"].(map[string]any)
	if account["plan"] != "standard" {
		t.Fatalf("account.plan = %v; want standard", account["plan"])
	}
	limits := doc["limits"].(map[string]any)
	if limits["agents"].(float64) != 250 || limits["realms"].(float64) != 10 {
		t.Fatalf("limits = %v; want the snapshot", limits)
	}
	features := account["plan_features"].([]any)
	if len(features) != 5 {
		t.Fatalf("plan_features = %v; want 5 entries", features)
	}
	policies := account["plan_policies"].(map[string]any)
	if policies["transcript_retention_days"] != float64(90) {
		t.Fatalf("plan_policies = %v; want transcript retention 90", policies)
	}
	billing := doc["billing"].(map[string]any)
	if billing["supported"].(bool) || billing["reason"] != "self_hosted" {
		t.Fatalf("billing = %v; want unsupported/self_hosted by default", billing)
	}

	// With the deployment config set, capabilities advertises the endpoint.
	t.Setenv("WITSELF_BILLING_ENDPOINT", "https://cp.example/v1")
	billing = get()["billing"].(map[string]any)
	if !billing["supported"].(bool) || billing["endpoint"] != "https://cp.example/v1" {
		t.Fatalf("billing = %v; want supported with the configured endpoint", billing)
	}
}

func TestCapabilitiesNeverAdvertiseUnsafeBillingEndpoint(t *testing.T) {
	for _, endpoint := range []string{
		"https://user:secret@cp.example/v1",
		"https://cp.example/v1?token=secret",
		"https://cp.example/v1#private",
		"http://cp.example/v1",
		"https://cp.example/v1%0aheader",
		"https://cp.example/v1\\unsafe",
	} {
		t.Run(endpoint, func(t *testing.T) {
			t.Setenv("WITSELF_BILLING_ENDPOINT", endpoint)
			srv := httptest.NewServer(apiMux(Config{}))
			t.Cleanup(srv.Close)
			resp, err := http.Get(srv.URL + "/v1/capabilities")
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = resp.Body.Close() }()
			var doc map[string]any
			if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
				t.Fatal(err)
			}
			billing := doc["billing"].(map[string]any)
			if billing["supported"] != false || billing["reason"] != "invalid_configuration" {
				t.Fatalf("billing = %v; want a fail-closed capability", billing)
			}
			if _, advertised := billing["endpoint"]; advertised {
				t.Fatalf("unsafe endpoint was advertised: %v", billing)
			}
		})
	}
}
