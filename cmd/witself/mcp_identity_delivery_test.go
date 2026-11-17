package main

import (
	"context"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/witwave-ai/witself/internal/client"
	"github.com/witwave-ai/witself/internal/memoryhydration"
	"github.com/witwave-ai/witself/internal/transcriptcapture"
)

func TestMCPIdentitySharedText(t *testing.T) {
	for _, row := range []struct{ name, value string }{
		{"empty", ""}, {"too_long", strings.Repeat("a", 49)},
		{"quote", "fer'n"}, {"double_quote", "fer\"n"}, {"backtick", "fer`n"},
		{"open_brace", "fer{n"}, {"close_brace", "fer}n"},
		{"newline", "fer\nn"}, {"tab", "fer\tn"}, {"control", "fer\x00n"},
		{"leading_space", " fern"}, {"trailing_space", "fern "}, {"double_space", "fern  leaf"},
		{"non_ascii_space", "fern\u00a0leaf"}, {"symbol", "fern★"},
		{"combining_mark", "fe\u0301rn"}, {"invalid_utf8", "fern\xff"},
		{"one_byte", "a"}, {"max_bytes", strings.Repeat("a", 48)},
		{"unicode_letters", "Férn 葉"}, {"unicode_max_bytes", strings.Repeat("葉", 16)},
		{"unicode_over_bytes", strings.Repeat("葉", 17)}, {"unicode_digits", "Fern ١٢"},
		{"allowed_punctuation", "Fern.Leaf-17_2"}, {"single_interior_spaces", "Fern Leaf 17"},
		{"modifier_letter", "fer\u02b0n"}, {"punctuation_only", ".-_ ."}, {"one_digit", "7"},
	} {
		t.Run(row.name, func(t *testing.T) {
			if validMCPIdentityName(row.value) != memoryhydration.ValidIdentityName(row.value) {
				t.Error("name validation drifted from shared implementation")
			}
			for _, form := range []mcpIdentityForm{mcpIdentityShort, mcpIdentityOneLine} {
				for _, names := range [][2]string{{row.value, ""}, {row.value, identityFixtureOwner}, {identityFixtureAgent, row.value}} {
					want := memoryhydration.RenderIdentityStatement(names[0], names[1], memoryhydration.IdentityForm(form))
					if renderMCPIdentityParagraph(names[0], names[1], form) != want {
						t.Error("identity renderer drifted from shared implementation")
					}
				}
			}
		})
	}
}

type identityDeliverySource struct{ digest client.SelfDigest }

func (s identityDeliverySource) Self(context.Context, client.SelfOptions) (client.SelfDigest, error) {
	return s.digest, nil
}

func (identityDeliverySource) Recall(context.Context, client.MemoryRecallInput) (client.MemoryRecallPage, error) {
	return client.MemoryRecallPage{}, nil
}

func TestMCPIdentitySingleDelivery(t *testing.T) {
	for _, runtime := range []string{
		transcriptcapture.RuntimeCodex, transcriptcapture.RuntimeClaudeCode, transcriptcapture.RuntimeGrokBuild,
		transcriptcapture.RuntimeCursor, transcriptcapture.RuntimeOpenClaw, transcriptcapture.RuntimeAntigravity,
		transcriptcapture.RuntimeCopilot, transcriptcapture.RuntimeDSH,
	} {
		if !memoryhydration.CapabilityFor(runtime).SessionHydration.Automatic {
			continue
		}
		t.Run(runtime, func(t *testing.T) {
			opts := mcpServerOptions{Profile: mcpProfileFull, IdentityAgentName: identityFixtureAgent}
			instructions := identityHandshakeInstructions(t, newWitselfMCPServerForRuntimeOptions(&fakeMCPBackend{}, runtime, opts))
			identity := client.SelfIdentity{AccountID: "acc_fixture", RealmID: "realm_fixture", RealmName: "grove", AgentID: "agent_fixture", AgentName: identityFixtureAgent}
			binding := memoryhydration.Binding{AccountID: identity.AccountID, RealmID: identity.RealmID, RealmName: identity.RealmName, AgentID: identity.AgentID, AgentName: identity.AgentName}
			result, err := memoryhydration.Execute(context.Background(), memoryhydration.Config{}, binding,
				memoryhydration.Request{Runtime: runtime, Event: memoryhydration.EventSessionStart}, identityDeliverySource{client.SelfDigest{Identity: identity}})
			if err != nil || !result.Injected {
				t.Fatal("session hydration did not succeed")
			}
			prefix, raw, ok := strings.Cut(result.Context, "\n")
			var envelope struct {
				Statement string `json:"identity_statement"`
			}
			if !ok || prefix != "WITSELF_AUTOMATIC_CONTEXT_V1" || json.Unmarshal([]byte(raw), &envelope) != nil {
				t.Fatal("session envelope is invalid")
			}
			statement := memoryhydration.RenderIdentityStatement(identity.AgentName, "", memoryhydration.IdentityShort)
			instructionCount := strings.Count(instructions, statement)
			if envelope.Statement != "" && envelope.Statement != statement {
				t.Fatal("session identity statement differs from shared renderer")
			}
			wantSession := runtime == transcriptcapture.RuntimeClaudeCode
			wantInstructions := 1
			if wantSession {
				wantInstructions = 0
			}
			if (envelope.Statement != "") != wantSession || instructionCount != wantInstructions {
				t.Fatal("identity statement must arrive through exactly one designated channel")
			}
		})
	}
}

// mcpProfilesFromSource reads the profiles validMCPProfile accepts from its
// switch in mcp.go, and every declared mcpProfile constant, so the delivery
// rows follow the code instead of a table that restates it.
func mcpProfilesFromSource(t *testing.T) (accepted, declared []string) {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), "mcp.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	values := map[string]string{}
	var cases []string
	for _, decl := range file.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Name.Name == "validMCPProfile" {
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				if clause, ok := n.(*ast.CaseClause); ok {
					for _, expr := range clause.List {
						if id, ok := expr.(*ast.Ident); ok {
							cases = append(cases, id.Name)
						}
					}
				}
				return true
			})
		} else if gen, ok := decl.(*ast.GenDecl); ok && gen.Tok == token.CONST {
			for _, spec := range gen.Specs {
				value := spec.(*ast.ValueSpec)
				for i, name := range value.Names {
					if i >= len(value.Values) || !strings.HasPrefix(name.Name, "mcpProfile") {
						continue
					}
					if lit, ok := value.Values[i].(*ast.BasicLit); ok && lit.Kind == token.STRING {
						values[name.Name], _ = strconv.Unquote(lit.Value)
						declared = append(declared, values[name.Name])
					}
				}
			}
		}
	}
	for _, name := range cases {
		accepted = append(accepted, values[name])
	}
	return accepted, declared
}

func TestMCPIdentitySingleDeliveryEveryVariant(t *testing.T) {
	accepted, declared := mcpProfilesFromSource(t)
	options := reflect.TypeFor[mcpServerOptions]()
	var toggles []int
	for i := range options.NumField() {
		if options.Field(i).Type.Kind() == reflect.Bool {
			toggles = append(toggles, i)
		}
	}
	// Curator instructions never carry the statement, so a curator profile of a
	// runtime without session delivery has it in neither channel: the one
	// documented exception. Every other row has exactly one channel.
	expected := map[[2]bool]string{{false, false}: "instructions", {false, true}: "session", {true, false}: "neither", {true, true}: "session"}
	observed := map[[2]bool]string{{false, false}: "neither", {true, false}: "instructions", {false, true}: "session", {true, true}: "both"}
	enumerated := map[string]bool{}
	for _, runtime := range transcriptcapture.SupportedRuntimes() {
		if !memoryhydration.CapabilityFor(runtime).SessionHydration.Automatic {
			continue
		}
		identity := client.SelfIdentity{AccountID: "acc_fixture", RealmID: "realm_fixture", RealmName: "grove", AgentID: "agent_fixture", AgentName: identityFixtureAgent}
		binding := memoryhydration.Binding{AccountID: identity.AccountID, RealmID: identity.RealmID, RealmName: identity.RealmName, AgentID: identity.AgentID, AgentName: identity.AgentName}
		result, err := memoryhydration.Execute(context.Background(), memoryhydration.Config{}, binding,
			memoryhydration.Request{Runtime: runtime, Event: memoryhydration.EventSessionStart}, identityDeliverySource{client.SelfDigest{Identity: identity}})
		_, raw, _ := strings.Cut(result.Context, "\n")
		var envelope struct {
			Statement string `json:"identity_statement"`
		}
		if err != nil || !result.Injected || json.Unmarshal([]byte(raw), &envelope) != nil ||
			envelope.Statement != "" && envelope.Statement != memoryhydration.RenderIdentityStatement(identityFixtureAgent, "", memoryhydration.IdentityShort) {
			t.Fatal("session hydration fixture did not render")
		}
		for _, profile := range accepted {
			for mask := range 1 << len(toggles) {
				opts := mcpServerOptions{Profile: profile, IdentityAgentName: identityFixtureAgent}
				name := runtime + "_" + profile
				for bit, field := range toggles {
					reflect.ValueOf(&opts).Elem().Field(field).SetBool(mask&(1<<bit) != 0)
					name += fmt.Sprintf("_%s=%t", options.Field(field).Name, mask&(1<<bit) != 0)
				}
				enumerated[profile] = true
				t.Run(name, func(t *testing.T) {
					instructions := identityHandshakeInstructions(t, newWitselfMCPServerForRuntimeOptions(&fakeMCPBackend{}, runtime, opts))
					copies := 0
					for _, form := range []memoryhydration.IdentityForm{memoryhydration.IdentityShort, memoryhydration.IdentityOneLine} {
						copies += strings.Count(instructions, memoryhydration.RenderIdentityStatement(identityFixtureAgent, "", form))
					}
					got := observed[[2]bool{copies > 0, envelope.Statement != ""}]
					want := expected[[2]bool{isCuratorMCPProfile(effectiveMCPProfile(opts)), envelope.Statement != ""}]
					if copies > 1 || got != want {
						t.Fatalf("statement channel is %s with %d instruction copies, want %s", got, copies, want)
					}
					t.Logf("channel=%s instruction_bytes=%d", got, len(instructions))
				})
			}
		}
	}
	t.Run("enumeration", func(t *testing.T) {
		if len(enumerated) == 0 || len(declared) == 0 || len(toggles) == 0 {
			t.Fatal("variant enumeration found no runtime, profile or toggle")
		}
		for _, profile := range accepted {
			if !validMCPProfile(profile) {
				t.Errorf("enumerated profile %q is not accepted", profile)
			}
		}
		for _, profile := range declared {
			if !enumerated[profile] {
				t.Errorf("declared profile %q has no delivery row", profile)
			}
		}
	})
}
