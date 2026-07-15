package main

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/witwave-ai/witself/internal/client"
)

type fakeMemoryMCPBackend struct {
	*fakeMCPBackend
	lastCapture         client.CaptureMemoryInput
	lastReadID          string
	lastList            client.MemoryListOptions
	lastRecall          client.MemoryRecallInput
	lastHistoryID       string
	lastHistory         client.MemoryHistoryOptions
	lastAdjust          client.AdjustMemoryInput
	lastSupersede       client.SupersedeMemoryInput
	lastForget          client.MemoryLifecycleInput
	lastRestore         client.MemoryLifecycleInput
	lastReactivate      client.MemoryLifecycleInput
	lastResolution      client.ResolveMemoryEvidenceInput
	lastDelete          client.DeleteMemoryInput
	lastDeletePreviewID string
	captureCalls        int
	supersedeCalls      int
	deleteCalls         int
}

func newFakeMemoryMCPBackend() *fakeMemoryMCPBackend {
	return &fakeMemoryMCPBackend{fakeMCPBackend: &fakeMCPBackend{}}
}

func (b *fakeMemoryMCPBackend) CaptureMemory(_ context.Context, in client.CaptureMemoryInput) (client.MemoryMutationResult, error) {
	b.captureCalls++
	b.lastCapture = in
	return memoryMutationFixture("capture", 1, in.IdempotencyKey), nil
}

func (b *fakeMemoryMCPBackend) GetMemory(_ context.Context, id string) (client.Memory, error) {
	b.lastReadID = id
	return memoryFixture(id, 1, "active", "capture"), nil
}

func (b *fakeMemoryMCPBackend) ListMemories(_ context.Context, in client.MemoryListOptions) (client.MemoryPage, error) {
	b.lastList = in
	return client.MemoryPage{Items: []client.Memory{memoryFixture("mem_1", 1, "active", "capture")}, NextCursor: "next-memory"}, nil
}

func (b *fakeMemoryMCPBackend) RecallMemories(_ context.Context, in client.MemoryRecallInput) (client.MemoryRecallPage, error) {
	b.lastRecall = in
	return client.MemoryRecallPage{
		Hits: []client.MemoryRecallHit{{
			Memory: memoryFixture("mem_1", 1, "active", "capture"),
			Score:  client.MemoryRecallScore{Lexical: 0.8, Salience: 0.7, Recency: 0.6, Total: 0.745},
		}},
		RetrievalMode: "lexical",
	}, nil
}

func (b *fakeMemoryMCPBackend) GetMemoryHistory(_ context.Context, id string, in client.MemoryHistoryOptions) (client.MemoryHistoryPage, error) {
	b.lastHistoryID, b.lastHistory = id, in
	return client.MemoryHistoryPage{Versions: []client.MemoryVersion{{MemoryID: id, Version: 1, ContentEncoding: "plain", State: "active"}}}, nil
}

func (b *fakeMemoryMCPBackend) AdjustMemory(_ context.Context, in client.AdjustMemoryInput) (client.MemoryMutationResult, error) {
	b.lastAdjust = in
	return memoryMutationFixture("adjust", in.ExpectedVersion+1, in.IdempotencyKey), nil
}

func (b *fakeMemoryMCPBackend) SupersedeMemory(_ context.Context, in client.SupersedeMemoryInput) (client.SupersedeMemoryResult, error) {
	b.supersedeCalls++
	b.lastSupersede = in
	replacements := make([]client.Memory, len(in.Replacements))
	refs := make([]client.MemoryVersionReference, len(in.Replacements))
	for i := range in.Replacements {
		id := fmt.Sprintf("mem_replacement_%d", i+1)
		replacements[i] = memoryFixture(id, 1, "active", "capture")
		refs[i] = client.MemoryVersionReference{MemoryID: id, Version: 1}
	}
	return client.SupersedeMemoryResult{
		Source:       memoryFixture(in.MemoryID, in.ExpectedVersion+1, "superseded", "supersede"),
		Replacements: replacements,
		Receipt: client.MemorySupersessionReceipt{
			Operation: "supersede", IdempotencyKey: in.IdempotencyKey,
			SupersessionSetID: "mset_1", SupersessionSetRevision: 1,
			ReplacementCount: int64(len(refs)), ReplacementDigest: strings.Repeat("c", 64),
			Source:       client.MemoryVersionReference{MemoryID: in.MemoryID, Version: in.ExpectedVersion + 1},
			Replacements: refs,
		},
	}, nil
}

func (b *fakeMemoryMCPBackend) ForgetMemory(_ context.Context, in client.MemoryLifecycleInput) (client.MemoryMutationResult, error) {
	b.lastForget = in
	return memoryMutationFixture("forget", in.ExpectedVersion+1, in.IdempotencyKey), nil
}

func (b *fakeMemoryMCPBackend) RestoreMemory(_ context.Context, in client.MemoryLifecycleInput) (client.MemoryMutationResult, error) {
	b.lastRestore = in
	return memoryMutationFixture("restore", in.ExpectedVersion+1, in.IdempotencyKey), nil
}

func (b *fakeMemoryMCPBackend) ReactivateMemory(_ context.Context, in client.MemoryLifecycleInput) (client.MemoryMutationResult, error) {
	b.lastReactivate = in
	return memoryMutationFixture("reactivate", in.ExpectedVersion+1, in.IdempotencyKey), nil
}

func (b *fakeMemoryMCPBackend) ResolveMemoryEvidence(_ context.Context, in client.ResolveMemoryEvidenceInput) (client.MemoryEvidence, error) {
	b.lastResolution = in
	return client.MemoryEvidence{ID: "mev_terminal", MemoryID: "mem_1", MemoryVersion: 1, State: "resolved", PendingEvidenceID: in.EvidenceID}, nil
}

func (b *fakeMemoryMCPBackend) PreviewDeleteMemory(_ context.Context, memoryID string) (client.MemoryDeletionReceipt, error) {
	b.lastDeletePreviewID = memoryID
	return memoryDeletionFixture(memoryID, false), nil
}

func (b *fakeMemoryMCPBackend) DeleteMemory(_ context.Context, in client.DeleteMemoryInput) (client.MemoryDeletionReceipt, error) {
	b.deleteCalls++
	b.lastDelete = in
	receipt := memoryDeletionFixture(in.MemoryID, true)
	receipt.PriorVersion = in.ExpectedVersion
	receipt.ScrubSetRevision = in.ScrubSetRevision
	return receipt, nil
}

func TestWitselfMCPMemoryToolsPreserveGuardsAndEvidence(t *testing.T) {
	ctx := context.Background()
	backend := newFakeMemoryMCPBackend()
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

	calls := []struct {
		name string
		args map[string]any
	}{
		{name: "witself.memory.capture", args: map[string]any{
			"content": "we chose PostgreSQL", "kind": "decision", "tags": []string{"architecture"},
			"salience": 0.8, "capture_reason": "explicit", "idempotency_key": "capture-1",
			"occurred_from": "2026-07-14T01:00:00-06:00", "occurred_until": "2026-07-14T02:00:00-06:00",
			"evidence": []map[string]any{{
				"state": "resolved", "type": "transcript", "role": "supports",
				"transcript_id": "trn_1", "entry_from_sequence": 2, "entry_until_sequence": 4,
			}},
		}},
		{name: "witself.memory.read", args: map[string]any{"memory_id": "mem_1"}},
		{name: "witself.memory.list", args: map[string]any{
			"state": "active", "kind": "decision", "tags": []string{"architecture"},
			"include_sensitive": true, "limit": 12, "cursor": "memory-cursor",
		}},
		{name: "witself.memory.recall", args: map[string]any{
			"query": "database decision", "kind": "decision",
			"tags": []string{"architecture"}, "limit": 6, "cursor": "recall-cursor",
		}},
		{name: "witself.memory.history", args: map[string]any{"memory_id": "mem_1", "limit": 7, "cursor": "history-cursor"}},
		{name: "witself.memory.adjust", args: map[string]any{
			"memory_id": "mem_1", "expected_version": 1, "set_content": "dXBkYXRlZCBkZWNpc2lvbg==",
			"set_content_encoding": "base64",
			"add_tags":             []string{"settled"}, "set_sensitive": false, "idempotency_key": "adjust-1",
		}},
		{name: "witself.memory.supersede", args: map[string]any{
			"memory_id": "mem_1", "expected_version": 2,
			"reason": "split two decisions", "idempotency_key": "supersede-1",
			"replacements": []map[string]any{{
				"content": "dGhlIGRhdGFiYXNlIGRlY2lzaW9u", "content_encoding": "base64", "kind": "decision",
				"capture_reason": "curation", "idempotency_key": "replacement-1",
				"evidence": []map[string]any{{
					"state": "resolved", "type": "memory", "role": "supports",
					"source_memory_id": "mem_1", "source_memory_version": 2,
				}},
			}},
		}},
		{name: "witself.memory.forget", args: map[string]any{
			"memory_id": "mem_1", "expected_version": 2, "reason": "not useful", "idempotency_key": "forget-1",
		}},
		{name: "witself.memory.restore", args: map[string]any{
			"memory_id": "mem_1", "expected_version": 3, "idempotency_key": "restore-1",
		}},
		{name: "witself.memory.reactivate", args: map[string]any{
			"memory_id": "mem_1", "expected_version": 4,
			"expected_supersession_set_revision": 4, "idempotency_key": "reactivate-1",
		}},
		{name: "witself.memory.evidence.resolve", args: map[string]any{
			"evidence_id": "mev_pending", "transcript_id": "trn_1",
			"entry_from_sequence": 2, "entry_until_sequence": 4,
			"idempotency_key": "resolve-1",
		}},
		{name: "witself.memory.delete", args: map[string]any{
			"mode": "preview", "memory_id": "mem_delete",
		}},
		{name: "witself.memory.delete", args: map[string]any{
			"mode": "apply", "memory_id": "mem_delete", "expected_version": 3,
			"scrub_set_revision": strings.Repeat("a", 64), "idempotency_key": "delete-1",
			"direct_user_authorized": true,
		}},
	}
	for _, call := range calls {
		result, err := clientSession.CallTool(ctx, &mcp.CallToolParams{Name: call.name, Arguments: call.args})
		if err != nil {
			t.Fatalf("%s: %v", call.name, err)
		}
		if result.IsError {
			t.Fatalf("%s returned tool error: %#v", call.name, result.Content)
		}
	}
	if backend.lastCapture.IdempotencyKey != "capture-1" || backend.lastCapture.ContentEncoding != "plain" ||
		len(backend.lastCapture.Evidence) != 1 || backend.lastCapture.Evidence[0].TranscriptID != "trn_1" {
		t.Fatalf("capture = %#v", backend.lastCapture)
	}
	if backend.lastCapture.OccurredFrom == nil || backend.lastCapture.OccurredFrom.Hour() != 7 {
		t.Fatalf("capture occurred_from = %#v", backend.lastCapture.OccurredFrom)
	}
	if backend.lastReadID != "mem_1" || backend.lastList.Limit != 12 || !backend.lastList.IncludeSensitive || backend.lastList.Cursor != "memory-cursor" {
		t.Fatalf("read/list = %q / %#v", backend.lastReadID, backend.lastList)
	}
	if backend.lastRecall.Query != "database decision" || backend.lastRecall.Kind != "decision" || backend.lastRecall.Limit != 6 || backend.lastRecall.Cursor != "recall-cursor" {
		t.Fatalf("recall = %#v", backend.lastRecall)
	}
	if backend.lastHistoryID != "mem_1" || backend.lastHistory.Limit != 7 || backend.lastHistory.Cursor != "history-cursor" {
		t.Fatalf("history = %q / %#v", backend.lastHistoryID, backend.lastHistory)
	}
	if backend.lastAdjust.ExpectedVersion != 1 || backend.lastAdjust.SetContent == nil ||
		backend.lastAdjust.SetContentEncoding == nil || *backend.lastAdjust.SetContentEncoding != "base64" ||
		backend.lastAdjust.SetSensitive == nil || *backend.lastAdjust.SetSensitive {
		t.Fatalf("adjust = %#v", backend.lastAdjust)
	}
	if backend.supersedeCalls != 1 || backend.lastSupersede.MemoryID != "mem_1" ||
		backend.lastSupersede.ExpectedVersion != 2 || backend.lastSupersede.IdempotencyKey != "supersede-1" ||
		len(backend.lastSupersede.Replacements) != 1 ||
		backend.lastSupersede.Replacements[0].IdempotencyKey != "replacement-1" ||
		backend.lastSupersede.Replacements[0].ContentEncoding != "base64" ||
		len(backend.lastSupersede.Replacements[0].Evidence) != 1 {
		t.Fatalf("supersede = %#v / calls=%d", backend.lastSupersede, backend.supersedeCalls)
	}
	if backend.lastForget.IdempotencyKey != "forget-1" || backend.lastRestore.ExpectedVersion != 3 || backend.lastReactivate.ExpectedSupersessionSetRevision == nil || *backend.lastReactivate.ExpectedSupersessionSetRevision != 4 {
		t.Fatalf("lifecycle = %#v / %#v / %#v", backend.lastForget, backend.lastRestore, backend.lastReactivate)
	}
	if backend.lastResolution.EvidenceID != "mev_pending" || backend.lastResolution.TranscriptID != "trn_1" || backend.lastResolution.IdempotencyKey != "resolve-1" {
		t.Fatalf("evidence resolution = %#v", backend.lastResolution)
	}
	if backend.lastDeletePreviewID != "mem_delete" || backend.deleteCalls != 1 ||
		backend.lastDelete.MemoryID != "mem_delete" || backend.lastDelete.ExpectedVersion != 3 ||
		backend.lastDelete.IdempotencyKey != "delete-1" || !backend.lastDelete.DirectUserAuthorized {
		t.Fatalf("memory deletion = %q / %#v / calls=%d", backend.lastDeletePreviewID, backend.lastDelete, backend.deleteCalls)
	}
}

func TestMCPMemoryDeleteRequiresDirectCurrentUserAuthority(t *testing.T) {
	ctx := context.Background()
	backend := newFakeMemoryMCPBackend()
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

	result, err := clientSession.CallTool(ctx, &mcp.CallToolParams{
		Name: "witself.memory.delete",
		Arguments: map[string]any{
			"mode": "apply", "memory_id": "mem_delete", "expected_version": 3,
			"scrub_set_revision": strings.Repeat("a", 64), "idempotency_key": "delete-1",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.IsError || backend.deleteCalls != 0 {
		t.Fatalf("unauthorized deletion result = %#v; calls = %d", result, backend.deleteCalls)
	}
}

func TestMCPMemoryCaptureRejectsAmbiguousEvidenceBeforeWrite(t *testing.T) {
	ctx := context.Background()
	backend := newFakeMemoryMCPBackend()
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
	result, err := clientSession.CallTool(ctx, &mcp.CallToolParams{
		Name: "witself.memory.capture",
		Arguments: map[string]any{
			"content": "ambiguous", "kind": "note", "capture_reason": "explicit", "idempotency_key": "bad-1",
			"evidence": []map[string]any{{
				"state": "pending", "external_locator": "codex://session/1", "transcript_id": "trn_1",
			}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.IsError || backend.captureCalls != 0 {
		t.Fatalf("ambiguous evidence result = %#v; calls = %d", result, backend.captureCalls)
	}
}

func TestMCPMemoryCaptureRequiresEvidenceBeforeWrite(t *testing.T) {
	ctx := context.Background()
	backend := newFakeMemoryMCPBackend()
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
	result, err := clientSession.CallTool(ctx, &mcp.CallToolParams{
		Name: "witself.memory.capture",
		Arguments: map[string]any{
			"content": "no evidence", "kind": "note",
			"capture_reason": "explicit", "idempotency_key": "bad-2",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.IsError || backend.captureCalls != 0 {
		t.Fatalf("evidence-free result = %#v; calls = %d", result, backend.captureCalls)
	}
}

func TestMCPMemoryConvertersRejectUnknownContentEncoding(t *testing.T) {
	encoding := "gzip"
	if _, err := toClientMemoryAdjust(mcpMemoryAdjustInput{
		MemoryID: "mem_1", ExpectedVersion: 1, SetContentEncoding: &encoding,
		IdempotencyKey: "adjust-1",
	}); err == nil || !strings.Contains(err.Error(), "set_content_encoding") {
		t.Fatalf("adjust encoding error = %v", err)
	}
	if _, err := toClientMemorySupersede(mcpMemorySupersedeInput{
		MemoryID: "mem_1", ExpectedVersion: 1, IdempotencyKey: "supersede-1",
		Replacements: []mcpMemoryCaptureInput{{
			Content: "value", ContentEncoding: "gzip", Kind: "note",
			CaptureReason: "curation", IdempotencyKey: "replacement-1",
			Evidence: []mcpMemoryEvidenceInput{{State: "unavailable", UnavailableReason: "not_recorded"}},
		}},
	}); err == nil || !strings.Contains(err.Error(), "content_encoding") {
		t.Fatalf("supersede encoding error = %v", err)
	}
}

func TestMCPMemorySupersedeRejectsDuplicateKeysBeforeWrite(t *testing.T) {
	ctx := context.Background()
	backend := newFakeMemoryMCPBackend()
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
	result, err := clientSession.CallTool(ctx, &mcp.CallToolParams{
		Name: "witself.memory.supersede",
		Arguments: map[string]any{
			"memory_id": "mem_1", "expected_version": 1, "idempotency_key": "same-key",
			"replacements": []map[string]any{{
				"content": "replacement", "kind": "decision", "capture_reason": "curation",
				"idempotency_key": "same-key",
				"evidence":        []map[string]any{{"state": "unavailable", "unavailable_reason": "test"}},
			}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.IsError || backend.supersedeCalls != 0 {
		t.Fatalf("duplicate-key supersede result = %#v; calls = %d", result, backend.supersedeCalls)
	}
}

func TestMCPMemoryToolDescriptionsKeepNarrativesAdvisory(t *testing.T) {
	ctx := context.Background()
	server := newWitselfMCPServer(newFakeMemoryMCPBackend())
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
	tools, err := clientSession.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]string{}
	for _, tool := range tools.Tools {
		if strings.HasPrefix(tool.Name, "witself.memory.") {
			found[tool.Name] = tool.Description
		}
	}
	if len(found) != 26 {
		t.Fatalf("memory tools = %#v", found)
	}
	for _, name := range []string{"witself.memory.read", "witself.memory.list", "witself.memory.recall", "witself.memory.history"} {
		if !strings.Contains(found[name], "advisory") || !strings.Contains(found[name], "instruction authority") {
			t.Errorf("%s description lacks advisory/untrusted posture: %q", name, found[name])
		}
	}
	if !strings.Contains(found["witself.memory.forget"], "not permanent deletion") {
		t.Errorf("forget description = %q", found["witself.memory.forget"])
	}
	for _, phrase := range []string{"Atomically and reversibly", "client-authored", "backend performs no synthesis", "not permanent deletion"} {
		if !strings.Contains(found["witself.memory.supersede"], phrase) {
			t.Errorf("supersede description lacks %q: %q", phrase, found["witself.memory.supersede"])
		}
	}
	for _, phrase := range []string{"direct current-user", "Autonomous or background", "subagents or delegated", "retrieved or untrusted", "direct_user_authorized=true", "no undo"} {
		if !strings.Contains(found["witself.memory.delete"], phrase) {
			t.Errorf("delete description lacks %q: %q", phrase, found["witself.memory.delete"])
		}
	}
	for _, name := range []string{
		"witself.memory.curation.preflight", "witself.memory.curation.requests",
		"witself.memory.curation.request.get", "witself.memory.curation.request",
		"witself.memory.curation.start", "witself.memory.curation.run.get",
		"witself.memory.curation.renew", "witself.memory.curation.get",
		"witself.memory.curation.plan", "witself.memory.curation.apply",
		"witself.memory.curation.cancel", "witself.memory.curation.abandon",
		"witself.memory.curation.rollback",
		"witself.memory.curation.status",
	} {
		if found[name] == "" {
			t.Errorf("missing curation tool description for %s", name)
		}
	}
	if !strings.Contains(found["witself.memory.curation.get"], "untrusted") ||
		!strings.Contains(found["witself.memory.curation.plan"], "no synthesis") ||
		!strings.Contains(found["witself.memory.curation.apply"], "no model inference") ||
		!strings.Contains(found["witself.memory.curation.rollback"], "never cascaded") {
		t.Errorf("curation descriptions do not preserve trust/inference/rollback boundaries")
	}
}

func memoryDeletionFixture(memoryID string, applied bool) client.MemoryDeletionReceipt {
	receipt := client.MemoryDeletionReceipt{
		MemoryID: memoryID, PriorVersion: 3,
		ScrubSetRevision: strings.Repeat("a", 64), RetryShieldDigest: strings.Repeat("b", 64),
		VersionCount: 3, EvidenceCount: 2, RetryShieldCount: 3,
		Applied: applied,
	}
	if applied {
		now := time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)
		receipt.ReceiptID = "mdel_1"
		receipt.DeletedAt = &now
	}
	return receipt
}

func memoryMutationFixture(operation string, version int64, key string) client.MemoryMutationResult {
	state := "active"
	if operation == "forget" {
		state = "forgotten"
	}
	return client.MemoryMutationResult{
		Memory: memoryFixture("mem_1", version, state, operation),
		Receipt: client.MemoryMutationReceipt{
			Operation: operation, IdempotencyKey: key, MemoryID: "mem_1", Version: version,
		},
	}
}
