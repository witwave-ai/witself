package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestMCPMemoryCurationGetMaxBytes pins the curation.get tool's max_bytes
// parameter (issue #649): the schema advertises it as optional and the
// description says when to pass it; a value in range reaches the backend
// exactly; its absence reaches the backend as zero; and a value outside
// 8192-65536 is a tool error with the exact message before any backend call.
func TestMCPMemoryCurationGetMaxBytes(t *testing.T) {
	ctx := context.Background()
	backend := &fakeCurationMCPBackend{}
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
	var getTool *mcp.Tool
	for _, tool := range listed.Tools {
		if tool.Name == "witself.memory.curation.get" {
			getTool = tool
		}
	}
	if getTool == nil {
		t.Fatal("MCP omitted witself.memory.curation.get")
	}
	raw, err := json.Marshal(getTool.InputSchema)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"max_bytes"`) {
		t.Fatalf("curation.get schema omitted max_bytes: %s", raw)
	}
	var schema struct {
		Required []string `json:"required"`
	}
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatal(err)
	}
	for _, required := range schema.Required {
		if required == "max_bytes" {
			t.Fatal("curation.get max_bytes unexpectedly required")
		}
	}
	if !strings.Contains(getTool.Description, "max_bytes") {
		t.Fatalf("curation.get description omitted max_bytes: %q", getTool.Description)
	}

	callCurationTool(ctx, t, clientSession, "witself.memory.curation.get", map[string]any{
		"run_id": "mrun_1", "fencing_generation": 4, "max_bytes": 24576,
	})
	if backend.maxBytes != 24576 || backend.runID != "mrun_1" || backend.fence != 4 || backend.limit != 50 {
		t.Fatalf("get mapping = max_bytes %d run %q fence %d limit %d, want 24576 mrun_1 4 50",
			backend.maxBytes, backend.runID, backend.fence, backend.limit)
	}
	callCurationTool(ctx, t, clientSession, "witself.memory.curation.get", map[string]any{
		"run_id": "mrun_1", "fencing_generation": 4,
	})
	if backend.maxBytes != 0 {
		t.Fatalf("get without max_bytes reached the backend as %d, want 0", backend.maxBytes)
	}

	const wantError = "max_bytes must be between 8192 and 65536"
	for _, maxBytes := range []int{8191, 65537, -1} {
		backend.maxBytes = -2
		result, err := clientSession.CallTool(ctx, &mcp.CallToolParams{
			Name: "witself.memory.curation.get",
			Arguments: map[string]any{
				"run_id": "mrun_1", "fencing_generation": 4, "max_bytes": maxBytes,
			},
		})
		if err != nil {
			t.Fatalf("max_bytes %d: %v", maxBytes, err)
		}
		if !result.IsError || len(result.Content) != 1 {
			t.Fatalf("max_bytes %d: is_error %t with %d content blocks, want one tool error",
				maxBytes, result.IsError, len(result.Content))
		}
		if text, ok := result.Content[0].(*mcp.TextContent); !ok || text.Text != wantError {
			t.Fatalf("max_bytes %d: tool error = %#v, want %q", maxBytes, result.Content[0], wantError)
		}
		if backend.maxBytes != -2 {
			t.Fatalf("max_bytes %d reached the backend as %d, want no backend call", maxBytes, backend.maxBytes)
		}
	}
}
