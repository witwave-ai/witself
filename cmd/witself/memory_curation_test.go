package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/witwave-ai/witself/internal/client"
)

func TestMemoryCurationCommandsCoverGuardedWorkflow(t *testing.T) {
	tokenFile := filepath.Join(t.TempDir(), "agent.token")
	if err := os.WriteFile(tokenFile, []byte("witself_agt_curation\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	planFile := filepath.Join(t.TempDir(), "plan.json")
	if err := os.WriteFile(planFile, []byte(`{"schema":"witself.memory-plan.v1","draft_revision":1,"actions":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	headsFile := filepath.Join(t.TempDir(), "heads.json")
	if err := os.WriteFile(headsFile, []byte(`[{"memory_id":"mem_created","version":1}]`), 0o600); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 14, 15, 0, 0, 0, time.UTC)
	planHash := strings.Repeat("a", 64)
	request := client.MemoryCurationRequest{ID: "mcrq_testrequest000", State: "queued", RequestGeneration: 7, DueAt: now, MaxAttempts: 5}
	run := client.MemoryCurationRun{ID: "mrun_testrun0000000", RequestID: request.ID, RequestGeneration: 7, FencingGeneration: 4, State: "open", LeaseExpiresAt: ptrTime(now.Add(5 * time.Minute)), InputCount: 1}
	var mu sync.Mutex
	seen := map[string]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer witself_agt_curation" {
			t.Errorf("authorization = %q", got)
		}
		key := r.Method + " " + r.URL.Path
		mu.Lock()
		seen[key]++
		mu.Unlock()
		switch key {
		case "POST /v1/memory-curation-requests":
			assertCurationKey(t, r, "request-key")
			body := decodeCurationCLIRequest(t, r)
			if body["coalescing_key"] != "manual" || body["trigger_reason"] != "manual_refine" {
				t.Errorf("request body = %#v", body)
			}
			_ = json.NewEncoder(w).Encode(client.RequestMemoryCurationResult{Request: request, Receipt: client.MemoryCurationMutationReceipt{Replayed: false}})
		case "GET /v1/memory-curation-requests":
			if r.URL.Query().Get("state") != "queued" || r.URL.Query().Get("limit") != "10" {
				t.Errorf("request list query = %s", r.URL.RawQuery)
			}
			_ = json.NewEncoder(w).Encode(client.MemoryCurationRequestPage{Requests: []client.MemoryCurationRequest{request}, NextCursor: "next"})
		case "POST /v1/memory-curation-requests/" + request.ID + "/start":
			assertCurationKey(t, r, "start-key")
			body := decodeCurationCLIRequest(t, r)
			if body["lease_seconds"] != float64(90) {
				t.Errorf("start body = %#v", body)
			}
			_ = json.NewEncoder(w).Encode(client.StartMemoryCurationResult{Run: run, Request: request, FirstInputCursor: "first"})
		case "GET /v1/memory-curation-runs/" + run.ID + "/inputs":
			if r.URL.Query().Get("fencing_generation") != "4" || r.URL.Query().Get("limit") != "1" {
				t.Errorf("input query = %s", r.URL.RawQuery)
			}
			_ = json.NewEncoder(w).Encode(client.MemoryCurationRunInputPage{Run: run, Inputs: []client.MemoryCurationRunInput{{RunID: run.ID, Ordinal: 1, Kind: "memory", MemoryID: "mem_source", MemoryVersion: 1}}})
		case "POST /v1/memory-curation-runs/" + run.ID + "/renew":
			assertCurationKey(t, r, "renew-key")
			body := decodeCurationCLIRequest(t, r)
			if body["fencing_generation"] != float64(4) || body["extension_seconds"] != float64(120) {
				t.Errorf("renew body = %#v", body)
			}
			_ = json.NewEncoder(w).Encode(client.RenewMemoryCurationResult{Run: run})
		case "POST /v1/memory-curation-runs/" + run.ID + "/plan":
			assertCurationKey(t, r, "plan-key")
			body := decodeCurationCLIRequest(t, r)
			draft, ok := body["draft"].(map[string]any)
			if !ok || draft["schema"] != "witself.memory-plan.v1" {
				t.Errorf("plan body = %#v", body)
			}
			plannedRun := run
			plannedRun.State, plannedRun.PlanRevision, plannedRun.PlanHash = "planned", 1, planHash
			_ = json.NewEncoder(w).Encode(client.PlanMemoryCurationResult{Run: plannedRun, Plan: json.RawMessage(`{"schema":"witself.memory-plan.v1","plan_revision":1,"actions":[]}`), Preview: client.MemoryCurationImpactPreview{}, Receipt: client.MemoryCurationPlanReceipt{PlanRevision: 1, PlanHash: planHash}})
		case "POST /v1/memory-curation-runs/" + run.ID + "/apply":
			assertCurationKey(t, r, "apply-key")
			body := decodeCurationCLIRequest(t, r)
			if body["plan_revision"] != float64(1) || body["plan_hash"] != planHash {
				t.Errorf("apply body = %#v", body)
			}
			appliedRun := run
			appliedRun.State, appliedRun.ApplyReceiptID = "applied", "mrec_apply"
			_ = json.NewEncoder(w).Encode(client.ApplyMemoryCurationResult{Run: appliedRun, Request: request, Receipt: client.MemoryCurationApplyReceipt{ID: "mrec_apply", ActionResults: []client.MemoryCurationActionApplyResult{}, CursorIntervals: []client.MemoryCurationCursorInterval{}}})
		case "POST /v1/memory-curation-runs/" + run.ID + "/cancel":
			assertCurationKey(t, r, "cancel-key")
			finished := run
			finished.State = "abandoned"
			_ = json.NewEncoder(w).Encode(client.FinishMemoryCurationResult{Run: finished})
		case "POST /v1/memory-curation-runs/" + run.ID + "/abandon":
			assertCurationKey(t, r, "abandon-key")
			finished := run
			finished.State = "abandoned"
			_ = json.NewEncoder(w).Encode(client.FinishMemoryCurationResult{Run: finished})
		case "POST /v1/memory-curation-runs/" + run.ID + "/rollback":
			assertCurationKey(t, r, "rollback-key")
			body := decodeCurationCLIRequest(t, r)
			heads, ok := body["expected_produced_heads"].([]any)
			if body["apply_receipt_id"] != "mrec_apply" || !ok || len(heads) != 1 {
				t.Errorf("rollback body = %#v", body)
			}
			rolled := run
			rolled.State = "rolled_back"
			_ = json.NewEncoder(w).Encode(client.RollbackMemoryCurationResult{Run: rolled, Receipt: client.MemoryCurationRollbackReceipt{ID: "mrec_rollback", ReplayRequestID: "mcrq_replay", ReplayGeneration: 8}})
		case "GET /v1/memory-curation-status":
			if r.URL.Query().Get("run_id") != run.ID {
				t.Errorf("status query = %s", r.URL.RawQuery)
			}
			_ = json.NewEncoder(w).Encode(client.MemoryCurationStatus{Lane: client.MemoryCurationLane{RequestGeneration: 8, FencingGeneration: 4}, Request: &request, Run: &run})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	conn := []string{"--endpoint", srv.URL, "--token-file", tokenFile, "--json"}
	calls := [][]string{
		append([]string{"request", "--idempotency-key", "request-key"}, conn...),
		append([]string{"requests", "--state", "queued", "--limit", "10"}, conn...),
		append([]string{"start", "--request", request.ID, "--lease-seconds", "90", "--idempotency-key", "start-key"}, conn...),
		append([]string{"show", run.ID, "--fence", "4", "--limit", "1"}, conn...),
		append([]string{"renew", run.ID, "--fence", "4", "--extension-seconds", "120", "--idempotency-key", "renew-key"}, conn...),
		append([]string{"plan", run.ID, "--fence", "4", "--file", planFile, "--idempotency-key", "plan-key"}, conn...),
		append([]string{"apply", run.ID, "--fence", "4", "--plan-revision", "1", "--plan-hash", planHash, "--idempotency-key", "apply-key", "--yes"}, conn...),
		append([]string{"cancel", run.ID, "--fence", "4", "--idempotency-key", "cancel-key"}, conn...),
		append([]string{"abandon", run.ID, "--fence", "4", "--idempotency-key", "abandon-key"}, conn...),
		append([]string{"rollback", run.ID, "--apply-receipt", "mrec_apply", "--expected-heads", headsFile, "--idempotency-key", "rollback-key", "--yes"}, conn...),
		append([]string{"status", run.ID}, conn...),
	}
	for _, args := range calls {
		if code := memoryCurate(args); code != 0 {
			t.Fatalf("memory curate %v = %d", args, code)
		}
	}
	for _, key := range []string{
		"POST /v1/memory-curation-requests",
		"GET /v1/memory-curation-requests",
		"POST /v1/memory-curation-requests/" + request.ID + "/start",
		"GET /v1/memory-curation-runs/" + run.ID + "/inputs",
		"POST /v1/memory-curation-runs/" + run.ID + "/renew",
		"POST /v1/memory-curation-runs/" + run.ID + "/plan",
		"POST /v1/memory-curation-runs/" + run.ID + "/apply",
		"POST /v1/memory-curation-runs/" + run.ID + "/cancel",
		"POST /v1/memory-curation-runs/" + run.ID + "/abandon",
		"POST /v1/memory-curation-runs/" + run.ID + "/rollback",
		"GET /v1/memory-curation-status",
	} {
		if seen[key] != 1 {
			t.Errorf("%s calls = %d", key, seen[key])
		}
	}
}

func TestMemoryCurationApplyAndRollbackRequireConfirmation(t *testing.T) {
	hash := strings.Repeat("a", 64)
	if code := memoryCurate([]string{"apply", "mrun_1", "--fence", "1", "--plan-revision", "1", "--plan-hash", hash, "--idempotency-key", "key"}); code != 2 {
		t.Fatalf("unguarded apply = %d", code)
	}
	if code := memoryCurate([]string{"rollback", "mrun_1", "--apply-receipt", "mrec_1", "--expected-heads", "heads.json", "--idempotency-key", "key"}); code != 2 {
		t.Fatalf("unguarded rollback = %d", code)
	}
}

func assertCurationKey(t *testing.T, r *http.Request, want string) {
	t.Helper()
	if got := r.Header.Get("Idempotency-Key"); got != want {
		t.Errorf("idempotency key = %q, want %q", got, want)
	}
}

func decodeCurationCLIRequest(t *testing.T, r *http.Request) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	return body
}

func ptrTime(value time.Time) *time.Time { return &value }
