package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

// TestMemoryCurationConflictReasonHTTPBody pins the 409 body of a conflict
// whose rule is known, on the plan and plan-read routes, and that an
// unclassified conflict keeps today's body (issue #608).
func TestMemoryCurationConflictReasonHTTPBody(t *testing.T) {
	index := 1
	cfg := Config{
		AuthenticatePrincipal: func(context.Context, string) (DomainPrincipal, bool, error) {
			return DomainPrincipal{
				Kind: PrincipalKindAgent, ID: "agent_1", AccountID: "acc_1",
				RealmID: "realm_1", AccountStatus: "active",
			}, true, nil
		},
		PlanMemoryCuration: func(_ context.Context, _ DomainPrincipal, runID string, _ PlanMemoryCurationRequest) (any, error) {
			if runID == "mrun_bare" {
				return nil, ErrConflict
			}
			return nil, &MemoryCurationConflictError{
				Reason: "transcript_range_not_covered", ActionOrdinal: 2, EvidenceIndex: &index,
			}
		},
		GetMemoryCurationPlan: func(context.Context, DomainPrincipal, string, int64) (any, error) {
			return nil, &MemoryCurationConflictError{Reason: "run_not_planned"}
		},
	}
	srv := httptest.NewServer(apiMux(cfg))
	defer srv.Close()

	planBody := `{"fencing_generation":1,"draft":{"schema":"witself.memory-plan.v1","draft_revision":1,"actions":[]}}`
	tests := []struct {
		name, method, path, body, key string
		want                          map[string]any
	}{
		{
			name: "plan rule with positions", method: http.MethodPost,
			path: "/v1/memory-curation-runs/mrun_1/plan", body: planBody, key: "plan-key",
			want: map[string]any{
				"schema_version": "witself.v0", "code": "memory_curation_conflict",
				"error": "memory curation state conflict", "retryable": false,
				"reason": "transcript_range_not_covered", "action_ordinal": float64(2), "evidence_index": float64(1),
			},
		},
		{
			name: "plan read rule without positions", method: http.MethodGet,
			path: "/v1/memory-curation-runs/mrun_1/plan?fencing_generation=1",
			want: map[string]any{
				"schema_version": "witself.v0", "code": "memory_curation_conflict",
				"error": "memory curation state conflict", "retryable": false, "reason": "run_not_planned",
			},
		},
		{
			name: "unclassified conflict keeps its body", method: http.MethodPost,
			path: "/v1/memory-curation-runs/mrun_bare/plan", body: planBody, key: "plan-key",
			want: map[string]any{"schema_version": "witself.v0", "error": "memory curation state conflict"},
		},
	}
	for _, test := range tests {
		response := memoryCurationHTTPResponse(t, srv.URL, "agent", test.method, test.path, test.body, test.key)
		var got map[string]any
		err := json.NewDecoder(response.Body).Decode(&got)
		_ = response.Body.Close()
		if err != nil {
			t.Fatalf("%s: decode body: %v", test.name, err)
		}
		if response.StatusCode != http.StatusConflict || !reflect.DeepEqual(got, test.want) {
			t.Fatalf("%s: status %d body %v, want 409 %v", test.name, response.StatusCode, got, test.want)
		}
	}
}
