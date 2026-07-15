package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestMemoryCurationHTTPContract(t *testing.T) {
	seen := map[string]int{}
	auth := func(_ context.Context, token string) (DomainPrincipal, bool, error) {
		kind := PrincipalKindAgent
		if token == "operator" {
			kind = PrincipalKindOperator
		}
		return DomainPrincipal{
			Kind: kind, ID: "agent_1", AccountID: "acc_1", RealmID: "realm_1",
			AccountStatus: "active",
		}, true, nil
	}
	cfg := Config{AuthenticatePrincipal: auth}
	cfg.RequestMemoryCuration = func(_ context.Context, _ DomainPrincipal, in RequestMemoryCurationRequest) (any, error) {
		seen["request"]++
		if in.TriggerReason != "manual" || in.IdempotencyKey != "request-key" {
			t.Errorf("request input = %#v", in)
		}
		return map[string]any{"request": map[string]any{"id": "mcrq_1"}, "receipt": map[string]any{}}, nil
	}
	cfg.ListMemoryCurationRequests = func(_ context.Context, _ DomainPrincipal, opts MemoryCurationRequestListOptions) (any, error) {
		seen["list"]++
		if opts.State != "queued" || opts.Limit != 17 || opts.Cursor != "request cursor" || !opts.ExcludeSensitive {
			t.Errorf("list options = %#v", opts)
		}
		return map[string]any{"requests": []any{}, "next_cursor": "next"}, nil
	}
	cfg.GetMemoryCurationRequest = func(_ context.Context, _ DomainPrincipal, requestID string) (any, error) {
		seen["get-request"]++
		if requestID != "mcrq_1" {
			t.Errorf("request id = %q", requestID)
		}
		return map[string]any{"id": requestID}, nil
	}
	cfg.StartMemoryCuration = func(_ context.Context, _ DomainPrincipal, in StartMemoryCurationRequest) (any, error) {
		seen["start"]++
		if in.RequestID != "mcrq_1" || in.LeaseDuration != 90*time.Second || in.IdempotencyKey != "start-key" {
			t.Errorf("start input = %#v", in)
		}
		return map[string]any{"run": map[string]any{"id": "mrun_1"}, "request": map[string]any{}, "receipt": map[string]any{}}, nil
	}
	cfg.GetMemoryCurationRun = func(_ context.Context, _ DomainPrincipal, runID string) (any, error) {
		seen["get-run"]++
		return map[string]any{"id": runID}, nil
	}
	cfg.GetMemoryCurationRunInputs = func(_ context.Context, _ DomainPrincipal, runID string, opts MemoryCurationRunInputOptions) (any, error) {
		seen["inputs"]++
		if runID != "mrun_1" || opts.FencingGeneration != 7 || opts.Cursor != "input cursor" || opts.Limit != 23 {
			t.Errorf("input page request = %q %#v", runID, opts)
		}
		return map[string]any{"run": map[string]any{"id": runID}, "inputs": []any{}}, nil
	}
	cfg.RenewMemoryCuration = func(_ context.Context, _ DomainPrincipal, runID string, in RenewMemoryCurationRequest) (any, error) {
		seen["renew"]++
		if runID != "mrun_1" || in.FencingGeneration != 7 || in.Extension != 45*time.Second || in.IdempotencyKey != "renew-key" {
			t.Errorf("renew input = %q %#v", runID, in)
		}
		return map[string]any{"run": map[string]any{}, "receipt": map[string]any{}}, nil
	}
	cfg.PlanMemoryCuration = func(_ context.Context, _ DomainPrincipal, runID string, in PlanMemoryCurationRequest) (any, error) {
		seen["plan"]++
		if runID != "mrun_1" || in.FencingGeneration != 7 || in.IdempotencyKey != "plan-key" ||
			!bytes.Contains(in.Draft, []byte(`"draft_revision":4`)) {
			t.Errorf("plan input = %q %#v", runID, in)
		}
		return map[string]any{
			"run": map[string]any{}, "plan": json.RawMessage(`{"schema":"witself.memory-plan.v1","plan_revision":5,"actions":[]}`),
			"preview": map[string]any{}, "receipt": map[string]any{},
		}, nil
	}
	cfg.ApplyMemoryCuration = func(_ context.Context, _ DomainPrincipal, runID string, in ApplyMemoryCurationRequest) (any, error) {
		seen["apply"]++
		if in.FencingGeneration != 7 || in.PlanRevision != 5 || in.PlanHash != strings.Repeat("a", 64) || in.IdempotencyKey != "apply-key" {
			t.Errorf("apply input = %q %#v", runID, in)
		}
		return map[string]any{"run": map[string]any{}, "request": map[string]any{}, "receipt": map[string]any{}}, nil
	}
	cfg.CancelMemoryCuration = func(_ context.Context, _ DomainPrincipal, _ string, in FinishMemoryCurationRequest) (any, error) {
		seen["cancel"]++
		if in.Reason != "stop" || in.IdempotencyKey != "cancel-key" {
			t.Errorf("cancel input = %#v", in)
		}
		return map[string]any{"run": map[string]any{}, "receipt": map[string]any{}}, nil
	}
	cfg.AbandonMemoryCuration = func(_ context.Context, _ DomainPrincipal, _ string, in FinishMemoryCurationRequest) (any, error) {
		seen["abandon"]++
		if in.Reason != "give_up" || in.IdempotencyKey != "abandon-key" {
			t.Errorf("abandon input = %#v", in)
		}
		return map[string]any{"run": map[string]any{}, "receipt": map[string]any{}}, nil
	}
	cfg.RollbackMemoryCuration = func(_ context.Context, _ DomainPrincipal, runID string, in RollbackMemoryCurationRequest) (any, error) {
		seen["rollback"]++
		if runID != "mrun_1" || in.ApplyReceiptID != "mrec_apply" || in.IdempotencyKey != "rollback-key" ||
			len(in.ExpectedProducedHeads) != 1 || in.ExpectedProducedHeads[0].MemoryID != "mem_1" {
			t.Errorf("rollback input = %q %#v", runID, in)
		}
		return map[string]any{"run": map[string]any{}, "replay_request": map[string]any{}, "receipt": map[string]any{}}, nil
	}
	cfg.GetMemoryCurationStatus = func(_ context.Context, _ DomainPrincipal, runID string) (any, error) {
		seen["status"]++
		if runID != "mrun_1" {
			t.Errorf("status run id = %q", runID)
		}
		return map[string]any{"lane": map[string]any{}, "run": map[string]any{"id": runID}}, nil
	}

	srv := httptest.NewServer(apiMux(cfg))
	defer srv.Close()
	tests := []struct {
		name, method, path, body, key string
		status                        int
	}{
		{"request", http.MethodPost, "/v1/memory-curation-requests", `{"scope":{"sources":[],"memory_states":[],"include_sensitive":false,"max_memories":0,"max_evidence":0,"max_transcript_entries":0},"coalescing_key":"owner","trigger_reason":"manual"}`, "request-key", http.StatusCreated},
		{"list", http.MethodGet, "/v1/memory-curation-requests?state=queued&limit=17&cursor=request+cursor&exclude_sensitive=true", "", "", http.StatusOK},
		{"get request", http.MethodGet, "/v1/memory-curation-requests/mcrq_1", "", "", http.StatusOK},
		{"start", http.MethodPost, "/v1/memory-curation-requests/mcrq_1/start", `{"lease_seconds":90}`, "start-key", http.StatusCreated},
		{"get run", http.MethodGet, "/v1/memory-curation-runs/mrun_1", "", "", http.StatusOK},
		{"inputs", http.MethodGet, "/v1/memory-curation-runs/mrun_1/inputs?fencing_generation=7&cursor=input+cursor&limit=23", "", "", http.StatusOK},
		{"renew", http.MethodPost, "/v1/memory-curation-runs/mrun_1/renew", `{"fencing_generation":7,"extension_seconds":45}`, "renew-key", http.StatusOK},
		{"plan", http.MethodPost, "/v1/memory-curation-runs/mrun_1/plan", `{"fencing_generation":7,"draft":{"schema":"witself.memory-plan.v1","draft_revision":4,"actions":[]}}`, "plan-key", http.StatusOK},
		{"apply", http.MethodPost, "/v1/memory-curation-runs/mrun_1/apply", `{"fencing_generation":7,"plan_revision":5,"plan_hash":"` + strings.Repeat("a", 64) + `"}`, "apply-key", http.StatusOK},
		{"cancel", http.MethodPost, "/v1/memory-curation-runs/mrun_1/cancel", `{"fencing_generation":7,"reason":"stop"}`, "cancel-key", http.StatusOK},
		{"abandon", http.MethodPost, "/v1/memory-curation-runs/mrun_1/abandon", `{"fencing_generation":7,"reason":"give_up"}`, "abandon-key", http.StatusOK},
		{"rollback", http.MethodPost, "/v1/memory-curation-runs/mrun_1/rollback", `{"apply_receipt_id":"mrec_apply","expected_produced_heads":[{"memory_id":"mem_1","version":2}],"reason":"bad synthesis"}`, "rollback-key", http.StatusOK},
		{"status", http.MethodGet, "/v1/memory-curation-status?run_id=mrun_1", "", "", http.StatusOK},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := memoryCurationHTTPResponse(t, srv.URL, "agent", test.method, test.path, test.body, test.key)
			defer func() { _ = response.Body.Close() }()
			if response.StatusCode != test.status {
				body, _ := io.ReadAll(response.Body)
				t.Fatalf("status = %d, body = %s", response.StatusCode, body)
			}
			if response.Header.Get("Cache-Control") != "private, no-store" {
				t.Fatalf("cache-control = %q", response.Header.Get("Cache-Control"))
			}
			var document map[string]json.RawMessage
			if err := json.NewDecoder(response.Body).Decode(&document); err != nil {
				t.Fatal(err)
			}
			if string(document["schema_version"]) != `"witself.v0"` {
				t.Fatalf("schema version = %s", document["schema_version"])
			}
		})
	}
	for _, name := range []string{
		"request", "list", "get-request", "start", "get-run", "inputs", "renew",
		"plan", "apply", "cancel", "abandon", "rollback", "status",
	} {
		if seen[name] != 1 {
			t.Fatalf("callback %s called %d times", name, seen[name])
		}
	}

	invalidFilter := memoryCurationHTTPResponse(t, srv.URL, "agent", http.MethodGet,
		"/v1/memory-curation-requests?exclude_sensitive=sometimes", "", "")
	defer func() { _ = invalidFilter.Body.Close() }()
	if invalidFilter.StatusCode != http.StatusBadRequest || seen["list"] != 1 {
		t.Fatalf("invalid exclude_sensitive response = %d, list calls = %d", invalidFilter.StatusCode, seen["list"])
	}

	response := memoryCurationHTTPResponse(t, srv.URL, "operator", http.MethodGet,
		"/v1/memory-curation-status", "", "")
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusForbidden || response.Header.Get("Cache-Control") != "private, no-store" {
		t.Fatalf("operator response = %d headers=%v", response.StatusCode, response.Header)
	}
}

func TestMemoryCurationHTTPRejectsAmbiguousOrHostileJSON(t *testing.T) {
	called := 0
	auth := func(context.Context, string) (DomainPrincipal, bool, error) {
		return DomainPrincipal{
			Kind: PrincipalKindAgent, ID: "agent_1", AccountID: "acc_1",
			RealmID: "realm_1", AccountStatus: "active",
		}, true, nil
	}
	cfg := Config{
		AuthenticatePrincipal: auth,
		RequestMemoryCuration: func(context.Context, DomainPrincipal, RequestMemoryCurationRequest) (any, error) {
			called++
			return map[string]any{}, nil
		},
		StartMemoryCuration: func(context.Context, DomainPrincipal, StartMemoryCurationRequest) (any, error) {
			called++
			return map[string]any{}, nil
		},
		PlanMemoryCuration: func(context.Context, DomainPrincipal, string, PlanMemoryCurationRequest) (any, error) {
			called++
			return map[string]any{}, nil
		},
	}
	srv := httptest.NewServer(apiMux(cfg))
	defer srv.Close()

	deep := strings.Repeat(`{"x":`, maxMemoryCurationJSONDepth+1) + `0` +
		strings.Repeat(`}`, maxMemoryCurationJSONDepth+1)
	tests := []struct{ path, body, key string }{
		{"/v1/memory-curation-requests", `{"scope":{},"trigger_reason":"one","trigger_reason":"two"}`, "key"},
		{"/v1/memory-curation-requests", `{"scope":{},"unknown":true}`, "key"},
		{"/v1/memory-curation-requests/mcrq_1/start", `{"lease_seconds":1.5}`, "key"},
		{"/v1/memory-curation-runs/mrun_1/plan", `{"fencing_generation":1,"draft":{"schema":"one","schema":"two"}}`, "key"},
		{"/v1/memory-curation-runs/mrun_1/plan", `{"fencing_generation":1,"draft":` + deep + `}`, "key"},
		{"/v1/memory-curation-requests", `{}`, ""},
	}
	for _, test := range tests {
		response := memoryCurationHTTPResponse(t, srv.URL, "agent", http.MethodPost, test.path, test.body, test.key)
		_ = response.Body.Close()
		if response.StatusCode != http.StatusBadRequest {
			t.Fatalf("%s body prefix %.80q status = %d", test.path, test.body, response.StatusCode)
		}
		if response.Header.Get("Cache-Control") != "private, no-store" {
			t.Fatalf("cache-control = %q", response.Header.Get("Cache-Control"))
		}
	}
	if called != 0 {
		t.Fatalf("callbacks invoked %d times for rejected requests", called)
	}
}

func TestMemoryCurationCapabilityRequiresCompleteSurface(t *testing.T) {
	auth := func(context.Context, string) (DomainPrincipal, bool, error) {
		return DomainPrincipal{Kind: PrincipalKindAgent, AccountStatus: "active"}, true, nil
	}
	noopOne := func(context.Context, DomainPrincipal, string) (any, error) { return map[string]any{}, nil }
	noopFinish := func(context.Context, DomainPrincipal, string, FinishMemoryCurationRequest) (any, error) {
		return map[string]any{}, nil
	}
	cfg := Config{
		AuthenticatePrincipal: auth,
		RequestMemoryCuration: func(context.Context, DomainPrincipal, RequestMemoryCurationRequest) (any, error) {
			return map[string]any{}, nil
		},
		ListMemoryCurationRequests: func(context.Context, DomainPrincipal, MemoryCurationRequestListOptions) (any, error) {
			return map[string]any{}, nil
		},
		GetMemoryCurationRequest: noopOne,
		StartMemoryCuration: func(context.Context, DomainPrincipal, StartMemoryCurationRequest) (any, error) {
			return map[string]any{}, nil
		},
		GetMemoryCurationRun: noopOne,
		GetMemoryCurationRunInputs: func(context.Context, DomainPrincipal, string, MemoryCurationRunInputOptions) (any, error) {
			return map[string]any{}, nil
		},
		RenewMemoryCuration: func(context.Context, DomainPrincipal, string, RenewMemoryCurationRequest) (any, error) {
			return map[string]any{}, nil
		},
		PlanMemoryCuration: func(context.Context, DomainPrincipal, string, PlanMemoryCurationRequest) (any, error) {
			return map[string]any{}, nil
		},
		ApplyMemoryCuration: func(context.Context, DomainPrincipal, string, ApplyMemoryCurationRequest) (any, error) {
			return map[string]any{}, nil
		},
		CancelMemoryCuration: noopFinish, AbandonMemoryCuration: noopFinish,
		RollbackMemoryCuration: func(context.Context, DomainPrincipal, string, RollbackMemoryCurationRequest) (any, error) {
			return map[string]any{}, nil
		},
		GetMemoryCurationStatus: noopOne,
	}
	assertCapability := func(t *testing.T, cfg Config, supported bool) {
		t.Helper()
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodGet, "/v1/capabilities", nil)
		apiMux(cfg).ServeHTTP(recorder, request)
		var document struct {
			Features map[string]struct {
				Supported bool   `json:"supported"`
				Reason    string `json:"reason"`
			} `json:"features"`
		}
		if err := json.NewDecoder(recorder.Body).Decode(&document); err != nil {
			t.Fatal(err)
		}
		if document.Features["opportunistic_curation"].Supported != supported {
			t.Fatalf("opportunistic_curation = %#v", document.Features["opportunistic_curation"])
		}
		if document.Features["scheduled_curation"].Supported {
			t.Fatal("scheduled curation must remain unsupported without a client supervisor")
		}
	}
	assertCapability(t, cfg, true)
	cfg.RollbackMemoryCuration = nil
	assertCapability(t, cfg, false)
}

func TestMemoryCurationAccessProfilesEnforceExactRouteMatrix(t *testing.T) {
	type route struct {
		name, method, path, body, key string
		preview, apply                bool
	}
	routes := []route{
		{name: "request", method: http.MethodPost, path: "/v1/memory-curation-requests", body: `{"scope":{},"trigger_reason":"manual"}`, key: "request-key"},
		{name: "list", method: http.MethodGet, path: "/v1/memory-curation-requests", preview: true, apply: true},
		{name: "get-request", method: http.MethodGet, path: "/v1/memory-curation-requests/mcrq_1", preview: true, apply: true},
		{name: "start", method: http.MethodPost, path: "/v1/memory-curation-requests/mcrq_1/start", body: `{"lease_seconds":90}`, key: "start-key", preview: true, apply: true},
		{name: "get-run", method: http.MethodGet, path: "/v1/memory-curation-runs/mrun_1", preview: true, apply: true},
		{name: "inputs", method: http.MethodGet, path: "/v1/memory-curation-runs/mrun_1/inputs?fencing_generation=7", preview: true, apply: true},
		{name: "renew", method: http.MethodPost, path: "/v1/memory-curation-runs/mrun_1/renew", body: `{"fencing_generation":7,"extension_seconds":45}`, key: "renew-key", preview: true, apply: true},
		{name: "plan", method: http.MethodPost, path: "/v1/memory-curation-runs/mrun_1/plan", body: `{"fencing_generation":7,"draft":{"schema":"witself.memory-plan.v1","draft_revision":1,"actions":[]}}`, key: "plan-key", preview: true, apply: true},
		{name: "apply", method: http.MethodPost, path: "/v1/memory-curation-runs/mrun_1/apply", body: `{"fencing_generation":7,"plan_revision":1,"plan_hash":"` + strings.Repeat("a", 64) + `"}`, key: "apply-key", apply: true},
		{name: "cancel", method: http.MethodPost, path: "/v1/memory-curation-runs/mrun_1/cancel", body: `{"fencing_generation":7}`, key: "cancel-key"},
		{name: "abandon", method: http.MethodPost, path: "/v1/memory-curation-runs/mrun_1/abandon", body: `{"fencing_generation":7}`, key: "abandon-key", preview: true, apply: true},
		{name: "rollback", method: http.MethodPost, path: "/v1/memory-curation-runs/mrun_1/rollback", body: `{"apply_receipt_id":"mrec_1","expected_produced_heads":[{"memory_id":"mem_1","version":1}]}`, key: "rollback-key"},
		{name: "status", method: http.MethodGet, path: "/v1/memory-curation-status?run_id=mrun_1", preview: true, apply: true},
	}

	profiles := []struct {
		name, profile string
		allowed       func(route) bool
	}{
		{name: "full", profile: AccessProfileFull, allowed: func(route) bool { return true }},
		{name: "preview", profile: AccessProfileCuratorPreview, allowed: func(r route) bool { return r.preview }},
		{name: "apply", profile: AccessProfileCuratorApply, allowed: func(r route) bool { return r.apply }},
		{name: "unknown", profile: "curator-superuser", allowed: func(route) bool { return false }},
	}

	for _, profile := range profiles {
		t.Run(profile.name, func(t *testing.T) {
			called := map[string]int{}
			auth := func(context.Context, string) (DomainPrincipal, bool, error) {
				return DomainPrincipal{
					Kind: PrincipalKindAgent, ID: "agent_1", AccountID: "acc_1", RealmID: "realm_1",
					AccountStatus: "active", AccessProfile: profile.profile,
				}, true, nil
			}
			cfg := completeMemoryCurationTestConfig(auth, called)
			srv := httptest.NewServer(apiMux(cfg))
			defer srv.Close()

			for _, route := range routes {
				response := memoryCurationHTTPResponse(t, srv.URL, "agent", route.method, route.path, route.body, route.key)
				_ = response.Body.Close()
				wantAllowed := profile.allowed(route)
				if wantAllowed && response.StatusCode >= http.StatusBadRequest {
					t.Errorf("%s status = %d, want allowed", route.name, response.StatusCode)
				}
				if !wantAllowed && response.StatusCode != http.StatusForbidden {
					t.Errorf("%s status = %d, want forbidden", route.name, response.StatusCode)
				}
				wantCalls := 0
				if wantAllowed {
					wantCalls = 1
				}
				if called[route.name] != wantCalls {
					t.Errorf("%s callback calls = %d, want %d", route.name, called[route.name], wantCalls)
				}
			}
		})
	}
}

func TestMemoryCurationPreflightReportsEffectiveCredential(t *testing.T) {
	expiresAt := time.Date(2026, 7, 15, 12, 30, 0, 0, time.UTC)
	auth := func(_ context.Context, token string) (DomainPrincipal, bool, error) {
		kind := PrincipalKindAgent
		if token == "operator" {
			kind = PrincipalKindOperator
		}
		return DomainPrincipal{
			Kind: kind, ID: "agent_1", AccountID: "acc_1", RealmID: "realm_1", AgentName: "memory-agent",
			AccountStatus: "active", TokenID: "tok_curator", AccessProfile: AccessProfileCuratorPreview,
			TokenExpiresAt: &expiresAt,
		}, true, nil
	}
	cfg := completeMemoryCurationTestConfig(auth, map[string]int{})
	srv := httptest.NewServer(apiMux(cfg))
	defer srv.Close()

	response := memoryCurationHTTPResponse(t, srv.URL, "curator", http.MethodGet, "/v1/memory-curation-preflight", "", "")
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK || response.Header.Get("Cache-Control") != "private, no-store" {
		t.Fatalf("preflight response = %d headers=%v", response.StatusCode, response.Header)
	}
	var out struct {
		SchemaVersion string                             `json:"schema_version"`
		Principal     MemoryCurationPreflightPrincipal   `json:"principal"`
		Credential    MemoryCurationPreflightCredential  `json:"credential"`
		Protocol      MemoryCurationPreflightProtocol    `json:"protocol"`
		Permissions   MemoryCurationPreflightPermissions `json:"permissions"`
		Limits        MemoryCurationPreflightLimits      `json:"limits"`
	}
	if err := json.NewDecoder(response.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out.SchemaVersion != "witself.v0" || out.Principal.AgentID != "agent_1" ||
		out.Principal.AgentName != "memory-agent" || out.Credential.TokenID != "tok_curator" ||
		out.Credential.AccessProfile != AccessProfileCuratorPreview ||
		out.Credential.ExpiresAt == nil || !out.Credential.ExpiresAt.Equal(expiresAt) {
		t.Fatalf("preflight identity = %#v", out)
	}
	if out.Protocol.PlanSchema != "witself.memory-plan.v1" || out.Protocol.BackendInference ||
		!out.Protocol.ClientInferenceRequired || len(out.Protocol.AllowedPrimitives) != 5 {
		t.Fatalf("preflight protocol = %#v", out.Protocol)
	}
	permissions := out.Permissions
	if !permissions.ListRequests || !permissions.GetRequest || !permissions.Start || !permissions.GetRun ||
		!permissions.GetInputs || !permissions.Renew || !permissions.Plan || !permissions.Abandon ||
		permissions.Apply || permissions.CreateRequest || permissions.Cancel || permissions.Rollback ||
		permissions.IncludeSensitive || permissions.DirectMemoryWrite || permissions.CanonicalFactWrite ||
		permissions.MessageWrite || permissions.PermanentDelete {
		t.Fatalf("preflight permissions = %#v", permissions)
	}
	if out.Limits.MaxPageSize <= 0 || out.Limits.MaxPlanBytes <= 0 ||
		out.Limits.MinLeaseSeconds >= out.Limits.MaxLeaseSeconds {
		t.Fatalf("preflight limits = %#v", out.Limits)
	}

	operator := memoryCurationHTTPResponse(t, srv.URL, "operator", http.MethodGet, "/v1/memory-curation-preflight", "", "")
	_ = operator.Body.Close()
	if operator.StatusCode != http.StatusForbidden {
		t.Fatalf("operator preflight status = %d", operator.StatusCode)
	}
}

func TestCuratorCredentialCannotReachOrdinaryDomainRoutes(t *testing.T) {
	for _, profile := range []string{AccessProfileCuratorPreview, AccessProfileCuratorApply} {
		t.Run(profile, func(t *testing.T) {
			called := map[string]int{}
			auth := func(context.Context, string) (DomainPrincipal, bool, error) {
				return DomainPrincipal{
					Kind: PrincipalKindAgent, ID: "agent_1", AccountID: "acc_1", RealmID: "realm_1",
					AccountStatus: "active", AccessProfile: profile,
				}, true, nil
			}
			cfg := Config{
				AuthenticatePrincipal: auth,
				CaptureMemory: func(context.Context, DomainPrincipal, CaptureMemoryRequest) (MemoryMutationResult, error) {
					called["memory"]++
					return MemoryMutationResult{}, nil
				},
				DeleteMemory: func(context.Context, DomainPrincipal, DeleteMemoryRequest) (MemoryDeletionReceipt, error) {
					called["delete"]++
					return MemoryDeletionReceipt{}, nil
				},
				SetFact: func(context.Context, DomainPrincipal, SetFactRequest) (Fact, error) {
					called["fact"]++
					return Fact{}, nil
				},
				SendMessage: func(context.Context, DomainPrincipal, SendMessageRequest) (Message, error) {
					called["message"]++
					return Message{}, nil
				},
			}
			srv := httptest.NewServer(apiMux(cfg))
			defer srv.Close()

			requests := []struct {
				name, method, path, body, key string
				directUser                    bool
			}{
				{name: "memory", method: http.MethodPost, path: "/v1/memories", body: `{}`},
				{name: "delete", method: http.MethodDelete, path: "/v1/memories/mem_1?expected_version=1&scrub_set_revision=revision", key: "delete-key", directUser: true},
				{name: "fact", method: http.MethodPost, path: "/v1/facts", body: `{}`},
				{name: "message", method: http.MethodPost, path: "/v1/messages", body: `{}`},
			}
			for _, request := range requests {
				req, err := http.NewRequest(request.method, srv.URL+request.path, strings.NewReader(request.body))
				if err != nil {
					t.Fatal(err)
				}
				req.Header.Set("Authorization", "Bearer curator")
				if request.body != "" {
					req.Header.Set("Content-Type", "application/json")
				}
				if request.key != "" {
					req.Header.Set("Idempotency-Key", request.key)
				}
				if request.directUser {
					req.Header.Set("X-Witself-Direct-User-Authorized", "true")
				}
				response, err := http.DefaultClient.Do(req)
				if err != nil {
					t.Fatal(err)
				}
				_ = response.Body.Close()
				if response.StatusCode != http.StatusForbidden {
					t.Errorf("%s status = %d, want forbidden", request.name, response.StatusCode)
				}
			}
			for name, count := range called {
				if count != 0 {
					t.Errorf("%s callback invoked %d times", name, count)
				}
			}
		})
	}
}

func completeMemoryCurationTestConfig(auth PrincipalAuthFunc, called map[string]int) Config {
	mark := func(name string) { called[name]++ }
	return Config{
		AuthenticatePrincipal: auth,
		RequestMemoryCuration: func(context.Context, DomainPrincipal, RequestMemoryCurationRequest) (any, error) {
			mark("request")
			return map[string]any{}, nil
		},
		ListMemoryCurationRequests: func(context.Context, DomainPrincipal, MemoryCurationRequestListOptions) (any, error) {
			mark("list")
			return map[string]any{}, nil
		},
		GetMemoryCurationRequest: func(context.Context, DomainPrincipal, string) (any, error) {
			mark("get-request")
			return map[string]any{}, nil
		},
		StartMemoryCuration: func(context.Context, DomainPrincipal, StartMemoryCurationRequest) (any, error) {
			mark("start")
			return map[string]any{}, nil
		},
		GetMemoryCurationRun: func(context.Context, DomainPrincipal, string) (any, error) {
			mark("get-run")
			return map[string]any{}, nil
		},
		GetMemoryCurationRunInputs: func(context.Context, DomainPrincipal, string, MemoryCurationRunInputOptions) (any, error) {
			mark("inputs")
			return map[string]any{}, nil
		},
		RenewMemoryCuration: func(context.Context, DomainPrincipal, string, RenewMemoryCurationRequest) (any, error) {
			mark("renew")
			return map[string]any{}, nil
		},
		PlanMemoryCuration: func(context.Context, DomainPrincipal, string, PlanMemoryCurationRequest) (any, error) {
			mark("plan")
			return map[string]any{}, nil
		},
		ApplyMemoryCuration: func(context.Context, DomainPrincipal, string, ApplyMemoryCurationRequest) (any, error) {
			mark("apply")
			return map[string]any{}, nil
		},
		CancelMemoryCuration: func(context.Context, DomainPrincipal, string, FinishMemoryCurationRequest) (any, error) {
			mark("cancel")
			return map[string]any{}, nil
		},
		AbandonMemoryCuration: func(context.Context, DomainPrincipal, string, FinishMemoryCurationRequest) (any, error) {
			mark("abandon")
			return map[string]any{}, nil
		},
		RollbackMemoryCuration: func(context.Context, DomainPrincipal, string, RollbackMemoryCurationRequest) (any, error) {
			mark("rollback")
			return map[string]any{}, nil
		},
		GetMemoryCurationStatus: func(context.Context, DomainPrincipal, string) (any, error) {
			mark("status")
			return map[string]any{}, nil
		},
	}
}

func TestMemoryCurationErrorMapping(t *testing.T) {
	tests := []struct {
		err    error
		status int
		text   string
	}{
		{ErrBadInput, http.StatusBadRequest, "invalid memory curation request"},
		{ErrForbidden, http.StatusForbidden, "memory curation access forbidden"},
		{ErrNotFound, http.StatusNotFound, "memory curation resource not found"},
		{ErrIdempotencyConflict, http.StatusConflict, "idempotency key was reused"},
		{ErrMemoryCurationBusy, http.StatusConflict, ErrMemoryCurationBusy.Error()},
		{ErrMemoryCurationNotDue, http.StatusConflict, ErrMemoryCurationNotDue.Error()},
		{ErrMemoryCurationLeaseExpired, http.StatusConflict, ErrMemoryCurationLeaseExpired.Error()},
		{ErrMemoryCurationFenceMismatch, http.StatusConflict, ErrMemoryCurationFenceMismatch.Error()},
		{ErrConflict, http.StatusConflict, "memory curation state conflict"},
	}
	for _, test := range tests {
		recorder := httptest.NewRecorder()
		if !writeMemoryCurationError(recorder, test.err) {
			t.Fatal("error was not written")
		}
		if recorder.Code != test.status || !strings.Contains(recorder.Body.String(), test.text) {
			t.Fatalf("error %v => %d %s", test.err, recorder.Code, recorder.Body.String())
		}
	}
	blocked := &MemoryCurationRollbackBlockedError{Blockers: []MemoryCurationRollbackBlocker{{
		Kind: "dependent_memory", MemoryID: "mem_1", Version: 2,
	}}}
	if !errors.Is(blocked, ErrConflict) {
		t.Fatal("rollback blocker does not unwrap to conflict")
	}
	recorder := httptest.NewRecorder()
	writeMemoryCurationError(recorder, blocked)
	if recorder.Code != http.StatusConflict || !strings.Contains(recorder.Body.String(), `"blockers"`) {
		t.Fatalf("blocked response = %d %s", recorder.Code, recorder.Body.String())
	}
}

func memoryCurationHTTPResponse(
	t *testing.T,
	endpoint, token, method, path, body, idempotencyKey string,
) *http.Response {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	request, err := http.NewRequest(method, endpoint+path, reader)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+token)
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	if idempotencyKey != "" {
		request.Header.Set("Idempotency-Key", idempotencyKey)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	return response
}
