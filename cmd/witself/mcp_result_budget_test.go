package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/witwave-ai/witself/internal/client"
	"github.com/witwave-ai/witself/internal/transcriptcapture"
)

func TestBoundMCPTranscriptEntriesKeepsSmallPagesUntouched(t *testing.T) {
	entries := []client.TranscriptEntry{{
		Sequence: 1, Body: "hello",
		Payload:   json.RawMessage(`{"kind":"message.user"}`),
		Artifacts: json.RawMessage(`[]`),
	}}
	boundMCPTranscriptEntries(entries)
	if entries[0].Body != "hello" ||
		string(entries[0].Payload) != `{"kind":"message.user"}` ||
		string(entries[0].Artifacts) != `[]` {
		t.Fatalf("small entry was rewritten: %+v", entries[0])
	}
}

func TestBoundMCPTranscriptEntriesElidesOversizedBody(t *testing.T) {
	big := strings.Repeat("b", maxMCPTranscriptPageBytes+50_000)
	entries := []client.TranscriptEntry{{Sequence: 1, Body: big}}
	boundMCPTranscriptEntries(entries)
	if len(entries[0].Body) > maxMCPTranscriptPageBytes+256 {
		t.Fatalf("bounded body is %d bytes", len(entries[0].Body))
	}
	if !strings.HasPrefix(entries[0].Body, "bbbb") ||
		!strings.Contains(entries[0].Body, "witself:elided omitted_bytes=") {
		t.Fatalf("bounded body lost its prefix or note: %.120q", entries[0].Body)
	}
}

func TestBoundMCPTranscriptEntriesSpendsSharedBudgetInOrder(t *testing.T) {
	first := strings.Repeat("a", maxMCPTranscriptPageBytes)
	entries := []client.TranscriptEntry{
		{Sequence: 1, Body: first},
		{
			Sequence: 2, Body: "late entry",
			Payload:   json.RawMessage(`{"data":"` + strings.Repeat("p", 200) + `"}`),
			Artifacts: json.RawMessage(`[{"data":"` + strings.Repeat("x", 200) + `"}]`),
		},
	}
	boundMCPTranscriptEntries(entries)
	if entries[1].Sequence != 2 {
		t.Fatal("membership changed")
	}
	if !strings.Contains(entries[1].Body, "witself:elided") {
		t.Fatalf("late body kept content past the budget: %.80q", entries[1].Body)
	}
	if !strings.Contains(string(entries[1].Payload), `"witself_elided":true`) {
		t.Fatalf("late payload was not elided: %s", entries[1].Payload)
	}
	if !strings.Contains(string(entries[1].Artifacts), `"witself_elided":true`) {
		t.Fatalf("late artifacts were not elided: %s", entries[1].Artifacts)
	}
}

func TestBoundMCPTranscriptEntriesCutsUTF8Safely(t *testing.T) {
	big := strings.Repeat("héllo wörld ", (maxMCPTranscriptPageBytes/12)+1000)
	entries := []client.TranscriptEntry{{Sequence: 1, Body: big}}
	boundMCPTranscriptEntries(entries)
	if !utf8.ValidString(entries[0].Body) {
		t.Fatal("bounded body is not valid UTF-8")
	}
}

func TestMCPResultSizeGuardReplacesOversizedToolResults(t *testing.T) {
	ctx := context.Background()
	oversized := &mcp.CallToolResult{Content: []mcp.Content{
		&mcp.TextContent{Text: strings.Repeat("z", maxMCPToolResultBytes+1)},
	}}
	handler := mcpResultSizeGuard()(func(context.Context, string, mcp.Request) (mcp.Result, error) {
		return oversized, nil
	})

	res, err := handler(ctx, "tools/call", nil)
	if err != nil {
		t.Fatal(err)
	}
	guarded, ok := res.(*mcp.CallToolResult)
	if !ok || !guarded.IsError {
		t.Fatalf("oversized result was not replaced: %+v", res)
	}
	text, ok := guarded.Content[0].(*mcp.TextContent)
	if !ok || !strings.Contains(text.Text, "transport budget") {
		t.Fatalf("guard error text = %+v", guarded.Content)
	}

	// A non-tool method keeps its result even when large.
	passthrough, err := handler(ctx, "resources/read", nil)
	if err != nil {
		t.Fatal(err)
	}
	if passthrough != mcp.Result(oversized) {
		t.Fatalf("non-tool result was replaced: %+v", passthrough)
	}
}

func TestMCPResultSizeGuardPassesOrdinaryResults(t *testing.T) {
	ctx := context.Background()
	small := &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "ok"}}}
	handler := mcpResultSizeGuard()(func(context.Context, string, mcp.Request) (mcp.Result, error) {
		return small, nil
	})
	res, err := handler(ctx, "tools/call", nil)
	if err != nil {
		t.Fatal(err)
	}
	if res != mcp.Result(small) {
		t.Fatalf("small result was replaced: %+v", res)
	}
}

// TestWitselfMCPTranscriptGetStaysTransportSized proves the full tool path:
// a page whose stored entries are several megabytes comes back elided,
// membership-complete, and small enough for frame-limited MCP clients.
func TestWitselfMCPTranscriptGetStaysTransportSized(t *testing.T) {
	ctx := context.Background()
	fat := make([]client.TranscriptEntry, 6)
	for i := range fat {
		fat[i] = client.TranscriptEntry{
			ID: "ent_big", TranscriptID: "trn_1", Sequence: int64(i + 1), Role: "assistant",
			Body:      strings.Repeat("b", 200_000),
			Payload:   json.RawMessage(`{"data":"` + strings.Repeat("p", 300_000) + `"}`),
			Artifacts: json.RawMessage(`[]`),
		}
	}
	backend := &fakeMCPBackend{transcriptEntries: fat}
	server := newWitselfMCPServerForRuntime(backend, transcriptcapture.RuntimeClaudeCode)
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
		Name:      "witself.transcript.get",
		Arguments: map[string]any{"transcript_id": "trn_1", "limit": 10},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError {
		t.Fatalf("transcript.get errored: %+v", result.Content)
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if len(encoded) > 2*maxMCPTranscriptPageBytes+64*1024 {
		t.Fatalf("bounded page still serializes to %d bytes", len(encoded))
	}
	restructured, err := json.Marshal(result.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	var page mcpTranscriptReadOutput
	if err := json.Unmarshal(restructured, &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Entries) != len(fat) {
		t.Fatalf("entries = %d, want %d", len(page.Entries), len(fat))
	}
	if !strings.Contains(string(encoded), "witself:elided") {
		t.Fatal("bounded page carries no elision note")
	}
}
