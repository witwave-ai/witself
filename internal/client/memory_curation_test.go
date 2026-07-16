package client

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestMemoryCurationClientHTTPContract(t *testing.T) {
	seen := map[string]int{}
	keys := map[string]string{
		"POST /v1/memory-curation-requests":              "request-key",
		"POST /v1/memory-curation-requests/mcrq_1/start": "start-key",
		"POST /v1/memory-curation-runs/mrun_1/renew":     "renew-key",
		"POST /v1/memory-curation-runs/mrun_1/plan":      "plan-key",
		"POST /v1/memory-curation-runs/mrun_1/apply":     "apply-key",
		"POST /v1/memory-curation-runs/mrun_1/cancel":    "cancel-key",
		"POST /v1/memory-curation-runs/mrun_1/abandon":   "abandon-key",
		"POST /v1/memory-curation-runs/mrun_1/rollback":  "rollback-key",
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer token" {
			t.Fatalf("authorization = %q", r.Header.Get("Authorization"))
		}
		route := r.Method + " " + r.URL.Path
		seen[route]++
		if want, mutating := keys[route]; mutating && r.Header.Get("Idempotency-Key") != want {
			t.Fatalf("%s idempotency key = %q, want %q", route, r.Header.Get("Idempotency-Key"), want)
		}
		if _, mutating := keys[route]; !mutating && r.Header.Get("Idempotency-Key") != "" {
			t.Fatalf("%s unexpectedly carried an idempotency key", route)
		}

		switch route {
		case "GET /v1/memory-curation-preflight":
			w.Header().Set("Cache-Control", "private, no-store")
			_, _ = io.WriteString(w, `{"principal":{"account_id":"acc_1","realm_id":"realm_1","agent_id":"agent_1","agent_name":"primary"},"credential":{"token_id":"tok_1","access_profile":"curator-preview","expires_at":"2026-07-15T00:00:00Z"},"protocol":{"plan_schema":"witself.memory-plan.v1","allowed_primitives":["create"],"backend_inference":false,"client_inference_required":true},"permissions":{"list_requests":true,"get_request":true,"start":true,"get_run":true,"get_inputs":true,"get_plan":true,"renew":true,"plan":true,"abandon":true},"limits":{"max_page_size":200,"max_memories":500,"max_evidence":1000,"max_transcript_entries":2000,"min_lease_seconds":30,"max_lease_seconds":1800,"max_plan_actions":128,"max_plan_bytes":33554432}}`)
		case "POST /v1/memory-curation-requests":
			body := decodeMemoryCurationClientBody(t, r)
			if body["trigger_reason"] != "manual" {
				t.Fatalf("request body = %#v", body)
			}
			if _, leaked := body["idempotency_key"]; leaked {
				t.Fatalf("request key leaked into body = %#v", body)
			}
			_, _ = io.WriteString(w, `{"request":{"id":"mcrq_1"},"receipt":{}}`)
		case "GET /v1/memory-curation-requests":
			if r.URL.Query().Get("state") != "queued" || r.URL.Query().Get("limit") != "17" ||
				r.URL.Query().Get("cursor") != "request cursor" || r.URL.Query().Get("exclude_sensitive") != "true" {
				t.Fatalf("list query = %s", r.URL.RawQuery)
			}
			_, _ = io.WriteString(w, `{"requests":[],"next_cursor":"next"}`)
		case "GET /v1/memory-curation-requests/mcrq_1":
			_, _ = io.WriteString(w, `{"request":{"id":"mcrq_1"}}`)
		case "POST /v1/memory-curation-requests/mcrq_1/start":
			body := decodeMemoryCurationClientBody(t, r)
			if body["lease_seconds"] != float64(90) {
				t.Fatalf("start lease body = %#v", body)
			}
			if _, leaked := body["request_id"]; leaked {
				t.Fatalf("request id leaked into start body = %#v", body)
			}
			_, _ = io.WriteString(w, `{"run":{"id":"mrun_1"},"request":{"id":"mcrq_1"},"receipt":{},"first_input_cursor":"input-1"}`)
		case "GET /v1/memory-curation-runs/mrun_1":
			_, _ = io.WriteString(w, `{"run":{"id":"mrun_1"}}`)
		case "GET /v1/memory-curation-runs/mrun_1/inputs":
			if r.URL.Query().Get("fencing_generation") != "7" || r.URL.Query().Get("limit") != "23" ||
				r.URL.Query().Get("cursor") != "input cursor" {
				t.Fatalf("input query = %s", r.URL.RawQuery)
			}
			_, _ = io.WriteString(w, `{"run":{"id":"mrun_1"},"inputs":[],"next_cursor":"inputs-next"}`)
		case "GET /v1/memory-curation-runs/mrun_1/plan":
			if r.URL.Query().Get("fencing_generation") != "7" {
				t.Fatalf("get plan query = %s", r.URL.RawQuery)
			}
			_, _ = io.WriteString(w, `{"run":{"id":"mrun_1","fencing_generation":7},"plan":{"schema":"witself.memory-plan.v1","plan_revision":5,"plan_hash":"`+strings.Repeat("a", 64)+`","actions":[]},"preallocated_memory_ids":[],"preview":{"action_count":0}}`)
		case "POST /v1/memory-curation-runs/mrun_1/renew":
			body := decodeMemoryCurationClientBody(t, r)
			if body["fencing_generation"] != float64(7) || body["extension_seconds"] != float64(45) {
				t.Fatalf("renew body = %#v", body)
			}
			_, _ = io.WriteString(w, `{"run":{},"receipt":{}}`)
		case "POST /v1/memory-curation-runs/mrun_1/plan":
			body := decodeMemoryCurationClientBody(t, r)
			draft, ok := body["draft"].(map[string]any)
			if !ok || draft["schema"] != "witself.memory-plan.v1" || draft["draft_revision"] != float64(4) {
				t.Fatalf("raw plan draft body = %#v", body)
			}
			_, _ = io.WriteString(w, `{"run":{},"plan":{"schema":"witself.memory-plan.v1","plan_revision":5,"actions":[]},"preview":{},"receipt":{}}`)
		case "POST /v1/memory-curation-runs/mrun_1/apply":
			body := decodeMemoryCurationClientBody(t, r)
			if body["plan_revision"] != float64(5) || body["plan_hash"] != strings.Repeat("a", 64) {
				t.Fatalf("apply body = %#v", body)
			}
			_, _ = io.WriteString(w, `{"run":{},"request":{},"receipt":{}}`)
		case "POST /v1/memory-curation-runs/mrun_1/cancel":
			body := decodeMemoryCurationClientBody(t, r)
			if body["reason"] != "stop" || body["fencing_generation"] != float64(7) {
				t.Fatalf("cancel body = %#v", body)
			}
			_, _ = io.WriteString(w, `{"run":{},"receipt":{}}`)
		case "POST /v1/memory-curation-runs/mrun_1/abandon":
			body := decodeMemoryCurationClientBody(t, r)
			if body["reason"] != "give_up" || body["fencing_generation"] != float64(7) {
				t.Fatalf("abandon body = %#v", body)
			}
			_, _ = io.WriteString(w, `{"run":{},"receipt":{}}`)
		case "POST /v1/memory-curation-runs/mrun_1/rollback":
			body := decodeMemoryCurationClientBody(t, r)
			if body["apply_receipt_id"] != "mrec_apply" || body["reason"] != "bad synthesis" {
				t.Fatalf("rollback body = %#v", body)
			}
			heads, ok := body["expected_produced_heads"].([]any)
			if !ok || len(heads) != 1 || heads[0].(map[string]any)["memory_id"] != "mem_1" {
				t.Fatalf("rollback heads = %#v", body["expected_produced_heads"])
			}
			if _, leaked := body["run_id"]; leaked {
				t.Fatalf("run id leaked into rollback body = %#v", body)
			}
			if _, leaked := body["idempotency_key"]; leaked {
				t.Fatalf("rollback key leaked into body = %#v", body)
			}
			_, _ = io.WriteString(w, `{"run":{},"replay_request":{},"receipt":{}}`)
		case "GET /v1/memory-curation-status":
			if r.URL.Query().Get("run_id") != "mrun_1" {
				t.Fatalf("status query = %s", r.URL.RawQuery)
			}
			_, _ = io.WriteString(w, `{"lane":{},"run":{"id":"mrun_1"}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	ctx := context.Background()
	preflight, err := GetMemoryCurationPreflight(ctx, srv.URL, "token")
	if err != nil {
		t.Fatal(err)
	}
	if preflight.Principal.AgentID != "agent_1" ||
		preflight.Credential.AccessProfile != "curator-preview" ||
		preflight.Credential.ExpiresAt == nil ||
		preflight.Protocol.PlanSchema != "witself.memory-plan.v1" ||
		len(preflight.Protocol.AllowedPrimitives) != 1 ||
		!preflight.Permissions.GetPlan || !preflight.Permissions.Plan || preflight.Permissions.Apply ||
		preflight.Limits.MaxPlanBytes != 32<<20 {
		t.Fatalf("preflight = %#v", preflight)
	}
	if _, err := RequestMemoryCuration(ctx, srv.URL, "token", RequestMemoryCurationInput{
		TriggerReason: "manual", IdempotencyKey: "request-key",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := ListMemoryCurationRequests(ctx, srv.URL, "token", MemoryCurationRequestListOptions{
		State: "queued", Limit: 17, Cursor: "request cursor", ExcludeSensitive: true,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := GetMemoryCurationRequest(ctx, srv.URL, "token", "mcrq_1"); err != nil {
		t.Fatal(err)
	}
	if _, err := StartMemoryCuration(ctx, srv.URL, "token", StartMemoryCurationInput{
		RequestID: "mcrq_1", LeaseDuration: 90*time.Second + 900*time.Millisecond,
		IdempotencyKey: "start-key",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := GetMemoryCurationRun(ctx, srv.URL, "token", "mrun_1"); err != nil {
		t.Fatal(err)
	}
	if _, err := GetMemoryCurationRunInputs(ctx, srv.URL, "token", "mrun_1", 7, "input cursor", 23); err != nil {
		t.Fatal(err)
	}
	accepted, err := GetMemoryCurationPlan(ctx, srv.URL, "token", "mrun_1", 7)
	if err != nil {
		t.Fatal(err)
	}
	if accepted.Run.ID != "mrun_1" || !json.Valid(accepted.Plan) ||
		!strings.Contains(string(accepted.Plan), `"plan_revision":5`) || accepted.PreallocatedMemoryIDs == nil {
		t.Fatalf("accepted plan = %#v", accepted)
	}
	if _, err := RenewMemoryCuration(ctx, srv.URL, "token", RenewMemoryCurationInput{
		RunID: "mrun_1", FencingGeneration: 7, Extension: 45*time.Second + time.Millisecond,
		IdempotencyKey: "renew-key",
	}); err != nil {
		t.Fatal(err)
	}
	planned, err := PlanMemoryCuration(ctx, srv.URL, "token", PlanMemoryCurationInput{
		RunID: "mrun_1", FencingGeneration: 7,
		Draft:          json.RawMessage(`{"schema":"witself.memory-plan.v1","draft_revision":4,"actions":[]}`),
		IdempotencyKey: "plan-key",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !json.Valid(planned.Plan) || !strings.Contains(string(planned.Plan), `"plan_revision":5`) {
		t.Fatalf("returned raw plan = %s", planned.Plan)
	}
	if _, err := ApplyMemoryCuration(ctx, srv.URL, "token", ApplyMemoryCurationInput{
		RunID: "mrun_1", FencingGeneration: 7, PlanRevision: 5,
		PlanHash: strings.Repeat("a", 64), IdempotencyKey: "apply-key",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := CancelMemoryCuration(ctx, srv.URL, "token", FinishMemoryCurationInput{
		RunID: "mrun_1", FencingGeneration: 7, Reason: "stop", IdempotencyKey: "cancel-key",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := AbandonMemoryCuration(ctx, srv.URL, "token", FinishMemoryCurationInput{
		RunID: "mrun_1", FencingGeneration: 7, Reason: "give_up", IdempotencyKey: "abandon-key",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := RollbackMemoryCuration(ctx, srv.URL, "token", RollbackMemoryCurationInput{
		RunID: "mrun_1", ApplyReceiptID: "mrec_apply", Reason: "bad synthesis",
		ExpectedProducedHeads: []MemoryVersionReference{{MemoryID: "mem_1", Version: 2}},
		IdempotencyKey:        "rollback-key",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := GetMemoryCurationStatus(ctx, srv.URL, "token", "mrun_1"); err != nil {
		t.Fatal(err)
	}

	for route := range keys {
		if seen[route] != 1 {
			t.Fatalf("%s seen %d times", route, seen[route])
		}
	}
	for _, route := range []string{
		"GET /v1/memory-curation-preflight",
		"GET /v1/memory-curation-requests", "GET /v1/memory-curation-requests/mcrq_1",
		"GET /v1/memory-curation-runs/mrun_1", "GET /v1/memory-curation-runs/mrun_1/inputs",
		"GET /v1/memory-curation-runs/mrun_1/plan",
		"GET /v1/memory-curation-status",
	} {
		if seen[route] != 1 {
			t.Fatalf("%s seen %d times", route, seen[route])
		}
	}
}

func decodeMemoryCurationClientBody(t *testing.T, r *http.Request) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	return body
}
