package main

import (
	"bytes"
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestMCPInputSchemasRequireHandlerRequiredFields(t *testing.T) {
	rows := []struct {
		tool     string
		required []string
		optional []string
	}{
		{tool: "witself.self.show"},
		{tool: "witself.agent.peers"},
		{tool: "witself.fact.set", required: []string{"predicate", "value", "idempotency_key"}, optional: []string{"subject", "recreate_deleted", "direct_user_authorized"}},
		{tool: "witself.fact.delete", required: []string{"mode"}, optional: []string{"subject", "predicate", "fact_id", "expected_resolved_assertion_id", "expected_candidate_revision", "idempotency_key", "direct_user_authorized"}},
		{tool: "witself.fact.propose", required: []string{"predicate", "value", "idempotency_key", "reason"}, optional: []string{"subject", "confidence"}},
		{tool: "witself.fact.propose_from_transcript", required: []string{"transcript_id", "entry_sequence", "predicate", "value", "reason", "idempotency_key"}, optional: []string{"subject"}},
		{tool: "witself.fact.candidate.get", required: []string{"candidate_id"}},
		{tool: "witself.fact.confirm", required: []string{"candidate_id", "idempotency_key"}},
		{tool: "witself.fact.reject", required: []string{"candidate_id", "idempotency_key"}},
		{tool: "witself.fact.get", required: []string{"predicate"}, optional: []string{"subject"}},
		{tool: "witself.fact.review"},
		{tool: "witself.fact.list"},
		{tool: "witself.fact.upcoming"},
		{tool: "witself.fact.subject.set", required: []string{"canonical_key"}, optional: []string{"display_name"}},
		{tool: "witself.fact.subject.alias", required: []string{"canonical_key", "alias"}},
		{tool: "witself.fact.subject.list"},
		{tool: "witself.fact.status"},
		{tool: "witself.transcript.list"},
		{tool: "witself.transcript.get", required: []string{"transcript_id"}},
		{tool: "witself.transcript.tail", required: []string{"transcript_id"}},
		{tool: "witself.message.send", required: []string{"body", "idempotency_key"}, optional: []string{"to", "to_agents", "to_realm", "to_kind"}},
		{tool: "witself.message.reply", required: []string{"message_id", "body", "idempotency_key"}},
		{tool: "witself.message.read", required: []string{"message_id"}},
		{tool: "witself.message.ack", required: []string{"message_id"}},
		{tool: "witself.message.claim", required: []string{"message_id", "idempotency_key"}, optional: []string{"lease_seconds"}},
		{tool: "witself.message.renew", required: []string{"message_id", "claim_id", "generation"}},
		{tool: "witself.message.release", required: []string{"message_id", "claim_id", "generation"}, optional: []string{"deterministic_failure"}},
		{tool: "witself.message.complete", required: []string{"message_id", "claim_id", "generation", "body", "idempotency_key"}},
		{tool: "witself.message.list"},
		{tool: "witself.message.listen"},
		{tool: "witself.message.request.open", required: []string{"body", "idempotency_key"}},
		{tool: "witself.message.request.list"},
		{tool: "witself.message.request.show", required: []string{"request_id"}},
		{tool: "witself.message.request.cancel", required: []string{"request_id"}},
		{tool: "witself.message.request.offer", required: []string{"request_id", "body", "idempotency_key"}},
		{tool: "witself.message.request.decline", required: []string{"request_id"}, optional: []string{"idempotency_key"}},
		{tool: "witself.message.request.select", required: []string{"request_id", "selected_agent_ids", "idempotency_key"}, optional: []string{"reservation_seconds"}},
		{tool: "witself.message.request.claim", required: []string{"request_id", "idempotency_key"}, optional: []string{"lease_seconds"}},
		{tool: "witself.message.request.renew", required: []string{"request_id", "claim_id", "generation"}},
		{tool: "witself.message.request.release", required: []string{"request_id", "claim_id", "generation"}},
		{tool: "witself.message.request.complete", required: []string{"request_id", "claim_id", "generation", "body", "idempotency_key"}},
		{tool: "witself.memory.capture", required: []string{"content", "kind", "evidence", "capture_reason", "idempotency_key"}},
		{tool: "witself.memory.read", required: []string{"memory_id"}},
		{tool: "witself.memory.history", required: []string{"memory_id"}},
		{tool: "witself.memory.status"},
		{tool: "witself.memory.list"},
		{tool: "witself.memory.recall", optional: []string{"query", "vector_profile_id", "query_vector"}},
		{tool: "witself.memory.adjust", required: []string{"memory_id", "expected_version", "idempotency_key"}, optional: []string{"set_content", "set_kind", "add_tags", "set_sensitive"}},
		{tool: "witself.memory.supersede", required: []string{"memory_id", "expected_version", "replacements", "idempotency_key"}, optional: []string{"reason"}},
		{tool: "witself.memory.forget", required: []string{"memory_id", "expected_version", "idempotency_key"}},
		{tool: "witself.memory.restore", required: []string{"memory_id", "expected_version", "idempotency_key"}},
		{tool: "witself.memory.reactivate", required: []string{"memory_id", "expected_version", "idempotency_key"}, optional: []string{"expected_supersession_set_revision"}},
		{tool: "witself.memory.evidence.resolve", required: []string{"evidence_id", "idempotency_key"}, optional: []string{"transcript_id", "entry_from_sequence", "entry_until_sequence", "source_memory_id", "source_memory_version", "message_id", "import_artifact_id", "unresolvable_reason"}},
		{tool: "witself.memory.delete", required: []string{"mode", "memory_id"}, optional: []string{"expected_version", "scrub_set_revision", "idempotency_key", "direct_user_authorized"}},
		{tool: "witself.memory.vector.profile.create", required: []string{"provider", "model", "recipe", "recipe_version", "dimensions", "distance_metric", "normalization"}},
		{tool: "witself.memory.vector.profile.list"},
		{tool: "witself.memory.vector.set", required: []string{"profile_id", "memory_id", "memory_version", "content_hash", "vector"}},
		{tool: "witself.memory.curation.preflight"},
		{tool: "witself.memory.curation.requests"},
		{tool: "witself.memory.curation.status"},
		{tool: "witself.memory.curation.request.get", required: []string{"request_id"}},
		{tool: "witself.memory.curation.request", required: []string{"idempotency_key"}},
		{tool: "witself.memory.curation.start", required: []string{"request_id", "idempotency_key"}},
		{tool: "witself.memory.curation.run.get", required: []string{"run_id"}},
		{tool: "witself.memory.curation.renew", required: []string{"run_id", "fencing_generation", "idempotency_key"}},
		{tool: "witself.memory.curation.get", required: []string{"run_id", "fencing_generation"}, optional: []string{"cursor", "limit", "max_bytes"}},
		{tool: "witself.memory.curation.plan", required: []string{"run_id", "fencing_generation", "draft", "idempotency_key"}},
		{tool: "witself.memory.curation.plan.get", required: []string{"run_id", "fencing_generation"}},
		{tool: "witself.memory.curation.apply", required: []string{"run_id", "fencing_generation", "plan_revision", "plan_hash", "idempotency_key"}},
		{tool: "witself.memory.curation.cancel", required: []string{"run_id", "fencing_generation", "idempotency_key"}, optional: []string{"reason"}},
		{tool: "witself.memory.curation.abandon", required: []string{"run_id", "fencing_generation", "idempotency_key"}, optional: []string{"reason"}},
		{tool: "witself.memory.curation.rollback", required: []string{"run_id", "apply_receipt_id", "expected_produced_heads", "idempotency_key"}},
		{tool: "witself.avatar.show"},
		{tool: "witself.avatar.history"},
		{tool: "witself.avatar.style.show"},
		{tool: "witself.avatar.version.show", required: []string{"version"}},
		{tool: "witself.avatar.propose", required: []string{"expected_profile_revision", "style_pack_id", "style_pack_version", "subject_form", "description", "visual_spec", "svg", "idempotency_key"}, optional: []string{"parent_version", "provenance"}},
		{tool: "witself.avatar.activate", required: []string{"version", "expected_profile_revision", "idempotency_key"}},
		{tool: "witself.avatar.rollback", required: []string{"version", "expected_profile_revision", "idempotency_key"}},
		{tool: "witself.avatar.reset", required: []string{"expected_profile_revision", "idempotency_key"}, optional: []string{"reason_code"}},
		{tool: "witself.avatar.generation.fail", required: []string{"expected_profile_revision", "reason_code", "idempotency_key"}},
		{tool: "witself.email.status"},
		{tool: "witself.email.address.show"},
		{tool: "witself.email.list"},
		{tool: "witself.email.listen"},
		{tool: "witself.email.read", required: []string{"message_id"}},
		{tool: "witself.email.code.candidates", required: []string{"message_id"}},
		{tool: "witself.email.code.consume", required: []string{"message_id"}},
		{tool: "witself.email.ack", required: []string{"message_id"}},
		{tool: "witself.email.claim", required: []string{"message_id", "idempotency_key"}, optional: []string{"lease_seconds"}},
		{tool: "witself.email.renew", required: []string{"message_id", "claim_id", "generation"}, optional: []string{"lease_seconds"}},
		{tool: "witself.email.release", required: []string{"message_id", "claim_id", "generation"}, optional: []string{"deterministic_failure"}},
		{tool: "witself.email.complete", required: []string{"message_id", "claim_id", "generation", "idempotency_key"}},
		{tool: "witself.email.send", required: []string{"to", "subject", "text", "idempotency_key"}},
		{tool: "witself.email.reply", required: []string{"inbound_message_id", "text", "idempotency_key"}},
		{tool: "witself.email.sent.show", required: []string{"message_id"}},
		{tool: "witself.email.sent.list"},
		{tool: "witself.secret.status"},
		{tool: "witself.secret.search"},
		{tool: "witself.secret.show", required: []string{"secret_id"}},
		{tool: "witself.secret.create", required: []string{"name", "fields", "idempotency_key"}, optional: []string{"description", "template", "tags"}},
		{tool: "witself.secret.delete", required: []string{"secret_id", "expected_row_version", "idempotency_key"}},
		{tool: "witself.secret.reveal", required: []string{"secret_id", "field_id", "idempotency_key"}},
		{tool: "witself.password.generate"},
		{tool: "witself.totp.code", required: []string{"secret_id", "field_id", "idempotency_key"}},
	}
	nestedRows := []struct {
		tool     string
		path     string
		required []string
		optional []string
	}{
		{
			tool: "witself.memory.capture", path: "evidence.items",
			required: []string{"state"},
			optional: []string{"external_locator", "unavailable_reason", "transcript_id", "entry_from_sequence", "entry_until_sequence", "source_memory_id", "source_memory_version", "message_id", "import_artifact_id"},
		},
		{
			tool: "witself.memory.supersede", path: "replacements.items",
			required: []string{"content", "kind", "evidence", "capture_reason", "idempotency_key"},
		},
		{
			tool: "witself.memory.supersede", path: "replacements.items.evidence.items",
			required: []string{"state"},
		},
		{
			tool: "witself.secret.create", path: "fields.items",
			required: []string{"name"},
			optional: []string{"kind", "value", "generate_password", "otpauth_uri", "password_policy"},
		},
		{
			tool: "witself.memory.curation.plan", path: "draft.actions.items.create.snapshot.evidence.items",
			required: []string{"resolution_state", "type"},
			optional: []string{"input_evidence_id", "role", "external_locator", "resolved_kind", "terminal_reason_code"},
		},
		{
			tool: "witself.memory.curation.plan", path: "draft.actions.items.replace.snapshot.evidence.items",
			required: []string{"resolution_state", "type"},
			optional: []string{"input_evidence_id", "role", "external_locator", "resolved_kind", "terminal_reason_code"},
		},
		{
			tool: "witself.memory.curation.plan", path: "draft.actions.items.propose_fact.evidence.items",
			required: []string{"resolution_state", "type"},
			optional: []string{"input_evidence_id", "role", "external_locator", "resolved_kind", "terminal_reason_code"},
		},
	}

	declared := make(map[string]bool, len(rows))
	for _, row := range rows {
		if declared[row.tool] {
			t.Errorf("duplicate top-level required-field row for tool %q", row.tool)
		}
		declared[row.tool] = true
	}
	declaredNested := make(map[[2]string]bool, len(nestedRows))
	for _, row := range nestedRows {
		key := [2]string{row.tool, row.path}
		if declaredNested[key] {
			t.Errorf("duplicate nested required-field row for tool %q path %q", row.tool, row.path)
		}
		declaredNested[key] = true
	}

	ctx := context.Background()
	schemas := make(map[string]map[string]any)
	encodedSchemas := make(map[string][]byte)
	for _, fixture := range []struct {
		name    string
		backend witselfMCPBackend
	}{
		{name: "base", backend: &fakeMCPBackend{}},
		{name: "vector", backend: newFakeMemoryVectorMCPBackend()},
		{name: "avatar", backend: newFakeAvatarMCPBackend()},
		{name: "inbound_email", backend: &fakeAgentEmailMCPBackend{fakeMCPBackend: &fakeMCPBackend{}}},
		{name: "outbound_email", backend: &fakeAgentEmailOutboundMCPBackend{fakeMCPBackend: &fakeMCPBackend{}}},
		{name: "secret", backend: newFakeSecretMCPBackend()},
	} {
		// Build the union in the parent test, not in subtests, so a -run
		// pattern that selects one row still sees every listed tool.
		func() {
			server := newWitselfMCPServer(fixture.backend)
			clientTransport, serverTransport := mcp.NewInMemoryTransports()
			serverSession, err := server.Connect(ctx, serverTransport, nil)
			if err != nil {
				t.Fatalf("%s backend: connect server: %v", fixture.name, err)
			}
			defer func() { _ = serverSession.Close() }()
			clientSession, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil).Connect(ctx, clientTransport, nil)
			if err != nil {
				t.Fatalf("%s backend: connect client: %v", fixture.name, err)
			}
			defer func() { _ = clientSession.Close() }()

			params := &mcp.ListToolsParams{}
			for {
				page, err := clientSession.ListTools(ctx, params)
				if err != nil {
					t.Fatalf("%s backend: list tools: %v", fixture.name, err)
				}
				for _, tool := range page.Tools {
					raw, err := json.Marshal(tool.InputSchema)
					if err != nil {
						t.Fatalf("%s backend: tool %q: marshal input schema: %v", fixture.name, tool.Name, err)
					}
					if prior, exists := encodedSchemas[tool.Name]; exists {
						if !bytes.Equal(prior, raw) {
							t.Errorf("%s backend: tool %q input schema differs between backend registrations", fixture.name, tool.Name)
						}
						continue
					}
					var schema map[string]any
					if err := json.Unmarshal(raw, &schema); err != nil {
						t.Fatalf("%s backend: tool %q: decode input schema: %v", fixture.name, tool.Name, err)
					}
					encodedSchemas[tool.Name] = raw
					schemas[tool.Name] = schema
				}
				if page.NextCursor == "" {
					break
				}
				params.Cursor = page.NextCursor
			}
		}()
	}
	t.Logf("listed %d distinct tools with %d top-level required-field rows", len(schemas), len(rows))
	for tool := range schemas {
		if !declared[tool] {
			t.Errorf("tool %q has no required-field row; declare its required set", tool)
		}
	}
	for _, inventory := range []struct {
		name  string
		tools []string
	}{
		{name: "mcpMutatingToolNames", tools: mcpMutatingToolNames("")},
		{name: "mcpValueReturningSecretToolNames", tools: mcpValueReturningSecretToolNames("")},
	} {
		for _, tool := range inventory.tools {
			if !declared[tool] {
				t.Errorf("tool %q in %s has no required-field row; declare its required set", tool, inventory.name)
			}
		}
	}

	check := func(t *testing.T, tool, path string, required, optional []string) {
		t.Helper()
		root, exists := schemas[tool]
		if !exists {
			t.Fatalf("required-field row names stale tool %q: no server lists it", tool)
		}
		schema := resolveMCPObjectSchema(t, root, root)
		if path != "" {
			for _, segment := range strings.Split(path, ".") {
				if segment == "items" {
					schema = requireMCPArrayItem(t, root, schema)
				} else {
					schema = requireMCPObjectProperty(t, root, schema, segment)
				}
			}
		}
		schema = resolveMCPObjectSchema(t, root, schema)
		got := mcpExactRequiredFields(t, schema)
		want := slices.Clone(required)
		slices.Sort(want)
		if !slices.Equal(got, want) {
			t.Errorf("tool %q path %q required fields = %v, want %v", tool, path, got, want)
		}
		for _, field := range required {
			requireMCPObjectProperty(t, root, schema, field)
			if !mcpSchemaRequiresProperty(t, root, schema, field) {
				t.Errorf("tool %q path %q field %q is not required", tool, path, field)
			}
		}
		for _, field := range got {
			if !slices.Contains(want, field) {
				t.Errorf("tool %q path %q field %q is unexpectedly required", tool, path, field)
			}
		}
		for _, field := range optional {
			requireMCPObjectProperty(t, root, schema, field)
			if mcpSchemaRequiresProperty(t, root, schema, field) {
				t.Errorf("tool %q path %q optional field %q is unexpectedly required", tool, path, field)
			}
		}
	}
	for _, row := range rows {
		t.Run(row.tool, func(t *testing.T) {
			check(t, row.tool, "", row.required, row.optional)
		})
	}
	for _, row := range nestedRows {
		t.Run(row.tool+"/"+row.path, func(t *testing.T) {
			check(t, row.tool, row.path, row.required, row.optional)
		})
	}
}

// mcpExactRequiredFields reads an already resolved object schema.
func mcpExactRequiredFields(t *testing.T, schema map[string]any) []string {
	t.Helper()
	raw, exists := schema["required"]
	if !exists {
		return nil
	}
	fields, ok := raw.([]any)
	if !ok {
		t.Fatalf("schema required array has unexpected type %T", raw)
	}
	required := make([]string, 0, len(fields))
	for _, value := range fields {
		field, ok := value.(string)
		if !ok {
			t.Fatalf("schema required array contains non-string field %v", value)
		}
		required = append(required, field)
	}
	slices.Sort(required)
	return required
}
