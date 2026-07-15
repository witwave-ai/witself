package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/witwave-ai/witself/internal/client"
)

type fakeMemoryVectorMCPBackend struct {
	*fakeMemoryMCPBackend
	lastProfileCreate client.CreateMemoryVectorProfileInput
	lastVectorPut     client.PutMemoryVectorInput
	profileCalls      int
	listCalls         int
	putCalls          int
}

func newFakeMemoryVectorMCPBackend() *fakeMemoryVectorMCPBackend {
	return &fakeMemoryVectorMCPBackend{fakeMemoryMCPBackend: newFakeMemoryMCPBackend()}
}

func (b *fakeMemoryVectorMCPBackend) CreateMemoryVectorProfile(_ context.Context, in client.CreateMemoryVectorProfileInput) (client.MemoryVectorProfile, error) {
	b.profileCalls++
	b.lastProfileCreate = in
	return client.MemoryVectorProfile{
		ID: "mvp_1", Provider: in.Provider, Model: in.Model, Recipe: in.Recipe,
		RecipeVersion: in.RecipeVersion, Dimensions: in.Dimensions,
		DistanceMetric: in.DistanceMetric, Normalization: in.Normalization,
		ContractHash: strings.Repeat("a", 64),
		CreatedAt:    time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC),
	}, nil
}

func (b *fakeMemoryVectorMCPBackend) ListMemoryVectorProfiles(context.Context) ([]client.MemoryVectorProfile, error) {
	b.listCalls++
	return []client.MemoryVectorProfile{{
		ID: "mvp_1", Provider: "local", Model: "embed-v1", Recipe: "plain",
		RecipeVersion: "1", Dimensions: 2, DistanceMetric: "cosine",
		Normalization: "l2", ContractHash: strings.Repeat("a", 64),
	}}, nil
}

func (b *fakeMemoryVectorMCPBackend) PutMemoryVector(_ context.Context, in client.PutMemoryVectorInput) (client.MemoryVectorReceipt, error) {
	b.putCalls++
	b.lastVectorPut = in
	return client.MemoryVectorReceipt{
		ProfileID: in.ProfileID, MemoryID: in.MemoryID, MemoryVersion: in.MemoryVersion,
		ContentHash: in.ContentHash, VectorHash: strings.Repeat("c", 64),
		Dimensions: len(in.Vector), CreatedAt: time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC),
	}, nil
}

func TestMCPMemoryVectorSchemasAndDispatchKeepReceiptsValueFree(t *testing.T) {
	ctx := context.Background()
	backend := newFakeMemoryVectorMCPBackend()
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

	tools, err := clientSession.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	wantTools := map[string][]string{
		"witself.memory.vector.profile.create": {"provider", "model", "recipe", "recipe_version", "dimensions", "distance_metric", "normalization"},
		"witself.memory.vector.profile.list":   {},
		"witself.memory.vector.set":            {"profile_id", "memory_id", "memory_version", "content_hash", "vector"},
	}
	found := map[string]bool{}
	for _, tool := range tools.Tools {
		fields, ok := wantTools[tool.Name]
		if !ok {
			continue
		}
		found[tool.Name] = true
		raw, err := json.Marshal(tool.InputSchema)
		if err != nil {
			t.Fatalf("marshal %s schema: %v", tool.Name, err)
		}
		for _, field := range fields {
			if !strings.Contains(string(raw), `"`+field+`"`) {
				t.Errorf("%s schema lacks %q: %s", tool.Name, field, raw)
			}
		}
		var inputSchema struct {
			Type       string                     `json:"type"`
			Properties map[string]json.RawMessage `json:"properties"`
			Required   []string                   `json:"required"`
		}
		if err := json.Unmarshal(raw, &inputSchema); err != nil {
			t.Fatalf("decode %s schema: %v", tool.Name, err)
		}
		if inputSchema.Type != "object" {
			t.Errorf("%s schema type = %q", tool.Name, inputSchema.Type)
		}
		required := make(map[string]bool, len(inputSchema.Required))
		for _, field := range inputSchema.Required {
			required[field] = true
		}
		for _, field := range fields {
			if _, ok := inputSchema.Properties[field]; !ok {
				t.Errorf("%s schema properties lack %q: %s", tool.Name, field, raw)
			}
			if !required[field] {
				t.Errorf("%s schema does not require %q: %s", tool.Name, field, raw)
			}
		}
		if tool.Name == "witself.memory.vector.set" && !strings.Contains(tool.Description, "vectors never appear in the response") {
			t.Errorf("vector set description lacks value-free receipt guarantee: %q", tool.Description)
		}
		if tool.Name == "witself.memory.vector.set" {
			outputRaw, err := json.Marshal(tool.OutputSchema)
			if err != nil {
				t.Fatalf("marshal vector output schema: %v", err)
			}
			var outputSchema struct {
				Properties map[string]json.RawMessage `json:"properties"`
			}
			if err := json.Unmarshal(outputRaw, &outputSchema); err != nil {
				t.Fatalf("decode vector output schema: %v", err)
			}
			if _, ok := outputSchema.Properties["vector"]; ok {
				t.Errorf("vector output schema exposes raw vector: %s", outputRaw)
			}
			if _, ok := outputSchema.Properties["vector_hash"]; !ok {
				t.Errorf("vector output schema lacks safe vector_hash receipt: %s", outputRaw)
			}
		}
	}
	if len(found) != len(wantTools) {
		t.Fatalf("vector tools = %#v", found)
	}

	createResult, err := clientSession.CallTool(ctx, &mcp.CallToolParams{
		Name: "witself.memory.vector.profile.create",
		Arguments: map[string]any{
			"provider": "local", "model": "embed-v1", "recipe": "plain",
			"recipe_version": "1", "dimensions": 2,
			"distance_metric": "cosine", "normalization": "l2",
		},
	})
	assertSuccessfulMemoryVectorToolResult(t, createResult, err)
	if backend.profileCalls != 1 || backend.lastProfileCreate.Provider != "local" ||
		backend.lastProfileCreate.Model != "embed-v1" || backend.lastProfileCreate.Dimensions != 2 {
		t.Fatalf("profile dispatch = calls:%d input:%#v", backend.profileCalls, backend.lastProfileCreate)
	}

	listResult, err := clientSession.CallTool(ctx, &mcp.CallToolParams{
		Name: "witself.memory.vector.profile.list", Arguments: map[string]any{},
	})
	assertSuccessfulMemoryVectorToolResult(t, listResult, err)
	if backend.listCalls != 1 {
		t.Fatalf("list calls = %d", backend.listCalls)
	}

	putResult, err := clientSession.CallTool(ctx, &mcp.CallToolParams{
		Name: "witself.memory.vector.set",
		Arguments: map[string]any{
			"profile_id": "mvp_1", "memory_id": "mem_1", "memory_version": 3,
			"content_hash": strings.Repeat("b", 64),
			"vector":       []float64{12345.6789, -98765.4321},
		},
	})
	assertSuccessfulMemoryVectorToolResult(t, putResult, err)
	if backend.putCalls != 1 || backend.lastVectorPut.ProfileID != "mvp_1" ||
		backend.lastVectorPut.MemoryID != "mem_1" || backend.lastVectorPut.MemoryVersion != 3 ||
		len(backend.lastVectorPut.Vector) != 2 || backend.lastVectorPut.Vector[0] != 12345.6789 ||
		backend.lastVectorPut.Vector[1] != -98765.4321 {
		t.Fatalf("put dispatch = calls:%d input:%#v", backend.putCalls, backend.lastVectorPut)
	}
	assertMemoryVectorToolResultDoesNotEcho(t, putResult, "12345.6789", "-98765.4321", `"vector":`)

	recallResult, err := clientSession.CallTool(ctx, &mcp.CallToolParams{
		Name: "witself.memory.recall",
		Arguments: map[string]any{
			"query": "database", "vector_profile_id": "mvp_1",
			"query_vector": []float64{12345.6789, -98765.4321}, "limit": 4,
		},
	})
	assertSuccessfulMemoryVectorToolResult(t, recallResult, err)
	if backend.lastRecall.VectorProfileID != "mvp_1" || len(backend.lastRecall.QueryVector) != 2 ||
		backend.lastRecall.QueryVector[0] != 12345.6789 || backend.lastRecall.QueryVector[1] != -98765.4321 {
		t.Fatalf("recall dispatch = %#v", backend.lastRecall)
	}
	assertMemoryVectorToolResultDoesNotEcho(t, recallResult, "12345.6789", "-98765.4321", `"query_vector":`)
}

func TestMCPMemoryRecallRejectsHalfVectorContractBeforeDispatch(t *testing.T) {
	ctx := context.Background()
	backend := newFakeMemoryVectorMCPBackend()
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
		Name: "witself.memory.recall",
		Arguments: map[string]any{
			"query": "database", "vector_profile_id": "mvp_1",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.IsError || backend.lastRecall.Query != "" {
		t.Fatalf("half vector contract result = %#v; backend = %#v", result, backend.lastRecall)
	}
}

func assertSuccessfulMemoryVectorToolResult(t *testing.T, result *mcp.CallToolResult, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
	if result == nil || result.IsError {
		t.Fatalf("tool result = %#v", result)
	}
}

func assertMemoryVectorToolResultDoesNotEcho(t *testing.T, result *mcp.CallToolResult, forbidden ...string) {
	t.Helper()
	raw, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range forbidden {
		if strings.Contains(string(raw), value) {
			t.Errorf("tool result echoed %q: %s", value, raw)
		}
	}
}
