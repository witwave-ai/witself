package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func newMCPAlignmentSession(t *testing.T, server *mcp.Server) *mcp.ClientSession {
	t.Helper()
	ctx := context.Background()
	clientTransport, serverTransport := mcp.NewInMemoryTransports()
	serverSession, err := server.Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = serverSession.Close() })
	clientSession, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil).Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = clientSession.Close() })
	return clientSession
}

func mcpAlignmentSchemaMap(t *testing.T, schema any) map[string]any {
	t.Helper()
	raw, err := json.Marshal(schema)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	return decoded
}

func mcpAlignmentListedSchema(t *testing.T, session *mcp.ClientSession, name string) []byte {
	t.Helper()
	params := &mcp.ListToolsParams{}
	for {
		page, err := session.ListTools(context.Background(), params)
		if err != nil {
			t.Fatal(err)
		}
		for _, tool := range page.Tools {
			if tool.Name == name {
				raw, err := json.Marshal(tool.InputSchema)
				if err != nil {
					t.Fatal(err)
				}
				return raw
			}
		}
		if page.NextCursor == "" {
			t.Fatalf("MCP omitted %s", name)
		}
		params.Cursor = page.NextCursor
	}
}

func TestMCPPatchedInputSchemasDifferOnlyByDeclaredArrayConstraints(t *testing.T) {
	for _, tc := range []struct {
		tool    string
		input   reflect.Type
		backend witselfMCPBackend
		path    []string
		nonNull bool
	}{
		{"witself.secret.create", reflect.TypeFor[mcpSecretCreateInput](), newFakeSecretMCPBackend(), []string{"fields"}, false},
		{"witself.memory.supersede", reflect.TypeFor[mcpMemorySupersedeInput](), newFakeMemoryMCPBackend(), []string{"replacements"}, false},
		{"witself.message.request.select", reflect.TypeFor[mcpMessageRequestSelectInput](), &fakeMCPBackend{}, []string{"selected_agent_ids"}, false},
		{"witself.memory.vector.set", reflect.TypeFor[mcpMemoryVectorPutInput](), newFakeMemoryVectorMCPBackend(), []string{"vector"}, false},
		{"witself.memory.curation.plan", reflect.TypeFor[mcpMemoryCurationPlanInput](), &fakeCurationMCPBackend{}, []string{"draft", "actions"}, true},
		{"witself.memory.curation.rollback", reflect.TypeFor[mcpMemoryCurationRollbackInput](), &fakeCurationMCPBackend{}, []string{"expected_produced_heads"}, true},
	} {
		t.Run(tc.tool, func(t *testing.T) {
			inferred, err := jsonschema.ForType(tc.input, &jsonschema.ForOptions{})
			if err != nil {
				t.Fatal(err)
			}
			want := mcpAlignmentSchemaMap(t, inferred)
			property := want
			for _, segment := range tc.path {
				property = requireMCPObjectProperty(t, want, property, segment)
			}
			if !reflect.DeepEqual(property["type"], []any{"null", "array"}) {
				t.Fatalf("inferred %s type = %v, want exactly [null array]", strings.Join(tc.path, "."), property["type"])
			}
			if _, exists := property["minItems"]; exists {
				t.Fatal("inferred array unexpectedly already has minItems")
			}
			if tc.nonNull {
				property["type"] = "array"
			} else {
				property["minItems"] = float64(1)
			}
			session := newMCPAlignmentSession(t, newWitselfMCPServer(tc.backend))
			raw := mcpAlignmentListedSchema(t, session, tc.tool)
			var got map[string]any
			if err := json.Unmarshal(raw, &got); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("%s listed input schema differs from inference plus only the declared array constraint at %s", tc.tool, strings.Join(tc.path, "."))
			}
			if tc.tool == "witself.memory.curation.plan" {
				for _, profile := range []string{mcpProfileCuratorPreview, mcpProfileCuratorApply} {
					t.Run(profile, func(t *testing.T) {
						curator := newMCPAlignmentSession(t, newWitselfMCPServerForRuntimeOptions(tc.backend, "", mcpServerOptions{Profile: profile}))
						if !bytes.Equal(raw, mcpAlignmentListedSchema(t, curator, tc.tool)) {
							t.Errorf("%s plan input schema is not byte-identical to the full profile", profile)
						}
					})
				}
			}
		})
	}
}

func TestMCPInputSchemaPatchShapes(t *testing.T) {
	for _, tc := range []struct {
		name string
		path string
		run  func()
	}{
		{"missing_path", "draft.missing", func() { mcpInputSchema[mcpMemoryCurationPlanInput](mcpNonNullArray("draft", "missing")) }},
		{"non_array", "name", func() { mcpInputSchema[mcpSecretCreateInput](mcpNonEmptyArray("name")) }},
		{"existing_min_items", "fields", func() {
			mcpInputSchema[mcpSecretCreateInput](mcpNonEmptyArray("fields"), mcpNonEmptyArray("fields"))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				failure := recover()
				if failure == nil {
					t.Fatalf("patch of %s did not panic", tc.path)
				}
				if !strings.Contains(fmt.Sprint(failure), tc.path) {
					t.Errorf("panic %q does not name path %q", failure, tc.path)
				}
			}()
			tc.run()
		})
	}
	t.Run("plain_array_non_empty", func(t *testing.T) {
		property := &jsonschema.Schema{Type: "array"}
		root := &jsonschema.Schema{Properties: map[string]*jsonschema.Schema{"values": property}}
		if err := mcpNonEmptyArray("values")(root); err != nil {
			t.Fatal(err)
		}
		if property.MinItems == nil || *property.MinItems != 1 || property.Type != "array" || property.Types != nil {
			t.Fatal("plain array patch must add only minItems: 1")
		}
	})
	t.Run("plain_array_non_null", func(t *testing.T) {
		property := &jsonschema.Schema{Type: "array"}
		before := *property
		root := &jsonschema.Schema{Properties: map[string]*jsonschema.Schema{"values": property}}
		if err := mcpNonNullArray("values")(root); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(*property, before) {
			t.Fatal("non-null patch changed an already plain array")
		}
	})
}

const (
	mcpAlignmentSDKError       = `validating "arguments"`
	mcpAlignmentSecretError    = "secret create input is missing or exceeds the supported limit"
	mcpAlignmentSupersedeError = "replacements must contain 1-32 memory capsules"
	mcpAlignmentSelectionError = "selected_agent_ids must contain one to eight unique agent_ ids"
	mcpAlignmentPlanError      = "run_id, positive fencing_generation, complete draft, and idempotency_key are required"
	mcpAlignmentRollbackError  = "run_id, apply_receipt_id, expected_produced_heads, and idempotency_key are required"
)

func TestMCPEmptyListGuardsRefuseBeforeThisSlice(t *testing.T) {
	t.Run("fields", func(t *testing.T) {
		input := mcpSecretCreateInput{Name: "Example", IdempotencyKey: "create-key", Fields: []mcpSecretCreateFieldInput{}}
		if err := validateMCPSecretCreateBounds(input); err == nil || err.Error() != mcpAlignmentSecretError {
			t.Fatalf("empty fields guard = %v, want %q", err, mcpAlignmentSecretError)
		}
		value := "example"
		input.Fields = []mcpSecretCreateFieldInput{{Name: "username", Kind: "username", Value: &value}}
		if err := validateMCPSecretCreateBounds(input); err != nil {
			t.Fatalf("one valid field guard = %v", err)
		}
	})
	t.Run("replacements", func(t *testing.T) {
		_, err := toClientMemorySupersede(mcpMemorySupersedeInput{
			MemoryID: "mem_1", ExpectedVersion: 1, IdempotencyKey: "supersede-key", Replacements: []mcpMemoryCaptureInput{},
		})
		if err == nil || err.Error() != mcpAlignmentSupersedeError {
			t.Fatalf("empty replacements guard = %v, want %q", err, mcpAlignmentSupersedeError)
		}
	})
	t.Run("selected_agent_ids", func(t *testing.T) {
		if validSelectedMessageRequestAgents([]string{}) {
			t.Error("selection guard accepted non-nil empty agent list")
		}
		if !validSelectedMessageRequestAgents([]string{"agent_x"}) {
			t.Error("selection guard refused one valid agent")
		}
	})
}

func callMCPAlignmentTool(t *testing.T, session *mcp.ClientSession, tool string, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	result, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		t.Fatal(err)
	}
	if result == nil {
		t.Fatal("MCP returned a nil result")
	}
	return result
}

func mcpAlignmentErrorText(result *mcp.CallToolResult) string {
	// Successful vector results contain hashes; only collect refusal text.
	if !result.IsError {
		return ""
	}
	var text strings.Builder
	for _, content := range result.Content {
		if message, ok := content.(*mcp.TextContent); ok {
			text.WriteString(message.Text)
		}
	}
	return text.String()
}

func requireMCPAlignmentRefusal(t *testing.T, result *mcp.CallToolResult, want []string, forbidden string) {
	t.Helper()
	text := mcpAlignmentErrorText(result)
	if !result.IsError {
		t.Error("call succeeded; want an MCP refusal")
	}
	for _, fragment := range want {
		if !strings.Contains(text, fragment) {
			t.Errorf("error text %q does not contain %q", text, fragment)
		}
	}
	if forbidden != "" && strings.Contains(text, forbidden) {
		t.Errorf("error text %q contains forbidden refusal path %q", text, forbidden)
	}
}

func TestMCPEmptyListsAreRefusedBySchemaBeforeHandler(t *testing.T) {
	for _, tc := range []struct {
		tool         string
		field        string
		handlerError string
		args         func() map[string]any
		fixture      func() (witselfMCPBackend, func() int)
	}{
		{
			tool: "witself.secret.create", field: "fields", handlerError: mcpAlignmentSecretError,
			args: func() map[string]any { return map[string]any{"name": "Example", "idempotency_key": "create-key"} },
			fixture: func() (witselfMCPBackend, func() int) {
				backend := newFakeSecretMCPBackend()
				return backend, func() int {
					if backend.lastCreate.Name != "" {
						return 1
					}
					return 0
				}
			},
		},
		{
			tool: "witself.memory.supersede", field: "replacements", handlerError: mcpAlignmentSupersedeError,
			args: func() map[string]any {
				return map[string]any{"memory_id": "mem_1", "expected_version": 1, "idempotency_key": "supersede-key"}
			},
			fixture: func() (witselfMCPBackend, func() int) {
				backend := newFakeMemoryMCPBackend()
				return backend, func() int { return backend.supersedeCalls }
			},
		},
		{
			tool: "witself.message.request.select", field: "selected_agent_ids", handlerError: mcpAlignmentSelectionError,
			args: func() map[string]any { return map[string]any{"request_id": "mrq_1", "idempotency_key": "select-key"} },
			fixture: func() (witselfMCPBackend, func() int) {
				backend := &fakeMCPBackend{}
				return backend, func() int {
					if backend.selectRequestID != "" || backend.lastRequestSelect.IdempotencyKey != "" {
						return 1
					}
					return 0
				}
			},
		},
		{
			tool: "witself.memory.vector.set", field: "vector",
			args: func() map[string]any {
				return map[string]any{"profile_id": "mvp_1", "memory_id": "mem_1", "memory_version": 1, "content_hash": strings.Repeat("b", 64)}
			},
			fixture: func() (witselfMCPBackend, func() int) {
				backend := newFakeMemoryVectorMCPBackend()
				return backend, func() int { return backend.putCalls }
			},
		},
	} {
		for _, value := range []struct {
			name string
			list any
		}{
			{"empty", []any{}},
			{"null", nil},
		} {
			t.Run(tc.field+"/"+value.name, func(t *testing.T) {
				backend, calls := tc.fixture()
				session := newMCPAlignmentSession(t, newWitselfMCPServer(backend))
				args := tc.args()
				args[tc.field] = value.list
				result := callMCPAlignmentTool(t, session, tc.tool, args)
				wantCalls := 0
				if value.name == "empty" {
					requireMCPAlignmentRefusal(t, result, []string{mcpAlignmentSDKError, "minItems"}, tc.handlerError)
				} else if tc.field == "vector" {
					wantCalls = 1
					if result.IsError {
						t.Errorf("null vector was refused before the fake could accept it: %s", mcpAlignmentErrorText(result))
					}
				} else {
					requireMCPAlignmentRefusal(t, result, []string{tc.handlerError}, mcpAlignmentSDKError)
				}
				if got := calls(); got != wantCalls {
					t.Errorf("%s backend calls = %d, want %d (vector uses putCalls; create uses lastCreate.Name)", tc.field, got, wantCalls)
				}
			})
		}
	}
}

func TestMCPCurationNullListsAreRefusedBySchemaAndEmptyListsStillReachBackend(t *testing.T) {
	for _, tc := range []struct {
		field        string
		tool         string
		handlerError string
		args         func(any) map[string]any
		calls        func(*fakeCurationMCPBackend) int
	}{
		{
			field: "draft.actions", tool: "witself.memory.curation.plan", handlerError: mcpAlignmentPlanError,
			args: func(actions any) map[string]any {
				return map[string]any{"run_id": "mrun_1", "fencing_generation": 1, "idempotency_key": "plan-key",
					"draft": map[string]any{"schema": "witself.memory-plan.v1", "draft_revision": 1, "actions": actions}}
			},
			calls: func(backend *fakeCurationMCPBackend) int { return backend.planCalls },
		},
		{
			field: "expected_produced_heads", tool: "witself.memory.curation.rollback", handlerError: mcpAlignmentRollbackError,
			args: func(heads any) map[string]any {
				return map[string]any{"run_id": "mrun_1", "apply_receipt_id": "mcar_1", "idempotency_key": "rollback-key", "expected_produced_heads": heads}
			},
			calls: func(backend *fakeCurationMCPBackend) int {
				if backend.rollback.RunID != "" {
					return 1
				}
				return 0
			},
		},
	} {
		for _, value := range []struct {
			name string
			list any
		}{
			{"null", nil},
			{"empty", []any{}},
		} {
			t.Run(tc.field+"/"+value.name, func(t *testing.T) {
				backend := &fakeCurationMCPBackend{}
				session := newMCPAlignmentSession(t, newWitselfMCPServer(backend))
				result := callMCPAlignmentTool(t, session, tc.tool, tc.args(value.list))
				wantCalls := 0
				if value.name == "null" {
					requireMCPAlignmentRefusal(t, result, []string{mcpAlignmentSDKError, `want "array"`}, tc.handlerError)
				} else {
					wantCalls = 1
					if result.IsError {
						t.Errorf("empty %s result refused: %s; backend calls = %d", tc.field, mcpAlignmentErrorText(result), tc.calls(backend))
					}
					if tc.field == "expected_produced_heads" && len(backend.rollback.ExpectedProducedHeads) != 0 {
						t.Error("empty rollback heads were not forwarded empty")
					}
				}
				if got := tc.calls(backend); got != wantCalls {
					t.Errorf("%s backend calls = %d, want %d (plan uses planCalls; rollback uses rollback.RunID)", tc.field, got, wantCalls)
				}
			})
		}
	}
}

func TestMCPFactProposeReasonIsOptional(t *testing.T) {
	for _, tc := range []struct {
		name   string
		reason string
	}{
		{"omitted", ""},
		{"provided", "An explicit durable preference"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			backend := &fakeMCPBackend{}
			session := newMCPAlignmentSession(t, newWitselfMCPServer(backend))
			args := map[string]any{"predicate": "preferences/editor", "value": "vim", "idempotency_key": "propose-key"}
			if tc.name == "provided" {
				args["reason"] = tc.reason
			}
			result := callMCPAlignmentTool(t, session, "witself.fact.propose", args)
			if result.IsError {
				t.Fatalf("fact proposal refused: %s", mcpAlignmentErrorText(result))
			}
			got := backend.lastFactProposal
			if got.Reason != tc.reason || got.Predicate != "preferences/editor" || string(got.Value) != `"vim"` || got.IdempotencyKey != "propose-key" {
				t.Error("fact proposal did not forward the predicate, value, key, and optional reason unchanged")
			}
		})
	}
}

func TestMCPCurationEvidenceTypeIsOptional(t *testing.T) {
	backend := &fakeCurationMCPBackend{}
	session := newMCPAlignmentSession(t, newWitselfMCPServer(backend))
	var omittedDraft []byte
	for _, tc := range []struct {
		name string
		kind string
	}{
		{"omitted", ""},
		{"blank", ""},
		{"transcript", "transcript"},
	} {
		evidence := map[string]any{"resolution_state": "resolved", "resolved_kind": "transcript",
			"source_transcript_id": "trn_1", "source_sequence_from": 2, "source_sequence_until": 4}
		if tc.name != "omitted" {
			evidence["type"] = tc.kind
		}
		beforeCalls := backend.planCalls
		result := callMCPAlignmentTool(t, session, "witself.memory.curation.plan", map[string]any{
			"run_id": "mrun_1", "fencing_generation": 1, "idempotency_key": "plan-" + tc.name,
			"draft": map[string]any{"schema": "witself.memory-plan.v1", "draft_revision": 1,
				"actions": []map[string]any{{"ordinal": 1, "operation": "create",
					"create": map[string]any{"local_ref": "new_decision", "snapshot": map[string]any{
						"content": "Use PostgreSQL", "kind": "decision", "evidence": []map[string]any{evidence},
					}},
				}},
			},
		})
		if result.IsError {
			t.Fatalf("curation evidence type %s refused: %s", tc.name, mcpAlignmentErrorText(result))
		}
		if backend.planCalls != beforeCalls+1 {
			t.Fatalf("planCalls = %d, want %d", backend.planCalls, beforeCalls+1)
		}
		var draft struct {
			Actions []struct {
				Create struct {
					Snapshot struct {
						Evidence []map[string]any `json:"evidence"`
					} `json:"snapshot"`
				} `json:"create"`
			} `json:"actions"`
		}
		if err := json.Unmarshal(backend.plan.Draft, &draft); err != nil {
			t.Fatal(err)
		}
		if len(draft.Actions) != 1 || len(draft.Actions[0].Create.Snapshot.Evidence) != 1 {
			t.Fatal("forwarded draft lost its single create evidence row")
		}
		row := draft.Actions[0].Create.Snapshot.Evidence[0]
		kind, hasType := row["type"]
		if tc.name == "transcript" {
			if !hasType || kind != "transcript" {
				t.Error("explicit transcript type was not forwarded unchanged")
			}
		} else if hasType {
			t.Error("blank or omitted evidence type was forwarded with a type key")
		}
		if _, exists := row["input_evidence_id"]; exists {
			t.Error("direct transcript evidence unexpectedly acquired input_evidence_id")
		}
		if tc.name == "omitted" {
			omittedDraft = bytes.Clone(backend.plan.Draft)
		} else if tc.name == "blank" && !bytes.Equal(omittedDraft, backend.plan.Draft) {
			t.Error("blank and omitted evidence types produced different forwarded draft bytes")
		}
	}
}
