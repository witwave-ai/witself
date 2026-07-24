package client

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAdminTranscriptRetentionOperations(t *testing.T) {
	var requests []struct {
		method string
		body   map[string]any
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/admin/accounts/acct_1/transcript-retention" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") != "Bearer witself_adm_test" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		var body map[string]any
		if r.Body != nil && r.Method != http.MethodGet {
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
		}
		requests = append(requests, struct {
			method string
			body   map[string]any
		}{r.Method, body})
		_ = json.NewEncoder(w).Encode(map[string]any{
			"schema_version": "witself.v0",
			"account_id":     "acct_1",
			"plan":           "free",
			"billing_plan":   "free",
			"applied":        "free",
			"plan_override":  nil,
			"transcript_retention": map[string]any{
				"default_days":   30,
				"effective_days": 60,
				"overridden":     true,
				"override": map[string]any{
					"days":         60,
					"actor_id":     "adm_abcdefghijklmnopqrst",
					"actor_handle": "scott",
					"reason":       "approved",
					"set_at":       "2026-07-23T00:00:00Z",
				},
			},
			"admin_history": []any{},
		})
	}))
	defer srv.Close()

	ctx := context.Background()
	got, err := GetAdminTranscriptRetention(ctx, srv.URL, "witself_adm_test", "acct_1")
	if err != nil {
		t.Fatal(err)
	}
	if got.AccountID != "acct_1" || got.TranscriptRetention.EffectiveDays == nil ||
		*got.TranscriptRetention.EffectiveDays != 60 {
		t.Fatalf("get = %#v", got)
	}
	if got.TranscriptRetention.Override == nil ||
		got.TranscriptRetention.Override.ActorID != "adm_abcdefghijklmnopqrst" ||
		got.TranscriptRetention.Override.ActorHandle != "scott" {
		t.Fatalf("get attribution = %#v", got.TranscriptRetention.Override)
	}
	days := int64(60)
	if _, err := SetAdminTranscriptRetention(ctx, srv.URL, "witself_adm_test", "acct_1",
		AdminTranscriptRetentionInput{Days: &days, Reason: " approved "}); err != nil {
		t.Fatal(err)
	}
	if _, err := SetAdminTranscriptRetention(ctx, srv.URL, "witself_adm_test", "acct_1",
		AdminTranscriptRetentionInput{Indefinite: true, Reason: "contract"}); err != nil {
		t.Fatal(err)
	}
	if _, err := ClearAdminTranscriptRetention(ctx, srv.URL, "witself_adm_test", "acct_1", " restore "); err != nil {
		t.Fatal(err)
	}

	if len(requests) != 4 {
		t.Fatalf("requests = %d, want 4", len(requests))
	}
	if requests[0].method != http.MethodGet ||
		requests[1].method != http.MethodPut ||
		requests[2].method != http.MethodPut ||
		requests[3].method != http.MethodDelete {
		t.Fatalf("methods = %#v", requests)
	}
	if requests[1].body["days"] != float64(60) || requests[1].body["reason"] != "approved" {
		t.Fatalf("finite body = %#v", requests[1].body)
	}
	if requests[2].body["indefinite"] != true || requests[2].body["reason"] != "contract" {
		t.Fatalf("indefinite body = %#v", requests[2].body)
	}
	if requests[3].body["reason"] != "restore" {
		t.Fatalf("clear body = %#v", requests[3].body)
	}
}

func TestAdminPlanOverrideOperations(t *testing.T) {
	var methods []string
	var bodies []map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/admin/accounts/acct_1/plan-override" {
			http.NotFound(w, r)
			return
		}
		methods = append(methods, r.Method)
		var body map[string]string
		if r.Method != http.MethodGet {
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
		}
		bodies = append(bodies, body)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"schema_version": "witself.v0",
			"account_id":     "acct_1",
			"plan":           "enterprise",
			"billing_plan":   "free",
			"applied":        "enterprise",
			"plan_override": map[string]any{
				"plan":         "enterprise",
				"actor_id":     "adm_abcdefghijklmnopqrst",
				"actor_handle": "scott",
				"reason":       "founder",
				"set_at":       "2026-07-23T00:00:00Z",
			},
			"transcript_retention": map[string]any{
				"default_days": nil, "effective_days": nil, "overridden": false,
			},
			"admin_history": []any{},
		})
	}))
	defer srv.Close()

	ctx := context.Background()
	if _, err := GetAdminPlanOverride(ctx, srv.URL, "token", "acct_1"); err != nil {
		t.Fatal(err)
	}
	got, err := SetAdminPlanOverride(ctx, srv.URL, "token", "acct_1", "enterprise", " founder ")
	if err != nil {
		t.Fatal(err)
	}
	if got.Plan != "enterprise" || got.BillingPlan != "free" ||
		got.PlanOverride == nil || got.PlanOverride.Plan != "enterprise" ||
		got.PlanOverride.ActorID != "adm_abcdefghijklmnopqrst" ||
		got.PlanOverride.ActorHandle != "scott" {
		t.Fatalf("set = %#v", got)
	}
	if _, err := ClearAdminPlanOverride(ctx, srv.URL, "token", "acct_1", "restore"); err != nil {
		t.Fatal(err)
	}
	if len(methods) != 3 || methods[0] != http.MethodGet ||
		methods[1] != http.MethodPut || methods[2] != http.MethodDelete {
		t.Fatalf("methods = %#v", methods)
	}
	if bodies[1]["plan"] != "enterprise" || bodies[1]["reason"] != "founder" {
		t.Fatalf("set body = %#v", bodies[1])
	}
	if bodies[2]["reason"] != "restore" {
		t.Fatalf("clear body = %#v", bodies[2])
	}
}

func TestAdminLimitOverrideOperations(t *testing.T) {
	var requests []struct {
		method string
		body   map[string]any
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/admin/accounts/acct_1/limit-overrides/stored_secret" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") != "Bearer witself_adm_test" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		var body map[string]any
		if r.Method != http.MethodGet {
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
		}
		requests = append(requests, struct {
			method string
			body   map[string]any
		}{r.Method, body})
		_ = json.NewEncoder(w).Encode(map[string]any{
			"schema_version": "witself.v0",
			"account_id":     "acct_1",
			"plan":           "free",
			"billing_plan":   "free",
			"applied":        "free",
			"limits":         map[string]int64{},
			"limit_defaults": map[string]int64{},
			"limit_overrides": map[string]any{
				"stored_secret": map[string]any{
					"max": nil, "actor_id": "adm_abcdefghijklmnopqrst",
					"actor_handle": "scott", "reason": "founder",
					"set_at": "2026-07-23T00:00:00Z",
				},
			},
			"limit": map[string]any{
				"dimension": "stored_secret", "default_max": nil,
				"effective_max": nil, "overridden": true,
				"override": map[string]any{
					"max": nil, "actor_id": "adm_abcdefghijklmnopqrst",
					"actor_handle": "scott", "reason": "founder",
					"set_at": "2026-07-23T00:00:00Z",
				},
			},
			"transcript_retention": map[string]any{
				"default_days": 30, "effective_days": 30, "overridden": false,
			},
			"admin_history": []any{},
		})
	}))
	defer srv.Close()

	ctx := t.Context()
	got, err := GetAdminLimitOverride(
		ctx, srv.URL, "witself_adm_test", "acct_1", "stored_secret")
	if err != nil {
		t.Fatal(err)
	}
	if got.Limit == nil || got.Limit.Dimension != "stored_secret" ||
		!got.Limit.Overridden || got.Limit.Override == nil ||
		got.Limit.Override.Max != nil ||
		got.Limit.Override.ActorID != "adm_abcdefghijklmnopqrst" ||
		got.LimitOverrides["stored_secret"].ActorHandle != "scott" {
		t.Fatalf("get limit = %#v", got)
	}
	zero := int64(0)
	if _, err := SetAdminLimitOverride(
		ctx, srv.URL, "witself_adm_test", "acct_1", "stored_secret",
		AdminAccountLimitInput{Max: &zero, Reason: " pause "},
	); err != nil {
		t.Fatal(err)
	}
	if _, err := SetAdminLimitOverride(
		ctx, srv.URL, "witself_adm_test", "acct_1", "stored_secret",
		AdminAccountLimitInput{Unlimited: true, Reason: " founder "},
	); err != nil {
		t.Fatal(err)
	}
	if _, err := ClearAdminLimitOverride(
		ctx, srv.URL, "witself_adm_test", "acct_1", "stored_secret", " restore ",
	); err != nil {
		t.Fatal(err)
	}

	if len(requests) != 4 ||
		requests[0].method != http.MethodGet ||
		requests[1].method != http.MethodPut ||
		requests[2].method != http.MethodPut ||
		requests[3].method != http.MethodDelete {
		t.Fatalf("requests = %#v", requests)
	}
	if requests[1].body["max"] != float64(0) ||
		requests[1].body["reason"] != "pause" {
		t.Fatalf("zero body = %#v", requests[1].body)
	}
	if requests[2].body["unlimited"] != true ||
		requests[2].body["reason"] != "founder" {
		t.Fatalf("unlimited body = %#v", requests[2].body)
	}
	if requests[3].body["reason"] != "restore" {
		t.Fatalf("clear body = %#v", requests[3].body)
	}
}

func TestAdminPolicyAcceptedResponsePreservesApplyFence(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"schema_version":   "witself.v0",
			"account_id":       "acct_1",
			"plan":             "free",
			"billing_plan":     "free",
			"applied":          "free",
			"plan_override":    nil,
			"apply_pending":    true,
			"desired_revision": 4,
			"applied_revision": 3,
			"transcript_retention": map[string]any{
				"default_days": 30, "effective_days": 60, "overridden": true,
			},
			"admin_history": []any{},
		})
	}))
	defer srv.Close()

	days := int64(60)
	got, err := SetAdminTranscriptRetention(
		t.Context(), srv.URL, "witself_adm_test", "acct_1",
		AdminTranscriptRetentionInput{Days: &days, Reason: "approved"},
	)
	if err != nil {
		t.Fatal(err)
	}
	if !got.ApplyPending || got.DesiredRevision != 4 || got.AppliedRevision != 3 {
		t.Fatalf("accepted apply fence = %#v", got)
	}
}

func TestAdminAccountPolicyValidation(t *testing.T) {
	days0, daysTooHigh := int64(0), MaxAdminTranscriptRetentionDays+1
	negativeLimit, excessiveLimit := int64(-1), MaxAdminAccountLimit+1
	tests := []struct {
		name string
		call func() error
	}{
		{"missing selection", func() error {
			_, err := SetAdminTranscriptRetention(t.Context(), "http://invalid", "t", "acct_1",
				AdminTranscriptRetentionInput{Reason: "r"})
			return err
		}},
		{"conflicting selection", func() error {
			_, err := SetAdminTranscriptRetention(t.Context(), "http://invalid", "t", "acct_1",
				AdminTranscriptRetentionInput{Days: &days0, Indefinite: true, Reason: "r"})
			return err
		}},
		{"zero days", func() error {
			_, err := SetAdminTranscriptRetention(t.Context(), "http://invalid", "t", "acct_1",
				AdminTranscriptRetentionInput{Days: &days0, Reason: "r"})
			return err
		}},
		{"excessive days", func() error {
			_, err := SetAdminTranscriptRetention(t.Context(), "http://invalid", "t", "acct_1",
				AdminTranscriptRetentionInput{Days: &daysTooHigh, Reason: "r"})
			return err
		}},
		{"missing reason", func() error {
			_, err := ClearAdminPlanOverride(t.Context(), "http://invalid", "t", "acct_1", "")
			return err
		}},
		{"unsafe account", func() error {
			_, err := GetAdminPlanOverride(t.Context(), "http://invalid", "t", "../acct")
			return err
		}},
		{"unsafe plan", func() error {
			_, err := SetAdminPlanOverride(t.Context(), "http://invalid", "t", "acct_1", "../../x", "r")
			return err
		}},
		{"limit missing selection", func() error {
			_, err := SetAdminLimitOverride(
				t.Context(), "http://invalid", "t", "acct_1", "stored_secret",
				AdminAccountLimitInput{Reason: "r"})
			return err
		}},
		{"limit conflicting selection", func() error {
			_, err := SetAdminLimitOverride(
				t.Context(), "http://invalid", "t", "acct_1", "stored_secret",
				AdminAccountLimitInput{Max: &days0, Unlimited: true, Reason: "r"})
			return err
		}},
		{"limit negative", func() error {
			_, err := SetAdminLimitOverride(
				t.Context(), "http://invalid", "t", "acct_1", "stored_secret",
				AdminAccountLimitInput{Max: &negativeLimit, Reason: "r"})
			return err
		}},
		{"limit excessive", func() error {
			_, err := SetAdminLimitOverride(
				t.Context(), "http://invalid", "t", "acct_1", "stored_secret",
				AdminAccountLimitInput{Max: &excessiveLimit, Reason: "r"})
			return err
		}},
		{"limit unknown dimension", func() error {
			_, err := GetAdminLimitOverride(
				t.Context(), "http://invalid", "t", "acct_1", "not_a_limit")
			return err
		}},
		{"limit clear missing reason", func() error {
			_, err := ClearAdminLimitOverride(
				t.Context(), "http://invalid", "t", "acct_1", "stored_secret", "")
			return err
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.call(); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
}
