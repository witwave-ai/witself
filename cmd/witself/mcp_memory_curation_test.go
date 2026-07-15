package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/witwave-ai/witself/internal/client"
	"github.com/witwave-ai/witself/internal/transcriptcapture"
)

type fakeCurationMCPBackend struct {
	fakeMCPBackend
	preflight client.MemoryCurationPreflight
	list      client.MemoryCurationRequestListOptions
	requestID string
	request   client.RequestMemoryCurationInput
	start     client.StartMemoryCurationInput
	runID     string
	getRunID  string
	fence     int64
	cursor    string
	limit     int
	renew     client.RenewMemoryCurationInput
	plan      client.PlanMemoryCurationInput
	apply     client.ApplyMemoryCurationInput
	cancel    client.FinishMemoryCurationInput
	abandon   client.FinishMemoryCurationInput
	rollback  client.RollbackMemoryCurationInput
	statusID  string
}

func (b *fakeCurationMCPBackend) GetMemoryCurationPreflight(context.Context) (client.MemoryCurationPreflight, error) {
	if b.preflight.Credential.AccessProfile == "" {
		b.preflight = client.MemoryCurationPreflight{
			Principal:  client.MemoryCurationPreflightPrincipal{AgentID: "agent_1"},
			Credential: client.MemoryCurationPreflightCredential{AccessProfile: mcpProfileFull},
			Protocol: client.MemoryCurationPreflightProtocol{
				PlanSchema: "witself.memory-plan.v1", AllowedPrimitives: []string{"create", "replace", "supersede", "relate", "propose_fact"},
				ClientInferenceRequired: true,
			},
		}
	}
	return b.preflight, nil
}

func (b *fakeCurationMCPBackend) ListMemoryCurationRequests(_ context.Context, opts client.MemoryCurationRequestListOptions) (client.MemoryCurationRequestPage, error) {
	b.list = opts
	return client.MemoryCurationRequestPage{Requests: []client.MemoryCurationRequest{{ID: "mcrq_1"}}}, nil
}

func (b *fakeCurationMCPBackend) GetMemoryCurationRequest(_ context.Context, id string) (client.MemoryCurationRequest, error) {
	b.requestID = id
	return client.MemoryCurationRequest{ID: id}, nil
}

func (b *fakeCurationMCPBackend) RequestMemoryCuration(_ context.Context, in client.RequestMemoryCurationInput) (client.RequestMemoryCurationResult, error) {
	b.request = in
	return client.RequestMemoryCurationResult{Request: client.MemoryCurationRequest{ID: "mcrq_1"}}, nil
}

func (b *fakeCurationMCPBackend) StartMemoryCuration(_ context.Context, in client.StartMemoryCurationInput) (client.StartMemoryCurationResult, error) {
	b.start = in
	return client.StartMemoryCurationResult{Run: client.MemoryCurationRun{ID: "mrun_1"}}, nil
}

func (b *fakeCurationMCPBackend) GetMemoryCurationRun(_ context.Context, id string) (client.MemoryCurationRun, error) {
	b.getRunID = id
	return client.MemoryCurationRun{ID: id}, nil
}

func (b *fakeCurationMCPBackend) GetMemoryCurationRunInputs(_ context.Context, runID string, fence int64, cursor string, limit int) (client.MemoryCurationRunInputPage, error) {
	b.runID, b.fence, b.cursor, b.limit = runID, fence, cursor, limit
	return client.MemoryCurationRunInputPage{Inputs: []client.MemoryCurationRunInput{}}, nil
}

func (b *fakeCurationMCPBackend) RenewMemoryCuration(_ context.Context, in client.RenewMemoryCurationInput) (client.RenewMemoryCurationResult, error) {
	b.renew = in
	return client.RenewMemoryCurationResult{}, nil
}

func (b *fakeCurationMCPBackend) PlanMemoryCuration(_ context.Context, in client.PlanMemoryCurationInput) (client.PlanMemoryCurationResult, error) {
	b.plan = in
	return client.PlanMemoryCurationResult{Plan: json.RawMessage(`{"schema":"witself.memory-plan.v1","plan_revision":1,"actions":[]}`)}, nil
}

func (b *fakeCurationMCPBackend) ApplyMemoryCuration(_ context.Context, in client.ApplyMemoryCurationInput) (client.ApplyMemoryCurationResult, error) {
	b.apply = in
	return client.ApplyMemoryCurationResult{}, nil
}

func (b *fakeCurationMCPBackend) CancelMemoryCuration(_ context.Context, in client.FinishMemoryCurationInput) (client.FinishMemoryCurationResult, error) {
	b.cancel = in
	return client.FinishMemoryCurationResult{}, nil
}

func (b *fakeCurationMCPBackend) AbandonMemoryCuration(_ context.Context, in client.FinishMemoryCurationInput) (client.FinishMemoryCurationResult, error) {
	b.abandon = in
	return client.FinishMemoryCurationResult{}, nil
}

func (b *fakeCurationMCPBackend) RollbackMemoryCuration(_ context.Context, in client.RollbackMemoryCurationInput) (client.RollbackMemoryCurationResult, error) {
	b.rollback = in
	return client.RollbackMemoryCurationResult{}, nil
}

func (b *fakeCurationMCPBackend) GetMemoryCurationStatus(_ context.Context, runID string) (client.MemoryCurationStatus, error) {
	b.statusID = runID
	return client.MemoryCurationStatus{}, nil
}

func TestMCPMemoryCurationWorkflowMapsProviderNeutralInputs(t *testing.T) {
	ctx := context.Background()
	backend := &fakeCurationMCPBackend{}
	server := newWitselfMCPServer(backend)
	clientTransport, serverTransport := mcp.NewInMemoryTransports()
	serverSession, err := server.Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = serverSession.Close() }()
	mcpClient := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	clientSession, err := mcpClient.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = clientSession.Close() }()

	callCurationTool(ctx, t, clientSession, "witself.memory.curation.preflight", map[string]any{})
	callCurationTool(ctx, t, clientSession, "witself.memory.curation.requests", map[string]any{
		"state": "queued", "limit": 12, "cursor": "request-next", "exclude_sensitive": true,
	})
	callCurationTool(ctx, t, clientSession, "witself.memory.curation.request.get", map[string]any{"request_id": "mcrq_1"})
	callCurationTool(ctx, t, clientSession, "witself.memory.curation.request", map[string]any{
		"sources": []string{"memory", "transcript"}, "coalescing_key": "manual",
		"trigger_reason": "manual_refine", "due_at": "2026-07-14T15:00:00Z",
		"idempotency_key": "request-key",
	})
	callCurationTool(ctx, t, clientSession, "witself.memory.curation.start", map[string]any{
		"request_id": "mcrq_1", "lease_seconds": 90,
		"client":  map[string]any{"runtime": "codex", "model": "gpt-5"},
		"budgets": map[string]any{"tokens": 2000}, "idempotency_key": "start-key",
	})
	callCurationTool(ctx, t, clientSession, "witself.memory.curation.run.get", map[string]any{"run_id": "mrun_1"})
	callCurationTool(ctx, t, clientSession, "witself.memory.curation.renew", map[string]any{
		"run_id": "mrun_1", "fencing_generation": 4, "extension_seconds": 120,
		"idempotency_key": "renew-key",
	})
	callCurationTool(ctx, t, clientSession, "witself.memory.curation.get", map[string]any{
		"run_id": "mrun_1", "fencing_generation": 4, "cursor": "next", "limit": 12,
	})
	callCurationTool(ctx, t, clientSession, "witself.memory.curation.plan", map[string]any{
		"run_id": "mrun_1", "fencing_generation": 4,
		"draft":           map[string]any{"schema": "witself.memory-plan.v1", "draft_revision": 1, "actions": []any{}},
		"idempotency_key": "plan-key",
	})
	hash := strings.Repeat("a", 64)
	callCurationTool(ctx, t, clientSession, "witself.memory.curation.apply", map[string]any{
		"run_id": "mrun_1", "fencing_generation": 4, "plan_revision": 1,
		"plan_hash": hash, "idempotency_key": "apply-key",
	})
	callCurationTool(ctx, t, clientSession, "witself.memory.curation.cancel", map[string]any{
		"run_id": "mrun_1", "fencing_generation": 4, "reason": "operator_cancel",
		"idempotency_key": "cancel-key",
	})
	callCurationTool(ctx, t, clientSession, "witself.memory.curation.abandon", map[string]any{
		"run_id": "mrun_1", "fencing_generation": 4, "reason": "preview_complete",
		"idempotency_key": "abandon-key",
	})
	callCurationTool(ctx, t, clientSession, "witself.memory.curation.rollback", map[string]any{
		"run_id": "mrun_1", "apply_receipt_id": "mrec_1",
		"expected_produced_heads": []map[string]any{{"memory_id": "mem_1", "version": 2}},
		"reason":                  "bad synthesis", "idempotency_key": "rollback-key",
	})
	callCurationTool(ctx, t, clientSession, "witself.memory.curation.status", map[string]any{"run_id": "mrun_1"})

	if backend.preflight.Credential.AccessProfile != mcpProfileFull ||
		backend.list.State != "queued" || backend.list.Limit != 12 || backend.list.Cursor != "request-next" ||
		!backend.list.ExcludeSensitive || backend.requestID != "mcrq_1" {
		t.Fatalf("preflight/list/get mapping = %#v %#v %q", backend.preflight, backend.list, backend.requestID)
	}
	if backend.request.IdempotencyKey != "request-key" || len(backend.request.Scope.Sources) != 2 || backend.request.DueAt == nil {
		t.Fatalf("request mapping = %#v", backend.request)
	}
	if backend.start.RequestID != "mcrq_1" || backend.start.LeaseDuration != 90*time.Second || backend.start.Client.Runtime != "codex" || string(backend.start.Budgets) != `{"tokens":2000}` {
		t.Fatalf("start mapping = %#v", backend.start)
	}
	if backend.getRunID != "mrun_1" {
		t.Fatalf("run get id = %q", backend.getRunID)
	}
	if backend.renew.Extension != 120*time.Second || backend.renew.FencingGeneration != 4 {
		t.Fatalf("renew mapping = %#v", backend.renew)
	}
	if backend.runID != "mrun_1" || backend.fence != 4 || backend.cursor != "next" || backend.limit != 12 {
		t.Fatalf("get mapping = %q %d %q %d", backend.runID, backend.fence, backend.cursor, backend.limit)
	}
	if !strings.Contains(string(backend.plan.Draft), `"draft_revision":1`) || backend.plan.IdempotencyKey != "plan-key" {
		t.Fatalf("plan mapping = %#v", backend.plan)
	}
	if backend.apply.PlanHash != hash || backend.apply.PlanRevision != 1 || backend.apply.IdempotencyKey != "apply-key" {
		t.Fatalf("apply mapping = %#v", backend.apply)
	}
	if backend.cancel.Reason != "operator_cancel" || backend.cancel.IdempotencyKey != "cancel-key" {
		t.Fatalf("cancel mapping = %#v", backend.cancel)
	}
	if backend.abandon.Reason != "preview_complete" || backend.abandon.IdempotencyKey != "abandon-key" {
		t.Fatalf("abandon mapping = %#v", backend.abandon)
	}
	if backend.rollback.ApplyReceiptID != "mrec_1" || len(backend.rollback.ExpectedProducedHeads) != 1 || backend.rollback.ExpectedProducedHeads[0].Version != 2 {
		t.Fatalf("rollback mapping = %#v", backend.rollback)
	}
	if backend.statusID != "mrun_1" {
		t.Fatalf("status run id = %q", backend.statusID)
	}
}

func TestMCPCuratorProfilesAdvertiseOnlyEffectiveWorkflow(t *testing.T) {
	base := []string{
		"witself.memory.curation.preflight",
		"witself.memory.curation.requests",
		"witself.memory.curation.request.get",
		"witself.memory.curation.start",
		"witself.memory.curation.run.get",
		"witself.memory.curation.get",
		"witself.memory.curation.renew",
		"witself.memory.curation.plan",
		"witself.memory.curation.abandon",
		"witself.memory.curation.status",
	}
	tests := []struct {
		profile      string
		instructions string
		apply        bool
	}{
		{profile: mcpProfileCuratorPreview, instructions: "This Witself MCP server is restricted to non-sensitive narrative-memory curation preview."},
		{profile: mcpProfileCuratorApply, instructions: "This Witself MCP server is restricted to non-sensitive reversible narrative-memory curation.", apply: true},
	}
	for _, tc := range tests {
		t.Run(tc.profile, func(t *testing.T) {
			ctx := context.Background()
			server := newWitselfMCPServerForRuntimeOptions(
				&fakeCurationMCPBackend{}, transcriptcapture.RuntimeCodex,
				mcpServerOptions{Profile: tc.profile},
			)
			clientTransport, serverTransport := mcp.NewInMemoryTransports()
			serverSession, err := server.Connect(ctx, serverTransport, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = serverSession.Close() }()
			mcpClient := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil)
			clientSession, err := mcpClient.Connect(ctx, clientTransport, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = clientSession.Close() }()

			page, err := clientSession.ListTools(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			want := append([]string(nil), base...)
			if tc.apply {
				want = append(want, "witself.memory.curation.apply")
			}
			got := make(map[string]bool, len(page.Tools))
			for _, tool := range page.Tools {
				got[tool.Name] = true
			}
			if len(page.Tools) != len(want) {
				t.Fatalf("%s tools = %d, want %d: %#v", tc.profile, len(page.Tools), len(want), page.Tools)
			}
			for _, name := range want {
				if !got[name] {
					t.Errorf("%s omitted %q", tc.profile, name)
				}
			}
			for _, forbidden := range []string{
				"witself.self.show", "witself.fact.set", "witself.memory.capture",
				"witself.memory.curation.request", "witself.memory.curation.cancel",
				"witself.memory.curation.rollback",
			} {
				if got[forbidden] {
					t.Errorf("%s advertised forbidden tool %q", tc.profile, forbidden)
				}
			}
			if !tc.apply && got["witself.memory.curation.apply"] {
				t.Fatal("preview profile advertised apply")
			}
			if instructions := clientSession.InitializeResult().Instructions; !strings.HasPrefix(instructions, tc.instructions) {
				t.Fatalf("%s instructions = %q", tc.profile, instructions)
			}
		})
	}
}

func callCurationTool(ctx context.Context, t *testing.T, session *mcp.ClientSession, name string, args map[string]any) {
	t.Helper()
	result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	if result.IsError {
		t.Fatalf("%s returned tool error: %#v", name, result.Content)
	}
}
