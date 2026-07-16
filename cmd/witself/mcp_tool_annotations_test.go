package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestMCPFactAndTranscriptToolsAdvertiseAccurateAnnotations(t *testing.T) {
	ctx := context.Background()
	server := newWitselfMCPServer(&fakeMCPBackend{})
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

	page, err := clientSession.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Tools) != 69 {
		t.Fatalf("full-profile tools = %d, want 69", len(page.Tools))
	}
	tools := make(map[string]*mcp.Tool, len(page.Tools))
	for _, tool := range page.Tools {
		if tools[tool.Name] != nil {
			t.Fatalf("full-profile MCP advertised duplicate tool %q", tool.Name)
		}
		tools[tool.Name] = tool
		if tool.Annotations == nil {
			t.Errorf("%s omitted annotations", tool.Name)
			continue
		}
		if tool.Annotations.DestructiveHint == nil {
			t.Errorf("%s omitted destructiveHint", tool.Name)
		}
		if tool.Annotations.OpenWorldHint == nil || *tool.Annotations.OpenWorldHint {
			t.Errorf("%s openWorldHint = %v, want explicit false", tool.Name, tool.Annotations.OpenWorldHint)
		}
	}

	checked := make(map[string]bool)
	for _, name := range []string{
		"witself.self.show",
		"witself.agent.peers",
		"witself.fact.review",
		"witself.fact.candidate.get",
		"witself.fact.get",
		"witself.fact.list",
		"witself.fact.upcoming",
		"witself.fact.subject.list",
		"witself.transcript.list",
		"witself.transcript.get",
		"witself.transcript.tail",
		"witself.message.list",
		"witself.message.listen",
		"witself.message.notification.list",
	} {
		checked[name] = true
		assertMCPToolAnnotations(t, tools[name], name, true, false, true)
	}

	writes := []struct {
		name        string
		destructive bool
		idempotent  bool
	}{
		{name: "witself.fact.set", destructive: true, idempotent: true},
		{name: "witself.fact.delete", destructive: true, idempotent: true},
		{name: "witself.fact.propose", idempotent: true},
		{name: "witself.fact.propose_from_transcript", idempotent: true},
		{name: "witself.fact.confirm", destructive: true, idempotent: true},
		{name: "witself.fact.reject", destructive: true, idempotent: true},
		{name: "witself.fact.subject.set", destructive: true},
		{name: "witself.fact.subject.alias", destructive: true, idempotent: true},
		{name: "witself.message.send", destructive: true, idempotent: true},
		{name: "witself.message.reply", destructive: true, idempotent: true},
		{name: "witself.message.notification.consume", destructive: true, idempotent: true},
		{name: "witself.message.read", destructive: true, idempotent: true},
		{name: "witself.message.ack", destructive: true, idempotent: true},
		{name: "witself.message.claim", destructive: true},
		{name: "witself.message.renew", destructive: true},
		{name: "witself.message.release", destructive: true, idempotent: true},
		{name: "witself.message.complete", destructive: true, idempotent: true},
		{name: "witself.message.request.open", destructive: true, idempotent: true},
		{name: "witself.message.request.list", destructive: true},
		{name: "witself.message.request.show", destructive: true},
		{name: "witself.message.request.offer", destructive: true, idempotent: true},
		{name: "witself.message.request.decline", destructive: true, idempotent: true},
		{name: "witself.message.request.select", destructive: true, idempotent: true},
		{name: "witself.message.request.cancel", destructive: true, idempotent: true},
		{name: "witself.message.request.claim", destructive: true, idempotent: true},
		{name: "witself.message.request.renew", destructive: true},
		{name: "witself.message.request.release", destructive: true, idempotent: true},
		{name: "witself.message.request.complete", destructive: true, idempotent: true},
	}
	for _, write := range writes {
		checked[write.name] = true
		assertMCPToolAnnotations(t, tools[write.name], write.name, false, write.destructive, write.idempotent)
	}
	for _, tool := range page.Tools {
		if strings.HasPrefix(tool.Name, "witself.memory.") {
			continue
		}
		if !checked[tool.Name] {
			t.Errorf("full-profile non-memory tool %q lacks an exact annotation expectation", tool.Name)
		}
	}
	for _, name := range []string{"witself.fact.confirm", "witself.fact.reject"} {
		raw, err := json.Marshal(tools[name].InputSchema)
		if err != nil {
			t.Fatalf("marshal %s input schema: %v", name, err)
		}
		var schema struct {
			Required []string `json:"required"`
		}
		if err := json.Unmarshal(raw, &schema); err != nil {
			t.Fatalf("decode %s input schema: %v", name, err)
		}
		required := make(map[string]bool, len(schema.Required))
		for _, field := range schema.Required {
			required[field] = true
		}
		for _, field := range []string{"candidate_id", "idempotency_key"} {
			if !required[field] {
				t.Errorf("%s input schema does not require %s: %s", name, field, raw)
			}
		}
	}
}

func assertMCPToolAnnotations(t *testing.T, tool *mcp.Tool, name string, readOnly, destructive, idempotent bool) {
	t.Helper()
	if tool == nil {
		t.Fatalf("MCP omitted %q", name)
	}
	annotations := tool.Annotations
	if annotations == nil {
		t.Fatalf("%s omitted annotations", name)
	}
	if annotations.ReadOnlyHint != readOnly {
		t.Errorf("%s readOnlyHint = %t, want %t", name, annotations.ReadOnlyHint, readOnly)
	}
	if annotations.OpenWorldHint == nil || *annotations.OpenWorldHint {
		t.Errorf("%s openWorldHint = %v, want false", name, annotations.OpenWorldHint)
	}
	if annotations.DestructiveHint == nil || *annotations.DestructiveHint != destructive {
		t.Errorf("%s destructiveHint = %v, want %t", name, annotations.DestructiveHint, destructive)
	}
	if annotations.IdempotentHint != idempotent {
		t.Errorf("%s idempotentHint = %t, want %t", name, annotations.IdempotentHint, idempotent)
	}
}
