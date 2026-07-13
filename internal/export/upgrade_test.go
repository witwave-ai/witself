package export

import (
	"bytes"
	"encoding/json"
	"errors"
	"testing"
)

func TestSchema25FactIdempotencyUpgrade(t *testing.T) {
	upgrade := UpgraderFor(25)
	if upgrade == nil {
		t.Fatal("schema 25 upgrader is not registered")
	}
	assertion, err := upgrade("fact_assertions", map[string]any{"id": "fas_1"})
	if err != nil {
		t.Fatal(err)
	}
	if assertion["idempotency_key"] != "" || assertion["idempotency_fingerprint"] != "" {
		t.Fatalf("assertion defaults = %#v", assertion)
	}
	candidate, err := upgrade("fact_candidates", map[string]any{"id": "fcand_1"})
	if err != nil {
		t.Fatal(err)
	}
	if candidate["idempotency_key"] != "" || candidate["idempotency_fingerprint"] != "" || candidate["decision_idempotency_key"] != "" || candidate["decision_assertion_id"] != nil {
		t.Fatalf("candidate defaults = %#v", candidate)
	}
	other, err := upgrade("agents", map[string]any{"id": "agent_1"})
	if err != nil || len(other) != 1 || other["id"] != "agent_1" {
		t.Fatalf("unrelated row = %#v / %v", other, err)
	}
}

func TestUpgradeRowPreservesLargeIntegers(t *testing.T) {
	const exact = "9007199254740993"
	upgraded, err := upgradeRow("agents", []byte(`{"id":"agent_1","sequence":`+exact+`}`), 25, 26)
	if err != nil {
		t.Fatal(err)
	}
	decoder := json.NewDecoder(bytes.NewReader(upgraded))
	decoder.UseNumber()
	var row map[string]any
	if err := decoder.Decode(&row); err != nil {
		t.Fatal(err)
	}
	if got := row["sequence"].(json.Number).String(); got != exact {
		t.Fatalf("large integer changed during upgrade: got %s, want %s", got, exact)
	}
}

func TestUpgradeRowRejectsTrailingJSON(t *testing.T) {
	_, err := upgradeRow("agents", []byte(`{"id":"agent_1"}{"id":"agent_2"}`), 25, 26)
	if !errors.Is(err, ErrCorrupt) {
		t.Fatalf("trailing JSON error = %v, want ErrCorrupt", err)
	}
}

func TestUpgradeRowRejectsNull(t *testing.T) {
	_, err := upgradeRow("fact_assertions", []byte(`null`), 25, 26)
	if !errors.Is(err, ErrCorrupt) {
		t.Fatalf("null row error = %v, want ErrCorrupt", err)
	}
}
