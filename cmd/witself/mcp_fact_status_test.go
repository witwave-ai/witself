package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/witwave-ai/witself/internal/client"
)

type fakeFactStatusMCPBackend struct {
	*fakeMCPBackend
	status      client.FactLimitStatus
	statusCalls int
}

func (b *fakeFactStatusMCPBackend) FactLimitStatus(context.Context) (client.FactLimitStatus, error) {
	b.statusCalls++
	return b.status, nil
}

func TestMCPFactStatusReturnsValueFreeCapacity(t *testing.T) {
	maximum, remaining := int64(1000), int64(100)
	backend := &fakeFactStatusMCPBackend{
		fakeMCPBackend: &fakeMCPBackend{},
		status: client.FactLimitStatus{
			Used: 900, Max: &maximum, Remaining: &remaining, NearLimit: true,
		},
	}
	ctx := context.Background()
	server := newWitselfMCPServer(backend)
	clientTransport, serverTransport := mcp.NewInMemoryTransports()
	serverSession, err := server.Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = serverSession.Close() }()
	clientSession, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil).Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = clientSession.Close() }()

	listed, err := clientSession.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	var description string
	for _, tool := range listed.Tools {
		if tool.Name == "witself.fact.status" {
			description = tool.Description
			break
		}
	}
	for _, want := range []string{"value-free", "current-fact capacity", "existing current fact", "create or recreate"} {
		if !strings.Contains(description, want) {
			t.Errorf("fact status description lacks %q: %q", want, description)
		}
	}

	result, err := clientSession.CallTool(ctx, &mcp.CallToolParams{
		Name: "witself.fact.status", Arguments: map[string]any{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError || backend.statusCalls != 1 {
		t.Fatalf("status result/calls = %#v / %d", result, backend.statusCalls)
	}
	raw, err := json.Marshal(result.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	var output mcpFactStatusOutput
	if err := json.Unmarshal(raw, &output); err != nil {
		t.Fatal(err)
	}
	status := output.FactCapacity
	if status.Used != 900 ||
		status.Max == nil || *status.Max != 1000 ||
		status.Remaining == nil || *status.Remaining != 100 ||
		!status.NearLimit || status.AtLimit || status.OverLimit ||
		status.Unlimited || status.Unavailable {
		t.Fatalf("fact status output = %#v", output)
	}
}

func TestMCPInstructionsCoverFactCapacityPolicy(t *testing.T) {
	instructions := mcpInstructions("", "witself.self.show", "witself.message.list")
	for _, want := range []string{
		"authenticated, value-free fact_capacity",
		"Near limit begins at 90 percent",
		"stored_fact_limit_reached",
		"updates to an already-current fact remain available",
		"never deletion authority",
		"do not infer unlimited capacity",
	} {
		if !strings.Contains(instructions, want) {
			t.Errorf("MCP fact-capacity routing lacks %q", want)
		}
	}
}

func TestRuntimeNeutralInstructionsCoverFactCapacityPolicy(t *testing.T) {
	for _, want := range []string{
		"authenticated value-free fact_capacity projection from self.show",
		"equivalent fact-status read",
		"Near limit begins at 90 percent",
		"stored_fact_limit_reached",
		"updates to an already-current fact remain available",
		"never deletion authority",
		"do not infer unlimited capacity",
	} {
		if !strings.Contains(runtimeNeutralMemoryRoutingInstructions, want) {
			t.Errorf("runtime-neutral fact-capacity routing lacks %q", want)
		}
	}
	if strings.Contains(runtimeNeutralMemoryRoutingInstructions, "witself.fact.status") {
		t.Fatal("runtime-neutral fact-capacity routing contains a runtime-specific dotted tool name")
	}
}
