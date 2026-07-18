package avatar

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

func TestGeneratePlaceholderSVGIsDeterministicAndStyleCompliant(t *testing.T) {
	first, err := GeneratePlaceholderSVG("agent_123", "Juniper")
	if err != nil {
		t.Fatal(err)
	}
	second, err := GeneratePlaceholderSVG("agent_123", "Juniper")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) {
		t.Fatal("same agent ID and name produced different placeholders")
	}
	if len(first) == 0 || len(first) > MaxSVGBytes {
		t.Fatalf("placeholder size = %d", len(first))
	}
	if err := ValidateSVGForStylePack(first, BuiltInFlatVectorStylePack()); err != nil {
		t.Fatalf("placeholder did not satisfy built-in style: %v\n%s", err, first)
	}
}

func TestGeneratePlaceholderSVGUsesBothSeedParts(t *testing.T) {
	base, err := GeneratePlaceholderSVG("agent_123", "Juniper")
	if err != nil {
		t.Fatal(err)
	}
	changedID, err := GeneratePlaceholderSVG("agent_124", "Juniper")
	if err != nil {
		t.Fatal(err)
	}
	changedName, err := GeneratePlaceholderSVG("agent_123", "Cedar")
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(base, changedID) {
		t.Fatal("changing agent ID did not change placeholder")
	}
	if bytes.Equal(base, changedName) {
		t.Fatal("changing agent name did not change placeholder")
	}
}

func TestGeneratePlaceholderSVGForStylePackUsesCustomHumanReference(t *testing.T) {
	pack := BuiltInFlatVectorStylePack()
	pack.ID = "operator-flat"
	pack.Name = "Operator Flat"

	got, err := GeneratePlaceholderSVGForStylePack("agent_123", "Juniper", pack)
	if err != nil {
		t.Fatal(err)
	}
	want, err := SanitizeSVGForStylePack([]byte(pack.References[0].SVG), pack)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("custom style placeholder was not derived from its human reference")
	}
	if err := ValidateSVGForStylePack(got, pack); err != nil {
		t.Fatalf("custom style placeholder is not style-compliant: %v", err)
	}
	builtIn, err := GeneratePlaceholderSVG("agent_123", "Juniper")
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(got, builtIn) {
		t.Fatal("custom style placeholder silently reused built-in generated art")
	}
}

func TestGeneratePlaceholderSVGForStylePackPreservesBuiltInVariation(t *testing.T) {
	pack := BuiltInFlatVectorStylePack()
	first, err := GeneratePlaceholderSVGForStylePack("agent_123", "Juniper", pack)
	if err != nil {
		t.Fatal(err)
	}
	second, err := GeneratePlaceholderSVGForStylePack("agent_124", "Juniper", pack)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(first, second) {
		t.Fatal("built-in style-aware placeholders lost seed-derived variation")
	}
}

func TestGeneratePlaceholderSVGDoesNotEmbedSeedInput(t *testing.T) {
	agentID := `agent_<script>`
	agentName := `Juniper <script>alert(1)</script>`
	got, err := GeneratePlaceholderSVG(agentID, agentName)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(got, []byte(agentID)) || bytes.Contains(got, []byte(agentName)) ||
		bytes.Contains(bytes.ToLower(got), []byte("script")) {
		t.Fatalf("placeholder leaked or embedded raw seed input: %s", got)
	}
	if err := ValidateSVG(got); err != nil {
		t.Fatalf("placeholder failed SVG validation: %v", err)
	}
}

func TestGeneratePlaceholderSVGNormalizesOuterWhitespace(t *testing.T) {
	trimmed, err := GeneratePlaceholderSVG("agent_123", "Juniper")
	if err != nil {
		t.Fatal(err)
	}
	padded, err := GeneratePlaceholderSVG("  agent_123  ", "  Juniper  ")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(trimmed, padded) {
		t.Fatal("outer whitespace changed placeholder seed")
	}
}

func TestGeneratePlaceholderSVGRejectsInvalidSeed(t *testing.T) {
	tests := []struct {
		id   string
		name string
	}{
		{"", "Juniper"},
		{"agent_123", ""},
		{strings.Repeat("a", MaxSeedAgentIDBytes+1), "Juniper"},
		{"agent_123", strings.Repeat("n", MaxSeedAgentNameBytes+1)},
		{"agent_\n123", "Juniper"},
		{"agent_123", "Juni\x00per"},
		{string([]byte{0xff}), "Juniper"},
		{"agent_123", string([]byte{0xff})},
	}
	for _, test := range tests {
		if _, err := GeneratePlaceholderSVG(test.id, test.name); !errors.Is(err, ErrInvalidPlaceholderSeed) {
			t.Errorf("GeneratePlaceholderSVG(%q, %q) error = %v", test.id, test.name, err)
		}
	}
}
