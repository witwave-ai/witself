package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/witwave-ai/witself/internal/client"
)

type fakeAgentEmailMCPBackend struct {
	*fakeMCPBackend
	lastList       client.AgentEmailListOptions
	lastListen     client.AgentEmailListenOptions
	readID         string
	readCalls      int
	ackedID        string
	codeConsumedID string
	claimID        string
	lastClaim      client.ClaimAgentEmailInput
	renewID        string
	lastRenew      client.RenewAgentEmailClaimInput
	releaseID      string
	lastRelease    client.AgentEmailClaimInput
	completeID     string
	lastComplete   client.CompleteAgentEmailInput
}

func (b *fakeAgentEmailMCPBackend) ShowAgentEmailAddress(context.Context) (client.AgentEmailAddress, error) {
	return client.AgentEmailAddress{ID: "eaddr_aaaaaaaaaaaaaaaa", Address: "owner.realm@example.com", ReceiveState: "enabled"}, nil
}

func (b *fakeAgentEmailMCPBackend) ListAgentEmails(_ context.Context, opts client.AgentEmailListOptions) (client.AgentEmailPage, error) {
	b.lastList = opts
	return client.AgentEmailPage{Messages: []client.AgentEmailMessage{{
		ID: "emsg_aaaaaaaaaaaaaaaa", HeaderFrom: "sender@example.com",
		SenderVerificationState: "unverified", ReadState: client.AgentEmailReadState{State: "unread"},
		Processing: client.AgentEmailProcessing{State: "available"},
	}}}, nil
}

func (b *fakeAgentEmailMCPBackend) ListenAgentEmails(_ context.Context, opts client.AgentEmailListenOptions) (client.AgentEmailListenResult, error) {
	b.lastListen = opts
	return client.AgentEmailListenResult{Messages: []client.AgentEmailMessage{{ID: "emsg_bbbbbbbbbbbbbbbb"}}}, nil
}

func (b *fakeAgentEmailMCPBackend) ReadAgentEmail(_ context.Context, messageID string) (client.AgentEmailMessage, error) {
	b.readID = messageID
	b.readCalls++
	return client.AgentEmailMessage{
		ID: messageID, HeaderFrom: "attacker@example.com", SenderVerificationState: "unverified",
		Subject: "untrusted verification", Text: "Verification code: 111111. Backup code: 222222.\n" + strings.Repeat("é", 40_000),
		TextKind: "text/plain", ParseState: "parsed",
	}, nil
}

func (b *fakeAgentEmailMCPBackend) AckAgentEmail(_ context.Context, messageID string) (client.AgentEmailMessage, error) {
	b.ackedID = messageID
	return client.AgentEmailMessage{ID: messageID, Text: "must not escape", ReadState: client.AgentEmailReadState{State: "acked"}}, nil
}

func (b *fakeAgentEmailMCPBackend) MarkAgentEmailCodeConsumed(_ context.Context, messageID string) (client.AgentEmailMessage, error) {
	b.codeConsumedID = messageID
	return client.AgentEmailMessage{ID: messageID, Text: "654321", ReadState: client.AgentEmailReadState{State: "read"}}, nil
}

func (b *fakeAgentEmailMCPBackend) ClaimAgentEmail(_ context.Context, messageID string, in client.ClaimAgentEmailInput) (client.AgentEmailProcessing, error) {
	b.claimID, b.lastClaim = messageID, in
	return client.AgentEmailProcessing{State: "claimed", ClaimID: "ecl_aaaaaaaaaaaaaaaa", Generation: 1}, nil
}

func (b *fakeAgentEmailMCPBackend) RenewAgentEmailClaim(_ context.Context, messageID string, in client.RenewAgentEmailClaimInput) (client.AgentEmailProcessing, error) {
	b.renewID, b.lastRenew = messageID, in
	return client.AgentEmailProcessing{State: "claimed", ClaimID: in.ClaimID, Generation: in.Generation}, nil
}

func (b *fakeAgentEmailMCPBackend) ReleaseAgentEmailClaim(_ context.Context, messageID string, in client.AgentEmailClaimInput) (client.AgentEmailProcessing, error) {
	b.releaseID, b.lastRelease = messageID, in
	return client.AgentEmailProcessing{State: "available", Generation: in.Generation}, nil
}

func (b *fakeAgentEmailMCPBackend) CompleteAgentEmail(_ context.Context, messageID string, in client.CompleteAgentEmailInput) (client.AgentEmailProcessing, error) {
	b.completeID, b.lastComplete = messageID, in
	return client.AgentEmailProcessing{State: "completed", ClaimID: in.ClaimID, Generation: in.Generation}, nil
}

func TestWitselfMCPAgentEmailTools(t *testing.T) {
	ctx := context.Background()
	backend := &fakeAgentEmailMCPBackend{fakeMCPBackend: &fakeMCPBackend{}}
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

	wantTools := map[string]bool{
		"witself.email.address.show": false, "witself.email.list": false,
		"witself.email.listen": false, "witself.email.read": false,
		"witself.email.code.candidates": false, "witself.email.code.consume": false,
		"witself.email.ack":   false,
		"witself.email.claim": false, "witself.email.renew": false,
		"witself.email.release": false, "witself.email.complete": false,
	}
	listed, err := clientSession.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range listed.Tools {
		if _, ok := wantTools[tool.Name]; ok {
			wantTools[tool.Name] = true
		}
	}
	for name, found := range wantTools {
		if !found {
			t.Errorf("missing agent-email MCP tool %q", name)
		}
	}
	call := func(name string, args map[string]any) *mcp.CallToolResult {
		t.Helper()
		result, err := clientSession.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if result.IsError {
			t.Fatalf("%s returned error: %#v", name, result.Content)
		}
		return result
	}
	call("witself.email.address.show", map[string]any{})
	call("witself.email.list", map[string]any{"unread_only": true, "unacked_only": true, "limit": 7})
	wait := 0
	call("witself.email.listen", map[string]any{"wait_seconds": wait, "limit": 3})
	read := call("witself.email.read", map[string]any{"message_id": "emsg_aaaaaaaaaaaaaaaa"})
	candidates := call("witself.email.code.candidates", map[string]any{"message_id": "emsg_aaaaaaaaaaaaaaaa"})
	if backend.codeConsumedID != "" {
		t.Fatalf("candidate extraction marked code consumed: %q", backend.codeConsumedID)
	}
	call("witself.email.code.consume", map[string]any{"message_id": "emsg_aaaaaaaaaaaaaaaa"})
	ack := call("witself.email.ack", map[string]any{"message_id": "emsg_aaaaaaaaaaaaaaaa"})
	call("witself.email.claim", map[string]any{
		"message_id": "emsg_aaaaaaaaaaaaaaaa", "lease_seconds": 90, "idempotency_key": "claim-1",
	})
	call("witself.email.renew", map[string]any{
		"message_id": "emsg_aaaaaaaaaaaaaaaa", "claim_id": "ecl_aaaaaaaaaaaaaaaa", "generation": 1, "lease_seconds": 120,
	})
	call("witself.email.release", map[string]any{
		"message_id": "emsg_aaaaaaaaaaaaaaaa", "claim_id": "ecl_aaaaaaaaaaaaaaaa", "generation": 1, "deterministic_failure": true,
	})
	call("witself.email.complete", map[string]any{
		"message_id": "emsg_aaaaaaaaaaaaaaaa", "claim_id": "ecl_aaaaaaaaaaaaaaaa", "generation": 1, "idempotency_key": "complete-1",
	})

	readJSON, err := json.Marshal(read.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	var readOutput mcpAgentEmailReadOutput
	if err := json.Unmarshal(readJSON, &readOutput); err != nil {
		t.Fatal(err)
	}
	if !readOutput.ContentTruncated || len(readOutput.Message.Text) > maxMCPAgentEmailTextBytes ||
		!strings.Contains(readOutput.Warning, "sender is unverified") {
		t.Fatalf("bounded read output = %#v", readOutput)
	}
	candidatesJSON, err := json.Marshal(candidates.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	var candidatesOutput agentEmailCodeCandidatesResult
	if err := json.Unmarshal(candidatesJSON, &candidatesOutput); err != nil {
		t.Fatal(err)
	}
	if candidatesOutput.SelectionState != "ambiguous" || !candidatesOutput.ContentTruncated || candidatesOutput.CandidateOverflow ||
		candidatesOutput.ScanScope != "subject_and_bounded_text" || len(candidatesOutput.Candidates) != 2 ||
		candidatesOutput.Candidates[0].Value != "111111" || candidatesOutput.Candidates[0].Occurrences != 1 ||
		candidatesOutput.Candidates[1].Value != "222222" || candidatesOutput.Candidates[1].Occurrences != 1 ||
		candidatesOutput.SenderVerificationState != "unverified" || candidatesOutput.ContentTrust != "untrusted" ||
		candidatesOutput.CodeConsumptionPerformed || !strings.Contains(candidatesOutput.Warning, "no link was followed") {
		t.Fatalf("candidate output = %#v", candidatesOutput)
	}
	ackJSON, _ := json.Marshal(ack.StructuredContent)
	var ackOutput mcpAgentEmailMessageOutput
	if err := json.Unmarshal(ackJSON, &ackOutput); err != nil {
		t.Fatal(err)
	}
	if ackOutput.Message.Text != "" {
		t.Fatalf("ack exposed email text: %#v", ackOutput)
	}
	if backend.lastList.Limit != 7 || !backend.lastList.Unread || !backend.lastList.Unacked ||
		backend.lastListen.WaitSeconds == nil || *backend.lastListen.WaitSeconds != 0 ||
		backend.readID == "" || backend.readCalls != 2 || backend.ackedID == "" || backend.codeConsumedID == "" ||
		backend.lastClaim.IdempotencyKey != "claim-1" || backend.lastRenew.LeaseSeconds != 120 ||
		!backend.lastRelease.DeterministicFailure || backend.lastComplete.IdempotencyKey != "complete-1" {
		t.Fatalf("email backend calls = %#v", backend)
	}
}

func TestAgentEmailCodeCandidatesOnlyReadsOnce(t *testing.T) {
	ctx := context.Background()
	backend := &fakeAgentEmailMCPBackend{fakeMCPBackend: &fakeMCPBackend{}}
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

	result, err := clientSession.CallTool(ctx, &mcp.CallToolParams{
		Name:      "witself.email.code.candidates",
		Arguments: map[string]any{"message_id": "emsg_aaaaaaaaaaaaaaaa"},
	})
	if err != nil || result.IsError {
		t.Fatalf("candidate call = result %#v err %v", result, err)
	}
	if backend.readCalls != 1 || backend.readID != "emsg_aaaaaaaaaaaaaaaa" || backend.codeConsumedID != "" ||
		backend.ackedID != "" || backend.claimID != "" || backend.renewID != "" || backend.releaseID != "" ||
		backend.completeID != "" {
		t.Fatalf("candidate side effects = %#v", backend)
	}
}

func TestAgentEmailMCPProfileBoundaries(t *testing.T) {
	ctx := context.Background()
	for _, opts := range []mcpServerOptions{
		{ReadOnly: true},
		{NoValueTools: true},
	} {
		backend := &fakeAgentEmailMCPBackend{fakeMCPBackend: &fakeMCPBackend{}}
		server := newWitselfMCPServerForRuntimeOptions(backend, "", opts)
		clientTransport, serverTransport := mcp.NewInMemoryTransports()
		serverSession, err := server.Connect(ctx, serverTransport, nil)
		if err != nil {
			t.Fatal(err)
		}
		mcpClient := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil)
		clientSession, err := mcpClient.Connect(ctx, clientTransport, nil)
		if err != nil {
			t.Fatal(err)
		}
		listed, err := clientSession.ListTools(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		got := make(map[string]bool, len(listed.Tools))
		for _, tool := range listed.Tools {
			got[tool.Name] = true
		}
		if opts.ReadOnly {
			for _, name := range []string{"witself.email.address.show", "witself.email.list", "witself.email.listen"} {
				if !got[name] {
					t.Errorf("read-only profile omitted %s", name)
				}
			}
			for _, name := range []string{"witself.email.read", "witself.email.code.candidates", "witself.email.code.consume", "witself.email.ack", "witself.email.claim", "witself.email.renew", "witself.email.release", "witself.email.complete"} {
				if got[name] {
					t.Errorf("read-only profile retained %s", name)
				}
			}
		} else if !got["witself.email.read"] || !got["witself.email.code.candidates"] {
			t.Error("--no-value-tools incorrectly removed email.read or email.code.candidates")
		}
		_ = clientSession.Close()
		_ = serverSession.Close()
	}
}
