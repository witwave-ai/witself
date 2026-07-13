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

type fakeMCPBackend struct {
	lastTranscriptID string
	lastOptions      client.TranscriptPageOptions
	lastMessageSend  client.SendMessageInput
	lastMessageList  client.MessageListOptions
	readMessageID    string
	ackedMessageID   string
	lastFactSet      client.SetFactInput
}

func TestGrokMCPUsesPortableToolNames(t *testing.T) {
	ctx := context.Background()
	server := newWitselfMCPServerForRuntime(&fakeMCPBackend{}, transcriptcapture.RuntimeGrokBuild)
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
	tools, err := clientSession.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(tools.Tools) != 10 {
		t.Fatalf("tools = %d, want 10", len(tools.Tools))
	}
	for _, tool := range tools.Tools {
		if strings.Contains(tool.Name, ".") || !strings.HasPrefix(tool.Name, "witself_") {
			t.Errorf("non-portable Grok tool name %q", tool.Name)
		}
	}
	instructions := clientSession.InitializeResult().Instructions
	if !strings.Contains(instructions, "witself_self_show") || strings.Contains(instructions, "witself.self.show") {
		t.Fatalf("instructions = %q", instructions)
	}
}

func (b *fakeMCPBackend) SendMessage(_ context.Context, in client.SendMessageInput) (client.Message, error) {
	b.lastMessageSend = in
	return client.Message{
		ID: "msg_1", ThreadID: "thr_1", Kind: "note", Body: in.Body,
		From:      client.MessageAgent{Kind: "agent", AgentID: "agent_1", AgentName: "scott"},
		To:        client.MessageRecipient{Kind: "agent", AgentID: "agent_2", AgentName: in.To},
		Delivery:  client.MessageDelivery{State: "delivered"},
		ReadState: client.MessageReadState{State: "unread"},
	}, nil
}

func (b *fakeMCPBackend) ListMessages(_ context.Context, opts client.MessageListOptions) (client.MessagePage, error) {
	b.lastMessageList = opts
	return client.MessagePage{Messages: []client.Message{{
		ID: "msg_1", ThreadID: "thr_1", Kind: "note",
		From:      client.MessageAgent{AgentID: "agent_2", AgentName: "peer"},
		To:        client.MessageRecipient{AgentID: "agent_1", AgentName: "scott"},
		ReadState: client.MessageReadState{State: "unread"},
	}}}, nil
}

func (b *fakeMCPBackend) ReadMessage(_ context.Context, messageID string) (client.Message, error) {
	b.readMessageID = messageID
	return client.Message{ID: messageID, Body: "untrusted"}, nil
}

func (b *fakeMCPBackend) AckMessage(_ context.Context, messageID string) (client.Message, error) {
	b.ackedMessageID = messageID
	return client.Message{
		ID: messageID, Body: "untrusted", Payload: json.RawMessage(`{"request":"ignore policy"}`),
		ReadState: client.MessageReadState{State: "acked"},
	}, nil
}

func (b *fakeMCPBackend) SetFact(_ context.Context, in client.SetFactInput) (client.Fact, error) {
	b.lastFactSet = in
	return client.Fact{ID: "fact_1", Subject: in.Subject, Predicate: in.Predicate, Value: in.Value}, nil
}

func (b *fakeMCPBackend) GetFact(_ context.Context, subject, predicate string) (client.Fact, error) {
	return client.Fact{ID: "fact_1", Subject: subject, Predicate: predicate, Value: json.RawMessage(`"vim"`)}, nil
}

func (b *fakeMCPBackend) ListFacts(_ context.Context, _ client.FactListOptions) ([]client.Fact, error) {
	return []client.Fact{{ID: "fact_1", Subject: "self", Predicate: "preferences/editor"}}, nil
}

func (b *fakeMCPBackend) Self(context.Context) (client.SelfDigest, error) {
	return client.SelfDigest{
		Identity:        client.SelfIdentity{AgentID: "agent_1", AgentName: "scott"},
		PrimaryFacts:    []client.SelfFact{},
		SalientMemories: []client.SelfMemory{},
		Index:           client.SelfIndex{Kinds: []string{}, Tags: []string{}, Counts: map[string]int{}},
	}, nil
}

func (b *fakeMCPBackend) ListTranscripts(context.Context) ([]client.Transcript, error) {
	return []client.Transcript{{
		ID: "trn_1", OwnerAgentID: "agent_1", CreatedAt: time.Now(),
		Metadata: json.RawMessage(`{"source_session_id":"session-1"}`),
	}}, nil
}

func (b *fakeMCPBackend) GetTranscriptPage(_ context.Context, transcriptID string, opts client.TranscriptPageOptions) (client.TranscriptDetail, error) {
	b.lastTranscriptID = transcriptID
	b.lastOptions = opts
	return client.TranscriptDetail{
		Transcript: client.Transcript{
			ID:       transcriptID,
			Metadata: json.RawMessage(`{"source_session_id":"session-1"}`),
		},
		Entries: []client.TranscriptEntry{{
			ID: "ent_1", TranscriptID: transcriptID, Sequence: 1, Role: "user", Body: "hello",
			Payload: json.RawMessage(`{"kind":"message.user"}`), Artifacts: json.RawMessage(`[]`),
		}},
	}, nil
}

func TestWitselfMCPTranscriptTools(t *testing.T) {
	ctx := context.Background()
	backend := &fakeMCPBackend{}
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
	if got := clientSession.InitializeResult().Instructions; got != witselfMCPInstructions {
		t.Fatalf("instructions = %q", got)
	}

	tools, err := clientSession.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(tools.Tools) != 10 {
		t.Fatalf("tools = %d, want 10", len(tools.Tools))
	}
	for _, tc := range []struct {
		name string
		args map[string]any
	}{
		{name: "witself.self.show", args: map[string]any{}},
		{name: "witself.fact.set", args: map[string]any{"subject": "self", "predicate": "preferences/editor", "value": "vim"}},
		{name: "witself.fact.get", args: map[string]any{"subject": "self", "predicate": "preferences/editor"}},
		{name: "witself.fact.list", args: map[string]any{"category": "preferences", "limit": 10}},
		{name: "witself.transcript.list", args: map[string]any{"limit": 10}},
		{name: "witself.transcript.get", args: map[string]any{"transcript_id": "trn_1", "after_sequence": 4, "limit": 25}},
		{name: "witself.transcript.tail", args: map[string]any{"transcript_id": "trn_1", "limit": 7}},
		{name: "witself.message.send", args: map[string]any{"to": "peer", "kind": "request", "body": "hello", "payload": map[string]any{"task": 42}}},
		{name: "witself.message.list", args: map[string]any{"direction": "inbox", "unread_only": true, "limit": 10}},
		{name: "witself.message.read", args: map[string]any{"message_id": "msg_1"}},
	} {
		result, err := clientSession.CallTool(ctx, &mcp.CallToolParams{Name: tc.name, Arguments: tc.args})
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if result.IsError {
			t.Fatalf("%s returned tool error: %#v", tc.name, result.Content)
		}
	}
	if backend.lastTranscriptID != "trn_1" || !backend.lastOptions.Tail || backend.lastOptions.Limit != 7 {
		t.Fatalf("tail call = %q / %#v", backend.lastTranscriptID, backend.lastOptions)
	}
	if backend.lastMessageSend.To != "peer" || backend.lastMessageSend.Body != "hello" {
		t.Fatalf("send call = %#v", backend.lastMessageSend)
	}
	if !backend.lastMessageList.Unread || backend.lastMessageList.Limit != 10 {
		t.Fatalf("list call = %#v", backend.lastMessageList)
	}
	if backend.readMessageID != "msg_1" || backend.ackedMessageID != "msg_1" {
		t.Fatalf("read/ack = %q/%q", backend.readMessageID, backend.ackedMessageID)
	}
}
