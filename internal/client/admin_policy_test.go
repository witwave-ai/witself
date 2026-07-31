package client

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/witwave-ai/witself/internal/plans"
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

func TestAdminMessagingPolicyOperations(t *testing.T) {
	var requests []struct {
		method string
		path   string
		body   map[string]any
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/admin/accounts/acct_1/messaging" &&
			r.URL.Path != "/v1/admin/accounts/acct_1/message-retention" {
			http.NotFound(w, r)
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
			path   string
			body   map[string]any
		}{method: r.Method, path: r.URL.Path, body: body})
		_ = json.NewEncoder(w).Encode(map[string]any{
			"schema_version":   "witself.v0",
			"account_id":       "acct_1",
			"plan":             "free",
			"billing_plan":     "free",
			"applied":          "free",
			"features":         []string{plans.MessagingFeature},
			"feature_defaults": []string{},
			"messaging": map[string]any{
				"default_enabled": false,
				"enabled":         true,
				"overridden":      true,
				"override": map[string]any{
					"enabled": true, "actor_id": "adm_abcdefghijklmnopqrst",
					"actor_handle": "scott", "reason": "founder",
					"set_at": "2026-07-24T00:00:00Z",
				},
			},
			"message_retention": map[string]any{
				"default_days": 30, "effective_days": nil, "overridden": true,
				"override": map[string]any{
					"days": nil, "actor_id": "adm_abcdefghijklmnopqrst",
					"actor_handle": "scott", "reason": "founder",
					"set_at": "2026-07-24T00:00:00Z",
				},
			},
			"admin_history": []any{},
		})
	}))
	defer srv.Close()

	ctx := t.Context()
	got, err := GetAdminMessaging(
		ctx, srv.URL, "witself_adm_test", "acct_1")
	if err != nil {
		t.Fatal(err)
	}
	if !got.Messaging.Enabled || !got.Messaging.Overridden ||
		got.Messaging.DefaultEnabled ||
		got.Messaging.Override == nil ||
		got.Messaging.Override.ActorID != "adm_abcdefghijklmnopqrst" ||
		!slices.Contains(got.Features, plans.MessagingFeature) {
		t.Fatalf("messaging response = %#v", got)
	}
	if _, err := SetAdminMessaging(
		ctx, srv.URL, "witself_adm_test", "acct_1", true, " founder ",
	); err != nil {
		t.Fatal(err)
	}
	if _, err := ClearAdminMessaging(
		ctx, srv.URL, "witself_adm_test", "acct_1", " restore ",
	); err != nil {
		t.Fatal(err)
	}
	got, err = GetAdminMessageRetention(
		ctx, srv.URL, "witself_adm_test", "acct_1")
	if err != nil {
		t.Fatal(err)
	}
	if got.MessageRetention.EffectiveDays != nil ||
		!got.MessageRetention.Overridden ||
		got.MessageRetention.Override == nil ||
		got.MessageRetention.Override.Days != nil {
		t.Fatalf("message retention response = %#v", got.MessageRetention)
	}
	days := int64(365)
	if _, err := SetAdminMessageRetention(
		ctx, srv.URL, "witself_adm_test", "acct_1",
		AdminMessageRetentionInput{Days: &days, Reason: " team "},
	); err != nil {
		t.Fatal(err)
	}
	if _, err := SetAdminMessageRetention(
		ctx, srv.URL, "witself_adm_test", "acct_1",
		AdminMessageRetentionInput{
			Indefinite: true,
			Reason:     "founder",
		},
	); err != nil {
		t.Fatal(err)
	}
	if _, err := ClearAdminMessageRetention(
		ctx, srv.URL, "witself_adm_test", "acct_1", " restore ",
	); err != nil {
		t.Fatal(err)
	}

	if len(requests) != 7 {
		t.Fatalf("requests = %#v; want seven", requests)
	}
	if requests[1].method != http.MethodPut ||
		requests[1].body["enabled"] != true ||
		requests[1].body["reason"] != "founder" {
		t.Fatalf("messaging PUT = %#v", requests[1])
	}
	if requests[2].method != http.MethodDelete ||
		requests[2].body["reason"] != "restore" {
		t.Fatalf("messaging DELETE = %#v", requests[2])
	}
	if requests[4].body["days"] != float64(365) ||
		requests[4].body["reason"] != "team" {
		t.Fatalf("finite retention PUT = %#v", requests[4])
	}
	if requests[5].body["indefinite"] != true ||
		requests[5].body["reason"] != "founder" {
		t.Fatalf("indefinite retention PUT = %#v", requests[5])
	}
}

func TestAdminAgentEmailPolicyOperations(t *testing.T) {
	var requests []struct {
		method string
		path   string
		body   map[string]any
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/admin/accounts/acct_1/email-receive" &&
			r.URL.Path != "/v1/admin/accounts/acct_1/email-retention" {
			http.NotFound(w, r)
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
			path   string
			body   map[string]any
		}{r.Method, r.URL.Path, body})
		_ = json.NewEncoder(w).Encode(map[string]any{
			"schema_version": "witself.v0", "account_id": "acct_1",
			"email_receive": map[string]any{
				"default_enabled": false, "enabled": true, "overridden": true,
			},
			"email_retention": map[string]any{
				"default_days": 30, "effective_days": nil, "overridden": true,
			},
		})
	}))
	defer srv.Close()
	ctx := t.Context()
	got, err := GetAdminAgentEmailReceive(
		ctx, srv.URL, "witself_adm_test", "acct_1")
	if err != nil || !got.EmailReceive.Enabled {
		t.Fatalf("GET receive = %#v, %v", got, err)
	}
	if _, err := SetAdminAgentEmailReceive(
		ctx, srv.URL, "witself_adm_test", "acct_1", true, " founder ",
	); err != nil {
		t.Fatal(err)
	}
	if _, err := ClearAdminAgentEmailReceive(
		ctx, srv.URL, "witself_adm_test", "acct_1", " restore ",
	); err != nil {
		t.Fatal(err)
	}
	got, err = GetAdminAgentEmailRetention(
		ctx, srv.URL, "witself_adm_test", "acct_1")
	if err != nil || got.EmailRetention.EffectiveDays != nil {
		t.Fatalf("GET retention = %#v, %v", got, err)
	}
	days := int64(365)
	if _, err := SetAdminAgentEmailRetention(
		ctx, srv.URL, "witself_adm_test", "acct_1",
		AdminAgentEmailRetentionInput{Days: &days, Reason: " team "},
	); err != nil {
		t.Fatal(err)
	}
	if _, err := SetAdminAgentEmailRetention(
		ctx, srv.URL, "witself_adm_test", "acct_1",
		AdminAgentEmailRetentionInput{Indefinite: true, Reason: "founder"},
	); err != nil {
		t.Fatal(err)
	}
	if _, err := ClearAdminAgentEmailRetention(
		ctx, srv.URL, "witself_adm_test", "acct_1", " restore ",
	); err != nil {
		t.Fatal(err)
	}
	if len(requests) != 7 ||
		requests[1].path != "/v1/admin/accounts/acct_1/email-receive" ||
		requests[1].body["enabled"] != true ||
		requests[4].path != "/v1/admin/accounts/acct_1/email-retention" ||
		requests[4].body["days"] != float64(365) ||
		requests[5].body["indefinite"] != true {
		t.Fatalf("requests = %#v", requests)
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

func TestAdminAgentPerRealmUnlimitedOverridePath(t *testing.T) {
	var gotMethod, gotPath string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatal(err)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"schema_version": "witself.v0",
			"account_id":     "acct_1",
			"plan":           "free",
			"billing_plan":   "free",
			"limit": map[string]any{
				"dimension":     plans.AgentPerRealmLimit,
				"default_max":   nil,
				"effective_max": nil,
				"overridden":    true,
			},
		})
	}))
	defer srv.Close()

	if _, err := SetAdminLimitOverride(
		t.Context(),
		srv.URL,
		"witself_adm_test",
		"acct_1",
		plans.AgentPerRealmLimit,
		AdminAccountLimitInput{
			Unlimited: true,
			Reason:    "founder agents per realm are unlimited",
		},
	); err != nil {
		t.Fatal(err)
	}
	if gotMethod != http.MethodPut ||
		gotPath != "/v1/admin/accounts/acct_1/limit-overrides/agents_per_realm" ||
		gotBody["unlimited"] != true ||
		gotBody["reason"] != "founder agents per realm are unlimited" {
		t.Fatalf("request = %s %s %#v", gotMethod, gotPath, gotBody)
	}
}

func TestAdminAgentEmailLimitOverridePathsAndBounds(t *testing.T) {
	type request struct {
		path string
		body map[string]any
	}
	var requests []request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		requests = append(requests, request{path: r.URL.Path, body: body})
		dimension := strings.TrimPrefix(
			r.URL.Path, "/v1/admin/accounts/acct_1/limit-overrides/")
		effectiveMax := body["max"]
		_ = json.NewEncoder(w).Encode(map[string]any{
			"schema_version": "witself.v0",
			"account_id":     "acct_1",
			"plan":           "free",
			"billing_plan":   "free",
			"limit": map[string]any{
				"dimension": dimension, "default_max": 0,
				"effective_max": effectiveMax, "overridden": true,
				"override": map[string]any{
					"max": effectiveMax, "actor_id": "adm_abcdefghijklmnopqrst",
					"actor_handle": "scott", "reason": body["reason"],
					"set_at": "2026-07-30T00:00:00Z",
				},
			},
		})
	}))
	defer srv.Close()

	rawMaximum := int64(10 * 1024 * 1024)
	rawPolicy, err := SetAdminLimitOverride(
		t.Context(), srv.URL, "witself_adm_test", "acct_1",
		plans.AgentEmailMaxRawBytesLimit,
		AdminAccountLimitInput{
			Max: &rawMaximum, Reason: "professional raw-message cap",
		},
	)
	if err != nil {
		t.Fatalf("set raw-message maximum: %v", err)
	}
	attachmentPolicy, err := SetAdminLimitOverride(
		t.Context(), srv.URL, "witself_adm_test", "acct_1",
		plans.AgentEmailAttachmentStorageBytesLimit,
		AdminAccountLimitInput{
			Unlimited: true, Reason: "founder attachment storage",
		},
	)
	if err != nil {
		t.Fatalf("set attachment storage unlimited: %v", err)
	}
	if rawPolicy.Limit == nil || rawPolicy.Limit.DefaultMax == nil ||
		*rawPolicy.Limit.DefaultMax != 0 ||
		rawPolicy.Limit.EffectiveMax == nil ||
		*rawPolicy.Limit.EffectiveMax != rawMaximum {
		t.Fatalf("Phase-B raw-message policy = %#v", rawPolicy.Limit)
	}
	if attachmentPolicy.Limit == nil ||
		attachmentPolicy.Limit.DefaultMax == nil ||
		*attachmentPolicy.Limit.DefaultMax != 0 ||
		attachmentPolicy.Limit.EffectiveMax != nil {
		t.Fatalf("Phase-B attachment-storage policy = %#v",
			attachmentPolicy.Limit)
	}
	tooLarge := plans.MaxAgentEmailRawBytes + 1
	if _, err := SetAdminLimitOverride(
		t.Context(), srv.URL, "witself_adm_test", "acct_1",
		plans.AgentEmailMaxRawBytesLimit,
		AdminAccountLimitInput{Max: &tooLarge, Reason: "too large"},
	); err == nil || !strings.Contains(err.Error(), "between 0 and 26214400") {
		t.Fatalf("above-ceiling raw-message override error = %v", err)
	}

	if len(requests) != 2 {
		t.Fatalf("requests = %#v; rejected local validation must not call server", requests)
	}
	if requests[0].path !=
		"/v1/admin/accounts/acct_1/limit-overrides/agent_email_max_raw_bytes" ||
		requests[0].body["max"] != float64(10_485_760) ||
		requests[0].body["reason"] != "professional raw-message cap" {
		t.Fatalf("raw-message request = %#v", requests[0])
	}
	if requests[1].path !=
		"/v1/admin/accounts/acct_1/limit-overrides/agent_email_attachment_storage_bytes" ||
		requests[1].body["unlimited"] != true ||
		requests[1].body["reason"] != "founder attachment storage" {
		t.Fatalf("attachment-storage request = %#v", requests[1])
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
		{"message retention missing selection", func() error {
			_, err := SetAdminMessageRetention(
				t.Context(), "http://invalid", "t", "acct_1",
				AdminMessageRetentionInput{Reason: "r"})
			return err
		}},
		{"message retention conflicting selection", func() error {
			_, err := SetAdminMessageRetention(
				t.Context(), "http://invalid", "t", "acct_1",
				AdminMessageRetentionInput{
					Days: &days0, Indefinite: true, Reason: "r",
				})
			return err
		}},
		{"message retention zero days", func() error {
			_, err := SetAdminMessageRetention(
				t.Context(), "http://invalid", "t", "acct_1",
				AdminMessageRetentionInput{Days: &days0, Reason: "r"})
			return err
		}},
		{"messaging missing reason", func() error {
			_, err := SetAdminMessaging(
				t.Context(), "http://invalid", "t", "acct_1", true, "")
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
