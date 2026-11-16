package main

import (
	"context"
	"encoding/json"
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
