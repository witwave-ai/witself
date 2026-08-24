package supportrunner

import (
	"bytes"
	"os"
	"testing"
)

func TestEmbeddedSupportPolicyMatchesPublishedPolicy(t *testing.T) {
	published, err := os.ReadFile("../../docs/support-policy.md")
	if err != nil {
		t.Fatalf("read published policy: %v", err)
	}
	if !bytes.Equal(supportPolicy, published) {
		t.Fatal("embedded support policy drifted from docs/support-policy.md; copy it byte-for-byte")
	}
}

func TestDecisionInputSchemaIsStrictAtEveryObject(t *testing.T) {
	schema := decisionInputSchema()
	if value, ok := schema["additionalProperties"].(bool); !ok || value {
		t.Fatalf("top-level additionalProperties = %#v", schema["additionalProperties"])
	}
	properties, ok := schema["properties"].(map[string]any)
	if !ok {
		t.Fatal("schema properties missing")
	}
	retriageSchema, ok := properties["retriage"].(map[string]any)
	if !ok {
		t.Fatal("retriage schema missing")
	}
	if value, ok := retriageSchema["additionalProperties"].(bool); !ok || value {
		t.Fatalf("retriage additionalProperties = %#v", retriageSchema["additionalProperties"])
	}
}
