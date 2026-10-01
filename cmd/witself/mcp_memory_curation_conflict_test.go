package main

import (
	"context"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/witwave-ai/witself/internal/client"
)

// conflictReasonCurationMCPBackend refuses every plan the way the HTTP client
// reports a 409 that names its rule.
type conflictReasonCurationMCPBackend struct {
	*fakeCurationMCPBackend
}

func (b *conflictReasonCurationMCPBackend) PlanMemoryCuration(context.Context, client.PlanMemoryCurationInput) (client.PlanMemoryCurationResult, error) {
	index := 1
	return client.PlanMemoryCurationResult{}, &client.MemoryCurationConflictError{
		Reason: "transcript_range_not_covered", ActionOrdinal: 2, EvidenceIndex: &index,
	}
}

// TestMCPMemoryCurationPlanConflictNamesReason pins that the plan tool's error
// text carries the conflict reason and plan positions (issue #608).
func TestMCPMemoryCurationPlanConflictNamesReason(t *testing.T) {
	ctx := context.Background()
	server := newWitselfMCPServer(&conflictReasonCurationMCPBackend{fakeCurationMCPBackend: &fakeCurationMCPBackend{}})
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

	result, err := clientSession.CallTool(ctx, &mcp.CallToolParams{
		Name: "witself.memory.curation.plan",
		Arguments: map[string]any{
			"run_id": "mrun_1", "fencing_generation": 4, "idempotency_key": "plan-key",
			"draft": map[string]any{"schema": "witself.memory-plan.v1", "draft_revision": 1, "actions": []any{}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.IsError || len(result.Content) != 1 {
		t.Fatalf("plan result: is_error %t with %d content blocks, want one tool error", result.IsError, len(result.Content))
	}
	want := "memory curation state conflict (reason=transcript_range_not_covered, action_ordinal=2, evidence_index=1)"
	if text, ok := result.Content[0].(*mcp.TextContent); !ok || text.Text != want {
		t.Fatalf("plan tool error = %#v, want %q", result.Content[0], want)
	}
}
